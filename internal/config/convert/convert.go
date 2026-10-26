// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package convert is the hub-and-spoke conversion registry of stage G
// (docs/architecture/02-configuration-model.md
// "Hub-and-spoke conversion"). Every resource is decoded under its own
// apiVersion's rendered schema and converted to the hub representation
// before it is normalized and canonicalized, so a Bundle may mix
// apiVersions and converting a Bundle never changes its Revision.
//
// In M1 the hub field set equals ruralz/v1alpha1 exactly and the registry
// holds one identity converter, but every caller still converts through a
// Registry, so ruralz/v1beta1 (Planned (M3)) plugs in without touching
// the callers. Converters work on generic value trees (tree.Node), not on
// typed structs, and accept partial documents: an overlay patch with
// $patch entries and unsubstituted ${VAR} strings converts like a whole
// resource, which ruralz bundle render --api-version needs to rewrite
// source files unmerged and unsubstituted.
//
// Fields that only a newer apiVersion has survive a down-conversion in
// the annotation ruralz.io/conversion-data and are restored by the
// up-conversion. The annotation never reaches the hub or the canonical
// form: Registry.ToHub strips it after the spoke converter ran. In M1 no
// converter writes it.
//
// The registry also holds the deprecation lifecycle table (empty in
// M1), from which Deprecations reports RZ-CFG-025 warnings on the tree
// as authored; stage G calls it before it writes the defaults.
//
// A Registry is immutable after construction and safe for concurrent use.
// The package never logs, reads files, the network or the clock.
package convert

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/bits"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ConversionDataAnnotation is the metadata.annotations key that carries
// the fields a down-conversion could not express, for the next
// up-conversion to restore. It is the only ruralz.io/ annotation a Bundle
// may author and never appears in the hub or the canonical form.
const ConversionDataAnnotation = "ruralz.io/conversion-data"

// HubAPIVersion is the apiVersion whose field set the hub has in M1: the
// hub types are the ruralz/v1alpha1 types.
const HubAPIVersion = v1alpha1.APIVersion

// RZ codes this package puts on diagnostics (all registered in
// internal/errcode).
const (
	// CodeUnserved is RZ-CFG-007: an apiVersion no converter
	// serves.
	CodeUnserved = "RZ-CFG-007"
	// CodeDeprecated is RZ-CFG-025, a warning for a deprecated apiVersion
	// or field.
	CodeDeprecated = "RZ-CFG-025"
)

// Converter converts the resources of one spoke apiVersion to and from
// the hub representation.
//
// doc is a resource envelope: a mapping with apiVersion, kind, metadata
// and spec. With partial set it may be any part of one, as an overlay
// document is: required members may be missing, keyed-list entries may
// carry $patch, and strings may hold unsubstituted ${VAR} text, which a
// converter must neither type nor reject. A converter may rewrite doc in
// place and returns the converted tree; it keeps the positions and the
// member order of every field it does not change, writes a field
// explicitly whenever the source version's default differs from the
// target's, and moves fields the target cannot express into
// ConversionDataAnnotation and back. A ToHub that reads the annotation
// consumes it: it deletes the member, and the annotations (and metadata)
// object when its FromHub created it, so a resource that had an empty
// annotations object keeps it. The Registry, not the converter, sets the
// apiVersion member, and strips any annotation left over from the hub
// form (StripConversionData). A converter's result holds each member name
// once per object, with no null and no nil node; stage G reports what
// does not as RZ-CFG-005, so a converter bug fails the resource instead
// of reaching the canonical form.
type Converter interface {
	// APIVersion returns the spoke apiVersion, such as ruralz/v1alpha1.
	APIVersion() string
	// ToHub converts doc, a resource of kind in APIVersion, to the hub.
	ToHub(kind string, doc *tree.Node, partial bool) (*tree.Node, diag.List)
	// FromHub converts doc, a hub resource of kind, to APIVersion.
	FromHub(kind string, doc *tree.Node, partial bool) (*tree.Node, diag.List)
}

