// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// The config of each Policy type. A config whose fields the Configuration
// model registers as its feature document authored them is closed. A config
// of which the Configuration model fixes only the fields it uses, or none,
// is open (+ruralz:open): its feature document may author more fields, which
// stay unchecked until they are registered (OQ-configuration-model-19).

// AuthJWTConfig is the config of an auth.jwt Policy.
// +ruralz:policyType=auth.jwt
// +ruralz:open
type AuthJWTConfig struct {
	// Issuers are the accepted token issuers; the token's iss selects exactly one.
	// +ruralz:required
	// +ruralz:minItems=1
	// +ruralz:list=map,key=issuer
	Issuers []JWTIssuer `json:"issuers"`
}

// JWTIssuer is one accepted token issuer.
// +ruralz:open
type JWTIssuer struct {
	// Issuer is the expected iss claim.
	// +ruralz:required
	Issuer string `json:"issuer"`
	// JWKSURL is the issuer's JWKS endpoint; it MUST be https (RZ-CFG-037).
	// +ruralz:required
	JWKSURL string `json:"jwksUrl"`
	// Audiences are the accepted aud values.
	// +ruralz:required
	// +ruralz:minItems=1
	// +ruralz:list=set
	Audiences []string `json:"audiences"`
}

// AuthAPIKeyConfig is the config of an auth.api-key Policy.
// +ruralz:policyType=auth.api-key
// +ruralz:open
type AuthAPIKeyConfig struct {
	// Header carries the API key; it is matched against Consumer apiKeys by hash.
	Header string `json:"header,omitempty"`
}

// AuthBasicConfig is the config of an auth.basic Policy; it has no fields
// and reads Authorization: Basic.
// +ruralz:policyType=auth.basic
type AuthBasicConfig struct{}

// AuthMTLSConfig is the config of an auth.mtls Policy.
// +ruralz:policyType=auth.mtls
type AuthMTLSConfig struct {
	// CACertificate verifies client certificates.
	// +ruralz:required
	// +ruralz:secret
	CACertificate SecretValue `json:"caCertificate"`
	// Subjects are the accepted certificate identities.
	// +ruralz:list=set
	Subjects []CertificateSubject `json:"subjects,omitempty"`
	// CRL is a certificate revocation list.
	// +ruralz:secret
	CRL *SecretValue `json:"crl,omitempty"`
}

// CertificateSubject names a certificate by exactly one of subject or
// uriSan.
// +ruralz:exactlyOneOf=subject,uriSan
type CertificateSubject struct {
	// Subject is the certificate subject distinguished name.
	Subject string `json:"subject,omitempty"`
	// URISAN is a URI subject alternative name, such as a SPIFFE ID.
	URISAN string `json:"uriSan,omitempty"`
}

// AuthzCELConfig is the config of an authz.cel Policy.
// +ruralz:policyType=authz.cel
// +ruralz:open
type AuthzCELConfig struct {
	// Rule allows the request when true.
	// +ruralz:required
	// +ruralz:cel=request,source,route,consumer,auth,now:bool
	Rule string `json:"rule"`
}

// AuthzOPAConfig is the config of an authz.opa Policy; its fields await
// OQ-security-and-identity-13.
// +ruralz:policyType=authz.opa
// +ruralz:open
type AuthzOPAConfig struct{}

// AuthzCedarConfig is the config of an authz.cedar Policy; its fields await
// OQ-security-and-identity-13.
// +ruralz:policyType=authz.cedar
// +ruralz:open
type AuthzCedarConfig struct{}

// AuthzIPConfig is the config of an authz.ip Policy. Both lists empty is
// RZ-CFG-005.
// +ruralz:policyType=authz.ip
// +ruralz:atLeastOneOf=allow,deny
type AuthzIPConfig struct {
	// Allow lists CIDRs or bare addresses to allow.
	// +ruralz:list=set
	Allow []string `json:"allow,omitempty"`
	// Deny lists CIDRs or bare addresses to deny.
	// +ruralz:list=set
	Deny []string `json:"deny,omitempty"`
}

// AuthzGeoIPConfig is the config of an authz.geoip Policy. Both lists empty
// is RZ-CFG-005.
// +ruralz:policyType=authz.geoip
// +ruralz:atLeastOneOf=allow,deny
type AuthzGeoIPConfig struct {
	// Allow lists ISO 3166-1 alpha-2 country codes to allow.
	// +ruralz:list=set
	Allow []CountryCode `json:"allow,omitempty"`
	// Deny lists ISO 3166-1 alpha-2 country codes to deny.
	// +ruralz:list=set
	Deny []CountryCode `json:"deny,omitempty"`
}

