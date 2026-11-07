// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package export

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ErrUnknownRoute marks a DOT Route that names no Route of the Bundle.
var ErrUnknownRoute = errors.New("unknown Route")

// ErrInput marks a call without a validated Bundle.
var ErrInput = errors.New("export: no validated Bundle")

// maxQuoted is the most code points of an authored value a message quotes
// (docs/architecture/02-configuration-model.md "Semantic validation").
const maxQuoted = 64

// graph is the nodes and edges of a DOT graph, nodes by kind in name
// order and edges in the order they are written.
type graph struct {
	routes, policies, upstreams []string
	edges                       [][2]string
}

// DOT returns the Graphviz DOT graph of v's Routes, Policies and
// Upstreams, or with route set the subgraph of that Route (package
// documentation, "DOT"). A route that names no Route of v is
// ErrUnknownRoute with the nearest Route name as a hint.
func DOT(v *hub.Validated, route string) ([]byte, error) {
	if v == nil || v.Bundle == nil {
		return nil, ErrInput
	}
	if route == "" {
		g := wholeGraph(v)
		return g.write(v.Bundle), nil
	}
	g, err := routeGraph(v, route)
	if err != nil {
		return nil, err
	}
	return g.write(v.Bundle), nil
}

// wholeGraph returns every Route, Policy and Upstream of v, each Route's
// edges to the Policies of its client chain and the Upstreams it reaches,
// and each Upstream's edges to its spec.policies.
func wholeGraph(v *hub.Validated) graph {
	var g graph
	for _, r := range v.Bundle.Resources() {
		switch r.Kind {
		case v1alpha1.KindRoute:
			g.routes = append(g.routes, r.Name)
		case v1alpha1.KindPolicy:
			g.policies = append(g.policies, r.Name)
		case v1alpha1.KindUpstream:
			g.upstreams = append(g.upstreams, r.Name)
		default:
		}
	}
	for _, name := range g.routes {
		g.routeEdges(v, name)
	}
	for _, name := range g.upstreams {
		if up, ok := v.Bundle.Upstream(name); ok {
			for _, p := range up.Spec.Policies {
				g.edge("Upstream/"+name, "Policy/"+p.Name)
			}
		}
	}
	return g
}

// routeGraph returns the subgraph of one Route: the Route, the Policies of
// its client chain and of every leg, the Upstreams it reaches, the
// Route's edges, and each reached Upstream's edges to the Policies of its
// leg in the Route's chain.
func routeGraph(v *hub.Validated, route string) (graph, error) {
	rt, ok := v.Bundle.Route(route)
	if !ok {
		return graph{}, unknownRoute(v.Bundle, route)
	}
	c := v.Chains[route]
	policies := clientPolicies(c)
	legs := map[string][]string{}
	if c != nil {
		for i := range c.Legs {
			ps := legPolicies(&c.Legs[i])
			legs[c.Legs[i].Upstream] = ps
			for _, p := range ps {
				if !slices.Contains(policies, p) {
					policies = append(policies, p)
				}
			}
		}
	}
	g := graph{
		routes:    []string{route},
		policies:  slices.Sorted(slices.Values(policies)),
		upstreams: slices.Sorted(slices.Values(reached(rt))),
	}
	g.routeEdges(v, route)
	for _, u := range g.upstreams {
		for _, p := range legs[u] {
			g.edge("Upstream/"+u, "Policy/"+p)
		}
	}
	return g, nil
}

// routeEdges adds the edges of the Route name: to each Policy of its
// client chain in execution order, then to each Upstream it reaches in
// spec order.
func (g *graph) routeEdges(v *hub.Validated, name string) {
	for _, p := range clientPolicies(v.Chains[name]) {
		g.edge("Route/"+name, "Policy/"+p)
	}
	if rt, ok := v.Bundle.Route(name); ok {
		for _, u := range reached(rt) {
			g.edge("Route/"+name, "Upstream/"+u)
		}
	}
}

// edge adds the edge from one node to another, once.
func (g *graph) edge(from, to string) {
	if e := [2]string{from, to}; !slices.Contains(g.edges, e) {
		g.edges = append(g.edges, e)
	}
}

// write returns the DOT text of g as the digraph named after b's Gateway,
// each Policy node labeled with its type from b.
func (g *graph) write(b *hub.Bundle) []byte {
	var s strings.Builder
	s.WriteString("digraph " + quote(gatewayName(b)) + " {\n")
	s.WriteString("  rankdir=LR;\n")
	s.WriteString("  node [shape=box];\n")
	for _, r := range g.routes {
		s.WriteString("  " + quote("Route/"+r) + " [label=" + label("Route", r) + "];\n")
	}
	for _, p := range g.policies {
		typ := ""
		if pol, ok := b.Policy(p); ok {
			typ = string(pol.Spec.Type)
		}
		s.WriteString("  " + quote("Policy/"+p) + " [shape=ellipse, label=" + label("Policy", p, typ) + "];\n")
	}
	for _, u := range g.upstreams {
		s.WriteString("  " + quote("Upstream/"+u) + " [shape=cylinder, label=" + label("Upstream", u) + "];\n")
	}
	for _, e := range g.edges {
		s.WriteString("  " + quote(e[0]) + " -> " + quote(e[1]) + ";\n")
	}
	s.WriteString("}\n")
	return []byte(s.String())
}

// quote returns s as a DOT quoted string: '"' and '\' escaped, so the
// string holds no escape sequence of its own.
func quote(s string) string {
	return `"` + escape(s) + `"`
}

// escape returns s with '"' and '\' escaped for a DOT quoted string.
func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// label returns the quoted DOT label of lines, each escaped and joined by
// the centered line break \n.
func label(lines ...string) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = escape(l)
	}
	return `"` + strings.Join(parts, `\n`) + `"`
}

// unknownRoute returns the ErrUnknownRoute error for route, hinting the
// nearest Route name of b; the names are sorted first, since diag.Nearest
// compares a bounded number of candidates, so the hint never depends on
// the order of b.
func unknownRoute(b *hub.Bundle, route string) error {
	var names []string
	for _, r := range b.Resources() {
		if r.Kind == v1alpha1.KindRoute {
			names = append(names, r.Name)
		}
	}
	slices.Sort(names)
	err := fmt.Errorf("%w %s", ErrUnknownRoute, strconv.Quote(clip(route)))
	if near, ok := diag.Nearest(route, names); ok {
		err = fmt.Errorf("%w (did you mean %s?)", err, strconv.Quote(near))
	}
	return err
}

// clip returns at most maxQuoted code points of s, "..." marking a longer
// value.
func clip(s string) string {
	if r := []rune(s); len(r) > maxQuoted {
		return string(r[:maxQuoted]) + "..."
	}
	return s
}
