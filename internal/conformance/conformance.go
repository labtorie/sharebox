// Package conformance is a transport-independent test suite for the guest
// API. It is run against direct HTTPS and against the WebRTC tunnel (both
// peer-to-peer and through a TURN relay) to prove that every transport
// exposes identical behaviour.
package conformance

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebox/internal/fileserver"
	"sharebox/internal/security"
)

// Env is one running host plus a client connected through some transport.
type Env struct {
	Client  *http.Client
	BaseURL string // e.g. https://127.0.0.1:1234 or http://tunnel
	Dir     string // shared directory on disk
	Server  *fileserver.Server
	Key     string // access key
}

// Factory starts a host sharing dir with cfg and returns a connected client.
type Factory func(t *testing.T, dir string, cfg fileserver.Config) *Env

// NewConfig returns a config with a random key and default permissions.
func NewConfig() fileserver.Config {
	b := make([]byte, 32)
	rand.Read(b)
	return fileserver.Config{
		Perms: fileserver.DefaultPermissions(), Limits: fileserver.DefaultLimits(),
		AccessKey: hex.EncodeToString(b),
	}
}

// Client is a small API client used by the suite.
type Client struct {
	t     testing.TB
	e     *Env
	Token string
}

func (c *Client) do(method, path string, body io.Reader, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(method, c.e.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return c.e.Client.Do(req)
}

// JSON performs a request and decodes a JSON reply, returning the status.
func (c *Client) JSON(method, path string, in any, out any) int {
	c.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	res, err := c.do(method, path, body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if out != nil {
		_ = json.Unmarshal(data, out)
	}
	return res.StatusCode
}

// Login authenticates with the access key.
func Login(t testing.TB, e *Env, key, password string) (*Client, int) {
	c := &Client{t: t, e: e}
	var out struct {
		Token string `json:"token"`
	}
	st := c.JSON("POST", "/api/auth", map[string]string{"key": key, "password": password}, &out)
	c.Token = out.Token
	return c, st
}

// Upload sends data as a resumable upload in chunks and completes it.
func (c *Client) Upload(dir, name string, data []byte, chunk int) (string, int) {
	c.t.Helper()
	var cr struct {
		ID string `json:"id"`
	}
	if st := c.JSON("POST", "/api/uploads", map[string]any{"dir": dir, "name": name, "size": len(data)}, &cr); st != 201 {
		return "", st
	}
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		sum := sha256.Sum256(data[off:end])
		res, err := c.do("PUT", fmt.Sprintf("/api/uploads/%s?offset=%d", cr.ID, off), bytes.NewReader(data[off:end]),
			map[string]string{"X-Chunk-SHA256": hex.EncodeToString(sum[:])})
		if err != nil {
			c.t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			return "", res.StatusCode
		}
	}
	var done struct {
		Path string `json:"path"`
	}
	st := c.JSON("POST", "/api/uploads/"+cr.ID+"/complete", nil, &done)
	return done.Path, st
}

// Download fetches a file.
func (c *Client) Download(p string, hdr map[string]string) ([]byte, int, http.Header) {
	c.t.Helper()
	res, err := c.do("GET", "/api/download/f?path="+url.QueryEscape(p), nil, hdr)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return b, res.StatusCode, res.Header
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Run executes the whole suite.
func Run(t *testing.T, newEnv Factory) {
	setup := func(t *testing.T, mut func(*fileserver.Config)) (*Env, *Client) {
		base := t.TempDir()
		dir := filepath.Join(base, "share")
		write(t, filepath.Join(dir, "hello.txt"), []byte("hello world"))
		write(t, filepath.Join(dir, "docs", "a.md"), []byte("# A"))
		write(t, filepath.Join(dir, "docs", "deep", "b.bin"), randBytes(100_000))
		write(t, filepath.Join(base, "outside.txt"), []byte("TOP SECRET"))
		cfg := NewConfig()
		if mut != nil {
			mut(&cfg)
		}
		e := newEnv(t, dir, cfg)
		e.Key = cfg.AccessKey
		c, st := Login(t, e, cfg.AccessKey, "")
		if mut == nil && st != 200 {
			t.Fatalf("login failed: %d", st)
		}
		return e, c
	}

	t.Run("Unauthorized", func(t *testing.T) {
		e, _ := setup(t, nil)
		anon := &Client{t: t, e: e}
		for _, p := range []string{"/api/list?path=", "/api/session", "/api/download/x?path=hello.txt", "/api/zip/x?path="} {
			if st := anon.JSON("GET", p, nil, nil); st != 401 {
				t.Errorf("GET %s without token: %d", p, st)
			}
		}
		bad := &Client{t: t, e: e, Token: "forged.token"}
		if st := bad.JSON("GET", "/api/list?path=", nil, nil); st != 401 {
			t.Errorf("forged token accepted: %d", st)
		}
		if _, st := Login(t, e, "wrong-key", ""); st != 401 {
			t.Errorf("wrong key: %d", st)
		}
	})

	t.Run("ListAndBrowse", func(t *testing.T) {
		_, c := setup(t, nil)
		var out struct {
			Entries []struct {
				Name  string `json:"name"`
				Path  string `json:"path"`
				IsDir bool   `json:"isDir"`
			} `json:"entries"`
		}
		if st := c.JSON("GET", "/api/list?path=", nil, &out); st != 200 {
			t.Fatalf("list: %d", st)
		}
		if len(out.Entries) != 2 || out.Entries[0].Name != "docs" || !out.Entries[0].IsDir || out.Entries[1].Name != "hello.txt" {
			t.Fatalf("unexpected listing %+v", out.Entries)
		}
		if st := c.JSON("GET", "/api/list?path=docs/deep", nil, &out); st != 200 || len(out.Entries) != 1 || out.Entries[0].Path != "docs/deep/b.bin" {
			t.Fatalf("deep list: %d %+v", st, out.Entries)
		}
	})

	t.Run("TraversalBlocked", func(t *testing.T) {
		_, c := setup(t, nil)
		for _, p := range []string{"../outside.txt", "docs/../../outside.txt", "/../outside.txt", "..\\outside.txt", "%2e%2e/outside.txt"} {
			b, st, _ := c.Download(p, nil)
			if st == 200 || bytes.Contains(b, []byte("SECRET")) {
				t.Errorf("traversal %q: status %d", p, st)
			}
		}
		if st := c.JSON("GET", "/api/list?path=..", nil, nil); st == 200 {
			t.Error("list .. succeeded")
		}
		if st := c.JSON("POST", "/api/mkdir", map[string]string{"path": "../escape"}, nil); st == 201 {
			t.Error("mkdir outside succeeded")
		}
		if _, st := c.Upload("..", "x.txt", []byte("x"), 10); st == 200 {
			t.Error("upload outside succeeded")
		}
	})

	t.Run("SymlinkEscapeBlocked", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("needs symlink privilege")
		}
		e, c := setup(t, nil)
		os.Symlink(filepath.Join(filepath.Dir(e.Dir), "outside.txt"), filepath.Join(e.Dir, "link.txt"))
		os.Symlink(filepath.Dir(e.Dir), filepath.Join(e.Dir, "linkdir"))
		for _, p := range []string{"link.txt", "linkdir/outside.txt"} {
			if b, st, _ := c.Download(p, nil); st == 200 || bytes.Contains(b, []byte("SECRET")) {
				t.Errorf("symlink escape %q: %d", p, st)
			}
		}
		res, _ := c.do("GET", "/api/zip/x?path=", nil, nil)
		zb, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if bytes.Contains(zb, []byte("TOP SECRET")) {
			t.Error("zip followed symlink outside share")
		}
	})

	t.Run("DownloadAndRange", func(t *testing.T) {
		e, c := setup(t, nil)
		want, _ := os.ReadFile(filepath.Join(e.Dir, "docs/deep/b.bin"))
		got, st, h := c.Download("docs/deep/b.bin", nil)
		if st != 200 || !bytes.Equal(got, want) {
			t.Fatalf("download: %d, %d bytes", st, len(got))
		}
		if !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") {
			t.Error("missing attachment disposition")
		}
		part, st, _ := c.Download("docs/deep/b.bin", map[string]string{"Range": "bytes=1000-1999"})
		if st != 206 || !bytes.Equal(part, want[1000:2000]) {
			t.Fatalf("range: %d len=%d", st, len(part))
		}
		tail, st, _ := c.Download("docs/deep/b.bin", map[string]string{"Range": "bytes=99000-"})
		if st != 206 || !bytes.Equal(tail, want[99000:]) {
			t.Fatalf("open range: %d", st)
		}
		if _, st, _ := c.Download("docs", nil); st != 400 {
			t.Errorf("download folder: %d", st)
		}
		if _, st, _ := c.Download("nope.txt", nil); st != 404 {
			t.Errorf("missing file: %d", st)
		}
	})

	t.Run("HTMLNotRenderedInline", func(t *testing.T) {
		e, c := setup(t, nil)
		write(t, filepath.Join(e.Dir, "x.html"), []byte("<script>alert(1)</script>"))
		_, _, h := c.Download("x.html", nil)
		if ct := h.Get("Content-Type"); strings.Contains(ct, "html") {
			t.Errorf("html served as %s", ct)
		}
		if !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
			t.Error("missing sandbox CSP")
		}
	})

	t.Run("UploadRoundTrip", func(t *testing.T) {
		e, c := setup(t, nil)
		data := randBytes(3_500_000)
		p, st := c.Upload("docs", "up.bin", data, 1<<20)
		if st != 200 || p != "docs/up.bin" {
			t.Fatalf("upload: %d %q", st, p)
		}
		disk, _ := os.ReadFile(filepath.Join(e.Dir, "docs", "up.bin"))
		if !bytes.Equal(disk, data) {
			t.Fatal("uploaded content differs")
		}
		got, _, _ := c.Download("docs/up.bin", nil)
		if !bytes.Equal(got, data) {
			t.Fatal("download of upload differs")
		}
		// Same name again: never overwrite.
		p2, st := c.Upload("docs", "up.bin", []byte("second"), 100)
		if st != 200 || p2 != "docs/up (2).bin" {
			t.Fatalf("second upload: %d %q", st, p2)
		}
		if disk, _ := os.ReadFile(filepath.Join(e.Dir, "docs", "up.bin")); !bytes.Equal(disk, data) {
			t.Fatal("original overwritten")
		}
		// Empty file.
		if _, st := c.Upload("", "empty.txt", nil, 10); st != 200 {
			t.Fatalf("empty upload: %d", st)
		}
		// Hostile name is sanitised.
		p3, st := c.Upload("", "../../evil:name?.txt", []byte("x"), 10)
		if st != 200 || p3 != "evil_name_.txt" {
			t.Fatalf("hostile name: %d %q", st, p3)
		}
	})

	t.Run("UploadResumeAndCorruption", func(t *testing.T) {
		e, c := setup(t, nil)
		data := randBytes(2_000_000)
		var cr struct {
			ID string `json:"id"`
		}
		c.JSON("POST", "/api/uploads", map[string]any{"dir": "", "name": "r.bin", "size": len(data)}, &cr)
		put := func(off, end int, sum []byte, body io.Reader) (int, int64) {
			h := map[string]string{}
			if sum != nil {
				h["X-Chunk-SHA256"] = hex.EncodeToString(sum)
			}
			res, err := c.do("PUT", fmt.Sprintf("/api/uploads/%s?offset=%d", cr.ID, off), body, h)
			if err != nil {
				return 0, -1
			}
			defer res.Body.Close()
			var o struct {
				Offset int64 `json:"offset"`
			}
			json.NewDecoder(res.Body).Decode(&o)
			return res.StatusCode, o.Offset
		}
		sum := func(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

		// First chunk OK.
		if st, off := put(0, 500_000, sum(data[:500_000]), bytes.NewReader(data[:500_000])); st != 200 || off != 500_000 {
			t.Fatalf("chunk1: %d %d", st, off)
		}
		// Corrupted chunk is rejected and nothing is committed.
		bad := append([]byte(nil), data[500_000:1_000_000]...)
		bad[100] ^= 0xff
		if st, off := put(500_000, 1_000_000, sum(data[500_000:1_000_000]), bytes.NewReader(bad)); st != 422 || off != 500_000 {
			t.Fatalf("corrupt chunk: %d %d", st, off)
		}
		// Wrong offset is refused with the server's offset.
		if st, off := put(900_000, 1_000_000, nil, bytes.NewReader(data[900_000:1_000_000])); st != 409 || off != 500_000 {
			t.Fatalf("wrong offset: %d %d", st, off)
		}
		// Interrupted chunk (body shorter than digest covers) is rolled back.
		if st, off := put(500_000, 1_000_000, sum(data[500_000:1_000_000]), bytes.NewReader(data[500_000:700_000])); st == 200 || off != 500_000 {
			t.Fatalf("truncated chunk: %d %d", st, off)
		}
		// Resume point.
		var stt struct {
			Offset int64 `json:"offset"`
		}
		c.JSON("GET", "/api/uploads/"+cr.ID, nil, &stt)
		if stt.Offset != 500_000 {
			t.Fatalf("resume offset %d", stt.Offset)
		}
		// Completing early fails.
		if st := c.JSON("POST", "/api/uploads/"+cr.ID+"/complete", nil, nil); st != 409 {
			t.Fatalf("early complete: %d", st)
		}
		// Not visible before completion.
		if _, err := os.Stat(filepath.Join(e.Dir, "r.bin")); err == nil {
			t.Fatal("partial upload visible")
		}
		if st, _ := put(500_000, len(data), sum(data[500_000:]), bytes.NewReader(data[500_000:])); st != 200 {
			t.Fatalf("final chunk: %d", st)
		}
		// Bytes beyond the declared size are refused.
		if st, _ := put(len(data), len(data)+10, nil, bytes.NewReader(make([]byte, 10))); st != 413 {
			t.Fatalf("overflow chunk: %d", st)
		}
		if st := c.JSON("POST", "/api/uploads/"+cr.ID+"/complete", nil, nil); st != 200 {
			t.Fatalf("complete: %d", st)
		}
		disk, _ := os.ReadFile(filepath.Join(e.Dir, "r.bin"))
		if !bytes.Equal(disk, data) {
			t.Fatal("resumed upload corrupted")
		}
		// Another guest cannot touch this guest's upload.
		other, _ := Login(t, e, e.Key, "")
		var cr2 struct {
			ID string `json:"id"`
		}
		c.JSON("POST", "/api/uploads", map[string]any{"dir": "", "name": "p.bin", "size": 10}, &cr2)
		if st := other.JSON("GET", "/api/uploads/"+cr2.ID, nil, nil); st != 404 {
			t.Fatalf("cross-guest upload access: %d", st)
		}
	})

	t.Run("ParallelUploadOutOfOrder", func(t *testing.T) {
		e, c := setup(t, nil)
		const chunk = 8 << 20
		data := randBytes(3*chunk + 12345) // 4 chunks, last one short
		var cr struct {
			ID        string `json:"id"`
			ChunkSize int    `json:"chunkSize"`
		}
		if st := c.JSON("POST", "/api/uploads", map[string]any{"dir": "", "name": "par.bin", "size": len(data), "parallel": true}, &cr); st != 201 || cr.ChunkSize != chunk {
			t.Fatalf("create: %d chunk=%d", st, cr.ChunkSize)
		}
		put := func(off int, body []byte, sum []byte) int {
			h := map[string]string{"X-Chunk-SHA256": hex.EncodeToString(sum)}
			res, err := c.do("PUT", fmt.Sprintf("/api/uploads/%s?offset=%d", cr.ID, off), bytes.NewReader(body), h)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			return res.StatusCode
		}
		piece := func(i int) ([]byte, []byte) {
			b := data[i*chunk : min((i+1)*chunk, len(data))]
			s := sha256.Sum256(b)
			return b, s[:]
		}
		// Chunks 3 and 1 arrive first, concurrently.
		var wg sync.WaitGroup
		for _, i := range []int{3, 1} {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				b, sum := piece(i)
				if st := put(i*chunk, b, sum); st != 200 {
					t.Errorf("chunk %d: %d", i, st)
				}
			}(i)
		}
		wg.Wait()
		// Not complete yet.
		if st := c.JSON("POST", "/api/uploads/"+cr.ID+"/complete", nil, nil); st != 409 {
			t.Fatalf("early complete: %d", st)
		}
		// A corrupted chunk is rejected; misaligned offsets are refused.
		b0, s0 := piece(0)
		bad := append([]byte(nil), b0...)
		bad[7] ^= 1
		if st := put(0, bad, s0); st != 422 {
			t.Fatalf("corrupt chunk: %d", st)
		}
		if st := put(100, b0[:100], s0); st != 400 {
			t.Fatalf("misaligned chunk: %d", st)
		}
		// Status reports which chunks the host has, for resuming.
		var stt struct {
			Offset int64   `json:"offset"`
			Done   []int64 `json:"done"`
		}
		c.JSON("GET", "/api/uploads/"+cr.ID, nil, &stt)
		if stt.Offset != 0 || len(stt.Done) != 2 || stt.Done[0] != chunk || stt.Done[1] != 3*chunk {
			t.Fatalf("status: %+v", stt)
		}
		for _, i := range []int{0, 2} {
			b, sum := piece(i)
			if st := put(i*chunk, b, sum); st != 200 {
				t.Fatalf("chunk %d: %d", i, st)
			}
		}
		// Re-sending a chunk is harmless.
		if b, sum := piece(2); put(2*chunk, b, sum) != 200 {
			t.Fatal("duplicate chunk refused")
		}
		var done struct {
			Path string `json:"path"`
		}
		if st := c.JSON("POST", "/api/uploads/"+cr.ID+"/complete", nil, &done); st != 200 {
			t.Fatalf("complete: %d", st)
		}
		disk, _ := os.ReadFile(filepath.Join(e.Dir, "par.bin"))
		if !bytes.Equal(disk, data) {
			t.Fatal("parallel upload corrupted")
		}
	})

	t.Run("Permissions", func(t *testing.T) {
		e, c := setup(t, nil)
		if st := c.JSON("POST", "/api/delete", map[string]string{"path": "hello.txt"}, nil); st != 403 {
			t.Fatalf("delete allowed by default: %d", st)
		}
		e.Server.Update(func(c *fileserver.Config) {
			c.Perms = fileserver.Permissions{}
		})
		if _, st, _ := c.Download("hello.txt", nil); st != 403 {
			t.Errorf("download denied: %d", st)
		}
		if _, st := c.Upload("", "x", []byte("x"), 1); st != 403 {
			t.Errorf("upload denied: %d", st)
		}
		if st := c.JSON("POST", "/api/mkdir", map[string]string{"path": "new"}, nil); st != 403 {
			t.Errorf("mkdir denied: %d", st)
		}
		if res, _ := c.do("GET", "/api/zip/x?path=", nil, nil); res.StatusCode != 403 {
			t.Errorf("zip denied: %d", res.StatusCode)
		}
		e.Server.Update(func(c *fileserver.Config) {
			c.Perms = fileserver.Permissions{Delete: true, Mkdir: true}
		})
		if st := c.JSON("POST", "/api/mkdir", map[string]string{"path": "new/"}, nil); st != 201 {
			t.Errorf("mkdir: %d", st)
		}
		if st := c.JSON("POST", "/api/delete", map[string]string{"path": "docs"}, nil); st != 200 {
			t.Errorf("delete: %d", st)
		}
		if _, err := os.Stat(filepath.Join(e.Dir, "docs")); err == nil {
			t.Error("folder not deleted")
		}
		if st := c.JSON("POST", "/api/delete", map[string]string{"path": ""}, nil); st == 200 {
			t.Error("deleted share root")
		}
	})

	t.Run("PasswordProtection", func(t *testing.T) {
		e, _ := setup(t, func(cfg *fileserver.Config) {
			h, _ := securityHash("correct horse")
			cfg.Password = h
		})
		if _, st := Login(t, e, e.Key, ""); st != 401 {
			t.Errorf("no password: %d", st)
		}
		if _, st := Login(t, e, e.Key, "nope"); st != 401 {
			t.Errorf("wrong password: %d", st)
		}
		if _, st := Login(t, e, e.Key, "correct horse"); st != 200 {
			t.Errorf("right password: %d", st)
		}
	})

	t.Run("RevocationAndExpiry", func(t *testing.T) {
		e, c := setup(t, nil)
		e.Server.RevokeAll()
		if st := c.JSON("GET", "/api/list?path=", nil, nil); st != 401 {
			t.Errorf("revoked token works: %d", st)
		}
		c2, _ := Login(t, e, e.Key, "")
		e.Server.Update(func(cfg *fileserver.Config) { cfg.AccessKey = "rotated-key-000000000000000000000" })
		if _, st := Login(t, e, e.Key, ""); st != 401 {
			t.Errorf("old link still logs in: %d", st)
		}
		e.Server.Update(func(cfg *fileserver.Config) { cfg.ExpiresAt = time.Now().Add(-time.Second) })
		if st := c2.JSON("GET", "/api/list?path=", nil, nil); st != 410 {
			t.Errorf("expired session: %d", st)
		}
	})

	t.Run("Zip", func(t *testing.T) {
		e, c := setup(t, nil)
		res, err := c.do("GET", "/api/zip/x?path=docs", nil, nil)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("zip: %v", err)
		}
		zb, _ := io.ReadAll(res.Body)
		res.Body.Close()
		zr, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
		if err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{}
		for _, f := range zr.File {
			if strings.HasSuffix(f.Name, "/") {
				continue
			}
			rc, _ := f.Open()
			files[f.Name], _ = io.ReadAll(rc)
			rc.Close()
		}
		want, _ := os.ReadFile(filepath.Join(e.Dir, "docs/deep/b.bin"))
		if !bytes.Equal(files["deep/b.bin"], want) || string(files["a.md"]) != "# A" || len(files) != 2 {
			t.Fatalf("zip contents: %v", len(files))
		}
	})

	t.Run("Search", func(t *testing.T) {
		_, c := setup(t, nil)
		var out struct {
			Results []struct{ Path string } `json:"results"`
		}
		c.JSON("GET", "/api/search?q=B.BIN", nil, &out)
		if len(out.Results) != 1 || out.Results[0].Path != "docs/deep/b.bin" {
			t.Fatalf("search: %+v", out.Results)
		}
	})

	t.Run("ConcurrentTransfers", func(t *testing.T) {
		e, c := setup(t, func(cfg *fileserver.Config) { cfg.Limits.MaxConcurrent = 32 })
		c, _ = Login(t, e, e.Key, "")
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := 0; i < 8; i++ {
			wg.Add(2)
			data := randBytes(400_000 + i*1000)
			go func(i int) {
				defer wg.Done()
				name := fmt.Sprintf("c%d.bin", i)
				if _, st := c.Upload("", name, data, 64<<10); st != 200 {
					errs <- fmt.Errorf("upload %d: %d", i, st)
					return
				}
				if disk, _ := os.ReadFile(filepath.Join(e.Dir, name)); !bytes.Equal(disk, data) {
					errs <- fmt.Errorf("upload %d corrupted", i)
				}
			}(i)
			go func() {
				defer wg.Done()
				if _, st, _ := c.Download("docs/deep/b.bin", nil); st != 200 {
					errs <- fmt.Errorf("download: %d", st)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})

	t.Run("DiskExhaustionGuard", func(t *testing.T) {
		_, c := setup(t, func(cfg *fileserver.Config) {
			cfg.Limits.MinFreeBytes = 1 << 62 // pretend the disk is always full
		})
		if st := c.JSON("POST", "/api/uploads", map[string]any{"dir": "", "name": "x", "size": 1}, nil); st != 507 {
			t.Fatalf("expected 507, got %d", st)
		}
	})

	t.Run("UploadSizeLimit", func(t *testing.T) {
		_, c := setup(t, func(cfg *fileserver.Config) { cfg.Limits.MaxUploadBytes = 1000 })
		if st := c.JSON("POST", "/api/uploads", map[string]any{"dir": "", "name": "x", "size": 1001}, nil); st != 413 {
			t.Fatalf("expected 413, got %d", st)
		}
	})
}

func securityHash(pw string) (*security.PasswordHash, error) { return security.HashPassword(pw) }
