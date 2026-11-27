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
	Configured  bool           `json:"configured"`
	Node        string         `json:"node"`
	Status      string         `json:"status"`
	ConnectedAt int64          `json:"connectedAt"` // Unix milliseconds, 0 when not connected
	Address     string         `json:"address"`
	SystemProxy bool           `json:"systemProxy"`
	Route       string         `json:"route"`
	AutoConnect bool           `json:"autoConnect"`
	EnvProxy    bool           `json:"envProxy"`
	Backup      bool           `json:"backup"`
	PreferRelay bool           `json:"preferRelay"`
	Error       string         `json:"error"`
	Version     string         `json:"version"`
	Update      *tunnel.Update `json:"update"`      // a newer release, or nil
	UpdateKnown bool           `json:"updateKnown"` // whether a check has succeeded
	// The GitHub relay: whether this build can set one up, whether the saved one was set up here, and a setup in progress.
	RelayAvailable bool       `json:"relayAvailable"`
	RelayManaged   bool       `json:"relayManaged"`
	Relay          RelaySetup `json:"relay"`
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

	mutex         sync.Mutex
	state         State
	cancel        context.CancelFunc
	stopped       chan struct{}
	logs          []string
	relayCancel   context.CancelFunc // set while a relay setup runs
	settingsMutex sync.Mutex         // serializes read-change-save of the settings file
}

func NewApp() *App {
	return &App{state: State{Status: statusDisconnected, Version: tunnel.Version, RelayAvailable: relayRepo != "" && githubClientID != ""}}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.startTray()
	a.watchSessionEnd()
	if updateRepo != "" {
		go a.watchForUpdates()
	}
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
	a.state.PreferRelay = settings.PreferRelay
	a.state.RelayManaged = settings.RelaySpace != ""
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
	if err := a.updateSettings(func(s *tunnel.Settings) { s.AutoConnect = enabled }); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			a.setError(err.Error())
		}
		return
	}
	a.mutex.Lock()
	a.state.AutoConnect = enabled
	a.mutex.Unlock()
	a.publish()
}

// watchForUpdates looks for a newer release now and every few hours; a failed check changes nothing.
func (a *App) watchForUpdates() {
	for {
		if a.ctx.Err() != nil {
			return
		}
		if update, err := tunnel.CheckForUpdate(a.ctx, updateRepo); err != nil {
			if a.ctx.Err() != nil {
				return
			}
			log.Printf("update check: %v", err)
		} else {
			a.mutex.Lock()
			a.state.Update, a.state.UpdateKnown = update, true
			a.mutex.Unlock()
			a.publish()
		}
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(6 * time.Hour):
		}
	}
}

// OpenUpdate opens the newer release's page in the browser.
func (a *App) OpenUpdate() {
	a.mutex.Lock()
	update := a.state.Update
	a.mutex.Unlock()
	if update != nil && strings.HasPrefix(update.Page, "https://github.com/") {
		runtime.BrowserOpenURL(a.ctx, update.Page)
	}
}

// SetPreferRelay saves whether connecting tries the relay before Google. It applies from the next connect.
func (a *App) SetPreferRelay(enabled bool) {
	if err := a.updateSettings(func(s *tunnel.Settings) { s.PreferRelay = enabled }); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			a.setError(err.Error())
		}
		return
	}
	a.mutex.Lock()
	a.state.PreferRelay = enabled
	a.mutex.Unlock()
	a.publish()
}

