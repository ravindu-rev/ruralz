// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package phase is the Filter Chain vocabulary shared by the configuration
// pipeline, the CEL module, the Filter SPI and the data plane: the fixed
// Phases (foundation pack section 4), the Filter classes and their order,
// and the attachment scopes (section 8.12). Values are small integers so
// chains index arrays by Phase; String returns the v1alpha1 spelling.
package phase

import "github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"

// Phase is one Filter Chain Phase, in execution order.
type Phase uint8

// Phases. Count is the number of Phases, for arrays indexed by Phase.
const (
	OnRequestHeaders Phase = iota
	OnRequestBody
	OnRoute
	OnUpstreamRequest
	OnUpstreamResponseHeaders
	OnUpstreamResponseBody
	OnResponse
	OnLog
	OnChunk
	Count
)

// V1alpha1 returns the configuration spelling, such as "onRequestHeaders".
func (p Phase) V1alpha1() v1alpha1.Phase {
	switch p {
	case OnRequestHeaders:
		return v1alpha1.PhaseOnRequestHeaders
	case OnRequestBody:
		return v1alpha1.PhaseOnRequestBody
	case OnRoute:
		return v1alpha1.PhaseOnRoute
	case OnUpstreamRequest:
		return v1alpha1.PhaseOnUpstreamRequest
	case OnUpstreamResponseHeaders:
		return v1alpha1.PhaseOnUpstreamResponseHeaders
	case OnUpstreamResponseBody:
		return v1alpha1.PhaseOnUpstreamResponseBody
	case OnResponse:
		return v1alpha1.PhaseOnResponse
	case OnLog:
		return v1alpha1.PhaseOnLog
	case OnChunk:
		return v1alpha1.PhaseOnChunk
	default:
		return ""
	}
}

// String returns the configuration spelling; it is also the metric label.
func (p Phase) String() string { return string(p.V1alpha1()) }

// Parse maps a configuration spelling to a Phase.
func Parse(v v1alpha1.Phase) (Phase, bool) {
	for p := OnRequestHeaders; p < Count; p++ {
		if p.V1alpha1() == v {
			return p, true
		}
	}
	return 0, false
}

// IsResponse reports whether the Phase runs in reverse chain order:
// onUpstreamResponseHeaders, onUpstreamResponseBody, onResponse, and
// onChunk (ordered as a response Phase in M1).
func (p Phase) IsResponse() bool {
	return p == OnUpstreamResponseHeaders || p == OnUpstreamResponseBody || p == OnResponse || p == OnChunk
}

// CanShortCircuit reports whether a Filter may respond in the Phase:
// onRequestHeaders, onRequestBody, onRoute and onUpstreamRequest.
func (p Phase) CanShortCircuit() bool { return p <= OnUpstreamRequest }

// ClientLeg reports whether the Phase runs on the client leg (Gateway and
// Route scope): onRequestHeaders, onRequestBody, onRoute, onResponse,
// onLog, onChunk.
func (p Phase) ClientLeg() bool {
	return p <= OnRoute || p == OnResponse || p == OnLog || p == OnChunk
}

// UpstreamLeg reports whether the Phase runs per upstream leg (Upstream
// scope): onUpstreamRequest, onUpstreamResponseHeaders,
// onUpstreamResponseBody, onChunk.
func (p Phase) UpstreamLeg() bool {
	return (p >= OnUpstreamRequest && p <= OnUpstreamResponseBody) || p == OnChunk
}

// Set is a set of Phases.
type Set uint16

// Of returns the set of ps.
func Of(ps ...Phase) Set {
	var s Set
	for _, p := range ps {
		s |= 1 << p
	}
	return s
}

// Has reports whether p is in s.
func (s Set) Has(p Phase) bool { return s&(1<<p) != 0 }

// Add returns s with p.
func (s Set) Add(p Phase) Set { return s | 1<<p }

// Empty reports whether s has no Phase.
func (s Set) Empty() bool { return s == 0 }

// First returns the earliest Phase of s in execution order.
func (s Set) First() (Phase, bool) {
	for p := OnRequestHeaders; p < Count; p++ {
		if s.Has(p) {
			return p, true
		}
	}
	return 0, false
}

// Phases lists the members of s in execution order.
func (s Set) Phases() []Phase {
	var out []Phase
	for p := OnRequestHeaders; p < Count; p++ {
		if s.Has(p) {
			out = append(out, p)
		}
	}
	return out
}

// Class is a Filter class; its value is its rank in a Phase.
type Class uint8

// Filter classes in chain order (foundation pack section 8.12).
const (
	ClassCORS Class = iota
	ClassAuth
	ClassAuthz
	ClassAdmission
	ClassValidation
	ClassCache
	ClassUpstreamAuth
	ClassTransform
	ClassCustom
	NumClasses
)

// V1alpha1 returns the configuration spelling.
func (c Class) V1alpha1() v1alpha1.FilterClass {
	switch c {
	case ClassCORS:
		return v1alpha1.FilterClassCORS
	case ClassAuth:
		return v1alpha1.FilterClassAuth
	case ClassAuthz:
		return v1alpha1.FilterClassAuthz
	case ClassAdmission:
		return v1alpha1.FilterClassAdmission
	case ClassValidation:
		return v1alpha1.FilterClassValidation
	case ClassCache:
		return v1alpha1.FilterClassCache
	case ClassUpstreamAuth:
		return v1alpha1.FilterClassUpstreamAuth
	case ClassTransform:
		return v1alpha1.FilterClassTransform
	case ClassCustom:
		return v1alpha1.FilterClassCustom
	default:
		return ""
	}
}

// String returns the configuration spelling.
func (c Class) String() string { return string(c.V1alpha1()) }

// ParseClass maps a configuration spelling to a Class.
func ParseClass(v v1alpha1.FilterClass) (Class, bool) {
	for c := ClassCORS; c < NumClasses; c++ {
		if c.V1alpha1() == v {
			return c, true
		}
	}
	return 0, false
}

// Scope is where a Policy is attached. The zero value means none (an
// unattached Policy or a non-Policy CEL field).
type Scope uint8

// Scopes in chain order.
const (
	ScopeNone Scope = iota
	ScopeGateway
	ScopeRoute
	ScopeUpstream
)

// String returns "Gateway", "Route", "Upstream" or "".
func (s Scope) String() string {
	switch s {
	case ScopeGateway:
		return "Gateway"
	case ScopeRoute:
		return "Route"
	case ScopeUpstream:
		return "Upstream"
	default:
		return ""
	}
}

// ScopeSet is a set of scopes, such as the registry's allowed scopes.
type ScopeSet uint8

// Scopes returns the set of ss.
func Scopes(ss ...Scope) ScopeSet {
	var out ScopeSet
	for _, s := range ss {
		out |= 1 << s
	}
	return out
}

// Has reports whether s is in set.
func (set ScopeSet) Has(s Scope) bool { return set&(1<<s) != 0 }
