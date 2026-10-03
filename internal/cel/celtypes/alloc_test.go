// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Tests for the WP-08 "Done when": every expr.Vars field is reachable from
// CEL with its documented type (03 req 17), and the adapters allocate
// nothing for fields an expression does not select (03 req 19, 20, 23).

// celFields maps each view's Go fields to the CEL field that reads it.
// Prepared is the CEL implementation's cache, not a variable field.
func celFields() map[reflect.Type]map[string]string {
	return map[reflect.Type]map[string]string{
		reflect.TypeFor[expr.Vars](): {
			"Request": "request", "Source": "source", "Route": "route", "Consumer": "consumer",
			"Auth": "auth", "Response": "response", "Error": "error", "Upstream": "upstream",
			"Attempt": "attempt", "Steps": "steps", "AI": "ai", "Now": "now", "Duration": "duration",
		},
		reflect.TypeFor[expr.Request](): {
			"Method": "method", "Scheme": "scheme", "Host": "host", "Path": "path",
			"PathParams": "pathParams", "Header": "headers", "Body": "body",
		},
		reflect.TypeFor[expr.Source]():       {"IP": "ip", "Port": "port", "TLSVersion": "tlsVersion", "ClientCertSubject": "clientCertSubject"},
		reflect.TypeFor[expr.Route]():        {"Name": "name", "Labels": "labels", "Prepared": ""},
		reflect.TypeFor[expr.Consumer]():     {"Name": "name", "Tier": "tier", "Tags": "tags", "Labels": "labels", "Quotas": "quotas", "Prepared": ""},
		reflect.TypeFor[expr.Auth]():         {"Method": "method", "Claims": "claims"},
		reflect.TypeFor[expr.Response]():     {"Status": "status", "Header": "headers", "Body": "body"},
		reflect.TypeFor[expr.AttemptError](): {"Kind": "kind"},
		reflect.TypeFor[expr.Upstream]():     {"Name": "name", "Endpoint": "endpoint"},
		reflect.TypeFor[expr.Step]():         {"Status": "status", "Header": "headers", "Body": "body"},
		reflect.TypeFor[expr.AI]():           {"Model": "model", "EstimatedInputTokens": "estimatedInputTokens", "MaxOutputTokens": "maxOutputTokens", "Stream": "stream"},
	}
}

// TestDoneWhenEveryFieldReachable fails when an expr view gains a field no
// CEL field reads, and checks every mapped CEL field is declared (the
// request query, a method, is request.query).
func TestDoneWhenEveryFieldReachable(t *testing.T) {
	p := newProvider(t)
	typeOf := map[reflect.Type]string{
		reflect.TypeFor[expr.Request](): TypeRequest, reflect.TypeFor[expr.Source](): TypeSource,
		reflect.TypeFor[expr.Route](): TypeRoute, reflect.TypeFor[expr.Consumer](): TypeConsumer,
		reflect.TypeFor[expr.Auth](): TypeAuth, reflect.TypeFor[expr.Response](): TypeResponse,
		reflect.TypeFor[expr.AttemptError](): TypeError, reflect.TypeFor[expr.Upstream](): TypeUpstream,
		reflect.TypeFor[expr.Step](): TypeStep, reflect.TypeFor[expr.AI](): TypeAI,
	}
	declared := map[string]bool{}
	for _, v := range Variables(allVars) {
		declared[v.Name] = true
	}
	for rt, fields := range celFields() {
		for i := range rt.NumField() {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			celName, ok := fields[f.Name]
			if !ok {
				t.Errorf("%s.%s has no CEL field", rt.Name(), f.Name)
				continue
			}
			if celName == "" {
				continue
			}
			if rt == reflect.TypeFor[expr.Vars]() {
				if !declared[celName] {
					t.Errorf("variable %s is not declared", celName)
				}
				continue
			}
			if _, ok := p.FindStructFieldType(typeOf[rt], celName); !ok {
				t.Errorf("%s.%s is not declared", typeOf[rt], celName)
			}
		}
	}
	if _, ok := p.FindStructFieldType(TypeRequest, "query"); !ok {
		t.Error("request.query is not declared")
	}
}

