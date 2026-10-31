// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"cmp"
	"errors"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// codeLimit is RZ-CFG-001, which the pipeline's diag.Collector appends at
// the cap; stage G never reports it.
const codeLimit = "RZ-CFG-001"

// hostileQuota returns a quota Policy named name whose open config member
// holds n numbers outside the double range: n RZ-CFG-005 diagnostics in
// one resource that passes stage F.
func hostileQuota(name string, n int) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "` + name +
		`"}, "spec": {"type": "quota", "config": {"consumerQuota": "c", "x": [`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("1e999")
	}
	b.WriteString(`]}}}`)
	return b.String()
}

// stageWith returns a Stage of the embedded schemas with options o.
func stageWith(t testing.TB, o Options) *Stage {
	t.Helper()
	o.Schemas, o.Registry = schemas(t), registry.New()
	s, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// hostileRun returns resources of the hostile quota Policies q0 to q<n-1>,
// each in its own file and with each errors.
func hostileRun(t testing.TB, n, each int) (*fixture, []*tree.Resource) {
	t.Helper()
	f := newFixture()
	rs := make([]*tree.Resource, n)
	for i := range rs {
		name := "q" + strconv.Itoa(i)
		rs[i] = f.resource(t, name+".json", hostileQuota(name, each))
	}
	return f, rs
}

// fullyDiagnosed returns the first n diagnostics, in diag.List.Sort
// order, of all the diagnostics of a hostile run: the definition of the
// result of Run at a cap of n-1.
func fullyDiagnosed(t *testing.T, resources, each, n int) []string {
	t.Helper()
	f, rs := hostileRun(t, resources, each)
	_, all, err := stageWith(t, Options{MaxDiagnostics: resources * each}).Run(t.Context(), rs, f.files, 4)
	if err != nil || len(all) != resources*each {
		t.Fatalf("Run with a cap above every diagnostic = %d diagnostics, %v; want %d", len(all), err, resources*each)
	}
	sorted := slices.Clone(all)
	sorted.Sort()
	return texts(sorted[:min(n, len(sorted))])
}

// TestRunCapComposesWithCollector checks that, at the cap, Run returns the
// cap plus one diagnostics and no RZ-CFG-001 of its own, so the pipeline's
// diag.Collector of the same cap, empty or already holding earlier stages'
// findings, ends with exactly one RZ-CFG-001; Resource does the same for
// one resource, in tree order.
func TestRunCapComposesWithCollector(t *testing.T) {
	f, rs := hostileRun(t, 3, 5000)
	for _, workers := range []int{1, 4} {
		_, ds, err := newStage(t).Run(t.Context(), rs, f.files, workers)
		if err != nil || len(ds) != DefaultMaxDiagnostics+1 || limits(ds) != 0 {
			t.Fatalf("workers %d: Run = %d diagnostics with %d RZ-CFG-001, %v; want %d and none", workers, len(ds), limits(ds), err, DefaultMaxDiagnostics+1)
		}
		for _, earlier := range []int{0, 3} {
			c := diag.NewCollector(DefaultMaxDiagnostics)
			for i := range earlier {
				c.Add(diag.Diagnostic{Code: "RZ-CFG-013", Severity: diag.SeverityWarning, Message: "earlier " + strconv.Itoa(i)})
			}
			c.AddAll(ds)
			got := c.List()
			if !c.Full() || len(got) != DefaultMaxDiagnostics+1 || limits(got) != 1 {
				t.Errorf("workers %d, %d earlier: Collector = %d diagnostics with %d RZ-CFG-001, full %v; want %d with one", workers, earlier, len(got), limits(got), c.Full(), DefaultMaxDiagnostics+1)
			}
		}
		// Every resource ran: the cap does not stop the run.
		for _, r := range rs {
			if _, materialized := lookup(r.Root, "spec.slot"); !materialized {
				t.Errorf("workers %d: %s was not processed", workers, r.ID)
			}
		}
	}
	h, ds := newStage(t).Resource(newFixture().resource(t, "q.json", hostileQuota("q", DefaultMaxDiagnostics+5)), nil)
	if h != nil || len(ds) != DefaultMaxDiagnostics+1 || limits(ds) != 0 ||
		ds[0].Path.String() != "spec.config.x[0]" || ds[len(ds)-1].Path.String() != "spec.config.x["+strconv.Itoa(DefaultMaxDiagnostics)+"]" {
		t.Errorf("Resource = %v, %d diagnostics from %s to %s", h, len(ds), ds[0].Path, ds[len(ds)-1].Path)
	}
	c := diag.NewCollector(DefaultMaxDiagnostics)
	c.AddAll(ds)
	if got := c.List(); limits(got) != 1 {
		t.Errorf("Collector of Resource = %d RZ-CFG-001", limits(got))
	}
}

// TestRunCapDeterministic is the determinism property at the cap: with 1,
// 2 and 8 workers, run after run and in reverse input order, Run keeps the
// same diagnostics, the first cap plus one in diag.List.Sort order of
// everything the resources reported.
func TestRunCapDeterministic(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		cap, resources, each int
		runs                 int
	}{
		{"default cap", 0, 40, DefaultMaxDiagnostics / 8, 1},
		{"small cap repeated", 500, 40, 50, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit := cmp.Or(tc.cap, DefaultMaxDiagnostics)
			want := fullyDiagnosed(t, tc.resources, tc.each, limit+1)
			s := stageWith(t, Options{MaxDiagnostics: tc.cap})
			for _, workers := range []int{1, 2, 8} {
				for run := range tc.runs {
					f, rs := hostileRun(t, tc.resources, tc.each)
					if run%2 == 1 {
						slices.Reverse(rs)
					}
					b, ds, err := s.Run(t.Context(), rs, f.files, workers)
					if err != nil || b != nil {
						t.Fatalf("workers %d run %d: Run = %v, %v", workers, run, b, err)
					}
					if got := texts(ds); !slices.Equal(got, want) {
						t.Fatalf("workers %d run %d: %d diagnostics, last %q; want %d, last %q",
							workers, run, len(got), got[len(got)-1], len(want), want[len(want)-1])
					}
				}
			}
		})
	}
}

