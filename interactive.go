package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lucyna-aqu/mailbox-tunnel/tunnel"
)

// runInteractive is what a double-click on the console build starts: it asks for the settings once,
// saves them next to the executable, connects, and points the system proxy at the tunnel until the window is closed.
func runInteractive() {
	if err := interactiveSession(); err != nil {
		fmt.Printf("\nError: %v\n", err)
	}
	fmt.Print("\nPress Enter to exit.")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func interactiveSession() error {
	if file, err := tunnel.OpenLogFile(); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, file))
	}
	path, err := tunnel.SettingsPath()
	if err != nil {
		return err
	}
	settings, err := tunnel.LoadSettings(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	tunnel.RestoreLeftoverProxy(path, &settings)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if settings.DeploymentID == "" {
		fmt.Println("mailbox-tunnel setup. Your answers are saved in " + tunnel.SettingsFileName + " next to this program.")
		if settings, err = askSettings(ctx); err != nil {
			return err
		}
		if err := tunnel.SaveSettings(path, settings); err != nil {
			return err
		}
	}
	node, err := tunnel.ParseNodeInfo(settings.Node)
	if err != nil {
		return fmt.Errorf("node in %s: %w", tunnel.SettingsFileName, err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:1080")
	if err != nil {
		if listener, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
	}
	address := listener.Addr().String()

	if previous, err := tunnel.ReadSystemProxy(); err == nil {
		settings.PreviousProxy = &previous
		if err := tunnel.SaveSettings(path, settings); err != nil {
			return err
		}
		if err := tunnel.WriteSystemProxy(tunnel.TunnelProxySettings(address)); err != nil {
			return fmt.Errorf("setting the system proxy: %w", err)
		}
		defer func() {
			tunnel.RestoreLeftoverProxy(path, &settings)
			fmt.Println("System proxy restored.")
		}()
		fmt.Printf("System proxy set to %s. Close this window or press Ctrl+C to disconnect.\n", address)
	} else {
		fmt.Printf("Point your applications at %s (SOCKS5 or HTTP proxy).\n", address)
	}

	tunnel.RunClient(ctx, tunnel.ClientOptions{Own: settings.OwnMailbox(), Node: node, Listener: listener})
	return nil
}

func askSettings(ctx context.Context) (tunnel.Settings, error) {
	input := bufio.NewReader(os.Stdin)
	ask := func(question string) string {
		fmt.Print(question)
		answer, _ := input.ReadString('\n')
		return strings.Trim(strings.TrimSpace(answer), `"'`)
	}
	for ctx.Err() == nil {
		settings := tunnel.Settings{
			DeploymentID: ask("\nDeployment ID of your Apps Script: "),
			AuthKey:      ask("AUTH_KEY from your Code.gs: "),
		}
		fmt.Println("Checking your mailbox...")
		pingCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := tunnel.NewMailbox(settings.OwnMailbox()).Ping(pingCtx)
		cancel()
		if err != nil {
			fmt.Printf("That did not work (%v). Please check both values and try again.\n", err)
			continue
		}
		fmt.Println("Mailbox OK.")
		for ctx.Err() == nil {
			settings.Node = ask("Node string (starts with mtn://): ")
			if _, err := tunnel.ParseNodeInfo(settings.Node); err == nil {
				return settings, nil
			}
			fmt.Println("That is not a valid node string.")
		}
	}
	return tunnel.Settings{}, ctx.Err()
}
