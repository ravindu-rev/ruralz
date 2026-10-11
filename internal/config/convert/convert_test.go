// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package convert

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

const v1 = v1alpha1.APIVersion

// TestDefaultServesV1alpha1 covers 02 req 4 and 5: M1 serves only
// ruralz/v1alpha1, through an identity converter.
func TestDefaultServesV1alpha1(t *testing.T) {
	r := Default()
	if got := r.Served(); !slices.Equal(got, []string{v1}) {
		t.Errorf("Served() = %v, want [%s]", got, v1)
	}
	if !r.Serves(v1) || r.Serves("ruralz/v1beta1") {
		t.Errorf("Serves: v1alpha1 %v, v1beta1 %v", r.Serves(v1), r.Serves("ruralz/v1beta1"))
	}
	c, ok := r.Converter(v1)
	if !ok || c.APIVersion() != v1 {
		t.Fatalf("Converter(%s) = %v, %v", v1, c, ok)
	}
	if HubAPIVersion != v1 {
		t.Errorf("HubAPIVersion = %q, want %q", HubAPIVersion, v1)
	}
	// Served returns a copy.
	s := r.Served()
	s[0] = "x"
	if r.Served()[0] != v1 {
		t.Error("Served exposes the registry's slice")
	}
}

// fixedConverter is a converter for tests with a settable apiVersion.
type fixedConverter struct {
	identity
}

func TestNewRegistry(t *testing.T) {
	other := fixedConverter{identity{version: "ruralz/v1beta1"}}
	tests := []struct {
		name string
		cs   []Converter
		ok   bool
	}{
		{"hub only", []Converter{V1alpha1()}, true},
		{"two versions", []Converter{other, V1alpha1()}, true},
		{"nil converter", []Converter{V1alpha1(), nil}, false},
		{"empty apiVersion", []Converter{V1alpha1(), fixedConverter{}}, false},
		{"duplicate", []Converter{V1alpha1(), V1alpha1()}, false},
		{"no hub", []Converter{other}, false},
		{"none", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewRegistry(Lifecycle{}, tc.cs...)
			if tc.ok {
				if err != nil {
					t.Fatalf("NewRegistry: %v", err)
				}
				if !slices.IsSorted(r.Served()) || len(r.Served()) != len(tc.cs) {
					t.Errorf("Served() = %v", r.Served())
				}
				return
			}
			if !errors.Is(err, ErrRegistry) {
				t.Errorf("NewRegistry error = %v, want ErrRegistry", err)
			}
		})
	}
}

// TestToHubIdentity covers 02 req 5 and 7: the v1alpha1 conversion keeps
// the tree, its member order and positions, and strips the conversion-data
// annotation.
func TestToHubIdentity(t *testing.T) {
	src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Route",
 "metadata": {"name": "a", "annotations": {"team": "x", "ruralz.io/conversion-data": "{}"}},
 "spec": {"upstreams": [{"name": "u"}], "match": {"path": {"prefix": "/"}}}}`
	doc := mustParse(t, src)
	want := mustParse(t, strings.Replace(src, `, "ruralz.io/conversion-data": "{}"`, "", 1))
	out, ds := Default().ToHub(v1, "Route", doc, false)
	if len(ds) != 0 {
		t.Fatalf("ToHub diagnostics: %v", ds)
	}
	if !equal(out, want) {
		t.Errorf("ToHub = %s\nwant %s", dump(out), dump(want))
	}
	spec, _ := out.Get("spec")
	if spec.Members[0].Key != "upstreams" {
		t.Errorf("member order changed: %s", dump(spec))
	}
	orig := mustParse(t, src)
	origSpec, _ := orig.Get("spec")
	if !samePositions(spec, origSpec) {
		t.Error("positions changed")
	}
}

func TestStripConversionData(t *testing.T) {
	tests := []struct {
		name, in, want string
		stripped       bool
	}{
		{
			"only member removes annotations",
			`{"metadata": {"name": "a", "annotations": {"ruralz.io/conversion-data": "{}"}}}`,
			`{"metadata": {"name": "a"}}`, true,
		},
		{
			"other annotations stay",
			`{"metadata": {"annotations": {"a": "1", "ruralz.io/conversion-data": "{}", "b": "2"}}}`,
			`{"metadata": {"annotations": {"a": "1", "b": "2"}}}`, true,
		},
		{
			"empty annotations authored stay",
			`{"metadata": {"annotations": {}}}`,
			`{"metadata": {"annotations": {}}}`, false,
		},
		{"no metadata", `{"spec": {}}`, `{"spec": {}}`, false},
		{"no annotations", `{"metadata": {"name": "a"}}`, `{"metadata": {"name": "a"}}`, false},
		{"annotations not a map", `{"metadata": {"annotations": "x"}}`, `{"metadata": {"annotations": "x"}}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := mustParse(t, tc.in)
			if got := StripConversionData(doc); got != tc.stripped {
				t.Errorf("StripConversionData = %v, want %v", got, tc.stripped)
			}
			if want := mustParse(t, tc.want); !equal(doc, want) {
				t.Errorf("got %s, want %s", dump(doc), dump(want))
			}
		})
	}
}

