// Package host orchestrates a sharing session on the host: the file service,
// the coordination connection, WebRTC peers, router port mappings, the
// optional direct HTTPS listener and continuous connectivity verification.
// The desktop app and the headless CLI are thin shells around Engine.
package host

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"sharebox/internal/config"
	"sharebox/internal/coord"
	"sharebox/internal/fileserver"
	"sharebox/internal/fsroot"
	"sharebox/internal/guestweb"
	"sharebox/internal/network"
	"sharebox/internal/rtc"
	"sharebox/internal/security"
	"sharebox/internal/transfers"
	p "sharebox/shared/protocol"
)

// Check states shown in the UI.
const (
	CheckOff         = "off"
	CheckPending     = "checking"
	CheckOK          = "ok"
	CheckWarn        = "warning"
	CheckUnavailable = "unavailable"
)

// Check is the result of verifying one transport.
type Check struct {
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// FolderInfo describes the shared folder.
type FolderInfo struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Files    int    `json:"files"`
	Scanning bool   `json:"scanning"`
	Partial  bool   `json:"partial"`
}

// Status is a snapshot for the UI.
type Status struct {
	State      string             `json:"state"` // idle | starting | sharing | stopping
	Link       string             `json:"link,omitempty"`
	LinkReady  bool               `json:"linkReady"`
	Headline   string             `json:"headline"`
	Server     Check              `json:"server"`
	Direct     Check              `json:"direct"`
	P2P        Check              `json:"p2p"`
	Relay      Check              `json:"relay"`
	Mapping    Check              `json:"mapping"`
	Errors     []string           `json:"errors"`
	Folder     FolderInfo         `json:"folder"`
	Stats      transfers.Snapshot `json:"stats"`
	Started    time.Time          `json:"started,omitzero"`
	ExpiresAt  time.Time          `json:"expiresAt,omitzero"`
	PreviewURL string             `json:"previewUrl,omitempty"`
	Password   bool               `json:"password"`
	// Peers are WebRTC connections with their network path details.
	Peers []rtc.PeerInfo `json:"peers"`
	// PublicIP is this host's public address as seen from the internet.
	PublicIP string `json:"publicIp,omitempty"`
}

// Engine runs at most one sharing session at a time.
type Engine struct {
	store *config.Store
	log   *slog.Logger

	mu       sync.Mutex
	st       Status
	settings config.Settings
	onChange func()

	// live session
	cancel    context.CancelFunc
	ctx       context.Context
	root      *fsroot.Root
	files     *fileserver.Server
	tracker   *transfers.Tracker
	coord     *coord.Client
	rtcHost   *rtc.Host
	preview   *http.Server
	direct    *directServer
	mapper    *network.Mapper
	mappings  []*network.Mapping
	accessKey string
	pwHash    *security.PasswordHash
	reg       p.Envelope
	ice       []webrtc.ICEServer
	checkMu   sync.Mutex
	device    config.Device
	udpPort   int  // ICE port (all interfaces), mapped on the router
	loopback  bool // tests: include loopback candidates
	wg        sync.WaitGroup
	stunIP    string // public address reported by STUN
}

// New creates an engine.
func New(store *config.Store, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{store: store, log: log, tracker: transfers.New(), mapper: network.DefaultMapper()}
	e.st = idleStatus()
	e.tracker.OnChange(e.changed)
	return e
}

func idleStatus() Status {
	off := Check{State: CheckOff}
	return Status{State: "idle", Headline: "Not sharing", Server: off, Direct: off, P2P: off, Relay: off, Mapping: off, Errors: []string{}}
}

// SetMapper replaces the router port mapper (tests simulate routers).
func (e *Engine) SetMapper(m *network.Mapper) { e.mu.Lock(); e.mapper = m; e.mu.Unlock() }

// OnChange registers a callback for status changes.
func (e *Engine) OnChange(f func()) { e.mu.Lock(); e.onChange = f; e.mu.Unlock() }

func (e *Engine) changed() {
	e.mu.Lock()
	f := e.onChange
	e.mu.Unlock()
	if f != nil {
		f()
	}
}

func (e *Engine) update(f func(*Status)) {
	e.mu.Lock()
	f(&e.st)
	e.recomputeLocked()
	e.mu.Unlock()
	e.changed()
}

