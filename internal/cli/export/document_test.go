// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package export

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// goldenBundle is the Bundle of the golden files: Gateway edge with the
// listeners secure and web and the Gateway Policy cors-default; Routes
// orders (a template with GET and POST, an api key and a body schema),
// items (a template under it), health (an exact path without methods,
// excluding cors-default), assets (a prefix path) and feed (to a
// websocket Upstream); Upstream orders with the leg Policy
// headers-internal, and the unattached Upstream spare and Policy office.
func goldenBundle() map[string]string {
	return bundleOf(gatewayYAML("  policies:\n    - name: cors-default\n"),
		policyYAML("cors-default", "cors", "  config:\n    allowOrigins: [\"https://shop.example\"]\n    allowMethods: [GET, POST]\n"),
		policyYAML("apikey", "auth.api-key", "  config: {}\n"),
		policyYAML("validate-order", "validation.json-schema", `  config:
    schema:
      type: object
      required: [sku]
      properties:
        sku: {type: string, pattern: "^[A-Z]{3}-[0-9]+$"}
        quantity: {type: integer, minimum: 1}
`),
		policyYAML("headers-internal", "headers", "  config:\n    request:\n      set:\n        - {name: x-internal, value: \"1\"}\n"),
		policyYAML("office", "authz.ip", "  config:\n    allow: [10.0.0.0/8]\n"),
		`apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: orders
spec:
  protocol: http
  endpoints:
    - address: orders.internal:80
  policies:
    - name: headers-internal
`,
		upstreamYAML("feed", "websocket"),
		upstreamYAML("spare", "http"),
		routeYAML("orders", "    path: {template: \"/v1/orders/{orderId}\"}\n    methods: [GET, POST]\n",
			"  policies:\n    - name: apikey\n    - name: validate-order\n"+toUpstream("orders")),
		routeYAML("items", "    path: {template: \"/v1/orders/{orderId}/items\"}\n    methods: [GET]\n", toUpstream("orders")),
		routeYAML("health", "    path: {exact: /healthz}\n", "  excludePolicies:\n    - name: cors-default\n"+toUpstream("orders")),
		routeYAML("assets", "    path: {prefix: /assets}\n", toUpstream("spare")),
		routeYAML("feed", "    path: {exact: /feed}\n", toUpstream("feed")),
	)
}

// encodeYAML returns n written as YAML by internal/config/profile with
// strings verbatim.
func encodeYAML(t *testing.T, n *tree.Node) string {
	t.Helper()
	var buf bytes.Buffer
	if err := profile.EncodeWith(&buf, []*tree.Node{n}, profile.EncodeOptions{Verbatim: true}); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestOpenAPIGolden pins the OpenAPI document of the golden Bundle in
// both forms: paths sorted bytewise, each path item's operations in
// method order, one path parameter per capture, the body schema as a
// required JSON request body, and info naming the Gateway and the
// Revision.
func TestOpenAPIGolden(t *testing.T) {
	v := validated(t, goldenBundle())
	ops, _ := Operations(v)
	doc := OpenAPI(v, ops)
	golden(t, "openapi.yaml", encodeYAML(t, doc))
	golden(t, "openapi.json", encodeJSON(t, doc))
	if got, want := encodeYAML(t, doc), "  version: "+v.Revision.Digest.Short()+"\n"; !strings.Contains(got, want) {
		t.Errorf("info.version is not the Revision %s:\n%s", v.Revision.Digest.Short(), got)
	}
}

// TestOpenAPIParsesBack: both forms parse back through
// internal/config/profile, YAML and JSON alike, to one document whose
// openapi member is 3.1.0 and whose paths hold the operations.
func TestOpenAPIParsesBack(t *testing.T) {
	v := validated(t, goldenBundle())
	ops, _ := Operations(v)
	doc := OpenAPI(v, ops)
	for _, tc := range []struct {
		name   string
		text   string
		format profile.Format
	}{
		{"yaml", encodeYAML(t, doc), profile.FormatYAML},
		{"json", encodeJSON(t, doc), profile.FormatJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs, ds, err := profile.Parse(t.Context(), []byte(tc.text), 0, "openapi."+tc.name, tc.format, profile.Options{})
			if err != nil || len(ds) > 0 || len(docs) != 1 {
				t.Fatalf("Parse = %d documents, %v, %v", len(docs), ds, err)
			}
			root := docs[0].Root
			if v, ok := root.Get("openapi"); !ok || v.Kind != tree.KindString || v.Text != OpenAPIVersion {
				t.Errorf("openapi = %+v, want the string %s", v, OpenAPIVersion)
			}
			paths, _ := root.Get("paths")
			var keys []string
			for _, m := range paths.Members {
				keys = append(keys, m.Key)
			}
			if want := []string{"/healthz", "/v1/orders/{orderId}", "/v1/orders/{orderId}/items"}; !slices.Equal(keys, want) {
				t.Errorf("paths = %q, want %q", keys, want)
			}
			item, _ := paths.Get("/healthz")
			var verbs []string
			for _, m := range item.Members {
				verbs = append(verbs, m.Key)
			}
			if want := []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}; !slices.Equal(verbs, want) {
				t.Errorf("/healthz operations = %q, want %q", verbs, want)
			}
		})
	}
}

