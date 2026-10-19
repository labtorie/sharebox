// Package signaling implements the Sharebox coordination server: it serves
// the guest web app at /s/{session}, relays WebRTC signalling between guests
// and hosts, hands out STUN/TURN configuration with short-lived credentials,
// verifies host reachability from the outside, and (optionally) issues DNS
// names and certificates for direct HTTPS. It stores no files and sees no
// file contents.
package signaling

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"sharebox/internal/guestweb"
	"sharebox/internal/security"
	"sharebox/services/relay"
	p "sharebox/shared/protocol"
)

// Config for the coordination server.
type Config struct {
	// PublicURL is the externally visible base URL, e.g. https://share.example.org.
	PublicURL string
	// STUNURLs are extra STUN servers handed to clients (stun:host:port).
	STUNURLs []string
	// TURN, if Secret is set, enables relay credentials for these URLs
	// (e.g. turn:relay.example.org:3478?transport=udp).
	TURNURLs   []string
	TURNSecret string
	TURNTTL    time.Duration
	// TrustProxy uses X-Forwarded-For to determine client IPs. Enable only
	// behind a reverse proxy that sets it.
	TrustProxy bool
	// Dev relaxes checks for local testing (allows probing private IPs).
	Dev bool
	// HostGrace keeps a session alive while its host reconnects.
	HostGrace time.Duration
	// ConnectRate is the sustained WebSocket connections per second allowed
	// per client IP (default 0.2 for hosts, 0.5 for guests). Tests raise it.
	ConnectRate float64
	// Direct enables DNS names + certificates for direct HTTPS (optional).
	Direct *DirectService
	Logger *slog.Logger
}

// Server is the coordination service.
type Server struct {
	cfg  Config
	log  *slog.Logger
	mux  *http.ServeMux
	web  http.Handler
	mu   sync.Mutex
	sess map[string]*session

	joinLimit *security.Limiter
	hostLimit *security.Limiter
	probeHTTP *http.Client
	stop      chan struct{}
}

type session struct {
	id            string
	hostTokenHash string
	signalHash    string
	expiresAt     time.Time
	host          *wsConn
	offlineSince  time.Time
	guests        map[string]*wsConn
	direct        string
	ownerIP       string
}