// identity is the converter of an apiVersion whose field set equals the
// hub's.
type identity struct{ version string }

func (c identity) APIVersion() string { return c.version }

func (identity) ToHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) { return doc, nil }

func (identity) FromHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) { return doc, nil }

// V1alpha1 returns the ruralz/v1alpha1 converter: the identity, since the
// M1 hub field set equals ruralz/v1alpha1.
func V1alpha1() Converter { return identity{version: v1alpha1.APIVersion} }

// Notice announces the removal of a deprecated apiVersion or field.
type Notice struct {
	// Removal is the release that removes it, such as "0.9.0".
	Removal string
	// Replacement names what to use instead, such as an apiVersion or a
	// field; "" when there is none.
	Replacement string
}

// Lifecycle is the explicit deprecation table: deprecated apiVersions,
// and deprecated fields per apiVersion keyed by their schema path
// (schemaidx.Index.SchemaPath, such as "spec.upstreams[].weight"). It is
// empty in M1.
type Lifecycle struct {
	// APIVersions maps a deprecated apiVersion to its notice.
	APIVersions map[string]Notice
	// Fields maps an apiVersion to its deprecated fields.
	Fields map[string]map[string]Notice
}

// clone returns a deep copy, so a Registry never shares the caller's maps.
func (lc Lifecycle) clone() Lifecycle {
	out := Lifecycle{APIVersions: maps.Clone(lc.APIVersions)}
	if lc.Fields != nil {
		out.Fields = make(map[string]map[string]Notice, len(lc.Fields))
		for v, fs := range lc.Fields {
			out.Fields[v] = maps.Clone(fs)
		}
	}
	return out
}

// Registry is the converter table. It is immutable and safe for
// concurrent use. Build one with NewRegistry or Default: the zero
// Registry serves no apiVersion, the hub's included, so every conversion
// through it is RZ-CFG-007 (defaults.New refuses it).
type Registry struct {
	byVersion map[string]Converter
	served    []string
	lifecycle Lifecycle
}

// ErrRegistry marks every error NewRegistry returns.
var ErrRegistry = errors.New("convert: invalid converter registry")

// NewRegistry returns a registry of the converters cs with the deprecation
// table lc. Each converter needs a distinct, non-empty apiVersion, and one
// of them must serve HubAPIVersion, the hub's own field set.
func NewRegistry(lc Lifecycle, cs ...Converter) (*Registry, error) {
	r := &Registry{byVersion: make(map[string]Converter, len(cs)), lifecycle: lc.clone()}
	for _, c := range cs {
		if c == nil {
			return nil, fmt.Errorf("%w: nil converter", ErrRegistry)
		}
		v := c.APIVersion()
		if v == "" {
			return nil, fmt.Errorf("%w: converter with an empty apiVersion", ErrRegistry)
		}
		if _, dup := r.byVersion[v]; dup {
			return nil, fmt.Errorf("%w: two converters for apiVersion %q", ErrRegistry, v)
		}
		r.byVersion[v] = c
		r.served = append(r.served, v)
	}
	if _, ok := r.byVersion[HubAPIVersion]; !ok {
		return nil, fmt.Errorf("%w: no converter serves the hub apiVersion %q", ErrRegistry, HubAPIVersion)
	}
	slices.Sort(r.served)
	return r, nil
}

// Default returns the M1 registry: the ruralz/v1alpha1 identity converter
// and an empty lifecycle table. It builds a new Registry on every call;
// there is no package-level registry.
func Default() *Registry {
	return &Registry{
		byVersion: map[string]Converter{v1alpha1.APIVersion: V1alpha1()},
		served:    []string{v1alpha1.APIVersion},
	}
}

// Served returns the served apiVersions, sorted.
func (r *Registry) Served() []string { return slices.Clone(r.served) }

// Serves reports whether a converter serves apiVersion.
func (r *Registry) Serves(apiVersion string) bool {
	_, ok := r.byVersion[apiVersion]
	return ok
}

