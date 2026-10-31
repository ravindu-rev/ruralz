// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package drain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
)

// waitFor polls cond with a real-time bound; it waits only for goroutines
// the test or the package started, never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

// recv waits for one value from ch with a real-time bound.
func recv[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// constSource is a rand.Source returning one value: 1<<63 draws the
// middle of every wave.
type constSource uint64

func (c constSource) Uint64() uint64 { return uint64(c) }

// half draws the middle of each wave: 625 ms into it.
const half = constSource(1 << 63)

// events records what the Drain did, each as "<what>@<offset>", the
// offset from the rig's start on the fake clock.
type events struct {
	clk   *clocktest.Fake
	start time.Time
	mu    sync.Mutex
	list  []string
}

func (e *events) add(what string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, fmt.Sprintf("%s@%v", what, e.clk.Now().Sub(e.start)))
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.list)
}

// has reports whether an event starts with prefix.
func (e *events) has(prefix string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.ContainsFunc(e.list, func(s string) bool { return strings.HasPrefix(s, prefix) })
}

type fakeReadiness struct {
	ev       *events
	answered bool
}

func (r *fakeReadiness) Drain()         { r.ev.add("readiness") }
func (r *fakeReadiness) Answered() bool { return r.answered }

type fakeNotifier struct{ ev *events }

func (n *fakeNotifier) Stopping(context.Context) error   { n.ev.add("stopping"); return nil }
func (n *fakeNotifier) SyncStatus(context.Context) error { n.ev.add("status"); return nil }
func (n *fakeNotifier) Mute()                            { n.ev.add("mute") }

type fakeLock struct {
	ev  *events
	err error
}

func (l *fakeLock) Release() error { l.ev.add("lock"); return l.err }

type fakeListeners struct {
	ev    *events
	bound bool
}

func (l *fakeListeners) Bound() bool         { return l.bound }
func (l *fakeListeners) StopAccepting()      { l.ev.add("stop") }
func (l *fakeListeners) ShutdownShard(i int) { l.ev.add(fmt.Sprintf("shard%d", i)) }
func (l *fakeListeners) Shutdown(context.Context) error {
	l.ev.add("shutdown")
	return nil
}
func (l *fakeListeners) Close() error { l.ev.add("close"); return nil }

type fakeRetirer struct{ ev *events }

func (r *fakeRetirer) EndAll(_ context.Context, reason snapshot.EndReason) (int, error) {
	if reason != snapshot.EndDrain {
		return 0, errors.New("not the drain reason")
	}
	r.ev.add("endall")
	return 3, nil
}

func (r *fakeRetirer) Close(context.Context) error { r.ev.add("retirer"); return nil }

// captureHandler keeps every record as "LEVEL message key=value ...".
type captureHandler struct {
	mu   sync.Mutex
	recs []string
}

func (*captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String() + " " + r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, b.String())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// count returns how many records contain every part.
func (h *captureHandler) count(parts ...string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.recs {
		if !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(r, p) }) {
			n++
		}
	}
	return n
}

// rig is a Drainer over fakes on a fake clock.
type rig struct {
	clk   *clocktest.Fake
	ev    *events
	ready *fakeReadiness
	lis   *fakeListeners
	lock  *fakeLock
	logs  *captureHandler
	d     *Drainer
	code  chan int
}

// newRig returns a Drainer over fakes: a Node seen ready, with a listener
// bound, two shutdown steps and the middle of every wave drawn.
func newRig(t *testing.T, mod ...func(*rig, *Config)) *rig {
	t.Helper()
	clk := clocktest.New(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	ev := &events{clk: clk, start: clk.Now()}
	r := &rig{
		clk:   clk,
		ev:    ev,
		ready: &fakeReadiness{ev: ev, answered: true},
		lis:   &fakeListeners{ev: ev, bound: true},
		lock:  &fakeLock{ev: ev},
		logs:  &captureHandler{},
		code:  make(chan int, 1),
	}
	cfg := Config{
		Readiness:  r.ready,
		Listeners:  r.lis,
		Retirer:    &fakeRetirer{ev: ev},
		Notifier:   &fakeNotifier{ev: ev},
		Lock:       r.lock,
		StopSource: func() { ev.add("source") },
		Steps: []Step{
			{Name: "admin", Run: func(context.Context) error { ev.add("step:admin"); return nil }},
			{Name: "telemetry", Run: func(context.Context) error { ev.add("step:telemetry"); return nil }},
		},
		Clock:  clk,
		Rand:   half,
		Logger: slog.New(r.logs),
	}
	for _, m := range mod {
		m(r, &cfg)
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.d = d
	return r
}

// run starts Run; its code arrives on r.code. The test ends every Drain it
// starts, or cancels ctx.
func (r *rig) run(ctx context.Context) {
	go func() { r.code <- r.d.Run(ctx) }()
}

// at returns the fake time offset from the rig's start.
func (r *rig) at(offset time.Duration) time.Time { return r.ev.start.Add(offset) }

// waitArmed waits until the Drain waits on its next timer, beside its exit
// bound's.
func (r *rig) waitArmed(t *testing.T) {
	t.Helper()
	waitFor(t, "the Drain's next timer", func() bool { return r.clk.Pending() == 2 })
}

// step advances the fake clock to offset once the Drain waits on its next
// timer.
func (r *rig) step(t *testing.T, offset time.Duration) {
	t.Helper()
	r.waitArmed(t)
	r.clk.Set(r.at(offset))
}

// finished waits for Run's exit code.
func (r *rig) finished(t *testing.T) int {
	t.Helper()
	return recv(t, "Run to return", r.code)
}

// wantEvents checks every event so far.
func (r *rig) wantEvents(t *testing.T, want []string) {
	t.Helper()
	if got := r.ev.all(); !slices.Equal(got, want) {
		t.Errorf("events:\n got  %q\n want %q", got, want)
	}
}

// waveAt is the middle of wave i after stop-accepting at stop.
func waveAt(stop time.Duration, i int) time.Duration {
	return stop + time.Duration(i)*wave + wave/2
}

// deadlineEvents are the deadline phase's events at offset at: every shard
// left shut down, the remaining work ended, the retirer and the listeners
// closed, then the steps.
func deadlineEvents(at time.Duration) []string {
	evs := []string{fmt.Sprintf("stop@%v", at)}
	for i := range Waves {
		evs = append(evs, fmt.Sprintf("shard%d@%v", i, at))
	}
	for _, e := range []string{"endall", "retirer", "shutdown", "close", "step:admin", "step:telemetry"} {
		evs = append(evs, fmt.Sprintf("%s@%v", e, at))
	}
	return evs
}

// eventsAt formats events at one offset.
func eventsAt(at time.Duration, what ...string) []string {
	out := make([]string, len(what))
	for i, w := range what {
		out[i] = fmt.Sprintf("%s@%v", w, at)
	}
	return out
}

// wavesEvents are stop-accepting at stop and the waves after it.
func wavesEvents(stop time.Duration, waves int) []string {
	evs := eventsAt(stop, "stop")
	for i := range waves {
		evs = append(evs, fmt.Sprintf("shard%d@%v", i, waveAt(stop, i)))
	}
	return evs
}

// stepWaves advances through stop-accepting at stop and the first n waves.
func (r *rig) stepWaves(t *testing.T, stop time.Duration, n int) {
	t.Helper()
	if stop > 0 {
		r.step(t, stop)
	}
	for i := range n {
		r.step(t, waveAt(stop, i))
	}
}
