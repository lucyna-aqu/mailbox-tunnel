//go:build windows

package tunnel

import (
	"errors"

	"golang.org/x/sys/windows"
)

const instanceMutexName = "Local\\mailbox-tunnel-client"

// AcquireSingleInstance returns false when another client (desktop or console) is already running for this user.
// The lock is held until the process exits.
func AcquireSingleInstance() bool {
	name, err := windows.UTF16PtrFromString(instanceMutexName)
	if err != nil {
		return true
	}
	_, err = windows.CreateMutex(nil, false, name)
	return !errors.Is(err, windows.ERROR_ALREADY_EXISTS)
}
