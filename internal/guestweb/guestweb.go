// Package guestweb embeds the compiled guest web interface (apps/guest-web)
// so that both the host (direct mode) and the coordination server (WebRTC
// mode) can serve the exact same single-page app.
package guestweb

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// Mode tells the guest app how it was loaded.
const (
	ModeDirect = "direct" // served by the host itself over HTTPS
	ModeCoord  = "coord"  // served by the coordination server; connects via WebRTC
)

// FS returns the embedded files.
func FS() fs.FS {
	sub, _ := fs.Sub(dist, "dist")
	return sub
}

// Handler serves the app. Unknown non-asset paths fall back to index.html so
// client-side routes such as /s/{id} work.
func Handler(mode string) http.Handler {
	files := FS()
	index, _ := fs.ReadFile(files, "index.html")
	index = bytes.ReplaceAll(index, []byte("__SHAREBOX_MODE__"), []byte(mode))
	static := http.FileServerFS(files)
	csp := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
		"connect-src 'self' https: wss: ws://localhost:* ws://127.0.0.1:*; worker-src 'self'; frame-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "sw.js" {
			// The service worker must be scoped to the whole origin and must
			// never be cached stale.
			h.Set("Cache-Control", "no-cache")
			h.Set("Service-Worker-Allowed", "/")
		}
		if p != "" && p != "index.html" {
			if f, err := files.Open(p); err == nil {
				f.Close()
				if strings.HasPrefix(p, "assets/") {
					h.Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				static.ServeHTTP(w, r)
				return
			}
			if strings.Contains(path.Base(p), ".") {
				http.NotFound(w, r)
				return
			}
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
	})
}
