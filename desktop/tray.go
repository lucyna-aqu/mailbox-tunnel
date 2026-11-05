package main

import (
	_ "embed"
	goruntime "runtime"
	"sync/atomic"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed winres/icon.ico
var trayIcon []byte

// trayMenu holds the tray items whose text follows the connection state.
type trayMenu struct {
	status *systray.MenuItem
	toggle *systray.MenuItem
}

var (
	tray     atomic.Pointer[trayMenu]
	quitting atomic.Bool
)

// startTray puts the icon next to the clock.
// It runs on a thread of its own, because a tray icon only gets messages on the thread that created it.
func (a *App) startTray() {
	go func() {
		goruntime.LockOSThread()
		systray.Run(a.trayReady, nil)
	}()
}

func (a *App) trayReady() {
	systray.SetIcon(trayIcon)
	systray.SetTooltip("Mailbox Tunnel")
	status := systray.AddMenuItem("Not connected", "")
	status.Disable()
	systray.AddSeparator()
	toggle := systray.AddMenuItem("اتصال", "")
	open := systray.AddMenuItem("باز کردن پنجره", "")
	systray.AddSeparator()
	quit := systray.AddMenuItem("خروج", "")
	tray.Store(&trayMenu{status: status, toggle: toggle})
	a.updateTray(a.State())

	go func() {
		for {
			select {
			case <-toggle.ClickedCh:
				go a.toggleFromTray()
			case <-open.ClickedCh:
				a.showWindow()
			case <-quit.ClickedCh:
				a.quit()
				return
			}
		}
	}()
}

// updateTray shows the connection state in the tray menu and tooltip.
func (a *App) updateTray(state State) {
	menu := tray.Load()
	if menu == nil {
		return
	}
	text := map[string]string{"scanning": "Finding a route…", "connecting": "Connecting…", "connected": "Connected"}[state.Status]
	if text == "" {
		text = "Not connected"
	}
	if state.Status == "connected" {
		route := "Google"
		if state.Route == "Backup relay" {
			route = "GitHub relay"
		}
		text += " · " + route
	}
	menu.status.SetTitle(text)
	systray.SetTooltip("Mailbox Tunnel · " + text)
	if state.Status == statusDisconnected {
		menu.toggle.SetTitle("اتصال")
	} else {
		menu.toggle.SetTitle("قطع اتصال")
	}
}

// toggleFromTray connects or disconnects; anything that needs the user's attention opens the window.
func (a *App) toggleFromTray() {
	state := a.State()
	switch {
	case !state.Configured:
		a.showWindow()
	case state.Status == statusDisconnected:
		if message := a.Connect(); message != "" {
			a.setError(message)
			a.showWindow()
		}
	default:
		a.Disconnect()
	}
}

func (a *App) showWindow() {
	runtime.WindowShow(a.ctx)
	runtime.WindowUnminimise(a.ctx)
	focusRunningWindow()
}

// quit is the only way out besides Windows shutting down: closing the window only hides it.
func (a *App) quit() {
	quitting.Store(true)
	runtime.Quit(a.ctx)
}
