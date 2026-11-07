// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package export

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// opKey is the comparable part of an Operation.
type opKey struct {
	route, method, path, params, id string
	body                            bool
}

// keysOf returns the comparable parts of ops.
func keysOf(ops []Operation) []opKey {
	out := make([]opKey, 0, len(ops))
	for _, op := range ops {
		out = append(out, opKey{op.Route, op.Method, op.Path, strings.Join(op.Params, ","), op.OperationID, op.Body != nil})
	}
	return out
}

// TestOperationsQualify: a Route whose match sets no grpc, graphql or
// topic, whose path is exact or template and whose Upstreams all speak
// http exports its path; every other Route is a Skip naming why.
func TestOperationsQualify(t *testing.T) {
	get := "    methods: [GET]\n"
	tests := []struct {
		name string
		docs []string
		want []opKey
		skip string // "<route>: <reason>" of the Route skipped; "" for none
	}{
		{
			name: "a template Route exports its template",
			docs: []string{upstreamYAML("orders", "http"), routeYAML("orders", "    path: {template: \"/v1/orders/{orderId}/items/{itemId}\"}\n"+get, toUpstream("orders"))},
			want: []opKey{{"orders", "GET", "/v1/orders/{orderId}/items/{itemId}", "orderId,itemId", "orders", false}},
		},
		{
			name: "an exact Route exports its path",
			docs: []string{upstreamYAML("orders", "http"), routeYAML("orders", "    path: {exact: /v1/orders}\n"+get, toUpstream("orders"))},
			want: []opKey{{"orders", "GET", "/v1/orders", "", "orders", false}},
		},
		{
			name: "a composition Route to http Upstreams exports its path",
			docs: []string{upstreamYAML("orders", "http"), upstreamYAML("stock", "http"), routeYAML("summary", "    path: {template: \"/v1/orders/{id}\"}\n"+get, `  composition:
    mode: aggregate
    steps:
      - {name: order, upstream: orders, path: /orders, group: order}
      - {name: stock, upstream: stock, path: /stock, group: stock}
`)},
			want: []opKey{{"summary", "GET", "/v1/orders/{id}", "id", "summary", false}},
		},
		{
			name: "match.grpc has no operation",
			docs: []string{upstreamYAML("cart", "grpc"), routeYAML("cart", "    grpc: {service: shop.cart.v1.CartService}\n", toUpstream("cart"))},
			skip: "cart: match.grpc has no OpenAPI operation",
		},
		{
			name: "match.graphql has no operation",
			docs: []string{upstreamYAML("orders", "http"), routeYAML("orders", "    path: {exact: /graphql}\n    graphql: {operationType: query}\n", toUpstream("orders"))},
			skip: "orders: match.graphql has no OpenAPI operation",
		},
		{
			name: "match.topic has no operation",
			docs: []string{upstreamYAML("orders", "http"), routeYAML("orders", "    path: {exact: /events}\n    topic: orders.created\n", toUpstream("orders"))},
			skip: "orders: match.topic has no OpenAPI operation",
		},
		{
			name: "a prefix path has no OpenAPI path",
			docs: []string{upstreamYAML("orders", "http"), routeYAML("orders", "    path: {prefix: /v1}\n", toUpstream("orders"))},
			skip: "orders: a prefix path has no OpenAPI path",
		},
		{
			name: "a regex path has no OpenAPI path",
			docs: []string{upstreamYAML("orders", "http"), routeYAML("orders", "    path: {regex: \"/v1/[a-z]+\"}\n", toUpstream("orders"))},
			skip: "orders: a regex path has no OpenAPI path",
		},
		{
			name: "a Route without a path has none",
			docs: []string{upstreamYAML("orders", "http"), routeYAML("orders", get, toUpstream("orders"))},
			skip: "orders: a Route without a path has no OpenAPI path",
		},
		{
			name: "an exact path holding a brace has none",
			docs: []string{upstreamYAML("orders", "http"), routeYAML("orders", "    path: {exact: \"/v1/{orders}\"}\n", toUpstream("orders"))},
			skip: "orders: an exact path holding { or } has no OpenAPI path",
		},
		{
			name: "an Upstream of another protocol",
			docs: []string{upstreamYAML("feed", "websocket"), routeYAML("feed", "    path: {exact: /feed}\n", toUpstream("feed"))},
			skip: `feed: Upstream "feed" speaks websocket, not http`,
		},
		{
			name: "a composition step to an Upstream of another protocol",
			docs: []string{upstreamYAML("orders", "http"), upstreamYAML("feed", "websocket"), routeYAML("summary", "    path: {exact: /summary}\n", `  composition:
    mode: sequential
    steps:
      - {name: order, upstream: orders, path: /orders}
      - {name: feed, upstream: feed, path: /feed}
`)},
			skip: `summary: Upstream "feed" speaks websocket, not http`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := validated(t, bundleOf(gatewayYAML(""), tc.docs...))
			ops, skips := Operations(v)
			if got := keysOf(ops); !slices.Equal(got, tc.want) {
				t.Errorf("Operations() = %+v, want %+v", got, tc.want)
			}
			var reasons []string
			for _, s := range skips {
				reasons = append(reasons, s.Route+": "+s.Reason)
			}
			var want []string
			if tc.skip != "" {
				want = []string{tc.skip}
			}
			if !slices.Equal(reasons, want) {
				t.Errorf("skips = %q, want %q", reasons, want)
			}
		})
	}
}

