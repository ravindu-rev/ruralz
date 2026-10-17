// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// loadBundle parses every JSON file under testdata/bundle into resources
// (file paths relative to it, role base) and checks that each passes
// stage F, the precondition of stage G (02 req 2).
func loadBundle(t testing.TB) (*fixture, []*tree.Resource) {
	t.Helper()
	f := newFixture()
	root := filepath.Join("testdata", "bundle")
	var rs []*tree.Resource
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		src, err := os.ReadFile(p) //nolint:gosec // G304: a test fixture.
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rs = append(rs, f.resource(t, filepath.ToSlash(rel), string(src)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	views, err := schemaview.Embedded(schemas(t))
	if err != nil {
		t.Fatalf("schemaview.Embedded: %v", err)
	}
	for _, r := range rs {
		if ds, _ := views.Validate(r, f.files); len(ds) != 0 {
			t.Fatalf("fixture fails stage F: %q", texts(ds))
		}
	}
	return f, rs
}

// golden compares got with testdata/golden/<name>, rewriting it with
// -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p) //nolint:gosec // G304: a test golden file.
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file; run go test -run %s -update to accept\ngot:\n%s", p, t.Name(), got)
	}
}

// bundleText renders a Bundle one resource per line in canonical order.
func bundleText(b *hub.Bundle) string {
	var out strings.Builder
	for _, r := range b.Resources() {
		out.WriteString(r.String() + " " + dump(r.Tree) + "\n")
	}
	return out.String()
}

// TestRunExampleBundle runs stage G over a Bundle shaped like the
// configuration model's example: no diagnostics, a Bundle in canonical
// order whose materialized trees match the golden file (01 golden tests:
// "its materialized trees as JSON"), conversion-data stripped, typed
// objects and configs decoded, and sources recorded.
func TestRunExampleBundle(t *testing.T) {
	f, rs := loadBundle(t)
	b, ds, err := newStage(t).Run(t.Context(), rs, f.files, 4)
	if err != nil || len(ds) != 0 || b == nil {
		t.Fatalf("Run = %v, %q, %v", b, texts(ds), err)
	}
	golden(t, "bundle.txt", bundleText(b))
	var order []string
	for _, r := range b.Resources() {
		order = append(order, r.String())
	}
	want := []string{
		"Gateway/edge", "Upstream/inventory", "Upstream/orders",
		"Policy/cors-default", "Policy/headers-security", "Policy/jwt-default", "Policy/order-schema",
		"Policy/orders-token", "Policy/quota-partner", "Policy/ratelimit-global",
		"Consumer/partner-acme", "Route/inventory", "Route/orders-create",
	}
	if !slices.Equal(order, want) {
		t.Errorf("order = %q\nwant %q", order, want)
	}
	c, _ := b.Get(hub.ID{Kind: v1alpha1.KindConsumer, Name: "partner-acme"})
	if c.Annotations["owner"] != "sales" || len(c.Annotations) != 1 {
		t.Errorf("consumer annotations = %v, want conversion-data stripped", c.Annotations)
	}
	if c.Source.File != "consumers/partner.json" || c.Source.APIVersion != v1 || c.Source.Start.Line != 1 || c.Source.Start.File != "consumers/partner.json" {
		t.Errorf("consumer source = %+v", c.Source)
	}
	g, _ := b.Get(hub.ID{Kind: v1alpha1.KindGateway, Name: "edge"})
	if g.Labels["team"] != "platform" || b.Gateway() == nil || *b.Gateway().Spec.Admin.Port != 9901 {
		t.Errorf("gateway = %+v", g)
	}
	rl, _ := b.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: "ratelimit-global"})
	if cfg, ok := rl.Config.(*v1alpha1.RateLimitConfig); !ok || len(cfg.Limits) != 2 {
		t.Errorf("ratelimit config = %#v", rl.Config)
	}
	if p, _ := b.Policy("ratelimit-global"); *p.Spec.Overridable {
		t.Error("explicit overridable: false not kept")
	}
	if p, _ := b.Policy("jwt-default"); !*p.Spec.Overridable || *p.Spec.FailureMode != v1alpha1.FailureModeClosed || p.Spec.Slot != "auth" {
		t.Errorf("jwt-default defaults = %+v", p.Spec)
	}
}

