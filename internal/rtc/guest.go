package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
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

// Dial connects to a share like the browser guest does: it joins through the
// coordination server, negotiates WebRTC, verifies the host's DTLS
// fingerprint MAC and returns a client tunnel session plus the transport
// kind (p2p or relay). It is used by integration tests and diagnostics.
func Dial(ctx context.Context, server, sessionID, accessKey string, o GuestOptions) (*tunnel.Session, string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, "", err
	}
	u.Scheme = strings.Replace(strings.Replace(u.Scheme, "https", "wss", 1), "http", "ws", 1)
	u.Path = "/ws/guest"
	ws, _, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	send := func(m p.Envelope) error {
		b, _ := json.Marshal(m)
		return ws.Write(ctx, websocket.MessageText, b)
	}
	read := func() (p.Envelope, error) {
		var m p.Envelope
		_, b, err := ws.Read(ctx)
		if err == nil {
			err = json.Unmarshal(b, &m)
		}
		return m, err
	}
	if err := send(p.Envelope{Type: p.MsgJoin, SessionID: sessionID, Signal: security.SignalToken(accessKey)}); err != nil {
		return nil, "", err
	}
	joined, err := read()
	if err != nil {
		return nil, "", err
	}
	if joined.Type != p.MsgJoined {
		ws.Close(websocket.StatusNormalClosure, "")
		return nil, "", fmt.Errorf("%s: %s", joined.Code, joined.Message)
	}

	se := webrtc.SettingEngine{}
	if o.AllowLoopback {
		se.SetIncludeLoopbackCandidate(true)
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	cfg := webrtc.Configuration{}
	for _, s := range joined.ICEServers {
		cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	if o.RelayOnly {
		cfg.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, "", err
	}
	ordered := true
	dc, err := pc.CreateDataChannel(p.TunnelLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		pc.Close()
		return nil, "", err
	}
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
		_ = send(p.Envelope{Type: p.MsgSignal, Data: sp})
	})
	offer, err := pc.CreateOffer(nil)
	if err == nil {
		err = pc.SetLocalDescription(offer)
	}
	if err != nil {
		pc.Close()
		return nil, "", err
	}
	policy := ""
	if o.RelayOnly {
		policy = "relay"
	}
	if err := send(p.Envelope{Type: p.MsgSignal, Data: &p.SignalPayload{Kind: "offer", SDP: offer.SDP, Policy: policy}}); err != nil {
		pc.Close()
		return nil, "", err
	}

	opened := make(chan struct{})
	dc.OnOpen(func() { close(opened) })
	errc := make(chan error, 1)
	go func() {
		for {
			m, err := read()
			if err != nil {
				return
			}
			if m.Type == p.MsgError {
				errc <- errors.New(m.Message)
				return
			}
			if m.Type != p.MsgSignal || m.Data == nil {
				continue
			}
			switch m.Data.Kind {
			case "answer":
				fp := ""
				if mm := fpRe.FindStringSubmatch(m.Data.SDP); mm != nil {
					fp = mm[1]
				}
				if !security.Equal(m.Data.FingerprintMAC, security.FingerprintMAC(accessKey, fp)) {
					errc <- errors.New("host identity could not be verified")
					return
				}
				if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.Data.SDP}); err != nil {
					errc <- err
					return
				}
			case "candidate":
				c := webrtc.ICECandidateInit{Candidate: m.Data.Candidate, SDPMid: &m.Data.SDPMid}
				_ = pc.AddICECandidate(c)
			}
		}
	}()
	select {
	case <-opened:
	case err := <-errc:
		pc.Close()
		return nil, "", err
	case <-time.After(30 * time.Second):
		pc.Close()
		return nil, "", errors.New("timed out connecting to the host")
	case <-ctx.Done():
		pc.Close()
		return nil, "", ctx.Err()
	}
	sess := tunnel.New(dcLink{dc}, tunnel.Options{OnClose: func(error) {
		pc.Close()
		ws.Close(websocket.StatusNormalClosure, "")
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
	return sess, kind, nil
}
