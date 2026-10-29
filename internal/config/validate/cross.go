// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// checkCredentials runs the static Consumer checks of Security and
// identity ("Basic and mTLS schemas", "Consumers and tiers"):
// RZ-CFG-035 for a basic username or certificate identity declared by two
// Consumers, RZ-CFG-036 for basic iterations out of range, RZ-CFG-005 for
// a jwt binding without subject or claims and for oauthClients without a
// jwt issuer. Each Consumer's lists are indexed once for locations.
func (v *Validator) checkCredentials(b *hub.Bundle, l locator, w *work, out *sink) {
	finders := map[*hub.Resource]*finder{}
	loc := func(r *hub.Resource, p diag.Path) diag.Location {
		f := finders[r]
		if f == nil {
			f = l.newFinder(r, w)
			finders[r] = f
		}
		return f.path(p)
	}
	identity.Check(b, loc, out.add)
}

// matchGroup is the Routes, in canonical order, that share one match key.
type matchGroup struct {
	// first is the first Route of the group on any listener, firstAll
	// the first bound to every listener (no spec.listeners), -1 when none.
	first, firstAll int
	// byListener maps a listener name to the first Route named on it.
	byListener map[string]int
}

// routeBinding is one Route as the match check reads it.
type routeBinding struct {
	r *hub.Resource
	// all reports a Route without spec.listeners, bound to every listener.
	all bool
	// listeners are the Route's listeners that exist (all of them without
	// a Gateway), in list order.
	listeners []string
}

// via returns the listener a Route bound to it shares with b, for the
// message: b's first named listener, else the Gateway's first one.
func (b routeBinding) via(ix *bundleIndex) string {
	if len(b.listeners) > 0 {
		return b.listeners[0]
	}
	if len(ix.listeners) > 0 {
		return ix.listeners[0]
	}
	return ""
}

// checkMatches reports a Route whose match criteria equal those of an
// earlier Route, in canonical order, on a common listener (RZ-CFG-023):
// equality is routematch.MatchKey, and a Route without spec.listeners is
// on every listener. Listener names the Gateway lacks bind nothing
// (RZ-CFG-009); without a Gateway every named listener counts. Routes
// whose criteria failed an RZ-CFG-005 rule are left out. Each Route is
// reported once, against the earliest Route it collides with.
func (v *Validator) checkMatches(ix *bundleIndex, l locator, w *work, out *sink) {
	var routes []routeBinding
	for i, r := range ix.resources {
		rt, ok := hub.Object[v1alpha1.Route](r)
		if !ok || rt == nil || r.Kind != v1alpha1.KindRoute || ix.badMatch[i] {
			continue
		}
		b := routeBinding{r: r, all: len(rt.Spec.Listeners) == 0}
		for _, name := range rt.Spec.Listeners {
			w.charge(1)
			if _, ok := ix.listenerSet[name]; ok || ix.gw == nil {
				b.listeners = append(b.listeners, name)
			}
		}
		if !b.all && len(b.listeners) == 0 {
			continue
		}
		routes = append(routes, b)
	}
	groups := map[routematch.Key]*matchGroup{}
	matchPath := specPath(diag.Field("match"))
	for i, b := range routes {
		rt, _ := hub.Object[v1alpha1.Route](b.r)
		c := routematch.CriteriaOf(&rt.Spec.Match)
		key := routematch.MatchKey(&c)
		w.charge(1)
		g := groups[key]
		if g == nil {
			g = &matchGroup{first: -1, firstAll: -1, byListener: map[string]int{}}
			groups[key] = g
		}
		other, via := -1, ""
		if b.all {
			if g.first >= 0 {
				other, via = g.first, routes[g.first].via(ix)
			}
		} else {
			if g.firstAll >= 0 {
				other, via = g.firstAll, b.listeners[0]
			}
			for _, name := range b.listeners {
				if j, ok := g.byListener[name]; ok && (other < 0 || j < other) {
					other, via = j, name
				}
			}
		}
		if other >= 0 {
			msg := "match criteria are identical to those of Route " + quote(routes[other].r.Name)
			if via != "" {
				msg += " on listener " + quote(via)
			}
			first := l.newFinder(routes[other].r, w).path(matchPath)
			out.add(diag.Diagnostic{
				Code: CodeDuplicateMatch, Severity: diag.SeverityError,
				Location: l.newFinder(b.r, w).path(matchPath), Resource: b.r.ResourceID(), Path: matchPath,
				Message: msg, Hint: "give one of the Routes a distinguishing criterion or other listeners",
				Related: []diag.Related{{Location: first, Message: "first declared in"}},
			})
		}
		if g.first < 0 {
			g.first = i
		}
		if b.all {
			if g.firstAll < 0 {
				g.firstAll = i
			}
			continue
		}
		for _, name := range b.listeners {
			if _, ok := g.byListener[name]; !ok {
				g.byListener[name] = i
			}
		}
	}
}
