// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"cmp"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// pathKind is the form of a Route's path criterion.
type pathKind uint8

// Path forms; anyPath stands for no path and for a regex, which proves
// nothing.
const (
	anyPath pathKind = iota
	exactPath
	prefixPath
	templatePath
)

// uriShape is what decides which URIs a Route can match: its listeners,
// its hosts (nil for any host) and its path criterion.
type uriShape struct {
	listeners []string
	hosts     []routematch.HostPattern
	kind      pathKind
	// path is the normalized exact or prefix path; tmpl the parsed
	// template.
	path string
	tmpl routematch.Template
}

// shapeOf returns the URI shape of a Route bound to listeners. A host or
// path that does not parse proves nothing: validation refuses it anyway.
func shapeOf(rt *v1alpha1.Route, listeners []string) uriShape {
	s := uriShape{listeners: listeners}
	m := &rt.Spec.Match
	for _, h := range m.Hosts {
		p, err := routematch.ParseHostPattern(h)
		if err != nil {
			s.hosts = nil
			break
		}
		s.hosts = append(s.hosts, p)
	}
	p := m.Path
	if p == nil {
		return s
	}
	switch {
	case p.Exact != "":
		if n, err := routematch.NormalizePath(p.Exact); err == nil {
			s.kind, s.path = exactPath, n
		}
	case p.Prefix != "":
		if n, err := routematch.NormalizePath(p.Prefix); err == nil {
			s.kind, s.path = prefixPath, n
		}
	case p.Template != "":
		if t, err := routematch.ParseTemplate(p.Template); err == nil {
			s.kind, s.tmpl = templatePath, t
		}
	default:
		// A regex proves nothing.
	}
	return s
}

// disjoint reports whether two Routes provably share no URI: their
// listeners, their hosts or their exact, prefix or template paths rule
// every common request out (docs/architecture/09-traffic-management-and-resilience.md
// "Response caching", Invalidating Routes).
func disjoint(a, b *uriShape) bool {
	return !listenersMeet(a.listeners, b.listeners) || !hostsMeet(a.hosts, b.hosts) || !pathsMeet(a, b)
}

// listenersMeet reports whether two sorted listener sets share a name.
func listenersMeet(a, b []string) bool {
	for _, l := range a {
		if _, ok := slices.BinarySearch(b, l); ok {
			return true
		}
	}
	return false
}

// hostsMeet reports whether some host matches an entry of a and one of b;
// no entries match every host.
func hostsMeet(a, b []routematch.HostPattern) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, x := range a {
		for _, y := range b {
			if hostPatternsMeet(x, y) {
				return true
			}
		}
	}
	return false
}

// hostPatternsMeet reports whether some host matches both x and y: two
// exact hosts when equal, an exact host and a wildcard when the wildcard
// matches it, two wildcards when one suffix ends with the other, as a
// host of more labels matches both ("*." matches one or more labels).
func hostPatternsMeet(x, y routematch.HostPattern) bool {
	switch {
	case x.Exact != "" && y.Exact != "":
		return x.Exact == y.Exact
	case x.Exact != "":
		return y.Match(x.Exact)
	case y.Exact != "":
		return x.Match(y.Exact)
	default:
		return strings.HasSuffix(x.Suffix, y.Suffix) || strings.HasSuffix(y.Suffix, x.Suffix)
	}
}

// pathsMeet reports whether some normalized path matches the path
// criteria of both shapes; no path or a regex matches any.
func pathsMeet(a, b *uriShape) bool {
	if a.kind == anyPath || b.kind == anyPath {
		return true
	}
	if a.kind > b.kind {
		a, b = b, a
	}
	switch {
	case a.kind == exactPath && b.kind == exactPath:
		return a.path == b.path
	case a.kind == exactPath && b.kind == prefixPath:
		return routematch.PrefixMatch(b.path, a.path)
	case a.kind == exactPath:
		_, ok := b.tmpl.Match(a.path, nil)
		return ok
	case a.kind == prefixPath && b.kind == prefixPath:
		return routematch.PrefixMatch(a.path, b.path) || routematch.PrefixMatch(b.path, a.path)
	case a.kind == prefixPath:
		return prefixMeetsTemplate(a.path, b.tmpl)
	default:
		return templatesMeet(a.tmpl, b.tmpl)
	}
}

