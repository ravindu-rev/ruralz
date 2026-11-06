// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/lkg"
	"github.com/ravindu-rev/ruralz/internal/gateway/readiness"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/source"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Test doubles of the activation loader's dependencies: a scripted
// source, pipeline and compiler, recording Parts, metrics, status and
// logs, around a real retirer, readiness tracker and Last-Known-Good
// store on a fake clock.

func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// bound is the real-time limit of a wait for a goroutine the test or the
// loader started; fake time never needs it.
const bound = 10 * time.Second

// testRequestID is the requestId every problem document of the tests
// carries.
const testRequestID = "0af7651916cd43dd8448eb211c80319c"

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

// rev returns the Revision of content.
func rev(content string) revision.Revision {
	return revision.Revision{Digest: revision.Sum([]byte(content)), Content: []byte(content)}
}

// result returns a pipeline run that accepted the Revision of content,
// with one Route per name.
func result(content string, routes ...string) *pipeline.Result {
	var rs []*hub.Resource
	rs = append(rs, &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindGateway, Name: "edge"}, Object: &v1alpha1.Gateway{}})
	for _, r := range routes {
		rs = append(rs, &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindRoute, Name: r}, Object: &v1alpha1.Route{}})
	}
	return &pipeline.Result{Validated: &hub.Validated{Revision: rev(content), Bundle: hub.NewBundle(rs)}}
}

// failedResult returns a pipeline run with diagnostics ds and no Revision.
func failedResult(ds diag.List) *pipeline.Result { return &pipeline.Result{Diagnostics: ds} }

// finding returns an error finding of code at line.
func finding(code string, line int, msg string) diag.Diagnostic {
	return diag.Diagnostic{
		Code: code, Severity: diag.SeverityError, Message: msg,
		Location: diag.Location{File: "ruralz.yaml", Line: line, Column: 1},
	}
}

// load returns a source Load of res.
func load(res *pipeline.Result) source.Load {
	return source.Load{Result: res, Trigger: source.TriggerSettled, Start: epoch()}
}

// events records what the fakes saw, in order.
type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	e.list = append(e.list, s)
	e.mu.Unlock()
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.list)
}

func (e *events) has(s string) bool { return slices.Contains(e.all(), s) }

// fakeSource is a scripted source: Load returns what loadFn returns, Run
// waits until its context ends or Stop.
type fakeSource struct {
	mu      sync.Mutex
	loadFn  func(ctx context.Context) source.Load
	runs    int
	reloads int
	stops   int
	stopped chan struct{}
	once    sync.Once
}

func newFakeSource(ld source.Load) *fakeSource {
	return &fakeSource{loadFn: func(context.Context) source.Load { return ld }, stopped: make(chan struct{})}
}

func (f *fakeSource) Run(ctx context.Context) error {
	f.mu.Lock()
	f.runs++
	f.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-f.stopped:
	}
	return nil
}

func (f *fakeSource) Load(ctx context.Context) source.Load {
	f.mu.Lock()
	fn := f.loadFn
	f.mu.Unlock()
	return fn(ctx)
}

func (f *fakeSource) Reload() {
	f.mu.Lock()
	f.reloads++
	f.mu.Unlock()
}

func (f *fakeSource) Stop() {
	f.mu.Lock()
	f.stops++
	f.mu.Unlock()
	f.once.Do(func() { close(f.stopped) })
}

func (f *fakeSource) counts() (runs, reloads, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs, f.reloads, f.stops
}

// fakeCanonical re-enters canonical content: the content's Revision,
// unless results names another outcome for it.
type fakeCanonical struct {
	mu      sync.Mutex
	results map[string]*pipeline.Result
	errs    map[string]error
	reqs    []pipeline.CanonicalRequest
}

func (f *fakeCanonical) FromCanonical(_ context.Context, req pipeline.CanonicalRequest) (*pipeline.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	c := string(req.Content)
	if err := f.errs[c]; err != nil {
		return nil, err
	}
	if r := f.results[c]; r != nil {
		return r, nil
	}
	if !req.Digest.IsZero() && revision.Sum(req.Content) != req.Digest {
		return failedResult(diag.List{finding("RZ-CFG-027", 1, "content does not hash to "+req.Digest.String())}), nil
	}
	return result(c), nil
}

