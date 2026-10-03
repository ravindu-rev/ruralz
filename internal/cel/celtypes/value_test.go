// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Tests for 03 C requirement 25 (JSON values as CEL reads them) and the
// expr.Value contract used by 03 F requirement 41 and 07 requirement 57
// (AppendBody and Native).

// TestReq25Numbers checks the int and double rule for decoded numbers.
func TestReq25Numbers(t *testing.T) {
	cases := []struct {
		lit  string
		want ref.Val
	}{
		{"0", types.Int(0)},
		{"-0", types.Int(0)},
		{"42", types.Int(42)},
		{"9007199254740993", types.Int(9007199254740993)},
		{"9223372036854775807", types.Int(math.MaxInt64)},
		{"-9223372036854775808", types.Int(math.MinInt64)},
		{"9223372036854775808", types.Double(9223372036854775808)},
		{"1.0", types.Double(1)},
		{"1.5", types.Double(1.5)},
		{"1e2", types.Double(100)},
		{"-2.5E-3", types.Double(-0.0025)},
		{"1e400", types.Double(math.Inf(1))},
		{"-1e400", types.Double(math.Inf(-1))},
	}
	for _, tc := range cases {
		got := numberVal(json.Number(tc.lit))
		if got.Type() != tc.want.Type() || got.Equal(tc.want) != types.True {
			t.Errorf("numberVal(%s) = %v (%s), want %v (%s)", tc.lit, got, got.Type().TypeName(), tc.want, tc.want.Type().TypeName())
		}
	}
	for _, bad := range []string{"", "-", "01", "1.", ".5", "1e", "1e+", "+1", "NaN", "Inf", "0x10", "1_000", " 1", "1 "} {
		if v := numberVal(json.Number(bad)); !errors.Is(v.(error), ErrUnsupported) {
			t.Errorf("numberVal(%q) = %v, want ErrUnsupported", bad, v)
		}
	}
}

