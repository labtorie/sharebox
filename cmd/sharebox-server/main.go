// Command sharebox-server runs the Sharebox coordination service: guest web
// app, WebRTC signalling, reachability probes, an embedded TURN relay and
// (optionally) DNS + ACME certificates for direct HTTPS.
//
// Quick local development (everything on localhost):
//
//	sharebox-server -dev
//
// Production (see docs/deploy-server.md):
//
//	sharebox-server -public-url https://share.example.org -autocert \
//	    -turn -turn-public-ip 203.0.113.10 -turn-secret "$SECRET"
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"sharebox/internal/security"
	"sharebox/services/relay"
	"sharebox/services/signaling"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		listen     = flag.String("listen", env("SHAREBOX_LISTEN", ":443"), "HTTP(S) listen address")
		publicURL  = flag.String("public-url", env("SHAREBOX_PUBLIC_URL", ""), "external base URL, e.g. https://share.example.org")
		tlsCert    = flag.String("tls-cert", env("SHAREBOX_TLS_CERT", ""), "TLS certificate file")
		tlsKey     = flag.String("tls-key", env("SHAREBOX_TLS_KEY", ""), "TLS key file")
		useAuto    = flag.Bool("autocert", env("SHAREBOX_AUTOCERT", "") == "1", "obtain a Let's Encrypt certificate for the public URL host (needs port 443 reachable)")
		plainHTTP  = flag.Bool("http", env("SHAREBOX_PLAIN_HTTP", "") == "1", "serve plain HTTP (only behind a TLS-terminating reverse proxy)")
		trustProxy = flag.Bool("trust-proxy", env("SHAREBOX_TRUST_PROXY", "") == "1", "use X-Forwarded-For for client IPs")
		dataDir    = flag.String("data-dir", env("SHAREBOX_DATA_DIR", "./sharebox-data"), "directory for certificates and state")
		stunURLs   = flag.String("stun-urls", env("SHAREBOX_STUN_URLS", ""), "comma-separated extra STUN URLs for clients")

		turnOn     = flag.Bool("turn", env("SHAREBOX_TURN", "") == "1", "run the embedded TURN relay")
		turnIP     = flag.String("turn-public-ip", env("SHAREBOX_TURN_PUBLIC_IP", ""), "public IPv4 address of this server (relay candidates)")
		turnUDP    = flag.String("turn-udp", env("SHAREBOX_TURN_UDP", ":3478"), "TURN/STUN UDP listen address")
		turnTCP    = flag.String("turn-tcp", env("SHAREBOX_TURN_TCP", ":3478"), "TURN TCP listen address (empty to disable)")
		turnTLS    = flag.String("turn-tls", env("SHAREBOX_TURN_TLS", ""), "TURN over TLS listen address, e.g. :5349 (uses the HTTPS certificate)")
		turnSecret = flag.String("turn-secret", env("SHAREBOX_TURN_SECRET", ""), "shared secret for TURN credentials")
		turnHost   = flag.String("turn-host", env("SHAREBOX_TURN_HOST", ""), "hostname clients use for TURN (default: public URL host)")
		turnPorts  = flag.String("turn-ports", env("SHAREBOX_TURN_PORTS", "49160-49999"), "UDP port range for relayed traffic")
		turnURLs   = flag.String("turn-urls", env("SHAREBOX_TURN_URLS", ""), "override advertised TURN URLs (comma-separated); use with an external relay such as coturn")

		directZone = flag.String("direct-zone", env("SHAREBOX_DIRECT_ZONE", ""), "DNS zone delegated to this server for direct-HTTPS host names, e.g. direct.example.org")
		dnsListen  = flag.String("dns-listen", env("SHAREBOX_DNS_LISTEN", ":53"), "authoritative DNS listen address for -direct-zone")
		dnsNS      = flag.String("dns-ns", env("SHAREBOX_DNS_NS", ""), "public host name of this DNS server (NS record), e.g. ns1.example.org")
		acmeDir    = flag.String("acme-directory", env("SHAREBOX_ACME_DIRECTORY", "https://acme-v02.api.letsencrypt.org/directory"), "ACME directory URL for host certificates")
		acmeEmail  = flag.String("acme-email", env("SHAREBOX_ACME_EMAIL", ""), "contact e-mail for the ACME account")
		acmeCA     = flag.String("acme-ca-file", env("SHAREBOX_ACME_CA_FILE", ""), "extra CA bundle to trust the ACME server (testing with Pebble)")

		dev     = flag.Bool("dev", false, "local development mode: http://127.0.0.1:8080, TURN on 127.0.0.1:3478")
		verbose = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *dev {
		if *publicURL == "" {
			*publicURL = "http://127.0.0.1:8080"
		}
		if *listen == ":443" {
			*listen = "127.0.0.1:8080"
		}
		*plainHTTP = true
		*turnOn = true
		if *turnIP == "" {
			*turnIP = "127.0.0.1"
		}
		set := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if !set["turn-udp"] {
			*turnUDP = "127.0.0.1:3478"
		}
		if !set["turn-tcp"] {
			*turnTCP = *turnUDP
		}
		if *turnSecret == "" {
			*turnSecret = security.RandomToken(24)
		}
		if *turnHost == "" {
			*turnHost = "127.0.0.1"
		}
	}
	if *publicURL == "" {
		fail(log, "-public-url is required (e.g. https://share.example.org), or use -dev")
	}
	pu, err := url.Parse(*publicURL)
	if err != nil || pu.Host == "" {
		fail(log, "invalid -public-url")
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		fail(log, err.Error())
	}

	// TLS for the web/signalling endpoint.
	var tlsConf *tls.Config
	var am *autocert.Manager
	switch {
	case *plainHTTP:
	case *tlsCert != "" && *tlsKey != "":
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			fail(log, "loading TLS certificate: "+err.Error())
		}
		tlsConf = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	case *useAuto:
		am = &autocert.Manager{Prompt: autocert.AcceptTOS, HostPolicy: autocert.HostWhitelist(pu.Hostname()),
			Cache: autocert.DirCache(filepath.Join(*dataDir, "autocert")), Email: *acmeEmail}
		tlsConf = am.TLSConfig()
		tlsConf.MinVersion = tls.VersionTLS12
	default:
		fail(log, "choose -tls-cert/-tls-key, -autocert, or -http (behind a TLS proxy)")
	}

	cfg := signaling.Config{PublicURL: *publicURL, TrustProxy: *trustProxy, Dev: *dev, Logger: log}
	for _, s := range strings.Split(*stunURLs, ",") {
		if s = strings.TrimSpace(s); s != "" {
			cfg.STUNURLs = append(cfg.STUNURLs, s)
		}
	}

	var rl *relay.Server
	if *turnOn || *turnURLs != "" {
		if *turnSecret == "" {
			fail(log, "-turn-secret (or SHAREBOX_TURN_SECRET) is required for the relay")
		}
		cfg.TURNSecret = *turnSecret
		host := *turnHost
		if host == "" {
			host = pu.Hostname()
		}
		if *turnURLs != "" {
			cfg.TURNURLs = splitList(*turnURLs)
		} else {
			_, up, _ := net.SplitHostPort(*turnUDP)
			cfg.TURNURLs = []string{fmt.Sprintf("turn:%s:%s?transport=udp", host, up)}
			cfg.STUNURLs = append([]string{fmt.Sprintf("stun:%s:%s", host, up)}, cfg.STUNURLs...)
			if *turnTCP != "" {
				_, tp, _ := net.SplitHostPort(*turnTCP)
				cfg.TURNURLs = append(cfg.TURNURLs, fmt.Sprintf("turn:%s:%s?transport=tcp", host, tp))
			}
			if *turnTLS != "" && tlsConf != nil {
				_, tp, _ := net.SplitHostPort(*turnTLS)
				cfg.TURNURLs = append(cfg.TURNURLs, fmt.Sprintf("turns:%s:%s?transport=tcp", host, tp))
			}
		}
		if *turnOn {
			if *turnIP == "" {
				fail(log, "-turn-public-ip is required for the embedded relay")
			}
			minP, maxP := parseRange(*turnPorts)
			rl, err = relay.Start(relay.Config{PublicIP: *turnIP, ListenUDP: *turnUDP, ListenTCP: *turnTCP,
				ListenTLS: *turnTLS, TLSConfig: tlsConf, Secret: *turnSecret, MinPort: minP, MaxPort: maxP,
				AllowPrivatePeers: *dev, Logger: log})
			if err != nil {
				fail(log, err.Error())
			}
			defer rl.Close()
		}
	}

	if *directZone != "" {
		ds, err := signaling.NewDirect(signaling.DirectConfig{
			Zone: *directZone, DNSListen: *dnsListen, NS: *dnsNS, ACMEDirectory: *acmeDir, ACMEEmail: *acmeEmail,
			ACMECAFile: *acmeCA, DataDir: *dataDir, Logger: log,
		})
		if err != nil {
			fail(log, "direct HTTPS: "+err.Error())
		}
		defer ds.Close()
		cfg.Direct = ds
	}

	srv, err := signaling.New(cfg)
	if err != nil {
		fail(log, err.Error())
	}
	defer srv.Close()

	hs := &http.Server{Addr: *listen, Handler: srv, TLSConfig: tlsConf,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	if am != nil {
		// ACME HTTP-01 challenges and HTTP->HTTPS redirect.
		go http.ListenAndServe(":80", am.HTTPHandler(nil))
	}
	go func() {
		log.Info("sharebox server listening", "addr", *listen, "public", *publicURL, "relay", rl != nil, "direct", cfg.Direct != nil)
		var err error
		if tlsConf != nil {
			err = hs.ListenAndServeTLS("", "")
		} else {
			err = hs.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fail(log, err.Error())
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hs.Shutdown(ctx)
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func parseRange(s string) (uint16, uint16) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0
	}
	x, _ := strconv.Atoi(a)
	y, _ := strconv.Atoi(b)
	return uint16(x), uint16(y)
}

func fail(log *slog.Logger, msg string) {
	log.Error(msg)
	os.Exit(1)
}
