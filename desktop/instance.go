package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	findWindow          = user32.NewProc("FindWindowW")
	showWindow          = user32.NewProc("ShowWindow")
	setForegroundWindow = user32.NewProc("SetForegroundWindow")
	messageBox          = user32.NewProc("MessageBoxW")
)

const windowTitle = "Mailbox Tunnel"

// focusRunningWindow brings an already open window of this app to the front. It returns false when there is none.
func focusRunningWindow() bool {
	title, _ := windows.UTF16PtrFromString(windowTitle)
	window, _, _ := findWindow.Call(0, uintptr(unsafe.Pointer(title)))
	if window == 0 {
		return false
	}
	const showRestore = 9
	showWindow.Call(window, showRestore)
	setForegroundWindow.Call(window)
	return true
}

func showMessage(text string) {
	caption, _ := windows.UTF16PtrFromString(windowTitle)
	body, _ := windows.UTF16PtrFromString(text)
	const iconInformation = 0x40
	messageBox.Call(0, uintptr(unsafe.Pointer(body)), uintptr(unsafe.Pointer(caption)), iconInformation)
}
