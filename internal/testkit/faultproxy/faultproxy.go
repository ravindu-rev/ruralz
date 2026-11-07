// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package faultproxy is the protocol-agnostic TCP fault proxy of the
// integration harness (docs/engineering/03-testing-and-quality-strategy.md,
// "Process harness"): it sits between a client (a Node) and an upstream
// (a State Store) and injects faults on live and new connections:
//
//   - Pass forwards bytes unchanged.
//   - Blackhole keeps sockets open, reads and discards, never forwards;
//     connections accepted meanwhile are not dialed upstream until the
//     mode changes.
//   - Reset sends RST (SO_LINGER 0) on existing and new connections.
//
// SetUpstream switches new connections to another upstream (scripted State
// Store failover). A connection's hooks (Config.ConnTap) see every byte
// it forwards synchronously, as the State Store's RESP counting proxy
// does, and its Hold may hold the chunks it reads, as the counting proxy
// holds the replies to named commands: reading continues while chunks are
// held and every chunk is forwarded in the order read, so held chunks
// delay the chunks behind them, never each other's holds. Every goroutine
// belongs to the Proxy: one accept loop, and two per connection (one per
// direction), bounded by MaxConns; Close stops and waits for all of them.
package faultproxy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults.
const (
	DefaultMaxConns    = 16384
	DefaultDialTimeout = 5 * time.Second
	// DefaultMaxQueuedBytes bounds the bytes a direction holds queued
	// behind a held chunk; reading pauses while it is full.
	DefaultMaxQueuedBytes = 4 << 20
	// chunkSize is the read buffer per direction.
	chunkSize = 32 << 10
)

// ErrClosed is returned by operations on a closed Proxy.
var ErrClosed = errors.New("faultproxy: closed")

// Direction is the flow a chunk takes.
type Direction int

// Directions.
const (
	// Up flows from the client to the upstream.
	Up Direction = iota + 1
	// Down flows from the upstream to the client.
	Down
)

// String returns "up" or "down".
func (d Direction) String() string {
	switch d {
	case Up:
		return "up"
	case Down:
		return "down"
	}
	return fmt.Sprintf("Direction(%d)", int(d))
}

// Mode is a fault mode: Pass, Blackhole or Reset.
type Mode interface{ mode() }

// Pass forwards bytes unchanged.
type Pass struct{}

// Blackhole keeps sockets open, reads and discards, and never forwards.
type Blackhole struct{}

// Reset resets existing and new connections with RST (SO_LINGER 0).
type Reset struct{}

func (Pass) mode()      {}
func (Blackhole) mode() {}
func (Reset) mode()     {}

// String names the mode.
func (Pass) String() string { return "pass" }

// String names the mode.
func (Blackhole) String() string { return "blackhole" }

// String names the mode.
func (Reset) String() string { return "reset" }

func validMode(m Mode) error {
	switch m.(type) {
	case Pass, Blackhole, Reset:
		return nil
	case nil:
		return errors.New("faultproxy: nil mode")
	}
	return fmt.Errorf("faultproxy: unknown mode %T", m)
}

// TapFunc inspects bytes forwarded in one direction. It runs synchronously
// before the bytes are written, on the direction's goroutine; b must not
// be retained.
type TapFunc func(dir Direction, b []byte)

// HoldFunc decides, as a pump reads a chunk in direction dir, whether the
// chunk waits for its release before it is forwarded. True holds it until
// release is called; false forwards it once every chunk read before it has
// been, and then release must never be called. The pump goes on reading
// while chunks are held and forwards every chunk in the order it read
// them, so a held chunk delays the chunks behind it but not their reads,
// and the holds of pipelined chunks run side by side, never one after
// another. release may be called from any goroutine, also after the
// connection ended; calls after the first do nothing. A chunk the mode
// drops (Blackhole) or the connection's end discards is never forwarded,
// held or not. Hold runs synchronously on the direction's goroutine before
// the tap sees the chunk; b must not be retained.
type HoldFunc func(dir Direction, b []byte, release func()) bool

// ConnHooks are one connection's own hooks, which Config.ConnTap returns;
// the zero value has none.
type ConnHooks struct {
	// Tap sees the connection's forwarded chunks.
	Tap TapFunc
	// Hold, when set, may hold each chunk the connection reads.
	Hold HoldFunc
}

// ConnInfo describes a proxied connection.
type ConnInfo struct {
	// ID numbers connections from 1 in accept order.
	ID uint64
}

