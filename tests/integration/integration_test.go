// Package integration runs the real coordination server, TURN relay, host
// engine and a pion WebRTC guest on localhost. The full guest API suite runs
// over a peer-to-peer DataChannel and over a relay-only DataChannel.
package integration

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sharebox/internal/config"
	"sharebox/internal/conformance"
	"sharebox/internal/fileserver"
	"sharebox/internal/host"
	"sharebox/internal/rtc"
	"sharebox/internal/tunnel"
	"sharebox/services/relay"
	"sharebox/services/signaling"
)

type infra struct {
	url   string
	relay *relay.Server
	srv   *signaling.Server
	http  *httptest.Server
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func freeUDPPort(t *testing.T) int {
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func startInfra(t *testing.T, withTURN bool) *infra {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	cfg := signaling.Config{PublicURL: url, Dev: true, Logger: quietLogger(), ConnectRate: 1000}
	in := &infra{url: url}
	if withTURN {
		port := freeUDPPort(t)
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		secret := "test-secret-0123456789"
		r, err := relay.Start(relay.Config{PublicIP: "127.0.0.1", ListenUDP: addr, Secret: secret,
			AllowPrivatePeers: true, Logger: quietLogger()})
		if err != nil {
			t.Fatal(err)
		}
		in.relay = r
		cfg.TURNSecret = secret
		cfg.TURNURLs = []string{"turn:" + addr + "?transport=udp"}
		cfg.STUNURLs = []string{"stun:" + addr}
		t.Cleanup(func() { r.Close() })
	}
	srv, err := signaling.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	in.srv = srv
	in.http = &httptest.Server{Listener: ln, Config: &http.Server{Handler: srv}}
	in.http.Start()
	t.Cleanup(func() { in.http.Close(); srv.Close() })
	return in
}

func startHost(t *testing.T, in *infra, dir string) *host.Engine {
	t.Helper()
	store := &config.Store{Dir: t.TempDir()}
	e := host.New(store, quietLogger())
	st := config.Defaults()
	st.Folder, st.ServerURL, st.PortMapping = dir, in.url, false
	if err := e.Start(st, host.StartOptions{AllowLoopback: true, NoPortMapping: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Stop)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s := e.Status()
		if s.LinkReady && s.P2P.State != host.CheckPending && s.Relay.State != host.CheckPending {
			return e
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("link never became ready: %+v", e.Status())
	return nil
}

func dial(t *testing.T, in *infra, e *host.Engine, relayOnly bool) (*tunnel.Session, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, kind, err := rtc.Dial(ctx, in.url, e.SessionID(), e.AccessKey(), rtc.GuestOptions{RelayOnly: relayOnly, AllowLoopback: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { sess.Close(nil) })
	return sess, kind
}

func factory(in *infra, relayOnly bool) conformance.Factory {
	return func(t *testing.T, dir string, cfg fileserver.Config) *conformance.Env {
		e := startHost(t, in, dir)
		e.Files().Update(func(c *fileserver.Config) {
			c.Perms, c.Limits, c.AccessKey, c.Password, c.ExpiresAt = cfg.Perms, cfg.Limits, cfg.AccessKey, cfg.Password, cfg.ExpiresAt
		})
		sess, kind := dial(t, in, e, relayOnly)
		want := "p2p"
		if relayOnly {
			want = "relay"
		}
		if kind != want {
			t.Fatalf("connected via %s, want %s", kind, want)
		}
		return &conformance.Env{Client: &http.Client{Transport: sess}, BaseURL: "http://tunnel", Dir: dir, Server: e.Files()}
	}
}

func TestWebRTCPeerToPeer(t *testing.T) {
	in := startInfra(t, true)
	conformance.Run(t, factory(in, false))
}

func TestWebRTCRelayOnly(t *testing.T) {
	in := startInfra(t, true)
	conformance.Run(t, factory(in, true))
}

func TestStatusReportsVerifiedTransports(t *testing.T) {
	in := startInfra(t, true)
	e := startHost(t, in, t.TempDir())
	s := e.Status()
	if s.Server.State != host.CheckOK || s.Relay.State != host.CheckOK || s.P2P.State != host.CheckOK {
		t.Fatalf("unexpected checks: server=%v relay=%v p2p=%v", s.Server, s.Relay, s.P2P)
	}
	if !strings.HasPrefix(s.Link, in.url+"/s/") || !strings.Contains(s.Link, "#k=") {
		t.Fatalf("bad link %q", s.Link)
	}
}

func TestNoRelayConfigured(t *testing.T) {
	in := startInfra(t, false)
	e := startHost(t, in, t.TempDir())
	s := e.Status()
	if s.Relay.State != host.CheckUnavailable {
		t.Fatalf("relay should be unavailable: %+v", s.Relay)
	}
	// Peer-to-peer still works on the same machine (host candidates).
	sess, kind := dial(t, in, e, false)
	if kind != "p2p" {
		t.Fatal(kind)
	}
	res, err := (&http.Client{Transport: sess}).Get("http://tunnel/api/ping")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("ping: %v", err)
	}
}

func TestStopRevokesAccess(t *testing.T) {
	in := startInfra(t, true)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	e := startHost(t, in, dir)
	sid, key := e.SessionID(), e.AccessKey()
	sess, _ := dial(t, in, e, false)
	e.Stop()
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("guest connection survived Stop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := rtc.Dial(ctx, in.url, sid, key, rtc.GuestOptions{AllowLoopback: true}); err == nil {
		t.Fatal("could still connect after Stop")
	}
}

func TestRegenerateLinkInvalidatesOldLink(t *testing.T) {
	in := startInfra(t, true)
	e := startHost(t, in, t.TempDir())
	oldSID, oldKey := e.SessionID(), e.AccessKey()
	sess, _ := dial(t, in, e, false)
	if err := e.RegenerateLink(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("old guest still connected")
	}
	if e.SessionID() == oldSID || e.AccessKey() == oldKey {
		t.Fatal("link did not change")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := rtc.Dial(ctx, in.url, oldSID, oldKey, rtc.GuestOptions{AllowLoopback: true}); err == nil {
		t.Fatal("old link still works")
	}
	// The new link works.
	sess2, _ := dial(t, in, e, false)
	res, err := (&http.Client{Transport: sess2}).Get("http://tunnel/api/ping")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("new link: %v", err)
	}
}

func TestWrongKeyCannotJoin(t *testing.T) {
	in := startInfra(t, true)
	e := startHost(t, in, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := rtc.Dial(ctx, in.url, e.SessionID(), "wrong-key", rtc.GuestOptions{AllowLoopback: true}); err == nil ||
		!strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}

// When the host's connection to the coordination server drops (network
// change, sleep), it reconnects and resumes the same session: the existing
// link keeps working.
func TestHostReconnectKeepsLink(t *testing.T) {
	in := startInfra(t, true)
	e := startHost(t, in, t.TempDir())
	sid := e.SessionID()
	// Simulate an outage by closing all server-side connections.
	in.http.CloseClientConnections()
	time.Sleep(200 * time.Millisecond)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && e.Status().Server.State != host.CheckOK {
		time.Sleep(100 * time.Millisecond)
	}
	if e.SessionID() != sid {
		t.Fatalf("session changed after reconnect: %s -> %s", sid, e.SessionID())
	}
	sess, _ := dial(t, in, e, false)
	res, err := (&http.Client{Transport: sess}).Get("http://tunnel/api/ping")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("after reconnect: %v", err)
	}
}

// An established DataChannel survives the loss of the signalling server.
func TestTransferSurvivesSignalingOutage(t *testing.T) {
	in := startInfra(t, true)
	dir := t.TempDir()
	data := make([]byte, 8<<20)
	os.WriteFile(filepath.Join(dir, "big.bin"), data, 0o644)
	e := startHost(t, in, dir)
	sess, _ := dial(t, in, e, false)
	c, _ := conformance.Login(t, &conformance.Env{Client: &http.Client{Transport: sess}, BaseURL: "http://tunnel"}, e.AccessKey(), "")
	in.http.CloseClientConnections()
	req, _ := http.NewRequest("GET", "http://tunnel/api/download/b?path=big.bin", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := (&http.Client{Transport: sess}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, res.Body)
	if err != nil || n != int64(len(data)) {
		t.Fatalf("download after outage: %d %v", n, err)
	}
}
