// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Unit tests of the ref.Val methods of the views (03 C requirements 17 to
// 25): paths cel-go reaches only for dyn operands, conversions and
// equality.

// TestMapViewMethods covers Find, Get, Contains, Equal and conversions.
func TestMapViewMethods(t *testing.T) {
	m := mapView[stringSource]{stringSource{&map[string]string{"b": "2", "a": "1"}}}
	errKey := types.WrapErr(errors.New("key failed"))
	unknown := types.NewUnknown(1, nil)

	if v, ok := m.Find(types.String("a")); !ok || v != types.String("1") {
		t.Errorf("Find(a) = %v, %v", v, ok)
	}
	if v, ok := m.Find(types.Int(1)); ok || v != nil {
		t.Errorf("Find(1) = %v, %v", v, ok)
	}
	if v, ok := m.Find(errKey); ok || v != errKey {
		t.Errorf("Find(err) = %v, %v", v, ok)
	}
	if v, ok := m.Find(unknown); ok || v != unknown {
		t.Errorf("Find(unknown) = %v, %v", v, ok)
	}
	if v := m.Get(types.String("b")); v != types.String("2") {
		t.Errorf("Get(b) = %v", v)
	}
	if v := m.Get(types.String("z")); !errors.Is(v.(error), ErrNoSuchKey) {
		t.Errorf("Get(z) = %v", v)
	}
	if v := m.Get(errKey); v != errKey {
		t.Errorf("Get(err) = %v", v)
	}
	if m.Contains(types.String("a")) != types.True || m.Contains(types.String("z")) != types.False || m.Contains(errKey) != errKey {
		t.Error("Contains")
	}
	// Equality.
	same := types.NewStringStringMap(types.DefaultTypeAdapter, map[string]string{"a": "1", "b": "2"})
	for _, tc := range []struct {
		other ref.Val
		want  ref.Val
	}{
		{same, types.True},
		{types.NewStringStringMap(types.DefaultTypeAdapter, map[string]string{"a": "1"}), types.False},
		{types.NewStringStringMap(types.DefaultTypeAdapter, map[string]string{"a": "1", "c": "2"}), types.False},
		{types.NewStringStringMap(types.DefaultTypeAdapter, map[string]string{"a": "1", "b": "3"}), types.False},
		{types.String("a"), types.False},
	} {
		if got := m.Equal(tc.other); got != tc.want {
			t.Errorf("Equal(%v) = %v, want %v", tc.other, got, tc.want)
		}
	}
	if same.Equal(m) != types.True {
		t.Error("cel-go map does not equal the view")
	}
	// Conversions.
	if got, err := m.ConvertToNative(reflect.TypeFor[map[string]string]()); err != nil || !reflect.DeepEqual(got, map[string]string{"a": "1", "b": "2"}) {
		t.Errorf("ConvertToNative(map[string]string) = %v, %v", got, err)
	}
	if got, err := m.ConvertToNative(reflect.TypeFor[map[string]any]()); err != nil || !reflect.DeepEqual(got, map[string]any{"a": "1", "b": "2"}) {
		t.Errorf("ConvertToNative(map[string]any) = %v, %v", got, err)
	}
	if _, err := m.ConvertToNative(reflect.TypeFor[int]()); err == nil {
		t.Error("ConvertToNative(int) succeeded")
	}
	bad := mapView[*jsonObject]{&jsonObject{map[string]any{"x": 1.5}}}
	if _, err := bad.ConvertToNative(reflect.TypeFor[map[string]any]()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ConvertToNative over a bad native = %v", err)
	}
	if m.ConvertToType(types.MapType) != m || m.ConvertToType(types.TypeType) != types.MapType || !types.IsError(m.ConvertToType(types.StringType)) {
		t.Error("ConvertToType")
	}
	if m.Type() != types.MapType || !reflect.DeepEqual(m.Value(), map[string]string{"b": "2", "a": "1"}) {
		t.Error("Type or Value")
	}
}

