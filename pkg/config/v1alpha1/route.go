// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// RouteSpec is the spec of a Route.
// +ruralz:exactlyOneOf=upstreams,composition
type RouteSpec struct {
	// Listeners names Gateway listeners; default all.
	// +ruralz:list=set
	Listeners []string `json:"listeners,omitempty"`
	// Match selects requests; every criterion must hold.
	// +ruralz:required
	Match RouteMatch `json:"match"`
	// Policies attaches Route-scoped Policies in authored order.
	// +ruralz:list=orderedMap,key=name
	Policies []PolicyRef `json:"policies,omitempty"`
	// ExcludePolicies removes inherited Gateway Policies.
	// +ruralz:list=map,key=name
	ExcludePolicies []PolicyRef `json:"excludePolicies,omitempty"`
	// Upstreams are weighted legs; required unless composition is set.
	// +ruralz:list=map,key=name
	Upstreams []RouteUpstream `json:"upstreams,omitempty"`
	// Composition calls several Upstreams per request; mutually exclusive with upstreams.
	Composition *Composition `json:"composition,omitempty"`
	// Timeout is the whole-request deadline.
	// +ruralz:impact=traffic
	Timeout *Duration `json:"timeout,omitempty"`
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
	GRPC *GRPCMatch `json:"grpc,omitempty"`
	// GraphQL matches a GraphQL operation. Planned (M3).
	GraphQL *GraphQLMatch `json:"graphql,omitempty"`
	// Topic matches an event topic. Planned (M4); its ingress declaration is OQ-configuration-model-11.
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
// exactly one of path or pathExpression.
// +ruralz:exactlyOneOf=path,pathExpression
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
	// Path is a literal path.
	Path string `json:"path,omitempty"`
	// PathExpression computes the path.
	// +ruralz:cel=request,source,route,consumer,auth,now,steps:string
	PathExpression string `json:"pathExpression,omitempty"`
	// Target unwraps a nested object of the response first.
	Target string `json:"target,omitempty"`
	// Select is an allowlist of response fields.
	// +ruralz:list=set
	Select []string `json:"select,omitempty"`
	// Rename renames response fields.
	Rename map[string]string `json:"rename,omitempty"`
	// Collection is true when the body is a JSON array.
	// +ruralz:default=false
	Collection *bool `json:"collection,omitempty"`
	// Group is the key under which the body is merged.
	Group string `json:"group,omitempty"`
	// MaxBodyBytes caps this step's body; default an equal share of Gateway limits.maxResponseBodyBytes.
	MaxBodyBytes *ByteSize `json:"maxBodyBytes,omitempty"`
	// Optional allows a partial response when the step fails.
	// +ruralz:default=false
	Optional *bool `json:"optional,omitempty"`
	// When runs the step only when true; conditional and sequential modes.
	// +ruralz:cel=request,source,route,consumer,auth,now,steps:bool
	When string `json:"when,omitempty"`
}
