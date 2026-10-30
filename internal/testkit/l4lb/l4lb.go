// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package l4lb is the L4 balancer of the test kit's process harness
// (Testing "End-to-end tests"): Nodes run behind it as they run behind an
// operator's L4 load balancer (Scalability "L4 load balancing", Deployment
// topologies "Network and ports", Zero-downtime upgrades "Drain timeline
// defaults"):
//
//   - Each client connection is spliced to one backend Node's listener,
//     bytes unchanged in both directions, half-closes included, so TLS,
//     ALPN and client certificates pass through to the Node; a reset on
//     one side resets the other.
//   - With ProxyProtocol, each backend connection starts with a PROXY
//     protocol v2 header carrying the client's address and the address it
//     reached the balancer at, for balanced listeners with proxyProtocol:
//     true, so each request's source.ip is its client's (Security and
//     identity "Client address"); without it every request's source.ip is
//     the balancer's loopback address.
//   - Each backend's /readyz on its admin port is probed every
//     ProbeInterval, and only healthy backends take new connections.
//     UnhealthyThreshold failed probes in a row mark a backend unhealthy,
//     so a Drain's 503 is seen within ProbeInterval × UnhealthyThreshold,
//     inside the Drain's 5 s accept window (DetectionTime);
//     HealthyThreshold successes bring it back, and a new backend's first
//     success marks it healthy. Probes reuse one keep-alive connection per
//     backend, the keep-alive health checker a handover moves to the new
//     process, unless ProbeNewConnections opens one per probe.
//   - No stickiness: new connections go round robin over the healthy
//     backends, weighted only by slow start: a backend that returns from
//     unhealthy, or joins by AddBackend, while another serves ramps
//     linearly from MinSlowStartWeight to a full share over SlowStart.
//     Backends configured at Start join at full weight together.
//   - A connection idle for IdleTimeout, above the Node's 120 s, is
//     closed, so the Node closes idle connections first.
//   - MaxConnsPerBackend caps each Node's connections (Capacity planning's
//     L4 connection cap); MaxConns caps the balancer.
//   - Redispatch retries a failed backend dial on another healthy backend
//     instead of resetting the client.
//
// The balancer records which backend carried each connection, live and
// for the last MaxConns closed ones, so BackendOf attributes a request to
// a Node by the client address it used (CE-1: errors only on the killed
// Node's in-flight requests).
//
// Probe schedule and timeout, dial timeout, idle timeout and slow start
// run on an injected clock. Goroutines: one accept loop, one prober per
// backend and two per connection, bounded by MaxConns, all owned by the
// Balancer; Close stops and waits for every one.
package l4lb

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Defaults and the documented bounds they meet.
const (
	// DefaultProbeInterval is the time between two probes of a backend
	// (target).
	DefaultProbeInterval = time.Second
	// DefaultProbeTimeout bounds one probe (target); a probe timeout
	// never exceeds the interval, so detection stays within DetectionTime.
	DefaultProbeTimeout = time.Second
	// DefaultUnhealthyThreshold is the failed probes in a row that mark a
	// backend unhealthy, the configuration model's healthCheck.active
	// default.
	DefaultUnhealthyThreshold = 3
	// DefaultHealthyThreshold is the successful probes in a row that bring
	// an unhealthy backend back, the healthCheck.active default.
	DefaultHealthyThreshold = 2
	// AcceptWindow is the Drain's accept window: balancers MUST detect a
	// failed /readyz within it (target).
	AcceptWindow = 5 * time.Second
	// NodeIdleTimeout is the Node's idle connection timeout (target).
	NodeIdleTimeout = 120 * time.Second
	// DefaultIdleTimeout closes idle connections (target), above
	// NodeIdleTimeout as balancers must be.
	DefaultIdleTimeout = 150 * time.Second
	// DefaultSlowStart is the lower end of the 30 to 60 s slow start
	// (target) balancers use.
	DefaultSlowStart = 30 * time.Second
	// MinSlowStartWeight is a backend's share at the start of slow start,
	// as a fraction of a full share (target).
	MinSlowStartWeight = 0.1
	// DefaultMaxConns bounds concurrent client connections (target).
	DefaultMaxConns = 16384
	// DefaultDialTimeout bounds one backend dial (target).
	DefaultDialTimeout = 5 * time.Second
	// TransitionLogSize bounds the health transitions kept (target).
	TransitionLogSize = 1024

	maxProbeBody = 64 << 10 // the /readyz body read for its reason
	chunkSize    = 32 << 10 // the copy buffer per direction
	weightScale  = 1000     // slow-start weights as integers
)

