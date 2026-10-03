// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Tests for 03 C requirement 17: the Ruralz object types, their fields and
// field types, and the variable declarations, served by a custom
// types.Provider without reflection.

func newProvider(t *testing.T) *Provider {
	t.Helper()
	p, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

// TestReq17ObjectTypes checks every object type and field of the 03 req 17
// table: names in declaration order and CEL types.
func TestReq17ObjectTypes(t *testing.T) {
	p := newProvider(t)
	const (
		str  = "string"
		i64  = "int"
		boo  = "bool"
		smap = "map(string, string)"
		slst = "list(string)"
		dyn  = "dyn"
	)
	type field struct{ name, typ string }
	want := []struct {
		typeName string
		fields   []field
	}{
		{TypeRequest, []field{{"method", str}, {"scheme", str}, {"host", str}, {"path", str}, {"pathParams", smap}, {"query", smap}, {"headers", smap}, {"body", dyn}}},
		{TypeSource, []field{{"ip", str}, {"port", i64}, {"tlsVersion", str}, {"clientCertSubject", str}}},
		{TypeRoute, []field{{"name", str}, {"labels", smap}}},
		{TypeConsumer, []field{{"name", str}, {"tier", str}, {"tags", slst}, {"labels", smap}, {"quotas", slst}}},
		{TypeAuth, []field{{"method", str}, {"claims", dyn}}},
		{TypeResponse, []field{{"status", i64}, {"headers", smap}, {"body", dyn}}},
		{TypeError, []field{{"kind", str}}},
		{TypeUpstream, []field{{"name", str}, {"endpoint", str}}},
		{TypeStep, []field{{"status", i64}, {"headers", smap}, {"body", dyn}}},
		{TypeAI, []field{{"model", str}, {"estimatedInputTokens", i64}, {"maxOutputTokens", i64}, {"stream", boo}}},
	}
	if len(want) != int(numKinds) {
		t.Fatalf("table has %d types, package %d", len(want), numKinds)
	}
	for _, tc := range want {
		t.Run(tc.typeName, func(t *testing.T) {
			tt, ok := p.FindStructType(tc.typeName)
			if !ok || tt.Kind() != types.TypeKind || tt.Parameters()[0].TypeName() != tc.typeName {
				t.Fatalf("FindStructType = %v, %v", tt, ok)
			}
			if _, ok := p.FindStructType("." + tc.typeName); !ok {
				t.Errorf("absolute name .%s not found", tc.typeName)
			}
			id, ok := p.FindIdent(tc.typeName)
			if !ok || id.(*types.Type).TypeName() != tc.typeName || id.(*types.Type).Kind() != types.StructKind {
				t.Errorf("FindIdent = %v, %v", id, ok)
			}
			names, ok := p.FindStructFieldNames(tc.typeName)
			var wantNames []string
			for _, f := range tc.fields {
				wantNames = append(wantNames, f.name)
			}
			if !ok || !slices.Equal(names, wantNames) {
				t.Errorf("FindStructFieldNames = %v, %v; want %v", names, ok, wantNames)
			}
			names[0] = "mutated"
			if again, _ := p.FindStructFieldNames(tc.typeName); again[0] == "mutated" {
				t.Error("FindStructFieldNames returns shared state")
			}
			for _, f := range tc.fields {
				ft, ok := p.FindStructFieldType(tc.typeName, f.name)
				if !ok || ft.Type.String() != f.typ || ft.GetFrom == nil || ft.IsSet == nil {
					t.Errorf("field %s = %+v, %v; want type %s with accessors", f.name, ft, ok, f.typ)
				}
			}
			if _, ok := p.FindStructFieldType(tc.typeName, "bogus"); ok {
				t.Error("undeclared field found")
			}
		})
	}
}

// TestReq17Variables checks the declared variable types, their order and the
// Var filter, and that every name is the expr.Var spelling.
func TestReq17Variables(t *testing.T) {
	want := []struct {
		bit  expr.Var
		name string
		typ  string
	}{
		{expr.VarRequest, "request", TypeRequest},
		{expr.VarSource, "source", TypeSource},
		{expr.VarRoute, "route", TypeRoute},
		{expr.VarConsumer, "consumer", TypeConsumer},
		{expr.VarAuth, "auth", TypeAuth},
		{expr.VarNow, "now", "google.protobuf.Timestamp"},
		{expr.VarResponse, "response", TypeResponse},
		{expr.VarError, "error", TypeError},
		{expr.VarUpstream, "upstream", TypeUpstream},
		{expr.VarAttempt, "attempt", "int"},
		{expr.VarSteps, "steps", "map(string, ruralz.Step)"},
		{expr.VarDuration, "duration", "google.protobuf.Duration"},
		{expr.VarAI, "ai", TypeAI},
	}
	got := Variables(allVars | expr.VarSelf)
	if len(got) != len(want) {
		t.Fatalf("Variables = %d entries, want %d (self is not declared in M1)", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Bit != w.bit || g.Name != w.name || g.Type.String() != w.typ {
			t.Errorf("Variables[%d] = {%v %s %s}, want {%v %s %s}", i, g.Bit, g.Name, g.Type, w.bit, w.name, w.typ)
		}
		if names := w.bit.Names(); len(names) != 1 || names[0] != w.name {
			t.Errorf("%s is not the expr.Var spelling %v", w.name, names)
		}
	}
	// A place's subset, in declaration order.
	sub := Variables(expr.VarRequest | expr.VarSource | expr.VarNow)
	var names []string
	for _, v := range sub {
		names = append(names, v.Name)
	}
	if !slices.Equal(names, []string{"request", "source", "now"}) {
		t.Errorf("Variables(match.when) = %v", names)
	}
	if len(Variables(0)) != 0 {
		t.Error("Variables(0) is not empty")
	}
}

// TestProviderDelegates checks that types outside Ruralz reach the cel-go
// registry.
func TestProviderDelegates(t *testing.T) {
	p := newProvider(t)
	if tt, ok := p.FindStructType("google.protobuf.Duration"); !ok || tt == nil {
		t.Error("well-known type not delegated")
	}
	if _, ok := p.FindStructType("ruralz.Bogus"); ok {
		t.Error("unknown ruralz type found")
	}
	if _, ok := p.FindIdent("int"); !ok {
		t.Error("FindIdent(int) not delegated")
	}
	if _, ok := p.FindIdent("ruralz.Bogus"); ok {
		t.Error("unknown ident found")
	}
	if names, ok := p.FindStructFieldNames("google.protobuf.Duration"); !ok || !slices.Contains(names, "seconds") {
		t.Errorf("FindStructFieldNames(Duration) = %v, %v", names, ok)
	}
	if ft, ok := p.FindStructFieldType("google.protobuf.Duration", "seconds"); !ok || ft.Type != types.IntType {
		t.Errorf("FindStructFieldType(Duration.seconds) = %v, %v", ft, ok)
	}
	if v := p.EnumValue("no.such.Enum"); !types.IsError(v) {
		t.Errorf("EnumValue(unknown) = %v", v)
	}
	d := p.NewValue("google.protobuf.Duration", map[string]ref.Val{"seconds": types.Int(2)})
	if types.IsError(d) {
		t.Errorf("NewValue(Duration) = %v", d)
	}
	for k := range numKinds {
		v := p.NewValue(k.TypeName(), nil)
		if err, ok := v.(*types.Err); !ok || !errors.Is(err, ErrUnsupported) {
			t.Errorf("NewValue(%s) = %v, want ErrUnsupported", k.TypeName(), v)
		}
	}
}

// TestProviderNativeToValue checks the adapter: CEL values pass through,
// views wrap (nil pointers are null), maps get the ordered views.
func TestProviderNativeToValue(t *testing.T) {
	p := newProvider(t)
	var nilReq *expr.Request
	cases := []struct {
		name string
		in   any
		want string // CEL type name
	}{
		{"ref.Val", types.String("x"), "string"},
		{"nil request", nilReq, "null_type"},
		{"request", &expr.Request{}, TypeRequest},
		{"source", &expr.Source{}, TypeSource},
		{"route", &expr.Route{}, TypeRoute},
		{"consumer", &expr.Consumer{}, TypeConsumer},
		{"auth", &expr.Auth{}, TypeAuth},
		{"response", &expr.Response{}, TypeResponse},
		{"error", &expr.AttemptError{}, TypeError},
		{"upstream", &expr.Upstream{}, TypeUpstream},
		{"step", &expr.Step{}, TypeStep},
		{"ai", &expr.AI{}, TypeAI},
		{"steps", &expr.Steps{}, "map"},
		{"value", FromNative(map[string]any{}), "map"},
		{"header", http.Header{"A": {"1"}}, "map"},
		{"string map", map[string]string{"a": "1"}, "map"},
		{"json object", map[string]any{"a": "1"}, "map"},
		{"json array", []any{"a"}, "list"},
		{"json number", json.Number("1.5"), "double"},
		{"int", 7, "int"},
		{"nil", nil, "null_type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := p.NativeToValue(tc.in)
			if got := v.Type().TypeName(); got != tc.want {
				t.Errorf("NativeToValue(%T).Type = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
	// Ordered iteration survives adaptation of a native map (03 req 6.3).
	m := p.NativeToValue(map[string]string{"b": "2", "a": "1", "c": "3"}).(traits.Mapper)
	if got := iterKeys(m); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("iteration = %v", got)
	}
}

// iterKeys drains an iterable of string keys.
func iterKeys(it traits.Iterable) []string {
	var out []string
	for i := it.Iterator(); i.HasNext() == types.True; {
		out = append(out, string(i.Next().(types.String)))
	}
	return out
}

// TestObjectKind checks the object type values: names, traits and lookup.
func TestObjectKind(t *testing.T) {
	for k := range numKinds {
		name := k.TypeName()
		if got, ok := kindByName(name); !ok || got != k {
			t.Errorf("kindByName(%s) = %v, %v", name, got, ok)
		}
		if !k.HasTrait(traits.FieldTesterType) || !k.HasTrait(traits.IndexerType) || k.HasTrait(traits.SizerType) {
			t.Errorf("%s traits wrong", name)
		}
		if len(objectFields(k)) == 0 {
			t.Errorf("%s has no fields", name)
		}
	}
	if numKinds.TypeName() != "" || objectFields(numKinds) != nil {
		t.Error("numKinds is not a type")
	}
	if _, ok := kindByName(""); ok {
		t.Error("empty name found")
	}
	if fieldType(0).celType() != types.DynType {
		t.Error("unknown field type is not dyn")
	}
	if _, ok := fieldNumber(kindRequest, "bogus"); ok {
		t.Error("fieldNumber(bogus) found")
	}
}
