// Package tunnel carries HTTP requests over a message-oriented link (a
// WebRTC DataChannel) using the framing defined in shared/protocol. The host
// side serves an http.Handler; the client side implements
// http.RoundTripper. The package is transport-agnostic: anything that can
// send and receive ordered, reliable binary messages works.
package tunnel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	p "sharebox/shared/protocol"
)

// Link is the underlying message transport.
type Link interface {
	// Send transmits one message. The implementation must not retain b.
	Send(b []byte) error
	// BufferedAmount is the number of bytes queued but not yet sent.
	BufferedAmount() uint64
	Close() error
}

// ErrClosed is returned when the session ends.
var ErrClosed = errors.New("connection to the host was lost")

// Session is one end of a tunnel.
type Session struct {
	link   Link
	log    *slog.Logger
	server bool

	handler http.Handler
	baseCtx context.Context

	mu       sync.Mutex
	streams  map[uint32]*stream
	nextID   uint32
	lastPeer uint32
	closed   bool
	closeErr error
	done     chan struct{}

	lowMu   sync.Mutex
	lowCh   chan struct{} // closed when buffered amount drops
	lastRx  atomic.Int64
	sendMu  sync.Mutex
	openMu  sync.Mutex // keeps stream ids monotonic on the wire
	onClose func(error)
}

// Options configure a session.
type Options struct {
	Logger *slog.Logger
	// Handler, if set, makes this the serving (host) side.
	Handler http.Handler
	// BaseContext is the parent context for served requests.
	BaseContext context.Context
	// OnClose is called once when the session ends.
	OnClose func(error)
}

// New creates a session over link. The transport must call Deliver for each
// received message, BufferedLow when the send buffer drains below
// LowWaterMark, and Close when the link goes away.
func New(link Link, o Options) *Session {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.BaseContext == nil {
		o.BaseContext = context.Background()
	}
	s := &Session{
		link: link, log: o.Logger, handler: o.Handler, server: o.Handler != nil,
		baseCtx: o.BaseContext, streams: map[uint32]*stream{}, nextID: 1,
		done: make(chan struct{}), lowCh: make(chan struct{}), onClose: o.OnClose,
	}
	s.lastRx.Store(time.Now().UnixNano())
	go s.keepalive()
	return s
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns why the session ended.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}

// Close ends the session and fails all streams.
func (s *Session) Close(err error) {
	if err == nil {
		err = ErrClosed
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.closeErr = err
	streams := s.streams
	s.streams = map[uint32]*stream{}
	s.mu.Unlock()
	close(s.done)
	for _, st := range streams {
		st.fail(ErrClosed)
	}
	s.BufferedLow()
	_ = s.link.Close()
	if s.onClose != nil {
		s.onClose(err)
	}
}

// BufferedLow wakes senders waiting for the link buffer to drain.
func (s *Session) BufferedLow() {
	s.lowMu.Lock()
	close(s.lowCh)
	s.lowCh = make(chan struct{})
	s.lowMu.Unlock()
}

func (s *Session) keepalive() {
	t := time.NewTicker(p.PingIntervalSec * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if time.Since(time.Unix(0, s.lastRx.Load())) > p.DeadAfterSec*time.Second {
				s.Close(errors.New("peer stopped responding"))
				return
			}
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], uint64(time.Now().UnixNano()))
			_ = s.send(p.FramePing, 0, 0, b[:])
		}
	}
}

// send writes a frame, waiting while the link buffer is above the high
// water mark. This is the global backpressure; per-stream credit is
// handled by stream.write.
func (s *Session) send(typ byte, id, seq uint32, payload []byte) error {
	if len(payload) > p.MaxPayload {
		return fmt.Errorf("frame too large")
	}
	for s.link.BufferedAmount() > p.HighWaterMark {
		s.lowMu.Lock()
		ch := s.lowCh
		s.lowMu.Unlock()
		select {
		case <-ch:
		case <-s.done:
			return ErrClosed
		case <-time.After(50 * time.Millisecond):
			// Safety net in case a low-water event is missed.
		}
	}
	buf := make([]byte, p.HeaderSize+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], id)
	binary.BigEndian.PutUint32(buf[5:9], seq)
	copy(buf[9:], payload)
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	select {
	case <-s.done:
		return ErrClosed
	default:
	}
	if err := s.link.Send(buf); err != nil {
		go s.Close(err)
		return ErrClosed
	}
	return nil
}

