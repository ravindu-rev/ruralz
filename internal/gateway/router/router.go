// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Codes the Router returns besides routematch.CodeInvalid (RZ-CFG-005).
const (
	// codeDuplicate is two Routes with identical match criteria.
	codeDuplicate = "RZ-CFG-023"
	// codeUnserved is a criterion the Router does not serve: GraphQL and
	// topic matching need more than pre-body data.
	codeUnserved = "RZ-CFG-040"
	// codeWhen is a match.when runtime error.
	codeWhen = "RZ-RT-006"
)

// Route is one Route bound to the listener, as Compile takes it.
type Route struct {
	// Index is the Route's position in snapshot.Snapshot.Routes, which
	// Match returns.
	Index int32
	// Name is metadata.name, precedence rank 6.
	Name string
	// Match is Route.spec.match; nil matches every request.
	Match *v1alpha1.RouteMatch
	// When is the compiled Match.When; nil exactly when Match.When is
	// empty.
	When expr.Program
	// Timeout is the effective Route timeout: When runs under the Route
	// deadline it gives, snapshot.MatchRequest.Start plus Timeout; 0 sets
	// no deadline.
	Timeout time.Duration
}

// Router is the compiled Router of one listener.
type Router struct {
	// gate holds the listener hostnames; nil accepts any host.
	gate      *routematch.HostTable[struct{}]
	hosts     routematch.HostTable[entry]
	maxParams int
}

// Compile-time check of the contract.
var _ snapshot.Router = (*Router)(nil)

// Compile builds the Router of a listener limited to hostnames (empty
// accepts any host) from the Routes bound to it.
func Compile(hostnames []string, routes []Route) (*Router, error) {
	r := &Router{}
	if len(hostnames) > 0 {
		r.gate = new(routematch.HostTable[struct{}])
		for _, h := range hostnames {
			p, err := routematch.ParseHostPattern(h)
			if err != nil {
				return nil, fmt.Errorf("router: listener hostname: %w", err)
			}
			r.gate.Entry(p)
		}
	}
	compiled := make([]*route, len(routes))
	names := make(map[routematch.Key]string, len(routes))
	for i := range routes {
		rt, err := compileRoute(&routes[i])
		if err != nil {
			return nil, fmt.Errorf("router: route %q: %w", routes[i].Name, err)
		}
		if other, ok := names[rt.key]; ok {
			return nil, errcode.Errorf(codeDuplicate, "router: routes %q and %q have identical match criteria",
				min(other, rt.name), max(other, rt.name))
		}
		names[rt.key] = rt.name
		compiled[i] = rt
		if rt.tmpl != nil {
			r.maxParams = max(r.maxParams, len(rt.tmpl.names))
		}
	}
	// Two Routes in one list share their host entry and path, so the
	// precedence order restricted to a list is ranks 5 and 6; adding the
	// Routes sorted keeps every list in that order.
	slices.SortStableFunc(compiled, func(a, b *route) int { return routematch.Compare(a.rank, b.rank) })
	for _, rt := range compiled {
		for _, h := range rt.hosts {
			r.hosts.Entry(h).add(rt)
		}
	}
	return r, nil
}

// MaxParams returns the most captures of any template on the listener:
// with params of at least this capacity, Match stores captures without
// allocating.
func (r *Router) MaxParams() int { return r.maxParams }

// Match implements snapshot.Router: q.Path is the normalized path and
// params is the caller's storage for the winner's captures. The result is
// the winner, Route -1 when none is left (RZ-RT-001), or Route -1 with an
// RZ-RT-006 error when a match.when failed. On return q.Vars holds the
// winner's captures and Route deadline, or none.
func (r *Router) Match(ctx context.Context, q *snapshot.MatchRequest, params []expr.Param) snapshot.MatchResult {
	m := matcher{q: q, preflight: preflightMethod(q), params: params[:0], route: -1}
	host := routematch.NormalizeHost(q.Host)
	if r.admits(host) {
		r.hosts.Lookup(host, func(_ routematch.HostPattern, e *entry) bool {
			return !e.match(ctx, &m)
		})
	}
	if m.route < 0 {
		m.setPathParams(m.params)
		m.setDeadline(0)
		return snapshot.MatchResult{Route: -1, Err: m.err}
	}
	m.setPathParams(m.won)
	m.setDeadline(m.timeout)
	return snapshot.MatchResult{Route: m.route, Params: m.won}
}

// Fields of a CORS preflight, in canonical form, as net/http stores them.
const (
	headerOrigin        = "Origin"
	headerRequestMethod = "Access-Control-Request-Method"
)

// preflightMethod returns, for a CORS preflight (an OPTIONS request with
// Origin and Access-Control-Request-Method fields), the method it asks for
// (the first field line), else "". A preflight meets match.methods when
// they list that method or OPTIONS, so it reaches the Route that serves
// the actual request, whose cors Policy answers it, and a Route that
// serves OPTIONS itself still receives it; match.when still sees
// request.method. It allocates nothing.
func preflightMethod(q *snapshot.MatchRequest) string {
	if q.Method != http.MethodOptions || len(q.Header[headerOrigin]) == 0 {
		return ""
	}
	if requested := q.Header[headerRequestMethod]; len(requested) > 0 {
		return requested[0]
	}
	return ""
}

// admits reports whether host is within the listener hostnames.
func (r *Router) admits(host string) bool {
	if r.gate == nil {
		return true
	}
	in := false
	r.gate.Lookup(host, func(routematch.HostPattern, *struct{}) bool {
		in = true
		return false
	})
	return in
}