func (f *fakeCanonical) requests() []pipeline.CanonicalRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// fakeCompiled is a compiled snapshot that records its Activate and
// Discard.
type fakeCompiled struct {
	snap      *snapshot.Snapshot
	warnings  []compile.Warning
	conns     int
	ev        *events
	activated atomic.Int32
	discarded atomic.Int32
}

func (c *fakeCompiled) Snapshot() *snapshot.Snapshot { return c.snap }
func (c *fakeCompiled) Secrets() secret.Store        { return nil }
func (c *fakeCompiled) Warnings() []compile.Warning  { return c.warnings }
func (c *fakeCompiled) UpstreamConnections() int     { return c.conns }

func (c *fakeCompiled) Activate() {
	c.activated.Add(1)
	c.ev.add("activate:" + c.snap.Revision.Digest.Short())
}

func (c *fakeCompiled) Discard() {
	c.discarded.Add(1)
	c.ev.add("discard:" + c.snap.Revision.Digest.Short())
}

// compileFn is one scripted compile.
type compileFn func(ctx context.Context, in compile.Input) (Compiled, diag.List, error)

// fakeCompiler compiles every Revision into a fakeCompiled, unless a
// script for its digest says otherwise; scripts run in order, the last
// one repeating.
type fakeCompiler struct {
	mu       sync.Mutex
	ev       *events
	scripts  map[revision.Digest][]compileFn
	inputs   []compile.Input
	compiled []*fakeCompiled
	warnings []compile.Warning
	conns    int
}

func (f *fakeCompiler) script(d revision.Digest, fns ...compileFn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scripts == nil {
		f.scripts = map[revision.Digest][]compileFn{}
	}
	f.scripts[d] = fns
}

func (f *fakeCompiler) Compile(ctx context.Context, in compile.Input) (Compiled, diag.List, error) {
	d := in.Validated.Revision.Digest
	f.mu.Lock()
	f.inputs = append(f.inputs, in)
	var fn compileFn
	if fns := f.scripts[d]; len(fns) > 0 {
		fn = fns[0]
		if len(fns) > 1 {
			f.scripts[d] = fns[1:]
		}
	}
	f.mu.Unlock()
	f.ev.add("compile:" + d.Short())
	if fn != nil {
		return fn(ctx, in)
	}
	return f.make(in), nil, nil
}

// make returns a fresh fakeCompiled of in.
func (f *fakeCompiler) make(in compile.Input) *fakeCompiled {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeCompiled{
		snap: &snapshot.Snapshot{Revision: in.Validated.Revision, Validated: in.Validated, Gateway: snapshot.Gateway{AdminPort: 9901}},
		ev:   f.ev, warnings: f.warnings, conns: f.conns,
	}
	f.compiled = append(f.compiled, c)
	return c
}

func (f *fakeCompiler) calls() []compile.Input {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.inputs)
}

func (f *fakeCompiler) last() *fakeCompiled {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.compiled) == 0 {
		return nil
	}
	return f.compiled[len(f.compiled)-1]
}

// findings returns a script that refuses the Revision with ds.
func findings(ds ...diag.Diagnostic) compileFn {
	return func(context.Context, compile.Input) (Compiled, diag.List, error) { return nil, ds, nil }
}

// fakePart records its Prepare, Commit and Abort; it is also a Listener
// and a Stopper.
type fakePart struct {
	name       string
	ev         *events
	holder     *retire.Holder
	mu         sync.Mutex
	prepareErr error
	commitErr  error
	listenErr  error
	// published records, at each Prepare and Commit, whether the snapshot
	// being activated was the published one.
	published []bool
}

func (p *fakePart) Prepare(_ context.Context, next Compiled) (Switch, error) {
	p.mu.Lock()
	err := p.prepareErr
	p.published = append(p.published, p.holder.Active() == next.Snapshot())
	p.mu.Unlock()
	if err != nil {
		p.ev.add("prepare-failed:" + p.name)
		return nil, err
	}
	p.ev.add("prepare:" + p.name + ":" + next.Snapshot().Revision.Digest.Short())
	return &fakeSwitch{p: p, snap: next.Snapshot()}, nil
}

