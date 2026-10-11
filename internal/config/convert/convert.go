// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package convert is the hub-and-spoke conversion registry of stage G
// (docs/architecture/02-configuration-model.md "Hub-and-spoke conversion";
// spec 02 requirements 4 to 8, 53, 54 and 76). Every resource is decoded
// under its own apiVersion's rendered schema and converted to the hub
// representation before it is normalized and canonicalized, so a Bundle
// may mix apiVersions and converting a Bundle never changes its Revision.
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
// The registry also holds the deprecation lifecycle table of 02 req 76
// (empty in M1), from which Deprecations reports RZ-CFG-025 warnings.
//
// A Registry is immutable after construction and safe for concurrent use.
// The package never logs, reads files, the network or the clock.
package convert

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ConversionDataAnnotation is the metadata.annotations key that carries
// the fields a down-conversion could not express, for the next
// up-conversion to restore (02 req 7). It is the only ruralz.io/
// annotation a Bundle may author (01 req 35) and never appears in the hub
// or the canonical form.
const ConversionDataAnnotation = "ruralz.io/conversion-data"

// HubAPIVersion is the apiVersion whose field set the hub has in M1: the
// hub types are the ruralz/v1alpha1 types (02 req 5).
const HubAPIVersion = v1alpha1.APIVersion

// RZ codes this package puts on diagnostics (all registered in
// internal/errcode).
const (
	// CodeUnserved is RZ-CFG-007: an apiVersion no converter serves
	// (02 req 4 and 53).
	CodeUnserved = "RZ-CFG-007"
	// CodeDeprecated is RZ-CFG-025, a warning for a deprecated apiVersion
	// or field (02 req 76).
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
// member order of every field it does not change (02 req 53), writes a
// field explicitly whenever the source version's default differs from
// the target's (02 req 8), and moves fields the target cannot express
// into ConversionDataAnnotation and back. A ToHub that reads the
// annotation consumes it: it deletes the member, and the annotations (and
// metadata) object when its FromHub created it, so a resource that had an
// empty annotations object keeps it. The Registry, not the converter,
// sets the apiVersion member, and strips any annotation left over from
// the hub form (StripConversionData).
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
// M1 hub field set equals ruralz/v1alpha1 (02 req 5).
func V1alpha1() Converter { return identity{version: v1alpha1.APIVersion} }

// Notice announces the removal of a deprecated apiVersion or field.
type Notice struct {
	// Removal is the release that removes it, such as "0.9.0".
	Removal string
	// Replacement names what to use instead, such as an apiVersion or a
	// field; "" when there is none.
	Replacement string
}

// Lifecycle is the explicit deprecation table of 02 req 76: deprecated
// apiVersions, and deprecated fields per apiVersion keyed by their schema
// path (schemaidx.Index.SchemaPath, such as "spec.upstreams[].weight").
// It is empty in M1.
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
// concurrent use.
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
// apiVersion, with the served apiVersions as the hint (02 req 4 and 53).
// The caller adds the resource and the source Location.
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
		Message: fmt.Sprintf("apiVersion %q is not served by this release", apiVersion),
		Hint:    hint,
	}, true
}

// ToHub converts doc, a resource of kind authored in apiVersion, to the
// hub representation: the spoke converter runs, then the apiVersion
// member (when present) becomes HubAPIVersion and the
// ConversionDataAnnotation is stripped (02 req 7). doc may be rewritten in
// place; callers use the returned tree. An unserved apiVersion is
// RZ-CFG-007 and returns doc unchanged.
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
	setAPIVersion(out, HubAPIVersion)
	StripConversionData(out)
	return out, ds
}

// FromHub converts doc, a hub resource of kind, to apiVersion and sets its
// apiVersion member (when present). An unserved apiVersion is RZ-CFG-007
// and returns doc unchanged.
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
	setAPIVersion(out, apiVersion)
	return out, ds
}

