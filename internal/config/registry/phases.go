// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// BodyOracle decides the Phase of an authz.cel Policy: whether its
// config.rule references request.body (02 req 13a, 03 req 13, 06 req 56).
// expr.Compiler implements it; the registry never imports the CEL module.
type BodyOracle interface {
	// ReferencesRequestBody reports whether the checked rule src selects
	// request.body.
	ReferencesRequestBody(src string) (bool, error)
}

// Attachment is one Policy attached at one scope, as Phase selection reads
// it.
type Attachment struct {
	// Policy supplies spec.type, spec.plugin and metadata.name.
	Policy *v1alpha1.Policy
	// Config is the decoded typed config (hub.Resource.Config), such as
	// *v1alpha1.HeadersConfig: the type Entry.NewConfig returns, or a value
	// of the type it points to. Phases rejects any other type for every
	// Policy type. Nil, or a typed nil, reads as an empty config, which
	// subscribes headers and transform.* to no Phase.
	Config any
	// Scope is the attaching scope: Gateway, Route or Upstream.
	Scope phase.Scope
	// PluginPhases is the spec.phases of the Plugin a plugin Policy names;
	// other types ignore it.
	PluginPhases []v1alpha1.Phase
}

// ScopeError is a Policy attached at a scope its type does not allow, or a
// plugin Policy whose Plugin lists Phases outside the scope's leg: RZ-CFG-
// 020 at the attaching PolicyRef (02 req 14 and 35.4, 01 req 39). Phases
// returns it wrapped in an errcode.Error carrying CodeScope; use
// errors.As to read it.
type ScopeError struct {
	// Type is the Policy type.
	Type v1alpha1.PolicyType
	// Scope is the attaching scope.
	Scope phase.Scope
	// Allowed are the type's scopes (Entry.Scopes).
	Allowed phase.ScopeSet
	// Plugin is the Plugin a plugin Policy names; "" for other types.
	Plugin string
	// Phases are the Plugin Phases not allowed at Scope, in the Plugin's
	// order, for a plugin Policy.
	Phases []v1alpha1.Phase
}

// Error returns the message of 02 req 35.4: `Policy type "<type>" is not
// allowed at <scope> scope (allowed: <scopes>)`, or for a plugin Policy
// `Plugin "<p>" Phases [<phases>] are not allowed at <scope> scope`.
func (e *ScopeError) Error() string {
	if e.Plugin != "" || len(e.Phases) > 0 {
		names := make([]string, len(e.Phases))
		for i, p := range e.Phases {
			names[i] = string(p)
		}
		return fmt.Sprintf("Plugin %q Phases [%s] are not allowed at %s scope",
			e.Plugin, strings.Join(names, ", "), scopeName(e.Scope))
	}
	return fmt.Sprintf("Policy type %q is not allowed at %s scope (allowed: %s)",
		e.Type, scopeName(e.Scope), scopeList(e.Allowed))
}

// Diagnostic returns the RZ-CFG-020 finding at path, the attaching
// PolicyRef inside the Gateway, Route or Upstream (such as
// spec.policies[name=jwt]). The caller adds Resource and Location.
func (e *ScopeError) Diagnostic(path diag.Path) diag.Diagnostic {
	return diag.Diagnostic{Code: CodeScope, Severity: diag.SeverityError, Path: path, Message: e.Error()}
}

// scopeName spells s for messages; the zero Scope reads "no".
func scopeName(s phase.Scope) string {
	if n := s.String(); n != "" {
		return n
	}
	return "no"
}

