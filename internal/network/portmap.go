// Package network handles automatic router port mapping (PCP, NAT-PMP,
// UPnP IGD), STUN/TURN reachability checks and network-change detection.
//
// A successful mapping is never treated as proof of reachability: the host
// additionally asks the coordination server to connect back from the
// internet (see internal/host). Carrier-grade NAT, double NAT and many
// corporate networks accept mappings that are useless from outside.
package network

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/jackpal/gateway"
)

// Mapping is an active router port mapping.
type Mapping struct {
	Protocol   string // "tcp" or "udp"
	Internal   int
	External   int
	ExternalIP string
	Method     string // pcp | natpmp | upnp
	Lifetime   time.Duration

	gw    net.IP
	nonce [12]byte
	upnp  upnpClient
	local net.IP
	desc  string
}

// ErrNoMapping is returned when no mapping protocol succeeded.
var ErrNoMapping = errors.New("the router does not support automatic port mapping (UPnP, NAT-PMP or PCP)")

// Mapper creates mappings. Fields are injectable for tests.
type Mapper struct {
	// Gateway returns the default gateway address.
	Gateway func() (net.IP, error)
	// PMPPort is the PCP/NAT-PMP server port (5351).
	PMPPort int
	// DisableUPnP skips UPnP discovery (tests).
	DisableUPnP bool
	Timeout     time.Duration
}

// DefaultMapper uses the system's default gateway.
func DefaultMapper() *Mapper {
	return &Mapper{Gateway: gateway.DiscoverGateway, PMPPort: 5351, Timeout: 3 * time.Second}
}

// Map maps internalPort for proto, asking for the same external port.
// Protocols are tried in order PCP, NAT-PMP, UPnP.
func (m *Mapper) Map(ctx context.Context, proto string, internalPort int, desc string, lifetime time.Duration) (*Mapping, error) {
	gw, err := m.Gateway()
	if err != nil {
		return nil, fmt.Errorf("no default gateway: %w", err)
	}
	var errs []string
	mp := &Mapping{Protocol: proto, Internal: internalPort, gw: gw, desc: desc, Lifetime: lifetime}
	rand.Read(mp.nonce[:])
	if err := m.pcpMap(ctx, mp, internalPort); err == nil {
		return mp, nil
	} else {
		errs = append(errs, "PCP: "+err.Error())
	}
	if err := m.pmpMap(ctx, mp, internalPort); err == nil {
		return mp, nil
	} else {
		errs = append(errs, "NAT-PMP: "+err.Error())
	}
	if !m.DisableUPnP {
		if err := upnpMap(ctx, mp, internalPort); err == nil {
			return mp, nil
		} else {
			errs = append(errs, "UPnP: "+err.Error())
		}
	}
	return nil, fmt.Errorf("%w (%s)", ErrNoMapping, strings.Join(errs, "; "))
}

// Renew refreshes the mapping before it expires.
func (m *Mapper) Renew(ctx context.Context, mp *Mapping) error {
	switch mp.Method {
	case "pcp":
		return m.pcpMap(ctx, mp, mp.External)
	case "natpmp":
		return m.pmpMap(ctx, mp, mp.External)
	case "upnp":
		return upnpMap(ctx, mp, mp.External)
	}
	return errors.New("unknown mapping method")
}

// Delete removes the mapping.
func (m *Mapper) Delete(ctx context.Context, mp *Mapping) error {
	switch mp.Method {
	case "pcp":
		saved := mp.Lifetime
		mp.Lifetime = 0
		err := m.pcpMap(ctx, mp, 0)
		mp.Lifetime = saved
		return err
	case "natpmp":
		return m.pmpRequest(ctx, mp, mp.Internal, 0, 0)
	case "upnp":
		if mp.upnp != nil {
			return mp.upnp.DeletePortMappingCtx(ctx, "", uint16(mp.External), strings.ToUpper(mp.Protocol))
		}
	}
	return nil
}

