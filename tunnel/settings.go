package tunnel

import (
	"bytes"
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
	DeploymentID  string             `json:"deployment_id"`
	AuthKey       string             `json:"auth_key"`
	Node          string             `json:"node"`
	GoogleIP      string             `json:"google_ip,omitempty"`
	FrontDomain   string             `json:"front_domain,omitempty"`
	AutoConnect   bool               `json:"auto_connect,omitempty"`
	SetEnvProxy   bool               `json:"set_env_proxy,omitempty"`
	PreviousProxy *ProxySettings     `json:"previous_proxy,omitempty"`
	PreviousEnv   map[string]*string `json:"previous_env,omitempty"`
}

// proxyEnvironmentNames are the variables command-line tools such as Claude Code, git and npm read for a proxy.
var proxyEnvironmentNames = []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY"}

// PointSystemAt points the Windows proxy, and the proxy environment variables when SetEnvProxy is on, at address.
// The previous values are saved first so RestoreLeftoverProxy can put them back even after a crash.
// It returns whether the Windows proxy was set.
func PointSystemAt(path string, settings *Settings, address string) bool {
	previous, err := ReadSystemProxy()
	if err != nil {
		return false
	}
	settings.PreviousProxy = &previous
	if settings.SetEnvProxy {
		if environment, err := ReadProxyEnvironment(); err == nil {
			settings.PreviousEnv = environment
		}
	}
	if err := SaveSettings(path, *settings); err != nil {
		settings.PreviousProxy, settings.PreviousEnv = nil, nil
		return false
	}
	if settings.PreviousEnv != nil {
		proxyURL := "http://" + address
		noProxy := "localhost,127.0.0.1"
		if err := WriteProxyEnvironment(map[string]*string{"HTTPS_PROXY": &proxyURL, "HTTP_PROXY": &proxyURL, "NO_PROXY": &noProxy}); err != nil {
			log.Printf("setting proxy environment variables: %v", err)
		}
	}
	return WriteSystemProxy(TunnelProxySettings(address)) == nil
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

// dataDir overrides where settings and logs live; empty means next to the executable.
var dataDir string

// SetDataDir keeps settings and logs in dir, for platforms such as Android where the executable's folder is read-only.
func SetDataDir(dir string) {
	dataDir = dir
}

// SettingsPath is the settings file in the data directory, by default next to the running executable.
func SettingsPath() (string, error) {
	if dataDir != "" {
		return filepath.Join(dataDir, SettingsFileName), nil
	}
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
	// Editors such as older Notepad save UTF-8 with a byte order mark, which JSON does not allow.
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
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
	if settings.PreviousProxy == nil && settings.PreviousEnv == nil {
		return
	}
	if settings.PreviousProxy != nil {
		WriteSystemProxy(*settings.PreviousProxy)
	}
	if settings.PreviousEnv != nil {
		WriteProxyEnvironment(settings.PreviousEnv)
	}
	settings.PreviousProxy, settings.PreviousEnv = nil, nil
	SaveSettings(path, *settings)
}
