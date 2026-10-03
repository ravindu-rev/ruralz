// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"fmt"
	"reflect"
	"slices"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// mapSource is the storage behind a string-keyed CEL map view. Sources hold
// pointers only, so views are comparable and hashable: cel-go uses values
// as Go map keys (map literals, set membership), which would panic on a Go
// map or slice.
type mapSource interface {
	comparable
	// lookup returns the CEL value of key.
	lookup(key string) (ref.Val, bool)
	// sortedKeys returns the distinct keys in ascending byte order.
	sortedKeys() []string
	// size returns the number of distinct keys.
	size() int
	// raw returns the Go value behind the view (ref.Val.Value).
	raw() any
}

// mapView is a read-only CEL map(string, V) over a mapSource. Every source
// is one pointer, so the view boxes without allocating. Iteration is in
// ascending key order (03 req 6.3).
type mapView[S mapSource] struct{ s S }

// Find returns the value of a string key; any other key is absent.
func (m mapView[S]) Find(key ref.Val) (ref.Val, bool) {
	switch k := key.(type) {
	case types.String:
		return m.s.lookup(string(k))
	case *types.Err, *types.Unknown:
		return key, false
	default:
		return nil, false
	}
}

// Get returns the value of key or a no-such-key error.
func (m mapView[S]) Get(key ref.Val) ref.Val {
	v, found := m.Find(key)
	if found {
		return v
	}
	if v != nil {
		return v
	}
	return errVal(noSuchKey())
}

// Contains reports whether key is present.
func (m mapView[S]) Contains(key ref.Val) ref.Val {
	v, found := m.Find(key)
	if !found && v != nil {
		return v
	}
	return types.Bool(found)
}

// Size returns the number of keys.
func (m mapView[S]) Size() ref.Val { return types.Int(m.s.size()) }

// Iterator iterates the keys in ascending byte order.
func (m mapView[S]) Iterator() traits.Iterator { return &keyIterator{keys: m.s.sortedKeys()} }

// Equal compares as CEL maps: same size, equal values under every key.
func (m mapView[S]) Equal(other ref.Val) ref.Val { return mapEqual(m, other) }

// ConvertToNative converts through a cel-go map of the same entries.
func (m mapView[S]) ConvertToNative(typeDesc reflect.Type) (any, error) {
	return mapToNative(m, typeDesc)
}

// ConvertToType supports type(x) and the identity conversion.
func (m mapView[S]) ConvertToType(t ref.Type) ref.Val {
	switch t {
	case types.MapType:
		return m
	case types.TypeType:
		return types.MapType
	}
	return types.NewErr("type conversion error from '%s' to '%s'", types.MapType, t)
}

// Type returns the CEL map type.
func (m mapView[S]) Type() ref.Type { return types.MapType }

// Value returns the Go value behind the view.
func (m mapView[S]) Value() any { return m.s.raw() }

// noSuchKey is the error of a missing key; the key is left out because it
// may come from request data.
func noSuchKey() error { return fmt.Errorf("%w in map", ErrNoSuchKey) }

// mapEqual is CEL map equality of m and other (cel-go's semantics).
func mapEqual(m traits.Mapper, other ref.Val) ref.Val {
	o, ok := other.(traits.Mapper)
	if !ok {
		return types.False
	}
	if m.Size().Equal(o.Size()) != types.True {
		return types.False
	}
	it := m.Iterator()
	for it.HasNext() == types.True {
		k := it.Next()
		a, _ := m.Find(k)
		b, found := o.Find(k)
		if !found {
			return types.False
		}
		if types.Equal(a, b) == types.False {
			return types.False
		}
	}
	return types.True
}

// mapToNative converts m through a cel-go map holding the same entries, so
// every cel-go target type (Go maps, JSON structs) is supported.
func mapToNative(m traits.Mapper, typeDesc reflect.Type) (any, error) {
	entries := make(map[ref.Val]ref.Val)
	it := m.Iterator()
	for it.HasNext() == types.True {
		k := it.Next()
		v := m.Get(k)
		if e, ok := v.(*types.Err); ok {
			return nil, e
		}
		entries[k] = v
	}
	return types.NewRefValMap(types.DefaultTypeAdapter, entries).ConvertToNative(typeDesc)
}

// keyIterator iterates string keys in the order given.
type keyIterator struct {
	iteratorBase
	keys []string
	i    int
}

// HasNext reports whether a key remains.
func (it *keyIterator) HasNext() ref.Val { return types.Bool(it.i < len(it.keys)) }

// Next returns the next key, or nil past the end.
func (it *keyIterator) Next() ref.Val {
	if it.i >= len(it.keys) {
		return nil
	}
	k := it.keys[it.i]
	it.i++
	return types.String(k)
}

// iteratorBase holds the ref.Val methods of an iterator, which CEL never
// converts or compares.
type iteratorBase struct{}

