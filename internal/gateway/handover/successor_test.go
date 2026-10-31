// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package handover

import (
	"context"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// successorPID is the new process's PID in the successor rig.
const successorPID = 4343

// ownLimit is the new process's own soft memory limit (GOMEMLIMIT) in
// the successor rig, which has no cgroup limit.
const ownLimit = 8 << 30

// successorRig is a new process's handover with fake parts, beside a
// holder the test plays: it holds the data-directory lock and answers on
// handover.sock.
type successorRig struct {
	t          *testing.T
	clk        *clocktest.Fake
	dir        *nodedir.Dir
	ev         *events
	holderLock *nodedir.Lock
	drain      *fakeDrainer
	bound      *fakeBound
	notifier   *fakeNotifier
	conns      *fakeCeiling
	units      *fakeCeiling
	budget     *fakeCeiling
	mem        *fakeMemory
	lock       *Lock
	logs       *logBuffer
	s          *Successor
	ln         net.Listener

	mu        sync.Mutex
	active    revision.Digest
	reloads   []revision.Digest
	reloadErr error
}

// runResult is what Run returned.
type runResult struct {
	out Outcome
	err error
}

// newSuccessorRig returns a new process whose active Revision is digestA,
// while the test holds the lock; mod changes the rig or the
// configuration first.
func newSuccessorRig(t *testing.T, mod ...func(*successorRig, *SuccessorConfig)) *successorRig {
	t.Helper()
	r := &successorRig{
		t: t, clk: newClock(), dir: dataDir(t), ev: &events{}, lock: NewLock(), logs: &logBuffer{},
		conns: &fakeCeiling{}, units: &fakeCeiling{}, budget: &fakeCeiling{},
		mem:    &fakeMemory{limit: ownLimit},
		active: digestA,
	}
	r.holderLock = takeLock(t, r.dir)
	r.drain = newFakeDrainer(r.ev)
	r.bound = &fakeBound{ev: r.ev}
	r.notifier = &fakeNotifier{ev: r.ev}
	cfg := SuccessorConfig{
		Dir:      r.dir,
		Lock:     r.lock,
		Drain:    r.drain,
		Start:    r.clk.Now(),
		Version:  "0.2.0-test",
		PID:      successorPID,
		Active:   r.activeDigest,
		Reload:   r.reload,
		Bound:    r.bound,
		Pinners:  []Pinner{&fakePinner{ev: r.ev, name: "web"}, &fakePinner{ev: r.ev, name: "adminport"}},
		Record:   testRecord(t, successorPID),
		Notifier: r.notifier,
		Ceilings: Ceilings{Conns: r.conns, Units: r.units, Budget: r.budget, SetMemoryLimit: r.mem.SetMemoryLimit, MemoryMax: r.mem.MemoryMax},
		Clock:    r.clk,
		Logger:   newLogger(r.logs),
	}
	for _, m := range mod {
		m(r, &cfg)
	}
	s, err := NewSuccessor(cfg)
	if err != nil {
		t.Fatalf("NewSuccessor: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r.s = s
	return r
}

func (r *successorRig) activeDigest() revision.Digest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

func (r *successorRig) reload(_ context.Context, d revision.Digest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reloads = append(r.reloads, d)
	r.ev.add("reload")
	if r.reloadErr != nil {
		return r.reloadErr
	}
	r.active = d
	return nil
}

// listenHolder serves handover.sock as the holder.
func (r *successorRig) listenHolder() {
	r.t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(r.t.Context(), "unix", r.dir.HandoverSocketPath())
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = ln.Close() })
	r.ln = ln
}

// conn accepts the new process's next connection.
func (r *successorRig) conn() *wire {
	r.t.Helper()
	type accepted struct {
		c   net.Conn
		err error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := r.ln.Accept()
		ch <- accepted{c, err}
	}()
	a := recv(r.t, "the new process's connection", ch)
	if a.err != nil {
		r.t.Fatal(a.err)
	}
	return newWire(r.t, a.c)
}

// run starts Run.
func (r *successorRig) run() <-chan runResult {
	ch := make(chan runResult, 1)
	go func() {
		out, err := r.s.Run(r.t.Context())
		ch <- runResult{out, err}
	}()
	return ch
}

// releaseHolderLock releases the test holder's lock, as its Drain or
// exit does.
func (r *successorRig) releaseHolderLock() {
	r.t.Helper()
	if err := r.holderLock.Release(); err != nil {
		r.t.Fatal(err)
	}
}

// gateToListening runs the new process to listening: it connects, sends
// ready and gets accepted with u.
func (r *successorRig) gateToListening(u Usage) (*wire, <-chan runResult) {
	r.t.Helper()
	r.listenHolder()
	done := r.run()
	w := r.conn()
	w.expect(TypeReady)
	w.send(acceptedWire{Type: TypeAccepted, Usage: u})
	w.expect(TypeListening)
	return w, done
}

// stepUntil advances the clock by RetryInterval, each time once the new
// process waits, until Run returns.
func (r *successorRig) stepUntil(done <-chan runResult) runResult {
	r.t.Helper()
	deadline := time.After(bound)
	for {
		select {
		case res := <-done:
			return res
		case <-deadline:
			r.t.Fatal("timed out waiting for Run")
		default:
		}
		if r.clk.Pending() > 0 {
			r.clk.Advance(RetryInterval)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestNewSuccessorConfig checks that NewSuccessor refuses a configuration
// without its parts or with a socket path a Unix socket address cannot
// hold, and that Run runs once.
func TestNewSuccessorConfig(t *testing.T) {
	ev := &events{}
	ok := func(t *testing.T) SuccessorConfig {
		t.Helper()
		return SuccessorConfig{
			Dir: dataDir(t), Lock: NewLock(), Drain: newFakeDrainer(ev), Bound: &fakeBound{ev: ev},
			Active: func() revision.Digest { return revision.Digest{} },
			Reload: func(context.Context, revision.Digest) error { return nil },
		}
	}
	for name, tt := range map[string]struct {
		mod  func(t *testing.T, c *SuccessorConfig)
		want error
	}{
		"no dir":    {mod: func(_ *testing.T, c *SuccessorConfig) { c.Dir = nil }, want: ErrConfig},
		"no lock":   {mod: func(_ *testing.T, c *SuccessorConfig) { c.Lock = nil }, want: ErrConfig},
		"no drain":  {mod: func(_ *testing.T, c *SuccessorConfig) { c.Drain = nil }, want: ErrConfig},
		"no active": {mod: func(_ *testing.T, c *SuccessorConfig) { c.Active = nil }, want: ErrConfig},
		"no reload": {mod: func(_ *testing.T, c *SuccessorConfig) { c.Reload = nil }, want: ErrConfig},
		"no bound":  {mod: func(_ *testing.T, c *SuccessorConfig) { c.Bound = nil }, want: ErrConfig},
		"nil pin":   {mod: func(_ *testing.T, c *SuccessorConfig) { c.Pinners = []Pinner{nil} }, want: ErrConfig},
		"socket path too long": {mod: func(t *testing.T, c *SuccessorConfig) {
			long, err := nodedir.Open(filepath.Join(c.Dir.Root(), strings.Repeat("d", 100)))
			if err != nil {
				t.Fatal(err)
			}
			c.Dir = long
		}, want: ErrSocketPath},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := ok(t)
			tt.mod(t, &cfg)
			if _, err := NewSuccessor(cfg); !errors.Is(err, tt.want) {
				t.Fatalf("NewSuccessor = %v, want %v", err, tt.want)
			}
		})
	}
	r := newSuccessorRig(t)
	r.releaseHolderLock()
	if res := recv(t, "Run", r.run()); res.out != OutcomeHolder {
		t.Fatalf("Run = %v, %v, want holder", res.out, res.err)
	}
	if _, err := r.s.Run(t.Context()); !errors.Is(err, ErrConfig) {
		t.Errorf("second Run = %v, want ErrConfig", err)
	}
}

// TestSuccessorSendsReady checks the ready message: the version, the PID,
// every marker and the active Revision, null without one.
func TestSuccessorSendsReady(t *testing.T) {
	for _, tt := range []struct {
		name   string
		active revision.Digest
	}{
		{name: "active Revision", active: digestA},
		{name: "no active Revision"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newSuccessorRig(t, func(r *successorRig, _ *SuccessorConfig) { r.active = tt.active })
			r.listenHolder()
			r.run()
			m := r.conn().expect(TypeReady)
			if m.Version != "0.2.0-test" || m.PID != successorPID || !maps.Equal(m.Markers, Markers()) {
				t.Errorf("ready %+v, want version, PID and markers", m)
			}
			switch {
			case tt.active.IsZero() && m.ActiveDigest != nil:
				t.Errorf("activeDigest %q, want null", *m.ActiveDigest)
			case !tt.active.IsZero() && (m.ActiveDigest == nil || *m.ActiveDigest != tt.active.String()):
				t.Errorf("activeDigest %v, want %s", m.ActiveDigest, tt.active)
			}
		})
	}
}

// TestSuccessorRefused checks a refused handover: the new process closes
// its sockets without listening and Run reports the reason.
func TestSuccessorRefused(t *testing.T) {
	for _, reason := range []string{ReasonUnknownMarker, ReasonOlderCandidate, ReasonDraining, ReasonBusy} {
		t.Run(reason, func(t *testing.T) {
			r := newSuccessorRig(t)
			r.listenHolder()
			done := r.run()
			w := r.conn()
			w.expect(TypeReady)
			w.send(refusedWire{Type: TypeRefused, Reason: reason})
			res := recv(t, "Run", done)
			var refused *RefusedError
			if res.out != OutcomeRefused || !errors.As(res.err, &refused) || refused.Reason != reason {
				t.Fatalf("Run = %v, %v, want refused with %q", res.out, res.err, reason)
			}
			if want := []string{"bound:close"}; !slices.Equal(r.ev.all(), want) {
				t.Errorf("events %v, want %v", r.ev.all(), want)
			}
		})
	}
}

// TestSuccessorReloads checks reload: the new process reloads the
// holder's candidate and sends ready again with it, and a reload of its
// active Revision sends ready again without reloading.
func TestSuccessorReloads(t *testing.T) {
	r := newSuccessorRig(t)
	r.listenHolder()
	done := r.run()
	w := r.conn()
	w.expect(TypeReady)
	w.send(reloadWire{Type: TypeReload, Digest: digestB})
	if m := w.expect(TypeReady); m.ActiveDigest == nil || *m.ActiveDigest != digestB.String() {
		t.Fatalf("ready after the reload: activeDigest %v, want %s", m.ActiveDigest, digestB)
	}
	w.send(reloadWire{Type: TypeReload, Digest: digestB})
	w.expect(TypeReady)
	r.mu.Lock()
	reloads := slices.Clone(r.reloads)
	r.mu.Unlock()
	if !slices.Equal(reloads, []revision.Digest{digestB}) {
		t.Errorf("reloads %v, want one of %s", reloads, digestB)
	}
	w.send(refusedWire{Type: TypeRefused, Reason: ReasonBusy})
	recv(t, "Run", done)
}

// TestSuccessorReloadFailure checks that a reload that fails ends the
// handover: refused, sockets closed.
func TestSuccessorReloadFailure(t *testing.T) {
	boom := errors.New("candidate does not validate")
	r := newSuccessorRig(t, func(r *successorRig, _ *SuccessorConfig) { r.reloadErr = boom })
	r.listenHolder()
	done := r.run()
	w := r.conn()
	w.expect(TypeReady)
	w.send(reloadWire{Type: TypeReload, Digest: digestB})
	res := recv(t, "Run", done)
	if res.out != OutcomeRefused || !errors.Is(res.err, boom) {
		t.Fatalf("Run = %v, %v, want refused with the reload error", res.out, res.err)
	}
	if want := []string{"reload", "bound:close"}; !slices.Equal(r.ev.all(), want) {
		t.Errorf("events %v, want %v", r.ev.all(), want)
	}
}

// TestSuccessorRetriesUntilTheSocketAnswers checks the gate's retry: with
// the socket missing the new process retries every RetryInterval and
// connects once the socket answers.
func TestSuccessorRetriesUntilTheSocketAnswers(t *testing.T) {
	r := newSuccessorRig(t)
	done := r.run()
	for range 3 {
		waitArmed(t, r.clk, 1)
		r.clk.Advance(RetryInterval)
	}
	waitArmed(t, r.clk, 1)
	r.listenHolder()
	r.clk.Advance(RetryInterval)
	w := r.conn()
	w.expect(TypeReady)
	w.send(refusedWire{Type: TypeRefused, Reason: ReasonBusy})
	recv(t, "Run", done)
}

// TestSuccessorTimesOut checks the gate's bound: a new process not
// accepted GateTimeout after its start, its socket missing or its ready
// unanswered, closes its sockets and exits 1.
func TestSuccessorTimesOut(t *testing.T) {
	t.Run("unanswered", func(t *testing.T) {
		r := newSuccessorRig(t)
		r.listenHolder()
		done := r.run()
		r.conn().expect(TypeReady)
		waitArmed(t, r.clk, 1)
		r.clk.Advance(GateTimeout - time.Nanosecond)
		select {
		case res := <-done:
			t.Fatalf("Run returned %v before the gate ended", res.out)
		default:
		}
		r.clk.Advance(time.Nanosecond)
		res := recv(t, "Run", done)
		if res.out != OutcomeRefused || !errors.Is(res.err, ErrTimeout) {
			t.Fatalf("Run = %v, %v, want refused with ErrTimeout", res.out, res.err)
		}
		if want := []string{"bound:close"}; !slices.Equal(r.ev.all(), want) {
			t.Errorf("events %v, want %v", r.ev.all(), want)
		}
	})
	t.Run("missing socket", func(t *testing.T) {
		r := newSuccessorRig(t)
		res := r.stepUntil(r.run())
		if res.out != OutcomeRefused || !errors.Is(res.err, ErrTimeout) {
			t.Fatalf("Run = %v, %v, want refused with ErrTimeout", res.out, res.err)
		}
		if got := r.clk.Since(r.s.start); got != GateTimeout {
			t.Errorf("gave up %v after the start, want %v", got, GateTimeout)
		}
	})
}

// TestSuccessorTakesOverWhenTheHolderExits checks a holder that exits
// first: the new process takes the lock, listens, pins its groups, writes
// holder.json naming it and sends MAINPID=, in that order.
func TestSuccessorTakesOverWhenTheHolderExits(t *testing.T) {
	r := newSuccessorRig(t)
	r.releaseHolderLock()
	res := recv(t, "Run", r.run())
	if res.out != OutcomeHolder || res.err != nil {
		t.Fatalf("Run = %v, %v, want holder", res.out, res.err)
	}
	want := []string{"bound:listen", "web:pin", "adminport:pin", "notify:mainpid 4343"}
	if got := r.ev.all(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
	h, err := r.dir.ReadHolder()
	if err != nil || h.PID != successorPID {
		t.Errorf("holder.json = %+v, %v, want PID %d", h, err, successorPID)
	}
	if r.lock.Held() == nil {
		t.Error("the Lock does not record the lock taken")
	}
}

// TestSuccessorHandover checks a handover from the new process's side:
// accepted lowers its ceilings by the holder's usage and sets its soft
// memory limit to GOMEMLIMIT minus the holder's resident set, it listens
// and sends listening, applies each usage report, and on released takes
// the lock: listens no more, pins, writes holder.json and sends MAINPID=.
// A follower applies the holder's later reports and, once the holder is
// gone, restores every ceiling and the own memory limit.
func TestSuccessorHandover(t *testing.T) {
	r := newSuccessorRig(t)
	w, done := r.gateToListening(Usage{Connections: 3, InflightUnits: 4, BufferedBytes: 5, RSSBytes: 1 << 30})
	if r.conns.last() != 3 || r.units.last() != 4 || r.budget.last() != 5 {
		t.Errorf("ceilings lowered by %d, %d, %d, want 3, 4, 5", r.conns.last(), r.units.last(), r.budget.last())
	}
	if got := r.mem.current(); got != ownLimit-1<<30 {
		t.Errorf("memory limit %d, want %d", got, ownLimit-1<<30)
	}
	w.send(usageWire{Type: TypeUsage, Usage: Usage{Connections: 2, RSSBytes: 2 << 30}})
	waitFor(t, "the usage report applied", func() bool { return r.conns.last() == 2 })
	r.releaseHolderLock()
	w.send(bareWire{Type: TypeReleased})
	res := recv(t, "Run", done)
	if res.out != OutcomeHolder || res.err != nil {
		t.Fatalf("Run = %v, %v, want holder", res.out, res.err)
	}
	want := []string{"bound:listen", "web:pin", "adminport:pin", "notify:mainpid 4343"}
	if got := r.ev.all(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
	if h, err := r.dir.ReadHolder(); err != nil || h.PID != successorPID {
		t.Errorf("holder.json = %+v, %v, want PID %d", h, err, successorPID)
	}
	w.send(usageWire{Type: TypeUsage, Usage: Usage{Connections: 1, RSSBytes: 3 << 30}})
	waitFor(t, "the follower's report applied", func() bool { return r.conns.last() == 1 })
	if got := r.mem.current(); got != ownLimit-3<<30 {
		t.Errorf("memory limit %d, want %d", got, ownLimit-3<<30)
	}
	_ = w.c.Close()
	waitFor(t, "the ceilings restored", func() bool { return r.conns.last() == 0 && r.mem.current() == ownLimit })
	if r.units.last() != 0 || r.budget.last() != 0 {
		t.Errorf("units %d, budget %d after the holder left, want 0", r.units.last(), r.budget.last())
	}
}

// TestSuccessorRetriesTheLockAfterReleased checks that a lock another
// process still holds after released is retried every RetryInterval.
func TestSuccessorRetriesTheLockAfterReleased(t *testing.T) {
	r := newSuccessorRig(t)
	w, done := r.gateToListening(Usage{})
	w.send(bareWire{Type: TypeReleased})
	waitArmed(t, r.clk, 1)
	r.clk.Advance(RetryInterval)
	waitArmed(t, r.clk, 1)
	r.releaseHolderLock()
	r.clk.Advance(RetryInterval)
	if res := recv(t, "Run", done); res.out != OutcomeHolder {
		t.Fatalf("Run = %v, %v, want holder", res.out, res.err)
	}
	if !r.logs.has("the lock is still held, retrying") {
		t.Error("no record of the retried lock")
	}
}

// TestSuccessorConnectionEndsAfterListening checks a connection that ends
// after listening, before released: a holder that crashed frees the lock
// and the new process takes it; a holder that keeps the lock leaves the
// new process lost once the gate ends, to Drain, its ceilings restored and
// its sockets left to the Drain.
func TestSuccessorConnectionEndsAfterListening(t *testing.T) {
	t.Run("holder crashed", func(t *testing.T) {
		r := newSuccessorRig(t)
		w, done := r.gateToListening(Usage{Connections: 3})
		r.releaseHolderLock()
		_ = w.c.Close()
		if res := recv(t, "Run", done); res.out != OutcomeHolder {
			t.Fatalf("Run = %v, %v, want holder", res.out, res.err)
		}
		if n := r.ev.count("bound:listen"); n != 1 {
			t.Errorf("listened %d times, want once", n)
		}
		if r.conns.last() != 0 {
			t.Errorf("connections external %d after the holder crashed, want 0", r.conns.last())
		}
	})
	t.Run("holder kept the lock", func(t *testing.T) {
		r := newSuccessorRig(t)
		w, done := r.gateToListening(Usage{Connections: 3})
		_ = w.c.Close()
		res := r.stepUntil(done)
		if res.out != OutcomeLost || !errors.Is(res.err, ErrLost) {
			t.Fatalf("Run = %v, %v, want lost", res.out, res.err)
		}
		if r.ev.count("bound:close") != 0 {
			t.Error("a lost new process closed its sockets instead of leaving them to its Drain")
		}
		if r.conns.last() != 0 {
			t.Errorf("connections external %d, want 0", r.conns.last())
		}
		if got := r.clk.Since(r.s.start); got < GateTimeout {
			t.Errorf("gave up %v after the start, want the gate's %v", got, GateTimeout)
		}
	})
}

// TestSuccessorLostBeforeListeningReached checks a connection that ends
// before listening reaches the holder: with the lock kept, the new process
// closes its sockets and exits 1.
func TestSuccessorLostBeforeListeningReached(t *testing.T) {
	gate := make(chan struct{})
	r := newSuccessorRig(t, func(r *successorRig, _ *SuccessorConfig) { r.bound.gate = gate })
	r.listenHolder()
	done := r.run()
	w := r.conn()
	w.expect(TypeReady)
	w.send(acceptedWire{Type: TypeAccepted})
	_ = w.c.Close()
	close(gate)
	res := r.stepUntil(done)
	if res.out != OutcomeRefused || !errors.Is(res.err, ErrLost) {
		t.Fatalf("Run = %v, %v, want refused with ErrLost", res.out, res.err)
	}
	if want := []string{"bound:listen", "bound:close"}; !slices.Equal(r.ev.all(), want) {
		t.Errorf("events %v, want %v", r.ev.all(), want)
	}
}

// TestSuccessorStops checks a Drain during the handover: before the new
// process listens it closes its sockets; once listening it leaves them to
// its Drain and closes the connection, so a lingering holder re-pins.
func TestSuccessorStops(t *testing.T) {
	t.Run("during the gate", func(t *testing.T) {
		r := newSuccessorRig(t)
		r.listenHolder()
		done := r.run()
		w := r.conn()
		w.expect(TypeReady)
		r.drain.start()
		if res := recv(t, "Run", done); res.out != OutcomeStopped || res.err != nil {
			t.Fatalf("Run = %v, %v, want stopped", res.out, res.err)
		}
		if want := []string{"drain:start", "bound:close"}; !slices.Equal(r.ev.all(), want) {
			t.Errorf("events %v, want %v", r.ev.all(), want)
		}
		if !w.closed() {
			t.Error("the connection stayed open")
		}
	})
	t.Run("at accepted", func(t *testing.T) {
		r := newSuccessorRig(t)
		r.listenHolder()
		done := r.run()
		w := r.conn()
		w.expect(TypeReady)
		r.drain.start()
		if b, err := encode(acceptedWire{Type: TypeAccepted}); err == nil {
			_, _ = w.c.Write(b) // the new process may have closed already
		}
		if res := recv(t, "Run", done); res.out != OutcomeStopped {
			t.Fatalf("Run = %v, %v, want stopped", res.out, res.err)
		}
		if r.ev.count("bound:listen") != 0 || r.ev.count("bound:close") != 1 {
			t.Errorf("events %v, want the sockets closed without listening", r.ev.all())
		}
	})
	t.Run("after listening", func(t *testing.T) {
		r := newSuccessorRig(t)
		w, done := r.gateToListening(Usage{})
		r.drain.start()
		if res := recv(t, "Run", done); res.out != OutcomeStopped {
			t.Fatalf("Run = %v, %v, want stopped", res.out, res.err)
		}
		if r.ev.count("bound:close") != 0 {
			t.Error("a listening new process closed its sockets instead of leaving them to its Drain")
		}
		if !w.closed() {
			t.Error("the connection stayed open")
		}
	})
}

// TestSuccessorRetriesMainPID checks that MAINPID= is retried every
// RetryInterval until delivered.
func TestSuccessorRetriesMainPID(t *testing.T) {
	r := newSuccessorRig(t, func(r *successorRig, _ *SuccessorConfig) { r.notifier.fails = 2 })
	r.releaseHolderLock()
	done := r.run()
	for range 2 {
		waitArmed(t, r.clk, 1)
		r.clk.Advance(RetryInterval)
	}
	if res := recv(t, "Run", done); res.out != OutcomeHolder {
		t.Fatalf("Run = %v, %v, want holder", res.out, res.err)
	}
	if got := r.ev.all()[3:]; !slices.Equal(got, []string{"notify:failed", "notify:failed", "notify:mainpid 4343"}) {
		t.Errorf("notifications %v, want two failures and MAINPID=", got)
	}
}

// TestSuccessorRefusesAHolderOfAnotherUser checks the SO_PEERCRED check
// of the holder: a socket served by another user gets no ready message.
func TestSuccessorRefusesAHolderOfAnotherUser(t *testing.T) {
	r := newSuccessorRig(t, func(_ *successorRig, c *SuccessorConfig) {
		c.euid = func() int64 { return int64(os.Geteuid()) + 1 }
	})
	r.listenHolder()
	done := r.run()
	if !r.conn().closed() {
		t.Fatal("the new process spoke to a holder of another user")
	}
	waitFor(t, "the refusal record", func() bool { return r.logs.has("handover socket refused", "peer runs as another user") })
	r.drain.start()
	recv(t, "Run", done)
}

// TestSuccessorAfterTheDrainReleasedTheLock checks that a new process
// whose Drain released its Lock first never holds the lock it takes.
func TestSuccessorAfterTheDrainReleasedTheLock(t *testing.T) {
	r := newSuccessorRig(t)
	if err := r.lock.Release(); err != nil { // the Drain's start
		t.Fatal(err)
	}
	r.releaseHolderLock()
	if res := recv(t, "Run", r.run()); res.out != OutcomeStopped {
		t.Fatalf("Run = %v, %v, want stopped", res.out, res.err)
	}
	if _, err := r.dir.TryLock(); err != nil {
		t.Errorf("TryLock after the refused take = %v, want the lock free", err)
	}
	if want := []string{"bound:close"}; !slices.Equal(r.ev.all(), want) {
		t.Errorf("events %v, want %v", r.ev.all(), want)
	}
}

// TestSuccessorProtocolErrors checks that a holder breaking the protocol
// before accepted ends the handover as a refusal.
func TestSuccessorProtocolErrors(t *testing.T) {
	for name, line := range map[string]string{
		"not JSON":       `refused`,
		"unexpected":     `{"type":"listening"}`,
		"unknown type":   `{"type":"steered"}`,
		"reload digest":  `{"type":"reload","digest":"sha256:zz"}`,
		"line too long":  `{"type":"refused","reason":"` + strings.Repeat("x", MaxMessage) + `"}`,
		"usage too soon": `{"type":"usage","connections":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newSuccessorRig(t)
			r.listenHolder()
			done := r.run()
			w := r.conn()
			w.expect(TypeReady)
			w.sendRaw(line)
			res := recv(t, "Run", done)
			if res.out != OutcomeRefused || !errors.Is(res.err, ErrProtocol) {
				t.Fatalf("Run = %v, %v, want refused with ErrProtocol", res.out, res.err)
			}
		})
	}
}

// TestSuccessorCloseStopsTheFollower checks that Close stops the follower
// and restores the ceilings while the previous holder still runs.
func TestSuccessorCloseStopsTheFollower(t *testing.T) {
	r := newSuccessorRig(t)
	w, done := r.gateToListening(Usage{Connections: 3, RSSBytes: 1 << 30})
	r.releaseHolderLock()
	w.send(bareWire{Type: TypeReleased})
	if res := recv(t, "Run", done); res.out != OutcomeHolder {
		t.Fatalf("Run = %v, %v, want holder", res.out, res.err)
	}
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	if r.conns.last() != 0 || r.mem.current() != ownLimit {
		t.Errorf("after Close: connections external %d, memory limit %d, want 0 and %d", r.conns.last(), r.mem.current(), ownLimit)
	}
	if !w.closed() {
		t.Error("the connection stayed open after Close")
	}
}