// recomputeLocked derives the headline and link readiness. The link is
// only shown once at least one transport has been verified.
func (e *Engine) recomputeLocked() {
	s := &e.st
	if s.State != "sharing" {
		s.LinkReady = false
		return
	}
	ok := func(c Check) bool { return c.State == CheckOK || c.State == CheckWarn }
	s.LinkReady = s.Server.State == CheckOK && (ok(s.Direct) || ok(s.P2P) || ok(s.Relay))
	switch {
	case s.Server.State != CheckOK:
		if s.Server.State == CheckPending {
			s.Headline = "Connecting to the Sharebox server…"
		} else {
			s.Headline = "Offline — cannot reach the Sharebox server"
		}
	case !s.LinkReady && (s.P2P.State == CheckPending || s.Relay.State == CheckPending || s.Direct.State == CheckPending):
		s.Headline = "Checking connectivity…"
	case !s.LinkReady:
		s.Headline = "No working connection method — guests cannot connect"
	case s.Direct.State == CheckOK:
		s.Headline = "Sharing — direct connection available"
	case s.Relay.State == CheckOK:
		s.Headline = "Sharing — guests can always connect"
	default:
		s.Headline = "Sharing — guests on restrictive networks may fail to connect"
	}
}

// Status returns the current snapshot.
func (e *Engine) Status() Status {
	e.tracker.Tick()
	var peers []rtc.PeerInfo
	e.withRTC(func(h *rtc.Host) { peers = h.Peers() })
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.st
	s.Peers = peers
	if s.Peers == nil {
		s.Peers = []rtc.PeerInfo{}
	}
	s.PublicIP = e.stunIP
	if s.PublicIP == "" {
		s.PublicIP = e.reg.ObservedIP
	}
	s.Stats = e.tracker.Snapshot()
	s.Errors = append([]string{}, e.st.Errors...)
	if !s.LinkReady {
		s.Link = ""
	}
	return s
}

func (e *Engine) addError(msg string) {
	e.update(func(s *Status) {
		s.Errors = append(s.Errors, time.Now().Format("15:04:05")+"  "+msg)
		if len(s.Errors) > 20 {
			s.Errors = s.Errors[len(s.Errors)-20:]
		}
	})
}

// ClearErrors empties the error list.
func (e *Engine) ClearErrors() { e.update(func(s *Status) { s.Errors = []string{} }) }

// Running reports whether a session is active.
func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancel != nil
}

// StartOptions are per-session inputs not stored in settings.
type StartOptions struct {
	Password string
	// AllowLoopback is for tests: include loopback ICE candidates.
	AllowLoopback bool
	// NoPortMapping overrides settings (tests).
	NoPortMapping bool
}

func (e *Engine) limits(st config.Settings) fileserver.Limits {
	l := fileserver.DefaultLimits()
	l.MaxUploadBytes = st.MaxUploadMB << 20
	l.MinFreeBytes = st.MinFreeMB << 20
	if st.MaxConcurrent > 0 {
		l.MaxConcurrent = st.MaxConcurrent
	}
	return l
}