// Errors.
var (
	// ErrClosed is returned by operations on a closed Balancer.
	ErrClosed = errors.New("l4lb: closed")
	// ErrProbeTimeout ends a probe that reached ProbeTimeout.
	ErrProbeTimeout = errors.New("l4lb: probe timed out")
	// ErrDialTimeout ends a backend dial that reached DialTimeout.
	ErrDialTimeout = errors.New("l4lb: backend dial timed out")
)

// State is a backend's health.
type State int

// States.
const (
	// Initial is a backend not yet probed successfully; it takes no
	// connections.
	Initial State = iota
	// Healthy takes new connections.
	Healthy
	// Unhealthy failed UnhealthyThreshold probes in a row; it takes no
	// new connections until HealthyThreshold probes succeed in a row.
	Unhealthy
)

// String names the state.
func (s State) String() string {
	switch s {
	case Initial:
		return "initial"
	case Healthy:
		return "healthy"
	case Unhealthy:
		return "unhealthy"
	}
	return fmt.Sprintf("State(%d)", int(s))
}

// Backend is one Node behind the balancer.
type Backend struct {
	// Name identifies the backend, such as "node-1".
	Name string
	// Addr is the listener new connections are spliced to (host:port).
	Addr string
	// ReadyURL is the readiness probe, such as
	// "http://127.0.0.1:9901/readyz"; 200 is healthy, anything else or no
	// answer is a failure.
	ReadyURL string
}

