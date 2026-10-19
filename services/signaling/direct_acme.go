package signaling

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/crypto/acme"

	"sharebox/internal/security"
)

// Direct HTTPS needs a publicly trusted certificate on every host. Sharebox
// solves this the way Plex's "plex.direct" does:
//
//   - The operator delegates a DNS zone (e.g. direct.example.org) to this
//     server, which answers authoritatively.
//   - Each device gets a random, stable label: <label>.direct.example.org.
//     Its A record always points to the IP address the device is currently
//     connecting from (never an address the client claims), so a name can
//     only ever point at its owner's own connection.
//   - The host generates its private key locally and sends only a CSR. This
//     server completes an ACME DNS-01 challenge by publishing the TXT record
//     itself and returns the signed chain. It never sees host private keys.

// DirectConfig configures NewDirect.
type DirectConfig struct {
	Zone          string // e.g. direct.example.org
	DNSListen     string // e.g. :53
	NS            string // public name of this DNS server, e.g. ns1.example.org
	ACMEDirectory string
	ACMEEmail     string
	ACMECAFile    string // extra trust roots for the ACME server (Pebble)
	DataDir       string
	Logger        *slog.Logger
	// MaxCertsPerDevicePerDay bounds ACME usage (Let's Encrypt rate limits).
	MaxCertsPerDevicePerDay int
}

type device struct {
	KeyHash string    `json:"keyHash"`
	Label   string    `json:"label"`
	IP      string    `json:"ip"`
	Updated time.Time `json:"updated"`
	Issued  []int64   `json:"issued,omitempty"` // unix times of issuance
}

type acmeDirect struct {
	cfg  DirectConfig
	log  *slog.Logger
	zone string // canonical, with trailing dot

	mu      sync.Mutex
	devices map[string]*device // by device id
	labels  map[string]string  // label -> device id
	txt     map[string][]string
	created map[string][]time.Time // ip -> device creations

	acmeMu sync.Mutex
	client *acme.Client
	udp    *dns.Server
	tcp    *dns.Server
}

// NewDirect starts the DNS server and returns the direct-HTTPS service.
func NewDirect(cfg DirectConfig) (*DirectService, error) {
	if cfg.Zone == "" || cfg.DataDir == "" {
		return nil, errors.New("zone and data dir are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxCertsPerDevicePerDay == 0 {
		cfg.MaxCertsPerDevicePerDay = 3
	}
	d := &acmeDirect{cfg: cfg, log: cfg.Logger, zone: dns.Fqdn(strings.ToLower(cfg.Zone)),
		devices: map[string]*device{}, labels: map[string]string{}, txt: map[string][]string{}, created: map[string][]time.Time{}}
	d.load()
	mux := dns.NewServeMux()
	mux.HandleFunc(".", d.serveDNS)
	d.udp = &dns.Server{Addr: cfg.DNSListen, Net: "udp", Handler: mux}
	d.tcp = &dns.Server{Addr: cfg.DNSListen, Net: "tcp", Handler: mux}
	errc := make(chan error, 2)
	started := make(chan struct{}, 2)
	d.udp.NotifyStartedFunc = func() { started <- struct{}{} }
	d.tcp.NotifyStartedFunc = func() { started <- struct{}{} }
	go func() { errc <- d.udp.ListenAndServe() }()
	go func() { errc <- d.tcp.ListenAndServe() }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			return nil, fmt.Errorf("DNS server: %w", err)
		case <-started:
		case <-time.After(5 * time.Second):
			return nil, errors.New("DNS server did not start")
		}
	}
	d.log.Info("direct HTTPS DNS zone active", "zone", d.zone, "listen", cfg.DNSListen)
	return &DirectService{impl: d}, nil
}

// Close stops the DNS server.
func (s *DirectService) Close() {
	if d, ok := s.impl.(*acmeDirect); ok {
		d.udp.Shutdown()
		d.tcp.Shutdown()
	}
}

func (d *acmeDirect) statePath() string { return filepath.Join(d.cfg.DataDir, "direct-devices.json") }

func (d *acmeDirect) load() {
	b, err := os.ReadFile(d.statePath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &d.devices)
	for id, dev := range d.devices {
		d.labels[dev.Label] = id
	}
}

func (d *acmeDirect) saveLocked() {
	b, _ := json.Marshal(d.devices)
	tmp := d.statePath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, d.statePath())
	}
}

