// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clientaddr"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Linger is how long a socket replaced on the same port keeps accepting
// after its reuseport group was steered to the new socket, so handshakes
// begun before the steering and one SYN-ACK retransmission still land
// (target): the linger before closing a TCP listener of
// docs/adr/0015-zero-downtime-upgrades-so-reuseport.md.
const Linger = 3 * time.Second

// codeBind is the RZ code of an activation that cannot bind a listener's
// port; the active Revision keeps serving.
const codeBind = "RZ-CFG-039"

// Set errors.
var (
	// ErrStopped reports a call after StopAccepting, Shutdown or Close.
	ErrStopped = errors.New("listener: the set stopped accepting")
	// ErrPending reports a Prepare while another Plan is neither committed
	// nor aborted.
	ErrPending = errors.New("listener: another plan is pending")
	// ErrPlanDone reports a Plan already committed or aborted.
	ErrPlanDone = errors.New("listener: the plan was committed or aborted")
	// ErrOptions reports unusable Options.
	ErrOptions = errors.New("listener: invalid options")
)

// Options configure a Set.
type Options struct {
	// Handler serves every request of every client listener; required.
	Handler http.Handler
	// Conns is the Node-wide client connection ceiling shared by every
	// client listener (admission.DefaultConnections); required.
	Conns *admission.ConnLimiter
	// Clock times lingers and accept retries, PROXY v2 header deadlines
	// and handshake durations; nil means clock.Real(). It must track wall
	// time, since the PROXY v2 deadline goes to the socket
	// (clientaddr.ListenerOptions).
	Clock clock.Clock
	// ConnStripe returns the stripe of a new connection
	// (retire.Holder.ConnStripe); nil gives stripe 0.
	ConnStripe func() emit.Stripe
	// ErrorLog receives Server.ErrorLog and accept-error records: the
	// telemetry error-log bridge for component "listener", rate limited;
	// nil discards them.
	ErrorLog slog.Handler
	// Logger receives listener lifecycle records; nil discards them.
	Logger *slog.Logger
	// Status, when set, receives the number of http listeners, the
	// cleartext client hops (ruralz_security_cleartext_hops{hop="client"}).
	Status emit.NodeStatus
	// Host is the host listeners bind: empty for every interface, as
	// ruralzd binds; an IP literal otherwise (tests use loopback).
	Host string
	// Steer is set in the process holding the data-directory lock: each
	// port's reuseport group is pinned to the socket serving it after every
	// bind, and each port the Set does not serve yet is first probe-bound
	// without SO_REUSEPORT (ADR-0015 "Pinning"). A handover successor
	// leaves it unset, so it skips the probe of ports the holder's sockets
	// listen on, and calls Pin once it holds the lock.
	Steer bool
}

// Set runs the client listeners of a Node. Its methods are safe for
// concurrent use; Prepare and its Plan's Commit or Abort run one at a
// time, as the loader activates one Revision at a time.
type Set struct {
	opts   Options
	clk    clock.Clock
	log    *slog.Logger
	errLog *slog.Logger
	errH   slog.Handler

	wg sync.WaitGroup

	mu        sync.Mutex
	active    map[snapshot.ListenerKey]*endpoint
	ports     map[int]*endpoint        // the endpoint serving each bound port
	groups    map[int]*reuseport.Group // per bound port with a listening socket
	lingering map[*endpoint]struct{}
	// unlingered is closed, and cleared, once no socket lingers; nil until
	// WaitLingers waits.
	unlingered chan struct{}
	servers    map[*server]struct{} // every Server shard not stopped yet
	pending    *Plan
	steer      bool
	stopped    bool
	closed     bool
}