// TestOpenAPIEmpty: a Bundle without an HTTP operation exports a document
// whose paths are empty.
func TestOpenAPIEmpty(t *testing.T) {
	v := validated(t, bundleOf(gatewayYAML(""), upstreamYAML("spare", "http"),
		routeYAML("assets", "    path: {prefix: /assets}\n", toUpstream("spare"))))
	ops, skips := Operations(v)
	if len(ops) != 0 || len(skips) != 1 {
		t.Fatalf("Operations() = %v, %v; want no operation and one Skip", ops, skips)
	}
	want := "openapi: \"3.1.0\"\ninfo:\n  title: edge\n  version: " + v.Revision.Digest.Short() + "\npaths: {}\n"
	if got := encodeYAML(t, OpenAPI(v, ops)); got != want {
		t.Errorf("OpenAPI() =\n%s\nwant\n%s", got, want)
	}
}

// TestOpenAPIBodyIsACopy: the document's body schema is a copy, so
// changing the document leaves the validated Bundle as it was.
func TestOpenAPIBodyIsACopy(t *testing.T) {
	v := validated(t, goldenBundle())
	ops, _ := Operations(v)
	before := encodeJSON(t, bodySchema(v, "orders"))
	doc := OpenAPI(v, ops)
	paths, _ := doc.Get("paths")
	item, _ := paths.Get("/v1/orders/{orderId}")
	post, _ := item.Get("post")
	body, _ := post.Get("requestBody")
	content, _ := body.Get("content")
	media, _ := content.Get("application/json")
	schema, _ := media.Get("schema")
	schema.Members = nil
	if after := encodeJSON(t, bodySchema(v, "orders")); after != before {
		t.Errorf("changing the document changed the validated Bundle:\n%s\nwas\n%s", after, before)
	}
}

// TestPostmanGolden pins the Postman collection of the golden Bundle: one
// item per operation in the document's order, :name captures in the raw
// URL, the path segments and url.variable, a JSON body for the operation
// with a body schema, and baseUrl from the first listener by name.
func TestPostmanGolden(t *testing.T) {
	v := validated(t, goldenBundle())
	ops, _ := Operations(v)
	out, err := Postman(v, ops)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "postman.json", string(out))
}

