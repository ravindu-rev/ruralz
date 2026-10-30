// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/composition"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret/resolver"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/manager"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/aggregate"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// t0 is the fake time of every harness.
var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// toolchain is the configuration pipeline and the CEL compiler the tests
// share: compiling the embedded schema views takes a while.
type toolchain struct {
	pipe *pipeline.Pipeline
	cel  *cel.Compiler
}

var sharedToolchain = sync.OnceValues(func() (*toolchain, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	// No offline checks of the Policy types: the fake Factories decide
	// what a Build refuses.
	p, err := pipeline.New(pipeline.Options{Checks: hub.Checks{}, CEL: c, Workers: 1})
	if err != nil {
		return nil, err
	}
	return &toolchain{pipe: p, cel: c}, nil
})

// harness compiles Revisions as a Node's activation does: a real
// configuration pipeline, CEL compiler, secret resolver, State Store
// manager, Upstream manager, composition engine and meter, and a fake
// Factory for every served Policy type. Each piece records what the
// compiler asked of it.
type harness struct {
	t      *testing.T
	tc     *toolchain
	clk    *clocktest.Fake
	env    *envTable
	cel    *recordingCEL
	res    *resolver.Resolver
	opens  *openers
	mgr    *manager.Manager
	stores *recordingStores
	ups    *upstream.Manager
	meter  *recordingMeter
	logs   *logRecorder
	facs   map[v1alpha1.PolicyType]*fakeFactory
	deps   Deps
	comp   *Compiler
}

// newHarness returns a harness whose Deps edit may change before New
// builds the Compiler.
func newHarness(t *testing.T, edit ...func(*Deps)) *harness {
	t.Helper()
	tc, err := sharedToolchain()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t: t, tc: tc, clk: clocktest.New(t0), env: &envTable{m: map[string]string{}},
		opens: &openers{fail: map[statestore.Role]error{}}, logs: &logRecorder{},
		facs: map[v1alpha1.PolicyType]*fakeFactory{},
	}
	h.cel = &recordingCEL{Compiler: tc.cel, fail: map[string]string{}}
	h.res, err = resolver.NewResolver(resolver.Config{Root: t.TempDir(), LookupEnv: h.env.lookup, Clock: h.clk})
	if err != nil {
		t.Fatal(err)
	}
	h.mgr = manager.New(t.Context(), manager.Config{
		Deps:    statestore.Deps{Clock: h.clk},
		Openers: manager.Openers{Memory: h.opens.memory, Redis: h.opens.redis},
	})
	t.Cleanup(func() { _ = h.mgr.Close(context.Background()) })
	h.stores = &recordingStores{inner: h.mgr}
	h.ups = upstream.NewManager(upstream.Options{Clock: h.clk, Rand: fixedSource{}, Dial: noDial})
	meter, err := aggregate.New(aggregate.Options{Clock: h.clk, Stripes: 1})
	if err != nil {
		t.Fatal(err)
	}
	h.meter = &recordingMeter{Meter: meter}
	factories := map[v1alpha1.PolicyType]filter.Factory{}
	for _, e := range registry.New().Entries() {
		if e.Served {
			f := &fakeFactory{typ: e.Type, behave: map[string]behavior{}}
			h.facs[e.Type] = f
			factories[e.Type] = f
		}
	}
	h.deps = Deps{
		Pipeline: tc.pipe, CEL: h.cel, Filters: filter.NewRegistry(factories), Secrets: h.res,
		LookupEnv: h.env.lookup, StateStores: h.stores, Upstreams: h.ups,
		Composition: composition.New(composition.Deps{Clock: h.clk}), Meter: h.meter,
		Status: &nopStatus{}, Clock: h.clk, Logger: slog.New(h.logs), Replayer: nopReplayer{}, Rand: fixedSource{},
		Workers: 2,
	}
	for _, e := range edit {
		e(&h.deps)
	}
	h.comp, err = New(h.deps)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// validate runs the pipeline on the YAML documents docs, one file
