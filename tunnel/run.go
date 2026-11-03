package tunnel

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync/atomic"
)

const (
	DefaultGoogleIP    = "216.239.38.120"
	DefaultFrontDomain = "www.google.com"
)

// Status is the client's connection state as shown to the user.
type Status string

const (
	StatusConnecting Status = "connecting"
	StatusConnected  Status = "connected"
)

// ClientOptions configures RunClient.
type ClientOptions struct {
	Own      MailboxConfig
	Node     NodeInfo
	Listener net.Listener
	OnStatus func(Status) // optional
}

// RunClient serves proxy connections until ctx ends, starting a new client run whenever the node loses the current one.
func RunClient(ctx context.Context, options ClientOptions) {
	onStatus := options.OnStatus
	if onStatus == nil {
		onStatus = func(Status) {}
	}
	go func() {
		<-ctx.Done()
		options.Listener.Close()
	}()

	var current atomic.Pointer[Client]
	go func() {
		for ctx.Err() == nil {
			onStatus(StatusConnecting)
			client, err := NewClient(options.Own, options.Node)
			if err != nil {
				log.Printf("client: %v", err)
				return
			}
			clientCtx, cancel := context.WithCancel(ctx)
			current.Store(client)
			go client.run(clientCtx)
			client.keepRegistered(clientCtx, onStatus)
			cancel()
		}
	}()

	log.Printf("client: proxy on %s", options.Listener.Addr())
	for {
		conn, err := options.Listener.Accept()
		if err != nil {
			return
		}
		if client := current.Load(); client != nil {
			go client.handle(conn)
		} else {
			conn.Close()
		}
	}
}

// RunNode serves every mailbox registered through the registry mailbox until ctx ends.
// With statusListen set it also serves a plain-text status page showing the node string.
func RunNode(ctx context.Context, registry MailboxConfig, nodeKey, statusListen string) error {
	privateKey, publicKey, err := parsePrivateKey(nodeKey)
	if err != nil {
		return fmt.Errorf("node key: %w", err)
	}
	info := NodeInfo{DeploymentID: registry.DeploymentID, AuthKey: registry.AuthKey, PublicKey: publicKey}
	log.Printf("node: %s", info)
	userConfig := MailboxConfig{GoogleIP: registry.GoogleIP, FrontDomain: registry.FrontDomain}
	server := NewNodeServer(NewMailbox(registry), userConfig, privateKey, publicKey)

	if statusListen != "" {
		go func() {
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				fmt.Fprintf(w, "mailbox-tunnel node\nactive sessions: %d\n\n%s\n", server.SessionCount(), info)
			})
			log.Printf("status page on %s", statusListen)
			if err := http.ListenAndServe(statusListen, handler); err != nil {
				log.Printf("status page: %v", err)
			}
		}()
	}
	server.Run(ctx)
	return nil
}

// GenerateNodeKey returns a new node private key and its public key, both base64url.
func GenerateNodeKey() (privateKey, publicKey string, err error) {
	private, public, err := generateKeyPair()
	if err != nil {
		return "", "", err
	}
	return base64URL.EncodeToString(private[:]), base64URL.EncodeToString(public[:]), nil
}
