// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package property

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/render"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// spokeAPIVersion is the test-only spoke apiVersion of FuzzConversion,
// which no binary serves.
const spokeAPIVersion = "ruralz/v0property"

// FuzzConversion is the Conversion row of the Required properties: every
// field round-trips through the hub type, and converting apiVersions
// keeps the Revision, also through a test-only spoke apiVersion that
// renames and moves fields both ways, so the property runs while one
// apiVersion is served (docs/architecture/02-configuration-model.md
// "Hub-and-spoke conversion"). The spoke, ruralz/v0property, renames a
// Route's spec.upstreams to spec.backends and an Upstream's spec.endpoints
// to spec.targets, moves a Route's spec.timeout down into spec.timing and
// its spec.match.path up to spec.path; its schema is the rendered
// ruralz/v1alpha1 schema with the same edits (spokeSchema).
//
// Before the seeds run, every field of the hub schema, with a generated
// value, converts to the spoke and back unchanged. Then, for a golden
// corpus Bundle, every render builds the same Revision after the files
// are converted (render.Convert, as ruralz bundle render --api-version
// converts them) to ruralz/v1alpha1, to the spoke, from the spoke back to
// ruralz/v1alpha1, and with a random set of resources, one per file, in
// the spoke beside the others in ruralz/v1alpha1: a Bundle may mix
// apiVersions.
func FuzzConversion(f *testing.F) {
	sp := spokeTools(f)
	checkEveryField(f, sp.pipe.Converters(), tools(f).pipe.HubIndex())
	addBundleSeeds(f, 1)
	f.Fuzz(func(t *testing.T, archive, envs []byte, seed uint64) {
		tc, sp := tools(t), spokeTools(t)
		m := newMemBundle(corpus.ParseArchive(archive).Files, envs)
		renders := tc.renders(t, m)
		want := tc.digests(t, m, renders)
		if !slices.ContainsFunc(want, func(d string) bool { return d != "" }) {
			t.Skip("no render of the Bundle builds: the negative corpus covers invalid input")
		}
		src, ok := tc.parseSource(t, m)
		if !ok {
			t.Skip("stages A to C refuse the Bundle")
		}
		check := func(name string, p *toolchain, files map[string][]byte) {
			t.Helper()
			if got := p.digests(t, m.withFiles(files), renders); !slices.Equal(got, want) {
				t.Fatalf("%s: the renders %q build %q, want %q\n%s", name, renders, got, want, dumpFiles(files))
			}
		}
		check("converted to "+v1alpha1.APIVersion, tc, convertFiles(t, src, v1alpha1.APIVersion, tc.pipe.Converters()))
		spoke := convertFiles(t, src, spokeAPIVersion, sp.pipe.Converters())
		if usesHubNames(t, spoke) {
			t.Fatalf("the spoke files keep a renamed or moved hub field:\n%s", dumpFiles(spoke))
		}
		check("converted to "+spokeAPIVersion, sp, spoke)
		back, ok := sp.parseSource(t, m.withFiles(spoke))
		if !ok {
			t.Fatalf("the spoke files do not parse:\n%s", dumpFiles(spoke))
		}
		check("converted to "+spokeAPIVersion+" and back", tc, convertFiles(t, back, v1alpha1.APIVersion, sp.pipe.Converters()))
		check("mixed apiVersions", sp, mixed(t, sp, newRand(seed), src))
	})
}

// convertFiles converts every file of src to target with conv, as ruralz
// bundle render --api-version does, and returns the files by path.
func convertFiles(t *testing.T, src []sourceFile, target string, conv *convert.Registry) map[string][]byte {
	t.Helper()
	files := make([]render.SourceFile, 0, len(src))
	for _, sf := range src {
		files = append(files, render.SourceFile{Path: sf.path, Documents: sf.docs})
	}
	out, ds, err := render.Convert(t.Context(), files, target, conv)
	if err != nil || ds.HasErrors() {
		t.Fatalf("render.Convert(%s): %v\n%s", target, err, texts(ds))
	}
	m := map[string][]byte{}
	for _, f := range out {
		m[f.Path] = f.Content
	}
	return m
}