func (s *Session) sendJSON(typ byte, id uint32, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > p.MaxPayload {
		return fmt.Errorf("headers too large")
	}
	return s.send(typ, id, 0, b)
}

func (s *Session) reset(id uint32, code, msg string) {
	_ = s.sendJSON(p.FrameReset, id, p.Reset{Code: code, Message: msg})
}

// Deliver processes one received message. It must be called sequentially in
// arrival order and never blocks on application reads.
func (s *Session) Deliver(msg []byte) {
	s.lastRx.Store(time.Now().UnixNano())
	if len(msg) < p.HeaderSize {
		s.Close(errors.New("protocol error: short frame"))
		return
	}
	typ := msg[0]
	id := binary.BigEndian.Uint32(msg[1:5])
	seq := binary.BigEndian.Uint32(msg[5:9])
	payload := msg[9:]

	if id == 0 {
		switch typ {
		case p.FramePing:
			go s.send(p.FramePong, 0, 0, append([]byte(nil), payload...))
		case p.FramePong:
		default:
			s.Close(errors.New("protocol error: bad control frame"))
		}
		return
	}

	s.mu.Lock()
	st := s.streams[id]
	if st == nil && typ == p.FrameHeaders && s.server {
		if id%2 == 0 || id <= s.lastPeer {
			s.mu.Unlock()
			s.Close(errors.New("protocol error: bad stream id"))
			return
		}
		s.lastPeer = id
		if len(s.streams) >= p.MaxStreams {
			s.mu.Unlock()
			go s.reset(id, p.ResetBusy, "too many concurrent requests")
			return
		}
		st = newStream(s, id)
		s.streams[id] = st
		s.mu.Unlock()
		var head p.RequestHead
		if err := json.Unmarshal(payload, &head); err != nil {
			s.removeStream(id)
			go s.reset(id, p.ResetProtocol, "bad headers")
			return
		}
		go s.serve(st, head)
		return
	}
	s.mu.Unlock()
	if st == nil {
		// Late frames for a finished stream are expected and ignored.
		return
	}

	switch typ {
	case p.FrameHeaders:
		var head p.ResponseHead
		if s.server || json.Unmarshal(payload, &head) != nil {
			st.fail(errors.New("protocol error"))
			return
		}
		st.gotResponse(head)
	case p.FrameData:
		st.recvData(seq, payload)
	case p.FrameEnd:
		if len(payload) != 8 {
			st.fail(errors.New("protocol error"))
			return
		}
		st.recvEnd(binary.BigEndian.Uint64(payload))
	case p.FrameWindow:
		if len(payload) != 4 {
			st.fail(errors.New("protocol error"))
			return
		}
		st.addCredit(int64(binary.BigEndian.Uint32(payload)))
	case p.FrameReset:
		var r p.Reset
		_ = json.Unmarshal(payload, &r)
		msg := r.Message
		if msg == "" {
			msg = r.Code
		}
		st.remoteReset(r.Code, msg)
	}
}