// TestDoneWhenResolveAllocatesNothing checks that resolving a variable wraps
// the view pointer without allocating.
func TestDoneWhenResolveAllocatesNothing(t *testing.T) {
	a := NewActivation(fullVars())
	empty := NewActivation(&expr.Vars{})
	for _, name := range []string{"request", "source", "route", "consumer", "auth", "response", "error", "upstream", "attempt", "steps", "ai"} {
		for _, act := range []*Activation{a, empty} {
			if n := testing.AllocsPerRun(100, func() { _, _ = act.ResolveName(name) }); n != 0 {
				t.Errorf("ResolveName(%s) allocates %v", name, n)
			}
		}
	}
	if _, ok := a.ResolveName("self"); ok {
		t.Error("self resolves in M1")
	}
	if a.Parent() != nil {
		t.Error("Parent is not nil")
	}
}

// TestDoneWhenContainerFieldsAllocateNothing checks that selecting a map,
// list or body field wraps a pointer into the view without converting it.
func TestDoneWhenContainerFieldsAllocateNothing(t *testing.T) {
	v := fullVars()
	prepared := fullVars()
	PrepareRoute(prepared.Route)
	PrepareConsumer(prepared.Consumer)
	st, _ := v.Steps.Get("order")
	noClaims := &expr.Auth{Method: "api-key"}
	cases := []struct {
		name   string
		k      objKind
		i      int
		target any
	}{
		{"request.pathParams", kindRequest, reqPathParams, v.Request},
		{"request.query", kindRequest, reqQuery, v.Request},
		{"request.headers", kindRequest, reqHeaders, v.Request},
		{"request.body", kindRequest, reqBody, v.Request},
		{"route.labels", kindRoute, routeLabels, v.Route},
		{"prepared route.labels", kindRoute, routeLabels, prepared.Route},
		{"consumer.tags", kindConsumer, consumerTags, v.Consumer},
		{"consumer.labels", kindConsumer, consumerLabels, v.Consumer},
		{"prepared consumer.labels", kindConsumer, consumerLabels, prepared.Consumer},
		{"consumer.quotas", kindConsumer, consumerQuotas, v.Consumer},
		{"auth.claims", kindAuth, authClaims, v.Auth},
		{"empty auth.claims", kindAuth, authClaims, noClaims},
		{"response.headers", kindResponse, respHeaders, v.Response},
		{"response.body", kindResponse, respBody, v.Response},
		{"steps.x.headers", kindStep, stepHeaders, &st},
		{"steps.x.body", kindStep, stepBody, &st},
		{"source.port (small)", kindSource, srcPort, &expr.Source{Port: 80}},
		{"source.tlsVersion", kindSource, srcTLSVersion, v.Source},
		{"ai.stream", kindAI, aiStream, v.AI},
	}
	for _, tc := range cases {
		if n := testing.AllocsPerRun(100, func() { _, _ = getField(tc.k, tc.i, tc.target) }); n != 0 {
			t.Errorf("%s allocates %v", tc.name, n)
		}
	}
	// A scalar field boxes its value once, nothing more.
	if n := testing.AllocsPerRun(100, func() { _, _ = getField(kindRequest, reqMethod, v.Request) }); n > 1 {
		t.Errorf("request.method allocates %v", n)
	}
	if n := testing.AllocsPerRun(100, func() { _, _ = getField(kindResponse, respStatus, v.Response) }); n > 1 {
		t.Errorf("response.status allocates %v", n)
	}
}

