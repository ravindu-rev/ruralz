// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"cmp"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// PathTier is precedence rank 2 (04 req 31): the kind of path criterion.
type PathTier uint8

// Path tiers, highest precedence first.
const (
	// PathExact is path.exact.
	PathExact PathTier = iota
	// PathTemplate is path.template.
	PathTemplate
	// PathPrefix is path.prefix.
	PathPrefix
	// PathRegex is path.regex.
	PathRegex
	// PathAny is a Route without a path criterion.
	PathAny
)

// Rank is the precedence position of one candidate: a Route reached
// through one of its host entries. [Compare] orders ranks.
type Rank struct {
	// Host is the host entry the candidate is reached through; the zero
	// pattern for a Route without hosts.
	Host HostPattern
	// Path is the path tier.
	Path PathTier
	// Template is the parsed template of a PathTemplate candidate.
	Template Template
	// Prefix is the normalized prefix of a PathPrefix candidate.
	Prefix string
	// Constraints counts which of methods, headers, grpc and when are set
	// (0 to 4).
	Constraints int
	// Name is the Route's metadata.name.
	Name string
}

// RankOf returns the rank of the Route named name with criteria c, reached
// through host (the zero pattern for a Route without hosts). It fails, with
// [CodeInvalid], only for a template or prefix that validation rejects.
func RankOf(host HostPattern, c *Criteria, name string) (Rank, error) {
	r := Rank{Host: host, Path: PathAny, Constraints: Constraints(c), Name: name}
	switch p := c.Path; {
	case p == nil:
	case p.Exact != "":
		r.Path = PathExact
	case p.Template != "":
		t, err := ParseTemplate(p.Template)
		if err != nil {
			return Rank{}, err
		}
		r.Path, r.Template = PathTemplate, t
	case p.Prefix != "":
		prefix, err := NormalizePath(p.Prefix)
		if err != nil {
			return Rank{}, errcode.Errorf(CodeInvalid, "path prefix %q is not a valid path (%s)", p.Prefix, rejectReason(err))
		}
		r.Path, r.Prefix = PathPrefix, prefix
	case p.Regex != "":
		r.Path = PathRegex
	}
	return r, nil
}

// Constraints returns how many of methods, headers, grpc and when c sets:
// precedence rank 5 counts fields, not entries (04 req 31).
func Constraints(c *Criteria) int {
	n := 0
	if len(c.Methods) > 0 {
		n++
	}
	if len(c.Headers) > 0 {
		n++
	}
	if c.GRPC != nil {
		n++
	}
	if c.When != "" {
		n++
	}
	return n
}

// Compare returns a negative number when a takes precedence over b, a
// positive number when b does, and 0 only for equal positions. The order is
// total, highest first (04 req 31; DP "Precedence"):
//
//  1. Host tier: an exact host, then a wildcard with the longer suffix, then
//     no hosts.
//  2. Path tier: exact, template, prefix, regex, no path.
//  3. Within template: segment by segment from the left, a literal before a
//     parameter; where one template has run out, the longer one first (a
//     trailing slash counts as a final literal segment).
//  4. Within prefix: the longer prefix first.
//  5. More of methods, headers, grpc and when set first.
//  6. metadata.name in byte order.
//
// Ranks 3 and 4 do not apply to regex paths, which order by ranks 5 and 6
// only. Two templates that both match one path have the same number of
// segments, so rank 3 there is exactly the literal-before-parameter rule.
func Compare(a, b Rank) int {
	if c := cmp.Compare(a.Host.Tier(), b.Host.Tier()); c != 0 {
		return c
	}
	if c := cmp.Compare(len(b.Host.Suffix), len(a.Host.Suffix)); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Path, b.Path); c != 0 {
		return c
	}
	switch a.Path {
	case PathTemplate:
		if c := compareTemplates(&a.Template, &b.Template); c != 0 {
			return c
		}
	case PathPrefix:
		if c := cmp.Compare(len(b.Prefix), len(a.Prefix)); c != 0 {
			return c
		}
	case PathExact, PathRegex, PathAny:
	}
	if c := cmp.Compare(b.Constraints, a.Constraints); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// Segment kinds in rank 3 order.
const (
	kindLiteral = iota
	kindParam
	kindEnd
)

// compareTemplates is rank 3 over the kinds of the uniform segment lists
// (a trailing slash is a final literal).
func compareTemplates(a, b *Template) int {
	for i := 0; ; i++ {
		ka, kb := a.kind(i), b.kind(i)
		if c := cmp.Compare(ka, kb); c != 0 || ka == kindEnd {
			return c
		}
	}
}

// kind returns the kind of uniform segment i.
func (t *Template) kind(i int) int {
	switch {
	case i < len(t.Segments) && t.Segments[i].Param != "":
		return kindParam
	case i < len(t.Segments), i == len(t.Segments) && t.TrailingSlash:
		return kindLiteral
	default:
		return kindEnd
	}
}