// prefixMeetsTemplate reports whether some path matches prefix p on
// segment boundaries (routematch.PrefixMatch) and template t, each of
// whose parameters takes one non-empty segment. Prefix "/" matches every
// path. Any other such path starts with p's segments, each equal to t's
// literal or non-empty for a parameter, and t has at least as many
// segments; after a prefix that ends in "/" the path must go on, with
// another segment or t's trailing slash.
func prefixMeetsTemplate(p string, t routematch.Template) bool {
	q := strings.TrimSuffix(p, "/")
	if q == "" {
		return true
	}
	trailing := strings.HasSuffix(p, "/")
	segs := strings.Split(q[1:], "/")
	if len(segs) > len(t.Segments) {
		return false
	}
	for i, s := range segs {
		ts := t.Segments[i]
		if (ts.Param != "" && s == "") || (ts.Param == "" && s != ts.Literal) {
			return false
		}
	}
	if trailing {
		return len(t.Segments) > len(segs) || t.TrailingSlash
	}
	return true
}

// templatesMeet reports whether some path matches both templates: as many
// segments, the same trailing slash, and at each segment two equal
// literals or a parameter, which takes any non-empty segment.
func templatesMeet(a, b routematch.Template) bool {
	if len(a.Segments) != len(b.Segments) || a.TrailingSlash != b.TrailingSlash {
		return false
	}
	for i := range a.Segments {
		x, y := a.Segments[i], b.Segments[i]
		if x.Param == "" && y.Param == "" && x.Literal != y.Literal {
			return false
		}
	}
	return true
}

// cacheRoute is a Route whose chain holds a cache Policy, with its URI
// shape and that Policy.
type cacheRoute struct {
	name   string
	shape  uriShape
	policy *snapshot.Policy
}

// invalidators gives every invalidating Route without a cache Policy its
// invalidation entry: the cache Filter of the overlapping cache Route
// whose cache Policy has the largest stateStoreTimeout (the first such
// Route by name on a tie), after its Policies in Chain.Policies. The entry
// subscribes to no Phase, so the executor never runs it but calls its
// Invalidate after every request, which bumps on the cache Route's State
// Store connection with that Policy's timeout. A Route with a cache Policy
// invalidates through its own.
func (b *build) invalidators() {
	var caches []cacheRoute
	shapes := make([]uriShape, len(b.routes))
	for i, rt := range b.b.Routes() {
		shapes[i] = shapeOf(rt, b.routes[i].Listeners)
		for _, p := range b.routes[i].Chain.Policies {
			if p.Type != v1alpha1.PolicyTypeCache {
				continue
			}
			if _, ok := p.Filter.(filter.Invalidator); ok {
				caches = append(caches, cacheRoute{name: rt.Metadata.Name, shape: shapes[i], policy: p})
			}
			break
		}
	}
	if len(caches) == 0 {
		return
	}
	// The first overlapping cache Route in this order is the one whose
	// connection and timeout a bump takes.
	slices.SortStableFunc(caches, func(x, y cacheRoute) int {
		return cmp.Or(cmp.Compare(y.policy.StateStoreTimeout, x.policy.StateStoreTimeout), cmp.Compare(x.name, y.name))
	})
	for i, out := range b.routes {
		if holdsCachePolicy(out.Chain.Policies) {
			continue
		}
		for j := range caches {
			if disjoint(&shapes[i], &caches[j].shape) {
				continue
			}
			p := *caches[j].policy
			p.Index, p.Phases, p.FirstPhase, p.When = len(out.Chain.Policies), 0, 0, nil
			out.Chain.Policies = append(out.Chain.Policies, &p)
			break
		}
	}
}

// holdsCachePolicy reports whether ps holds a cache Policy.
func holdsCachePolicy(ps []*snapshot.Policy) bool {
	for _, p := range ps {
		if p.Type == v1alpha1.PolicyTypeCache {
			return true
		}
	}
	return false
}