// TestUnserved covers 02 req 4 and 53: an unserved apiVersion is
// RZ-CFG-007 with the served apiVersions in the hint.
func TestUnserved(t *testing.T) {
	r := Default()
	doc := mustParse(t, `{"apiVersion": "ruralz/v9", "kind": "Route"}`)
	tests := []struct {
		name string
		run  func() diag.List
		msg  string
	}{
		{"ToHub", func() diag.List { _, ds := r.ToHub("ruralz/v9", "Route", doc, false); return ds }, `apiVersion "ruralz/v9" is not served`},
		{"FromHub", func() diag.List { _, ds := r.FromHub("ruralz/v9", "Route", doc, false); return ds }, `apiVersion "ruralz/v9" is not served`},
		{"Convert from", func() diag.List { _, ds := r.Convert("ruralz/v9", v1, "Route", doc, true); return ds }, `apiVersion "ruralz/v9" is not served`},
		{"Convert to", func() diag.List { _, ds := r.Convert(v1, "ruralz/v9", "Route", doc, true); return ds }, `target apiVersion "ruralz/v9" is not served`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ds := tc.run()
			if len(ds) != 1 || ds[0].Code != CodeUnserved || ds[0].Severity != diag.SeverityError {
				t.Fatalf("diagnostics = %v, want one %s error", ds, CodeUnserved)
			}
			if !strings.Contains(ds[0].Message, tc.msg) {
				t.Errorf("message %q, want it to contain %q", ds[0].Message, tc.msg)
			}
			if !strings.Contains(ds[0].Hint, "served apiVersions: "+v1) {
				t.Errorf("hint %q names no served apiVersion", ds[0].Hint)
			}
			if ds[0].Path.String() != "apiVersion" {
				t.Errorf("path %q, want apiVersion", ds[0].Path.String())
			}
		})
	}
	if _, bad := r.CheckServed(v1); bad {
		t.Error("CheckServed(v1alpha1) reported")
	}
	d, _ := r.CheckServed("ruralz/v1alpa1")
	if !strings.HasPrefix(d.Hint, "did you mean "+v1+"?") {
		t.Errorf("hint %q has no nearest match", d.Hint)
	}
}