// ruralz.yaml, and fails the test unless it validates.
func (h *harness) validate(docs ...string) *pipeline.Result {
	h.t.Helper()
	fs := fstest.MapFS{"ruralz.yaml": {Data: []byte(strings.Join(docs, "---\n"))}}
	res, err := h.tc.pipe.Run(h.t.Context(), pipeline.Request{Source: loader.Source{FS: fs, Path: "."}})
	if err != nil {
		h.t.Fatalf("pipeline: %v", err)
	}
	if res.Validated == nil {
		h.t.Fatalf("the Bundle does not validate:\n%s", text(res.Diagnostics))
	}
	return res
}

// compile compiles res over the active snapshot prev.
func (h *harness) compile(res *pipeline.Result, prev *snapshot.Snapshot) (*Compiled, diag.List, error) {
	return h.comp.Compile(h.t.Context(), Input{Validated: res.Validated, Files: res.Files, Previous: prev})
}

// mustCompile compiles res over prev and fails the test unless it
// succeeds. The test's end discards the result, which an Activate turns
// into nothing.
func (h *harness) mustCompile(res *pipeline.Result, prev *snapshot.Snapshot) *Compiled {
	h.t.Helper()
	c, ds, err := h.compile(res, prev)
	if err != nil || len(ds) > 0 {
		h.t.Fatalf("Compile = %v\n%s", err, text(ds))
	}
	h.t.Cleanup(func() {
		c.Discard()
		// After an Activate the resources are the retirer's, so the test's.
		_ = c.Upstreams.Close()
		c.opened.handles.Release()
	})
	return c
}

// rejects compiles res and returns the findings in text form, failing the
// test unless the compile rejects res with findings alone.
func (h *harness) rejects(res *pipeline.Result) []string {
	h.t.Helper()
	c, ds, err := h.compile(res, nil)
	if err != nil || c != nil || len(ds) == 0 {
		h.t.Fatalf("Compile = %v, %v, %d findings; want findings alone", c, err, len(ds))
	}
	for _, d := range ds {
		if d.Severity != diag.SeverityError || !strings.HasPrefix(d.Code, "RZ-CFG-") {
			h.t.Errorf("finding %s is not an RZ-CFG error", d.AppendText(nil))
		}
	}
	return lines(ds)
}

// builds returns every BuildEnv the Factories got, in Policy name and
// scope order.
func (h *harness) builds() []filter.BuildEnv {
	var out []filter.BuildEnv
	for _, f := range h.facs {
		out = append(out, f.envsOf("")...)
	}
	sortEnvs(out)
	return out
}

// buildsOf returns the BuildEnvs of Policy name, in scope order.
func (h *harness) buildsOf(name string) []filter.BuildEnv {
	var out []filter.BuildEnv
	for _, f := range h.facs {
		out = append(out, f.envsOf(name)...)
	}
	sortEnvs(out)
	return out
}

// built returns every fake Filter the Factories built.
func (h *harness) built() []*fakeFilter {
	var out []*fakeFilter
	for _, f := range h.facs {
		f.mu.Lock()
		out = append(out, f.built...)
		f.mu.Unlock()
	}
	return out
}

// yield lets other goroutines run while a test waits for one.
func yield() { runtime.Gosched() }

// behave sets how the Factory of typ builds Policy name.
func (h *harness) behave(typ v1alpha1.PolicyType, name string, b behavior) {
	f := h.facs[typ]
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behave[name] = b
}

// released reports whether the State Store handles of the last Acquire are
// all released: the Manager opens a new driver for that Spec only once no
// snapshot holds the old one.
func (h *harness) released() bool {
	h.t.Helper()
	spec, ok := h.stores.last()
	if !ok {
		h.t.Fatal("no State Store Acquire")
	}
	before := h.opens.count()
	hs, err := h.mgr.Acquire(h.t.Context(), spec)
	if err != nil {
		h.t.Fatalf("Acquire: %v", err)
	}
	hs.Release()
	return h.opens.count() > before
}

// runtimes returns the names of the Upstream runtimes the Upstream
// manager holds.
func (h *harness) runtimes() []string {
	var out []string
	for _, s := range h.ups.Status() {
		out = append(out, s.Name)
	}
	return out
}

