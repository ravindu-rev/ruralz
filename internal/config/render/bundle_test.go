// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// minimalGateway is the smallest valid Bundle root.
const minimalGateway = `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
`

// orderedSources is a small Bundle whose files list resources out of
// canonical order and author envelopes in unusual member order.
func orderedSources() []source {
	return []source{
		{"a-routes.yaml", []byte(`spec:
  upstreams: [{name: backend}]
  match: {path: {prefix: /}}
  timeout: 2s
metadata:
  annotations: {shop.example/owner: web}
  labels: {team: web}
  name: zeta
kind: Route
apiVersion: ruralz/v1alpha1
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata: {name: alpha}
spec:
  match: {path: {prefix: /alpha}}
  upstreams: [{name: backend}]
`)},
		{"b-upstreams.yaml", []byte(`kind: Upstream
apiVersion: ruralz/v1alpha1
metadata: {name: backend}
spec:
  protocol: http
  endpoints: [{address: "backend.svc:8080"}]
`)},
		{"ruralz.yaml", []byte(minimalGateway)},
	}
}

// documentIDs returns "<Kind>/<name>" of each YAML document of out.
func documentIDs(t *testing.T, out []byte) []string {
	t.Helper()
	docs, ds, err := profile.Parse(t.Context(), out, 0, "out.yaml", profile.FormatYAML, profile.Options{})
	if err != nil || len(ds) > 0 {
		t.Fatalf("parse rendered output: %v %v\n%s", ds, err, out)
	}
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = text(d.Root, "kind") + "/" + text(d.Root, "metadata", "name")
	}
	return ids
}

// memberKeys returns the member names of n.
func memberKeys(n *tree.Node) []string {
	keys := make([]string, len(n.Members))
	for i, m := range n.Members {
		keys[i] = m.Key
	}
	return keys
}

// TestCanonicalResourceOrder checks that resources are written by kind in
// the canonical order and then by name, whatever the file and document
// order, so moving a resource between files never changes the output.
func TestCanonicalResourceOrder(t *testing.T) {
	r := mustPipeline(t, orderedSources(), nil, subst.Map{})
	out := renderYAML(t, r)
	want := []string{"Gateway/edge", "Upstream/backend", "Route/alpha", "Route/zeta"}
	if got := documentIDs(t, out); !slices.Equal(got, want) {
		t.Errorf("document order = %q, want %q", got, want)
	}
	for _, perm := range [][]int{{3, 2, 1, 0}, {1, 3, 0, 2}, {2, 0, 3, 1}} {
		rs := make([]*tree.Resource, len(perm))
		for i, j := range perm {
			rs[i] = r.rendered[j]
		}
		var buf bytes.Buffer
		if _, err := WriteBundle(t.Context(), &buf, rs, FormatYAML, Options{Schemas: mustStages(t).idx}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), out) {
			t.Errorf("input order %v changes the output:\n%s", perm, buf.Bytes())
		}
	}
}

// TestEnvelopeMemberOrder checks that every rendered resource starts with
// apiVersion, kind, metadata and spec, and its metadata with name,
// labels and annotations, while the members below keep authored order.
func TestEnvelopeMemberOrder(t *testing.T) {
	r := mustPipeline(t, orderedSources(), nil, subst.Map{})
	docs, ds, err := Documents(t.Context(), r.rendered, Options{Schemas: mustStages(t).idx})
	if err != nil || len(ds) > 0 {
		t.Fatalf("Documents = %v, %v", ds, err)
	}
	for _, d := range docs {
		if got := memberKeys(d); !slices.Equal(got, []string{"apiVersion", "kind", "metadata", "spec"}) {
			t.Errorf("%s members = %q", text(d, "metadata", "name"), got)
		}
	}
	zeta := docs[len(docs)-1]
	meta, _ := zeta.Get("metadata")
	if got := memberKeys(meta); !slices.Equal(got, []string{"name", "labels", "annotations"}) {
		t.Errorf("metadata members = %q, want name, labels, annotations", got)
	}
	spec, _ := zeta.Get("spec")
	if got := memberKeys(spec); !slices.Equal(got, []string{"upstreams", "match", "timeout"}) {
		t.Errorf("spec members = %q, want the authored upstreams, match, timeout", got)
	}
}

