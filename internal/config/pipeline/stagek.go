// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"slices"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// CodeOutputCap is RZ-CFG-034: an AIModel reachable from a Route with an
// ai.token-budget Policy sets no limits.maxOutputTokens.
const CodeOutputCap = "RZ-CFG-034"

// chainRules is stage K: the effective-chain rules of b, whose effective
// Filter Chains are chains (docs/architecture/02-configuration-model.md,
// Figure 3). In M1 that is RZ-CFG-034, from the configuration model's
// Policy section: a Route reaches the AIModels that the ai.models of each
// ai Upstream in its upstreams or composition steps name, and each
// AIModel without limits.maxOutputTokens that a Route whose effective
// chain holds an ai.token-budget Policy reaches is reported once, at its
// limits or, without them, at the resource, with every such Route as a
// related location in canonical order. A Token Budget reserves the input
// estimate plus the output cap, which is limits.maxOutputTokens when a
// client sends none (foundation pack section 8.9).
func chainRules(b *hub.Bundle, chains map[string]*hub.Chain, files *tree.FileTable) diag.List {
	type reached struct {
		model  *hub.Resource
		routes []*hub.Resource
	}
	var order []string
	byModel := map[string]*reached{}
	for _, r := range b.Resources() {
		rt, ok := hub.Object[v1alpha1.Route](r)
		if !ok || rt == nil || !holdsTokenBudget(chains[r.Name]) {
			continue
		}
		for _, up := range reachedUpstreams(rt) {
			u, ok := b.Upstream(up)
			if !ok || u == nil || u.Spec.Protocol != v1alpha1.UpstreamProtocolAI || u.Spec.AI == nil {
				continue
			}
			for _, ref := range u.Spec.AI.Models {
				mr, ok := b.Get(hub.ID{Kind: v1alpha1.KindAIModel, Name: ref.Name})
				if !ok {
					continue
				}
				m, ok := hub.Object[v1alpha1.AIModel](mr)
				if !ok || m == nil || (m.Spec.Limits != nil && m.Spec.Limits.MaxOutputTokens != nil) {
					continue
				}
				x := byModel[ref.Name]
				if x == nil {
					x = &reached{model: mr}
					byModel[ref.Name] = x
					order = append(order, ref.Name)
				}
				if len(x.routes) == 0 || x.routes[len(x.routes)-1] != r {
					x.routes = append(x.routes, r)
				}
			}
		}
	}
	// AIModels of one kind sort canonically by name; b lists the Routes in
	// that order, so each model's Routes are too.
	slices.Sort(order)
	var ds diag.List
	for _, name := range order {
		x := byModel[name]
		limits := diag.Path{diag.Field("spec"), diag.Field("limits")}
		loc := x.model.Source.Start
		if n, ok := x.model.Tree.At(limits); ok && n.Style != tree.StyleDefaulted && n.Pos.Known() {
			loc = locate(files, x.model, n.Pos)
		}
		related := make([]diag.Related, len(x.routes))
		for i, r := range x.routes {
			related[i] = diag.Related{Location: r.Source.Start, Message: "reached from Route " + strconv.Quote(r.Name) + " in"}
		}
		ds = append(ds, diag.Diagnostic{
			Code: CodeOutputCap, Severity: diag.SeverityError, Location: loc, Resource: x.model.ResourceID(),
			Path:    limits.Append(diag.Field("maxOutputTokens")),
			Message: "AIModel " + strconv.Quote(name) + " sets no limits.maxOutputTokens, but a Route with an ai.token-budget Policy reaches it",
			Hint:    "set limits.maxOutputTokens: a Token Budget reserves it as the output cap when a client sends none",
			Related: related,
		})
	}
	return ds
}

// holdsTokenBudget reports whether the effective chain c runs an
// ai.token-budget Policy, which attaches at Gateway and Route scope and so
// runs on the client leg.
func holdsTokenBudget(c *hub.Chain) bool {
	if c == nil {
		return false
	}
	for _, es := range c.Client {
		for _, e := range es {
			if e.Type == v1alpha1.PolicyTypeAITokenBudget {
				return true
			}
		}
	}
	return false
}

// reachedUpstreams returns the Upstreams a Route names in spec.upstreams
// and in its composition steps, each once, in that order.
func reachedUpstreams(rt *v1alpha1.Route) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
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
