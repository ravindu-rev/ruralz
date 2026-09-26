// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package expr is the contract of Ruralz CEL expressions (ADR-0011): the
// places where CEL is allowed, the site of one occurrence, compiled
// programs, the activation (Vars) and its request views, and runtime
// errors. It imports no CEL library: internal/cel implements Compiler,
// Builder, Program and Value on cel.dev/cel-go, and every other package
// (configuration validation, Router, Filters, Upstream layer, access log)
// depends only on this package.
package expr

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// PlaceID names a field whose schema carries x-ruralz-cel.
type PlaceID string

// The 21 places of docs/architecture/02-configuration-model.md "Allowed
// places". A test in internal/cel maps each one to exactly one schema
// annotation.
const (
	PlaceRouteMatchWhen         PlaceID = "Route.spec.match.when"
	PlacePolicyWhen             PlaceID = "Policy.spec.when"
	PlaceStepPathExpression     PlaceID = "Route.spec.composition.steps[].pathExpression"
	PlaceStepWhen               PlaceID = "Route.spec.composition.steps[].when" //nolint:gosec // G101: a CEL place name, not a credential.
	PlaceAuthzCELRule           PlaceID = "authz.cel config.rule"
	PlaceRateLimitKey           PlaceID = "ratelimit config.key"
	PlaceQuotaKey               PlaceID = "quota config.key"
	PlaceHeadersRequestValue    PlaceID = "headers config.request.set[].valueExpression"
	PlaceHeadersResponseValue   PlaceID = "headers config.response.set[].valueExpression"
	PlaceCacheKey               PlaceID = "cache config.key"
	PlaceTransformRequestBody   PlaceID = "transform.request config.body"
	PlaceTransformRequestValue  PlaceID = "transform.request config.set[].valueExpression"
	PlaceTransformResponseBody  PlaceID = "transform.response config.body"
	PlaceTransformResponseValue PlaceID = "transform.response config.set[].valueExpression"
	PlaceHashKey                PlaceID = "Upstream.spec.loadBalancing.hashKey"
	PlaceRetryOn                PlaceID = "Upstream.spec.retries.retryOn"
	PlaceFailureWhen            PlaceID = "Upstream.spec.circuitBreaker.failureWhen"
	PlaceAccessLogWhen          PlaceID = "Gateway.spec.telemetry.accessLog.when"
	PlaceSemanticCacheKey       PlaceID = "ai.semantic-cache config.key"   // validation only until M3
	PlaceCandidateWhen          PlaceID = "AIModel.spec.candidates[].when" // validation only until M3
	PlaceMessagingKey           PlaceID = "Upstream.spec.messaging.key"    // validation only until M4
)

// Result is the result type a place requires.
type Result uint8

// Result types.
const (
	ResultBool Result = iota + 1
	ResultString
	ResultDyn
)

// Var is a bit set of CEL variables.
type Var uint16

// Variables.
const (
	VarRequest Var = 1 << iota
	VarSource
	VarRoute
	VarConsumer
	VarAuth
	VarNow
	VarResponse
	VarError
	VarUpstream
	VarAttempt
	VarSteps
	VarDuration
	VarAI   // declared for validation; evaluated from M3
	VarSelf // reserved for x-ruralz-validations (M2)
)

// VarsBase is request, source, route, consumer, auth and now.
const VarsBase = VarRequest | VarSource | VarRoute | VarConsumer | VarAuth | VarNow