// TestReq25JSONThroughCEL checks decoded bodies as CEL reads them: ordered
// objects, exact int64, doubles, null members, arrays.
func TestReq25JSONThroughCEL(t *testing.T) {
	body := func(tree any) func() *expr.Vars {
		return func() *expr.Vars {
			v := fullVars()
			v.Request.Body = FromNative(tree)
			return v
		}
	}
	doc := map[string]any{
		"b":     jsonNum("1"),
		"a":     jsonNum("2"),
		"id":    jsonNum("9007199254740993"),
		"f":     jsonNum("2.50"),
		"e":     jsonNum("1e2"),
		"n":     nil,
		"t":     true,
		"s":     "text",
		"items": []any{map[string]any{"tags": []any{"x", "y"}}, map[string]any{"tags": []any{}}},
		"obj":   map[string]any{"z": "1", "y": "2"},
	}
	runEval(t, []evalCase{
		{name: "ordered keys", vars: body(map[string]any{"b": jsonNum("1"), "a": jsonNum("2")}), src: `request.body.map(k, k)`, want: []string{"a", "b"}},
		{name: "ordered nested", vars: body(doc), src: `request.body.obj.map(k, k + request.body.obj[k]).join(",")`, want: "y2,z1"},
		{name: "exact int64", vars: body(doc), src: `request.body.id == 9007199254740993`, want: true},
		{name: "int type", vars: body(doc), src: `type(request.body.a) == int`, want: true},
		{name: "fraction double", vars: body(doc), src: `type(request.body.f) == double && request.body.f == 2.5`, want: true},
		{name: "exponent double", vars: body(doc), src: `type(request.body.e) == double && request.body.e == 100.0`, want: true},
		{name: "heterogeneous compare", vars: body(doc), src: `request.body.e == 100`, want: true},
		{name: "null member", vars: body(doc), src: `request.body.n == null && has(request.body.n)`, want: true},
		{name: "bool member", vars: body(doc), src: `request.body.t`, want: true},
		{name: "string member", vars: body(doc), src: `request.body.s`, want: "text"},
		{name: "missing member", vars: body(doc), src: `has(request.body.zz)`, want: false},
		{name: "missing member select", vars: body(doc), src: `request.body.zz`, wantText: "no such key"},
		{name: "array access", vars: body(doc), src: `request.body.items[0].tags[1]`, want: "y"},
		{name: "array size", vars: body(doc), src: `request.body.items.size()`, want: 2},
		{name: "array in", vars: body(doc), src: `"x" in request.body.items[0].tags`, want: true},
		{name: "nested comprehension", vars: body(doc), src: `request.body.items.all(i, i.tags.all(t, t != "z"))`, want: true},
		{name: "size of object", vars: body(doc), src: `request.body.size()`, want: 10},
		{name: "object equality", vars: body(map[string]any{"a": jsonNum("1")}), src: `request.body == {"a": 1}`, want: true},
		{name: "list equality", vars: body([]any{"a", jsonNum("1")}), src: `request.body == ["a", 1]`, want: true},
		{name: "top-level list", vars: body([]any{"a", "b"}), src: `request.body.join("-")`, want: "a-b"},
		{name: "top-level null", vars: body(nil), src: `request.body == null`, want: true},
		{name: "nil map is null", vars: body(map[string]any(nil)), src: `request.body == null`, want: true},
		{name: "nil slice is null", vars: body([]any(nil)), src: `request.body == null`, want: true},
		{name: "non-JSON content type", vars: func() *expr.Vars {
			v := fullVars()
			v.Request.Body = Null()
			return v
		}, src: `request.body == null`, want: true},
		{name: "malformed JSON", vars: func() *expr.Vars {
			v := fullVars()
			v.Request.Body = ErrorValue(errors.New("unexpected end of input"))
			return v
		}, src: `request.body.x == 1`, wantErr: ErrBody},
		{name: "malformed JSON has", vars: func() *expr.Vars {
			v := fullVars()
			v.Request.Body = ErrorValue(errors.New("bad"))
			return v
		}, src: `request.body == null`, wantErr: ErrBody},
		{name: "unsupported native", vars: body(map[string]any{"x": 5}), src: `request.body.x`, wantErr: ErrUnsupported},
		{name: "claims from a foreign Value", vars: func() *expr.Vars {
			v := fullVars()
			v.Auth.Claims = foreignValue{tree: map[string]any{"sub": "u2"}}
			return v
		}, src: `auth.claims.sub`, want: "u2"},
		{name: "claims from a failing Value", vars: func() *expr.Vars {
			v := fullVars()
			v.Auth.Claims = foreignValue{err: errors.New("broken")}
			return v
		}, src: `auth.claims.sub`, wantErr: ErrBody},
	})
}

// foreignValue is an expr.Value implemented outside this package.
type foreignValue struct {
	tree any
	err  error
}

func (f foreignValue) IsNull() bool                          { return f.tree == nil }
func (f foreignValue) AppendBody(dst []byte) ([]byte, error) { return dst, f.err }
func (f foreignValue) Native() (any, error)                  { return f.tree, f.err }

