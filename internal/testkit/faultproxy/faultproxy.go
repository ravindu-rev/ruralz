// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package faultproxy is the protocol-agnostic TCP fault proxy of the chaos
// and integration harness (OQ-scalability-and-distributed-state-10 (a);
// 11 section 3, H 49): it sits between a client (a Node) and an upstream
// (a State Store, an Upstream Endpoint, a Node behind the balancer) and
// injects faults on live and new connections:
//
//   - Pass forwards bytes unchanged.
//   - Delay holds every chunk, per direction, for a normally distributed
//     time (clamped at 0) while keeping byte order; reading continues, so
//     pipelined traffic is delayed, not serialized.
//   - Blackhole keeps sockets open, reads and discards, never forwards;
//     connections accepted meanwhile are not dialed upstream until the
//     mode changes.
//   - Reset sends RST (SO_LINGER 0) on existing and new connections.
//   - Refuse closes the listener (new connections get ECONNREFUSED) and
//     resets live ones; the next other mode reopens the same address.
//
// SetUpstream switches new connections to another upstream (scripted State
// Store failover, CE-5). A Tap sees every forwarded byte synchronously
// (A8's RESP counting proxy is a Tap). Every goroutine belongs to the
// Proxy: one accept loop, and two per connection (one per direction),
// bounded by MaxConns; Close stops and waits for all of them.
package faultproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
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
	// DefaultMaxQueuedBytes bounds the bytes a delayed direction holds;
	// reading pauses while it is full.
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

// Mode is a fault mode: Pass, Delay, Blackhole, Reset or Refuse.
type Mode interface{ mode() }

// Pass forwards bytes unchanged.
type Pass struct{}

// Delay holds each chunk, in each direction, for a duration drawn from a
// normal distribution with mean Mean and standard deviation StdDev,
// clamped at 0. Order is kept: a chunk is never released before the one
// read ahead of it.
type Delay struct{ Mean, StdDev time.Duration }

// Blackhole keeps sockets open, reads and discards, and never forwards.
type Blackhole struct{}

// Reset resets existing and new connections with RST (SO_LINGER 0).
type Reset struct{}

// Refuse closes the listener, so dials get ECONNREFUSED, and resets live
// connections; the next non-Refuse mode reopens the same address.
type Refuse struct{}

func (Pass) mode()      {}
func (Delay) mode()     {}
func (Blackhole) mode() {}
func (Reset) mode()     {}
func (Refuse) mode()    {}

// String names the mode.
func (Pass) String() string { return "pass" }

// String names the mode and its distribution.
func (d Delay) String() string { return fmt.Sprintf("delay(%v±%v)", d.Mean, d.StdDev) }

// String names the mode.
func (Blackhole) String() string { return "blackhole" }

// String names the mode.
func (Reset) String() string { return "reset" }

// String names the mode.
func (Refuse) String() string { return "refuse" }

