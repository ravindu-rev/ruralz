// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// TestLimitDefaults covers the default limits: 64 MiB of source text,
// 20,000 resources, 64 nesting levels and 1,000,000 YAML nodes per
// document (target), 20,000 files, 1,000 ignore patterns and 10,000
// diagnostics, and max(1, GOMAXPROCS/2) workers yielding every 100 µs.
func TestLimitDefaults(t *testing.T) {
	l := newLoader(t, Options{})
	want := Limits{
		MaxSourceBytes: 64 << 20, MaxResources: 20_000, MaxFiles: 20_000, MaxDepth: 64,
		MaxDocumentNodes: 1_000_000, MaxIgnorePatterns: 1_000, MaxDiagnostics: 10_000,
	}
	if got := l.Limits(); got != want {
		t.Errorf("Limits = %+v\nwant %+v", got, want)
	}
	if l.workers < 1 || l.yieldAfter != 100*time.Microsecond {
		t.Errorf("workers %d, yieldAfter %v", l.workers, l.yieldAfter)
	}
	custom := Limits{MaxSourceBytes: 1, MaxResources: 2, MaxFiles: 3, MaxDepth: 4, MaxDocumentNodes: 5, MaxIgnorePatterns: 6, MaxDiagnostics: 7}
	if got := newLoader(t, Options{Limits: custom}).Limits(); got != custom {
		t.Errorf("custom Limits = %+v", got)
	}
}

// TestNegativeOptions covers New's checks: a negative limit, worker count
// or yield time is ErrOptions.
func TestNegativeOptions(t *testing.T) {
	base, err := shared()
	if err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Options{
		"MaxSourceBytes":    {Limits: Limits{MaxSourceBytes: -1}},
		"MaxResources":      {Limits: Limits{MaxResources: -1}},
		"MaxFiles":          {Limits: Limits{MaxFiles: -1}},
		"MaxDepth":          {Limits: Limits{MaxDepth: -1}},
		"MaxDocumentNodes":  {Limits: Limits{MaxDocumentNodes: -1}},
		"MaxIgnorePatterns": {Limits: Limits{MaxIgnorePatterns: -1}},
		"MaxDiagnostics":    {Limits: Limits{MaxDiagnostics: -1}},
		"Workers":           {Workers: -1},
		"YieldAfter":        {YieldAfter: -time.Second},
	} {
		o.Schemas, o.Views = base.schemas, base.views
		if _, err := New(o); !errors.Is(err, ErrOptions) {
			t.Errorf("%s: New error %v, want ErrOptions", name, err)
		}
	}
}

