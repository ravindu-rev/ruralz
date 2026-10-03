// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"iter"
	"maps"
	"slices"
	"strings"
)

// Member is one object member.
type Member struct {
	// Name is the decoded member name.
	Name string
	// Value is the member value, a tree value.
	Value any
}

// Object is a mutable JSON object whose members keep their order: input
// order after Decode, then insertion order. Names are unique. Lookups are
// linear up to 8 members; a larger object keeps a name index that Decode,
// Clone and every mutation maintain, so reads (Len, At, All, Index, Get and
// Has) never write. Any number of goroutines may therefore read an object,
// such as a decoded body shared by every reader (03 req 25; 07 req 56),
// while none changes it; a mutation needs exclusive access. The zero Object
// is empty and ready to use; a nil *Object reads as empty.
type Object struct {
	members []Member
	index   map[string]int
}

// indexedMembers is the member count above which lookups use an index.
const indexedMembers = 8

// NewObject returns an empty object with room for n members.
func NewObject(n int) *Object {
	return &Object{members: make([]Member, 0, max(n, 0))}
}

// Len returns the number of members.
func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.members)
}

// At returns member i in order; it panics when i is out of range.
func (o *Object) At(i int) Member { return o.members[i] }

// All yields the members in order. The object must not change during the
// iteration except through SetAt.
func (o *Object) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		if o == nil {
			return
		}
		for i := range o.members {
			if !yield(o.members[i].Name, o.members[i].Value) {
				return
			}
		}
	}
}

// Index returns the position of member name, or -1. It never writes.
func (o *Object) Index(name string) int {
	if o == nil {
		return -1
	}
	if o.index != nil {
		if i, ok := o.index[name]; ok {
			return i
		}
		return -1
	}
	for i := range o.members {
		if o.members[i].Name == name {
			return i
		}
	}
	return -1
}

// reindex makes the index match the members: built above indexedMembers
// and absent otherwise.
func (o *Object) reindex() {
	if len(o.members) <= indexedMembers {
		o.index = nil
		return
	}
	if o.index == nil {
		o.index = make(map[string]int, len(o.members))
	} else {
		clear(o.index)
	}
	for i := range o.members {
		o.index[o.members[i].Name] = i
	}
}

// indexAppended records the last member in the index, building the index
// when the object grows past indexedMembers.
func (o *Object) indexAppended() {
	switch n := len(o.members); {
	case o.index != nil:
		o.index[o.members[n-1].Name] = n - 1
	case n > indexedMembers:
		o.reindex()
	}
}

// Get returns the value of member name.
func (o *Object) Get(name string) (any, bool) {
	if i := o.Index(name); i >= 0 {
		return o.members[i].Value, true
	}
	return nil, false
}

// Has reports whether member name exists.
func (o *Object) Has(name string) bool { return o.Index(name) >= 0 }

// Set replaces the value of member name in place, or appends the member.
func (o *Object) Set(name string, v any) {
	if i := o.Index(name); i >= 0 {
		o.members[i].Value = v
		return
	}
	o.members = append(o.members, Member{Name: name, Value: v})
	o.indexAppended()
}

// SetAt replaces the value of member i; it panics when i is out of range.
func (o *Object) SetAt(i int, v any) { o.members[i].Value = v }

// Delete removes member name, keeping the order of the others, and reports
// whether it existed.
func (o *Object) Delete(name string) bool {
	i := o.Index(name)
	if i < 0 {
		return false
	}
	o.DeleteAt(i)
	return true
}

// DeleteAt removes member i, keeping the order of the others; it panics
// when i is out of range.
func (o *Object) DeleteAt(i int) {
	name := o.members[i].Name
	o.members = slices.Delete(o.members, i, i+1)
	if o.index == nil || len(o.members) <= indexedMembers {
		o.reindex()
		return
	}
	delete(o.index, name)
	for j := i; j < len(o.members); j++ {
		o.index[o.members[j].Name] = j
	}
}

// Sort orders the members by ascending byte order of their names, the
// order Append writes (07 req 60).
func (o *Object) Sort() {
	if o == nil {
		return
	}
	slices.SortFunc(o.members, func(a, b Member) int { return strings.Compare(a.Name, b.Name) })
	if o.index == nil {
		o.reindex()
		return
	}
	for i := range o.members {
		o.index[o.members[i].Name] = i
	}
}

// Clone returns a deep copy, indexed like a decoded object.
func (o *Object) Clone() *Object {
	if o == nil {
		return nil
	}
	c := &Object{members: make([]Member, len(o.members))}
	for i, m := range o.members {
		c.members[i] = Member{Name: m.Name, Value: Clone(m.Value)}
	}
	if o.index != nil {
		c.index = maps.Clone(o.index)
	} else {
		c.reindex()
	}
	return c
}