// TestNoDefaultsWritten checks that render writes authored values only:
// the schema defaults (Route upstream weight, Gateway admin port,
// overridable) and the registry defaults (slot, failureMode,
// filterClass) that the canonical form materializes are absent, and
// values keep their authored spelling instead of the canonical one.
func TestNoDefaultsWritten(t *testing.T) {
	sources := []source{
		{"ruralz.yaml", []byte(minimalGateway + "  policies: [{name: limit}]\n")},
		{"policies.yaml", []byte(`apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: limit}
spec:
  type: ratelimit
  config:
    key: source.ip
    limits: [{requests: 10, window: 90s}]
`)},
		{"routes.yaml", []byte(`apiVersion: ruralz/v1alpha1
kind: Route
metadata: {name: web}
spec:
  match: {path: {prefix: /}}
  upstreams: [{name: backend}]
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata: {name: backend}
spec:
  protocol: http
  endpoints: [{address: "backend.svc:8080"}]
`)},
	}
	r := mustPipeline(t, sources, nil, subst.Map{})
	out := string(renderYAML(t, r))
	for _, absent := range []string{"weight", "admin", "overridable", "slot", "failureMode", "filterClass", "1m30s"} {
		if strings.Contains(out, absent) {
			t.Errorf("rendered Bundle holds %q, which the source does not author:\n%s", absent, out)
		}
	}
	if !strings.Contains(out, `window: "90s"`) {
		t.Errorf("rendered Bundle lost the authored window 90s:\n%s", out)
	}
	canon := string(r.validated.Revision.Content)
	for _, present := range []string{`"weight":1`, `"slot":"limit"`, `"window":"1m30s"`} {
		if !strings.Contains(canon, present) {
			t.Errorf("canonical form lacks %s, so the test proves nothing:\n%s", present, canon)
		}
	}
}

// TestLiteralDollarBraceEscaped checks the idempotence rule for $${: a
// literal ${ left by an escape, in an ordinary string, a CEL field and a
// substituted variable value, is written back as $${ in YAML and JSON,
// loads back to the same text and renders to the same bytes; keys are
// written as they are.
func TestLiteralDollarBraceEscaped(t *testing.T) {
	gateway := `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
  annotations:
    shop.example/price: "costs $${PRICE} today"
    shop.example/value: "${VALUE}"
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
  telemetry:
    accessLog:
      when: 'request.path.startsWith("/$${")'
`
	vars := subst.Map{"VALUE": "a ${B} c"}
	r := mustPipeline(t, []source{{"ruralz.yaml", []byte(gateway)}}, nil, vars)
	yamlOut := renderYAML(t, r)
	for _, want := range []string{
		`shop.example/price: "costs $${PRICE} today"`,
		`shop.example/value: "a $${B} c"`,
		`when: "request.path.startsWith(\"/$${\")"`,
	} {
		if !bytes.Contains(yamlOut, []byte(want)) {
			t.Errorf("YAML lacks %s:\n%s", want, yamlOut)
		}
	}
	jsonOut := renderAs(t, r, FormatJSON)
	for _, want := range []string{`"costs $${PRICE} today"`, `"a $${B} c"`} {
		if !bytes.Contains(jsonOut, []byte(want)) {
			t.Errorf("JSON lacks %s:\n%s", want, jsonOut)
		}
	}
	again := rendered(t, yamlOut)
	meta, _ := again.rendered[0].Root.Get("metadata")
	if got := text(meta, "annotations", "shop.example/value"); got != "a ${B} c" {
		t.Errorf("reloaded value = %q, want the substituted text a ${B} c", got)
	}
	if got := text(meta, "annotations", "shop.example/price"); got != "costs ${PRICE} today" {
		t.Errorf("reloaded price = %q, want the literal costs ${PRICE} today", got)
	}
	if again.validated.Revision.Digest != r.validated.Revision.Digest {
		t.Error("the reloaded Bundle builds another Revision")
	}
	if second := renderYAML(t, again); !bytes.Equal(second, yamlOut) {
		t.Errorf("second render differs:\n%s", second)
	}
}