// Config configures a Proxy.
type Config struct {
	// Listen is the listen address; default "127.0.0.1:0".
	Listen string
	// Upstream is the address new connections dial.
	Upstream string
	// MaxConns bounds concurrent connections; beyond it new connections
	// are reset. Default DefaultMaxConns.
	MaxConns int
	// DialTimeout bounds each upstream dial; default DefaultDialTimeout. A
	// failed dial resets the client connection.
	DialTimeout time.Duration
	// MaxQueuedBytes bounds the bytes each direction holds queued behind a
	// held chunk; reading from that side pauses while it is full. Default
	// DefaultMaxQueuedBytes.
	MaxQueuedBytes int
	// Mode is the initial mode; default Pass.
	Mode Mode
	// ConnTap, when set, is called once per connection when it is dialed
	// and returns that connection's own hooks, so stateful parsers such as
	// a RESP command counter keep per-connection state.
	ConnTap func(ConnInfo) ConnHooks
}

// Stats counts connections and forwarded bytes.
type Stats struct {
	// Accepted counts accepted connections, including those reset at once.
	Accepted int64
	// Active counts open connections.
	Active int64
	// BytesUp and BytesDown count forwarded bytes. Each write is counted
	// after it completes, so a peer can read bytes before they appear here.
	BytesUp, BytesDown int64
}

// modeBox holds a Mode in an atomic.Pointer.
type modeBox struct{ m Mode }

// Proxy is a running fault proxy.
type Proxy struct {
	cfg  Config
	addr string

	closing   chan struct{} // closed by Close
	stopAfter func() bool

	mode     atomic.Pointer[modeBox]
	upstream atomic.Pointer[string]

	mu     sync.Mutex // guards ln, conns, nextID, closed and mode changes
	ln     net.Listener
	conns  map[uint64]*conn
	nextID uint64
	closed bool
	wg     sync.WaitGroup

	closeOnce sync.Once
	closeErr  error

	accepted, active, bytesUp, bytesDown atomic.Int64
	queuedUp                             atomic.Int64 // bytes the client-to-upstream pumps hold queued; tests wait on it
}

// Start listens and starts proxying. The proxy closes when ctx ends or
// Close is called.
func Start(ctx context.Context, c Config) (*Proxy, error) {
	if c.Upstream == "" {
		return nil, errors.New("faultproxy: Config.Upstream is empty")
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:0"
	}
	if c.MaxConns <= 0 {
		c.MaxConns = DefaultMaxConns
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.MaxQueuedBytes <= 0 {
		c.MaxQueuedBytes = DefaultMaxQueuedBytes
	}
	if c.Mode == nil {
		c.Mode = Pass{}
	}
	if err := validMode(c.Mode); err != nil {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.Listen)
	if err != nil {
		return nil, fmt.Errorf("faultproxy: listen %s: %w", c.Listen, err)
	}
	p := &Proxy{
		cfg:     c,
		addr:    ln.Addr().String(),
		ln:      ln,
		conns:   map[uint64]*conn{},
		closing: make(chan struct{}),
	}
	p.upstream.Store(&c.Upstream)
	p.mode.Store(&modeBox{c.Mode})
	p.wg.Add(1)
	// Connections outlive neither Close nor ctx (the AfterFunc below
	// closes the proxy), so they carry ctx's values, not its end.
	go p.acceptLoop(context.WithoutCancel(ctx), ln)
	// The callback may run at once (ctx already done); Close then waits
	// for p.mu, so stopAfter is always assigned before it is read.
	p.mu.Lock()
	p.stopAfter = context.AfterFunc(ctx, func() { _ = p.Close() })
	p.mu.Unlock()
	return p, nil
}

// Addr returns the listen address.
func (p *Proxy) Addr() string { return p.addr }

// Upstream returns the address new connections dial.
func (p *Proxy) Upstream() string { return *p.upstream.Load() }

// SetUpstream makes new connections dial addr; live connections keep
// their upstream (scripted failover).
func (p *Proxy) SetUpstream(addr string) { p.upstream.Store(&addr) }

// SetMode switches the mode for live and new connections; Reset resets
// every live connection.
func (p *Proxy) SetMode(m Mode) error {
	if err := validMode(m); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	p.mode.Store(&modeBox{m})
	_, reset := m.(Reset)
	for _, c := range p.conns {
		if reset {
			c.reset()
		} else {
			c.poke()
		}
	}
	return nil
}

// Conns lists the live connections by ID.
func (p *Proxy) Conns() []ConnInfo {
	p.mu.Lock()
	out := make([]ConnInfo, 0, len(p.conns))
	for _, c := range p.conns {
		out = append(out, ConnInfo{ID: c.id})
	}
	p.mu.Unlock()
	slices.SortFunc(out, func(a, b ConnInfo) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Stats returns the counters.
func (p *Proxy) Stats() Stats {
	return Stats{
		Accepted:  p.accepted.Load(),
		Active:    p.active.Load(),
		BytesUp:   p.bytesUp.Load(),
		BytesDown: p.bytesDown.Load(),
	}
}

// Close stops listening, closes every connection and waits for every
// goroutine of the proxy. It is idempotent.
func (p *Proxy) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.stopAfter()
		p.closed = true
		if p.ln != nil {
			p.closeErr = p.ln.Close()
			p.ln = nil
		}
		conns := make([]*conn, 0, len(p.conns))
		for _, c := range p.conns {
			conns = append(conns, c)
		}
		p.mu.Unlock()
		close(p.closing)
		for _, c := range conns {
			c.abort(false)
		}
		p.wg.Wait()
	})
	if p.closeErr != nil && !errors.Is(p.closeErr, net.ErrClosed) {
		return fmt.Errorf("faultproxy: close: %w", p.closeErr)
	}
	return nil
}

