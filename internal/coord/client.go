// Package coord is the host's connection to the coordination server. It
// keeps a WebSocket open with automatic reconnection (network changes,
// sleep/wake, server restarts) and resumes the same session so the share
// link stays valid.
package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"sharebox/internal/security"
	p "sharebox/shared/protocol"
)

// Handlers receive events. They are called from the read loop and must not
// block for long.
type Handlers struct {
	Registered  func(p.Envelope)
	GuestJoined func(peer string)
	GuestLeft   func(peer string)
	Signal      func(peer string, d *p.SignalPayload)
	Status      func(connected bool, err error)
}

// Registration describes the session to (re-)register.
type Registration struct {
	SignalHash string
	ExpiresAt  time.Time
	DeviceID   string
	DeviceKey  string
}

// Client is a reconnecting coordination client.
type Client struct {
	base string // https://server
	ws   string // wss://server/ws/host
	h    Handlers
	log  *slog.Logger

	mu        sync.Mutex
	conn      *websocket.Conn
	reg       func() Registration
	sessionID string
	hostToken string
	pending   map[string]chan p.Envelope
	seq       atomic.Uint64
	connected atomic.Bool
	kick      chan struct{}
}

// New creates a client for a server base URL (https://..., or http://localhost
// for development).
func New(base string, reg func() Registration, h Handlers, log *slog.Logger) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid server URL")
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		if !isLocal(u.Hostname()) {
			return nil, fmt.Errorf("the server URL must start with https://")
		}
		u.Scheme = "ws"
	default:
		return nil, fmt.Errorf("the server URL must start with https://")
	}
	if log == nil {
		log = slog.Default()
	}
	u.Path = "/ws/host"
	return &Client{base: base, ws: u.String(), h: h, log: log, reg: reg, pending: map[string]chan p.Envelope{}, kick: make(chan struct{}, 1)}, nil
}

func isLocal(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".localhost")
}

// Connected reports whether the WebSocket is up and registered.
func (c *Client) Connected() bool { return c.connected.Load() }

// Reconnect forces a fresh connection (e.g. after wake from sleep).
func (c *Client) Reconnect() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		conn.Close(websocket.StatusGoingAway, "reconnect")
	}
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// ForgetSession drops resume credentials so the next registration creates a
// brand-new session (new link).
func (c *Client) ForgetSession() {
	c.mu.Lock()
	c.sessionID, c.hostToken = "", ""
	c.mu.Unlock()
}

// Run connects and reconnects until ctx ends.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx)
		c.connected.Store(false)
		c.failPending()
		if ctx.Err() != nil {
			return
		}
		if c.h.Status != nil {
			c.h.Status(false, err)
		}
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		c.log.Info("coordination connection lost; retrying", "in", backoff, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		case <-c.kick:
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Client) failPending() {
	c.mu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

func (c *Client) session(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, _, err := websocket.Dial(dctx, c.ws, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("cannot reach the Sharebox server: %w", err)
	}
	conn.SetReadLimit(1 << 20)
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "")
	}()

	// Registration is sent before the read loop starts; its reply is
	// dispatched by the loop below.
	regID := c.nextID()
	if err := c.sendRegister(ctx, regID); err != nil {
		return err
	}

	// Application-level keepalive detects dead connections quickly after
	// sleep or a network switch.
	kctx, kcancel := context.WithCancel(ctx)
	defer kcancel()
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-kctx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(kctx, 10*time.Second)
				err := conn.Ping(pctx)
				cancel()
				if err != nil {
					conn.Close(websocket.StatusGoingAway, "keepalive failed")
					return
				}
			}
		}
	}()

	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var m p.Envelope
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		c.dispatch(m, regID)
	}
}

