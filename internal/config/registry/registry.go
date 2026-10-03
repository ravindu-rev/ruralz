// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package registry is the Policy type registry of foundation pack section
// 10 (docs/_meta/foundation-pack.md) and the Configuration model's Policy
// section: for every ruralz/v1alpha1 Policy type, its Filter class, the
// Phases it can run in at client and Upstream scope, the scopes it may be
// attached at, its slot, its failureMode default and whether open is
// allowed, whether this release serves it, and the typed config its
// spec.config decodes into.
//
// One package serves every stage that needs the table (architecture R-7):
// stage G (internal/config/defaults and internal/config/convert)
// materializes Defaults and decodes spec.config into NewConfig; stage H
// (internal/config/validate) reports unserved types with CheckServed
// (RZ-CFG-040); stage I (internal/config/precedence) reads EffectiveSlot,
// EffectiveClass, Phases (RZ-CFG-020) and CheckFailureMode (RZ-CFG-029).
// The package holds data and pure functions only: it never imports the
// hub, the CEL module or a Filter, so every binary links it. Phase
// selection for authz.cel asks a BodyOracle the caller passes, which the
// CEL compiler (expr.Compiler) implements; without one it fails with
// ErrNoBodyOracle.
//
// A Registry is immutable after New and safe for concurrent use; build
// one per process and pass it explicitly.
package registry

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// RZ codes this package puts on diagnostics and errors. The stage that
// raises each one is fixed by architecture R-46: stage G or H for
// CodeFilterClass, stage I for CodeScope and CodeFailureMode, and the
// ruralzd serve check after stage M for CodeUnserved (R-75).
const (
	// CodeFilterClass is RZ-CFG-005 for an authored filterClass the
	// registry rejects (02 req 38): on a type other than plugin, a value
	// that differs from the registry class; on plugin, a value that is not
	// one of the nine Filter classes.
	CodeFilterClass = "RZ-CFG-005"
	// CodeScope is RZ-CFG-020: a Policy type, or a Plugin's Phases, not
	// allowed at the attaching scope (02 req 35.4).
	CodeScope = "RZ-CFG-020"
	// CodeFailureMode is RZ-CFG-029: failureMode open on a closed-only
	// type (02 req 37).
	CodeFailureMode = "RZ-CFG-029"
	// CodeUnserved is RZ-CFG-040: a Policy type this release does not
	// serve (architecture section 0 item 8, R-7).
	CodeUnserved = "RZ-CFG-040"
)

// SlotName is the Entry.Slot of types whose slot is the Policy's own
// metadata.name (foundation pack section 10 slot "name"): such Policies
// stack unless an author sets spec.slot.
const SlotName = ""

// ErrUnknownType reports a spec.type the registry does not list. The
// rendered schema's PolicyType enum rejects such a value at stage F
// (RZ-CFG-005), so later stages meet it only through a programming error.
var ErrUnknownType = errors.New("unknown Policy type")

// ErrNoBodyOracle reports Phase selection for an authz.cel Policy with a
// rule but no BodyOracle. Without the oracle the registry cannot tell an
// onRequestBody rule from an onRequestHeaders one (02 req 13a), and a
// silent onRequestHeaders fallback would run a body rule before the body
// exists and hide its body read from the cache guardrail (RZ-CFG-038).
// Every binary links the CEL compiler, so this is a programming error.
var ErrNoBodyOracle = errors.New("authz.cel Phase selection needs a BodyOracle")

// selector names how a type's Phases at one attachment follow from its
// config (Phase selection).
type selector uint8

const (
	// selFixed runs every declared Phase of the scope.
	selFixed selector = iota
	// selAuthzCEL runs onRequestBody when config.rule references
	// request.body, else onRequestHeaders (02 req 13a, 06 req 56).
	selAuthzCEL
	// selHeadersOnly runs onRequestHeaders only: authz.opa and
	// authz.cedar until their body analysis lands (M2; 02 req 13).
	selHeadersOnly
	// selHeaders runs the request-op Phase iff request ops exist and the
	// response-op Phase iff response ops exist (07 req 2).
	selHeaders
	// selTransform runs the scope's Phase iff config sets body or any list
	// entry (07 req 2).
	selTransform
	// selPlugin runs the Plugin's spec.phases (foundation pack section 10).
	selPlugin
)

