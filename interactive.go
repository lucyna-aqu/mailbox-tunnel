package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const settingsFileName = "mailbox-tunnel.json"

// ProxySettings is the part of the Windows proxy configuration this program changes.
type ProxySettings struct {
	Enable   uint32 `json:"enable"`
	Server   string `json:"server"`
	Override string `json:"override"`
}

type savedSettings struct {
	DeploymentID  string         `json:"deployment_id"`
	AuthKey       string         `json:"auth_key"`
	Node          string         `json:"node"`
	PreviousProxy *ProxySettings `json:"previous_proxy,omitempty"`
}

// runInteractive is what a double-click starts: it asks for the settings once, saves them next to the executable,
// connects, and points the system proxy at the tunnel until the window is closed.
func runInteractive() {
	if err := interactiveSession(); err != nil {
		fmt.Printf("\nError: %v\n", err)
	}
	fmt.Print("\nPress Enter to exit.")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func interactiveSession() error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	settings, err := loadSettings(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if settings.PreviousProxy != nil {
		// The last run ended without restoring the proxy; put it back before anything else.
		writeSystemProxy(*settings.PreviousProxy)
		settings.PreviousProxy = nil
		saveSettings(path, settings)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if settings.DeploymentID == "" {
		fmt.Println("mailbox-tunnel setup. Your answers are saved in " + settingsFileName + " next to this program.")
		if settings, err = askSettings(ctx); err != nil {
			return err
		}
		if err := saveSettings(path, settings); err != nil {
			return err
		}
	}
	node, err := ParseNodeInfo(settings.Node)
	if err != nil {
		return fmt.Errorf("node in %s: %w", settingsFileName, err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:1080")
	if err != nil {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
	}
	address := listener.Addr().String()

	if previous, err := readSystemProxy(); err == nil {
		settings.PreviousProxy = &previous
		if err := saveSettings(path, settings); err != nil {
			return err
		}
		if err := writeSystemProxy(ProxySettings{Enable: 1, Server: address, Override: "<local>;localhost;127.*;10.*;192.168.*"}); err != nil {
			return fmt.Errorf("setting the system proxy: %w", err)
		}
		defer func() {
			writeSystemProxy(previous)
			settings.PreviousProxy = nil
			saveSettings(path, settings)
			fmt.Println("System proxy restored.")
		}()
		fmt.Printf("System proxy set to %s. Close this window or press Ctrl+C to disconnect.\n", address)
	} else {
		fmt.Printf("Point your applications at %s (SOCKS5 or HTTP proxy).\n", address)
	}

	own := MailboxConfig{DeploymentID: settings.DeploymentID, AuthKey: settings.AuthKey, GoogleIP: defaultGoogleIP, FrontDomain: "www.google.com"}
	runClient(ctx, own, node, listener)
	return nil
}

func askSettings(ctx context.Context) (savedSettings, error) {
	input := bufio.NewReader(os.Stdin)
	ask := func(question string) string {
		fmt.Print(question)
		answer, _ := input.ReadString('\n')
		return strings.Trim(strings.TrimSpace(answer), `"'`)
	}
	for ctx.Err() == nil {
		settings := savedSettings{
			DeploymentID: ask("\nDeployment ID of your Apps Script: "),
			AuthKey:      ask("AUTH_KEY from your Code.gs: "),
		}
		fmt.Println("Checking your mailbox...")
		mailbox := NewMailbox(MailboxConfig{DeploymentID: settings.DeploymentID, AuthKey: settings.AuthKey, GoogleIP: defaultGoogleIP, FrontDomain: "www.google.com"})
		pingCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := mailbox.Ping(pingCtx)
		cancel()
		if err != nil {
			fmt.Printf("That did not work (%v). Please check both values and try again.\n", err)
			continue
		}
		fmt.Println("Mailbox OK.")
		for ctx.Err() == nil {
			settings.Node = ask("Node string (starts with mtn://): ")
			if _, err := ParseNodeInfo(settings.Node); err == nil {
				return settings, nil
			}
			fmt.Println("That is not a valid node string.")
		}
	}
	return savedSettings{}, ctx.Err()
}

func settingsPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(executable), settingsFileName), nil
}

func loadSettings(path string) (savedSettings, error) {
	var settings savedSettings
	content, err := os.ReadFile(path)
	if err != nil {
		return settings, err
	}
	return settings, json.Unmarshal(content, &settings)
}

func saveSettings(path string, settings savedSettings) error {
	content, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		log.Printf("saving %s: %v", path, err)
		return err
	}
	return nil
}
