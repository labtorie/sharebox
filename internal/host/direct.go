package host

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"sharebox/internal/network"
	"sharebox/internal/security"
	p "sharebox/shared/protocol"
)

// mapPorts maps the ICE UDP port and the direct HTTPS TCP port on the
// router and keeps the mappings renewed.
func (e *Engine) mapPorts(ctx context.Context) {
	e.mu.Lock()
	old := e.mappings
	e.mappings = nil
	port := e.udpPort
	e.mu.Unlock()
	e.unmap(old)
	if port == 0 {
		return
	}
	e.update(func(s *Status) { s.Mapping = Check{State: CheckPending} })
	mctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	um, uerr := e.mapper.Map(mctx, "udp", port, "Sharebox", 2*time.Hour)
	var tm *network.Mapping
	var terr error
	if uerr == nil {
		tm, terr = e.mapper.Map(mctx, "tcp", port, "Sharebox HTTPS", 2*time.Hour)
	} else {
		terr = uerr
	}
	var maps []*network.Mapping
	for _, m := range []*network.Mapping{um, tm} {
		if m != nil {
			maps = append(maps, m)
		}
	}
	e.mu.Lock()
	e.mappings = maps
	e.mu.Unlock()

	switch {
	case uerr != nil:
		e.update(func(s *Status) { s.Mapping = Check{State: CheckUnavailable, Detail: shortErr(uerr)} })
		return
	case !network.IsPublicIP(um.ExternalIP):
		e.update(func(s *Status) {
			s.Mapping = Check{State: CheckWarn, Detail: fmt.Sprintf("Router mapped the port via %s, but its external address %s is not public (your provider may use carrier-grade NAT)", strings.ToUpper(um.Method), um.ExternalIP)}
		})
	default:
		detail := fmt.Sprintf("%s mapped port %d on %s", strings.ToUpper(um.Method), um.External, um.ExternalIP)
		if terr != nil {
			detail += "; TCP mapping failed"
		}
		e.update(func(s *Status) { s.Mapping = Check{State: CheckOK, Detail: detail} })
		// Advertise the mapped address to guests as a candidate. This only
		// works if the router kept the same external port.
		if um.External == um.Internal {
			e.rebuildRTC(um.ExternalIP)
		}
	}

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.renewLoop(ctx, maps)
	}()
}

