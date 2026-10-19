// Package rtc accepts WebRTC connections from guests and serves the file API
// over a DataChannel tunnel. ICE (host, STUN server-reflexive and TURN relay
// candidates) picks the best working path automatically; the selected
// candidate pair tells us whether the guest is connected peer-to-peer or
// through the relay.
package rtc

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"sharebox/internal/fileserver"
	"sharebox/internal/security"
	"sharebox/internal/tunnel"
	p "sharebox/shared/protocol"
)

// Config for the host-side WebRTC endpoint.
type Config struct {
	Handler http.Handler
	// AccessKey returns the current key (used to authenticate the DTLS
	// fingerprint to guests).
	AccessKey func() string
	// ICEServers returns the current STUN/TURN configuration.
	ICEServers func() []webrtc.ICEServer
	// SendSignal delivers a payload to a guest via the coordination server.
	SendSignal func(peer string, d *p.SignalPayload)
	// UDPConn, if set, is used for all ICE traffic (single port), which lets
	// the host map one UDP port on the router.
	UDPConn net.PacketConn
	// PublicIP is advertised as a server-reflexive candidate on UDPConn's
	// port when a router port mapping exists.
	PublicIP string
	// AllowLoopback includes loopback candidates (tests only).
	AllowLoopback bool
	Logger        *slog.Logger
	// OnChange is called when peers connect, disconnect or change path.
	OnChange func()
}

// Host manages guest peer connections.
type Host struct {
	cfg Config
	api *webrtc.API
	log *slog.Logger
	mux ice.UDPMux

	mu    sync.Mutex
	peers map[string]*peer
}

type peer struct {
	id        string
	pc        *webrtc.PeerConnection
	sess      *tunnel.Session
	transport atomic.Value // string
	pending   []webrtc.ICECandidateInit
	haveRD    bool
	created   time.Time
	guests    map[string]bool
}

// PeerInfo describes a connected peer.
type PeerInfo struct {
	ID        string
	Transport string
	Connected bool
}

// New creates a host endpoint.
func New(cfg Config) (*Host, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	se := webrtc.SettingEngine{}
	se.SetICETimeouts(6*time.Second, 20*time.Second, 2*time.Second)
	se.SetSCTPMaxReceiveBufferSize(8 << 20)
	if cfg.AllowLoopback {
		se.SetIncludeLoopbackCandidate(true)
	}
	// Do not reveal local addresses through mDNS names to guests; plain host
	// candidates are needed for LAN connections.
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	h := &Host{cfg: cfg, log: cfg.Logger, peers: map[string]*peer{}}
	if cfg.UDPConn != nil {
		h.mux = webrtc.NewICEUDPMux(nil, cfg.UDPConn)
		se.SetICEUDPMux(h.mux)
		se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
		if cfg.PublicIP != "" {
			se.SetNAT1To1IPs([]string{cfg.PublicIP}, webrtc.ICECandidateTypeSrflx)
		}
	}
	h.api = webrtc.NewAPI(webrtc.WithSettingEngine(se))
	return h, nil
}

// Close disconnects every guest.
func (h *Host) Close() {
	h.mu.Lock()
	peers := h.peers
	h.peers = map[string]*peer{}
	h.mu.Unlock()
	for _, pr := range peers {
		h.closePeer(pr, "host stopped sharing")
	}
	if h.mux != nil {
		h.mux.Close()
	}
}

// Peers lists peer connections.
func (h *Host) Peers() []PeerInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []PeerInfo
	for _, pr := range h.peers {
		t, _ := pr.transport.Load().(string)
		out = append(out, PeerInfo{ID: pr.id, Transport: t, Connected: pr.sess != nil})
	}
	return out
}

// GuestAuthenticated links a file-API guest id to the peer it came from, so
// revoking the guest can close its connection.
func (h *Host) GuestAuthenticated(guest, peerTag string) {
	id, ok := strings.CutPrefix(peerTag, "rtc:")
	if !ok {
		return
	}
	h.mu.Lock()
	if pr := h.peers[id]; pr != nil {
		pr.guests[guest] = true
	}
	h.mu.Unlock()
}