// acceptLoop accepts until ln closes (Close); ctx is the base of the
// connections' dials.
func (p *Proxy) acceptLoop(ctx context.Context, ln net.Listener) {
	defer p.wg.Done()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient (for example EMFILE): back off briefly.
			t := time.NewTimer(10 * time.Millisecond)
			select {
			case <-p.closing:
				t.Stop()
				return
			case <-t.C:
			}
			continue
		}
		p.accepted.Add(1)
		tc, ok := nc.(*net.TCPConn)
		if !ok {
			_ = nc.Close()
			continue
		}
		p.admit(ctx, tc)
	}
}

// admit registers a new connection or resets it.
func (p *Proxy) admit(ctx context.Context, tc *net.TCPConn) {
	p.mu.Lock()
	_, reset := p.mode.Load().m.(Reset)
	if reset || p.closed || len(p.conns) >= p.cfg.MaxConns {
		p.mu.Unlock()
		resetTCP(tc)
		return
	}
	p.nextID++
	c := newConn(p, p.nextID, tc)
	p.conns[c.id] = c
	p.active.Add(1)
	p.wg.Add(1)
	p.mu.Unlock()
	go c.run(ctx)
}

// resetTCP closes tc with RST.
func resetTCP(tc *net.TCPConn) {
	_ = tc.SetLinger(0)
	_ = tc.Close()
}

func (p *Proxy) remove(c *conn) {
	p.mu.Lock()
	delete(p.conns, c.id)
	p.mu.Unlock()
	p.active.Add(-1)
}

// conn is one proxied connection.
type conn struct {
	p      *Proxy
	id     uint64
	client *net.TCPConn
	done   chan struct{} // closed by abort

	mu         sync.Mutex // guards upstream, aborted, cancelDial, pumps
	upstream   *net.TCPConn
	aborted    bool
	cancelDial context.CancelFunc // ends an upstream dial in progress
	up, down   *pump

	// kept are the client bytes hold read once the mode had left
	// Blackhole, forwarded first after the dial; only run touches them.
	kept []byte
}

func newConn(p *Proxy, id uint64, client *net.TCPConn) *conn {
	c := &conn{p: p, id: id, client: client, done: make(chan struct{})}
	c.up = newPump(c, Up, client)
	return c
}

// mode is the proxy's mode.
func (c *conn) mode() Mode { return c.p.mode.Load().m }

// poke wakes both directions to re-read the mode.
func (c *conn) poke() {
	c.mu.Lock()
	pumps := []*pump{c.up, c.down}
	c.mu.Unlock()
	for _, pm := range pumps {
		if pm != nil {
			pm.poke()
		}
	}
}

// reset closes both sides with RST.
func (c *conn) reset() { c.abort(true) }

// abort closes both sides once; with rst the closes send RST.
func (c *conn) abort(rst bool) {
	c.mu.Lock()
	if c.aborted {
		c.mu.Unlock()
		return
	}
	c.aborted = true
	up, cancelDial := c.upstream, c.cancelDial
	c.mu.Unlock()
	if cancelDial != nil {
		cancelDial()
	}
	if rst {
		_ = c.client.SetLinger(0)
		if up != nil {
			_ = up.SetLinger(0)
		}
	}
	_ = c.client.Close()
	if up != nil {
		_ = up.Close()
	}
	close(c.done)
}

// abortFrom propagates a failure seen on one side: a reset from one peer
// becomes a reset of the other.
func (c *conn) abortFrom(err error) {
	c.abort(!errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF))
}

