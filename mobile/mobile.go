// Package mobile is the Android binding of mailbox-tunnel, built with gomobile bind.
// The Android VpnService hands over its TUN file descriptor; tun2socks turns the device's traffic into
// SOCKS5 connections to the tunnel client, and fake DNS keeps lookups off the tunnel.
package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lucyna-aqu/mailbox-tunnel/tunnel"
	"github.com/xjasonlyu/tun2socks/v2/core"
	"github.com/xjasonlyu/tun2socks/v2/core/device/fdbased"
	"github.com/xjasonlyu/tun2socks/v2/proxy/socks5"
	t2s "github.com/xjasonlyu/tun2socks/v2/tunnel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const (
	statusDisconnected = "disconnected"
	statusScanning     = "scanning"
	logLimit           = 300
	stopWait           = 3 * time.Second
)

// state mirrors the desktop app's State so the same web frontend can render it.
type state struct {
	Configured  bool   `json:"configured"`
	Node        string `json:"node"`
	Status      string `json:"status"`
	ConnectedAt int64  `json:"connectedAt"`
	Address     string `json:"address"`
	SystemProxy bool   `json:"systemProxy"`
	Route       string `json:"route"`
	AutoConnect bool   `json:"autoConnect"`
	EnvProxy    bool   `json:"envProxy"`
	Error       string `json:"error"`
}

var controller struct {
	sync.Mutex
	state   state
	cancel  context.CancelFunc
	stopped chan struct{}
	logs    []string
}

type logSink struct{}

func (logSink) Write(line []byte) (int, error) {
	controller.Lock()
	controller.logs = append(controller.logs, strings.TrimRight(string(line), "\n"))
	if len(controller.logs) > logLimit {
		controller.logs = controller.logs[len(controller.logs)-logLimit:]
	}
	controller.Unlock()
	return len(line), nil
}

var setupOnce sync.Once

// SetDataDir sets up the app's private files directory and the log. Only the first call has an effect.
func SetDataDir(dir string) {
	setupOnce.Do(func() {
		tunnel.SetDataDir(dir)
		sink := io.Writer(logSink{})
		if file, err := tunnel.OpenLogFile(); err == nil {
			sink = io.MultiWriter(sink, file)
		}
		log.SetOutput(sink)
		settings, err := tunnel.LoadSettings(settingsPath())
		controller.Lock()
		controller.state.Status = statusDisconnected
		if err == nil {
			applySettings(settings)
		}
		controller.Unlock()
	})
}

func settingsPath() string {
	path, _ := tunnel.SettingsPath()
	return path
}

// applySettings copies saved settings into the state. The caller holds the lock.
func applySettings(settings tunnel.Settings) {
	controller.state.Configured = settings.DeploymentID != ""
	controller.state.AutoConnect = settings.AutoConnect
	controller.state.Node = ""
	if node, err := tunnel.ParseNodeInfo(settings.Node); err == nil {
		controller.state.Node = fmt.Sprintf("%x", node.PublicKey[:4])
	}
	route := settings.Route()
	controller.state.Route = route.FrontDomain + " via " + route.GoogleIP
}

// State returns the connection state as JSON.
func State() string {
	controller.Lock()
	defer controller.Unlock()
	content, _ := json.Marshal(controller.state)
	return string(content)
}

// Traffic returns the relayed totals and the current round trip to Google as JSON.
func Traffic() string {
	sent, received := tunnel.TrafficTotals()
	content, _ := json.Marshal(map[string]int64{"sent": sent, "received": received, "latencyMs": tunnel.Latency().Milliseconds()})
	return string(content)
}

// Logs returns the recent log lines, one per line.
func Logs() string {
	controller.Lock()
	defer controller.Unlock()
	return strings.Join(controller.logs, "\n")
}

// SaveSettings takes a setup code, or a mailbox address and a node string, checks them and saves them.
// It returns an error message or "".
func SaveSettings(mailbox, node string) string {
	settings, _ := tunnel.LoadSettings(settingsPath())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := tunnel.Setup(ctx, &settings, mailbox, node); err != nil {
		return err.Error()
	}
	if err := tunnel.SaveSettings(settingsPath(), settings); err != nil {
		return err.Error()
	}
	controller.Lock()
	applySettings(settings)
	controller.Unlock()
	return ""
}

// SetupCode is the saved settings in one string, to set up another device; it is empty before setup.
func SetupCode() string {
	settings, err := tunnel.LoadSettings(settingsPath())
	if err != nil || settings.DeploymentID == "" {
		return ""
	}
	return settings.SetupCode()
}

// ResetSettings deletes the saved settings.
func ResetSettings() {
	Stop()
	if err := os.Remove(settingsPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		setError(err.Error())
		return
	}
	controller.Lock()
	controller.state = state{Status: statusDisconnected}
	controller.Unlock()
}