func (m *Mapper) exchange(ctx context.Context, gw net.IP, req []byte, accept func([]byte) bool) ([]byte, net.IP, error) {
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: gw, Port: m.PMPPort})
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	local := conn.LocalAddr().(*net.UDPAddr).IP
	deadline := time.Now().Add(m.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	// RFC 6886 retransmission: 250 ms doubling.
	wait := 250 * time.Millisecond
	buf := make([]byte, 1100)
	for time.Now().Before(deadline) {
		if _, err := conn.Write(req); err != nil {
			return nil, local, err
		}
		conn.SetReadDeadline(time.Now().Add(min(wait, time.Until(deadline))))
		for {
			n, err := conn.Read(buf)
			if err != nil {
				break
			}
			if accept(buf[:n]) {
				return append([]byte(nil), buf[:n]...), local, nil
			}
		}
		wait *= 2
	}
	return nil, local, errors.New("no response from router")
}

// --- PCP (RFC 6887) ----------------------------------------------------------

func protoNum(p string) byte {
	if p == "tcp" {
		return 6
	}
	return 17
}

func (m *Mapper) pcpMap(ctx context.Context, mp *Mapping, suggested int) error {
	// Determine our local address towards the gateway first; PCP requires it
	// in the request.
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: mp.gw, Port: m.PMPPort})
	if err != nil {
		return err
	}
	local := c.LocalAddr().(*net.UDPAddr).IP.To16()
	c.Close()

	req := make([]byte, 60)
	req[0] = 2 // version
	req[1] = 1 // MAP request
	binary.BigEndian.PutUint32(req[4:8], uint32(mp.Lifetime.Seconds()))
	copy(req[8:24], local)
	copy(req[24:36], mp.nonce[:])
	req[36] = protoNum(mp.Protocol)
	binary.BigEndian.PutUint16(req[40:42], uint16(mp.Internal))
	binary.BigEndian.PutUint16(req[42:44], uint16(suggested))
	copy(req[44:60], net.IPv4zero.To16())

	res, _, err := m.exchange(ctx, mp.gw, req, func(b []byte) bool {
		// Accept PCP MAP responses with our nonce, or a NAT-PMP style
		// "unsupported version" reply.
		if len(b) >= 4 && b[0] == 0 {
			return true
		}
		return len(b) >= 60 && b[0] == 2 && b[1] == 0x81 && string(b[24:36]) == string(mp.nonce[:])
	})
	if err != nil {
		return err
	}
	if res[0] != 2 {
		return errors.New("router speaks NAT-PMP only")
	}
	if code := res[3]; code != 0 {
		return fmt.Errorf("router refused mapping (result %d)", code)
	}
	mp.Method = "pcp"
	mp.Lifetime = time.Duration(binary.BigEndian.Uint32(res[4:8])) * time.Second
	mp.External = int(binary.BigEndian.Uint16(res[42:44]))
	ip := net.IP(res[44:60])
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	mp.ExternalIP = ip.String()
	mp.local = local
	return nil
}

// --- NAT-PMP (RFC 6886) ------------------------------------------------------

func (m *Mapper) pmpMap(ctx context.Context, mp *Mapping, suggested int) error {
	if err := m.pmpRequest(ctx, mp, mp.Internal, suggested, uint32(mp.Lifetime.Seconds())); err != nil {
		return err
	}
	// Public address.
	res, _, err := m.exchange(ctx, mp.gw, []byte{0, 0}, func(b []byte) bool {
		return len(b) >= 12 && b[0] == 0 && b[1] == 128
	})
	if err == nil && binary.BigEndian.Uint16(res[2:4]) == 0 {
		mp.ExternalIP = net.IP(res[8:12]).String()
	}
	mp.Method = "natpmp"
	return nil
}