// New returns a Set serving no listener.
func New(o Options) (*Set, error) {
	switch {
	case o.Handler == nil:
		return nil, fmt.Errorf("%w: Handler is required", ErrOptions)
	case o.Conns == nil:
		return nil, fmt.Errorf("%w: Conns is required", ErrOptions)
	}
	if o.Clock == nil {
		o.Clock = clock.Real()
	}
	errH := o.ErrorLog
	if errH == nil {
		errH = slog.DiscardHandler
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Set{
		opts:      o,
		clk:       o.Clock,
		log:       log,
		errLog:    slog.New(errH),
		errH:      errH,
		active:    make(map[snapshot.ListenerKey]*endpoint),
		ports:     make(map[int]*endpoint),
		groups:    make(map[int]*reuseport.Group),
		lingering: make(map[*endpoint]struct{}),
		servers:   make(map[*server]struct{}),
		steer:     o.Steer,
	}, nil
}

// stripe returns the stripe of a new connection.
func (s *Set) stripe() emit.Stripe {
	if s.opts.ConnStripe != nil {
		return s.opts.ConnStripe()
	}
	return 0
}

// Plan is a Hot Reload of the listeners prepared before the snapshot swap.
type Plan struct {
	s     *Set
	added []*endpoint // bound, not serving
	kept  []update
	gone  []*endpoint
	done  bool // committed or released; guarded by Set.mu
}

// update is what a Hot Reload changes on an endpoint it keeps.
type update struct {
	ep      *endpoint
	metrics *emit.ListenerMetrics
	tls     *tlsState // nil when the TLS settings did not change
}

// Prepare readies the listeners of next before the snapshot swap: an
// endpoint whose listener identity is unchanged is kept (with a new TLS
// state when its TLS settings changed); every other listener binds a new
// socket on its port, on every interface or Options.Host, after a probe
// bind when the Set steers and does not serve the port yet. Two listeners
// on one port, a listener on the admin port, a port another process
// listens on and a port that cannot be bound are RZ-CFG-039; a TLS state
// that cannot be built carries its tlsconf code (RZ-CFG-026 for
// certificates). On error nothing changes. secrets is the Revision's
// resolved secret table.
func (s *Set) Prepare(ctx context.Context, next *snapshot.Snapshot, secrets secret.Store) (*Plan, error) {
	if next == nil {
		return nil, errors.New("listener: prepare: no snapshot")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.stopped:
		return nil, ErrStopped
	case s.pending != nil:
		return nil, ErrPending
	}
	if err := checkPorts(next); err != nil {
		return nil, err
	}
	p := &Plan{s: s}
	keep := make(map[snapshot.ListenerKey]bool, len(next.Listeners))
	for _, l := range next.Listeners {
		key := l.Key()
		if ep, ok := s.active[key]; ok {
			u := update{ep: ep, metrics: metricsOf(l)}
			if cur := ep.tls.Load(); ep.https && !cur.same(l.TLS) {
				st, err := newTLSState(l.TLS, secrets)
				if err != nil {
					p.release()
					return nil, fmt.Errorf("listener %q: %w", l.Name, err)
				}
				u.tls = st
			}
			p.kept = append(p.kept, u)
			keep[key] = true
			continue
		}
		ep, err := s.newEndpoint(ctx, l, secrets)
		if err != nil {
			p.release()
			return nil, err
		}
		p.added = append(p.added, ep)
	}
	for key, ep := range s.active {
		if !keep[key] {
			p.gone = append(p.gone, ep)
		}
	}
	slices.SortFunc(p.gone, func(a, b *endpoint) int { return cmp.Compare(a.key.Name, b.key.Name) })
	s.pending = p
	return p, nil
}

// checkPorts refuses two listeners on one port and a listener on the
// admin port; port 0, which binds a free port, is exempt.
func checkPorts(next *snapshot.Snapshot) error {
	byPort := make(map[int]string, len(next.Listeners))
	for _, l := range next.Listeners {
		if l.Port == 0 {
			continue
		}
		if l.Port == next.Gateway.AdminPort {
			return errcode.Errorf(codeBind, "listener %q: port %d is the admin port", l.Name, l.Port)
		}
		if other, dup := byPort[l.Port]; dup {
			return errcode.Errorf(codeBind, "listeners %q and %q: both use port %d", other, l.Name, l.Port)
		}
		byPort[l.Port] = l.Name
	}
	return nil
}

// newEndpoint builds the endpoint of l: its TLS state on https, then its
// socket, bound without listening where the platform can (Linux), else
// listening. In the lock holder (steer) a port with none of the Set's
// sockets is first probe-bound without SO_REUSEPORT, so a port another
// process listens on, such as a ruralzd with another RURALZ_DATA_DIR,
// refuses the Revision instead of joining that process's reuseport group
// (docs/architecture/03-data-plane.md "Listeners and protocols"); port 0,
// a free port, needs no probe. The caller holds s.mu.
func (s *Set) newEndpoint(ctx context.Context, l *snapshot.Listener, secrets secret.Store) (*endpoint, error) {
	ep := &endpoint{s: s, key: l.Key(), stopped: make(chan struct{}), unlinger: make(chan struct{})}
	ep.metrics.Store(metricsOf(l))
	switch l.Protocol {
	case v1alpha1.ListenerProtocolHTTP:
	case v1alpha1.ListenerProtocolHTTPS:
		st, err := newTLSState(l.TLS, secrets)
		if err != nil {
			return nil, fmt.Errorf("listener %q: %w", l.Name, err)
		}
		ep.https = true
		ep.tls.Store(st)
		ep.tlsCfg = tlsconf.Server(tlsconf.ServerOptions{State: ep.handshakeState})
	default:
		return nil, errcode.Errorf(codeSchema, "listener %q: protocol %q is not http or https", l.Name, l.Protocol)
	}
	addr := net.JoinHostPort(s.opts.Host, strconv.Itoa(l.Port))
	if s.steer && l.Port != 0 && s.groups[l.Port] == nil {
		if err := reuseport.Probe(ctx, addr); err != nil {
			ep.discard()
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			return nil, errcode.Wrap(codeBind, fmt.Errorf("listener %q: probe bind: %w", l.Name, err))
		}
	}
	sock, err := reuseport.Bind(ctx, addr)
	if errors.Is(err, errors.ErrUnsupported) {
		var tcp *net.TCPListener
		if tcp, err = reuseport.Listen(ctx, addr); err == nil {
			ep.tcp, ep.local = tcp, tcp.Addr()
		}
	} else if err == nil {
		ep.sock, ep.local = sock, sock.Addr()
	}
	if err != nil {
		ep.discard()
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, errcode.Wrap(codeBind, fmt.Errorf("listener %q: %w", l.Name, err))
	}
	return ep, nil
}

// Commit applies the plan right after the snapshot swap: new sockets
// listen and serve and their ports are steered to them, kept endpoints
// take the new snapshot's metric handles and TLS states, removed listeners
// stop accepting and their Servers shut down, and sockets replaced on the
// same port linger first. A socket that fails to listen is closed and its
// error returned; the rest of the plan still applies.
func (p *Plan) Commit() error {
	s := p.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.done {
		if s.stopped {
			return ErrStopped
		}
		return ErrPlanDone
	}
	p.done = true
	s.pending = nil
	var errs []error
	for _, ep := range p.added {
		if err := s.startLocked(ep); err != nil {
			errs = append(errs, err)
		}
	}
	for _, u := range p.kept {
		s.updateLocked(u)
	}
	for _, ep := range p.gone {
		delete(s.active, ep.key)
		if cur := s.ports[ep.port()]; cur != nil && cur != ep {
			s.lingerLocked(ep)
			continue
		}
		s.stopLocked(context.Background(), ep, true)
	}
	s.reportHopsLocked()
	return errors.Join(errs...)
}

// Abort releases what Prepare bound and built when the activation fails
// before the swap; the running listeners never changed.
func (p *Plan) Abort() {
	s := p.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.done {
		return
	}
	p.done = true
	if s.pending == p {
		s.pending = nil
	}
	p.release()
}

// release closes the plan's new sockets and TLS states.
func (p *Plan) release() {
	for _, ep := range p.added {
		ep.discard()
	}
	for _, u := range p.kept {
		if u.tls != nil {
			u.tls.close()
		}
	}
}

// startLocked makes ep listen and serve: the ceiling and PROXY v2 wrappers,
// its first group of Server shards, its dispatching accept loop, and its
// place in the port's reuseport group.
func (s *Set) startLocked(ep *endpoint) error {
	if ep.tcp == nil {
		tcp, err := ep.sock.Listen()
		if err != nil {
			ep.discard()
			s.log.Error("listener cannot listen", catalog.KeyListener, ep.key.Name, catalog.KeyError, err.Error())
			return fmt.Errorf("listener %q: %w", ep.key.Name, err)
		}
		ep.tcp, ep.sock = tcp, nil
	}
	ep.acc = s.opts.Conns.Wrap(ep.tcp)
	if ep.key.ProxyProtocol {
		pl, err := clientaddr.NewListener(ep.acc, clientaddr.ListenerOptions{Clock: s.clk})
		if err != nil {
			_ = ep.acc.Close()
			ep.discard()
			return fmt.Errorf("listener %q: %w", ep.key.Name, err)
		}
		ep.acc = pl
	}
	s.active[ep.key] = ep
	port := ep.port()
	s.ports[port] = ep
	g := s.groups[port]
	if g == nil {
		g = new(reuseport.Group)
		s.groups[port] = g
	}
	g.Add(ep.tcp)
	if s.steer {
		s.pinLocked(port)
	}
	ep.group.Store(s.newGroupLocked(ep))
	s.wg.Add(1)
	go ep.acceptLoop()
	s.log.Info("listener serving", catalog.KeyListener, ep.key.Name)
	return nil
}

// updateLocked applies a kept endpoint's update. A changed client
// certificate request mode starts a new group of Server shards on the
// socket and shuts the previous group down, so connections negotiated
// under the old mode end (Security and identity, "Mutual TLS").
func (s *Set) updateLocked(u update) {
	ep := u.ep
	ep.metrics.Store(u.metrics)
	if u.tls == nil {
		return
	}
	old := ep.tls.Swap(u.tls)
	if old == nil {
		return
	}
	old.close()
	if old.mtls == u.tls.mtls {
		return
	}
	prev := ep.group.Swap(s.newGroupLocked(ep))
	s.shutdownGroupLocked(context.Background(), prev)
}

// lingerLocked keeps a replaced socket accepting for the Linger, then
// stops it.
func (s *Set) lingerLocked(ep *endpoint) {
	s.lingering[ep] = struct{}{}
	t := s.clk.NewTimer(Linger)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer t.Stop()
		select {
		case <-t.C():
		case <-ep.unlinger:
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.stopLocked(context.Background(), ep, true)
	}()
}

// stopLocked closes ep's socket once, which closes its dispatching
// listener, updates its port's reuseport group (pinning the group to the
// socket now serving the port) and, with shutdown, shuts its current
// Server shards down under ctx so their connections finish.
func (s *Set) stopLocked(ctx context.Context, ep *endpoint, shutdown bool) {
	ep.stopOnce.Do(func() {
		delete(s.lingering, ep)
		if len(s.lingering) == 0 && s.unlingered != nil {
			close(s.unlingered)
			s.unlingered = nil
		}
		if ep.acc != nil {
			_ = ep.acc.Close()
		}
		port := ep.port()
		if g := s.groups[port]; g != nil && ep.tcp != nil {
			g.Remove(ep.tcp)
			if g.Len() == 0 {
				delete(s.groups, port)
			} else if s.steer {
				s.pinLocked(port)
			}
		}
		if s.ports[port] == ep {
			delete(s.ports, port)
		}
		if st := ep.tls.Load(); st != nil {
			st.close()
		}
		close(ep.stopped)
		s.log.InfoContext(ctx, "listener stopped accepting", catalog.KeyListener, ep.key.Name)
	})
	if shutdown {
		if g := ep.group.Load(); g != nil {
			s.shutdownGroupLocked(ctx, g)
		}
	}
}

// pinLocked steers port's group to the socket serving the port.
func (s *Set) pinLocked(port int) {
	ep, g := s.ports[port], s.groups[port]
	if ep == nil || g == nil || ep.tcp == nil {
		return
	}
	if err := g.Pin(ep.tcp); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		s.log.Warn("listener steering failed", catalog.KeyListener, ep.key.Name, catalog.KeyError, err.Error())
	}
}