// mixed writes src one document per file, the Gateway in ruralz.yaml, and
// converts the files of a random set of resources, their overlay
// documents included, to the spoke.
func mixed(t *testing.T, sp *toolchain, r *rand.Rand, src []sourceFile) map[string][]byte {
	t.Helper()
	inSpoke := map[tree.ID]bool{}
	var keep, move []sourceFile
	n := 0
	for _, sf := range src {
		for _, d := range sf.docs {
			id := idOf(d)
			if _, seen := inSpoke[id]; !seen {
				inSpoke[id] = r.IntN(2) == 0
			}
			name := fmt.Sprintf("m/%04d.yaml", n)
			switch {
			case sf.overlay != "":
				name = fmt.Sprintf("overlays/%s/%04d.yaml", sf.overlay, n)
			case id.Kind == v1alpha1.KindGateway:
				name = "ruralz.yaml"
			}
			n++
			one := sourceFile{path: name, overlay: sf.overlay, docs: []*tree.Node{d}}
			if inSpoke[id] {
				move = append(move, one)
			} else {
				keep = append(keep, one)
			}
		}
	}
	files := map[string][]byte{}
	for _, sf := range keep {
		files[sf.path] = yamlOf(t, sf.docs)
	}
	if len(move) > 0 {
		for name, data := range convertFiles(t, move, spokeAPIVersion, sp.pipe.Converters()) {
			files[name] = data
		}
	}
	return files
}

// usesHubNames reports a spoke file that still holds a field the spoke
// renames or moves, a Route's spec.upstreams, spec.timeout or
// spec.match.path or an Upstream's spec.endpoints, or a document that is
// not in the spoke: the conversion did not run.
func usesHubNames(t *testing.T, files map[string][]byte) bool {
	t.Helper()
	for name, data := range files {
		format, ok := profile.FormatOf(name)
		if !ok {
			continue
		}
		docs, ds, err := profile.Parse(t.Context(), data, 0, name, format, profile.Options{})
		if err != nil || ds.HasErrors() {
			t.Fatalf("%s does not parse: %v\n%s", name, err, texts(ds))
		}
		for _, d := range docs {
			if stringAt(d.Root, "apiVersion") != spokeAPIVersion {
				return true
			}
			spec, _ := d.Root.Get("spec")
			has := func(n *tree.Node, keys ...string) bool {
				for _, k := range keys {
					if n == nil || n.Kind != tree.KindMap {
						return false
					}
					n, _ = n.Get(k)
				}
				return n != nil
			}
			switch stringAt(d.Root, "kind") {
			case string(v1alpha1.KindRoute):
				if has(spec, "upstreams") || has(spec, "timeout") || has(spec, "match", "path") {
					return true
				}
			case string(v1alpha1.KindUpstream):
				if has(spec, "endpoints") {
					return true
				}
			}
		}
	}
	return false
}

// sharedSpoke builds the spoke's pipeline once per test binary.
var sharedSpoke = sync.OnceValues(func() (*toolchain, error) {
	tc, err := sharedTools()
	if err != nil {
		return nil, err
	}
	rendered, err := spokeSchema()
	if err != nil {
		return nil, err
	}
	embedded, err := schemaidx.Embedded()
	if err != nil {
		return nil, err
	}
	hubIdx, ok := embedded.Index(v1alpha1.APIVersion)
	if !ok {
		return nil, errors.New("no embedded ruralz/v1alpha1 schema")
	}
	hubViews, err := schemaview.Embedded(embedded)
	if err != nil {
		return nil, err
	}
	hubView, ok := hubViews.View(v1alpha1.APIVersion)
	if !ok {
		return nil, errors.New("no ruralz/v1alpha1 view")
	}
	idx, err := schemaidx.Load(spokeAPIVersion, rendered)
	if err != nil {
		return nil, fmt.Errorf("index the spoke schema: %w", err)
	}
	view, err := schemaview.Compile(rendered, idx)
	if err != nil {
		return nil, fmt.Errorf("compile the spoke schema: %w", err)
	}
	schemas, err := schemaidx.NewSet(hubIdx, idx)
	if err != nil {
		return nil, err
	}
	views, err := schemaview.NewSet(hubView, view)
	if err != nil {
		return nil, err
	}
	conv, err := convert.NewRegistry(convert.Lifecycle{}, convert.V1alpha1(), spokeConverter{})
	if err != nil {
		return nil, err
	}
	p, err := pipeline.New(pipeline.Options{
		Checks: checks.Table(), CEL: tc.cel, Workers: 2, Schemas: schemas, Views: views, Converters: conv,
	})
	if err != nil {
		return nil, err
	}
	return &toolchain{cel: tc.cel, pipe: p}, nil
})