// Converter returns the converter of apiVersion.
func (r *Registry) Converter(apiVersion string) (Converter, bool) {
	c, ok := r.byVersion[apiVersion]
	return c, ok
}

// CheckServed reports an apiVersion no converter serves as RZ-CFG-007 at
// apiVersion, with the served apiVersions as the hint. The caller adds
// the resource and the source Location.
func (r *Registry) CheckServed(apiVersion string) (diag.Diagnostic, bool) {
	if r.Serves(apiVersion) {
		return diag.Diagnostic{}, false
	}
	hint := "served apiVersions: " + strings.Join(r.served, ", ")
	if near, ok := diag.Nearest(apiVersion, r.served); ok {
		hint = "did you mean " + near + "? " + hint
	}
	return diag.Diagnostic{
		Code: CodeUnserved, Severity: diag.SeverityError,
		Path:    diag.Path{diag.Field("apiVersion")},
		Message: fmt.Sprintf("apiVersion %q is not served by this release", clip(apiVersion)),
		Hint:    hint,
	}, true
}

// ToHub converts doc, a resource of kind authored in apiVersion, to the
// hub representation: the spoke converter runs, then the apiVersion member
// (when present) becomes HubAPIVersion and the ConversionDataAnnotation is
// stripped. doc may be rewritten in place; callers use the returned tree.
// An unserved apiVersion is RZ-CFG-007 and returns doc unchanged. A
// converter that returns no document, or a non-object for a whole resource
// object, is RZ-CFG-007 too and returns doc as the converter left it, so
// callers can rely on a result without errors being a document, and an
// object for an object.
func (r *Registry) ToHub(apiVersion, kind string, doc *tree.Node, partial bool) (*tree.Node, diag.List) {
	c, ok := r.byVersion[apiVersion]
	if !ok {
		d, _ := r.CheckServed(apiVersion)
		return doc, diag.List{d}
	}
	out, ds := c.ToHub(kind, doc, partial)
	if ds.HasErrors() {
		return out, ds
	}
	if d, bad := checkResult(apiVersion, "to the hub", doc, out, partial); bad {
		return doc, append(ds, d)
	}
	setAPIVersion(out, HubAPIVersion)
	StripConversionData(out)
	return out, ds
}

// FromHub converts doc, a hub resource of kind, to apiVersion and sets its
// apiVersion member (when present). An unserved apiVersion is RZ-CFG-007
// and returns doc unchanged; a converter result ToHub would refuse is
// RZ-CFG-007 too and returns doc as the converter left it.
func (r *Registry) FromHub(apiVersion, kind string, doc *tree.Node, partial bool) (*tree.Node, diag.List) {
	c, ok := r.byVersion[apiVersion]
	if !ok {
		d, _ := r.CheckServed(apiVersion)
		return doc, diag.List{d}
	}
	out, ds := c.FromHub(kind, doc, partial)
	if ds.HasErrors() {
		return out, ds
	}
	if d, bad := checkResult(apiVersion, "from the hub", doc, out, partial); bad {
		return doc, append(ds, d)
	}
	setAPIVersion(out, apiVersion)
	return out, ds
}

// checkResult reports a converter result the callers cannot use as
// RZ-CFG-007 at the resource (its location is the caller's): no document
// for a document, or, for a whole resource (not partial), something other
// than an object for an object. A converter's own errors come first; a
// nil or non-object document the caller passed is the caller's to report.
func checkResult(apiVersion, direction string, doc, out *tree.Node, partial bool) (diag.Diagnostic, bool) {
	var got string
	switch {
	case doc == nil:
		return diag.Diagnostic{}, false
	case out == nil:
		got = "no document"
	case !partial && doc.Kind == tree.KindMap && out.Kind != tree.KindMap:
		got = "a " + kindName(out.Kind) + " instead of an object"
	default:
		return diag.Diagnostic{}, false
	}
	return diag.Diagnostic{
		Code: CodeUnserved, Severity: diag.SeverityError,
		Message: fmt.Sprintf("the converter of apiVersion %q returned %s converting %s", clip(apiVersion), got, direction),
	}, true
}