// sortEnvs orders BuildEnvs by Policy name, scope and Phase set.
func sortEnvs(envs []filter.BuildEnv) {
	key := func(e filter.BuildEnv) attachKey {
		return attachKey{policy: e.Policy.Metadata.Name, scope: e.Scope, phases: e.Phases}
	}
	slices.SortStableFunc(envs, func(a, b filter.BuildEnv) int { return compareKeys(key(a), key(b)) })
}

// text renders diagnostics one per line.
func text(ds diag.List) string { return strings.Join(lines(ds), "\n") }

// lines renders each diagnostic as its text form.
func lines(ds diag.List) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = string(d.AppendText(nil))
	}
	return out
}

// envTable is the fake process environment of the secret resolver and the
// compiler.
type envTable struct {
	mu sync.Mutex
	m  map[string]string
}

func (e *envTable) set(k, v string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.m[k] = v
}

func (e *envTable) lookup(k string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.m[k]
	return v, ok
}

// openers opens the State Store drivers: the memory driver, and fake
// redis drivers recording each Config, or failing with fail[role].
type openers struct {
	mu      sync.Mutex
	n       int
	configs []statestore.Config
	fail    map[statestore.Role]error
}

func (o *openers) memory(ctx context.Context, cfg statestore.Config, deps statestore.Deps) (statestore.Driver, error) {
	o.mu.Lock()
	o.n++
	o.configs = append(o.configs, cfg)
	o.mu.Unlock()
	return memory.Open(ctx, cfg, deps)
}

func (o *openers) redis(_ context.Context, cfg statestore.Config, _ statestore.Deps) (statestore.Driver, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.configs = append(o.configs, cfg)
	if err := o.fail[cfg.Role]; err != nil {
		return nil, err
	}
	o.n++
	return &statestoretest.Fake{}, nil
}

// count returns how many drivers opened.
func (o *openers) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n
}

// opened returns the Configs of every open, failed ones included.
func (o *openers) opened() []statestore.Config {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]statestore.Config(nil), o.configs...)
}

// recordingStores records each Spec the compiler acquires; it calls
// before, when set, first, and fails with err when set.
type recordingStores struct {
	inner  StateStores
	mu     sync.Mutex
	specs  []manager.Spec
	err    error
	before func()
}

func (s *recordingStores) Acquire(ctx context.Context, spec manager.Spec) (manager.Handles, error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	err, before := s.err, s.before
	s.mu.Unlock()
	if before != nil {
		before()
	}
	if err != nil {
		return manager.Handles{}, err
	}
	return s.inner.Acquire(ctx, spec)
}

// calls returns how many times Acquire ran.
func (s *recordingStores) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.specs)
}

// last returns the last Spec acquired.
func (s *recordingStores) last() (manager.Spec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.specs) == 0 {
		return manager.Spec{}, false
	}
	return s.specs[len(s.specs)-1], true
}

// recordingMeter records each Shape it admits.
type recordingMeter struct {
	emit.Meter
	mu     sync.Mutex
	shapes []emit.Shape
}

func (m *recordingMeter) Admit(s emit.Shape) (emit.Plan, error) {
	m.mu.Lock()
	m.shapes = append(m.shapes, s)
	m.mu.Unlock()
	return m.Meter.Admit(s)
}

// lastShape returns the last Shape admitted.
func (m *recordingMeter) lastShape(t *testing.T) emit.Shape {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.shapes) == 0 {
		t.Fatal("no Admit")
	}
	return m.shapes[len(m.shapes)-1]
}

// recordingCEL wraps the CEL compiler: it records the previous program set
// and input caps of each Builder and the Routes and Consumers prepared,
// and its Builders refuse each source in fail with an Issue of its code.
type recordingCEL struct {
	expr.Compiler
	mu        sync.Mutex
	caps      []expr.InputCaps
	prevs     []expr.ProgramSet
	routes    []*expr.Route
	consumers []*expr.Consumer
	compiled  []compiledSrc
	fail      map[string]string
}

// compiledSrc is one source a Builder compiled, at its site.
type compiledSrc struct {
	site expr.Site
	src  string
}

