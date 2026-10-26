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
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// embedded loads the embedded hub schema index once for all tests.
var embedded = sync.OnceValues(func() (*schemaidx.Index, error) {
	set, err := schemaidx.Embedded()
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

// loadBundle parses every .yaml and .json file of a testdata directory
// with the restricted profile, as the loader does, into a hub Bundle.
func loadBundle(t testing.TB, dir string) *hub.Bundle {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	files := &tree.FileTable{}
	var rs []*hub.Resource
	for _, name := range names {
		f, ok := profile.FormatOf(name)
		if !ok {
			continue
		}
		src, err := os.ReadFile(name) //nolint:gosec // G304: a file under testdata.
		if err != nil {
			t.Fatal(err)
		}
		id := files.Add(tree.File{Path: filepath.ToSlash(name), Role: tree.RoleBase})
		docs, ds, err := profile.Parse(context.Background(), src, id, name, f, profile.Options{})
		if err != nil || len(ds) > 0 {
			t.Fatalf("Parse(%s) = %v, %v", name, ds, err)
		}
		for _, d := range docs {
			rs = append(rs, resourceOf(t, d.Root))
		}
	}
	if len(rs) == 0 {
		t.Fatalf("no resources in %s", dir)
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