// TestRunDeterministic is 01 property P7 for stage G: the Bundle and the
// diagnostics are identical for 1, 2 and 8 workers and any input order.
func TestRunDeterministic(t *testing.T) {
	var want string
	for _, workers := range []int{1, 2, 8, 0} {
		f, rs := loadBundle(t)
		// Two resources with stage G errors, so diagnostics are compared too.
		rs = append(rs,
			f.resource(t, "bad/a.json", route(`{"match": {}, "timeout": "9999999h"}`)),
			f.resource(t, "bad/b.json", route(`{"match": {"hosts": ["x.example", "X.example"]}}`)))
		if workers == 2 {
			slices.Reverse(rs)
		}
		b, ds, err := newStage(t).Run(t.Context(), rs, f.files, workers)
		if err != nil || b != nil {
			t.Fatalf("Run with errors = %v, %v", b, err)
		}
		got := strings.Join(texts(ds), "\n")
		if want == "" {
			want = got
			if len(ds) != 2 {
				t.Fatalf("diagnostics = %q", texts(ds))
			}
		} else if got != want {
			t.Errorf("workers %d: diagnostics\n%s\nwant\n%s", workers, got, want)
		}
	}
	// Empty input.
	b, ds, err := newStage(t).Run(t.Context(), nil, nil, 4)
	if err != nil || len(ds) != 0 || b == nil || len(b.Resources()) != 0 {
		t.Errorf("Run(nil) = %v, %v, %v", b, ds, err)
	}
}

