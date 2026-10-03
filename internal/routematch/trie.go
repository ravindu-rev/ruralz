// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

// Trie is a segment trie over exact paths and path templates, one per host
// entry of a Router (04 req 28). An edge is a literal segment or the one
// parameter edge; a trailing slash is a final empty literal segment, so
// "/v1/" and "/v1" end at different nodes. Values ending at one node keep
// insertion order: insert them sorted by [Compare] so ranks 5 and 6 hold.
//
// The zero value is empty and ready to use. Build it single-threaded; once
// built, [Trie.Lookup] is safe for concurrent use.
type Trie[V any] struct {
	root trieNode[V]
	n    int
}

type trieNode[V any] struct {
	literal map[string]*trieNode[V]
	param   *trieNode[V]
	// exact and template hold the values whose exact path or template ends
	// at this node.
	exact    []V
	template []V
}

func (n *trieNode[V]) child(literal string) *trieNode[V] {
	if n.literal == nil {
		n.literal = make(map[string]*trieNode[V])
	}
	c := n.literal[literal]
	if c == nil {
		c = new(trieNode[V])
		n.literal[literal] = c
	}
	return c
}

func (n *trieNode[V]) paramChild() *trieNode[V] {
	if n.param == nil {
		n.param = new(trieNode[V])
	}
	return n.param
}

// Len returns the number of values inserted.
func (t *Trie[V]) Len() int { return t.n }

// InsertExact adds v under the exact path, which must be normalized (see
// [NormalizePath]).
func (t *Trie[V]) InsertExact(path string, v V) {
	n := &t.root
	for i := 1; i <= len(path); {
		seg, next := NextSegment(path, i)
		n = n.child(seg)
		i = next
	}
	n.exact = append(n.exact, v)
	t.n++
}

// InsertTemplate adds v under the template.
func (t *Trie[V]) InsertTemplate(tp Template, v V) {
	n := &t.root
	for _, s := range tp.Segments {
		if s.Param != "" {
			n = n.paramChild()
		} else {
			n = n.child(s.Literal)
		}
	}
	if tp.TrailingSlash {
		n = n.child("")
	}
	n.template = append(n.template, v)
	t.n++
}

// Lookup calls yield for each value whose exact path or template matches
// the normalized path, in precedence ranks 2 and 3 order (04 req 31): every
// exact value first, then template values depth-first with a literal
// segment before a parameter, and within one node in insertion order. For a
// template value, raw holds its raw captures in template order (decode them
// with [DecodeParam]); for an exact value raw is empty. raw aliases buf and
// is valid only during the call. Lookup stops when yield returns false and
// allocates nothing when buf has room for the deepest template's captures.
func (t *Trie[V]) Lookup(path string, buf []string, yield func(v V, raw []string) bool) {
	if path == "" || path[0] != '/' {
		return
	}
	n := &t.root
	for i := 1; n != nil && i <= len(path); {
		seg, next := NextSegment(path, i)
		n = n.literal[seg]
		i = next
	}
	if n != nil {
		for _, v := range n.exact {
			if !yield(v, buf[:0]) {
				return
			}
		}
	}
	t.root.walk(path, 1, buf[:0], yield)
}

// walk visits the templates below n that match path from byte i. It returns
// false once yield has asked to stop. The recursion depth is bounded by the
// depth of the trie, which the configuration sets, not by the request.
func (n *trieNode[V]) walk(path string, i int, raw []string, yield func(V, []string) bool) bool {
	if i > len(path) {
		for _, v := range n.template {
			if !yield(v, raw) {
				return false
			}
		}
		return true
	}
	seg, next := NextSegment(path, i)
	if c := n.literal[seg]; c != nil && !c.walk(path, next, raw, yield) {
		return false
	}
	if n.param != nil && seg != "" {
		return n.param.walk(path, next, append(raw, seg), yield)
	}
	return true
}