// TestRunCapOptions covers Options.MaxDiagnostics: a cap below and one
// above the default are honored by Run and Resource, a run of exactly the
// cap is complete, and a negative cap is refused.
func TestRunCapOptions(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		cap, resources, each int
		want                 int
	}{
		{"below the default", 100, 3, 80, 101},
		{"above the default", 12_000, 2, 7000, 12_001},
		{"exactly the cap", 100, 2, 50, 100},
		{"one above the cap", 100, 1, 101, 101},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, rs := hostileRun(t, tc.resources, tc.each)
			s := stageWith(t, Options{MaxDiagnostics: tc.cap})
			_, ds, err := s.Run(t.Context(), rs, f.files, 3)
			if err != nil || len(ds) != tc.want {
				t.Fatalf("Run = %d diagnostics, %v; want %d", len(ds), err, tc.want)
			}
			if got, want := texts(ds), fullyDiagnosed(t, tc.resources, tc.each, tc.cap+1); !slices.Equal(got, want) {
				t.Errorf("Run kept other diagnostics than the first %d in sorted order", tc.cap+1)
			}
			c := diag.NewCollector(tc.cap)
			c.AddAll(ds)
			if got, wantLimits := limits(c.List()), tc.want-tc.cap; got != wantLimits {
				t.Errorf("Collector(%d) = %d RZ-CFG-001, want %d", tc.cap, got, wantLimits)
			}
			// One resource keeps its first cap+1 in tree order.
			one := newFixture().resource(t, "one.json", hostileQuota("one", tc.cap+7))
			if _, ds := s.Resource(one, nil); len(ds) != tc.cap+1 || ds[tc.cap].Path.String() != "spec.config.x["+strconv.Itoa(tc.cap)+"]" {
				t.Errorf("Resource = %d diagnostics, want %d ending at x[%d]", len(ds), tc.cap+1, tc.cap)
			}
		})
	}
	if _, err := New(Options{Schemas: schemas(t), Registry: registry.New(), MaxDiagnostics: -1}); !errors.Is(err, ErrOptions) {
		t.Errorf("New(MaxDiagnostics -1) error = %v, want ErrOptions", err)
	}
	if s := newStage(t); s.maxDiagnostics != DefaultMaxDiagnostics {
		t.Errorf("default cap = %d", s.maxDiagnostics)
	}
}

