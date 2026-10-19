// Package fileserver implements every file-management operation a guest can
// perform, exposed as a single http.Handler. All transports (direct HTTPS and
// the WebRTC tunnel, whether peer-to-peer or relayed) feed requests into this
// one handler, so authorisation, permissions and filesystem logic exist
// exactly once.
package fileserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"sharebox/internal/fsroot"
	"sharebox/internal/security"
	"sharebox/internal/transfers"
)

// Permissions control what guests may do. They are evaluated on every
// request, so changes made by the host apply immediately.
type Permissions struct {
	Download bool `json:"download"`
	Upload   bool `json:"upload"`
	Mkdir    bool `json:"mkdir"`
	Delete   bool `json:"delete"`
}

// DefaultPermissions: read and upload, never delete.
func DefaultPermissions() Permissions {
	return Permissions{Download: true, Upload: true, Mkdir: true, Delete: false}
}

// Limits bound resource usage by guests.
type Limits struct {
	MaxUploadBytes int64   `json:"maxUploadBytes"` // per file; 0 = no limit
	MinFreeBytes   int64   `json:"minFreeBytes"`   // keep at least this much disk free
	MaxConcurrent  int     `json:"maxConcurrent"`  // simultaneous transfers per share
	SessionQuota   int64   `json:"sessionQuota"`   // total upload bytes per session; 0 = no limit
	RequestsPerSec float64 `json:"requestsPerSec"` // per guest connection
}

// DefaultLimits are conservative but allow very large files.
func DefaultLimits() Limits {
	return Limits{MaxUploadBytes: 0, MinFreeBytes: 1 << 30, MaxConcurrent: 8, RequestsPerSec: 50}
}

// Config is the live configuration of a share.
type Config struct {
	Perms     Permissions
	Limits    Limits
	AccessKey string                 // secret from the share link fragment
	Password  *security.PasswordHash // optional
	ExpiresAt time.Time              // zero = never
}

// Transport labels.
const (
	TransportDirect = "direct"
	TransportP2P    = "p2p"
	TransportRelay  = "relay"
	TransportLocal  = "local"
)

type ctxKey int

const (
	transportKey ctxKey = iota
	peerKey
	guestKey
	routeKey
)

// WithTransport tags a request context with the transport it arrived on and
// a stable identifier for the underlying connection (used for rate limits).
func WithTransport(ctx context.Context, kind func() string, peer string) context.Context {
	ctx = context.WithValue(ctx, transportKey, kind)
	return context.WithValue(ctx, peerKey, peer)
}

// WithRoute tags a request context with a description of the network path
// ("local network", "internet", "relay").
func WithRoute(ctx context.Context, route func() string) context.Context {
	return context.WithValue(ctx, routeKey, route)
}

// routeOf describes the network path a request took.
func routeOf(r *http.Request) string {
	if f, ok := r.Context().Value(routeKey).(func() string); ok {
		return f()
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			if ip.IsLoopback() {
				return "this computer"
			}
			if ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				return "local network"
			}
		}
	}
	return "internet"
}

func transportOf(r *http.Request) string {
	if f, ok := r.Context().Value(transportKey).(func() string); ok {
		return f()
	}
	// The host's own preview is plain HTTP on loopback; anything over TLS is
	// a direct guest connection.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && r.TLS == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return TransportLocal
		}
	}
	return TransportDirect
}