func validMode(m Mode) error {
	switch m := m.(type) {
	case Pass, Blackhole, Reset, Refuse:
		return nil
	case Delay:
		if m.Mean < 0 || m.StdDev < 0 {
			return fmt.Errorf("faultproxy: negative delay %v", m)
		}
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

// ConnInfo describes a proxied connection.
type ConnInfo struct {
	// ID numbers connections from 1 in accept order.
	ID uint64
	// Client is the client's address as the proxy sees it.
	Client string
	// Upstream is the upstream address dialed ("" until dialed).
	Upstream string
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
	// MaxQueuedBytes bounds the bytes each delayed direction holds;
	// reading from that side pauses while it is full. Default
	// DefaultMaxQueuedBytes.
	MaxQueuedBytes int
	// Mode is the initial mode; default Pass.
	Mode Mode
	// Tap, when set, sees every forwarded chunk of every connection.
	Tap TapFunc
	// ConnTap, when set, is called once per connection when it is dialed
	// and returns that connection's own tap (nil for none), so stateful
	// parsers such as a RESP command counter keep per-connection state.
	ConnTap func(ConnInfo) TapFunc
	// Seed seeds the delay distribution; 0 picks a random seed.
	Seed uint64
}

// Stats counts connections and forwarded bytes.
type Stats struct {
	// Accepted counts accepted connections, including those reset at once.
	Accepted int64
	// Active counts open connections.
	Active int64
	// Reset counts connections the proxy reset (Reset and Refuse modes,
	// MaxConns overflow, failed upstream dials).
	Reset int64
	// BytesUp and BytesDown count forwarded bytes.
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

	rngMu sync.Mutex
	rng   *rand.Rand

	accepted, active, resets, bytesUp, bytesDown atomic.Int64
	queuedUp                                     atomic.Int64 // bytes the client-to-upstream pumps hold queued; tests wait on it
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
	seed := c.Seed
	if seed == 0 {
		seed = rand.Uint64() //nolint:gosec // G404: delay jitter, not security
	}
	ln, err := listen(ctx, c.Listen)
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		cfg:     c,
		addr:    ln.Addr().String(),
		conns:   map[uint64]*conn{},
		closing: make(chan struct{}),
		rng:     rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), //nolint:gosec // G404: delay jitter, not security
	}
	p.upstream.Store(&c.Upstream)
	p.mode.Store(&modeBox{c.Mode})
	if _, refuse := c.Mode.(Refuse); refuse {
		_ = ln.Close()
	} else {
		p.ln = ln
		p.wg.Add(1)
		// Connections outlive neither Close nor ctx (the AfterFunc below
		// closes the proxy), so they carry ctx's values, not its end.
		go p.acceptLoop(context.WithoutCancel(ctx), ln)
	}
	// The callback may run at once (ctx already done); Close then waits
	// for p.mu, so stopAfter is always assigned before it is read.
	p.mu.Lock()
	p.stopAfter = context.AfterFunc(ctx, func() { _ = p.Close() })
	p.mu.Unlock()
	return p, nil
}

func listen(ctx context.Context, addr string) (net.Listener, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("faultproxy: listen %s: %w", addr, err)
	}
	return ln, nil
}

// Addr returns the listen address; it stays the same across Refuse.
func (p *Proxy) Addr() string { return p.addr }

// Mode returns the current mode.
func (p *Proxy) Mode() Mode { return p.mode.Load().m }

// Upstream returns the address new connections dial.
func (p *Proxy) Upstream() string { return *p.upstream.Load() }

// SetUpstream makes new connections dial addr; live connections keep
// their upstream (scripted failover).
func (p *Proxy) SetUpstream(addr string) { p.upstream.Store(&addr) }

// SetMode switches the mode for live and new connections. Reset and Refuse
// reset every live connection (per-connection modes included); leaving
// Refuse reopens the listener on Addr, which fails when the port was
// taken meanwhile.
func (p *Proxy) SetMode(m Mode) error {
	if err := validMode(m); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	_, refuse := m.(Refuse)
	switch {
	case refuse && p.ln != nil:
		_ = p.ln.Close()
		p.ln = nil
	case !refuse && p.ln == nil:
		ln, err := listen(context.Background(), p.addr)
		if err != nil {
			return err
		}
		p.ln = ln
		p.wg.Add(1)
		go p.acceptLoop(context.Background(), ln)
	}
	p.mode.Store(&modeBox{m})
	for _, c := range p.conns {
		switch m.(type) {
		case Reset, Refuse:
			c.reset()
		default:
			c.poke()
		}
	}
	return nil
}