// Entry is one row of the registry. It is a value; changing a copy never
// changes the Registry.
type Entry struct {
	// Type is the exact spec.type string, such as "auth.api-key".
	Type v1alpha1.PolicyType
	// Class is the registry Filter class. For plugin it is the default
	// class, custom; the Policy's spec.filterClass chooses (ClassFromSpec).
	Class phase.Class
	// ClassFromSpec is true for plugin only: spec.filterClass chooses the
	// class. Every other type may only restate Class (RZ-CFG-005).
	ClassFromSpec bool
	// Phases are the client-leg Phases the type can run in at Gateway or
	// Route scope (foundation pack section 10 "Phases"). Phase selection
	// narrows them by config for authz.cel, headers and transform.*. Empty
	// for types without Gateway or Route scope and for plugin.
	Phases phase.Set
	// UpstreamPhases are the upstream-leg Phases the type can run in at
	// Upstream scope ("at U" in foundation pack section 10). Empty for
	// types without Upstream scope and for plugin.
	UpstreamPhases phase.Set
	// PhasesFromPlugin is true for plugin only: its Phases are the
	// Plugin's spec.phases, and its scopes those matching them.
	PhasesFromPlugin bool
	// Scopes are the scopes the type may be attached at. For plugin all
	// three; its Plugin's Phases decide (Phases, RZ-CFG-020).
	Scopes phase.ScopeSet
	// Slot is the registry slot, or SlotName for the Policy's own name.
	Slot string
	// DefaultFailureMode is materialized on a Policy without failureMode.
	DefaultFailureMode v1alpha1.FailureMode
	// ClosedOnly is true when failureMode open is RZ-CFG-029 for every
	// Policy of the type. For plugin it is false: ClosedOnlyFor decides by
	// the Policy's filterClass (auth and authz are closed only).
	ClosedOnly bool
	// Served is true when this release runs the type; a Revision using an
	// unserved type fails validation with RZ-CFG-040.
	Served bool
	// Planned is the milestone of the type's "Planned (Mn)" tag: "M1",
	// "M2" or "M3".
	Planned string
	// NewConfig returns a new zero typed config into which spec.config
	// decodes: a pointer such as *v1alpha1.RateLimitConfig, named by the
	// type's +ruralz:policyType marker in pkg/config/v1alpha1 (R-62). It is
	// nil for a type without a config type; every v1alpha1 type has one.
	NewConfig func() any

	sel selector
	// cfgType is the dynamic type NewConfig returns, such as
	// *v1alpha1.RateLimitConfig; nil without NewConfig. New fills it once
	// so Phases checks Attachment.Config without allocating.
	cfgType reflect.Type
}

// Registry is the Policy type registry. The zero value is empty; use New.
type Registry struct {
	entries []Entry
	byType  map[v1alpha1.PolicyType]int
}

// New returns the registry of ruralz/v1alpha1: every Policy type of
// foundation pack section 10, in that table's row order.
func New() *Registry {
	t := table()
	r := &Registry{entries: t, byType: make(map[v1alpha1.PolicyType]int, len(t))}
	for i, e := range t {
		r.byType[e.Type] = i
		if e.NewConfig != nil {
			t[i].cfgType = reflect.TypeOf(e.NewConfig())
		}
	}
	return r
}

// Lookup returns the entry of t.
func (r *Registry) Lookup(t v1alpha1.PolicyType) (Entry, bool) {
	i, ok := r.byType[t]
	if !ok {
		return Entry{}, false
	}
	return r.entries[i], true
}

// Entries returns every entry in foundation pack section 10 row order.
func (r *Registry) Entries() []Entry {
	return append([]Entry(nil), r.entries...)
}

// Types returns every registered type in foundation pack section 10 row
// order.
func (r *Registry) Types() []v1alpha1.PolicyType {
	out := make([]v1alpha1.PolicyType, len(r.entries))
	for i, e := range r.entries {
		out[i] = e.Type
	}
	return out
}

// Served reports whether this release serves t; an unknown type is not
// served.
func (r *Registry) Served(t v1alpha1.PolicyType) bool {
	e, ok := r.Lookup(t)
	return ok && e.Served
}

// NewConfig returns a new zero typed config for t (Entry.NewConfig). It
// fails with ErrUnknownType for an unregistered type and returns nil, nil
// for a type without a config type.
func (r *Registry) NewConfig(t v1alpha1.PolicyType) (any, error) {
	e, err := r.lookup(t)
	if err != nil || e.NewConfig == nil {
		return nil, err
	}
	return e.NewConfig(), nil
}

// lookup returns the entry of t or ErrUnknownType.
func (r *Registry) lookup(t v1alpha1.PolicyType) (Entry, error) {
	e, ok := r.Lookup(t)
	if !ok {
		return Entry{}, fmt.Errorf("registry: %w %q", ErrUnknownType, t)
	}
	return e, nil
}

// Registry slots of the table (01 req 40).
const (
	slotAuth          = "auth"
	slotValidation    = "validation"
	slotCORS          = "cors"
	slotCache         = "cache"
	slotUpstreamAuth  = "upstream-auth"
	slotSemanticCache = "semantic-cache"
)

