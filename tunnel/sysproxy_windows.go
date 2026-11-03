//go:build windows

package tunnel

import (
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

var internetSetOption = windows.NewLazySystemDLL("wininet.dll").NewProc("InternetSetOptionW")

func ReadSystemProxy() (ProxySettings, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return ProxySettings{}, err
	}
	defer key.Close()
	var settings ProxySettings
	if enable, _, err := key.GetIntegerValue("ProxyEnable"); err == nil {
		settings.Enable = uint32(enable)
	}
	settings.Server, _, _ = key.GetStringValue("ProxyServer")
	settings.Override, _, _ = key.GetStringValue("ProxyOverride")
	return settings, nil
}

func WriteSystemProxy(settings ProxySettings) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.SetDWordValue("ProxyEnable", settings.Enable); err != nil {
		return err
	}
	if err := key.SetStringValue("ProxyServer", settings.Server); err != nil {
		return err
	}
	if err := key.SetStringValue("ProxyOverride", settings.Override); err != nil {
		return err
	}
	const settingsChanged, refresh = 39, 37
	internetSetOption.Call(0, settingsChanged, 0, 0)
	internetSetOption.Call(0, refresh, 0, 0)
	return nil
}