// spokeTools returns the pipeline that serves ruralz/v1alpha1 and the
// spoke, failing tb when it does not build.
func spokeTools(tb testing.TB) *toolchain {
	tb.Helper()
	sp, err := sharedSpoke()
	if err != nil {
		tb.Fatalf("build the spoke pipeline: %v", err)
	}
	return sp
}

// spokeSchema returns the spoke's rendered schema: the rendered
// ruralz/v1alpha1 schema under spokeAPIVersion, its RouteSpec with
// backends for upstreams, timing.timeout for timeout and path from
// RouteMatch (match no longer required, RouteMatch without minProperties,
// since a match holding only a path moves out whole), and its UpstreamSpec
// with targets for endpoints. Every moved field keeps its schema and
// x-ruralz-* keywords.
func spokeSchema() ([]byte, error) {
	from := []byte(`"const": "` + v1alpha1.APIVersion + `"`)
	rendered := schema.RenderedV1alpha1()
	if !bytes.Contains(rendered, from) {
		return nil, fmt.Errorf("the rendered schema holds no %s", from)
	}
	rendered = bytes.ReplaceAll(rendered, from, []byte(`"const": "`+spokeAPIVersion+`"`))
	var doc map[string]any
	if err := json.Unmarshal(rendered, &doc); err != nil {
		return nil, err
	}
	defs, _ := doc["$defs"].(map[string]any)
	route, _ := defs["RouteSpec"].(map[string]any)
	match, _ := defs["RouteMatch"].(map[string]any)
	upstream, _ := defs["UpstreamSpec"].(map[string]any)
	if route == nil || match == nil || upstream == nil {
		return nil, errors.New("the rendered schema lacks RouteSpec, RouteMatch or UpstreamSpec")
	}
	routeProps, _ := route["properties"].(map[string]any)
	matchProps, _ := match["properties"].(map[string]any)
	upstreamProps, _ := upstream["properties"].(map[string]any)
	for _, need := range []struct {
		props map[string]any
		name  string
	}{{routeProps, "upstreams"}, {routeProps, "timeout"}, {matchProps, "path"}, {upstreamProps, "endpoints"}} {
		if _, ok := need.props[need.name]; !ok {
			return nil, fmt.Errorf("the rendered schema has no %s to move", need.name)
		}
	}
	routeProps["backends"] = routeProps["upstreams"]
	delete(routeProps, "upstreams")
	routeProps["timing"] = map[string]any{
		"type": "object", "additionalProperties": false,
		"description": "Timing holds the whole-request deadline.",
		"properties":  map[string]any{"timeout": routeProps["timeout"]},
	}
	delete(routeProps, "timeout")
	routeProps["path"] = matchProps["path"]
	delete(matchProps, "path")
	delete(match, "minProperties")
	route["allOf"] = renameRequired(route["allOf"], "upstreams", "backends")
	if req, ok := route["required"].([]any); ok {
		route["required"] = slices.DeleteFunc(req, func(v any) bool { return v == "match" })
	}
	upstreamProps["targets"] = upstreamProps["endpoints"]
	delete(upstreamProps, "endpoints")
	upstream["allOf"] = renameRequired(upstream["allOf"], "endpoints", "targets")
	return json.Marshal(doc)
}

// renameRequired returns v, a JSON Schema fragment, with every string from
// in its required arrays replaced by to.
func renameRequired(v any, from, to string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "required" {
				if arr, ok := val.([]any); ok {
					for i, s := range arr {
						if s == from {
							arr[i] = to
						}
					}
				}
				continue
			}
			x[k] = renameRequired(val, from, to)
		}
		return x
	case []any:
		for i := range x {
			x[i] = renameRequired(x[i], from, to)
		}
		return x
	default:
		return v
	}
}

