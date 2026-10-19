package fileserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"sharebox/internal/fsroot"
	"sharebox/internal/security"
	"sharebox/internal/transfers"
)

// Resumable upload protocol (identical over every transport):
//
//	POST   /api/uploads            {dir,name,size,modTime} -> {id, offset, chunkSize}
//	PUT    /api/uploads/{id}?offset=N   body = bytes [N, N+len)
//	                                    X-Chunk-SHA256: hex digest of the body (optional)
//	GET    /api/uploads/{id}            -> {offset, size}   (resume point)
//	POST   /api/uploads/{id}/complete   -> {path, name}     (atomic rename)
//	DELETE /api/uploads/{id}
//
// Data is written to a temp file inside the share's hidden internal folder,
// so the final rename is atomic on the same volume and partially uploaded
// files are never visible under their real name. When a chunk carries a
// digest it is committed all-or-nothing: on mismatch or a broken connection
// the temp file is truncated back to the previous offset, so a resumed upload
// can never contain corrupt or duplicated bytes.

const (
	chunkSize          = 8 << 20  // recommended chunk size for clients
	maxChunkBytes      = 64 << 20 // largest single PUT we accept
	uploadIdleTimeout  = 6 * time.Hour
	maxUploadsPerGuest = 64
	maxUploadsTotal    = 512
)

type upload struct {
	mu      sync.Mutex
	id      string
	guest   string
	dir     string // cleaned guest dir
	name    string // sanitised file name
	size    int64
	offset  int64
	modTime time.Time
	touched time.Time
	busy    bool
	// Parallel uploads accept chunk-aligned pieces in any order and from
	// several connections at once.
	parallel bool
	done     map[int64]int64 // received chunks: offset -> length
	inflight map[int64]bool  // chunks being written right now
	tr       *transfers.Handle
	ctx      context.Context
	cancel   context.CancelFunc
	finished bool
}

type uploadManager struct {
	s        *Server
	internal *os.Root

	mu       sync.Mutex
	uploads  map[string]*upload
	reserved int64 // bytes promised to in-progress uploads
	received int64 // total accepted upload bytes this session (quota)
	finalize sync.Mutex
	stop     chan struct{}
}

func newUploadManager(s *Server) (*uploadManager, error) {
	// Remove leftovers from a previous run (e.g. after a crash).
	_ = s.root.RemoveInternal()
	m := &uploadManager{s: s, uploads: map[string]*upload{}, stop: make(chan struct{})}
	go m.reaper()
	return m, nil
}

func (m *uploadManager) dir() (*os.Root, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.internal != nil {
		return m.internal, nil
	}
	ir, err := m.s.root.Internal()
	if err != nil {
		return nil, err
	}
	m.internal = ir
	return ir, nil
}

func (m *uploadManager) close() {
	select {
	case <-m.stop:
		return
	default:
		close(m.stop)
	}
	m.mu.Lock()
	for _, u := range m.uploads {
		if u.cancel != nil {
			u.cancel()
		}
	}
	m.uploads = map[string]*upload{}
	if m.internal != nil {
		m.internal.Close()
		m.internal = nil
	}
	m.mu.Unlock()
	_ = m.s.root.RemoveInternal()
}

func (m *uploadManager) reaper() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.mu.Lock()
			for id, u := range m.uploads {
				u.mu.Lock()
				idle := !u.busy && len(u.inflight) == 0 && time.Since(u.touched) > uploadIdleTimeout
				u.mu.Unlock()
				if idle {
					m.dropLocked(id, u, errors.New("upload abandoned"))
				}
			}
			m.mu.Unlock()
		}
	}
}

// dropLocked removes an upload; m.mu must be held.
func (m *uploadManager) dropLocked(id string, u *upload, err error) {
	delete(m.uploads, id)
	u.cancel()
	m.reserved -= u.size - u.offset
	if m.internal != nil {
		_ = m.internal.Remove(id + ".part")
	}
	if u.tr != nil {
		m.s.tracker.End(u.tr, err)
	}
}

type createReq struct {
	Dir     string `json:"dir"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"` // unix milliseconds, optional
	// Parallel opts into out-of-order chunk writes (see handleParallelChunk).
	Parallel bool `json:"parallel,omitempty"`
}