// Start begins sharing. Connectivity is established in the background;
// watch Status for progress.
func (e *Engine) Start(st config.Settings, o StartOptions) error {
	if e.Running() {
		return errors.New("already sharing")
	}
	if strings.TrimSpace(st.Folder) == "" {
		return errors.New("choose a folder to share first")
	}
	if strings.TrimSpace(st.ServerURL) == "" {
		return errors.New("no Sharebox server is configured — set one in Settings")
	}
	root, err := fsroot.Open(st.Folder)
	if err != nil {
		return fmt.Errorf("cannot open the folder: %w", err)
	}
	pw, err := security.HashPassword(o.Password)
	if err != nil {
		root.Close()
		return err
	}
	key := security.RandomToken(32)
	var exp time.Time
	if st.ExpiryHours > 0 {
		exp = time.Now().Add(time.Duration(st.ExpiryHours) * time.Hour)
	}
	e.tracker.Reset()
	files, err := fileserver.New(fileserver.Options{
		Root: root, Tracker: e.tracker, Logger: e.log, Web: guestweb.Handler(guestweb.ModeDirect),
		Config: fileserver.Config{Perms: st.Perms, Limits: e.limits(st), AccessKey: key, Password: pw, ExpiresAt: exp},
	})
	if err != nil {
		root.Close()
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.ctx, e.cancel = ctx, cancel
	e.root, e.files, e.accessKey, e.pwHash, e.settings = root, files, key, pw, st
	e.reg, e.ice = p.Envelope{}, nil
	e.device = e.store.Device()
	pending := Check{State: CheckPending}
	e.st = idleStatus()
	e.st.State = "starting"
	e.st.Headline = "Starting…"
	e.st.Server = pending
	e.st.Started = time.Now()
	e.st.ExpiresAt = exp
	e.st.Password = pw != nil
	e.st.Folder = FolderInfo{Path: root.HostPath(), Name: root.Name(), Scanning: true}
	e.mu.Unlock()
	e.changed()

	if err := e.startLocal(ctx, files, o); err != nil {
		e.Stop()
		return err
	}
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.scanFolder(ctx, root) }()

	cc, err := coord.New(st.ServerURL, e.registration, coord.Handlers{
		Registered:  e.onRegistered,
		GuestJoined: func(string) {},
		GuestLeft:   func(peer string) { e.withRTC(func(h *rtc.Host) { h.GuestLeft(peer) }) },
		Signal:      func(peer string, d *p.SignalPayload) { e.withRTC(func(h *rtc.Host) { h.OnSignal(peer, d) }) },
		Status: func(ok bool, err error) {
			if !ok {
				msg := "Connection to the Sharebox server lost; reconnecting"
				if err != nil {
					msg = err.Error()
				}
				e.update(func(s *Status) { s.Server = Check{State: CheckWarn, Detail: msg} })
			}
		},
	}, e.log)
	if err != nil {
		e.Stop()
		return err
	}
	e.mu.Lock()
	e.coord = cc
	e.st.State = "sharing"
	e.mu.Unlock()
	e.update(func(*Status) {})

	files.OnAuth = func(guest, peer string) { e.withRTC(func(h *rtc.Host) { h.GuestAuthenticated(guest, peer) }) }

	e.wg.Add(2)
	go func() {
		defer e.wg.Done()
		// Map ports before announcing ourselves so the first guests already
		// benefit from them.
		if st.PortMapping && !o.NoPortMapping {
			e.mapPorts(ctx)
		}
		cc.Run(ctx)
	}()
	go func() {
		defer e.wg.Done()
		w := &network.Watcher{OnChange: func(reason string) {
			e.log.Info("re-establishing connectivity", "reason", reason)
			e.addError("Network change detected (" + reason + "); reconnecting")
			cc.Reconnect()
			if st.PortMapping && !o.NoPortMapping {
				go e.mapPorts(ctx)
			}
		}}
		w.Run(ctx)
	}()
	return nil
}

func (e *Engine) withRTC(f func(*rtc.Host)) {
	e.mu.Lock()
	h := e.rtcHost
	e.mu.Unlock()
	if h != nil {
		f(h)
	}
}

// startLocal binds the loopback preview server and the ICE UDP socket.
func (e *Engine) startLocal(ctx context.Context, files *fileserver.Server, o StartOptions) error {
	// Loopback preview so the host can see exactly what guests see. It is
	// bound to 127.0.0.1 only and never exposed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	e.preview = &http.Server{Handler: files, ReadHeaderTimeout: 10 * time.Second}
	go e.preview.Serve(ln)

	// One UDP socket for all ICE traffic, on a stable port that can be
	// mapped on the router.
	dev := e.device
	if dev.UDPPort == 0 {
		dev.UDPPort = 40000 + rand.IntN(20000)
		dev.TCPPort = dev.UDPPort
		_ = e.store.SaveDevice(dev)
		e.device = dev
	}
	// Reserve the stable port (or any free one if it's taken); the WebRTC
	// endpoint then opens one socket per real network interface on it.
	port := dev.UDPPort
	if probe, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", port)); err == nil {
		probe.Close()
	} else {
		probe, err := net.ListenPacket("udp4", ":0")
		if err != nil {
			return fmt.Errorf("cannot open a network port: %w", err)
		}
		port = probe.LocalAddr().(*net.UDPAddr).Port
		probe.Close()
	}
	e.udpPort, e.loopback = port, o.AllowLoopback
	h, err := e.newRTC("")
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.rtcHost = h
	e.st.PreviewURL = fmt.Sprintf("http://%s/#k=%s", ln.Addr(), e.accessKey)
	e.mu.Unlock()
	return nil
}

func (e *Engine) newRTC(publicIP string) (*rtc.Host, error) {
	return rtc.New(rtc.Config{
		Handler:   e.files,
		AccessKey: func() string { e.mu.Lock(); defer e.mu.Unlock(); return e.accessKey },
		ICEServers: func() []webrtc.ICEServer {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.ice
		},
		SendSignal: func(peer string, d *p.SignalPayload) {
			e.mu.Lock()
			cc := e.coord
			e.mu.Unlock()
			if cc != nil {
				_ = cc.Send(p.Envelope{Type: p.MsgSignal, Peer: peer, Data: d})
			}
		},
		UDPPort: e.udpPort, PublicIP: publicIP, AllowLoopback: e.loopback, Logger: e.log, OnChange: e.changed,
		PublicIPs: func() []string {
			e.mu.Lock()
			defer e.mu.Unlock()
			return []string{e.stunIP, e.reg.ObservedIP}
		},
	})
}

