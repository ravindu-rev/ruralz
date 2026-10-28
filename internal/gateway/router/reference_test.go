// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The reference matcher is the oracle of the Router fuzz target
// (docs/engineering/03-testing-and-quality-strategy.md "Fuzzing": matches
// equal those of a reference matcher). It reads docs/architecture/
// 03-data-plane.md "Precedence" and Figure 2 literally: every (Route,
// host entry) pair whose host and path match is ranked with
// routematch.Compare, and the first whose other criteria hold wins; a
// match.when error stops. It shares only the routematch primitives with
// the Router, not its trie or its lists.

// refRoute is a Route as the reference matcher sees it.
type refRoute struct {
	index int32
	name  string
	c     routematch.Criteria
	when  expr.Program
}

// refResult is the reference answer.
type refResult struct {
	route  int32
	params []expr.Param
	code   string
}

// referenceMatch answers one request.
func referenceMatch(ctx context.Context, hostnames []string, routes []refRoute, method, rawHost, path string, h http.Header) refResult {
	none := refResult{route: -1}
	host := routematch.NormalizeHost(rawHost)
	if len(hostnames) > 0 && !slices.ContainsFunc(hostnames, func(s string) bool {
		p, err := routematch.ParseHostPattern(s)
		return err == nil && p.Match(host)
	}) {
		return none
	}
	type candidate struct {
		rt     *refRoute
		rank   routematch.Rank
		params []expr.Param
	}
	var cands []candidate
	for i := range routes {
		rt := &routes[i]
		patterns := []routematch.HostPattern{{}}
		if len(rt.c.Hosts) > 0 {
			patterns = nil
			for _, s := range rt.c.Hosts {
				p, _ := routematch.ParseHostPattern(s)
				patterns = append(patterns, p)
			}
		}
		for _, hp := range patterns {
			params, ok := refPath(rt.c.Path, path)
			if !hp.Match(host) || !ok {
				continue
			}
			rank, err := routematch.RankOf(hp, &rt.c, rt.name)
			if err != nil {
				panic(err)
			}
			cands = append(cands, candidate{rt: rt, rank: rank, params: params})
		}
	}
	slices.SortStableFunc(cands, func(a, b candidate) int { return routematch.Compare(a.rank, b.rank) })
	for _, c := range cands {
		if len(c.rt.c.Methods) > 0 && !slices.Contains(c.rt.c.Methods, method) {
			continue
		}
		if !refHeaders(c.rt.c.Headers, h) || !refGRPC(c.rt.c.GRPC, path) {
			continue
		}
		if c.rt.when != nil {
			v := &expr.Vars{Request: &expr.Request{Method: method, Host: host, Path: path, Header: h, PathParams: c.params}}
			ok, err := c.rt.when.EvalBool(ctx, v)
			if err != nil {
				return refResult{route: -1, code: "RZ-RT-006"}
			}
			if !ok {
				continue
			}
		}
		return refResult{route: c.rt.index, params: c.params}
	}
	return none
}

// refPath matches a path criterion and returns the decoded captures.
func refPath(c *routematch.PathCriteria, path string) ([]expr.Param, bool) {
	switch {
	case c == nil:
		return nil, true
	case c.Exact != "":
		n, err := routematch.NormalizePath(c.Exact)
		return nil, err == nil && n == path
	case c.Prefix != "":
		n, err := routematch.NormalizePath(c.Prefix)
		return nil, err == nil && routematch.PrefixMatch(n, path)
	case c.Template != "":
		t, err := routematch.ParseTemplate(c.Template)
		if err != nil {
			return nil, false
		}
		raw, ok := t.Match(path, nil)
		if !ok {
			return nil, false
		}
		var params []expr.Param
		for _, s := range t.Segments {
			if s.Param != "" {
				params = append(params, expr.Param{Name: s.Param, Value: routematch.DecodeParam(raw[len(params)])})
			}
		}
		return params, true
	default:
		rx, err := routematch.CompileRegex(c.Regex)
		return nil, err == nil && rx.MatchString(path)
	}
}

// refHeaders checks header criteria against the value CEL sees.
func refHeaders(cs []routematch.HeaderCriteria, h http.Header) bool {
	for _, c := range cs {
		v, ok := expr.JoinedHeader(h, c.Name)
		switch {
		case c.Present != nil:
			if ok != *c.Present {
				return false
			}
		case c.Regex != "":
			rx, err := routematch.CompileRegex(c.Regex)
			if err != nil || !ok || !rx.MatchString(v) {
				return false
			}
		default:
			if !ok || v != c.Exact {
				return false
			}
		}
	}
	return true
}

// refGRPC checks a grpc criterion.
func refGRPC(g *routematch.GRPCCriteria, path string) bool {
	switch {
	case g == nil:
		return true
	case g.Method == "":
		return strings.HasPrefix(path, "/"+g.Service+"/")
	default:
		return path == "/"+g.Service+"/"+g.Method
	}
}

