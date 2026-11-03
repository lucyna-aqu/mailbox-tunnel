package main

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lucyna-aqu/mailbox-tunnel/tunnel"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	statusDisconnected = "disconnected"
	logLimit           = 300
)

// State is what the window shows.
type State struct {
	Configured  bool   `json:"configured"`
	Status      string `json:"status"`
	Address     string `json:"address"`
	SystemProxy bool   `json:"systemProxy"`
	Error       string `json:"error"`
}

// App connects the window to the tunnel.
type App struct {
	ctx          context.Context
	settingsPath string

	mutex   sync.Mutex
	state   State
	cancel  context.CancelFunc
	stopped chan struct{}
	logs    []string
}

func NewApp() *App {
	return &App{state: State{Status: statusDisconnected}}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	log.SetOutput(logSink{a})
	path, err := tunnel.SettingsPath()
	if err != nil {
		a.setError(err.Error())
		return
	}
	a.settingsPath = path
	settings, err := tunnel.LoadSettings(path)
	if err == nil {
		tunnel.RestoreLeftoverProxy(path, &settings)
	}
	a.mutex.Lock()
	a.state.Configured = settings.DeploymentID != ""
	a.mutex.Unlock()
}

// beforeClose disconnects, which also restores the system proxy, before the window closes.
func (a *App) beforeClose(context.Context) bool {
	a.Disconnect()
	return false
}

// logSink receives the tunnel's log lines and forwards them to the window.
// It is a separate type so Wails does not expose Write to the frontend.
type logSink struct {
	app *App
}

func (s logSink) Write(line []byte) (int, error) {
	a := s.app
	text := strings.TrimRight(string(line), "\n")
	a.mutex.Lock()
	a.logs = append(a.logs, text)
	if len(a.logs) > logLimit {
		a.logs = a.logs[len(a.logs)-logLimit:]
	}
	a.mutex.Unlock()
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "log", text)
	}
	return len(line), nil
}

func (a *App) State() State {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return a.state
}

func (a *App) Logs() []string {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return append([]string(nil), a.logs...)
}

// SaveSettings checks the mailbox and the node string, then saves them. It returns an error message or "".
func (a *App) SaveSettings(deploymentID, authKey, node string) string {
	settings := tunnel.Settings{
		DeploymentID: strings.TrimSpace(deploymentID),
		AuthKey:      strings.TrimSpace(authKey),
		Node:         strings.TrimSpace(node),
	}
	if _, err := tunnel.ParseNodeInfo(settings.Node); err != nil {
		return "کد node درست نیست."
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := tunnel.NewMailbox(settings.OwnMailbox()).Ping(ctx); err != nil {
		log.Printf("mailbox check: %v", err)
		return "صندوق پستی جواب نداد. شناسه و کلید را بررسی کنید."
	}
	if err := tunnel.SaveSettings(a.settingsPath, settings); err != nil {
		return err.Error()
	}
	a.mutex.Lock()
	a.state.Configured = true
	a.mutex.Unlock()
	a.publish()
	return ""
}

// Connect starts the tunnel and points the system proxy at it. It returns an error message or "".
func (a *App) Connect() string {
	a.mutex.Lock()
	running := a.cancel != nil
	a.mutex.Unlock()
	if running {
		return ""
	}
	settings, err := tunnel.LoadSettings(a.settingsPath)
	if err != nil {
		return "اول تنظیمات را وارد کنید."
	}
	node, err := tunnel.ParseNodeInfo(settings.Node)
	if err != nil {
		return "کد node درست نیست."
	}
	listener, err := net.Listen("tcp", "127.0.0.1:1080")
	if err != nil {
		if listener, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err.Error()
		}
	}
	address := listener.Addr().String()

	systemProxy := false
	if previous, err := tunnel.ReadSystemProxy(); err == nil {
		settings.PreviousProxy = &previous
		if err := tunnel.SaveSettings(a.settingsPath, settings); err == nil {
			systemProxy = tunnel.WriteSystemProxy(tunnel.TunnelProxySettings(address)) == nil
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	a.mutex.Lock()
	a.cancel, a.stopped = cancel, stopped
	a.state.Address, a.state.SystemProxy, a.state.Error = address, systemProxy, ""
	a.state.Status = string(tunnel.StatusConnecting)
	a.mutex.Unlock()
	a.publish()

	go func() {
		defer close(stopped)
		tunnel.RunClient(ctx, tunnel.ClientOptions{
			Own:      settings.OwnMailbox(),
			Node:     node,
			Listener: listener,
			OnStatus: a.setStatus,
		})
		if saved, err := tunnel.LoadSettings(a.settingsPath); err == nil {
			tunnel.RestoreLeftoverProxy(a.settingsPath, &saved)
		}
		a.mutex.Lock()
		a.cancel, a.stopped = nil, nil
		a.state.Status, a.state.Address, a.state.SystemProxy = statusDisconnected, "", false
		a.mutex.Unlock()
		a.publish()
	}()
	return ""
}

// Disconnect stops the tunnel and waits until the system proxy is restored.
func (a *App) Disconnect() {
	a.mutex.Lock()
	cancel, stopped := a.cancel, a.stopped
	a.mutex.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		log.Printf("disconnect: timed out waiting for the tunnel to stop")
	}
}

// ResetSettings deletes the saved settings so the setup form shows again.
func (a *App) ResetSettings() {
	a.Disconnect()
	if err := os.Remove(a.settingsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.setError(err.Error())
		return
	}
	a.mutex.Lock()
	a.state.Configured = false
	a.mutex.Unlock()
	a.publish()
}

func (a *App) setStatus(status tunnel.Status) {
	a.mutex.Lock()
	if a.cancel != nil {
		a.state.Status = string(status)
	}
	a.mutex.Unlock()
	a.publish()
}

func (a *App) setError(message string) {
	a.mutex.Lock()
	a.state.Error = message
	a.mutex.Unlock()
	a.publish()
}

func (a *App) publish() {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "state", a.State())
	}
}