// TestReq41AppendBody checks the transform body form of every value kind.
func TestReq41AppendBody(t *testing.T) {
	ts := time.Date(2026, 9, 26, 12, 30, 0, 500_000_000, time.FixedZone("CEST", 2*3600))
	cases := []struct {
		name    string
		v       *Value
		want    string
		wantErr error
	}{
		{"object sorted", FromNative(map[string]any{"b": jsonNum("1"), "a": []any{true, nil, "x"}}), `{"a":[true,null,"x"],"b":1}`, nil},
		{"numbers verbatim", FromNative([]any{jsonNum("1.50"), jsonNum("1e400"), jsonNum("-0")}), `[1.50,1e400,-0]`, nil},
		{"top-level number verbatim", FromNative(jsonNum("-0")), `-0`, nil},
		{"top-level large number verbatim", FromNative(jsonNum("1e400")), `1e400`, nil},
		{"top-level bool native", FromNative(true), `true`, nil},
		{"empty object", FromNative(map[string]any{}), `{}`, nil},
		{"empty array", FromNative([]any{}), `[]`, nil},
		{"top-level string raw", FromNative("héllo \"x\"\n"), "héllo \"x\"\n", nil},
		{"top-level invalid UTF-8 string raw", FromVal(types.String("\xff")), "\xff", nil},
		{"top-level bytes raw", FromVal(types.Bytes{0, 0xff, 'a'}), "\x00\xffa", nil},
		{"int", FromVal(types.Int(-5)), `-5`, nil},
		{"uint", FromVal(types.Uint(5)), `5`, nil},
		{"double", FromVal(types.Double(1.5)), `1.5`, nil},
		{"double integral", FromVal(types.Double(123456789)), `123456789`, nil},
		{"double large", FromVal(types.Double(1e21)), `1e+21`, nil},
		{"double small", FromVal(types.Double(1e-7)), `1e-7`, nil},
		{"double negative zero", FromVal(types.Double(math.Copysign(0, -1))), `-0`, nil},
		{"double NaN", FromVal(types.Double(math.NaN())), ``, ErrNotJSON},
		{"double infinity", FromVal(types.Double(math.Inf(-1))), ``, ErrNotJSON},
		{"bool", FromVal(types.True), `true`, nil},
		{"null", Null(), `null`, nil},
		{"native null", FromNative(nil), `null`, nil},
		{"timestamp", FromVal(types.Timestamp{Time: ts}), `"2026-09-26T10:30:00.5Z"`, nil},
		{"duration", FromVal(types.Duration{Duration: 1500 * time.Millisecond}), `"1.5s"`, nil},
		{"duration negative", FromVal(types.Duration{Duration: -1}), `"-0.000000001s"`, nil},
		{"duration zero", FromVal(types.Duration{}), `"0s"`, nil},
		{"duration whole", FromVal(types.Duration{Duration: 90 * time.Second}), `"90s"`, nil},
		{"duration min", FromVal(types.Duration{Duration: math.MinInt64}), `"-9223372036.854775808s"`, nil},
		{"nested bytes base64", FromVal(celMap(types.String("b"), types.Bytes("hi"))), `{"b":"aGk="}`, nil},
		{"nested timestamp and duration", FromVal(celList(types.Timestamp{Time: ts}, types.Duration{Duration: time.Second})), `["2026-09-26T10:30:00.5Z","1s"]`, nil},
		{"int keys", FromVal(celMap(types.Int(2), types.String("b"), types.Int(-1), types.String("a"))), `{"-1":"a","2":"b"}`, nil},
		{"uint and bool keys", FromVal(celMap(types.Uint(7), types.NullValue, types.True, types.False)), `{"7":null,"true":false}`, nil},
		{"duplicate member names", FromVal(celMap(types.Int(1), types.String("a"), types.String("1"), types.String("b"))), ``, ErrNotJSON},
		{"double key", FromVal(celMap(types.Double(1.5), types.String("a"))), ``, ErrNotJSON},
		{"escaping", FromNative([]any{"\"\\\b\t\n\f\r\x01\x1f /<>&é"}), `["\"\\\b\t\n\f\r\u0001\u001f` + " /<>&é" + `"]`, nil},
		{"nested invalid UTF-8", FromVal(celList(types.String("\xff"))), ``, ErrNotJSON},
		{"invalid UTF-8 member name", FromNative(map[string]any{"\xff": "x"}), ``, ErrNotJSON},
		{"object view", FromVal(viewOf(&expr.AttemptError{Kind: "reset"})), ``, ErrNotJSON},
		{"type value", FromVal(types.IntType), ``, ErrNotJSON},
		{"error value", ErrorValue(errors.New("bad")), ``, ErrBody},
		{"nested error", FromVal(celList(types.WrapErr(ErrNull))), ``, ErrNull},
		{"unsupported native", FromNative(map[string]any{"x": 1.5}), ``, ErrUnsupported},
		{"unsupported top-level native", FromNative(7), ``, ErrUnsupported},
		{"malformed native number", FromNative([]any{jsonNum("01")}), ``, ErrUnsupported},
		{"header view", FromVal(mapView[headerSource]{headerSource{&http.Header{"B": {"2"}, "A": {"1", "3"}}}}), `{"a":"1, 3","b":"2"}`, nil},
		{"string list view", FromVal(listView[stringsSource]{stringsSource{&[]string{"x", "y"}}}), `["x","y"]`, nil},
		{"CEL map holding a body", FromVal(celMap(types.String("x"), nativeVal(map[string]any{"n": jsonNum("1.50")}))), `{"x":{"n":1.50}}`, nil},
		{"CEL list holding a body list", FromVal(celList(nativeVal([]any{jsonNum("2.0")}))), `[[2.0]]`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.v.AppendBody([]byte("prefix:"))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("AppendBody err = %v (%q), want %v", err, got, tc.wantErr)
				}
				if _, err := tc.v.Native(); err == nil {
					t.Errorf("Native succeeded where AppendBody failed")
				}
				return
			}
			if err != nil {
				t.Fatalf("AppendBody: %v", err)
			}
			if want := "prefix:" + tc.want; string(got) != want {
				t.Errorf("AppendBody = %q, want %q", got, want)
			}
		})
	}
}