func (s *Session) removeStream(id uint32) {
	s.mu.Lock()
	delete(s.streams, id)
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// stream

type stream struct {
	s  *Session
	id uint32

	ctx    context.Context
	cancel context.CancelCauseFunc

	// inbound body
	inMu       sync.Mutex
	inQ        [][]byte
	inQBytes   int
	inSeq      uint32
	inTotal    uint64
	inEnded    bool
	inErr      error
	inSignal   chan struct{}
	unacked    int
	inCredit   int64 // bytes the peer may still send us
	inConsumed bool  // reader closed

	// outbound
	credit   atomic.Int64
	creditCh chan struct{}
	outSeq   uint32
	outTotal uint64

	// client side: response
	resOnce sync.Once
	resCh   chan p.ResponseHead
}

func newStream(s *Session, id uint32) *stream {
	ctx, cancel := context.WithCancelCause(s.baseCtx)
	st := &stream{s: s, id: id, ctx: ctx, cancel: cancel,
		inSignal: make(chan struct{}, 1), creditCh: make(chan struct{}, 1), resCh: make(chan p.ResponseHead, 1)}
	st.credit.Store(p.InitialWindow)
	st.inCredit = p.InitialWindow
	return st
}

func (st *stream) signalIn() {
	select {
	case st.inSignal <- struct{}{}:
	default:
	}
}

func (st *stream) recvData(seq uint32, b []byte) {
	st.inMu.Lock()
	if st.inEnded || st.inErr != nil {
		st.inMu.Unlock()
		return
	}
	if seq != st.inSeq {
		st.inMu.Unlock()
		st.fail(fmt.Errorf("data frame out of sequence (got %d, want %d)", seq, st.inSeq))
		go st.s.reset(st.id, p.ResetProtocol, "sequence error")
		return
	}
	st.inSeq++
	st.inCredit -= int64(len(b))
	if st.inCredit < 0 {
		st.inMu.Unlock()
		st.fail(errors.New("peer exceeded flow-control window"))
		go st.s.reset(st.id, p.ResetProtocol, "window exceeded")
		return
	}
	st.inTotal += uint64(len(b))
	if !st.inConsumed {
		st.inQ = append(st.inQ, append([]byte(nil), b...))
		st.inQBytes += len(b)
	} else {
		// Reader is gone; keep returning credit so the peer is not stuck.
		st.inCredit += int64(len(b))
		go st.sendWindow(len(b))
	}
	st.inMu.Unlock()
	st.signalIn()
}

func (st *stream) recvEnd(total uint64) {
	st.inMu.Lock()
	if total != st.inTotal {
		st.inErr = fmt.Errorf("transfer incomplete: received %d of %d bytes", st.inTotal, total)
	}
	st.inEnded = true
	st.inMu.Unlock()
	st.signalIn()
	st.maybeFinish()
}

// ack returns n bytes of credit to the peer.
func (st *stream) ack(n int) {
	st.inMu.Lock()
	st.inCredit += int64(n)
	st.inMu.Unlock()
	st.sendWindow(n)
}

func (st *stream) sendWindow(n int) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(n))
	_ = st.s.send(p.FrameWindow, st.id, 0, b[:])
}

// Read implements io.Reader for the inbound body.
func (st *stream) Read(buf []byte) (int, error) {
	for {
		st.inMu.Lock()
		if len(st.inQ) > 0 {
			n := copy(buf, st.inQ[0])
			if n == len(st.inQ[0]) {
				st.inQ[0] = nil
				st.inQ = st.inQ[1:]
			} else {
				st.inQ[0] = st.inQ[0][n:]
			}
			st.inQBytes -= n
			st.unacked += n
			var ackN int
			if st.unacked >= p.InitialWindow/4 {
				ackN, st.unacked = st.unacked, 0
			}
			st.inMu.Unlock()
			if ackN > 0 {
				st.ack(ackN)
			}
			return n, nil
		}
		if st.inErr != nil {
			err := st.inErr
			st.inMu.Unlock()
			return 0, err
		}
		if st.inEnded {
			st.inMu.Unlock()
			return 0, io.EOF
		}
		st.inMu.Unlock()
		select {
		case <-st.inSignal:
		case <-st.ctx.Done():
			return 0, context.Cause(st.ctx)
		}
	}
}

// closeRead discards further inbound data.
func (st *stream) closeRead() {
	st.inMu.Lock()
	st.inConsumed = true
	n := st.inQBytes + st.unacked
	st.inQ, st.inQBytes, st.unacked = nil, 0, 0
	ended := st.inEnded || st.inErr != nil
	st.inMu.Unlock()
	if n > 0 && !ended {
		st.ack(n)
	}
}

func (st *stream) addCredit(n int64) {
	st.credit.Add(n)
	select {
	case st.creditCh <- struct{}{}:
	default:
	}
}