// DropGuest closes connections used by a guest id.
func (h *Host) DropGuest(guest string) {
	h.mu.Lock()
	var victims []*peer
	for id, pr := range h.peers {
		if pr.guests[guest] {
			victims = append(victims, pr)
			delete(h.peers, id)
		}
	}
	h.mu.Unlock()
	for _, pr := range victims {
		h.closePeer(pr, "access revoked")
	}
}

// GuestLeft is called when a guest's signalling connection ends. An
// established DataChannel keeps working without signalling, so only peers
// that never connected are cleaned up.
func (h *Host) GuestLeft(id string) {
	h.mu.Lock()
	pr := h.peers[id]
	if pr != nil && pr.sess == nil {
		delete(h.peers, id)
	} else {
		pr = nil
	}
	h.mu.Unlock()
	if pr != nil {
		h.closePeer(pr, "")
	}
}

func (h *Host) closePeer(pr *peer, why string) {
	if pr.sess != nil {
		pr.sess.Close(errors.New(why))
	}
	pr.pc.Close()
	if h.cfg.OnChange != nil {
		h.cfg.OnChange()
	}
}

// OnSignal handles a signalling message from a guest.
func (h *Host) OnSignal(id string, d *p.SignalPayload) {
	switch d.Kind {
	case "offer":
		go h.handleOffer(id, d)
	case "candidate":
		h.mu.Lock()
		pr := h.peers[id]
		if pr == nil {
			h.mu.Unlock()
			return
		}
		c := webrtc.ICECandidateInit{Candidate: d.Candidate, SDPMid: &d.SDPMid}
		if d.SDPMLine != nil {
			v := uint16(*d.SDPMLine)
			c.SDPMLineIndex = &v
		}
		if !pr.haveRD {
			if len(pr.pending) < 64 {
				pr.pending = append(pr.pending, c)
			}
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()
		if err := pr.pc.AddICECandidate(c); err != nil {
			h.log.Debug("bad ICE candidate from guest", "err", err)
		}
	case "bye":
		h.mu.Lock()
		pr := h.peers[id]
		delete(h.peers, id)
		h.mu.Unlock()
		if pr != nil {
			h.closePeer(pr, "guest left")
		}
	}
}

var fpRe = regexp.MustCompile(`(?m)^a=fingerprint:sha-256 ([0-9A-Fa-f:]+)`)

func (h *Host) handleOffer(id string, d *p.SignalPayload) {
	h.mu.Lock()
	if old := h.peers[id]; old != nil {
		delete(h.peers, id)
		h.mu.Unlock()
		h.closePeer(old, "renegotiating")
		h.mu.Lock()
	}
	if len(h.peers) >= 64 {
		h.mu.Unlock()
		h.log.Warn("rejecting guest: too many peers")
		return
	}
	cfg := webrtc.Configuration{ICEServers: h.cfg.ICEServers()}
	if d.Policy == "relay" {
		cfg.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	pc, err := h.api.NewPeerConnection(cfg)
	if err != nil {
		h.mu.Unlock()
		h.log.Error("cannot create peer connection", "err", err)
		return
	}
	pr := &peer{id: id, pc: pc, created: time.Now(), guests: map[string]bool{}}
	pr.transport.Store("")
	h.peers[id] = pr
	h.mu.Unlock()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		ci := c.ToJSON()
		sp := &p.SignalPayload{Kind: "candidate", Candidate: ci.Candidate}
		if ci.SDPMid != nil {
			sp.SDPMid = *ci.SDPMid
		}
		if ci.SDPMLineIndex != nil {
			v := int(*ci.SDPMLineIndex)
			sp.SDPMLine = &v
		}
		h.cfg.SendSignal(id, sp)
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			h.mu.Lock()
			if h.peers[id] == pr {
				delete(h.peers, id)
			}
			h.mu.Unlock()
			if pr.sess != nil {
				pr.sess.Close(errors.New("connection " + s.String()))
			}
			pc.Close()
			if h.cfg.OnChange != nil {
				h.cfg.OnChange()
			}
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != p.TunnelLabel {
			dc.Close()
			return
		}
		dc.OnOpen(func() { h.attach(pr, dc) })
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: d.SDP}); err != nil {
		h.log.Warn("invalid offer", "err", err)
		h.dropPeer(pr)
		return
	}
	h.mu.Lock()
	pr.haveRD = true
	pending := pr.pending
	pr.pending = nil
	h.mu.Unlock()
	for _, c := range pending {
		_ = pc.AddICECandidate(c)
	}
	ans, err := pc.CreateAnswer(nil)
	if err == nil {
		err = pc.SetLocalDescription(ans)
	}
	if err != nil {
		h.log.Warn("cannot answer", "err", err)
		h.dropPeer(pr)
		return
	}
	sdp := pc.LocalDescription().SDP
	fp := ""
	if m := fpRe.FindStringSubmatch(sdp); m != nil {
		fp = m[1]
	}
	h.cfg.SendSignal(id, &p.SignalPayload{Kind: "answer", SDP: sdp,
		FingerprintMAC: security.FingerprintMAC(h.cfg.AccessKey(), fp)})

	// Give up on peers that never connect.
	time.AfterFunc(45*time.Second, func() {
		h.mu.Lock()
		stale := h.peers[id] == pr && pr.sess == nil
		h.mu.Unlock()
		if stale {
			h.dropPeer(pr)
		}
	})
}

