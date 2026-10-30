// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"strconv"
	"strings"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
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
	reg   *registry.Registry
	cache *sync.Map
}

// policySite returns the site of the field at place and path (key-aware,
// under the resource) of Policy r attached at scope, where it runs in
// phases (hub.Entry.Phases): the place, scope and type; for
// Policy.spec.when the first Phase there, which declares response when it
// is a response Phase, and whether the Policy gates the request body
// there, which lets the condition select request.body (configuration
// model, "Variables"); and for a transform set[] valueExpression the
// result its entry's target takes (entryResult).
func (s *sites) policySite(r *hub.Resource, p *v1alpha1.Policy, place expr.PlaceID, path diag.Path, scope phase.Scope, phases phase.Set) expr.Site {
	site := expr.Site{Place: place, Scope: scope, PolicyType: p.Spec.Type, Result: entryResult(r, place, path)}
	if place == expr.PlacePolicyWhen {
		site.FirstPhase, _ = phases.First()
		site.GatesBody = s.gatesBody(r, p, scope, phases)
	}
	return site
}

// entryResult returns the result the transform set[] valueExpression at
// path takes by its entry's target (expr.TransformValueResult): string for
// targets header, query and path, dyn for body. Any other field, and a
// path that names no entry of r's typed config, keeps its place's result
// (0).
func entryResult(r *hub.Resource, place expr.PlaceID, path diag.Path) expr.Result {
	if place != expr.PlaceTransformRequestValue && place != expr.PlaceTransformResponseValue {
		return 0
	}
	i := setIndex(path)
	if i < 0 {
		return 0
	}
	switch cfg := r.Config.(type) {
	case *v1alpha1.TransformRequestConfig:
		if cfg != nil && i < len(cfg.Set) {
			return expr.TransformValueResult(string(cfg.Set[i].Target))
		}
	case *v1alpha1.TransformResponseConfig:
		if cfg != nil && i < len(cfg.Set) {
			return expr.TransformValueResult(string(cfg.Set[i].Target))
		}
	}
	return 0
}

// setIndex returns the index of the config.set[] entry path names, -1
// when it names none.
func setIndex(path diag.Path) int {
	for i, e := range path {
		if e.Kind == diag.ElemIndex && i > 0 && isField(path[i-1], "set") {
			return e.Index
		}
	}
	return -1
}

// gatesBody reports whether Policy p gates the request body at an
// attachment at scope where it runs in phases: it holds the request until
// the body is complete, so the body can be read
// (docs/architecture/02-configuration-model.md "Body buffering and
// limits"). Every Policy that runs in onRequestBody does, such as authz.*
// with a rule that reads the body, validation.json-schema, ai.token-budget
// and a Plugin with onRequestBody, except transform.request: it gates the
// body at every scope exactly when its config needs the body, as the
// registry decides (registry.Registry.TransformBody), and otherwise runs
// while the body streams on (docs/architecture/03-data-plane.md
// "Transform Policies").
func (s *sites) gatesBody(r *hub.Resource, p *v1alpha1.Policy, scope phase.Scope, phases phase.Set) bool {
	if p.Spec.Type == v1alpha1.PolicyTypeTransformRequest {
		needs, err := s.reg.TransformBody(registry.Attachment{Policy: p, Config: r.Config, Scope: scope}, s)
		return err == nil && needs
	}
	return phases.Has(phase.OnRequestBody)
}

// refsResult is one References answer the cache keeps.
type refsResult struct {
	refs expr.Refs
	err  error
}

// References implements registry.CELRefs over the compiler, remembering
// each site and source for the run: what src reads at site, an error when
// it does not check there. stage J reports a source that does not compile
// at its own field.
func (s *sites) References(site expr.Site, src string) (expr.Refs, error) {
	key := "refs\x00" + siteKey(site) + src
	if s.cache != nil {
		if v, ok := s.cache.Load(key); ok {
			r, _ := v.(refsResult)
			return r.refs, r.err
		}
	}
	refs, err := s.cel.References(site, src)
	if s.cache != nil {
		s.cache.Store(key, refsResult{refs: refs, err: err})
	}
	return refs, err
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
	b.WriteString(strconv.Itoa(int(s.Result)))
	b.WriteByte(',')
	b.WriteString(strconv.Itoa(len(s.EarlierSteps)))
	for _, st := range s.EarlierSteps {
		part(st)
	}
	b.WriteByte(0)
	return b.String()
}