func (p *fakePart) Listen(context.Context) error {
	p.ev.add("listen:" + p.name)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listenErr
}

func (p *fakePart) StopAccepting() { p.ev.add("stop:" + p.name) }

func (p *fakePart) set(fn func(p *fakePart)) {
	p.mu.Lock()
	fn(p)
	p.mu.Unlock()
}

type fakeSwitch struct {
	p    *fakePart
	snap *snapshot.Snapshot
}

func (s *fakeSwitch) Commit(context.Context) error {
	s.p.mu.Lock()
	err := s.p.commitErr
	s.p.published = append(s.p.published, s.p.holder.Active() == s.snap)
	s.p.mu.Unlock()
	s.p.ev.add("commit:" + s.p.name + ":" + s.snap.Revision.Digest.Short())
	return err
}

func (s *fakeSwitch) Abort() { s.p.ev.add("abort:" + s.p.name + ":" + s.snap.Revision.Digest.Short()) }

// fakeMetrics records the configuration handles.
type fakeMetrics struct {
	mu        sync.Mutex
	activated int
	rejected  map[string]int
	durations map[string][]time.Duration
	info      map[string]string
}

func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{rejected: map[string]int{}, durations: map[string][]time.Duration{}, info: map[string]string{}}
}

func (m *fakeMetrics) handles() *emit.ConfigMetrics {
	return &emit.ConfigMetrics{
		Activated: counterFunc(func(n uint64) {
			m.mu.Lock()
			m.activated += int(n) //nolint:gosec // G115: small test counts.
			m.mu.Unlock()
		}),
		Rejected: codesFunc(func(code string) {
			m.mu.Lock()
			m.rejected[code]++
			m.mu.Unlock()
		}),
		ActivationDuration: func(stage, class string) emit.Histogram {
			return histFunc(func(v uint64) {
				m.mu.Lock()
				m.durations[stage+"/"+class] = append(m.durations[stage+"/"+class], time.Duration(v)) //nolint:gosec // G115: test durations.
				m.mu.Unlock()
			})
		},
		RevisionInfo: func(role, display string) {
			m.mu.Lock()
			m.info[role] = display
			m.mu.Unlock()
		},
	}
}

func (m *fakeMetrics) activations() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activated
}

func (m *fakeMetrics) rejections(code string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rejected[code]
}

func (m *fakeMetrics) totalRejections() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, v := range m.rejected {
		n += v
	}
	return n
}

func (m *fakeMetrics) revisionInfo(role string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.info[role]
}

func (m *fakeMetrics) stage(key string) []time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.durations[key])
}

type (
	counterFunc func(uint64)
	codesFunc   func(string)
	histFunc    func(uint64)
)

func (f counterFunc) Add(_ emit.Stripe, n uint64)                          { f(n) }
func (f codesFunc) Inc(_ emit.Stripe, code string)                         { f(code) }
func (f histFunc) Record(_ emit.Stripe, v uint64)                          { f(v) }
func (f histFunc) RecordExemplar(_ emit.Stripe, v uint64, _ emit.Exemplar) { f(v) }

// fakeStatus records degraded reasons by name.
type fakeStatus struct {
	mu  sync.Mutex
	on  map[string]bool
	log []string
}

func (s *fakeStatus) SetDegraded(r catalog.Reason, source string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.on == nil {
		s.on = map[string]bool{}
	}
	s.on[r.String()] = on
	s.log = append(s.log, fmt.Sprintf("%s/%s=%v", r, source, on))
}

func (s *fakeStatus) SetCleartextHops(catalog.Hop, int) {}

func (s *fakeStatus) raised(reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on[reason]
}

// fakeNotifier counts SyncStatus calls.
type fakeNotifier struct{ n atomic.Int32 }

func (f *fakeNotifier) SyncStatus(context.Context) error {
	f.n.Add(1)
	return nil
}

