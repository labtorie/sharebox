package tunnel_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"sharebox/internal/conformance"
	"sharebox/internal/fileserver"
	"sharebox/internal/fsroot"
	"sharebox/internal/tunnel"
	p "sharebox/shared/protocol"
)

type pair struct {
	client, server *tunnel.Session
	cl, sl         *tunnel.MemLink
}

func newPair(t *testing.T, h http.Handler, tweak func(cl, sl *tunnel.MemLink)) *pair {
	cl, sl := tunnel.NewMemPair()
	if tweak != nil {
		tweak(cl, sl)
	}
	server := tunnel.New(sl, tunnel.Options{Handler: h})
	client := tunnel.New(cl, tunnel.Options{})
	cl.Bind(client, server)
	sl.Bind(server, client)
	t.Cleanup(func() { client.Close(nil); server.Close(nil) })
	return &pair{client, server, cl, sl}
}

func newEnv(t *testing.T, dir string, cfg fileserver.Config) *conformance.Env {
	root, err := fsroot.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := fileserver.New(fileserver.Options{Root: root, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	pr := newPair(t, srv, nil)
	t.Cleanup(func() { srv.Close(); root.Close() })
	return &conformance.Env{Client: &http.Client{Transport: pr.client}, BaseURL: "http://tunnel", Dir: dir, Server: srv}
}

// The full guest API behaves identically over the tunnel.
func TestTunnelConformance(t *testing.T) { conformance.Run(t, newEnv) }

func TestTunnelLargeStreamBoundedMemory(t *testing.T) {
	size := int64(1 << 30)
	if testing.Short() {
		size = 64 << 20
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			n, _ := io.Copy(io.Discard, r.Body)
			w.Write([]byte(strings.Repeat("x", 0)))
			binary.Write(w, binary.BigEndian, n)
			return
		}
		io.CopyN(w, zeroReader{}, size)
	})
	pr := newPair(t, h, nil)
	c := &http.Client{Transport: pr.client}

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	res, err := c.Get("http://tunnel/big")
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if err != nil || n != size {
		t.Fatalf("download %d bytes: %v", n, err)
	}
	t.Logf("download %d MiB in %v", size>>20, time.Since(start).Round(time.Millisecond))

	// Upload the same amount.
	req, _ := http.NewRequest("PUT", "http://tunnel/up", io.LimitReader(zeroReader{}, size))
	res, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got int64
	binary.Read(res.Body, binary.BigEndian, &got)
	res.Body.Close()
	if got != size {
		t.Fatalf("server received %d of %d", got, size)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapInuse > before.HeapInuse+128<<20 {
		t.Fatalf("heap grew by %d MiB", (after.HeapInuse-before.HeapInuse)>>20)
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

// A data frame that arrives out of sequence must fail the transfer instead of
// silently producing corrupt output.
func TestTunnelDetectsSequenceCorruption(t *testing.T) {
	payload := make([]byte, 200_000)
	rand.Read(payload)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
	dropped := false
	pr := newPair(t, h, func(cl, sl *tunnel.MemLink) {
		sl.Mutate = func(m []byte) []byte {
			if m[0] == p.FrameData && binary.BigEndian.Uint32(m[5:9]) == 3 && !dropped {
				dropped = true
				return nil // lose one frame
			}
			return m
		}
	})
	res, err := (&http.Client{Transport: pr.client}).Get("http://tunnel/x")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(res.Body)
	if err == nil {
		t.Fatalf("corruption not detected (%d bytes, equal=%v)", len(got), bytes.Equal(got, payload))
	}
}

// When the link dies mid-download the client gets an error, and resuming
// with a Range request over a new link yields the exact original bytes.
func TestTunnelDisconnectAndResume(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 3_000_000)
	rand.Read(data)
	os.WriteFile(filepath.Join(dir, "f.bin"), data, 0o644)
	cfg := conformance.NewConfig()
	root, _ := fsroot.Open(dir)
	defer root.Close()
	srv, _ := fileserver.New(fileserver.Options{Root: root, Config: cfg})
	defer srv.Close()
	tok, _ := srv.Issuer().Issue("g1")

	get := func(c *http.Client, rng string) ([]byte, error) {
		req, _ := http.NewRequest("GET", "http://tunnel/api/download/f?path=f.bin", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		res, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		return io.ReadAll(res.Body)
	}

	pr := newPair(t, srv, func(cl, sl *tunnel.MemLink) { sl.DropAfter = 60 })
	part, err := get(&http.Client{Transport: pr.client}, "")
	if err == nil {
		t.Fatal("expected failure after link drop")
	}
	select {
	case <-pr.client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("client session not closed after link drop")
	}
	pr2 := newPair(t, srv, nil)
	rest, err := get(&http.Client{Transport: pr2.client}, "bytes="+strconv.Itoa(len(part))+"-")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(part, rest...), data) {
		t.Fatalf("resumed data differs (%d + %d bytes)", len(part), len(rest))
	}
}

// Cancelling a request on the client stops the handler on the host.
func TestTunnelCancellation(t *testing.T) {
	stopped := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(w, zeroReader{})
		stopped <- err
	})
	pr := newPair(t, h, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://tunnel/inf", nil)
	res, err := (&http.Client{Transport: pr.client}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.CopyN(io.Discard, res.Body, 5<<20)
	cancel()
	res.Body.Close()
	select {
	case err := <-stopped:
		if err == nil {
			t.Fatal("handler finished without error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host kept sending after cancellation")
	}
}

// A peer that ignores flow control is disconnected from the stream.
func TestTunnelRejectsWindowViolation(t *testing.T) {
	gotErr := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // not reading: no credit is returned
		_, err := io.Copy(io.Discard, r.Body)
		gotErr <- err
	})
	cl, sl := tunnel.NewMemPair()
	server := tunnel.New(sl, tunnel.Options{Handler: h})
	sl.Bind(server, nil)
	defer server.Close(nil)
	// Hand-crafted malicious client: headers then far more data than allowed,
	// without ever reading WINDOW frames.
	frame := func(typ byte, id, seq uint32, b []byte) []byte {
		m := make([]byte, 9+len(b))
		m[0] = typ
		binary.BigEndian.PutUint32(m[1:], id)
		binary.BigEndian.PutUint32(m[5:], seq)
		copy(m[9:], b)
		return m
	}
	server.Deliver(frame(p.FrameHeaders, 1, 0, []byte(`{"m":"PUT","u":"/x","h":{},"b":true}`)))
	chunk := make([]byte, p.MaxPayload)
	for i := 0; i < (p.InitialWindow/p.MaxPayload)*3; i++ {
		server.Deliver(frame(p.FrameData, 1, uint32(i), chunk))
	}
	_ = cl
	select {
	case err := <-gotErr:
		if err == nil {
			t.Fatal("window violation accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler not failed")
	}
}

// Header names from browsers are not canonical ("X-Chunk-SHA256"); the host
// must still see them, or digest checks would be silently skipped.
func TestTunnelCanonicalisesHeaders(t *testing.T) {
	got := make(chan string, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got <- r.Header.Get("X-Chunk-Sha256") })
	cl, sl := tunnel.NewMemPair()
	server := tunnel.New(sl, tunnel.Options{Handler: h})
	sl.Bind(server, nil)
	defer server.Close(nil)
	m := make([]byte, 9)
	m[0] = p.FrameHeaders
	binary.BigEndian.PutUint32(m[1:], 1)
	server.Deliver(append(m, []byte(`{"m":"GET","u":"/x","h":{"X-Chunk-SHA256":["abc"]},"b":false}`)...))
	_ = cl
	select {
	case v := <-got:
		if v != "abc" {
			t.Fatalf("header not found: %q", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler not called")
	}
}
