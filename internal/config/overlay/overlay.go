// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package overlay is stage D of the configuration pipeline: it merges the
// documents of one overlay (overlays/<env>/) into the base union of a
// Bundle with Kubernetes strategic-merge semantics driven by the
// x-ruralz-list keyword of the rendered schema
// (docs/architecture/02-configuration-model.md "Overlays"; spec 01
// requirements 23 to 25).
//
// The merge reads the schema of the base resource's apiVersion through
// internal/config/schemaidx (architecture R-3): objects merge recursively
// with if/then dispatch applied to the merged object, so a Policy config
// merges by the merged spec.type; map and orderedMap lists merge by their
// key field; set and atomic lists, lists without x-ruralz-list, scalars and
// the value of the JSONSchemaDocument definition (validation.json-schema
// config.schema) are replaced whole. An overlay null removes a member.
//
// The directives are the annotation ruralz.io/patch (delete or replace)
// and the $patch member of a keyed-list entry ({<key>: v, $patch: delete})
// or of the first element of an orderedMap list ({$patch: replace}).
// $patch is valid only there and only in overlays (CM "Overlays"; 01 req
// 25): anywhere else in an overlay document it is RZ-CFG-005, including
// free content (undeclared members of open objects such as the M1 open
// configs, lists without x-ruralz-list, plugin configs and Plugin
// configSchema), and anywhere in a base document it is RZ-CFG-006
// (CheckBase). The only exemption is the value of the JSONSchemaDocument
// definition, which is replaced whole and never looked into.
//
// The pipeline runs CheckBase on every base resource, in its worker pool
// and whether or not an overlay is selected, and Apply once for the
// cross-resource identity merge (01 req 53).
//
// Overlays merge before substitution (01 req 27), so a dispatch member
// holding a ${VAR} expression (a Policy spec.type of ${TYPE}) selects no
// if/then branch: the object's composite members then merge as free
// content, keyed lists inside them are replaced whole and a $patch
// inside them is RZ-CFG-005. Spec 01 does not decide this case; it is
// raised as a contract change request against 01 req 25 and 27.
//
// Results never depend on the order of the overlay files: an identity in
// two overlay documents is RZ-CFG-008 and none of its documents applies,
// and new identities are appended in (kind, name) order.
package overlay