func (e *Engine) registration() coord.Registration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return coord.Registration{
		SignalHash: security.SignalHash(security.SignalToken(e.accessKey)),
		ExpiresAt:  e.st.ExpiresAt, DeviceID: e.device.ID, DeviceKey: e.device.Key,
	}
}

func (e *Engine) onRegistered(m p.Envelope) {
	var ice []webrtc.ICEServer
	for _, s := range m.ICEServers {
		ice = append(ice, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	e.mu.Lock()
	e.reg, e.ice = m, ice
	key := e.accessKey
	e.st.Link = m.URL + "#k=" + key
	e.mu.Unlock()
	e.update(func(s *Status) { s.Server = Check{State: CheckOK, Detail: "Connected"} })
	go e.verify()
}

// verify checks every transport for real and updates the status. It is
// re-run after reconnects and network changes.
func (e *Engine) verify() {
	if !e.checkMu.TryLock() {
		return
	}
	defer e.checkMu.Unlock()
	e.mu.Lock()
	ctx, reg := e.ctx, e.reg
	e.mu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		return
	}
	e.update(func(s *Status) {
		s.P2P = Check{State: CheckPending}
		s.Relay = Check{State: CheckPending}
	})
	var stunHosts []string
	var turnURL *network.ICEURL
	var turnUser, turnPass string
	for _, s := range reg.ICEServers {
		for _, raw := range s.URLs {
			u, err := network.ParseICEURL(raw)
			if err != nil {
				continue
			}
			if u.Transport == "udp" {
				stunHosts = append(stunHosts, u.Host) // TURN servers answer STUN too
			}
			if (u.Scheme == "turn") && u.Transport == "udp" && turnURL == nil {
				uu := u
				turnURL, turnUser, turnPass = &uu, s.Username, s.Credential
			}
		}
	}

	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if len(stunHosts) == 0 {
			// Without STUN, only same-network guests can connect directly.
			e.update(func(s *Status) {
				s.P2P = Check{State: CheckWarn, Detail: "No STUN server configured; only guests on your local network can connect peer-to-peer"}
			})
			return
		}
		rep, err := network.ProbeNAT(cctx, stunHosts)
		switch {
		case err != nil:
			e.update(func(s *Status) { s.P2P = Check{State: CheckUnavailable, Detail: "STUN check failed: " + err.Error()} })
		case rep.Symmetric:
			e.mu.Lock()
			e.stunIP = rep.PublicIP
			e.mu.Unlock()
			e.update(func(s *Status) {
				s.P2P = Check{State: CheckWarn, Detail: "Your network uses strict (symmetric) NAT; most guests will need the relay"}
			})
		default:
			e.mu.Lock()
			e.stunIP = rep.PublicIP
			e.mu.Unlock()
			e.update(func(s *Status) { s.P2P = Check{State: CheckOK, Detail: "Public address " + rep.PublicIP} })
		}
	}()
	go func() {
		defer wg.Done()
		if turnURL == nil {
			e.update(func(s *Status) {
				s.Relay = Check{State: CheckUnavailable, Detail: "The server has no relay configured"}
			})
			return
		}
		addr, err := network.TURNCheck(cctx, *turnURL, turnUser, turnPass)
		if err != nil {
			e.update(func(s *Status) { s.Relay = Check{State: CheckUnavailable, Detail: err.Error()} })
			return
		}
		e.update(func(s *Status) { s.Relay = Check{State: CheckOK, Detail: "Relay allocation verified (" + addr + ")"} })
	}()
	wg.Wait()
	e.verifyDirect(ctx)
}

func (e *Engine) scanFolder(ctx context.Context, root *fsroot.Root) {
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var size int64
	var count int
	last := time.Now()
	err := root.Walk(sctx, "", func(en fsroot.Entry) error {
		if !en.IsDir {
			size += en.Size
			count++
		}
		if count > 2_000_000 {
			return errors.New("stop")
		}
		if time.Since(last) > 500*time.Millisecond {
			last = time.Now()
			e.update(func(s *Status) { s.Folder.Size, s.Folder.Files = size, count })
		}
		return nil
	})
	if ctx.Err() != nil {
		return
	}
	e.update(func(s *Status) {
		s.Folder.Size, s.Folder.Files, s.Folder.Scanning = size, count, false
		s.Folder.Partial = err != nil
	})
}

