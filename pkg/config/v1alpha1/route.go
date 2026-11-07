// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// RouteSpec is the spec of a Route. It sets exactly one of upstreams,
// composition and redirect.
// +ruralz:exactlyOneOf=upstreams,composition,redirect
type RouteSpec struct {
	// Listeners names Gateway listeners; default all.
	// +ruralz:list=set
	Listeners []string `json:"listeners,omitempty"`
	// Match selects requests; every criterion must hold.
	// +ruralz:required
	// +ruralz:impact=routing
	Match RouteMatch `json:"match"`
	// Policies attaches Route-scoped Policies in authored order.
	// +ruralz:list=orderedMap,key=name
	Policies []PolicyRef `json:"policies,omitempty"`
	// ExcludePolicies removes inherited Gateway Policies.
	// +ruralz:list=map,key=name
	ExcludePolicies []PolicyRef `json:"excludePolicies,omitempty"`
	// Upstreams are weighted legs; exactly one of upstreams, composition and redirect is set.
	// +ruralz:list=map,key=name
	// +ruralz:impact=routing
	Upstreams []RouteUpstream `json:"upstreams,omitempty"`
	// Composition calls several Upstreams per request; exactly one of upstreams, composition and redirect is set.
	// +ruralz:impact=routing
	Composition *Composition `json:"composition,omitempty"`
	// Redirect answers every request with a redirect after the onRequestHeaders Policies, instead of forwarding it; exactly one of upstreams, composition and redirect is set.
	// +ruralz:impact=routing
	// +ruralz:since=1
	Redirect *RouteRedirect `json:"redirect,omitempty"`
	// Mirrors send a copy of a sampled share of the requests to other Upstreams and discard their responses; valid only beside upstreams.
	// +ruralz:list=map,key=upstream
	// +ruralz:impact=routing,traffic
	// +ruralz:since=1
	Mirrors []RouteMirror `json:"mirrors,omitempty"`
	// Timeout is the whole-request deadline.
	// +ruralz:impact=traffic
	Timeout *Duration `json:"timeout,omitempty"`
}

// RouteMirror sends a copy of a sampled share of a Route's requests to one
// Upstream: what the upstreams leg's first attempt sends after its
// onUpstreamRequest Policies, with no Policy or retry of its own, its
// response discarded. A mirror never delays or fails the request it copies.
type RouteMirror struct {
	// Upstream is the metadata.name of the Upstream the copies go to.
	// +ruralz:required
	// +ruralz:ref=Upstream
	// +ruralz:since=1
	Upstream string `json:"upstream"`
	// Percent is the share of the Route's requests copied, from 0 to 100.
	// +ruralz:default=100
	// +ruralz:minimum=0
	// +ruralz:maximum=100
	// +ruralz:since=1
	Percent *int32 `json:"percent,omitempty"`
	// MaxInFlight bounds the copies in flight to this Upstream on each Node; a copy past it is dropped.
	// +ruralz:default=1000
	// +ruralz:minimum=1
	// +ruralz:since=1
	MaxInFlight *int32 `json:"maxInFlight,omitempty"`
}

// RedirectScheme is the scheme of a redirect's Location.
type RedirectScheme string

// Redirect schemes.
const (
	// RedirectSchemeHTTP redirects to http.
	RedirectSchemeHTTP RedirectScheme = "http"
	// RedirectSchemeHTTPS redirects to https.
	RedirectSchemeHTTPS RedirectScheme = "https"
)

// RedirectQuery is what a redirect does with the request's query.
type RedirectQuery string

// Redirect query handling.
const (
	// RedirectQueryKeep keeps the request's query.
	RedirectQueryKeep RedirectQuery = "keep"
	// RedirectQueryDrop drops it.
	RedirectQueryDrop RedirectQuery = "drop"
)