// TestRetypedValuesKeepTheirType checks that a value that was exactly one
// ${VAR} expression is written with the type the rendered schema view gave
// it (an integer port, a boolean, a float), while a substituted string
// that looks like a number stays a quoted string, so the rendered Bundle
// validates against the rendered view as it stands.
func TestRetypedValuesKeepTheirType(t *testing.T) {
	gateway := `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
  labels:
    version: "${VERSION}"
spec:
  listeners:
    - name: https
      protocol: https
      port: ${HTTPS_PORT:-8443}
      http3: ${H3}
      tls:
        certificates:
          - name: edge
            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls.crt}}
            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls.key}}
  telemetry:
    traceSampling: ${RATE}
`
	vars := subst.Map{"VERSION": "1.3", "H3": "true", "RATE": "0.25"}
	r := mustPipeline(t, []source{{"ruralz.yaml", []byte(gateway)}}, nil, vars)
	out := renderYAML(t, r)
	for _, want := range []string{"port: 8443\n", "http3: true\n", "traceSampling: 0.25\n", "version: \"1.3\"\n"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("rendered Bundle lacks %q:\n%s", want, out)
		}
	}
	again := rendered(t, out)
	if again.validated.Revision.Digest != r.validated.Revision.Digest {
		t.Error("the reloaded Bundle builds another Revision")
	}
}

// TestRenderedValidatesUnderRenderedView checks the rendered.schema.json
// rule: every resource of the rendered example Bundle, parsed back,
// validates against the strict rendered view with no diagnostic, and no
// string holds an expression substitution would read.
func TestRenderedValidatesUnderRenderedView(t *testing.T) {
	// A ${VAR} or ${VAR:-default} expression not preceded by another $.
	substituted := regexp.MustCompile(`(^|[^$])\$\{[A-Za-z_][A-Za-z0-9_]*(:-[^}]*)?\}`)
	s := mustStages(t)
	for _, env := range []string{"prod", "staging", ""} {
		out := renderYAML(t, shop(t, env))
		files := &tree.FileTable{}
		for _, r := range parse(t.Context(), t, files, source{"ruralz.yaml", out}, tree.RoleBase) {
			ds, ok := s.views.Validate(r, files)
			if !ok || len(ds) > 0 {
				t.Errorf("env %q: %s: served %v, diagnostics:\n%s", env, r.ID, ok, diagText(t, ds))
			}
		}
		if m := substituted.Find(out); m != nil {
			t.Errorf("env %q: rendered Bundle holds the expression %s", env, m)
		}
	}
}

// TestJSONSameResources checks the --output json form: the YAML form's
// documents, in the same order, as one JSON array indented by two spaces
// with HTML characters unescaped and a final newline.
func TestJSONSameResources(t *testing.T) {
	r := shop(t, "prod")
	yamlOut, jsonOut := renderYAML(t, r), renderAs(t, r, FormatJSON)
	docs, ds, err := profile.Parse(t.Context(), yamlOut, 0, "out.yaml", profile.FormatYAML, profile.Options{})
	if err != nil || len(ds) > 0 {
		t.Fatal(ds, err)
	}
	fromYAML := make([]any, len(docs))
	for i, d := range docs {
		fromYAML[i] = d.Root.JSONValue()
	}
	dec := json.NewDecoder(bytes.NewReader(jsonOut))
	dec.UseNumber()
	var fromJSON []any
	if err := dec.Decode(&fromJSON); err != nil {
		t.Fatalf("decode JSON output: %v", err)
	}
	a, _ := json.Marshal(fromYAML)
	b, _ := json.Marshal(fromJSON)
	if !bytes.Equal(a, b) {
		t.Errorf("JSON and YAML forms hold different resources:\n%s\n%s", a, b)
	}
	if !bytes.HasPrefix(jsonOut, []byte("[\n  {\n    \"apiVersion\": ")) || !bytes.HasSuffix(jsonOut, []byte("}\n]\n")) {
		t.Errorf("JSON layout:\n%.80s ... %q", jsonOut, jsonOut[len(jsonOut)-8:])
	}
	if !bytes.Contains(jsonOut, []byte(`"response.status >= 400 || duration > duration(\"1s\")"`)) {
		t.Error("JSON output escapes HTML characters in CEL text")
	}
}

