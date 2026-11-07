// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package export

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// methods returns the HTTP methods an OpenAPI path item describes, in the
// order the document lists them.
func methods() []string {
	return []string{"GET", "PUT", "POST", "DELETE", "OPTIONS", "HEAD", "PATCH", "TRACE"}
}

// methodRank is the position of method in methods, or len(methods()) for
// a method OpenAPI does not describe.
func methodRank(method string) int {
	all := methods()
	if i := slices.Index(all, method); i >= 0 {
		return i
	}
	return len(all)
}

// Operation is one HTTP operation of a Route: a path and a method.
type Operation struct {
	// Route is the metadata.name of the Route.
	Route string
	// Method is the HTTP method, one of the eight OpenAPI describes.
	Method string
	// Path is the OpenAPI path: the Route's exact path, or its template,
	// or the path an earlier Route of the same hierarchy exported.
	Path string
	// Params are the names of Path's template captures, in order.
	Params []string
	// Body is the config.schema of the first validation.json-schema
	// Policy of the Route's effective client chain, in the validated
	// Bundle's normalized form (numbers exact, members sorted); nil
	// without one. It is the validated Bundle's own node: read it, never
	// change it.
	Body *tree.Node
	// OperationID is the Route name when the Route exports one method,
	// else "<route>-<method in lower case>", made unique by "-2", "-3".
	OperationID string
}

// Skip is a Route, or one method of it, that has no operation.
type Skip struct {
	// Route is the metadata.name of the Route.
	Route string
	// Reason says why, such as "a prefix path has no OpenAPI path".
	Reason string
}

// Operations returns the HTTP operations of v's Routes and the Routes or
// methods that have none, the Routes in canonical order by name and each
// Route's methods in OpenAPI order (package documentation, "Operations").
func Operations(v *hub.Validated) ([]Operation, []Skip) {
	if v == nil || v.Bundle == nil {
		return nil, nil
	}
	var (
		ops   []Operation
		skips []Skip
		// taken maps "<METHOD> <shape>" to the Route that exported it.
		taken = map[string]string{}
		// exported maps a path's shape to the path text that holds it.
		exported = map[string]string{}
		ids      = map[string]bool{}
	)
	for _, r := range v.Bundle.Resources() {
		rt, ok := hub.Object[v1alpha1.Route](r)
		if r.Kind != v1alpha1.KindRoute || !ok {
			continue
		}
		own, reason := qualify(v.Bundle, rt)
		if reason != "" {
			skips = append(skips, Skip{Route: r.Name, Reason: reason})
			continue
		}
		known, unknown := routeMethods(rt.Spec.Match.Methods)
		for _, m := range unknown {
			skips = append(skips, Skip{Route: r.Name, Reason: "method " + quoteMethod(m) + " has no OpenAPI operation"})
		}
		shape := shapeOf(own)
		path, shared := exported[shape]
		if !shared {
			path = own
		}
		var methodsOut []string
		for _, m := range known {
			key := m + " " + shape
			if other, ok := taken[key]; ok {
				skips = append(skips, Skip{Route: r.Name, Reason: fmt.Sprintf("%s %s is already exported by Route %s", m, own, strconv.Quote(other))})
				continue
			}
			taken[key] = r.Name
			methodsOut = append(methodsOut, m)
		}
		if len(methodsOut) == 0 {
			continue
		}
		if !shared {
			exported[shape] = path
		}
		params := captures(path)
		body := bodySchema(v, r.Name)
		for _, m := range methodsOut {
			id := r.Name
			if len(methodsOut) > 1 {
				id += "-" + strings.ToLower(m)
			}
			ops = append(ops, Operation{
				Route: r.Name, Method: m, Path: path, Params: params, Body: body, OperationID: unique(ids, id),
			})
		}
	}
	return ops, skips
}

// qualify returns the OpenAPI path of rt, the exact path or the template
// as written, or the reason rt has no operation: a grpc, graphql or topic
// match, a path that is not exact or template, an exact path holding a
// brace, or an Upstream it reaches that does not speak http.
func qualify(b *hub.Bundle, rt *v1alpha1.Route) (path, reason string) {
	m := rt.Spec.Match
	switch {
	case m.GRPC != nil:
		return "", "match.grpc has no OpenAPI operation"
	case m.GraphQL != nil:
		return "", "match.graphql has no OpenAPI operation"
	case m.Topic != "":
		return "", "match.topic has no OpenAPI operation"
	}
	switch p := m.Path; {
	case p == nil:
		return "", "a Route without a path has no OpenAPI path"
	case p.Prefix != "":
		return "", "a prefix path has no OpenAPI path"
	case p.Regex != "":
		return "", "a regex path has no OpenAPI path"
	case p.Exact != "":
		if strings.ContainsAny(p.Exact, "{}") {
			return "", "an exact path holding { or } has no OpenAPI path"
		}
		path = p.Exact
	case p.Template != "":
		path = p.Template
	default:
		return "", "a Route without a path has no OpenAPI path"
	}
	for _, name := range reached(rt) {
		up, ok := b.Upstream(name)
		if ok && up.Spec.Protocol != v1alpha1.UpstreamProtocolHTTP {
			return "", fmt.Sprintf("Upstream %s speaks %s, not http", strconv.Quote(name), up.Spec.Protocol)
		}
	}
	return path, ""
}