// TestMapSourcesValue covers the Value of every map source.
func TestMapSourcesValue(t *testing.T) {
	params := []expr.Param{{Name: "b", Value: "2"}, {Name: "a", Value: "1"}, {Name: "b", Value: "3"}}
	pm := mapView[paramSource]{paramSource{&params}}
	if !reflect.DeepEqual(pm.Value(), map[string]string{"a": "1", "b": "2"}) || pm.Size() != types.Int(2) {
		t.Errorf("paramSource Value %v Size %v", pm.Value(), pm.Size())
	}
	if got := iterKeys(pm); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("paramSource keys %v", got)
	}
	steps := &expr.Steps{}
	steps.Add("x", expr.Step{Status: 200})
	steps.Add("x", expr.Step{Status: 500})
	sm := mapView[stepsSource]{stepsSource{steps}}
	if sm.Value() != steps || sm.Size() != types.Int(1) {
		t.Errorf("stepsSource Value %v Size %v", sm.Value(), sm.Size())
	}
	empty := mapView[stepsSource]{}
	if empty.Size() != types.Int(0) || len(iterKeys(empty)) != 0 || empty.Contains(types.String("x")) != types.False {
		t.Error("nil steps not empty")
	}
	hm := mapView[headerSource]{headerSource{&http.Header{"A": {"1"}}}}
	if !reflect.DeepEqual(hm.Value(), map[string]string{"a": "1"}) {
		t.Errorf("headerSource Value %v", hm.Value())
	}
	jm := mapView[*jsonObject]{&jsonObject{map[string]any{"k": "v"}}}
	if !reflect.DeepEqual(jm.Value(), map[string]any{"k": "v"}) {
		t.Errorf("jsonObject Value %v", jm.Value())
	}
	var noClaims mapView[*jsonObject]
	if noClaims.Size() != types.Int(0) || len(noClaims.Value().(map[string]any)) != 0 || len(iterKeys(noClaims)) != 0 {
		t.Error("nil jsonObject not empty")
	}
	for _, empty := range []mapView[stringSource]{{}, {stringSource{new(map[string]string)}}} {
		if empty.Size() != types.Int(0) || len(iterKeys(empty)) != 0 {
			t.Error("empty stringSource")
		}
	}
	var noHeader mapView[headerSource]
	if noHeader.Size() != types.Int(0) || len(noHeader.Value().(map[string]string)) != 0 {
		t.Error("nil headerSource not empty")
	}
	r := &expr.Request{}
	r.SetRawQuery("b=2&a=1")
	qm := mapView[querySource]{querySource{r}}
	if !reflect.DeepEqual(qm.Value(), map[string]string{"a": "1", "b": "2"}) || !slices.Equal(iterKeys(qm), []string{"a", "b"}) {
		t.Errorf("querySource Value %v", qm.Value())
	}
	lm := sortedLabels(map[string]string{"z": "1", "a": "2"}).(mapView[*sortedStrings])
	if !reflect.DeepEqual(lm.Value(), map[string]string{"z": "1", "a": "2"}) || lm.Size() != types.Int(2) {
		t.Errorf("sortedStrings Value %v", lm.Value())
	}
	if v, ok := lm.Find(types.String("z")); !ok || v != types.String("1") {
		t.Errorf("sortedStrings Find %v %v", v, ok)
	}
}