// celMap builds a cel-go map from alternating keys and values.
func celMap(kv ...ref.Val) ref.Val {
	m := map[ref.Val]ref.Val{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return types.NewRefValMap(types.DefaultTypeAdapter, m)
}

// celList builds a cel-go list.
func celList(vs ...ref.Val) ref.Val { return types.NewRefValList(types.DefaultTypeAdapter, vs) }

// decodeJSON decodes one JSON text with json.Number numbers.
func decodeJSON(t testing.TB, b []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return v
}

// TestReq41NativeMatchesAppendBody checks 07 req 57: for maps and lists,
// Native equals decoding what AppendBody writes, and the tree is new.
func TestReq41NativeMatchesAppendBody(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	values := []*Value{
		FromNative(map[string]any{"b": jsonNum("1.50"), "a": []any{true, nil, "x", map[string]any{}}}),
		FromNative([]any{jsonNum("9007199254740993"), "s"}),
		FromVal(celMap(types.String("n"), types.Int(-3), types.Int(4), types.Uint(9), types.String("d"), types.Double(0.1))),
		FromVal(celList(types.Bytes("hi"), types.Timestamp{Time: ts}, types.Duration{Duration: 1500 * time.Microsecond}, types.NullValue, types.False)),
		FromVal(celMap(types.String("body"), nativeVal(map[string]any{"z": []any{jsonNum("1e2")}}))),
		FromVal(mapView[headerSource]{headerSource{&http.Header{"X-A": {"1", "2"}}}}),
		FromVal(types.Int(12)),
		FromVal(types.Double(2.5)),
		FromVal(types.True),
		Null(),
	}
	for i, v := range values {
		body, err := v.AppendBody(nil)
		if err != nil {
			t.Fatalf("value %d: AppendBody: %v", i, err)
		}
		native, err := v.Native()
		if err != nil {
			t.Fatalf("value %d: Native: %v", i, err)
		}
		if want := decodeJSON(t, body); !reflect.DeepEqual(native, want) {
			t.Errorf("value %d: Native = %#v, decode(AppendBody) = %#v", i, native, want)
		}
	}
	// The tree shares nothing with the Value.
	src := map[string]any{"a": []any{"x"}, "m": map[string]any{"k": "v"}}
	v := FromNative(src)
	n, err := v.Native()
	if err != nil {
		t.Fatal(err)
	}
	n.(map[string]any)["a"].([]any)[0] = "changed"
	n.(map[string]any)["m"].(map[string]any)["k"] = "changed"
	n.(map[string]any)["new"] = true
	if body, _ := v.AppendBody(nil); string(body) != `{"a":["x"],"m":{"k":"v"}}` {
		t.Errorf("Native aliases the Value: %s", body)
	}
	// Top-level strings and bytes give their contents.
	for _, tc := range []struct {
		v    *Value
		want any
	}{
		{FromNative("plain"), "plain"},
		{FromVal(types.Bytes("raw")), "raw"},
		{FromVal(types.Uint(3)), json.Number("3")},
		{FromVal(types.Duration{Duration: time.Second}), "1s"},
		{FromVal(types.Timestamp{Time: ts}), "2026-01-02T03:04:05.000000006Z"},
	} {
		if got, err := tc.v.Native(); err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Native = %#v, %v; want %#v", got, err, tc.want)
		}
	}
	for _, bad := range []*Value{
		FromVal(celList(types.Double(math.NaN()))),
		FromVal(celMap(types.Double(1), types.True)),
		FromVal(celList(types.String("\xff"))),
		FromNative([]any{"\xff"}),
		FromNative([]any{jsonNum("x")}),
		FromNative([]any{int64(1)}),
		FromVal(celList(types.IntType)),
		FromVal(celList(types.WrapErr(ErrBody))),
	} {
		if _, err := bad.Native(); err == nil {
			t.Errorf("Native(%v) succeeded", bad.Val())
		}
	}
}