// gen draws choices from fuzz input; once it runs out, every choice is
// the first.
type gen struct{ b []byte }

func (g *gen) n(k int) int {
	if len(g.b) == 0 || k <= 1 {
		return 0
	}
	v := int(g.b[0]) % k
	g.b = g.b[1:]
	return v
}

func (g *gen) pick(s []string) string { return s[g.n(len(s))] }

// Pools of the generator: few values, so Routes overlap often.
func routeHosts() []string {
	return []string{"api.shop.example", "*.shop.example", "eu.shop.example", "*.eu.shop.example", "other.example", "API.Shop.Example."}
}

func requestHosts() []string {
	return []string{
		"api.shop.example", "eu.shop.example", "a.eu.shop.example", "shop.example", "other.example",
		"API.SHOP.EXAMPLE:8443", "x.y.shop.example", "",
	}
}

func segments() []string { return []string{"v1", "orders", "42", "export", "a%2Fb", "%C3%A9", ""} }

func regexes() []string {
	return []string{"/v1/.*", "/v1/orders/[0-9]+", ".*export", "/(v1|v2)/orders.*", "/", ".*/"}
}

// whenSources are the match.when sources of generated Routes; the last
// errors unless X-Err is present.
func whenSources() []string {
	return []string{
		`request.method == "GET"`,
		`size(request.pathParams) > 0`,
		`request.path.startsWith("/v1")`,
		`request.headers["x-err"] == "1"`,
	}
}

// genPath returns a path of up to four segments, with a trailing slash
// when the generator says so.
func genPath(g *gen, pool []string, maxDepth int) string {
	var b strings.Builder
	for range g.n(maxDepth + 1) {
		b.WriteByte('/')
		b.WriteString(g.pick(pool))
	}
	if b.Len() == 0 || g.n(3) == 0 {
		b.WriteByte('/')
	}
	return b.String()
}

// genTemplate returns a template of one to three segments.
func genTemplate(g *gen) string {
	var b strings.Builder
	for i := range 1 + g.n(3) {
		b.WriteByte('/')
		if g.n(2) == 0 {
			fmt.Fprintf(&b, "{p%d}", i)
		} else {
			b.WriteString(g.pick([]string{"v1", "orders", "42", "export"}))
		}
	}
	if g.n(3) == 0 {
		b.WriteByte('/')
	}
	return b.String()
}

// genMatch returns generated criteria.
func genMatch(g *gen) v1alpha1.RouteMatch {
	var m v1alpha1.RouteMatch
	for range g.n(3) {
		m.Hosts = append(m.Hosts, g.pick(routeHosts()))
	}
	switch g.n(5) {
	case 1:
		m.Path = exactPath(genPath(g, segments(), 3))
	case 2:
		m.Path = templatePath(genTemplate(g))
	case 3:
		m.Path = prefixPath(genPath(g, segments(), 2))
	case 4:
		m.Path = regexPath(g.pick(regexes()))
	}
	switch g.n(4) {
	case 1:
		m.Methods = []string{http.MethodGet}
	case 2:
		m.Methods = []string{http.MethodGet, http.MethodPost}
	case 3:
		m.Methods = []string{http.MethodDelete}
	}
	switch g.n(4) {
	case 1:
		m.Headers = []v1alpha1.HeaderMatch{{Name: "x-tier", Exact: "gold"}}
	case 2:
		m.Headers = []v1alpha1.HeaderMatch{{Name: "X-Env", Present: present(false)}}
	case 3:
		m.Headers = []v1alpha1.HeaderMatch{{Name: "X-Tier", Regex: "g.*|a, b"}}
	}
	switch g.n(6) {
	case 0:
		m.GRPC = &v1alpha1.GRPCMatch{Service: "pkg.Svc", Method: "Get"}
	case 1:
		m.GRPC = &v1alpha1.GRPCMatch{Service: "pkg.Svc"}
	}
	if i := g.n(len(whenSources()) + 3); i < len(whenSources()) {
		m.When = whenSources()[i]
	}
	return m
}

