// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package handover

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// holderUsage is the usage the holder rig reports.
var holderUsage = Usage{Connections: 3, InflightUnits: 4, BufferedBytes: 5, RSSBytes: 7}

// holderRig is a holder serving its socket with fake parts.
type holderRig struct {
	t     *testing.T
	clk   *clocktest.Fake
	dir   *nodedir.Dir
	lock  *Lock
	ev    *events
	drain *fakeDrainer
	acts  *fakeActivations
	web   *fakeSockets
	admin *fakeSockets
	logs  *logBuffer
	h     *Holder
}

// newHolderRig starts a holder whose active Revision and candidate are
// digestA; mod changes the rig or the configuration first.
func newHolderRig(t *testing.T, mod ...func(*holderRig, *HolderConfig)) *holderRig {
	t.Helper()
	r := &holderRig{t: t, clk: newClock(), dir: dataDir(t), lock: NewLock(), ev: &events{}, logs: &logBuffer{}}
	r.drain = newFakeDrainer(r.ev)
	r.acts = &fakeActivations{ev: r.ev, revs: Revisions{Active: digestA, Candidate: digestA}}
	r.web = &fakeSockets{ev: r.ev, name: "web"}
	r.admin = &fakeSockets{ev: r.ev, name: "adminport"}
	if !r.lock.Take(takeLock(t, r.dir)) {
		t.Fatal("Take refused the lock")
	}
	cfg := HolderConfig{
		Dir:         r.dir,
		Lock:        r.lock,
		Activations: r.acts,
		Drain:       r.drain,
		Sockets:     []Sockets{r.web, r.admin},
		Admin:       &fakeAdmin{ev: r.ev},
		Meter: Meter{
			Conns:  constReader(holderUsage.Connections),
			Units:  constReader(holderUsage.InflightUnits),
			Budget: constReader(holderUsage.BufferedBytes),
			RSS:    func() (int64, error) { return holderUsage.RSSBytes, nil },
		},
		Clock:  r.clk,
		Logger: newLogger(r.logs),
	}
	for _, m := range mod {
		m(r, &cfg)
	}
	h, err := StartHolder(t.Context(), cfg)
	if err != nil {
		t.Fatalf("StartHolder: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	r.h = h
	return r
}

func (r *holderRig) dial() *wire { return dialSocket(r.t, r.dir.HandoverSocketPath()) }

// accept takes a new process with active Revision d to accepted.
func (r *holderRig) accept(d revision.Digest) *wire {
	r.t.Helper()
	w := r.dial()
	w.send(readyFor(d))
	m := w.expect(TypeAccepted)
	if m.Usage != holderUsage {
		r.t.Fatalf("accepted usage %+v, want %+v", m.Usage, holderUsage)
	}
	return w
}

// steered takes a new process to the holder's linger: accepted, then
// listening, until the holder steered.
func (r *holderRig) steered() *wire {
	r.t.Helper()
	w := r.accept(digestA)
	w.send(bareWire{Type: TypeListening})
	waitFor(r.t, "the steering", func() bool { return r.ev.count("admin:keepalives off") == 1 })
	return w
}

// TestStartHolderConfig checks that StartHolder refuses a configuration
// without its parts, a process that does not hold the lock or whose
// Drain released it, and a data directory whose socket path a Unix socket
// address cannot hold.
func TestStartHolderConfig(t *testing.T) {
	ev := &events{}
	ok := func(t *testing.T) HolderConfig {
		t.Helper()
		d := dataDir(t)
		l := NewLock()
		l.Take(takeLock(t, d))
		return HolderConfig{Dir: d, Lock: l, Activations: &fakeActivations{ev: ev}, Drain: newFakeDrainer(ev)}
	}
	for name, tt := range map[string]struct {
		mod  func(t *testing.T, c *HolderConfig)
		want error
	}{
		"no dir":            {mod: func(_ *testing.T, c *HolderConfig) { c.Dir = nil }, want: ErrConfig},
		"no lock":           {mod: func(_ *testing.T, c *HolderConfig) { c.Lock = nil }, want: ErrConfig},
		"no activations":    {mod: func(_ *testing.T, c *HolderConfig) { c.Activations = nil }, want: ErrConfig},
		"no drain":          {mod: func(_ *testing.T, c *HolderConfig) { c.Drain = nil }, want: ErrConfig},
		"nil sockets":       {mod: func(_ *testing.T, c *HolderConfig) { c.Sockets = []Sockets{nil} }, want: ErrConfig},
		"negative watchdog": {mod: func(_ *testing.T, c *HolderConfig) { c.Watchdog = -time.Second }, want: ErrConfig},
		"lock not taken":    {mod: func(_ *testing.T, c *HolderConfig) { c.Lock = NewLock() }, want: ErrNotHeld},
		"lock released": {mod: func(t *testing.T, c *HolderConfig) {
			if err := c.Lock.Release(); err != nil {
				t.Fatal(err)
			}
		}, want: ErrReleased},
		"socket path too long": {mod: func(t *testing.T, c *HolderConfig) {
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
			h, err := StartHolder(t.Context(), cfg)
			if err == nil {
				_ = h.Close()
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("StartHolder = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestStartHolderReplacesStaleSocket checks that the holder removes a
// file a crashed holder left at handover.sock and creates an owner-only
// socket there, and that Close removes it while the lock is held.
func TestStartHolderReplacesStaleSocket(t *testing.T) {
	var path string
	r := newHolderRig(t, func(r *holderRig, _ *HolderConfig) {
		path = r.dir.HandoverSocketPath()
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Type() != fs.ModeSocket || fi.Mode().Perm() != nodedir.FilePerm {
		t.Errorf("handover.sock mode %v, want an owner-only socket", fi.Mode())
	}
	if err := r.h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("handover.sock after Close: %v, want it removed", err)
	}
}

// TestHolderAccepts checks accepted: for a ready message whose active
// Revision is the holder's, and for any ready message while the holder
// has none, with the usage at acceptance; the holder holds its Drain and
// its activations from then on.
func TestHolderAccepts(t *testing.T) {
	for _, tt := range []struct {
		name   string
		holder Revisions
		ready  revision.Digest
	}{
		{name: "same active Revision", holder: Revisions{Active: digestA, Candidate: digestA}, ready: digestA},
		{name: "holder without an active Revision", ready: digestB},
		{name: "neither has an active Revision"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newHolderRig(t, func(r *holderRig, _ *HolderConfig) { r.acts.set(tt.holder) })
			r.accept(tt.ready)
			if want := []string{"activations:hold", "drain:hold"}; !slices.Equal(r.ev.all(), want) {
				t.Errorf("events %v, want %v", r.ev.all(), want)
			}
			if !r.acts.isHeld() {
				t.Error("activations run again after accepted")
			}
		})
	}
}

// TestHolderAcceptsWithItsWatchdog checks the holder's watchdog interval
// on accepted: watchdogUsec in microseconds while the holder runs a
// watchdog, and no watchdogUsec member while it runs none, so its
// successor runs no watchdog unless its own environment enables one. The
// usage reports carry no interval.
func TestHolderAcceptsWithItsWatchdog(t *testing.T) {
	for _, tt := range []struct {
		name     string
		watchdog time.Duration
		want     int64
	}{
		{name: "the T5 unit's 30 s", watchdog: 30 * time.Second, want: 30_000_000},
		{name: "a 4 s interval", watchdog: 4 * time.Second, want: 4_000_000},
		{name: "no watchdog"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newHolderRig(t, func(_ *holderRig, c *HolderConfig) { c.Watchdog = tt.watchdog })
			w := r.dial()
			w.send(readyFor(digestA))
			line := w.readLine()
			m, err := decode([]byte(line))
			if err != nil || m.Type != TypeAccepted || m.Usage != holderUsage || m.WatchdogUsec != tt.want {
				t.Fatalf("accepted %s: %+v, %v; want the usage and watchdogUsec %d", line, m, err, tt.want)
			}
			if has := strings.Contains(line, `"watchdogUsec"`); has != (tt.want != 0) {
				t.Errorf("accepted %s has a watchdogUsec member: %t, want %t", line, has, tt.want != 0)
			}
			waitArmed(t, r.clk, 2) // the next report and the listening bound
			r.clk.Advance(UsageInterval)
			if line := w.readLine(); strings.Contains(line, `"watchdogUsec"`) {
				t.Errorf("usage %s carries the watchdog interval, want accepted alone to", line)
			}
		})
	}
}

// TestHolderReportsUsageEverySecond checks usage after accepted: one
// report each UsageInterval with the holder's usage.
func TestHolderReportsUsageEverySecond(t *testing.T) {
	r := newHolderRig(t)
	w := r.accept(digestA)
	for range 3 {
		waitArmed(t, r.clk, 2) // the next report and the listening bound
		r.clk.Advance(UsageInterval)
		if m := w.expect(TypeUsage); m.Usage != holderUsage {
			t.Fatalf("usage %+v, want %+v", m.Usage, holderUsage)
		}
	}
}

// TestHolderReloadsNewerCandidate checks reload: a ready message whose
// active Revision is not the holder's, while the holder's candidate is
// its active Revision, gets reload with the candidate's digest, the
// holder's activations run again, and the ready message sent after the
// reload is accepted.
func TestHolderReloadsNewerCandidate(t *testing.T) {
	r := newHolderRig(t)
	w := r.dial()
	w.send(readyFor(digestB))
	m := w.expect(TypeReload)
	if m.Digest != digestA.String() {
		t.Fatalf("reload digest %q, want %s", m.Digest, digestA)
	}
	if r.acts.isHeld() {
		t.Fatal("activations held after reload")
	}
	w.send(readyFor(digestA))
	w.expect(TypeAccepted)
}

// TestHolderRefusesOlderCandidate checks older_candidate: a ready message
// whose active Revision is not the holder's, while the holder's
// candidate is older than its active Revision or missing, is refused, and
// the holder's activations run again.
func TestHolderRefusesOlderCandidate(t *testing.T) {
	for _, tt := range []struct {
		name string
		revs Revisions
	}{
		{name: "failed candidate write", revs: Revisions{Active: digestA, Candidate: digestB}},
		{name: "no candidate", revs: Revisions{Active: digestA}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newHolderRig(t, func(r *holderRig, _ *HolderConfig) { r.acts.set(tt.revs) })
			w := r.dial()
			w.send(readyFor(digestB))
			if m := w.expect(TypeRefused); m.Reason != ReasonOlderCandidate {
				t.Fatalf("reason %q, want %q", m.Reason, ReasonOlderCandidate)
			}
			if !w.closed() {
				t.Error("the holder kept the refused connection open")
			}
			if r.acts.isHeld() {
				t.Error("activations held after the refusal")
			}
		})
	}
}

// TestHolderRefusesUnknownMarker checks unknown_marker: a first message
// the holder cannot read is refused before any activation is held.
func TestHolderRefusesUnknownMarker(t *testing.T) {
	for name, line := range map[string]string{
		"missing marker":     `{"type":"ready","markers":{"handover":"ruralz.handover.v1","lkg":"ruralz.lkg.v1","holder":"ruralz.holder.v1"},"activeDigest":null}`,
		"unknown identifier": `{"type":"ready","markers":{"handover":"ruralz.handover.v1","lkg":"ruralz.lkg.v2","holder":"ruralz.holder.v1","canonical":"ruralz.canonical.v1"},"activeDigest":null}`,
		"unknown type":       `{"type":"hello"}`,
		"not JSON":           `ready please`,
		"malformed digest":   `{"type":"ready","markers":{"handover":"ruralz.handover.v1","lkg":"ruralz.lkg.v1","holder":"ruralz.holder.v1","canonical":"ruralz.canonical.v1"},"activeDigest":"sha256:00"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newHolderRig(t)
			w := r.dial()
			w.sendRaw(line)
			if m := w.expect(TypeRefused); m.Reason != ReasonUnknownMarker {
				t.Fatalf("reason %q, want %q", m.Reason, ReasonUnknownMarker)
			}
			if n := r.ev.count("activations:hold"); n != 0 {
				t.Errorf("activations held %d times for an unreadable ready", n)
			}
		})
	}
}

// TestHolderRefusesWhileDraining checks draining: a ready message after
// the holder's Drain started, or one whose answer waits on an activation
// when the Drain starts, is refused with draining.
func TestHolderRefusesWhileDraining(t *testing.T) {
	t.Run("drain before ready", func(t *testing.T) {
		r := newHolderRig(t)
		r.drain.start()
		w := r.dial()
		w.send(readyFor(digestA))
		if m := w.expect(TypeRefused); m.Reason != ReasonDraining {
			t.Fatalf("reason %q, want %q", m.Reason, ReasonDraining)
		}
	})
	t.Run("drain during an activation", func(t *testing.T) {
		r := newHolderRig(t, func(r *holderRig, _ *HolderConfig) { r.acts.busy = make(chan struct{}) })
		w := r.dial()
		w.send(readyFor(digestA))
		waitFor(t, "the session to claim the handover", func() bool { return r.logs.has("handover started") })
		r.drain.start()
		if m := w.expect(TypeRefused); m.Reason != ReasonDraining {
			t.Fatalf("reason %q, want %q", m.Reason, ReasonDraining)
		}
	})
	t.Run("drain hold refused", func(t *testing.T) {
		r := newHolderRig(t, func(r *holderRig, _ *HolderConfig) { r.drain.refuseHold = true })
		w := r.dial()
		w.send(readyFor(digestA))
		if m := w.expect(TypeRefused); m.Reason != ReasonDraining {
			t.Fatalf("reason %q, want %q", m.Reason, ReasonDraining)
		}
		if r.acts.isHeld() {
			t.Error("activations held after the refusal")
		}
	})
}

// TestHolderRefusesBusy checks busy: while one new process's handover is
// in progress, another's ready message is refused.
func TestHolderRefusesBusy(t *testing.T) {
	r := newHolderRig(t)
	r.accept(digestA)
	w := r.dial()
	w.send(readyFor(digestA))
	if m := w.expect(TypeRefused); m.Reason != ReasonBusy {
		t.Fatalf("reason %q, want %q", m.Reason, ReasonBusy)
	}
}

// TestHolderAnswersOnceTheActivationEnds checks that a holder activating
// a Revision answers a ready message once that activation ends.
func TestHolderAnswersOnceTheActivationEnds(t *testing.T) {
	busy := make(chan struct{})
	r := newHolderRig(t, func(r *holderRig, _ *HolderConfig) { r.acts.busy = busy })
	w := r.dial()
	w.send(readyFor(digestB))
	waitFor(t, "the session to claim the handover", func() bool { return r.logs.has("handover started") })
	if n := r.ev.count("activations:hold"); n != 0 {
		t.Fatalf("activations held during an activation: %v", r.ev.all())
	}
	r.acts.set(Revisions{Active: digestB, Candidate: digestB}) // the activation's Revision
	close(busy)
	w.expect(TypeAccepted)
}

// TestHolderWaitsForLingeringSockets checks that the holder accepts only
// once no socket a Hot Reload replaced lingers, holding its activations
// meanwhile, and holds its Drain after.
func TestHolderWaitsForLingeringSockets(t *testing.T) {
	lingering := make(chan struct{})
	r := newHolderRig(t, func(r *holderRig, _ *HolderConfig) { r.web.lingering = lingering })
	w := r.dial()
	w.send(readyFor(digestA))
	waitFor(t, "the wait for lingering sockets", func() bool { return r.ev.count("web:wait") == 1 })
	if r.ev.count("drain:hold") != 0 {
		t.Fatal("the holder held its Drain while a socket lingers")
	}
	close(lingering)
	w.expect(TypeAccepted)
	if want := []string{"activations:hold", "web:wait", "drain:hold"}; !slices.Equal(r.ev.all(), want) {
		t.Errorf("events %v, want %v", r.ev.all(), want)
	}
}

// TestHolderRefusesAnotherUser checks the SO_PEERCRED check: a peer that
// runs as another user gets no answer and its connection closes.
func TestHolderRefusesAnotherUser(t *testing.T) {
	r := newHolderRig(t, func(_ *holderRig, c *HolderConfig) {
		c.euid = func() int64 { return int64(os.Geteuid()) + 1 }
	})
	w := r.dial()
	if b, err := encode(readyFor(digestA)); err == nil {
		_, _ = w.c.Write(b) // the holder may have closed already
	}
	if !w.closed() {
		t.Fatal("the holder answered a peer of another user")
	}
	waitFor(t, "the refusal record", func() bool { return r.logs.has("handover connection refused", "peer runs as another user") })
}

// TestHolderGateTimeout checks the gate's bound on the holder's side: a
// connection without an acceptable ready message GateTimeout after it
// connected is closed.
func TestHolderGateTimeout(t *testing.T) {
	r := newHolderRig(t)
	w := r.dial()
	w.send(readyFor(digestB))
	w.expect(TypeReload)
	waitArmed(t, r.clk, 1)
	r.clk.Advance(GateTimeout - time.Nanosecond)
	waitArmed(t, r.clk, 1)
	r.clk.Advance(time.Nanosecond)
	if !w.closed() {
		t.Fatal("the connection stayed open past the gate")
	}
}

// TestHolderAbortsWithoutListening checks the listening bound: a new
// process that sends no listening within ListeningTimeout of accepted, or
// closes first, or sends anything else, aborts the handover; the holder
// resumes its Drain hold and activations and never steers.
func TestHolderAbortsWithoutListening(t *testing.T) {
	for _, tt := range []struct {
		name string
		act  func(r *holderRig, w *wire)
	}{
		{name: "timeout", act: func(r *holderRig, _ *wire) {
			waitArmed(r.t, r.clk, 2) // the next report and the listening bound
			r.clk.Advance(ListeningTimeout - time.Nanosecond)
			if r.ev.count("drain:resume") != 0 {
				r.t.Fatal("the handover aborted before the listening bound")
			}
			r.clk.Advance(time.Nanosecond)
		}},
		{name: "closed", act: func(_ *holderRig, w *wire) { _ = w.c.Close() }},
		{name: "another message", act: func(_ *holderRig, w *wire) { w.send(readyFor(digestA)) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newHolderRig(t)
			w := r.accept(digestA)
			tt.act(r, w)
			waitFor(t, "the abort", func() bool { return r.ev.count("activations:release") == 1 })
			if want := []string{"activations:hold", "drain:hold", "drain:resume", "activations:release"}; !slices.Equal(r.ev.all(), want) {
				t.Errorf("events %v, want %v", r.ev.all(), want)
			}
		})
	}
}

// TestHolderSteersLingersAndHandsOver checks the steering: on listening
// the holder steers every group and moves keep-alive health checkers off
// the admin server; after the Linger it stops accepting on every socket,
// then starts the handover's Drain. Its Drain's lock release removes
// handover.sock and sends released; usage reports go on until Close.
func TestHolderSteersLingersAndHandsOver(t *testing.T) {
	r := newHolderRig(t)
	w := r.steered()
	waitArmed(t, r.clk, 2) // the next report and the linger
	r.clk.Advance(Linger - time.Nanosecond)
	if r.ev.count("web:stop") != 0 || r.ev.count("drain:handover") != 0 {
		t.Fatal("the holder stopped accepting before the Linger ended")
	}
	r.clk.Advance(time.Nanosecond)
	waitFor(t, "the handover's Drain", func() bool { return r.ev.count("drain:handover") == 1 })
	want := []string{
		"activations:hold", "drain:hold",
		"web:steer", "adminport:steer", "admin:keepalives off",
		"web:stop", "adminport:stop", "drain:handover",
	}
	if got := r.ev.all(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}

	if err := r.lock.Release(); err != nil { // as the Drain's start does
		t.Fatal(err)
	}
	if _, err := os.Stat(r.dir.HandoverSocketPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("handover.sock after the release: %v, want it removed", err)
	}
	for {
		if m := w.read(); m.Type == TypeReleased {
			break
		}
	}
	waitArmed(t, r.clk, 1)
	r.clk.Advance(UsageInterval)
	w.expect(TypeUsage)
	if !r.acts.isHeld() {
		t.Error("activations run again after the handover")
	}
	if err := r.h.Close(); err != nil {
		t.Fatal(err)
	}
	if !w.closed() {
		t.Error("the connection stayed open after Close")
	}
}

// TestHolderRePinsLostSuccessor checks a new process lost during the
// linger: the holder re-pins every group to its own sockets, keeps admin
// connections alive again, resumes its Drain hold and activations, and
// neither stops accepting nor Drains.
func TestHolderRePinsLostSuccessor(t *testing.T) {
	r := newHolderRig(t)
	w := r.steered()
	_ = w.c.Close()
	waitFor(t, "the re-pin", func() bool { return r.ev.count("activations:release") == 1 })
	want := []string{
		"activations:hold", "drain:hold",
		"web:steer", "adminport:steer", "admin:keepalives off",
		"web:pin", "adminport:pin", "admin:keepalives on", "drain:resume", "activations:release",
	}
	if got := r.ev.all(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
	r.clk.Advance(Linger)
	if r.ev.count("web:stop") != 0 || r.ev.count("drain:handover") != 0 {
		t.Errorf("the holder stopped or drained after a lost successor: %v", r.ev.all())
	}
	// A later new process gets its own handover.
	waitFor(t, "the handover slot", func() bool {
		r.h.mu.Lock()
		defer r.h.mu.Unlock()
		return r.h.session == nil
	})
	r.accept(digestA)
}

// TestHolderBoundsConnections checks that the holder serves at most two
// connections at once: a third closes at once without an answer.
func TestHolderBoundsConnections(t *testing.T) {
	r := newHolderRig(t, func(r *holderRig, _ *HolderConfig) { r.acts.busy = make(chan struct{}) })
	first := r.dial()
	first.send(readyFor(digestA))
	waitFor(t, "the first session", func() bool { return r.logs.has("handover started") })
	_ = r.dial() // sends nothing, holding the second slot
	third := r.dial()
	if !third.closed() {
		t.Fatal("a third connection was served")
	}
	waitFor(t, "the record of the closed connection", func() bool { return r.logs.has("handover connection closed: too many at once") })
}

// TestHolderCloseAbortsAcceptedHandover checks that Close during an
// accepted handover gives the holder back its Drain and activations and
// removes handover.sock while the lock is held.
func TestHolderCloseAbortsAcceptedHandover(t *testing.T) {
	r := newHolderRig(t)
	w := r.accept(digestA)
	if err := r.h.Close(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"activations:hold", "drain:hold", "drain:resume", "activations:release"}; !slices.Equal(r.ev.all(), want) {
		t.Errorf("events %v, want %v", r.ev.all(), want)
	}
	if !w.closed() {
		t.Error("the connection stayed open after Close")
	}
	if _, err := os.Stat(r.dir.HandoverSocketPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("handover.sock after Close: %v, want it removed", err)
	}
}
