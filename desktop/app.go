package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/lucyna-aqu/mailbox-tunnel/tunnel"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	statusDisconnected = "disconnected"
	statusScanning     = "scanning"
	logLimit           = 300
)

// State is what the window shows.
type State struct {
	Configured  bool   `json:"configured"`
	Node        string `json:"node"`
	Status      string `json:"status"`
	ConnectedAt int64  `json:"connectedAt"` // Unix milliseconds, 0 when not connected
	Address     string `json:"address"`
	SystemProxy bool   `json:"systemProxy"`
	Route       string `json:"route"`
	AutoConnect bool   `json:"autoConnect"`
	EnvProxy    bool   `json:"envProxy"`
	Backup      bool   `json:"backup"`
	Error       string `json:"error"`
	Version     string `json:"version"`
}

// Traffic is the total relayed for local applications, plus the current round trip to Google.
type Traffic struct {
	Sent      int64 `json:"sent"`
	Received  int64 `json:"received"`
	LatencyMs int64 `json:"latencyMs"`
}

// nodeLabel is a short, stable name for a node: the start of its public key.
func nodeLabel(nodeString string) string {
	node, err := tunnel.ParseNodeInfo(nodeString)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", node.PublicKey[:4])
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
	return &App{state: State{Status: statusDisconnected, Version: tunnel.Version}}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.startTray()
	a.watchSessionEnd()
	sink := io.Writer(logSink{a})
	if file, err := tunnel.OpenLogFile(); err == nil {
		sink = io.MultiWriter(sink, file)
	}
	log.SetOutput(sink)
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
	a.state.Node = nodeLabel(settings.Node)
	a.state.AutoConnect = settings.AutoConnect
	a.state.EnvProxy = settings.SetEnvProxy
	a.state.Backup = settings.Backup != ""
	if a.state.Configured {
		route := settings.Route()
		a.state.Route = route.FrontDomain + " via " + route.GoogleIP
	}
	a.mutex.Unlock()
	if a.state.Configured && settings.AutoConnect {
		a.Connect()
	}
}

// SetAutoConnect saves whether the app connects as soon as it starts.
func (a *App) SetAutoConnect(enabled bool) {
	settings, err := tunnel.LoadSettings(a.settingsPath)
	if err != nil {
		return
	}
	settings.AutoConnect = enabled
	if err := tunnel.SaveSettings(a.settingsPath, settings); err != nil {
		a.setError(err.Error())
		return
	}
	a.mutex.Lock()
	a.state.AutoConnect = enabled
	a.mutex.Unlock()
	a.publish()
}

// SetEnvProxy saves whether connecting also sets HTTPS_PROXY and friends for command-line tools. It applies from the next connect.
func (a *App) SetEnvProxy(enabled bool) {
	settings, err := tunnel.LoadSettings(a.settingsPath)
	if err != nil {
		return
	}
	settings.SetEnvProxy = enabled
	if err := tunnel.SaveSettings(a.settingsPath, settings); err != nil {
		a.setError(err.Error())
		return
	}
	a.mutex.Lock()
	a.state.EnvProxy = enabled
	a.mutex.Unlock()
	a.publish()
}