// newGroupLocked starts the Shards Server shards of ep, which share the
// handler.
func (s *Set) newGroupLocked(ep *endpoint) *shardGroup {
	g := &shardGroup{}
	for i := range g.shards {
		sv := &server{
			ep:    ep,
			shard: i,
			http:  newHTTPServer(ep.key.Protocol, s.opts.Handler, s.errH),
			ln:    newShardListener(ep),
			done:  make(chan struct{}),
		}
		g.shards[i] = sv
		s.servers[sv] = struct{}{}
		s.wg.Add(1)
		go s.serve(sv)
	}
	return g
}

// shutdownGroupLocked starts the graceful shutdown of every shard of g.
func (s *Set) shutdownGroupLocked(ctx context.Context, g *shardGroup) {
	for _, sv := range g.shards {
		s.shutdownLocked(ctx, sv)
	}
}

// serve runs one Server shard until it closes. Its channel-fed listener
// closes with it, so the dispatching listener never waits on a shard that
// stopped serving.
func (s *Set) serve(sv *server) {
	defer s.wg.Done()
	defer func() { _ = sv.ln.Close() }()
	defer func() {
		if v := recover(); v != nil {
			s.log.Error("listener server panicked", catalog.KeyListener, sv.ep.key.Name, catalog.KeyError, fmt.Sprint(v))
		}
	}()
	if err := sv.http.Serve(sv.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.log.Error("listener server stopped", catalog.KeyListener, sv.ep.key.Name, catalog.KeyError, err.Error())
	}
}

