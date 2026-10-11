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
	for name, o := range map[string]Options{
		"no schemas":       {Registry: registry.New()},
		"no registry":      {Schemas: schemas(t)},
		"no hub schema":    {Schemas: empty, Registry: registry.New()},
		"unindexed spoke":  {Schemas: schemas(t), Registry: registry.New(), Converters: beta},
		"default and good": {},
	} {
		_, err := New(o)
		if name == "default and good" {
			continue
		}
		if !errors.Is(err, ErrOptions) {
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
