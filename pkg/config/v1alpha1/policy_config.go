// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// The config of each Policy type. A config whose fields the Configuration
// model registers as its feature document authored them is closed. A config
// of which the Configuration model fixes only the fields it uses, or none,
// is open (+ruralz:open): its feature document may author more fields, which
// stay unchecked until they are registered.

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
	// Header names the request header that carries the API key, matched case-insensitively; the key is matched against Consumer apiKeys by hash.
	// +ruralz:default=x-api-key
	Header *string `json:"header,omitempty"`
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
	// +ruralz:impact=security
	CACertificate SecretValue `json:"caCertificate"`
	// Subjects are the accepted certificate identities.
	// +ruralz:list=set
	Subjects []CertificateSubject `json:"subjects,omitempty"`
	// CRL is a certificate revocation list.
	// +ruralz:secret
	// +ruralz:impact=security
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
	// Rule allows the request when true; it is not empty.
	// +ruralz:required
	// +ruralz:minLength=1
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

// RateLimitConfig is the config of a ratelimit Policy. More than 8 limits,
// two limits with equal requests and window, a window below 1ms or not in
// whole milliseconds, a window divided by requests below 1µs, and a
// perNodeCeiling or burst above requests are RZ-CFG-005.
// +ruralz:policyType=ratelimit
type RateLimitConfig struct {
	// Key partitions the limit, for example per Consumer; absent, or an empty result, puts every request of the Policy in one partition.
	// +ruralz:cel=request,source,route,consumer,auth,now:string
	Key string `json:"key,omitempty"`
	// Limits are the enforced windows, 1 to 8.
	// +ruralz:required
	// +ruralz:minItems=1
	// +ruralz:list=atomic
	Limits []RateLimit `json:"limits"`
	// Cost is the number of tokens a request takes from every applied limit, at least 1; a runtime error or a smaller value applies failureMode.
	// +ruralz:default="1"
	// +ruralz:cel=request,source,route,consumer,auth,now:int
	Cost *string `json:"cost,omitempty"`
	// ResponseHeaders selects the RateLimit-Policy and RateLimit response fields the Policy records: ietf records them, none records nothing, though a 429 of the Policy still carries Retry-After.
	// +ruralz:default=ietf
	ResponseHeaders *RateLimitResponseHeaders `json:"responseHeaders,omitempty"`
	// LocalOnly keeps the limit in each Node's local buckets and never calls the State Store, so a Cell admits at most the Node count times the per-Node ceiling per window, even when the State Store is healthy (OQ-scalability-and-distributed-state-11 (a)).
	// +ruralz:default=false
	LocalOnly *bool `json:"localOnly,omitempty"`
}

// RateLimitResponseHeaders selects the RateLimit response fields a
// ratelimit Policy records.
type RateLimitResponseHeaders string

// RateLimit response field choices.
const (
	// RateLimitResponseHeadersIETF records the RateLimit-Policy and RateLimit fields of draft-ietf-httpapi-ratelimit-headers.
	RateLimitResponseHeadersIETF RateLimitResponseHeaders = "ietf"
	// RateLimitResponseHeadersNone records no RateLimit field.
	RateLimitResponseHeadersNone RateLimitResponseHeaders = "none"
)

// RateLimit is one limit window.
type RateLimit struct {
	// Requests is the number of requests allowed per window.
	// +ruralz:required
	// +ruralz:minimum=1
	Requests int64 `json:"requests"`
	// Window is the window length, at least 1ms in whole milliseconds.
	// +ruralz:required
	Window Duration `json:"window"`
	// PerNodeCeiling caps one Node's share, 1 to requests; unset means derived.
	// +ruralz:minimum=1
	PerNodeCeiling *int64 `json:"perNodeCeiling,omitempty"`
	// Burst is 0 to requests; default requests.
	// +ruralz:minimum=0
	Burst *int64 `json:"burst,omitempty"`
	// When applies the limit only to requests for which it is true; absent, the limit always applies.
	// +ruralz:cel=request,source,route,consumer,auth,now:bool
	When string `json:"when,omitempty"`
}

// QuotaConfig is the config of a quota Policy.
// +ruralz:policyType=quota
type QuotaConfig struct {
	// ConsumerQuota names a Consumer quota with unit requests.
	// +ruralz:required
	ConsumerQuota string `json:"consumerQuota"`
	// Key partitions the quota.
	// +ruralz:default=consumer.name
	// +ruralz:cel=request,source,route,consumer,auth,now:string
	Key *string `json:"key,omitempty"`
	// Cost is the number of units a request reserves, at least 1, and a refund returns; a runtime error or a smaller value applies failureMode, and a cost above the units left in the window is denied, reserving nothing.
	// +ruralz:default="1"
	// +ruralz:cel=request,source,route,consumer,auth,now:int
	Cost *string `json:"cost,omitempty"`
}