// RouteRedirect answers a Route's requests with a redirect. Its Location is
// the request URL with scheme, host, path or prefix and query applied, or
// the URI locationExpression builds, which excludes those fields.
// +ruralz:atMostOneOf=path,prefix
// +ruralz:atMostOneOf=locationExpression,scheme
// +ruralz:atMostOneOf=locationExpression,host
// +ruralz:atMostOneOf=locationExpression,path
// +ruralz:atMostOneOf=locationExpression,prefix
// +ruralz:atMostOneOf=locationExpression,query
type RouteRedirect struct {
	// Status is the redirect status: 301, 302, 307 or 308.
	// +ruralz:default=302
	// +ruralz:minimum=301
	// +ruralz:maximum=308
	// +ruralz:since=1
	Status *int32 `json:"status,omitempty"`
	// Scheme replaces the request's scheme; default the request's.
	// +ruralz:since=1
	Scheme RedirectScheme `json:"scheme,omitempty"`
	// Host replaces the request's host and port: a DNS name or an IP address, IPv6 in brackets, with an optional port; default the request's host.
	// +ruralz:since=1
	Host string `json:"host,omitempty"`
	// Path replaces the request's path: it starts with /, holds no ? or #, and passes the hardening of the client path with no . or .. segment.
	// +ruralz:since=1
	Path string `json:"path,omitempty"`
	// Prefix replaces the match.path.prefix the request matched, under the rules of path; only on a Route whose match.path is a prefix.
	// +ruralz:since=1
	Prefix string `json:"prefix,omitempty"`
	// Query keeps or drops the request's query.
	// +ruralz:default="keep"
	// +ruralz:since=1
	Query *RedirectQuery `json:"query,omitempty"`
	// LocationExpression builds the whole Location; it excludes scheme, host, path, prefix and query. Planned (M2): its runtime-error rule is OQ-traffic-management-and-resilience-26.
	// +ruralz:cel=request,source,route,consumer,auth,now:string
	// +ruralz:since=1
	// +ruralz:planned=M2
	LocationExpression string `json:"locationExpression,omitempty"`
}

// RouteMatch holds match criteria; at least one is set and all must hold.
// +ruralz:minProperties=1
type RouteMatch struct {
	// Hosts are host names; an entry may start with one "*." label.
	// +ruralz:list=set
	Hosts []string `json:"hosts,omitempty"`
	// Path matches the request path.
	Path *PathMatch `json:"path,omitempty"`
	// Methods are HTTP methods.
	// +ruralz:list=set
	Methods []string `json:"methods,omitempty"`
	// Headers match request headers.
	// +ruralz:list=map,key=name
	Headers []HeaderMatch `json:"headers,omitempty"`
	// GRPC matches a gRPC service and method. Planned (M3).
	// +ruralz:planned=M3
	GRPC *GRPCMatch `json:"grpc,omitempty"`
	// GraphQL matches a GraphQL operation. Planned (M3).
	// +ruralz:planned=M3
	GraphQL *GraphQLMatch `json:"graphql,omitempty"`
	// Topic matches an event topic. Planned (M4); its ingress declaration is OQ-configuration-model-11.
	// +ruralz:planned=M4
	Topic string `json:"topic,omitempty"`
	// When is a CEL condition over the request without its body.
	// +ruralz:cel=request,source,now:bool
	When string `json:"when,omitempty"`
}

// PathMatch matches the request path with exactly one form.
// +ruralz:exactlyOneOf=exact,prefix,template,regex
type PathMatch struct {
	// Exact matches the whole path.
	Exact string `json:"exact,omitempty"`
	// Prefix matches a path prefix.
	Prefix string `json:"prefix,omitempty"`
	// Template matches a path template such as /v1/orders/{orderId}; captures reach CEL as request.pathParams.
	Template string `json:"template,omitempty"`
	// Regex matches an RE2 regular expression.
	Regex string `json:"regex,omitempty"`
}

// HeaderMatch matches one request header with exactly one form.
// +ruralz:exactlyOneOf=exact,regex,present
type HeaderMatch struct {
	// Name is the header name.
	// +ruralz:required
	Name string `json:"name"`
	// Exact matches the whole value.
	Exact string `json:"exact,omitempty"`
	// Regex matches an RE2 regular expression.
	Regex string `json:"regex,omitempty"`
	// Present matches on presence alone.
	Present *bool `json:"present,omitempty"`
}