// spokeConverter converts the spoke both ways: renames and moves fields
// of Route and Upstream resources, whole or partial (an overlay document,
// whose null removes a field), keeping every other member in place.
type spokeConverter struct{}

func (spokeConverter) APIVersion() string { return spokeAPIVersion }

// ToHub converts a spoke resource to the hub.
func (spokeConverter) ToHub(kind string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	spec, ok := doc.Get("spec")
	if !ok || spec.Kind != tree.KindMap {
		return doc, nil
	}
	switch kind {
	case string(v1alpha1.KindRoute):
		renameMember(spec, "backends", "upstreams")
		if timing, ok := spec.Get("timing"); ok {
			spec.Delete("timing")
			switch timing.Kind {
			case tree.KindMap:
				if v, ok := timing.Get("timeout"); ok {
					spec.Set("timeout", tree.Pos{}, v)
				}
			case tree.KindNull:
				spec.Set("timeout", tree.Pos{}, timing)
			default:
			}
		}
		if p, ok := spec.Get("path"); ok {
			spec.Delete("path")
			ensureMap(spec, "match").Set("path", tree.Pos{}, p)
		}
	case string(v1alpha1.KindUpstream):
		renameMember(spec, "targets", "endpoints")
	}
	return doc, nil
}

// FromHub converts a hub resource to the spoke.
func (spokeConverter) FromHub(kind string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	spec, ok := doc.Get("spec")
	if !ok || spec.Kind != tree.KindMap {
		return doc, nil
	}
	switch kind {
	case string(v1alpha1.KindRoute):
		renameMember(spec, "upstreams", "backends")
		if v, ok := spec.Get("timeout"); ok {
			spec.Delete("timeout")
			if v.Kind == tree.KindNull {
				spec.Set("timing", tree.Pos{}, v)
			} else {
				spec.Set("timing", tree.Pos{}, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "timeout", Value: v}}})
			}
		}
		if match, ok := spec.Get("match"); ok && match.Kind == tree.KindMap {
			if p, ok := match.Get("path"); ok {
				match.Delete("path")
				spec.Set("path", tree.Pos{}, p)
				if len(match.Members) == 0 {
					spec.Delete("match")
				}
			}
		}
	case string(v1alpha1.KindUpstream):
		renameMember(spec, "endpoints", "targets")
	}
	return doc, nil
}

// renameMember renames the member from of n to, in place.
func renameMember(n *tree.Node, from, to string) {
	for i := range n.Members {
		if n.Members[i].Key == from {
			n.Members[i].Key = to
		}
	}
}

// checkEveryField converts, for every field of the hub schema, a resource
// holding a generated value there to the spoke and back, and fails tb when
// one comes back changed or the schema lists suspiciously few fields.
func checkEveryField(tb testing.TB, conv *convert.Registry, hub *schemaidx.Index) {
	tb.Helper()
	count := 0
	var failure string
	for f := range hub.FieldsSeq() {
		if failure = roundTripField(tb, conv, f, &count); failure != "" {
			break
		}
	}
	if failure != "" {
		tb.Fatal(failure)
	}
	if count < 500 {
		tb.Fatalf("the hub schema lists %d fields; ruralz/v1alpha1 has many more", count)
	}
}

// roundTripField converts the resource fieldDoc builds for f to the spoke
// and back, counting it in count, and describes how the round trip
// failed; "" when it kept the resource.
func roundTripField(tb testing.TB, conv *convert.Registry, f schemaidx.Field, count *int) string {
	doc, ok := fieldDoc(f)
	if !ok {
		return ""
	}
	*count++
	down, ds := conv.Convert(convert.HubAPIVersion, spokeAPIVersion, f.Kind, doc.Clone(), false)
	if ds.HasErrors() {
		return fmt.Sprintf("%s %s: to the spoke: %s", f.Kind, f.Path, texts(ds))
	}
	up, ds := conv.Convert(spokeAPIVersion, convert.HubAPIVersion, f.Kind, down, false)
	if ds.HasErrors() {
		return fmt.Sprintf("%s %s: from the spoke: %s", f.Kind, f.Path, texts(ds))
	}
	if got, want := canonicalText(tb, up), canonicalText(tb, doc); got != want {
		return fmt.Sprintf("%s %s: the round trip through the spoke changed the resource:\n got %s\nwant %s", f.Kind, f.Path, got, want)
	}
	return ""
}