// ValidationJSONSchemaConfig is the config of a validation.json-schema
// Policy, which validates JSON request bodies (400 RZ-RT-009 on failure).
// +ruralz:policyType=validation.json-schema
type ValidationJSONSchemaConfig struct {
	// Schema is the inline JSON Schema (draft 2020-12) a request body must match: an object or a boolean. Its $ref resolves only within the document, formats are annotations, patterns are RE2, and an invalid schema is RZ-CFG-005. Environment substitution applies to its strings, so a literal ${ is written $${.
	// +ruralz:required
	Schema JSONSchemaDocument `json:"schema"`
}

// CORSConfig is the config of a cors Policy. allowCredentials true with "*"
// or "null" in allowOrigins, or with "*" in allowHeaders or exposeHeaders,
// is RZ-CFG-005.
// +ruralz:policyType=cors
type CORSConfig struct {
	// AllowOrigins are the allowed origins, matched exactly: "*" allows any origin, and every other entry is a serialized origin such as https://shop.example (lower-case scheme and host, no default port, no path) or null, else RZ-CFG-005.
	// +ruralz:list=set
	AllowOrigins []string `json:"allowOrigins,omitempty"`
	// AllowMethods are the allowed methods, matched exactly.
	// +ruralz:list=set
	AllowMethods []string `json:"allowMethods,omitempty"`
	// AllowHeaders are the request header names a preflight may ask for, matched ignoring case; "*" allows any. Without them only CORS-safelisted request headers are allowed.
	// +ruralz:list=set
	AllowHeaders []string `json:"allowHeaders,omitempty"`
	// AllowCredentials lets a browser send cookies, HTTP authentication or a client certificate and read the response; absent means false.
	AllowCredentials *bool `json:"allowCredentials,omitempty"`
	// MaxAge is how long a browser may cache a preflight answer, sent in whole seconds; absent sends no Access-Control-Max-Age.
	MaxAge *Duration `json:"maxAge,omitempty"`
	// ExposeHeaders are the response header names a page may read beyond the CORS-safelisted ones; "*" exposes all.
	// +ruralz:list=set
	ExposeHeaders []string `json:"exposeHeaders,omitempty"`
}

// CacheConfig is the config of a cache (Response Cache) Policy.
// +ruralz:policyType=cache
// +ruralz:open
type CacheConfig struct {
	// Key partitions entries, for example per Consumer.
	// +ruralz:cel=request,source,route,consumer,auth,now:string
	Key string `json:"key,omitempty"`
}

// HeadersConfig is the config of a headers Policy. Each side applies its
// remove, then set, then add entries; more than 32 entries across all lists
// is RZ-CFG-005.
// +ruralz:policyType=headers
type HeadersConfig struct {
	// Request edits request headers.
	Request *HeaderRequestOps `json:"request,omitempty"`
	// Response edits response headers.
	Response *HeaderResponseOps `json:"response,omitempty"`
}

// HeaderRequestOps are request header operations.
type HeaderRequestOps struct {
	// Set replaces every field line of each named request header with one line.
	// +ruralz:list=map,key=name
	Set []HeaderRequestSet `json:"set,omitempty"`
	// Add appends one request header field line per entry, keeping existing lines.
	// +ruralz:list=atomic
	Add []HeaderRequestSet `json:"add,omitempty"`
	// Remove deletes every field line of each named request header.
	// +ruralz:list=set
	Remove []string `json:"remove,omitempty"`
}

// HeaderRequestSet is one request header entry of set or add: a name and
// exactly one of value or valueExpression.
// +ruralz:exactlyOneOf=value,valueExpression
type HeaderRequestSet struct {
	// Name is the header name.
	// +ruralz:required
	Name string `json:"name"`
	// Value is a literal value; it may be empty.
	Value *string `json:"value,omitempty"`
	// ValueExpression computes the value; it is not empty.
	// +ruralz:minLength=1
	// +ruralz:cel=request,source,route,consumer,auth,now:string
	ValueExpression string `json:"valueExpression,omitempty"`
}

// HeaderResponseOps are response header operations.
type HeaderResponseOps struct {
	// Set replaces every field line of each named response header with one line.
	// +ruralz:list=map,key=name
	Set []HeaderResponseSet `json:"set,omitempty"`
	// Add appends one response header field line per entry, keeping existing lines.
	// +ruralz:list=atomic
	Add []HeaderResponseSet `json:"add,omitempty"`
	// Remove deletes every field line of each named response header.
	// +ruralz:list=set
	Remove []string `json:"remove,omitempty"`
}

