package integration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sharebox/internal/config"
	"sharebox/internal/conformance"
	"sharebox/internal/host"
	"sharebox/internal/network"
	"sharebox/services/signaling"
)

// fakeNATPMP is a router that grants every mapping on the requested port.
func fakeNATPMP(t *testing.T) *network.Mapper {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1100)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req := buf[:n]
			switch {
			case req[0] == 2: // PCP: unsupported version
				conn.WriteToUDP([]byte{0, 0x81, 0, 1, 0, 0, 0, 0}, from)
			case req[1] == 0:
				conn.WriteToUDP([]byte{0, 128, 0, 0, 0, 0, 0, 1, 127, 0, 0, 1}, from)
			default:
				res := make([]byte, 16)
				res[1] = 128 + req[1]
				copy(res[8:10], req[4:6])
				copy(res[10:12], req[4:6]) // external port = internal port
				copy(res[12:16], req[8:12])
				conn.WriteToUDP(res, from)
			}
		}
	}()
	_ = binary.BigEndian
	return &network.Mapper{
		Gateway:     func() (net.IP, error) { return net.IPv4(127, 0, 0, 1), nil },
		PMPPort:     conn.LocalAddr().(*net.UDPAddr).Port,
		DisableUPnP: true, Timeout: time.Second,
	}
}

func TestDirectHTTPSEndToEnd(t *testing.T) {
	bin, src := os.Getenv("SHAREBOX_PEBBLE"), os.Getenv("SHAREBOX_PEBBLE_SRC")
	if bin == "" || src == "" {
		t.Skip("set SHAREBOX_PEBBLE and SHAREBOX_PEBBLE_SRC to run")
	}
	dnsAddr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	acmePort, mgmtPort := freeUDPPort(t), freeUDPPort(t)
	cfgDir := t.TempDir()
	os.WriteFile(filepath.Join(cfgDir, "p.json"), []byte(fmt.Sprintf(`{"pebble":{"listenAddress":"127.0.0.1:%d","managementListenAddress":"127.0.0.1:%d","certificate":"%s/test/certs/localhost/cert.pem","privateKey":"%s/test/certs/localhost/key.pem","httpPort":5002,"tlsPort":5001}}`, acmePort, mgmtPort, src, src)), 0o644)
	cmd := exec.Command(bin, "-config", filepath.Join(cfgDir, "p.json"), "-dnsserver", dnsAddr)
	cmd.Env = append(os.Environ(), "PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	time.Sleep(time.Second)

	ds, err := signaling.NewDirect(signaling.DirectConfig{Zone: "direct.test", DNSListen: dnsAddr,
		ACMEDirectory: fmt.Sprintf("https://127.0.0.1:%d/dir", acmePort), ACMECAFile: src + "/test/certs/pebble.minica.pem",
		DataDir: t.TempDir(), Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	url := "http://" + ln.Addr().String()
	srv, _ := signaling.New(signaling.Config{PublicURL: url, Dev: true, Direct: ds, Logger: quietLogger(), ConnectRate: 100})
	hs := &httptest.Server{Listener: ln, Config: &http.Server{Handler: srv}}
	hs.Start()
	defer hs.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "direct.txt"), []byte("served over direct HTTPS"), 0o644)
	e := host.New(&config.Store{Dir: t.TempDir()}, quietLogger())
	e.SetMapper(fakeNATPMP(t))
	st := config.Defaults()
	st.Folder, st.ServerURL, st.PortMapping = dir, url, true
	if err := e.Start(st, host.StartOptions{AllowLoopback: true}); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	var s host.Status
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		s = e.Status()
		if s.Direct.State == host.CheckOK || s.Direct.State == host.CheckUnavailable {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if s.Direct.State != host.CheckOK {
		t.Fatalf("direct not verified: %+v (mapping %+v)", s.Direct, s.Mapping)
	}
	directURL := strings.TrimPrefix(s.Direct.Detail, "Reachable at ")
	t.Logf("direct URL verified by external probe: %s", directURL)

	// Fetch Pebble's (runtime-generated) root and connect like a browser
	// would: real hostname, publicly trusted chain, DNS from our server.
	mgmt := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	res, err := mgmt.Get(fmt.Sprintf("https://127.0.0.1:%d/roots/0", mgmtPort))
	if err != nil {
		t.Fatal(err)
	}
	rootPEM, _ := io.ReadAll(res.Body)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(rootPEM)
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", dnsAddr)
	}}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h, port, _ := net.SplitHostPort(addr)
			ips, err := resolver.LookupHost(ctx, h)
			if err != nil {
				return nil, err
			}
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(ips[0], port))
		},
	}}
	env := &conformance.Env{Client: client, BaseURL: directURL, Dir: dir, Server: e.Files()}
	c, code := conformance.Login(t, env, e.AccessKey(), "")
	if code != 200 {
		t.Fatalf("login over direct HTTPS: %d", code)
	}
	body, code, hdr := c.Download("direct.txt", nil)
	if code != 200 || string(body) != "served over direct HTTPS" {
		t.Fatalf("download: %d %q", code, body)
	}
	if !strings.Contains(hdr.Get("Strict-Transport-Security"), "max-age") {
		t.Error("missing HSTS on direct HTTPS")
	}
	if s := e.Status(); len(s.Stats.Guests) == 0 || s.Stats.Guests[0].Transport != "direct" {
		t.Errorf("guest not recorded as direct: %+v", s.Stats.Guests)
	}
}
