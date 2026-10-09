// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaidx

import (
	"fmt"
	"iter"
	"slices"
)

// Field is one schema position reachable from a resource root, as Fields
// enumerates it.
type Field struct {
	// Kind is the resource kind.
	Kind string
	// Where lists the if/then rules applied on the way, outermost first,
	// such as type == "quota" at "spec" for the fields of a quota Policy
	// config.
	Where []Dispatch
	// Path is the schema path (see Index.SchemaPath); "" is the root.
	Path string
	// Node is the schema.
	Node *Node
}

// Dispatch is one if/then/else result applied to the object at schema
// path Path: the rule whose Conditions matched, every other rule's else
// branch applying; with no Conditions, no rule matched and every else
// branch applies.
type Dispatch struct {
	// Path is the schema path of the object the rules test.
	Path string
	// Conditions are the matching rule's member consts; nil when no rule
	// matched.
	Conditions []Condition
}

// Enumeration bounds. Fields lists paths, not nodes, so a schema that
// reuses definitions as a DAG (each referenced twice, nested deeply) has
// exponentially many fields although it resolves to few nodes; Load rejects
// a schema past either bound, so Fields stays bounded for every Index. Both
// are about 100 times what the committed ruralz/v1alpha1 schema needs.
const (
	// maxFields bounds the fields of all kinds together.
	maxFields = 1 << 16
	// maxFieldBytes bounds the summed length of their schema paths.
	maxFieldBytes = 1 << 22
)

// Fields enumerates every schema position of every kind: the resource
// root, each declared member, array elements ("[]") and typed-map values
// ("{}"), depth first with members in name order. Below an object with
// if/then/else dispatch it adds the positions each rule changes when it
// alone matches, then those the else branches change when none matches,
// each with its Dispatch appended to Where. When a branch adds
// x-ruralz-* keywords to the dispatched object itself, that object is
// listed again at its path with the Dispatch, so Fields reports every
// marker Lookup and Walk apply. A recursive schema is listed once per
// cycle. Consumers enumerate markers from it instead of hard-coding field
// lists (R-62): defaults, keyed lists, CEL places, secrets, references
// and impact classes. Load bounds the enumeration (at most 65,536
// fields), so the result is always complete.
func (x *Index) Fields() []Field {
	return slices.Collect(x.FieldsSeq())
}

// FieldsSeq iterates the fields Fields lists, in the same order, without
// building the whole slice; a consumer that stops early pays only for
// the fields it read.
func (x *Index) FieldsSeq() iter.Seq[Field] {
	return func(yield func(Field) bool) {
		x.enumerate(func(kind string, path []byte, n *Node, where []Dispatch) bool {
			return yield(Field{Kind: kind, Where: slices.Clone(where), Path: string(path), Node: n})
		})
	}
}

// checkFields runs the enumeration without building fields and fails when
// it passes either bound.
func (x *Index) checkFields(limit, byteLimit int) error {
	count, size := 0, 0
	over := false
	x.enumerate(func(_ string, path []byte, _ *Node, _ []Dispatch) bool {
		count++
		size += len(path)
		over = count > limit || size > byteLimit
		return !over
	})
	if over {
		return fmt.Errorf("%w: more than %d schema paths or %d bytes of them; "+
			"a definition reused at many nested positions multiplies the paths", ErrSchema, limit, byteLimit)
	}
	return nil
}

// emitFunc receives one field: path and where are valid only during the
// call. Returning false stops the enumeration.
type emitFunc func(kind string, path []byte, n *Node, where []Dispatch) bool

// enumerate calls emit for every field of every kind until it returns
// false.
func (x *Index) enumerate(emit emitFunc) {
	for _, kind := range x.kinds {
		e := enumerator{kind: kind, emit: emit}
		e.visit(x.resources[kind], nil)
		if e.stopped {
			return
		}
	}
}

type enumerator struct {
	kind    string
	emit    emitFunc
	stopped bool
	stack   []*Node
	// path is the schema path of the current position, grown and cut back
	// as the enumeration descends, so no path is built per level.
	path []byte
}

func (e *enumerator) visit(n *Node, where []Dispatch) {
	if e.stopped {
		return
	}
	if !e.emit(e.kind, e.path, n, where) {
		e.stopped = true
		return
	}
	if slices.Contains(e.stack, n) {
		return
	}
	e.stack = append(e.stack, n)
	defer func() { e.stack = e.stack[:len(e.stack)-1] }()
	e.children(n, nil, where)
	if len(n.cases) == 0 {
		return
	}
	path := string(e.path)
	mask := make([]byte, len(n.cases))
	for i := range mask {
		mask[i] = '0'
	}
	for i, c := range n.cases {
		mask[i] = '1'
		d := Dispatch{Path: path, Conditions: slices.Clone(c.ifc.consts)}
		e.variant(n, mask, append(slices.Clone(where), d))
		mask[i] = '0'
	}
	e.variant(n, mask, append(slices.Clone(where), Dispatch{Path: path}))
}

// variant visits the positions that the dispatch result for mask changes.
// It does nothing once the consumer stopped the enumeration, since a
// FieldsSeq yield must not be called again after it returned false.
func (e *enumerator) variant(n *Node, mask []byte, where []Dispatch) {
	if e.stopped {
		return
	}
	v, ok := n.variants[string(mask)]
	if !ok {
		v = n.runtimeVariant(mask)
	}
	if v != n {
		if !sameKeywords(v.kw, n.kw) {
			if !e.emit(e.kind, e.path, v, where) {
				e.stopped = true
				return
			}
		}
		e.children(v, n, where)
	}
}

// sameKeywords reports equal annotations; mergeKeywords only adds, so equal
// Validations lengths mean equal lists.
func sameKeywords(a, b Keywords) bool {
	return a.List == b.List && a.ListKey == b.ListKey && a.Ref == b.Ref && a.Secret == b.Secret &&
		a.CEL == b.CEL && a.Impact == b.Impact && a.Since == b.Since && len(a.Validations) == len(b.Validations)
}

// children visits the members, elements and values of n; with base set it
// skips those whose schema equals base's (unchanged by a dispatch branch).
func (e *enumerator) children(n, base *Node, where []Dispatch) {
	mark := len(e.path)
	for name, child := range n.Properties() {
		if prev, ok := base.Property(name); ok && prev == child {
			continue
		}
		if mark > 0 {
			e.path = append(e.path, '.')
		}
		e.path = append(e.path, name...)
		e.visit(child, where)
		e.path = e.path[:mark]
	}
	if it, ok := n.Items(); ok {
		if prev, _ := base.Items(); base == nil || prev != it {
			e.path = append(e.path, "[]"...)
			e.visit(it, where)
			e.path = e.path[:mark]
		}
	}
	if v, ok := n.Values(); ok {
		if prev, _ := base.Values(); base == nil || prev != v {
			e.path = append(e.path, "{}"...)
			e.visit(v, where)
			e.path = e.path[:mark]
		}
	}
}