// record is one captured log record.
type record struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

// logRecorder captures every record.
type logRecorder struct {
	mu      sync.Mutex
	records []record
	attrs   []slog.Attr
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *logRecorder) Handle(_ context.Context, r slog.Record) error {
	rec := record{level: r.Level, msg: r.Message, attrs: map[string]any{}}
	for _, a := range h.attrs {
		rec.attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *logRecorder) WithAttrs(as []slog.Attr) slog.Handler {
	return &logRecorder{attrs: append(slices.Clone(h.attrs), as...)}
}

func (h *logRecorder) WithGroup(string) slog.Handler { return h }

// find returns the records whose message contains msg.
func (h *logRecorder) find(msg string) []record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []record
	for _, r := range h.records {
		if strings.Contains(r.msg, msg) {
			out = append(out, r)
		}
	}
	return out
}

// bindError returns the RZ-CFG-039 error of a socket that cannot be
// bound.
func bindError(msg string) error { return errcode.Wrap(codeBind, errors.New(msg)) }

// fixedRand always draws v.
type fixedRand struct{ v uint64 }

func (f fixedRand) Uint64() uint64 { return f.v }

// rig is a Loader with every fake, a real retirer, readiness tracker and
// Last-Known-Good store, on a fake clock.
type rig struct {
	t        *testing.T
	clk      *clocktest.Fake
	ev       *events
	src      *fakeSource
	canon    *fakeCanonical
	comp     *fakeCompiler
	parts    []*fakePart
	holder   *retire.Holder
	ready    *readiness.Tracker
	dir      *nodedir.Dir
	lock     *nodedir.Lock
	held     atomic.Bool // the process holds the lock
	store    *lkg.Store
	lkgStat  *fakeStatus
	metrics  *fakeMetrics
	status   *fakeStatus
	notifier *fakeNotifier
	logs     *logRecorder
	fdLimit  atomic.Uint64
	cfg      Config
	l        *Loader
	cancel   context.CancelFunc
	done     chan error
	stopFn   func()
}

// rigOption edits a rig before New builds its Loader.
type rigOption func(r *rig)

// withSuccessorBoot sets the boot order of a handover successor.
func withSuccessorBoot() rigOption { return func(r *rig) { r.cfg.Boot = BootSuccessor } }

// withSource sets the source's boot Load.
func withSource(ld source.Load) rigOption { return func(r *rig) { r.src = newFakeSource(ld) } }

// withRetire sets the retirer's configuration.
func withRetire(cfg retire.Config) rigOption {
	return func(r *rig) {
		cfg.Clock = r.clk
		r.holder = retire.New(cfg)
	}
}

// newRig returns a rig whose source boots content "A"; Start runs it.
func newRig(t *testing.T, opts ...rigOption) *rig { return newRigOn(t, nil, nil, opts...) }

