// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// routes returns canonical content of n Routes, the sixth (when there is
// one) holding a non-ASCII annotation so positions after it leave the
// locator's leading printable run.
func routes(n int) []byte {
	rs := make([]string, n)
	for i := range n {
		meta := `{"name":"r` + fmt.Sprintf("%04d", i) + `"}`
		if i == 5 {
			meta = `{"annotations":{"note":"é€😀"},"name":"r0005"}`
		}
		rs[i] = `{"kind":"Route","metadata":` + meta + `,"spec":{"match":{"path":{"prefix":"/` + strconv.Itoa(i) + `"}},"timeout":"4s"}}`
	}
	return []byte(doc(rs...))
}

// diffTree returns where two trees first differ, positions included, or
// "".
func diffTree(at string, a, b *tree.Node) string {
	switch {
	case a == nil || b == nil:
		if a != b {
			return at + ": one side is nil"
		}
		return ""
	case a.Kind != b.Kind || a.Style != b.Style || a.Pos != b.Pos || a.Text != b.Text || a.Bool != b.Bool:
		return fmt.Sprintf("%s: %v %v %+v %q %v != %v %v %+v %q %v", at, a.Kind, a.Style, a.Pos, a.Text, a.Bool, b.Kind, b.Style, b.Pos, b.Text, b.Bool)
	case len(a.Members) != len(b.Members) || len(a.Items) != len(b.Items) || (a.Items == nil) != (b.Items == nil):
		return at + ": different member or item counts"
	}
	for i := range a.Members {
		ma, mb := a.Members[i], b.Members[i]
		if ma.Key != mb.Key || ma.KeyPos != mb.KeyPos {
			return fmt.Sprintf("%s: member %d %q %+v != %q %+v", at, i, ma.Key, ma.KeyPos, mb.Key, mb.KeyPos)
		}
		if d := diffTree(at+"."+ma.Key, ma.Value, mb.Value); d != "" {
			return d
		}
	}
	for i := range a.Items {
		if d := diffTree(at+"["+strconv.Itoa(i)+"]", a.Items[i], b.Items[i]); d != "" {
			return d
		}
	}
	return ""
}

// diffResources returns where two decodes first differ, or "".
func diffResources(a, b []tree.Resource) string {
	if len(a) != len(b) {
		return fmt.Sprintf("%d resources != %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].APIVersion != b[i].APIVersion || a[i].Start != b[i].Start {
			return fmt.Sprintf("resource %d: %+v != %+v", i, a[i], b[i])
		}
		if d := diffTree(a[i].ID.String(), a[i].Root, b[i].Root); d != "" {
			return d
		}
	}
	return ""
}

// TestDecodeWorkersSameTrees: the decoded trees, positions included, do
// not depend on the worker count at any chunk boundary of the stream (no
// resource, fewer than one chunk, one chunk exactly, one more, many
// chunks), for content and for a dump, whose positions count from the
// document start.
func TestDecodeWorkersSameTrees(t *testing.T) {
	ctx := context.Background()
	for _, n := range []int{0, 1, streamChunk - 1, streamChunk, streamChunk + 1, 2 * streamChunk, 2*streamChunk + 1, 300} {
		content := routes(n)
		want := mustDecode(t, content)
		if len(want) != n {
			t.Fatalf("%d routes: decoded %d", n, len(want))
		}
		rev := revision.Revision{Content: content, Digest: revision.Sum(content)}
		body := dumpOf(t, nil, rev, revision.Digest{})
		_, wantDump, err := NewDecoder(hubIndex(t), DecoderOptions{}).ReadDump(ctx, bytes.NewReader(body), 0, Source{Name: "c.json"})
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []int{2, 3, 8} {
			dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: w})
			got, err := dec.Decode(ctx, content, Source{Name: "c.json"})
			if err != nil {
				t.Fatalf("%d routes, workers %d: %v", n, w, err)
			}
			if d := diffResources(want, got); d != "" {
				t.Errorf("%d routes, workers %d: %s", n, w, d)
			}
			_, gotDump, err := dec.ReadDump(ctx, bytes.NewReader(body), 0, Source{Name: "c.json"})
			if err != nil {
				t.Fatal(err)
			}
			if d := diffResources(wantDump, gotDump); d != "" {
				t.Errorf("%d routes, workers %d, dump: %s", n, w, d)
			}
		}
	}
}

