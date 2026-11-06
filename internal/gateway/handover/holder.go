// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handover

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Accept retry backoff of the holder's socket after an accept error other
// than a closed socket, as for client listeners: 5 ms, doubling up to 1 s
// (docs/architecture/03-data-plane.md "Listeners and protocols").
const (
	acceptRetryMin = 5 * time.Millisecond
	acceptRetryMax = time.Second
)

// HolderConfig configures the holder side of the handover.
type HolderConfig struct {
	// Dir is the data directory; the socket is its handover.sock.
	// Required.
	Dir *nodedir.Dir
	// Lock is the process's Lock, holding the data-directory lock; its
	// Release stops the socket and sends released. Required.
	Lock *Lock
	// Activations is the activation loader; required.
	Activations Activations
	// Drain is the process's Drainer; required.
	Drain Drainer
	// Sockets are the holder's socket groups: the client listeners and
	// the admin port.
	Sockets []Sockets
	// Admin is the admin server; nil skips the readiness move.
	Admin Admin
	// Meter reads the usage that accepted and usage messages report.
	Meter Meter
	// Watchdog is the interval the holder's watchdog runs with, 0 for
	// none: its own, as sd_watchdog_enabled(3) reads its environment, or,
	// for a process that took the lock as a handover successor without
	// one, the interval its own holder's accepted carried. The accepted
	// message carries it as watchdogUsec, so a new process the service
	// manager gives no interval, as systemd gives none to a child of
	// ExecReload=, runs its watchdog with it. It is not negative.
	Watchdog time.Duration
	// Clock times the gate, the listening bound, the linger and the usage
	// reports; nil means clock.Real().
	Clock clock.Clock
	// Logger receives the handover's records; nil discards them.
	Logger *slog.Logger

	// euid returns the user peers must run as; nil means os.Geteuid.
	// Tests only.
	euid func() int64
}

// Holder serves the handover socket of the lock holder: it answers each
// new process's ready message, steers every group to an accepted one,
// lingers, then stops accepting and starts its handover Drain, and
// reports its usage until it exits.
type Holder struct {
	cfg  HolderConfig
	clk  clock.Clock
	log  *slog.Logger
	path string
	ln   *net.UnixListener

	cancel context.CancelFunc
	wg     sync.WaitGroup
	slots  chan struct{}
	once   sync.Once

	mu       sync.Mutex
	stopped  bool          // the socket is closed
	session  *session      // the handover in progress, nil when none
	released chan struct{} // closed once the lock is released
	rssWarn  bool          // a resident set read failure was logged
}

// StartHolder creates the owner-only handover socket, removing one a
// crashed holder left, and serves it until Close or until its Lock is
// released. The process must hold the lock (cfg.Lock.Take). ctx bounds
// every session.
func StartHolder(ctx context.Context, cfg HolderConfig) (*Holder, error) {
	switch {
	case cfg.Dir == nil:
		return nil, fmt.Errorf("%w: Dir is required", ErrConfig)
	case cfg.Lock == nil:
		return nil, fmt.Errorf("%w: Lock is required", ErrConfig)
	case cfg.Activations == nil:
		return nil, fmt.Errorf("%w: Activations is required", ErrConfig)
	case cfg.Drain == nil:
		return nil, fmt.Errorf("%w: Drain is required", ErrConfig)
	case cfg.Watchdog < 0:
		return nil, fmt.Errorf("%w: negative Watchdog %v", ErrConfig, cfg.Watchdog)
	}
	for _, g := range cfg.Sockets {
		if g == nil {
			return nil, fmt.Errorf("%w: a nil Sockets", ErrConfig)
		}
	}
	path := cfg.Dir.HandoverSocketPath()
	if err := checkSocketPath(path); err != nil {
		return nil, err
	}
	h := &Holder{
		cfg:      cfg,
		clk:      cfg.Clock,
		log:      cfg.Logger,
		path:     path,
		slots:    make(chan struct{}, maxConns),
		released: make(chan struct{}),
	}
	if h.clk == nil {
		h.clk = clock.Real()
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	if h.cfg.euid == nil {
		h.cfg.euid = func() int64 { return int64(os.Geteuid()) }
	}
	if err := cfg.Lock.attach(h, func() error { return h.listen(ctx) }); err != nil {
		return nil, err
	}
	hctx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	h.wg.Add(1)
	go h.acceptLoop(hctx)
	h.log.InfoContext(ctx, "handover socket serving", catalog.KeyPath, path)
	return h, nil
}

// listen creates the socket, owner-only, after removing a file a crashed
// holder left; the caller holds the lock.
func (h *Holder) listen(ctx context.Context) error {
	if err := os.Remove(h.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("handover: removing a stale socket: %w", err)
	}
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "unix", h.path)
	if err != nil {
		return fmt.Errorf("handover: %w", err)
	}
	ln, ok := l.(*net.UnixListener)
	if !ok {
		_ = l.Close()
		return fmt.Errorf("handover: %T is not a Unix listener", l)
	}
	// The socket file is removed while the lock is held, never by Close,
	// which could run after a successor created its own.
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(h.path, nodedir.FilePerm); err != nil {
		_ = ln.Close()
		_ = os.Remove(h.path)
		return fmt.Errorf("handover: %w", err)
	}
	h.ln = ln
	return nil
}