// beforeClose hides the window to the tray, where the app keeps running.
// Only quitting from the tray closes it, after disconnecting, which also restores the system proxy.
func (a *App) beforeClose(ctx context.Context) bool {
	if !quitting.Load() {
		runtime.WindowHide(ctx)
		return true
	}
	a.Disconnect()
	systray.Quit()
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

func (a *App) Traffic() Traffic {
	sent, received := tunnel.TrafficTotals()
	return Traffic{Sent: sent, Received: received, LatencyMs: tunnel.Latency().Milliseconds()}
}

func (a *App) Logs() []string {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return append([]string{}, a.logs...)
}

// SaveSettings takes a setup code, or a mailbox address and a node string, checks them and saves them.
// It returns an error message or "".
func (a *App) SaveSettings(mailbox, node string) string {
	// Keep the saved route and options; only the answers change.
	settings, _ := tunnel.LoadSettings(a.settingsPath)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := tunnel.Setup(ctx, &settings, mailbox, node); err != nil {
		return err.Error()
	}
	if err := tunnel.SaveSettings(a.settingsPath, settings); err != nil {
		return err.Error()
	}
	a.mutex.Lock()
	a.state.Configured = true
	a.state.Node = nodeLabel(settings.Node)
	a.state.Backup = settings.Backup != ""
	a.mutex.Unlock()
	a.publish()
	return ""
}

// SetupCode is the saved settings in one string, to set up another device; it is empty before setup.
func (a *App) SetupCode() string {
	settings, err := tunnel.LoadSettings(a.settingsPath)
	if err != nil || settings.DeploymentID == "" {
		return ""
	}
	return settings.SetupCode()
}

// SaveBackup checks a relay's node string and saves it as the backup route; an empty string removes the backup.
// It returns an error message or "".
func (a *App) SaveBackup(nodeString string) string {
	settings, err := tunnel.LoadSettings(a.settingsPath)
	if err != nil {
		return "Enter your settings first."
	}
	settings.Backup = strings.TrimSpace(nodeString)
	if settings.Backup != "" {
		route, ok := tunnel.RelayRoute(settings.Backup)
		if !ok {
			return "That is not a relay's node string."
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := tunnel.NewMailbox(route.Own).Ping(ctx); err != nil {
			log.Printf("backup check: %v", err)
			return "The relay did not answer. Check that it is running and its port is public."
		}
	}
	if err := tunnel.SaveSettings(a.settingsPath, settings); err != nil {
		return err.Error()
	}
	a.mutex.Lock()
	a.state.Backup = settings.Backup != ""
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
		return "Enter your settings first."
	}
	node, err := tunnel.ParseNodeInfo(settings.Node)
	if err != nil {
		return "That is not a valid node string."
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	a.mutex.Lock()
	a.cancel, a.stopped = cancel, stopped
	a.state.Error = ""
	a.state.Status = statusScanning
	a.mutex.Unlock()
	a.publish()

	go func() {
		defer close(stopped)
		defer func() {
			a.mutex.Lock()
			a.cancel, a.stopped = nil, nil
			a.state.Status, a.state.Address, a.state.SystemProxy, a.state.ConnectedAt = statusDisconnected, "", false, 0
			a.mutex.Unlock()
			a.publish()
		}()

		backup, hasBackup := settings.BackupRoute()
		var googleWorks bool
		if hasBackup {
			// With a backup there is no need to wait for a scan: the client starts on the relay and Google stays next in line.
			_, googleWorks = tunnel.TestRoute(ctx, settings.OwnMailbox(), settings.Route())
		} else {
			googleWorks = tunnel.EnsureRoute(ctx, a.settingsPath, &settings, a.scanProgress)
		}
		if !googleWorks && !hasBackup {
			if ctx.Err() == nil {
				a.setError("No working route to Google was found on this network.")
			}
			return
		}
		googleRoute := settings.Route()
		routes := []tunnel.ClientRoute{{Name: "Google", Own: settings.OwnMailbox(), Node: node}}
		if hasBackup {
			routes = append(routes, backup)
			if !googleWorks {
				routes[0], routes[1] = routes[1], routes[0]
			}
		}

		listener, err := net.Listen("tcp", "127.0.0.1:1080")
		if err != nil {
			if listener, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
				a.setError(err.Error())
				return
			}
		}
		address := listener.Addr().String()
		systemProxy := tunnel.PointSystemAt(a.settingsPath, &settings, address)
		a.mutex.Lock()
		a.state.Address, a.state.SystemProxy = address, systemProxy
		a.mutex.Unlock()

		tunnel.RunClient(ctx, tunnel.ClientOptions{
			Routes:   routes,
			Listener: listener,
			OnStatus: a.setStatus,
			OnRoute:  func(name string) { a.showRoute(name, googleRoute) },
		})
		if saved, err := tunnel.LoadSettings(a.settingsPath); err == nil {
			tunnel.RestoreLeftoverProxy(a.settingsPath, &saved)
		}
	}()
	return ""
}

// FindRoute scans this network for the fastest working route to Google and saves it. It returns a message for the user.
func (a *App) FindRoute() string {
	settings, err := tunnel.LoadSettings(a.settingsPath)
	if err != nil {
		return "Enter your settings first."
	}
	routes := tunnel.ScanRoutes(context.Background(), settings.OwnMailbox(), a.scanProgress)
	if len(routes) == 0 {
		return "No working route to Google was found on this network."
	}
	best := routes[0]
	settings.GoogleIP, settings.FrontDomain = best.GoogleIP, best.FrontDomain
	if err := tunnel.SaveSettings(a.settingsPath, settings); err != nil {
		return err.Error()
	}
	a.setRoute(best)
	return fmt.Sprintf("Using %s via %s (%d ms). Reconnect to use it.", best.FrontDomain, best.GoogleIP, best.Latency.Milliseconds())
}

func (a *App) scanProgress(done, total int) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "scan", map[string]int{"done": done, "total": total})
	}
}

func (a *App) setRoute(route tunnel.Route) {
	a.mutex.Lock()
	a.state.Route = route.FrontDomain + " via " + route.GoogleIP
	a.mutex.Unlock()
	a.publish()
}

// showRoute shows which route the client is on.
func (a *App) showRoute(name string, google tunnel.Route) {
	if name == "Google" {
		a.setRoute(google)
		return
	}
	a.mutex.Lock()
	a.state.Route = "Backup relay"
	a.mutex.Unlock()
	a.publish()
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
	a.state.Configured, a.state.Node, a.state.Backup = false, "", false
	a.mutex.Unlock()
	a.publish()
}

func (a *App) setStatus(status tunnel.Status) {
	a.mutex.Lock()
	if a.cancel != nil {
		a.state.Status = string(status)
		a.state.ConnectedAt = 0
		if status == tunnel.StatusConnected {
			a.state.ConnectedAt = time.Now().UnixMilli()
		}
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
	state := a.State()
	a.updateTray(state)
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "state", state)
	}
}