func TestRunCanceled(t *testing.T) {
	f, rs := loadBundle(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	b, ds, err := newStage(t).Run(ctx, rs, f.files, 2)
	if !errors.Is(err, context.Canceled) || b != nil || ds != nil {
		t.Errorf("Run(canceled) = %v, %v, %v", b, ds, err)
	}
}

// TestIdempotentOnCanonical covers R-47 and 02 req 20: stage G applied to
// its own output, re-entering as canonical content, changes nothing and
// reports nothing; the same holds when it re-enters as authored content.
func TestIdempotentOnCanonical(t *testing.T) {
	f, rs := loadBundle(t)
	s := newStage(t)
	b, ds, err := s.Run(t.Context(), rs, f.files, 2)
	if err != nil || len(ds) != 0 {
		t.Fatalf("Run = %q, %v", texts(ds), err)
	}
	for _, role := range []tree.Role{tree.RoleCanonical, tree.RoleBase} {
		g := newFixture()
		var again []*tree.Resource
		for _, r := range b.Resources() {
			again = append(again, g.resourceRole(t, "lkg.json", dump(r.Tree), role))
		}
		b2, ds, err := s.Run(t.Context(), again, g.files, 3)
		if err != nil || len(ds) != 0 {
			t.Fatalf("role %d: Run = %q, %v", role, texts(ds), err)
		}
		if got, want := bundleText(b2), bundleText(b); got != want {
			t.Errorf("role %d: second run changed the Bundle\n%s\nwant\n%s", role, got, want)
		}
		for _, r := range b2.Resources() {
			if (r.Source.File == "") != (role == tree.RoleCanonical) {
				t.Errorf("role %d: %s source file %q", role, r.ID, r.Source.File)
			}
		}
	}
}

// TestOverridablePresence is the stage G part of 02 test plan item 9: an
// absent and an explicit true overridable materialize to the same tree,
// an explicit false survives.
func TestOverridablePresence(t *testing.T) {
	s := newStage(t)
	tree := func(spec string) string {
		f := newFixture()
		r, ds := s.Resource(f.resource(t, "p.json", `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "ratelimit-global"}, "spec": `+spec+`}`), f.files)
		if len(ds) != 0 {
			t.Fatalf("Resource: %q", texts(ds))
		}
		return dump(r.Tree)
	}
	cfg := `"config": {"limits": [{"requests": 1, "window": "1s"}]}`
	absent := tree(`{"type": "ratelimit", ` + cfg + `}`)
	explicitTrue := tree(`{"type": "ratelimit", "overridable": true, ` + cfg + `}`)
	explicitFalse := tree(`{"type": "ratelimit", "overridable": false, ` + cfg + `}`)
	if absent != explicitTrue || absent == explicitFalse || !strings.Contains(explicitFalse, `"overridable":false`) {
		t.Errorf("absent %s\ntrue %s\nfalse %s", absent, explicitTrue, explicitFalse)
	}
}

func TestNew(t *testing.T) {
	empty, err := schemaidx.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	beta, err := convert.NewRegistry(convert.Lifecycle{}, convert.V1alpha1(), otherConverter{})
	if err != nil {
		t.Fatal(err)
	}
	// The zero values of the two registries (WP-37 review round 5: a zero
	// convert.Registry passed New, and every resource then failed with
	// RZ-CFG-007 "not served", a configuration error for the program's;
	// a zero registry.Registry made every Policy decode without a config).
	for name, o := range map[string]Options{
		"no schemas":                {Registry: registry.New()},
		"no registry":               {Schemas: schemas(t)},
		"no hub schema":             {Schemas: empty, Registry: registry.New()},
		"unindexed spoke":           {Schemas: schemas(t), Registry: registry.New(), Converters: beta},
		"zero converter registry":   {Schemas: schemas(t), Registry: registry.New(), Converters: &convert.Registry{}},
		"zero Policy registry":      {Schemas: schemas(t), Registry: &registry.Registry{}},
		"negative MaxDiagnostics":   {Schemas: schemas(t), Registry: registry.New(), MaxDiagnostics: -1},
		"negative YieldAfter":       {Schemas: schemas(t), Registry: registry.New(), YieldAfter: -1},
		"default converters, good":  {Schemas: schemas(t), Registry: registry.New()},
		"explicit converters, good": {Schemas: schemas(t), Registry: registry.New(), Converters: convert.Default()},
	} {
		s, err := New(o)
		if good := strings.HasSuffix(name, ", good"); good != (err == nil) || good != (s != nil) {
			t.Errorf("%s: New = %v, %v", name, s != nil, err)
		} else if !good && !errors.Is(err, ErrOptions) {
			t.Errorf("%s: New error = %v, want ErrOptions", name, err)
		}
	}
	s := newStage(t)
	if s.HubIndex() != hubIndex(t) {
		t.Error("HubIndex is not the v1alpha1 index")
	}
}

type otherConverter struct{}

func (otherConverter) APIVersion() string { return "ruralz/v1beta1" }

func (otherConverter) ToHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	return doc, nil
}

func (otherConverter) FromHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	return doc, nil
}

// faultyHub is a hub converter that fails, to check the stage's handling
// of conversion diagnostics.
type faultyHub struct{}

func (faultyHub) APIVersion() string { return v1 }

func (faultyHub) ToHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	return doc, diag.List{{Code: "RZ-CFG-005", Severity: diag.SeverityError, Message: "cannot convert"}}
}

func (faultyHub) FromHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	return doc, nil
}

