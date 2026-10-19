package tunnel

import (
	"errors"
	"sync"

	p "sharebox/shared/protocol"
)

// MemLink is an in-memory ordered message link used by tests and local
// simulations. It models a bounded send buffer like an RTCDataChannel and
// can inject failures.
type MemLink struct {
	mu       sync.Mutex
	cond     *sync.Cond
	queue    [][]byte
	buffered uint64
	closed   bool
	peer     *MemLink
	local    *Session // receiver of BufferedLow
	remote   *Session // receiver of messages
	// Mutate, if set, may alter or drop (return nil) outgoing messages.
	Mutate func([]byte) []byte
	// DropAfter closes the link after this many messages (0 = never).
	DropAfter int
	sent      int
}

// NewMemPair returns two connected links.
func NewMemPair() (*MemLink, *MemLink) {
	a, b := &MemLink{}, &MemLink{}
	a.cond, b.cond = sync.NewCond(&a.mu), sync.NewCond(&b.mu)
	a.peer, b.peer = b, a
	return a, b
}

// Bind attaches sessions and starts delivery: local owns this link, remote
// receives what is sent on it.
func (l *MemLink) Bind(local, remote *Session) {
	l.mu.Lock()
	l.local, l.remote = local, remote
	l.mu.Unlock()
	go l.pump()
}

func (l *MemLink) pump() {
	for {
		l.mu.Lock()
		for len(l.queue) == 0 && !l.closed {
			l.cond.Wait()
		}
		if l.closed {
			remote := l.remote
			l.mu.Unlock()
			if remote != nil {
				remote.Close(ErrClosed)
			}
			return
		}
		m := l.queue[0]
		l.queue = l.queue[1:]
		l.buffered -= uint64(len(m))
		low := l.buffered < p.LowWaterMark
		local, remote := l.local, l.remote
		l.mu.Unlock()
		if remote != nil {
			remote.Deliver(m)
		}
		if low && local != nil {
			local.BufferedLow()
		}
	}
}

// Send implements Link.
func (l *MemLink) Send(b []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("link closed")
	}
	l.sent++
	if l.DropAfter > 0 && l.sent > l.DropAfter {
		l.closed = true
		l.cond.Broadcast()
		return errors.New("link dropped")
	}
	m := append([]byte(nil), b...)
	if l.Mutate != nil {
		if m = l.Mutate(m); m == nil {
			return nil
		}
	}
	l.queue = append(l.queue, m)
	l.buffered += uint64(len(m))
	l.cond.Broadcast()
	return nil
}

// BufferedAmount implements Link.
func (l *MemLink) BufferedAmount() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffered
}

// Close implements Link; closing either end closes both.
func (l *MemLink) Close() error {
	for _, x := range []*MemLink{l, l.peer} {
		x.mu.Lock()
		x.closed = true
		x.cond.Broadcast()
		x.mu.Unlock()
	}
	return nil
}