func (m *uploadManager) handleCreate(w http.ResponseWriter, r *http.Request) {
	s := m.s
	cfg := s.Config()
	if !cfg.Perms.Upload {
		forbid(w, "uploads")
		return
	}
	var req createReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Size < 0 {
		writeErr(w, http.StatusBadRequest, "invalid size")
		return
	}
	if cfg.Limits.MaxUploadBytes > 0 && req.Size > cfg.Limits.MaxUploadBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("files larger than %s are not accepted", humanBytes(cfg.Limits.MaxUploadBytes)))
		return
	}
	name, err := fsroot.SanitizeName(req.Name)
	if err != nil {
		s.fsError(w, err)
		return
	}
	dir, err := s.root.Stat(req.Dir)
	if err != nil {
		s.fsError(w, err)
		return
	}
	if !dir.IsDir {
		s.fsError(w, fsroot.ErrNotDir)
		return
	}
	ir, err := m.dir()
	if err != nil {
		s.fsError(w, err)
		return
	}
	free, err := s.root.FreeSpace()
	if err != nil {
		s.log.Warn("cannot determine free disk space", "err", err)
		free = 1 << 62
	}
	guest := guestOf(r)
	m.mu.Lock()
	perGuest := 0
	for _, u := range m.uploads {
		if u.guest == guest {
			perGuest++
		}
	}
	switch {
	case perGuest >= maxUploadsPerGuest || len(m.uploads) >= maxUploadsTotal:
		m.mu.Unlock()
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusTooManyRequests, "too many uploads in progress")
		return
	case int64(free)-m.reserved-req.Size < cfg.Limits.MinFreeBytes:
		m.mu.Unlock()
		writeErr(w, http.StatusInsufficientStorage, "the host does not have enough free disk space for this file")
		return
	case cfg.Limits.SessionQuota > 0 && m.received+m.reserved+req.Size > cfg.Limits.SessionQuota:
		m.mu.Unlock()
		writeErr(w, http.StatusInsufficientStorage, "the upload limit for this share has been reached")
		return
	}
	id := "u" + strings.ToLower(security.RandomToken(12))
	id = strings.NewReplacer("-", "x", "_", "y").Replace(id)
	f, err := ir.OpenFile(id+".part", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		m.mu.Unlock()
		s.fsError(w, err)
		return
	}
	f.Close()
	u := &upload{id: id, guest: guest, dir: dir.Path, name: name, size: req.Size, touched: time.Now(),
		parallel: req.Parallel, done: map[int64]int64{}, inflight: map[int64]bool{}}
	u.ctx, u.cancel = context.WithCancel(context.Background())
	if req.ModTime > 0 {
		u.modTime = time.UnixMilli(req.ModTime)
	}
	m.uploads[id] = u
	m.reserved += req.Size
	m.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "offset": 0, "chunkSize": chunkSize, "name": name})
}

func (m *uploadManager) get(w http.ResponseWriter, r *http.Request) *upload {
	m.mu.Lock()
	u := m.uploads[r.PathValue("id")]
	m.mu.Unlock()
	if u == nil || u.guest != guestOf(r) {
		writeErr(w, http.StatusNotFound, "upload not found; it may have expired")
		return nil
	}
	return u
}

func (m *uploadManager) handleStatus(w http.ResponseWriter, r *http.Request) {
	u := m.get(w, r)
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	res := map[string]any{"offset": u.offset, "size": u.size}
	if u.parallel {
		done := make([]int64, 0, len(u.done))
		for off := range u.done {
			done = append(done, off)
		}
		slices.Sort(done)
		res["done"] = done // chunk offsets already received
	}
	writeJSON(w, http.StatusOK, res)
}

func (m *uploadManager) handleChunk(w http.ResponseWriter, r *http.Request) {
	s := m.s
	if !s.Config().Perms.Upload {
		forbid(w, "uploads")
		return
	}
	u := m.get(w, r)
	if u == nil {
		return
	}
	off, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || off < 0 {
		writeErr(w, http.StatusBadRequest, "missing offset")
		return
	}
	var wantSum []byte
	if h := r.Header.Get("X-Chunk-SHA256"); h != "" {
		if wantSum, err = hex.DecodeString(h); err != nil || len(wantSum) != sha256.Size {
			writeErr(w, http.StatusBadRequest, "invalid chunk digest")
			return
		}
	}

	if u.parallel {
		m.handleParallelChunk(w, r, u, off, wantSum)
		return
	}

	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		writeErr(w, http.StatusConflict, "another request is writing this upload")
		return
	}
	if u.finished {
		u.mu.Unlock()
		writeErr(w, http.StatusConflict, "upload already completed")
		return
	}
	if off != u.offset {
		cur := u.offset
		u.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"error": "offset mismatch", "offset": cur})
		return
	}
	u.busy = true
	u.touched = time.Now()
	if u.tr == nil {
		u.tr = s.tracker.Begin(u.guest, transfers.Upload, u.name, u.size, transportOf(r), func() { m.abort(u) })
	}
	u.mu.Unlock()

	release, ok := s.acquire(w, r)
	if !ok {
		u.mu.Lock()
		u.busy = false
		u.mu.Unlock()
		return
	}
	defer release()

	n, werr := m.write(r, u, off, wantSum)

	u.mu.Lock()
	u.busy = false
	u.touched = time.Now()
	if werr == nil {
		u.offset = off + n
	}
	cur := u.offset
	u.mu.Unlock()

	if werr == nil && n > 0 {
		m.mu.Lock()
		m.reserved -= n
		m.received += n
		m.mu.Unlock()
	}
	switch {
	case werr == nil:
		writeJSON(w, http.StatusOK, map[string]any{"offset": cur})
	case errors.Is(werr, errDigest):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "chunk was corrupted in transit; retrying", "offset": cur})
	case errors.Is(werr, errTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "chunk exceeds declared file size", "offset": cur})
	default:
		if r.Context().Err() == nil {
			s.log.Warn("upload chunk failed", "err", werr)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "upload interrupted; it will resume", "offset": cur})
	}
}