func peerOf(r *http.Request) string {
	if p, ok := r.Context().Value(peerKey).(string); ok {
		return p
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return "ip:" + host
}

// Server is one sharing session's file service.
type Server struct {
	root    *fsroot.Root
	tracker *transfers.Tracker
	issuer  *security.Issuer
	log     *slog.Logger
	uploads *uploadManager

	mu  sync.RWMutex
	cfg Config

	authLimit *security.Limiter
	pwLimit   *security.Limiter // share-wide: guests can rotate connections
	reqLimit  *security.Limiter
	sem       chan struct{}
	mux       *http.ServeMux
	web       http.Handler

	// OnGuestRevoked is called when a guest's access ends (used by the RTC
	// layer to drop that guest's connection).
	OnAuth func(guest, peer string)
}

// Options for New.
type Options struct {
	Root     *fsroot.Root
	Tracker  *transfers.Tracker
	Config   Config
	TokenTTL time.Duration
	Logger   *slog.Logger
	Web      http.Handler // static guest UI; optional
}

// New creates a file service. The caller owns Root.
func New(o Options) (*Server, error) {
	if o.TokenTTL == 0 {
		o.TokenTTL = 12 * time.Hour
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Tracker == nil {
		o.Tracker = transfers.New()
	}
	if o.Config.Limits.MaxConcurrent <= 0 {
		o.Config.Limits.MaxConcurrent = DefaultLimits().MaxConcurrent
	}
	if o.Config.Limits.RequestsPerSec <= 0 {
		o.Config.Limits.RequestsPerSec = DefaultLimits().RequestsPerSec
	}
	s := &Server{
		root:      o.Root,
		tracker:   o.Tracker,
		issuer:    security.NewIssuer(o.TokenTTL),
		log:       o.Logger,
		cfg:       o.Config,
		authLimit: security.NewLimiter(0.2, 10), // 10 attempts, then one per 5 s
		pwLimit:   security.NewLimiter(0.2, 20),
		reqLimit:  security.NewLimiter(o.Config.Limits.RequestsPerSec, int(o.Config.Limits.RequestsPerSec*4)),
		sem:       make(chan struct{}, 64),
		web:       o.Web,
	}
	um, err := newUploadManager(s)
	if err != nil {
		return nil, err
	}
	s.uploads = um
	s.routes()
	return s, nil
}

// Close stops background work and removes incomplete uploads.
func (s *Server) Close() error {
	s.uploads.close()
	return nil
}

// Config returns the current configuration.
func (s *Server) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update changes the live configuration.
func (s *Server) Update(f func(*Config)) {
	s.mu.Lock()
	f(&s.cfg)
	s.mu.Unlock()
}

// RevokeAll invalidates all guest tokens (used when the link is regenerated).
func (s *Server) RevokeAll() { s.issuer.RevokeAll() }

// RevokeGuest invalidates one guest's token.
func (s *Server) RevokeGuest(id string) {
	s.issuer.RevokeGuest(id)
	s.tracker.RemoveGuest(id)
}

// Issuer exposes the token issuer (tests).
func (s *Server) Issuer() *security.Issuer { return s.issuer }

// Tracker returns the transfer tracker.
func (s *Server) Tracker() *transfers.Tracker { return s.tracker }

func (s *Server) routes() {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/ping", s.handlePing)
	m.HandleFunc("OPTIONS /api/ping", s.handlePing)
	m.HandleFunc("POST /api/auth", s.handleAuth)
	m.HandleFunc("GET /api/session", s.auth(s.handleSession, false))
	m.HandleFunc("GET /api/list", s.auth(s.handleList, false))
	m.HandleFunc("GET /api/search", s.auth(s.handleSearch, false))
	m.HandleFunc("GET /api/download/{name...}", s.auth(s.handleDownload, true))
	m.HandleFunc("HEAD /api/download/{name...}", s.auth(s.handleDownload, true))
	m.HandleFunc("GET /api/zip/{name...}", s.auth(s.handleZip, true))
	m.HandleFunc("POST /api/mkdir", s.auth(s.handleMkdir, false))
	m.HandleFunc("POST /api/delete", s.auth(s.handleDelete, false))
	m.HandleFunc("POST /api/uploads", s.auth(s.uploads.handleCreate, false))
	m.HandleFunc("GET /api/uploads/{id}", s.auth(s.uploads.handleStatus, false))
	m.HandleFunc("PUT /api/uploads/{id}", s.auth(s.uploads.handleChunk, false))
	m.HandleFunc("POST /api/uploads/{id}/complete", s.auth(s.uploads.handleComplete, false))
	m.HandleFunc("DELETE /api/uploads/{id}", s.auth(s.uploads.handleCancel, false))
	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "unknown endpoint")
	})
	if s.web != nil {
		m.Handle("/", s.web)
	}
	s.mux = m
}

// ServeHTTP implements http.Handler with security headers and rate limiting.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	if r.TLS != nil {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.Set("Cache-Control", "no-store")
		if !s.reqLimit.Allow(peerOf(r)) {
			h.Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, "too many requests, slow down")
			return
		}
	}
	if exp := s.Config().ExpiresAt; !exp.IsZero() && time.Now().After(exp) && r.URL.Path != "/api/ping" {
		writeErr(w, http.StatusGone, "this share link has expired")
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	// Public and CORS-enabled so the coordination page can test direct
	// reachability. Contains nothing sensitive.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sharebox": "host", "v": 1})
}

const cookieName = "sharebox_token"