// Config configures a Balancer.
type Config struct {
	// Listen is the listen address; default "127.0.0.1:0".
	Listen string
	// Backends are the initial backends; AddBackend adds more.
	Backends []Backend
	// Clock schedules probes and runs the timeouts and slow start; nil
	// means clock.Real().
	Clock clock.Clock
	// ProbeInterval is the time between probes; default
	// DefaultProbeInterval.
	ProbeInterval time.Duration
	// ProbeTimeout bounds a probe, at most ProbeInterval; default the
	// smaller of DefaultProbeTimeout and ProbeInterval.
	ProbeTimeout time.Duration
	// UnhealthyThreshold and HealthyThreshold are the probe streaks that
	// change a backend's state; defaults DefaultUnhealthyThreshold and
	// DefaultHealthyThreshold.
	UnhealthyThreshold, HealthyThreshold int
	// ProbeTLS configures https ReadyURLs.
	ProbeTLS *tls.Config
	// ProbeNewConnections opens a connection per probe instead of reusing
	// one keep-alive connection per backend.
	ProbeNewConnections bool
	// IdleTimeout closes connections without traffic in either direction
	// for that long; 0 means DefaultIdleTimeout, negative never.
	IdleTimeout time.Duration
	// SlowStart is the ramp of a backend that returns from unhealthy, or
	// joins by AddBackend, while another serves; 0 means DefaultSlowStart,
	// negative none.
	SlowStart time.Duration
	// MaxConns bounds concurrent client connections; beyond it new ones
	// are reset. Default DefaultMaxConns.
	MaxConns int
	// MaxConnsPerBackend bounds each backend's connections; 0 means
	// unbounded.
	MaxConnsPerBackend int
	// Redispatch retries a failed backend dial on another healthy backend;
	// otherwise the client connection is reset.
	Redispatch bool
	// ProxyProtocol sends a PROXY protocol v2 header (command PROXY, TCP4
	// or TCP6) as the first bytes of each backend connection, with the
	// client's address as the source and the balancer address it
	// connected to as the destination; the balanced listeners must set
	// proxyProtocol: true. A header that cannot be written counts as a
	// failed dial.
	ProxyProtocol bool
	// DialTimeout bounds a backend dial; default DefaultDialTimeout.
	DialTimeout time.Duration
	// Dial, when set, dials backends instead of a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (c *Config) setDefaults() error {
	switch {
	case c.ProbeInterval < 0, c.ProbeTimeout < 0, c.DialTimeout < 0, c.MaxConns < 0, c.MaxConnsPerBackend < 0,
		c.UnhealthyThreshold < 0, c.HealthyThreshold < 0:
		return errors.New("l4lb: negative interval, timeout, threshold or bound")
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:0"
	}
	if c.Clock == nil {
		c.Clock = clock.Real()
	}
	if c.ProbeInterval == 0 {
		c.ProbeInterval = DefaultProbeInterval
	}
	if c.ProbeTimeout == 0 {
		c.ProbeTimeout = min(DefaultProbeTimeout, c.ProbeInterval)
	}
	if c.ProbeTimeout > c.ProbeInterval {
		return fmt.Errorf("l4lb: probe timeout %v above the probe interval %v", c.ProbeTimeout, c.ProbeInterval)
	}
	if c.UnhealthyThreshold == 0 {
		c.UnhealthyThreshold = DefaultUnhealthyThreshold
	}
	if c.HealthyThreshold == 0 {
		c.HealthyThreshold = DefaultHealthyThreshold
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.SlowStart == 0 {
		c.SlowStart = DefaultSlowStart
	}
	if c.MaxConns == 0 {
		c.MaxConns = DefaultMaxConns
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.Dial == nil {
		c.Dial = (&net.Dialer{}).DialContext
	}
	return nil
}

func (be Backend) validate() error {
	if be.Name == "" {
		return errors.New("l4lb: backend without a name")
	}
	if _, _, err := net.SplitHostPort(be.Addr); err != nil {
		return fmt.Errorf("l4lb: backend %s address: %w", be.Name, err)
	}
	u, err := url.Parse(be.ReadyURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("l4lb: backend %s ready URL %q is not an absolute http or https URL", be.Name, be.ReadyURL)
	}
	return nil
}

// ProbeResult is one readiness probe.
type ProbeResult struct {
	// At is when the probe started.
	At time.Time
	// OK reports a 200 answer.
	OK bool
	// Status is the answer's status; 0 when none came.
	Status int
	// Reason is the first reason of a not_ready /readyz body, such as
	// "draining".
	Reason string
	// Err is the failure when no answer came.
	Err error
}

// Transition is a backend's change of state.
type Transition struct {
	// Backend names the backend.
	Backend string
	// From and To are the states.
	From, To State
	// At is when the deciding probe started.
	At time.Time
	// Probe is the deciding probe.
	Probe ProbeResult
}

// BackendStatus describes one backend.
type BackendStatus struct {
	Backend
	// State is the backend's health.
	State State
	// Weight is the backend's share of new connections against a full
	// share: below 1 during slow start, 0 when not healthy.
	Weight float64
	// Active counts open connections, dials in progress included.
	Active int64
	// Routed counts connections spliced to the backend.
	Routed int64
	// DialFailures counts failed dials.
	DialFailures int64
	// BytesUp and BytesDown count bytes forwarded to and from it.
	BytesUp, BytesDown int64
	// Probes and ProbeFailures count readiness probes.
	Probes, ProbeFailures int64
	// LastProbe is the latest probe.
	LastProbe ProbeResult
	// Transitions counts state changes.
	Transitions int64
}

// Stats counts the balancer's client connections.
type Stats struct {
	// Accepted counts client connections accepted; Active those open.
	Accepted, Active int64
	// Routed counts connections spliced to a backend.
	Routed int64
	// NoBackend counts connections reset for lack of a healthy backend;
	// AtCapacity those reset because every healthy backend was at
	// MaxConnsPerBackend; Overflow those reset at MaxConns.
	NoBackend, AtCapacity, Overflow int64
	// DialFailed counts connections reset because no backend dial
	// succeeded; Redispatched counts dials moved to another backend after
	// a failure.
	DialFailed, Redispatched int64
	// IdleClosed counts connections closed by IdleTimeout.
	IdleClosed int64
}

// ConnInfo describes a client connection.
type ConnInfo struct {
	// ID numbers connections from 1 in accept order.
	ID uint64
	// Client is the client's address as the balancer saw it: the address
	// the client reports as its local address.
	Client string
	// Backend names the backend carrying the connection; "" when no dial
	// succeeded.
	Backend string
	// Opened is when the balancer accepted it; Closed when it ended (zero
	// while open).
	Opened, Closed time.Time
	// Redispatches counts failed dials before Backend.
	Redispatches int
	// BytesUp and BytesDown count bytes forwarded each way.
	BytesUp, BytesDown int64
	// IdleClosed reports a connection closed by IdleTimeout.
	IdleClosed bool
}

// backend is a Backend's state; fields without a comment are guarded by
// the Balancer's mu.
type backend struct {
	Backend
	client *http.Client
	tr     *http.Transport
	cancel context.CancelFunc // ends the backend's probes

	state          State
	okRun, failRun int
	slow           bool          // ramping up
	healthySince   time.Duration // offset from the balancer's base
	current        int64         // smooth weighted round robin state
	active         int64
	routed         int64
	dialFailures   int64
	probes         int64
	probeFailures  int64
	last           ProbeResult
	transitions    int64
	removed        bool
	joined         bool // added by AddBackend, not configured at Start

	bytesUp, bytesDown atomic.Int64
}

// Balancer is a running L4 balancer.
type Balancer struct {
	cfg  Config
	clk  clock.Clock
	base time.Time // the clock's reading at Start; schedule offsets count from it
	addr string
	ln   net.Listener

	cancel context.CancelFunc // ends the accept loop, dials and the configured backends' probes

	closeOnce sync.Once
	wg        sync.WaitGroup

	mu          sync.Mutex // guards everything below
	backends    []*backend // in configuration order
	byName      map[string]*backend
	conns       map[uint64]*conn
	byClient    map[string]uint64 // live connections by client address
	history     []ConnInfo        // ring of the last MaxConns closed connections
	histNext    int
	histClient  map[string]int // newest history entry by client address
	transitions []Transition   // ring of the last TransitionLogSize transitions
	transNext   int
	changed     chan struct{} // closed and replaced on every change
	nextID      uint64
	closed      bool
	stopAfter   func() bool
	stats       Stats
}

// Start listens and starts probing the backends. The balancer closes when
// ctx ends or Close is called.
func Start(ctx context.Context, c Config) (*Balancer, error) {
	if err := c.setDefaults(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, be := range c.Backends {
		if err := be.validate(); err != nil {
			return nil, err
		}
		if seen[be.Name] {
			return nil, fmt.Errorf("l4lb: duplicate backend %s", be.Name)
		}
		seen[be.Name] = true
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.Listen)
	if err != nil {
		return nil, fmt.Errorf("l4lb: listen %s: %w", c.Listen, err)
	}
	b := &Balancer{
		cfg:        c,
		clk:        c.Clock,
		addr:       ln.Addr().String(),
		ln:         ln,
		byName:     map[string]*backend{},
		conns:      map[uint64]*conn{},
		byClient:   map[string]uint64{},
		histClient: map[string]int{},
		changed:    make(chan struct{}),
	}
	b.base = b.clk.Now()
	// The accept loop, dials and probes outlive neither Close nor ctx (the
	// AfterFunc below closes the balancer), so they carry ctx's values,
	// not its end.
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	b.cancel = cancel
	b.mu.Lock()
	for _, be := range c.Backends {
		b.addLocked(lctx, be, false)
	}
	b.wg.Add(1)
	b.mu.Unlock()
	go b.acceptLoop(lctx)
	// The callback may run at once (ctx already done); Close then waits
	// for b.mu, so stopAfter is assigned before it is read.
	b.mu.Lock()
	b.stopAfter = context.AfterFunc(ctx, func() { _ = b.Close() })
	b.mu.Unlock()
	return b, nil
}

// Addr returns the listen address clients connect to.
func (b *Balancer) Addr() string { return b.addr }

// DetectionTime returns the longest time a backend that fails every probe
// keeps taking connections: ProbeInterval × UnhealthyThreshold, which
// MUST stay within AcceptWindow for a Drain to lose no new connection.
func (b *Balancer) DetectionTime() time.Duration {
	return b.cfg.ProbeInterval * time.Duration(b.cfg.UnhealthyThreshold)
}

// AddBackend adds a backend and starts probing it, as a Cluster scales
// out; it takes connections after its first successful probe, in slow
// start when another backend serves. Its probes carry ctx's values and
// end when the backend is removed or the balancer closes.
func (b *Balancer) AddBackend(ctx context.Context, be Backend) error {
	if err := be.validate(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	if _, dup := b.byName[be.Name]; dup {
		return fmt.Errorf("l4lb: duplicate backend %s", be.Name)
	}
	b.addLocked(context.WithoutCancel(ctx), be, true)
	b.notifyLocked()
	return nil
}

// addLocked registers be and starts its prober on a context derived from
// parent; joined marks a backend added after Start.
func (b *Balancer) addLocked(parent context.Context, be Backend, joined bool) {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	tr := &http.Transport{
		DialContext:         (&net.Dialer{}).DialContext,
		TLSClientConfig:     b.cfg.ProbeTLS.Clone(),
		DisableKeepAlives:   b.cfg.ProbeNewConnections,
		DisableCompression:  true,
		MaxIdleConnsPerHost: 1,
		Protocols:           &protocols,
	}
	s := &backend{
		Backend: be,
		tr:      tr,
		client: &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		joined: joined,
	}
	pctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	b.backends = append(b.backends, s)
	b.byName[be.Name] = s
	b.wg.Add(1)
	go b.probeLoop(pctx, s)
}

// RemoveBackend stops routing new connections to a backend and stops its
// probes; its open connections continue.
func (b *Balancer) RemoveBackend(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	be, ok := b.byName[name]
	if !ok {
		return fmt.Errorf("l4lb: no backend %s", name)
	}
	be.removed = true
	be.cancel()
	delete(b.byName, name)
	b.backends = slices.DeleteFunc(b.backends, func(x *backend) bool { return x == be })
	be.tr.CloseIdleConnections()
	b.notifyLocked()
	return nil
}

// Close stops listening, probing and dialing, closes every connection and
// waits for every goroutine of the balancer. It is idempotent.
func (b *Balancer) Close() error {
	var err error
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		if b.stopAfter != nil {
			b.stopAfter()
		}
		err = b.ln.Close()
		conns := make([]*conn, 0, len(b.conns))
		for _, c := range b.conns {
			conns = append(conns, c)
		}
		for _, be := range b.backends {
			be.cancel()
		}
		backends := slices.Clone(b.backends)
		b.notifyLocked()
		b.mu.Unlock()
		b.cancel()
		for _, c := range conns {
			c.abort(false)
		}
		b.wg.Wait()
		for _, be := range backends {
			be.tr.CloseIdleConnections()
		}
	})
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("l4lb: close: %w", err)
	}
	return nil
}

// notifyLocked wakes every waiter.
func (b *Balancer) notifyLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// Stats returns the connection counters.
func (b *Balancer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.stats
	s.Active = int64(len(b.conns))
	return s
}

// Backend returns one backend's status.
func (b *Balancer) Backend(name string) (BackendStatus, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	be, ok := b.byName[name]
	if !ok {
		return BackendStatus{}, false
	}
	return b.statusLocked(be, b.clk.Since(b.base)), true
}

// Backends returns every backend's status in configuration order.
func (b *Balancer) Backends() []BackendStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clk.Since(b.base)
	out := make([]BackendStatus, 0, len(b.backends))
	for _, be := range b.backends {
		out = append(out, b.statusLocked(be, now))
	}
	return out
}