// TestReq19LookupAllocatesNothing checks the header lookup of a
// single-valued field and the size of a canonical map (03 req 19).
func TestReq19LookupAllocatesNothing(t *testing.T) {
	h := http.Header{}
	for i := range 40 {
		h.Set(fmt.Sprintf("X-Header-%d", i), "v")
	}
	h.Set("X-Tenant", "acme")
	for _, name := range []string{"x-tenant", "X-TENANT", "X-Tenant", "x-api-key", "x-header-17"} {
		if n := testing.AllocsPerRun(100, func() { _, _ = joinedHeader(h, name) }); n != 0 {
			t.Errorf("joinedHeader(%s) allocates %v", name, n)
		}
	}
	for _, hideHost := range []bool{false, true} {
		if n := testing.AllocsPerRun(100, func() { _ = headerSize(h, hideHost) }); n != 0 {
			t.Errorf("headerSize(%v) allocates %v", hideHost, n)
		}
	}
	key := types.String("x-api-key")
	for _, m := range []traits.Container{
		mapView[headerSource]{headerSource{&h}},
		mapView[requestHeaderSource]{requestHeaderSource{&h}},
	} {
		if n := testing.AllocsPerRun(100, func() { _ = m.Contains(key) }); n != 0 {
			t.Errorf("%T Contains allocates %v", m, n)
		}
	}
}

// TestReq20QueryParsedLazily checks that request.query is parsed on its
// first selection only: an expression that does not select it leaves the
// raw query unparsed.
func TestReq20QueryParsedLazily(t *testing.T) {
	parsed := func(r *expr.Request) bool {
		f := reflect.ValueOf(r).Elem().FieldByName("query")
		if !f.IsValid() || f.NumField() == 0 {
			t.Skip("expr.Request keeps its query cache elsewhere")
		}
		v := f.FieldByName("v")
		if !v.IsValid() {
			t.Skip("atomic.Pointer layout changed")
		}
		return v.Pointer() != 0
	}
	env := testEnv(t)
	v := fullVars()
	v.Request.SetRawQuery("a=1&b=2")
	for _, src := range []string{`request.method == "GET"`, `request.headers["x-tenant"] == "acme"`, `has(request.body)`, `request.pathParams.orderId`} {
		if _, _, err := compile(t, env, src).Eval(NewActivation(v)); err != nil {
			t.Fatal(err)
		}
		if parsed(v.Request) {
			t.Fatalf("%s parsed the query", src)
		}
	}
	if _, _, err := compile(t, env, `request.query.a == "1"`).Eval(NewActivation(v)); err != nil {
		t.Fatal(err)
	}
	if !parsed(v.Request) {
		t.Error("request.query did not parse the query")
	}
}

// TestDoneWhenUnusedFieldsAllocateNothing evaluates expressions over a
// minimal activation and over one with every field set and large maps,
// query and body: the allocation counts are equal, so fields an expression
// does not select cost nothing.
func TestDoneWhenUnusedFieldsAllocateNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts of whole evaluations are noisy under -race (sync.Pool)")
	}
	heavy := fullVars()
	for i := range 64 {
		heavy.Request.Header.Set(fmt.Sprintf("X-Extra-%d", i), strings.Repeat("v", 100))
		heavy.Response.Header.Set(fmt.Sprintf("X-Extra-%d", i), "v")
	}
	heavy.Request.SetRawQuery(strings.Repeat("k=v&", 64))
	big := map[string]any{}
	for i := range 256 {
		big[fmt.Sprintf("k%d", i)] = []any{"x", jsonNum("1")}
	}
	heavy.Request.Body = FromNative(big)
	minimal := func() *expr.Vars {
		f := fullVars()
		return &expr.Vars{
			Request:  &expr.Request{Method: f.Request.Method, Header: http.Header{"X-Tenant": {"acme"}}},
			Source:   &expr.Source{IP: f.Source.IP},
			Route:    &expr.Route{Name: f.Route.Name},
			Consumer: &expr.Consumer{Name: f.Consumer.Name, Tier: f.Consumer.Tier, Quotas: f.Consumer.Quotas},
			Response: &expr.Response{Status: f.Response.Status},
			Error:    f.Error,
		}
	}()
	env := testEnv(t)
	for _, src := range []string{
		`request.method == "GET"`,
		`request.headers["x-tenant"] == "acme"`,
		`"x-api-key" in request.headers`,
		`consumer != null && consumer.tier == "gold"`,
		`consumer == null ? source.ip : consumer.name`,
		`consumer != null && "daily-tokens" in consumer.quotas`,
		`route.name == "orders"`,
		expr.DefaultFailureWhen,
	} {
		prg := compile(t, env, src)
		lo, hi := NewActivation(minimal), NewActivation(heavy)
		for _, a := range []*Activation{lo, hi} {
			if out, _, err := prg.Eval(a); err != nil || types.IsError(out) {
				t.Fatalf("%s: %v %v", src, out, err)
			}
		}
		nLo := testing.AllocsPerRun(200, func() { _, _, _ = prg.Eval(lo) })
		nHi := testing.AllocsPerRun(200, func() { _, _, _ = prg.Eval(hi) })
		if nLo != nHi {
			t.Errorf("%s: %v allocations over a minimal activation, %v over a full one", src, nLo, nHi)
		}
	}
}

