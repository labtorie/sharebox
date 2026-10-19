// Package transfers tracks active and completed transfers, guests and
// throughput for the host UI. It knows nothing about transports or files.
package transfers

import (
	"context"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Kind of transfer, from the guest's perspective.
type Kind string

const (
	Download Kind = "download" // host -> guest
	Upload   Kind = "upload"   // guest -> host
	Zip      Kind = "zip"      // folder download
)

// Status of a transfer.
type Status string

const (
	Active    Status = "active"
	Done      Status = "done"
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
)

// Transfer is one file movement.
type Transfer struct {
	ID        string    `json:"id"`
	Guest     string    `json:"guest"`
	Kind      Kind      `json:"kind"`
	Name      string    `json:"name"`
	Total     int64     `json:"total"` // -1 if unknown
	Bytes     int64     `json:"bytes"`
	Speed     float64   `json:"speed"` // bytes/s
	Status    Status    `json:"status"`
	Error     string    `json:"error,omitempty"`
	Transport string    `json:"transport"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitzero"`
}

// Handle is a live transfer owned by the tracker.
type Handle struct {
	info   Transfer
	bytes  atomic.Int64
	cancel context.CancelFunc
	lastB  int64
}

// Guest is a connected guest.
type Guest struct {
	ID        string    `json:"id"`
	Transport string    `json:"transport"` // direct | p2p | relay
	Route     string    `json:"route"`     // local network | internet | relay
	Since     time.Time `json:"since"`
	LastSeen  time.Time `json:"lastSeen"`
	Label     string    `json:"label"` // coarse client description (browser/OS)
}

// Snapshot is a consistent view for the UI.
type Snapshot struct {
	Active    []Transfer `json:"active"`
	Recent    []Transfer `json:"recent"`
	Guests    []Guest    `json:"guests"`
	UpSpeed   float64    `json:"upSpeed"`   // guest->host bytes/s
	DownSpeed float64    `json:"downSpeed"` // host->guest bytes/s
	TotalUp   int64      `json:"totalUp"`
	TotalDown int64      `json:"totalDown"`
}

// Tracker aggregates transfer state. Safe for concurrent use.
type Tracker struct {
	mu        sync.Mutex
	active    map[string]*Handle
	recent    []Transfer
	guests    map[string]*Guest
	totalUp   atomic.Int64
	totalDown atomic.Int64
	upSpeed   float64
	downSpeed float64
	lastUp    int64
	lastDown  int64
	lastTick  time.Time
	seq       atomic.Uint64
	onChange  func()
}

// New creates a tracker.
func New() *Tracker {
	return &Tracker{active: map[string]*Handle{}, guests: map[string]*Guest{}, lastTick: time.Now()}
}

// OnChange registers a callback invoked (without locks held) when a transfer
// starts or ends.
func (t *Tracker) OnChange(f func()) { t.mu.Lock(); t.onChange = f; t.mu.Unlock() }

func (t *Tracker) changed() {
	t.mu.Lock()
	f := t.onChange
	t.mu.Unlock()
	if f != nil {
		go f()
	}
}

// Begin registers a transfer. cancel, if non-nil, lets the host abort it.
func (t *Tracker) Begin(guest string, kind Kind, name string, total int64, transport string, cancel context.CancelFunc) *Handle {
	id := "t" + itoa(t.seq.Add(1))
	tr := &Handle{info: Transfer{ID: id, Guest: guest, Kind: kind, Name: name, Total: total, Status: Active,
		Transport: transport, Started: time.Now()}, cancel: cancel}
	t.mu.Lock()
	t.active[id] = tr
	t.mu.Unlock()
	t.changed()
	return tr
}

// Add records n transferred bytes.
func (t *Tracker) Add(tr *Handle, n int64) {
	if n <= 0 {
		return
	}
	tr.bytes.Add(n)
	if tr.info.Kind == Upload {
		t.totalUp.Add(n)
	} else {
		t.totalDown.Add(n)
	}
}

// End finishes a transfer.
func (t *Tracker) End(tr *Handle, err error) {
	if tr == nil {
		return
	}
	t.mu.Lock()
	if _, ok := t.active[tr.info.ID]; !ok {
		t.mu.Unlock()
		return
	}
	delete(t.active, tr.info.ID)
	tr.info.Ended = time.Now()
	switch {
	case err == nil:
		tr.info.Status = Done
	case err == context.Canceled:
		tr.info.Status = Cancelled
	default:
		tr.info.Status = Failed
		tr.info.Error = err.Error()
	}
	cp := tr.copy()
	t.recent = append([]Transfer{cp}, t.recent...)
	if len(t.recent) > 100 {
		t.recent = t.recent[:100]
	}
	t.mu.Unlock()
	t.changed()
}

// Cancel aborts an active transfer if it can be cancelled.
func (t *Tracker) Cancel(id string) bool {
	t.mu.Lock()
	tr := t.active[id]
	t.mu.Unlock()
	if tr == nil || tr.cancel == nil {
		return false
	}
	tr.cancel()
	return true
}

func (tr *Handle) copy() Transfer {
	c := tr.info
	c.Bytes = tr.bytes.Load()
	return c
}

// ID returns the transfer identifier.
func (tr *Handle) ID() string { return tr.info.ID }

// TouchGuest marks a guest as active.
func (t *Tracker) TouchGuest(id, transport, route, label string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	g := t.guests[id]
	if g == nil {
		g = &Guest{ID: id, Since: now, Label: label}
		t.guests[id] = g
		defer func() { go t.changed() }()
	}
	g.Transport = transport
	g.Route = route
	g.LastSeen = now
}

// RemoveGuest forgets a guest (disconnected or revoked).
func (t *Tracker) RemoveGuest(id string) {
	t.mu.Lock()
	delete(t.guests, id)
	t.mu.Unlock()
	t.changed()
}

// Reset clears everything (new session).
func (t *Tracker) Reset() {
	t.mu.Lock()
	for _, tr := range t.active {
		if tr.cancel != nil {
			tr.cancel()
		}
	}
	t.active = map[string]*Handle{}
	t.guests = map[string]*Guest{}
	t.recent = nil
	t.mu.Unlock()
	t.totalUp.Store(0)
	t.totalDown.Store(0)
	t.changed()
}

// Tick updates speed estimates; call about once per second.
func (t *Tracker) Tick() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	dt := now.Sub(t.lastTick).Seconds()
	if dt <= 0 {
		return
	}
	t.lastTick = now
	up, down := t.totalUp.Load(), t.totalDown.Load()
	t.upSpeed = ewma(t.upSpeed, float64(up-t.lastUp)/dt)
	t.downSpeed = ewma(t.downSpeed, float64(down-t.lastDown)/dt)
	t.lastUp, t.lastDown = up, down
	for _, tr := range t.active {
		b := tr.bytes.Load()
		tr.info.Speed = ewma(tr.info.Speed, float64(b-tr.lastB)/dt)
		tr.lastB = b
	}
	// Guests that have not been seen for a while are considered gone. RTC
	// guests send keepalives; direct guests poll while the page is open.
	for id, g := range t.guests {
		if now.Sub(g.LastSeen) > 90*time.Second {
			delete(t.guests, id)
		}
	}
}

