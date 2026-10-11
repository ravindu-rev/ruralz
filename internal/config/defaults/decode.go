// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Decode decodes res.Root, a normalized hub resource, into its typed
// pkg/config/v1alpha1 kind struct (such as *v1alpha1.Route) and, for a
// Policy of a registered type, spec.config into the registry's config
// type (such as *v1alpha1.RateLimitConfig), with encoding/json (01 req
// 38). A value its Go field cannot hold, such as 4294967296 in an int32
// weight, is RZ-CFG-005 at that field's key-aware path. idx is the hub
// schema, which names list entries in paths and tells declared config
// members from free ones; files gives source locations.
//
// The kind struct receives spec.config whole as its json.RawMessage. The
// config type receives only the declared members of each object, so an
// undeclared member of an open config whose name differs from a field's
// only in case (encoding/json matches names case-insensitively) cannot
// overwrite that field: the typed config always agrees with the tree.
// Numbers in untyped (any) positions decode as json.Number, never
// float64 (02 req 16).
func Decode(idx *schemaidx.Index, reg *registry.Registry, res *tree.Resource, files *tree.FileTable) (object, config any, ds diag.List) {
	object, config, ds, _ = decode(idx, reg, res, files, newWork(nil))
	return object, config, ds
}

// decode is Decode on worker w. The trees are decoded in pieces of at
// most decodeChunk nodes (chunker), each with one encoding/json call,
// which cannot yield inside: building the JSON text counts toward w's
// yield timer and w checks it before and after each call (01 req 53).
// Diagnostics draw on w's budget; failed reports an error even when the
// budget dropped it.
func decode(idx *schemaidx.Index, reg *registry.Registry, res *tree.Resource, files *tree.FileTable, w *work) (object, config any, ds diag.List, failed bool) {
	if res == nil || res.Root == nil {
		return nil, nil, nil, false
	}
	fail := func(path diag.Path, at *tree.Node, msg string) (any, any, diag.List, bool) {
		pos := res.Start
		if at != nil && at.Pos.Known() {
			pos = at.Pos
		}
		return nil, nil, w.admit(diag.List{{
			Code: CodeSchema, Severity: diag.SeverityError, Location: location(files, pos),
			Resource: res.ID.ResourceID(), Path: path, Message: msg,
		}}), true
	}
	obj, ok := newObject(res.ID.Kind)
	if !ok {
		at, _ := res.Root.Get("kind")
		return nil, nil, w.admit(diag.List{{
			Code: CodeUnserved, Severity: diag.SeverityError, Location: location(files, posOr(at, res.Start)),
			Resource: res.ID.ResourceID(), Path: diag.Path{diag.Field("kind")},
			Message: fmt.Sprintf("kind %q has no hub type", res.ID.Kind),
		}}), true
	}
	kindSchema, _ := idx.Resource(string(res.ID.Kind))
	c := &chunker{w: w, limit: w.chunkLimit()}
	if err := c.decode(res.Root, reflect.ValueOf(obj).Elem(), nil); err != nil {
		l := &locator{stack: stack{{node: res.Root}}, w: w}
		return fail(l.locate(reflect.TypeOf(obj).Elem(), res.Root, kindSchema, err))
	}
	if res.ID.Kind != v1alpha1.KindPolicy || reg == nil {
		return obj, nil, nil, false
	}
	p, _ := obj.(*v1alpha1.Policy)
	cfg, err := reg.NewConfig(p.Spec.Type)
	if err != nil || cfg == nil {
		// An unregistered type is the schema's RZ-CFG-005 at spec.type.
		return obj, nil, nil, false
	}
	spec, _ := res.Root.Get("spec")
	cfgNode, ok := spec.Get("config")
	if !ok {
		return obj, cfg, nil, false
	}
	specSchema, _ := idx.Spec(string(v1alpha1.KindPolicy))
	cfgSchema := memberSchema(specSchema.Select(spec), "config")
	if err := c.decode(cfgNode, reflect.ValueOf(cfg).Elem(), &declaredOnly{cfgSchema}); err != nil {
		l := &locator{stack: stack{{node: res.Root}, {node: spec, name: "spec"}, {node: cfgNode, name: "config"}}, w: w}
		return fail(l.locate(reflect.TypeOf(cfg).Elem(), cfgNode, cfgSchema, err))
	}
	return obj, cfg, nil, false
}

// posOr returns n's position when known, else fallback.
func posOr(n *tree.Node, fallback tree.Pos) tree.Pos {
	if n != nil && n.Pos.Known() {
		return n.Pos
	}
	return fallback
}