// TestListViewMethods covers the list views.
func TestListViewMethods(t *testing.T) {
	tags := []string{"a", "b"}
	l := listView[stringsSource]{stringsSource{&tags}}
	var empty []string
	e := listView[stringsSource]{stringsSource{&empty}}
	other := types.NewStringList(types.DefaultTypeAdapter, []string{"c"})

	if got := l.Add(other); got.Equal(types.NewStringList(types.DefaultTypeAdapter, []string{"a", "b", "c"})) != types.True {
		t.Errorf("Add = %v", got)
	}
	if got := e.Add(other); got != other {
		t.Errorf("empty.Add = %v", got)
	}
	if got := l.Add(types.NewStringList(types.DefaultTypeAdapter, nil)); got != l {
		t.Errorf("Add(empty) = %v", got)
	}
	if !types.IsError(l.Add(types.String("x"))) {
		t.Error("Add(string) succeeded")
	}
	if l.Contains(types.String("b")) != types.True || l.Contains(types.String("z")) != types.False {
		t.Error("Contains")
	}
	if l.Get(types.Double(1)) != types.String("b") || l.Get(types.Uint(0)) != types.String("a") {
		t.Error("Get numeric index")
	}
	if !types.IsError(l.Get(types.String("0"))) || !types.IsError(l.Get(types.Int(2))) || !types.IsError(l.Get(types.Int(-1))) {
		t.Error("Get bad index")
	}
	for _, tc := range []struct {
		other ref.Val
		want  ref.Val
	}{
		{types.NewStringList(types.DefaultTypeAdapter, []string{"a", "b"}), types.True},
		{types.NewStringList(types.DefaultTypeAdapter, []string{"a"}), types.False},
		{types.NewStringList(types.DefaultTypeAdapter, []string{"a", "c"}), types.False},
		{types.String("a"), types.False},
	} {
		if got := l.Equal(tc.other); got != tc.want {
			t.Errorf("Equal(%v) = %v", tc.other, got)
		}
	}
	if got, err := l.ConvertToNative(reflect.TypeFor[[]string]()); err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ConvertToNative([]string) = %v, %v", got, err)
	}
	bad := listView[*jsonArray]{&jsonArray{[]any{1.5}}}
	if _, err := bad.ConvertToNative(reflect.TypeFor[[]any]()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ConvertToNative over a bad native = %v", err)
	}
	if l.ConvertToType(types.ListType) != l || l.ConvertToType(types.TypeType) != types.ListType || !types.IsError(l.ConvertToType(types.MapType)) {
		t.Error("ConvertToType")
	}
	if l.Type() != types.ListType || !reflect.DeepEqual(l.Value(), []string{"a", "b"}) {
		t.Error("Type or Value")
	}
	jl := listView[*jsonArray]{&jsonArray{[]any{"x"}}}
	if !reflect.DeepEqual(jl.Value(), []any{"x"}) {
		t.Error("jsonArray Value")
	}
	it := l.Iterator()
	var got []ref.Val
	for it.HasNext() == types.True {
		got = append(got, it.Next())
	}
	if len(got) != 2 || it.Next() != nil {
		t.Errorf("iteration %v", got)
	}
}

// TestIterators covers the iterator ref.Val methods.
func TestIterators(t *testing.T) {
	for _, it := range []traits.Iterator{&keyIterator{keys: []string{"a"}}, &listIterator[stringsSource]{s: stringsSource{&[]string{"a"}}}} {
		if it.Next() != types.String("a") || it.HasNext() != types.False || it.Next() != nil {
			t.Error("iteration")
		}
		if _, err := it.ConvertToNative(reflect.TypeFor[any]()); !errors.Is(err, ErrUnsupported) {
			t.Error("ConvertToNative")
		}
		if !types.IsError(it.ConvertToType(types.StringType)) || !types.IsError(it.Equal(types.String("a"))) {
			t.Error("ConvertToType or Equal")
		}
		if it.Type() != types.IteratorType || it.Value() != nil {
			t.Error("Type or Value")
		}
	}
}

// TestObjectMethods covers the object views' ref.Val, Indexer and
// FieldTester methods.
func TestObjectMethods(t *testing.T) {
	r := &expr.Request{Method: "GET", Path: "/a"}
	o := object[expr.Request]{r}
	if got, err := o.ConvertToNative(reflect.TypeFor[*expr.Request]()); err != nil || got != r {
		t.Errorf("ConvertToNative(*Request) = %v, %v", got, err)
	}
	if got, err := o.ConvertToNative(reflect.TypeFor[any]()); err != nil || got != r {
		t.Errorf("ConvertToNative(any) = %v, %v", got, err)
	}
	if _, err := o.ConvertToNative(reflect.TypeFor[string]()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ConvertToNative(string) = %v", err)
	}
	if tt, ok := o.ConvertToType(types.TypeType).(*types.Type); !ok || tt.TypeName() != TypeRequest {
		t.Errorf("ConvertToType(type) = %v", tt)
	}
	if o.ConvertToType(kindRequest) != o || !types.IsError(o.ConvertToType(types.StringType)) {
		t.Error("ConvertToType")
	}
	if o.Type().TypeName() != TypeRequest || o.Value() != r {
		t.Error("Type or Value")
	}
	// Equality: identity, equal fields, different fields, other types.
	twin := object[expr.Request]{&expr.Request{Method: "GET", Path: "/a"}}
	diff := object[expr.Request]{&expr.Request{Method: "POST", Path: "/a"}}
	withBody := object[expr.Request]{&expr.Request{Method: "GET", Path: "/a", Body: Null()}}
	same := object[expr.Request]{r}
	if o.Equal(same) != types.True || o.Equal(twin) != types.True || o.Equal(diff) != types.False ||
		o.Equal(withBody) != types.False || o.Equal(types.String("x")) != types.False ||
		o.Equal(object[expr.Route]{&expr.Route{}}) != types.False {
		t.Error("Equal")
	}
	// Dynamic field access.
	if o.Get(types.String("method")) != types.String("GET") {
		t.Error("Get(method)")
	}
	if !types.IsError(o.Get(types.Int(1))) {
		t.Error("Get(1)")
	}
	if v := o.Get(types.String("bogus")); !errors.Is(v.(error), ErrNoSuchKey) {
		t.Errorf("Get(bogus) = %v", v)
	}
	if v := o.Get(types.String("body")); !errors.Is(v.(error), ErrBody) {
		t.Errorf("Get(body) = %v", v)
	}
	if o.IsSet(types.String("method")) != types.True || o.IsSet(types.String("host")) != types.False {
		t.Error("IsSet")
	}
	if !types.IsError(o.IsSet(types.Int(1))) || !types.IsError(o.IsSet(types.String("bogus"))) {
		t.Error("IsSet bad field")
	}
	// Every kind reports its own type.
	views := []ref.Val{
		viewOf(&expr.Request{}), viewOf(&expr.Source{}), viewOf(&expr.Route{}), viewOf(&expr.Consumer{}),
		viewOf(&expr.Auth{}), viewOf(&expr.Response{}), viewOf(&expr.AttemptError{}), viewOf(&expr.Upstream{}),
		viewOf(&expr.Step{}), viewOf(&expr.AI{}),
	}
	for k, v := range views {
		if v.Type() != objKind(k) {
			t.Errorf("view %d type %v", k, v.Type())
		}
	}
}