func (c *Client) sendRegister(ctx context.Context, id string) error {
	r := c.reg()
	c.mu.Lock()
	m := p.Envelope{Type: p.MsgRegister, ID: id, SignalHash: r.SignalHash, SessionID: c.sessionID,
		HostToken: c.hostToken, DeviceID: r.DeviceID, DeviceKey: r.DeviceKey}
	c.mu.Unlock()
	if !r.ExpiresAt.IsZero() {
		m.ExpiresAt = r.ExpiresAt.Unix()
	}
	return c.write(ctx, m)
}

// Reregister sends the registration again on the live connection (used
// after the link was regenerated). It returns the registration reply.
func (c *Client) Reregister(ctx context.Context) (p.Envelope, error) {
	r := c.reg()
	c.mu.Lock()
	m := p.Envelope{Type: p.MsgRegister, SignalHash: r.SignalHash, SessionID: c.sessionID, HostToken: c.hostToken,
		DeviceID: r.DeviceID, DeviceKey: r.DeviceKey}
	c.mu.Unlock()
	if !r.ExpiresAt.IsZero() {
		m.ExpiresAt = r.ExpiresAt.Unix()
	}
	res, err := c.Request(ctx, m)
	if err == nil && res.Type == p.MsgRegistered {
		c.onRegistered(res)
	}
	return res, err
}

func (c *Client) onRegistered(m p.Envelope) {
	c.mu.Lock()
	c.sessionID, c.hostToken = m.SessionID, m.HostToken
	c.mu.Unlock()
	c.connected.Store(true)
	if c.h.Registered != nil {
		c.h.Registered(m)
	}
	if c.h.Status != nil {
		c.h.Status(true, nil)
	}
}

func (c *Client) dispatch(m p.Envelope, regID string) {
	if m.ID != "" {
		if m.ID == regID {
			if m.Type == p.MsgRegistered {
				c.onRegistered(m)
			} else if c.h.Status != nil {
				c.h.Status(false, errors.New(m.Message))
			}
			return
		}
		c.mu.Lock()
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
			close(ch)
		}
		return
	}
	switch m.Type {
	case p.MsgGuestJoined:
		if c.h.GuestJoined != nil {
			c.h.GuestJoined(m.Peer)
		}
	case p.MsgGuestLeft:
		if c.h.GuestLeft != nil {
			c.h.GuestLeft(m.Peer)
		}
	case p.MsgSignal:
		if c.h.Signal != nil && m.Data != nil {
			c.h.Signal(m.Peer, m.Data)
		}
	}
}

func (c *Client) nextID() string { return fmt.Sprintf("r%d", c.seq.Add(1)) }

func (c *Client) write(ctx context.Context, m p.Envelope) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("not connected to the Sharebox server")
	}
	b, _ := json.Marshal(m)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

// Send transmits a message without waiting for a reply.
func (c *Client) Send(m p.Envelope) error { return c.write(context.Background(), m) }

// Request sends m and waits for the reply with the same id.
func (c *Client) Request(ctx context.Context, m p.Envelope) (p.Envelope, error) {
	m.ID = c.nextID()
	ch := make(chan p.Envelope, 1)
	c.mu.Lock()
	c.pending[m.ID] = ch
	c.mu.Unlock()
	if err := c.write(ctx, m); err != nil {
		c.mu.Lock()
		delete(c.pending, m.ID)
		c.mu.Unlock()
		return p.Envelope{}, err
	}
	select {
	case res, ok := <-ch:
		if !ok {
			return p.Envelope{}, errors.New("connection to the Sharebox server was lost")
		}
		if res.Type == p.MsgError {
			return res, errors.New(res.Message)
		}
		return res, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, m.ID)
		c.mu.Unlock()
		return p.Envelope{}, ctx.Err()
	}
}

// Probe asks the server to connect back to port on our public IP.
func (c *Client) Probe(ctx context.Context, port int, hostname string) (nonce string, res p.Envelope, err error) {
	nonce = security.RandomToken(24)
	res, err = c.Request(ctx, p.Envelope{Type: p.MsgProbe, Port: port, Nonce: nonce, Hostname: hostname})
	return nonce, res, err
}