type authReq struct {
	Key      string `json:"key"`
	Password string `json:"password"`
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	if !s.authLimit.Allow(peerOf(r)) {
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusTooManyRequests, "too many attempts, wait a moment")
		return
	}
	var req authReq
	if !readJSON(w, r, &req) {
		return
	}
	cfg := s.Config()
	if cfg.AccessKey == "" || !security.Equal(req.Key, cfg.AccessKey) {
		writeErr(w, http.StatusUnauthorized, "this link is no longer valid")
		return
	}
	if cfg.Password != nil {
		if req.Password == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "password required", "passwordRequired": true})
			return
		}
		if !s.pwLimit.Allow("share") {
			w.Header().Set("Retry-After", "10")
			writeErr(w, http.StatusTooManyRequests, "too many password attempts; wait a moment")
			return
		}
		if !cfg.Password.Verify(req.Password) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong password", "passwordRequired": true})
			return
		}
	}
	guest := "g" + security.RandomID()
	tok, exp := s.issuer.Issue(guest)
	if !cfg.ExpiresAt.IsZero() && exp.After(cfg.ExpiresAt) {
		exp = cfg.ExpiresAt
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/api/", Expires: exp,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
	s.tracker.TouchGuest(guest, transportOf(r), routeOf(r), clientLabel(r))
	if s.OnAuth != nil {
		s.OnAuth(guest, peerOf(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": tok, "guest": guest, "expiresAt": exp, "session": s.sessionInfo(),
	})
}

// auth wraps a handler with token verification. Cookie authentication is
// only accepted for safe GET requests (native browser downloads); anything
// that changes state needs the Authorization header, which a cross-site
// page cannot set, preventing CSRF.
func (s *Server) auth(h http.HandlerFunc, allowCookie bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := ""
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			tok = strings.TrimPrefix(a, "Bearer ")
		} else if allowCookie && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if c, err := r.Cookie(cookieName); err == nil {
				tok = c.Value
			}
		}
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		claims, err := s.issuer.Verify(tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "access expired or revoked; reopen the link")
			return
		}
		s.tracker.TouchGuest(claims.Guest, transportOf(r), routeOf(r), clientLabel(r))
		ctx := context.WithValue(r.Context(), guestKey, claims.Guest)
		h(w, r.WithContext(ctx))
	}
}

func guestOf(r *http.Request) string {
	g, _ := r.Context().Value(guestKey).(string)
	return g
}

// acquire reserves a transfer slot or replies 429.
func (s *Server) acquire(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	limit := s.Config().Limits.MaxConcurrent
	if limit <= 0 || limit > cap(s.sem) {
		limit = cap(s.sem)
	}
	if len(s.sem) >= limit {
		w.Header().Set("Retry-After", "2")
		writeErr(w, http.StatusTooManyRequests, "the host is busy with other transfers; retrying")
		return nil, false
	}
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, true
	case <-r.Context().Done():
		return nil, false
	}
}

func (s *Server) sessionInfo() map[string]any {
	cfg := s.Config()
	info := map[string]any{
		"name":           s.root.Name(),
		"permissions":    cfg.Perms,
		"maxUploadBytes": cfg.Limits.MaxUploadBytes,
		"chunkSize":      chunkSize,
	}
	if !cfg.ExpiresAt.IsZero() {
		info["expiresAt"] = cfg.ExpiresAt
	}
	return info
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sessionInfo())
}

// --- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return false
	}
	return true
}

// fsError maps filesystem errors to HTTP responses without leaking paths.
func (s *Server) fsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fsroot.ErrInvalidPath):
		writeErr(w, http.StatusBadRequest, "invalid file or folder name")
	case errors.Is(err, fsroot.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, fsroot.ErrExists):
		writeErr(w, http.StatusConflict, "already exists")
	case errors.Is(err, fsroot.ErrNotDir):
		writeErr(w, http.StatusBadRequest, "not a folder")
	case errors.Is(err, fsroot.ErrIsDir):
		writeErr(w, http.StatusBadRequest, "is a folder")
	case errors.Is(err, fsroot.ErrNotRegular):
		writeErr(w, http.StatusForbidden, "this item cannot be shared")
	case errors.Is(err, fs.ErrPermission):
		writeErr(w, http.StatusForbidden, "the host's system denied access to this item")
	case errors.Is(err, context.Canceled):
	default:
		s.log.Warn("filesystem error", "err", err)
		writeErr(w, http.StatusInternalServerError, "the host could not complete the operation")
	}
}

func forbid(w http.ResponseWriter, what string) {
	writeErr(w, http.StatusForbidden, "the host does not allow "+what)
}

// clientLabel returns a coarse description such as "Chrome on macOS".
func clientLabel(r *http.Request) string {
	ua := r.Header.Get("User-Agent")
	browser := "Browser"
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	case strings.Contains(ua, "Go-http-client"):
		browser = "Client"
	}
	osName := ""
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		osName = "iOS"
	case strings.Contains(ua, "Android"):
		osName = "Android"
	case strings.Contains(ua, "Mac OS X"):
		osName = "macOS"
	case strings.Contains(ua, "Windows"):
		osName = "Windows"
	case strings.Contains(ua, "Linux"):
		osName = "Linux"
	}
	if osName == "" {
		return browser
	}
	return browser + " on " + osName
}

// discard drains up to 64 KiB of an unread body so connections can be reused.
func discard(r io.Reader) { _, _ = io.CopyN(io.Discard, r, 64<<10) }