// decodeInto decodes one JSON text into v, numbers in untyped positions as
// json.Number. encoding/json cannot yield inside the call, so the worker
// checks its yield timer before and after it (01 req 53).
func (w *work) decodeInto(data []byte, v any) error {
	w.check()
	if w != nil {
		w.decodes++
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	err := dec.Decode(v)
	w.check()
	return err
}

// declaredOnly selects, below a config schema, the members encoding/json
// sees: every member of a typed map or of free content, but only the
// declared members of an object with declared properties (a struct).
type declaredOnly struct{ schema *schemaidx.Node }

// appendJSON appends n as JSON text: strings escaped minimally, numbers as
// their literal text, members in tree order. A string or member name that
// is not UTF-8 and a number literal outside the RFC 8259 grammar are
// errors, so the text is always valid JSON. With f set, members are
// filtered as declaredOnly says. Each node counts toward w's yield timer
// (w may be nil).
func appendJSON(dst []byte, n *tree.Node, f *declaredOnly, w *work) ([]byte, error) {
	w.tick()
	var err error
	switch n.Kind {
	case tree.KindNull:
		dst = append(dst, "null"...)
	case tree.KindBool:
		dst = strconv.AppendBool(dst, n.Bool)
	case tree.KindInt, tree.KindFloat:
		// Only valid JSON leaves here, so a value whose own
		// UnmarshalJSON the chunker calls gets what encoding/json would
		// have checked and handed it.
		if !jsonval.ValidNumber(n.Text) {
			return dst, fmt.Errorf("defaults: invalid number %s", strconv.Quote(clip(n.Text)))
		}
		dst = append(dst, n.Text...)
	case tree.KindString:
		dst, err = jsonval.AppendString(dst, n.Text)
	case tree.KindMap:
		var sel *schemaidx.Node
		structLike := false
		if f != nil {
			sel = f.schema.Select(n)
			structLike = isStruct(sel)
		}
		dst = append(dst, '{')
		first := true
		for _, m := range n.Members {
			var child *declaredOnly
			if f != nil {
				c := memberSchema(sel, m.Key)
				if _, declared := sel.Property(m.Key); structLike && !declared {
					continue
				}
				child = &declaredOnly{c}
			}
			if !first {
				dst = append(dst, ',')
			}
			first = false
			if dst, err = jsonval.AppendString(dst, m.Key); err != nil {
				return dst, err
			}
			dst = append(dst, ':')
			if dst, err = appendJSON(dst, m.Value, child, w); err != nil {
				return dst, err
			}
		}
		dst = append(dst, '}')
	case tree.KindList:
		var child *declaredOnly
		if f != nil {
			child = &declaredOnly{itemSchema(f.schema)}
		}
		dst = append(dst, '[')
		for i, it := range n.Items {
			if i > 0 {
				dst = append(dst, ',')
			}
			if dst, err = appendJSON(dst, it, child, w); err != nil {
				return dst, err
			}
		}
		dst = append(dst, ']')
	default:
		return dst, fmt.Errorf("defaults: node kind %d has no JSON form", n.Kind)
	}
	return dst, err
}

// isStruct reports a schema object that decodes into a Go struct: it
// declares properties and is not a typed map.
func isStruct(sel *schemaidx.Node) bool {
	if sel == nil {
		return false
	}
	if _, ok := sel.Values(); ok {
		return false
	}
	for range sel.Properties() {
		return true
	}
	return false
}

// locator finds the value a failed decode could not hold by walking the
// tree along the Go type, so a decode failure is reported at its field
// (01 req 38) with a message of Ruralz's own, identical in every build
// (encoding/json's messages differ under GOEXPERIMENT=jsonv2).
type locator struct {
	stack stack
	// w counts the nodes walked toward its yield timer; nil in tests.
	w *work
}

// locate returns the path, node and message of the first value of n (of
// Go type t, schema s) that does not fit, or the path of the stack top
// with err's text when no value is at fault.
func (l *locator) locate(t reflect.Type, n *tree.Node, s *schemaidx.Node, err error) (diag.Path, *tree.Node, string) {
	if at, msg, ok := l.find(t, n, s); ok {
		return l.stack.path(), at, "cannot decode: " + msg
	}
	return l.stack.path(), n, "cannot decode: " + err.Error()
}

// find walks n along t; on a value that does not fit it returns the node
// and a message with l.stack at its position.
func (l *locator) find(t reflect.Type, n *tree.Node, s *schemaidx.Node) (*tree.Node, string, bool) {
	if n == nil {
		return nil, "", false
	}
	l.w.tick()
	if reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		raw, err := appendJSON(nil, n, nil, l.w)
		if err == nil {
			u, _ := reflect.New(t).Interface().(json.Unmarshaler)
			err = u.UnmarshalJSON(raw)
		}
		if err != nil {
			return n, err.Error(), true
		}
		return nil, "", false
	}
	if n.Kind == tree.KindNull {
		// encoding/json leaves the value as it is.
		return nil, "", false
	}
	switch t.Kind() {
	case reflect.Pointer:
		return l.find(t.Elem(), n, s)
	case reflect.Interface:
		return nil, "", false
	case reflect.Bool:
		return want(n, n.Kind == tree.KindBool, "a boolean")
	case reflect.String:
		return want(n, n.Kind == tree.KindString, "a string")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n.Kind != tree.KindInt && n.Kind != tree.KindFloat {
			return want(n, false, "an integer")
		}
		if _, err := strconv.ParseInt(n.Text, 10, t.Bits()); err != nil {
			return n, intRange(n.Text, t.Bits(), false), true
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n.Kind != tree.KindInt && n.Kind != tree.KindFloat {
			return want(n, false, "an integer")
		}
		if _, err := strconv.ParseUint(n.Text, 10, t.Bits()); err != nil {
			return n, intRange(n.Text, t.Bits(), true), true
		}
	case reflect.Float32, reflect.Float64:
		if n.Kind != tree.KindInt && n.Kind != tree.KindFloat {
			return want(n, false, "a number")
		}
		if _, err := strconv.ParseFloat(n.Text, t.Bits()); err != nil {
			return n, fmt.Sprintf("number %s is outside the range of a %d-bit float", clip(n.Text), t.Bits()), true
		}
	case reflect.Slice, reflect.Array:
		if n.Kind != tree.KindList {
			return want(n, false, "a list")
		}
		it := itemSchema(s)
		for i, item := range n.Items {
			l.stack = append(l.stack, step{node: item, item: true, list: s, index: i})
			if at, msg, ok := l.find(t.Elem(), item, it); ok {
				return at, msg, true
			}
			l.stack = l.stack[:len(l.stack)-1]
		}
	case reflect.Map:
		if n.Kind != tree.KindMap {
			return want(n, false, "an object")
		}
		sel := s.Select(n)
		for _, m := range n.Members {
			if at, msg, ok := l.member(t.Elem(), m, sel); ok {
				return at, msg, true
			}
		}
	case reflect.Struct:
		if n.Kind != tree.KindMap {
			return want(n, false, "an object")
		}
		fields := jsonFields(t)
		sel := s.Select(n)
		for _, m := range n.Members {
			ft, ok := fields.lookup(m.Key)
			if !ok {
				continue
			}
			if at, msg, ok := l.member(ft, m, sel); ok {
				return at, msg, true
			}
		}
	default:
		return n, "no decoder for Go type " + t.String(), true
	}
	return nil, "", false
}

// member walks one object member of Go type t.
func (l *locator) member(t reflect.Type, m tree.Member, sel *schemaidx.Node) (*tree.Node, string, bool) {
	l.stack = append(l.stack, step{node: m.Value, name: m.Key})
	if at, msg, ok := l.find(t, m.Value, memberSchema(sel, m.Key)); ok {
		return at, msg, true
	}
	l.stack = l.stack[:len(l.stack)-1]
	return nil, "", false
}

// want reports a JSON type mismatch, which stage F normally prevents.
func want(n *tree.Node, ok bool, what string) (*tree.Node, string, bool) {
	if ok {
		return nil, "", false
	}
	return n, "want " + what, true
}

// intRange describes an integer outside a Go integer type of bits bits.
func intRange(text string, bits int, unsigned bool) string {
	if strings.ContainsAny(text, ".eE") {
		return fmt.Sprintf("%s is not an integer", clip(text))
	}
	if unsigned {
		return fmt.Sprintf("integer %s is outside the range 0 to %d of this field", clip(text), uint64(1)<<bits-1)
	}
	return fmt.Sprintf("integer %s is outside the range %d to %d of this field", clip(text), -(int64(1) << (bits - 1)), int64(1)<<(bits-1)-1)
}

// fieldTable maps JSON member names to the Go types of a struct's fields.
type fieldTable struct {
	names []string
	types []reflect.Type
}

// jsonFields lists the fields encoding/json decodes into for struct type t,
// promoted fields of embedded structs included.
func jsonFields(t reflect.Type) fieldTable {
	var ft fieldTable
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() {
			continue
		}
		tag, hasTag := f.Tag.Lookup("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" && !strings.HasPrefix(tag, "-,") {
			continue
		}
		if f.Anonymous && !hasTag && f.Type.Kind() == reflect.Struct {
			// Its fields are promoted; VisibleFields lists them.
			continue
		}
		if name == "" {
			name = f.Name
		}
		ft.names = append(ft.names, name)
		ft.types = append(ft.types, f.Type)
	}
	return ft
}

// lookup finds the field of member name as encoding/json does: an exact
// match first, else a case-insensitive one.
func (ft fieldTable) lookup(name string) (reflect.Type, bool) {
	for i, n := range ft.names {
		if n == name {
			return ft.types[i], true
		}
	}
	for i, n := range ft.names {
		if strings.EqualFold(n, name) {
			return ft.types[i], true
		}
	}
	return nil, false
}