// ConvertToNative is not supported on iterators.
func (iteratorBase) ConvertToNative(reflect.Type) (any, error) {
	return nil, fmt.Errorf("%w: iterator conversion", ErrUnsupported)
}

// ConvertToType is not supported on iterators. The error is new on every
// call: cel-go's types.NoSuchOverloadErr is one shared value, which an
// evaluation must never be handed (it labels error values in place).
func (iteratorBase) ConvertToType(ref.Type) ref.Val { return types.NewErr("no such overload") }

// Equal is not supported on iterators (a new error, as for ConvertToType).
func (iteratorBase) Equal(ref.Val) ref.Val { return types.NewErr("no such overload") }

// Type returns the cel-go iterator type.
func (iteratorBase) Type() ref.Type { return types.IteratorType }

// Value returns nil.
func (iteratorBase) Value() any { return nil }

// stringSource is a map[string]string field of a view (labels), addressed
// by pointer; a nil pointer is an empty map.
type stringSource struct{ m *map[string]string }

// strings returns the map.
func (s stringSource) strings() map[string]string {
	if s.m == nil {
		return nil
	}
	return *s.m
}

func (s stringSource) lookup(key string) (ref.Val, bool) { return lookupString(s.strings(), key) }

func (s stringSource) sortedKeys() []string { return sortedMapKeys(s.strings()) }

func (s stringSource) size() int { return len(s.strings()) }

func (s stringSource) raw() any { return s.strings() }

// lookupString returns the CEL string under key.
func lookupString(m map[string]string, key string) (ref.Val, bool) {
	v, ok := m[key]
	if !ok {
		return nil, false
	}
	return types.String(v), true
}

// querySource is request.query: parsed from the raw query on first use and
// cached in the view (03 req 20).
type querySource struct{ r *expr.Request }

func (s querySource) lookup(key string) (ref.Val, bool) { return lookupString(s.r.Query(), key) }

func (s querySource) sortedKeys() []string { return sortedMapKeys(s.r.Query()) }

func (s querySource) size() int { return len(s.r.Query()) }

func (s querySource) raw() any { return s.r.Query() }

// sortedStrings is a map[string]string with its keys sorted once, for the
// labels of a prepared Route or Consumer.
type sortedStrings struct {
	m    map[string]string
	keys []string
}

func (s *sortedStrings) lookup(key string) (ref.Val, bool) { return lookupString(s.m, key) }

func (s *sortedStrings) sortedKeys() []string { return s.keys }

func (s *sortedStrings) size() int { return len(s.m) }

func (s *sortedStrings) raw() any { return s.m }

// sortedMapKeys returns the keys of m in ascending byte order.
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// paramSource is request.pathParams: template captures by name (the first
// capture wins if a name repeats).
type paramSource struct{ p *[]expr.Param }

func (s paramSource) lookup(key string) (ref.Val, bool) {
	for _, p := range *s.p {
		if p.Name == key {
			return types.String(p.Value), true
		}
	}
	return nil, false
}

func (s paramSource) sortedKeys() []string {
	keys := make([]string, 0, len(*s.p))
	for _, p := range *s.p {
		keys = append(keys, p.Name)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

func (s paramSource) size() int {
	ps := *s.p
	n := 0
	for i := range ps {
		if !slices.ContainsFunc(ps[:i], func(q expr.Param) bool { return q.Name == ps[i].Name }) {
			n++
		}
	}
	return n
}

func (s paramSource) raw() any {
	m := make(map[string]string, len(*s.p))
	for _, p := range *s.p {
		if _, dup := m[p.Name]; !dup {
			m[p.Name] = p.Value
		}
	}
	return m
}

// stepsSource is the steps variable: completed composition steps by name.
type stepsSource struct{ s *expr.Steps }

func (s stepsSource) lookup(key string) (ref.Val, bool) {
	if s.s == nil {
		return nil, false
	}
	st, ok := s.s.Get(key)
	if !ok {
		return nil, false
	}
	return object[expr.Step]{&st}, true
}

func (s stepsSource) sortedKeys() []string {
	if s.s == nil {
		return nil
	}
	keys := slices.Clone(s.s.Names())
	slices.Sort(keys)
	return slices.Compact(keys)
}

func (s stepsSource) size() int { return len(s.sortedKeys()) }

func (s stepsSource) raw() any { return s.s }

// jsonObject is a decoded JSON object; members convert on selection. A nil
// *jsonObject is the empty map (auth.claims for methods other than jwt).
type jsonObject struct{ m map[string]any }

// tree returns the object's map.
func (o *jsonObject) tree() map[string]any {
	if o == nil {
		return nil
	}
	return o.m
}

func (o *jsonObject) lookup(key string) (ref.Val, bool) {
	v, ok := o.tree()[key]
	if !ok {
		return nil, false
	}
	return nativeVal(v), true
}

func (o *jsonObject) sortedKeys() []string { return sortedMapKeys(o.tree()) }

func (o *jsonObject) size() int { return len(o.tree()) }

func (o *jsonObject) raw() any { return o.tree() }