// limits counts the RZ-CFG-001 diagnostics of ds.
func limits(ds diag.List) int {
	n := 0
	for _, d := range ds {
		if d.Code == codeLimit {
			n++
		}
	}
	return n
}

// hubFunc is a hub converter whose ToHub is toHub, for the stage's
// handling of converter results.
type hubFunc struct {
	toHub func(doc *tree.Node) (*tree.Node, diag.List)
}

func (hubFunc) APIVersion() string { return v1 }

func (c hubFunc) ToHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) { return c.toHub(doc) }

func (hubFunc) FromHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) { return doc, nil }

// convertedStage returns a Stage whose hub converter is toHub.
func convertedStage(t *testing.T, o Options, toHub func(doc *tree.Node) (*tree.Node, diag.List)) *Stage {
	t.Helper()
	conv, err := convert.NewRegistry(convert.Lifecycle{}, hubFunc{toHub: toHub})
	if err != nil {
		t.Fatal(err)
	}
	o.Converters = conv
	return stageWith(t, o)
}

// warnings returns n converter warnings.
func warnings(n int) diag.List {
	var ds diag.List
	for i := range n {
		ds = append(ds, diag.Diagnostic{Code: "RZ-CFG-025", Severity: diag.SeverityWarning, Message: "warning " + strconv.Itoa(i)})
	}
	return ds
}