func (b *Balancer) statusLocked(be *backend, now time.Duration) BackendStatus {
	s := BackendStatus{
		Backend:       be.Backend,
		State:         be.state,
		Active:        be.active,
		Routed:        be.routed,
		DialFailures:  be.dialFailures,
		BytesUp:       be.bytesUp.Load(),
		BytesDown:     be.bytesDown.Load(),
		Probes:        be.probes,
		ProbeFailures: be.probeFailures,
		LastProbe:     be.last,
		Transitions:   be.transitions,
	}
	if be.state == Healthy {
		s.Weight = float64(b.weightLocked(be, now)) / weightScale
	}
	return s
}

// Transitions returns the kept health transitions, oldest first.
func (b *Balancer) Transitions() []Transition {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.transitions) < TransitionLogSize {
		return slices.Clone(b.transitions)
	}
	return append(slices.Clone(b.transitions[b.transNext:]), b.transitions[:b.transNext]...)
}

// Conns returns the open client connections by ID.
func (b *Balancer) Conns() []ConnInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ConnInfo, 0, len(b.conns))
	for _, c := range b.conns {
		out = append(out, c.infoLocked())
	}
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

// ConnOf returns the newest connection from client, an address such as a
// load generator's ConnInfo.Local: an open one, or one of the last
// MaxConns closed ones.
func (b *Balancer) ConnOf(client string) (ConnInfo, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id, ok := b.byClient[client]; ok {
		return b.conns[id].infoLocked(), true
	}
	if i, ok := b.histClient[client]; ok {
		return b.history[i], true
	}
	return ConnInfo{}, false
}

