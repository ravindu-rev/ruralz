// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// decodeChunk is the most nodes one encoding/json call decodes where the
// value can be split, so a worker reads its yield timer between calls
// (01 req 53): a 12,000-entry list is about fifty calls, not one.
const decodeChunk = 256

// chunker decodes a tree into a Go value in pieces of at most limit
// nodes, each with one encoding/json call, with the result one call over
// the whole tree would give. It splits only where encoding/json's result
// is the same piece by piece, on fresh (zero) targets:
//
//   - A struct receives batches of its members as objects: encoding/json
//     decodes an object into a struct member by member, in order, into
//     the fields it matches, so consecutive batches set the same fields.
//     A large member value is decoded into its field directly when the
//     member name is exactly the JSON name of one field of the struct
//     itself (not promoted, not a ",string" field), else as a one-member
//     batch.
//   - A map with string keys receives batches of its members, into the
//     same map (encoding/json keeps the entries it has; member names are
//     unique); a large member value is decoded into a new element.
//   - A slice is allocated at its final length; batches of elements are
//     decoded as arrays into a new slice and copied into place, a large
//     element into its place.
//   - A pointer is allocated as encoding/json would.
//
// Everything else is decoded whole, as one piece: a null, a value of a
// type with its own UnmarshalJSON or UnmarshalText, an interface, an
// array, and a value whose JSON type does not match its Go type, so
// encoding/json reports the mismatch as it would for the whole tree.
type chunker struct {
	w     *work
	limit int
	buf   []byte
}

// decode decodes n into v, an addressable value; f filters the members of
// a config as appendJSON does.
func (c *chunker) decode(n *tree.Node, v reflect.Value, f *declaredOnly) error {
	if n == nil {
		return nil
	}
	t := v.Type()
	if n.Kind != tree.KindNull && reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		// encoding/json would hand UnmarshalJSON the value's bytes,
		// which appendJSON writes compactly; skip its scan of them.
		return c.unmarshaler(n, v, f)
	}
	if size(n, c.limit) < c.limit || !splittable(t, n) {
		return c.whole(n, v, f)
	}
	switch t.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(t.Elem()))
		}
		return c.decode(n, v.Elem(), f)
	case reflect.Map:
		if v.IsNil() {
			v.Set(reflect.MakeMap(t))
		}
		return c.object(n, v, f)
	case reflect.Struct:
		return c.object(n, v, f)
	default: // reflect.Slice, by splittable.
		return c.list(n, v, f)
	}
}

// splittable reports a Go type the chunker may split for JSON value n.
func splittable(t reflect.Type, n *tree.Node) bool {
	if n.Kind == tree.KindNull || textType(t) {
		return false
	}
	switch t.Kind() {
	case reflect.Pointer:
		return true
	case reflect.Struct:
		return n.Kind == tree.KindMap
	case reflect.Map:
		return n.Kind == tree.KindMap && t.Key().Kind() == reflect.String && !textType(t.Key())
	case reflect.Slice:
		return n.Kind == tree.KindList && t.Elem().Kind() != reflect.Uint8
	default:
		return false
	}
}

// textType reports a type encoding/json decodes through UnmarshalText.
func textType(t reflect.Type) bool {
	return reflect.PointerTo(t).Implements(reflect.TypeFor[encoding.TextUnmarshaler]())
}

// size counts the nodes of n, stopping at limit.
func size(n *tree.Node, limit int) int {
	if n == nil {
		return 0
	}
	count := 1
	for _, m := range n.Members {
		if count >= limit {
			return count
		}
		count += size(m.Value, limit-count)
	}
	for _, it := range n.Items {
		if count >= limit {
			return count
		}
		count += size(it, limit-count)
	}
	return count
}

// whole decodes n into v with one encoding/json call.
func (c *chunker) whole(n *tree.Node, v reflect.Value, f *declaredOnly) error {
	var err error
	if c.buf, err = appendJSON(c.buf[:0], n, f, c.w); err != nil {
		return err
	}
	return c.w.decodeInto(c.buf, v.Addr().Interface())
}

// unmarshaler calls the UnmarshalJSON of v's type with n's JSON text.
func (c *chunker) unmarshaler(n *tree.Node, v reflect.Value, f *declaredOnly) error {
	var err error
	if c.buf, err = appendJSON(c.buf[:0], n, f, c.w); err != nil {
		return err
	}
	u, _ := v.Addr().Interface().(json.Unmarshaler)
	c.w.check()
	err = u.UnmarshalJSON(c.buf)
	c.w.check()
	return err
}

