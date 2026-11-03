//go:build windows

package tunnel

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var sendMessageTimeout = windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")

// ReadProxyEnvironment returns the user's proxy environment variables; a nil value means the variable is not set.
func ReadProxyEnvironment() (map[string]*string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE)
	if err != nil {
		return nil, err
	}
	defer key.Close()
	values := map[string]*string{}
	for _, name := range proxyEnvironmentNames {
		value, _, err := key.GetStringValue(name)
		if err == nil {
			values[name] = &value
		} else {
			values[name] = nil
		}
	}
	return values, nil
}

// WriteProxyEnvironment sets or deletes the user's proxy environment variables and tells Windows they changed.
// Programs started afterwards see the new values; already open terminals do not.
func WriteProxyEnvironment(values map[string]*string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	for name, value := range values {
		if value == nil {
			if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
				return err
			}
			continue
		}
		if err := key.SetStringValue(name, *value); err != nil {
			return err
		}
	}
	// The timeout applies per window. Keep it short: our own window may be busy closing and cannot answer.
	environment, _ := windows.UTF16PtrFromString("Environment")
	const broadcast, settingChange, abortIfHung, timeoutMs = 0xffff, 0x001A, 0x0002, 300
	sendMessageTimeout.Call(broadcast, settingChange, 0, uintptr(unsafe.Pointer(environment)), abortIfHung, timeoutMs, 0)
	return nil
}