// TestPostmanShape decodes the collection: its schema, its name, the
// items in the document's order and the collection variable baseUrl of
// the first listener by name, secure (https on 8443) before web.
func TestPostmanShape(t *testing.T) {
	v := validated(t, goldenBundle())
	ops, _ := Operations(v)
	out, err := Postman(v, ops)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Info struct{ Name, Schema string }
		Item []struct {
			Name    string
			Request struct {
				Method string
				Header []map[string]string
				Body   map[string]any
				URL    struct {
					Raw      string
					Host     []string
					Path     []string
					Variable []map[string]string
				}
			}
		}
		Variable []map[string]string
	}
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatalf("the collection is not JSON: %v\n%s", err, out)
	}
	if c.Info.Name != "edge" || c.Info.Schema != PostmanSchema {
		t.Errorf("info = %+v", c.Info)
	}
	var names []string
	for _, it := range c.Item {
		names = append(names, it.Name+" "+it.Request.Method+" "+it.Request.URL.Raw)
	}
	want := []string{
		"health-get GET {{baseUrl}}/healthz", "health-put PUT {{baseUrl}}/healthz", "health-post POST {{baseUrl}}/healthz",
		"health-delete DELETE {{baseUrl}}/healthz", "health-options OPTIONS {{baseUrl}}/healthz",
		"health-head HEAD {{baseUrl}}/healthz", "health-patch PATCH {{baseUrl}}/healthz", "health-trace TRACE {{baseUrl}}/healthz",
		"orders-get GET {{baseUrl}}/v1/orders/:orderId", "orders-post POST {{baseUrl}}/v1/orders/:orderId",
		"items GET {{baseUrl}}/v1/orders/:orderId/items",
	}
	if !slices.Equal(names, want) {
		t.Errorf("items = %q, want %q", names, want)
	}
	post := c.Item[9].Request
	if !slices.Equal(post.URL.Path, []string{"v1", "orders", ":orderId"}) || len(post.URL.Variable) != 1 ||
		post.URL.Variable[0]["key"] != "orderId" || post.Body["mode"] != "raw" || len(post.Header) != 1 {
		t.Errorf("orders-post request = %+v", post)
	}
	if get := c.Item[0].Request; get.Body != nil || get.Header != nil || get.URL.Variable != nil {
		t.Errorf("health-get carries a body, header or variable: %+v", get)
	}
	if len(c.Variable) != 1 || c.Variable[0]["key"] != "baseUrl" || c.Variable[0]["value"] != "https://localhost:8443" {
		t.Errorf("variable = %v, want baseUrl https://localhost:8443 (secure sorts before web)", c.Variable)
	}
}

// TestPostmanRootPath: the path / is the raw URL {{baseUrl}}/ with one
// empty path segment, as Postman writes a trailing slash.
func TestPostmanRootPath(t *testing.T) {
	r := postmanRequest(Operation{Method: "GET", Path: "/"})
	if r.URL.Raw != "{{baseUrl}}/" || !slices.Equal(r.URL.Path, []string{""}) {
		t.Errorf("postmanRequest(/) = %+v", r.URL)
	}
}

// TestDOTGolden pins the DOT graph of the golden Bundle, whole and of the
// Route orders.
func TestDOTGolden(t *testing.T) {
	v := validated(t, goldenBundle())
	for _, tc := range []struct{ name, route string }{{"dot.txt", ""}, {"dot-orders.txt", "orders"}} {
		out, err := DOT(v, tc.route)
		if err != nil {
			t.Fatalf("DOT(%q): %v", tc.route, err)
		}
		golden(t, tc.name, string(out))
	}
}

// TestDOTWhole: the whole graph holds every Route, Policy and Upstream,
// unattached ones as lone nodes; each Route's edges reach the Policies of
// its effective client chain, the inherited and replaced ones as the
// chain has them, then its Upstreams; each Upstream's edges reach its
// spec.policies.
func TestDOTWhole(t *testing.T) {
	v := validated(t, goldenBundle())
	out, err := DOT(v, "")
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		`"Policy/office" [shape=ellipse, label="Policy\noffice\nauthz.ip"];`,
		`"Upstream/spare" [shape=cylinder, label="Upstream\nspare"];`,
		`"Route/orders" -> "Policy/cors-default";`,
		`"Route/orders" -> "Policy/apikey";`,
		`"Route/orders" -> "Policy/validate-order";`,
		`"Route/orders" -> "Upstream/orders";`,
		`"Upstream/orders" -> "Policy/headers-internal";`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("DOT lacks %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"Route/health" -> "Policy/cors-default"`) {
		t.Errorf("health excludes cors-default, yet has an edge to it:\n%s", text)
	}
	if strings.Contains(text, `"Policy/office" ->`) || strings.Contains(text, `-> "Policy/office"`) {
		t.Errorf("the unattached office has an edge:\n%s", text)
	}
}