// CountryCode is an ISO 3166-1 alpha-2 country code.
// +ruralz:pattern=^[A-Z]{2}$
type CountryCode string

// RateLimitConfig is the config of a ratelimit Policy.
// +ruralz:policyType=ratelimit
// +ruralz:open
type RateLimitConfig struct {
	// Key partitions the limit, for example per Consumer.
	// +ruralz:cel=request,source,route,consumer,auth,now:string
	Key string `json:"key,omitempty"`
	// Limits are the enforced windows.
	// +ruralz:required
	// +ruralz:minItems=1
	// +ruralz:list=atomic
	Limits []RateLimit `json:"limits"`
	// LocalOnly keeps the limit in the Node; it waits for OQ-scalability-and-distributed-state-11.
	// +ruralz:default=false
	LocalOnly *bool `json:"localOnly,omitempty"`
}

// RateLimit is one limit window.
// +ruralz:open
type RateLimit struct {
	// Requests is the number of requests allowed per window.
	// +ruralz:required
	// +ruralz:minimum=1
	Requests int64 `json:"requests"`
	// Window is the window length.
	// +ruralz:required
	Window Duration `json:"window"`
	// PerNodeCeiling caps one Node's share, 1 to requests; unset means derived.
	// +ruralz:minimum=1
	PerNodeCeiling *int64 `json:"perNodeCeiling,omitempty"`
	// Burst is 0 to requests; default requests.
	// +ruralz:minimum=0
	Burst *int64 `json:"burst,omitempty"`
}

// QuotaConfig is the config of a quota Policy.
// +ruralz:policyType=quota
// +ruralz:open
type QuotaConfig struct {
	// ConsumerQuota names a Consumer quota with unit requests.
	// +ruralz:required
	ConsumerQuota string `json:"consumerQuota"`
	// Key partitions the quota.
	// +ruralz:default=consumer.name
	// +ruralz:cel=request,source,route,consumer,auth,now:string
	Key *string `json:"key,omitempty"`
}

// ValidationJSONSchemaConfig is the config of a validation.json-schema
// Policy; its fields are not yet registered.
// +ruralz:policyType=validation.json-schema
// +ruralz:open
type ValidationJSONSchemaConfig struct{}

// CORSConfig is the config of a cors Policy.
// +ruralz:policyType=cors
// +ruralz:open
type CORSConfig struct {
	// AllowOrigins are the allowed origins.
	// +ruralz:list=set
	AllowOrigins []string `json:"allowOrigins,omitempty"`
	// AllowMethods are the allowed methods.
	// +ruralz:list=set
	AllowMethods []string `json:"allowMethods,omitempty"`
}

// CacheConfig is the config of a cache (Response Cache) Policy.
// +ruralz:policyType=cache
// +ruralz:open
type CacheConfig struct {
	// Key partitions entries, for example per Consumer.
	// +ruralz:cel=request,source,route,consumer,auth,now:string
	Key string `json:"key,omitempty"`
}

// HeadersConfig is the config of a headers Policy.
// +ruralz:policyType=headers
// +ruralz:open
type HeadersConfig struct {
	// Request sets request headers.
	Request *HeaderRequestOps `json:"request,omitempty"`
	// Response sets response headers.
	Response *HeaderResponseOps `json:"response,omitempty"`
}

// HeaderRequestOps are request header operations.
// +ruralz:open
type HeaderRequestOps struct {
	// Set sets request headers.
	// +ruralz:list=map,key=name
	Set []HeaderRequestSet `json:"set,omitempty"`
}

// HeaderRequestSet sets one request header from exactly one of value or
// valueExpression.
// +ruralz:exactlyOneOf=value,valueExpression
// +ruralz:open
type HeaderRequestSet struct {
	// Name is the header name.
	// +ruralz:required
	Name string `json:"name"`
	// Value is a literal value.
	Value string `json:"value,omitempty"`
	// ValueExpression computes the value.
	// +ruralz:cel=request,source,route,consumer,auth,now:string
	ValueExpression string `json:"valueExpression,omitempty"`
}

// HeaderResponseOps are response header operations.
// +ruralz:open
type HeaderResponseOps struct {
	// Set sets response headers.
	// +ruralz:list=map,key=name
	Set []HeaderResponseSet `json:"set,omitempty"`
}

