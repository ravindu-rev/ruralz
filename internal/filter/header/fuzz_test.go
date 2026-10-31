// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package header

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// fuzzBases are the field names the property draws from, in lower case.
func fuzzBases() [3]string { return [3]string{"x-a", "x-bb", "x-c-c"} }

// byteReader hands out the fuzz input one byte at a time, then zeros.
type byteReader struct{ b []byte }

func (r *byteReader) next() byte {
	if len(r.b) == 0 {
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

// withCase returns base with the letter at position i upper-cased when bit
// i of mask is set.
func withCase(base string, mask byte) string {
	b := []byte(base)
	n := 0
	for i, c := range b {
		if c < 'a' || c > 'z' {
			continue
		}
		if mask&(1<<n) != 0 {
			b[i] = c - 'a' + 'A'
		}
		n++
	}
	return string(b)
}

// FuzzEditMatchesModel checks a side of literal values against a model of
// fields as case-insensitive names: for every named field, its lines are
// the canonical key's, then the other keys' in ascending byte order, with
// remove, set and add applied in that order, held under the canonical key
// alone; every other key and its lines stay as they were.
func FuzzEditMatchesModel(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{3, 0, 0, 2, 0, 1, 1, 1, 0, 2, 7, 3, 7, 3, 2})
	f.Add([]byte{6, 0, 5, 3, 0, 0, 1, 0, 2, 2, 1, 1, 1, 9, 2, 4, 3, 0, 7, 7, 7, 1, 2, 3, 4, 5, 6})
	f.Add([]byte{4, 1, 0, 0, 1, 255, 1, 1, 3, 2, 1, 6, 3, 0, 5, 5, 2, 8, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &byteReader{data}
		bases := fuzzBases()
		h := http.Header{}
		for i := range int(r.next() % 7) {
			key := withCase(bases[r.next()%3], r.next())
			for j := range int(r.next() % 4) {
				h[key] = append(h[key], "v"+strconv.Itoa(i)+"."+strconv.Itoa(j))
			}
			if _, ok := h[key]; !ok {
				h[key] = []string{}
			}
		}
		// Each list names each base at most once, in some case.
		ops := &v1alpha1.HeaderRequestOps{}
		removed, set, added := map[string]bool{}, map[string]string{}, map[string]string{}
		pick := func() []string {
			var out []string
			mask := r.next()
			for i, b := range bases {
				if mask&(1<<i) != 0 {
					out = append(out, b)
				}
			}
			return out
		}
		for _, b := range pick() {
			ops.Remove = append(ops.Remove, withCase(b, r.next()))
			removed[b] = true
		}
		for _, b := range pick() {
			set[b] = "s-" + b
			ops.Set = append(ops.Set, reqLit(withCase(b, r.next()), set[b]))
		}
		for _, b := range pick() {
			added[b] = "a-" + b
			ops.Add = append(ops.Add, reqLit(withCase(b, r.next()), added[b]))
		}
		flt, err := Factory{}.Build(t.Context(), filter.BuildEnv{Config: &v1alpha1.HeadersConfig{Request: ops}})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		orig := h.Clone()
		x := &filtertest.Exchange{Msg: &filtertest.Message{Hdr: h}}
		if res := flt.Handle(t.Context(), phase.OnRequestHeaders, x); res.Outcome != filter.Continue {
			t.Fatalf("Handle = %+v, want Continue", res)
		}
		want := model(orig, removed, set, added)
		if !equalHeader(h, want) {
			t.Errorf("header %q after %+v = %q, want %q", orig, ops, h, want)
		}
	})
}

// model applies remove, set and add to the fields of orig named by the
// three maps, keyed by lower-case name, as case-insensitive fields.
func model(orig http.Header, removed map[string]bool, set, added map[string]string) http.Header {
	named := func(base string) bool {
		_, s := set[base]
		_, a := added[base]
		return removed[base] || s || a
	}
	out := http.Header{}
	for k, v := range orig {
		if !named(strings.ToLower(k)) {
			out[k] = v
		}
	}
	for _, base := range fuzzBases() {
		if !named(base) {
			continue
		}
		canon := http.CanonicalHeaderKey(base)
		lines := slices.Clone(orig[canon])
		var others []string
		for k := range orig {
			if k != canon && strings.ToLower(k) == base {
				others = append(others, k)
			}
		}
		slices.Sort(others)
		for _, k := range others {
			lines = append(lines, orig[k]...)
		}
		if removed[base] {
			lines = nil
		}
		if v, ok := set[base]; ok {
			lines = []string{v}
		}
		if v, ok := added[base]; ok {
			lines = append(lines, v)
		}
		if len(lines) > 0 {
			out[canon] = lines
		}
	}
	return out
}