// HeaderResponseSet is one response header entry of set or add: a name and
// exactly one of value or valueExpression.
// +ruralz:exactlyOneOf=value,valueExpression
type HeaderResponseSet struct {
	// Name is the header name.
	// +ruralz:required
	Name string `json:"name"`
	// Value is a literal value; it may be empty.
	Value *string `json:"value,omitempty"`
	// ValueExpression computes the value; it is not empty.
	// +ruralz:minLength=1
	// +ruralz:cel=request,source,route,consumer,auth,now,response,upstream:string
	ValueExpression string `json:"valueExpression,omitempty"`
}

// TransformRequestTarget is what a transform.request set or remove entry
// edits.
type TransformRequestTarget string

// Request transform targets.
const (
	// TransformRequestTargetHeader is a request header.
	TransformRequestTargetHeader TransformRequestTarget = "header"
	// TransformRequestTargetQuery is a query parameter.
	TransformRequestTargetQuery TransformRequestTarget = "query"
	// TransformRequestTargetBody is a field of the JSON body.
	TransformRequestTargetBody TransformRequestTarget = "body"
	// TransformRequestTargetPath is the forwarded request path, which only
	// a set entry without a name rewrites.
	TransformRequestTargetPath TransformRequestTarget = "path"
)

// TransformResponseTarget is what a transform.response set or remove entry
// edits.
type TransformResponseTarget string

// Response transform targets.
const (
	// TransformResponseTargetHeader is a response header.
	TransformResponseTargetHeader TransformResponseTarget = "header"
	// TransformResponseTargetBody is a field of the JSON body.
	TransformResponseTargetBody TransformResponseTarget = "body"
)

// ArrayOpKind is a JSON array operation.
type ArrayOpKind string

// Array operations.
const (
	// ArrayOpMove takes the value at from, or the array of a * path's
	// matches, out of the body and writes it at to.
	ArrayOpMove ArrayOpKind = "move"
	// ArrayOpAppend adds the value or matches at from to the end of the
	// array at to, keeping from.
	ArrayOpAppend ArrayOpKind = "append"
	// ArrayOpDelete removes the elements from names from their array.
	ArrayOpDelete ArrayOpKind = "delete"
)

// TransformRequestConfig is the config of a transform.request Policy. It
// applies body, then arrayOps, set, remove and replace, each list in order,
// and every CEL field reads the request as it reached the Policy. More than
// 32 entries across set, remove, arrayOps and replace is RZ-CFG-005.
// +ruralz:policyType=transform.request
type TransformRequestConfig struct {
	// Body computes the forwarded request body: a top-level string is written as its UTF-8 bytes, bytes as they are, and any other value as JSON. A string under a JSON contentType must be one JSON value.
	// +ruralz:cel=request,source,route,consumer,auth,now,upstream:dyn
	Body string `json:"body,omitempty"`
	// ContentType is the Content-Type set whenever body is set; an RFC 9110 media type, else RZ-CFG-005.
	// +ruralz:default=application/json
	ContentType *string `json:"contentType,omitempty"`
	// Set sets headers or query parameters to a computed string, rewrites the forwarded path, or writes computed JSON values at body paths, creating missing objects.
	// +ruralz:list=atomic
	Set []TransformRequestSet `json:"set,omitempty"`
	// Remove drops headers, every occurrence of a query parameter, or body paths.
	// +ruralz:list=atomic
	Remove []TransformRequestRemove `json:"remove,omitempty"`
	// ArrayOps move, append or delete JSON values at body paths.
	// +ruralz:list=atomic
	ArrayOps []ArrayOp `json:"arrayOps,omitempty"`
	// Replace rewrites matches in the strings at a body path, or in the raw body, with an RE2 or a literal pattern.
	// +ruralz:list=atomic
	Replace []Replace `json:"replace,omitempty"`
}

// TransformRequestSet sets one request value: a header, a query parameter,
// a body field, or the forwarded path. At most one entry has target path,
// and it takes no name.
// +ruralz:requiredIf=name,target=header
// +ruralz:requiredIf=name,target=query
// +ruralz:requiredIf=name,target=body
type TransformRequestSet struct {
	// Target is header, query, body or path.
	// +ruralz:required
	Target TransformRequestTarget `json:"target"`
	// Name is the header name, the query parameter name or the body dot path; target path takes none.
	Name string `json:"name,omitempty"`
	// ValueExpression computes the value: a string for targets header, query and path, and any value for target body, written there as JSON.
	// +ruralz:required
	// +ruralz:minLength=1
	// +ruralz:cel=request,source,route,consumer,auth,now,upstream:dyn
	ValueExpression string `json:"valueExpression"`
}

// TransformRequestRemove removes one request value: a header, every
// occurrence of a query parameter, or a body path. Target path is
// RZ-CFG-005.
type TransformRequestRemove struct {
	// Target is header, query or body.
	// +ruralz:required
	Target TransformRequestTarget `json:"target"`
	// Name is the header name, the query parameter name or the body dot path.
	// +ruralz:required
	Name string `json:"name"`
}

