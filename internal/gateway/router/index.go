// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/routematch"
)

// entry is the path index of one host entry. Every list holds its Routes
// in precedence order.
type entry struct {
	root node
	// exact reports whether the trie holds an exact path.
	exact bool
	regex []*route
	any   []*route
}

// node is one node of the segment trie. An edge is a literal segment or
// the one parameter edge; a trailing slash is a final empty literal
// segment, so "/v1/" and "/v1" end at different nodes.
type node struct {
	literal map[string]*node
	param   *node
	// exact and template hold the Routes whose exact path or template
	// ends at the node.
	exact, template []*route
	// prefix holds the prefixes without a trailing slash that end at the
	// node: they match every path that reaches it. slash holds those with
	// one, keyed without it: they match a path that goes on past the node.
	prefix, slash []*route
	// templates and prefixes report whether a template or a prefix ends at
	// or below the node, so walks skip the other branches.
	templates, prefixes bool
}

// child returns the literal child for seg, creating it.
func (n *node) child(seg string) *node {
	if n.literal == nil {
		n.literal = make(map[string]*node)
	}
	c := n.literal[seg]
	if c == nil {
		c = new(node)
		n.literal[seg] = c
	}
	return c
}

// paramChild returns the parameter child, creating it.
func (n *node) paramChild() *node {
	if n.param == nil {
		n.param = new(node)
	}
	return n.param
}

// add appends rt to the list of its path. Compile adds Routes in
// precedence order, so every list stays sorted.
func (e *entry) add(rt *route) {
	switch {
	case rt.exact != "":
		n := &e.root
		for i := 1; i <= len(rt.exact); {
			seg, next := routematch.NextSegment(rt.exact, i)
			n = n.child(seg)
			i = next
		}
		n.exact = append(n.exact, rt)
		e.exact = true
	case rt.tmpl != nil:
		n := &e.root
		n.templates = true
		for _, s := range rt.tmpl.t.Segments {
			if s.Param != "" {
				n = n.paramChild()
			} else {
				n = n.child(s.Literal)
			}
			n.templates = true
		}
		if rt.tmpl.t.TrailingSlash {
			n = n.child("")
			n.templates = true
		}
		n.template = append(n.template, rt)
	case rt.prefix != "":
		// "/" keys the root with a trailing slash: every path goes on.
		p, slash := strings.CutSuffix(rt.prefix, "/")
		n := &e.root
		n.prefixes = true
		for i := 1; i <= len(p); {
			seg, next := routematch.NextSegment(p, i)
			n = n.child(seg)
			n.prefixes = true
			i = next
		}
		if slash {
			n.slash = append(n.slash, rt)
		} else {
			n.prefix = append(n.prefix, rt)
		}
	case rt.regex != nil:
		e.regex = append(e.regex, rt)
	default:
		e.any = append(e.any, rt)
	}
}

// match tries the entry's candidates for the request in precedence ranks
// 2 to 6: exact, template, prefix, regex, no path. It reports whether
// matching is over.
func (e *entry) match(ctx context.Context, m *matcher) bool {
	path := m.q.Path
	if strings.HasPrefix(path, "/") {
		if e.exact {
			if n := e.root.find(path); n != nil {
				for _, rt := range n.exact {
					if m.try(ctx, rt) {
						return true
					}
				}
			}
		}
		if e.root.templates && e.root.matchTemplates(ctx, m, path, 1) {
			return true
		}
		if e.root.prefixes && e.root.matchPrefixes(ctx, m, path, 1) {
			return true
		}
	}
	for _, rt := range e.regex {
		if rt.regex.MatchString(path) && m.try(ctx, rt) {
			return true
		}
	}
	for _, rt := range e.any {
		if m.try(ctx, rt) {
			return true
		}
	}
	return false
}

// find returns the node path's segments lead to over literal edges, or
// nil.
func (n *node) find(path string) *node {
	for i := 1; n != nil && i <= len(path); {
		seg, next := routematch.NextSegment(path, i)
		n = n.literal[seg]
		i = next
	}
	return n
}

// matchTemplates tries the templates below n that match path from byte i
// (the start of the next segment), depth first with a literal segment
// before the parameter edge: precedence rank 3. A parameter takes exactly
// one non-empty segment.
func (n *node) matchTemplates(ctx context.Context, m *matcher, path string, i int) bool {
	if i > len(path) {
		for _, rt := range n.template {
			if m.try(ctx, rt) {
				return true
			}
		}
		return false
	}
	seg, next := routematch.NextSegment(path, i)
	if c := n.literal[seg]; c != nil && c.templates && c.matchTemplates(ctx, m, path, next) {
		return true
	}
	if c := n.param; c != nil && seg != "" {
		return c.matchTemplates(ctx, m, path, next)
	}
	return false
}

// matchPrefixes tries the prefixes that match path from byte i, deepest
// first. Every matching prefix is a string prefix of the path, so a
// deeper one is longer (precedence rank 4), and at one node the prefix
// with a trailing slash is the longer.
func (n *node) matchPrefixes(ctx context.Context, m *matcher, path string, i int) bool {
	if i <= len(path) {
		seg, next := routematch.NextSegment(path, i)
		if c := n.literal[seg]; c != nil && c.prefixes && c.matchPrefixes(ctx, m, path, next) {
			return true
		}
		for _, rt := range n.slash {
			if m.try(ctx, rt) {
				return true
			}
		}
	}
	for _, rt := range n.prefix {
		if m.try(ctx, rt) {
			return true
		}
	}
	return false
}