// BackendOf returns the backend that carried the newest connection from
// client; ok is false for an unknown client, and the name is "" when no
// dial succeeded.
func (b *Balancer) BackendOf(client string) (name string, ok bool) {
	ci, ok := b.ConnOf(client)
	return ci.Backend, ok
}

// WaitState waits until the named backend is in state s.
func (b *Balancer) WaitState(ctx context.Context, name string, s State) error {
	return b.wait(ctx, func() (bool, error) {
		be, ok := b.byName[name]
		if !ok {
			return false, fmt.Errorf("l4lb: no backend %s", name)
		}
		return be.state == s, nil
	})
}

// WaitHealthy waits until every named backend, or every backend when none
// is named, is healthy.
func (b *Balancer) WaitHealthy(ctx context.Context, names ...string) error {
	return b.wait(ctx, func() (bool, error) {
		if len(names) == 0 {
			for _, be := range b.backends {
				if be.state != Healthy {
					return false, nil
				}
			}
			return true, nil
		}
		for _, n := range names {
			be, ok := b.byName[n]
			if !ok {
				return false, fmt.Errorf("l4lb: no backend %s", n)
			}
			if be.state != Healthy {
				return false, nil
			}
		}
		return true, nil
	})
}

// WaitProbes waits until the named backend has completed at least n
// probes; a test on a fake clock waits for one before advancing the clock
// to the next.
func (b *Balancer) WaitProbes(ctx context.Context, name string, n int64) error {
	return b.wait(ctx, func() (bool, error) {
		be, ok := b.byName[name]
		if !ok {
			return false, fmt.Errorf("l4lb: no backend %s", name)
		}
		return be.probes >= n, nil
	})
}

