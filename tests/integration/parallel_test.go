package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"sharebox/internal/conformance"
	"sharebox/internal/rtc"
	"sharebox/internal/tunnel"
)

// A guest opens several WebRTC connections and downloads Range pieces of a
// file over all of them at once; the host groups them, and revoking the
// guest closes every connection.
func TestParallelConnections(t *testing.T) {
	in := startInfra(t, true)
	dir := t.TempDir()
	data := make([]byte, 20<<20+777)
	rand.Read(data)
	os.WriteFile(filepath.Join(dir, "big.bin"), data, 0o644)
	e := startHost(t, in, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	g, err := rtc.DialGuest(ctx, in.url, e.SessionID(), e.AccessKey(), rtc.GuestOptions{AllowLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.Parallel != 4 {
		t.Fatalf("host offered %d connections, want 4", g.Parallel)
	}
	sessions := make([]*tunnel.Session, g.Parallel)
	sessions[0], _ = g.Session(0)
	if sessions[0] == nil {
		t.Fatal("no primary connection")
	}
	for i := 1; i < g.Parallel; i++ {
		s, _, err := g.Open(ctx, i)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		sessions[i] = s
	}
	// One login; the token works on every connection.
	c, st := conformance.Login(t, &conformance.Env{Client: &http.Client{Transport: sessions[0]}, BaseURL: "http://tunnel"}, e.AccessKey(), "")
	if st != 200 {
		t.Fatalf("login: %d", st)
	}

	piece := (len(data) + len(sessions) - 1) / len(sessions)
	got := make([][]byte, len(sessions))
	var wg sync.WaitGroup
	errs := make(chan error, len(sessions))
	for i, s := range sessions {
		wg.Add(1)
		go func(i int, s *tunnel.Session) {
			defer wg.Done()
			start, end := i*piece, min((i+1)*piece, len(data))-1
			req, _ := http.NewRequest("GET", "http://tunnel/api/download/big.bin?path=big.bin", nil)
			req.Header.Set("Authorization", "Bearer "+c.Token)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
			req.Header.Set("X-Sharebox-Transfer", "dl1")
			res, err := (&http.Client{Transport: s}).Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer res.Body.Close()
			if res.StatusCode != 206 {
				errs <- fmt.Errorf("piece %d: HTTP %d", i, res.StatusCode)
				return
			}
			got[i], err = io.ReadAll(res.Body)
			if err != nil {
				errs <- err
			}
		}(i, s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.Join(got, nil), data) {
		t.Fatal("reassembled pieces differ from the file")
	}

	// The host sees four connections of one guest, and one transfer.
	s := e.Status()
	groups := map[string]int{}
	for _, p := range s.Peers {
		groups[p.Group]++
	}
	if len(groups) != 1 {
		t.Fatalf("expected one guest group, got %v", groups)
	}
	for _, n := range groups {
		if n != 4 {
			t.Fatalf("expected 4 connections, got %d", n)
		}
	}
	if n := len(s.Stats.Active) + len(s.Stats.Recent); n != 1 {
		t.Fatalf("expected the pieces to show as one transfer, got %d", n)
	}

	// Revoking the guest closes every one of its connections.
	if len(s.Stats.Guests) != 1 {
		t.Fatalf("guests: %+v", s.Stats.Guests)
	}
	e.RevokeGuest(s.Stats.Guests[0].ID)
	for i, sess := range sessions {
		select {
		case <-sess.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("connection %d survived revocation", i)
		}
	}
}