// TestMaxSourceBytes covers the source limit: every file a load reads
// counts, .ruralzignore and every overlay's files included, in path order;
// the first byte past the limit is RZ-CFG-001 at its line and column, and
// the load stops.
func TestMaxSourceBytes(t *testing.T) {
	m := mapFS(minimal())
	m[IgnoreFile] = &fstest.MapFile{Data: []byte("x\n")}
	m["a.yaml"] = &fstest.MapFile{Data: []byte(upstream("a"))}
	m["overlays/prod/b.yaml"] = &fstest.MapFile{Data: []byte("#é\n" + strings.Repeat("#", 20) + "\n")}
	total := int64(0)
	for _, f := range m {
		total += int64(len(f.Data))
	}
	r := load(t, newLoader(t, Options{Limits: Limits{MaxSourceBytes: total}}), m)
	wantClean(t, "at the limit", r.Diagnostics)
	// Path order: .ruralzignore, a.yaml, overlays/prod/b.yaml, ruralz.yaml.
	limit := int64(len(m[IgnoreFile].Data) + len(m["a.yaml"].Data) + len("#é\n#####"))
	r = load(t, newLoader(t, Options{Limits: Limits{MaxSourceBytes: limit}}), m)
	wantTexts(t, "past the limit", r.Diagnostics, "overlays/prod/b.yaml:2:6 error RZ-CFG-001: the source text passes the limit of "+strconv.FormatInt(limit, 10)+" bytes")
	if !r.Stopped || r.Resources != nil {
		t.Errorf("result: %+v", r)
	}
	r = load(t, newLoader(t, Options{Limits: Limits{MaxSourceBytes: 1}}), m)
	wantTexts(t, "ignore file past the limit", r.Diagnostics, ".ruralzignore:1:2 error RZ-CFG-001: the source text passes the limit of 1 bytes")

	one := fstest.MapFS{"r.yaml": {Data: []byte(gateway)}}
	res, err := newLoader(t, Options{Limits: Limits{MaxSourceBytes: 10}}).Load(t.Context(), Request{Source: Source{FS: one, Path: "r.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	wantTexts(t, "one file", res.Diagnostics, "r.yaml:1:11 error RZ-CFG-001: the source text passes the limit of 10 bytes")
}

// TestMaxFiles covers the file limit: the configuration files of the
// Bundle, overlays included, beyond the limit are RZ-CFG-001 at the first
// file past it in walk order, and the load stops.
func TestMaxFiles(t *testing.T) {
	m := mapFS(minimal())
	m["a.yaml"] = &fstest.MapFile{Data: []byte(upstream("a"))}
	m["notes.txt"] = &fstest.MapFile{Data: []byte("not configuration")}
	m["overlays/prod/b.yaml"] = &fstest.MapFile{Data: []byte(upstream("b"))}
	wantClean(t, "three files", load(t, newLoader(t, Options{Limits: Limits{MaxFiles: 3}}), m).Diagnostics)
	r := load(t, newLoader(t, Options{Limits: Limits{MaxFiles: 2}}), m)
	wantTexts(t, "two files", r.Diagnostics,
		"ruralz.yaml error RZ-CFG-001: the Bundle holds more than 2 configuration files (exclude files that are not configuration with .ruralzignore)")
	if !r.Stopped {
		t.Error("the load did not stop")
	}
}

// TestMaxResources covers the resource limit: the base union, one
// overlay's documents and the rendered Bundle each stay within it, else
// RZ-CFG-001 at the first resource past it, and the load stops.
func TestMaxResources(t *testing.T) {
	m := mapFS(minimal())
	m["a.yaml"] = &fstest.MapFile{Data: []byte(docs(upstream("a"), upstream("b")))}
	l := newLoader(t, Options{Limits: Limits{MaxResources: 3}})
	wantClean(t, "three", load(t, l, m).Diagnostics)
	l2 := newLoader(t, Options{Limits: Limits{MaxResources: 2}})
	r := load(t, l2, m)
	wantTexts(t, "base", r.Diagnostics, "a.yaml:9:1 error RZ-CFG-001: the Bundle holds more than 2 resources")
	if !r.Stopped || r.Resources != nil {
		t.Errorf("result: %+v", r)
	}

	m["overlays/prod/c.yaml"] = &fstest.MapFile{Data: []byte(upstream("c"))}
	r = loadEnv(t, l, m, environment("prod", " {}"), "prod")
	wantTexts(t, "rendered", r.Diagnostics, "overlays/prod/c.yaml:1:1 error RZ-CFG-001: the rendered Bundle holds more than 3 resources")
	if !r.Stopped || r.Resources != nil {
		t.Errorf("result: %+v", r)
	}

	m = mapFS(minimal())
	m["overlays/prod/c.yaml"] = &fstest.MapFile{Data: []byte(docs(upstream("a"), upstream("b"), upstream("c")))}
	r = loadEnv(t, l2, m, environment("prod", " {}"), "prod")
	wantTexts(t, "overlay", r.Diagnostics, "overlays/prod/c.yaml:18:1 error RZ-CFG-001: the overlay holds more than 2 documents")
}

// TestMaxDepthAndNodes covers the per-document limits the loader passes to
// the parser: nesting and YAML nodes past them are RZ-CFG-001.
func TestMaxDepthAndNodes(t *testing.T) {
	m := mapFS(minimal())
	m["deep.yaml"] = &fstest.MapFile{Data: []byte(upstream("deep") + "  x: [[[[1]]]]\n")}
	r := load(t, newLoader(t, Options{Limits: Limits{MaxDepth: 5}}), m)
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{CodeLimit}) || r.Diagnostics[0].File != "deep.yaml" {
		t.Errorf("depth: %q", texts(r.Diagnostics))
	}
	m["deep.yaml"] = &fstest.MapFile{Data: []byte(upstream("wide") + "  x: [" + strings.Repeat("1, ", 100) + "1]\n")}
	r = load(t, newLoader(t, Options{Limits: Limits{MaxDocumentNodes: 60}}), m)
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{CodeLimit}) || r.Diagnostics[0].File != "deep.yaml" {
		t.Errorf("nodes: %q", texts(r.Diagnostics))
	}
}

// TestMaxDiagnostics covers the cap of one run: past it, the first
// MaxDiagnostics findings in sort order and one RZ-CFG-001 "too many
// diagnostics", whatever the worker count, and the load stops.
func TestMaxDiagnostics(t *testing.T) {
	m := mapFS(minimal())
	for i := range 12 {
		m["f"+strconv.Itoa(i)+".yaml"] = &fstest.MapFile{Data: []byte("kind: Upstream\nmetadata:\n  name: u" + strconv.Itoa(i) + "\n")}
	}
	var first []string
	for _, workers := range []int{1, 2, 7} {
		r := load(t, newLoader(t, Options{Workers: workers, Limits: Limits{MaxDiagnostics: 5}}), m)
		got := texts(r.Diagnostics)
		if len(got) != 6 || got[0] != "- error RZ-CFG-001: too many diagnostics; stopped after 5" ||
			!strings.HasPrefix(got[1], "f0.yaml:1:1 error RZ-CFG-007") || !strings.HasPrefix(got[5], "f12.yaml") && !strings.HasPrefix(got[5], "f2.yaml") {
			t.Errorf("workers %d: %q", workers, got)
		}
		if !r.Stopped || r.Resources != nil {
			t.Errorf("workers %d: the load did not stop", workers)
		}
		if first == nil {
			first = got
		} else if !slices.Equal(got, first) {
			t.Errorf("workers %d: %q differ from one worker's %q", workers, got, first)
		}
	}
	r := load(t, newLoader(t, Options{Limits: Limits{MaxDiagnostics: 12}}), m)
	if len(r.Diagnostics) != 12 || r.Stopped {
		t.Errorf("at the cap: %d diagnostics, stopped %v", len(r.Diagnostics), r.Stopped)
	}
}

// TestHugeLimits covers limits large enough to mean "no limit", such as
// math.MaxInt64 bytes and math.MaxInt diagnostics: nothing overflows, and
// the Bundle loads as with the defaults.
func TestHugeLimits(t *testing.T) {
	l := newLoader(t, Options{Limits: Limits{
		MaxSourceBytes: math.MaxInt64, MaxResources: math.MaxInt, MaxFiles: math.MaxInt,
		MaxDepth: math.MaxInt, MaxDocumentNodes: math.MaxInt, MaxIgnorePatterns: math.MaxInt, MaxDiagnostics: math.MaxInt,
	}})
	m := overlayBundle()
	m[IgnoreFile] = &fstest.MapFile{Data: []byte("drafts/\n")}
	r := loadEnv(t, l, m, envs, "prod")
	wantClean(t, "huge limits", r.Diagnostics)
	if got := ids(r.Resources); !slices.Equal(got, []string{"Gateway/edge", "Upstream/orders"}) {
		t.Errorf("resources = %q", got)
	}
	k := newKeeper(math.MaxInt)
	k.add(stepParse, 0, diag.List{findingAt("a", 1, diag.SeverityError), findingAt("a", 2, diag.SeverityError)})
	if got := k.result(); len(got) != 2 || k.full() {
		t.Errorf("a keeper without a cap: %q, full %v", texts(got), k.full())
	}
}

// listFile returns a YAML file of one key and a flow list of n items: n+3
// nodes (the mapping, the key, the list and its items), item k at column
// 5+3(k-1) of line 1.
func listFile(key string, n int) string {
	return key + ": [" + strings.TrimSuffix(strings.Repeat("1, ", n), ", ") + "]\n"
}

// TestMaxNodes covers the node budget of a load: 200 nodes per resource of
// the resource limit, 4,000,000 at the default, math.MaxInt when that
// overflows.
func TestMaxNodes(t *testing.T) {
	for l, want := range map[Limits]int{
		{}:                             4_000_000,
		{MaxResources: 3}:              600,
		{MaxResources: math.MaxInt}:    math.MaxInt,
		{MaxResources: 1, MaxFiles: 9}: 200,
	} {
		if got := l.MaxNodes(); got != want {
			t.Errorf("%+v.MaxNodes() = %d, want %d", l, got, want)
		}
	}
}

// TestNodeBudget covers the node budget across every file of one load: the
// node past it, counting the files in path order, is RZ-CFG-001 at that
// node, the same for every worker count, and the load stops.
func TestNodeBudget(t *testing.T) {
	m := mapFS(map[string]string{
		RootFile: gateway,
		// 150 nodes, then 100: the budget of 200 runs out at b.yaml's 51st
		// node, its 48th item.
		"a.yaml": listFile("a", 147),
		"b.yaml": listFile("b", 97),
	})
	want := "b.yaml:1:146 error RZ-CFG-001: the Bundle's files hold more than 200 YAML and JSON nodes, 200 per resource of the resource limit"
	for _, workers := range []int{1, 2, 8} {
		r := load(t, newLoader(t, Options{Workers: workers, Limits: Limits{MaxResources: 1}}), m)
		wantTexts(t, "workers "+strconv.Itoa(workers), r.Diagnostics, want)
		if !r.Stopped || r.Resources != nil {
			t.Errorf("workers %d: the load did not stop", workers)
		}
	}
	// 100, 50 and the Gateway's 22: within the budget.
	m["a.yaml"] = &fstest.MapFile{Data: []byte(listFile("a", 97))}
	m["b.yaml"] = &fstest.MapFile{Data: []byte(listFile("b", 47))}
	r := load(t, newLoader(t, Options{Limits: Limits{MaxResources: 1}}), m)
	if r.Stopped || slices.Contains(codes(r.Diagnostics), CodeLimit) {
		t.Errorf("172 nodes in all: %q", texts(r.Diagnostics))
	}
}

// TestNodeBudgetCountsJSON covers JSON files, whose values count toward the
// load's budget like YAML nodes.
func TestNodeBudgetCountsJSON(t *testing.T) {
	m := mapFS(map[string]string{
		RootFile: gateway,
		"a.json": `{"a": [` + strings.TrimSuffix(strings.Repeat("0, ", 300), ", ") + `]}`,
	})
	r := load(t, newLoader(t, Options{Limits: Limits{MaxResources: 1}}), m)
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].File != "a.json" || r.Diagnostics[0].Code != CodeLimit || !r.Stopped {
		t.Errorf("diagnostics %q", texts(r.Diagnostics))
	}
}

