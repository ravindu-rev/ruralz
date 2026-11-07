// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"slices"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/netaddr"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// route checks a Route: its listeners name Gateway listeners
// (RZ-CFG-009), its match criteria compile as the Router compiles them
// (RZ-CFG-005), its composition fits the Gateway limits (RZ-CFG-031,
// RZ-CFG-032), its redirect builds a Location (RZ-CFG-005) and its mirrors
// copy an upstreams leg (RZ-CFG-005).
func (u *unit) route() {
	rt, ok := hub.Object[v1alpha1.Route](u.r)
	if !ok || rt == nil {
		return
	}
	if u.ix.gw != nil {
		for _, name := range rt.Spec.Listeners {
			u.w.charge(1)
			if _, ok := u.ix.listenerSet[name]; !ok {
				u.report(CodeReference, specPath(diag.Field("listeners"), diag.Item(name)),
					"Gateway listener "+quote(name)+" not found", u.ix.listenerNames.hint(name), nil)
			}
		}
	}
	before := u.out.n
	u.match(&rt.Spec.Match)
	if u.out.n > before {
		// Criteria the Router cannot compile take no part in the
		// identical-criteria check: their keys mean nothing.
		u.ix.badMatch[u.idx] = true
	}
	if c := rt.Spec.Composition; c != nil {
		u.composition(c)
	}
	if r := rt.Spec.Redirect; r != nil {
		u.redirect(r, &rt.Spec.Match)
	}
	if len(rt.Spec.Mirrors) > 0 && len(rt.Spec.Upstreams) == 0 {
		// The schema holds exactly one of upstreams, composition and
		// redirect: without upstreams there is no leg whose first attempt a
		// mirror copies.
		other := "composition"
		if rt.Spec.Redirect != nil {
			other = "redirect"
		}
		u.report(CodeSchema, specPath(diag.Field("mirrors")),
			"mirrors copy the first attempt of the upstreams leg, and this Route sets "+other+" instead",
			"remove mirrors, or forward the Route with upstreams", nil)
	}
}

// redirectStatuses are the statuses a redirect answers with
// (docs/architecture/09-traffic-management-and-resilience.md "Redirects");
// the schema bounds status to 301 to 308.
func redirectStatuses() []int32 { return []int32{301, 302, 307, 308} }

// redirect checks a Route's redirect, each failure RZ-CFG-005 at its field,
// so the Node builds every Location validation passed: a status of 301,
// 302, 307 or 308; a host that is a URL authority without user information
// (netaddr.CheckAuthority); a path and a prefix that may replace the
// client's in a Location (routematch.CheckRedirectPath); and a prefix only
// on a Route whose match.path is a prefix, the prefix it replaces. The
// schema states the rest: at most one of path and prefix, and
// locationExpression without scheme, host, path, prefix or query.
func (u *unit) redirect(r *v1alpha1.RouteRedirect, m *v1alpha1.RouteMatch) {
	u.w.charge(1)
	base := specPath(diag.Field("redirect"))
	if s := r.Status; s != nil && !slices.Contains(redirectStatuses(), *s) {
		u.report(CodeSchema, base.Append(diag.Field("status")),
			"redirect status "+strconv.Itoa(int(*s))+" is not 301, 302, 307 or 308", "", nil)
	}
	if p := base.Append(diag.Field("host")); r.Host != "" || u.f.node(p) != nil {
		if err := netaddr.CheckAuthority(r.Host); err != nil {
			u.report(CodeSchema, p, "redirect host "+quote(r.Host)+" is not a host with an optional port ("+err.Error()+")",
				"write a DNS name or an IP address, IPv6 in brackets, and optionally :port", nil)
		}
	}
	for _, f := range []struct{ name, value string }{{"path", r.Path}, {"prefix", r.Prefix}} {
		p := base.Append(diag.Field(f.name))
		if f.value == "" && u.f.node(p) == nil {
			continue
		}
		if err := routematch.CheckRedirectPath(f.value); err != nil {
			u.report(CodeSchema, p, "redirect "+f.name+" "+quote(f.value)+" is not a valid path ("+pathReason(err)+")",
				"start it with /, and keep ? and # out of it: query names what the redirect does with the query", nil)
		}
	}
	if p := base.Append(diag.Field("prefix")); (r.Prefix != "" || u.f.node(p) != nil) && (m.Path == nil || m.Path.Prefix == "") {
		u.report(CodeSchema, p, "redirect prefix replaces the matched match.path.prefix, and this Route's match.path is not a prefix",
			"use path to replace the whole path, or match the Route with a path prefix", nil)
	}
}

// match checks the criteria the Router compiles (Data plane "Router"),
// each failure RZ-CFG-005 at its field: hosts in the exact or one leading
// "*." form, a path form whose value is valid for it, header criteria
// with a token name and one valid form, and a gRPC service. GraphQL and
// topic matching are Planned (M3) and (M4).
func (u *unit) match(m *v1alpha1.RouteMatch) {
	base := specPath(diag.Field("match"))
	for _, h := range m.Hosts {
		u.w.charge(1)
		if err := routematch.CheckHost(h); err != nil {
			u.report(CodeSchema, base.Append(diag.Field("hosts"), diag.Item(h)), causeText(err, h), "", nil)
		}
	}
	if p := m.Path; p != nil {
		u.path(base.Append(diag.Field("path")), p)
	}
	for _, h := range m.Headers {
		u.w.charge(1)
		hp := base.Append(diag.Field("headers"), diag.Keyed("name", h.Name))
		_, err := routematch.CompileHeader(routematch.HeaderCriteria(h))
		switch {
		case err == nil:
		case h.Regex != "" && isRegexError(err):
			u.report(CodeSchema, hp.Append(diag.Field("regex")), regexText(err), "", nil)
		default:
			u.report(CodeSchema, hp, causeText(err, h.Name), "", nil)
		}
	}
	if g := m.GRPC; g != nil && g.Service == "" {
		u.report(CodeSchema, base.Append(diag.Field("grpc"), diag.Field("service")), "match.grpc.service is empty", "", nil)
	}
}

