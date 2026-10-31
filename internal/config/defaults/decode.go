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
// type (such as *v1alpha1.RateLimitConfig), with encoding/json. Every
// value its Go field cannot hold, such as 4294967296 in an int32 weight,
// is RZ-CFG-005 at that field's key-aware path (every finding in one
// run, up to the diagnostic budget), and the config is decoded, and its
// failures reported, even when the kind struct failed. idx is the hub
// schema, which names list entries in paths and tells declared config
// members from free ones; files gives source locations. A nil idx or
// resource decodes nothing, as Normalize does.
//
// The kind struct receives spec.config whole as its json.RawMessage. The
// config type receives only the declared members of each object, so an
// undeclared member of an open config whose name differs from a field's
// only in case (encoding/json matches names case-insensitively) cannot
// overwrite that field: the typed config always agrees with the tree.
// Normalize rejects such a member (RZ-CFG-005), so after stage G any
// encoding/json decode of the raw spec.config into the config type, a
// Filter factory's included, gives the typed config too. Numbers in
// untyped (any) positions decode as json.Number, never float64.
func Decode(idx *schemaidx.Index, reg *registry.Registry, res *tree.Resource, files *tree.FileTable) (object, config any, ds diag.List) {
	object, config, ds, _ = decode(idx, reg, res, files, newWork(), nil)
	return object, config, ds
}

// decode is Decode on worker w. The trees are decoded in pieces of at
// most decodeChunk nodes and about decodeBytes bytes of text (chunker),
// each with one encoding/json call, which cannot yield inside: building
// the JSON text counts toward w's yield timer and w checks it before and
// after each call. A value whose type has its own UnmarshalJSON is the
// one exception: it gets its whole text in one call (see chunker). On a
// failure the locator walks the whole tree and reports every value that
// does not fit (locator).
//
// prior is what normalization reported, nil when it found no error. With
// prior set, the result is discarded anyway, so no decode runs: the
// locator alone walks the trees and reports what does not fit, but for
// the nodes prior holds, whose failures are reported already.
// Diagnostics draw on w's budget; failed reports an error even when the
// budget dropped it.
func decode(idx *schemaidx.Index, reg *registry.Registry, res *tree.Resource, files *tree.FileTable, w *work, prior *reported) (object, config any, ds diag.List, failed bool) {
	if idx == nil || res == nil || res.Root == nil {
		return nil, nil, nil, false
	}
	obj, ok := newObject(res.ID.Kind)
	if !ok {
		at, _ := w.get(res.Root, "kind")
		return nil, nil, w.admit(diag.List{{
			Code: CodeUnserved, Severity: diag.SeverityError, Location: location(files, posOr(at, res.Start)),
			Resource: res.ID.ResourceID(), Path: diag.Path{diag.Field("kind")},
			Message: fmt.Sprintf("kind %q has no hub type", clip(string(res.ID.Kind))),
		}}), true
	}
	d := &decoder{res: res, files: files, w: w, prior: prior, c: &chunker{w: w, limit: w.chunkLimit(), bytes: w.bytesLimit()}}
	kindSchema, _ := idx.Resource(string(res.ID.Kind))
	d.one(res.Root, reflect.ValueOf(obj).Elem(), kindSchema, stack{{node: res.Root}}, false)
	if res.ID.Kind != v1alpha1.KindPolicy || reg == nil {
		return d.result(obj, nil)
	}
	// The config type is the decoded spec.type, as one encoding/json call
	// gives it, or, when the kind struct was not decoded, the tree's
	// spec.type member, so the config is decoded even then (stage F
	// checked spec.type against the registry's names; one it does not
	// know has no config type).
	spec, _ := w.get(res.Root, "spec")
	var typ v1alpha1.PolicyType
	if p, ok := obj.(*v1alpha1.Policy); ok && !d.failed && d.prior == nil {
		typ = p.Spec.Type
	} else if t, ok := w.get(spec, "type"); ok && t != nil && t.Kind == tree.KindString {
		typ = v1alpha1.PolicyType(t.Text)
	} else {
		return d.result(obj, nil)
	}
	cfg, err := reg.NewConfig(typ)
	if err != nil || cfg == nil {
		// An unregistered type is the schema's RZ-CFG-005 at spec.type.
		return d.result(obj, nil)
	}
	cfgNode, ok := w.get(spec, "config")
	if !ok {
		return d.result(obj, cfg)
	}
	specSchema, _ := idx.Spec(string(v1alpha1.KindPolicy))
	cfgSchema := memberSchema(w.selectSchema(specSchema, spec), "config")
	d.one(cfgNode, reflect.ValueOf(cfg).Elem(), cfgSchema, stack{{node: res.Root}, {node: spec, name: "spec"}, {node: cfgNode, name: "config"}}, true)
	return d.result(obj, cfg)
}

// decoder holds the state of one resource's decode.
type decoder struct {
	res   *tree.Resource
	files *tree.FileTable
	w     *work
	prior *reported
	c     *chunker
	ds    diag.List
	// failed reports a value that does not fit, recorded or not.
	failed bool
}