// SetEnvProxy saves whether connecting also sets HTTPS_PROXY and friends for command-line tools. It applies from the next connect.
func (a *App) SetEnvProxy(enabled bool) {
	if err := a.updateSettings(func(s *tunnel.Settings) { s.SetEnvProxy = enabled }); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			a.setError(err.Error())
		}
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
	a.settingsMutex.Lock()
	settings, _ := tunnel.LoadSettings(a.settingsPath)
	a.settingsMutex.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := tunnel.Setup(ctx, &settings, mailbox, node); err != nil {
		return err.Error()
	}
	fromSetupCode := strings.HasPrefix(strings.TrimSpace(mailbox), "mtc://")
	a.settingsMutex.Lock()
	current, _ := tunnel.LoadSettings(a.settingsPath)
	current.DeploymentID, current.AuthKey, current.Node = settings.DeploymentID, settings.AuthKey, settings.Node
	if fromSetupCode {
		current.Backup = settings.Backup
	}
	err := tunnel.SaveSettings(a.settingsPath, current)
	a.settingsMutex.Unlock()
	if err != nil {
		return err.Error()
	}
	a.mutex.Lock()
	a.state.Configured = true
	a.state.Node = nodeLabel(current.Node)
	a.state.Backup = current.Backup != ""
	route := current.Route()
	a.state.Route = route.FrontDomain + " via " + route.GoogleIP
	a.mutex.Unlock()
	a.publish()
	return ""
}

// SetupCode is the saved settings in one string, to set up another device; it is empty before setup.
func (a *App) SetupCode() string {
	a.settingsMutex.Lock()
	settings, err := tunnel.LoadSettings(a.settingsPath)
	a.settingsMutex.Unlock()
	if err != nil || settings.DeploymentID == "" {
		return ""
	}
	return settings.SetupCode()
}

// SaveBackup checks a relay's node string and saves it as the backup route; an empty string removes the backup.
// It returns an error message or "".
func (a *App) SaveBackup(nodeString string) string {
	a.settingsMutex.Lock()
	_, err := tunnel.LoadSettings(a.settingsPath)
	a.settingsMutex.Unlock()
	if err != nil {
		return "Enter your settings first."
	}
	backup := strings.TrimSpace(nodeString)
	if backup != "" {
		route, ok := tunnel.RelayRoute(backup)
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
	if err := a.updateSettings(func(s *tunnel.Settings) {
		s.Backup = backup
		if backup == "" {
			s.PreferRelay = false
		}
	}); err != nil {
		return err.Error()
	}
	a.mutex.Lock()
	a.state.Backup = backup != ""
	if backup == "" {
		a.state.PreferRelay = false
	}
	a.mutex.Unlock()
	a.publish()
	return ""
}

// scansGoogle reports whether connecting checks Google's route and scans for a new one when it fails.
// Only a relay that answered and is preferred spares the wait, since the client starts on it.
func scansGoogle(relayUp, preferRelay bool) bool {
	return !(relayUp && preferRelay)
}

// routeOrder lists the routes the client tries in turn.
// The relay goes first when it answered and is preferred or Google has no working route; a relay that did not answer still comes last, since it may be started later.
func routeOrder(google, relay tunnel.ClientRoute, hasRelay, relayUp, preferRelay, googleWorks bool) []tunnel.ClientRoute {
	if !hasRelay {
		return []tunnel.ClientRoute{google}
	}
	if relayUp && (preferRelay || !googleWorks) {
		return []tunnel.ClientRoute{relay, google}
	}
	return []tunnel.ClientRoute{google, relay}
}

// relayAnswers pings the relay's mailbox, giving it a few seconds since a codespace may be waking up.
func relayAnswers(ctx context.Context, relay tunnel.ClientRoute) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := tunnel.NewMailbox(relay.Own).Ping(ctx)
	if err != nil {
		log.Printf("relay check: %v", err)
	}
	return err == nil
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

	tunnel.ResetTrafficTotals()
	tunnel.ResetLatency()
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

		// The relay counts only when it answers now, and Google's route is scanned for whenever the saved one fails.
		// Trusting a relay that was off used to leave the client with nothing but a stale Google route that no scan ever replaced.
		backup, hasBackup := settings.BackupRoute()
		relayUp := hasBackup && relayAnswers(ctx, backup)
		// A relay this app set up sleeps after a few idle hours, and waking it spends the user's codespace hours.
		// So it is woken only when needed: before connecting when preferred, when Google has no route, or when the client moves to it.
		canWake := hasBackup && settings.RelaySpace != ""
		wake := func() bool {
			if err := wakeRelay(ctx, settings); err != nil {
				log.Printf("relay wake: %v", err)
				return false
			}
			return relayAnswers(ctx, backup)
		}
		if canWake && !relayUp && settings.PreferRelay {
			relayUp = wake()
		}
		var googleWorks bool
		if !scansGoogle(relayUp, settings.PreferRelay) {
			// The relay goes first, so a failing Google route is only noted; the next connect without the relay scans for a new one.
			if _, googleWorks = tunnel.TestRoute(ctx, settings.OwnMailbox(), settings.Route()); !googleWorks {
				log.Printf("route %s via %s does not work here; Google stays next in line until a later scan", settings.Route().FrontDomain, settings.Route().GoogleIP)
			}
		} else {
			googleWorks = tunnel.EnsureRoute(ctx, a.settingsPath, &settings, a.scanProgress)
		}
		if canWake && !googleWorks && !relayUp {
			relayUp = wake()
		}
		if !googleWorks && !relayUp {
			if ctx.Err() == nil {
				a.setError("No working route to Google was found on this network.")
			}
			return
		}
		googleRoute := settings.Route()
		google := tunnel.ClientRoute{Name: "Google", Own: settings.OwnMailbox(), Node: node}
		routes := routeOrder(google, backup, hasBackup, relayUp, settings.PreferRelay, googleWorks)

		listener, err := net.Listen("tcp", "127.0.0.1:1080")
		if err != nil {
			if listener, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
				a.setError(err.Error())
				return
			}
		}
		address := listener.Addr().String()
		a.settingsMutex.Lock()
		fresh, err := tunnel.LoadSettings(a.settingsPath)
		if err != nil {
			fresh = settings
		}
		systemProxy := tunnel.PointSystemAt(a.settingsPath, &fresh, address)
		a.settingsMutex.Unlock()
		a.mutex.Lock()
		a.state.Address, a.state.SystemProxy = address, systemProxy
		a.mutex.Unlock()

		tunnel.RunClient(ctx, tunnel.ClientOptions{
			Routes:   routes,
			Listener: listener,
			OnStatus: a.setStatus,
			OnRoute: func(name string) {
				a.showRoute(name, googleRoute)
				if name == "Relay" && canWake {
					go func() {
						if !relayAnswers(ctx, backup) {
							wake()
						}
					}()
				}
			},
		})
		a.settingsMutex.Lock()
		if saved, err := tunnel.LoadSettings(a.settingsPath); err == nil {
			tunnel.RestoreLeftoverProxy(a.settingsPath, &saved)
		}
		a.settingsMutex.Unlock()
	}()
	return ""
}

