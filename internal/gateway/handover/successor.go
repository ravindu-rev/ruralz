// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handover

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// SuccessorConfig configures a new process's side of the handover: a
// process that found a live lock holder at start, booted the holder's
// active Revision from its candidate (or, when the holder has none, by the
// file-mode order) and bound its sockets without listening.
type SuccessorConfig struct {
	// Dir is the data directory; required.
	Dir *nodedir.Dir
	// Lock is the process's Lock, which records the data-directory lock
	// once the process takes it; required.
	Lock *Lock
	// Drain is the process's Drainer; required.
	Drain DrainStart
	// Start is when the process started: the gate lasts GateTimeout from
	// it. Zero means when NewSuccessor runs.
	Start time.Time
	// Version is the build version the ready message reports.
	Version string
	// PID is the process ID the ready message and MAINPID= report; 0
	// means os.Getpid().
	PID int
	// Active returns the process's active Revision, zero for none;
	// required.
	Active func() revision.Digest
	// Reload boots the holder's newer candidate of the given digest
	// through the full gate, replacing the Revision booted before and
	// rebinding as it needs, so Active returns the digest; an error ends
	// the handover. Required.
	Reload func(ctx context.Context, digest revision.Digest) error
	// Bound are the sockets bound before the gate; required.
	Bound Bound
	// Pinners pin the process's groups once it takes the lock: the client
	// listeners and the admin port.
	Pinners []Pinner
	// Record is the holder.json record written once the process takes the
	// lock (nodedir.NewHolder).
	Record nodedir.Holder
	// Notifier sends MAINPID= once the process takes the lock; nil
	// without a service manager.
	Notifier Notifier
	// Ceilings are lowered by the holder's usage until the holder is gone.
	Ceilings Ceilings
	// Clock times the gate and the retries; nil means clock.Real().
	Clock clock.Clock
	// Logger receives the handover's records; nil discards them.
	Logger *slog.Logger

	// euid returns the user the holder must run as; nil means os.Geteuid.
	// Tests only.
	euid func() int64
}

// Successor runs a new process's handover: the gate, listening once
// accepted, and taking the lock once the holder released it or exited.
type Successor struct {
	cfg   SuccessorConfig
	clk   clock.Clock
	log   *slog.Logger
	path  string
	pid   int
	start time.Time
	ceil  *shared

	ran       atomic.Bool
	listening bool // Bound.Listen succeeded; Run's goroutine only
	abandon   sync.Once
	usageWarn sync.Once

	wg sync.WaitGroup
	mu sync.Mutex
	// cancel ends the follower; nil without one.
	cancel context.CancelFunc
	closed bool
}

