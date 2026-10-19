package network

import (
	"context"
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGateway emulates a router speaking PCP (pcp=true) or NAT-PMP only.
// If drop is true it never answers (no mapping support).
type fakeGateway struct {
	conn     *net.UDPConn
	pcp      bool
	drop     bool
	external uint16
	deleted  atomic.Bool
	requests atomic.Int32
}

func startGateway(t *testing.T, pcp, drop bool) (*fakeGateway, *Mapper) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGateway{conn: conn, pcp: pcp, drop: drop, external: 40001}
	go g.serve()
	t.Cleanup(func() { conn.Close() })
	m := &Mapper{
		Gateway:     func() (net.IP, error) { return net.IPv4(127, 0, 0, 1), nil },
		PMPPort:     conn.LocalAddr().(*net.UDPAddr).Port,
		DisableUPnP: true,
		Timeout:     800 * time.Millisecond,
	}
	return g, m
}

func (g *fakeGateway) serve() {
	buf := make([]byte, 1100)
	for {
		n, from, err := g.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		g.requests.Add(1)
		if g.drop {
			continue
		}
		req := buf[:n]
		switch {
		case req[0] == 2 && !g.pcp:
			// NAT-PMP-only routers answer PCP with UNSUPP_VERSION.
			g.conn.WriteToUDP([]byte{0, 0x81, 0, 1, 0, 0, 0, 0}, from)
		case req[0] == 2:
			res := make([]byte, 60)
			res[0], res[1] = 2, 0x81
			lifetime := binary.BigEndian.Uint32(req[4:8])
			binary.BigEndian.PutUint32(res[4:8], lifetime)
			copy(res[24:36], req[24:36]) // nonce
			res[36] = req[36]
			copy(res[40:42], req[40:42])
			if lifetime == 0 {
				g.deleted.Store(true)
			}
			binary.BigEndian.PutUint16(res[42:44], g.external)
			copy(res[44:60], net.IPv4(203, 0, 113, 7).To16())
			g.conn.WriteToUDP(res, from)
		case req[0] == 0 && req[1] == 0:
			res := []byte{0, 128, 0, 0, 0, 0, 0, 1, 203, 0, 113, 7}
			g.conn.WriteToUDP(res, from)
		case req[0] == 0 && (req[1] == 1 || req[1] == 2):
			res := make([]byte, 16)
			res[1] = 128 + req[1]
			copy(res[8:10], req[4:6])
			lifetime := binary.BigEndian.Uint32(req[8:12])
			if lifetime == 0 {
				g.deleted.Store(true)
			}
			binary.BigEndian.PutUint16(res[10:12], g.external)
			binary.BigEndian.PutUint32(res[12:16], lifetime)
			g.conn.WriteToUDP(res, from)
		}
	}
}

func TestPCPMapping(t *testing.T) {
	g, m := startGateway(t, true, false)
	mp, err := m.Map(context.Background(), "tcp", 8443, "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if mp.Method != "pcp" || mp.External != 40001 || mp.ExternalIP != "203.0.113.7" || mp.Lifetime != time.Hour {
		t.Fatalf("unexpected mapping %+v", mp)
	}
	if err := m.Renew(context.Background(), mp); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), mp); err != nil || !g.deleted.Load() {
		t.Fatalf("delete: %v deleted=%v", err, g.deleted)
	}
}

func TestNATPMPFallback(t *testing.T) {
	g, m := startGateway(t, false, false)
	mp, err := m.Map(context.Background(), "udp", 50000, "test", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if mp.Method != "natpmp" || mp.External != 40001 || mp.ExternalIP != "203.0.113.7" {
		t.Fatalf("unexpected mapping %+v", mp)
	}
	if err := m.Delete(context.Background(), mp); err != nil || !g.deleted.Load() {
		t.Fatalf("delete: %v", err)
	}
}

func TestNoMappingSupport(t *testing.T) {
	_, m := startGateway(t, true, true)
	if _, err := m.Map(context.Background(), "tcp", 8443, "test", time.Hour); err == nil {
		t.Fatal("expected failure when router does not answer")
	}
}

func TestIsPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"203.0.113.7": true, "8.8.8.8": true, "192.168.1.2": false, "10.0.0.1": false,
		"100.64.1.1": false, "127.0.0.1": false, "169.254.1.1": false, "": false,
	} {
		if IsPublicIP(ip) != want {
			t.Errorf("IsPublicIP(%q) != %v", ip, want)
		}
	}
}

func TestParseICEURL(t *testing.T) {
	u, err := ParseICEURL("turn:relay.example.org?transport=tcp")
	if err != nil || u.Host != "relay.example.org:3478" || u.Transport != "tcp" || u.Scheme != "turn" {
		t.Fatalf("%+v %v", u, err)
	}
	u, _ = ParseICEURL("turns:relay.example.org")
	if u.Host != "relay.example.org:5349" || u.Transport != "tcp" {
		t.Fatalf("%+v", u)
	}
}