func ewma(prev, cur float64) float64 {
	if prev == 0 {
		return cur
	}
	return prev*0.6 + cur*0.4
}

// Snapshot returns the current state.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := Snapshot{UpSpeed: t.upSpeed, DownSpeed: t.downSpeed, TotalUp: t.totalUp.Load(), TotalDown: t.totalDown.Load()}
	for _, tr := range t.active {
		s.Active = append(s.Active, tr.copy())
	}
	sort.Slice(s.Active, func(i, j int) bool { return s.Active[i].Started.Before(s.Active[j].Started) })
	s.Recent = append(s.Recent, t.recent...)
	for _, g := range t.guests {
		s.Guests = append(s.Guests, *g)
	}
	sort.Slice(s.Guests, func(i, j int) bool { return s.Guests[i].Since.Before(s.Guests[j].Since) })
	return s
}

// Reader wraps r and records bytes against tr.
func (t *Tracker) Reader(tr *Handle, r io.Reader) io.Reader { return &countR{t, tr, r} }

// Writer wraps w and records bytes against tr.
func (t *Tracker) Writer(tr *Handle, w io.Writer) io.Writer { return &countW{t, tr, w} }

type countR struct {
	t  *Tracker
	tr *Handle
	r  io.Reader
}

func (c *countR) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.t.Add(c.tr, int64(n))
	return n, err
}

type countW struct {
	t  *Tracker
	tr *Handle
	w  io.Writer
}

func (c *countW) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.t.Add(c.tr, int64(n))
	return n, err
}

func itoa(u uint64) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
		if u == 0 {
			break
		}
	}
	return string(b[i:])
}