// TestConvertPartial covers 02 req 6 and 53: a partial overlay document
// with $patch entries and unsubstituted ${VAR} text converts unchanged,
// keeping member order and positions.
func TestConvertPartial(t *testing.T) {
	src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "edge"},
 "spec": {"listeners": [{"name": "http", "$patch": "delete"}],
  "policies": [{"$patch": "replace"}, {"name": "b"}],
  "admin": {"port": "${ADMIN_PORT:-9901}"}, "stateStore": {"timeout": "$${literal}"}}}`
	out, ds := Default().Convert(v1, v1, "Gateway", mustParse(t, src), true)
	if len(ds) != 0 {
		t.Fatalf("Convert diagnostics: %v", ds)
	}
	want := mustParse(t, src)
	if !equal(out, want) || !samePositions(out, want) {
		t.Errorf("Convert = %s\nwant %s", dump(out), dump(want))
	}
	// A partial document without apiVersion or metadata converts too.
	frag := mustParse(t, `{"spec": {"timeout": "${T}"}}`)
	out, ds = Default().Convert(v1, v1, "Route", frag, true)
	if len(ds) != 0 || !equal(out, mustParse(t, `{"spec": {"timeout": "${T}"}}`)) {
		t.Errorf("Convert fragment = %s, %v", dump(out), ds)
	}
}

// betaConverter is a test spoke ruralz/v1test whose resources lack the hub
// field spec.timeout of a Route: FromHub moves it into the conversion-data
// annotation and ToHub restores it and consumes the annotation (02 req 7).
// It also renames the hub field spec.upstreams to spec.backends. The
// annotation records which of metadata and annotations FromHub created,
// so ToHub removes exactly those.
type betaConverter struct{}

const vTest = "ruralz/v1test"

func (betaConverter) APIVersion() string { return vTest }

func (betaConverter) ToHub(kind string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	spec, ok := doc.Get("spec")
	if kind != "Route" || !ok {
		return doc, nil
	}
	renameMember(spec, "backends", "upstreams")
	meta, ok := doc.Get("metadata")
	if !ok {
		return doc, nil
	}
	ann, ok := meta.Get("annotations")
	if !ok {
		return doc, nil
	}
	data, ok := ann.Get(ConversionDataAnnotation)
	if !ok || !strings.HasPrefix(data.Text, "timeout=") {
		return doc, nil
	}
	value, flags, _ := strings.Cut(strings.TrimPrefix(data.Text, "timeout="), ";")
	spec.Set("timeout", data.Pos, &tree.Node{Kind: tree.KindString, Text: value, Pos: data.Pos})
	ann.Delete(ConversionDataAnnotation)
	if strings.Contains(flags, "ann") {
		meta.Delete("annotations")
	}
	if strings.Contains(flags, "meta") {
		doc.Delete("metadata")
	}
	return doc, nil
}

func (betaConverter) FromHub(kind string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	spec, ok := doc.Get("spec")
	if kind != "Route" || !ok {
		return doc, nil
	}
	renameMember(spec, "upstreams", "backends")
	timeout, ok := spec.Get("timeout")
	if !ok {
		return doc, nil
	}
	spec.Delete("timeout")
	flags := ""
	meta, ok := doc.Get("metadata")
	if !ok {
		meta = &tree.Node{Kind: tree.KindMap}
		doc.Set("metadata", tree.Pos{}, meta)
		flags += "meta"
	}
	ann, ok := meta.Get("annotations")
	if !ok {
		ann = &tree.Node{Kind: tree.KindMap}
		meta.Set("annotations", tree.Pos{}, ann)
		flags += "ann"
	}
	ann.Set(ConversionDataAnnotation, timeout.Pos, &tree.Node{Kind: tree.KindString, Text: "timeout=" + timeout.Text + ";" + flags, Pos: timeout.Pos})
	return doc, nil
}

func renameMember(n *tree.Node, from, to string) {
	for i := range n.Members {
		if n.Members[i].Key == from {
			n.Members[i].Key = to
		}
	}
}

// TestConversionDataRoundTrip covers 02 req 7 and 8: a newer-only field
// survives a down-conversion in the annotation, is restored by the
// up-conversion, and the annotation never reaches the hub form.
func TestConversionDataRoundTrip(t *testing.T) {
	r, err := NewRegistry(Lifecycle{}, V1alpha1(), betaConverter{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "a"},
 "spec": {"upstreams": [{"name": "u"}], "timeout": "5s"}}`
	down, ds := r.Convert(v1, vTest, "Route", mustParse(t, src), false)
	if len(ds) != 0 {
		t.Fatalf("down diagnostics: %v", ds)
	}
	wantDown := mustParse(t, `{"apiVersion": "ruralz/v1test", "kind": "Route",
 "metadata": {"name": "a", "annotations": {"ruralz.io/conversion-data": "timeout=5s;ann"}},
 "spec": {"backends": [{"name": "u"}]}}`)
	if !equal(down, wantDown) {
		t.Fatalf("down = %s\nwant %s", dump(down), dump(wantDown))
	}
	up, ds := r.Convert(vTest, v1, "Route", down, false)
	if len(ds) != 0 {
		t.Fatalf("up diagnostics: %v", ds)
	}
	if want := mustParse(t, src); !equivalent(up, want) {
		t.Errorf("round trip = %s\nwant %s", dump(up), dump(want))
	}
	// An authored empty annotations object survives the round trip.
	withEmpty := strings.Replace(src, `{"name": "a"}`, `{"name": "a", "annotations": {}}`, 1)
	down, _ = r.Convert(v1, vTest, "Route", mustParse(t, withEmpty), false)
	up, _ = r.Convert(vTest, v1, "Route", down, false)
	if want := mustParse(t, withEmpty); !equivalent(up, want) {
		t.Errorf("round trip with empty annotations = %s\nwant %s", dump(up), dump(want))
	}
}

// faultConverter fails every conversion.
type faultConverter struct{ identity }

func (faultConverter) ToHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	return doc, diag.List{{Code: "RZ-CFG-005", Severity: diag.SeverityError, Message: "cannot convert"}}
}

func (faultConverter) FromHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	return doc, diag.List{{Code: "RZ-CFG-005", Severity: diag.SeverityError, Message: "cannot convert back"}}
}

