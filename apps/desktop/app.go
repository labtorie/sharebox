package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"sharebox/internal/config"
	"sharebox/internal/host"
)

// App is bound to the frontend; its exported methods are callable from JS.
type App struct {
	ctx    context.Context
	store  *config.Store
	engine *host.Engine
	log    *slog.Logger

	mu       sync.Mutex
	settings config.Settings
	quitting bool
	emitT    *time.Timer
}

// NewApp creates the application.
func NewApp(store *config.Store, log *slog.Logger) *App {
	a := &App{store: store, log: log, settings: store.LoadSettings()}
	a.engine = host.New(store, log)
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.engine.OnChange(a.scheduleEmit)
	// Live stats while sharing.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			if a.engine.Running() {
				a.emit()
			}
		}
	}()
}

// scheduleEmit coalesces bursts of status changes.
func (a *App) scheduleEmit() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.emitT != nil {
		return
	}
	a.emitT = time.AfterFunc(150*time.Millisecond, func() {
		a.mu.Lock()
		a.emitT = nil
		a.mu.Unlock()
		a.emit()
	})
}

func (a *App) emit() {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "status", a.engine.Status())
	}
}

// beforeClose implements the "keep sharing when the window is closed"
// setting. Wails v2 has no tray API, so the window is minimised to the Dock
// or taskbar instead; launching Sharebox again also brings it back.
func (a *App) beforeClose(ctx context.Context) bool {
	a.mu.Lock()
	keep, quitting := a.settings.KeepRunningOnClose, a.quitting
	a.mu.Unlock()
	if !quitting && keep && a.engine.Running() {
		runtime.WindowMinimise(ctx)
		return true
	}
	return false
}

func (a *App) shutdown(context.Context) { a.engine.Stop() }

// onSecondInstance focuses the running window.
func (a *App) onSecondInstance() {
	if a.ctx == nil {
		return
	}
	runtime.WindowUnminimise(a.ctx)
	runtime.WindowShow(a.ctx)
}

// --- bound methods ----------------------------------------------------------

// Status returns the current host status.
func (a *App) Status() host.Status { return a.engine.Status() }

// GetSettings returns saved settings.
func (a *App) GetSettings() config.Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings
}

// SaveSettings validates and persists settings; permissions and limits are
// applied immediately to a running share.
func (a *App) SaveSettings(s config.Settings) error {
	s.ServerURL = strings.TrimRight(strings.TrimSpace(s.ServerURL), "/")
	if s.ServerURL == "" {
		s.ServerURL = config.DefaultServer() // clearing the field restores the default
	}
	if s.ServerURL != "" {
		u, err := url.Parse(s.ServerURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLocal(u.Hostname()))) {
			return errors.New("the server address must start with https://")
		}
	}
	if s.MaxConcurrent < 1 || s.MaxConcurrent > 64 {
		s.MaxConcurrent = 8
	}
	if s.MinFreeMB < 0 || s.MaxUploadMB < 0 || s.ExpiryHours < 0 {
		return errors.New("limits cannot be negative")
	}
	a.mu.Lock()
	a.settings = s
	a.mu.Unlock()
	a.engine.UpdateSettings(s)
	return a.store.SaveSettings(s)
}

func isLocal(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

// DefaultServer is the built-in shared server ("" if none was built in).
func (a *App) DefaultServer() string { return config.DefaultServer() }

// ChooseFolder opens the native folder picker and saves the choice.
func (a *App) ChooseFolder() (string, error) {
	cur := a.GetSettings()
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Choose a folder to share", DefaultDirectory: cur.Folder, CanCreateDirectories: true,
	})
	if err != nil || dir == "" {
		return "", err
	}
	if err := checkFolder(dir); err != nil {
		return "", err
	}
	cur.Folder = dir
	return dir, a.SaveSettings(cur)
}

// checkFolder refuses folders that would expose far more than intended.
func checkFolder(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	clean := filepath.Clean(abs)
	if clean == filepath.VolumeName(clean)+string(filepath.Separator) {
		return errors.New("sharing an entire disk is not allowed; choose a specific folder")
	}
	if home != "" && clean == filepath.Clean(home) {
		return errors.New("sharing your whole home folder is not allowed; choose a specific folder")
	}
	return nil
}

// Start begins sharing with an optional password.
func (a *App) Start(password string) error {
	s := a.GetSettings()
	if err := checkFolder(s.Folder); err != nil && s.Folder != "" {
		return err
	}
	err := a.engine.Start(s, host.StartOptions{Password: password})
	a.emit()
	return err
}

// Stop ends sharing immediately.
func (a *App) Stop() {
	a.engine.Stop()
	a.emit()
}

// RegenerateLink revokes the current link and all guests.
func (a *App) RegenerateLink() error { return a.engine.RegenerateLink() }

// RevokeGuest disconnects one guest.
func (a *App) RevokeGuest(id string) { a.engine.RevokeGuest(id) }

// CancelTransfer aborts a transfer.
func (a *App) CancelTransfer(id string) bool { return a.engine.CancelTransfer(id) }

// SetPassword changes the password of the running share ("" removes it).
func (a *App) SetPassword(pw string) error { return a.engine.SetPassword(pw) }

// ClearErrors empties the error log.
func (a *App) ClearErrors() { a.engine.ClearErrors() }

// CopyLink copies the verified share link to the clipboard.
func (a *App) CopyLink() error {
	s := a.engine.Status()
	if !s.LinkReady {
		return errors.New("the link is not ready yet")
	}
	return runtime.ClipboardSetText(a.ctx, s.Link)
}

// OpenPreview shows the host's own view in the default browser.
func (a *App) OpenPreview() {
	if u := a.engine.Status().PreviewURL; u != "" {
		runtime.BrowserOpenURL(a.ctx, u)
	}
}

// RevealFolder opens the shared folder in Finder/Explorer.
func (a *App) RevealFolder() {
	if f := a.GetSettings().Folder; f != "" {
		runtime.BrowserOpenURL(a.ctx, (&url.URL{Scheme: "file", Path: filepath.ToSlash(f)}).String())
	}
}

// QRCode returns a PNG data URL for the link.
func (a *App) QRCode(text string) (string, error) {
	png, err := qrcode.Encode(text, qrcode.Medium, 512)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

// Quit stops sharing and exits.
func (a *App) Quit() {
	a.mu.Lock()
	a.quitting = true
	a.mu.Unlock()
	runtime.Quit(a.ctx)
}