// TestDOTRoute: the subgraph of one Route holds that Route, the Policies
// of its client chain and legs, the Upstreams it reaches, and those
// Upstreams' leg Policies, and no other node.
func TestDOTRoute(t *testing.T) {
	v := validated(t, goldenBundle())
	out, err := DOT(v, "health")
	if err != nil {
		t.Fatal(err)
	}
	want := `digraph "edge" {
  rankdir=LR;
  node [shape=box];
  "Route/health" [label="Route\nhealth"];
  "Policy/headers-internal" [shape=ellipse, label="Policy\nheaders-internal\nheaders"];
  "Upstream/orders" [shape=cylinder, label="Upstream\norders"];
  "Route/health" -> "Upstream/orders";
  "Upstream/orders" -> "Policy/headers-internal";
}
`
	if string(out) != want {
		t.Errorf("DOT(health) =\n%s\nwant\n%s", out, want)
	}
}

// TestDOTUnknownRoute: a Route the Bundle lacks is ErrUnknownRoute, with
// the nearest Route name as a hint when one is near.
func TestDOTUnknownRoute(t *testing.T) {
	v := validated(t, goldenBundle())
	for _, tc := range []struct{ route, want string }{
		{"ordrs", `unknown Route "ordrs" (did you mean "orders"?)`},
		{"zzzzzzzzzzzz", `unknown Route "zzzzzzzzzzzz"`},
		{strings.Repeat("x", 70), `unknown Route "` + strings.Repeat("x", 64) + `..."`},
	} {
		_, err := DOT(v, tc.route)
		if !errors.Is(err, ErrUnknownRoute) || err.Error() != tc.want {
			t.Errorf("DOT(%q) = %v, want %s", tc.route, err, tc.want)
		}
	}
	if _, err := DOT(nil, ""); !errors.Is(err, ErrInput) {
		t.Errorf("DOT(nil) = %v, want ErrInput", err)
	}
}

// TestDOTQuoting: quoted strings escape '"' and '\', and a label joins
// its escaped lines with the DOT line break \n.
func TestDOTQuoting(t *testing.T) {
	if got := quote(`a"b\c`); got != `"a\"b\\c"` {
		t.Errorf("quote = %s", got)
	}
	if got := label("Route", `x"y`, `z\n`); got != `"Route\nx\"y\nz\\n"` {
		t.Errorf("label = %s", got)
	}
}

// TestShopRestBundle exports the REST example Bundle, read in place: both
// Routes export their GET, the Route of the partners replacing the
// Gateway's JWT Policy with its api key Policy in the DOT edges, and the
// composition Route reaching both Upstreams.
func TestShopRestBundle(t *testing.T) {
	v := validatedDir(t, examplesDir+"/shop-rest-bundle")
	ops, skips := Operations(v)
	if got := keysOf(ops); len(skips) != 0 || !slices.Equal(got, []opKey{
		{"orders-summary", "GET", "/v1/orders/{orderId}/summary", "orderId", "orders-summary", false},
		{"partner-orders", "GET", "/partner/v1/orders/{orderId}", "orderId", "partner-orders", false},
	}) {
		t.Errorf("Operations() = %+v, %+v", got, skips)
	}
	out, err := DOT(v, "partner-orders")
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		`"Route/partner-orders" -> "Policy/apikey-partner";`,
		`"Route/partner-orders" -> "Upstream/orders";`,
		`"Upstream/orders" -> "Policy/upstream-oauth";`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("DOT(partner-orders) lacks %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"Policy/jwt-default"`) || strings.Contains(text, `"Policy/cors-default"`) {
		t.Errorf("DOT(partner-orders) holds a Policy its chain removed:\n%s", text)
	}
	whole, err := DOT(v, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"Route/orders-summary" -> "Upstream/orders";`, `"Route/orders-summary" -> "Upstream/inventory";`} {
		if !strings.Contains(string(whole), want) {
			t.Errorf("DOT lacks %s:\n%s", want, whole)
		}
	}
	coll, err := Postman(v, ops)
	if err != nil || !strings.Contains(string(coll), `"raw": "{{baseUrl}}/partner/v1/orders/:orderId"`) {
		t.Errorf("Postman() = %s, %v", coll, err)
	}
}
