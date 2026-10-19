// Package relay runs the TURN server used as the last-resort transport when
// no direct or peer-to-peer path exists. It also answers STUN binding
// requests, so a single deployment provides both.
//
// The relay only forwards DTLS-encrypted packets between a guest and a host:
// it cannot read file contents and never stores anything. Credentials are
// short-lived and derived from a shared secret using the widely supported
// "TURN REST API" scheme (username = "<expiry>:<id>", password =
// base64(HMAC-SHA1(secret, username))), so the coordination server can mint
// them without talking to the relay.
package relay

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v5"
)

// Config for the relay.
type Config struct {
	// PublicIP is the address advertised in relay candidates. Required.
	PublicIP string
	// ListenUDP / ListenTCP addresses, e.g. ":3478". Empty disables.
	ListenUDP string
	ListenTCP string
	// ListenTLS (e.g. ":5349") with TLSConfig enables TURN over TLS, which
	// passes through firewalls that only allow HTTPS-like traffic.
	ListenTLS string
	TLSConfig *tls.Config
	// Secret shared with the coordination server.
	Secret string
	Realm  string
	// Relay port range; 0 means OS-assigned.
	MinPort, MaxPort uint16
	// MaxAllocationsPerUser bounds resources per credential.
	MaxAllocationsPerUser int
	// AllowPrivatePeers permits relaying to loopback/private addresses.
	// Only for local development: in production it would let clients use
	// the relay to reach the server's internal network.
	AllowPrivatePeers bool
	Logger            *slog.Logger
}

// Server is a running relay.
type Server struct {
	srv *turn.Server
	cfg Config

	mu     sync.Mutex
	allocs map[string]int
}

// Start launches the relay.
func Start(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Realm == "" {
		cfg.Realm = "sharebox"
	}
	if cfg.MaxAllocationsPerUser == 0 {
		cfg.MaxAllocationsPerUser = 10
	}
	ip := net.ParseIP(cfg.PublicIP)
	if ip == nil {
		return nil, fmt.Errorf("relay: invalid public IP %q", cfg.PublicIP)
	}
	if cfg.Secret == "" {
		return nil, fmt.Errorf("relay: secret is required")
	}
	s := &Server{cfg: cfg, allocs: map[string]int{}}
	perm := func(_ net.Addr, peer net.IP) bool {
		if cfg.AllowPrivatePeers {
			return true
		}
		return isPublic(peer)
	}
	gen := func() turn.RelayAddressGenerator {
		if cfg.MinPort > 0 && cfg.MaxPort >= cfg.MinPort {
			return &turn.RelayAddressGeneratorPortRange{RelayAddress: ip, Address: "0.0.0.0", MinPort: cfg.MinPort, MaxPort: cfg.MaxPort}
		}
		return &turn.RelayAddressGeneratorStatic{RelayAddress: ip, Address: "0.0.0.0"}
	}
	var sc turn.ServerConfig
	if cfg.ListenUDP != "" {
		pc, err := net.ListenPacket("udp4", cfg.ListenUDP)
		if err != nil {
			return nil, fmt.Errorf("relay udp: %w", err)
		}
		sc.PacketConnConfigs = append(sc.PacketConnConfigs, turn.PacketConnConfig{PacketConn: pc, RelayAddressGenerator: gen(), PermissionHandler: perm})
	}
	if cfg.ListenTCP != "" {
		l, err := net.Listen("tcp4", cfg.ListenTCP)
		if err != nil {
			return nil, fmt.Errorf("relay tcp: %w", err)
		}
		sc.ListenerConfigs = append(sc.ListenerConfigs, turn.ListenerConfig{Listener: l, RelayAddressGenerator: gen(), PermissionHandler: perm})
	}
	if cfg.ListenTLS != "" && cfg.TLSConfig != nil {
		l, err := tls.Listen("tcp4", cfg.ListenTLS, cfg.TLSConfig)
		if err != nil {
			return nil, fmt.Errorf("relay tls: %w", err)
		}
		sc.ListenerConfigs = append(sc.ListenerConfigs, turn.ListenerConfig{Listener: l, RelayAddressGenerator: gen(), PermissionHandler: perm})
	}
	lf := logging.NewDefaultLoggerFactory()
	lf.DefaultLogLevel = logging.LogLevelWarn
	sc.LoggerFactory = lf
	sc.Realm = cfg.Realm
	rest := turn.LongTermTURNRESTAuthHandler(cfg.Secret, lf.NewLogger("turn-auth"))
	sc.AuthHandler = func(ra *turn.RequestAttributes) (string, []byte, bool) {
		// Reject expired credentials explicitly (the REST handler does too;
		// this keeps the policy obvious and independent of library details).
		exp, _, ok := strings.Cut(ra.Username, ":")
		if !ok {
			return "", nil, false
		}
		ts, err := strconv.ParseInt(exp, 10, 64)
		if err != nil || time.Now().Unix() > ts {
			return "", nil, false
		}
		_, key, ok := rest(ra)
		return ra.Username, key, ok
	}
	sc.QuotaHandler = func(username, _ string, _ net.Addr) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.allocs[username] < cfg.MaxAllocationsPerUser
	}
	sc.EventHandler = turn.EventHandler{
		OnAllocationCreated: func(_, _ net.Addr, _ string, username, _ string, _ net.Addr, _ int) {
			s.mu.Lock()
			s.allocs[username]++
			s.mu.Unlock()
		},
		OnAllocationDeleted: func(_, _ net.Addr, _ string, username, _ string) {
			s.mu.Lock()
			if s.allocs[username]--; s.allocs[username] <= 0 {
				delete(s.allocs, username)
			}
			s.mu.Unlock()
		},
	}
	if len(sc.PacketConnConfigs) == 0 && len(sc.ListenerConfigs) == 0 {
		return nil, fmt.Errorf("relay: no listeners configured")
	}
	srv, err := turn.NewServer(sc)
	if err != nil {
		return nil, err
	}
	s.srv = srv
	cfg.Logger.Info("relay started", "udp", cfg.ListenUDP, "tcp", cfg.ListenTCP, "tls", cfg.ListenTLS, "publicIP", cfg.PublicIP)
	return s, nil
}

// Close stops the relay.
func (s *Server) Close() error { return s.srv.Close() }

// Credentials mints a time-limited TURN credential for id.
func Credentials(secret, id string, ttl time.Duration) (username, password string, err error) {
	return turn.GenerateLongTermTURNRESTCredentials(secret, id, ttl)
}

func isPublic(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() ||
		// Carrier-grade NAT shared address space (RFC 6598).
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64))
}
