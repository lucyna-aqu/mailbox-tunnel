package tunnel

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

const (
	SettingsFileName = "mailbox-tunnel.json"
	LogFileName      = "mailbox-tunnel.log"
	logFileLimit     = 2 << 20
)

// OpenLogFile opens the log file next to the executable, starting it over once it passes 2 MB.
func OpenLogFile() (*os.File, error) {
	path, err := SettingsPath()
	if err != nil {
		return nil, err
	}
	path = filepath.Join(filepath.Dir(path), LogFileName)
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if info, err := os.Stat(path); err == nil && info.Size() > logFileLimit {
		flags |= os.O_TRUNC
	}
	return os.OpenFile(path, flags, 0o600)
}

// ProxySettings is the part of the Windows proxy configuration this program changes.
type ProxySettings struct {
	Enable   uint32 `json:"enable"`
	Server   string `json:"server"`
	Override string `json:"override"`
}

// TunnelProxySettings points the system proxy at a local tunnel address.
func TunnelProxySettings(address string) ProxySettings {
	return ProxySettings{Enable: 1, Server: address, Override: "<local>;localhost;127.*;10.*;192.168.*"}
}

// Settings are the user's saved answers, plus the system proxy to restore if a run ends without cleaning up.
type Settings struct {
	DeploymentID  string         `json:"deployment_id"`
	AuthKey       string         `json:"auth_key"`
	Node          string         `json:"node"`
	GoogleIP      string         `json:"google_ip,omitempty"`
	FrontDomain   string         `json:"front_domain,omitempty"`
	PreviousProxy *ProxySettings `json:"previous_proxy,omitempty"`
}

// Route is the saved route to Google, or the default one.
func (s Settings) Route() Route {
	if s.GoogleIP == "" || s.FrontDomain == "" {
		return Route{GoogleIP: DefaultGoogleIP, FrontDomain: DefaultFrontDomain}
	}
	return Route{GoogleIP: s.GoogleIP, FrontDomain: s.FrontDomain}
}

// OwnMailbox is the user's mailbox configuration, fronted through the saved route.
func (s Settings) OwnMailbox() MailboxConfig {
	route := s.Route()
	return MailboxConfig{DeploymentID: s.DeploymentID, AuthKey: s.AuthKey, GoogleIP: route.GoogleIP, FrontDomain: route.FrontDomain}
}

// EnsureRoute checks the saved route and, when it no longer works on this network, scans for a new one and saves it.
// It returns false when no working route was found.
func EnsureRoute(ctx context.Context, path string, settings *Settings, progress func(done, total int)) bool {
	if _, ok := TestRoute(ctx, settings.OwnMailbox(), settings.Route()); ok {
		return true
	}
	log.Printf("route %s via %s does not work here, scanning", settings.Route().FrontDomain, settings.Route().GoogleIP)
	routes := ScanRoutes(ctx, settings.OwnMailbox(), progress)
	if len(routes) == 0 {
		log.Printf("scan: no working route found")
		return false
	}
	best := routes[0]
	log.Printf("scan: using %s via %s (%s)", best.FrontDomain, best.GoogleIP, best.Latency.Round(time.Millisecond))
	settings.GoogleIP, settings.FrontDomain = best.GoogleIP, best.FrontDomain
	SaveSettings(path, *settings)
	return true
}

// SettingsPath is the settings file next to the running executable.
func SettingsPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(executable), SettingsFileName), nil
}

func LoadSettings(path string) (Settings, error) {
	var settings Settings
	content, err := os.ReadFile(path)
	if err != nil {
		return settings, err
	}
	return settings, json.Unmarshal(content, &settings)
}

func SaveSettings(path string, settings Settings) error {
	content, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o600)
}

// RestoreLeftoverProxy puts back a system proxy that a previous run left switched on.
func RestoreLeftoverProxy(path string, settings *Settings) {
	if settings.PreviousProxy == nil {
		return
	}
	WriteSystemProxy(*settings.PreviousProxy)
	settings.PreviousProxy = nil
	SaveSettings(path, *settings)
}