func (h *Host) dropPeer(pr *peer) {
	h.mu.Lock()
	if h.peers[pr.id] == pr {
		delete(h.peers, pr.id)
	}
	h.mu.Unlock()
	h.closePeer(pr, "")
}

type dcLink struct{ dc *webrtc.DataChannel }

func (l dcLink) Send(b []byte) error    { return l.dc.Send(b) }
func (l dcLink) BufferedAmount() uint64 { return l.dc.BufferedAmount() }
func (l dcLink) Close() error           { return l.dc.Close() }

func (h *Host) attach(pr *peer, dc *webrtc.DataChannel) {
	h.updateTransport(pr)
	tag := "rtc:" + pr.id
	kind := func() string {
		t, _ := pr.transport.Load().(string)
		if t == "" {
			return fileserver.TransportP2P
		}
		return t
	}
	sess := tunnel.New(dcLink{dc}, tunnel.Options{
		Handler:     h.cfg.Handler,
		BaseContext: fileserver.WithTransport(context.Background(), kind, tag),
		Logger:      h.log,
		OnClose: func(error) {
			h.mu.Lock()
			if h.peers[pr.id] == pr {
				delete(h.peers, pr.id)
			}
			h.mu.Unlock()
			pr.pc.Close()
			if h.cfg.OnChange != nil {
				h.cfg.OnChange()
			}
		},
	})
	dc.SetBufferedAmountLowThreshold(p.LowWaterMark)
	dc.OnBufferedAmountLow(sess.BufferedLow)
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if m.IsString {
			sess.Close(errors.New("protocol error: text message"))
			return
		}
		sess.Deliver(m.Data)
	})
	dc.OnClose(func() { sess.Close(nil) })
	h.mu.Lock()
	pr.sess = sess
	h.mu.Unlock()
	if sctp := pr.pc.SCTP(); sctp != nil {
		sctp.Transport().ICETransport().OnSelectedCandidatePairChange(func(*webrtc.ICECandidatePair) {
			h.updateTransport(pr)
		})
	}
	h.log.Info("guest connected", "transport", kind())
	if h.cfg.OnChange != nil {
		h.cfg.OnChange()
	}
}

func (h *Host) updateTransport(pr *peer) {
	sctp := pr.pc.SCTP()
	if sctp == nil {
		return
	}
	pair, err := sctp.Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return
	}
	t := fileserver.TransportP2P
	if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
		t = fileserver.TransportRelay
	}
	pr.transport.Store(t)
}
