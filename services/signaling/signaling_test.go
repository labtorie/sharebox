package signaling

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/miekg/dns"

	"sharebox/internal/security"
	p "sharebox/shared/protocol"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func freePort(t *testing.T) int {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func TestPublicURLMustBeHTTPS(t *testing.T) {
	for _, u := range []string{"http://share.example.org", "ftp://x", ""} {
		if _, err := New(Config{PublicURL: u, Logger: quiet()}); err == nil {
			t.Errorf("accepted %q", u)
		}
	}
	for _, u := range []string{"https://share.example.org", "http://localhost:8080", "http://127.0.0.1:1"} {
		if _, err := New(Config{PublicURL: u, Logger: quiet()}); err != nil {
			t.Errorf("rejected %q: %v", u, err)
		}
	}
}

// The reachability probe never targets anything but the caller's own public
// address, so it cannot be used to scan other hosts.
func TestProbeRefusesPrivateTargets(t *testing.T) {
	s, _ := New(Config{PublicURL: "https://share.example.org", Logger: quiet()})
	defer s.Close()
	for _, ip := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "169.254.169.254"} {
		if ok, _, err := s.probe(context.Background(), ip, 8443, strings.Repeat("n", 20), ""); ok || err == nil {
			t.Errorf("probe to %s allowed", ip)
		}
	}
	if _, _, err := s.probe(context.Background(), "203.0.113.5", 22, strings.Repeat("n", 20), ""); err == nil {
		t.Error("privileged port allowed")
	}
}