// TestResourceErrors covers the per-resource failures of stage G: an
// unserved apiVersion (RZ-CFG-007, 02 req 4), a document that is not an
// object, a failed conversion and failed decode, each carrying the
// resource identity and a location.
func TestResourceErrors(t *testing.T) {
	s := newStage(t)
	f := newFixture()
	res := f.resource(t, "r.json", `{"apiVersion": "ruralz/v9", "kind": "Route", "metadata": {"name": "r"}, "spec": {}}`)
	_, ds := s.Resource(res, f.files)
	if got := texts(ds); len(got) != 1 || !strings.HasPrefix(got[0], `r.json:1:16 error RZ-CFG-007 Route/r apiVersion: apiVersion "ruralz/v9" is not served by this release`) {
		t.Errorf("unserved apiVersion: %q", got)
	}
	res = f.resource(t, "s.json", `["x"]`)
	if _, ds := s.Resource(res, f.files); len(ds) != 1 || ds[0].Code != CodeSchema || ds[0].Line != 1 {
		t.Errorf("non-object: %q", texts(ds))
	}
	if h, ds := s.Resource(nil, nil); h != nil || ds != nil {
		t.Error("Resource(nil) returned something")
	}
	conv, err := convert.NewRegistry(convert.Lifecycle{}, faultyHub{})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := New(Options{Schemas: schemas(t), Registry: registry.New(), Converters: conv})
	if err != nil {
		t.Fatal(err)
	}
	res = f.resource(t, "c.json", route(`{"match": {}}`))
	_, ds = fs.Resource(res, f.files)
	if got := texts(ds); len(got) != 1 || got[0] != "c.json:1:16 error RZ-CFG-005 Route/r: cannot convert" {
		t.Errorf("conversion failure: %q", got)
	}
	res = f.resource(t, "d.json", route(`{"match": {}, "upstreams": [{"name": "u", "weight": 4294967296}]}`))
	if h, ds := s.Resource(res, f.files); h != nil || len(ds) != 1 || !strings.Contains(ds[0].Message, "cannot decode") {
		t.Errorf("decode failure: %v %q", h, texts(ds))
	}
	// A spoke whose apiVersion has a schema but no converter.
	res = f.resource(t, "e.json", route(`{"match": {}}`))
	res.APIVersion = "ruralz/v1beta1"
	if _, ds := s.Resource(res, f.files); len(ds) != 1 || ds[0].Code != CodeUnserved {
		t.Errorf("no converter: %q", texts(ds))
	}
}

// TestResourceNoSchema covers an apiVersion a converter serves without a
// schema, which New prevents; Resource still reports RZ-CFG-007.
func TestResourceNoSchema(t *testing.T) {
	conv, err := convert.NewRegistry(convert.Lifecycle{}, convert.V1alpha1(), otherConverter{})
	if err != nil {
		t.Fatal(err)
	}
	s := &Stage{schemas: schemas(t), hub: hubIndex(t), reg: registry.New(), conv: conv}
	f := newFixture()
	res := f.resource(t, "r.json", route(`{"match": {}}`))
	res.APIVersion = "ruralz/v1beta1"
	_, ds := s.Resource(res, f.files)
	if len(ds) != 1 || ds[0].Code != CodeUnserved || !strings.Contains(ds[0].Message, "has no schema") {
		t.Errorf("no schema: %q", texts(ds))
	}
}

func TestSourceFile(t *testing.T) {
	files := &tree.FileTable{}
	base := files.Add(tree.File{Path: "a.yaml", Role: tree.RoleBase})
	canon := files.Add(tree.File{Path: "lkg.json", Role: tree.RoleCanonical})
	if got := sourceFile(files, tree.Pos{File: base, Line: 1}); got != "a.yaml" {
		t.Errorf("base = %q", got)
	}
	if got := sourceFile(files, tree.Pos{File: canon, Line: 1}); got != "" {
		t.Errorf("canonical = %q", got)
	}
	if got := sourceFile(nil, tree.Pos{}); got != "" {
		t.Errorf("no files = %q", got)
	}
	if metaOf(3) != nil {
		t.Error("metaOf of a non-kind")
	}
	if _, ok := newObject("Nope"); ok {
		t.Error("newObject of an unknown kind")
	}
}