// shutdownLocked starts sv's graceful shutdown, once: HTTP/2 GOAWAY, idle
// connections closed, Connection: close on busy HTTP/1.1 ones, then it
// waits for every connection to end. The end of ctx, Shutdown's deadline
// or Close cuts it short, closing the remaining connections. After Close
// it closes sv at once.
func (s *Set) shutdownLocked(ctx context.Context, sv *server) {
	if sv.shutting {
		return
	}
	sv.shutting = true
	if s.closed {
		_ = sv.http.Close()
		return
	}
	// The HTTP/1.1 half of Shutdown at once, before the goroutine runs it:
	// idle connections close and every later response carries Connection:
	// close, so a response written as the shutdown starts cannot keep its
	// connection alive.
	sv.http.SetKeepAlivesEnabled(false)
	ctx, cancel := context.WithCancel(ctx)
	sv.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(sv.done)
		defer cancel()
		if err := sv.http.Shutdown(ctx); err != nil {
			_ = sv.http.Close()
		}
		s.mu.Lock()
		delete(s.servers, sv)
		s.mu.Unlock()
	}()
}

// reportHopsLocked reports the http listeners as cleartext client hops.
func (s *Set) reportHopsLocked() {
	if s.opts.Status == nil {
		return
	}
	n := 0
	for _, ep := range s.active {
		if !ep.https {
			n++
		}
	}
	s.opts.Status.SetCleartextHops(catalog.HopClient, n)
}

