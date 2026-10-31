// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// shared builds the embedded schemas once per test binary: compiling the
// views takes a while.
var shared = sync.OnceValues(func() (*Loader, error) { return New(Options{}) })

// newLoader returns a Loader with o and the shared compiled schemas.
func newLoader(t testing.TB, o Options) *Loader {
	t.Helper()
	base, err := shared()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if o.Schemas == nil {
		o.Schemas, o.Views = base.schemas, base.views
	}
	l, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

// gateway is ruralz.yaml of the test Bundles.
const gateway = `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
`

// upstream returns an Upstream document.
func upstream(name string) string {
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

// route returns a Route document to upstream "orders".
func route(name string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: ` + name + `
spec:
  match:
    path: {prefix: /` + name + `}
  upstreams:
    - name: orders
`
}

// docs joins YAML documents into one stream.
func docs(ds ...string) string { return strings.Join(ds, "---\n") }

// mapFS builds a file system from path to content.
func mapFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for p, c := range files {
		m[p] = &fstest.MapFile{Data: []byte(c), Mode: 0o644}
	}
	return m
}

// symlink adds a symbolic link to m.
func symlink(m fstest.MapFS, path, target string) {
	m[path] = &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink | 0o777}
}

// minimal is the smallest valid Bundle: ruralz.yaml with the Gateway.
func minimal() map[string]string { return map[string]string{RootFile: gateway} }

// load runs Load on m with no Environment and no variables.
func load(t testing.TB, l *Loader, m fs.FS) *Result {
	t.Helper()
	r, err := l.Load(t.Context(), Request{Source: Source{FS: m, Path: "."}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

// loadEnv runs Load on m for Environment env of the environments file
// envs, stored in m at "envs.yaml".
func loadEnv(t testing.TB, l *Loader, m fstest.MapFS, envs, env string) *Result {
	t.Helper()
	files := fstest.MapFS{"envs.yaml": &fstest.MapFile{Data: []byte(envs)}}
	r, err := l.Load(t.Context(), Request{
		Source: Source{FS: m, Path: "."}, Environments: Source{FS: files, Path: "envs.yaml"}, Environment: env,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
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

// ids returns the identities of rs, "Kind/name", in order.
func ids(rs []*tree.Resource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID.String()
	}
	return out
}

// find returns the resource kind/name of rs.
func find(t testing.TB, rs []*tree.Resource, id string) *tree.Resource {
	t.Helper()
	for _, r := range rs {
		if r.ID.String() == id {
			return r
		}
	}
	t.Fatalf("no resource %s among %q", id, ids(rs))
	return nil
}

// at returns the node at a dotted path of field names, numbers indexing
// lists.
func at(t testing.TB, n *tree.Node, path string) *tree.Node {
	t.Helper()
	cur := n
	for _, seg := range strings.Split(path, ".") {
		if i, err := strconv.Atoi(seg); err == nil && cur.Kind == tree.KindList {
			if i >= len(cur.Items) {
				t.Fatalf("at(%s): index %d out of range", path, i)
			}
			cur = cur.Items[i]
			continue
		}
		next, ok := cur.Get(seg)
		if !ok {
			t.Fatalf("at(%s): no member %q in %s", path, seg, dump(cur))
		}
		cur = next
	}
	return cur
}

// dump writes n as JSON in member order, without positions.
func dump(n *tree.Node) string {
	var b bytes.Buffer
	var walk func(n *tree.Node)
	walk = func(n *tree.Node) {
		switch {
		case n == nil:
			b.WriteString("<nil>")
		case n.Kind == tree.KindMap:
			b.WriteByte('{')
			for i, m := range n.Members {
				if i > 0 {
					b.WriteByte(',')
				}
				q, _ := json.Marshal(m.Key)
				b.Write(q)
				b.WriteByte(':')
				walk(m.Value)
			}
			b.WriteByte('}')
		case n.Kind == tree.KindList:
			b.WriteByte('[')
			for i, it := range n.Items {
				if i > 0 {
					b.WriteByte(',')
				}
				walk(it)
			}
			b.WriteByte(']')
		case n.Kind == tree.KindString:
			q, _ := json.Marshal(n.Text)
			b.Write(q)
		case n.Kind == tree.KindBool:
			b.WriteString(strconv.FormatBool(n.Bool))
		case n.Kind == tree.KindNull:
			b.WriteString("null")
		default:
			b.WriteString(n.Text)
		}
	}
	walk(n)
	return b.String()
}

// dumpAll dumps every resource of rs in order.
func dumpAll(rs []*tree.Resource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.APIVersion + " " + r.ID.String() + " " + dump(r.Root)
	}
	return out
}

// registered fails on a diagnostic whose code internal/errcode does not
// register.
func registered(t testing.TB, l diag.List) {
	t.Helper()
	for _, d := range l {
		if _, ok := errcode.Lookup(d.Code); !ok {
			t.Errorf("diagnostic code %q is not registered: %s", d.Code, d.AppendText(nil))
		}
	}
}

// sameConverter converts a test apiVersion whose field set equals the
// hub's.
type sameConverter string

func (c sameConverter) APIVersion() string { return string(c) }

func (sameConverter) ToHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	return doc, nil
}

func (sameConverter) FromHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	return doc, nil
}

// twoVersions returns a Loader serving ruralz/v1alpha1 and ruralz/v1beta9,
// the committed rendered schema relabeled.
func twoVersions(t testing.TB) *Loader {
	t.Helper()
	const beta = "ruralz/v1beta9"
	src := bytes.ReplaceAll(schema.RenderedV1alpha1(), []byte(`"ruralz/v1alpha1"`), []byte(`"`+beta+`"`))
	alpha, err := schemaidx.Load("ruralz/v1alpha1", schema.RenderedV1alpha1())
	if err != nil {
		t.Fatal(err)
	}
	betaIdx, err := schemaidx.Load(beta, src)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := schemaidx.NewSet(alpha, betaIdx)
	if err != nil {
		t.Fatal(err)
	}
	va, err := schemaview.Compile(schema.RenderedV1alpha1(), alpha)
	if err != nil {
		t.Fatal(err)
	}
	vb, err := schemaview.Compile(src, betaIdx)
	if err != nil {
		t.Fatal(err)
	}
	views, err := schemaview.NewSet(va, vb)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := convert.NewRegistry(convert.Lifecycle{}, convert.V1alpha1(), sameConverter(beta))
	if err != nil {
		t.Fatal(err)
	}
	return newLoader(t, Options{Schemas: idx, Views: views, Converters: conv})
}

// vars is a variable source for tests.
func vars(kv ...string) subst.Map {
	m := subst.Map{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}
