// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"net/http"
	"testing"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Tests that the values the authz.cel stage reads most convert without
// allocating (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Per-stage latency budget": authz.cel, 0 allocations), and convert to the
// same values as before.

// TestPreparedFieldsAllocateNothing checks that every field of a prepared
// Route and Consumer, a label lookup and a list element read the value the
// snapshot converted, without allocating, and equal the lazy values.
func TestPreparedFieldsAllocateNothing(t *testing.T) {
	lazy := fullVars()
	ready := fullVars()
	PrepareRoute(ready.Route)
	PrepareConsumer(ready.Consumer)
	fields := []struct {
		name string
		k    objKind
		i    int
	}{
		{"route.name", kindRoute, routeName},
		{"route.labels", kindRoute, routeLabels},
		{"consumer.name", kindConsumer, consumerName},
		{"consumer.tier", kindConsumer, consumerTier},
		{"consumer.tags", kindConsumer, consumerTags},
		{"consumer.labels", kindConsumer, consumerLabels},
		{"consumer.quotas", kindConsumer, consumerQuotas},
	}
	targets := func(v *expr.Vars, k objKind) any {
		if k == kindRoute {
			return v.Route
		}
		return v.Consumer
	}
	for _, f := range fields {
		want, _ := getField(f.k, f.i, targets(lazy, f.k))
		got, _ := getField(f.k, f.i, targets(ready, f.k))
		if got.(ref.Val).Equal(want.(ref.Val)) != types.True {
			t.Errorf("prepared %s = %v, lazy %v", f.name, got, want)
		}
		target := targets(ready, f.k)
		if n := testing.AllocsPerRun(100, func() { _, _ = getField(f.k, f.i, target) }); n != 0 {
			t.Errorf("prepared %s allocates %v", f.name, n)
		}
	}
	labels, _ := getField(kindRoute, routeLabels, ready.Route)
	quotas, _ := getField(kindConsumer, consumerQuotas, ready.Consumer)
	m := labels.(traits.Mapper)
	l := quotas.(traits.Lister)
	for name, read := range map[string]func() ref.Val{
		"route.labels.team":            func() ref.Val { return m.Get(types.String("team")) },
		"'daily-tokens' in quotas":     func() ref.Val { return l.Contains(types.String("daily-tokens")) },
		"quotas[0]":                    func() ref.Val { return l.Get(types.IntZero) },
		"'missing' in consumer.quotas": func() ref.Val { return l.Contains(types.String("missing")) },
	} {
		if n := testing.AllocsPerRun(100, func() { _ = read() }); n != 0 {
			t.Errorf("%s allocates %v", name, n)
		}
	}
	if v := m.Get(types.String("team")); v != types.String("core") {
		t.Errorf("route.labels.team = %v, want core", v)
	}
	if l.Contains(types.String("daily-tokens")) != types.True || l.Get(types.IntZero) != types.String("daily-tokens") {
		t.Error("prepared quotas lost an element")
	}
}

// TestFixedVocabularyAllocatesNothing checks that the known values of
// request.method, request.scheme, auth.method and error.kind convert
// without allocating, and that every value, known or not, converts to the
// CEL string it is.
func TestFixedVocabularyAllocatesNothing(t *testing.T) {
	cases := []struct {
		name   string
		conv   func(string) ref.Val
		values []string
	}{
		{"request.method", methodVal, []string{
			http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
			http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace,
		}},
		{"request.scheme", schemeVal, []string{"http", "https"}},
		{"auth.method", authMethodVal, []string{"jwt", "api-key", "basic", "mtls"}},
		{"error.kind", errorKindVal, []string{"connect", "timeout", "reset", "tls"}},
	}
	for _, tc := range cases {
		for _, v := range tc.values {
			if got := tc.conv(v); got != types.String(v) {
				t.Errorf("%s(%q) = %v", tc.name, v, got)
			}
			if n := testing.AllocsPerRun(100, func() { _ = tc.conv(v) }); n != 0 {
				t.Errorf("%s(%q) allocates %v", tc.name, v, n)
			}
		}
		for _, other := range []string{"PROPFIND", "get", "", "ws"} {
			if got := tc.conv(other); got != types.String(other) {
				t.Errorf("%s(%q) = %v", tc.name, other, got)
			}
		}
	}
}