import (
	"cmp"
	"context"
	"slices"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Configuration codes this package raises (all registered in
// internal/errcode).
const (
	// CodeSchema: a malformed directive ($patch in an object, in an
	// element of a set or atomic list or in free content, an unsupported
	// $patch or ruralz.io/patch value, {$patch: replace} not first or
	// outside an orderedMap list, a keyed-list entry without its key
	// field, a delete entry with other members) or two entries with one
	// key in one overlay list.
	CodeSchema = "RZ-CFG-005"
	// CodeBasePatch: $patch in a base document (valid only in overlays).
	CodeBasePatch = "RZ-CFG-006"
	// CodeAPIVersion: a document whose apiVersion has no schema in the
	// Set. Stage C reports and drops such documents first; Apply repeats
	// the check so it never merges without a schema.
	CodeAPIVersion = "RZ-CFG-007"
	// CodeDuplicate: one identity in two documents of one overlay.
	CodeDuplicate = "RZ-CFG-008"
	// CodeVersionMismatch: an overlay document patching an existing
	// identity with another apiVersion.
	CodeVersionMismatch = "RZ-CFG-030"
)

// Directive names.
const (
	// PatchAnnotation is the resource-level directive annotation; it never
	// survives into the result.
	PatchAnnotation = "ruralz.io/patch"
	// PatchKey is the list-entry directive member.
	PatchKey = "$patch"
	// PatchDelete removes a resource (annotation) or a keyed-list entry.
	PatchDelete = "delete"
	// PatchReplace replaces a resource's spec (annotation) or a whole
	// orderedMap list ({$patch: replace} as its first element).
	PatchReplace = "replace"
)

// schemaDocumentDef is the $defs name of the inline JSON Schema document
// whose value an overlay replaces whole (CM "Overlays" table; 07 req 74).
const schemaDocumentDef = "JSONSchemaDocument"

// Options configure Apply and CheckBase.
type Options struct {
	// Schemas holds the schema index of every served apiVersion.
	Schemas *schemaidx.Set
	// Files converts tree positions to diagnostic locations; nil leaves
	// locations empty.
	Files *tree.FileTable
}

// Apply merges patches, the documents of one overlay, into base, the base
// union of a Bundle, and returns the merged resources with the stage D
// diagnostics (01 req 23 to 25). It takes ownership of both slices'
// resources: merged trees are rewritten in place and share nodes with the
// patches, so callers use only the returned resources afterwards.
//
// The result holds the base resources in their input order (a patched one
// keeps its Start, a deleted one is dropped) followed by the resources the
// overlay adds, in (kind, name) order. Identities are taken from
// tree.Resource.ID as the loader set them; documents that failed stage C
// (envelope and identity) must not be passed. Apply does not look for
// $patch in base documents: the pipeline runs CheckBase on each of them.
//
// ctx is checked between resources; on cancellation Apply returns
// ctx.Err() and no result (01 req 53).
func Apply(ctx context.Context, base, patches []*tree.Resource, opts Options) ([]*tree.Resource, diag.List, error) {
	a := &applier{opts: opts}
	out := make([]*tree.Resource, 0, len(base)+len(patches))
	byID := make(map[tree.ID]int, len(base))
	for _, r := range base {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if _, ok := a.index(r); !ok {
			continue
		}
		if _, dup := byID[r.ID]; !dup {
			byID[r.ID] = len(out)
		}
		out = append(out, r)
	}
	var added []*tree.Resource
	for _, p := range a.unique(patches) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		x, ok := a.index(p)
		if !ok {
			continue
		}
		i, exists := byID[p.ID]
		if exists && p.APIVersion != out[i].APIVersion {
			a.mismatch(p, out[i])
			continue
		}
		m := a.merger(p.ID)
		action, ok := m.takeAction(p.Root)
		if !ok {
			continue
		}
		root, ok := x.Resource(string(p.ID.Kind))
		if !ok {
			a.report(diag.Diagnostic{
				Code: CodeAPIVersion, Location: a.loc(p.Start), Resource: p.ID.ResourceID(),
				Message: "kind " + strconv.Quote(string(p.ID.Kind)) + " is not served by apiVersion " + strconv.Quote(p.APIVersion),
			})
			continue
		}
		switch {
		case action == PatchDelete && exists:
			out[i] = nil
			delete(byID, p.ID)
		case action == PatchDelete:
			// Deleting an absent resource is a no-op (01 risk 18).
		case exists:
			b := out[i]
			if action == PatchReplace {
				b.Root.Delete("spec")
			}
			b.Root = m.mergeMap(b.Root, p.Root, root, nil)
		default:
			p.Root = m.normalize(p.Root, root, nil)
			added = append(added, p)
		}
	}
	out = slices.DeleteFunc(out, func(r *tree.Resource) bool { return r == nil })
	slices.SortStableFunc(added, func(x, y *tree.Resource) int {
		return cmp.Or(cmp.Compare(x.ID.Kind, y.ID.Kind), cmp.Compare(x.ID.Name, y.ID.Name))
	})
	return append(out, added...), a.diags, nil
}

// CheckBase reports every $patch member of res, a base document, as
// RZ-CFG-006 ("valid only in overlays", 01 req 25), except inside the
// value of the JSONSchemaDocument definition, which is data. It reads
// only res and opts, so the loader runs it per resource in its worker
// pool (01 req 53). A resource whose apiVersion or kind has no schema is
// skipped: stage C and Apply report it (RZ-CFG-007).
func CheckBase(res *tree.Resource, opts Options) diag.List {
	if res == nil || res.Root == nil || res.Root.Kind != tree.KindMap || opts.Schemas == nil {
		return nil
	}
	x, ok := opts.Schemas.Index(res.APIVersion)
	if !ok {
		return nil
	}
	root, ok := x.Resource(string(res.ID.Kind))
	if !ok {
		return nil
	}
	a := &applier{opts: opts}
	a.merger(res.ID).checkBase(res.Root, root)
	return a.diags
}

// applier holds the state of one Apply call.
type applier struct {
	opts  Options
	diags diag.List
}

func (a *applier) loc(p tree.Pos) diag.Location {
	if a.opts.Files == nil {
		return diag.Location{}
	}
	return a.opts.Files.Location(p)
}

func (a *applier) report(d diag.Diagnostic) {
	if d.Severity == 0 {
		d.Severity = diag.SeverityError
	}
	a.diags = append(a.diags, d)
}

func (a *applier) merger(id tree.ID) *merger { return &merger{a: a, id: id} }

// index returns the schema index of r's apiVersion, reporting
// RZ-CFG-007 when none is served.
func (a *applier) index(r *tree.Resource) (*schemaidx.Index, bool) {
	if r == nil || r.Root == nil || r.Root.Kind != tree.KindMap {
		return nil, false
	}
	if a.opts.Schemas != nil {
		if x, ok := a.opts.Schemas.Index(r.APIVersion); ok {
			return x, true
		}
	}
	a.report(diag.Diagnostic{
		Code: CodeAPIVersion, Location: a.loc(memberPos(r, "apiVersion")), Resource: r.ID.ResourceID(),
		Path:    diag.Path{diag.Field("apiVersion")},
		Message: "apiVersion " + strconv.Quote(r.APIVersion) + " is not served",
	})
	return nil, false
}

// unique orders patches by file, line and column and drops every identity
// declared by two or more of them, reporting RZ-CFG-008 at each later
// declaration (01 req 24): the result never depends on file order.
func (a *applier) unique(patches []*tree.Resource) []*tree.Resource {
	sorted := slices.DeleteFunc(slices.Clone(patches), func(r *tree.Resource) bool { return r == nil })
	slices.SortStableFunc(sorted, func(x, y *tree.Resource) int {
		lx, ly := a.loc(x.Start), a.loc(y.Start)
		return cmp.Or(
			cmp.Compare(lx.File, ly.File), cmp.Compare(x.Start.File, y.Start.File),
			cmp.Compare(x.Start.Line, y.Start.Line), cmp.Compare(x.Start.Column, y.Start.Column),
		)
	})
	first := make(map[tree.ID]*tree.Resource, len(sorted))
	dup := map[tree.ID]bool{}
	for _, p := range sorted {
		f, seen := first[p.ID]
		if !seen {
			first[p.ID] = p
			continue
		}
		dup[p.ID] = true
		a.report(diag.Diagnostic{
			Code: CodeDuplicate, Location: a.loc(namePos(p)), Resource: p.ID.ResourceID(),
			Path:    diag.Path{diag.Field("metadata"), diag.Field("name")},
			Message: p.ID.String() + " is declared twice in the overlay; results never depend on file order",
			Related: []diag.Related{{Location: a.loc(namePos(f)), Message: "first declared in"}},
		})
	}
	return slices.DeleteFunc(sorted, func(r *tree.Resource) bool { return dup[r.ID] })
}

// mismatch reports RZ-CFG-030: an overlay document patching base under
// another apiVersion (01 req 24).
func (a *applier) mismatch(p, base *tree.Resource) {
	a.report(diag.Diagnostic{
		Code: CodeVersionMismatch, Location: a.loc(memberPos(p, "apiVersion")), Resource: p.ID.ResourceID(),
		Path: diag.Path{diag.Field("apiVersion")},
		Message: "overlay apiVersion " + strconv.Quote(p.APIVersion) + " differs from the base resource's " +
			strconv.Quote(base.APIVersion),
		Hint:    "patch a resource with the apiVersion of its base document",
		Related: []diag.Related{{Location: a.loc(memberPos(base, "apiVersion")), Message: "base resource declared in"}},
	})
}

// memberPos returns the position of the root member key's value, or the
// document start.
func memberPos(r *tree.Resource, key string) tree.Pos {
	if v, ok := r.Root.Get(key); ok && v.Pos.Known() {
		return v.Pos
	}
	return r.Start
}

// namePos returns the position of metadata.name, or the document start.
func namePos(r *tree.Resource) tree.Pos {
	md, _ := r.Root.Get("metadata")
	if v, ok := md.Get("name"); ok && v.Pos.Known() {
		return v.Pos
	}
	return r.Start
}