// GRPCMatch matches a gRPC service and optional method.
type GRPCMatch struct {
	// Service is the fully qualified service name.
	// +ruralz:required
	Service string `json:"service"`
	// Method is the method name; default any.
	Method string `json:"method,omitempty"`
}

// GraphQLMatch matches a GraphQL operation.
type GraphQLMatch struct {
	// OperationType is the operation type, such as query.
	OperationType string `json:"operationType,omitempty"`
	// OperationName is the operation name.
	OperationName string `json:"operationName,omitempty"`
}

// RouteUpstream is one weighted leg of a Route.
type RouteUpstream struct {
	// Name is the metadata.name of an Upstream.
	// +ruralz:required
	// +ruralz:ref=Upstream
	Name string `json:"name"`
	// Weight splits traffic across entries.
	// +ruralz:default=1
	// +ruralz:minimum=0
	Weight *int32 `json:"weight,omitempty"`
}

// CompositionMode is how composition steps run.
type CompositionMode string

// Composition modes.
const (
	// CompositionModeAggregate runs steps in parallel and merges bodies under group.
	CompositionModeAggregate CompositionMode = "aggregate"
	// CompositionModeSequential runs steps in list order and exposes earlier results as steps.
	CompositionModeSequential CompositionMode = "sequential"
	// CompositionModeConditional runs the first step whose when is true.
	CompositionModeConditional CompositionMode = "conditional"
)

// Composition calls several Upstreams for one request.
type Composition struct {
	// Mode is aggregate, sequential or conditional.
	// +ruralz:required
	Mode CompositionMode `json:"mode"`
	// Steps run in the order the mode defines; more than Gateway limits.maxCompositionSteps is RZ-CFG-031.
	// +ruralz:required
	// +ruralz:minItems=1
	// +ruralz:list=orderedMap,key=name
	Steps []CompositionStep `json:"steps"`
}

// CompositionStep is one Upstream call in a composition. Each step has
// exactly one of path or pathExpression, and collection true needs group.
// +ruralz:exactlyOneOf=path,pathExpression
// +ruralz:requiredIf=group,collection=true
type CompositionStep struct {
	// Name identifies the step.
	// +ruralz:required
	Name string `json:"name"`
	// Upstream is the metadata.name of an Upstream.
	// +ruralz:required
	// +ruralz:ref=Upstream
	Upstream string `json:"upstream"`
	// Method is the HTTP method; default the incoming method.
	Method string `json:"method,omitempty"`
	// Path is a literal path starting with / that passes the hardening of the client path, with no . or .. segment; its first ? starts the raw query sent, else the client's query is forwarded.
	Path string `json:"path,omitempty"`
	// PathExpression computes the path, under the rules of path; splice decoded captures with url.pathEscape or url.queryEscape, or read request.rawPathParams.
	// +ruralz:cel=request,source,route,consumer,auth,now,steps:string
	PathExpression string `json:"pathExpression,omitempty"`
	// Target is a dot path that unwraps a nested value of the response body first.
	Target string `json:"target,omitempty"`
	// Select is an allowlist of response fields.
	// +ruralz:list=set
	Select []string `json:"select,omitempty"`
	// Rename renames response fields.
	Rename map[string]string `json:"rename,omitempty"`
	// Collection is true when the value after target is a JSON array; it needs group.
	// +ruralz:default=false
	Collection *bool `json:"collection,omitempty"`
	// Group is the key under which the body is merged.
	Group string `json:"group,omitempty"`
	// MaxBodyBytes caps this step's body, at least 1 byte in either spelling; default an equal share of Gateway limits.maxResponseBodyBytes.
	// +ruralz:minimum=1
	// +ruralz:pattern=^[0-9]*[1-9][0-9]*((\.[0-9]+)?(k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei))?$|^[0-9]+\.[0-9]*[1-9][0-9]*(k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)$
	MaxBodyBytes *ByteSize `json:"maxBodyBytes,omitempty"`
	// Optional allows a partial response when the step fails.
	// +ruralz:default=false
	Optional *bool `json:"optional,omitempty"`
	// When runs the step only when true; conditional and sequential modes.
	// +ruralz:cel=request,source,route,consumer,auth,now,steps:bool
	When string `json:"when,omitempty"`
}