// TestDecodeWorkersSameFindings: the findings of every level, and their
// order, do not depend on the worker count, with findings spread over many
// chunks.
func TestDecodeWorkersSameFindings(t *testing.T) {
	content := routes(200)
	newer := bytes.ReplaceAll(content, []byte(`"timeout":"4s"`), []byte(`"timeout":"4s","zz":1`))
	newer = bytes.Replace(newer, []byte(`"resources":[`), []byte(`"aa":1,"resources":[`), 1)
	form := bytes.ReplaceAll(content, []byte(`{"kind":"Route","metadata":{"name":"r01`), []byte(`{"apiVersion":"x","kind":"Route","metadata":{"name":"r01`))
	for _, tc := range []struct {
		name    string
		content []byte
		code    string
		count   int
	}{
		{"newer", newer, CodeNewer, 201},
		{"form", form, CodeMismatch, 100},
	} {
		var first string
		for _, w := range []int{1, 2, 3, 8} {
			_, err := NewDecoder(hubIndex(t), DecoderOptions{Workers: w}).Decode(context.Background(), tc.content, Source{Name: "c.json"})
			ds := DiagnosticsOf(err)
			if codeOf(err) != tc.code || len(ds) != tc.count {
				t.Fatalf("%s, workers %d: %d findings, error %v", tc.name, w, len(ds), err)
			}
			got := describe(ds)
			if first == "" {
				first = got
			} else if got != first {
				t.Errorf("%s, workers %d: findings differ from one worker's", tc.name, w)
			}
		}
	}
}

// describe prints findings by value: code, location, resource, path and
// message.
func describe(ds diag.List) string {
	var b strings.Builder
	for _, d := range ds {
		res := ""
		if d.Resource != nil {
			res = d.Resource.Kind + "/" + d.Resource.Name
		}
		fmt.Fprintf(&b, "%s %s:%d:%d %s %s %s\n", d.Code, d.File, d.Line, d.Column, res, d.Path, d.Message)
	}
	return b.String()
}

// TestDecodedSlicesAreIndependent: every member and item slice of a
// decoded tree, the envelope roots included, has its length as capacity,
// so a later stage that appends to one reallocates instead of writing
// into the slab slot of a neighbor.
func TestDecodedSlicesAreIndependent(t *testing.T) {
	var walk func(*tree.Node)
	walk = func(n *tree.Node) {
		if cap(n.Members) != len(n.Members) || cap(n.Items) != len(n.Items) {
			t.Fatalf("node at %+v: members %d/%d, items %d/%d", n.Pos, len(n.Members), cap(n.Members), len(n.Items), cap(n.Items))
		}
		for _, m := range n.Members {
			walk(m.Value)
		}
		for _, it := range n.Items {
			walk(it)
		}
	}
	rs := mustDecode(t, mustEncodeBundle(t, largeBundle(t, 40)))
	for _, r := range rs {
		walk(r.Root)
	}
	// Appending to one resource's members leaves the next one intact.
	a, b := rs[1].Root, rs[2].Root
	before := describeTree(b)
	a.Members = append(a.Members, tree.Member{Key: "x", Value: &tree.Node{Kind: tree.KindNull}})
	if describeTree(b) != before {
		t.Error("appending to one resource's members changed its neighbor")
	}
}

// describeTree prints the keys and texts of n in order.
func describeTree(n *tree.Node) string {
	var b strings.Builder
	var walk func(*tree.Node)
	walk = func(n *tree.Node) {
		b.WriteString(n.Text)
		for _, m := range n.Members {
			b.WriteString("{" + m.Key + ":")
			walk(m.Value)
		}
		for _, it := range n.Items {
			b.WriteString("[")
			walk(it)
		}
	}
	walk(n)
	return b.String()
}

// countdown is a context whose Err is nil for its first live calls and
// context.Canceled after, from any goroutine.
type countdown struct {
	context.Context
	live atomic.Int64
}

func (c *countdown) Err() error {
	if c.live.Add(-1) >= 0 {
		return nil
	}
	return context.Canceled
}

// TestDecodeCanceledMidStream: a context that ends at any point of a
// decode, in the parser between resources or in a worker within a chunk,
// fails the decode with its error, and a decode the context outlives
// succeeds. The decode asks the context once, then once per resource as
// the parser hands it over and once as it is checked, so live counts below
// 1 + 2n end it and the others do not.
func TestDecodeCanceledMidStream(t *testing.T) {
	const n = streamChunk + 8
	content := routes(n)
	for _, w := range []int{1, 2, 4} {
		dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: w})
		for live := range int64(2*n + 4) {
			ctx := &countdown{Context: context.Background()}
			ctx.live.Store(live)
			rs, err := dec.Decode(ctx, content, Source{})
			switch {
			case live < 1+2*n && !errors.Is(err, context.Canceled):
				t.Errorf("workers %d, live %d: error = %v, want context.Canceled", w, live, err)
			case live >= 1+2*n && (err != nil || len(rs) != n):
				t.Errorf("workers %d, live %d: %d resources, error = %v", w, live, len(rs), err)
			}
		}
	}
}
