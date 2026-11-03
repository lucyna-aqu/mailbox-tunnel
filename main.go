// Command mailbox-tunnel tunnels TCP through a Google Apps Script web app used as a mailbox.
//
//	client (SOCKS5) <-> Apps Script cache <-> node <-> internet
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"time"
)

func main() {
	mode := flag.String("mode", "", "client or node")
	deploymentID := flag.String("deployment", os.Getenv("MAILBOX_DEPLOYMENT_ID"), "Apps Script deployment ID (or MAILBOX_DEPLOYMENT_ID)")
	authKey := flag.String("key", os.Getenv("MAILBOX_AUTH_KEY"), "AUTH_KEY from Code.gs (or MAILBOX_AUTH_KEY)")
	session := flag.String("session", "", "session name; client and node must match")
	listen := flag.String("listen", "127.0.0.1:1080", "SOCKS5 listen address (client)")
	googleIP := flag.String("google-ip", "216.239.38.120", "Google IP to dial instead of script.google.com; empty to connect directly")
	frontDomain := flag.String("front-domain", "www.google.com", "TLS SNI shown to the network when -google-ip is set")
	duration := flag.Duration("duration", 0, "stop after this long (0 = until interrupted)")
	flag.IntVar(&testDropEvery, "test-drop-every", 0, "testing only: skip the first put of every Nth batch to exercise retransmission")
	flag.Parse()

	if (*mode != "client" && *mode != "node") || *deploymentID == "" || *authKey == "" || *session == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	mailbox := NewMailbox(MailboxConfig{
		DeploymentID: *deploymentID,
		AuthKey:      *authKey,
		GoogleIP:     *googleIP,
		FrontDomain:  *frontDomain,
	})
	started := time.Now()
	if err := mailbox.Ping(ctx); err != nil {
		log.Fatalf("mailbox unreachable: %v", err)
	}
	log.Printf("mailbox OK (%s, google-ip %q)", time.Since(started).Round(time.Millisecond), *googleIP)

	if *mode == "node" {
		log.Printf("node: serving session %q", *session)
		serveNode(ctx, mailbox, *session)
		return
	}

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	client := NewClient(mailbox, *session)
	go client.run(ctx)
	go client.announce(ctx)
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	log.Printf("client: SOCKS5 on %s, session %q", *listen, *session)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go client.handle(conn)
	}
}