// wait re-evaluates cond, under b.mu, at every change until it holds,
// fails, the balancer closes or ctx ends.
func (b *Balancer) wait(ctx context.Context, cond func() (bool, error)) error {
	for {
		b.mu.Lock()
		ok, err := cond()
		ch, closed := b.changed, b.closed
		b.mu.Unlock()
		switch {
		case err != nil:
			return err
		case ok:
			return nil
		case closed:
			return ErrClosed
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// acceptLoop accepts until the listener closes; ctx ends at Close.
func (b *Balancer) acceptLoop(ctx context.Context) {
	defer b.wg.Done()
	for {
		nc, err := b.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient (for example EMFILE): back off briefly.
			if b.clk.Sleep(ctx, 10*time.Millisecond) != nil {
				return
			}
			continue
		}
		tc, ok := nc.(*net.TCPConn)
		if !ok {
			_ = nc.Close()
			continue
		}
		b.admit(ctx, tc)
	}
}

// admit picks a backend for a new client connection and starts it, or
// resets the connection.
func (b *Balancer) admit(ctx context.Context, tc *net.TCPConn) {
	b.mu.Lock()
	b.stats.Accepted++
	var be *backend
	switch {
	case b.closed:
	case len(b.conns) >= b.cfg.MaxConns:
		b.stats.Overflow++
	default:
		var capped bool
		if be, capped = b.pickLocked(nil); be == nil {
			if capped {
				b.stats.AtCapacity++
			} else {
				b.stats.NoBackend++
			}
		}
	}
	if be == nil {
		b.mu.Unlock()
		reset(tc)
		return
	}
	be.active++
	b.nextID++
	c := &conn{b: b, id: b.nextID, client: tc, clientAddr: tc.RemoteAddr().String(), be: be, opened: b.clk.Now()}
	b.conns[c.id] = c
	b.byClient[c.clientAddr] = c.id
	b.wg.Add(1)
	b.notifyLocked()
	b.mu.Unlock()
	go c.run(ctx)
}

// pickLocked chooses a healthy backend by smooth weighted round robin,
// skipping exclude and backends at MaxConnsPerBackend; capped reports
// that a healthy backend was skipped for its cap.
func (b *Balancer) pickLocked(exclude map[*backend]bool) (best *backend, capped bool) {
	now := b.clk.Since(b.base)
	var total int64
	for _, be := range b.backends {
		if be.state != Healthy || exclude[be] {
			continue
		}
		if b.cfg.MaxConnsPerBackend > 0 && be.active >= int64(b.cfg.MaxConnsPerBackend) {
			capped = true
			continue
		}
		w := b.weightLocked(be, now)
		be.current += w
		total += w
		if best == nil || be.current > best.current {
			best = be
		}
	}
	if best != nil {
		best.current -= total
	}
	return best, capped
}

// weightLocked returns a healthy backend's weight, ramping linearly from
// MinSlowStartWeight to full over SlowStart.
func (b *Balancer) weightLocked(be *backend, now time.Duration) int64 {
	if !be.slow {
		return weightScale
	}
	elapsed := now - be.healthySince
	if elapsed >= b.cfg.SlowStart {
		be.slow = false
		return weightScale
	}
	f := max(MinSlowStartWeight, float64(elapsed)/float64(b.cfg.SlowStart))
	return max(1, int64(f*weightScale))
}

// reset closes c with a TCP reset.
func reset(c net.Conn) {
	if l, ok := c.(interface{ SetLinger(int) error }); ok {
		_ = l.SetLinger(0)
	}
	_ = c.Close()
}

// conn is one client connection.
type conn struct {
	b          *Balancer
	id         uint64
	client     *net.TCPConn
	clientAddr string
	opened     time.Time
	last       atomic.Int64 // offset from the balancer's base of the last byte forwarded
	up, down   atomic.Int64 // bytes forwarded each way

	// Guarded by the Balancer's mu.
	be           *backend // nil once every dial failed
	redispatches int

	mu         sync.Mutex // guards upstream, aborted, idle and idleClosed
	upstream   net.Conn
	aborted    bool
	idle       clock.Timer
	idleClosed bool
}

func (c *conn) infoLocked() ConnInfo {
	ci := ConnInfo{
		ID:           c.id,
		Client:       c.clientAddr,
		Opened:       c.opened,
		Redispatches: c.redispatches,
		BytesUp:      c.up.Load(),
		BytesDown:    c.down.Load(),
	}
	if c.be != nil {
		ci.Backend = c.be.Name
	}
	c.mu.Lock()
	ci.IdleClosed = c.idleClosed
	c.mu.Unlock()
	return ci
}

// run dials a backend, splices the connection, and records its end; ctx
// ends dials at Close.
func (c *conn) run(ctx context.Context) {
	defer c.b.wg.Done()
	if up := c.connect(ctx); up != nil {
		c.splice(up)
	}
	c.b.finish(c)
}

// connect dials the chosen backend, redispatching failed dials when
// configured; it returns nil after resetting the client.
func (c *conn) connect(ctx context.Context) net.Conn {
	b := c.b
	tried := map[*backend]bool{}
	for {
		b.mu.Lock()
		be := c.be
		b.mu.Unlock()
		up, err := b.dialBackend(ctx, be.Addr)
		if err == nil && b.cfg.ProxyProtocol {
			err = sendProxyHeader(up, c.client)
		}
		b.mu.Lock()
		if err == nil {
			be.routed++
			b.stats.Routed++
			b.notifyLocked()
			b.mu.Unlock()
			return up
		}
		be.dialFailures++
		be.active--
		tried[be] = true
		var next *backend
		if b.cfg.Redispatch && !b.closed {
			next, _ = b.pickLocked(tried)
		}
		if next == nil {
			c.be = nil
			if !b.closed {
				b.stats.DialFailed++
			}
			b.notifyLocked()
			b.mu.Unlock()
			reset(c.client)
			return nil
		}
		next.active++
		c.be = next
		c.redispatches++
		b.stats.Redispatched++
		b.mu.Unlock()
	}
}

// dialBackend dials addr within DialTimeout on the balancer's clock.
func (b *Balancer) dialBackend(ctx context.Context, addr string) (net.Conn, error) {
	dctx, cancel := context.WithCancelCause(ctx)
	t := b.clk.AfterFunc(b.cfg.DialTimeout, func() { cancel(ErrDialTimeout) })
	defer cancel(nil)
	defer t.Stop()
	nc, err := b.cfg.Dial(dctx, "tcp", addr)
	if err != nil {
		if cause := context.Cause(dctx); errors.Is(cause, ErrDialTimeout) {
			return nil, fmt.Errorf("%w: %w", ErrDialTimeout, err)
		}
		return nil, err
	}
	return nc, nil
}

// splice forwards both directions until both end, closing everything.
func (c *conn) splice(up net.Conn) {
	c.mu.Lock()
	if c.aborted {
		c.mu.Unlock()
		_ = up.Close()
		return
	}
	c.upstream = up
	c.mu.Unlock()
	c.touch()
	c.armIdle(c.b.cfg.IdleTimeout)
	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		c.pump(c.client, up, &c.up, &c.backend().bytesUp)
	}()
	c.pump(up, c.client, &c.down, &c.backend().bytesDown)
	<-upDone
	c.abort(false)
}

func (c *conn) backend() *backend {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	return c.be
}

// pump forwards src to dst. EOF half-closes dst; a reset or other failure
// resets both sides, as a balancer passes a reset on.
func (c *conn) pump(src, dst net.Conn, n, total *atomic.Int64) {
	buf := make([]byte, chunkSize)
	for {
		k, err := src.Read(buf)
		if k > 0 {
			c.touch()
			if _, werr := dst.Write(buf[:k]); werr != nil {
				c.abort(!errors.Is(werr, net.ErrClosed))
				return
			}
			n.Add(int64(k))
			total.Add(int64(k))
		}
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				if cw, ok := dst.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
			case errors.Is(err, net.ErrClosed):
			default:
				c.abort(true)
			}
			return
		}
	}
}