// newRigOn returns a rig over the data directory dir and its lock, or a
// fresh one when dir is nil: a restarted process, or a handover
// successor beside its holder.
func newRigOn(t *testing.T, dir *nodedir.Dir, lock *nodedir.Lock, opts ...rigOption) *rig {
	t.Helper()
	r := &rig{
		t: t, clk: clocktest.New(epoch()), ev: &events{}, canon: &fakeCanonical{},
		ready: readiness.New(), metrics: newFakeMetrics(), status: &fakeStatus{}, lkgStat: &fakeStatus{},
		notifier: &fakeNotifier{}, logs: &logRecorder{}, done: make(chan error, 1),
	}
	r.fdLimit.Store(1 << 20)
	r.comp = &fakeCompiler{ev: r.ev}
	r.src = newFakeSource(load(result("A")))
	r.holder = retire.New(retire.Config{Clock: r.clk, Stripes: 1})
	var err error
	if dir == nil {
		if dir, err = nodedir.Open(t.TempDir()); err != nil {
			t.Fatal(err)
		}
		lock, err = dir.TryLock()
		if errors.Is(err, errors.ErrUnsupported) {
			t.Skip("no flock on this platform")
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lock.Release() })
	}
	r.dir, r.lock = dir, lock
	r.held.Store(true)
	r.store, err = lkg.New(lkg.Config{Dir: dir, Version: "0.1.0-test", Clock: r.clk, Status: r.lkgStat})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"listeners", "admin"} {
		r.parts = append(r.parts, &fakePart{name: name, ev: r.ev})
	}
	r.cfg = Config{
		Boot: BootHolder, Pipeline: r.canon, Compiler: r.comp, Readiness: r.ready, LKG: r.store,
		Lock: func() *nodedir.Lock {
			if r.held.Load() {
				return r.lock
			}
			return nil
		},
		Metrics: r.metrics.handles(), Status: r.status, Notifier: r.notifier,
		RequestID: func(*http.Request) string { return testRequestID },
		Clock:     r.clk, Rand: fixedRand{}, Logger: slog.New(r.logs),
		FileLimit: func() (uint64, bool) { return r.fdLimit.Load(), true },
	}
	for _, o := range opts {
		o(r)
	}
	for _, p := range r.parts {
		p.holder = r.holder
		r.cfg.Parts = append(r.cfg.Parts, p)
	}
	r.cfg.Source, r.cfg.Holder = r.src, r.holder
	r.l, err = New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// start runs the retirer and the loader; stop, or the test's end, stops
// both.
func (r *rig) start() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	retirerDone := make(chan error, 1)
	go func() { retirerDone <- r.holder.Run(ctx) }()
	go func() { r.done <- r.l.Run(ctx) }()
	var once sync.Once
	r.stopFn = func() {
		once.Do(func() {
			cancel()
			for _, ch := range []chan error{r.done, retirerDone} {
				select {
				case err := <-ch:
					if err != nil {
						r.t.Errorf("Run = %v", err)
					}
				case <-time.After(bound):
					r.t.Error("a Run did not return")
				}
			}
		})
	}
	r.t.Cleanup(r.stopFn)
}

// stop ends the loader and the retirer, as a process exit does.
func (r *rig) stop() {
	if r.stopFn != nil {
		r.stopFn()
	}
}

// booted waits for the first boot attempt to end.
func (r *rig) booted() {
	r.t.Helper()
	select {
	case <-r.l.Booted():
	case <-time.After(bound):
		r.t.Fatal("the boot did not end")
	}
	r.idle()
}

// idle waits until the loader runs no job.
func (r *rig) idle() {
	r.t.Helper()
	waitFor(r.t, "the loader to be idle", func() bool {
		r.l.mu.Lock()
		defer r.l.mu.Unlock()
		return r.l.job == nil
	})
}

// armed waits until the activation loop has armed the timer of its work
// due after wait, as a retry's: the loop reads the clock, then arms the
// timer for what is left, so an advance between the two would arm it that
// much late and the work would never come due.
func (r *rig) armed(wait time.Duration) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	at := r.clk.Now().Add(wait)
	if err := r.clk.BlockUntilDue(ctx, at); err != nil {
		r.t.Fatalf("no timer armed for %v: %v", at.Sub(epoch()), err)
	}
}

// active returns the published snapshot's Revision.
func (r *rig) active() revision.Digest {
	if s := r.holder.Active(); s != nil {
		return s.Revision.Digest
	}
	return revision.Digest{}
}

// waitActive waits until content's Revision is published.
func (r *rig) waitActive(content string) {
	r.t.Helper()
	d := rev(content).Digest
	waitFor(r.t, "Revision "+content+" to activate", func() bool { return r.active() == d && r.l.Active() == d })
	r.idle()
}

// writeLKG writes content under lkg/ as candidate and Last-Known-Good.
func (r *rig) writeLKG(content string) {
	r.t.Helper()
	if err := r.store.Write(r.lock, rev(content), true); err != nil {
		r.t.Fatal(err)
	}
}

// reasons returns the /readyz reasons as "reason/code".
func (r *rig) reasons() []string {
	var out []string
	for _, rr := range r.ready.Readyz().Reasons {
		out = append(out, rr.Reason+"/"+rr.Code)
	}
	return out
}