// stopListening closes the socket once, removing its file when remove is
// set, which the Lock sets only while it holds the lock.
func (h *Holder) stopListening(remove bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	h.stopped = true
	_ = h.ln.Close()
	if remove {
		_ = os.Remove(h.path)
	}
}

// lockReleased lets the handover in progress send released.
func (h *Holder) lockReleased() {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.released:
	default:
		close(h.released)
	}
}

// Close stops the socket and every session, a session after accepted
// re-pinning and resuming first, and waits for them. The handover
// connection closes, so a new process that took the lock restores its
// ceilings. Close is idempotent.
func (h *Holder) Close() error {
	h.once.Do(func() {
		h.cfg.Lock.detach(h)
		h.cancel()
	})
	h.wg.Wait()
	return nil
}

// acceptLoop serves the socket until it closes, at most maxConns
// connections at a time.
func (h *Holder) acceptLoop(ctx context.Context) {
	defer h.wg.Done()
	var delay time.Duration
	for {
		c, err := h.ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			delay = min(max(2*delay, acceptRetryMin), acceptRetryMax)
			h.log.WarnContext(ctx, "handover socket accept error, retrying", catalog.KeyError, err.Error())
			if h.clk.Sleep(ctx, delay) != nil {
				return
			}
			continue
		}
		delay = 0
		select {
		case h.slots <- struct{}{}:
			h.wg.Add(1)
			go h.serve(ctx, c)
		default:
			_ = c.Close()
			h.log.WarnContext(ctx, "handover connection closed: too many at once")
		}
	}
}

// serve checks a connection's peer and runs its session.
func (h *Holder) serve(ctx context.Context, c *net.UnixConn) {
	defer h.wg.Done()
	defer func() { <-h.slots }()
	uid, pid, err := peerCredentials(c)
	switch {
	case err != nil:
		_ = c.Close()
		h.log.WarnContext(ctx, "handover connection refused: no peer credentials", catalog.KeyError, err.Error())
		return
	case uid != h.cfg.euid():
		_ = c.Close()
		h.log.WarnContext(ctx, "handover connection refused", catalog.KeyError, ErrPeerUser.Error(), "uid", uid, "pid", pid)
		return
	}
	s := &session{h: h, p: newPeer(c), pid: pid, start: h.clk.Now()}
	defer s.end(ctx)
	s.run(ctx)
}

// claim takes the one handover slot for s; false while another session
// holds it.
func (h *Holder) claim(s *session) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.session != nil {
		return false
	}
	h.session = s
	return true
}

// unclaim frees the handover slot s holds.
func (h *Holder) unclaim(s *session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.session == s {
		h.session = nil
	}
}

// usage reads the usage to report; a resident set read failure is logged
// once and reports 0.
func (h *Holder) usage(ctx context.Context) Usage {
	u, err := h.cfg.Meter.Usage()
	if err != nil {
		h.mu.Lock()
		first := !h.rssWarn
		h.rssWarn = true
		h.mu.Unlock()
		if first {
			h.log.WarnContext(ctx, "handover usage: resident set unreadable", catalog.KeyError, err.Error())
		}
	}
	return u
}