// SetConnMode overrides the mode of one live connection (nil clears the
// override). Refuse is not a connection mode.
func (p *Proxy) SetConnMode(id uint64, m Mode) error {
	if m != nil {
		if err := validMode(m); err != nil {
			return err
		}
		if _, ok := m.(Refuse); ok {
			return errors.New("faultproxy: Refuse applies to the listener, not a connection")
		}
	}
	p.mu.Lock()
	c, ok := p.conns[id]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("faultproxy: no connection %d", id)
	}
	if m == nil {
		c.override.Store(nil)
	} else {
		c.override.Store(&modeBox{m})
	}
	if _, ok := m.(Reset); ok {
		c.reset()
		return nil
	}
	c.poke()
	return nil
}

// Conns lists the live connections by ID.
func (p *Proxy) Conns() []ConnInfo {
	p.mu.Lock()
	out := make([]ConnInfo, 0, len(p.conns))
	for _, c := range p.conns {
		out = append(out, c.info())
	}
	p.mu.Unlock()
	slices.SortFunc(out, func(a, b ConnInfo) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out
}

// Stats returns the counters.
func (p *Proxy) Stats() Stats {
	return Stats{
		Accepted:  p.accepted.Load(),
		Active:    p.active.Load(),
		Reset:     p.resets.Load(),
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

// acceptLoop accepts until ln closes (Refuse or Close); ctx is the base of
// the connections' dials.
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
	switch p.mode.Load().m.(type) {
	case Reset, Refuse:
		p.mu.Unlock()
		p.resetTCP(tc)
		return
	}
	if p.closed || len(p.conns) >= p.cfg.MaxConns {
		p.mu.Unlock()
		p.resetTCP(tc)
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

// resetTCP closes tc with RST and counts it (before the peer can see it).
func (p *Proxy) resetTCP(tc *net.TCPConn) {
	p.resets.Add(1)
	_ = tc.SetLinger(0)
	_ = tc.Close()
}

func (p *Proxy) remove(c *conn) {
	p.mu.Lock()
	delete(p.conns, c.id)
	p.mu.Unlock()
	p.active.Add(-1)
}

// sampleDelay draws one delay of d.
func (p *Proxy) sampleDelay(d Delay) time.Duration {
	if d.StdDev == 0 {
		return d.Mean
	}
	p.rngMu.Lock()
	z := p.rng.NormFloat64()
	p.rngMu.Unlock()
	v := float64(d.Mean) + z*float64(d.StdDev)
	if v < 0 {
		return 0
	}
	return time.Duration(v)
}

// conn is one proxied connection.
type conn struct {
	p        *Proxy
	id       uint64
	client   *net.TCPConn
	override atomic.Pointer[modeBox]
	done     chan struct{} // closed by abort

	mu         sync.Mutex // guards upstream, upAddr, aborted, cancelDial, pumps
	upstream   *net.TCPConn
	upAddr     string
	aborted    bool
	cancelDial context.CancelFunc // ends an upstream dial in progress
	up, down   *pump
}

func newConn(p *Proxy, id uint64, client *net.TCPConn) *conn {
	c := &conn{p: p, id: id, client: client, done: make(chan struct{})}
	c.up = newPump(c, Up, client)
	return c
}

func (c *conn) info() ConnInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ConnInfo{ID: c.id, Client: c.client.RemoteAddr().String(), Upstream: c.upAddr}
}

// mode is the connection's override, else the proxy's mode.
func (c *conn) mode() Mode {
	if o := c.override.Load(); o != nil {
		return o.m
	}
	return c.p.mode.Load().m
}

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
		c.p.resets.Add(1)
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
	c.upstream, c.upAddr = up, addr
	c.up.dst = up
	c.down = newPump(c, Down, up)
	c.down.dst = c.client
	c.mu.Unlock()
	var tap TapFunc
	if c.p.cfg.ConnTap != nil {
		tap = c.p.cfg.ConnTap(c.info())
	}
	c.up.tap, c.down.tap = tap, tap
	// The connection may have been switched while dialing.
	c.poke()
	downDone := make(chan struct{})
	go func() {
		defer close(downDone)
		c.down.run()
	}()
	c.up.run()
	<-downDone
}

// hold reads and discards client bytes while the connection's mode is
// Blackhole and nothing was dialed; it reports whether to dial.
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
		case Reset, Refuse:
			c.reset()
			return false
		case Pass, Delay:
			return true
		}
		if !pm.armRead(seen, time.Time{}) {
			continue
		}
		_, err := c.client.Read(pm.buf)
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			return false
		}
	}
}