// TestValueDepth checks the nesting ceiling of AppendBody and Native.
func TestValueDepth(t *testing.T) {
	var deep any = "leaf"
	for range maxDepth + 2 {
		deep = []any{deep}
	}
	v := FromNative(deep)
	if _, err := v.AppendBody(nil); !errors.Is(err, ErrNotJSON) {
		t.Errorf("AppendBody deep = %v", err)
	}
	if _, err := v.Native(); !errors.Is(err, ErrNotJSON) {
		t.Errorf("Native deep = %v", err)
	}
	var deepMap any = "leaf"
	for range maxDepth + 2 {
		deepMap = map[string]any{"k": deepMap}
	}
	if _, err := FromNative(deepMap).AppendBody(nil); !errors.Is(err, ErrNotJSON) {
		t.Errorf("AppendBody deep map = %v", err)
	}
	// Through CEL containers (generic path).
	var deepCEL ref.Val = types.String("leaf")
	for range maxDepth + 2 {
		deepCEL = celList(deepCEL)
	}
	if _, err := FromVal(deepCEL).AppendBody(nil); !errors.Is(err, ErrNotJSON) {
		t.Errorf("AppendBody deep CEL = %v", err)
	}
	if _, err := FromVal(deepCEL).Native(); !errors.Is(err, ErrNotJSON) {
		t.Errorf("Native deep CEL = %v", err)
	}
	if _, err := appendJSON(nil, nil, 0); !errors.Is(err, ErrNotJSON) {
		t.Errorf("appendJSON(nil) = %v", err)
	}
	if _, err := toNative(nil, 0); !errors.Is(err, ErrNotJSON) {
		t.Errorf("toNative(nil) = %v", err)
	}
	if _, err := appendNativeMap(nil, map[string]any{}, maxDepth+1); !errors.Is(err, ErrNotJSON) {
		t.Errorf("appendNativeMap past depth = %v", err)
	}
	if _, err := appendNativeList(nil, []any{}, maxDepth+1); !errors.Is(err, ErrNotJSON) {
		t.Errorf("appendNativeList past depth = %v", err)
	}
	if _, err := cloneNative(nil, maxDepth+1); !errors.Is(err, ErrNotJSON) {
		t.Errorf("cloneNative past depth = %v", err)
	}
}

