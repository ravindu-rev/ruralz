// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
)

// Tests for the file-mode watcher of docs/architecture/03-data-plane.md
// "Sources, Last-Known-Good and Drain": polling, the settle debounce, a
// replaced root loading at once, loads discarded when the tree changes
// under them, Reload (SIGHUP), the load deadline, panics and Stop. Time is
// a fake clock; each poll of Run reports its trigger to the test.

func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// bound is the real-time limit of a wait for a goroutine the test
// started; fake time never needs it.
const bound = 10 * time.Second

// fakePipe stands in for the pipeline: Run counts its calls and runs the
// test's hook; Parse is the real pipeline's, when one is set.
type fakePipe struct {
	mu    sync.Mutex
	runs  int
	onRun func(ctx context.Context, run int) (*pipeline.Result, error)
	real  *pipeline.Pipeline
}

func (f *fakePipe) Run(ctx context.Context, _ pipeline.Request) (*pipeline.Result, error) {
	f.mu.Lock()
	f.runs++
	n, fn := f.runs, f.onRun
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, n)
	}
	return &pipeline.Result{}, nil
}

func (f *fakePipe) Parse(ctx context.Context, src loader.Source) (*loader.Bundle, error) {
	return f.real.Parse(ctx, src)
}

func (f *fakePipe) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

func (f *fakePipe) set(fn func(ctx context.Context, run int) (*pipeline.Result, error)) {
	f.mu.Lock()
	f.onRun = fn
	f.mu.Unlock()
}

// wrig is a Watcher over a temporary tree on a fake clock.
type wrig struct {
	t     *testing.T
	clk   *clocktest.Fake
	pipe  *fakePipe
	w     *Watcher
	root  string // the temporary directory
	path  string // RURALZ_CONFIG
	loads chan Load
	polls chan Trigger
	mtime time.Time
}

// newRig returns a Watcher of root/bundle, a directory with ruralz.yaml.
func newRig(t *testing.T) *wrig {
	t.Helper()
	root := t.TempDir()
	r := &wrig{
		t: t, clk: clocktest.New(epoch()), pipe: &fakePipe{}, root: root, path: filepath.Join(root, "bundle"),
		loads: make(chan Load, 64), polls: make(chan Trigger, 64), mtime: epoch(),
	}
	if err := os.Mkdir(r.path, 0o700); err != nil {
		t.Fatal(err)
	}
	r.file("ruralz.yaml", "a")
	r.w = r.newWatcher(r.path)
	return r
}

func (r *wrig) newWatcher(path string) *Watcher {
	r.t.Helper()
	w, err := New(Config{
		Path: path, Pipeline: r.pipe, Clock: r.clk,
		Sink: func(ld Load) { r.loads <- ld },
	})
	if err != nil {
		r.t.Fatal(err)
	}
	w.afterPoll = func(tr Trigger) { r.polls <- tr }
	return w
}

// file writes name under the bundle with a fresh modification time.
func (r *wrig) file(name, data string) {
	r.t.Helper()
	p := filepath.Join(r.path, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.mtime = r.mtime.Add(time.Second)
	if err := os.Chtimes(p, r.mtime, r.mtime); err != nil {
		r.t.Fatal(err)
	}
}

// run starts Run and stops it at the test's end.
func (r *wrig) run() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.w.Run(ctx) }()
	r.t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				r.t.Errorf("Run = %v", err)
			}
		case <-time.After(bound):
			r.t.Error("Run did not return")
		}
	})
}

// nextPoll waits for Run's next poll and returns its trigger.
func (r *wrig) nextPoll() Trigger {
	r.t.Helper()
	select {
	case tr := <-r.polls:
		return tr
	case <-time.After(bound):
		r.t.Fatal("no poll")
		return TriggerNone
	}
}

// tick advances the clock one poll interval and returns that poll's
// trigger.
func (r *wrig) tick() Trigger {
	r.t.Helper()
	waitFor(r.t, "Run to wait for its poll timer", func() bool { return r.clk.Pending() > 0 })
	r.clk.Advance(PollInterval)
	return r.nextPoll()
}

