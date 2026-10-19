package rtc

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/transport/v5/vnet"
	"github.com/pion/webrtc/v4"

	"sharebox/internal/tunnel"
	p "sharebox/shared/protocol"
)

// netCondition describes a simulated link between host and guest.
type netCondition struct {
	name   string
	delay  time.Duration // one way
	jitter time.Duration
	loss   float64 // per packet, each direction
	rate   float64 // bytes/s bottleneck shared by both directions; 0 = none
}

// policer drops packets beyond a token-bucket rate, modelling a bottleneck
// router with a shallow queue (how congestion shows up as loss).
type policer struct {
	mu            sync.Mutex
	rate, tokens  float64
	burst         float64
	last          time.Time
	passed, drops int
}

func (p *policer) allow(n int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.tokens = min(p.burst, p.tokens+now.Sub(p.last).Seconds()*p.rate)
	p.last = now
	if p.tokens < float64(n) {
		p.drops++
		return false
	}
	p.tokens -= float64(n)
	p.passed++
	return true
}

// measure transfers size bytes host->guest over a real pion DataChannel on a
// virtual network and returns MB/s.
func measure(t *testing.T, tun TransportTuning, c netCondition, size int64) (float64, float64) {
	return measureN(t, tun, c, size, 1)
}

// measureN downloads size bytes split evenly over conns independent
// PeerConnections (each with its own SCTP congestion control).
func measureN(t *testing.T, tun TransportTuning, c netCondition, size int64, conns int) (float64, float64) {
	t.Helper()
	lf := logging.NewDefaultLoggerFactory()
	lf.DefaultLogLevel = logging.LogLevelError
	wan, err := vnet.NewRouter(&vnet.RouterConfig{CIDR: "10.0.0.0/24", MinDelay: c.delay, MaxJitter: c.jitter, LoggerFactory: lf, QueueSize: 100000})
	if err != nil {
		t.Fatal(err)
	}
	var pol *policer
	if c.rate > 0 {
		pol = &policer{rate: c.rate, tokens: 64 << 10, burst: 64 << 10, last: time.Now()}
		wan.AddChunkFilter(func(ch vnet.Chunk) bool { return pol.allow(len(ch.UserData()) + 28) })
	}
	if c.loss > 0 {
		var mu sync.Mutex
		r := rand.New(rand.NewPCG(1, 2))
		wan.AddChunkFilter(func(vnet.Chunk) bool {
			mu.Lock()
			defer mu.Unlock()
			return r.Float64() >= c.loss
		})
	}
	mk := func(ip string) *vnet.Net {
		n, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{ip}})
		if err != nil {
			t.Fatal(err)
		}
		if err := wan.AddNet(n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	hostNet, guestNet := mk("10.0.0.2"), mk("10.0.0.3")
	if err := wan.Start(); err != nil {
		t.Fatal(err)
	}
	defer wan.Stop()

	part := size / int64(conns)
	clients := make([]*tunnel.Session, conns)
	for i := range clients {
		clients[i] = connectPair(t, tun, hostNet, guestNet, part)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, conns)
	for _, cl := range clients {
		wg.Add(1)
		go func(cl *tunnel.Session) {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://tunnel/x", nil)
			res, err := (&http.Client{Transport: cl}).Do(req)
			if err != nil {
				errs <- err
				return
			}
			n, err := io.Copy(io.Discard, res.Body)
			if err == nil && n != part {
				err = fmt.Errorf("got %d of %d bytes", n, part)
			}
			if err != nil {
				errs <- err
			}
		}(cl)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	size = part * int64(conns)
	dropPct := 0.0
	if pol != nil {
		pol.mu.Lock()
		dropPct = 100 * float64(pol.drops) / float64(max(1, pol.drops+pol.passed))
		pol.mu.Unlock()
	}
	return float64(size) / (1 << 20) / elapsed.Seconds(), dropPct
}

// connectPair negotiates one host/guest PeerConnection pair on the virtual
// network and returns the guest's tunnel client; the host serves part bytes.
func connectPair(t *testing.T, tun TransportTuning, hostNet, guestNet *vnet.Net, part int64) *tunnel.Session {
	t.Helper()
	newPC := func(n *vnet.Net) *webrtc.PeerConnection {
		se := tun.settingEngine()
		se.SetNet(n)
		pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pc.Close() })
		return pc
	}
	hostPC, guestPC := newPC(hostNet), newPC(guestNet)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.CopyN(w, zeros{}, part) })
	serving := make(chan struct{})
	hostPC.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			s := tunnel.New(dcLink{dc}, tunnel.Options{Handler: h})
			dc.SetBufferedAmountLowThreshold(p.LowWaterMark)
			dc.OnBufferedAmountLow(s.BufferedLow)
			dc.OnMessage(func(m webrtc.DataChannelMessage) { s.Deliver(m.Data) })
			close(serving)
		})
	})
	ordered := true
	dc, _ := guestPC.CreateDataChannel(p.TunnelLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	opened := make(chan struct{})
	dc.OnOpen(func() { close(opened) })
	offer, _ := guestPC.CreateOffer(nil)
	g := webrtc.GatheringCompletePromise(guestPC)
	guestPC.SetLocalDescription(offer)
	<-g
	hostPC.SetRemoteDescription(*guestPC.LocalDescription())
	ans, _ := hostPC.CreateAnswer(nil)
	g = webrtc.GatheringCompletePromise(hostPC)
	hostPC.SetLocalDescription(ans)
	<-g
	guestPC.SetRemoteDescription(*hostPC.LocalDescription())
	select {
	case <-opened:
	case <-time.After(20 * time.Second):
		t.Fatal("data channel did not open")
	}
	<-serving
	client := tunnel.New(dcLink{dc}, tunnel.Options{})
	dc.SetBufferedAmountLowThreshold(p.LowWaterMark)
	dc.OnBufferedAmountLow(client.BufferedLow)
	dc.OnMessage(func(m webrtc.DataChannelMessage) { client.Deliver(m.Data) })
	return client
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) { clear(b); return len(b), nil }