// run owns the connection: it holds it while black-holed, dials the
// upstream, forwards both directions and closes everything.
func (c *conn) run(ctx context.Context) {
	defer c.p.wg.Done()
	defer c.p.remove(c)
	defer c.abort(false)
	if !c.hold() {
		return
	}
	addr := c.p.Upstream()
	dctx, cancel := context.WithTimeout(ctx, c.p.cfg.DialTimeout)
	defer cancel()
	c.mu.Lock()
	if c.aborted {
		c.mu.Unlock()
		return
	}
	c.cancelDial = cancel
	c.mu.Unlock()
	nc, err := (&net.Dialer{}).DialContext(dctx, "tcp", addr)
	if err != nil {
		c.reset()
		return
	}
	up, ok := nc.(*net.TCPConn)
	if !ok {
		_ = nc.Close()
		c.reset()
		return
	}
	c.mu.Lock()
	if c.aborted {
		c.mu.Unlock()
		_ = up.Close()
		return
	}
	c.upstream = up
	c.up.dst = up
	c.down = newPump(c, Down, up)
	c.down.dst = c.client
	c.mu.Unlock()
	var hooks ConnHooks
	if c.p.cfg.ConnTap != nil {
		hooks = c.p.cfg.ConnTap(ConnInfo{ID: c.id})
	}
	c.up.tap, c.down.tap = hooks.Tap, hooks.Tap
	c.up.holdFn, c.down.holdFn = hooks.Hold, hooks.Hold
	// The connection may have been switched while dialing.
	c.poke()
	if len(c.kept) > 0 {
		if err := c.up.handle(c.kept); err != nil {
			c.abortFrom(err)
			return
		}
		c.kept = nil
	}
	downDone := make(chan struct{})
	go func() {
		defer close(downDone)
		c.down.run()
	}()
	c.up.run()
	<-downDone
}

// hold reads and discards client bytes while the mode is Blackhole and
// nothing was dialed; it reports whether to dial. Like the pumps, it
// treats bytes by the mode when it has read them: a poke's past deadline
// readies the blocked read, but bytes that arrive before that read runs
// are returned instead of the timeout, and once the mode has left
// Blackhole they are kept for the upstream.
func (c *conn) hold() bool {
	pm := c.up
	for {
		seen := pm.generation()
		select {
		case <-c.done:
			return false
		default:
		}
		switch c.mode().(type) {
		case Reset:
			c.reset()
			return false
		case Pass:
			return true
		}
		if !pm.armRead(seen) {
			continue
		}
		n, err := c.client.Read(pm.buf)
		if n > 0 {
			if _, pass := c.mode().(Pass); pass {
				c.kept = append(c.kept, pm.buf[:n]...)
			}
		}
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			return false
		}
	}
}

// chunk is bytes a direction keeps queued while the hold a HoldFunc took
// on it, or on a chunk ahead of it, waits for its release. The queue
// forwards its chunks in the order read.
type chunk struct {
	b    []byte
	hold *hold // nil unless the connection's Hold held the chunk
}

// waiting reports whether ch still waits for its release.
func (ch *chunk) waiting() bool { return ch.hold != nil && !ch.hold.released.Load() }

// hold is the release of one chunk a HoldFunc held.
type hold struct {
	pm       *pump
	released atomic.Bool
	// release is free, bound once, which the HoldFunc gets.
	release func()
}

// free releases the chunk and pokes its pump, which forwards it once the
// chunks ahead of it went; calls after the first do nothing.
func (h *hold) free() {
	if h.released.CompareAndSwap(false, true) {
		h.pm.poke()
	}
}

// pump forwards one direction.
type pump struct {
	c        *conn
	dir      Direction
	src, dst *net.TCPConn
	tap      TapFunc
	holdFn   HoldFunc
	buf      []byte
	wake     chan struct{} // capacity 1

	mu  sync.Mutex // guards gen and the read deadline of src
	gen uint64

	queue  []chunk
	queued int
	// spare is a hold the last HoldFunc call did not take, for the next.
	spare *hold
}

func newPump(c *conn, dir Direction, src *net.TCPConn) *pump {
	return &pump{c: c, dir: dir, src: src, buf: make([]byte, chunkSize), wake: make(chan struct{}, 1)}
}

// aLongTimeAgo is a read deadline in the past, which interrupts a read.
func aLongTimeAgo() time.Time { return time.Unix(1, 0) }

// poke interrupts a blocked read or wait so the pump re-reads the mode.
func (pm *pump) poke() {
	pm.mu.Lock()
	pm.gen++
	_ = pm.src.SetReadDeadline(aLongTimeAgo())
	pm.mu.Unlock()
	select {
	case pm.wake <- struct{}{}:
	default:
	}
}