func (d *acmeDirect) hostname(label string) string {
	return label + "." + strings.TrimSuffix(d.zone, ".")
}

// Register implements directImpl.
func (d *acmeDirect) Register(deviceID, deviceKey, ip string) (string, error) {
	if len(deviceID) < 8 || len(deviceID) > 64 || len(deviceKey) < 32 {
		return "", errors.New("invalid device identity")
	}
	if net.ParseIP(ip).To4() == nil {
		return "", errors.New("direct HTTPS currently requires IPv4")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dev := d.devices[deviceID]
	if dev == nil {
		// Throttle new device creation per source address.
		var recent []time.Time
		for _, t := range d.created[ip] {
			if time.Since(t) < 24*time.Hour {
				recent = append(recent, t)
			}
		}
		if len(recent) >= 10 {
			return "", errors.New("too many new devices from this address")
		}
		d.created[ip] = append(recent, time.Now())
		label := ""
		for label == "" || d.labels[label] != "" {
			b := make([]byte, 8)
			rand.Read(b)
			label = strings.ToLower(idEnc.EncodeToString(b))[:12]
		}
		dev = &device{KeyHash: hashStr(deviceKey), Label: label}
		d.devices[deviceID] = dev
		d.labels[label] = deviceID
	} else if !security.Equal(dev.KeyHash, hashStr(deviceKey)) {
		return "", errors.New("device key mismatch")
	}
	dev.IP, dev.Updated = ip, time.Now()
	d.saveLocked()
	return d.hostname(dev.Label), nil
}

// serveDNS answers authoritatively for the zone.
func (d *acmeDirect) serveDNS(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	if len(r.Question) != 1 {
		m.Rcode = dns.RcodeFormatError
		w.WriteMsg(m)
		return
	}
	q := r.Question[0]
	name := strings.ToLower(q.Name)
	if name != d.zone && !strings.HasSuffix(name, "."+d.zone) {
		m.Rcode = dns.RcodeRefused
		m.Authoritative = false
		w.WriteMsg(m)
		return
	}
	hdr := func(t uint16, ttl uint32) dns.RR_Header {
		return dns.RR_Header{Name: q.Name, Rrtype: t, Class: dns.ClassINET, Ttl: ttl}
	}
	soa := &dns.SOA{Hdr: dns.RR_Header{Name: d.zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
		Ns: dns.Fqdn(d.nsName()), Mbox: "hostmaster." + d.zone, Serial: uint32(time.Now().Unix()),
		Refresh: 3600, Retry: 600, Expire: 86400, Minttl: 30}

	d.mu.Lock()
	defer d.mu.Unlock()
	found := false
	switch {
	case name == d.zone:
		found = true
		switch q.Qtype {
		case dns.TypeSOA:
			m.Answer = append(m.Answer, soa)
		case dns.TypeNS:
			m.Answer = append(m.Answer, &dns.NS{Hdr: hdr(dns.TypeNS, 3600), Ns: dns.Fqdn(d.nsName())})
		}
	case strings.HasPrefix(name, "_acme-challenge."):
		key := strings.TrimPrefix(name, "_acme-challenge.")
		if vals, ok := d.txt[key]; ok {
			found = true
			if q.Qtype == dns.TypeTXT {
				for _, v := range vals {
					m.Answer = append(m.Answer, &dns.TXT{Hdr: hdr(dns.TypeTXT, 10), Txt: []string{v}})
				}
			}
		}
	default:
		label := strings.TrimSuffix(name, "."+d.zone)
		if id, ok := d.labels[label]; ok && !strings.Contains(label, ".") {
			found = true
			if ip := net.ParseIP(d.devices[id].IP).To4(); ip != nil && q.Qtype == dns.TypeA {
				m.Answer = append(m.Answer, &dns.A{Hdr: hdr(dns.TypeA, 60), A: ip})
			}
		}
	}
	if !found {
		m.Rcode = dns.RcodeNameError
	}
	if len(m.Answer) == 0 {
		m.Ns = append(m.Ns, soa)
	}
	w.WriteMsg(m)
}

func (d *acmeDirect) nsName() string {
	if d.cfg.NS != "" {
		return d.cfg.NS
	}
	return "ns." + strings.TrimSuffix(d.zone, ".")
}

func (d *acmeDirect) acmeClient(ctx context.Context) (*acme.Client, error) {
	d.acmeMu.Lock()
	defer d.acmeMu.Unlock()
	if d.client != nil {
		return d.client, nil
	}
	keyPath := filepath.Join(d.cfg.DataDir, "acme-account.key")
	var key crypto.Signer
	if b, err := os.ReadFile(keyPath); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			key, _ = x509.ParseECPrivateKey(blk.Bytes)
		}
	}
	if key == nil {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, _ := x509.MarshalECPrivateKey(k)
		if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, err
		}
		key = k
	}
	c := &acme.Client{Key: key, DirectoryURL: d.cfg.ACMEDirectory, UserAgent: "sharebox"}
	if d.cfg.ACMECAFile != "" {
		pool := x509.NewCertPool()
		b, err := os.ReadFile(d.cfg.ACMECAFile)
		if err != nil || !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("cannot load ACME CA file")
		}
		c.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}, Timeout: 30 * time.Second}
	}
	acct := &acme.Account{}
	if d.cfg.ACMEEmail != "" {
		acct.Contact = []string{"mailto:" + d.cfg.ACMEEmail}
	}
	if _, err := c.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("ACME account: %w", err)
	}
	d.client = c
	return c, nil
}