// TestOwnLocatesOnce covers how Resource completes a converter's
// diagnostics (02 section 3: converters report a path, stage G adds the
// file positions; WP-37 review: every one got the apiVersion position):
// each gets the resource identity unless it has one; each without a
// location gets the position of the node its path names (a member, a
// keyed entry, an index, a set element), or of the nearest positioned
// node on that path; an empty or unresolvable path gets the apiVersion
// member's, looked up once for all of them; and nothing is looked up when
// every diagnostic has a location.
func TestOwnLocatesOnce(t *testing.T) {
	s := newStage(t)
	f := newFixture()
	res := f.resource(t, "r.json", route(`{"match": {"hosts": ["a.example"]},
 "upstreams": [{"name": "a"}, {"name": "b", "weight": 2}]}`))
	// A node without a position, as a converter may add: its parent's
	// position stands for it.
	at(t, res.Root, "spec.upstreams[1]").Set("added", tree.Pos{}, &tree.Node{Kind: tree.KindString, Text: "x"})
	other := &diag.ResourceID{Kind: "Route", Name: "other"}
	kept := diag.Location{File: "x.yaml", Line: 9, Column: 2}
	apiVersion := diag.Location{File: "r.json", Line: 1, Column: 16}
	pos := func(path string) diag.Location {
		p := at(t, res.Root, path).Pos
		return diag.Location{File: "r.json", Line: int(p.Line), Column: int(p.Column)}
	}
	tests := []struct {
		d   diag.Diagnostic
		res *diag.ResourceID
		loc diag.Location
	}{
		{diag.Diagnostic{Message: "empty path"}, res.ID.ResourceID(), apiVersion},
		{diag.Diagnostic{Message: "other resource", Resource: other}, other, apiVersion},
		{diag.Diagnostic{Message: "has a location", Location: kept, Path: diag.Path{diag.Field("spec")}}, res.ID.ResourceID(), kept},
		{diag.Diagnostic{Message: "member", Path: diag.Path{diag.Field("spec"), diag.Field("match")}}, res.ID.ResourceID(), pos("spec.match")},
		{diag.Diagnostic{Message: "keyed", Path: diag.Path{diag.Field("spec"), diag.Field("upstreams"), diag.Keyed("name", "b"), diag.Field("weight")}}, res.ID.ResourceID(), pos("spec.upstreams[1].weight")},
		{diag.Diagnostic{Message: "index", Path: diag.Path{diag.Field("spec"), diag.Field("upstreams"), diag.Index(0)}}, res.ID.ResourceID(), pos("spec.upstreams[0]")},
		{diag.Diagnostic{Message: "set item", Path: diag.Path{diag.Field("spec"), diag.Field("match"), diag.Field("hosts"), diag.Item("a.example")}}, res.ID.ResourceID(), pos("spec.match.hosts[0]")},
		{diag.Diagnostic{Message: "unpositioned", Path: diag.Path{diag.Field("spec"), diag.Field("upstreams"), diag.Keyed("name", "b"), diag.Field("added")}}, res.ID.ResourceID(), pos("spec.upstreams[1]")},
		{diag.Diagnostic{Message: "unresolvable", Path: diag.Path{diag.Field("spec"), diag.Field("nope")}}, res.ID.ResourceID(), apiVersion},
		{diag.Diagnostic{Message: "unresolvable index", Path: diag.Path{diag.Field("spec"), diag.Field("upstreams"), diag.Index(7)}}, res.ID.ResourceID(), apiVersion},
	}
	ds := make(diag.List, len(tests))
	for i, tc := range tests {
		ds[i] = tc.d
	}
	w := newWork()
	got := s.own(res, f.files, ds, w)
	for i, tc := range tests {
		if *got[i].Resource != *tc.res || got[i].Location != tc.loc {
			t.Errorf("%s: %v at %+v, want %v at %+v", tc.d.Message, got[i].Resource, got[i].Location, tc.res, tc.loc)
		}
	}
	// One finder for all the paths, and one apiVersion lookup for all
	// the fallbacks.
	walks := newWork()
	shared := walks.newFinder()
	for _, tc := range tests {
		if tc.d.Location == (diag.Location{}) {
			shared.at(res.Root, tc.d.Path)
		}
	}
	lookup := newWork()
	lookup.get(res.Root, "apiVersion")
	if want := walks.total + lookup.total; w.total != want {
		t.Errorf("own counted %d units, want %d: one finder for the paths and one lookup of the %d envelope members", w.total, want, len(res.Root.Members))
	}
	w = newWork()
	s.own(res, f.files, diag.List{{Message: "e", Location: kept}}, w)
	if w.total != 0 {
		t.Errorf("own with every location known counted %d units, want 0", w.total)
	}
	// Without a position anywhere on the path, the fallback applies.
	bare := &tree.Resource{ID: res.ID, Root: mustParse(t, `{"apiVersion": "ruralz/v1alpha1", "spec": {}}`)}
	clearPositions(bare.Root)
	if got := s.own(bare, nil, diag.List{{Path: diag.Path{diag.Field("spec")}}}, newWork()); got[0].Location != (diag.Location{}) {
		t.Errorf("unpositioned tree: location %+v, want none", got[0].Location)
	}
}