// path checks a path criterion. Stage F admits exactly one member, so the
// form is the member the tree holds; an empty value is no criterion the
// Router can compile.
func (u *unit) path(base diag.Path, p *v1alpha1.PathMatch) {
	form, value := "", ""
	switch {
	case p.Exact != "":
		form, value = "exact", p.Exact
	case p.Prefix != "":
		form, value = "prefix", p.Prefix
	case p.Template != "":
		form, value = "template", p.Template
	case p.Regex != "":
		form, value = "regex", p.Regex
	default:
		for _, f := range []string{"exact", "prefix", "template", "regex"} {
			if u.f.node(base.Append(diag.Field(f))) != nil {
				form = f
				break
			}
		}
		if form == "" {
			return
		}
	}
	at := base.Append(diag.Field(form))
	var err error
	switch form {
	case "exact", "prefix":
		err = routematch.CheckPath(value)
	case "template":
		err = routematch.CheckTemplate(value)
	default:
		if value == "" {
			u.report(CodeSchema, at, "match.path.regex is empty", "", nil)
			return
		}
		if _, err := routematch.CompileRegex(value); err != nil {
			u.report(CodeSchema, at, regexText(err), "", nil)
		}
		return
	}
	if err != nil {
		u.report(CodeSchema, at, causeText(err, value), "", nil)
	}
}

// composition checks the steps of a composition: no when on an aggregate
// step, which runs whatever when says (RZ-CFG-005; when applies to the
// sequential and conditional modes); each step's path, target and group
// (step, RZ-CFG-005); the step count against
// limits.maxCompositionSteps (RZ-CFG-031, at the first step past it); and
// the steps' maxBodyBytes against limits.maxResponseBodyBytes
// (RZ-CFG-032): explicit values, in list order, must not total more than
// the limit, and must leave each step without one a share of at least one
// byte of what remains.
func (u *unit) composition(c *v1alpha1.Composition) {
	steps := specPath(diag.Field("composition"), diag.Field("steps"))
	if c.Mode == v1alpha1.CompositionModeAggregate {
		for _, s := range c.Steps {
			if s.When != "" {
				u.report(CodeSchema, steps.Append(diag.Keyed("name", s.Name), diag.Field("when")),
					"step "+quote(s.Name)+" sets when, which an aggregate composition never evaluates",
					"remove when, or use mode sequential or conditional", nil)
			}
		}
	}
	for i := range c.Steps {
		u.step(steps.Append(diag.Keyed("name", c.Steps[i].Name)), &c.Steps[i])
	}
	maxSteps := u.ix.maxSteps
	if maxSteps.value >= 0 && int64(len(c.Steps)) > maxSteps.value {
		extra := c.Steps[maxSteps.value]
		u.report(CodeSteps, steps.Append(diag.Keyed("name", extra.Name)),
			"composition has "+strconv.Itoa(len(c.Steps))+" steps, more than limits.maxCompositionSteps ("+strconv.FormatInt(maxSteps.value, 10)+")",
			"", maxSteps.related("limit set in"))
	}
	limit := u.ix.maxResponse
	if limit.value < 0 {
		return
	}
	var total uint64
	var free []string
	for _, s := range c.Steps {
		u.w.charge(1)
		if s.MaxBodyBytes == nil {
			free = append(free, s.Name)
			continue
		}
		if *s.MaxBodyBytes < 0 {
			continue
		}
		total += uint64(*s.MaxBodyBytes)
		if total > uint64(limit.value) {
			u.report(CodeStepBodies, steps.Append(diag.Keyed("name", s.Name), diag.Field("maxBodyBytes")),
				"step maxBodyBytes values total "+strconv.FormatUint(total, 10)+" bytes up to step "+quote(s.Name)+
					", more than limits.maxResponseBodyBytes ("+strconv.FormatInt(limit.value, 10)+")",
				"a Route's step bodies together fit in one limits.maxResponseBodyBytes", limit.related("limit set in"))
			return
		}
	}
	if len(free) == 0 {
		return
	}
	left := uint64(limit.value) - total
	if left/uint64(len(free)) == 0 {
		noun := " steps"
		if len(free) == 1 {
			noun = " step"
		}
		u.report(CodeStepBodies, steps.Append(diag.Keyed("name", free[0])),
			"step "+quote(free[0])+" has no maxBodyBytes and no default share: the explicit values leave "+strconv.FormatUint(left, 10)+
				" bytes of limits.maxResponseBodyBytes ("+strconv.FormatInt(limit.value, 10)+") for "+strconv.Itoa(len(free))+noun+" without one",
			"lower the explicit maxBodyBytes values or set maxBodyBytes on every step", limit.related("limit set in"))
	}
}