// scopeList spells a scope set in chain order, such as "Gateway, Route".
func scopeList(set phase.ScopeSet) string {
	var names []string
	for _, s := range []phase.Scope{phase.ScopeGateway, phase.ScopeRoute, phase.ScopeUpstream} {
		if set.Has(s) {
			names = append(names, s.String())
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// Phases returns the Phases a runs in (Phase selection; hub.Entry.Phases).
// A scope the type does not allow, or a plugin whose Plugin Phases are not
// all on the scope's leg (client Phases at Gateway and Route, upstream-leg
// Phases at Upstream), fails with a *ScopeError wrapped in an
// errcode.Error with CodeScope. Within an allowed scope:
//
//   - authz.cel runs in onRequestBody when body reports that config.rule
//     references request.body, else in onRequestHeaders, never both (02
//     req 13a, 06 req 56). A non-empty rule needs body: with a nil body
//     Phases fails with ErrNoBodyOracle rather than guess, because a body
//     rule placed in onRequestHeaders would deny every request and hide
//     its body read from the cache guardrail (RZ-CFG-038). An empty rule,
//     or a rule body cannot check, selects onRequestHeaders: stage J
//     reports the rule itself (RZ-CFG-014).
//   - authz.opa and authz.cedar run in onRequestHeaders (body analysis is
//     M2; 02 req 13).
//   - headers runs in onRequestHeaders (onUpstreamRequest at Upstream
//     scope) iff config.request holds an entry, and in onResponse
//     (onUpstreamResponseHeaders) iff config.response holds one (07 req 2).
//   - transform.request runs in onRequestBody (onUpstreamRequest), and
//     transform.response in onResponse (onUpstreamResponseBody), iff
//     config sets body or holds any list entry (07 req 2).
//   - plugin runs in its Plugin's Phases.
//   - every other type runs in its declared Phases (cache in
//     onRequestHeaders and onResponse, R-40).
//
// An empty result subscribes the Policy to nothing: it gets no chain row.
// Phases fails with ErrUnknownType for an unregistered type, with
// ErrNoBodyOracle as above, and with a plain error for a nil Policy, the
// zero Scope or a Config whose type is not the type's config type (for
// every type, not only those whose Phases depend on config). These are
// programming errors, checked before the scope rule.
func (r *Registry) Phases(a Attachment, body BodyOracle) (phase.Set, error) {
	if a.Policy == nil {
		return 0, errors.New("registry: Phases of a nil Policy")
	}
	e, err := r.lookup(a.Policy.Spec.Type)
	if err != nil {
		return 0, err
	}
	if err := e.checkConfig(a.Config); err != nil {
		return 0, err
	}
	if a.Scope == phase.ScopeNone {
		return 0, fmt.Errorf("registry: Policy %q has no attachment scope", a.Policy.Metadata.Name)
	}
	if !e.Scopes.Has(a.Scope) {
		return 0, errcode.Wrap(CodeScope, &ScopeError{Type: e.Type, Scope: a.Scope, Allowed: e.Scopes})
	}
	upstream := a.Scope == phase.ScopeUpstream
	declared := e.Phases
	if upstream {
		declared = e.UpstreamPhases
	}
	switch e.sel {
	case selAuthzCEL:
		return authzCELPhase(a.Policy.Metadata.Name, typedConfig[v1alpha1.AuthzCELConfig](a.Config), body)
	case selHeadersOnly:
		return phase.Of(phase.OnRequestHeaders), nil
	case selHeaders:
		return headersPhases(typedConfig[v1alpha1.HeadersConfig](a.Config), upstream), nil
	case selTransform:
		return transformPhases(e.Type, a.Config, declared), nil
	case selPlugin:
		return pluginPhases(a)
	default:
		return declared, nil
	}
}

// checkConfig accepts nil, a value of e's config type (cfgType, such as
// *v1alpha1.CORSConfig, typed nil included) or a value of the type it
// points to; anything else is a programming error. A type without a
// config type accepts only nil.
func (e Entry) checkConfig(cfg any) error {
	if cfg == nil {
		return nil
	}
	got := reflect.TypeOf(cfg)
	if e.cfgType != nil && (got == e.cfgType || (e.cfgType.Kind() == reflect.Pointer && got == e.cfgType.Elem())) {
		return nil
	}
	return fmt.Errorf("registry: %s config is %T, not its registered config type", e.Type, cfg)
}

// typedConfig returns cfg as *T: nil for nil (or a nil *T), the pointer
// for a *T, a copy's address for a T. checkConfig has already rejected
// every other type; for one it returns nil.
func typedConfig[T any](cfg any) *T {
	switch v := cfg.(type) {
	case *T:
		return v
	case T:
		return &v
	default:
		return nil
	}
}

// authzCELPhase selects onRequestBody or onRequestHeaders for the authz.cel
// config c of the Policy named name.
func authzCELPhase(name string, c *v1alpha1.AuthzCELConfig, body BodyOracle) (phase.Set, error) {
	headers := phase.Of(phase.OnRequestHeaders)
	if c == nil || c.Rule == "" {
		return headers, nil
	}
	if body == nil {
		return 0, fmt.Errorf("registry: Policy %q: %w", name, ErrNoBodyOracle)
	}
	if refs, err := body.ReferencesRequestBody(c.Rule); err == nil && refs {
		return phase.Of(phase.OnRequestBody), nil
	}
	return headers, nil
}

// headersPhases selects the request-op and response-op Phases of a headers
// config.
func headersPhases(c *v1alpha1.HeadersConfig, upstream bool) phase.Set {
	var s phase.Set
	if c == nil {
		return s
	}
	reqPhase, respPhase := phase.OnRequestHeaders, phase.OnResponse
	if upstream {
		reqPhase, respPhase = phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders
	}
	if hasListEntry(c.Request) {
		s = s.Add(reqPhase)
	}
	if hasListEntry(c.Response) {
		s = s.Add(respPhase)
	}
	return s
}

// transformPhases returns declared iff a transform.request or
// transform.response config sets body or holds a list entry.
func transformPhases(t v1alpha1.PolicyType, cfg any, declared phase.Set) phase.Set {
	var body string
	var used any
	if t == v1alpha1.PolicyTypeTransformRequest {
		if c := typedConfig[v1alpha1.TransformRequestConfig](cfg); c != nil {
			body, used = c.Body, c
		}
	} else if c := typedConfig[v1alpha1.TransformResponseConfig](cfg); c != nil {
		body, used = c.Body, c
	}
	if body != "" || hasListEntry(used) {
		return declared
	}
	return 0
}

// pluginPhases returns the Plugin's Phases, or a ScopeError listing those
// outside the attaching scope's leg.
func pluginPhases(a Attachment) (phase.Set, error) {
	onLeg := phase.Phase.ClientLeg
	if a.Scope == phase.ScopeUpstream {
		onLeg = phase.Phase.UpstreamLeg
	}
	var s phase.Set
	var bad []v1alpha1.Phase
	for _, v := range a.PluginPhases {
		p, ok := phase.Parse(v)
		if !ok || !onLeg(p) {
			bad = append(bad, v)
			continue
		}
		s = s.Add(p)
	}
	if len(bad) > 0 {
		return 0, errcode.Wrap(CodeScope, &ScopeError{
			Type: v1alpha1.PolicyTypePlugin, Scope: a.Scope, Allowed: phase.Scopes(phase.ScopeGateway, phase.ScopeRoute, phase.ScopeUpstream),
			Plugin: a.Policy.Spec.Plugin, Phases: bad,
		})
	}
	return s, nil
}

// indirect follows pointers and interfaces to the underlying value; it
// returns the zero Value for a nil pointer or interface.
func indirect(v any) reflect.Value {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return reflect.Value{}
		}
		rv = rv.Elem()
	}
	return rv
}

// hasListEntry reports whether the struct v (or *v) holds a non-empty list
// field. Reading every list field, rather than naming them, keeps Phase
// selection right when the schema adds an operation list, such as
// headers add and remove (07 req 2 and 20; additive schema changes, R-62).
func hasListEntry(v any) bool {
	rv := indirect(v)
	if rv.Kind() != reflect.Struct {
		return false
	}
	for i := range rv.NumField() {
		if f := rv.Field(i); f.Kind() == reflect.Slice && f.Len() > 0 {
			return true
		}
	}
	return false
}