var (
	errDigest   = errors.New("digest mismatch")
	errTooLarge = errors.New("too large")
)

// write appends the request body at off. It returns the number of bytes
// durably accepted (0 on error when a digest was supplied).
func (m *uploadManager) write(r *http.Request, u *upload, off int64, wantSum []byte) (int64, error) {
	ir, err := m.dir()
	if err != nil {
		return 0, err
	}
	f, err := ir.OpenFile(u.id+".part", os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	// Make sure stale bytes from an interrupted chunk are discarded.
	if err := f.Truncate(off); err != nil {
		return 0, err
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	remaining := u.size - off
	limit := remaining
	if limit > maxChunkBytes {
		limit = maxChunkBytes
	}
	h := sha256.New()
	body := io.LimitReader(ctxReader{u.ctx, r.Body}, limit+1)
	var dst io.Writer = f
	if wantSum != nil {
		dst = io.MultiWriter(f, h)
	}
	n, err := io.Copy(m.s.tracker.Writer(u.tr, dst), body)
	if err == nil && n > limit {
		err = errTooLarge
	}
	if err == nil && wantSum != nil && !equalBytes(h.Sum(nil), wantSum) {
		err = errDigest
	}
	if err != nil {
		if wantSum != nil || errors.Is(err, errTooLarge) {
			_ = f.Truncate(off)
			return 0, err
		}
		// Without a digest the bytes that did arrive are a correct prefix
		// (TLS/DTLS guarantee integrity); keep them so the upload resumes.
		return n, err
	}
	return n, nil
}

// handleParallelChunk writes one chunk of a parallel upload. Chunks must be
// aligned to chunkSize, carry a SHA-256 digest and may arrive in any order
// and concurrently; each is committed only if its digest matches. u.offset
// tracks the contiguous received prefix, so completion and resume work as
// for sequential uploads.
func (m *uploadManager) handleParallelChunk(w http.ResponseWriter, r *http.Request, u *upload, off int64, wantSum []byte) {
	s := m.s
	if wantSum == nil {
		writeErr(w, http.StatusBadRequest, "parallel uploads require X-Chunk-SHA256")
		return
	}
	if off%chunkSize != 0 || off >= u.size {
		writeErr(w, http.StatusBadRequest, "invalid chunk offset")
		return
	}
	want := min(int64(chunkSize), u.size-off)

	u.mu.Lock()
	switch {
	case u.finished:
		u.mu.Unlock()
		writeErr(w, http.StatusConflict, "upload already completed")
		return
	case u.done[off] > 0:
		cur := u.offset
		u.mu.Unlock()
		discard(r.Body)
		writeJSON(w, http.StatusOK, map[string]any{"offset": cur, "duplicate": true})
		return
	case u.inflight[off]:
		u.mu.Unlock()
		writeErr(w, http.StatusConflict, "this chunk is already being written")
		return
	}
	u.inflight[off] = true
	u.touched = time.Now()
	if u.tr == nil {
		u.tr = s.tracker.Begin(u.guest, transfers.Upload, u.name, u.size, transportOf(r), func() { m.abort(u) })
	}
	u.mu.Unlock()

	release, ok := s.acquire(w, r)
	if !ok {
		u.mu.Lock()
		delete(u.inflight, off)
		u.mu.Unlock()
		return
	}
	defer release()

	err := m.writeAt(r, u, off, want, wantSum)

	u.mu.Lock()
	delete(u.inflight, off)
	u.touched = time.Now()
	if err == nil {
		u.done[off] = want
		for u.done[u.offset] > 0 {
			u.offset += u.done[u.offset]
		}
	}
	cur := u.offset
	u.mu.Unlock()
	if err == nil {
		m.mu.Lock()
		m.reserved -= want
		m.received += want
		m.mu.Unlock()
	}
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"offset": cur})
	case errors.Is(err, errDigest):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "chunk was corrupted in transit; retrying", "offset": cur})
	case errors.Is(err, errTooLarge), errors.Is(err, errShort):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "chunk has the wrong length", "offset": cur})
	default:
		if r.Context().Err() == nil {
			s.log.Warn("upload chunk failed", "err", err)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "upload interrupted; it will resume", "offset": cur})
	}
}

