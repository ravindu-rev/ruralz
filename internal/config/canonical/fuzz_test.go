// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
)

// addGolden seeds f with the golden canonical documents of this package,
// a dump of each and the canonical documents of the configuration corpus
// (TQ "Fuzzing": seeded from the golden corpora).
func addGolden(f *testing.F, dumps bool) {
	f.Helper()
	names, err := filepath.Glob(filepath.Join("testdata", "golden", "*.json"))
	if err != nil {
		f.Fatal(err)
	}
	for _, name := range names {
		data, err := os.ReadFile(name) //nolint:gosec // G304: a seed under testdata.
		if err != nil {
			f.Fatal(err)
		}
		if dumps {
			rs, err := NewDecoder(hubIndex(f), DecoderOptions{}).Decode(context.Background(), data, Source{})
			if err != nil {
				f.Fatal(err)
			}
			rev, err := NewEncoder(hubIndex(f)).EncodeResources(context.Background(), rs, Options{})
			if err != nil {
				f.Fatal(err)
			}
			f.Add(AppendDump(nil, rev, rev.Digest))
			continue
		}
		f.Add(data)
	}
	corpus.AddCanonical(f)
}

// FuzzCanonical is the canonicalization oracle of TQ "Fuzzing": parse,
// canonicalize and parse again gives identical bytes (02 req 26, test
// plan 32), and the canonical text holds the same JSON value, with
// numbers compared as doubles.
func FuzzCanonical(f *testing.F) {
	addGolden(f, false)
	for _, s := range []string{
		`{"b":1,"a":[1.0,-0,1e21,1E-7,0.000001,5e-324,123456789012345678901],"é":" <&>","😀":{}}`,
		`[true,false,"\u0000\u001f\"\\/"]`,
		`{"דּ":1,"😀":2,"€":3}`,
		`1e400`, `null`, `{"a":null}`, `"x"`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := jsonNode(data)
		if err != nil {
			return
		}
		c1, err := AppendNode(nil, n)
		if err != nil {
			if !errors.Is(err, ErrEncode) {
				t.Fatalf("AppendNode error %v does not wrap ErrEncode", err)
			}
			return
		}
		n2, err := jsonNode(c1)
		if err != nil {
			t.Fatalf("canonical text %q does not parse: %v", c1, err)
		}
		c2, err := AppendNode(nil, n2)
		if err != nil || !bytes.Equal(c1, c2) {
			t.Fatalf("canonicalize twice:\n%q\n%q (%v)", c1, c2, err)
		}
		if !sameValue(n, n2) {
			t.Fatalf("canonical text %q changed the value of %q", c1, data)
		}
	})
}