// TestOperationsMethods: a Route without methods exports the eight
// OpenAPI methods in document order; a method OpenAPI does not describe,
// such as PURGE or the lower-case get, is a Skip of that method alone;
// and a Route exporting one method keeps its name as operationId.
func TestOperationsMethods(t *testing.T) {
	tests := []struct {
		name    string
		methods string
		want    []opKey
		skips   []string
	}{
		{
			name: "a Route without methods gets all eight",
			want: []opKey{
				{"api", "GET", "/api", "", "api-get", false},
				{"api", "PUT", "/api", "", "api-put", false},
				{"api", "POST", "/api", "", "api-post", false},
				{"api", "DELETE", "/api", "", "api-delete", false},
				{"api", "OPTIONS", "/api", "", "api-options", false},
				{"api", "HEAD", "/api", "", "api-head", false},
				{"api", "PATCH", "/api", "", "api-patch", false},
				{"api", "TRACE", "/api", "", "api-trace", false},
			},
		},
		{
			name:    "methods in OpenAPI order whatever the authored order",
			methods: "    methods: [PATCH, GET, DELETE]\n",
			want: []opKey{
				{"api", "GET", "/api", "", "api-get", false},
				{"api", "DELETE", "/api", "", "api-delete", false},
				{"api", "PATCH", "/api", "", "api-patch", false},
			},
		},
		{
			name:    "a method OpenAPI does not describe is skipped",
			methods: "    methods: [GET, PURGE, get]\n",
			want:    []opKey{{"api", "GET", "/api", "", "api", false}},
			skips:   []string{"api: method PURGE has no OpenAPI operation", "api: method get has no OpenAPI operation"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := validated(t, bundleOf(gatewayYAML(""), upstreamYAML("api", "http"),
				routeYAML("api", "    path: {exact: /api}\n"+tc.methods, toUpstream("api"))))
			ops, skips := Operations(v)
			if got := keysOf(ops); !slices.Equal(got, tc.want) {
				t.Errorf("Operations() = %+v, want %+v", got, tc.want)
			}
			var got []string
			for _, s := range skips {
				got = append(got, s.Route+": "+s.Reason)
			}
			if !slices.Equal(got, tc.skips) {
				t.Errorf("skips = %q, want %q", got, tc.skips)
			}
		})
	}
}