// Convert converts doc, a resource (or with partial, a partial document)
// of kind, from apiVersion from to apiVersion to through the hub (02 req 5
// and 53). Converting to the same apiVersion still runs both halves, so
// render --api-version exercises the whole path in M1 (02 req 54). An
// unserved from or to is RZ-CFG-007 at apiVersion.
func (r *Registry) Convert(from, to, kind string, doc *tree.Node, partial bool) (*tree.Node, diag.List) {
	if d, bad := r.CheckServed(to); bad {
		d.Message = fmt.Sprintf("target apiVersion %q is not served by this release", to)
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
// position; a doc without one (a partial document) is left alone.
func setAPIVersion(doc *tree.Node, v string) {
	if n, ok := doc.Get("apiVersion"); ok && n.Kind == tree.KindString {
		n.Text = v
	}
}

// StripConversionData removes ConversionDataAnnotation from doc's
// metadata.annotations and reports whether it was there: the strip step
// of 02 req 7, which Registry.ToHub runs after the spoke converter. When
// the annotation was the only member, annotations is removed too, so an
// authored conversion-data annotation that no converter consumed leaves
// no empty annotations object in the hub and canonical form.
func StripConversionData(doc *tree.Node) bool {
	meta, ok := doc.Get("metadata")
	if !ok {
		return false
	}
	ann, ok := meta.Get("annotations")
	if !ok || ann.Kind != tree.KindMap || !ann.Delete(ConversionDataAnnotation) {
		return false
	}
	if len(ann.Members) == 0 {
		meta.Delete("annotations")
	}
	return true
}

// Deprecations returns the RZ-CFG-025 warnings of res, a resource authored
// in its apiVersion, from the lifecycle table (02 req 76): one at
// apiVersion when its apiVersion is deprecated, and one per present field
// whose schema path is deprecated. idx is the schema of res's apiVersion;
// files gives source locations (nil: line and column only). Nothing is
// reported for an empty table, which is the M1 case.
func (r *Registry) Deprecations(idx *schemaidx.Index, res *tree.Resource, files *tree.FileTable) diag.List {
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
		if v, ok := res.Root.Get("apiVersion"); ok {
			at = v.Pos
		}
		add(diag.Path{diag.Field("apiVersion")}, at,
			fmt.Sprintf("apiVersion %s is deprecated and is removed in %s", res.APIVersion, n.Removal), n)
	}
	fields := r.lifecycle.Fields[res.APIVersion]
	if len(fields) == 0 || idx == nil {
		return out
	}
	// The schema path of each position is derived from its parent's as
	// the walk descends, in the form schemaidx.Index.SchemaPath gives, so
	// the cost stays linear in the size of the resource: resolving every
	// path again from the root would scan keyed lists once per entry.
	var levels []schemaLevel
	_ = idx.Walk(string(res.ID.Kind), res.Root, func(c *schemaidx.Cursor) error {
		d := c.Depth()
		levels = levels[:d]
		if d == 0 {
			levels = append(levels, schemaLevel{node: c.Info.Node})
			return nil
		}
		e, _ := c.Last()
		sp, ok := levels[d-1].child(e)
		if !ok {
			// The schema does not allow the position, so nothing below it
			// has a schema path either.
			return schemaidx.ErrSkipChildren
		}
		levels = append(levels, schemaLevel{path: sp, node: c.Info.Node})
		if n, ok := fields[sp]; ok {
			add(c.Path(), c.Node.Pos, fmt.Sprintf("field %s is deprecated and is removed in %s", sp, n.Removal), n)
		}
		return nil
	})
	return out
}

// schemaLevel is one position the schema allows on the path of a
// Deprecations walk.
type schemaLevel struct {
	// path is the schema path, such as "spec.upstreams[]".
	path string
	// node is the position's schema with dispatch applied; nil for free
	// content.
	node *schemaidx.Node
}

// child returns the schema path of the child position e of l, with the
// rules of schemaidx.Index.SchemaPath: a declared member or a member of an
// open object or of free content is ".<name>", a typed-map value "{}", a
// list element "[]"; ok is false for a position the schema does not
// allow.
func (l schemaLevel) child(e diag.PathElem) (string, bool) {
	if e.Kind != diag.ElemField {
		if l.node != nil {
			if _, ok := l.node.Items(); !ok && !l.node.Types().Has(schemaidx.TypeArray) {
				return "", false
			}
		}
		return l.path + "[]", true
	}
	if l.node != nil {
		if _, ok := l.node.Property(e.Name); !ok {
			if _, ok := l.node.Values(); ok {
				return l.path + "{}", true
			}
			if !l.node.Open() {
				return "", false
			}
		}
	}
	if l.path == "" {
		return e.Name, true
	}
	return l.path + "." + e.Name, true
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