// one decodes n into v, an addressable value of a fresh target whose
// schema is s, with top as the locator's stack at n; declared selects the
// config decode, which sees only declared members (declaredOnly). With
// prior set, or once a value failed, only the locator runs, since the
// result is discarded.
func (d *decoder) one(n *tree.Node, v reflect.Value, s *schemaidx.Node, top stack, declared bool) {
	l := &locator{stack: top, w: d.w, declared: declared, prior: d.prior, report: d.report}
	if d.prior != nil || d.failed {
		l.locateAll(v.Type(), n, s)
		return
	}
	var f *declaredOnly
	if declared {
		f = &declaredOnly{s}
	}
	err := d.c.decode(n, v, f)
	if err == nil {
		return
	}
	d.failed = true
	if l.locateAll(v.Type(), n, s) == 0 {
		// No value is at fault in Ruralz's own terms: report encoding/json's
		// reason at the top, clipped (its errors quote the value).
		d.report(l.stack.path(d.w), n, "cannot decode: "+clip(err.Error()))
	}
}

// report records an RZ-CFG-005 decode failure at path, located at node
// (or the resource start), and returns false once the budget is spent.
func (d *decoder) report(path diag.Path, at *tree.Node, msg string) bool {
	d.failed = true
	if !d.w.take() {
		return false
	}
	d.ds = append(d.ds, diag.Diagnostic{
		Code: CodeSchema, Severity: diag.SeverityError, Location: location(d.files, posOr(at, d.res.Start)),
		Resource: d.res.ID.ResourceID(), Path: path, Message: msg,
	})
	return true
}