// clearPositions removes every position below n.
func clearPositions(n *tree.Node) {
	n.Pos = tree.Pos{}
	for i := range n.Members {
		n.Members[i].KeyPos = tree.Pos{}
		clearPositions(n.Members[i].Value)
	}
	for _, it := range n.Items {
		clearPositions(it)
	}
}

// TestConverterResultChecked covers stage G with a registered converter
// that returns no object (02 req 5, the extension point; WP-37 review: a
// converter returning nil gave a hub.Resource with no tree): the
// registry's RZ-CFG-007 rejects the resource at its apiVersion member, and
// Run returns no Bundle, for a nil, a scalar and a list result.
func TestConverterResultChecked(t *testing.T) {
	for name, out := range map[string]*tree.Node{
		"nil":    nil,
		"scalar": {Kind: tree.KindString, Text: "x"},
		"list":   {Kind: tree.KindList},
	} {
		t.Run(name, func(t *testing.T) {
			s := convertedStage(t, Options{}, func(*tree.Node) (*tree.Node, diag.List) { return out, nil })
			f := newFixture()
			h, ds := s.Resource(f.resource(t, "r.json", route(`{"match": {}}`)), f.files)
			if got := texts(ds); h != nil || len(got) != 1 ||
				!strings.HasPrefix(got[0], `r.json:1:16 error RZ-CFG-007 Route/r: the converter of apiVersion "ruralz/v1alpha1" returned `) {
				t.Errorf("Resource = %v, %q", h, got)
			}
			b, ds, err := s.Run(t.Context(), []*tree.Resource{f.resource(t, "r.json", route(`{"match": {}}`))}, f.files, 1)
			if b != nil || err != nil || len(ds) != 1 || ds[0].Code != CodeUnserved {
				t.Errorf("Run = %v, %q, %v", b, texts(ds), err)
			}
		})
	}
}

