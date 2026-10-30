// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"strconv"
	"strings"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// PlaceOf returns the CEL place of the x-ruralz-cel field at path p of a
// resource of kind (docs/architecture/02-configuration-model.md "Allowed
// places"); policyType is a Policy's spec.type, unused for other kinds.
// The place names the field by its path from the resource root with every
// list element written "[]", such as Route.spec.composition.steps[].when;
// a Policy config field is named by its type, as in
// "ratelimit config.key". The add[] entries of a headers config share the
// place of its set[] entries, which the set[] spelling names
// (expr.PlaceHeadersRequestValue). false means expr.Places has no such
// place.
func PlaceOf(kind v1alpha1.Kind, policyType v1alpha1.PolicyType, p diag.Path) (expr.PlaceID, bool) {
	var b strings.Builder
	rest := p
	if kind == v1alpha1.KindPolicy && len(p) >= 2 && isField(p[0], "spec") && isField(p[1], "config") {
		b.WriteString(string(policyType))
		b.WriteString(" config")
		rest = p[2:]
	} else {
		b.WriteString(string(kind))
	}
	for _, e := range rest {
		if e.Kind == diag.ElemField {
			b.WriteByte('.')
			b.WriteString(e.Name)
		} else {
			b.WriteString("[]")
		}
	}
	id := expr.PlaceID(b.String())
	if _, ok := expr.LookupPlace(id); ok {
		return id, true
	}
	if policyType == v1alpha1.PolicyTypeHeaders {
		alias := expr.PlaceID(strings.Replace(string(id), ".add[].", ".set[].", 1))
		if _, ok := expr.LookupPlace(alias); ok {
			return alias, true
		}
	}
	return "", false
}

// isField reports whether e is the object member name.
func isField(e diag.PathElem, name string) bool { return e.Kind == diag.ElemField && e.Name == name }

// RouteSite returns the site of the Route field at place: the bare place,
// or for the composition places Route.spec.composition.steps[].pathExpression
// and .when the site of step, the step of that name, which carries the
// composition mode and the names of the steps before it in list order, so
// steps reads only in a sequential composition and a constant key names
// an earlier step (configuration model, "Variables"). The snapshot
// compiler passes the same sites to expr.Builder.Compile.
func RouteSite(route *v1alpha1.Route, place expr.PlaceID, step string) expr.Site {
	site := expr.Site{Place: place}
	if route == nil || route.Spec.Composition == nil ||
		(place != expr.PlaceStepPathExpression && place != expr.PlaceStepWhen) {
		return site
	}
	c := route.Spec.Composition
	site.Mode = c.Mode
	for _, s := range c.Steps {
		if s.Name == step {
			return site
		}
		site.EarlierSteps = append(site.EarlierSteps, s.Name)
	}
	// A step the composition does not hold reads no earlier step.
	site.EarlierSteps = nil
	return site
}

// stepOf returns the name of the composition step a path below
// spec.composition.steps names, "" when it names none.
func stepOf(p diag.Path) string {
	for i, e := range p {
		if e.Kind == diag.ElemKeyed && i > 0 && isField(p[i-1], "steps") {
			return e.Key
		}
	}
	return ""
}

// sites builds the sites of Policy fields. Its cache, when set, holds what
// one run learned about expressions, keyed by site and source.
type sites struct {
	cel   expr.Compiler
	cache *sync.Map
}

// policySite returns the site of a field at place of Policy r attached at
// scope, where it runs in phases (hub.Entry.Phases): the place, scope and
// type, and for Policy.spec.when the first Phase there, which declares
// response when it is a response Phase, and whether the Policy gates the
// request body there, which lets the condition select request.body
// (configuration model, "Variables").
func (s *sites) policySite(r *hub.Resource, p *v1alpha1.Policy, place expr.PlaceID, scope phase.Scope, phases phase.Set) expr.Site {
	site := expr.Site{Place: place, Scope: scope, PolicyType: p.Spec.Type}
	if place == expr.PlacePolicyWhen {
		site.FirstPhase, _ = phases.First()
		site.GatesBody = s.gatesBody(r, p, scope, phases)
	}
	return site
}

