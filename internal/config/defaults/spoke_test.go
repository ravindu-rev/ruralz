// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// vSpoke is the apiVersion of the synthetic spoke of the hub-and-spoke
// tests.
const vSpoke = "ruralz/v1test"

// spokeSchemaJSON returns the rendered schema of the spoke ruralz/v1test:
// the ruralz/v1alpha1 rendered view with its apiVersion consts replaced,
// Route spec.upstreams named spec.backends, the RouteUpstream weight
// default 3 instead of 1, and the auth.api-key config member header named
// hdr with the default "x-spoke" instead of "x-api-key". Each difference
// tells the spoke schema from the hub's in one step of stage G.
func spokeSchemaJSON(t testing.TB) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(schema.RenderedV1alpha1()))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	obj := func(v any, path ...string) map[string]any {
		t.Helper()
		for _, p := range path {
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("rendered schema: no object at %s", strings.Join(path, "."))
			}
			v = m[p]
		}
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("rendered schema: no object at %s", strings.Join(path, "."))
		}
		return m
	}
	routeSpec := obj(doc, "$defs", "RouteSpec")
	props := obj(routeSpec, "properties")
	props["backends"] = props["upstreams"]
	delete(props, "upstreams")
	// The oneOf of upstreams and composition names the member too.
	text, err := json.Marshal(routeSpec["allOf"])
	if err != nil {
		t.Fatal(err)
	}
	var allOf any
	if err := json.Unmarshal(bytes.ReplaceAll(text, []byte(`"upstreams"`), []byte(`"backends"`)), &allOf); err != nil {
		t.Fatal(err)
	}
	routeSpec["allOf"] = allOf
	obj(doc, "$defs", "RouteUpstream", "properties", "weight")["default"] = json.Number("3")
	apiKey := obj(doc, "$defs", "AuthAPIKeyConfig", "properties")
	hdr := obj(apiKey, "header")
	hdr["default"] = "x-spoke"
	apiKey["hdr"] = hdr
	delete(apiKey, "header")
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(out, []byte(`"const":"`+v1+`"`), []byte(`"const":"`+vSpoke+`"`))
}

// spokeConverter converts ruralz/v1test to and from the hub: Route
// spec.backends is the hub's spec.upstreams, and the auth.api-key config
// member hdr the hub's header. Defaults need no handling of their own:
// stage G writes the spoke's defaults before it converts.
type spokeConverter struct{}

func (spokeConverter) APIVersion() string { return vSpoke }

func (spokeConverter) ToHub(kind string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	spokeRename(kind, doc, "backends", "hdr", "upstreams", "header")
	return doc, nil
}

func (spokeConverter) FromHub(kind string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	spokeRename(kind, doc, "upstreams", "header", "backends", "hdr")
	return doc, nil
}

// spokeRename renames a Route's spec member route to routeTo and an
// auth.api-key Policy's config member key to keyTo.
func spokeRename(kind string, doc *tree.Node, route, key, routeTo, keyTo string) {
	spec, ok := doc.Get("spec")
	if !ok || spec == nil {
		return
	}
	target, from, to := spec, route, routeTo
	if kind == string(v1alpha1.KindPolicy) {
		if typ, ok := spec.Get("type"); !ok || typ == nil || typ.Text != string(v1alpha1.PolicyType("auth.api-key")) {
			return
		}
		cfg, ok := spec.Get("config")
		if !ok || cfg == nil {
			return
		}
		target, from, to = cfg, key, keyTo
	} else if kind != string(v1alpha1.KindRoute) {
		return
	}
	for i := range target.Members {
		if target.Members[i].Key == from {
			target.Members[i].Key = to
		}
	}
}

