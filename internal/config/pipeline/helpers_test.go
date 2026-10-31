// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
)

// base holds what every test pipeline shares: the embedded schemas,
// compiled once per test binary, and one CEL compiler.
type base struct {
	schemas *schemaidx.Set
	views   *schemaview.Set
	cel     *cel.Compiler
}

// shared builds the base once: compiling the schema views takes a while.
var shared = sync.OnceValues(func() (*base, error) {
	schemas, err := schemaidx.Embedded()
	if err != nil {
		return nil, err
	}
	views, err := schemaview.Embedded(schemas)
	if err != nil {
		return nil, err
	}
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	return &base{schemas: schemas, views: views, cel: c}, nil
})

// newPipeline returns a Pipeline with o, filling in the shared schemas,
// the CEL compiler and no Policy checks where o leaves them unset.
func newPipeline(t testing.TB, o Options) *Pipeline {
	t.Helper()
	b, err := shared()
	if err != nil {
		t.Fatalf("build the shared schemas: %v", err)
	}
	if o.Schemas == nil {
		o.Schemas, o.Views = b.schemas, b.views
	}
	if o.CEL == nil {
		o.CEL = b.cel
	}
	if o.Checks == nil {
		o.Checks = hub.Checks{}
	}
	p, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// gatewayDoc is ruralz.yaml of the test Bundles: one http listener.
const gatewayDoc = `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
`

// upstreamDoc returns an Upstream document.
func upstreamDoc(name string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: ` + name + `
spec:
  protocol: http
  endpoints:
    - address: ` + name + `.internal:80
`
}

// routeDoc returns a Route document to Upstream up with tail appended
// under spec.
func routeDoc(name, up, tail string) string { return routeWhen(name, up, "", tail) }

// routeWhen returns a Route document to Upstream up whose match holds the
// lines of match, indented by four spaces, with tail appended under spec.
func routeWhen(name, up, match, tail string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: ` + name + `
spec:
  match:
    path: {prefix: /` + name + `}
` + match + `  upstreams:
    - name: ` + up + `
` + tail
}

// policyDoc returns a Policy document of type t with the YAML of spec
// members after type, indented by two spaces.
func policyDoc(name, t, spec string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: ` + name + `
spec:
  type: ` + t + `
` + spec
}

// docs joins YAML documents into one stream.
func docs(ds ...string) string { return strings.Join(ds, "---\n") }

// bundle builds a Bundle file system from path to content.
func bundle(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for p, c := range files {
		m[p] = &fstest.MapFile{Data: []byte(c), Mode: 0o644}
	}
	return m
}

// run runs p on m with req's other fields, failing the test on an error.
func run(t testing.TB, p *Pipeline, m fstest.MapFS, req Request) *Result {
	t.Helper()
	req.Source = loader.Source{FS: m, Path: "."}
	res, err := p.Run(t.Context(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// texts returns the one-line text form of each diagnostic.
func texts(l diag.List) []string {
	out := make([]string, len(l))
	for i, d := range l {
		out[i] = string(d.AppendText(nil))
	}
	return out
}

// codes returns the codes of l in order.
func codes(l diag.List) []string {
	out := make([]string, len(l))
	for i, d := range l {
		out[i] = d.Code
	}
	return out
}

// wantTexts fails unless the diagnostics' text forms equal want.
func wantTexts(t testing.TB, what string, got diag.List, want ...string) {
	t.Helper()
	if g := texts(got); !slices.Equal(g, want) {
		t.Errorf("%s: diagnostics\n%s\nwant\n%s", what, strings.Join(g, "\n"), strings.Join(want, "\n"))
	}
}

// wantClean fails on any diagnostic.
func wantClean(t testing.TB, what string, got diag.List) {
	t.Helper()
	if len(got) > 0 {
		t.Errorf("%s: unexpected diagnostics\n%s", what, strings.Join(texts(got), "\n"))
	}
}

// withCode returns the diagnostics of l with code.
func withCode(l diag.List, code string) diag.List {
	var out diag.List
	for _, d := range l {
		if d.Code == code {
			out = append(out, d)
		}
	}
	return out
}