// TestDroppedErrorsStillReject checks that every failure path of a
// resource rejects it when its error diagnostic is dropped by the
// resource's budget: an object-less document, an unserved apiVersion, a
// failed conversion, a normalization error, a decode error and an
// unknown kind; a clean resource is still accepted. At the cap, Run
// returns no Bundle even when every diagnostic it kept is a warning.
func TestDroppedErrorsStillReject(t *testing.T) {
	s := newStage(t)
	f := newFixture()
	w := s.newWork()
	w.keep = 0
	for _, src := range []string{
		`["x"]`,
		`{"apiVersion": "ruralz/v9", "kind": "Route", "metadata": {"name": "r"}, "spec": {}}`,
		hostileQuota("q", 3),
		route(`{"match": {}, "upstreams": [{"name": "u", "weight": 4294967296}]}`),
	} {
		if h, ds := s.resource(f.resource(t, "r.json", src), f.files, w); h != nil || len(ds) != 0 {
			t.Errorf("no budget: Resource(%.40s) = %v, %q; want nil and nothing recorded", src, h, texts(ds))
		}
	}
	failing := convertedStage(t, Options{}, func(doc *tree.Node) (*tree.Node, diag.List) {
		return doc, diag.List{{Code: CodeSchema, Severity: diag.SeverityError, Message: "cannot convert"}}
	})
	if h, ds := failing.resource(f.resource(t, "c.json", route(`{"match": {}}`)), f.files, w); h != nil || len(ds) != 0 {
		t.Errorf("no budget: failed conversion = %v, %q", h, texts(ds))
	}
	if h, ds := s.resource(f.resource(t, "ok.json", route(`{"match": {}}`)), f.files, w); h == nil || len(ds) != 0 {
		t.Errorf("clean resource without a budget = %v, %q", h, texts(ds))
	}
	w.begin()
	if obj, cfg, ds, failed := decode(hubIndex(t), nil, &tree.Resource{ID: tree.ID{Kind: "Nope"}, Root: mustParse(t, `{}`)}, nil, w, nil); obj != nil || cfg != nil || len(ds) != 0 || !failed {
		t.Errorf("decode of an unknown kind without a budget = %v %v %q %v", obj, cfg, texts(ds), failed)
	}
	// Two converter warnings fill a cap of one plus one: the decode error
	// after them is dropped, and the resource and the run still fail.
	warned := convertedStage(t, Options{MaxDiagnostics: 1}, func(doc *tree.Node) (*tree.Node, diag.List) { return doc, warnings(2) })
	bad := f.resource(t, "d.json", route(`{"match": {}, "upstreams": [{"name": "u", "weight": 4294967296}]}`))
	if h, ds := warned.Resource(bad, f.files); h != nil || len(ds) != 2 || ds.HasErrors() {
		t.Errorf("warnings then a dropped error: Resource = %v, %q", h, texts(ds))
	}
	bad = f.resource(t, "d.json", route(`{"match": {}, "upstreams": [{"name": "u", "weight": 4294967296}]}`))
	good := f.resource(t, "g.json", route(`{"match": {}}`))
	if b, ds, err := warned.Run(t.Context(), []*tree.Resource{good, bad}, f.files, 2); b != nil || err != nil || len(ds) != 2 || ds.HasErrors() {
		t.Errorf("Run with a dropped error = %v, %q, %v; want no Bundle and the two kept warnings", b, texts(ds), err)
	}
	// Warnings alone keep the resource and the Bundle.
	good = f.resource(t, "g.json", route(`{"match": {}}`))
	if b, ds, err := warned.Run(t.Context(), []*tree.Resource{good}, f.files, 1); b == nil || err != nil || len(ds) != 2 {
		t.Errorf("Run with warnings only = %v, %q, %v; want the Bundle", b, texts(ds), err)
	}
}

// TestKeeper covers the keeper of Run: with room for all, it returns
// the diagnostics in the order diag.List.Sort gives the concatenation
// in resource order, and with room for n, the first n of them,
// whatever the order and the goroutines that add them.
func TestKeeper(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 9)) //nolint:gosec // G404: a seeded corpus makes the property reproducible.
	pick := func(xs ...string) string { return xs[r.IntN(len(xs))] }
	lists := make([]diag.List, 30)
	var concat diag.List
	for i := range lists {
		for range r.IntN(40) {
			d := diag.Diagnostic{
				Code: pick("RZ-CFG-005", "RZ-CFG-007"), Severity: diag.SeverityError,
				Location:    diag.Location{File: pick("", "a.json", "b.json"), Line: r.IntN(3), Column: r.IntN(3)},
				Path:        diag.Path{diag.Field(pick("a", "b"))},
				Message:     pick("x", "y"),
				Environment: pick("", "prod"),
				// Hint is outside the sort key: it shows the tie order.
				Hint:     strconv.Itoa(i),
				Resource: &diag.ResourceID{Kind: "Route", Name: "r" + strconv.Itoa(i)},
			}
			if r.IntN(4) == 0 {
				d.Path = append(d.Path, diag.Index(r.IntN(3)))
			}
			lists[i] = append(lists[i], d)
		}
		concat = append(concat, lists[i]...)
	}
	want := slices.Clone(concat)
	want.Sort()
	render := func(ds diag.List) []string {
		out := make([]string, len(ds))
		for i, d := range ds {
			out[i] = string(d.AppendText(nil)) + " env=" + d.Environment + " hint=" + d.Hint
		}
		return out
	}
	for _, n := range []int{len(concat), len(concat) + 5, 1, 7, len(concat) / 3} {
		for _, order := range []string{"in order", "shuffled", "concurrent"} {
			k := newKeeper(n)
			idx := r.Perm(len(lists))
			if order == "in order" {
				slices.Sort(idx)
			}
			if order == "concurrent" {
				var wg sync.WaitGroup
				for _, i := range idx {
					wg.Go(func() { k.add(i, lists[i]) })
				}
				wg.Wait()
			} else {
				for _, i := range idx {
					k.add(i, lists[i])
				}
			}
			if got, w := render(k.list()), render(want[:min(n, len(want))]); !slices.Equal(got, w) {
				t.Errorf("n %d %s: keeper\n%s\nwant\n%s", n, order, strings.Join(got, "\n"), strings.Join(w, "\n"))
			}
		}
	}
	if got := newKeeper(3).list(); got != nil {
		t.Errorf("empty keeper = %v", got)
	}
	if k := newKeeper(0); k.n != 1 {
		t.Errorf("newKeeper(0) keeps %d", k.n)
	}
}