// RescanFolder refreshes folder statistics.
func (e *Engine) RescanFolder() {
	e.mu.Lock()
	ctx, root := e.ctx, e.root
	e.mu.Unlock()
	if root != nil {
		go e.scanFolder(ctx, root)
	}
}

// Stop ends sharing immediately: guests are disconnected, the link is
// invalidated on the server, port mappings are removed and incomplete
// uploads are deleted.
func (e *Engine) Stop() {
	e.mu.Lock()
	if e.cancel == nil {
		e.mu.Unlock()
		return
	}
	e.st.State = "stopping"
	cc, h, files, root, prev, dir := e.coord, e.rtcHost, e.files, e.root, e.preview, e.direct
	maps, cancel := e.mappings, e.cancel
	e.mu.Unlock()
	e.changed()

	if files != nil {
		files.Update(func(c *fileserver.Config) { c.AccessKey = "" })
		files.RevokeAll()
	}
	if cc != nil {
		ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = cc.Request(ctx, p.Envelope{Type: p.MsgUnregister})
		c()
	}
	cancel()
	if h != nil {
		h.Close()
	}
	if dir != nil {
		dir.close()
	}
	if prev != nil {
		prev.Close()
	}
	e.unmap(maps)
	e.wg.Wait()
	if files != nil {
		files.Close()
	}
	if root != nil {
		root.Close()
	}
	e.tracker.Reset()
	e.mu.Lock()
	e.cancel, e.ctx, e.coord, e.rtcHost, e.files, e.root, e.preview, e.direct = nil, nil, nil, nil, nil, nil, nil, nil
	e.mappings, e.udpPort, e.accessKey = nil, 0, ""
	e.st = idleStatus()
	e.mu.Unlock()
	e.changed()
}

// RegenerateLink invalidates the current link and every guest's access, and
// issues a brand-new link (new session id and key).
func (e *Engine) RegenerateLink() error {
	e.mu.Lock()
	if e.cancel == nil {
		e.mu.Unlock()
		return errors.New("not sharing")
	}
	key := security.RandomToken(32)
	e.accessKey = key
	files, cc, h := e.files, e.coord, e.rtcHost
	pub := ""
	if h != nil && e.st.Mapping.State == CheckOK {
		pub = e.reg.ObservedIP
	}
	e.st.Link = ""
	e.mu.Unlock()

	files.Update(func(c *fileserver.Config) { c.AccessKey = key })
	files.RevokeAll()
	e.tracker.Reset()
	if h != nil {
		h.Close() // closes the ICE sockets, freeing the port for the new endpoint
		nh, err := e.newRTC(pub)
		if err == nil {
			e.mu.Lock()
			e.rtcHost = nh
			e.mu.Unlock()
		}
	}
	cc.ForgetSession()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cc.Reregister(ctx); err != nil {
		e.addError("Could not register the new link: " + err.Error())
		return err
	}
	return nil
}

// RevokeGuest ends one guest's access and closes its connection.
func (e *Engine) RevokeGuest(id string) {
	e.mu.Lock()
	files := e.files
	e.mu.Unlock()
	if files != nil {
		files.RevokeGuest(id)
	}
	e.withRTC(func(h *rtc.Host) { h.DropGuest(id) })
	e.changed()
}

// CancelTransfer aborts an active transfer.
func (e *Engine) CancelTransfer(id string) bool { return e.tracker.Cancel(id) }

// UpdateSettings applies settings that can change while sharing
// (permissions and limits) and returns whether a restart is needed for the
// rest.
func (e *Engine) UpdateSettings(st config.Settings) {
	e.mu.Lock()
	files := e.files
	e.settings.Perms = st.Perms
	e.mu.Unlock()
	if files != nil {
		l := e.limits(st)
		files.Update(func(c *fileserver.Config) { c.Perms = st.Perms; c.Limits = l })
	}
	e.changed()
}

// SetPassword changes (or clears) the share password for new logins.
func (e *Engine) SetPassword(pw string) error {
	h, err := security.HashPassword(pw)
	if err != nil {
		return err
	}
	e.mu.Lock()
	files := e.files
	e.pwHash = h
	e.st.Password = h != nil
	e.mu.Unlock()
	if files != nil {
		files.Update(func(c *fileserver.Config) { c.Password = h })
	}
	e.changed()
	return nil
}

// SessionID returns the coordination session id (tests).
func (e *Engine) SessionID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reg.SessionID
}

// AccessKey returns the current key (tests).
func (e *Engine) AccessKey() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.accessKey
}

// Files returns the file service (tests).
func (e *Engine) Files() *fileserver.Server {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.files
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
