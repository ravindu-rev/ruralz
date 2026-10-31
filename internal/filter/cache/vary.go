// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"container/list"
	"crypto/sha256"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// MaxLearnedURIs bounds the Vary-name LRU: the URIs whose Vary names a
// Node remembers ("Stateless data plane"; target).
const MaxLearnedURIs = 65536

// Names returns n, the Vary names of a response header h as a partition
// stores them (docs/architecture/11-scalability-and-distributed-state.md
// "Response Cache layout"): the members of every Vary line, trimmed,
// lower-cased, deduplicated, sorted by bytes and joined by "," without
// spaces; "" without Vary. A "*" member is never stored (Storable).
func Names(h http.Header) string {
	vs := h["Vary"]
	if len(vs) == 0 {
		return ""
	}
	var names []string
	for _, v := range vs {
		for m := range strings.SplitSeq(v, ",") {
			if m = httpfield.TrimOWS(m); m != "" {
				names = append(names, strings.ToLower(m))
			}
		}
	}
	slices.Sort(names)
	return strings.Join(slices.Compact(names), ",")
}

// Variant returns V, the variant digest of a request with header h under
// the Vary names names (Names form): the SHA-256 of one record per name,
// in order, the name, ":", then "-" when the request lacks the field, else
// "=" and its field lines, each trimmed of leading and trailing spaces and
// tabs, joined by ", ", then a line feed. Without names it is the SHA-256
// of the empty string. Field names match without regard to case: the
// canonical key's lines come first, then those of other spellings in key
// order.
func Variant(h http.Header, names string) statestore.Digest {
	if names == "" {
		return sha256.Sum256(nil)
	}
	d := sha256.New()
	var buf [256]byte
	for name := range strings.SplitSeq(names, ",") {
		b := append(buf[:0], name...)
		b = append(b, ':')
		lines := fieldLines(h, name)
		if len(lines) == 0 {
			b = append(b, '-')
		} else {
			b = append(b, '=')
			for i, l := range lines {
				if i > 0 {
					b = append(b, ", "...)
				}
				b = append(b, httpfield.TrimOWS(l)...)
			}
		}
		b = append(b, '\n')
		_, _ = d.Write(b)
	}
	var out statestore.Digest
	d.Sum(out[:0])
	return out
}

// fieldLines returns the lines of the field name in h: the canonical
// key's, then those of keys of other case in byte order.
func fieldLines(h http.Header, name string) []string {
	canonical := http.CanonicalHeaderKey(name)
	lines := h[canonical]
	var others []string
	for k := range h {
		if k != canonical && len(k) == len(name) && strings.EqualFold(k, name) {
			others = append(others, k)
		}
	}
	if len(others) == 0 {
		return lines
	}
	slices.Sort(others)
	out := slices.Clone(lines)
	for _, k := range others {
		out = append(out, h[k]...)
	}
	return out
}

// nameLRU is the Node's Vary-name LRU ("Response caching"): the Vary names
// it last learned per URI, from which a lookup computes V. It is safe for
// concurrent use.
type nameLRU struct {
	mu  sync.Mutex
	max int
	ll  list.List
	m   map[statestore.Digest]*list.Element
}

// nameItem is one learned URI.
type nameItem struct {
	uri   statestore.Digest
	names string
}

// newNameLRU returns an empty LRU of at most max URIs.
func newNameLRU(maxURIs int) *nameLRU {
	return &nameLRU{max: maxURIs, m: map[statestore.Digest]*list.Element{}}
}

// get returns the names learned for uri, "" when none.
func (l *nameLRU) get(uri statestore.Digest) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[uri]; e != nil {
		l.ll.MoveToFront(e)
		return e.Value.(*nameItem).names
	}
	return ""
}

// learn records names for uri, evicting the least recently used URI past
// the bound.
func (l *nameLRU) learn(uri statestore.Digest, names string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[uri]; e != nil {
		e.Value.(*nameItem).names = names
		l.ll.MoveToFront(e)
		return
	}
	l.m[uri] = l.ll.PushFront(&nameItem{uri: uri, names: names})
	for l.ll.Len() > l.max {
		old := l.ll.Back()
		l.ll.Remove(old)
		delete(l.m, old.Value.(*nameItem).uri)
	}
}

// len returns the number of learned URIs.
func (l *nameLRU) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ll.Len()
}