// load returns the next delivered Load.
func (r *wrig) load() Load {
	r.t.Helper()
	select {
	case ld := <-r.loads:
		return ld
	case <-time.After(bound):
		r.t.Fatal("no Load delivered")
		return Load{}
	}
}

// noLoad checks that nothing was delivered.
func (r *wrig) noLoad() {
	r.t.Helper()
	select {
	case ld := <-r.loads:
		r.t.Fatalf("unexpected Load %+v", ld)
	default:
	}
}

// waitFor polls cond with a real-time bound.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(bound)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestRunLoadsAtStart(t *testing.T) {
	r := newRig(t)
	r.run()
	if tr := r.nextPoll(); tr != TriggerBoot {
		t.Fatalf("first poll = %v, want boot", tr)
	}
	ld := r.load()
	if ld.Err != nil || ld.Result == nil || ld.Trigger != TriggerBoot || !ld.Start.Equal(epoch()) {
		t.Fatalf("Load = %+v, want a boot result at the start time", ld)
	}
	// Nothing changes: no further load.
	for range 3 {
		if tr := r.tick(); tr != TriggerNone {
			t.Fatalf("poll of an unchanged tree = %v", tr)
		}
	}
	r.noLoad()
	if n := r.pipe.count(); n != 1 {
		t.Fatalf("pipeline ran %d times, want 1", n)
	}
}

func TestChangeLoadsAfterSettle(t *testing.T) {
	r := newRig(t)
	r.run()
	r.nextPoll()
	r.load()
	r.file("routes.yaml", "x")
	// The first poll that sees the change waits for it to settle.
	if tr := r.tick(); tr != TriggerNone {
		t.Fatalf("poll seeing a fresh change = %v, want none", tr)
	}
	r.noLoad()
	if tr := r.tick(); tr != TriggerSettled {
		t.Fatalf("poll after the change settled = %v, want settled", tr)
	}
	if ld := r.load(); ld.Trigger != TriggerSettled || ld.Err != nil {
		t.Fatalf("Load = %+v", ld)
	}
}

func TestContinuousChangesWaitForSettle(t *testing.T) {
	// A tree that changes at every poll never loads half-written.
	r := newRig(t)
	r.run()
	r.nextPoll()
	r.load()
	for i := range 4 {
		r.file("routes.yaml", string(rune('a'+i)))
		if tr := r.tick(); tr != TriggerNone {
			t.Fatalf("poll %d while the tree keeps changing = %v", i, tr)
		}
	}
	r.noLoad()
	if tr := r.tick(); tr != TriggerSettled {
		t.Fatalf("poll after the last change = %v, want settled", tr)
	}
	r.load()
}

func TestChangeKinds(t *testing.T) {
	// Every change to a walked name, size, mode or modification time is a
	// change; one only to an ignored dot entry is not.
	tests := []struct {
		name   string
		change func(r *wrig)
		loads  bool
	}{
		{"new file", func(r *wrig) { r.file("extra.yaml", "b") }, true},
		{"removed file", func(r *wrig) { _ = os.Remove(filepath.Join(r.path, "ruralz.yaml")) }, true},
		{"size", func(r *wrig) { r.file("ruralz.yaml", "ab") }, true},
		{"modification time only", func(r *wrig) {
			r.mtime = r.mtime.Add(time.Hour)
			_ = os.Chtimes(filepath.Join(r.path, "ruralz.yaml"), r.mtime, r.mtime)
		}, true},
		{"mode", func(r *wrig) { _ = os.Chmod(filepath.Join(r.path, "ruralz.yaml"), 0o400) }, true},
		{"nested file", func(r *wrig) { r.file("routes/orders.yaml", "c") }, true},
		{"overlay file", func(r *wrig) { r.file("overlays/prod/patch.yaml", "d") }, true},
		{".ruralzignore", func(r *wrig) { r.file(".ruralzignore", "*.txt\n") }, true},
		{"dot directory", func(r *wrig) { r.file(".git/HEAD", "ref") }, false},
		{"dot file", func(r *wrig) { r.file(".ruralz.yaml.swp", "swap") }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			r.file(".git/config", "x")
			r.run()
			r.nextPoll()
			r.load()
			tt.change(r)
			r.tick()
			got := r.tick() == TriggerSettled
			if got != tt.loads {
				t.Fatalf("loaded = %v, want %v", got, tt.loads)
			}
		})
	}
}