// genRequest returns a generated request with its path normalized; half
// of the requests aim at one of the generated criteria, so most of them
// reach some candidate.
func genRequest(g *gen, matches []v1alpha1.RouteMatch) (method, host, path string, h http.Header) {
	method = g.pick([]string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodHead})
	host = g.pick(requestHosts())
	raw := genPath(g, append(segments(), "7", "x/../v1"), 4)
	if g.n(5) == 0 {
		raw = g.pick([]string{"/pkg.Svc/Get", "/pkg.Svc/Put"})
	}
	if len(matches) > 0 && g.n(2) == 0 {
		m := matches[g.n(len(matches))]
		if len(m.Hosts) > 0 {
			host = strings.Replace(g.pick(m.Hosts), "*", g.pick([]string{"a", "b.c"}), 1)
		}
		if len(m.Methods) > 0 {
			method = g.pick(m.Methods)
		}
		switch p := m.Path; {
		case m.GRPC != nil:
			raw = "/pkg.Svc/" + g.pick([]string{"Get", "Put"})
		case p == nil:
		case p.Exact != "":
			raw = p.Exact
		case p.Prefix != "":
			raw = p.Prefix + g.pick([]string{"", "/", "/42", "x"})
		case p.Template != "":
			raw = p.Template
			for i := range 3 {
				raw = strings.ReplaceAll(raw, fmt.Sprintf("{p%d}", i), g.pick([]string{"7", "42", "a%2Fb", "%C3%A9", ""}))
			}
		default:
			raw = g.pick([]string{"/v1/orders/42", "/v1/x", "/v2/orders", "/export", "/a/", "/"})
		}
	}
	path, err := routematch.NormalizePath(raw)
	if err != nil {
		panic(err)
	}
	h = http.Header{}
	switch g.n(4) {
	case 1:
		h["X-Tier"] = []string{"gold"}
	case 2:
		h["X-Tier"] = []string{"a", "b"}
	case 3:
		h["x-tier"] = []string{"gold"}
	}
	if g.n(2) == 1 {
		h["X-Env"] = []string{"prod"}
	}
	if g.n(3) == 1 {
		h["X-Err"] = []string{"1"}
	}
	return method, host, path, h
}

// checkReference builds a listener from data and compares Match with
// referenceMatch on generated requests. progs holds the compiled
// whenSources.
func checkReference(t *testing.T, progs map[string]expr.Program, data []byte) {
	t.Helper()
	g := &gen{b: data}
	var hostnames []string
	switch g.n(4) {
	case 2:
		hostnames = []string{"api.shop.example"}
	case 3:
		hostnames = []string{"*.shop.example", "other.example"}
	}
	var (
		routes  []Route
		refs    []refRoute
		matches []v1alpha1.RouteMatch
		idx     int32
	)
	for range 1 + g.n(8) {
		m := genMatch(g)
		name := fmt.Sprintf("%c-%d", 'a'+g.n(3), idx)
		routes = append(routes, Route{Index: idx, Name: name, Match: &m, When: progs[m.When]})
		refs = append(refs, refRoute{index: idx, name: name, c: routematch.CriteriaOf(&m), when: progs[m.When]})
		matches = append(matches, m)
		idx++
	}
	r, err := Compile(hostnames, routes)
	if err != nil {
		if code, _ := errcode.CodeOf(err); code == "RZ-CFG-023" {
			return
		}
		t.Fatalf("Compile: %v", err)
	}
	params := make([]expr.Param, 0, r.MaxParams())
	for range 1 + g.n(6) {
		method, host, path, h := genRequest(g, matches)
		want := referenceMatch(t.Context(), hostnames, refs, method, host, path, h)
		v := &expr.Vars{Request: &expr.Request{Method: method, Host: routematch.NormalizeHost(host), Path: path, Header: h}}
		res := r.Match(t.Context(), &snapshot.MatchRequest{Host: host, Path: path, Method: method, Header: h, Vars: v}, params)
		code, _ := errcode.CodeOf(res.Err)
		if res.Route != want.route || code != want.code || !slices.Equal(res.Params, want.params) {
			t.Fatalf("listener %q, Routes %+v:\n%s %s%s %v: Match = %d %v %q, reference %d %v %q",
				hostnames, refs, method, host, path, h, res.Route, res.Params, code, want.route, want.params, want.code)
		}
	}
}

// compileWhens compiles whenSources once.
func compileWhens(tb testing.TB) map[string]expr.Program {
	tb.Helper()
	b := celtest.Compiler(tb).NewBuilder(nil)
	progs := map[string]expr.Program{}
	for _, src := range whenSources() {
		prg, iss := b.Compile(celtest.At(expr.PlaceRouteMatchWhen), src)
		if prg == nil {
			tb.Fatalf("compile %q: %+v", src, iss)
		}
		progs[src] = prg
	}
	return progs
}

// TestMatchesReference compares Match with the reference matcher on 5,000
// generated listeners; the inputs come from a fixed seed, so every run
// checks the same cases.
func TestMatchesReference(t *testing.T) {
	progs := compileWhens(t)
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: a fixed seed makes every run check the same cases; nothing is secret.
	data := make([]byte, 256)
	for range 5000 {
		for i := 0; i < len(data); i += 8 {
			binary.LittleEndian.PutUint64(data[i:], rng.Uint64())
		}
		checkReference(t, progs, data)
	}
}