// sitesOf returns the sites the Builders compiled src at.
func (c *recordingCEL) sitesOf(src string) []expr.Site {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []expr.Site
	for _, x := range c.compiled {
		if x.src == src {
			out = append(out, x.site)
		}
	}
	return out
}

func (c *recordingCEL) NewBuilder(prev expr.ProgramSet, caps expr.InputCaps) expr.Builder {
	c.mu.Lock()
	c.caps = append(c.caps, caps)
	c.prevs = append(c.prevs, prev)
	c.mu.Unlock()
	return &failingBuilder{Builder: c.Compiler.NewBuilder(prev, caps), c: c}
}

func (c *recordingCEL) PrepareRoute(r *expr.Route) {
	c.mu.Lock()
	c.routes = append(c.routes, r)
	c.mu.Unlock()
	c.Compiler.PrepareRoute(r)
}

func (c *recordingCEL) PrepareConsumer(x *expr.Consumer) {
	c.mu.Lock()
	c.consumers = append(c.consumers, x)
	c.mu.Unlock()
	c.Compiler.PrepareConsumer(x)
}

// refuse makes the Builders refuse src with code, or with no Issue for an
// empty code.
func (c *recordingCEL) refuse(src, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail[src] = code
}

func (c *recordingCEL) failure(src string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	code, ok := c.fail[src]
	return code, ok
}

// lastCaps returns the input caps of the last Builder.
func (c *recordingCEL) lastCaps(t *testing.T) (expr.InputCaps, expr.ProgramSet) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.caps) == 0 {
		t.Fatal("no Builder")
	}
	return c.caps[len(c.caps)-1], c.prevs[len(c.prevs)-1]
}

// failingBuilder is a Builder that refuses the sources its recordingCEL
// names.
type failingBuilder struct {
	expr.Builder
	c *recordingCEL
}

// refusedMessage is the message of a CEL field the test Builders refuse.
const refusedMessage = "CEL refused by the test"

func (b *failingBuilder) Compile(site expr.Site, src string) (expr.Program, []expr.Issue) {
	b.c.mu.Lock()
	b.c.compiled = append(b.c.compiled, compiledSrc{site: site, src: src})
	b.c.mu.Unlock()
	code, ok := b.c.failure(src)
	switch {
	case !ok:
		return b.Builder.Compile(site, src)
	case code == "":
		return nil, nil
	default:
		return nil, []expr.Issue{{Code: code, Offset: -1, Message: refusedMessage}}
	}
}

// behavior is how a fakeFactory builds one Policy.
type behavior struct {
	// err is Build's error.
	err error
	// panic is the value Build panics with.
	panic any
	// none returns neither a Filter nor an error.
	none bool
	// block waits for ctx to end and returns its error.
	block bool
	// reuse returns BuildEnv.Previous when it is set.
	reuse bool
	// share returns one Filter for every attachment of the Policy.
	share bool
	// unhashable returns a Filter that cannot be compared.
	unhashable bool
	// plain returns a Filter that holds no resources (no Close).
	plain bool
	// closePanics makes the Filter's Close panic.
	closePanics bool
}

// fakeFactory is the Factory of one Policy type: it records every
// BuildEnv and builds a fake Filter per behavior, a cacheFilter for cache
// and a limitFilter for ratelimit and quota.
type fakeFactory struct {
	typ    v1alpha1.PolicyType
	mu     sync.Mutex
	envs   []filter.BuildEnv
	built  []*fakeFilter
	behave map[string]behavior
	shared map[string]filter.Filter
}