// FindRoute scans this network for the fastest working route to Google and saves it. It returns a message for the user.
func (a *App) FindRoute() string {
	a.settingsMutex.Lock()
	settings, err := tunnel.LoadSettings(a.settingsPath)
	a.settingsMutex.Unlock()
	if err != nil {
		return "Enter your settings first."
	}
	routes := tunnel.ScanRoutes(context.Background(), settings.OwnMailbox(), a.scanProgress)
	if len(routes) == 0 {
		return "No working route to Google was found on this network."
	}
	best := routes[0]
	if err := a.updateSettings(func(s *tunnel.Settings) {
		s.GoogleIP, s.FrontDomain = best.GoogleIP, best.FrontDomain
	}); err != nil {
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
	a.CancelRelaySetup()
	a.Disconnect()
	a.settingsMutex.Lock()
	err := os.Remove(a.settingsPath)
	a.settingsMutex.Unlock()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		a.setError(err.Error())
		return
	}
	a.mutex.Lock()
	a.state.Configured = false
	a.state.Node = ""
	a.state.Route = ""
	a.state.AutoConnect = false
	a.state.EnvProxy = false
	a.state.Backup = false
	a.state.PreferRelay = false
	a.state.RelayManaged = false
	a.state.Relay = RelaySetup{}
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