// chunk is bytes held by a delayed direction.
type chunk struct {
	b   []byte
	due time.Time
}

// pump forwards one direction.
type pump struct {
	c        *conn
	dir      Direction
	src, dst *net.TCPConn
	tap      TapFunc
	buf      []byte
	wake     chan struct{} // capacity 1

	mu  sync.Mutex // guards gen and the read deadline of src
	gen uint64

	queue   []chunk
	queued  int
	lastDue time.Time
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

// armRead sets the read deadline unless a poke arrived since seen was
// read; it reports whether the caller may read.
func (pm *pump) armRead(seen uint64, deadline time.Time) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.gen != seen {
		return false
	}
	_ = pm.src.SetReadDeadline(deadline)
	return true
}

// wait blocks until t, a poke, or the connection's end; it reports false
// at the connection's end.
func (pm *pump) wait(t time.Time) bool {
	var timer <-chan time.Time
	if !t.IsZero() {
		tm := time.NewTimer(time.Until(t))
		defer tm.Stop()
		timer = tm.C
	}
	select {
	case <-timer:
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
		case Reset, Refuse:
			pm.c.reset()
			return
		case Blackhole:
			pm.queue = nil
			pm.setQueued(0)
		case Pass:
			if err := pm.flush(time.Time{}); err != nil {
				pm.c.abortFrom(err)
				return
			}
		case Delay:
			if err := pm.flush(time.Now()); err != nil {
				pm.c.abortFrom(err)
				return
			}
		}
		if eof {
			if _, bh := m.(Blackhole); bh {
				if !pm.wait(time.Time{}) {
					return
				}
				continue
			}
			if len(pm.queue) == 0 {
				_ = pm.dst.CloseWrite()
				return
			}
			if !pm.wait(pm.queue[0].due) {
				return
			}
			continue
		}
		if pm.queued >= pm.c.p.cfg.MaxQueuedBytes {
			if !pm.wait(pm.queue[0].due) {
				return
			}
			continue
		}
		var deadline time.Time
		if len(pm.queue) > 0 {
			deadline = pm.queue[0].due
		}
		if !pm.armRead(seen, deadline) {
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

// handle forwards, queues or drops one chunk per the current mode.
func (pm *pump) handle(b []byte) error {
	switch m := pm.c.mode().(type) {
	case Pass:
		if err := pm.flush(time.Time{}); err != nil {
			return err
		}
		return pm.forward(b)
	case Delay:
		// Order only binds chunks still queued: once Pass flushed or
		// Blackhole dropped the queue, an earlier, longer delay must not
		// hold back the chunks of a later, shorter one.
		if len(pm.queue) == 0 {
			pm.lastDue = time.Time{}
		}
		due := time.Now().Add(pm.c.p.sampleDelay(m))
		if due.Before(pm.lastDue) {
			due = pm.lastDue
		}
		pm.lastDue = due
		pm.queue = append(pm.queue, chunk{b: slices.Clone(b), due: due})
		pm.setQueued(pm.queued + len(b))
	}
	// Blackhole drops; Reset and Refuse are handled by the loop.
	return nil
}

// flush forwards queued chunks due by now (all of them when now is zero).
func (pm *pump) flush(now time.Time) error {
	for len(pm.queue) > 0 && (now.IsZero() || !pm.queue[0].due.After(now)) {
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
	if t := pm.c.p.cfg.Tap; t != nil {
		t(pm.dir, b)
	}
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