// TestOperationsCollisions: a path and method an earlier Route exported
// is a Skip of the later Route's method; a template of the hierarchy of
// a path an earlier Route exported takes that path and its capture
// names, so the document holds one path per hierarchy.
func TestOperationsCollisions(t *testing.T) {
	v := validated(t, bundleOf(gatewayYAML(""), upstreamYAML("api", "http"),
		routeYAML("a-orders", "    hosts: [a.example]\n    path: {template: \"/v1/orders/{id}\"}\n    methods: [GET]\n", toUpstream("api")),
		routeYAML("b-orders", "    hosts: [b.example]\n    path: {template: \"/v1/orders/{orderId}\"}\n    methods: [GET, POST]\n", toUpstream("api")),
		routeYAML("c-orders", "    hosts: [c.example]\n    path: {template: \"/v1/orders/{orderId}\"}\n    methods: [POST]\n", toUpstream("api")),
	))
	ops, skips := Operations(v)
	want := []opKey{
		{"a-orders", "GET", "/v1/orders/{id}", "id", "a-orders", false},
		{"b-orders", "POST", "/v1/orders/{id}", "id", "b-orders", false},
	}
	if got := keysOf(ops); !slices.Equal(got, want) {
		t.Errorf("Operations() = %+v, want %+v", got, want)
	}
	var got []string
	for _, s := range skips {
		got = append(got, s.Route+": "+s.Reason)
	}
	wantSkips := []string{
		`b-orders: GET /v1/orders/{orderId} is already exported by Route "a-orders"`,
		`c-orders: POST /v1/orders/{orderId} is already exported by Route "b-orders"`,
	}
	if !slices.Equal(got, wantSkips) {
		t.Errorf("skips = %q, want %q", got, wantSkips)
	}
}

// TestOperationIDs: the Route name for a Route that exports one method,
// "<route>-<method>" for one that exports several, and "-2" after an
// operationId an earlier operation took.
func TestOperationIDs(t *testing.T) {
	v := validated(t, bundleOf(gatewayYAML(""), upstreamYAML("api", "http"),
		routeYAML("a", "    path: {exact: /a}\n    methods: [GET, POST]\n", toUpstream("api")),
		routeYAML("a-get", "    path: {exact: /b}\n    methods: [GET]\n", toUpstream("api")),
		routeYAML("c", "    path: {exact: /c}\n    methods: [GET, PURGE]\n", toUpstream("api")),
	))
	ops, _ := Operations(v)
	var ids []string
	for _, op := range ops {
		ids = append(ids, op.OperationID)
	}
	if want := []string{"a-get", "a-post", "a-get-2", "c"}; !slices.Equal(ids, want) {
		t.Errorf("operationIds = %q, want %q", ids, want)
	}
}

// bodyBundle returns a Bundle whose Route orders attaches the
// validation.json-schema Policies named in attach and runs whose schemas
// are first and second.
func bodyBundle(attach string) map[string]string {
	return bundleOf(gatewayYAML(""), upstreamYAML("orders", "http"),
		policyYAML("first", "validation.json-schema", `  slot: first
  config:
    schema:
      type: object
      required: [sku]
      properties:
        sku: {type: string, maxLength: 64}
        quantity: {type: integer, minimum: 1, maximum: 1e3}
`),
		policyYAML("second", "validation.json-schema", `  slot: second
  config:
    schema: {type: array}
`),
		routeYAML("orders", "    path: {exact: /v1/orders}\n    methods: [POST]\n", toUpstream("orders")+attach),
	)
}

