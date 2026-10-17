// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestComparePaths checks comparePaths against strings.Compare of the
// paths' texts (diag.Path.String), the order diag.List.Sort gives, on
// every pair of a corpus of paths: bare and bracketed member names (a
// "." only before a bare one after the first position), indexes, keyed
// entries with bare and quoted keys, set items of every diag.Item type
// and of other types, shared prefixes, one path the prefix of another,
// and texts that tie across element boundaries ("a.b" as one name or as
// two).
func TestComparePaths(t *testing.T) {
	elems := []diag.PathElem{
		diag.Field("a"), diag.Field("b"), diag.Field("a.b"), diag.Field(""), diag.Field("$x"), diag.Field("é"),
		diag.Field("a-b"), diag.Field("1a"), diag.Field("[x"), diag.Field("<&>"), diag.Field("a\"b"),
		diag.Index(0), diag.Index(2), diag.Index(10),
		diag.Keyed("name", "x"), diag.Keyed("name", "x y"), diag.Keyed("n", ""), diag.Keyed("name", "x]"),
		diag.Item("GET"), diag.Item("a b"), diag.Item(json.Number("1")), diag.Item(json.Number("1.0")), diag.Item(true),
		diag.Item(false), diag.Item(json.RawMessage(`{"a":1}`)), diag.Item(json.RawMessage(`[1]`)), diag.Item(3), diag.Item([]int{1}),
		{Kind: diag.ElemKind(9)},
	}
	r := rand.New(rand.NewPCG(3, 7)) //nolint:gosec // G404: a seeded corpus makes the property reproducible.
	paths := []diag.Path{nil, {diag.Field("a"), diag.Field("b")}, {diag.Field("a.b")}, {diag.Field("a")}}
	for range 150 {
		p := make(diag.Path, r.IntN(5))
		for i := range p {
			p[i] = elems[r.IntN(len(elems))]
		}
		paths = append(paths, p)
		// A twin sharing a prefix, so the skip of equal elements runs.
		q := slices.Clone(p)
		if len(q) > 0 {
			q[r.IntN(len(q))] = elems[r.IntN(len(elems))]
		}
		paths = append(paths, q, append(slices.Clone(p), elems[r.IntN(len(elems))]))
	}
	texts := make([]string, len(paths))
	for i, p := range paths {
		texts[i] = p.String()
	}
	for i, a := range paths {
		for j, b := range paths {
			want := strings.Compare(texts[i], texts[j])
			if got := comparePaths(a, b); got != want {
				t.Fatalf("comparePaths(%s, %s) = %d, want %d", a, b, got, want)
			}
		}
	}
}

// TestKeeperTiedLongPaths covers the keeper's path comparison (WP-37
// review round 3): diagnostics that tie on everything before the path
// come back in diag.List.Sort order, with paths that share a long
// authored key and differ after it, and the keeper never renders those
// paths whole, so its allocations stay far below their combined text.
func TestKeeperTiedLongPaths(t *testing.T) {
	key := strings.Repeat("k", 20_000)
	var ds diag.List
	for i := range 600 {
		ds = append(ds, diag.Diagnostic{
			Code: CodeSchema, Severity: diag.SeverityError, Location: diag.Location{File: "a.json", Line: 1, Column: 1},
			Path:    diag.Path{diag.Field("spec"), diag.Field("l"), diag.Keyed("name", key), diag.Field("m" + string(rune('a'+i%26)))},
			Message: "tie",
		})
	}
	want := slices.Clone(ds)
	want.Sort()
	k := newKeeper(200)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	k.add(0, ds)
	got := k.list()
	runtime.ReadMemStats(&after)
	for i := range got {
		if got[i].Path[3].Name != want[i].Path[3].Name {
			t.Fatalf("keeper diagnostic %d = %s, want %s", i, got[i].Path[3].Name, want[i].Path[3].Name)
		}
	}
	if len(got) != 200 {
		t.Fatalf("keeper kept %d, want 200", len(got))
	}
	if alloc, text := after.TotalAlloc-before.TotalAlloc, uint64(len(ds)*len(key)); alloc > text/20 {
		t.Errorf("keeper allocated %d bytes for paths of %d bytes of text, want under a twentieth", alloc, text)
	}
}

// longKeyCases build resources that pass stage F and give stage G 10,001
// RZ-CFG-005 diagnostics below one long authored key or member name
// (WP-37 review round 3): an auth.jwt issuer whose issuer key is long,
// and an auth.api-key config member with a long name.
func longKeyCases() map[string]func(key string) string {
	nulls := func(n int) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(`, "z` + fmt.Sprintf("%07d", i) + `": null`)
		}
		return b.String()
	}
	return map[string]func(key string) string{
		"jwt issuer key": func(key string) string {
			return policyDoc("auth.jwt", `{"issuers": [{"issuer": "`+key+`", "jwksUrl": "https://x.example/j", "audiences": ["a"]`+nulls(DefaultMaxDiagnostics+1)+`}]}`)
		},
		"api-key member name": func(key string) string {
			return policyDoc("auth.api-key", `{"header": "x-a", "`+key+`": {"k": 1`+nulls(DefaultMaxDiagnostics+1)+`}}`)
		},
	}
}

// TestRunLongKeyMemory checks Run's memory on longKeyCases (WP-37 review
// round 3: the keeper rendered every diagnostic's path, 1 GB for a 280 KB
// source): the 10,001 diagnostics below a 100,000-byte key allocate at
// most sixteen copies of the key more than below a 9-byte one, where
// rendering each path took one copy per diagnostic: the paths share the
// key, and nothing renders them.
func TestRunLongKeyMemory(t *testing.T) {
	s := newStage(t)
	for name, build := range longKeyCases() {
		t.Run(name, func(t *testing.T) {
			alloc := func(key string) (uint64, int) {
				res := envelope(mustParse(t, build(key)))
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				b, ds, err := s.Run(t.Context(), []*tree.Resource{res}, nil, 1)
				runtime.ReadMemStats(&after)
				if b != nil || err != nil || len(ds) != DefaultMaxDiagnostics+1 {
					t.Fatalf("Run = %v, %d diagnostics, %v; want no Bundle and %d", b, len(ds), err, DefaultMaxDiagnostics+1)
				}
				return after.TotalAlloc - before.TotalAlloc, len(build(key))
			}
			alloc("short-key") // warm the schema caches
			short, _ := alloc("short-key")
			long, src := alloc(strings.Repeat("i", 100_000))
			t.Logf("short key %d bytes, long key %d bytes allocated; source %d bytes", short, long, src)
			if extra := int64(long) - int64(short); extra > 16*100_000 { //nolint:gosec // G115: byte counts are far below 2^63.
				t.Errorf("a 100,000-byte key added %d bytes allocated, want at most sixteen copies of it (%d); source %d bytes", extra, 16*100_000, src)
			}
		})
	}
}
