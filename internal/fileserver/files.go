package fileserver

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"sharebox/internal/fsroot"
	"sharebox/internal/transfers"
)

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	dir, err := s.root.Stat(p)
	if err != nil {
		s.fsError(w, err)
		return
	}
	if !dir.IsDir {
		s.fsError(w, fsroot.ErrNotDir)
		return
	}
	entries, err := s.root.List(p)
	truncated := false
	if errors.Is(err, fsroot.ErrTooManyHits) {
		truncated, err = true, nil
	}
	if err != nil {
		s.fsError(w, err)
		return
	}
	if entries == nil {
		entries = []fsroot.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": dir.Path, "entries": entries, "truncated": truncated})
}

const (
	maxSearchResults = 500
	maxSearchVisited = 200000
	searchTimeout    = 10 * time.Second
)

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" || len(q) > 200 {
		writeErr(w, http.StatusBadRequest, "enter something to search for")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	results := []fsroot.Entry{}
	visited := 0
	errStop := errors.New("stop")
	err := s.root.Walk(ctx, r.URL.Query().Get("path"), func(e fsroot.Entry) error {
		visited++
		if visited > maxSearchVisited {
			return errStop
		}
		if strings.Contains(strings.ToLower(e.Name), q) {
			results = append(results, e)
			if len(results) >= maxSearchResults {
				return errStop
			}
		}
		return nil
	})
	partial := err == errStop || errors.Is(err, context.DeadlineExceeded)
	if err != nil && !partial {
		s.fsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "partial": partial})
}

// contentDisposition builds an attachment header safe for any file name.
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == ';' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(name))
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !s.Config().Perms.Download {
		forbid(w, "downloads")
		return
	}
	f, fi, err := s.root.OpenFile(r.URL.Query().Get("path"))
	if err != nil {
		s.fsError(w, err)
		return
	}
	defer f.Close()
	release, ok := s.acquire(w, r)
	if !ok {
		return
	}
	defer release()

	name := fi.Name()
	h := w.Header()
	h.Set("Content-Disposition", contentDisposition(name))
	// Never let guest-uploaded content run as a page on the host's origin.
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" || strings.HasPrefix(ctype, "text/html") || strings.Contains(ctype, "javascript") || strings.Contains(ctype, "xml") {
		ctype = "application/octet-stream"
	}
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "private, no-store")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var tr *transfers.Handle
	if r.Method == http.MethodGet {
		total := fi.Size()
		if rg := r.Header.Get("Range"); rg != "" {
			total = -1 // ServeContent decides; progress shown as bytes only
			if start, end, ok := parseSingleRange(rg, fi.Size()); ok {
				total = end - start
			}
		}
		tr = s.tracker.Begin(guestOf(r), transfers.Download, name, total, transportOf(r), cancel)
	}
	cw := &countingWriter{ResponseWriter: w, s: s, tr: tr, ctx: ctx}
	http.ServeContent(cw, r.WithContext(ctx), name, fi.ModTime(), f)
	if tr != nil {
		var err error
		if ctx.Err() != nil {
			err = context.Canceled
		} else if cw.status >= 400 {
			err = fmt.Errorf("HTTP %d", cw.status)
		}
		s.tracker.End(tr, err)
	}
}

// parseSingleRange parses "bytes=a-b" for progress display only; the actual
// range handling is done by http.ServeContent.
func parseSingleRange(h string, size int64) (int64, int64, bool) {
	var a, b int64
	if n, _ := fmt.Sscanf(h, "bytes=%d-%d", &a, &b); n == 2 && a <= b && b < size {
		return a, b + 1, true
	}
	if n, _ := fmt.Sscanf(h, "bytes=%d-", &a); n == 1 && a < size {
		return a, size, true
	}
	return 0, 0, false
}

type countingWriter struct {
	http.ResponseWriter
	s      *Server
	tr     *transfers.Handle
	ctx    context.Context
	status int
}

func (c *countingWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err // host cancelled the transfer
	}
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(p)
	if c.tr != nil {
		c.s.tracker.Add(c.tr, int64(n))
	}
	return n, err
}

func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

const maxZipEntries = 1_000_000

// handleZip streams a folder as a ZIP archive. Entries are stored without
// compression: most large files (media, archives) are already compressed and
// storing keeps CPU and latency low. Go's writer emits ZIP64 records
// automatically, so archives and members larger than 4 GiB work.
func (s *Server) handleZip(w http.ResponseWriter, r *http.Request) {
	if !s.Config().Perms.Download {
		forbid(w, "downloads")
		return
	}
	base, err := s.root.Stat(r.URL.Query().Get("path"))
	if err != nil {
		s.fsError(w, err)
		return
	}
	if !base.IsDir {
		s.fsError(w, fsroot.ErrNotDir)
		return
	}
	release, ok := s.acquire(w, r)
	if !ok {
		return
	}
	defer release()

	name := base.Name
	if name == "" {
		name = s.root.Name()
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	tr := s.tracker.Begin(guestOf(r), transfers.Zip, name+".zip", -1, transportOf(r), cancel)

	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", contentDisposition(name+".zip"))
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.WriteHeader(http.StatusOK)

	zw := zip.NewWriter(s.tracker.Writer(tr, w))
	prefix := base.Path
	count := 0
	err = s.root.Walk(ctx, base.Path, func(e fsroot.Entry) error {
		count++
		if count > maxZipEntries {
			return errors.New("folder has too many items to zip")
		}
		rel := e.Path
		if prefix != "" {
			rel = strings.TrimPrefix(e.Path, prefix+"/")
		}
		if e.IsDir {
			_, err := zw.CreateHeader(&zip.FileHeader{Name: rel + "/", Method: zip.Store, Modified: e.ModTime})
			return err
		}
		f, fi, err := s.root.OpenFile(e.Path)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) || errors.Is(err, fsroot.ErrNotFound) {
				return nil // skip unreadable or vanished files
			}
			return err
		}
		defer f.Close()
		hw, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: zip.Store, Modified: fi.ModTime(),
			UncompressedSize64: uint64(fi.Size())})
		if err != nil {
			return err
		}
		n, err := io.Copy(hw, f)
		if err == nil && n != fi.Size() {
			// The file changed while zipping; fail rather than produce an
			// archive with silently truncated content.
			err = fmt.Errorf("file changed while zipping")
		}
		return err
	})
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		if ctx.Err() != nil {
			err = context.Canceled
		}
		s.tracker.End(tr, err)
		// Abort the connection so the client sees a failed download rather
		// than a corrupt archive that looks complete.
		panic(http.ErrAbortHandler)
	}
	s.tracker.End(tr, nil)
}

type pathReq struct {
	Path string `json:"path"`
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	if !s.Config().Perms.Mkdir {
		forbid(w, "creating folders")
		return
	}
	var req pathReq
	if !readJSON(w, r, &req) {
		return
	}
	e, err := s.root.Mkdir(req.Path)
	if err != nil {
		s.fsError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !s.Config().Perms.Delete {
		forbid(w, "deleting")
		return
	}
	var req pathReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.root.Remove(req.Path); err != nil {
		s.fsError(w, err)
		return
	}
	s.log.Info("guest deleted an item", "guest", guestOf(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