func (pm *pump) generation() uint64 {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.gen
}

// armRead clears the read deadline unless a poke arrived since seen was
// read; it reports whether the caller may read.
func (pm *pump) armRead(seen uint64) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.gen != seen {
		return false
	}
	_ = pm.src.SetReadDeadline(time.Time{})
	return true
}

// wait blocks until a poke or the connection's end; it reports false at
// the connection's end.
func (pm *pump) wait() bool {
	select {
	case <-pm.wake:
	case <-pm.c.done:
		return false
	}
	return true
}

// setQueued records that the pump holds n bytes queued.
func (pm *pump) setQueued(n int) {
	if pm.dir == Up {
		pm.c.p.queuedUp.Add(int64(n - pm.queued))
	}
	pm.queued = n
}

// run forwards until the source ends or the connection is aborted.
func (pm *pump) run() {
	defer pm.setQueued(0) // an ended pump forwards nothing it still holds
	eof := false
	for {
		seen := pm.generation()
		select {
		case <-pm.c.done:
			return
		default:
		}
		m := pm.c.mode()
		switch m.(type) {
		case Reset:
			pm.c.reset()
			return
		case Blackhole:
			pm.queue = nil
			pm.setQueued(0)
		case Pass:
			if err := pm.flush(); err != nil {
				pm.c.abortFrom(err)
				return
			}
		}
		if eof {
			if _, bh := m.(Blackhole); !bh && len(pm.queue) == 0 {
				_ = pm.dst.CloseWrite()
				return
			}
			if !pm.wait() {
				return
			}
			continue
		}
		if pm.queued >= pm.c.p.cfg.MaxQueuedBytes {
			if !pm.wait() {
				return
			}
			continue
		}
		if !pm.armRead(seen) {
			continue
		}
		n, err := pm.src.Read(pm.buf)
		if n > 0 {
			if ferr := pm.handle(pm.buf[:n]); ferr != nil {
				pm.c.abortFrom(ferr)
				return
			}
		}
		switch {
		case err == nil, errors.Is(err, os.ErrDeadlineExceeded):
		case errors.Is(err, io.EOF):
			eof = true
		default:
			pm.c.abortFrom(err)
			return
		}
	}
}

// handle forwards or queues one chunk under Pass and drops it under
// Blackhole; the connection's Hold sees every chunk forwarded or queued,
// as it is read.
func (pm *pump) handle(b []byte) error {
	if _, pass := pm.c.mode().(Pass); !pass {
		// Blackhole drops; Reset is handled by the loop.
		return nil
	}
	h := pm.holdOf(b)
	if err := pm.flush(); err != nil {
		return err
	}
	if h == nil && len(pm.queue) == 0 {
		return pm.forward(b)
	}
	pm.enqueue(b, h)
	return nil
}

// holdOf asks the connection's Hold whether to hold b, just read, and
// returns the chunk's hold: nil when b goes as soon as the chunks ahead of
// it have.
func (pm *pump) holdOf(b []byte) *hold {
	if pm.holdFn == nil {
		return nil
	}
	h := pm.spare
	if h == nil {
		h = &hold{pm: pm}
		h.release = h.free
	}
	if !pm.holdFn(pm.dir, b, h.release) {
		pm.spare = h
		return nil
	}
	pm.spare = nil
	return h
}

// enqueue queues a copy of b, held by h (nil for no hold).
func (pm *pump) enqueue(b []byte, h *hold) {
	pm.queue = append(pm.queue, chunk{b: slices.Clone(b), hold: h})
	pm.setQueued(pm.queued + len(b))
}

// flush forwards the queued chunks in order while the first one waits for
// no release.
func (pm *pump) flush() error {
	for len(pm.queue) > 0 && !pm.queue[0].waiting() {
		ch := pm.queue[0]
		pm.queue[0] = chunk{}
		pm.queue = pm.queue[1:]
		pm.setQueued(pm.queued - len(ch.b))
		if err := pm.forward(ch.b); err != nil {
			return err
		}
	}
	if len(pm.queue) == 0 {
		pm.queue = nil
	}
	return nil
}

// forward taps and writes b to the destination.
func (pm *pump) forward(b []byte) error {
	if pm.tap != nil {
		pm.tap(pm.dir, b)
	}
	if _, err := pm.dst.Write(b); err != nil {
		return fmt.Errorf("faultproxy: write %s: %w", pm.dir, err)
	}
	if pm.dir == Up {
		pm.c.p.bytesUp.Add(int64(len(b)))
	} else {
		pm.c.p.bytesDown.Add(int64(len(b)))
	}
	return nil
}
