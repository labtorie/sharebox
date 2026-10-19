// Command sharebox is the desktop host application (Wails + React).
package main

import (
	"embed"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"sharebox/internal/config"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	store, err := config.DefaultStore()
	if err != nil {
		println("cannot open settings:", err.Error())
		os.Exit(1)
	}
	// Log to a file in the config dir. Paths and secrets are never logged.
	var log *slog.Logger
	if f, err := os.OpenFile(filepath.Join(store.Dir, "sharebox.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		log = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
	} else {
		log = slog.Default()
	}
	app := NewApp(store, log)

	err = wails.Run(&options.App{
		Title:            "Sharebox",
		Width:            1040,
		Height:           720,
		MinWidth:         760,
		MinHeight:        560,
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 250, G: 250, B: 250, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		OnBeforeClose:    app.beforeClose,
		Bind:             []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "app.sharebox.desktop",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) { app.onSecondInstance() },
		},
		Mac: &mac.Options{
			TitleBar:             mac.TitleBarHiddenInset(),
			Appearance:           mac.DefaultAppearance,
			WebviewIsTransparent: true,
			WindowIsTranslucent:  true,
			About:                &mac.AboutInfo{Title: "Sharebox", Message: "One-click peer-to-peer folder sharing."},
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			Theme:                windows.SystemDefault,
		},
	})
	if err != nil {
		log.Error("wails", "err", err)
	}
}