// TestNodeBudgetOverlay covers the overlay of a render: its files have
// what the base files left of the budget.
func TestNodeBudgetOverlay(t *testing.T) {
	m := mapFS(minimal())
	// A budget of 400: the base files take about 140, the overlay's 303
	// pass what they leave.
	m["a.yaml"] = &fstest.MapFile{Data: []byte(upstream("orders") + "  " + listFile("x", 100))}
	m["overlays/prod/big.yaml"] = &fstest.MapFile{Data: []byte(listFile("o", 300))}
	l := newLoader(t, Options{Limits: Limits{MaxResources: 2}})
	b, err := l.Parse(t.Context(), Source{FS: m, Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(codes(b.Diagnostics()), CodeLimit) {
		t.Fatalf("the base files alone: %q", texts(b.Diagnostics()))
	}
	r := loadEnv(t, l, m, environment("prod", " {}"), "prod")
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].File != "overlays/prod/big.yaml" || r.Diagnostics[0].Code != CodeLimit || !r.Stopped {
		t.Errorf("with the overlay: %q", texts(r.Diagnostics))
	}
	if r, err := b.Render(t.Context(), nil, nil); err != nil || r.Stopped {
		t.Errorf("without the overlay: %v, stopped %v", err, r.Stopped)
	}
}

// TestNodeBudgetReplacesFileBound covers the per-file bound the budget
// replaces: one file may hold more than five quarters of the per-document
// limit, as a rendered one-file Bundle of the whole resource limit does,
// while each document keeps its own limit.
func TestNodeBudgetReplacesFileBound(t *testing.T) {
	var ds []string
	for i := range 10 {
		ds = append(ds, upstream("u"+strconv.Itoa(i))+"  "+listFile("x", 40))
	}
	one := fstest.MapFS{"rendered.yaml": {Data: []byte(docs(append([]string{gateway}, ds...)...))}}
	// Ten documents of about 55 nodes: far past five quarters of 100, the
	// old per-file bound, and within the budget of 2,000.
	l := newLoader(t, Options{Limits: Limits{MaxDocumentNodes: 100, MaxResources: 11}})
	r, err := l.Load(t.Context(), Request{Source: Source{FS: one, Path: "rendered.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	wantClean(t, "one file", r.Diagnostics)
	if len(r.Resources) != 11 {
		t.Errorf("%d resources", len(r.Resources))
	}
	l = newLoader(t, Options{Limits: Limits{MaxDocumentNodes: 30, MaxResources: 11}})
	if r, _ := l.Load(t.Context(), Request{Source: Source{FS: one, Path: "rendered.yaml"}}); !slices.Contains(codes(r.Diagnostics), CodeLimit) {
		t.Errorf("a document past its own limit: %q", texts(r.Diagnostics))
	}
}

// TestNodeBudgetEnvironments covers the environments file, which has a node
// budget of its own beside the Bundle's.
func TestNodeBudgetEnvironments(t *testing.T) {
	l := newLoader(t, Options{Limits: Limits{MaxResources: 1}})
	e := readEnvs(t, l, environment("prod", "\n  variables:\n    V: x"))
	wantClean(t, "small", e.Diagnostics())
	var vars strings.Builder
	for i := range 150 {
		vars.WriteString("    V" + strconv.Itoa(i) + ": x\n")
	}
	e = readEnvs(t, l, environment("prod", "\n  variables:\n"+vars.String()))
	got := e.Diagnostics()
	if len(got) != 1 || got[0].Code != CodeLimit || got[0].Message != "the environments file holds more than 200 YAML and JSON nodes, 200 per resource of the resource limit" {
		t.Errorf("diagnostics %q", texts(got))
	}
}