// TestMessagesClipped covers every stage G message that quotes an
// authored value (WP-37 review: a megabyte value gave a megabyte
// message): a ByteSize that passes stage F but does not parse (too large,
// too many fraction digits), an unserved apiVersion, an apiVersion with a
// converter but no schema, a kind with no hub type, a Duration whose
// UnmarshalJSON error quotes it twice, and a decode error no field is
// blamed for. Each shows at most 64 code points of the value.
func TestMessagesClipped(t *testing.T) {
	// 64 KiB values: long enough that an unclipped message is far above
	// most, short enough for the race detector.
	const most, size = 300, 1 << 16
	nines, ones := strings.Repeat("9", size), strings.Repeat("1", size)
	views, err := schemaview.Embedded(schemas(t))
	if err != nil {
		t.Fatal(err)
	}
	s := newStage(t)
	for _, tc := range []struct{ name, value, reason string }{
		{"byte size too large", nines + "Ki", "is too large"},
		{"byte size fraction digits", "1." + ones + "Ki", "has too many fraction digits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			res := f.resource(t, "g.json", gateway(`{"listeners": [{"name": "h", "port": 80, "protocol": "http"}], "limits": {"maxRequestBodyBytes": "`+tc.value+`"}}`))
			if ds, _ := views.Validate(res, f.files); len(ds) != 0 {
				t.Fatalf("stage F: %q", texts(ds))
			}
			_, ds := s.Resource(res, f.files)
			if len(ds) != 1 || len(ds[0].Message) > most || !strings.Contains(ds[0].Message, tc.reason) || !strings.Contains(ds[0].Message, `..."`) {
				t.Errorf("Resource: %d diagnostics, message of %d bytes %.120q", len(ds), len(ds[0].Message), ds[0].Message)
			}
		})
	}
	long := "ruralz/" + strings.Repeat("v", size)
	check := func(name string, ds diag.List) {
		t.Helper()
		if len(ds) != 1 || len(ds[0].Message) > most || !strings.Contains(ds[0].Message, "...") {
			t.Errorf("%s: %d diagnostics, message of %d bytes %.120q", name, len(ds), len(ds[0].Message), ds[0].Message)
		}
	}
	f := newFixture()
	res := f.resource(t, "r.json", route(`{"match": {}}`))
	res.APIVersion = long
	_, ds := s.Resource(res, f.files)
	check("unserved apiVersion", ds)
	conv, err := convert.NewRegistry(convert.Lifecycle{}, convert.V1alpha1(), versionConverter{long})
	if err != nil {
		t.Fatal(err)
	}
	noSchema := &Stage{schemas: schemas(t), hub: hubIndex(t), reg: registry.New(), conv: conv, maxDiagnostics: DefaultMaxDiagnostics}
	res = f.resource(t, "r.json", route(`{"match": {}}`))
	res.APIVersion = long
	_, ds = noSchema.Resource(res, f.files)
	check("apiVersion without a schema", ds)
	_, _, ds = Decode(hubIndex(t), nil, &tree.Resource{ID: tree.ID{Kind: v1alpha1.Kind(strings.Repeat("K", size))}, Root: mustParse(t, `{}`)}, nil)
	check("kind without a hub type", ds)
	timeout := envelope(mustParse(t, route(`{"match": {}, "timeout": "x"}`)))
	at(t, timeout.Root, "spec.timeout").Text = strings.Repeat("9", size) + "x"
	_, _, ds = Decode(hubIndex(t), nil, timeout, nil)
	check("duration", ds)
	// The decode's fallback quotes encoding/json's error, which may hold
	// the whole value: a number literal outside the grammar in a plugin
	// config (any-valued, so no value is at fault in Go terms).
	plugin := envelope(mustParse(t, policyDoc("plugin", `{"n": 1}`)))
	at(t, plugin.Root, "spec.config.n").Text = strings.Repeat("9", size) + "x"
	_, _, ds = Decode(hubIndex(t), registry.New(), plugin, nil)
	check("decode fallback", ds)
}

// versionConverter is an identity converter of apiVersion v.
type versionConverter struct{ v string }

func (c versionConverter) APIVersion() string { return c.v }

func (versionConverter) ToHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	return doc, nil
}

func (versionConverter) FromHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	return doc, nil
}