// encodeJSON returns n written as JSON by internal/config/profile.
func encodeJSON(t *testing.T, n *tree.Node) string {
	t.Helper()
	var buf bytes.Buffer
	if err := profile.EncodeJSON(&buf, n, profile.EncodeOptions{Verbatim: true}); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestOperationsBody: the body schema is the config.schema of the first
// validation.json-schema Policy of the Route's effective client chain in
// execution order, as stage G normalized it: numbers exact, members
// sorted; a Route without one has none.
func TestOperationsBody(t *testing.T) {
	tests := []struct {
		name   string
		attach string
		want   string // the JSON of the body schema; "" for none
	}{
		{name: "no validation Policy", want: ""},
		{
			name:   "the Route's validation Policy",
			attach: "  policies:\n    - name: first\n",
			want: `{
  "properties": {
    "quantity": {
      "maximum": 1e3,
      "minimum": 1,
      "type": "integer"
    },
    "sku": {
      "maxLength": 64,
      "type": "string"
    }
  },
  "required": [
    "sku"
  ],
  "type": "object"
}
`,
		},
		{
			name:   "the first of two in execution order",
			attach: "  policies:\n    - name: second\n    - name: first\n",
			want:   "{\n  \"type\": \"array\"\n}\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := validated(t, bodyBundle(tc.attach))
			ops, skips := Operations(v)
			if len(ops) != 1 || len(skips) != 0 {
				t.Fatalf("Operations() = %+v, %+v; want one operation", ops, skips)
			}
			got := ""
			if ops[0].Body != nil {
				got = encodeJSON(t, ops[0].Body)
			}
			if got != tc.want {
				t.Errorf("body = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestOperationsShopBundle: of the example Bundle's Routes, the template
// Route reaching http Upstreams through composition exports its GET; the
// gRPC Route and the Route to the ai Upstream are skipped.
func TestOperationsShopBundle(t *testing.T) {
	v := validatedDir(t, examplesDir+"/shop-bundle")
	ops, skips := Operations(v)
	want := []opKey{{"orders-summary", "GET", "/v1/orders/{orderId}/summary", "orderId", "orders-summary", false}}
	if got := keysOf(ops); !slices.Equal(got, want) {
		t.Errorf("Operations() = %+v, want %+v", got, want)
	}
	var got []string
	for _, s := range skips {
		got = append(got, s.Route+": "+s.Reason)
	}
	wantSkips := []string{"cart-grpc: match.grpc has no OpenAPI operation", `support-chat: Upstream "llm" speaks ai, not http`}
	if !slices.Equal(got, wantSkips) {
		t.Errorf("skips = %q, want %q", got, wantSkips)
	}
}

// TestOperationsNoBundle: a call without a validated Bundle has no
// operation and no Skip.
func TestOperationsNoBundle(t *testing.T) {
	if ops, skips := Operations(nil); ops != nil || skips != nil {
		t.Errorf("Operations(nil) = %v, %v; want nil, nil", ops, skips)
	}
}

// TestHelpers covers the path and method helpers: shapes erase capture
// names, captures list them in order, a method outside the eight ranks
// last, and a method that is no token is quoted in messages.
func TestHelpers(t *testing.T) {
	if got := shapeOf("/v1/{a}/x/{b}/"); got != "/v1/{}/x/{}/" {
		t.Errorf("shapeOf = %q", got)
	}
	if got := captures("/v1/{a}/x/{b}"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("captures = %q", got)
	}
	if got := captures("/v1/{}/x"); got != nil {
		t.Errorf("captures of an empty brace pair = %q, want none", got)
	}
	if methodRank("TRACE") != 7 || methodRank("PURGE") != 8 {
		t.Errorf("methodRank(TRACE, PURGE) = %d, %d; want 7, 8", methodRank("TRACE"), methodRank("PURGE"))
	}
	for in, want := range map[string]string{"PURGE": "PURGE", "a b": `"a b"`, "": `""`} {
		if got := quoteMethod(in); got != want {
			t.Errorf("quoteMethod(%q) = %s, want %s", in, got, want)
		}
	}
	ids := map[string]bool{}
	if got := []string{unique(ids, "x"), unique(ids, "x"), unique(ids, "x")}; !slices.Equal(got, []string{"x", "x-2", "x-3"}) {
		t.Errorf("unique = %q", got)
	}
}
