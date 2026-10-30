// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/routematch"
)

// route is one compiled Route.
type route struct {
	index int32
	name  string
	key   routematch.Key
	// rank is the precedence position through the any-host entry; within
	// one host entry it orders Routes by ranks 2 to 6.
	rank routematch.Rank
	// hosts are the distinct host entries; the any-host pattern alone for
	// a Route without hosts.
	hosts []routematch.HostPattern

	// At most one of exact, tmpl, prefix and regex is set; none for a
	// Route without a path.
	exact  string
	tmpl   *template
	prefix string
	regex  *regexp.Regexp

	methods []string
	headers []routematch.HeaderMatcher
	// grpc is "/<service>/<method>", or "/<service>/" matched as a prefix
	// when grpcPrefix is set; empty without a grpc criterion.
	grpc       string
	grpcPrefix bool
	when       expr.Program
}

// compileRoute checks and compiles in's criteria.
func compileRoute(in *Route) (*route, error) {
	c := routematch.CriteriaOf(in.Match)
	switch {
	case c.GraphQL != nil:
		return nil, errcode.Errorf(codeUnserved, "match.graphql needs the request body; the Router matches pre-body data only")
	case c.Topic != "":
		return nil, errcode.Errorf(codeUnserved, "match.topic matches event ingress, which the HTTP Router does not serve")
	case c.When != "" && in.When == nil:
		return nil, errors.New("match.when has no compiled program")
	case c.When == "" && in.When != nil:
		return nil, errors.New("a match.when program is set without match.when")
	}
	rt := &route{index: in.Index, name: in.Name, key: routematch.MatchKey(&c), methods: c.Methods, when: in.When}
	if err := rt.compilePath(c.Path); err != nil {
		return nil, err
	}
	rank, err := routematch.RankOf(routematch.HostPattern{}, &c, in.Name)
	if err != nil {
		return nil, err
	}
	rt.rank = rank
	if rank.Path == routematch.PathTemplate {
		rt.tmpl = newTemplate(rank.Template)
	}
	seen := make(map[string]bool, len(c.Hosts))
	for _, h := range c.Hosts {
		p, err := routematch.ParseHostPattern(h)
		if err != nil {
			return nil, err
		}
		if s := p.String(); !seen[s] {
			seen[s] = true
			rt.hosts = append(rt.hosts, p)
		}
	}
	if len(rt.hosts) == 0 {
		rt.hosts = []routematch.HostPattern{{}}
	}
	for _, h := range c.Headers {
		m, err := routematch.CompileHeader(h)
		if err != nil {
			return nil, err
		}
		rt.headers = append(rt.headers, m)
	}
	if g := c.GRPC; g != nil {
		switch {
		case g.Service == "":
			return nil, errcode.Errorf(routematch.CodeInvalid, "match.grpc.service is empty")
		case g.Method == "":
			rt.grpc, rt.grpcPrefix = "/"+g.Service+"/", true
		default:
			rt.grpc = "/" + g.Service + "/" + g.Method
		}
	}
	return rt, nil
}

// compilePath checks p, which sets exactly one form, and keeps the exact
// path, prefix or regex in matching form; a template is kept from its
// rank.
func (rt *route) compilePath(p *routematch.PathCriteria) error {
	if p == nil {
		return nil
	}
	forms := 0
	for _, s := range []string{p.Exact, p.Prefix, p.Template, p.Regex} {
		if s != "" {
			forms++
		}
	}
	if forms != 1 {
		return errcode.Errorf(routematch.CodeInvalid, "match.path sets %d of exact, prefix, template and regex; want exactly one", forms)
	}
	var err error
	switch {
	case p.Exact != "":
		rt.exact, err = normalized(p.Exact)
	case p.Prefix != "":
		rt.prefix, err = normalized(p.Prefix)
	case p.Regex != "":
		rt.regex, err = routematch.CompileRegex(p.Regex)
	}
	return err
}

// normalized checks an exact path or prefix and returns the normalized
// form the Router matches.
func normalized(s string) (string, error) {
	if err := routematch.CheckPath(s); err != nil {
		return "", err
	}
	return routematch.NormalizePath(s)
}

