// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Criteria are a Route's match criteria, a field-for-field mirror of
// v1alpha1.RouteMatch (Route.spec.match), so [MatchKey] stays independent of
// the API types (R-68). Callers build it with [CriteriaOf].
type Criteria struct {
	// Hosts are host entries; empty matches any host.
	Hosts []string
	// Path is the path criterion; nil matches any path.
	Path *PathCriteria
	// Methods are HTTP methods, exact and case-sensitive.
	Methods []string
	// Headers are header criteria.
	Headers []HeaderCriteria
	// GRPC is the gRPC service and method criterion (M3).
	GRPC *GRPCCriteria
	// GraphQL is the GraphQL operation criterion (M3).
	GraphQL *GraphQLCriteria
	// Topic is the event topic criterion (M4).
	Topic string
	// When is the CEL match.when source.
	When string
}

// PathCriteria mirror v1alpha1.PathMatch: exactly one form is set.
type PathCriteria struct {
	// Exact matches the whole path.
	Exact string
	// Prefix matches on segment boundaries.
	Prefix string
	// Template matches a path template.
	Template string
	// Regex matches an RE2 expression against the whole path.
	Regex string
}

// HeaderCriteria mirror v1alpha1.HeaderMatch: exactly one form is set.
type HeaderCriteria struct {
	// Name is the header name, compared case-insensitively.
	Name string
	// Exact equals the joined header value.
	Exact string
	// Regex fully matches the joined header value.
	Regex string
	// Present requires presence (true) or absence (false).
	Present *bool
}

// GRPCCriteria mirror v1alpha1.GRPCMatch.
type GRPCCriteria struct {
	// Service is the fully qualified service name.
	Service string
	// Method is the method name; empty means any.
	Method string
}

// GraphQLCriteria mirror v1alpha1.GraphQLMatch.
type GraphQLCriteria struct {
	// OperationType is the operation type, such as query.
	OperationType string
	// OperationName is the operation name.
	OperationName string
}

// CriteriaOf returns the Criteria of m, the one converter the Router and
// configuration validation share (R-68). The nested criteria are struct
// conversions of v1alpha1.PathMatch, HeaderMatch, GRPCMatch and
// GraphQLMatch, whose field sets are identical, so a field that diverges
// breaks compilation. The result shares no memory with m; a nil m gives
// the zero Criteria.
func CriteriaOf(m *v1alpha1.RouteMatch) Criteria {
	if m == nil {
		return Criteria{}
	}
	c := Criteria{
		Hosts:   slices.Clone(m.Hosts),
		Methods: slices.Clone(m.Methods),
		Topic:   m.Topic,
		When:    m.When,
	}
	if m.Path != nil {
		p := PathCriteria(*m.Path)
		c.Path = &p
	}
	if m.Headers != nil {
		c.Headers = make([]HeaderCriteria, len(m.Headers))
		for i := range m.Headers {
			c.Headers[i] = HeaderCriteria(m.Headers[i])
			if pr := m.Headers[i].Present; pr != nil {
				v := *pr
				c.Headers[i].Present = &v
			}
		}
	}
	if m.GRPC != nil {
		g := GRPCCriteria(*m.GRPC)
		c.GRPC = &g
	}
	if m.GraphQL != nil {
		g := GraphQLCriteria(*m.GraphQL)
		c.GraphQL = &g
	}
	return c
}

// Key is the canonical identity of a Route's match criteria. Two Routes
// bound to a common listener with equal keys are RZ-CFG-023 (04 req 31);
// validation compares keys per effective listener.
type Key string

// MatchKey returns the identity of c. The hosts, methods and headers are
// sets, so their order and repetition do not change the key; hosts take
// their canonical form (see [ParseHostPattern]) and header names are
// lower-cased (spec 01 risk 21). Paths are keyed in the form the Router
// matches, so spellings of one path share a key: an exact or prefix path
// as [NormalizePath] gives it, and a template by its canonical text
// ([Template.String]: normalized literals, parameter names kept). Every
// other difference, such as a template parameter name, a regex spelling, a
// method's case or a When source, gives a different key. MatchKey never
// fails: a host that does not parse is folded by case only, a path that
// does not parse is keyed verbatim, and validation reports both
// separately.
func MatchKey(c *Criteria) Key {
	b := make([]byte, 0, 64)
	b = append(b, "hosts="...)
	hosts := make([]string, len(c.Hosts))
	for i, h := range c.Hosts {
		if p, err := ParseHostPattern(h); err == nil {
			hosts[i] = p.String()
		} else {
			hosts[i] = lowerASCII(h)
		}
	}
	b = appendSet(b, hosts, true)
	b = append(b, " path="...)
	if p := c.Path; p != nil {
		b = appendFields(b, keyPath(p.Exact), keyPath(p.Prefix), keyTemplate(p.Template), p.Regex)
	} else {
		b = append(b, '-')
	}
	b = append(b, " methods="...)
	b = appendSet(b, slices.Clone(c.Methods), true)
	b = append(b, " headers="...)
	headers := make([]string, len(c.Headers))
	for i, h := range c.Headers {
		present := "-"
		if h.Present != nil {
			present = strconv.FormatBool(*h.Present)
		}
		headers[i] = string(appendFields(nil, lowerASCII(h.Name), h.Exact, h.Regex, present))
	}
	b = appendSet(b, headers, false)
	b = append(b, " grpc="...)
	if g := c.GRPC; g != nil {
		b = appendFields(b, g.Service, g.Method)
	} else {
		b = append(b, '-')
	}
	b = append(b, " graphql="...)
	if g := c.GraphQL; g != nil {
		b = appendFields(b, g.OperationType, g.OperationName)
	} else {
		b = append(b, '-')
	}
	b = append(b, " topic="...)
	b = strconv.AppendQuote(b, c.Topic)
	b = append(b, " when="...)
	b = strconv.AppendQuote(b, c.When)
	return Key(b)
}

// keyPath returns the key text of an exact or prefix path: its normalized
// form, s itself when it does not parse, and "" when it is unset.
func keyPath(s string) string {
	if !strings.HasPrefix(s, "/") {
		return s
	}
	if n, err := NormalizePath(s); err == nil {
		return n
	}
	return s
}

// keyTemplate returns the key text of a path template: its canonical text,
// s itself when it does not parse, and "" when it is unset.
func keyTemplate(s string) string {
	if s == "" {
		return s
	}
	if t, err := ParseTemplate(s); err == nil {
		return t.String()
	}
	return s
}

// appendSet appends the sorted, deduplicated elements of s, quoted when
// quote is set (elements that are not quoted are already unambiguous); it
// sorts s in place.
func appendSet(b []byte, s []string, quote bool) []byte {
	slices.Sort(s)
	s = slices.Compact(s)
	b = append(b, '[')
	for i, e := range s {
		if i > 0 {
			b = append(b, ',')
		}
		if quote {
			b = strconv.AppendQuote(b, e)
		} else {
			b = append(b, e...)
		}
	}
	return append(b, ']')
}

// appendFields appends the quoted fields in parentheses.
func appendFields(b []byte, fields ...string) []byte {
	b = append(b, '(')
	b = strconv.AppendQuote(b, fields[0])
	for _, f := range fields[1:] {
		b = append(b, ',')
		b = strconv.AppendQuote(b, f)
	}
	return append(b, ')')
}

// String returns the key text, which is readable in diagnostics.
func (k Key) String() string { return string(k) }