func wsDial(t *testing.T, url, path string) *websocket.Conn {
	c, _, err := websocket.Dial(context.Background(), strings.Replace(url, "http", "ws", 1)+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func send(t *testing.T, c *websocket.Conn, m p.Envelope) {
	b, _ := json.Marshal(m)
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func recv(t *testing.T, c *websocket.Conn) p.Envelope {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m p.Envelope
	json.Unmarshal(b, &m)
	return m
}

func TestGuestJoinRequiresSignalToken(t *testing.T) {
	s, _ := New(Config{PublicURL: "http://127.0.0.1:1", Logger: quiet(), ConnectRate: 100})
	ts := httptest.NewServer(s)
	defer ts.Close()
	key := security.RandomToken(32)
	h := wsDial(t, ts.URL, "/ws/host")
	send(t, h, p.Envelope{Type: p.MsgRegister, ID: "1", SignalHash: security.SignalHash(security.SignalToken(key))})
	reg := recv(t, h)
	if reg.Type != p.MsgRegistered || reg.SessionID == "" || reg.HostToken == "" {
		t.Fatalf("register: %+v", reg)
	}

	g := wsDial(t, ts.URL, "/ws/guest")
	send(t, g, p.Envelope{Type: p.MsgJoin, SessionID: reg.SessionID, Signal: security.SignalToken("wrong")})
	if m := recv(t, g); m.Code != p.ErrCodeUnauthorized {
		t.Fatalf("wrong token: %+v", m)
	}
	g2 := wsDial(t, ts.URL, "/ws/guest")
	send(t, g2, p.Envelope{Type: p.MsgJoin, SessionID: "nonexistent", Signal: "x"})
	if m := recv(t, g2); m.Code != p.ErrCodeNotFound {
		t.Fatalf("unknown session: %+v", m)
	}
	g3 := wsDial(t, ts.URL, "/ws/guest")
	send(t, g3, p.Envelope{Type: p.MsgJoin, SessionID: reg.SessionID, Signal: security.SignalToken(key)})
	joined := recv(t, g3)
	if joined.Type != p.MsgJoined || !joined.Online {
		t.Fatalf("join: %+v", joined)
	}
	if m := recv(t, h); m.Type != p.MsgGuestJoined || m.Peer != joined.Peer {
		t.Fatalf("host not notified: %+v", m)
	}
	// Signals are routed both ways.
	send(t, g3, p.Envelope{Type: p.MsgSignal, Data: &p.SignalPayload{Kind: "offer", SDP: "x"}})
	if m := recv(t, h); m.Type != p.MsgSignal || m.Peer != joined.Peer || m.Data.SDP != "x" {
		t.Fatalf("offer not routed: %+v", m)
	}
	send(t, h, p.Envelope{Type: p.MsgSignal, Peer: joined.Peer, Data: &p.SignalPayload{Kind: "answer", SDP: "y"}})
	if m := recv(t, g3); m.Data == nil || m.Data.SDP != "y" {
		t.Fatalf("answer not routed: %+v", m)
	}
	// A different host cannot hijack the session without the host token.
	h2 := wsDial(t, ts.URL, "/ws/host")
	send(t, h2, p.Envelope{Type: p.MsgRegister, ID: "2", SignalHash: reg.SignalHash + "x" + strings.Repeat("y", 40), SessionID: reg.SessionID, HostToken: "forged"})
	if m := recv(t, h2); m.SessionID == reg.SessionID {
		t.Fatal("session hijacked with forged host token")
	}
	// Unregistering ends the share for guests.
	send(t, h, p.Envelope{Type: p.MsgUnregister, ID: "3"})
	if m := recv(t, g3); m.Type != p.MsgError || m.Code != p.ErrCodeExpired {
		t.Fatalf("guest not told the share ended: %+v", m)
	}
}

func startDirect(t *testing.T, acmeDir, caFile string) (*DirectService, string) {
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ds, err := NewDirect(DirectConfig{Zone: "direct.test", DNSListen: addr, ACMEDirectory: acmeDir, ACMECAFile: caFile,
		DataDir: t.TempDir(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ds.Close)
	return ds, addr
}

func query(t *testing.T, server, name string, qt uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qt)
	r, _, err := new(dns.Client).Exchange(m, server)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDirectDNS(t *testing.T) {
	ds, addr := startDirect(t, "https://invalid.example", "")
	key := security.RandomToken(32)
	host, err := ds.Register("device-1234", key, "203.0.113.9")
	if err != nil || !strings.HasSuffix(host, ".direct.test") {
		t.Fatalf("register: %q %v", host, err)
	}
	r := query(t, addr, host, dns.TypeA)
	if len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "203.0.113.9" || !r.Authoritative {
		t.Fatalf("A record: %v", r)
	}
	// The address follows the device's connection, not a claimed value.
	if _, err := ds.Register("device-1234", key, "198.51.100.4"); err != nil {
		t.Fatal(err)
	}
	if r := query(t, addr, host, dns.TypeA); r.Answer[0].(*dns.A).A.String() != "198.51.100.4" {
		t.Fatal("A record not updated")
	}
	// Another device cannot take over the name.
	if _, err := ds.Register("device-1234", "x"+key, "192.0.2.1"); err == nil {
		t.Fatal("device key not checked")
	}
	if r := query(t, addr, "nope.direct.test", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Fatalf("unknown name: %v", r.Rcode)
	}
	if r := query(t, addr, "example.com", dns.TypeA); r.Rcode != dns.RcodeRefused {
		t.Fatalf("out-of-zone query not refused: %v", r.Rcode)
	}
	// CSRs must name exactly the device's host name.
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{"evil.example.com"}}, k)
	if _, err := ds.IssueCert(context.Background(), "device-1234", key, "198.51.100.4", base64.StdEncoding.EncodeToString(csr)); err == nil || !strings.Contains(err.Error(), "exactly") {
		t.Fatalf("foreign CSR accepted: %v", err)
	}
}

// TestDirectACMEWithPebble runs a real ACME DNS-01 issuance against Pebble.
// Set SHAREBOX_PEBBLE=/path/to/pebble to enable.
func TestDirectACMEWithPebble(t *testing.T) {
	bin := os.Getenv("SHAREBOX_PEBBLE")
	if bin == "" {
		t.Skip("set SHAREBOX_PEBBLE to the pebble binary to run")
	}
	src := os.Getenv("SHAREBOX_PEBBLE_SRC") // pebble module dir (test certs)
	dnsPort := freePort(t)
	dnsAddr := fmt.Sprintf("127.0.0.1:%d", dnsPort)
	cfgDir := t.TempDir()
	acmePort := freePort(t)
	cfg := fmt.Sprintf(`{"pebble":{"listenAddress":"127.0.0.1:%d","managementListenAddress":"127.0.0.1:%d",
		"certificate":"%s/test/certs/localhost/cert.pem","privateKey":"%s/test/certs/localhost/key.pem",
		"httpPort":5002,"tlsPort":5001,"ocspResponderURL":"","externalAccountBindingRequired":false}}`,
		acmePort, freePort(t), src, src)
	os.WriteFile(filepath.Join(cfgDir, "pebble.json"), []byte(cfg), 0o644)
	cmd := exec.Command(bin, "-config", filepath.Join(cfgDir, "pebble.json"), "-dnsserver", dnsAddr)
	cmd.Env = append(os.Environ(), "PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	time.Sleep(time.Second)

	ds, err := NewDirect(DirectConfig{Zone: "direct.test", DNSListen: dnsAddr,
		ACMEDirectory: fmt.Sprintf("https://127.0.0.1:%d/dir", acmePort), ACMECAFile: src + "/test/certs/pebble.minica.pem",
		DataDir: t.TempDir(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	key := security.RandomToken(32)
	host, err := ds.Register("device-acme-1", key, "203.0.113.20")
	if err != nil {
		t.Fatal(err)
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}}, k)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	chain, err := ds.IssueCert(ctx, "device-acme-1", key, "203.0.113.20", base64.StdEncoding.EncodeToString(csr))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode([]byte(chain))
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.VerifyHostname(host) != nil {
		t.Fatalf("certificate not valid for %s: %v", host, cert.DNSNames)
	}
	pub, _ := cert.PublicKey.(*ecdsa.PublicKey)
	if pub == nil || !pub.Equal(&k.PublicKey) {
		t.Fatal("certificate is not for the host's own key")
	}
	t.Logf("issued certificate for %s by %s", host, cert.Issuer.CommonName)
}

func TestForwardedIP(t *testing.T) {
	cases := map[string]string{
		"203.0.113.5":                         "203.0.113.5",
		"203.0.113.5, 10.0.0.2":               "203.0.113.5", // platform LB appended
		"6.6.6.6, 203.0.113.5, 10.1.1.1":      "203.0.113.5", // client-forged entry ignored
		"203.0.113.5, 100.64.0.9, 172.16.0.1": "203.0.113.5",
		"10.0.0.2":                            "10.0.0.2", // only internal: best effort
		"garbage":                             "",
	}
	for in, want := range cases {
		if got := forwardedIP(in); got != want {
			t.Errorf("forwardedIP(%q) = %q, want %q", in, got, want)
		}
	}
}