// TestValuesHashable checks that every value the package makes can be a Go
// map key: cel-go hashes values in map literals and set membership.
func TestValuesHashable(t *testing.T) {
	v := fullVars()
	a := NewActivation(v)
	set := map[ref.Val]bool{}
	for _, name := range []string{"request", "source", "route", "consumer", "auth", "response", "error", "upstream", "attempt", "steps", "ai", "now", "duration"} {
		val, _ := a.ResolveName(name)
		set[val.(ref.Val)] = true
	}
	for k := range numKinds {
		for i := range objectFields(k) {
			var target any
			switch k {
			case kindRequest:
				target = v.Request
			case kindSource:
				target = v.Source
			case kindRoute:
				target = v.Route
			case kindConsumer:
				target = v.Consumer
			case kindAuth:
				target = v.Auth
			case kindResponse:
				target = v.Response
			case kindError:
				target = v.Error
			case kindUpstream:
				target = v.Upstream
			case kindStep:
				st, _ := v.Steps.Get("order")
				target = &st
			default:
				target = v.AI
			}
			if val, err := getField(k, i, target); err == nil {
				set[val.(ref.Val)] = true
			}
		}
	}
	set[nativeVal(map[string]any{"a": []any{}})] = true
	set[nativeVal([]any{map[string]any{}})] = true
	set[sortedLabels(map[string]string{"a": "b"})] = true
	if len(set) < 40 {
		t.Errorf("only %d distinct values", len(set))
	}
	// Through cel-go: map literal keys and set membership over views.
	runEval(t, []evalCase{
		{name: "set membership", src: `dyn(request.headers) in ["a", "b"]`, want: false},
		{name: "map literal key", src: `{dyn(request.headers): 1, dyn(request): 2, dyn(request.body.items): 3}.size()`, want: 3},
	})
}

// TestPrepare checks that prepared and lazy views agree and that Prepare
// tolerates nil and foreign caches.
func TestPrepare(t *testing.T) {
	PrepareRoute(nil)
	PrepareConsumer(nil)
	r := &expr.Route{Name: "r", Labels: map[string]string{"b": "2", "a": "1"}}
	lazy, _ := getField(kindRoute, routeLabels, r)
	PrepareRoute(r)
	if _, ok := r.Prepared.(*prepared); !ok {
		t.Fatalf("Prepared = %T", r.Prepared)
	}
	ready, _ := getField(kindRoute, routeLabels, r)
	if lazy.(ref.Val).Equal(ready.(ref.Val)) != types.True || !slices.Equal(iterKeys(ready.(mapView[*sortedStrings])), []string{"a", "b"}) {
		t.Error("prepared labels differ")
	}
	c := &expr.Consumer{Name: "c", Prepared: "foreign"}
	if v, _ := getField(kindConsumer, consumerLabels, c); v.(ref.Val).Type() != types.MapType {
		t.Error("foreign Prepared not ignored")
	}
	PrepareConsumer(c)
	if v, _ := getField(kindConsumer, consumerLabels, c); v.(ref.Val).Equal(types.NewStringStringMap(types.DefaultTypeAdapter, nil)) != types.True {
		t.Error("prepared nil labels are not an empty map")
	}
}
