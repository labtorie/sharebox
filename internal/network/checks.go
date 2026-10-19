package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun/v4"
	"github.com/pion/turn/v5"
)

// ICEURL is a parsed stun:/turn: URL.
type ICEURL struct {
	Scheme    string // stun | turn | turns
	Host      string // host:port
	Transport string // udp | tcp
}

// ParseICEURL parses "stun:host:port" or "turn:host:port?transport=udp".
func ParseICEURL(s string) (ICEURL, error) {
	scheme, rest, ok := strings.Cut(s, ":")
	if !ok {
		return ICEURL{}, errors.New("bad ICE URL")
	}
	u := ICEURL{Scheme: strings.ToLower(scheme), Transport: "udp"}
	if hp, q, ok := strings.Cut(rest, "?"); ok {
		rest = hp
		if v, err := url.ParseQuery(q); err == nil && v.Get("transport") != "" {
			u.Transport = strings.ToLower(v.Get("transport"))
		}
	}
	if u.Scheme == "turns" {
		u.Transport = "tcp"
	}
	if _, _, err := net.SplitHostPort(rest); err != nil {
		port := "3478"
		if u.Scheme == "turns" {
			port = "5349"
		}
		rest = net.JoinHostPort(rest, port)
	}
	u.Host = rest
	return u, nil
}

// STUNBinding sends a STUN binding request to server (host:port) and returns
// our public (server-reflexive) address as seen by that server.
func STUNBinding(ctx context.Context, server string) (*net.UDPAddr, error) {
	raddr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return stunOn(ctx, conn, raddr)
}

func stunOn(ctx context.Context, conn *net.UDPConn, raddr *net.UDPAddr) (*net.UDPAddr, error) {
	msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	buf := make([]byte, 1500)
	wait := 300 * time.Millisecond
	for time.Now().Before(deadline) {
		if _, err := conn.WriteToUDP(msg.Raw, raddr); err != nil {
			return nil, err
		}
		conn.SetReadDeadline(time.Now().Add(min(wait, time.Until(deadline))))
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				break
			}
			res := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if res.Decode() != nil || res.TransactionID != msg.TransactionID {
				continue
			}
			var xa stun.XORMappedAddress
			if err := xa.GetFrom(res); err != nil {
				return nil, err
			}
			return &net.UDPAddr{IP: xa.IP, Port: xa.Port}, nil
		}
		wait *= 2
	}
	return nil, errors.New("no STUN response")
}

// NATReport summarises STUN results.
type NATReport struct {
	PublicIP string
	// Symmetric is true when different servers observed different public
	// ports for the same local socket ("endpoint-dependent mapping"). Direct
	// peer-to-peer connections then usually need a relay.
	Symmetric bool
	Checked   int
	firstPort int
}

// ProbeNAT queries several STUN servers from one local socket.
func ProbeNAT(ctx context.Context, servers []string) (NATReport, error) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return NATReport{}, err
	}
	defer conn.Close()
	var rep NATReport
	seen := map[string]bool{}
	var lastErr error
	for _, s := range servers {
		raddr, err := net.ResolveUDPAddr("udp4", s)
		if err != nil {
			lastErr = err
			continue
		}
		if seen[raddr.String()] {
			continue
		}
		seen[raddr.String()] = true
		addr, err := stunOn(ctx, conn, raddr)
		if err != nil {
			lastErr = err
			continue
		}
		if rep.Checked > 0 && (addr.IP.String() != rep.PublicIP || addr.Port != rep.firstPort) {
			rep.Symmetric = true
		}
		if rep.Checked == 0 {
			rep.PublicIP = addr.IP.String()
			rep.firstPort = addr.Port
		}
		rep.Checked++
	}
	if rep.Checked == 0 {
		if lastErr == nil {
			lastErr = errors.New("no STUN servers configured")
		}
		return rep, lastErr
	}
	return rep, nil
}

// TURNCheck performs a real allocation on a TURN server to verify that the
// relay is reachable and the credentials work.
func TURNCheck(ctx context.Context, u ICEURL, user, pass string) (string, error) {
	if u.Transport != "udp" {
		return "", errors.New("only UDP TURN is verified by the host")
	}
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	lf := logging.NewDefaultLoggerFactory()
	lf.DefaultLogLevel = logging.LogLevelDisabled
	c, err := turn.NewClient(&turn.ClientConfig{STUNServerAddr: u.Host, TURNServerAddr: u.Host, Conn: conn,
		Username: user, Password: pass, LoggerFactory: lf, RTO: 300 * time.Millisecond})
	if err != nil {
		return "", err
	}
	defer c.Close()
	if err := c.Listen(); err != nil {
		return "", err
	}
	actx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	relayConn, err := c.AllocateWithContext(actx)
	if err != nil {
		return "", fmt.Errorf("relay allocation failed: %w", err)
	}
	addr := relayConn.LocalAddr().String()
	relayConn.Close()
	return addr, nil
}

// --- network change / sleep detection ---------------------------------------

// Watcher reports network changes and wake-from-sleep.
type Watcher struct {
	Interval time.Duration
	OnChange func(reason string)

	mu   sync.Mutex
	last string
}

// Run blocks until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	if w.Interval == 0 {
		w.Interval = 5 * time.Second
	}
	w.last = fingerprint()
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	lastTick := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			// time.Now's wall clock jumps forward after sleep while the
			// ticker does not fire; a large gap means we just woke up.
			gap := now.Round(0).Sub(lastTick.Round(0))
			lastTick = now
			if gap > 3*w.Interval {
				w.OnChange("woke from sleep")
				w.last = fingerprint()
				continue
			}
			if fp := fingerprint(); fp != w.last {
				w.last = fp
				w.OnChange("network changed")
			}
		}
	}
}

// fingerprint summarises local addresses and the default gateway.
func fingerprint() string {
	var parts []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				parts = append(parts, i.Name+"="+ipn.IP.String())
			}
		}
	}
	sort.Strings(parts)
	if gw, err := DefaultMapper().Gateway(); err == nil {
		parts = append(parts, "gw="+gw.String())
	}
	return strings.Join(parts, ",")
}

// LocalIPv4 returns the primary LAN address.
func LocalIPv4() string {
	c, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET; nothing is sent
	if err != nil {
		return ""
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}
