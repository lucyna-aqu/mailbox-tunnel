package tunnel

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const SettingsFileName = "mailbox-tunnel.json"

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
	PreviousProxy *ProxySettings `json:"previous_proxy,omitempty"`
}

// OwnMailbox is the user's mailbox configuration, fronted through Google.
func (s Settings) OwnMailbox() MailboxConfig {
	return MailboxConfig{DeploymentID: s.DeploymentID, AuthKey: s.AuthKey, GoogleIP: DefaultGoogleIP, FrontDomain: DefaultFrontDomain}
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