func TestReplacedRootLoadsAtOnce(t *testing.T) {
	r := newRig(t)
	r.run()
	r.nextPoll()
	r.load()
	// A directory renamed into place: no settle.
	next := filepath.Join(r.root, "next")
	if err := os.Mkdir(next, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(next, "ruralz.yaml"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(r.path, filepath.Join(r.root, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, r.path); err != nil {
		t.Fatal(err)
	}
	if tr := r.tick(); tr != TriggerReplaced {
		t.Fatalf("poll after the replacement = %v, want replaced", tr)
	}
	if ld := r.load(); ld.Trigger != TriggerReplaced {
		t.Fatalf("Load = %+v", ld)
	}
}

func TestReplacedFileLoadsAtOnce(t *testing.T) {
	// A rendered one-file Bundle swapped by rename, as ruralz dev run and
	// the test harness do.
	r := newRig(t)
	file := filepath.Join(r.root, "rendered.yaml")
	if err := os.WriteFile(file, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.w = r.newWatcher(file)
	r.run()
	r.nextPoll()
	r.load()
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, file); err != nil {
		t.Fatal(err)
	}
	if tr := r.tick(); tr != TriggerReplaced {
		t.Fatalf("poll after the swap = %v, want replaced", tr)
	}
	r.load()
}

func TestNewSymlinkTargetLoadsAtOnce(t *testing.T) {
	r := newRig(t)
	v2 := filepath.Join(r.root, "v2")
	if err := os.Mkdir(v2, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(r.root, "current")
	if err := os.Symlink(r.path, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	r.w = r.newWatcher(link)
	r.run()
	r.nextPoll()
	r.load()
	tmp := filepath.Join(r.root, "current.tmp")
	if err := os.Symlink(v2, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Fatal(err)
	}
	if tr := r.tick(); tr != TriggerReplaced {
		t.Fatalf("poll after the retarget = %v, want replaced", tr)
	}
	r.load()
}

func TestTreeChangedDuringLoadIsDiscarded(t *testing.T) {
	r := newRig(t)
	r.run()
	r.nextPoll()
	r.load()
	r.file("routes.yaml", "1")
	r.tick()
	// The settled load sees the tree change under it: discarded.
	r.pipe.set(func(_ context.Context, run int) (*pipeline.Result, error) {
		if run == 2 {
			r.file("routes.yaml", "22")
		}
		return &pipeline.Result{}, nil
	})
	if tr := r.tick(); tr != TriggerSettled {
		t.Fatalf("poll = %v, want settled", tr)
	}
	r.noLoad()
	// Loaded again once the new tree settles.
	if tr := r.tick(); tr != TriggerSettled {
		t.Fatalf("poll after the discarded load = %v, want settled", tr)
	}
	if ld := r.load(); ld.Err != nil {
		t.Fatalf("Load = %+v", ld)
	}
	if n := r.pipe.count(); n != 3 {
		t.Fatalf("pipeline ran %d times, want 3", n)
	}
}

func TestRootReplacedDuringLoadLoadsAgainAtOnce(t *testing.T) {
	r := newRig(t)
	r.run()
	r.nextPoll()
	r.load()
	r.pipe.set(func(_ context.Context, run int) (*pipeline.Result, error) {
		if run == 2 {
			next := filepath.Join(r.root, "next")
			_ = os.Mkdir(next, 0o700)
			_ = os.WriteFile(filepath.Join(next, "ruralz.yaml"), []byte("b"), 0o600)
			_ = os.Rename(r.path, filepath.Join(r.root, "old"))
			_ = os.Rename(next, r.path)
		}
		return &pipeline.Result{}, nil
	})
	r.w.Reload()
	if tr := r.nextPoll(); tr != TriggerReload {
		t.Fatalf("poll = %v, want reload", tr)
	}
	if ld := r.load(); ld.Trigger != TriggerReplaced {
		t.Fatalf("Load = %+v, want the replaced root loaded at once", ld)
	}
	if n := r.pipe.count(); n != 3 {
		t.Fatalf("pipeline ran %d times, want 3", n)
	}
}

func TestReloadSkipsTheSettle(t *testing.T) {
	r := newRig(t)
	r.run()
	r.nextPoll()
	r.load()
	r.file("routes.yaml", "x")
	r.w.Reload()
	if tr := r.nextPoll(); tr != TriggerReload {
		t.Fatalf("poll = %v, want reload", tr)
	}
	r.load()
	// An unchanged tree loads too: SIGHUP loads the source at once.
	r.w.Reload()
	if tr := r.nextPoll(); tr != TriggerReload {
		t.Fatalf("poll = %v, want reload", tr)
	}
	r.load()
}

func TestReloadBeforeRun(t *testing.T) {
	r := newRig(t)
	if ld := r.w.Load(t.Context()); ld.Err != nil {
		t.Fatal(ld.Err)
	}
	r.w.Reload()
	r.run()
	if tr := r.nextPoll(); tr != TriggerReload {
		t.Fatalf("first poll = %v, want the Reload received before Run", tr)
	}
	r.load()
}

func TestMissingSource(t *testing.T) {
	r := newRig(t)
	gone := filepath.Join(r.root, "absent")
	r.w = r.newWatcher(gone)
	r.run()
	r.nextPoll()
	ld := r.load()
	if !ld.Unavailable() || !errors.Is(ld.Err, os.ErrNotExist) {
		t.Fatalf("Load = %+v, want unavailable", ld)
	}
	// Reported once per disappearance.
	r.tick()
	r.tick()
	r.noLoad()
	// Every Reload reports it again.
	r.w.Reload()
	r.nextPoll()
	if ld := r.load(); !ld.Unavailable() || ld.Trigger != TriggerReload {
		t.Fatalf("Load = %+v, want unavailable on Reload", ld)
	}
	// A source that appears loads at once.
	if err := os.Rename(r.path, gone); err != nil {
		t.Fatal(err)
	}
	if tr := r.tick(); tr != TriggerReplaced {
		t.Fatalf("poll after the source appeared = %v", tr)
	}
	if ld := r.load(); ld.Err != nil {
		t.Fatalf("Load = %+v", ld)
	}
	// One that disappears is reported.
	if err := os.Rename(gone, r.path); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if ld := r.load(); !ld.Unavailable() {
		t.Fatalf("Load = %+v, want unavailable", ld)
	}
	if n := r.pipe.count(); n != 1 {
		t.Fatalf("pipeline ran %d times, want 1", n)
	}
}

func TestUnreadableSourceIsUnavailable(t *testing.T) {
	r := newRig(t)
	r.pipe.set(func(context.Context, int) (*pipeline.Result, error) {
		return nil, loader.ErrSource
	})
	r.run()
	r.nextPoll()
	if ld := r.load(); !ld.Unavailable() || ld.Result != nil {
		t.Fatalf("Load = %+v, want unavailable", ld)
	}
}

func TestLoadDeadline(t *testing.T) {
	r := newRig(t)
	started := make(chan struct{})
	r.pipe.set(func(ctx context.Context, _ int) (*pipeline.Result, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	r.run()
	<-started
	r.clk.Advance(DefaultDeadline)
	ld := r.load()
	if !errors.Is(ld.Err, ErrDeadline) || ld.Spent != DefaultDeadline {
		t.Fatalf("Load = %+v, want ErrDeadline after the deadline", ld)
	}
}

func TestLoadPanicIsRecovered(t *testing.T) {
	r := newRig(t)
	r.pipe.set(func(context.Context, int) (*pipeline.Result, error) { panic("broken stage") })
	r.run()
	r.nextPoll()
	ld := r.load()
	var pe *PanicError
	if !errors.As(ld.Err, &pe) || pe.Value != "broken stage" || len(pe.Stack) == 0 {
		t.Fatalf("Load = %+v, want a *PanicError with its stack", ld)
	}
}

func TestPipelineErrorIsAFault(t *testing.T) {
	r := newRig(t)
	fault := errors.New("no result")
	r.pipe.set(func(context.Context, int) (*pipeline.Result, error) { return nil, fault })
	r.run()
	r.nextPoll()
	if ld := r.load(); !errors.Is(ld.Err, fault) || ld.Unavailable() {
		t.Fatalf("Load = %+v, want the fault", ld)
	}
	r.pipe.set(func(context.Context, int) (*pipeline.Result, error) { return nil, nil })
	r.w.Reload()
	r.nextPoll()
	if ld := r.load(); ld.Err == nil {
		t.Fatalf("Load = %+v, want an error for neither a result nor an error", ld)
	}
}

func TestStopEndsRun(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	done := make(chan error, 1)
	go func() { done <- r.w.Run(ctx) }()
	r.nextPoll()
	r.load()
	r.w.Stop()
	r.w.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(bound):
		t.Fatal("Run did not return after Stop")
	}
	r.w.Reload()
	r.noLoad()
	if err := r.w.Run(ctx); err != nil {
		t.Fatalf("Run after Stop = %v, want nil at once", err)
	}
}

func TestRunTwice(t *testing.T) {
	r := newRig(t)
	r.run()
	r.nextPoll()
	if err := r.w.Run(t.Context()); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Run = %v, want ErrRunning", err)
	}
}

func TestLoadRecordsWhatItLoaded(t *testing.T) {
	// Load at boot, then Run: Run loads only after a change.
	r := newRig(t)
	if ld := r.w.Load(t.Context()); ld.Err != nil || ld.Trigger != TriggerBoot {
		t.Fatalf("Load = %+v", ld)
	}
	r.run()
	if tr := r.nextPoll(); tr != TriggerNone {
		t.Fatalf("Run's first poll after Load = %v, want none", tr)
	}
	r.noLoad()
	r.file("x.yaml", "1")
	r.tick()
	if tr := r.tick(); tr != TriggerSettled {
		t.Fatalf("poll = %v", tr)
	}
}

func TestLoadRetriesAChangingTree(t *testing.T) {
	r := newRig(t)
	r.pipe.set(func(_ context.Context, run int) (*pipeline.Result, error) {
		if run == 1 {
			r.file("routes.yaml", "changed")
		}
		return &pipeline.Result{}, nil
	})
	done := make(chan Load, 1)
	go func() { done <- r.w.Load(t.Context()) }()
	// The first load is discarded; Load waits one interval for the tree
	// to settle, then loads again.
	waitFor(t, "the settle wait", func() bool { return r.clk.Pending() > 0 && r.pipe.count() == 1 })
	r.clk.Advance(PollInterval)
	select {
	case ld := <-done:
		if ld.Err != nil {
			t.Fatalf("Load = %+v", ld)
		}
	case <-time.After(bound):
		t.Fatal("Load did not return")
	}
	if n := r.pipe.count(); n != 2 {
		t.Fatalf("pipeline ran %d times, want 2", n)
	}
}

func TestLoadOfAMissingSource(t *testing.T) {
	r := newRig(t)
	r.w = r.newWatcher(filepath.Join(r.root, "absent"))
	if ld := r.w.Load(t.Context()); !ld.Unavailable() {
		t.Fatalf("Load = %+v, want unavailable", ld)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if ld := r.w.Load(ctx); !errors.Is(ld.Err, context.Canceled) {
		t.Fatalf("Load(canceled) = %+v", ld)
	}
}

func TestNewValidates(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no path":           {Pipeline: &fakePipe{}},
		"no pipeline":       {Path: "/x"},
		"negative interval": {Path: "/x", Pipeline: &fakePipe{}, Interval: -1},
	} {
		if _, err := New(cfg); !errors.Is(err, ErrConfig) {
			t.Errorf("New(%s) = %v, want ErrConfig", name, err)
		}
	}
	w, err := New(Config{Path: "/x", Pipeline: &fakePipe{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Run(t.Context()); !errors.Is(err, ErrConfig) {
		t.Errorf("Run without a Sink = %v, want ErrConfig", err)
	}
	if w.Path() != "/x" {
		t.Errorf("Path = %q", w.Path())
	}
}

func TestTriggerNames(t *testing.T) {
	want := map[Trigger]string{
		TriggerNone: "none", TriggerBoot: "boot", TriggerReplaced: "replaced",
		TriggerSettled: "settled", TriggerReload: "reload",
	}
	for tr, name := range want {
		if tr.String() != name {
			t.Errorf("%d.String() = %q, want %q", tr, tr.String(), name)
		}
	}
}