// TestFieldAccessors covers getField, isFieldSet and their errors on every
// kind.
func TestFieldAccessors(t *testing.T) {
	targets := []any{
		&expr.Request{}, &expr.Source{}, &expr.Route{}, &expr.Consumer{}, &expr.Auth{},
		&expr.Response{}, &expr.AttemptError{}, &expr.Upstream{}, &expr.Step{}, &expr.AI{},
	}
	nils := []any{
		(*expr.Request)(nil), (*expr.Source)(nil), (*expr.Route)(nil), (*expr.Consumer)(nil), (*expr.Auth)(nil),
		(*expr.Response)(nil), (*expr.AttemptError)(nil), (*expr.Upstream)(nil), (*expr.Step)(nil), (*expr.AI)(nil),
	}
	for k := range numKinds {
		for i := range objectFields(k) {
			// Empty views: fields read, and nothing is set.
			v, err := getField(k, i, targets[k])
			body := objectFields(k)[i].name == "body"
			if body != (err != nil) {
				t.Errorf("%s field %d on an empty view: %v, %v", k.TypeName(), i, v, err)
			}
			if isFieldSet(k, i, targets[k]) {
				t.Errorf("%s field %d set on an empty view", k.TypeName(), i)
			}
			for _, null := range []any{types.NullValue, types.NullValue.Value(), nil, nils[k]} {
				if _, err := getField(k, i, null); !errors.Is(err, ErrNull) {
					t.Errorf("%s field %d on %v: %v", k.TypeName(), i, null, err)
				}
				if isFieldSet(k, i, null) {
					t.Errorf("%s field %d set on null", k.TypeName(), i)
				}
			}
			if _, err := getField(k, i, types.String("x")); !errors.Is(err, ErrUnsupported) {
				t.Errorf("%s field %d on a string: %v", k.TypeName(), i, err)
			}
			if _, err := getField(k, i, 42); !errors.Is(err, ErrUnsupported) {
				t.Errorf("%s field %d on an int: %v", k.TypeName(), i, err)
			}
		}
	}
	if _, err := getField(numKinds, 0, &expr.Request{}); err == nil || isFieldSet(numKinds, 0, &expr.Request{}) {
		t.Error("numKinds accessor")
	}
	if err := selectError(kindRequest, 99, nil); !errors.Is(err, ErrNull) {
		t.Errorf("selectError out of range = %v", err)
	}
	// Getter and tester closures are the provider's accessors.
	if v, err := getter(kindError, errKind)(&expr.AttemptError{Kind: "tls"}); err != nil || v != types.String("tls") {
		t.Errorf("getter = %v, %v", v, err)
	}
	if !tester(kindError, errKind)(&expr.AttemptError{Kind: "tls"}) {
		t.Error("tester")
	}
}
