// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"encoding"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"unicode"

	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// decodeChunk is the most nodes one encoding/json call decodes where the
// value can be split, so a worker reads its yield timer between calls: a
// 12,000-entry list is about fifty calls, not one.
const decodeChunk = 256

// decodeBytes bounds the text of one piece where the value can be split:
// a value whose strings, member names and number literals hold more bytes
// is split as one of more than decodeChunk nodes is, so a few nodes of
// long strings are not one long call either.
const decodeBytes = 64 << 10

// chunker decodes a tree into a Go value in pieces of at most limit
// nodes, the object or list around a batch counted, and about bytes bytes
// of text (the bytes of the strings, member names and number literals of
// a piece, punctuation aside), each with one encoding/json call, with the
// result one call over the whole tree would give. A value is split when
// either bound would be passed; a scalar, and a member or element whose
// value cannot be split, is one piece whatever its size. It splits only
// where encoding/json's result is the same piece by piece, on fresh
// (zero) targets:
//
//   - A struct receives batches of its members as objects: encoding/json
//     decodes an object into a struct member by member, in order, into
//     the fields it matches, so consecutive batches set the same fields.
//     A large member value is decoded into its field directly when the
//     member name is exactly the JSON name of one field of the struct
//     itself (not promoted, not a ",string" field) and no other member
//     the decode sees has that name under case folding (strings.EqualFold,
//     as encoding/json matches names): only then is the field still zero
//     when its member comes, as a direct decode needs, since encoding/json
//     decodes a second member of a field into what the first left (a
//     slice keeps its elements). Otherwise the member is a one-member
//     batch, decoded by encoding/json into the field as it stands.
//   - A map with string keys receives batches of its members, into the
//     same map (encoding/json keeps the entries it has; member names are
//     unique); a large member value is decoded into a new element.
//   - A slice is allocated at its final length; batches of elements are
//     decoded as arrays into a new slice and copied into place, a large
//     element into its place.
//   - A nil empty interface (any), such as a member of a plugin config
//     (v1alpha1.PluginConfig) or of a Plugin's spec.configSchema, receives
//     what encoding/json builds for it: an object becomes a
//     map[string]any and a list an []any, each split as a map or a slice
//     is, and the leaves become json.Number, string or bool.
//   - A pointer is allocated as encoding/json would.
//
// Everything else is decoded whole, as one piece: a null, a value of a
// type with its own UnmarshalJSON or UnmarshalText, an interface that
// has methods or already holds a value, an array, and a value whose JSON
// type does not match its Go type, so encoding/json reports the mismatch
// as it would for the whole tree. After stage F and Normalize, a large
// value of these kinds reaches the chunker only as a type with its own
// UnmarshalJSON: v1alpha1.Duration, ByteSize and IntOrString take only
// scalars, the json.RawMessage of a Policy's spec.config copies its
// text, and v1alpha1.JSONSchemaDocument (a validation.json-schema
// config.schema) scans its text once with json.Valid and copies it. The
// last two are one pass over the value's compact JSON text, linear in
// its size, which no worker can yield inside; the text is built node by
// node with the yield timer running, and the timer is read again just
// before and after the call (TestDecodedTypes lists every Go type stage
// G decodes into).
type chunker struct {
	w *work
	// limit is the most nodes one encoding/json call decodes, and bytes
	// the most text bytes of one piece that is split further.
	limit, bytes int
	// buf holds the JSON text of the piece being built.
	buf []byte
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
	if k, b := c.size(n); (k <= c.limit && b <= c.bytes) || !splittable(v, n) {
		return c.whole(n, v, f, k)
	}
	switch t.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(t.Elem()))
		}
		return c.decode(n, v.Elem(), f)
	case reflect.Interface:
		return c.dynamic(n, v, f)
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

