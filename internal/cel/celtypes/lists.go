// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"reflect"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// listSource is the storage behind a CEL list view; like mapSource, it
// holds pointers only.
type listSource interface {
	comparable
	// at returns element i, 0 <= i < length().
	at(i int) ref.Val
	// length returns the number of elements.
	length() int
	// raw returns the Go value behind the view (ref.Val.Value).
	raw() any
}

// listView is a read-only CEL list over a listSource.
type listView[S listSource] struct{ s S }

// Add concatenates other, a list.
func (l listView[S]) Add(other ref.Val) ref.Val {
	o, ok := other.(traits.Lister)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	if l.s.length() == 0 {
		return other
	}
	if o.Size() == types.IntZero {
		return l
	}
	elems := appendElems(nil, l)
	elems = appendElems(elems, o)
	return types.NewRefValList(types.DefaultTypeAdapter, elems)
}

// Contains reports whether an element equals elem.
func (l listView[S]) Contains(elem ref.Val) ref.Val {
	for i := range l.s.length() {
		if b, ok := elem.Equal(l.s.at(i)).(types.Bool); ok && b == types.True {
			return types.True
		}
	}
	return types.False
}

// Get returns the element at index.
func (l listView[S]) Get(index ref.Val) ref.Val {
	i, err := types.IndexOrError(index)
	if err != nil {
		return types.ValOrErr(index, "%v", err)
	}
	if i < 0 || i >= l.s.length() {
		return types.NewErr("index '%d' out of range in list size '%d'", i, l.s.length())
	}
	return l.s.at(i)
}

// Iterator iterates the elements in order.
func (l listView[S]) Iterator() traits.Iterator { return &listIterator[S]{s: l.s} }

// Size returns the number of elements.
func (l listView[S]) Size() ref.Val { return types.Int(l.s.length()) }

// Equal compares as CEL lists: same size, pairwise equal elements.
func (l listView[S]) Equal(other ref.Val) ref.Val {
	o, ok := other.(traits.Lister)
	if !ok {
		return types.False
	}
	if o.Size() != types.Int(l.s.length()) {
		return types.False
	}
	for i := range l.s.length() {
		if types.Equal(l.s.at(i), o.Get(types.Int(i))) == types.False {
			return types.False
		}
	}
	return types.True
}

// ConvertToNative converts through a cel-go list of the same elements.
func (l listView[S]) ConvertToNative(typeDesc reflect.Type) (any, error) {
	elems := appendElems(nil, l)
	for _, e := range elems {
		if err, ok := e.(*types.Err); ok {
			return nil, err
		}
	}
	return types.NewRefValList(types.DefaultTypeAdapter, elems).ConvertToNative(typeDesc)
}

// ConvertToType supports type(x) and the identity conversion.
func (l listView[S]) ConvertToType(t ref.Type) ref.Val {
	switch t {
	case types.ListType:
		return l
	case types.TypeType:
		return types.ListType
	}
	return types.NewErr("type conversion error from '%s' to '%s'", types.ListType, t)
}

// Type returns the CEL list type.
func (l listView[S]) Type() ref.Type { return types.ListType }

// Value returns the Go value behind the view.
func (l listView[S]) Value() any { return l.s.raw() }

// appendElems appends the elements of l to dst.
func appendElems(dst []ref.Val, l traits.Lister) []ref.Val {
	n := int(l.Size().(types.Int))
	for i := range n {
		dst = append(dst, l.Get(types.Int(i)))
	}
	return dst
}

// listIterator iterates a listSource in order.
type listIterator[S listSource] struct {
	iteratorBase
	s S
	i int
}

// HasNext reports whether an element remains.
func (it *listIterator[S]) HasNext() ref.Val { return types.Bool(it.i < it.s.length()) }

// Next returns the next element, or nil past the end.
func (it *listIterator[S]) Next() ref.Val {
	if it.i >= it.s.length() {
		return nil
	}
	v := it.s.at(it.i)
	it.i++
	return v
}

// stringsSource is a []string field of a view (consumer.tags, quotas),
// addressed by pointer so the view boxes without allocating.
type stringsSource struct{ p *[]string }

func (s stringsSource) at(i int) ref.Val { return types.String((*s.p)[i]) }

func (s stringsSource) length() int { return len(*s.p) }

func (s stringsSource) raw() any { return *s.p }

// jsonArray is a decoded JSON array; elements convert on selection.
type jsonArray struct{ l []any }

func (a *jsonArray) at(i int) ref.Val { return nativeVal(a.l[i]) }

func (a *jsonArray) length() int { return len(a.l) }

func (a *jsonArray) raw() any { return a.l }
