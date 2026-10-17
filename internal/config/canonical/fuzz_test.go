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
	"time"

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
			f.Add(dumpOf(f, nil, rev, rev.Digest))
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

// FuzzDecodeCanonical: arbitrary bytes never panic Decode, with its
// workers taking part once the content holds more than one chunk; a
// failure carries RZ-CFG-024, RZ-CFG-027 or, past a limit, RZ-CFG-001 (a
// scan failure keeps its jsonval error), and a success re-encodes to the
// input (02 reqs 26, 31 and 33; test plan 33). Under a cap of two
// findings (01 req 50) the outcome is the same with one worker and with
// three, at most three diagnostics, the last an RZ-CFG-001 exactly when
// the Findings are truncated, and, when not truncated, the outcome of the
// default cap. Under small limits (01 req 6: three resources, 40 values,
// depth 8) the outcome is the same with one worker and with three, a
// limit is one RZ-CFG-001 finding, and content within the limits has the
// outcome of the default limits. Yields never change an outcome (01 req
// 53): a decode whose goroutines yield at every read of their timer
// gives the same findings or trees, and a re-encoding that yields the
// same bytes.
func FuzzDecodeCanonical(f *testing.F) {
	addGolden(f, false)
	f.Add([]byte(doc(`{"kind":"Route","metadata":{"name":"r","owner":"x"},"spec":{}}`)))
	f.Add([]byte(`{"format":"ruralz.canonical.v2","resources":[]}`))
	f.Add([]byte(doc(`{"kind":"Plugin","metadata":{"name":"g"},"spec":{"configSchema":{"n":123456789012345680000}}}`)))
	// More resources than two chunks, so the workers take part.
	f.Add(routes(2*streamChunk + 1))
	// Findings past the small cap, of each code, before invalid JSON and
	// another format.
	f.Add([]byte(capCases()[5].gen(2*streamChunk + 3)))
	f.Add([]byte(capCases()[0].gen(5)))
	f.Add([]byte(`{"a":1,"b":2,"c":3,"format":"ruralz.canonical.v2"}`))
	f.Add([]byte(header + `{"kind":"Route","metadata":{"name":"r"},"spec":{"x":1,"y":2,"z":3}},]}`))
	dec := NewDecoder(hubIndex(f), DecoderOptions{Workers: 3})
	clock := &stepClock{step: time.Microsecond}
	yielding := NewDecoder(hubIndex(f), DecoderOptions{Workers: 3, Now: clock.now, YieldAfter: time.Microsecond})
	small := []*Decoder{
		NewDecoder(hubIndex(f), DecoderOptions{Workers: 1, MaxDiagnostics: 2}),
		NewDecoder(hubIndex(f), DecoderOptions{Workers: 3, MaxDiagnostics: 2}),
	}
	limited := []*Decoder{
		NewDecoder(hubIndex(f), DecoderOptions{Workers: 1, MaxResources: 3, MaxValues: 40, MaxDepth: 8}),
		NewDecoder(hubIndex(f), DecoderOptions{Workers: 3, MaxResources: 3, MaxValues: 40, MaxDepth: 8}),
	}
	f.Add([]byte(doc(`{"kind":"Route","metadata":{"name":"a"},"spec":{}}`, `{"kind":"Route","metadata":{"name":"b"},"spec":{}}`, `{"kind":"Route","metadata":{"name":"c"},"spec":{}}`, `{"kind":"Route","metadata":{"name":"d"},"spec":{}}`)))
	f.Fuzz(func(t *testing.T, data []byte) {
		rs, err := dec.Decode(context.Background(), data, Source{Name: "f.json"})
		yrs, yerr := yielding.Decode(context.Background(), data, Source{Name: "f.json"})
		if got, want := codeOf(yerr)+"\n"+describe(DiagnosticsOf(yerr)), codeOf(err)+"\n"+describe(DiagnosticsOf(err)); got != want || (err == nil) != (yerr == nil) {
			t.Fatalf("yielding: outcome %q, want %q", got, want)
		}
		if d := diffResources(rs, yrs); err == nil && d != "" {
			t.Fatalf("yielding: %s", d)
		}
		var limits [2]string
		for i, ld := range limited {
			_, lerr := ld.Decode(context.Background(), data, Source{Name: "f.json"})
			ds := DiagnosticsOf(lerr)
			limits[i] = codeOf(lerr) + "\n" + describe(ds)
			switch {
			case codeOf(lerr) == CodeLimit && (len(ds) != 1 || ds[0].Code != CodeLimit || findingsOf(lerr).Truncated):
				t.Fatalf("limits: a limit with diagnostics %v", ds)
			case codeOf(lerr) != CodeLimit && codeOf(err) != CodeLimit && limits[i] != codeOf(err)+"\n"+describe(DiagnosticsOf(err)):
				t.Fatalf("limits: outcome %q within the limits differs from the default limits' %v", limits[i], err)
			}
		}
		if limits[0] != limits[1] {
			t.Fatalf("limits: one worker gives %q, three give %q", limits[0], limits[1])
		}
		var outcomes [2]string
		for i, sd := range small {
			_, serr := sd.Decode(context.Background(), data, Source{Name: "f.json"})
			ds, fs := DiagnosticsOf(serr), findingsOf(serr)
			if len(ds) > 3 || fs.Truncated != (len(ds) == 3) || (fs.Truncated && ds[2].Code != CodeTooMany) {
				t.Fatalf("cap 2: %d diagnostics, truncated %v: %v", len(ds), fs.Truncated, ds)
			}
			outcomes[i] = codeOf(serr) + "\n" + describe(ds)
			if !fs.Truncated && outcomes[i] != codeOf(err)+"\n"+describe(DiagnosticsOf(err)) {
				t.Fatalf("cap 2 untruncated outcome %q differs from the default cap's %v", outcomes[i], err)
			}
		}
		if outcomes[0] != outcomes[1] {
			t.Fatalf("cap 2: one worker gives %q, three give %q", outcomes[0], outcomes[1])
		}
		if err != nil {
			if c := codeOf(err); c != CodeNewer && c != CodeMismatch && c != CodeLimit {
				t.Fatalf("Decode error %v has no RZ-CFG-024, RZ-CFG-027 or RZ-CFG-001 code", err)
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
		yrev, err := NewEncoder(hubIndex(t)).EncodeResources(context.Background(), yrs, Options{Workers: 3, Now: clock.now, YieldAfter: time.Microsecond})
		if err != nil || !bytes.Equal(yrev.Content, data) || yrev.Digest != rev.Digest {
			t.Fatalf("yielding: decoded content does not re-encode to itself: %v\n%q\n%q", err, data, yrev.Content)
		}
	})
}

// FuzzReadDump: arbitrary bytes never panic ReadDump; a failure is
// ErrNotDump, ErrDumpTooLarge or carries RZ-CFG-024, RZ-CFG-027 or
// RZ-CFG-001, and a success keeps the body unchanged (02 req 74; test
// plan 33).
func FuzzReadDump(f *testing.F) {
	addGolden(f, true)
	f.Add([]byte(`{"content":{},"digest":"sha256:00","revision":"rev-0"}`))
	dec := NewDecoder(hubIndex(f), DecoderOptions{})
	f.Fuzz(func(t *testing.T, data []byte) {
		d, _, err := dec.ReadDump(context.Background(), bytes.NewReader(data), 1<<20, Source{Name: "dump.json"})
		if err != nil {
			c := codeOf(err)
			if c != CodeNewer && c != CodeMismatch && c != CodeLimit && !errors.Is(err, ErrNotDump) && !errors.Is(err, ErrDumpTooLarge) {
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