// kindName names a node kind in a message.
func kindName(k tree.Kind) string {
	switch k {
	case tree.KindNull:
		return "null"
	case tree.KindBool:
		return "boolean"
	case tree.KindInt, tree.KindFloat:
		return "number"
	case tree.KindString:
		return "string"
	case tree.KindList:
		return "list"
	case tree.KindMap:
		return "object"
	default:
		return "value"
	}
}

// Convert converts doc, a resource (or with partial, a partial document)
// of kind, from apiVersion from to apiVersion to through the hub.
// Converting to the same apiVersion still runs both halves, so render
// --api-version exercises the whole path in M1. An unserved from or to is
// RZ-CFG-007 at apiVersion.
func (r *Registry) Convert(from, to, kind string, doc *tree.Node, partial bool) (*tree.Node, diag.List) {
	if d, bad := r.CheckServed(to); bad {
		d.Message = fmt.Sprintf("target apiVersion %q is not served by this release", clip(to))
		return doc, diag.List{d}
	}
	hub, ds := r.ToHub(from, kind, doc, partial)
	if ds.HasErrors() {
		return hub, ds
	}
	out, more := r.FromHub(to, kind, hub, partial)
	return out, append(ds, more...)
}

// setAPIVersion rewrites the string apiVersion member of doc, keeping its
// position; a doc without one (a partial document), or whose apiVersion
// is not a string (a nil member value among them, which is null), is left
// alone.
func setAPIVersion(doc *tree.Node, v string) {
	if n, ok := doc.Get("apiVersion"); ok && n != nil && n.Kind == tree.KindString {
		n.Text = v
	}
}

// StripConversionData removes ConversionDataAnnotation from doc's
// metadata.annotations and reports whether it was there: the strip step,
// which Registry.ToHub runs after the spoke converter. When the
// annotation was the only member, annotations is removed too, so an
// authored conversion-data annotation that no converter consumed leaves
// no empty annotations object in the hub and canonical form. A nil
// metadata or annotations value is a null, which holds no annotation.
func StripConversionData(doc *tree.Node) bool {
	meta, ok := doc.Get("metadata")
	if !ok {
		return false
	}
	ann, ok := meta.Get("annotations")
	if !ok || ann == nil || ann.Kind != tree.KindMap || !ann.Delete(ConversionDataAnnotation) {
		return false
	}
	if len(ann.Members) == 0 {
		meta.Delete("annotations")
	}
	return true
}

// Deprecations returns the RZ-CFG-025 warnings of res, a resource authored
// in its apiVersion, from the lifecycle table: one at apiVersion when its
// apiVersion is deprecated, and one per present field whose schema path is
// deprecated. idx is the schema of res's apiVersion; files gives source
// locations (nil: line and column only). Nothing is reported for an empty
// table, which is the M1 case.
//
// It reads the tree as authored: stage G calls it before it writes the
// defaults, and a node styled tree.StyleDefaulted, with everything below
// it, is skipped, so a later caller never warns about a deprecated field
// that only a default wrote.
func (r *Registry) Deprecations(idx *schemaidx.Index, res *tree.Resource, files *tree.FileTable) diag.List {
	return r.DeprecationsCounted(idx, res, files, nil)
}

