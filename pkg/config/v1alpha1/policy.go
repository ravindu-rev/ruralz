// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "encoding/json"

// PolicyType is a registered Policy type (foundation pack section 10).
type PolicyType string

// Policy types. Each has a config type below, marked with its policyType.
const (
	// PolicyTypeAuthJWT validates JWTs, OpenID Connect and OAuth2 tokens. Planned (M1).
	PolicyTypeAuthJWT PolicyType = "auth.jwt"
	// PolicyTypeAuthAPIKey matches API keys against Consumer apiKeys by hash. Planned (M1).
	PolicyTypeAuthAPIKey PolicyType = "auth.api-key" //nolint:gosec // A Policy type name, not a credential.
	// PolicyTypeAuthBasic checks Authorization: Basic against Consumer credentials. Planned (M1).
	PolicyTypeAuthBasic PolicyType = "auth.basic"
	// PolicyTypeAuthMTLS authenticates client certificates. Planned (M1).
	PolicyTypeAuthMTLS PolicyType = "auth.mtls"
	// PolicyTypeAuthzCEL authorizes with a CEL rule. Planned (M1).
	PolicyTypeAuthzCEL PolicyType = "authz.cel"
	// PolicyTypeAuthzOPA authorizes with OPA. Planned (M2).
	PolicyTypeAuthzOPA PolicyType = "authz.opa"
	// PolicyTypeAuthzCedar authorizes with Cedar. Planned (M2).
	PolicyTypeAuthzCedar PolicyType = "authz.cedar"
	// PolicyTypeAuthzIP allows or denies by client address. Planned (M1).
	PolicyTypeAuthzIP PolicyType = "authz.ip"
	// PolicyTypeAuthzGeoIP allows or denies by client country. Planned (M2).
	PolicyTypeAuthzGeoIP PolicyType = "authz.geoip"
	// PolicyTypeRateLimit is a Rate Limit. Planned (M1).
	PolicyTypeRateLimit PolicyType = "ratelimit"
	// PolicyTypeQuota enforces a Consumer quota in requests. Planned (M1).
	PolicyTypeQuota PolicyType = "quota"
	// PolicyTypeValidationJSONSchema validates request bodies. Planned (M1).
	PolicyTypeValidationJSONSchema PolicyType = "validation.json-schema"
	// PolicyTypeCORS handles CORS. Planned (M1).
	PolicyTypeCORS PolicyType = "cors"
	// PolicyTypeCache is the Response Cache. Planned (M1).
	PolicyTypeCache PolicyType = "cache"
	// PolicyTypeHeaders sets request and response headers. Planned (M1).
	PolicyTypeHeaders PolicyType = "headers"
	// PolicyTypeTransformRequest transforms the request. Planned (M1).
	PolicyTypeTransformRequest PolicyType = "transform.request"
	// PolicyTypeTransformResponse transforms the response. Planned (M1).
	PolicyTypeTransformResponse PolicyType = "transform.response"
	// PolicyTypeAuthUpstreamOAuth2 obtains upstream OAuth2 tokens by client credentials. Planned (M1).
	PolicyTypeAuthUpstreamOAuth2 PolicyType = "auth.upstream-oauth2"
	// PolicyTypeAuthUpstreamSigV4 signs upstream requests with AWS SigV4. Planned (M2).
	PolicyTypeAuthUpstreamSigV4 PolicyType = "auth.upstream-sigv4"
	// PolicyTypeAITokenBudget enforces a Token Budget. Planned (M3).
	PolicyTypeAITokenBudget PolicyType = "ai.token-budget"
	// PolicyTypeAISemanticCache is the Semantic Cache. Planned (M3).
	PolicyTypeAISemanticCache PolicyType = "ai.semantic-cache"
	// PolicyTypeAIGuardrail runs guardrail hooks. Planned (M3).
	PolicyTypeAIGuardrail PolicyType = "ai.guardrail"
	// PolicyTypePlugin runs a WASM Plugin. Planned (M2).
	PolicyTypePlugin PolicyType = "plugin"
)

// FailureMode is what a Policy does when it cannot decide.
type FailureMode string

// Failure modes.
const (
	// FailureModeOpen skips the Policy.
	FailureModeOpen FailureMode = "open"
	// FailureModeClosed rejects the request or ends the stream.
	FailureModeClosed FailureMode = "closed"
)

// FilterClass is a position within a Phase.
type FilterClass string

// Filter classes.
const (
	// FilterClassCORS runs cors Policies.
	FilterClassCORS FilterClass = "cors"
	// FilterClassAuth runs authentication.
	FilterClassAuth FilterClass = "auth"
	// FilterClassAuthz runs authorization.
	FilterClassAuthz FilterClass = "authz"
	// FilterClassAdmission runs Rate Limits, Quotas and Token Budgets.
	FilterClassAdmission FilterClass = "admission"
	// FilterClassValidation runs validation and guardrails.
	FilterClassValidation FilterClass = "validation"
	// FilterClassCache runs the Response Cache and Semantic Cache.
	FilterClassCache FilterClass = "cache"
	// FilterClassUpstreamAuth runs upstream authentication.
	FilterClassUpstreamAuth FilterClass = "upstream-auth"
	// FilterClassTransform runs headers and transform Policies.
	FilterClassTransform FilterClass = "transform"
	// FilterClassCustom runs Plugins without another class.
	FilterClassCustom FilterClass = "custom"
)

// PolicySpec is the spec of a Policy.
type PolicySpec struct {
	// Type is the registered Policy type; it selects the config schema.
	// +ruralz:required
	Type PolicyType `json:"type"`
	// Slot is the precedence key; default from the type registry.
	Slot string `json:"slot,omitempty"`
	// Overridable, when false on a Gateway Policy, forbids Route replace or exclude.
	// +ruralz:default=true
	Overridable *bool `json:"overridable,omitempty"`
	// FailureMode is open or closed; default from the type registry. Security types allow closed only (RZ-CFG-029).
	FailureMode *FailureMode `json:"failureMode,omitempty"`
	// StateStoreTimeout bounds State Store calls; default Gateway spec.stateStore.timeout.
	StateStoreTimeout *Duration `json:"stateStoreTimeout,omitempty"`
	// When skips the Policy when false.
	// +ruralz:cel=request,source,route,consumer,auth,now,response:bool
	When string `json:"when,omitempty"`
	// Plugin is the metadata.name of a Plugin; required when type is plugin.
	// +ruralz:ref=Plugin
	Plugin string `json:"plugin,omitempty"`
	// FilterClass places a plugin Policy in the Filter Chain; plugin only, default custom.
	FilterClass *FilterClass `json:"filterClass,omitempty"`
	// Config is validated by the schema registered for type.
	// +ruralz:required
	Config json.RawMessage `json:"config"`
}
