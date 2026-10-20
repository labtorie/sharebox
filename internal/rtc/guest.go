package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"sharebox/internal/security"
	"sharebox/internal/tunnel"
	p "sharebox/shared/protocol"
)

// GuestOptions configure Dial.
type GuestOptions struct {
	RelayOnly     bool
	AllowLoopback bool
}

// Guest is a Go implementation of the browser guest: it joins through the
// coordination server, negotiates WebRTC connections (optionally several in
// parallel), and verifies the host's DTLS fingerprint MAC. It is used by
// integration tests and diagnostics.
type Guest struct {
	ws        *websocket.Conn
	key       string
	opts      GuestOptions
	api       *webrtc.API
	ice       []webrtc.ICEServer
	ctx       context.Context
	cancel    context.CancelFunc
	sendMu    sync.Mutex
	mu        sync.Mutex
	conns     map[int]*guestConn
	Parallel  int // connections the host allows (1 for hosts without support)
	closeOnce sync.Once
	errc      chan error
}

type guestConn struct {
	pc     *webrtc.PeerConnection
	sess   *tunnel.Session
	kind   string
	signal chan *p.SignalPayload
}

// Dial connects the primary connection and returns its tunnel session and
// transport kind (p2p or relay). Closing the session closes the guest.
func Dial(ctx context.Context, server, sessionID, accessKey string, o GuestOptions) (*tunnel.Session, string, error) {
	g, err := DialGuest(ctx, server, sessionID, accessKey, o)
	if err != nil {
		return nil, "", err
	}
	sess, kind := g.Session(0)
	if sess == nil {
		return nil, "", errors.New("connection closed")
	}
	return sess, kind, nil
}