// Addr returns the bound address of the serving listener named name, or
// nil.
func (s *Set) Addr(name string) net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, ep := range s.active {
		if key.Name == name {
			return ep.local
		}
	}
	return nil
}

// Pin pins each port's reuseport group to the socket serving the port, and
// every group after later binds, probing the ports it does not serve yet:
// the lock holder's pinning of ADR-0015. A handover successor calls it
// once it holds the lock, when the previous holder's sockets are closed;
// a holder calls it to re-pin each group to its own sockets when the
// successor it steered to is lost during the linger, and keeps serving.
// Off Linux it returns errors.ErrUnsupported.
func (s *Set) Pin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steer = true
	var errs []error
	for port, ep := range s.ports {
		if err := s.groups[port].Pin(ep.tcp); err != nil {
			errs = append(errs, fmt.Errorf("listener %q: %w", ep.key.Name, err))
		}
	}
	return errors.Join(errs...)
}

// WaitLingers waits until no socket a Hot Reload replaced on its port
// lingers, so each port's reuseport group holds only the socket serving
// it; it returns ctx's error when ctx ends first. The kernel moves the
// last socket of a group into the place of one that closes, so a socket
// closing while a handover successor's socket is in its group would move
// the successor's socket to a position the Set's groups do not mirror: a
// holder waits before it accepts a handover, holding activations, so no
// socket of its own closes until its linger's end.
func (s *Set) WaitLingers(ctx context.Context) error {
	for {
		s.mu.Lock()
		if len(s.lingering) == 0 {
			s.mu.Unlock()
			return nil
		}
		if s.unlingered == nil {
			s.unlingered = make(chan struct{})
		}
		done := s.unlingered
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// SteerToSuccessor steers every port's reuseport group to the socket a
// handover successor added after this process's sockets (ADR-0015
// "Steering") and stops pinning; the sockets keep accepting until
// StopAccepting. Off Linux it returns errors.ErrUnsupported.
func (s *Set) SteerToSuccessor() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steer = false
	var errs []error
	for port, g := range s.groups {
		if err := g.SteerTo(g.Len()); err != nil {
			errs = append(errs, fmt.Errorf("port %d: %w", port, err))
		}
	}
	return errors.Join(errs...)
}

// Bound reports whether a client listener socket is bound and accepting:
// a serving or lingering one not closed yet. A Plan's socket, bound but
// not serving, does not count. A Node that never answered /readyz with 200
// and has none skips the Drain's accept window
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "Drain
// timeline defaults").
func (s *Set) Bound() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ep := range s.endpointsLocked() {
		select {
		case <-ep.stopped:
		default:
			return true
		}
	}
	return false
}