// grpcMatch reports whether path passes the grpc criterion.
func (rt *route) grpcMatch(path string) bool {
	if rt.grpcPrefix {
		return strings.HasPrefix(path, rt.grpc)
	}
	return path == rt.grpc
}

// template is a compiled path template and where its captures are.
type template struct {
	t routematch.Template
	// names and segs are the parameter names and the 0-based positions of
	// their segments, in template order.
	names []string
	segs  []int
}

// newTemplate returns the compiled form of t.
func newTemplate(t routematch.Template) *template {
	tp := &template{t: t}
	for i, s := range t.Segments {
		if s.Param != "" {
			tp.names = append(tp.names, s.Param)
			tp.segs = append(tp.segs, i)
		}
	}
	return tp
}

// capture appends the captures of path, which tp matches, to params: each
// decoded for request.pathParams and as the path holds it, still
// percent-encoded, for request.rawPathParams (a substring of path, so it
// costs no allocation).
func (tp *template) capture(path string, params []expr.Param) []expr.Param {
	k := 0
	for i, seg := 1, 0; k < len(tp.segs) && i <= len(path); seg++ {
		s, next := routematch.NextSegment(path, i)
		if seg == tp.segs[k] {
			params = append(params, expr.Param{Name: tp.names[k], Value: routematch.DecodeParam(s), Raw: s})
			k++
		}
		i = next
	}
	return params
}

// matcher is the state of one Match call.
type matcher struct {
	q *snapshot.MatchRequest
	// preflight is the method a CORS preflight asks for, "" for any other
	// request (preflightMethod).
	preflight string
	// params is the caller's storage, length 0.
	params []expr.Param
	// route is the winner's index, -1 until a Route wins; won holds its
	// captures.
	route int32
	won   []expr.Param
	// err is the RZ-RT-006 error of a failed match.when.
	err error
}

// try checks rt's criteria beyond host and path, in the order of
// docs/architecture/03-data-plane.md Figure 2. It reports whether matching
// is over: rt won, or its match.when failed.
func (m *matcher) try(ctx context.Context, rt *route) bool {
	q := m.q
	if !routematch.MethodMatch(rt.methods, q.Method) && (m.preflight == "" || !slices.Contains(rt.methods, m.preflight)) {
		return false
	}
	for i := range rt.headers {
		h := &rt.headers[i]
		if !h.Match(fieldLines(q.Header, h)) {
			return false
		}
	}
	if rt.grpc != "" && !rt.grpcMatch(q.Path) {
		return false
	}
	params := m.params
	if rt.tmpl != nil {
		params = rt.tmpl.capture(q.Path, params)
	}
	if rt.when != nil {
		m.setPathParams(params)
		ok, err := rt.when.EvalBool(ctx, q.Vars)
		if err != nil {
			m.err = errcode.Wrap(codeWhen, fmt.Errorf("router: route %q: match.when: %w", rt.name, err))
			return true
		}
		if !ok {
			return false
		}
	}
	m.route, m.won = rt.index, params
	return true
}

// setPathParams sets request.pathParams of the activation, if any.
func (m *matcher) setPathParams(p []expr.Param) {
	if v := m.q.Vars; v != nil && v.Request != nil {
		v.Request.PathParams = p
	}
}

// fieldLines returns the field lines of h's header as CEL request.headers
// reads them (expr.JoinedHeader): those of the canonical key, else of the
// smallest key equal to the name under ASCII case folding.
func fieldLines(hdr http.Header, h *routematch.HeaderMatcher) []string {
	if lines, ok := hdr[h.Key]; ok {
		return lines
	}
	var (
		best  string
		lines []string
		found bool
	)
	for k, vs := range hdr {
		if equalFoldLower(k, h.Name) && (!found || k < best) {
			best, lines, found = k, vs, true
		}
	}
	return lines
}

// equalFoldLower reports whether s equals lower, an ASCII lower-case
// name, under ASCII case folding.
func equalFoldLower(s, lower string) bool {
	if len(s) != len(lower) {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}