// TestValueStates checks IsNull, Err, Val and ToVal over every state.
func TestValueStates(t *testing.T) {
	var nilValue *Value
	nulls := []*Value{nilValue, {}, Null(), FromNative(nil), FromNative(map[string]any(nil)), FromNative([]any(nil)), FromVal(nil), FromVal(types.NullValue)}
	for i, v := range nulls {
		if !v.IsNull() || v.Err() != nil || v.Val() != types.NullValue {
			t.Errorf("null %d: IsNull %v Err %v Val %v", i, v.IsNull(), v.Err(), v.Val())
		}
	}
	for i, v := range []*Value{FromNative(map[string]any{}), FromNative(""), FromNative(false), FromVal(types.Int(0))} {
		if v.IsNull() {
			t.Errorf("value %d is null", i)
		}
	}
	ev := ErrorValue(errors.New("truncated"))
	if ev.IsNull() || !errors.Is(ev.Err(), ErrBody) || !strings.Contains(ev.Err().Error(), "truncated") {
		t.Errorf("ErrorValue: IsNull %v Err %v", ev.IsNull(), ev.Err())
	}
	if _, err := ev.Native(); !errors.Is(err, ErrBody) {
		t.Errorf("ErrorValue.Native = %v", err)
	}
	if _, err := ev.AppendBody(nil); !errors.Is(err, ErrBody) {
		t.Errorf("ErrorValue.AppendBody = %v", err)
	}
	// Error values never share a *types.Err: cel-go labels the error values
	// it is handed with a node ID in place (03 req 25, 38, 39).
	errorValues := []*Value{
		ev,
		FromVal(types.NewErr("cel failure")),
		FromVal(types.WrapErr(nil)),
		FromNative(struct{}{}),
		FromNative(json.Number("01")),
	}
	for i, v := range errorValues {
		v1, ok1 := v.Val().(*types.Err)
		v2, ok2 := ToVal(v).(*types.Err)
		if !ok1 || !ok2 || v1 == v2 || v.IsNull() || v.Err() == nil {
			t.Errorf("error value %d: Val %v, ToVal %v, IsNull %v, Err %v", i, v.Val(), ToVal(v), v.IsNull(), v.Err())
			continue
		}
		if ce := (*types.Err)(nil); errors.As(v.Err(), &ce) {
			t.Errorf("error value %d: Err holds a shared *types.Err", i)
		}
		if !errors.Is(v1, v.Err()) || v1.Error() != v.Err().Error() {
			t.Errorf("error value %d: Val %v does not wrap Err %v", i, v1, v.Err())
		}
		types.LabelErrNode(7, v1)
		if v3 := v.Val().(*types.Err); v3.NodeID() != 0 {
			t.Errorf("error value %d: label leaked into a later read (node %d)", i, v3.NodeID())
		}
	}
	if err := FromVal(types.WrapErr(nil)).Err(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("FromVal(empty error) Err = %v", err)
	}
	if err := FromNative(struct{}{}).Err(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("FromNative(struct) Err = %v", err)
	}
	if _, err := FromVal(types.NewErr("cel failure")).AppendBody(nil); err == nil || err.Error() != "cel failure" {
		t.Errorf("FromVal(error).AppendBody = %v", err)
	}
	if _, err := FromVal(types.NewErr("cel failure")).Native(); err == nil || err.Error() != "cel failure" {
		t.Errorf("FromVal(error).Native = %v", err)
	}
	// ToVal over every expr.Value shape.
	if v := ToVal(nil); !errors.Is(v.(error), ErrBody) {
		t.Errorf("ToVal(nil) = %v", v)
	}
	if v := ToVal(nilValue); v != types.NullValue {
		t.Errorf("ToVal(nil *Value) = %v", v)
	}
	if v := ToVal(FromNative("s")); v != types.String("s") {
		t.Errorf("ToVal(*Value) = %v", v)
	}
	if v := ToVal(foreignValue{tree: []any{"a"}}); v.Type() != types.ListType {
		t.Errorf("ToVal(foreign) = %v", v)
	}
	if v := ToVal(foreignValue{err: errors.New("x")}); !errors.Is(v.(error), ErrBody) {
		t.Errorf("ToVal(failing foreign) = %v", v)
	}
	var _ expr.Value = FromNative(nil)
}