// sameValue compares two trees as JSON values: objects by member set,
// lists in order, numbers by their double value (their RFC 8785 text).
func sameValue(a, b *tree.Node) bool {
	num := func(n *tree.Node) bool { return n.Kind == tree.KindInt || n.Kind == tree.KindFloat }
	switch {
	case num(a) && num(b):
		x, errX := jsonval.AppendCanonicalNumber(nil, json.Number(a.Text))
		y, errY := jsonval.AppendCanonicalNumber(nil, json.Number(b.Text))
		return errX == nil && errY == nil && bytes.Equal(x, y)
	case a.Kind != b.Kind:
		return false
	}
	switch a.Kind {
	case tree.KindBool:
		return a.Bool == b.Bool
	case tree.KindString:
		return a.Text == b.Text
	case tree.KindList:
		if len(a.Items) != len(b.Items) {
			return false
		}
		for i := range a.Items {
			if !sameValue(a.Items[i], b.Items[i]) {
				return false
			}
		}
		return true
	case tree.KindMap:
		if len(a.Members) != len(b.Members) {
			return false
		}
		index := make(map[string]*tree.Node, len(b.Members))
		for _, m := range b.Members {
			index[m.Key] = m.Value
		}
		for _, m := range a.Members {
			v, ok := index[m.Key]
			if !ok || !sameValue(m.Value, v) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

// FuzzDecodeCanonical: arbitrary bytes never panic Decode; a failure
// carries RZ-CFG-024 or RZ-CFG-027 (a scan failure keeps its jsonval error
// under RZ-CFG-027), and a success re-encodes to the input (02 reqs 26,
// 31 and 33; test plan 33).
func FuzzDecodeCanonical(f *testing.F) {
	addGolden(f, false)
	f.Add([]byte(doc(`{"kind":"Route","metadata":{"name":"r","owner":"x"},"spec":{}}`)))
	f.Add([]byte(`{"format":"ruralz.canonical.v2","resources":[]}`))
	f.Add([]byte(doc(`{"kind":"Plugin","metadata":{"name":"g"},"spec":{"configSchema":{"n":123456789012345680000}}}`)))
	dec := NewDecoder(hubIndex(f), DecoderOptions{Workers: 2})
	f.Fuzz(func(t *testing.T, data []byte) {
		rs, err := dec.Decode(context.Background(), data, Source{Name: "f.json"})
		if err != nil {
			if c := codeOf(err); c != CodeNewer && c != CodeMismatch {
				t.Fatalf("Decode error %v has no RZ-CFG-024 or RZ-CFG-027 code", err)
			}
			if len(DiagnosticsOf(err)) == 0 {
				t.Fatalf("Decode error %v without diagnostics", err)
			}
			return
		}
		rev, err := NewEncoder(hubIndex(t)).EncodeResources(context.Background(), rs, Options{})
		if err != nil || !bytes.Equal(rev.Content, data) {
			t.Fatalf("decoded content does not re-encode to itself: %v\n%q\n%q", err, data, rev.Content)
		}
	})
}

// FuzzReadDump: arbitrary bytes never panic ReadDump; a failure is
// ErrNotDump, ErrDumpTooLarge or carries RZ-CFG-024 or RZ-CFG-027, and a
// success keeps the body unchanged (02 req 74; test plan 33).
func FuzzReadDump(f *testing.F) {
	addGolden(f, true)
	f.Add([]byte(`{"content":{},"digest":"sha256:00","revision":"rev-0"}`))
	dec := NewDecoder(hubIndex(f), DecoderOptions{})
	f.Fuzz(func(t *testing.T, data []byte) {
		d, _, err := dec.ReadDump(context.Background(), bytes.NewReader(data), 1<<20, Source{Name: "dump.json"})
		if err != nil {
			c := codeOf(err)
			if c != CodeNewer && c != CodeMismatch && !errors.Is(err, ErrNotDump) && !errors.Is(err, ErrDumpTooLarge) {
				t.Fatalf("ReadDump error %v is not a dump or canonical error", err)
			}
			return
		}
		if !bytes.Equal(d.Body, data) {
			t.Fatal("ReadDump changed the body")
		}
		if _, err := dec.Verify(context.Background(), d.Content, d.Digest, Source{}); err != nil {
			t.Fatalf("read content does not verify: %v", err)
		}
	})
}

// FuzzReorderInvariance is the reorder property (WP-39): shuffling the
// members of every object and the order of the resources of a canonical
// document, with a fuzzed seed, re-encodes to the same bytes.
func FuzzReorderInvariance(f *testing.F) {
	names, err := filepath.Glob(filepath.Join("testdata", "golden", "*.json"))
	if err != nil {
		f.Fatal(err)
	}
	for i, name := range names {
		data, err := os.ReadFile(name) //nolint:gosec // G304: a seed under testdata.
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data, uint64(i)+1)
	}
	dec := NewDecoder(hubIndex(f), DecoderOptions{})
	f.Fuzz(func(t *testing.T, data []byte, seed uint64) {
		rs, err := dec.Decode(context.Background(), data, Source{})
		if err != nil {
			return
		}
		r := rand.New(rand.NewPCG(seed, seed>>1)) //nolint:gosec // G404: deterministic test data
		out := make([]*hub.Resource, len(rs))
		for i, p := range r.Perm(len(rs)) {
			out[i] = &hub.Resource{ID: rs[p].ID, Tree: shuffled(rs[p].Root, r)}
		}
		rev, err := NewEncoder(hubIndex(t)).Encode(context.Background(), hub.NewBundle(out), Options{Workers: 1 + int(seed%4)})
		if err != nil || !bytes.Equal(rev.Content, data) {
			t.Fatalf("reordered resources re-encode differently: %v", err)
		}
	})
}
