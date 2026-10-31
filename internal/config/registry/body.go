// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"errors"
	"fmt"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// CELRefs reports what a CEL source reads at a site, for decisions taken
// from a Policy's config before its Filter is built. The CEL compiler
// implements it (its References method); the registry never imports the
// CEL module.
type CELRefs interface {
	// References returns what src reads checked at site; an error when it
	// does not check there.
	References(site expr.Site, src string) (expr.Refs, error)
}

// ErrNoCELRefs reports a body-need decision for a transform config whose
// CEL fields must be asked whether they read the body, without CELRefs.
// Guessing either way would be wrong: a missed read leaves the body
// streaming under an expression that needs it, a false one gates a body
// for nothing. Every binary links the CEL compiler, so this is a
// programming error.
var ErrNoCELRefs = errors.New("transform body selection needs CEL references")

// TransformBody reports whether a, a transform.request or
// transform.response attachment, reads the body it acts on whole, which
// makes it a gate (docs/architecture/03-data-plane.md "Transform
// Policies", docs/architecture/02-configuration-model.md "Body buffering
// and limits"): its config sets body, holds an arrayOps or replace entry
// or a set or remove entry with target body, or has a CEL field that reads
// request.body (transform.request) or response.body (transform.response)
// checked at the attachment's site, as refs reports. Any other transform
// runs at its Phase while the body streams on.
//
// The body a transform acts on is the client request body for
// transform.request at every scope, since one at Upstream scope reads the
// same buffered input on each attempt, and for transform.response the
// client response at Gateway and Route scope and its leg's response at
// Upstream scope. A Policy whose spec.when reads a body gates it too; the
// caller adds that, as for every Policy type.
//
// A CEL field refs cannot check reads no body here: stage J reports it
// (RZ-CFG-014) and the Revision is refused anyway. The errors follow
// Phases: a nil Policy, the zero Scope or a Config of another type is a
// plain error, an unregistered type ErrUnknownType, a type other than
// transform.request and transform.response a plain error, a scope the type
// does not allow a *ScopeError with CodeScope, and a CEL field to ask
// without refs ErrNoCELRefs.
func (r *Registry) TransformBody(a Attachment, refs CELRefs) (bool, error) {
	if a.Policy == nil {
		return false, errors.New("registry: TransformBody of a nil Policy")
	}
	e, err := r.lookup(a.Policy.Spec.Type)
	if err != nil {
		return false, err
	}
	if e.sel != selTransform {
		return false, fmt.Errorf("registry: TransformBody of Policy %q of type %s, not a transform type", a.Policy.Metadata.Name, e.Type)
	}
	if err := e.checkConfig(a.Config); err != nil {
		return false, err
	}
	if a.Scope == phase.ScopeNone {
		return false, fmt.Errorf("registry: Policy %q has no attachment scope", a.Policy.Metadata.Name)
	}
	if !e.Scopes.Has(a.Scope) {
		return false, errcode.Wrap(CodeScope, &ScopeError{Type: e.Type, Scope: a.Scope, Allowed: e.Scopes})
	}
	var c transformBodyFields
	if e.Type == v1alpha1.PolicyTypeTransformRequest {
		c = requestBodyFields(typedConfig[v1alpha1.TransformRequestConfig](a.Config))
	} else {
		c = responseBodyFields(typedConfig[v1alpha1.TransformResponseConfig](a.Config))
	}
	if c.rewrites {
		return true, nil
	}
	if len(c.values) == 0 {
		return false, nil
	}
	if refs == nil {
		return false, fmt.Errorf("registry: Policy %q: %w", a.Policy.Metadata.Name, ErrNoCELRefs)
	}
	site := expr.Site{Place: c.place, Scope: a.Scope, PolicyType: e.Type, Result: expr.ResultString}
	for _, src := range c.values {
		got, err := refs.References(site, src)
		if err != nil {
			continue
		}
		if (c.request && got.RequestBody) || (!c.request && got.ResponseBody) {
			return true, nil
		}
	}
	return false, nil
}

// transformBodyFields is what decides a transform config's body need: the
// fields that rewrite the body, and the valueExpressions of the entries
// whose targets are not the body, which read it only through CEL.
type transformBodyFields struct {
	// rewrites is set when the config sets body or holds an arrayOps,
	// replace, or body-target set or remove entry.
	rewrites bool
	// request is set for transform.request, which reads request.body;
	// transform.response reads response.body.
	request bool
	// place is the valueExpression place, values the valueExpressions of
	// the header, query and path entries.
	place  expr.PlaceID
	values []string
}

// requestBodyFields returns the body-need fields of a transform.request
// config; nil is an empty config.
func requestBodyFields(c *v1alpha1.TransformRequestConfig) transformBodyFields {
	out := transformBodyFields{request: true, place: expr.PlaceTransformRequestValue}
	if c == nil {
		return out
	}
	out.rewrites = c.Body != "" || len(c.ArrayOps) > 0 || len(c.Replace) > 0
	for _, s := range c.Set {
		if s.Target == v1alpha1.TransformRequestTargetBody {
			out.rewrites = true
			continue
		}
		out.values = append(out.values, s.ValueExpression)
	}
	for _, rm := range c.Remove {
		out.rewrites = out.rewrites || rm.Target == v1alpha1.TransformRequestTargetBody
	}
	return out
}

// responseBodyFields returns the body-need fields of a transform.response
// config; nil is an empty config.
func responseBodyFields(c *v1alpha1.TransformResponseConfig) transformBodyFields {
	out := transformBodyFields{place: expr.PlaceTransformResponseValue}
	if c == nil {
		return out
	}
	out.rewrites = c.Body != "" || len(c.ArrayOps) > 0 || len(c.Replace) > 0
	for _, s := range c.Set {
		if s.Target == v1alpha1.TransformResponseTargetBody {
			out.rewrites = true
			continue
		}
		out.values = append(out.values, s.ValueExpression)
	}
	for _, rm := range c.Remove {
		out.rewrites = out.rewrites || rm.Target == v1alpha1.TransformResponseTargetBody
	}
	return out
}