// DeprecationsCounted is Deprecations reporting the work of its walk to
// count (nil: not reported), once per position visited with the members
// or elements the walk scans there, the bytes of the canonical text that
// names a set element of objects or lists (schemaidx.ItemElem encodes
// it), and the bytes of each segment the table lookups compare, so a
// caller with a yield timer (stage G's worker) counts the walk as its own
// work.
func (r *Registry) DeprecationsCounted(idx *schemaidx.Index, res *tree.Resource, files *tree.FileTable, count func(units int)) diag.List {
	if res == nil || res.Root == nil {
		return nil
	}
	var out diag.List
	add := func(path diag.Path, at tree.Pos, msg string, n Notice) {
		d := diag.Diagnostic{
			Code: CodeDeprecated, Severity: diag.SeverityWarning,
			Location: location(files, at, res.Start),
			Resource: res.ID.ResourceID(), Path: path, Message: msg,
		}
		if n.Replacement != "" {
			d.Hint = "use " + n.Replacement
		}
		out = append(out, d)
	}
	if n, ok := r.lifecycle.APIVersions[res.APIVersion]; ok {
		at := res.Start
		if v, ok := res.Root.Get("apiVersion"); ok && v != nil {
			at = v.Pos
		}
		add(diag.Path{diag.Field("apiVersion")}, at,
			fmt.Sprintf("apiVersion %s is deprecated and is removed in %s", res.APIVersion, n.Removal), n)
	}
	fields := r.lifecycle.Fields[res.APIVersion]
	if len(fields) == 0 || idx == nil {
		return out
	}
	// Each position carries the range of the sorted deprecated schema
	// paths that its own schema path starts, in the form
	// schemaidx.Index.SchemaPath gives, narrowed from its parent's by
	// the position's segment alone (".<name>", "{}" or "[]"): two binary
	// searches of the table per segment, whatever the depth or the length
	// of the names above it, and no walk below a position no deprecated
	// path starts. The schema path text is the table's own key, so none
	// is built.
	keys := slices.Sorted(maps.Keys(fields))
	var levels []schemaLevel
	_ = idx.Walk(string(res.ID.Kind), nullsFilled(res.Root, count), func(c *schemaidx.Cursor) error {
		d := c.Depth()
		var e diag.PathElem
		if d > 0 {
			e, _ = c.Last()
		}
		if count != nil {
			units := 1 + len(c.Node.Members) + len(c.Node.Items)
			if raw, ok := e.Item.(json.RawMessage); ok {
				units += len(raw)
			}
			// Two binary searches narrow the parent's range by the
			// segment; each comparison reads at most its bytes.
			if d > 0 && d <= len(levels) {
				if r := levels[d-1].m; r.hi > r.lo {
					units += (len(e.Name) + 2) * 2 * bits.Len(uint(r.hi-r.lo))
				}
			}
			count(units)
		}
		if c.Node.Style == tree.StyleDefaulted {
			return schemaidx.ErrSkipChildren
		}
		levels = levels[:d]
		if d == 0 {
			levels = append(levels, schemaLevel{node: c.Info.Node, m: pathRange{hi: len(keys)}})
			return nil
		}
		m, ok := levels[d-1].child(keys, e)
		if !ok {
			// The schema does not allow the position, or no deprecated
			// path starts with its schema path: nothing below it is
			// deprecated either.
			return schemaidx.ErrSkipChildren
		}
		levels = append(levels, schemaLevel{node: c.Info.Node, m: m})
		if sp := keys[m.lo]; len(sp) == m.off {
			n := fields[sp]
			add(c.Path(), c.Node.Pos, fmt.Sprintf("field %s is deprecated and is removed in %s", sp, n.Removal), n)
		}
		return nil
	})
	return out
}

// nullsFilled returns n, or, when a member value or element anywhere
// below n is nil, a copy of n in which each nil is a null node with the
// position of its member name or list: schemaidx.Walk reads dispatch
// members and keyed entries' key members, and takes a nil one for a node.
// A nil node is a null to stage G; the copy keeps every position and
// style, so the warnings name the same paths and places. count, when set,
// counts the nodes scanned and copied.
func nullsFilled(n *tree.Node, count func(units int)) *tree.Node {
	nodes, nils := 0, false
	var scan func(n *tree.Node)
	scan = func(n *tree.Node) {
		nodes++
		for _, m := range n.Members {
			if m.Value == nil {
				nils = true
				continue
			}
			scan(m.Value)
		}
		for _, it := range n.Items {
			if it == nil {
				nils = true
				continue
			}
			scan(it)
		}
	}
	scan(n)
	if count != nil {
		count(nodes)
	}
	if !nils {
		return n
	}
	if count != nil {
		count(nodes)
	}
	var fill func(n *tree.Node, at tree.Pos) *tree.Node
	fill = func(n *tree.Node, at tree.Pos) *tree.Node {
		if n == nil {
			return &tree.Node{Kind: tree.KindNull, Pos: at}
		}
		c := *n
		if n.Members != nil {
			c.Members = make([]tree.Member, len(n.Members))
			for i, m := range n.Members {
				c.Members[i] = m
				c.Members[i].Value = fill(m.Value, m.KeyPos)
			}
		}
		if n.Items != nil {
			c.Items = make([]*tree.Node, len(n.Items))
			for i, it := range n.Items {
				c.Items[i] = fill(it, n.Pos)
			}
		}
		return &c
	}
	return fill(n, n.Pos)
}