// spokeStage returns a Stage serving the hub and the spoke ruralz/v1test
// with the lifecycle table lc.
func spokeStage(t testing.TB, lc convert.Lifecycle) *Stage {
	t.Helper()
	spoke, err := schemaidx.Load(vSpoke, spokeSchemaJSON(t))
	if err != nil {
		t.Fatalf("schemaidx.Load: %v", err)
	}
	set, err := schemaidx.NewSet(hubIndex(t), spoke)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := convert.NewRegistry(lc, convert.V1alpha1(), spokeConverter{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Schemas: set, Registry: registry.New(), Converters: conv})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func spokeRoute(name, spec string) string {
	return `{"apiVersion": "` + vSpoke + `", "kind": "Route", "metadata": {"name": "` + name + `"}, "spec": ` + spec + `}`
}

// TestSpokeStage covers the hub-and-spoke rule of stage G (CM
// "Hub-and-spoke conversion": each resource is defaulted under its own
// apiVersion before hub conversion) with a spoke apiVersion, since
// ruralz/v1alpha1's index is the hub's and cannot tell the two indexes
// apart. One case per step, each failing when that step uses the other
// index or the other apiVersion:
//
//   - the RZ-CFG-025 lifecycle warnings read the spoke's schema paths
//     (spec.backends[].weight), which the hub's schema does not allow;
//   - the defaults are the spoke's (weight 3, and hdr "x-spoke" inside the
//     auth.api-key config through its type dispatch), made explicit before
//     the conversion;
//   - normalization sorts spec.upstreams as the hub's map list by name
//     after the conversion, which the spoke's schema does not declare;
//   - the decode names list entries by the hub's keyed lists and passes
//     the hub's declared config members (header, which the spoke calls
//     hdr) to the config type;
//   - hub.Resource.Source.APIVersion is the authored spoke apiVersion,
//     while the tree's apiVersion member and the object's are the hub's.
func TestSpokeStage(t *testing.T) {
	s := spokeStage(t, convert.Lifecycle{Fields: map[string]map[string]convert.Notice{
		vSpoke: {"spec.backends[].weight": {Removal: "0.9.0"}},
	}})
	f := newFixture()
	run := func(t *testing.T, src string) (*hub.Resource, []string) {
		t.Helper()
		res := f.resource(t, "s.json", src)
		h, ds := s.Resource(res, f.files)
		return h, texts(ds)
	}
	t.Run("deprecations read the spoke schema", func(t *testing.T) {
		h, ds := run(t, spokeRoute("r", `{"match": {}, "backends": [{"name": "a", "weight": 2}, {"name": "b"}]}`))
		want := []string{"s.json:1:136 warning RZ-CFG-025 Route/r spec.backends[name=a].weight: field spec.backends[].weight is deprecated and is removed in 0.9.0"}
		if h == nil || !slices.Equal(ds, want) {
			t.Errorf("Resource = %v, %q; want the resource and %q", h != nil, ds, want)
		}
	})
	t.Run("defaults come from the spoke schema", func(t *testing.T) {
		h, ds := run(t, spokeRoute("r", `{"match": {}, "backends": [{"name": "a"}]}`))
		if h == nil {
			t.Fatalf("Resource: %q", ds)
		}
		if w := at(t, h.Tree, "spec.upstreams[0].weight"); w.Text != "3" {
			t.Errorf("hub weight = %s, want the spoke default 3", dump(w))
		}
		if got := h.Object.(*v1alpha1.Route).Spec.Upstreams[0].Weight; got == nil || *got != 3 {
			t.Errorf("decoded weight = %v, want 3", got)
		}
		h, ds = run(t, `{"apiVersion": "`+vSpoke+`", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "auth.api-key", "config": {}}}`)
		if h == nil {
			t.Fatalf("Resource: %q", ds)
		}
		if got := dump(at(t, h.Tree, "spec.config")); got != `{"header":"x-spoke"}` {
			t.Errorf("hub config = %s, want the spoke default made explicit", got)
		}
		if got := h.Config.(*v1alpha1.AuthAPIKeyConfig).Header; got == nil || *got != "x-spoke" {
			t.Errorf("decoded header = %v, want x-spoke", got)
		}
	})
	t.Run("normalization uses the hub schema", func(t *testing.T) {
		h, ds := run(t, spokeRoute("r", `{"match": {}, "backends": [{"name": "b", "weight": 1}, {"name": "a", "weight": 1}]}`))
		if h == nil {
			t.Fatalf("Resource: %q", ds)
		}
		if got := dump(at(t, h.Tree, "spec.upstreams")); got != `[{"name":"a","weight":1},{"name":"b","weight":1}]` {
			t.Errorf("hub upstreams = %s, want the hub's map list sorted by name", got)
		}
	})
	t.Run("decode uses the hub schema", func(t *testing.T) {
		h, ds := run(t, spokeRoute("r", `{"match": {}, "backends": [{"name": "a", "weight": 4294967296}]}`))
		want := []string{
			"s.json:1:136 warning RZ-CFG-025 Route/r spec.backends[name=a].weight: field spec.backends[].weight is deprecated and is removed in 0.9.0",
			"s.json:1:136 error RZ-CFG-005 Route/r spec.upstreams[name=a].weight: cannot decode: integer 4294967296 is outside the range -2147483648 to 2147483647 of this field",
		}
		if h != nil || !slices.Equal(ds, want) {
			t.Errorf("Resource = %v, %q; want %q", h != nil, ds, want)
		}
		h, ds = run(t, `{"apiVersion": "`+vSpoke+`", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "auth.api-key", "config": {"hdr": "x-key"}}}`)
		if h == nil {
			t.Fatalf("Resource: %q", ds)
		}
		if got := h.Config.(*v1alpha1.AuthAPIKeyConfig).Header; got == nil || *got != "x-key" {
			t.Errorf("decoded header = %v, want x-key: the hub declares header", got)
		}
	})
	t.Run("source apiVersion", func(t *testing.T) {
		h, ds := run(t, spokeRoute("r", `{"match": {}, "backends": [{"name": "a"}]}`))
		if h == nil {
			t.Fatalf("Resource: %q", ds)
		}
		if h.Source.APIVersion != vSpoke {
			t.Errorf("Source.APIVersion = %q, want the authored %s", h.Source.APIVersion, vSpoke)
		}
		if got := at(t, h.Tree, "apiVersion").Text; got != convert.HubAPIVersion {
			t.Errorf("tree apiVersion = %q, want the hub's %s", got, convert.HubAPIVersion)
		}
		if got := h.Object.(*v1alpha1.Route).APIVersion; got != convert.HubAPIVersion {
			t.Errorf("object apiVersion = %q, want the hub's %s", got, convert.HubAPIVersion)
		}
	})
}

// TestSpokeRunSameHub covers a Run that mixes apiVersions: a ruralz/v1test
// Route and the ruralz/v1alpha1 Route that says the same (the spoke's
// weight default 3 written explicitly) give equal hub trees, so converting
// a Bundle keeps its Revision, while the Bundle keeps each resource's
// authored apiVersion as its source.
func TestSpokeRunSameHub(t *testing.T) {
	s := spokeStage(t, convert.Lifecycle{})
	f := newFixture()
	spoke := f.resource(t, "spoke.json", spokeRoute("a", `{"match": {"path": {"prefix": "/"}}, "timeout": "90s", "backends": [{"name": "y"}, {"name": "x", "weight": 3}]}`))
	alpha := f.resource(t, "alpha.json", route(`{"match": {"path": {"prefix": "/"}}, "timeout": "1m30s", "upstreams": [{"name": "x", "weight": 3}, {"name": "y", "weight": 3}]}`))
	b, ds, err := s.Run(t.Context(), []*tree.Resource{spoke, alpha}, f.files, 2)
	if err != nil || b == nil || len(ds) != 0 {
		t.Fatalf("Run = %v, %q, %v", b, texts(ds), err)
	}
	var trees []string
	var sources []string
	for _, h := range b.Resources() {
		meta, _ := h.Tree.Get("metadata")
		meta.Delete("name")
		trees = append(trees, dump(h.Tree))
		sources = append(sources, h.Source.APIVersion)
	}
	if len(trees) != 2 || trees[0] != trees[1] {
		t.Errorf("hub trees differ:\n%s", strings.Join(trees, "\n"))
	}
	slices.Sort(sources)
	if want := []string{v1, vSpoke}; !slices.Equal(sources, want) {
		t.Errorf("sources = %q, want %q", sources, want)
	}
}