// fieldDoc returns a resource envelope of f.Kind whose position f.Path
// holds a generated value, every dispatch condition on the way holding;
// false for the resource root.
func fieldDoc(f schemaidx.Field) (*tree.Node, bool) {
	if f.Path == "" {
		return nil, false
	}
	root := &tree.Node{Kind: tree.KindMap}
	root.Set("apiVersion", tree.Pos{}, str(convert.HubAPIVersion))
	root.Set("kind", tree.Pos{}, str(f.Kind))
	root.Set("metadata", tree.Pos{}, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "name", Value: str("x")}}})
	for _, d := range f.Where {
		obj := ensurePath(root, d.Path)
		for _, c := range d.Conditions {
			if s, ok := c.Value.(string); ok && obj.Kind == tree.KindMap {
				obj.Set(c.Member, tree.Pos{}, str(s))
			}
		}
	}
	segs := schemaSegments(f.Path)
	parent := ensurePath(root, strings.Join(segs[:len(segs)-1], "."))
	leaf := generated(f.Node)
	switch last := segs[len(segs)-1]; last {
	case "[]":
		parent.Kind, parent.Members = tree.KindList, nil
		parent.Items = append(parent.Items, leaf)
	case "{}":
		parent.Set("k", tree.Pos{}, leaf)
	default:
		if parent.Kind != tree.KindMap {
			return nil, false
		}
		parent.Set(last, tree.Pos{}, leaf)
	}
	return root, true
}

// schemaSegments splits a schema path into member names, "[]" and "{}".
func schemaSegments(p string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(p); i++ {
		switch {
		case p[i] == '.':
			flush()
		case strings.HasPrefix(p[i:], "[]"), strings.HasPrefix(p[i:], "{}"):
			flush()
			out = append(out, p[i:i+2])
			i++
		default:
			cur.WriteByte(p[i])
		}
	}
	flush()
	return out
}

// ensurePath returns the node at the schema path p ("" is root), creating
// mappings, and lists for "[]", on the way.
func ensurePath(root *tree.Node, p string) *tree.Node {
	cur := root
	for _, s := range schemaSegments(p) {
		switch s {
		case "[]":
			if cur.Kind != tree.KindList {
				cur.Kind, cur.Members = tree.KindList, nil
			}
			if len(cur.Items) == 0 {
				cur.Items = append(cur.Items, &tree.Node{Kind: tree.KindMap})
			}
			cur = cur.Items[0]
		case "{}":
			cur = ensureMap(cur, "k")
		default:
			if cur.Kind != tree.KindMap {
				cur.Kind, cur.Items = tree.KindMap, nil
			}
			cur = ensureMap(cur, s)
		}
	}
	return cur
}

// generated returns a value the schema node admits, or a plausible one.
func generated(n *schemaidx.Node) *tree.Node {
	if e := n.Enum(); len(e) > 0 {
		return str(e[0])
	}
	switch n.Scalar() {
	case schemaidx.ScalarDuration:
		return str("90s")
	case schemaidx.ScalarByteSize:
		return str("10Mi")
	case schemaidx.ScalarDecimal:
		return str("1.50")
	case schemaidx.ScalarIntOrString:
		return &tree.Node{Kind: tree.KindInt, Text: "8080"}
	default:
	}
	switch t := n.Types(); {
	case t.Has(schemaidx.TypeString):
		return str("v")
	case t.Has(schemaidx.TypeNumber):
		return &tree.Node{Kind: tree.KindFloat, Text: "1.5"}
	case t.Has(schemaidx.TypeInteger):
		return &tree.Node{Kind: tree.KindInt, Text: "7"}
	case t.Has(schemaidx.TypeBoolean):
		return &tree.Node{Kind: tree.KindBool, Bool: true}
	case t.Has(schemaidx.TypeArray):
		return &tree.Node{Kind: tree.KindList, Items: []*tree.Node{str("e")}}
	default:
		return &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "m", Value: str("v")}}}
	}
}