// DialGuest joins a share and opens the primary connection.
func DialGuest(ctx context.Context, server, sessionID, accessKey string, o GuestOptions) (*Guest, error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, err
	}
	u.Scheme = strings.Replace(strings.Replace(u.Scheme, "https", "wss", 1), "http", "ws", 1)
	u.Path = "/ws/guest"
	ws, _, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		return nil, err
	}
	gctx, cancel := context.WithCancel(context.Background())
	g := &Guest{ws: ws, key: accessKey, opts: o, ctx: gctx, cancel: cancel, conns: map[int]*guestConn{}, Parallel: 1, errc: make(chan error, 1)}
	if err := g.send(p.Envelope{Type: p.MsgJoin, SessionID: sessionID, Signal: security.SignalToken(accessKey)}); err != nil {
		g.Close()
		return nil, err
	}
	var joined p.Envelope
	if _, b, err := ws.Read(ctx); err != nil || json.Unmarshal(b, &joined) != nil {
		g.Close()
		return nil, fmt.Errorf("join failed: %v", err)
	}
	if joined.Type != p.MsgJoined {
		g.Close()
		return nil, fmt.Errorf("%s: %s", joined.Code, joined.Message)
	}
	for _, s := range joined.ICEServers {
		g.ice = append(g.ice, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	se := webrtc.SettingEngine{}
	if o.AllowLoopback {
		se.SetIncludeLoopbackCandidate(true)
	}
	g.api = webrtc.NewAPI(webrtc.WithSettingEngine(se))
	go g.readLoop()
	if _, _, err := g.Open(ctx, 0); err != nil {
		g.Close()
		return nil, err
	}
	return g, nil
}

func (g *Guest) send(m p.Envelope) error {
	g.sendMu.Lock()
	defer g.sendMu.Unlock()
	b, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return g.ws.Write(ctx, websocket.MessageText, b)
}

// readLoop routes signalling messages to the connection they belong to.
func (g *Guest) readLoop() {
	for {
		var m p.Envelope
		_, b, err := g.ws.Read(g.ctx)
		if err != nil {
			return
		}
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if m.Type == p.MsgError {
			select {
			case g.errc <- errors.New(m.Message):
			default:
			}
			continue
		}
		if m.Type != p.MsgSignal || m.Data == nil {
			continue
		}
		g.mu.Lock()
		c := g.conns[m.Data.Conn]
		g.mu.Unlock()
		if c != nil {
			select {
			case c.signal <- m.Data:
			default:
			}
		}
	}
}

// Open negotiates connection number conn and returns its session and kind.
// Connections other than 0 are only accepted by hosts that advertise
// parallel support (Guest.Parallel > conn).
func (g *Guest) Open(ctx context.Context, conn int) (*tunnel.Session, string, error) {
	cfg := webrtc.Configuration{ICEServers: g.ice}
	if g.opts.RelayOnly {
		cfg.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	pc, err := g.api.NewPeerConnection(cfg)
	if err != nil {
		return nil, "", err
	}
	c := &guestConn{pc: pc, signal: make(chan *p.SignalPayload, 64)}
	g.mu.Lock()
	g.conns[conn] = c
	g.mu.Unlock()
	fail := func(err error) (*tunnel.Session, string, error) {
		pc.Close()
		g.mu.Lock()
		delete(g.conns, conn)
		g.mu.Unlock()
		return nil, "", err
	}
	ordered := true
	dc, err := pc.CreateDataChannel(p.TunnelLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return fail(err)
	}
	pc.OnICECandidate(func(ic *webrtc.ICECandidate) {
		if ic == nil {
			return
		}
		ci := ic.ToJSON()
		sp := &p.SignalPayload{Kind: "candidate", Candidate: ci.Candidate, Conn: conn}
		if ci.SDPMid != nil {
			sp.SDPMid = *ci.SDPMid
		}
		if ci.SDPMLineIndex != nil {
			v := int(*ci.SDPMLineIndex)
			sp.SDPMLine = &v
		}
		_ = g.send(p.Envelope{Type: p.MsgSignal, Data: sp})
	})
	offer, err := pc.CreateOffer(nil)
	if err == nil {
		err = pc.SetLocalDescription(offer)
	}
	if err != nil {
		return fail(err)
	}
	policy := ""
	if g.opts.RelayOnly {
		policy = "relay"
	}
	if err := g.send(p.Envelope{Type: p.MsgSignal, Data: &p.SignalPayload{Kind: "offer", SDP: offer.SDP, Policy: policy, Conn: conn}}); err != nil {
		return fail(err)
	}
	opened := make(chan struct{})
	dc.OnOpen(func() { close(opened) })
	timeout := time.After(30 * time.Second)
	for {
		select {
		case d := <-c.signal:
			switch d.Kind {
			case "answer":
				fp := ""
				if mm := fpRe.FindStringSubmatch(d.SDP); mm != nil {
					fp = mm[1]
				}
				if !security.Equal(d.FingerprintMAC, security.FingerprintMAC(g.key, fp)) {
					return fail(errors.New("host identity could not be verified"))
				}
				if conn == 0 && d.Parallel > 1 {
					g.Parallel = d.Parallel
				}
				if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: d.SDP}); err != nil {
					return fail(err)
				}
			case "candidate":
				_ = pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: d.Candidate, SDPMid: &d.SDPMid})
			}
			continue
		case <-opened:
		case err := <-g.errc:
			return fail(err)
		case <-timeout:
			return fail(errors.New("timed out connecting to the host"))
		case <-ctx.Done():
			return fail(ctx.Err())
		}
		break
	}
	sess := tunnel.New(dcLink{dc}, tunnel.Options{OnClose: func(error) {
		pc.Close()
		if conn == 0 {
			// Not inline: Close itself closes this session, and calling it
			// again from inside its sync.Once would deadlock.
			go g.Close()
		}
	}})
	dc.SetBufferedAmountLowThreshold(p.LowWaterMark)
	dc.OnBufferedAmountLow(sess.BufferedLow)
	dc.OnMessage(func(m webrtc.DataChannelMessage) { sess.Deliver(m.Data) })
	dc.OnClose(func() { sess.Close(nil) })
	kind := "p2p"
	if pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
		if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
			kind = "relay"
		}
	}
	g.mu.Lock()
	c.sess, c.kind = sess, kind
	g.mu.Unlock()
	// Answers and candidates for this connection were consumed above; keep
	// draining late candidates.
	go func() {
		for {
			select {
			case d := <-c.signal:
				if d.Kind == "candidate" {
					_ = pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: d.Candidate, SDPMid: &d.SDPMid})
				}
			case <-g.ctx.Done():
				return
			}
		}
	}()
	return sess, kind, nil
}

// Session returns connection conn's tunnel session and transport kind.
func (g *Guest) Session(conn int) (*tunnel.Session, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c := g.conns[conn]; c != nil {
		return c.sess, c.kind
	}
	return nil, ""
}

// Close ends all connections.
func (g *Guest) Close() {
	g.closeOnce.Do(func() {
		g.cancel()
		g.mu.Lock()
		conns := g.conns
		g.conns = map[int]*guestConn{}
		g.mu.Unlock()
		for _, c := range conns {
			if c.sess != nil {
				c.sess.Close(nil)
			}
			c.pc.Close()
		}
		g.ws.Close(websocket.StatusNormalClosure, "")
	})
}