// result returns decode's results: the values when nothing failed.
func (d *decoder) result(obj, cfg any) (any, any, diag.List, bool) {
	if d.failed || d.prior != nil {
		return nil, nil, d.ds, d.failed
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
// checks its yield timer before and after it.
func (w *work) decodeInto(data []byte, v any) error {
	w.check()
	if w != nil {
		w.decodes++
		if w.decoded != nil {
			w.decoded(data)
		}
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
// their literal text, members in tree order, and a nil node as null. A
// string or member name that is not UTF-8, a number literal outside the
// RFC 8259 grammar and a node of no JSON kind are errors, so the text is
// always valid JSON (Normalize reports each first, so stage G never meets
// one here). With f set, members are filtered as declaredOnly says. Each
// node, and each byte of a string, member name or number literal, counts
// toward w's yield timer (w may be nil).
func appendJSON(dst []byte, n *tree.Node, f *declaredOnly, w *work) ([]byte, error) {
	w.tick()
	if n == nil {
		return append(dst, "null"...), nil
	}
	w.charge(len(n.Text))
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
			sel = w.selectSchema(f.schema, n)
			structLike = isStruct(sel)
		}
		dst = append(dst, '{')
		first := true
		for _, m := range n.Members {
			w.charge(len(m.Key))
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

// locator finds every value a failed decode could not hold by walking the
// tree along the Go type, so each decode failure is reported at its field
// with a message of Ruralz's own, identical in every build
// (encoding/json's messages differ under GOEXPERIMENT=jsonv2). It walks
// the whole tree once, reporting each value that does not fit as it meets
// it, until the diagnostic budget is spent; each list element on a
// reported path is named once, however many failures lie below it
// (stack.path).
type locator struct {
	stack stack
	// w counts the nodes walked and the bytes of the number literals
	// parsed toward its yield timer; nil in tests.
	w *work
	// declared reports a config decode, which sees only the declared
	// members of each object with declared properties (declaredOnly): the
	// locator then skips the others too, so an undeclared member that
	// matches a field case-insensitively, which the decode never saw, is
	// never blamed for its failure.
	declared bool
	// prior holds the nodes normalization reported, not reported again;
	// with it set, a value whose JSON text appendJSON refuses is skipped
	// too, since normalization reported what makes it so.
	prior *reported
	// report records one failure and returns false once the budget is
	// spent, which ends the walk.
	report func(path diag.Path, at *tree.Node, msg string) bool
	// found counts the failures met, recorded or not.
	found int
}

// filter returns the member filter of the decode below schema s:
// declaredOnly for a config decode, else nil.
func (l *locator) filter(s *schemaidx.Node) *declaredOnly {
	if !l.declared {
		return nil
	}
	return &declaredOnly{s}
}

// skip reports a member of an object (schema sel, dispatch applied) that
// the decode did not see: an undeclared member of a struct-like object in
// a config decode.
func (l *locator) skip(sel *schemaidx.Node, name string) bool {
	if !l.declared || !isStruct(sel) {
		return false
	}
	_, declared := sel.Property(name)
	return !declared
}

// locateAll walks n (of Go type t, schema s) and reports every value that
// does not fit; it returns the number met. Error texts are clipped: the
// errors of a type's UnmarshalJSON quote the value, which may be a
// megabyte long.
func (l *locator) locateAll(t reflect.Type, n *tree.Node, s *schemaidx.Node) int {
	l.walk(t, n, s)
	return l.found
}

// fail reports n, the value at the top of l.stack, as one that does not
// fit, unless normalization reported it; it returns true when the budget
// is spent and the walk must stop.
func (l *locator) fail(n *tree.Node, msg string) bool {
	if l.prior.has(n) {
		return false
	}
	l.found++
	return !l.report(l.stack.path(l.w), n, "cannot decode: "+msg)
}

// walk checks n along t, with l.stack at its position, and below it; stop
// reports a spent budget.
func (l *locator) walk(t reflect.Type, n *tree.Node, s *schemaidx.Node) (stop bool) {
	if n == nil || l.prior.rejected(n) {
		// A nil node is null, which encoding/json leaves as it is; the
		// value of a member normalization rejected by name is wrong
		// whatever it holds.
		return false
	}
	l.w.tick()
	if reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		raw, err := appendJSON(nil, n, l.filter(s), l.w)
		if err != nil {
			if l.prior != nil {
				return false
			}
			return l.fail(n, clip(err.Error()))
		}
		u, _ := reflect.New(t).Interface().(json.Unmarshaler)
		if err := u.UnmarshalJSON(raw); err != nil {
			return l.fail(n, clip(err.Error()))
		}
		return false
	}
	if n.Kind == tree.KindNull {
		// encoding/json leaves the value as it is.
		return false
	}
	switch t.Kind() {
	case reflect.Pointer:
		return l.walk(t.Elem(), n, s)
	case reflect.Interface:
		return false
	case reflect.Bool:
		return l.want(n, n.Kind == tree.KindBool, "a boolean")
	case reflect.String:
		return l.want(n, n.Kind == tree.KindString, "a string")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n.Kind != tree.KindInt && n.Kind != tree.KindFloat {
			return l.want(n, false, "an integer")
		}
		l.w.charge(len(n.Text))
		if _, err := strconv.ParseInt(n.Text, 10, t.Bits()); err != nil {
			return l.fail(n, intRange(n.Text, t.Bits(), false))
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n.Kind != tree.KindInt && n.Kind != tree.KindFloat {
			return l.want(n, false, "an integer")
		}
		l.w.charge(len(n.Text))
		if _, err := strconv.ParseUint(n.Text, 10, t.Bits()); err != nil {
			return l.fail(n, intRange(n.Text, t.Bits(), true))
		}
	case reflect.Float32, reflect.Float64:
		if n.Kind != tree.KindInt && n.Kind != tree.KindFloat {
			return l.want(n, false, "a number")
		}
		l.w.charge(len(n.Text))
		if _, err := strconv.ParseFloat(n.Text, t.Bits()); err != nil {
			return l.fail(n, fmt.Sprintf("number %s is outside the range of a %d-bit float", clip(n.Text), t.Bits()))
		}
	case reflect.Slice, reflect.Array:
		if n.Kind != tree.KindList {
			return l.want(n, false, "a list")
		}
		it := itemSchema(s)
		for i, item := range n.Items {
			l.stack = append(l.stack, step{node: item, item: true, list: s, index: i})
			stop := l.walk(t.Elem(), item, it)
			l.stack = l.stack[:len(l.stack)-1]
			if stop {
				return true
			}
		}
	case reflect.Map:
		if n.Kind != tree.KindMap {
			return l.want(n, false, "an object")
		}
		sel := l.w.selectSchema(s, n)
		for _, m := range n.Members {
			if l.skip(sel, m.Key) {
				continue
			}
			if l.member(t.Elem(), m, sel) {
				return true
			}
		}
	case reflect.Struct:
		if n.Kind != tree.KindMap {
			return l.want(n, false, "an object")
		}
		fields := jsonFields(t)
		sel := l.w.selectSchema(s, n)
		for _, m := range n.Members {
			if l.skip(sel, m.Key) {
				continue
			}
			ft, ok := fields.lookup(m.Key)
			if !ok {
				continue
			}
			if l.member(ft, m, sel) {
				return true
			}
		}
	default:
		return l.fail(n, "no decoder for Go type "+t.String())
	}
	return false
}

// member walks one object member of Go type t; stop reports a spent
// budget.
func (l *locator) member(t reflect.Type, m tree.Member, sel *schemaidx.Node) (stop bool) {
	l.stack = append(l.stack, step{node: m.Value, name: m.Key})
	stop = l.walk(t, m.Value, memberSchema(sel, m.Key))
	l.stack = l.stack[:len(l.stack)-1]
	return stop
}

// want reports a JSON type mismatch, which stage F normally prevents; stop
// reports a spent budget.
func (l *locator) want(n *tree.Node, ok bool, what string) (stop bool) {
	if ok {
		return false
	}
	return l.fail(n, "want "+what)
}

// intRange describes an integer outside a Go integer type of bits bits. A
// literal with a fraction or an exponent is "not an integer", unless it is
// an integer too large for the canonical form, which only canonical
// content brings this far, such as 1e9223372036854775807.
func intRange(text string, bits int, unsigned bool) string {
	if strings.ContainsAny(text, ".eE") {
		if _, _, tooLarge := integerText(text); !tooLarge {
			return fmt.Sprintf("%s is not an integer", clip(text))
		}
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