// bounded returns a context that ends with parent, when the Drain
// starts, or at deadline on the Holder's clock; stop releases it.
func (h *Holder) bounded(parent context.Context, deadline time.Time) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	t := h.clk.AfterFuncAt(deadline, cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-h.cfg.Drain.Started():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		t.Stop()
		cancel()
		<-done
	}
}

// draining reports whether the holder's Drain started.
func (h *Holder) draining() bool {
	select {
	case <-h.cfg.Drain.Started():
		return true
	default:
		return false
	}
}

// session is one connection of the holder's socket: a new process's
// handover, or a connection refused.
type session struct {
	h     *Holder
	p     *peer
	pid   int       // the peer's process ID (SO_PEERCRED)
	start time.Time // when the connection was accepted

	claimed    bool        // it holds the holder's handover slot
	accepted   bool        // accepted sent: the Drain and activations are held
	steered    bool        // every group steers to the new process
	handedOver bool        // the linger ended and the handover's Drain started
	tick       clock.Timer // the next usage report, from accepted
	listen     clock.Timer // the end of the wait for listening, from accepted
}

// decision is what a ready message got.
type decision uint8

const (
	// decisionEnd: refused, or the session cannot go on.
	decisionEnd decision = iota
	// decisionReload: reload sent; the next ready is answered.
	decisionReload
	// decisionAccepted: accepted sent.
	decisionAccepted
)

// run takes the session through the gate, the wait for listening, the
// steering and linger, and the handed-over reports.
func (s *session) run(ctx context.Context) {
	if !s.gate(ctx) {
		return
	}
	if !s.awaitListening(ctx) {
		s.abort()
		return
	}
	if !s.steer(ctx) {
		return
	}
	s.follow(ctx)
}

// end closes the connection and frees the slot. A session that panicked
// after accepted, before its Drain began, gives the holder back its
// sockets, its Drain and its activations, as a lost successor does.
func (s *session) end(ctx context.Context) {
	if v := recover(); v != nil {
		s.h.log.ErrorContext(ctx, "handover session panicked", catalog.KeyError, fmt.Sprint(v))
		if s.accepted && !s.handedOver {
			s.abort()
		}
	}
	if s.tick != nil {
		s.tick.Stop()
	}
	if s.listen != nil {
		s.listen.Stop()
	}
	s.p.close()
	if s.claimed {
		s.h.unclaim(s)
	}
}

// gate answers ready messages until one is accepted or refused, or the
// connection, the gate or ctx ends: GateTimeout from the connection, the
// new process's own bound.
func (s *session) gate(ctx context.Context) bool {
	h := s.h
	deadline := s.start.Add(GateTimeout)
	timer := h.clk.NewTimerAt(deadline)
	defer timer.Stop()
	for {
		var l line
		select {
		case l = <-s.p.in:
		case <-timer.C():
			h.log.WarnContext(ctx, "handover gate timed out", "pid", s.pid)
			return false
		case <-ctx.Done():
			return false
		}
		if l.err != nil {
			if s.claimed {
				h.log.InfoContext(ctx, "handover ended: the new process closed before accepted", "pid", s.pid)
			}
			return false
		}
		r, err := decodeReady(l.b)
		if err != nil {
			s.refuse(ctx, ReasonUnknownMarker, err)
			return false
		}
		if h.draining() {
			s.refuse(ctx, ReasonDraining, nil)
			return false
		}
		if !s.claimed {
			if !h.claim(s) {
				s.refuse(ctx, ReasonBusy, nil)
				return false
			}
			s.claimed = true
			h.log.InfoContext(ctx, "handover started", "pid", s.pid, "ready_pid", r.PID, "version", r.Version)
		}
		switch s.decide(ctx, r, deadline) {
		case decisionAccepted:
			return true
		case decisionReload:
		default:
			return false
		}
	}
}