// TransformResponseConfig is the config of a transform.response Policy. It
// applies body, then arrayOps, set, remove and replace, each list in order,
// and every CEL field reads the response as it reached the Policy. More
// than 32 entries across set, remove, arrayOps and replace is RZ-CFG-005.
// +ruralz:policyType=transform.response
type TransformResponseConfig struct {
	// Body computes the response body: a top-level string is written as its UTF-8 bytes, bytes as they are, and any other value as JSON. A string under a JSON contentType must be one JSON value.
	// +ruralz:cel=request,source,route,consumer,auth,now,response,upstream:dyn
	Body string `json:"body,omitempty"`
	// ContentType is the Content-Type set whenever body is set; an RFC 9110 media type, else RZ-CFG-005.
	// +ruralz:default=application/json
	ContentType *string `json:"contentType,omitempty"`
	// Set sets headers to a computed string, or writes computed JSON values at body paths, creating missing objects.
	// +ruralz:list=atomic
	Set []TransformResponseSet `json:"set,omitempty"`
	// Remove drops headers or body paths.
	// +ruralz:list=atomic
	Remove []TransformResponseRemove `json:"remove,omitempty"`
	// ArrayOps move, append or delete JSON values at body paths.
	// +ruralz:list=atomic
	ArrayOps []ArrayOp `json:"arrayOps,omitempty"`
	// Replace rewrites matches in the strings at a body path, or in the raw body, with an RE2 or a literal pattern.
	// +ruralz:list=atomic
	Replace []Replace `json:"replace,omitempty"`
}

// TransformResponseSet sets one response value: a header or a body field.
type TransformResponseSet struct {
	// Target is header or body.
	// +ruralz:required
	Target TransformResponseTarget `json:"target"`
	// Name is the header name or the body dot path.
	// +ruralz:required
	Name string `json:"name"`
	// ValueExpression computes the value: a string for target header, and any value for target body, written there as JSON.
	// +ruralz:required
	// +ruralz:minLength=1
	// +ruralz:cel=request,source,route,consumer,auth,now,response,upstream:dyn
	ValueExpression string `json:"valueExpression"`
}

// TransformResponseRemove removes one response value: a header or a body
// path.
type TransformResponseRemove struct {
	// Target is header or body.
	// +ruralz:required
	Target TransformResponseTarget `json:"target"`
	// Name is the header name or the body dot path.
	// +ruralz:required
	Name string `json:"name"`
}

// ArrayOp is one JSON array operation: move and append need from and to,
// and delete needs from and takes no to.
// +ruralz:requiredIf=from,op=move
// +ruralz:requiredIf=to,op=move
// +ruralz:requiredIf=from,op=append
// +ruralz:requiredIf=to,op=append
// +ruralz:requiredIf=from,op=delete
type ArrayOp struct {
	// Op is move, append or delete.
	// +ruralz:required
	Op ArrayOpKind `json:"op"`
	// From is the body dot path of the values the operation takes; a * segment matches every element of an array.
	From string `json:"from,omitempty"`
	// To is the body dot path the operation writes, without a * segment.
	To string `json:"to,omitempty"`
}

// Replace rewrites matches in the strings at a body path, or in the raw
// body. A pattern over 1 KiB, or one that is not valid RE2 unless literal is
// true, is RZ-CFG-005.
type Replace struct {
	// Path is the body dot path of the strings to rewrite; without it the raw body is rewritten.
	Path string `json:"path,omitempty"`
	// Pattern is an RE2 pattern, or plain text when literal is true.
	// +ruralz:required
	// +ruralz:maxLength=1024
	Pattern string `json:"pattern"`
	// Replacement replaces each match: $1 or $name refers to a group of an RE2 pattern (write ${1} as $${1}, which environment substitution requires), and with literal it is plain text.
	Replacement string `json:"replacement,omitempty"`
	// Literal treats pattern and replacement as plain text; absent means false.
	Literal *bool `json:"literal,omitempty"`
}

// AuthUpstreamOAuth2Config is the config of an auth.upstream-oauth2 Policy.
// +ruralz:policyType=auth.upstream-oauth2
// +ruralz:open
type AuthUpstreamOAuth2Config struct {
	// TokenURL is the token endpoint and the only destination of clientSecret; it MUST be https (RZ-CFG-037).
	// +ruralz:required
	// +ruralz:impact=security
	TokenURL string `json:"tokenUrl"`
	// ClientID is the OAuth2 client identifier.
	// +ruralz:required
	ClientID string `json:"clientId"`
	// ClientSecret is the OAuth2 client secret.
	// +ruralz:required
	// +ruralz:secret
	// +ruralz:impact=security
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