// write sends b as DATA frames within the flow-control credit.
func (st *stream) write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		for st.credit.Load() <= 0 {
			select {
			case <-st.creditCh:
			case <-st.ctx.Done():
				return written, context.Cause(st.ctx)
			}
		}
		n := min(len(b), p.MaxPayload, int(st.credit.Load()))
		if err := st.ctx.Err(); err != nil {
			return written, context.Cause(st.ctx)
		}
		if err := st.s.send(p.FrameData, st.id, st.outSeq, b[:n]); err != nil {
			return written, err
		}
		st.outSeq++
		st.outTotal += uint64(n)
		st.credit.Add(-int64(n))
		written += n
		b = b[n:]
	}
	return written, nil
}

func (st *stream) end() error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], st.outTotal)
	return st.s.send(p.FrameEnd, st.id, 0, b[:])
}

func (st *stream) fail(err error) {
	st.inMu.Lock()
	if st.inErr == nil && !st.inEnded {
		st.inErr = err
	}
	st.inMu.Unlock()
	st.cancel(err)
	st.signalIn()
	st.s.removeStream(st.id)
}

func (st *stream) remoteReset(code, msg string) {
	err := fmt.Errorf("stream reset by peer: %s", msg)
	if code == p.ResetCancel {
		err = context.Canceled
	}
	if code == p.ResetBusy {
		err = ErrBusy
	}
	st.fail(err)
}

// ErrBusy means the host refused the request because of load.
var ErrBusy = errors.New("host is busy")

func (st *stream) gotResponse(h p.ResponseHead) {
	st.resOnce.Do(func() { st.resCh <- h })
}

// maybeFinish removes client streams once the response body has ended.
func (st *stream) maybeFinish() {
	if !st.s.server {
		st.s.removeStream(st.id)
	}
}

// ---------------------------------------------------------------------------
// server side

type rw struct {
	st          *stream
	header      http.Header
	wroteHeader bool
	err         error
}

func (w *rw) Header() http.Header { return w.header }

func (w *rw) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	if code >= 100 && code < 200 {
		return // informational responses are not forwarded
	}
	w.wroteHeader = true
	h := map[string][]string{}
	for k, v := range w.header {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "transfer-encoding", "upgrade", "set-cookie", "strict-transport-security":
			continue // meaningless or unwanted over the tunnel
		}
		h[k] = v
	}
	w.err = w.st.s.sendJSON(p.FrameHeaders, w.st.id, p.ResponseHead{Status: code, Header: h})
}

func (w *rw) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		if w.header.Get("Content-Type") == "" {
			w.header.Set("Content-Type", http.DetectContentType(b))
		}
		w.WriteHeader(http.StatusOK)
	}
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.st.write(b)
	if err != nil {
		w.err = err
	}
	return n, err
}

func (w *rw) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
}

type bodyReader struct{ st *stream }

func (b bodyReader) Read(p []byte) (int, error) { return b.st.Read(p) }
func (b bodyReader) Close() error               { b.st.closeRead(); return nil }

func (s *Session) serve(st *stream, head p.RequestHead) {
	defer s.removeStream(st.id)
	defer st.cancel(context.Canceled)
	u, err := url.ParseRequestURI(head.URI)
	if err != nil || !strings.HasPrefix(head.URI, "/") {
		s.reset(st.id, p.ResetProtocol, "bad uri")
		return
	}
	var body io.ReadCloser = http.NoBody
	if head.HasBody {
		body = bodyReader{st}
	} else {
		st.inMu.Lock()
		st.inEnded = true
		st.inMu.Unlock()
	}
	// Browsers send header names in any case; canonicalise them like
	// net/http does for real connections so Header.Get finds them.
	hdr := http.Header{}
	for k, v := range head.Header {
		for _, vv := range v {
			hdr.Add(k, vv)
		}
	}
	req := &http.Request{
		Method: head.Method, URL: u, RequestURI: head.URI, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: hdr, Body: body, Host: "sharebox.tunnel", RemoteAddr: "tunnel",
		ContentLength: -1,
	}
	if cl := req.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n >= 0 {
			req.ContentLength = n
		}
	}
	if !head.HasBody {
		req.ContentLength = 0
	}
	req = req.WithContext(st.ctx)
	w := &rw{st: st, header: http.Header{}}

	aborted := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				aborted = true
				if r != http.ErrAbortHandler {
					s.log.Error("tunnel handler panic", "panic", r)
				}
			}
		}()
		s.handler.ServeHTTP(w, req)
	}()
	body.Close()
	if aborted || w.err != nil {
		if st.ctx.Err() == nil {
			s.reset(st.id, p.ResetInternal, "transfer aborted by host")
		}
		return
	}
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.err == nil {
		_ = st.end()
	}
}