// IssueCert implements directImpl.
func (d *acmeDirect) IssueCert(ctx context.Context, deviceID, deviceKey, ip, csrB64 string) (string, error) {
	host, err := d.Register(deviceID, deviceKey, ip)
	if err != nil {
		return "", err
	}
	der, err := base64.StdEncoding.DecodeString(csrB64)
	if err != nil {
		return "", errors.New("malformed CSR")
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil || csr.CheckSignature() != nil {
		return "", errors.New("invalid CSR")
	}
	if len(csr.DNSNames) != 1 || !strings.EqualFold(csr.DNSNames[0], host) || len(csr.IPAddresses) > 0 || len(csr.EmailAddresses) > 0 || len(csr.URIs) > 0 {
		return "", fmt.Errorf("CSR must contain exactly the name %s", host)
	}
	d.mu.Lock()
	dev := d.devices[deviceID]
	var recent []int64
	for _, t := range dev.Issued {
		if time.Since(time.Unix(t, 0)) < 24*time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= d.cfg.MaxCertsPerDevicePerDay {
		d.mu.Unlock()
		return "", errors.New("certificate limit reached for today; try again tomorrow")
	}
	d.mu.Unlock()

	c, err := d.acmeClient(ctx)
	if err != nil {
		return "", err
	}
	order, err := c.AuthorizeOrder(ctx, acme.DomainIDs(host))
	if err != nil {
		return "", fmt.Errorf("ACME order: %w", err)
	}
	key := strings.ToLower(dns.Fqdn(host))
	defer func() {
		d.mu.Lock()
		delete(d.txt, key)
		d.mu.Unlock()
	}()
	for _, u := range order.AuthzURLs {
		z, err := c.GetAuthorization(ctx, u)
		if err != nil {
			return "", err
		}
		if z.Status == acme.StatusValid {
			continue
		}
		var chal *acme.Challenge
		for _, ch := range z.Challenges {
			if ch.Type == "dns-01" {
				chal = ch
			}
		}
		if chal == nil {
			return "", errors.New("ACME server offered no dns-01 challenge")
		}
		val, err := c.DNS01ChallengeRecord(chal.Token)
		if err != nil {
			return "", err
		}
		d.mu.Lock()
		d.txt[key] = append(d.txt[key], val)
		d.mu.Unlock()
		if _, err := c.Accept(ctx, chal); err != nil {
			return "", fmt.Errorf("ACME accept: %w", err)
		}
		if _, err := c.WaitAuthorization(ctx, z.URI); err != nil {
			return "", fmt.Errorf("ACME validation: %w", err)
		}
	}
	ready, err := c.WaitOrder(ctx, order.URI)
	if err != nil {
		return "", fmt.Errorf("ACME order: %w", err)
	}
	chain, _, err := c.CreateCertFromOrder(ctx, ready, der, true)
	if err != nil {
		return "", fmt.Errorf("ACME finalize: %w", err)
	}
	var out strings.Builder
	for _, b := range chain {
		pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: b})
	}
	d.mu.Lock()
	dev.Issued = append(recent, time.Now().Unix())
	d.saveLocked()
	d.mu.Unlock()
	d.log.Info("issued direct certificate", "host", host)
	return out.String(), nil
}