// TestWorkAdmit covers the per-resource budget helpers.
func TestWorkAdmit(t *testing.T) {
	w := newWork()
	w.keep = 2
	w.begin()
	ds := diag.List{{Code: CodeSchema}, {Code: CodeSchema}, {Code: CodeUnserved}}
	if got := w.admit(ds); len(got) != 2 || w.take() {
		t.Errorf("admit = %d, then take %v; want 2, false", len(got), w.take())
	}
	w.begin()
	if !w.take() {
		t.Error("begin did not reset the budget")
	}
	if w := newWork(); w.keep != DefaultMaxDiagnostics+1 || w.left != w.keep {
		t.Errorf("default budget = %d, %d", w.keep, w.left)
	}
}

// fakeClock is a clock tests advance by hand; reads counts its reads.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	reads int
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// TestWorkYield covers the yield timer on the injected clock, never
// time.Now: the timer is read when a worker starts and every yieldCheck
// units, the worker yields once YieldAfter has passed on that clock since
// its last yield and then restarts its timer, not before, and without a
// clock it never reads one.
func TestWorkYield(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	s := stageWith(t, Options{Now: c.now, YieldAfter: time.Millisecond})
	w := s.newWork()
	if c.count() != 1 || !w.last.Equal(time.Unix(1000, 0)) {
		t.Fatalf("start: %d reads, last %v", c.count(), w.last)
	}
	for range yieldCheck - 1 {
		w.tick()
	}
	if c.count() != 1 {
		t.Fatalf("timer read before yieldCheck units: %d reads", c.count())
	}
	w.tick()
	if c.count() != 2 || w.yields != 0 || w.units != 0 {
		t.Fatalf("at yieldCheck units: %d reads, %d yields, units %d; want 2, 0, 0", c.count(), w.yields, w.units)
	}
	c.advance(time.Millisecond - 1)
	w.check()
	if w.yields != 0 {
		t.Error("yielded before YieldAfter had passed")
	}
	c.advance(1)
	w.check()
	if w.yields != 1 || !w.last.Equal(time.Unix(1000, 0).Add(time.Millisecond)) {
		t.Errorf("after YieldAfter: %d yields, last %v; want 1 and a restarted timer", w.yields, w.last)
	}
	w.check()
	if w.yields != 1 {
		t.Error("yielded again without new work time")
	}
	// The default YieldAfter is 100 µs.
	if d := stageWith(t, Options{Now: c.now}); d.yieldAfter != DefaultYieldAfter || DefaultYieldAfter != 100*time.Microsecond {
		t.Errorf("default YieldAfter = %v", d.yieldAfter)
	}
	if _, err := New(Options{Schemas: schemas(t), Registry: registry.New(), YieldAfter: -1}); !errors.Is(err, ErrOptions) {
		t.Errorf("New(YieldAfter -1) error = %v, want ErrOptions", err)
	}
	// Without a clock nothing is read and nothing yields.
	none := newStage(t).newWork()
	for range 10 * yieldCheck {
		none.tick()
	}
	none.check()
	if none.yields != 0 || none.now != nil {
		t.Errorf("no clock: %d yields", none.yields)
	}
	// A nil worker, as the test helpers pass, does nothing.
	var nilWork *work
	nilWork.tick()
	nilWork.check()
}

