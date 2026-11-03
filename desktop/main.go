// Command mailbox-tunnel-desktop is the Windows app for mailbox-tunnel users.
package main

import (
	"embed"
	"log"

	"github.com/lucyna-aqu/mailbox-tunnel/tunnel"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// One client at a time: a second launch brings the open window forward instead of competing for the mailbox.
	if !tunnel.AcquireSingleInstance() {
		if !focusRunningWindow() {
			showMessage("mailbox-tunnel is already running in a console window. Close it first.")
		}
		return
	}
	app := NewApp()
	err := wails.Run(&options.App{
		Title:            windowTitle,
		Width:            540,
		Height:           720,
		MinWidth:         480,
		MinHeight:        620,
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 16, G: 28, B: 36, A: 1},
		OnStartup:        app.startup,
		OnBeforeClose:    app.beforeClose,
		Bind:             []interface{}{app},
	})
	if err != nil {
		log.Fatal(err)
	}
}