func TestConverterErrors(t *testing.T) {
	r, err := NewRegistry(Lifecycle{}, V1alpha1(), faultConverter{identity{version: vTest}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	src := `{"apiVersion": "ruralz/v1test", "kind": "Route", "metadata": {"name": "a", "annotations": {"ruralz.io/conversion-data": "x"}}}`
	doc := mustParse(t, src)
	out, ds := r.Convert(vTest, v1, "Route", doc, false)
	if len(ds) != 1 || ds[0].Message != "cannot convert" {
		t.Fatalf("Convert diagnostics = %v", ds)
	}
	// A failed ToHub neither rewrites apiVersion nor strips.
	if !equal(out, mustParse(t, src)) {
		t.Errorf("failed ToHub changed the tree: %s", dump(out))
	}
	_, ds = r.Convert(v1, vTest, "Route", mustParse(t, src), false)
	if len(ds) != 1 || ds[0].Message != "cannot convert back" {
		t.Errorf("FromHub diagnostics = %v", ds)
	}
}

// TestDeprecations covers 02 req 76: RZ-CFG-025 warnings from the
// lifecycle table, none for the empty M1 table.
func TestDeprecations(t *testing.T) {
	idx := hubIndex(t)
	src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "a"},
 "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u", "weight": 2}, {"name": "v"}], "timeout": "5s"}}`
	files := &tree.FileTable{}
	fid := files.Add(tree.File{Path: "routes/a.json", Role: tree.RoleBase})
	root, err := parse(src, fid)
	if err != nil {
		t.Fatal(err)
	}
	res := &tree.Resource{ID: tree.ID{Kind: "Route", Name: "a"}, APIVersion: v1, Root: root, Start: root.Pos}
	if ds := Default().Deprecations(idx, res, files); len(ds) != 0 {
		t.Errorf("empty lifecycle reported %v", ds)
	}
	lc := Lifecycle{
		APIVersions: map[string]Notice{v1: {Removal: "0.9.0", Replacement: "ruralz/v1beta1"}},
		Fields: map[string]map[string]Notice{v1: {
			"spec.upstreams[].weight": {Removal: "0.8.0"},
			"spec.timeout":            {Removal: "0.8.0", Replacement: "spec.deadline"},
		}},
	}
	r, err := NewRegistry(lc, V1alpha1())
	if err != nil {
		t.Fatal(err)
	}
	// The registry keeps its own copy of the table.
	lc.APIVersions[v1] = Notice{Removal: "changed"}
	lc.Fields[v1]["spec.timeout"] = Notice{Removal: "changed"}
	ds := r.Deprecations(idx, res, files)
	ds.Sort()
	var got []string
	for _, d := range ds {
		if d.Code != CodeDeprecated || d.Severity != diag.SeverityWarning || d.Resource == nil || d.Resource.Name != "a" {
			t.Errorf("unexpected diagnostic %+v", d)
		}
		got = append(got, string(d.AppendText(nil)))
	}
	want := []string{
		"routes/a.json:1:16 warning RZ-CFG-025 Route/a apiVersion: apiVersion ruralz/v1alpha1 is deprecated and is removed in 0.9.0 (use ruralz/v1beta1)",
		"routes/a.json:2:85 warning RZ-CFG-025 Route/a spec.upstreams[name=u].weight: field spec.upstreams[].weight is deprecated and is removed in 0.8.0",
		"routes/a.json:2:116 warning RZ-CFG-025 Route/a spec.timeout: field spec.timeout is deprecated and is removed in 0.8.0 (use spec.deadline)",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Deprecations =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Without files, locations carry line and column only; nil inputs
	// report nothing.
	if ds := r.Deprecations(idx, res, nil); len(ds) != 3 || ds[0].File != "" || ds[0].Line != 1 {
		t.Errorf("Deprecations without files = %v", ds)
	}
	if ds := r.Deprecations(idx, nil, files); ds != nil {
		t.Errorf("Deprecations(nil) = %v", ds)
	}
	if ds := r.Deprecations(nil, res, files); len(ds) != 1 {
		t.Errorf("Deprecations without index = %v, want the apiVersion warning only", ds)
	}
}

func TestLocation(t *testing.T) {
	files := &tree.FileTable{}
	fid := files.Add(tree.File{Path: "a.yaml"})
	if got := location(files, tree.Pos{}, tree.Pos{}); got != (diag.Location{}) {
		t.Errorf("unknown = %+v", got)
	}
	if got := location(files, tree.Pos{}, tree.Pos{File: fid, Line: 2, Column: 3}); got.File != "a.yaml" || got.Line != 2 {
		t.Errorf("fallback = %+v", got)
	}
}