func (e *Engine) renewLoop(ctx context.Context, maps []*network.Mapping) {
	for {
		next := time.Hour
		for _, m := range maps {
			if m.Lifetime > 0 && m.Lifetime/2 < next {
				next = max(m.Lifetime/2, time.Minute)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
		e.mu.Lock()
		current := len(e.mappings) > 0 && len(maps) > 0 && e.mappings[0] == maps[0]
		e.mu.Unlock()
		if !current {
			return // replaced by a newer mapping round
		}
		for _, m := range maps {
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := e.mapper.Renew(rctx, m)
			cancel()
			if err != nil {
				e.addError("Router port mapping expired and could not be renewed; retrying")
				go e.mapPorts(ctx)
				go e.verify()
				return
			}
		}
	}
}

func (e *Engine) unmap(maps []*network.Mapping) {
	for _, m := range maps {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := e.mapper.Delete(ctx, m); err != nil {
			e.log.Debug("port mapping removal failed", "err", err)
		}
		cancel()
	}
}

// rebuildRTC recreates the WebRTC endpoint so it advertises publicIP. It is
// skipped while guests are connected.
func (e *Engine) rebuildRTC(publicIP string) {
	e.mu.Lock()
	h := e.rtcHost
	e.mu.Unlock()
	if h == nil || len(h.Peers()) > 0 {
		return
	}
	h.Close() // frees the port for the new endpoint
	nh, err := e.newRTC(publicIP)
	if err != nil {
		e.addError("Could not reopen the network port: " + err.Error())
		return
	}
	e.mu.Lock()
	e.rtcHost = nh
	e.mu.Unlock()
}

// verifyDirect enables direct HTTPS only when every precondition holds and
// the coordination server has connected back to us from the internet.
func (e *Engine) verifyDirect(ctx context.Context) {
	e.mu.Lock()
	st, reg, maps, cc := e.settings, e.reg, e.mappings, e.coord
	e.mu.Unlock()
	setDirect := func(c Check, url string) {
		e.update(func(s *Status) { s.Direct = c })
		if cc != nil {
			_, _ = cc.Request(ctx, p.Envelope{Type: p.MsgSetDirect, Direct: url})
		}
	}
	if !st.PortMapping {
		e.update(func(s *Status) { s.Direct = Check{State: CheckOff, Detail: "Automatic port mapping is turned off"} })
		return
	}
	var tm *network.Mapping
	for _, m := range maps {
		if m.Protocol == "tcp" {
			tm = m
		}
	}
	if tm == nil {
		setDirect(Check{State: CheckUnavailable, Detail: "Router port mapping is not available"}, "")
		return
	}
	if reg.Features == nil || !reg.Features.Direct || reg.Hostname == "" {
		setDirect(Check{State: CheckUnavailable, Detail: "The Sharebox server does not provide certificates for direct connections"}, "")
		return
	}
	e.update(func(s *Status) { s.Direct = Check{State: CheckPending, Detail: "Obtaining certificate"} })
	cert, err := e.directCert(ctx, reg.Hostname)
	if err != nil {
		setDirect(Check{State: CheckUnavailable, Detail: "Certificate: " + shortErr(err)}, "")
		return
	}
	ds, err := e.ensureDirectServer(tm.Internal, cert)
	if err != nil {
		setDirect(Check{State: CheckUnavailable, Detail: shortErr(err)}, "")
		return
	}
	e.update(func(s *Status) {
		s.Direct = Check{State: CheckPending, Detail: "Testing reachability from the internet"}
	})
	pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	nonce, res, err := cc.Probe(pctx, tm.External, reg.Hostname)
	cancel()
	if err != nil || !res.OK || !security.Equal(res.Body, ds.probeAnswer(nonce)) {
		msg := "Port is mapped but not reachable from the internet"
		if err != nil {
			msg += " (" + shortErr(err) + ")"
		}
		ds.close()
		e.mu.Lock()
		e.direct = nil
		e.mu.Unlock()
		setDirect(Check{State: CheckUnavailable, Detail: msg}, "")
		return
	}
	url := fmt.Sprintf("https://%s:%d", reg.Hostname, tm.External)
	setDirect(Check{State: CheckOK, Detail: "Reachable at " + url}, url)
}

// --- direct HTTPS server ----------------------------------------------------

type directServer struct {
	srv      *http.Server
	port     int
	probeKey []byte
	mu       sync.Mutex
	cert     *tls.Certificate
}

func (d *directServer) probeAnswer(nonce string) string {
	return b64(security.HMAC(d.probeKey, "sharebox-probe-v1", nonce))
}

func (d *directServer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = d.srv.Shutdown(ctx)
}

func (e *Engine) ensureDirectServer(port int, cert *tls.Certificate) (*directServer, error) {
	e.mu.Lock()
	ds, files := e.direct, e.files
	e.mu.Unlock()
	if ds != nil && ds.port == port {
		ds.mu.Lock()
		ds.cert = cert
		ds.mu.Unlock()
		return ds, nil
	}
	if ds != nil {
		ds.close()
	}
	ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("cannot listen for direct connections: %w", err)
	}
	k := make([]byte, 32)
	rand.Read(k)
	ds = &directServer{port: port, probeKey: k, cert: cert}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+p.ProbePath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(ds.probeAnswer(r.URL.Query().Get("n"))))
	})
	mux.Handle("/", files)
	ds.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				ds.mu.Lock()
				defer ds.mu.Unlock()
				return ds.cert, nil
			},
		},
	}
	go ds.srv.ServeTLS(ln, "", "")
	e.mu.Lock()
	e.direct = ds
	e.mu.Unlock()
	return ds, nil
}

// directCert returns a cached certificate for hostname or obtains one from
// the coordination server. The private key never leaves this machine: only
// a CSR is sent.
func (e *Engine) directCert(ctx context.Context, hostname string) (*tls.Certificate, error) {
	keyPath, certPath := e.store.Path("direct.key"), e.store.Path("direct.crt")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil && c.Leaf != nil {
		if c.Leaf.VerifyHostname(hostname) == nil && time.Until(c.Leaf.NotAfter) > 30*24*time.Hour {
			return &c, nil
		}
	}
	var key *ecdsa.PrivateKey
	if b, err := os.ReadFile(keyPath); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			key, _ = x509.ParseECPrivateKey(blk.Bytes)
		}
	}
	if key == nil {
		var err error
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, err
		}
		der, _ := x509.MarshalECPrivateKey(key)
		if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, err
		}
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: hostname}, DNSNames: []string{hostname},
	}, key)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	cc, dev := e.coord, e.device
	e.mu.Unlock()
	if cc == nil {
		return nil, errors.New("not connected")
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	res, err := cc.Request(rctx, p.Envelope{Type: p.MsgCert, CSR: b64(csr), DeviceID: dev.ID, DeviceKey: dev.Key})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, []byte(res.Cert), 0o600); err != nil {
		return nil, err
	}
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("server returned an invalid certificate: %w", err)
	}
	return &c, nil
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