var errShort = errors.New("short chunk")

// writeAt stores exactly want bytes at off. Nothing is marked as received
// unless the length and digest match, so a failed write is simply retried.
func (m *uploadManager) writeAt(r *http.Request, u *upload, off, want int64, wantSum []byte) error {
	ir, err := m.dir()
	if err != nil {
		return err
	}
	f, err := ir.OpenFile(u.id+".part", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	dst := io.MultiWriter(io.NewOffsetWriter(f, off), h)
	n, err := io.Copy(m.s.tracker.Writer(u.tr, dst), io.LimitReader(ctxReader{u.ctx, r.Body}, want+1))
	switch {
	case err != nil:
		return err
	case n > want:
		return errTooLarge
	case n < want:
		return errShort
	case !equalBytes(h.Sum(nil), wantSum):
		return errDigest
	}
	return nil
}

// abort is called when the host cancels an upload from its UI.
func (m *uploadManager) abort(u *upload) {
	u.cancel()
	m.mu.Lock()
	if m.uploads[u.id] == u {
		m.dropLocked(u.id, u, context.Canceled)
	}
	m.mu.Unlock()
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func equalBytes(a, b []byte) bool { return security.Equal(string(a), string(b)) }

func (m *uploadManager) handleComplete(w http.ResponseWriter, r *http.Request) {
	s := m.s
	u := m.get(w, r)
	if u == nil {
		return
	}
	u.mu.Lock()
	if u.busy || len(u.inflight) > 0 || u.finished {
		u.mu.Unlock()
		writeErr(w, http.StatusConflict, "upload is busy or already completed")
		return
	}
	if u.offset != u.size {
		off := u.offset
		u.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"error": "upload incomplete", "offset": off})
		return
	}
	u.busy = true
	u.mu.Unlock()

	final, err := m.finish(u)

	u.mu.Lock()
	u.busy = false
	if err == nil {
		u.finished = true
	}
	u.mu.Unlock()
	if err != nil {
		s.fsError(w, err)
		return
	}
	m.mu.Lock()
	delete(m.uploads, u.id)
	m.mu.Unlock()
	if u.tr == nil { // empty file: no chunk was ever sent
		u.tr = s.tracker.Begin(u.guest, transfers.Upload, u.name, 0, transportOf(r), nil)
	}
	s.tracker.End(u.tr, nil)
	_, name := fsroot.Split(final)
	writeJSON(w, http.StatusOK, map[string]any{"path": final, "name": name})
}

// finish syncs the temp file and renames it to a free name in the target
// folder. Existing files are never overwritten: "a.txt" becomes "a (2).txt".
func (m *uploadManager) finish(u *upload) (string, error) {
	ir, err := m.dir()
	if err != nil {
		return "", err
	}
	f, err := ir.OpenFile(u.id+".part", os.O_WRONLY, 0)
	if err != nil {
		return "", err
	}
	fi, err := f.Stat()
	if err == nil && fi.Size() != u.size {
		err = fmt.Errorf("size mismatch after upload")
	}
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		return "", err
	}
	// Temp files are private while incomplete; finished files get normal
	// permissions (further restricted by the process umask on Unix).
	_ = ir.Chmod(u.id+".part", 0o644)
	if !u.modTime.IsZero() {
		_ = ir.Chtimes(u.id+".part", time.Now(), u.modTime)
	}

	m.finalize.Lock()
	defer m.finalize.Unlock()
	// The destination folder may have been removed meanwhile.
	if e, err := m.s.root.Stat(u.dir); err != nil || !e.IsDir {
		return "", fsroot.ErrNotFound
	}
	base, ext := splitExt(u.name)
	for i := 1; i < 10000; i++ {
		name := u.name
		if i > 1 {
			name = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		dst := fsroot.Join(u.dir, name)
		err := m.s.root.RenameFromInternal(u.id+".part", dst)
		if errors.Is(err, fsroot.ErrExists) {
			continue
		}
		if err != nil {
			return "", err
		}
		return dst, nil
	}
	return "", fsroot.ErrExists
}

func splitExt(name string) (string, string) {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 {
		return name, ""
	}
	return name[:i], name[i:]
}

func (m *uploadManager) handleCancel(w http.ResponseWriter, r *http.Request) {
	u := m.get(w, r)
	if u == nil {
		return
	}
	m.mu.Lock()
	m.dropLocked(u.id, u, context.Canceled)
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