// decide answers one ready message once no activation runs: accepted,
// with the holder's usage and watchdog interval, when its active Revision
// is the holder's, or the holder has none; reload of the holder's
// candidate when that is its active Revision; else refused with
// older_candidate. Before accepted the holder waits until no replaced
// socket lingers and holds its Drain.
func (s *session) decide(ctx context.Context, r ready, deadline time.Time) decision {
	h := s.h
	hctx, stop := h.bounded(ctx, deadline)
	defer stop()
	revs, err := h.cfg.Activations.Hold(hctx)
	if err != nil {
		s.unfinished(ctx, err)
		return decisionEnd
	}
	switch {
	case revs.Active.IsZero() || r.ActiveDigest == revs.Active:
		for _, g := range h.cfg.Sockets {
			if err := g.WaitLingers(hctx); err != nil {
				h.cfg.Activations.Release()
				s.unfinished(ctx, err)
				return decisionEnd
			}
		}
		if !h.cfg.Drain.Hold() {
			h.cfg.Activations.Release()
			s.refuse(ctx, ReasonDraining, nil)
			return decisionEnd
		}
		// Both bounds count from accepted.
		s.tick = h.clk.NewTimer(UsageInterval)
		s.listen = h.clk.NewTimer(ListeningTimeout)
		if err := s.p.send(ctx, acceptedMessage(h.usage(ctx), h.cfg.Watchdog)); err != nil {
			h.cfg.Drain.Resume()
			h.cfg.Activations.Release()
			h.log.WarnContext(ctx, "handover ended: accepted not delivered", "pid", s.pid, catalog.KeyError, err.Error())
			return decisionEnd
		}
		s.accepted = true
		h.log.InfoContext(ctx, "handover accepted", "pid", s.pid, "digest", digestAttr(revs.Active))
		return decisionAccepted
	case revs.Candidate == revs.Active:
		h.cfg.Activations.Release()
		if err := s.p.send(ctx, reloadWire{Type: TypeReload, Digest: revs.Candidate}); err != nil {
			h.log.InfoContext(ctx, "handover ended: reload not delivered", "pid", s.pid, catalog.KeyError, err.Error())
			return decisionEnd
		}
		h.log.InfoContext(ctx, "handover asked for a reload", "pid", s.pid, "digest", revs.Candidate.String())
		return decisionReload
	default:
		h.cfg.Activations.Release()
		s.refuse(ctx, ReasonOlderCandidate, nil)
		return decisionEnd
	}
}

// unfinished ends a decision whose wait ended early: refused with
// draining once the Drain started; nothing more when ctx or the gate
// ended.
func (s *session) unfinished(ctx context.Context, err error) {
	h := s.h
	switch {
	case ctx.Err() != nil:
	case h.draining():
		s.refuse(ctx, ReasonDraining, nil)
	default:
		h.log.WarnContext(ctx, "handover gate timed out", "pid", s.pid, catalog.KeyError, err.Error())
	}
}

// refuse sends refused with reason; cause is logged.
func (s *session) refuse(ctx context.Context, reason string, cause error) {
	attrs := []any{catalog.KeyReason, reason, "pid", s.pid}
	if cause != nil {
		attrs = append(attrs, catalog.KeyError, cause.Error())
	}
	s.h.log.WarnContext(ctx, "handover refused", attrs...)
	if err := s.p.send(ctx, refusedWire{Type: TypeRefused, Reason: reason}); err != nil {
		s.h.log.DebugContext(ctx, "handover refusal not delivered", catalog.KeyError, err.Error())
	}
}

// report sends a usage message and arms the next; false when it cannot be
// delivered.
func (s *session) report(ctx context.Context) bool {
	if err := s.p.send(ctx, usageWire{Type: TypeUsage, Usage: s.h.usage(ctx)}); err != nil {
		return false
	}
	s.tick.Reset(UsageInterval)
	return true
}

