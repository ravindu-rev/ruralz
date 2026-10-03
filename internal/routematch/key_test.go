// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import "testing"

func boolPtr(b bool) *bool { return &b }

func baseCriteria() *Criteria {
	return &Criteria{
		Hosts:   []string{"api.shop.example", "*.shop.example"},
		Path:    &PathCriteria{Template: "/v1/orders/{orderId}"},
		Methods: []string{"GET", "HEAD"},
		Headers: []HeaderCriteria{
			{Name: "X-Tenant", Exact: "eu"},
			{Name: "x-debug", Present: boolPtr(false)},
		},
		When: `request.query["v"] == "2"`,
	}
}

// TestMatchKey covers the RZ-CFG-023 identity (04 req 31; test plan item 5):
// equal for reordered hosts, methods and headers, different for any other
// change.
func TestMatchKey(t *testing.T) {
	base := MatchKey(baseCriteria())
	equal := map[string]func(c *Criteria){
		"hosts reordered":         func(c *Criteria) { c.Hosts = []string{"*.shop.example", "api.shop.example"} },
		"hosts case and dot":      func(c *Criteria) { c.Hosts = []string{"API.Shop.Example.", "*.SHOP.example"} },
		"hosts repeated":          func(c *Criteria) { c.Hosts = append(c.Hosts, "api.shop.example") },
		"methods reordered":       func(c *Criteria) { c.Methods = []string{"HEAD", "GET"} },
		"methods repeated":        func(c *Criteria) { c.Methods = []string{"GET", "HEAD", "GET"} },
		"headers reordered":       func(c *Criteria) { c.Headers[0], c.Headers[1] = c.Headers[1], c.Headers[0] },
		"header name case":        func(c *Criteria) { c.Headers[0].Name = "x-tenant" },
		"template literal escape": func(c *Criteria) { c.Path.Template = "/v1/%6Frders/{orderId}" },
		"ipv6 host spelling": func(c *Criteria) {
			c.Hosts = append(c.Hosts, "[2001:db8::1]")
			base6 := baseCriteria()
			base6.Hosts = append(base6.Hosts, "[2001:DB8:0::1]")
			if MatchKey(c) != MatchKey(base6) {
				t.Error("IPv6 host spellings differ in the key")
			}
			c.Hosts = c.Hosts[:2]
		},
		"nothing changed at all": func(*Criteria) {},
	}
	for name, change := range equal {
		c := baseCriteria()
		change(c)
		if got := MatchKey(c); got != base {
			t.Errorf("%s: key changed:\n%s\n%s", name, got, base)
		}
	}
	different := map[string]func(c *Criteria){
		"host added":            func(c *Criteria) { c.Hosts = append(c.Hosts, "b.example") },
		"host removed":          func(c *Criteria) { c.Hosts = c.Hosts[:1] },
		"no hosts":              func(c *Criteria) { c.Hosts = nil },
		"template param rename": func(c *Criteria) { c.Path.Template = "/v1/orders/{id}" },
		"path form":             func(c *Criteria) { c.Path = &PathCriteria{Prefix: "/v1/orders/{orderId}"} },
		"path exact":            func(c *Criteria) { c.Path = &PathCriteria{Exact: "/v1/orders/{orderId}"} },
		"path regex":            func(c *Criteria) { c.Path = &PathCriteria{Regex: "/v1/orders/{orderId}"} },
		"no path":               func(c *Criteria) { c.Path = nil },
		"method case":           func(c *Criteria) { c.Methods = []string{"get", "HEAD"} },
		"method removed":        func(c *Criteria) { c.Methods = []string{"GET"} },
		"no methods":            func(c *Criteria) { c.Methods = nil },
		"header value":          func(c *Criteria) { c.Headers[0].Exact = "us" },
		"header regex":          func(c *Criteria) { c.Headers[0] = HeaderCriteria{Name: "x-tenant", Regex: "eu"} },
		"header present true":   func(c *Criteria) { c.Headers[1].Present = boolPtr(true) },
		"header present unset":  func(c *Criteria) { c.Headers[1].Present = nil },
		"header removed":        func(c *Criteria) { c.Headers = c.Headers[:1] },
		"grpc":                  func(c *Criteria) { c.GRPC = &GRPCCriteria{Service: "shop.v1.Orders"} },
		"graphql":               func(c *Criteria) { c.GraphQL = &GraphQLCriteria{OperationType: "query"} },
		"topic":                 func(c *Criteria) { c.Topic = "orders" },
		"when":                  func(c *Criteria) { c.When = `request.query["v"]=="2"` },
		"no when":               func(c *Criteria) { c.When = "" },
	}
	// Spellings of one path that the Router sees as one normalized path
	// share a key (04 req 23, 31), so RZ-CFG-023 fires for them.
	samePath := []struct{ a, b PathCriteria }{
		{PathCriteria{Exact: "/a/./b"}, PathCriteria{Exact: "/a/b"}},
		{PathCriteria{Exact: "/a/c/../b"}, PathCriteria{Exact: "/a/b"}},
		{PathCriteria{Exact: "/%7Ea"}, PathCriteria{Exact: "/~a"}},
		{PathCriteria{Exact: "/a%2fb"}, PathCriteria{Exact: "/a%2Fb"}},
		{PathCriteria{Prefix: "/%7Ea"}, PathCriteria{Prefix: "/~a"}},
		{PathCriteria{Prefix: "/v1/.."}, PathCriteria{Prefix: "/"}},
		{PathCriteria{Prefix: "/a b"}, PathCriteria{Prefix: "/a%20b"}},
		{PathCriteria{Template: "/%7e/{x}"}, PathCriteria{Template: "/~/{x}"}},
		{PathCriteria{Template: "/a%2f%41/{x}/"}, PathCriteria{Template: "/a%2FA/{x}/"}},
	}
	for _, tc := range samePath {
		a, b := tc.a, tc.b
		if ka, kb := MatchKey(&Criteria{Path: &a}), MatchKey(&Criteria{Path: &b}); ka != kb {
			t.Errorf("equivalent paths differ:\n%s\n%s", ka, kb)
		}
	}
	// Different normalized paths, a different form, a renamed parameter and
	// an unparsable value (kept verbatim) stay distinct.
	distinctPaths := []PathCriteria{
		{Exact: "/a/b"},
		{Exact: "/a/b/"},
		{Exact: "/a//b"},
		{Exact: "/a%2Fb"},
		{Exact: "/A/b"},
		{Prefix: "/a/b"},
		{Template: "/a/b"},
		{Regex: "/a/b"},
		{Regex: "/a/./b"},
		{Template: "/a/{x}"},
		{Template: "/a/{y}"},
		{Template: "/a/{x}/"},
		{Exact: "/%zz"},
		{Exact: "relative"},
		{Template: "/a/{x"},
		{Exact: "/a%0A"},
	}
	seenPath := map[Key]PathCriteria{}
	for _, p := range distinctPaths {
		k := MatchKey(&Criteria{Path: &p})
		if prev, dup := seenPath[k]; dup {
			t.Errorf("paths %+v and %+v share key %s", prev, p, k)
		}
		seenPath[k] = p
	}
	seen := map[Key]string{base: "base"}
	for name, change := range different {
		c := baseCriteria()
		change(c)
		got := MatchKey(c)
		if prev, dup := seen[got]; dup {
			t.Errorf("%s: key equals that of %s: %s", name, prev, got)
		}
		seen[got] = name
	}
	grpc := &Criteria{GRPC: &GRPCCriteria{Service: "s", Method: "m"}}
	if MatchKey(grpc) == MatchKey(&Criteria{GRPC: &GRPCCriteria{Service: "s"}}) {
		t.Error("gRPC method ignored")
	}
	gql := &Criteria{GraphQL: &GraphQLCriteria{OperationName: "a"}}
	if MatchKey(gql) == MatchKey(&Criteria{GraphQL: &GraphQLCriteria{OperationName: "b"}}) {
		t.Error("GraphQL operation name ignored")
	}
}

func TestMatchKeyText(t *testing.T) {
	c := &Criteria{
		Hosts:   []string{"B.example", "a.example", "bad host"},
		Path:    &PathCriteria{Prefix: "/v1"},
		Methods: []string{"GET"},
		Headers: []HeaderCriteria{{Name: "X-A", Exact: "1"}},
		When:    "true",
	}
	want := `hosts=["a.example","b.example","bad host"] path=("","/v1","","") methods=["GET"] ` +
		`headers=[("x-a","1","","-")] grpc=- graphql=- topic="" when="true"`
	if got := MatchKey(c).String(); got != want {
		t.Errorf("MatchKey =\n%s\nwant\n%s", got, want)
	}
	if MatchKey(&Criteria{}) != MatchKey(&Criteria{Hosts: []string{}, Methods: []string{}, Headers: []HeaderCriteria{}}) {
		t.Error("nil and empty lists differ")
	}
}
