package main

import (
	"log"
	goruntime "runtime"
	"unsafe"

	"github.com/lucyna-aqu/mailbox-tunnel/tunnel"
	"golang.org/x/sys/windows"
)

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	getModuleHandle  = kernel32.NewProc("GetModuleHandleW")
	registerClassEx  = user32.NewProc("RegisterClassExW")
	createWindowEx   = user32.NewProc("CreateWindowExW")
	defWindowProc    = user32.NewProc("DefWindowProcW")
	getMessage       = user32.NewProc("GetMessageW")
	dispatchMessage  = user32.NewProc("DispatchMessageW")
	sessionClassName = "MailboxTunnelSession"
)

type windowClass struct {
	size       uint32
	style      uint32
	windowProc uintptr
	classExtra int32
	extra      int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	smallIcon  uintptr
}

// watchSessionEnd puts the system proxy back when Windows signs out or shuts down while the app runs in the tray.
// Windows tells only top-level windows, and the Wails window does not pass the message on, so a hidden window of its own listens.
func (a *App) watchSessionEnd() {
	go func() {
		goruntime.LockOSThread()
		const queryEndSession, endSession = 0x0011, 0x0016
		windowProc := windows.NewCallback(func(window, message, wParam, lParam uintptr) uintptr {
			switch message {
			case queryEndSession:
				return 1
			case endSession:
				if wParam != 0 {
					a.endSession()
				}
				return 0
			}
			result, _, _ := defWindowProc.Call(window, message, wParam, lParam)
			return result
		})
		instance, _, _ := getModuleHandle.Call(0)
		className, _ := windows.UTF16PtrFromString(sessionClassName)
		class := windowClass{windowProc: windowProc, instance: instance, className: className}
		class.size = uint32(unsafe.Sizeof(class))
		if atom, _, _ := registerClassEx.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
			return
		}
		window, _, _ := createWindowEx.Call(0, uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, 0, 0, instance, 0)
		if window == 0 {
			return
		}
		var message [48]byte
		for {
			if result, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&message[0])), 0, 0, 0); int32(result) <= 0 {
				return
			}
			dispatchMessage.Call(uintptr(unsafe.Pointer(&message[0])))
		}
	}()
}

// endSession restores the proxy first, because Windows gives a closing app only a few seconds.
func (a *App) endSession() {
	log.Printf("windows is ending the session, restoring the system proxy")
	if saved, err := tunnel.LoadSettings(a.settingsPath); err == nil {
		tunnel.RestoreLeftoverProxy(a.settingsPath, &saved)
	}
	a.Disconnect()
}