// HeaderResponseSet sets one response header from exactly one of value or
// valueExpression.
// +ruralz:exactlyOneOf=value,valueExpression
// +ruralz:open
type HeaderResponseSet struct {
	// Name is the header name.
	// +ruralz:required
	Name string `json:"name"`
	// Value is a literal value.
	Value string `json:"value,omitempty"`
	// ValueExpression computes the value.
	// +ruralz:cel=request,source,route,consumer,auth,now,response,upstream:string
	ValueExpression string `json:"valueExpression,omitempty"`
}

// TransformRequestTarget is where a transform.request operation applies.
type TransformRequestTarget string

// Request transform targets.
const (
	// TransformRequestTargetHeader is a request header.
	TransformRequestTargetHeader TransformRequestTarget = "header"
	// TransformRequestTargetQuery is a query parameter.
	TransformRequestTargetQuery TransformRequestTarget = "query"
	// TransformRequestTargetBody is a JSON body field.
	TransformRequestTargetBody TransformRequestTarget = "body"
)

// TransformResponseTarget is where a transform.response operation applies.
type TransformResponseTarget string

// Response transform targets.
const (
	// TransformResponseTargetHeader is a response header.
	TransformResponseTargetHeader TransformResponseTarget = "header"
	// TransformResponseTargetBody is a JSON body field.
	TransformResponseTargetBody TransformResponseTarget = "body"
)

// ArrayOpKind is a JSON array operation.
type ArrayOpKind string

// Array operations.
const (
	// ArrayOpMove moves an element.
	ArrayOpMove ArrayOpKind = "move"
	// ArrayOpAppend appends an element.
	ArrayOpAppend ArrayOpKind = "append"
	// ArrayOpDelete deletes an element.
	ArrayOpDelete ArrayOpKind = "delete"
)

// TransformRequestConfig is the config of a transform.request Policy. More
// than 32 entries across set, remove, arrayOps and replace is RZ-CFG-005.
// +ruralz:policyType=transform.request
type TransformRequestConfig struct {
	// Body computes the new body; a map or list is written as JSON, a string as its bytes.
	// +ruralz:cel=request,source,route,consumer,auth,now,upstream:dyn
	Body string `json:"body,omitempty"`
	// ContentType is the content type of a computed body.
	// +ruralz:default=application/json
	ContentType *string `json:"contentType,omitempty"`
	// Set sets headers, query parameters or body fields.
	// +ruralz:list=atomic
	Set []TransformRequestSet `json:"set,omitempty"`
	// Remove removes headers, query parameters or body fields.
	// +ruralz:list=atomic
	Remove []TransformRequestRemove `json:"remove,omitempty"`
	// ArrayOps edit JSON arrays.
	// +ruralz:list=atomic
	ArrayOps []ArrayOp `json:"arrayOps,omitempty"`
	// Replace rewrites string values with RE2 patterns.
	// +ruralz:list=atomic
	Replace []Replace `json:"replace,omitempty"`
}

// TransformRequestSet sets one request value.
type TransformRequestSet struct {
	// Target is header, query or body.
	// +ruralz:required
	Target TransformRequestTarget `json:"target"`
	// Name is the header, parameter or body path.
	// +ruralz:required
	Name string `json:"name"`
	// ValueExpression computes the value.
	// +ruralz:required
	// +ruralz:cel=request,source,route,consumer,auth,now,upstream:string
	ValueExpression string `json:"valueExpression"`
}

// TransformRequestRemove removes one request value.
type TransformRequestRemove struct {
	// Target is header, query or body.
	// +ruralz:required
	Target TransformRequestTarget `json:"target"`
	// Name is the header, parameter or body path.
	// +ruralz:required
	Name string `json:"name"`
}

// TransformResponseConfig is the config of a transform.response Policy.
// More than 32 entries across set, remove, arrayOps and replace is
// RZ-CFG-005.
// +ruralz:policyType=transform.response
type TransformResponseConfig struct {
	// Body computes the new body; a map or list is written as JSON, a string as its bytes.
	// +ruralz:cel=request,source,route,consumer,auth,now,response,upstream:dyn
	Body string `json:"body,omitempty"`
	// ContentType is the content type of a computed body.
	// +ruralz:default=application/json
	ContentType *string `json:"contentType,omitempty"`
	// Set sets headers or body fields.
	// +ruralz:list=atomic
	Set []TransformResponseSet `json:"set,omitempty"`
	// Remove removes headers or body fields.
	// +ruralz:list=atomic
	Remove []TransformResponseRemove `json:"remove,omitempty"`
	// ArrayOps edit JSON arrays.
	// +ruralz:list=atomic
	ArrayOps []ArrayOp `json:"arrayOps,omitempty"`
	// Replace rewrites string values with RE2 patterns.
	// +ruralz:list=atomic
	Replace []Replace `json:"replace,omitempty"`
}