// New creates a coordination server.
func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HostGrace == 0 {
		cfg.HostGrace = 15 * time.Minute
	}
	if cfg.TURNTTL == 0 {
		cfg.TURNTTL = 12 * time.Hour
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && isLocalHost(u.Hostname()))) {
		return nil, fmt.Errorf("public URL must be https:// (http:// is allowed only for localhost): %q", cfg.PublicURL)
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	s := &Server{
		cfg: cfg, log: cfg.Logger, sess: map[string]*session{},
		web:       guestweb.Handler(guestweb.ModeCoord),
		joinLimit: security.NewLimiter(max(cfg.ConnectRate, 0.5), 20),
		hostLimit: security.NewLimiter(max(cfg.ConnectRate, 0.2), 10),
		stop:      make(chan struct{}),
		probeHTTP: &http.Client{
			Timeout: 6 * time.Second,
			Transport: &http.Transport{
				// The probe proves reachability; identity is proven by the
				// HMAC in the response body, not by the certificate (the
				// host may not have a trusted certificate yet).
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
				DisableKeepAlives: true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	m := http.NewServeMux()
	m.HandleFunc("GET /ws/host", s.handleHostWS)
	m.HandleFunc("GET /ws/guest", s.handleGuestWS)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := len(s.sess)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"sessions":%d}`, n)
	})
	m.Handle("/", s.web)
	s.mux = m
	go s.janitor()
	return s, nil
}

// Close stops background work.
func (s *Server) Close() { close(s.stop) }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if strings.HasPrefix(s.cfg.PublicURL, "https://") {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) janitor() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		now := time.Now()
		s.mu.Lock()
		for id, ss := range s.sess {
			expired := !ss.expiresAt.IsZero() && now.After(ss.expiresAt)
			gone := ss.host == nil && now.Sub(ss.offlineSince) > s.cfg.HostGrace
			if expired || gone {
				s.deleteLocked(id, ss, p.ErrCodeExpired)
			}
		}
		s.mu.Unlock()
	}
}

func (s *Server) deleteLocked(id string, ss *session, code string) {
	delete(s.sess, id)
	for _, g := range ss.guests {
		g.send(p.Envelope{Type: p.MsgError, Code: code, Message: "this share has ended"})
		g.closeSoon()
	}
	if ss.host != nil {
		ss.host.session = ""
	}
}

// clientIP returns the caller's IP address.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if ip := forwardedIP(r.Header.Get("X-Forwarded-For")); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// forwardedIP returns the client address from X-Forwarded-For. Each proxy
// appends the address it received the request from, so the rightmost entry
// that is not an internal address is the real client: entries further left
// can be forged by the client, and entries further right are the hosting
// platform's own load balancers.
func forwardedIP(xff string) string {
	parts := strings.Split(xff, ",")
	last := ""
	for i := len(parts) - 1; i >= 0; i-- {
		v := strings.TrimSpace(parts[i])
		ip := net.ParseIP(v)
		if ip == nil {
			continue
		}
		if last == "" {
			last = v
		}
		if !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
			(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64)) {
			return v
		}
	}
	return last
}

// iceServers builds the ICE configuration for a client.
func (s *Server) iceServers(id string) []p.ICEServer {
	var out []p.ICEServer
	if len(s.cfg.STUNURLs) > 0 {
		out = append(out, p.ICEServer{URLs: s.cfg.STUNURLs})
	}
	if s.cfg.TURNSecret != "" && len(s.cfg.TURNURLs) > 0 {
		user, pass, err := relay.Credentials(s.cfg.TURNSecret, id, s.cfg.TURNTTL)
		if err == nil {
			out = append(out, p.ICEServer{URLs: s.cfg.TURNURLs, Username: user, Credential: pass})
		}
	}
	return out
}

func (s *Server) features() *p.Features {
	return &p.Features{TURN: s.cfg.TURNSecret != "" && len(s.cfg.TURNURLs) > 0, Probe: true, Direct: s.cfg.Direct != nil}
}

var idEnc = base32.NewEncoding("abcdefghijkmnpqrstuvwxyz23456789").WithPadding(base32.NoPadding)

func newSessionID() string {
	b, _ := base64.RawURLEncoding.DecodeString(security.RandomToken(15))
	return idEnc.EncodeToString(b) // 24 chars, 120 bits, unambiguous alphabet
}

func hashStr(v string) string {
	h := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// WebSocket plumbing

type wsConn struct {
	c       *websocket.Conn
	out     chan p.Envelope
	done    chan struct{}
	once    sync.Once
	ip      string
	peer    string // guests
	session string // session id this connection belongs to
	ua      string
}

const maxWSMessage = 64 << 10

func (s *Server) accept(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Browsers send Origin; only the coordination server's own pages may
		// connect as guests. Hosts are native clients.
		OriginPatterns: s.originPatterns(),
	})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(maxWSMessage)
	wc := &wsConn{c: c, out: make(chan p.Envelope, 128), done: make(chan struct{}), ip: s.clientIP(r), ua: r.UserAgent()}
	go wc.writer()
	return wc, nil
}

func (s *Server) originPatterns() []string {
	u, _ := url.Parse(s.cfg.PublicURL)
	return []string{u.Host}
}

func (c *wsConn) writer() {
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case m := <-c.out:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			b, _ := json.Marshal(m)
			err := c.c.Write(ctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				c.close()
				return
			}
		case <-ping.C:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := c.c.Ping(ctx)
			cancel()
			if err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *wsConn) send(m p.Envelope) {
	select {
	case c.out <- m:
	case <-c.done:
	default:
		c.close() // slow consumer
	}
}

func (c *wsConn) close() {
	c.once.Do(func() {
		close(c.done)
		c.c.Close(websocket.StatusNormalClosure, "")
	})
}

// closeSoon lets queued messages flush before closing.
func (c *wsConn) closeSoon() { time.AfterFunc(500*time.Millisecond, c.close) }

func (c *wsConn) read(ctx context.Context) (p.Envelope, error) {
	var m p.Envelope
	typ, b, err := c.c.Read(ctx)
	if err != nil {
		return m, err
	}
	if typ != websocket.MessageText {
		return m, errors.New("binary message")
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

// ---------------------------------------------------------------------------
// host connections

func (s *Server) handleHostWS(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.hostLimit.Allow(ip) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	c, err := s.accept(w, r)
	if err != nil {
		return
	}
	defer s.hostGone(c)
	ctx := r.Context()
	for {
		m, err := c.read(ctx)
		if err != nil {
			return
		}
		s.onHostMessage(ctx, c, m)
	}
}

func (s *Server) hostGone(c *wsConn) {
	c.close()
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sess[c.session]
	if ss == nil || ss.host != c {
		return
	}
	ss.host = nil
	ss.offlineSince = time.Now()
	for _, g := range ss.guests {
		g.send(p.Envelope{Type: p.MsgHostOffline})
	}
}

func (s *Server) onHostMessage(ctx context.Context, c *wsConn, m p.Envelope) {
	reply := func(e p.Envelope) {
		e.ID = m.ID
		if e.Type == "" {
			e.Type = p.MsgResult
		}
		c.send(e)
	}
	fail := func(msg string) { reply(p.Envelope{Type: p.MsgError, Message: msg}) }

	switch m.Type {
	case p.MsgPing:
		reply(p.Envelope{Type: p.MsgPong})

	case p.MsgRegister:
		if len(m.SignalHash) < 32 {
			fail("invalid registration")
			return
		}
		s.mu.Lock()
		// Leaving a previous session (e.g. link regenerated) ends it.
		if old := s.sess[c.session]; old != nil && old.host == c && old.id != m.SessionID {
			s.deleteLocked(old.id, old, p.ErrCodeExpired)
		}
		var ss *session
		if m.SessionID != "" && m.HostToken != "" {
			if ex := s.sess[m.SessionID]; ex != nil && security.Equal(ex.hostTokenHash, hashStr(m.HostToken)) {
				ss = ex // resume after reconnect
				if ss.host != nil && ss.host != c {
					ss.host.close()
				}
			}
		}
		hostToken := m.HostToken
		if ss == nil {
			count := 0
			for _, x := range s.sess {
				if x.ownerIP == c.ip {
					count++
				}
			}
			if count >= 20 {
				s.mu.Unlock()
				fail("too many active shares from this network")
				return
			}
			hostToken = security.RandomToken(32)
			ss = &session{id: newSessionID(), hostTokenHash: hashStr(hostToken), guests: map[string]*wsConn{}, ownerIP: c.ip}
			s.sess[ss.id] = ss
		}
		ss.signalHash = m.SignalHash
		ss.expiresAt = time.Time{}
		if m.ExpiresAt > 0 {
			ss.expiresAt = time.Unix(m.ExpiresAt, 0)
		}
		ss.host = c
		c.session = ss.id
		for _, g := range ss.guests {
			g.send(p.Envelope{Type: p.MsgHostOnline, Direct: ss.direct})
		}
		id := ss.id
		s.mu.Unlock()
		hostname := ""
		if s.cfg.Direct != nil && m.DeviceID != "" {
			hostname, _ = s.cfg.Direct.Register(m.DeviceID, m.DeviceKey, c.ip)
		}
		reply(p.Envelope{Type: p.MsgRegistered, SessionID: id, HostToken: hostToken,
			URL: s.cfg.PublicURL + "/s/" + id, ICEServers: s.iceServers("h-" + id),
			ObservedIP: c.ip, Features: s.features(), Hostname: hostname})

	case p.MsgUnregister:
		s.mu.Lock()
		if ss := s.sess[c.session]; ss != nil && ss.host == c {
			s.deleteLocked(ss.id, ss, p.ErrCodeExpired)
		}
		c.session = ""
		s.mu.Unlock()
		reply(p.Envelope{OK: true})

	case p.MsgSetDirect:
		if m.Direct != "" {
			u, err := url.Parse(m.Direct)
			if err != nil || u.Scheme != "https" || u.Path != "" && u.Path != "/" {
				fail("direct URL must be https://host:port")
				return
			}
		}
		s.mu.Lock()
		if ss := s.sess[c.session]; ss != nil && ss.host == c {
			ss.direct = strings.TrimRight(m.Direct, "/")
			for _, g := range ss.guests {
				g.send(p.Envelope{Type: p.MsgHostOnline, Direct: ss.direct})
			}
		}
		s.mu.Unlock()
		reply(p.Envelope{OK: true})

	case p.MsgSignal:
		if m.Data == nil {
			return
		}
		s.mu.Lock()
		ss := s.sess[c.session]
		var g *wsConn
		if ss != nil && ss.host == c {
			g = ss.guests[m.Peer]
		}
		s.mu.Unlock()
		if g != nil {
			g.send(p.Envelope{Type: p.MsgSignal, Data: m.Data})
		}

	case p.MsgKick:
		s.mu.Lock()
		if ss := s.sess[c.session]; ss != nil && ss.host == c {
			if g := ss.guests[m.Peer]; g != nil {
				g.send(p.Envelope{Type: p.MsgError, Code: p.ErrCodeUnauthorized, Message: "the host ended your access"})
				g.closeSoon()
			}
		}
		s.mu.Unlock()

	case p.MsgTurnCreds:
		reply(p.Envelope{ICEServers: s.iceServers("h-" + c.session)})

	case p.MsgProbe:
		go func() {
			ok, body, err := s.probe(ctx, c.ip, m.Port, m.Nonce, m.Hostname)
			e := p.Envelope{OK: ok, Body: body}
			if err != nil {
				e.Message = err.Error()
			}
			reply(e)
		}()

	case p.MsgCert:
		if s.cfg.Direct == nil {
			fail("this server does not issue certificates")
			return
		}
		go func() {
			cctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			pem, err := s.cfg.Direct.IssueCert(cctx, m.DeviceID, m.DeviceKey, c.ip, m.CSR)
			if err != nil {
				s.log.Warn("certificate issuance failed", "err", err)
				fail("certificate issuance failed: " + err.Error())
				return
			}
			reply(p.Envelope{OK: true, Cert: pem})
		}()

	default:
		fail("unknown message")
	}
}

// probe connects back to the host from the outside to verify that its
// mapped port is actually reachable. The target is always the IP the host
// connected from, so the server cannot be abused to scan third parties.
func (s *Server) probe(ctx context.Context, ip string, port int, nonce, hostname string) (bool, string, error) {
	if port < 1024 || port > 65535 || len(nonce) < 16 || len(nonce) > 128 {
		return false, "", errors.New("invalid probe request")
	}
	pip := net.ParseIP(ip)
	if pip == nil {
		return false, "", errors.New("unknown address")
	}
	if !s.cfg.Dev && (pip.IsLoopback() || pip.IsPrivate() || pip.IsLinkLocalUnicast()) {
		return false, "", errors.New("host address is not public")
	}
	target := net.JoinHostPort(ip, strconv.Itoa(port))
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+target+p.ProbePath+"?n="+url.QueryEscape(nonce), nil)
	if hostname != "" {
		req.Host = hostname
	}
	res, err := s.probeHTTP.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("not reachable from the internet")
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	if res.StatusCode != 200 {
		return false, "", fmt.Errorf("unexpected response %d", res.StatusCode)
	}
	return true, strings.TrimSpace(string(b)), nil
}

// ---------------------------------------------------------------------------
// guest connections

func (s *Server) handleGuestWS(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.joinLimit.Allow(ip) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	c, err := s.accept(w, r)
	if err != nil {
		return
	}
	defer c.close()
	ctx := r.Context()

	jctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	m, err := c.read(jctx)
	cancel()
	if err != nil || m.Type != p.MsgJoin {
		return
	}
	s.mu.Lock()
	ss := s.sess[m.SessionID]
	switch {
	case ss == nil:
		s.mu.Unlock()
		c.send(p.Envelope{Type: p.MsgError, Code: p.ErrCodeNotFound, Message: "this share link does not exist or has ended"})
		c.closeSoon()
		<-c.done
		return
	case !ss.expiresAt.IsZero() && time.Now().After(ss.expiresAt):
		s.mu.Unlock()
		c.send(p.Envelope{Type: p.MsgError, Code: p.ErrCodeExpired, Message: "this share link has expired"})
		c.closeSoon()
		<-c.done
		return
	case !security.Equal(hashStr(m.Signal), ss.signalHash):
		s.mu.Unlock()
		c.send(p.Envelope{Type: p.MsgError, Code: p.ErrCodeUnauthorized, Message: "this share link is no longer valid"})
		c.closeSoon()
		<-c.done
		return
	case len(ss.guests) >= 64:
		s.mu.Unlock()
		c.send(p.Envelope{Type: p.MsgError, Code: p.ErrCodeRateLimited, Message: "too many people are connected to this share"})
		c.closeSoon()
		<-c.done
		return
	}
	c.peer = "p" + security.RandomID()
	c.session = ss.id
	ss.guests[c.peer] = c
	host := ss.host
	online, direct := host != nil, ss.direct
	s.mu.Unlock()

	c.send(p.Envelope{Type: p.MsgJoined, Peer: c.peer, ICEServers: s.iceServers("g-" + c.peer), Online: online, Direct: direct})
	if host != nil {
		host.send(p.Envelope{Type: p.MsgGuestJoined, Peer: c.peer})
	}
	defer func() {
		s.mu.Lock()
		if ss := s.sess[c.session]; ss != nil {
			delete(ss.guests, c.peer)
			if ss.host != nil {
				ss.host.send(p.Envelope{Type: p.MsgGuestLeft, Peer: c.peer})
			}
		}
		s.mu.Unlock()
	}()

	msgLimit := security.NewLimiter(20, 200) // ICE candidates arrive in bursts
	for {
		m, err := c.read(ctx)
		if err != nil {
			return
		}
		if !msgLimit.Allow("x") {
			return
		}
		switch m.Type {
		case p.MsgSignal:
			if m.Data == nil {
				continue
			}
			s.mu.Lock()
			var h *wsConn
			if ss := s.sess[c.session]; ss != nil {
				h = ss.host
			}
			s.mu.Unlock()
			if h != nil {
				h.send(p.Envelope{Type: p.MsgSignal, Peer: c.peer, Data: m.Data})
			}
		case p.MsgPing:
			c.send(p.Envelope{Type: p.MsgPong})
		}
	}
}

func isLocalHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