// NewSuccessor returns a Successor; Run runs its handover.
func NewSuccessor(cfg SuccessorConfig) (*Successor, error) {
	switch {
	case cfg.Dir == nil:
		return nil, fmt.Errorf("%w: Dir is required", ErrConfig)
	case cfg.Lock == nil:
		return nil, fmt.Errorf("%w: Lock is required", ErrConfig)
	case cfg.Drain == nil:
		return nil, fmt.Errorf("%w: Drain is required", ErrConfig)
	case cfg.Active == nil:
		return nil, fmt.Errorf("%w: Active is required", ErrConfig)
	case cfg.Reload == nil:
		return nil, fmt.Errorf("%w: Reload is required", ErrConfig)
	case cfg.Bound == nil:
		return nil, fmt.Errorf("%w: Bound is required", ErrConfig)
	}
	for _, p := range cfg.Pinners {
		if p == nil {
			return nil, fmt.Errorf("%w: a nil Pinner", ErrConfig)
		}
	}
	path := cfg.Dir.HandoverSocketPath()
	if err := checkSocketPath(path); err != nil {
		return nil, err
	}
	s := &Successor{cfg: cfg, clk: cfg.Clock, log: cfg.Logger, path: path, pid: cfg.PID, start: cfg.Start, ceil: newShared(cfg.Ceilings)}
	if s.clk == nil {
		s.clk = clock.Real()
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.pid == 0 {
		s.pid = os.Getpid()
	}
	if s.start.IsZero() {
		s.start = s.clk.Now()
	}
	if s.cfg.euid == nil {
		s.cfg.euid = func() int64 { return int64(os.Geteuid()) }
	}
	return s, nil
}

// Run runs the handover once and returns its Outcome; the error explains
// any Outcome but OutcomeHolder, and with OutcomeHolder a listen failure
// the wiring treats as a failed part. Every RetryInterval until the gate
// ends, GateTimeout after the process's start, it takes the lock if the
// holder exited, else connects to the holder's socket and sends ready,
// answering reload with Reload and ready again. Accepted, it lowers its
// ceilings by the holder's usage, listens and sends listening; once the
// holder released the lock it takes it. A follower then applies the
// holder's usage reports until the holder is gone, when the ceilings are
// restored; Close or the end of ctx stops it.
func (s *Successor) Run(ctx context.Context) (Outcome, error) {
	if s.ran.Swap(true) {
		return 0, fmt.Errorf("%w: Run runs once", ErrConfig)
	}
	deadline := s.start.Add(GateTimeout)
	s.log.InfoContext(ctx, "handover gate started", catalog.KeyPath, s.path)
	for {
		if nl := s.tryLock(ctx); nl != nil {
			s.log.InfoContext(ctx, "handover: the holder exited first, taking over")
			return s.becomeHolder(ctx, nl)
		}
		if out, done, err := s.attempt(ctx, deadline); done {
			return out, err
		}
		now := s.clk.Now()
		if !now.Before(deadline) {
			s.close()
			s.log.WarnContext(ctx, "handover not accepted in time: exiting")
			return OutcomeRefused, ErrTimeout
		}
		if out, done, err := s.wait(ctx, min(RetryInterval, deadline.Sub(now))); done {
			return out, err
		}
	}
}

// Close stops the follower, which restores the ceilings, and waits for
// it. It is idempotent.
func (s *Successor) Close() error {
	s.mu.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

// tryLock takes the lock when the holder exited; nil while a live holder
// keeps it.
func (s *Successor) tryLock(ctx context.Context) *nodedir.Lock {
	nl, err := s.cfg.Dir.TryLock()
	if err != nil {
		if !errors.Is(err, nodedir.ErrLocked) {
			s.log.WarnContext(ctx, "handover cannot try the lock", catalog.KeyError, err.Error())
		}
		return nil
	}
	return nl
}

// wait waits d, or until the Drain starts or ctx ends: then done.
func (s *Successor) wait(ctx context.Context, d time.Duration) (Outcome, bool, error) {
	t := s.clk.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return 0, false, nil
	case <-s.cfg.Drain.Started():
		s.stop()
		return OutcomeStopped, true, nil
	case <-ctx.Done():
		s.stop()
		return OutcomeStopped, true, ctx.Err()
	}
}

// attempt connects to the holder's socket and runs the gate on the
// connection; not done when the socket is missing or unanswered.
func (s *Successor) attempt(ctx context.Context, deadline time.Time) (Outcome, bool, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", s.path)
	if err != nil {
		s.log.DebugContext(ctx, "handover socket unanswered", catalog.KeyError, err.Error())
		return 0, false, nil
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		_ = c.Close()
		return 0, false, nil
	}
	uid, _, err := peerCredentials(uc)
	switch {
	case err != nil:
		_ = c.Close()
		s.log.WarnContext(ctx, "handover socket refused: no peer credentials", catalog.KeyError, err.Error())
		return 0, false, nil
	case uid != s.cfg.euid():
		_ = c.Close()
		s.log.WarnContext(ctx, "handover socket refused", catalog.KeyError, ErrPeerUser.Error(), "uid", uid)
		return 0, false, nil
	}
	return s.gate(ctx, newPeer(uc), deadline)
}

// sendReady sends the ready message: the version, PID, markers and active
// Revision.
func (s *Successor) sendReady(ctx context.Context, p *peer) error {
	return p.send(ctx, readyMessage(ready{Version: s.cfg.Version, PID: s.pid, Markers: Markers(), ActiveDigest: s.cfg.Active()}))
}

// gate sends ready and answers the holder until accepted, refused, the
// gate's end, the Drain or ctx; not done when the connection closed
// unanswered.
func (s *Successor) gate(ctx context.Context, p *peer, deadline time.Time) (Outcome, bool, error) {
	if err := s.sendReady(ctx, p); err != nil {
		p.close()
		s.log.DebugContext(ctx, "handover socket unanswered", catalog.KeyError, err.Error())
		return 0, false, nil
	}
	timer := s.clk.NewTimer(deadline.Sub(s.clk.Now()))
	defer timer.Stop()
	for {
		select {
		case l := <-p.in:
			if l.err != nil {
				p.close()
				if errors.Is(l.err, ErrProtocol) {
					return OutcomeRefused, true, s.giveUp(ctx, l.err)
				}
				s.log.DebugContext(ctx, "handover connection closed unanswered", catalog.KeyError, l.err.Error())
				return 0, false, nil
			}
			m, err := decode(l.b)
			if err != nil {
				p.close()
				return OutcomeRefused, true, s.giveUp(ctx, err)
			}
			switch m.Type {
			case TypeRefused:
				p.close()
				return OutcomeRefused, true, s.giveUp(ctx, &RefusedError{Reason: m.Reason})
			case TypeReload:
				switch out, next, err := s.reload(ctx, p, m.Digest, deadline); next {
				case stepDone:
					return out, true, err
				case stepRetry:
					return 0, false, nil
				case stepContinue:
				}
			case TypeAccepted:
				timer.Stop() // the gate bounds the wait for accepted only
				out, err := s.accepted(ctx, p, m.Usage, deadline)
				return out, true, err
			default:
				p.close()
				return OutcomeRefused, true, s.giveUp(ctx, fmt.Errorf("%w: type %q before accepted", ErrProtocol, m.Type))
			}
		case <-timer.C():
			p.close()
			s.close()
			s.log.WarnContext(ctx, "handover not accepted in time: exiting")
			return OutcomeRefused, true, ErrTimeout
		case <-s.cfg.Drain.Started():
			p.close()
			s.stop()
			return OutcomeStopped, true, nil
		case <-ctx.Done():
			p.close()
			s.stop()
			return OutcomeStopped, true, ctx.Err()
		}
	}
}

// giveUp ends a handover the holder refused or that broke the protocol:
// the process closes its sockets and exits 1. It returns err.
func (s *Successor) giveUp(ctx context.Context, err error) error {
	s.close()
	s.log.WarnContext(ctx, "handover refused: exiting", catalog.KeyError, err.Error())
	return err
}

// step is where the gate goes after a reload.
type step uint8

const (
	// stepContinue: ready sent again; the gate reads the answer.
	stepContinue step = iota
	// stepRetry: the connection closed unanswered; retry.
	stepRetry
	// stepDone: the handover ended.
	stepDone
)

// reload reloads the holder's newer candidate, unless it is active
// already, and sends ready again. A failed reload ends the handover, as
// does the Drain, the gate's end or ctx.
func (s *Successor) reload(ctx context.Context, p *peer, digest string, deadline time.Time) (Outcome, step, error) {
	d, err := revision.Parse(digest)
	if err != nil {
		p.close()
		return OutcomeRefused, stepDone, s.giveUp(ctx, fmt.Errorf("%w: reload digest: %w", ErrProtocol, err))
	}
	s.log.InfoContext(ctx, "handover reloading the holder's candidate", "digest", d.String())
	if s.cfg.Active() != d {
		rctx, stop := s.bounded(ctx, deadline)
		err := s.cfg.Reload(rctx, d)
		stop()
		if err != nil {
			p.close()
			out, err := s.reloadFailed(ctx, d, deadline, err)
			return out, stepDone, err
		}
	}
	if err := s.sendReady(ctx, p); err != nil {
		p.close()
		s.log.DebugContext(ctx, "handover connection closed unanswered", catalog.KeyError, err.Error())
		return 0, stepRetry, nil
	}
	return 0, stepContinue, nil
}

// reloadFailed ends the handover after the reload of d failed with
// cause: stopped when the Drain or ctx ended it, timed out when the gate
// ended, else refused.
func (s *Successor) reloadFailed(ctx context.Context, d revision.Digest, deadline time.Time, cause error) (Outcome, error) {
	switch {
	case s.draining():
		s.stop()
		return OutcomeStopped, nil
	case ctx.Err() != nil:
		s.stop()
		return OutcomeStopped, ctx.Err()
	case !s.clk.Now().Before(deadline):
		s.close()
		s.log.WarnContext(ctx, "handover not accepted in time: exiting")
		return OutcomeRefused, ErrTimeout
	default:
		return OutcomeRefused, s.giveUp(ctx, fmt.Errorf("handover: reload %s: %w", d, cause))
	}
}

// accepted lowers the ceilings by the holder's usage, listens and sends
// listening, then waits for released. A Drain that starts before the
// sockets listen closes them.
func (s *Successor) accepted(ctx context.Context, p *peer, u Usage, deadline time.Time) (Outcome, error) {
	s.log.InfoContext(ctx, "handover accepted")
	s.apply(ctx, u)
	if s.draining() {
		p.close()
		s.ceil.restore()
		s.stop()
		return OutcomeStopped, nil
	}
	if err := s.cfg.Bound.Listen(ctx); err != nil {
		p.close()
		s.ceil.restore()
		return OutcomeRefused, s.giveUp(ctx, fmt.Errorf("handover: listen: %w", err))
	}
	s.listening = true
	if s.draining() {
		// The holder never gets listening, so it aborts and keeps every
		// connection; the Drain stops the sockets.
		p.close()
		s.ceil.restore()
		return OutcomeStopped, nil
	}
	if err := p.send(ctx, bareWire{Type: TypeListening}); err != nil {
		s.log.WarnContext(ctx, "handover: listening not delivered", catalog.KeyError, err.Error())
		return s.lost(ctx, p, deadline, false)
	}
	s.log.InfoContext(ctx, "handover listening: the holder steers to this process")
	return s.awaitRelease(ctx, p, deadline)
}

// awaitRelease applies usage reports until released, then takes the lock.
// A Drain that starts meanwhile closes the connection, so a holder still
// lingering re-pins and keeps serving.
func (s *Successor) awaitRelease(ctx context.Context, p *peer, deadline time.Time) (Outcome, error) {
	for {
		select {
		case l := <-p.in:
			if l.err != nil {
				return s.lost(ctx, p, deadline, true)
			}
			m, err := decode(l.b)
			if err != nil {
				return s.lost(ctx, p, deadline, true)
			}
			switch m.Type {
			case TypeUsage:
				s.apply(ctx, m.Usage)
			case TypeReleased:
				return s.released(ctx, p)
			default:
				s.log.WarnContext(ctx, "handover: a message other than usage or released", "type", m.Type)
				return s.lost(ctx, p, deadline, true)
			}
		case <-s.cfg.Drain.Started():
			p.close()
			return OutcomeStopped, nil
		case <-ctx.Done():
			p.close()
			return OutcomeStopped, ctx.Err()
		}
	}
}

// released takes the lock the holder released, retrying every
// RetryInterval while another process holds it, then follows the holder's
// usage reports.
func (s *Successor) released(ctx context.Context, p *peer) (Outcome, error) {
	s.log.InfoContext(ctx, "handover: the holder released the lock")
	warned := false
	for {
		nl, err := s.cfg.Dir.TryLock()
		if err == nil {
			out, err := s.becomeHolder(ctx, nl)
			if out == OutcomeHolder {
				s.follow(ctx, p)
			} else {
				p.close()
			}
			return out, err
		}
		if !warned {
			warned = true
			s.log.WarnContext(ctx, "handover: the lock is still held, retrying", catalog.KeyError, err.Error())
		}
		t := s.clk.NewTimer(RetryInterval)
		select {
		case <-t.C():
		case <-s.cfg.Drain.Started():
			t.Stop()
			p.close()
			return OutcomeStopped, nil
		case <-ctx.Done():
			t.Stop()
			p.close()
			return OutcomeStopped, ctx.Err()
		}
	}
}

// lost handles a connection that ended after accepted, before released:
// it takes the lock if the holder exited, retrying every RetryInterval
// until the gate's end and at least once, else the holder kept the lock.
// A process whose listening reached the holder may hold steered
// connections, so it Drains (OutcomeLost); one whose listening did not
// closes its sockets (OutcomeRefused).
func (s *Successor) lost(ctx context.Context, p *peer, deadline time.Time, announced bool) (Outcome, error) {
	p.close()
	until := deadline
	if first := s.clk.Now().Add(RetryInterval); until.Before(first) {
		until = first
	}
	for {
		if nl := s.tryLock(ctx); nl != nil {
			s.log.InfoContext(ctx, "handover: the holder is gone, taking over")
			s.ceil.restore()
			return s.becomeHolder(ctx, nl)
		}
		now := s.clk.Now()
		if !now.Before(until) {
			break
		}
		if out, done, err := s.wait(ctx, min(RetryInterval, until.Sub(now))); done {
			return out, err
		}
	}
	s.ceil.restore()
	if announced {
		s.log.WarnContext(ctx, "handover lost: the holder kept the lock, draining")
		return OutcomeLost, ErrLost
	}
	s.close()
	s.log.WarnContext(ctx, "handover lost before listening reached the holder: exiting")
	return OutcomeRefused, ErrLost
}

// becomeHolder makes the process the lock holder: it records nl in the
// Lock, listens if it does not yet, pins its groups, writes holder.json
// and sends MAINPID=, in that order. A Drain that released the Lock first
// keeps the process from holding the lock.
func (s *Successor) becomeHolder(ctx context.Context, nl *nodedir.Lock) (Outcome, error) {
	if !s.cfg.Lock.Take(nl) {
		s.stop()
		return OutcomeStopped, nil
	}
	var errs []error
	if !s.listening {
		if err := s.cfg.Bound.Listen(ctx); err != nil {
			errs = append(errs, fmt.Errorf("handover: listen: %w", err))
		} else {
			s.listening = true
		}
	}
	for _, pn := range s.cfg.Pinners {
		if err := pn.Pin(); err != nil && !isUnsupported(err) {
			s.log.WarnContext(ctx, "handover pin failed", catalog.KeyError, err.Error())
		}
	}
	if err := nl.WriteHolder(s.cfg.Record); err != nil {
		s.log.ErrorContext(ctx, "handover: holder.json not written", catalog.KeyError, err.Error())
	}
	s.mainPID(ctx)
	s.log.InfoContext(ctx, "handover took the lock", "pid", s.pid)
	return OutcomeHolder, errors.Join(errs...)
}

// mainPID sends MAINPID= until it is delivered, the Drain starts or ctx
// ends, retrying every RetryInterval: without it the service manager
// would take the previous holder's exit for the unit's.
func (s *Successor) mainPID(ctx context.Context) {
	if s.cfg.Notifier == nil {
		return
	}
	warned := false
	for {
		err := s.cfg.Notifier.MainPID(ctx, s.pid)
		if err == nil {
			return
		}
		if !warned {
			warned = true
			s.log.WarnContext(ctx, "handover: MAINPID= not delivered, retrying", catalog.KeyError, err.Error())
		}
		t := s.clk.NewTimer(RetryInterval)
		select {
		case <-t.C():
		case <-s.cfg.Drain.Started():
			t.Stop()
			return
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

// follow applies the previous holder's usage reports on p until it is
// gone, then restores the ceilings; Close or the end of ctx stops it.
func (s *Successor) follow(ctx context.Context, p *peer) {
	fctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		cancel()
		p.close()
		s.ceil.restore()
		return
	}
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer s.ceil.restore()
		defer p.close()
		for {
			select {
			case l := <-p.in:
				if l.err != nil {
					s.log.InfoContext(fctx, "handover: the previous holder is gone, ceilings restored")
					return
				}
				if m, err := decode(l.b); err == nil && m.Type == TypeUsage {
					s.apply(fctx, m.Usage)
				}
			case <-fctx.Done():
				return
			}
		}
	}()
}

// apply lowers the ceilings by u, logging an unreadable memory.max once.
func (s *Successor) apply(ctx context.Context, u Usage) {
	if err := s.ceil.apply(u); err != nil {
		s.usageWarn.Do(func() {
			s.log.WarnContext(ctx, "handover: memory.max unreadable", catalog.KeyError, err.Error())
		})
	}
}

// stop ends the handover for a Drain or ctx: a process not listening yet
// closes its sockets, so its Drain skips to the flush; a listening one
// leaves them to its Drain.
func (s *Successor) stop() {
	if !s.listening {
		s.close()
	}
}

// close closes every bound or listening socket, once.
func (s *Successor) close() {
	s.abandon.Do(func() {
		if err := s.cfg.Bound.Close(); err != nil {
			s.log.Warn("handover: closing the bound sockets failed", catalog.KeyError, err.Error())
		}
	})
}

// draining reports whether the process's Drain started.
func (s *Successor) draining() bool {
	select {
	case <-s.cfg.Drain.Started():
		return true
	default:
		return false
	}
}

// bounded returns a context that ends with parent, when the Drain
// starts, or at deadline on the Successor's clock; stop releases it.
func (s *Successor) bounded(parent context.Context, deadline time.Time) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	t := s.clk.AfterFunc(deadline.Sub(s.clk.Now()), cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-s.cfg.Drain.Started():
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