// gatesBody reports whether Policy p gates the request body at an
// attachment at scope where it runs in phases: it holds the request until
// the body is complete, so the body can be read
// (docs/architecture/02-configuration-model.md "Body buffering and
// limits"). Every Policy that runs in onRequestBody does, such as authz.*
// with a rule that reads the body, validation.json-schema, ai.token-budget
// and a Plugin with onRequestBody, except transform.request: it gates the
// body at every scope exactly when its config needs the body
// (transformNeedsBody), and otherwise runs while the body streams on
// (docs/architecture/03-data-plane.md "Transform Policies").
func (s *sites) gatesBody(r *hub.Resource, p *v1alpha1.Policy, scope phase.Scope, phases phase.Set) bool {
	if p.Spec.Type == v1alpha1.PolicyTypeTransformRequest {
		cfg, _ := r.Config.(*v1alpha1.TransformRequestConfig)
		return s.transformNeedsBody(cfg, scope)
	}
	return phases.Has(phase.OnRequestBody)
}

// transformNeedsBody reports whether a transform.request config at scope
// needs the request body: it sets body, holds an arrayOps[] or replace[]
// entry or a set[] or remove[] entry with target body, or a set[]
// valueExpression reads request.body (docs/architecture/03-data-plane.md
// "Transform Policies").
func (s *sites) transformNeedsBody(cfg *v1alpha1.TransformRequestConfig, scope phase.Scope) bool {
	if cfg == nil {
		return false
	}
	if cfg.Body != "" || len(cfg.ArrayOps) > 0 || len(cfg.Replace) > 0 {
		return true
	}
	for _, r := range cfg.Remove {
		if r.Target == v1alpha1.TransformRequestTargetBody {
			return true
		}
	}
	for _, e := range cfg.Set {
		if e.Target == v1alpha1.TransformRequestTargetBody {
			return true
		}
	}
	site := expr.Site{Place: expr.PlaceTransformRequestValue, Scope: scope, PolicyType: v1alpha1.PolicyTypeTransformRequest}
	for _, e := range cfg.Set {
		if s.readsRequestBody(site, e.ValueExpression) {
			return true
		}
	}
	return false
}

// readsRequestBody reports whether src, compiled at site, selects
// request.body. A source that does not compile reads nothing: stage J
// reports it at its own field.
func (s *sites) readsRequestBody(site expr.Site, src string) bool {
	if src == "" {
		return false
	}
	key := "body\x00" + siteKey(site) + src
	if s.cache != nil {
		if v, ok := s.cache.Load(key); ok {
			b, _ := v.(bool)
			return b
		}
	}
	prg, _ := s.cel.NewBuilder(nil, expr.InputCaps{}).Compile(site, src)
	reads := prg != nil && prg.Refs().RequestBody
	if s.cache != nil {
		s.cache.Store(key, reads)
	}
	return reads
}

// siteKey returns a key that tells sites apart, each variable-length part
// prefixed by its length so no two sites share one, ending in a NUL.
func siteKey(s expr.Site) string {
	var b strings.Builder
	part := func(v string) {
		b.WriteString(strconv.Itoa(len(v)))
		b.WriteByte(':')
		b.WriteString(v)
	}
	part(string(s.Place))
	b.WriteString(strconv.Itoa(int(s.Scope)))
	b.WriteByte(',')
	part(string(s.PolicyType))
	b.WriteString(strconv.Itoa(int(s.FirstPhase)))
	b.WriteByte(',')
	b.WriteString(strconv.FormatBool(s.GatesBody))
	part(string(s.Mode))
	b.WriteString(strconv.Itoa(len(s.EarlierSteps)))
	for _, st := range s.EarlierSteps {
		part(st)
	}
	b.WriteByte(0)
	return b.String()
}