func (m *Mapper) pmpRequest(ctx context.Context, mp *Mapping, internal, suggested int, lifetime uint32) error {
	op := byte(1)
	if mp.Protocol == "tcp" {
		op = 2
	}
	req := make([]byte, 12)
	req[1] = op
	binary.BigEndian.PutUint16(req[4:6], uint16(internal))
	binary.BigEndian.PutUint16(req[6:8], uint16(suggested))
	binary.BigEndian.PutUint32(req[8:12], lifetime)
	res, _, err := m.exchange(ctx, mp.gw, req, func(b []byte) bool {
		return len(b) >= 16 && b[0] == 0 && b[1] == 128+op && int(binary.BigEndian.Uint16(b[8:10])) == internal
	})
	if err != nil {
		return err
	}
	if code := binary.BigEndian.Uint16(res[2:4]); code != 0 {
		return fmt.Errorf("router refused mapping (result %d)", code)
	}
	if lifetime > 0 {
		mp.External = int(binary.BigEndian.Uint16(res[10:12]))
		mp.Lifetime = time.Duration(binary.BigEndian.Uint32(res[12:16])) * time.Second
	}
	return nil
}

// --- UPnP IGD ---------------------------------------------------------------

type upnpClient interface {
	AddPortMappingCtx(ctx context.Context, remoteHost string, extPort uint16, proto string, intPort uint16,
		client string, enabled bool, desc string, lease uint32) error
	DeletePortMappingCtx(ctx context.Context, remoteHost string, extPort uint16, proto string) error
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
	LocalAddr() net.IP
}

var upnpOnce struct {
	sync.Mutex
	clients []upnpClient
	at      time.Time
}

func discoverUPnP(ctx context.Context) []upnpClient {
	upnpOnce.Lock()
	defer upnpOnce.Unlock()
	if time.Since(upnpOnce.at) < 5*time.Minute && len(upnpOnce.clients) > 0 {
		return upnpOnce.clients
	}
	var out []upnpClient
	var wg sync.WaitGroup
	var mu sync.Mutex
	add := func(c upnpClient) { mu.Lock(); out = append(out, c); mu.Unlock() }
	wg.Add(3)
	go func() {
		defer wg.Done()
		cs, _, _ := internetgateway2.NewWANIPConnection2ClientsCtx(ctx)
		for _, c := range cs {
			add(c)
		}
	}()
	go func() {
		defer wg.Done()
		cs, _, _ := internetgateway2.NewWANIPConnection1ClientsCtx(ctx)
		for _, c := range cs {
			add(c)
		}
	}()
	go func() {
		defer wg.Done()
		cs, _, _ := internetgateway2.NewWANPPPConnection1ClientsCtx(ctx)
		for _, c := range cs {
			add(c)
		}
	}()
	wg.Wait()
	upnpOnce.clients, upnpOnce.at = out, time.Now()
	return out
}

func upnpMap(ctx context.Context, mp *Mapping, external int) error {
	dctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	clients := discoverUPnP(dctx)
	cancel()
	if len(clients) == 0 {
		return errors.New("no UPnP gateway found")
	}
	var last error
	for _, c := range clients {
		local := c.LocalAddr()
		if local == nil {
			continue
		}
		lease := uint32(mp.Lifetime.Seconds())
		if external == 0 {
			external = mp.Internal
		}
		err := c.AddPortMappingCtx(ctx, "", uint16(external), strings.ToUpper(mp.Protocol), uint16(mp.Internal),
			local.String(), true, mp.desc, lease)
		if err != nil && lease != 0 && strings.Contains(err.Error(), "725") {
			// OnlyPermanentLeasesSupported: fall back to a permanent lease,
			// which we delete explicitly on stop.
			lease = 0
			err = c.AddPortMappingCtx(ctx, "", uint16(external), strings.ToUpper(mp.Protocol), uint16(mp.Internal),
				local.String(), true, mp.desc, 0)
		}
		if err != nil {
			last = err
			continue
		}
		mp.Method, mp.External, mp.upnp, mp.local = "upnp", external, c, local
		if lease == 0 {
			mp.Lifetime = 0
		}
		if ip, err := c.GetExternalIPAddressCtx(ctx); err == nil {
			mp.ExternalIP = ip
		}
		return nil
	}
	if last == nil {
		last = errors.New("no usable UPnP gateway")
	}
	return last
}

// IsPublicIP reports whether ip is a globally routable unicast address.
func IsPublicIP(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
		return false // CGNAT 100.64.0.0/10: mapping exists but is not reachable
	}
	return true
}
