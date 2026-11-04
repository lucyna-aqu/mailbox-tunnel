// Command mailbox-tunnel tunnels TCP through a Google Apps Script web app used as a mailbox.
//
//	client (SOCKS5/HTTP proxy) <-> user's Apps Script cache <-> node <-> internet
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
	"syscall"
	"time"

	"github.com/lucyna-aqu/mailbox-tunnel/tunnel"
)

func main() {
	if len(os.Args) == 1 {
		runInteractive()
		return
	}
	mode := flag.String("mode", "", "client, node, relay, scan or keygen")
	deploymentID := flag.String("deployment", os.Getenv("MAILBOX_DEPLOYMENT_ID"), "your Apps Script deployment ID (or MAILBOX_DEPLOYMENT_ID)")
	authKey := flag.String("key", os.Getenv("MAILBOX_AUTH_KEY"), "AUTH_KEY from your Code.gs (or MAILBOX_AUTH_KEY)")
	nodeString := flag.String("node", os.Getenv("MAILBOX_NODE"), "client: the node to use, mtn://... (or MAILBOX_NODE)")
	nodeKey := flag.String("node-key", os.Getenv("MAILBOX_NODE_KEY"), "node: private key from -mode keygen (or MAILBOX_NODE_KEY)")
	listen := flag.String("listen", "127.0.0.1:1080", "client: SOCKS5 and HTTP proxy listen address; relay: mailbox listen address")
	statusListen := flag.String("status-listen", os.Getenv("STATUS_LISTEN"), "node: serve a plain-text status page on this address, e.g. :7860 (or STATUS_LISTEN)")
	googleIP := flag.String("google-ip", "", "Google IP to dial instead of script.google.com (default "+tunnel.DefaultGoogleIP+" for the client, direct for the node)")
	frontDomain := flag.String("front-domain", tunnel.DefaultFrontDomain, "TLS SNI shown to the network when fronting")
	duration := flag.Duration("duration", 0, "stop after this long (0 = until interrupted)")
	handover := flag.Duration("handover", 0, "node: with -duration, take no new registrations this long before stopping, so clients move to the next node")
	flag.IntVar(&tunnel.TestDropEvery, "test-drop-every", 0, "testing only: skip the first put of every Nth batch")
	flag.BoolVar(&tunnel.Trace, "trace", false, "testing only: log the timing of every put, wait and backlog stall")
	flag.Parse()
	if tunnel.Trace {
		log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	}

	if *mode == "keygen" {
		privateKey, publicKey, err := tunnel.GenerateNodeKey()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("node key (keep secret): %s\npublic key:             %s\n", privateKey, publicKey)
		return
	}
	relay := *mode == "relay"
	if relay {
		// A relay serves its own mailbox and runs a node on it, for a server such as a GitHub codespace.
		// Its deployment is the public host name that clients reach the mailbox at.
		if *deploymentID == "" {
			*deploymentID = codespaceHost(*listen)
		}
		if *deploymentID == "" || *authKey == "" {
			log.Fatal("relay: set -deployment to this server's public host name, and -key")
		}
		listener, err := net.Listen("tcp", *listen)
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			log.Fatal(http.Serve(listener, tunnel.NewMailboxServer(*authKey)))
		}()
		*mode = "node"
	}
	if (*mode != "client" && *mode != "node" && *mode != "scan") || *deploymentID == "" || *authKey == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *googleIP == "" && *mode == "client" {
		*googleIP = tunnel.DefaultGoogleIP
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	config := tunnel.MailboxConfig{DeploymentID: *deploymentID, AuthKey: *authKey, GoogleIP: *googleIP, FrontDomain: *frontDomain}
	if *mode == "scan" {
		routes := tunnel.ScanRoutes(ctx, config, func(done, total int) {
			fmt.Printf("\rtesting %d/%d", done, total)
		})
		fmt.Println()
		if len(routes) == 0 {
			log.Fatal("no working route to Google found on this network")
		}
		for _, route := range routes {
			fmt.Printf("%-16s %-22s %s\n", route.GoogleIP, route.FrontDomain, route.Latency.Round(time.Millisecond))
		}
		return
	}
	started := time.Now()
	err := tunnel.NewMailbox(config).Ping(ctx)
	// A relay's public port may open a little later, for example once a codespace port is made public.
	for relay && err != nil && ctx.Err() == nil && time.Since(started) < 5*time.Minute {
		log.Printf("relay: public address not reachable yet (%v), retrying", err)
		time.Sleep(5 * time.Second)
		err = tunnel.NewMailbox(config).Ping(ctx)
	}
	if err != nil {
		log.Fatalf("mailbox unreachable: %v", err)
	}
	log.Printf("mailbox OK (%s)", time.Since(started).Round(time.Millisecond))

	if *mode == "node" {
		if err := tunnel.RunNode(ctx, config, *nodeKey, *statusListen, *handover); err != nil {
			log.Fatalf("%v (create a key with -mode keygen)", err)
		}
		return
	}
	node, err := tunnel.ParseNodeInfo(*nodeString)
	if err != nil {
		log.Fatalf("-node: %v", err)
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	tunnel.RunClient(ctx, tunnel.ClientOptions{Own: config, Node: node, Listener: listener})
}

// codespaceHost is the public host name GitHub Codespaces gives the listen port, or "" outside a codespace.
func codespaceHost(listen string) string {
	name, domain := os.Getenv("CODESPACE_NAME"), os.Getenv("GITHUB_CODESPACES_PORT_FORWARDING_DOMAIN")
	_, port, err := net.SplitHostPort(listen)
	if name == "" || domain == "" || err != nil {
		return ""
	}
	return name + "-" + port + "." + domain
}