// TestThroughputUnderNetworkConditions compares transport tunings on
// realistic links. Run with SHAREBOX_BENCH=1 go test -run Throughput -v ./internal/rtc
func TestThroughputUnderNetworkConditions(t *testing.T) {
	if os.Getenv("SHAREBOX_BENCH") == "" {
		t.Skip("set SHAREBOX_BENCH=1")
	}
	conds := []netCondition{
		{"wired LAN (0.5 ms)", 250 * time.Microsecond, 0, 0, 0},
		{"Wi-Fi (3 ms ±2, 0.5% loss)", 1500 * time.Microsecond, 1 * time.Millisecond, 0.005, 0},
		{"busy Wi-Fi (8 ms ±4, 2% loss)", 4 * time.Millisecond, 2 * time.Millisecond, 0.02, 0},
		{"internet (40 ms, 0.5% loss)", 20 * time.Millisecond, 2 * time.Millisecond, 0.005, 0},
		{"iso: jitter only (3 ms ±2)", 1500 * time.Microsecond, 1 * time.Millisecond, 0, 0},
		{"iso: loss only (3 ms, 0.5%)", 1500 * time.Microsecond, 0, 0.005, 0},
		{"iso: loss only (3 ms, 0.1%)", 1500 * time.Microsecond, 0, 0.001, 0},
		{"cmp: Wi-Fi 0.5% loss", 1500 * time.Microsecond, 1 * time.Millisecond, 0.005, 0},
		{"cmp: Wi-Fi 0.1% loss", 1500 * time.Microsecond, 1 * time.Millisecond, 0.001, 0},
		{"cmp: Wi-Fi 0.5%, 25 MB/s cap", 1500 * time.Microsecond, 1 * time.Millisecond, 0.005, 25 << 20},
		{"cmp: internet 0.1%, 3 MB/s cap", 20 * time.Millisecond, 2 * time.Millisecond, 0.001, 3 << 20},
		{"Wi-Fi, 25 MB/s cap", 1500 * time.Microsecond, 1 * time.Millisecond, 0.005, 25 << 20},
		{"internet, 3 MB/s cap", 20 * time.Millisecond, 2 * time.Millisecond, 0.001, 3 << 20},
	}
	if v := os.Getenv("SHAREBOX_BENCH_ONLY"); v != "" {
		var keep []netCondition
		for _, c := range conds {
			if strings.Contains(c.name, v) {
				keep = append(keep, c)
			}
		}
		conds = keep
	}
	variants := map[string]TransportTuning{}
	if os.Getenv("SHAREBOX_BENCH_SKIP_CURRENT") == "" {
		variants["current"] = Tuning
	}
	if v := os.Getenv("SHAREBOX_BENCH_VARIANTS"); v != "" {
		for _, name := range strings.Split(v, ",") {
			if tv, ok := benchVariants[name]; ok {
				variants[name] = tv
			} else if strings.HasPrefix(name, "x") {
				variants[name] = Tuning // xN: current tuning over N connections
			}
		}
	}
	size := int64(48 << 20)
	if mb, err := strconv.Atoi(os.Getenv("SHAREBOX_BENCH_MB")); err == nil && mb > 0 {
		size = int64(mb) << 20
	}
	var out strings.Builder
	for _, c := range conds {
		for name, v := range variants {
			conns := 1
			if n, err := strconv.Atoi(strings.TrimPrefix(name, "x")); err == nil && strings.HasPrefix(name, "x") {
				conns = n
			}
			mbps, drops := measureN(t, v, c, size, conns)
			fmt.Fprintf(&out, "%-32s %-14s %6.1f MB/s  bottleneck drops %4.1f%%\n", c.name, name, mbps, drops)
		}
	}
	t.Log("\n" + out.String())
}