// awaitListening waits for listening within ListeningTimeout of
// accepted, reporting usage; false when the new process closed, sent
// anything else, or the bound or ctx ended.
func (s *session) awaitListening(ctx context.Context) bool {
	h := s.h
	defer s.listen.Stop()
	for {
		select {
		case l := <-s.p.in:
			if l.err != nil {
				h.log.WarnContext(ctx, "handover aborted: the new process closed before listening", "pid", s.pid)
				return false
			}
			m, err := decode(l.b)
			if err != nil || m.Type != TypeListening {
				h.log.WarnContext(ctx, "handover aborted: a message other than listening", "pid", s.pid, "type", m.Type)
				return false
			}
			return true
		case <-s.tick.C():
			if !s.report(ctx) {
				h.log.WarnContext(ctx, "handover aborted: usage not delivered", "pid", s.pid)
				return false
			}
		case <-s.listen.C():
			h.log.WarnContext(ctx, "handover aborted: no listening in time", "pid", s.pid)
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// abort ends an accepted handover before steering: the holder keeps its
// pins and serves on, a stop signal or failure waiting since accepted
// starting its Drain, and activations run again.
func (s *session) abort() {
	h := s.h
	if s.steered {
		s.repin()
	}
	s.accepted = false
	h.cfg.Drain.Resume()
	h.cfg.Activations.Release()
}

// repin pins every group back to the holder's own sockets and keeps
// admin connections alive again.
func (s *session) repin() {
	h := s.h
	s.steered = false
	for _, g := range h.cfg.Sockets {
		if err := g.Pin(); err != nil && !isUnsupported(err) {
			h.log.Warn("handover re-pin failed", catalog.KeyError, err.Error())
		}
	}
	if h.cfg.Admin != nil {
		h.cfg.Admin.EnableKeepAlives()
	}
}

// steer steers every group to the new process, moves keep-alive health
// checkers off the admin server and lingers; at the linger's end it stops
// accepting and starts the handover's Drain. A new process lost during
// the linger makes the holder re-pin, skip its Drain and keep serving:
// false.
func (s *session) steer(ctx context.Context) bool {
	h := s.h
	s.steered = true
	for _, g := range h.cfg.Sockets {
		if err := g.SteerToSuccessor(); err != nil && !isUnsupported(err) {
			h.log.WarnContext(ctx, "handover steering failed", catalog.KeyError, err.Error())
		}
	}
	if h.cfg.Admin != nil {
		h.cfg.Admin.DisableKeepAlives()
	}
	h.log.InfoContext(ctx, "handover steered every group to the new process", "pid", s.pid)
	linger := h.clk.NewTimer(Linger)
	defer linger.Stop()
	for {
		select {
		case l := <-s.p.in:
			if l.err != nil {
				h.log.WarnContext(ctx, "handover lost the new process during the linger: re-pinned, serving", "pid", s.pid)
				s.abort()
				return false
			}
		case <-s.tick.C():
			if !s.report(ctx) {
				h.log.WarnContext(ctx, "handover lost the new process during the linger: re-pinned, serving", "pid", s.pid)
				s.abort()
				return false
			}
		case <-linger.C():
			for _, g := range h.cfg.Sockets {
				g.StopAccepting()
			}
			s.handedOver = true
			h.log.InfoContext(ctx, "handover linger ended: listeners closed, draining", "pid", s.pid)
			h.cfg.Drain.Handover()
			return true
		case <-ctx.Done():
			s.abort()
			return false
		}
	}
}

// follow reports usage until the holder exits, sending released once its
// Drain releases the lock.
func (s *session) follow(ctx context.Context) {
	h := s.h
	released := h.released
	for {
		select {
		case l := <-s.p.in:
			if l.err != nil {
				h.log.InfoContext(ctx, "handover connection closed by the new process", "pid", s.pid)
				return
			}
		case <-s.tick.C():
			if !s.report(ctx) {
				return
			}
		case <-released:
			released = nil
			if err := s.p.send(ctx, bareWire{Type: TypeReleased}); err != nil {
				h.log.WarnContext(ctx, "handover released not delivered", "pid", s.pid, catalog.KeyError, err.Error())
				return
			}
			h.log.InfoContext(ctx, "handover released the lock to the new process", "pid", s.pid)
		case <-ctx.Done():
			return
		}
	}
}

// digestAttr formats a digest for a log record; "none" for zero.
func digestAttr(d revision.Digest) string {
	if d.IsZero() {
		return "none"
	}
	return d.String()
}