func (f *fakeFactory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	name := env.Policy.Metadata.Name
	f.mu.Lock()
	f.envs = append(f.envs, env)
	b := f.behave[name]
	if b.share && f.shared[name] != nil {
		flt := f.shared[name]
		f.mu.Unlock()
		return flt, nil
	}
	f.mu.Unlock()
	switch {
	case b.panic != nil:
		panic(b.panic)
	case b.err != nil:
		return nil, b.err
	case b.none:
		return nil, nil
	case b.block:
		<-ctx.Done()
		return nil, ctx.Err()
	case b.reuse && env.Previous != nil:
		return env.Previous, nil
	case b.unhashable:
		return sliceFilter{closes: &atomic.Int32{}, phases: []phase.Phase{phase.OnRequestHeaders}}, nil
	case b.plain:
		return plainFilter{}, nil
	}
	base := &fakeFilter{policy: name, scope: env.Scope, closePanics: b.closePanics}
	f.mu.Lock()
	f.built = append(f.built, base)
	f.mu.Unlock()
	var flt filter.Filter = base
	switch f.typ {
	case v1alpha1.PolicyTypeCache:
		flt = &cacheFilter{fakeFilter: base}
	case v1alpha1.PolicyTypeRateLimit, v1alpha1.PolicyTypeQuota:
		flt = &limitFilter{fakeFilter: base}
	default:
	}
	if b.share {
		f.mu.Lock()
		if f.shared == nil {
			f.shared = map[string]filter.Filter{}
		}
		f.shared[name] = flt
		f.mu.Unlock()
	}
	return flt, nil
}

// envsOf returns the BuildEnvs of Policy name, every one for "".
func (f *fakeFactory) envsOf(name string) []filter.BuildEnv {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []filter.BuildEnv
	for _, e := range f.envs {
		if name == "" || e.Policy.Metadata.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// fakeFilter is a built Filter that counts its Close calls.
type fakeFilter struct {
	policy      string
	scope       phase.Scope
	closePanics bool
	closes      atomic.Int32
}

func (*fakeFilter) Handle(context.Context, phase.Phase, filter.Exchange) filter.Result {
	return filter.Result{}
}

func (f *fakeFilter) Close() error {
	f.closes.Add(1)
	if f.closePanics {
		panic("fakeFilter: Close panics")
	}
	return nil
}

// cacheFilter is a fake Response Cache Filter.
type cacheFilter struct{ *fakeFilter }

func (*cacheFilter) Invalidate(context.Context, filter.Exchange) {}

// limitFilter is a fake consumptive Filter.
type limitFilter struct{ *fakeFilter }

func (*limitFilter) Prepare(context.Context, filter.Exchange, *statestore.Call) (filter.Result, bool) {
	return filter.Result{}, true
}

func (*limitFilter) Complete(context.Context, filter.Exchange, *statestore.Call) filter.Result {
	return filter.Result{}
}

func (*limitFilter) Undo(filter.Exchange) {}

// sliceFilter is a Filter value that cannot be compared.
type sliceFilter struct {
	closes *atomic.Int32
	phases []phase.Phase
}

func (sliceFilter) Handle(context.Context, phase.Phase, filter.Exchange) filter.Result {
	return filter.Result{}
}

func (f sliceFilter) Close() error {
	f.closes.Add(1)
	return nil
}

// plainFilter is a Filter that holds no resources.
type plainFilter struct{}

func (plainFilter) Handle(context.Context, phase.Phase, filter.Exchange) filter.Result {
	return filter.Result{}
}

// fakeOf returns the fakeFilter of a built Filter, or nil.
func fakeOf(f filter.Filter) *fakeFilter {
	switch v := f.(type) {
	case *fakeFilter:
		return v
	case *cacheFilter:
		return v.fakeFilter
	case *limitFilter:
		return v.fakeFilter
	default:
		return nil
	}
}

// fixedSource is a rand.Source that always draws 0.
type fixedSource struct{}

func (fixedSource) Uint64() uint64 { return 0 }

// errNoDial is the error of every dial: the tests never forward.
var errNoDial = errors.New("compile test: no dialing")

// noDial refuses every connection.
func noDial(context.Context, string, string) (net.Conn, error) { return nil, errNoDial }

// nopStatus is an emit.NodeStatus that records nothing.
type nopStatus struct{ emit.NodeStatus }

// nopReplayer is a filter.Replayer that replays nothing.
type nopReplayer struct{}

func (nopReplayer) Replay(context.Context, string, string, *http.Request) (*http.Response, error) {
	return nil, filter.ErrNotAvailable
}

// logRecorder is a slog.Handler that keeps every record's message.
type logRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (*logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, r.Message)
	return nil
}

func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l *logRecorder) WithGroup(string) slog.Handler { return l }

// messages returns the messages logged.
func (l *logRecorder) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.msgs...)
}