// Milestones of the Planned column.
const (
	m1 = "M1"
	m2 = "M2"
	m3 = "M3"
)

// newConfig returns a constructor of a zero *T.
func newConfig[T any]() func() any { return func() any { return new(T) } }

// table builds foundation pack section 10 row by row. Rows that list
// several types are expanded in their listed order. Served is true exactly
// for the types M1 builds; plugin, authz.opa, authz.cedar, authz.geoip,
// auth.upstream-sigv4 and ai.* are RZ-CFG-040 (architecture WP-04).
func table() []Entry {
	const (
		closed = v1alpha1.FailureModeClosed
		open   = v1alpha1.FailureModeOpen
	)
	var (
		gr         = phase.Scopes(phase.ScopeGateway, phase.ScopeRoute)
		routeOnly  = phase.Scopes(phase.ScopeRoute)
		upOnly     = phase.Scopes(phase.ScopeUpstream)
		gru        = phase.Scopes(phase.ScopeGateway, phase.ScopeRoute, phase.ScopeUpstream)
		reqHeaders = phase.Of(phase.OnRequestHeaders)
		reqBody    = phase.Of(phase.OnRequestBody)
		headersAnd = phase.Of(phase.OnRequestHeaders, phase.OnRequestBody)
		upReq      = phase.Of(phase.OnUpstreamRequest)
	)
	// auth is the shared row of the four client authentication types
	// (06 req 1).
	auth := func(t v1alpha1.PolicyType, cfg func() any) Entry {
		return Entry{
			Type: t, Class: phase.ClassAuth, Phases: reqHeaders, Scopes: gr, Slot: slotAuth,
			DefaultFailureMode: closed, ClosedOnly: true, Served: true, Planned: m1, NewConfig: cfg,
		}
	}
	// authz is the shared row shape of authz.* (06 req 53).
	authz := func(t v1alpha1.PolicyType, ps phase.Set, sel selector, served bool, planned string, cfg func() any) Entry {
		return Entry{
			Type: t, Class: phase.ClassAuthz, Phases: ps, Scopes: gr, Slot: SlotName,
			DefaultFailureMode: closed, ClosedOnly: true, Served: served, Planned: planned, NewConfig: cfg, sel: sel,
		}
	}
	// upstreamAuth is the shared row of auth.upstream-* (06 req 68).
	upstreamAuth := func(t v1alpha1.PolicyType, served bool, planned string, cfg func() any) Entry {
		return Entry{
			Type: t, Class: phase.ClassUpstreamAuth, UpstreamPhases: upReq, Scopes: upOnly, Slot: slotUpstreamAuth,
			DefaultFailureMode: closed, ClosedOnly: true, Served: served, Planned: planned, NewConfig: cfg,
		}
	}
	return []Entry{
		auth(v1alpha1.PolicyTypeAuthJWT, newConfig[v1alpha1.AuthJWTConfig]()),
		auth(v1alpha1.PolicyTypeAuthAPIKey, newConfig[v1alpha1.AuthAPIKeyConfig]()),
		auth(v1alpha1.PolicyTypeAuthBasic, newConfig[v1alpha1.AuthBasicConfig]()),
		auth(v1alpha1.PolicyTypeAuthMTLS, newConfig[v1alpha1.AuthMTLSConfig]()),
		authz(v1alpha1.PolicyTypeAuthzCEL, headersAnd, selAuthzCEL, true, m1, newConfig[v1alpha1.AuthzCELConfig]()),
		authz(v1alpha1.PolicyTypeAuthzOPA, headersAnd, selHeadersOnly, false, m2, newConfig[v1alpha1.AuthzOPAConfig]()),
		authz(v1alpha1.PolicyTypeAuthzCedar, headersAnd, selHeadersOnly, false, m2, newConfig[v1alpha1.AuthzCedarConfig]()),
		authz(v1alpha1.PolicyTypeAuthzIP, reqHeaders, selFixed, true, m1, newConfig[v1alpha1.AuthzIPConfig]()),
		authz(v1alpha1.PolicyTypeAuthzGeoIP, reqHeaders, selFixed, false, m2, newConfig[v1alpha1.AuthzGeoIPConfig]()),
		{
			// 05 req 56.
			Type: v1alpha1.PolicyTypeRateLimit, Class: phase.ClassAdmission, Phases: reqHeaders, Scopes: gr,
			Slot: SlotName, DefaultFailureMode: open, Served: true, Planned: m1,
			NewConfig: newConfig[v1alpha1.RateLimitConfig](),
		},
		{
			// 05 req 69; open per OQ-configuration-model-8 (a).
			Type: v1alpha1.PolicyTypeQuota, Class: phase.ClassAdmission,
			Phases: phase.Of(phase.OnRequestHeaders, phase.OnLog), Scopes: gr,
			Slot: SlotName, DefaultFailureMode: open, Served: true, Planned: m1,
			NewConfig: newConfig[v1alpha1.QuotaConfig](),
		},
		{
			// 07 req 1 and 3.
			Type: v1alpha1.PolicyTypeValidationJSONSchema, Class: phase.ClassValidation, Phases: reqBody, Scopes: routeOnly,
			Slot: slotValidation, DefaultFailureMode: closed, Served: true, Planned: m1,
			NewConfig: newConfig[v1alpha1.ValidationJSONSchemaConfig](),
		},
		{
			// 06 req 64.
			Type: v1alpha1.PolicyTypeCORS, Class: phase.ClassCORS,
			Phases: phase.Of(phase.OnRequestHeaders, phase.OnResponse), Scopes: gr,
			Slot: slotCORS, DefaultFailureMode: closed, Served: true, Planned: m1,
			NewConfig: newConfig[v1alpha1.CORSConfig](),
		},
		{
			// 05 req 75; stores run in filter.Finisher, the Phases stay (R-40).
			Type: v1alpha1.PolicyTypeCache, Class: phase.ClassCache,
			Phases: phase.Of(phase.OnRequestHeaders, phase.OnResponse), Scopes: routeOnly,
			Slot: slotCache, DefaultFailureMode: open, Served: true, Planned: m1,
			NewConfig: newConfig[v1alpha1.CacheConfig](),
		},
		{
			// 07 req 1 and 2.
			Type: v1alpha1.PolicyTypeHeaders, Class: phase.ClassTransform,
			Phases:         phase.Of(phase.OnRequestHeaders, phase.OnResponse),
			UpstreamPhases: phase.Of(phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders),
			Scopes:         gru, Slot: SlotName, DefaultFailureMode: closed, Served: true, Planned: m1,
			NewConfig: newConfig[v1alpha1.HeadersConfig](), sel: selHeaders,
		},
		{
			Type: v1alpha1.PolicyTypeTransformRequest, Class: phase.ClassTransform,
			Phases: reqBody, UpstreamPhases: upReq,
			Scopes: gru, Slot: SlotName, DefaultFailureMode: closed, Served: true, Planned: m1,
			NewConfig: newConfig[v1alpha1.TransformRequestConfig](), sel: selTransform,
		},
		{
			Type: v1alpha1.PolicyTypeTransformResponse, Class: phase.ClassTransform,
			Phases: phase.Of(phase.OnResponse), UpstreamPhases: phase.Of(phase.OnUpstreamResponseBody),
			Scopes: gru, Slot: SlotName, DefaultFailureMode: closed, Served: true, Planned: m1,
			NewConfig: newConfig[v1alpha1.TransformResponseConfig](), sel: selTransform,
		},
		upstreamAuth(v1alpha1.PolicyTypeAuthUpstreamOAuth2, true, m1, newConfig[v1alpha1.AuthUpstreamOAuth2Config]()),
		upstreamAuth(v1alpha1.PolicyTypeAuthUpstreamSigV4, false, m2, newConfig[v1alpha1.AuthUpstreamSigV4Config]()),
		{
			// Closed per OQ-configuration-model-8 (a).
			Type: v1alpha1.PolicyTypeAITokenBudget, Class: phase.ClassAdmission,
			Phases: phase.Of(phase.OnRequestBody, phase.OnChunk, phase.OnLog), Scopes: gr,
			Slot: SlotName, DefaultFailureMode: closed, Planned: m3,
			NewConfig: newConfig[v1alpha1.AITokenBudgetConfig](),
		},
		{
			Type: v1alpha1.PolicyTypeAISemanticCache, Class: phase.ClassCache,
			Phases: phase.Of(phase.OnRequestBody, phase.OnResponse), Scopes: routeOnly,
			Slot: slotSemanticCache, DefaultFailureMode: open, Planned: m3,
			NewConfig: newConfig[v1alpha1.AISemanticCacheConfig](),
		},
		{
			Type: v1alpha1.PolicyTypeAIGuardrail, Class: phase.ClassValidation,
			Phases: phase.Of(phase.OnRequestBody, phase.OnChunk, phase.OnResponse), Scopes: gr,
			Slot: SlotName, DefaultFailureMode: closed, Planned: m3,
			NewConfig: newConfig[v1alpha1.AIGuardrailConfig](),
		},
		{
			Type: v1alpha1.PolicyTypePlugin, Class: phase.ClassCustom, ClassFromSpec: true,
			PhasesFromPlugin: true, Scopes: gru, Slot: SlotName, DefaultFailureMode: closed, Planned: m2,
			NewConfig: newConfig[v1alpha1.PluginConfig](), sel: selPlugin,
		},
	}
}