// SetAutoConnect saves whether the app connects as soon as it opens.
func SetAutoConnect(enabled bool) {
	settings, err := tunnel.LoadSettings(settingsPath())
	if err != nil {
		return
	}
	settings.AutoConnect = enabled
	if tunnel.SaveSettings(settingsPath(), settings) == nil {
		controller.Lock()
		controller.state.AutoConnect = enabled
		controller.Unlock()
	}
}

// FindRoute scans this network for the fastest working route to Google and saves it. It returns a message for the user.
func FindRoute() string {
	settings, err := tunnel.LoadSettings(settingsPath())
	if err != nil {
		return "Enter your settings first."
	}
	routes := tunnel.ScanRoutes(context.Background(), settings.OwnMailbox(), nil)
	if len(routes) == 0 {
		return "No working route to Google was found on this network."
	}
	best := routes[0]
	settings.GoogleIP, settings.FrontDomain = best.GoogleIP, best.FrontDomain
	if err := tunnel.SaveSettings(settingsPath(), settings); err != nil {
		return err.Error()
	}
	controller.Lock()
	applySettings(settings)
	controller.Unlock()
	return fmt.Sprintf("Using %s via %s (%d ms). Reconnect to use it.", best.FrontDomain, best.GoogleIP, best.Latency.Milliseconds())
}

// Start connects and routes the VPN interface tunFd through the tunnel. It returns an error message or "".
func Start(tunFd int) string {
	Stop()
	// The VPN stays up for as long as the file descriptor is open, so every path below must close it.
	device, err := fdbased.Open(strconv.Itoa(tunFd), 1500, 0)
	if err != nil {
		os.NewFile(uintptr(tunFd), "tun").Close()
		return err.Error()
	}
	settings, err := tunnel.LoadSettings(settingsPath())
	if err != nil {
		device.Close()
		return "Enter your settings first."
	}
	node, err := tunnel.ParseNodeInfo(settings.Node)
	if err != nil {
		device.Close()
		return "That is not a valid node string."
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	controller.Lock()
	controller.cancel, controller.stopped = cancel, stopped
	controller.state.Status, controller.state.Error = statusScanning, ""
	controller.Unlock()

	go func() {
		defer close(stopped)
		defer markStopped(stopped)
		defer device.Close()

		if !tunnel.EnsureRoute(ctx, settingsPath(), &settings, nil) {
			if ctx.Err() == nil {
				setError("No working route to Google was found on this network.")
			}
			return
		}
		controller.Lock()
		applySettings(settings)
		controller.Unlock()

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			setError(err.Error())
			return
		}
		proxy, err := socks5.New(listener.Addr().String(), "", "")
		if err != nil {
			listener.Close()
			setError(err.Error())
			return
		}
		t2s.T().SetProxy(proxy)
		t2s.T().SetUDPTimeout(30 * time.Second)
		netstack, err := core.CreateStack(&core.Config{LinkEndpoint: device, TransportHandler: t2s.T()})
		if err != nil {
			listener.Close()
			setError(err.Error())
			return
		}
		defer closeStack(device, netstack)
		controller.Lock()
		controller.state.Address = "VPN"
		controller.Unlock()

		tunnel.RunClient(ctx, tunnel.ClientOptions{
			Own:      settings.OwnMailbox(),
			Node:     node,
			Listener: listener,
			OnStatus: setStatus,
			FakeDNS:  tunnel.NewFakeDNS(),
		})
	}()
	return ""
}

// closeStack closes the VPN interface and the network stack.
// The stack can take long to drain its connections, so it is not waited on for more than a moment.
func closeStack(device interface{ Close() }, netstack *stack.Stack) {
	device.Close()
	netstack.Close()
	drained := make(chan struct{})
	go func() {
		netstack.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(stopWait):
		log.Printf("stop: the network stack is still closing")
	}
}

// markStopped resets the state, unless a newer connection has already taken over.
func markStopped(stopped chan struct{}) {
	controller.Lock()
	defer controller.Unlock()
	if controller.stopped != stopped {
		return
	}
	controller.cancel, controller.stopped = nil, nil
	controller.state.Status, controller.state.Address, controller.state.ConnectedAt = statusDisconnected, "", 0
}

// Stop disconnects. The state is reset at once even if the tunnel takes longer to shut down.
func Stop() {
	controller.Lock()
	cancel, stopped := controller.cancel, controller.stopped
	controller.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(stopWait):
		log.Printf("stop: timed out waiting for the tunnel")
	}
	markStopped(stopped)
}

func setStatus(status tunnel.Status) {
	controller.Lock()
	defer controller.Unlock()
	if controller.cancel == nil {
		return
	}
	controller.state.Status = string(status)
	controller.state.ConnectedAt = 0
	if status == tunnel.StatusConnected {
		controller.state.ConnectedAt = time.Now().UnixMilli()
	}
}

func setError(message string) {
	controller.Lock()
	controller.state.Error = message
	controller.Unlock()
}