// reached returns the Upstreams rt sends to, in spec order, each once: its
// upstreams entries, or its composition steps' upstream.
func reached(rt *v1alpha1.Route) []string {
	var out []string
	add := func(name string) {
		if name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	for _, u := range rt.Spec.Upstreams {
		add(u.Name)
	}
	if c := rt.Spec.Composition; c != nil {
		for _, s := range c.Steps {
			add(s.Upstream)
		}
	}
	return out
}

// routeMethods splits a Route's match.methods into the methods OpenAPI
// describes, in OpenAPI order, and the others, sorted; a Route without
// methods serves every method, so it gets all eight.
func routeMethods(set []string) (known, unknown []string) {
	if len(set) == 0 {
		return methods(), nil
	}
	for _, m := range methods() {
		if slices.Contains(set, m) {
			known = append(known, m)
		}
	}
	for _, m := range set {
		if methodRank(m) == len(methods()) && !slices.Contains(unknown, m) {
			unknown = append(unknown, m)
		}
	}
	slices.Sort(unknown)
	return known, unknown
}

// quoteMethod returns a method as a message shows it: bare when it is an
// RFC 9110 token, quoted otherwise.
func quoteMethod(m string) string {
	ok := m != ""
	for i := 0; ok && i < len(m); i++ {
		ok = isTChar(m[i])
	}
	if !ok {
		return strconv.Quote(m)
	}
	return m
}

// isTChar reports whether c is an RFC 9110 tchar.
func isTChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	default:
		return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
	}
}

// shapeOf returns the hierarchy of an OpenAPI path: every {name} segment
// written {}, so templates that differ only in their capture names, which
// OpenAPI holds to be one path, share it.
func shapeOf(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if isCapture(s) {
			segs[i] = "{}"
		}
	}
	return strings.Join(segs, "/")
}

// captures returns the names of path's {name} segments, in order.
func captures(path string) []string {
	var out []string
	for _, s := range strings.Split(path, "/") {
		if isCapture(s) {
			out = append(out, s[1:len(s)-1])
		}
	}
	return out
}

// isCapture reports whether a path segment is a whole-segment {name}
// capture, the only form a Route template holds.
func isCapture(seg string) bool {
	return len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}'
}

// unique returns id, or id with the first of "-2", "-3", ... that ids
// lacks, and records it in ids.
func unique(ids map[string]bool, id string) string {
	out := id
	for n := 2; ids[out]; n++ {
		out = id + "-" + strconv.Itoa(n)
	}
	ids[out] = true
	return out
}

// schemaPath is the path of a validation.json-schema Policy's schema.
func schemaPath() diag.Path {
	return diag.Path{diag.Field("spec"), diag.Field("config"), diag.Field("schema")}
}

// bodySchema returns the config.schema node of the first
// validation.json-schema Policy of route's effective client chain, in
// execution order: Phase by Phase, each Phase's Policies in order; nil
// without one.
func bodySchema(v *hub.Validated, route string) *tree.Node {
	c := v.Chains[route]
	if c == nil {
		return nil
	}
	for p := phase.OnRequestHeaders; p < phase.Count; p++ {
		for _, e := range c.Client[p] {
			if e.Type != v1alpha1.PolicyTypeValidationJSONSchema {
				continue
			}
			r, ok := v.Bundle.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: e.Policy})
			if !ok {
				continue
			}
			if n, ok := r.Tree.At(schemaPath()); ok {
				return n
			}
		}
	}
	return nil
}

// clientPolicies returns the Policies of c's client leg in execution
// order, Phase by Phase, each once.
func clientPolicies(c *hub.Chain) []string {
	var out []string
	if c == nil {
		return out
	}
	for p := phase.OnRequestHeaders; p < phase.Count; p++ {
		for _, e := range c.Client[p] {
			if !slices.Contains(out, e.Policy) {
				out = append(out, e.Policy)
			}
		}
	}
	return out
}

// legPolicies returns the Policies of leg in execution order, Phase by
// Phase, each once.
func legPolicies(leg *hub.Leg) []string {
	var out []string
	for p := phase.OnRequestHeaders; p < phase.Count; p++ {
		for _, e := range leg.Phases[p] {
			if !slices.Contains(out, e.Policy) {
				out = append(out, e.Policy)
			}
		}
	}
	return out
}

// documentOrder returns ops in the order of the OpenAPI document: by path
// bytewise, then in method order.
func documentOrder(ops []Operation) []Operation {
	out := slices.Clone(ops)
	slices.SortStableFunc(out, func(a, b Operation) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), cmp.Compare(methodRank(a.Method), methodRank(b.Method)))
	})
	return out
}

// gatewayName returns the Gateway's metadata.name, or "" without one.
func gatewayName(b *hub.Bundle) string {
	if g := b.Gateway(); g != nil {
		return g.Metadata.Name
	}
	return ""
}
