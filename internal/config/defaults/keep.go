// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"bytes"
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// keeper collects the diagnostics of one Run from its workers and keeps
// the first n of them in diag.List.Sort order (01 req 50), ties broken by
// resource index and then by the order within the resource, so the kept
// set and its order do not depend on the worker count or on which worker
// finished first. Its memory stays below 2n diagnostics: once it holds
// 2n, it sorts and keeps n, and from then on drops at once every
// diagnostic that does not sort before the last one kept. The threshold
// is tested as len(entries)-n ≥ n, which cannot overflow for any n, so
// prunes stay at most one per n entries added however large n is. It is
// safe for concurrent use.
type keeper struct {
	mu sync.Mutex
	n  int
	// entries holds the candidates; after a prune, the first n in order.
	entries []entry
	// bound is the last entry kept by a prune; hasBound is false before
	// the first one that dropped anything.
	bound    entry
	hasBound bool
	// prunes counts the prunes add made, for tests.
	prunes int
}

// entry is one candidate diagnostic with its place in the Run input.
type entry struct {
	d diag.Diagnostic
	// res is the index of the resource in the Run input, seq the index
	// of the diagnostic in that resource's list.
	res, seq int
}

// newKeeper returns a keeper of the first n diagnostics (n ≥ 1).
func newKeeper(n int) *keeper { return &keeper{n: max(1, n)} }

// compareEntries orders by the key of diag.List.Sort (environment, file,
// line, column, code, path, message), then by resource index and order
// within the resource, which no two entries share. The paths are
// compared only when everything before them ties, and then by
// comparePaths, which never renders a whole path: a path repeats every
// ancestor's member name and key, so rendering one per diagnostic would
// hold the cap times an authored key's length.
func compareEntries(a, b entry) int {
	if c := cmp.Or(
		cmp.Compare(a.d.Environment, b.d.Environment),
		cmp.Compare(a.d.File, b.d.File),
		cmp.Compare(a.d.Line, b.d.Line),
		cmp.Compare(a.d.Column, b.d.Column),
		cmp.Compare(a.d.Code, b.d.Code),
	); c != 0 {
		return c
	}
	return cmp.Or(
		comparePaths(a.d.Path, b.d.Path),
		cmp.Compare(a.d.Message, b.d.Message),
		cmp.Compare(a.res, b.res),
		cmp.Compare(a.seq, b.seq),
	)
}

// comparePaths orders a and b as strings.Compare orders their texts
// (diag.Path.String), without building them: the leading elements both
// paths share (equal kind and value at the same position) give equal
// text and are skipped, and the rest is rendered one element at a time,
// as far as the first differing byte. Its memory is the text of the
// elements it renders, not of the paths.
func comparePaths(a, b diag.Path) int {
	i := 0
	for i < len(a) && i < len(b) && sameElem(a[i], b[i]) {
		i++
	}
	var ra, rb []byte
	ia, ib := i, i
	for {
		for len(ra) == 0 && ia < len(a) {
			ra = appendElemText(ra[:0], ia, a[ia])
			ia++
		}
		for len(rb) == 0 && ib < len(b) {
			rb = appendElemText(rb[:0], ib, b[ib])
			ib++
		}
		if len(ra) == 0 || len(rb) == 0 {
			return cmp.Compare(len(ra), len(rb))
		}
		k := min(len(ra), len(rb))
		if c := bytes.Compare(ra[:k], rb[:k]); c != 0 {
			return c
		}
		ra, rb = ra[k:], rb[k:]
	}
}

// sameElem reports path elements whose text is equal at any one position
// of a path: the same kind and value. An item of a type other than those
// of diag.Item's documentation is never the same, so its text decides.
func sameElem(x, y diag.PathElem) bool {
	if x.Kind != y.Kind {
		return false
	}
	switch x.Kind {
	case diag.ElemField:
		return x.Name == y.Name
	case diag.ElemIndex:
		return x.Index == y.Index
	case diag.ElemKeyed:
		return x.KeyField == y.KeyField && x.Key == y.Key
	case diag.ElemItem:
		switch v := x.Item.(type) {
		case string:
			w, ok := y.Item.(string)
			return ok && v == w
		case bool:
			w, ok := y.Item.(bool)
			return ok && v == w
		case json.Number:
			w, ok := y.Item.(json.Number)
			return ok && v == w
		case json.RawMessage:
			w, ok := y.Item.(json.RawMessage)
			return ok && bytes.Equal(v, w)
		default:
			return false
		}
	default:
		return false
	}
}

// appendElemText appends the text element e has at position i of a
// path's diag.Path.String form: its own text, after a "." when it is a
// member name written bare (not bracketed) after the first position.
func appendElemText(dst []byte, i int, e diag.PathElem) []byte {
	t := diag.Path{e}.String()
	if i > 0 && e.Kind == diag.ElemField && !strings.HasPrefix(t, "[") {
		dst = append(dst, '.')
	}
	return append(dst, t...)
}

// add offers the diagnostics of resource res.
func (k *keeper) add(res int, ds diag.List) {
	if len(ds) == 0 {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for j := range ds {
		e := entry{d: ds[j], res: res, seq: j}
		if k.hasBound && compareEntries(e, k.bound) >= 0 {
			continue
		}
		k.entries = append(k.entries, e)
		if len(k.entries)-k.n >= k.n {
			k.prunes++
			k.prune()
		}
	}
}

// prune sorts the entries and keeps the first n.
func (k *keeper) prune() {
	slices.SortFunc(k.entries, compareEntries)
	if len(k.entries) <= k.n {
		return
	}
	clear(k.entries[k.n:])
	k.entries = k.entries[:k.n]
	k.bound, k.hasBound = k.entries[k.n-1], true
}

// list returns the kept diagnostics in order.
func (k *keeper) list() diag.List {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.prune()
	if len(k.entries) == 0 {
		return nil
	}
	out := make(diag.List, len(k.entries))
	for i, e := range k.entries {
		out[i] = e.d
	}
	return out
}
