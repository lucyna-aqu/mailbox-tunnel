// Command mailbox-tunnel tunnels TCP through a Google Apps Script web app used as a mailbox.
//
//	client (SOCKS5) <-> user's Apps Script cache <-> node <-> internet
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"time"
)

const defaultGoogleIP = "216.239.38.120"

func main() {
	mode := flag.String("mode", "", "client, node or keygen")
	deploymentID := flag.String("deployment", os.Getenv("MAILBOX_DEPLOYMENT_ID"), "your Apps Script deployment ID (or MAILBOX_DEPLOYMENT_ID)")
	authKey := flag.String("key", os.Getenv("MAILBOX_AUTH_KEY"), "AUTH_KEY from your Code.gs (or MAILBOX_AUTH_KEY)")
	nodeString := flag.String("node", os.Getenv("MAILBOX_NODE"), "client: the node to use, mtn://... (or MAILBOX_NODE)")
	nodeKey := flag.String("node-key", os.Getenv("MAILBOX_NODE_KEY"), "node: private key from -mode keygen (or MAILBOX_NODE_KEY)")
	listen := flag.String("listen", "127.0.0.1:1080", "client: SOCKS5 listen address")
	statusListen := flag.String("status-listen", os.Getenv("STATUS_LISTEN"), "node: serve a plain-text status page on this address, e.g. :7860 (or STATUS_LISTEN)")
	googleIP := flag.String("google-ip", "", "Google IP to dial instead of script.google.com (default "+defaultGoogleIP+" for the client, direct for the node)")
	frontDomain := flag.String("front-domain", "www.google.com", "TLS SNI shown to the network when fronting")
	duration := flag.Duration("duration", 0, "stop after this long (0 = until interrupted)")
	flag.IntVar(&testDropEvery, "test-drop-every", 0, "testing only: skip the first put of every Nth batch")
	flag.Parse()

	if *mode == "keygen" {
		privateKey, publicKey, err := generateKeyPair()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("node key (keep secret): %s\npublic key:             %s\n", base64URL.EncodeToString(privateKey[:]), base64URL.EncodeToString(publicKey[:]))
		return
	}
	if (*mode != "client" && *mode != "node") || *deploymentID == "" || *authKey == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *googleIP == "" && *mode == "client" {
		*googleIP = defaultGoogleIP
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	config := MailboxConfig{DeploymentID: *deploymentID, AuthKey: *authKey, GoogleIP: *googleIP, FrontDomain: *frontDomain}
	started := time.Now()
	if err := NewMailbox(config).Ping(ctx); err != nil {
		log.Fatalf("mailbox unreachable: %v", err)
	}
	log.Printf("mailbox OK (%s)", time.Since(started).Round(time.Millisecond))

	if *mode == "node" {
		runNode(ctx, config, *nodeKey, *statusListen)
	} else {
		runClient(ctx, config, *nodeString, *listen)
	}
}

func runNode(ctx context.Context, registry MailboxConfig, nodeKey, statusListen string) {
	privateKey, publicKey, err := parsePrivateKey(nodeKey)
	if err != nil {
		log.Fatalf("-node-key: %v (create one with -mode keygen)", err)
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
}

func runClient(ctx context.Context, own MailboxConfig, nodeString, listenAddress string) {
	node, err := ParseNodeInfo(nodeString)
	if err != nil {
		log.Fatalf("-node: %v", err)
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	var current atomic.Pointer[Client]
	go func() {
		for ctx.Err() == nil {
			client, err := NewClient(own, node)
			if err != nil {
				log.Fatal(err)
			}
			clientCtx, cancel := context.WithCancel(ctx)
			current.Store(client)
			go client.run(clientCtx)
			client.keepRegistered(clientCtx)
			cancel()
		}
	}()

	log.Printf("client: SOCKS5 on %s", listenAddress)
	for {
		conn, err := listener.Accept()
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