// ---------------------------------------------------------------------------
// client side

// RoundTrip implements http.RoundTripper over the tunnel.
func (s *Session) RoundTrip(req *http.Request) (*http.Response, error) {
	s.openMu.Lock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.openMu.Unlock()
		return nil, ErrClosed
	}
	if s.server {
		s.mu.Unlock()
		s.openMu.Unlock()
		return nil, errors.New("serving session cannot issue requests")
	}
	if len(s.streams) >= p.MaxStreams {
		s.mu.Unlock()
		s.openMu.Unlock()
		return nil, ErrBusy
	}
	id := s.nextID
	s.nextID += 2
	st := newStream(s, id)
	s.streams[id] = st
	s.mu.Unlock()

	hasBody := req.Body != nil && req.Body != http.NoBody
	hdr := map[string][]string{}
	for k, v := range req.Header {
		hdr[k] = v
	}
	if hasBody && req.ContentLength > 0 {
		hdr["Content-Length"] = []string{strconv.FormatInt(req.ContentLength, 10)}
	}
	err := s.sendJSON(p.FrameHeaders, id, p.RequestHead{Method: req.Method, URI: req.URL.RequestURI(), Header: hdr, HasBody: hasBody})
	s.openMu.Unlock()
	if err != nil {
		st.fail(err)
		return nil, err
	}

	// Cancel the stream when the caller's context ends.
	stop := context.AfterFunc(req.Context(), func() {
		st.fail(context.Canceled)
		s.reset(id, p.ResetCancel, "")
	})

	if hasBody {
		go func() {
			defer req.Body.Close()
			buf := make([]byte, 64<<10)
			for {
				n, err := req.Body.Read(buf)
				if n > 0 {
					if _, werr := st.write(buf[:n]); werr != nil {
						return
					}
				}
				if err == io.EOF {
					_ = st.end()
					return
				}
				if err != nil {
					st.fail(err)
					s.reset(id, p.ResetCancel, "request body failed")
					return
				}
			}
		}()
	}

	select {
	case head := <-st.resCh:
		res := &http.Response{
			Status: fmt.Sprintf("%d %s", head.Status, http.StatusText(head.Status)), StatusCode: head.Status,
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: canonical(head.Header),
			Body: &clientBody{st: st, stop: stop}, ContentLength: -1, Request: req,
		}
		if res.Header == nil {
			res.Header = http.Header{}
		}
		if cl, err := strconv.ParseInt(res.Header.Get("Content-Length"), 10, 64); err == nil {
			res.ContentLength = cl
		}
		if req.Method == http.MethodHead {
			res.ContentLength = -1
		}
		return res, nil
	case <-st.ctx.Done():
		stop()
		return nil, context.Cause(st.ctx)
	}
}

func canonical(h map[string][]string) http.Header {
	out := http.Header{}
	for k, v := range h {
		for _, vv := range v {
			out.Add(k, vv)
		}
	}
	return out
}

type clientBody struct {
	st   *stream
	stop func() bool
	once sync.Once
}

func (b *clientBody) Read(p []byte) (int, error) { return b.st.Read(p) }

func (b *clientBody) Close() error {
	b.once.Do(func() {
		b.stop()
		b.st.inMu.Lock()
		finished := b.st.inEnded || b.st.inErr != nil
		b.st.inMu.Unlock()
		if !finished {
			b.st.fail(context.Canceled)
			b.st.s.reset(b.st.id, p.ResetCancel, "")
		}
		// Stops a request-body writer that may still be waiting for credit.
		b.st.cancel(context.Canceled)
		b.st.s.removeStream(b.st.id)
	})
	return nil
}