var benchVariants = map[string]TransportTuning{
	"zerocsum":   {SCTPReceiveBuffer: 8 << 20, ZeroChecksum: true},
	"aggressive": {SCTPReceiveBuffer: 8 << 20, ZeroChecksum: true, MinCwnd: 128 * 1024, FastRtxWnd: 64 * 1024, CwndCAStep: 16 * 1024, RTOMax: 2 * time.Second},
	"bigbuf":     {SCTPReceiveBuffer: 32 << 20, ZeroChecksum: true},
	"agg2":       {SCTPReceiveBuffer: 8 << 20, ZeroChecksum: true, MinCwnd: 256 * 1024, FastRtxWnd: 128 * 1024, CwndCAStep: 32 * 1024, RTOMax: time.Second},
	"agg3":       {SCTPReceiveBuffer: 8 << 20, ZeroChecksum: true, MinCwnd: 512 * 1024, FastRtxWnd: 256 * 1024, CwndCAStep: 64 * 1024, RTOMax: time.Second},
	"rto1s":      {SCTPReceiveBuffer: 8 << 20, ZeroChecksum: true, RTOMax: time.Second},
	"ca8":        {SCTPReceiveBuffer: 8 << 20, CwndCAStep: 8 * 1228},
	"ca16":       {SCTPReceiveBuffer: 8 << 20, CwndCAStep: 16 * 1228},
	"ca32":       {SCTPReceiveBuffer: 8 << 20, CwndCAStep: 32 * 1228},
}

func TestClassifyRoute(t *testing.T) {
	own := []string{"203.0.113.7"}
	cases := []struct{ lt, rt, addr, want string }{
		{"host", "host", "192.168.1.20", "local network"},
		{"host", "prflx", "10.0.0.5", "local network"},
		{"srflx", "srflx", "203.0.113.7", "router loopback"},
		{"srflx", "srflx", "198.51.100.9", "internet"},
		{"relay", "srflx", "198.51.100.9", "relay"},
		{"host", "relay", "192.0.2.1", "relay"},
	}
	for _, c := range cases {
		if got := ClassifyRoute(c.lt, c.rt, c.addr, own); got != c.want {
			t.Errorf("%s/%s %s = %q, want %q", c.lt, c.rt, c.addr, got, c.want)
		}
	}
}

func TestIsTunnelInterface(t *testing.T) {
	for name, want := range map[string]bool{
		"en0": false, "eth0": false, "wlan0": false, "Wi-Fi": false, "Ethernet": false, "bridge100": false,
		"utun9": true, "tun0": true, "wg0": true, "ppp0": true, "tailscale0": true, "zt7nnig26": true,
		"Radmin VPN": true, "WireGuard Tunnel": true, "OpenVPN TAP-Windows6": true, "Hiddify Tunnel": true,
	} {
		if got := IsTunnelInterface(name); got != want {
			t.Errorf("IsTunnelInterface(%q) = %v, want %v", name, got, want)
		}
	}
}