// touch records traffic now.
func (c *conn) touch() { c.last.Store(int64(c.b.clk.Since(c.b.base))) }

// armIdle closes the connection once it has been idle for d.
func (c *conn) armIdle(d time.Duration) {
	if d <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.aborted {
		c.idle = c.b.clk.AfterFunc(d, c.checkIdle)
	}
}

// checkIdle closes an idle connection, or re-arms for the rest of the
// timeout after recent traffic.
func (c *conn) checkIdle() {
	b := c.b
	idle := b.clk.Since(b.base) - time.Duration(c.last.Load())
	if rest := b.cfg.IdleTimeout - idle; rest > 0 {
		c.armIdle(rest)
		return
	}
	c.mu.Lock()
	if c.aborted {
		c.mu.Unlock()
		return
	}
	c.idleClosed = true
	c.mu.Unlock()
	b.mu.Lock()
	b.stats.IdleClosed++
	b.mu.Unlock()
	c.abort(false)
}

// abort closes both sides once; with rst the closes send a reset.
func (c *conn) abort(rst bool) {
	c.mu.Lock()
	if c.aborted {
		c.mu.Unlock()
		return
	}
	c.aborted = true
	up, idle := c.upstream, c.idle
	c.idle = nil
	c.mu.Unlock()
	if idle != nil {
		idle.Stop()
	}
	if rst {
		reset(c.client)
		if up != nil {
			reset(up)
		}
		return
	}
	_ = c.client.Close()
	if up != nil {
		_ = up.Close()
	}
}

// finish records a connection's end.
func (b *Balancer) finish(c *conn) {
	c.abort(false)
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.be != nil {
		c.be.active--
	}
	delete(b.conns, c.id)
	if b.byClient[c.clientAddr] == c.id {
		delete(b.byClient, c.clientAddr)
	}
	ci := c.infoLocked()
	ci.Closed = b.clk.Now()
	if b.cfg.MaxConns > 0 {
		if len(b.history) < b.cfg.MaxConns {
			b.history = append(b.history, ci)
			b.histClient[ci.Client] = len(b.history) - 1
		} else {
			old := b.history[b.histNext]
			if b.histClient[old.Client] == b.histNext {
				delete(b.histClient, old.Client)
			}
			b.history[b.histNext] = ci
			b.histClient[ci.Client] = b.histNext
			b.histNext = (b.histNext + 1) % b.cfg.MaxConns
		}
	}
	b.notifyLocked()
}
