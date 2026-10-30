// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// schemaSet indexes the embedded schemas once for all tests.
var schemaSet = sync.OnceValues(schemaidx.Embedded)

// embedded loads the embedded hub schema index once for all tests.
var embedded = sync.OnceValues(func() (*schemaidx.Index, error) {
	set, err := schemaSet()
	if err != nil {
		return nil, err
	}
	idx, ok := set.Index("ruralz/v1alpha1")
	if !ok {
		return nil, errors.New("no ruralz/v1alpha1 index")
	}
	return idx, nil
})

// hubIndex returns the ruralz/v1alpha1 index.
func hubIndex(t testing.TB) *schemaidx.Index {
	t.Helper()
	idx, err := embedded()
	if err != nil {
		t.Fatalf("schema index: %v", err)
	}
	return idx
}

// synthetic is a schema of level 2 with real kind names, so the since
// rules and the level checks can be tested before any committed field
// carries x-ruralz-since.
const synthetic = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["apiVersion", "kind", "metadata", "spec"],
  "properties": {"apiVersion": {"const": "test/v1"}, "kind": {"enum": ["Gateway", "Route"]}},
  "allOf": [
    {"if": {"properties": {"kind": {"const": "Gateway"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Gateway"}},
    {"if": {"properties": {"kind": {"const": "Route"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Route"}}
  ],
  "$defs": {
    "Meta": {"type": "object", "additionalProperties": false, "required": ["name"], "properties": {
      "name": {"type": "string"},
      "labels": {"type": "object", "additionalProperties": {"type": "string"}},
      "annotations": {"type": "object", "additionalProperties": {"type": "string"}},
      "owner": {"type": "string", "default": "platform", "x-ruralz-since": 1}}},
    "Gateway": {"type": "object", "additionalProperties": false, "properties": {
      "apiVersion": {"const": "test/v1"}, "kind": {"const": "Gateway"},
      "metadata": {"$ref": "#/$defs/Meta"}, "spec": {"$ref": "#/$defs/GatewaySpec"}}},
    "GatewaySpec": {"type": "object", "additionalProperties": false, "properties": {
      "port": {"type": "integer"},
      "mode": {"type": "string", "default": "fast", "x-ruralz-since": 1},
      "extra": {"type": "string", "x-ruralz-since": 1},
      "future": {"type": "string", "x-ruralz-since": 2},
      "tuning": {"$ref": "#/$defs/Tuning"},
      "items": {"type": "array", "items": {"$ref": "#/$defs/Tuning"}, "x-ruralz-list": {"type": "atomic"}},
      "byName": {"type": "object", "additionalProperties": {"$ref": "#/$defs/Tuning"}},
      "open": {"type": "object"}}},
    "Tuning": {"type": "object", "additionalProperties": false, "properties": {
      "name": {"type": "string"},
      "depth": {"type": "integer", "default": 3, "x-ruralz-since": 1},
      "limits": {"type": "object", "default": {"b": 1, "a": 2.50}, "x-ruralz-since": 1,
        "properties": {"a": {"type": "number"}, "b": {"type": "integer"}}}}},
    "Route": {"type": "object", "additionalProperties": false, "properties": {
      "apiVersion": {"const": "test/v1"}, "kind": {"const": "Route"},
      "metadata": {"$ref": "#/$defs/Meta"}, "spec": {"type": "object"}}}
  }
}`

// syntheticIndex loads the synthetic schema.
func syntheticIndex(t testing.TB) *schemaidx.Index {
	t.Helper()
	idx, err := schemaidx.Load("test/v1", []byte(synthetic))
	if err != nil {
		t.Fatalf("Load(synthetic) error = %v", err)
	}
	return idx
}

// jsonTree builds a tree from one JSON text with the strict scanner,
// keeping member order and number text as the JSON front end does.
func jsonTree(t testing.TB, data string) *tree.Node {
	t.Helper()
	n, err := jsonNode([]byte(data))
	if err != nil {
		t.Fatalf("jsonTree(%.60q) error = %v", data, err)
	}
	return n
}

// jsonNode is jsonTree returning its error, for fuzz targets.
func jsonNode(data []byte) (*tree.Node, error) {
	var s jsonval.Scanner
	s.Reset(data, jsonval.ScanOptions{MaxDepth: jsonval.MaxDepthLimit})
	var value func(tok jsonval.Token) (*tree.Node, error)
	value = func(tok jsonval.Token) (*tree.Node, error) {
		n := &tree.Node{Style: tree.StyleJSON}
		switch tok.Kind {
		case jsonval.KindNull:
			n.Kind = tree.KindNull
		case jsonval.KindTrue, jsonval.KindFalse:
			n.Kind, n.Bool = tree.KindBool, tok.Kind == jsonval.KindTrue
		case jsonval.KindNumber:
			n.Kind, n.Text = tree.KindInt, s.Text(tok)
			if strings.ContainsAny(n.Text, ".eE") {
				n.Kind = tree.KindFloat
			}
		case jsonval.KindString:
			n.Kind, n.Text = tree.KindString, s.Text(tok)
		case jsonval.KindArrayStart:
			n.Kind, n.Items = tree.KindList, []*tree.Node{}
			for {
				it, err := s.Next()
				if err != nil {
					return nil, err
				}
				if it.Kind == jsonval.KindArrayEnd {
					return n, nil
				}
				v, err := value(it)
				if err != nil {
					return nil, err
				}
				n.Items = append(n.Items, v)
			}
		case jsonval.KindObjectStart:
			n.Kind = tree.KindMap
			for {
				k, err := s.Next()
				if err != nil {
					return nil, err
				}
				if k.Kind == jsonval.KindObjectEnd {
					return n, nil
				}
				vt, err := s.Next()
				if err != nil {
					return nil, err
				}
				v, err := value(vt)
				if err != nil {
					return nil, err
				}
				n.Members = append(n.Members, tree.Member{Key: s.Text(k), Value: v})
			}
		default:
			return nil, errors.New("unexpected token")
		}
		return n, nil
	}
	tok, err := s.Next()
	if err != nil {
		return nil, err
	}
	n, err := value(tok)
	if err != nil {
		return nil, err
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		return nil, err
	}
	return n, nil
}

// resourceOf returns the hub resource of an envelope tree.
func resourceOf(t testing.TB, root *tree.Node) *hub.Resource {
	t.Helper()
	k, ok := root.Get("kind")
	if !ok {
		t.Fatal("resource without kind")
	}
	meta, _ := root.Get("metadata")
	name, ok := meta.Get("name")
	if !ok {
		t.Fatal("resource without metadata.name")
	}
	return &hub.Resource{ID: tree.ID{Kind: v1alpha1.Kind(k.Text), Name: name.Text}, Tree: root}
}

// bundleOf returns a hub Bundle of JSON resources.
func bundleOf(t testing.TB, docs ...string) *hub.Bundle {
	t.Helper()
	rs := make([]*hub.Resource, len(docs))
	for i, d := range docs {
		rs[i] = resourceOf(t, jsonTree(t, d))
	}
	return hub.NewBundle(rs)
}

// encode encodes docs with the hub index.
func encode(t testing.TB, docs ...string) []byte {
	t.Helper()
	rev, err := NewEncoder(hubIndex(t)).Encode(context.Background(), bundleOf(t, docs...), Options{})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	return rev.Content
}

// codeOf returns err's RZ code, or "".
func codeOf(err error) string {
	c, _ := errcode.CodeOf(err)
	return c
}

// bundleFile is one configuration file of a test Bundle: its slash path and
// its content.
type bundleFile struct {
	path string
	data []byte
}

// dirFiles returns the configuration files directly in dir, a testdata or
// temporary directory, in path order.
func dirFiles(t testing.TB, dir string) []bundleFile {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	var out []bundleFile
	for _, name := range names {
		if _, ok := profile.FormatOf(name); !ok {
			continue
		}
		src, err := os.ReadFile(name) //nolint:gosec // G304: a file under testdata or t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, bundleFile{path: filepath.ToSlash(name), data: src})
	}
	return out
}

// goldenCorpus loads the configuration conformance corpus once for all
// tests: the one golden corpus of pipeline outputs, which holds the
// example Bundles this package's tests encode
// (docs/engineering/03-testing-and-quality-strategy.md "Golden tests").
var goldenCorpus = sync.OnceValues(corpus.Default)

// shopEntry is the corpus entry of the configuration model's example
// Bundle, examples/shop-bundle.
const shopEntry = "golden/shop-bundle"

// shopIgnored lists the files of the entry's bundle/ that its .ruralzignore
// keeps out of every render; its JSON twin, bundle-json/, holds none.
func shopIgnored() []string {
	return []string{"routes/orders-history.draft.yaml", "samples/orders-summary.json"}
}

// shopCorpusEntry returns the corpus entry of the example Bundle.
func shopCorpusEntry(t testing.TB) *corpus.Entry {
	t.Helper()
	c, err := goldenCorpus()
	if err != nil {
		t.Fatalf("load the golden corpus: %v", err)
	}
	e, ok := c.Entry(shopEntry)
	if !ok {
		t.Fatalf("the golden corpus has no entry %s", shopEntry)
	}
	return e
}

// shopFiles returns the base files of the example Bundle's directory dir in
// the golden corpus, corpus.BundleDir or its JSON twin corpus.BundleJSONDir,
// in path order: the configuration files outside overlays/, which only a
// render for an Environment reads, less those of shopIgnored. Discovery is
// stage A, which internal/config/loader runs a layer above this package
// (docs/engineering/02-repository-layout-and-conventions.md "Package
// layers"), so the test names the files a render leaves out, and
// TestShopBaseFiles holds the rest to the resources of the entry's render
// without --env.
func shopFiles(t testing.TB, dir string) []bundleFile {
	t.Helper()
	b, ok := shopCorpusEntry(t).Bundle(dir)
	if !ok {
		t.Fatalf("%s has no %s/", shopEntry, dir)
	}
	ignored := shopIgnored()
	if dir != corpus.BundleDir {
		ignored = nil
	}
	for _, name := range ignored {
		if _, ok := b.File(name); !ok {
			t.Fatalf("%s/%s has no file %s to leave out", shopEntry, dir, name)
		}
	}
	var out []bundleFile
	for _, f := range b.Files {
		if _, ok := profile.FormatOf(f.Name); !ok || strings.HasPrefix(f.Name, "overlays/") || slices.Contains(ignored, f.Name) {
			continue
		}
		out = append(out, bundleFile{path: f.Name, data: f.Data})
	}
	return out
}

// shopLevel returns the outputs of the example Bundle at the hub schema's
// level, the one level the encoder renders at.
func shopLevel(t testing.TB) *corpus.Level {
	t.Helper()
	n := hubIndex(t).Level()
	l, ok := shopCorpusEntry(t).Level(n)
	if !ok {
		t.Fatalf("%s has no %s", shopEntry, corpus.LevelDir(n))
	}
	return l
}

// parseFiles parses files with the restricted profile into envelope trees,
// as stages A to E hand them to stage F: each file registered in table
// with RoleBase, in the order given, and ${VAR} substituted over an empty
// variable set, as the render without --env does.
func parseFiles(t testing.TB, files []bundleFile, table *tree.FileTable) []*tree.Resource {
	t.Helper()
	set, err := schemaSet()
	if err != nil {
		t.Fatal(err)
	}
	sub := subst.New(subst.Options{Schemas: set, Files: table, Variables: subst.Map{}})
	var rs []*tree.Resource
	for _, file := range files {
		f, ok := profile.FormatOf(file.path)
		if !ok {
			t.Fatalf("%s is no configuration file", file.path)
		}
		id := table.Add(tree.File{Path: file.path, Role: tree.RoleBase})
		docs, ds, err := profile.Parse(context.Background(), file.data, id, file.path, f, profile.Options{})
		if err != nil || len(ds) > 0 {
			t.Fatalf("Parse(%s) = %v, %v", file.path, ds, err)
		}
		for _, d := range docs {
			k, _ := d.Root.Get("kind")
			meta, _ := d.Root.Get("metadata")
			n, _ := meta.Get("name")
			av, _ := d.Root.Get("apiVersion")
			r := &tree.Resource{ID: tree.ID{Kind: v1alpha1.Kind(k.Text), Name: n.Text}, APIVersion: av.Text, Root: d.Root, Start: d.Start}
			if ds := sub.Apply(r); len(ds) > 0 {
				t.Fatalf("substitute %s in %s: %v", r.ID, file.path, ds)
			}
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		t.Fatal("no resources to load")
	}
	return rs
}

// loadBundle parses files (parseFiles) into a hub Bundle.
func loadBundle(t testing.TB, files []bundleFile) *hub.Bundle {
	t.Helper()
	trees := parseFiles(t, files, &tree.FileTable{})
	rs := make([]*hub.Resource, len(trees))
	for i, r := range trees {
		rs[i] = resourceOf(t, r.Root)
	}
	return hub.NewBundle(rs)
}

// shuffled returns a deep copy of n with the members of every object and
// the order of rs permuted by r; lists keep their order (stage G sorts
// them).
func shuffled(n *tree.Node, r *rand.Rand) *tree.Node {
	c := n.Clone()
	var walk func(*tree.Node)
	walk = func(x *tree.Node) {
		if x == nil {
			return
		}
		r.Shuffle(len(x.Members), func(i, j int) { x.Members[i], x.Members[j] = x.Members[j], x.Members[i] })
		for _, m := range x.Members {
			walk(m.Value)
		}
		for _, it := range x.Items {
			walk(it)
		}
	}
	walk(c)
	return c
}

// mustDecode decodes content with the hub index.
func mustDecode(t testing.TB, content []byte) []tree.Resource {
	t.Helper()
	rs, err := NewDecoder(hubIndex(t), DecoderOptions{}).Decode(context.Background(), content, Source{Name: "c.json"})
	if err != nil {
		t.Fatalf("Decode() error = %v\n%s", err, content)
	}
	return rs
}

// reencode encodes decoded resources with the hub index.
func reencode(t testing.TB, rs []tree.Resource) []byte {
	t.Helper()
	rev, err := NewEncoder(hubIndex(t)).EncodeResources(context.Background(), rs, Options{})
	if err != nil {
		t.Fatalf("EncodeResources() error = %v", err)
	}
	return rev.Content
}

// sameBytes fails when got differs from want, showing where.
func sameBytes(t testing.TB, what string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		i := firstDiff(got, want)
		t.Fatalf("%s differs at byte %d:\n got %q\nwant %q", what, i, around(got, i), around(want, i))
	}
}

// around returns the bytes near offset i.
func around(b []byte, i int) []byte {
	return b[max(0, i-40):min(len(b), i+40)]
}