// Names returns the x-ruralz-cel spellings of the variables in v, in
// declaration order.
func (v Var) Names() []string {
	names := [...]string{
		"request", "source", "route", "consumer", "auth", "now", "response",
		"error", "upstream", "attempt", "steps", "duration", "ai", "self",
	}
	var out []string
	for i, n := range names {
		if v&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return out
}

// ErrorRule is a place's normative runtime-error behavior.
type ErrorRule uint8

// Runtime error rules (docs/architecture/02-configuration-model.md
// "Allowed places", runtime-error column).
const (
	RuleInternalError  ErrorRule = iota + 1 // 500 RZ-RT-006, no fallthrough
	RulePolicyWhen                          // closed: the Policy runs; open: skipped
	RuleStepFails                           // optional decides; else 502 RZ-RT-015
	RuleAuthzUndecided                      // 403 RZ-AUTH-015
	RuleFailureMode                         // the Policy's failureMode
	RuleCacheBypass                         // cache bypassed, whatever failureMode
	RuleRandomEndpoint                      // weighted random Endpoint
	RuleNoRetry                             // no retry
	RuleCountFailure                        // the attempt counts as a failure
	RuleWriteEntry                          // the access log entry is written
	RuleSkipCandidate                       // M3
	RuleBadGateway                          // M4 messaging: 502
)

// Place describes one CEL place.
type Place struct {
	// ID names the place.
	ID PlaceID
	// Result is the required result type.
	Result Result
	// Vars is the union over scopes; it equals the x-ruralz-cel variables.
	Vars Var
	// Rule is the runtime-error behavior.
	Rule ErrorRule
	// Runtime is the milestone whose code evaluates the place.
	Runtime string
}

// Places returns the place table, sorted by ID.
func Places() []Place {
	base := VarsBase
	out := []Place{
		{PlaceRouteMatchWhen, ResultBool, VarRequest | VarSource | VarNow, RuleInternalError, "M1"},
		{PlacePolicyWhen, ResultBool, base | VarResponse, RulePolicyWhen, "M1"},
		{PlaceStepPathExpression, ResultString, base | VarSteps, RuleStepFails, "M1"},
		{PlaceStepWhen, ResultBool, base | VarSteps, RuleStepFails, "M1"},
		{PlaceAuthzCELRule, ResultBool, base, RuleAuthzUndecided, "M1"},
		{PlaceRateLimitKey, ResultString, base, RuleFailureMode, "M1"},
		{PlaceQuotaKey, ResultString, base, RuleFailureMode, "M1"},
		{PlaceHeadersRequestValue, ResultString, base, RuleFailureMode, "M1"},
		{PlaceHeadersResponseValue, ResultString, base | VarResponse | VarUpstream, RuleFailureMode, "M1"},
		{PlaceCacheKey, ResultString, base, RuleCacheBypass, "M1"},
		{PlaceTransformRequestBody, ResultDyn, base | VarUpstream, RuleFailureMode, "M1"},
		{PlaceTransformRequestValue, ResultString, base | VarUpstream, RuleFailureMode, "M1"},
		{PlaceTransformResponseBody, ResultDyn, base | VarResponse | VarUpstream, RuleFailureMode, "M1"},
		{PlaceTransformResponseValue, ResultString, base | VarResponse | VarUpstream, RuleFailureMode, "M1"},
		{PlaceHashKey, ResultString, base, RuleRandomEndpoint, "M1"},
		{PlaceRetryOn, ResultBool, VarRequest | VarResponse | VarError | VarAttempt | VarUpstream, RuleNoRetry, "M1"},
		{PlaceFailureWhen, ResultBool, VarRequest | VarResponse | VarError | VarUpstream, RuleCountFailure, "M1"},
		{PlaceAccessLogWhen, ResultBool, base | VarResponse | VarUpstream | VarDuration, RuleWriteEntry, "M1"},
		{PlaceSemanticCacheKey, ResultString, base | VarAI, RuleCacheBypass, "M3"},
		{PlaceCandidateWhen, ResultBool, base | VarAI, RuleSkipCandidate, "M3"},
		{PlaceMessagingKey, ResultString, base, RuleBadGateway, "M4"},
	}
	slices.SortFunc(out, func(a, b Place) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out
}

// LookupPlace returns the place with id.
func LookupPlace(id PlaceID) (Place, bool) {
	for _, p := range Places() {
		if p.ID == id {
			return p, true
		}
	}
	return Place{}, false
}

// Site is one occurrence of a place; it refines the environment by
// attachment (docs: 03-cel requirements 10 to 15). The configuration
// pipeline computes sites after effective Filter Chains (stage J); the
// snapshot compiler passes the same sites to Builder.Compile.
type Site struct {
	// Place is the field's place.
	Place PlaceID
	// Scope is the attachment scope of a Policy field; ScopeNone otherwise.
	Scope phase.Scope
	// PolicyType is set for Policy fields.
	PolicyType v1alpha1.PolicyType
	// FirstPhase is the Policy's first Phase at this attachment
	// (Policy.spec.when only; response Phases declare response).
	FirstPhase phase.Phase
	// GatesBody is true when the Policy gates the request body at this
	// attachment, so Policy.spec.when may select request.body.
	GatesBody bool
	// Mode is the composition mode (composition places).
	Mode v1alpha1.CompositionMode
	// EarlierSteps are the names of earlier steps in list order.
	EarlierSteps []string
}

// Issue is one compile finding: RZ-CFG-014 or RZ-CFG-015.
type Issue struct {
	// Code is "RZ-CFG-014" or "RZ-CFG-015".
	Code string
	// Offset is the byte offset in the expression; -1 when not positional.
	Offset int
	// Line and Column are 1-based within the expression; 0 when unknown.
	Line, Column int
	// Message is deterministic and prefixed "CEL <place>: ".
	Message string
	// Hint is optional.
	Hint string
}

// CostRange is a static cost estimate at nominal sizes.
type CostRange struct{ Min, Max uint64 }

// Refs are static facts about what a program reads.
type Refs struct {
	// Vars are the variables referenced.
	Vars Var
	// RequestBody is true when request.body is selected (a body gate).
	RequestBody bool
	// ResponseBody is true when response.body is selected.
	ResponseBody bool
	// Steps are constant steps keys, sorted and unique.
	Steps []string
	// StepBodies are constant keys whose .body is selected (step gates).
	StepBodies []string
	// AnyStep is true for a non-constant steps key.
	AnyStep bool
}

// Program is an immutable compiled expression, safe for concurrent
// evaluation. Evaluation reads v and never writes it; ctx supplies the
// request deadline used by cost-tracked programs with comprehensions.
type Program interface {
	// Place returns the place the program was compiled for.
	Place() PlaceID
	// Source returns the expression text.
	Source() string
	// Refs returns what the program reads.
	Refs() Refs
	// Cost returns the static estimate at nominal sizes.
	Cost() CostRange
	// EvalBool evaluates a bool place; a non-bool result is KindResultType.
	EvalBool(ctx context.Context, v *Vars) (bool, error)
	// EvalString evaluates a string place.
	EvalString(ctx context.Context, v *Vars) (string, error)
	// EvalValue evaluates a dyn place (transform bodies).
	EvalValue(ctx context.Context, v *Vars) (Value, error)
}

// ProgramSet is the immutable program set of one snapshot, passed as the
// previous set to the next snapshot's Builder so identical (environment,
// source) pairs reuse their Program across Revisions.
type ProgramSet interface {
	// Len returns the number of programs.
	Len() int
}

// Builder collects the programs of one snapshot. Compile is safe for
// concurrent use by the loader's workers; Build is called once after.
type Builder interface {
	// Compile returns a program, or nil and at least one error Issue.
	Compile(site Site, src string) (Program, []Issue)
	// Build returns the set; the Builder is spent.
	Build() ProgramSet
}

// Compiler owns the CEL environments. It is safe for concurrent use and
// built once per process.
type Compiler interface {
	// Check compiles and cost-checks src at site without keeping a program
	// (validation in every binary). An empty src is absent: no Issue.
	Check(site Site, src string) []Issue
	// ReferencesRequestBody reports whether an authz.cel rule selects
	// request.body, which moves it to onRequestBody.
	ReferencesRequestBody(src string) (bool, error)
	// NewBuilder starts a snapshot's program set; prev may be nil.
	NewBuilder(prev ProgramSet) Builder
	// Default returns the compiled default of PlaceRetryOn or
	// PlaceFailureWhen, used when the field is absent.
	Default(p PlaceID) Program
	// DecodeJSON decodes one JSON document into a Value for CEL (bodies,
	// claims). built is the size charged to limits.maxBufferedBytes; past
	// budget it returns ErrTooLarge.
	DecodeJSON(data []byte, budget int64) (v Value, built int64, err error)
}

// Default CEL sources of Upstream.spec.retries.retryOn and
// circuitBreaker.failureWhen when the field is absent (runtime rule,
// OQ-traffic-management-and-resilience-6).
const (
	DefaultRetryOn     = `error != null ? (error.kind == "connect" || (error.kind == "reset" && request.method in ["GET", "HEAD", "OPTIONS", "PUT", "DELETE"])) : (response.status == 503 && request.method in ["GET", "HEAD", "OPTIONS", "PUT", "DELETE"])`
	DefaultFailureWhen = `error != null || response.status in [502, 503, 504]`
)

// Value is an opaque decoded value owned by the CEL implementation: a JSON
// body, JWT claims or a dyn result. Maps iterate keys in ascending byte
// order.
type Value interface {
	// IsNull reports JSON null.
	IsNull() bool
	// AppendBody writes a transform body result: a map or list as JSON with
	// sorted keys, a top-level string as its UTF-8 bytes, bytes raw, other
	// scalars as JSON text.
	AppendBody(dst []byte) ([]byte, error)
	// Native converts to map[string]any, []any, string, json.Number, bool
	// or nil, for code that edits documents (transforms).
	Native() (any, error)
}

// ErrTooLarge is returned by DecodeJSON past its budget.
var ErrTooLarge = errors.New("expr: decoded value over budget")

// ErrorKind classifies a runtime error.
type ErrorKind uint8

// Runtime error kinds.
const (
	KindCostLimit ErrorKind = iota + 1
	KindDeadline
	KindNull
	KindNoSuchKey
	KindNoSuchOverload
	KindConversion
	KindArithmetic
	KindResultType
	KindBody
	KindOther
)

// String returns the kind name used as span error.type.
func (k ErrorKind) String() string {
	switch k {
	case KindCostLimit:
		return "cost_limit"
	case KindDeadline:
		return "deadline"
	case KindNull:
		return "null"
	case KindNoSuchKey:
		return "no_such_key"
	case KindNoSuchOverload:
		return "no_such_overload"
	case KindConversion:
		return "conversion"
	case KindArithmetic:
		return "arithmetic"
	case KindResultType:
		return "result_type"
	case KindBody:
		return "body"
	default:
		return "other"
	}
}

// EvalError is a runtime error. Error never contains request data; the
// library text is available only through Detail, for debug logs and tests.
type EvalError struct {
	// Place is where the program ran.
	Place PlaceID
	// Kind classifies the error.
	Kind   ErrorKind
	detail string
}

// NewEvalError returns an EvalError; detail may contain request data.
func NewEvalError(place PlaceID, kind ErrorKind, detail string) *EvalError {
	return &EvalError{Place: place, Kind: kind, detail: detail}
}

// Error returns "cel: <place>: <kind>".
func (e *EvalError) Error() string { return "cel: " + string(e.Place) + ": " + e.Kind.String() }

// Detail returns the library text; never send it to a client or a span.
func (e *EvalError) Detail() string { return e.detail }

// Param is one path template capture.
type Param struct{ Name, Value string }

// Request is the request view. Fields are set by the data plane; Header is
// the live canonical header map (read-only while referenced by an
// evaluation).
type Request struct {
	// Method is the method as received.
	Method string
	// Scheme is "http" or "https".
	Scheme string
	// Host is lowercased without port (the Router's normalized host).
	Host string
	// Path is the one normalized path.
	Path string
	// PathParams are template captures in template order.
	PathParams []Param
	// Header holds request headers; CEL reads them lowercased and joined.
	Header http.Header
	// Body is the decoded JSON body; nil when not available.
	Body Value

	rawQuery string
	query    atomic.Pointer[map[string]string]
}

// SetRawQuery sets the raw query and drops the parsed cache. Call it only
// while no evaluation reads the view.
func (r *Request) SetRawQuery(q string) {
	r.rawQuery = q
	r.query.Store(nil)
}

// RawQuery returns the raw query.
func (r *Request) RawQuery() string { return r.rawQuery }

// Query returns request.query: parameters percent-decoded, a repeated
// parameter joined by ",". It is parsed once, lazily, and is safe for
// concurrent readers.
func (r *Request) Query() map[string]string {
	if m := r.query.Load(); m != nil {
		return *m
	}
	vals, _ := url.ParseQuery(r.rawQuery)
	m := make(map[string]string, len(vals))
	for k, vs := range vals {
		m[k] = strings.Join(vs, ",")
	}
	r.query.CompareAndSwap(nil, &m)
	return *r.query.Load()
}

// Reset clears r for reuse from a pool.
func (r *Request) Reset() {
	h := r.Header
	clear(h)
	*r = Request{Header: h, PathParams: r.PathParams[:0]}
}

// Source is the client address view.
type Source struct {
	// IP is source.ip after trusted-proxy handling, IPv4-mapped unmapped.
	IP netip.Addr
	// Port is the peer port; 0 when taken from a forwarding header.
	Port int
	// TLSVersion is tls.VersionTLS12 or VersionTLS13; 0 on cleartext.
	TLSVersion uint16
	// ClientCertSubject is the RFC 4514 subject verified by this Route's
	// auth.mtls, else "".
	ClientCertSubject string
}

// Route is the route variable, built once per snapshot.
type Route struct {
	// Name is metadata.name.
	Name string
	// Labels are metadata.labels.
	Labels map[string]string
	// Prepared is owned by the CEL implementation: converted values cached
	// once per snapshot by Compiler preparation, never set by other code.
	Prepared any
}

// Consumer is the compiled, read-only Consumer shared by CEL, quota keys,
// cache partitions and logs; built once per snapshot.
type Consumer struct {
	// Name is metadata.name.
	Name string
	// Tier is spec.tier, "" when unset.
	Tier string
	// Tags is spec.tags, sorted.
	Tags []string
	// Labels are metadata.labels.
	Labels map[string]string
	// Quotas are the names of spec.quotas, sorted.
	Quotas []string
	// Prepared is owned by the CEL implementation, like Route.Prepared.
	Prepared any
}

// Auth is the auth variable.
type Auth struct {
	// Method is "jwt", "api-key", "basic" or "mtls".
	Method string
	// Claims is the verified JWT payload; an empty map for other methods.
	Claims Value
}

// Response is the response view.
type Response struct {
	// Status is the HTTP status.
	Status int
	// Header holds response headers.
	Header http.Header
	// Body is the decoded JSON body; nil when not available.
	Body Value
}

// AttemptError is the error variable: kind is connect, timeout, reset or tls.
type AttemptError struct{ Kind string }

// Upstream is the upstream variable: the Upstream name and the selected
// Endpoint address host:port.
type Upstream struct{ Name, Endpoint string }

// Step is one completed composition step.
type Step struct {
	// Status is the step's response status.
	Status int
	// Header holds the step's response headers.
	Header http.Header
	// Body is the decoded body when the step is a gate, else nil.
	Body Value
}

// Steps holds completed steps in list order; skipped, failed-optional and
// not-yet-run steps are absent.
type Steps struct {
	names []string
	steps []Step
}

// Add records a completed step.
func (s *Steps) Add(name string, st Step) {
	s.names = append(s.names, name)
	s.steps = append(s.steps, st)
}

// Get returns the named step.
func (s *Steps) Get(name string) (Step, bool) {
	i := slices.Index(s.names, name)
	if i < 0 {
		return Step{}, false
	}
	return s.steps[i], true
}

// Names returns the completed step names in list order.
func (s *Steps) Names() []string { return s.names }

// Reset clears s for reuse.
func (s *Steps) Reset() { s.names, s.steps = s.names[:0], s.steps[:0] }

// AI is the ai variable (declared for validation; evaluated from M3).
type AI struct {
	Model                string
	EstimatedInputTokens int64
	MaxOutputTokens      int64
	Stream               bool
}

// Vars is the activation of one evaluation point. A nil pointer is CEL
// null. One Vars belongs to one goroutine; an upstream leg evaluates
// against its own copy with the leg fields replaced.
type Vars struct {
	Request  *Request
	Source   *Source
	Route    *Route
	Consumer *Consumer
	Auth     *Auth
	Response *Response
	Error    *AttemptError
	Upstream *Upstream
	Attempt  int
	Steps    *Steps
	AI       *AI
	// Now is the request start time, UTC.
	Now time.Time
	// Duration is the total request time (onLog and accessLog.when).
	Duration time.Duration
}

// Reset clears v for reuse from a pool.
func (v *Vars) Reset() { *v = Vars{} }

// JoinedHeader returns a request or response header as CEL sees it: the
// lookup is case-insensitive and repeated field lines are joined by ", ".
func JoinedHeader(h http.Header, name string) (string, bool) {
	vs, ok := h[http.CanonicalHeaderKey(name)]
	if !ok {
		for k, v := range h {
			if strings.EqualFold(k, name) {
				vs, ok = v, true
				break
			}
		}
	}
	if !ok {
		return "", false
	}
	if len(vs) == 1 {
		return vs[0], true
	}
	return strings.Join(vs, ", "), true
}