// TestStatusAllocatesNothing checks that the common statuses convert
// without allocating and that every status converts to its CEL int.
func TestStatusAllocatesNothing(t *testing.T) {
	for _, s := range []int{200, 201, 202, 204, 301, 302, 304, 400, 401, 403, 404, 409, 429, 500, 502, 503, 504} {
		if got := statusVal(s); got != types.Int(s) {
			t.Errorf("statusVal(%d) = %v", s, got)
		}
		if n := testing.AllocsPerRun(100, func() { _ = statusVal(s) }); n != 0 {
			t.Errorf("statusVal(%d) allocates %v", s, n)
		}
	}
	for _, s := range []int{0, 100, 418, 599, 1000} {
		if got := statusVal(s); got != types.Int(s) {
			t.Errorf("statusVal(%d) = %v", s, got)
		}
	}
	r := &expr.Response{Status: 503}
	if n := testing.AllocsPerRun(100, func() { _, _ = getField(kindResponse, respStatus, r) }); n != 0 {
		t.Errorf("response.status 503 allocates %v", n)
	}
}

// copyClaims is an expr.Value that records every Native call, which copies
// a document.
type copyClaims struct {
	m      map[string]any
	copies *int
}

func (copyClaims) IsNull() bool { return false }

func (c copyClaims) AppendBody(dst []byte) ([]byte, error) { return FromNative(c.m).AppendBody(dst) }

func (c copyClaims) Native() (any, error) {
	*c.copies++
	return c.m, nil
}

// treeClaims is copyClaims that also implements expr.Tree.
type treeClaims struct{ copyClaims }

func (c treeClaims) Tree() any { return c.m }

// TestTreeIsViewedInPlace checks that a Value implementing expr.Tree, such
// as verified JWT claims, is viewed in place, never through Native's copy,
// and reads the same members as a copied one.
func TestTreeIsViewedInPlace(t *testing.T) {
	tree := map[string]any{"scope": "orders:read", "n": jsonNum("7")}
	var copies int
	viewed := ToVal(treeClaims{copyClaims{m: tree, copies: &copies}})
	if copies != 0 {
		t.Errorf("an expr.Tree was copied %d times", copies)
	}
	if m, ok := viewed.Value().(map[string]any); !ok || len(m) != len(tree) {
		t.Fatalf("viewed tree = %v", viewed.Value())
	}
	copied := ToVal(copyClaims{m: tree, copies: &copies})
	if copies != 1 {
		t.Errorf("a plain Value was copied %d times, want 1", copies)
	}
	if viewed.Equal(copied) != types.True {
		t.Errorf("viewed %v and copied %v differ", viewed, copied)
	}
	a := &expr.Auth{Method: "jwt", Claims: treeClaims{copyClaims{m: tree, copies: &copies}}}
	v, err := getField(kindAuth, authClaims, a)
	if err != nil || copies != 1 {
		t.Fatalf("auth.claims = %v, %v after %d copies", v, err, copies)
	}
	if got := v.(traits.Mapper).Get(types.String("scope")); got != types.String("orders:read") {
		t.Errorf("auth.claims.scope = %v", got)
	}
}

// TestOtherMethodsHaveEmptyClaims checks that auth.claims of a method
// other than jwt is the empty map without reading Claims, so it never
// copies a document.
func TestOtherMethodsHaveEmptyClaims(t *testing.T) {
	var copies int
	for _, method := range []string{"api-key", "basic", "mtls", ""} {
		a := &expr.Auth{Method: method, Claims: copyClaims{m: map[string]any{"x": "y"}, copies: &copies}}
		v, err := getField(kindAuth, authClaims, a)
		if err != nil || v.(traits.Mapper).Size() != types.IntZero {
			t.Errorf("auth.claims of %q = %v, %v; want the empty map", method, v, err)
		}
		if n := testing.AllocsPerRun(100, func() { _, _ = getField(kindAuth, authClaims, a) }); n != 0 {
			t.Errorf("auth.claims of %q allocates %v", method, n)
		}
	}
	if copies != 0 {
		t.Errorf("claims of other methods were read %d times", copies)
	}
}
