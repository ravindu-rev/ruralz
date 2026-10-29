// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// route checks a Route: its listeners name Gateway listeners
// (RZ-CFG-009), its match criteria compile as the Router compiles them
// (RZ-CFG-005), and its composition fits the Gateway limits (RZ-CFG-031,
// RZ-CFG-032).
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
// sequential and conditional modes); the step count against
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
