package fileserver_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"sharebox/internal/conformance"
	"sharebox/internal/fileserver"
	"sharebox/internal/fsroot"
)

func newDirect(t *testing.T, dir string, cfg fileserver.Config) *conformance.Env {
	root, err := fsroot.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := fileserver.New(fileserver.Options{Root: root, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewTLSServer(srv)
	t.Cleanup(func() { ts.Close(); srv.Close(); root.Close() })
	return &conformance.Env{Client: ts.Client(), BaseURL: ts.URL, Dir: dir, Server: srv}
}

func TestDirectHTTPS(t *testing.T) { conformance.Run(t, newDirect) }

// TestLargeFileStreaming downloads an 11 GiB sparse file and checks that the
// process heap stays small, proving transfers are streamed.
func TestLargeFileStreaming(t *testing.T) {
	if testing.Short() {
		t.Skip("large file test")
	}
	dir := t.TempDir()
	const size = 11 << 30
	f, err := os.Create(filepath.Join(dir, "huge.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Skip("filesystem does not support sparse files:", err)
	}
	f.WriteAt([]byte("END!"), size-4)
	f.Close()

	e := newDirect(t, dir, conformance.NewConfig())
	cfg := e.Server.Config()
	c, _ := conformance.Login(t, e, cfg.AccessKey, "")
	req, _ := http.NewRequest("GET", e.BaseURL+"/api/download/huge.bin?path=huge.bin", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := e.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var peak uint64
	done := make(chan struct{})
	go func() {
		var ms runtime.MemStats
		for {
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > peak {
					peak = ms.HeapInuse
				}
			}
		}
	}()
	start := time.Now()
	buf := make([]byte, 1<<20)
	var n int64
	var last []byte
	for {
		k, err := res.Body.Read(buf)
		n += int64(k)
		if k >= 4 {
			last = append(last[:0], buf[k-4:k]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	if n != size || string(last) != "END!" {
		t.Fatalf("got %d bytes, tail %q", n, last)
	}
	t.Logf("streamed %d GiB in %v (%.0f MB/s), peak heap %d MiB", size>>30, time.Since(start).Round(time.Millisecond),
		float64(size)/time.Since(start).Seconds()/1e6, peak>>20)
	if peak > 256<<20 {
		t.Fatalf("heap grew to %d MiB while streaming", peak>>20)
	}
	_ = fmt.Sprint()
}