// TestWriteBundleInputErrors checks the errors of the rendered Bundle:
// a format other than YAML and JSON, no schema index, a nil resource, a
// root that is not a mapping, and an apiVersion or kind without schema.
func TestWriteBundleInputErrors(t *testing.T) {
	s := mustStages(t)
	files := &tree.FileTable{}
	good := resource(t, files, "ruralz.yaml", minimalGateway)
	unserved := resource(t, files, "x.yaml", strings.Replace(minimalGateway, "ruralz/v1alpha1", "ruralz/v9", 1))
	unknownKind := resource(t, files, "y.yaml", minimalGateway)
	unknownKind.ID.Kind = "Gizmo"
	scalarRoot := &tree.Resource{ID: good.ID, APIVersion: good.APIVersion, Root: &tree.Node{Kind: tree.KindString, Text: "x"}}
	cases := []struct {
		name string
		rs   []*tree.Resource
		f    Format
		o    Options
		want error
	}{
		{"text format", []*tree.Resource{good}, FormatText, Options{Schemas: s.idx}, ErrFormat},
		{"zero format", []*tree.Resource{good}, 0, Options{Schemas: s.idx}, ErrFormat},
		{"no schemas", []*tree.Resource{good}, FormatYAML, Options{}, ErrOptions},
		{"nil resource", []*tree.Resource{good, nil}, FormatYAML, Options{Schemas: s.idx}, ErrInput},
		{"scalar root", []*tree.Resource{scalarRoot}, FormatJSON, Options{Schemas: s.idx}, ErrInput},
		{"unserved apiVersion", []*tree.Resource{unserved}, FormatYAML, Options{Schemas: s.idx}, ErrInput},
		{"unknown kind", []*tree.Resource{unknownKind}, FormatYAML, Options{Schemas: s.idx}, ErrInput},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		_, err := WriteBundle(t.Context(), &buf, tc.rs, tc.f, tc.o)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: WriteBundle error = %v, want %v", tc.name, err, tc.want)
		}
		if buf.Len() > 0 {
			t.Errorf("%s: wrote %d bytes on error", tc.name, buf.Len())
		}
	}
}

// failWriter fails every write.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestWriteBundleWriteError checks that a failing writer is reported.
func TestWriteBundleWriteError(t *testing.T) {
	files := &tree.FileTable{}
	rs := []*tree.Resource{resource(t, files, "ruralz.yaml", minimalGateway)}
	_, err := WriteBundle(t.Context(), failWriter{}, rs, FormatYAML, Options{Schemas: mustStages(t).idx})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("WriteBundle error = %v, want the writer's", err)
	}
}

// TestWriteBundleCancelled checks that an ended context stops a render
// before anything is written.
func TestWriteBundleCancelled(t *testing.T) {
	r := shop(t, "prod")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var buf bytes.Buffer
	if _, err := WriteBundle(ctx, &buf, r.rendered, FormatYAML, Options{Schemas: mustStages(t).idx}); !errors.Is(err, context.Canceled) {
		t.Errorf("WriteBundle error = %v, want context.Canceled", err)
	}
	if buf.Len() > 0 {
		t.Error("a canceled render wrote output")
	}
}

// TestRenderLeavesInputUnchanged checks that rendering, in both formats,
// changes neither the resources' order nor any tree.
func TestRenderLeavesInputUnchanged(t *testing.T) {
	r := mustPipeline(t, orderedSources(), nil, subst.Map{})
	before := snapshot(t, r.rendered)
	order := slices.Clone(r.rendered)
	renderYAML(t, r)
	renderAs(t, r, FormatJSON)
	if after := snapshot(t, r.rendered); !bytes.Equal(after, before) {
		t.Errorf("render changed its input:\n--- before\n%s--- after\n%s", before, after)
	}
	if !slices.Equal(order, r.rendered) {
		t.Error("render reordered its input slice")
	}
}