// splittable reports a target v the chunker may split for JSON value n.
func splittable(v reflect.Value, n *tree.Node) bool {
	t := v.Type()
	if n.Kind == tree.KindNull || textType(t) {
		return false
	}
	switch t.Kind() {
	case reflect.Pointer:
		return true
	case reflect.Interface:
		// encoding/json replaces a nil empty interface with the value
		// it builds; it decodes into whatever a non-nil one holds.
		return t.NumMethod() == 0 && v.IsNil() && (n.Kind == tree.KindMap || n.Kind == tree.KindList)
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

// dynamic decodes n, an object or a list, into v, a nil empty interface,
// as encoding/json does: into a new map[string]any or []any, which object
// and list fill piece by piece, decoding each large member or element
// into a new interface value in turn.
func (c *chunker) dynamic(n *tree.Node, v reflect.Value, f *declaredOnly) error {
	var into reflect.Value
	if n.Kind == tree.KindMap {
		into = reflect.New(reflect.TypeFor[map[string]any]()).Elem()
		into.Set(reflect.MakeMap(into.Type()))
		if err := c.object(n, into, f); err != nil {
			return err
		}
	} else {
		into = reflect.New(reflect.TypeFor[[]any]()).Elem()
		if err := c.list(n, into, f); err != nil {
			return err
		}
	}
	v.Set(into)
	return nil
}

// textType reports a type encoding/json decodes through UnmarshalText.
func textType(t reflect.Type) bool {
	return reflect.PointerTo(t).Implements(reflect.TypeFor[encoding.TextUnmarshaler]())
}

// size measures n: its nodes, counted up to c.limit+1, and its text
// bytes, counted up to c.bytes+1, so a result above either marks a value
// too large for one piece; the nodes it visits count as work (it reads
// the lengths of the texts, not their bytes).
func (c *chunker) size(n *tree.Node) (nodes, text int) {
	nodes, text = measure(n, c.limit+1, c.bytes+1)
	c.w.charge(nodes)
	return nodes, text
}

// measure counts the nodes of n and the bytes of its strings, member
// names and number literals, stopping once the nodes reach nodeLimit or
// the bytes byteLimit. A nil node counts as one, as the null it is.
func measure(n *tree.Node, nodeLimit, byteLimit int) (nodes, text int) {
	if n == nil {
		return 1, 0
	}
	nodes, text = 1, len(n.Text)
	for _, m := range n.Members {
		if nodes >= nodeLimit || text >= byteLimit {
			return nodes, text
		}
		k, b := measure(m.Value, nodeLimit-nodes, byteLimit-text-min(len(m.Key), byteLimit-text))
		nodes, text = nodes+k, text+len(m.Key)+b
	}
	for _, it := range n.Items {
		if nodes >= nodeLimit || text >= byteLimit {
			return nodes, text
		}
		k, b := measure(it, nodeLimit-nodes, byteLimit-text)
		nodes, text = nodes+k, text+b
	}
	return nodes, text
}

// size counts the nodes of n, stopping at limit.
func size(n *tree.Node, limit int) int {
	k, _ := measure(n, limit, math.MaxInt)
	return k
}

// whole decodes n, of nodes nodes (counted up to c.limit+1), into v with
// one encoding/json call.
func (c *chunker) whole(n *tree.Node, v reflect.Value, f *declaredOnly, nodes int) error {
	var err error
	if c.buf, err = appendJSON(c.buf[:0], n, f, c.w); err != nil {
		return err
	}
	c.w.piece(nodes, len(c.buf))
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
	if c.w != nil {
		c.w.unmarshals++
	}
	err = u.UnmarshalJSON(c.buf)
	c.w.check()
	return err
}

// object decodes the members of n into v, a struct or a non-nil map, in
// batches: the object around a batch is one of its nodes, so a batch
// decodes at most c.limit nodes. A member too large for any batch (its
// value with the object around it above c.limit nodes, or its text above
// c.bytes) is decoded on its own.
func (c *chunker) object(n *tree.Node, v reflect.Value, f *declaredOnly) error {
	var sel *schemaidx.Node
	structLike := false
	if f != nil {
		sel = c.w.selectSchema(f.schema, n)
		structLike = isStruct(sel)
	}
	open, pending := false, 0
	// folds counts the members the decode sees by case-folded name, built
	// for a struct at its first large member.
	var folds map[string]int
	flush := func() error {
		if !open {
			return nil
		}
		c.buf = append(c.buf, '}')
		c.w.piece(pending, len(c.buf))
		open, pending = false, 0
		return c.w.decodeInto(c.buf, v.Addr().Interface())
	}
	for _, m := range n.Members {
		c.w.charge(len(m.Key))
		var child *declaredOnly
		if f != nil {
			if _, declared := sel.Property(m.Key); structLike && !declared {
				continue
			}
			child = &declaredOnly{memberSchema(sel, m.Key)}
		}
		k, b := c.size(m.Value)
		b += len(m.Key)
		if open && (pending+k > c.limit || len(c.buf)+b > c.bytes) {
			if err := flush(); err != nil {
				return err
			}
		}
		if 1+k > c.limit || b > c.bytes {
			if v.Kind() == reflect.Struct && folds == nil {
				folds = c.foldCounts(n, sel, structLike)
			}
			c.w.charge(len(m.Key))
			if into, ok := memberTarget(v, m.Key); ok && (folds == nil || folds[foldName(m.Key)] == 1) {
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
			open, pending = true, 1
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

// foldCounts counts the members of n that the decode sees (with a
// declaredOnly filter, only the declared members of a struct-like object
// sel) by foldName; w counts the members and their names' bytes.
func (c *chunker) foldCounts(n *tree.Node, sel *schemaidx.Node, structLike bool) map[string]int {
	out := make(map[string]int, len(n.Members))
	for _, m := range n.Members {
		c.w.charge(1 + len(m.Key))
		if structLike {
			if _, declared := sel.Property(m.Key); !declared {
				continue
			}
		}
		out[foldName(m.Key)]++
	}
	return out
}

// foldName returns a key equal for two names exactly when
// strings.EqualFold reports them equal: each code point (an invalid UTF-8
// byte as U+FFFD, as strings.EqualFold reads it) replaced by the least
// code point of its unicode.SimpleFold orbit.
func foldName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		out = append(out, least)
	}
	return string(out)
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

// list decodes the elements of n into v, a slice, in batches counted as
// object counts them: the array around a batch is one of its nodes, and
// an element too large for any batch is decoded into its place on its
// own.
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
		c.buf = append(c.buf, ']')
		c.w.piece(pending, len(c.buf))
		open, pending = false, 0
		tmp := reflect.New(t)
		if err := c.w.decodeInto(c.buf, tmp.Interface()); err != nil {
			return err
		}
		reflect.Copy(v.Slice(start, end), tmp.Elem())
		return nil
	}
	for i, it := range n.Items {
		k, b := c.size(it)
		if open && (pending+k > c.limit || len(c.buf)+b > c.bytes) {
			if err := flush(i); err != nil {
				return err
			}
		}
		if 1+k > c.limit || b > c.bytes {
			if err := c.decode(it, v.Index(i), child); err != nil {
				return err
			}
			continue
		}
		if open {
			c.buf = append(c.buf, ',')
		} else {
			c.buf = append(c.buf[:0], '[')
			open, start, pending = true, i, 1
		}
		var err error
		if c.buf, err = appendJSON(c.buf, it, child, c.w); err != nil {
			return err
		}
		pending += k
	}
	return flush(len(n.Items))
}