// object decodes the members of n into v, a struct or a non-nil map.
func (c *chunker) object(n *tree.Node, v reflect.Value, f *declaredOnly) error {
	var sel *schemaidx.Node
	structLike := false
	if f != nil {
		sel = f.schema.Select(n)
		structLike = isStruct(sel)
	}
	open, pending := false, 0
	flush := func() error {
		if !open {
			return nil
		}
		open, pending = false, 0
		c.buf = append(c.buf, '}')
		return c.w.decodeInto(c.buf, v.Addr().Interface())
	}
	for _, m := range n.Members {
		var child *declaredOnly
		if f != nil {
			if _, declared := sel.Property(m.Key); structLike && !declared {
				continue
			}
			child = &declaredOnly{memberSchema(sel, m.Key)}
		}
		k := size(m.Value, c.limit)
		if pending > 0 && pending+k > c.limit {
			if err := flush(); err != nil {
				return err
			}
		}
		if k >= c.limit {
			if into, ok := memberTarget(v, m.Key); ok {
				if err := c.decode(m.Value, into, child); err != nil {
					return err
				}
				if v.Kind() == reflect.Map {
					v.SetMapIndex(reflect.ValueOf(m.Key).Convert(v.Type().Key()), into)
				}
				continue
			}
		}
		if open {
			c.buf = append(c.buf, ',')
		} else {
			c.buf = append(c.buf[:0], '{')
			open = true
		}
		var err error
		if c.buf, err = jsonval.AppendString(c.buf, m.Key); err != nil {
			return err
		}
		c.buf = append(c.buf, ':')
		if c.buf, err = appendJSON(c.buf, m.Value, child, c.w); err != nil {
			return err
		}
		pending += k
	}
	return flush()
}

// memberTarget returns where a large member value goes: a new element of
// a map, or the field of a struct whose JSON name is exactly name, when
// encoding/json would pick that field without ambiguity.
func memberTarget(v reflect.Value, name string) (reflect.Value, bool) {
	t := v.Type()
	if t.Kind() == reflect.Map {
		return reflect.New(t.Elem()).Elem(), true
	}
	index, found := -1, 0
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() || sf.Anonymous {
			continue
		}
		tag := sf.Tag.Get("json")
		jsonName, opts, _ := strings.Cut(tag, ",")
		if jsonName == "-" && opts == "" && !strings.HasPrefix(tag, "-,") {
			continue
		}
		if jsonName == "" {
			jsonName = sf.Name
		}
		if jsonName != name {
			continue
		}
		found++
		if !strings.Contains(","+opts+",", ",string,") {
			index = i
		}
	}
	if found != 1 || index < 0 {
		return reflect.Value{}, false
	}
	return v.Field(index), true
}

// list decodes the elements of n into v, a slice.
func (c *chunker) list(n *tree.Node, v reflect.Value, f *declaredOnly) error {
	t := v.Type()
	v.Set(reflect.MakeSlice(t, len(n.Items), len(n.Items)))
	var child *declaredOnly
	if f != nil {
		child = &declaredOnly{itemSchema(f.schema)}
	}
	open, pending, start := false, 0, 0
	flush := func(end int) error {
		if !open {
			return nil
		}
		open, pending = false, 0
		c.buf = append(c.buf, ']')
		tmp := reflect.New(t)
		if err := c.w.decodeInto(c.buf, tmp.Interface()); err != nil {
			return err
		}
		reflect.Copy(v.Slice(start, end), tmp.Elem())
		return nil
	}
	for i, it := range n.Items {
		k := size(it, c.limit)
		if pending > 0 && pending+k > c.limit {
			if err := flush(i); err != nil {
				return err
			}
		}
		if k >= c.limit {
			if err := c.decode(it, v.Index(i), child); err != nil {
				return err
			}
			continue
		}
		if open {
			c.buf = append(c.buf, ',')
		} else {
			c.buf = append(c.buf[:0], '[')
			open, start = true, i
		}
		var err error
		if c.buf, err = appendJSON(c.buf, it, child, c.w); err != nil {
			return err
		}
		pending += k
	}
	return flush(len(n.Items))
}