// StopAccepting closes every socket, lingering ones included, and so
// every dispatching listener, while the Server shards keep serving their
// connections; a pending Plan is aborted and Prepare then fails. It is the
// Drain's stop-accepting step, and a handover's holder calls it at the
// linger's end. It is idempotent.
func (s *Set) StopAccepting() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopAllLocked(context.Background(), false)
}

// ShutdownShard starts the graceful shutdown of Server shard i of every
// client listener, lingering and removed ones included: Server.Shutdown
// sends one HTTP/2 GOAWAY (NO_ERROR) on each of the shard's connections,
// closes its idle HTTP/1.1 ones and gives each busy HTTP/1.1 one
// Connection: close on its next response. Its HTTP/1.1 part is done when
// it returns, so a response written after it carries Connection: close;
// it does not wait for the shard's connections, which finish in the
// background until Shutdown or Close cuts them short. A shard already
// shutting down is left as it is, and an i outside [0, Shards) shuts down
// nothing. The Drain calls it for each i across its jitter window, after
// StopAccepting, so clients reconnect in Shards waves; on a Set still
// accepting, the dispatching listener passes a shut down shard's turn to
// the next shard still serving.
func (s *Set) ShutdownShard(i int) {
	if i < 0 || i >= Shards {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for sv := range s.servers {
		if sv.shard == i {
			s.shutdownLocked(context.Background(), sv)
		}
	}
}

// stopAllLocked stops accepting for good; with shutdown, the endpoints'
// Servers shut down under ctx.
func (s *Set) stopAllLocked(ctx context.Context, shutdown bool) {
	s.stopped = true
	if p := s.pending; p != nil {
		p.done = true
		s.pending = nil
		p.release()
	}
	for _, ep := range s.endpointsLocked() {
		ep.endLinger()
		s.stopLocked(ctx, ep, shutdown)
	}
}

// endpointsLocked returns the serving and lingering endpoints.
func (s *Set) endpointsLocked() []*endpoint {
	eps := make([]*endpoint, 0, len(s.active)+len(s.lingering))
	for _, ep := range s.active {
		eps = append(eps, ep)
	}
	for ep := range s.lingering {
		eps = append(eps, ep)
	}
	return eps
}

// Shutdown is the Drain deadline's listener step: every socket closes,
// every Server shard not shutting down yet shuts down (HTTP/2 GOAWAY,
// idle connections closed, Connection: close on busy HTTP/1.1 ones), and
// it waits until every connection has ended or ctx is done; then it closes
// the remaining connections and returns ctx's error.
func (s *Set) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrStopped
	}
	s.stopAllLocked(ctx, true)
	done := make([]chan struct{}, 0, len(s.servers))
	for sv := range s.servers {
		s.shutdownLocked(ctx, sv)
		done = append(done, sv.done)
	}
	s.mu.Unlock()
	for _, d := range done {
		select {
		case <-d:
		case <-ctx.Done():
			s.mu.Lock()
			for sv := range s.servers {
				if sv.cancel != nil {
					sv.cancel()
				}
			}
			s.mu.Unlock()
			for _, d := range done {
				<-d
			}
			return ctx.Err()
		}
	}
	return nil
}

// Close stops everything at once: sockets close, Servers close with their
// connections, and Close waits for every goroutine the Set started, a
// handshake still running included (net/http bounds it by
// ReadHeaderTimeout). It is idempotent.
func (s *Set) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.stopAllLocked(context.Background(), false)
		for sv := range s.servers {
			if sv.cancel != nil {
				sv.cancel()
			}
			_ = sv.http.Close()
		}
		if s.opts.Status != nil {
			s.opts.Status.SetCleartextHops(catalog.HopClient, 0)
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}