// TransformResponseSet sets one response value.
type TransformResponseSet struct {
	// Target is header or body.
	// +ruralz:required
	Target TransformResponseTarget `json:"target"`
	// Name is the header or body path.
	// +ruralz:required
	Name string `json:"name"`
	// ValueExpression computes the value.
	// +ruralz:required
	// +ruralz:cel=request,source,route,consumer,auth,now,response,upstream:string
	ValueExpression string `json:"valueExpression"`
}

// TransformResponseRemove removes one response value.
type TransformResponseRemove struct {
	// Target is header or body.
	// +ruralz:required
	Target TransformResponseTarget `json:"target"`
	// Name is the header or body path.
	// +ruralz:required
	Name string `json:"name"`
}

// ArrayOp is one JSON array operation.
type ArrayOp struct {
	// Op is move, append or delete.
	// +ruralz:required
	Op ArrayOpKind `json:"op"`
	// From is the source path.
	From string `json:"from,omitempty"`
	// To is the destination path.
	To string `json:"to,omitempty"`
}

// Replace rewrites a string value. A pattern over 1 KiB or not valid RE2 is
// RZ-CFG-005.
type Replace struct {
	// Path selects the value.
	// +ruralz:required
	Path string `json:"path"`
	// Pattern is an RE2 pattern, or a literal when literal is true.
	// +ruralz:required
	// +ruralz:maxLength=1024
	Pattern string `json:"pattern"`
	// Replacement is the replacement text.
	Replacement string `json:"replacement,omitempty"`
	// Literal treats pattern as literal text.
	Literal *bool `json:"literal,omitempty"`
}

// AuthUpstreamOAuth2Config is the config of an auth.upstream-oauth2 Policy.
// +ruralz:policyType=auth.upstream-oauth2
// +ruralz:open
type AuthUpstreamOAuth2Config struct {
	// TokenURL is the token endpoint; it MUST be https (RZ-CFG-037).
	// +ruralz:required
	TokenURL string `json:"tokenUrl"`
	// ClientID is the OAuth2 client identifier.
	// +ruralz:required
	ClientID string `json:"clientId"`
	// ClientSecret is the OAuth2 client secret.
	// +ruralz:required
	// +ruralz:secret
	ClientSecret SecretValue `json:"clientSecret"`
	// Scopes are the requested scopes.
	// +ruralz:list=set
	Scopes []string `json:"scopes,omitempty"`
	// Timeout bounds a token fetch.
	// +ruralz:default=2s
	Timeout *Duration `json:"timeout,omitempty"`
}

// SigV4Payload selects whether the payload is signed.
type SigV4Payload string

// SigV4 payload modes.
const (
	// SigV4PayloadSigned signs the payload hash.
	SigV4PayloadSigned SigV4Payload = "signed"
	// SigV4PayloadUnsigned sends UNSIGNED-PAYLOAD.
	SigV4PayloadUnsigned SigV4Payload = "unsigned"
)

// AuthUpstreamSigV4Config is the config of an auth.upstream-sigv4 Policy.
// +ruralz:policyType=auth.upstream-sigv4
type AuthUpstreamSigV4Config struct {
	// Region is the AWS region.
	// +ruralz:required
	Region string `json:"region"`
	// Service is the AWS service name.
	// +ruralz:required
	Service string `json:"service"`
	// Payload is signed or unsigned.
	// +ruralz:default=signed
	Payload *SigV4Payload `json:"payload,omitempty"`
}

// AITokenBudgetConfig is the config of an ai.token-budget Policy.
// +ruralz:policyType=ai.token-budget
// +ruralz:open
type AITokenBudgetConfig struct {
	// ConsumerQuota names a Consumer quota with unit tokens.
	// +ruralz:required
	ConsumerQuota string `json:"consumerQuota"`
}

// AISemanticCacheConfig is the config of an ai.semantic-cache Policy.
// +ruralz:policyType=ai.semantic-cache
// +ruralz:open
type AISemanticCacheConfig struct {
	// Key partitions entries, for example per Consumer.
	// +ruralz:cel=request,source,route,consumer,auth,now,ai:string
	Key string `json:"key,omitempty"`
}

// AIGuardrailConfig is the config of an ai.guardrail Policy; its fields
// await OQ-ai-llm-gateway-4.
// +ruralz:policyType=ai.guardrail
// +ruralz:open
type AIGuardrailConfig struct{}

// PluginConfig is the config of a plugin Policy: whatever the Plugin's
// configSchema defines, checked against it at validation.
// +ruralz:policyType=plugin
type PluginConfig map[string]any