// TestStageDeprecations covers the RZ-CFG-025 warnings of stage G (02 req
// 76; CM Figure 3, stage G: 005, 025; WP-37 review round 3: nothing
// called Registry.Deprecations): with a lifecycle table in
// Options.Converters, a deprecated apiVersion gives one warning at the
// apiVersion member and a deprecated field one at the authored field,
// each with the removing release and the replacement as the hint, while
// the resource is still accepted; a field only a default wrote gives
// none, since the warnings read the tree as authored; the warnings draw
// on the diagnostic budget like any other; and the M1 table gives none.
func TestStageDeprecations(t *testing.T) {
	reg, err := convert.NewRegistry(convert.Lifecycle{
		APIVersions: map[string]convert.Notice{v1: {Removal: "0.9.0", Replacement: "ruralz/v1beta1"}},
		Fields:      map[string]map[string]convert.Notice{v1: {"spec.upstreams[].weight": {Removal: "0.8.0"}}},
	}, convert.V1alpha1())
	if err != nil {
		t.Fatal(err)
	}
	s := stageWith(t, Options{Converters: reg})
	f := newFixture()
	res := f.resource(t, "r.json", route(`{"match": {}, "upstreams": [{"name": "a", "weight": 2}, {"name": "b"}]}`))
	h, ds := s.Resource(res, f.files)
	if h == nil {
		t.Fatalf("Resource rejected a resource with warnings only: %q", texts(ds))
	}
	want := []string{
		"r.json:1:16 warning RZ-CFG-025 Route/r apiVersion: apiVersion ruralz/v1alpha1 is deprecated and is removed in 0.9.0 (use ruralz/v1beta1)",
		"r.json:1:139 warning RZ-CFG-025 Route/r spec.upstreams[name=a].weight: field spec.upstreams[].weight is deprecated and is removed in 0.8.0",
	}
	if got := texts(ds); !slices.Equal(got, want) {
		t.Errorf("Resource diagnostics\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if w := at(t, h.Tree, "spec.upstreams[1].weight"); w.Style != tree.StyleDefaulted {
		t.Errorf("entry b weight = %s, want the materialized default", dump(w))
	}
	// The warnings draw on the budget: a cap of 1 keeps the first.
	capped := stageWith(t, Options{Converters: reg, MaxDiagnostics: 1})
	res = f.resource(t, "r.json", route(`{"match": {}, "upstreams": [{"name": "a", "weight": 2}, {"name": "b", "weight": 3}]}`))
	if h, ds := capped.Resource(res, f.files); h == nil || len(ds) != 2 || ds.HasErrors() {
		t.Errorf("capped Resource = %v, %q; want the resource and cap+1 warnings", h, texts(ds))
	}
	// The M1 registry has an empty table.
	res = f.resource(t, "r.json", route(`{"match": {}, "upstreams": [{"name": "a", "weight": 2}]}`))
	if h, ds := newStage(t).Resource(res, f.files); h == nil || len(ds) != 0 {
		t.Errorf("M1 Resource = %v, %q; want no warning", h, texts(ds))
	}
	// The walk of the table counts toward the worker's yield timer (01
	// req 53): one unit or more per position it visits.
	units := func(s *Stage) int {
		w := s.newWork()
		s.resource(f.resource(t, "r.json", route(`{"match": {}, "upstreams": [{"name": "a", "weight": 2}, {"name": "b"}]}`)), f.files, w)
		return w.total
	}
	if with, without := units(s), units(newStage(t)); with < without+10 {
		t.Errorf("work units with the lifecycle table %d, without %d: the walk of the table is not counted", with, without)
	}
}

// TestCanonicalNoDeprecations covers the RZ-CFG-025 warnings on content
// that re-enters as ruralz.canonical.v1 (WP-37 review round 3: stage G
// warned on Last-Known-Good boots, handovers and diffs about an
// apiVersion the user never wrote, since canonical content names the hub
// apiVersion, and about deprecated fields an earlier run defaulted): a
// tree.RoleCanonical resource gets no warning for a deprecated apiVersion
// or a deprecated field, while the same resource as authored content
// gets both.
func TestCanonicalNoDeprecations(t *testing.T) {
	reg, err := convert.NewRegistry(convert.Lifecycle{
		APIVersions: map[string]convert.Notice{v1: {Removal: "0.9.0"}},
		Fields:      map[string]map[string]convert.Notice{v1: {"spec.upstreams[].weight": {Removal: "0.8.0"}}},
	}, convert.V1alpha1())
	if err != nil {
		t.Fatal(err)
	}
	s := stageWith(t, Options{Converters: reg})
	src := route(`{"match": {}, "upstreams": [{"name": "a", "weight": 2}]}`)
	f := newFixture()
	for _, tc := range []struct {
		role tree.Role
		want int
	}{{tree.RoleCanonical, 0}, {tree.RoleBase, 2}} {
		h, ds := s.Resource(f.resourceRole(t, "r.json", src, tc.role), f.files)
		if h == nil || len(ds) != tc.want {
			t.Errorf("role %v: Resource = %v, %q; want the resource and %d warnings", tc.role, h != nil, texts(ds), tc.want)
		}
		for _, d := range ds {
			if d.Code != convert.CodeDeprecated {
				t.Errorf("role %v: %s", tc.role, d.AppendText(nil))
			}
		}
	}
}