// snapshot writes the trees of rs, in slice order and member order.
func snapshot(t *testing.T, rs []*tree.Resource) []byte {
	t.Helper()
	docs := make([]*tree.Node, len(rs))
	for i, r := range rs {
		docs[i] = r.Root
	}
	var buf bytes.Buffer
	if err := profile.EncodeWith(&buf, docs, profile.EncodeOptions{Verbatim: true}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestConcurrentRenders checks that renders of one input on several
// goroutines give identical output (run under the race detector).
func TestConcurrentRenders(t *testing.T) {
	r := shop(t, "prod")
	want := renderYAML(t, r)
	var wg sync.WaitGroup
	outs := make([][]byte, 8)
	for i := range outs {
		wg.Go(func() {
			var buf bytes.Buffer
			if _, err := WriteBundle(t.Context(), &buf, r.rendered, FormatYAML, Options{Schemas: mustStages(t).idx}); err != nil {
				t.Error(err)
			}
			outs[i] = buf.Bytes()
		})
	}
	wg.Wait()
	for i, out := range outs {
		if !bytes.Equal(out, want) {
			t.Errorf("render %d differs", i)
		}
	}
}

// TestCanonicalOrderMatchesRevision checks that the rendered documents
// follow the Revision's resource order, hub.KindOrder and then name.
func TestCanonicalOrderMatchesRevision(t *testing.T) {
	r := shop(t, "prod")
	var want []string
	for _, res := range r.validated.Bundle.Resources() {
		want = append(want, string(res.Kind)+"/"+res.Name)
	}
	if got := documentIDs(t, renderYAML(t, r)); !slices.Equal(got, want) {
		t.Errorf("rendered order = %q\nRevision order = %q", got, want)
	}
	if hub.KindOrder(v1alpha1.KindGateway) != 0 {
		t.Error("the Gateway no longer sorts first")
	}
}

// diagCodes returns the codes of ds.
func diagCodes(ds diag.List) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Code
	}
	return out
}

// TestEncodeChangedCopy checks Encode on a changed copy of a document, as
// ruralz dev run --ephemeral-ports rewrites a listener port: the copy is
// written in the rendered form with the new port, and the documents
// Documents returned, and the render of the input, keep the old one.
func TestEncodeChangedCopy(t *testing.T) {
	r := mustPipeline(t, orderedSources(), nil, subst.Map{})
	docs, ds, err := Documents(t.Context(), r.rendered, Options{Schemas: mustStages(t).idx})
	if err != nil || len(ds) > 0 {
		t.Fatal(ds, err)
	}
	changed := slices.Clone(docs)
	changed[0] = docs[0].Clone()
	spec, _ := changed[0].Get("spec")
	listeners, _ := spec.Get("listeners")
	port, _ := listeners.Items[0].Get("port")
	port.Text = "18080"
	var buf bytes.Buffer
	if err := Encode(&buf, changed, FormatYAML); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("port: 18080\n")) {
		t.Errorf("the changed port is missing:\n%s", buf.Bytes())
	}
	if again := renderYAML(t, r); !bytes.Contains(again, []byte("port: 8080\n")) || bytes.Contains(again, []byte("18080")) {
		t.Errorf("changing a copy changed the input:\n%s", again)
	}
}

// TestEncodeErrors checks that Encode refuses a format other than YAML
// and JSON, and writes nothing when a document cannot be encoded.
func TestEncodeErrors(t *testing.T) {
	doc := resource(t, &tree.FileTable{}, "ruralz.yaml", minimalGateway).Root
	var buf bytes.Buffer
	if err := Encode(&buf, []*tree.Node{doc}, FormatText); !errors.Is(err, ErrFormat) {
		t.Errorf("Encode(text) error = %v, want ErrFormat", err)
	}
	bad := doc.Clone()
	meta, _ := bad.Get("metadata")
	meta.Set("annotations", tree.Pos{}, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
		{Key: "a", Value: &tree.Node{Kind: tree.KindString, Text: "\xff"}},
	}})
	for _, f := range []Format{FormatYAML, FormatJSON} {
		if err := Encode(&buf, []*tree.Node{doc, bad}, f); !errors.Is(err, profile.ErrEncode) {
			t.Errorf("Encode(%s, invalid UTF-8) error = %v, want profile.ErrEncode", f, err)
		}
	}
	if buf.Len() > 0 {
		t.Errorf("a failed Encode wrote:\n%s", buf.Bytes())
	}
}