// alwaysDue returns a clock that moves a second at every read, so with a
// YieldAfter of one second every timer check yields, and the count of its
// reads.
func alwaysDue() (func() time.Time, *atomic.Int64) {
	var reads atomic.Int64
	return func() time.Time { return time.Unix(reads.Add(1), 0) }, &reads
}

// TestYieldBrackets covers the calls no worker can yield inside: an
// encoding/json call and a direct UnmarshalJSON call are each preceded
// and followed by a timer check.
func TestYieldBrackets(t *testing.T) {
	now, _ := alwaysDue()
	s := stageWith(t, Options{Now: now, YieldAfter: time.Second})
	w := s.newWork()
	var v any
	if err := w.decodeInto([]byte(`[1]`), &v); err != nil || w.yields != 2 || w.decodes != 1 {
		t.Errorf("decodeInto: %v, %d yields, %d calls; want 2 yields around 1 call", err, w.yields, w.decodes)
	}
	w = s.newWork()
	c := &chunker{w: w, limit: decodeChunk, bytes: decodeBytes}
	var d v1alpha1.Duration
	if err := c.unmarshaler(mustParse(t, `"5s"`), reflect.ValueOf(&d).Elem(), nil); err != nil || w.yields != 2 || w.unmarshals != 1 || d != v1alpha1.Duration(5*time.Second) {
		t.Errorf("unmarshaler: %v, %d yields, %d calls, %v; want 2 yields around 1 call", err, w.yields, w.unmarshals, d)
	}
}

// TestRunYields covers the yield timer through Run: each worker reads the
// injected clock when it starts, and checks its timer between resources
// and around each call it cannot yield inside: the clock reads are that
// count plus at most the reads the resources' work units add, one per
// yieldCheck units (bytes of text count), and Run's workers read it
// concurrently.
func TestRunYields(t *testing.T) {
	now, reads := alwaysDue()
	s := stageWith(t, Options{Now: now, YieldAfter: time.Second})
	src := route(`{"match": {}, "timeout": "5s"}`)
	const k = 3
	one := s.newWork()
	if h, ds := s.resource(envelope(mustParse(t, src)), nil, one); h == nil || len(ds) != 0 {
		t.Fatalf("Resource: %q", texts(ds))
	}
	if one.decodes == 0 {
		t.Fatalf("one resource makes %d calls; the test needs one", one.decodes)
	}
	calls := one.decodes + one.unmarshals
	rs := make([]*tree.Resource, k)
	for i := range rs {
		rs[i] = envelope(mustParse(t, src))
	}
	reads.Store(0)
	if b, ds, err := s.Run(t.Context(), rs, nil, 1); b == nil || err != nil || len(ds) != 0 {
		t.Fatalf("Run = %v, %q, %v", b, texts(ds), err)
	}
	// One read to start, two per yield: one per call bracket side and one
	// between resources, each due; and at most two per yieldCheck units
	// of the k resources' work (charge), plus one partial batch.
	least := int64(1 + 2*k*(2*calls+1))
	most := least + int64(2*(k*one.total/yieldCheck+1))
	if got := reads.Load(); got < least || got > most {
		t.Errorf("Run read the clock %d times, want %d to %d (%d calls per resource, %d units each)", got, least, most, calls, one.total)
	}
	// Several workers read the clock concurrently (the race detector checks).
	f, all := loadBundle(t)
	if _, ds, err := s.Run(t.Context(), all, f.files, 4); err != nil || len(ds) != 0 {
		t.Fatalf("Run = %q, %v", texts(ds), err)
	}
}