// pathRange is the deprecated schema paths keys[lo:hi] of a sorted table
// that start with the schema path of one position, which is off bytes
// long.
type pathRange struct{ lo, hi, off int }

// narrow returns the part of m whose paths continue with seg.
func (m pathRange) narrow(keys []string, seg string) (pathRange, bool) {
	end := m.off + len(seg)
	part := func(k string) string { return k[m.off:min(end, len(k))] }
	sub := keys[m.lo:m.hi]
	lo := sort.Search(len(sub), func(i int) bool { return part(sub[i]) >= seg })
	hi := sort.Search(len(sub), func(i int) bool { return part(sub[i]) > seg })
	return pathRange{lo: m.lo + lo, hi: m.lo + hi, off: end}, lo < hi
}

// schemaLevel is one position the schema allows on the path of a
// Deprecations walk.
type schemaLevel struct {
	// node is the position's schema with dispatch applied; nil for free
	// content.
	node *schemaidx.Node
	// m is the range of deprecated paths its schema path starts.
	m pathRange
}

// child returns the range of the child position e of l, whose schema
// path extends l's with the rules of schemaidx.Index.SchemaPath: a
// declared member or a member of an open object or of free content is
// ".<name>" ("<name>" first), a typed-map value "{}", a list element
// "[]"; ok is false for a position the schema does not allow, and for one
// whose schema path no deprecated path starts.
func (l schemaLevel) child(keys []string, e diag.PathElem) (pathRange, bool) {
	if e.Kind != diag.ElemField {
		if l.node != nil {
			if _, ok := l.node.Items(); !ok && !l.node.Types().Has(schemaidx.TypeArray) {
				return pathRange{}, false
			}
		}
		return l.m.narrow(keys, "[]")
	}
	if l.node != nil {
		if _, ok := l.node.Property(e.Name); !ok {
			if _, ok := l.node.Values(); ok {
				return l.m.narrow(keys, "{}")
			}
			if !l.node.Open() {
				return pathRange{}, false
			}
		}
	}
	m := l.m
	if m.off > 0 {
		var ok bool
		if m, ok = m.narrow(keys, "."); !ok {
			return pathRange{}, false
		}
	}
	return m.narrow(keys, e.Name)
}

// location converts p to a diagnostic location, falling back to fallback
// when p is unknown.
func location(files *tree.FileTable, p, fallback tree.Pos) diag.Location {
	if !p.Known() {
		p = fallback
	}
	if !p.Known() {
		return diag.Location{}
	}
	if files == nil {
		return diag.Location{Line: int(p.Line), Column: int(p.Column)}
	}
	return files.Location(p)
}

// maxShown bounds the code points of an authored value quoted in a
// message, as stage G's messages do.
const maxShown = 64

// clip shortens s for a message: its first maxShown code points and
// "...", or all of s when it has no more, an invalid UTF-8 byte counting
// as one code point and written as U+FFFD either way, so a megabyte-long
// apiVersion gives a short message and no message holds bytes that are
// not UTF-8. It reads at most maxShown+1 code points of s and allocates
// only the result.
func clip(s string) string {
	n := 0
	for i := range s {
		if n == maxShown {
			return string([]rune(s[:i])) + "..."
		}
		n++
	}
	if !utf8.ValidString(s) {
		return string([]rune(s))
	}
	return s
}
