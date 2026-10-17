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
// definition, which is replaced whole and never looked into. An overlay
// that changes how a base value is read (a Policy spec.type changed from
// validation.json-schema) can move such a value out of that position:
// a base $patch exempted there and read elsewhere after the merge is
// RZ-CFG-006 like any other base $patch, related the overlay member that
// changed the dispatch, and is removed.
//
// The pipeline runs CheckBase on every base resource, in its worker pool
// and whether or not an overlay is selected, and Apply once for the
// cross-resource identity merge (01 req 53).
//
// Overlays merge before substitution (01 req 27), so a dispatch member
// holding a ${VAR} expression (a Policy spec.type of ${TYPE}) selects no
// if/then branch, in a base document as in a merged one. Three
// consequences follow, each pinned by TestUnresolvedDispatch: the
// object's composite members merge as free content, so keyed lists inside
// them are replaced whole and an overlay $patch inside them is
// RZ-CFG-005; CheckBase reads a base value there as outside a JSON Schema
// document, so a base Policy of spec.type ${TYPE} whose config.schema
// holds a $patch member is RZ-CFG-006; and an overlay that sets the
// spec.type of a base validation.json-schema Policy to an expression
// moves the base config.schema out of the document position, so a base
// $patch there is the RZ-CFG-006 of a changed dispatch (Apply). When the
// variable names validation.json-schema, substitution would have made
// each of these errors unnecessary. Spec 01 does not decide the case; a
// contract change request against 01 req 25 and 27 names all three.
//
// Removing what is absent changes nothing (01 req 25 "absent key: no-op",
// 01 risk 18): a member the base lacks is not added when the overlay
// value holds nothing but removals (null members and $patch: delete
// entries, at any depth), and such a value against a base value of
// another kind leaves the base value in place, also for the dispatch: a
// Policy spec.type the overlay writes as {a: null} stays the base type,
// and config merges under it. A written {} or [] and an
// applied {$patch: replace} or ruralz.io/patch: replace are values and
// stay (contract change request against 01 req 25 for the object case).
//
// The merge costs time linear in its input: objects and keyed lists
// longer than a few members, merged with an overlay of more than a few,
// are indexed by key once (01 req 54). Each call records at most
// Options.MaxDiagnostics+1 diagnostics, so the caller's cap appends its
// RZ-CFG-001, and calls Options.Yield every 256 work units (01 req 50 and
// 53). The units are counted one at a time as the work is done, so the
// calls are spread over the work, never in a burst before or after a loop
// (meter.go): every loop over the input counts each element it reads,
// and every library call whose cost grows with the input runs through a
// counted helper; schemaidx Select reads a proxy of at most the members
// its rules may test. What a unit cannot interrupt is a single runtime
// operation on one value at memory speed (a map hash, a string
// comparison, the allocation or growth of one slice, with the garbage
// collector's assist for it).
//
// Results never depend on the order of the overlay files: an identity in
// two overlay documents is RZ-CFG-008 and none of its documents applies,
// and new identities are appended in (kind, name) order.
package overlay

import (
	"cmp"
	"context"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Configuration codes this package raises (all registered in
// internal/errcode).
const (
	// CodeSchema: a malformed overlay directive ($patch in an object, in
	// an element of a set or atomic list or in free content, an
	// unsupported $patch or ruralz.io/patch value, {$patch: replace} not
	// first or outside an orderedMap list, a keyed-list entry without its
	// key field, a delete entry with other members), or two entries with
	// one key in one overlay list.
	CodeSchema = "RZ-CFG-005"
	// CodeBasePatch: $patch in a base document (valid only in overlays),
	// found by CheckBase, or by Apply for a base $patch that was data
	// inside a JSON Schema document and that the merged dispatch reads
	// outside one.
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

// DefaultMaxDiagnostics is the diagnostics one Apply or CheckBase call
// records by default: the per-run cap of 01 req 50 (10,000, proposed).
const DefaultMaxDiagnostics = 10_000

// yieldEvery is the number of work units between Yield calls (01 req 53:
// "every 256 nodes inside a unit"). A work unit is a node visited or a
// member, entry or key a lookup reads, so the units also measure the
// merge's cost independently of the machine (TestScaleLinear).
const yieldEvery = 256

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

// Options configure Apply and CheckBase. Both take them by value and only
// read them, so concurrent CheckBase calls may share one Options; a
// loader worker that tracks the time since its own last yield (01 req 53)
// passes its own copy, whose Yield holds that state.
type Options struct {
	// Schemas holds the schema index of every served apiVersion.
	Schemas *schemaidx.Set
	// Files converts tree positions to diagnostic locations; nil leaves
	// locations empty.
	Files *tree.FileTable
	// MaxDiagnostics is the run's diagnostic cap (01 req 50;
	// DefaultMaxDiagnostics when zero or less), so one document cannot
	// allocate more than the run may report (01 risk 1). A call records
	// the first MaxDiagnostics+1 diagnostics in walk order and stops
	// walking once it has: MaxDiagnostics+1 results mean the call stopped
	// early. The caller appends the single RZ-CFG-001 "too many
	// diagnostics" error, as the loader's diag.Collector given the same
	// cap does on the extra one, and an Apply result that stopped early
	// is incomplete, which that error keeps from use.
	MaxDiagnostics int
	// Yield, when set, is called between resources and every 256 work
	// units inside one, so the caller's worker can yield the processor
	// (01 req 53). The units are counted as the work is done, so the
	// calls are spread over it and a time-based Yield sees no burst (see
	// the package doc). A call invokes it on its own goroutine only, so a
	// function given to one worker's CheckBase calls needs no lock; one
	// shared by CheckBase calls on several goroutines must be safe for
	// concurrent use. The package never reads the clock.
	Yield func()
}

// Apply merges patches, the documents of one overlay, into base, the base
// union of a Bundle, and returns the merged resources with the stage D
// diagnostics (01 req 23 to 25). It takes ownership of both slices'
// resources: merged trees are rewritten in place and share nodes with the
// patches, so callers use only the returned resources afterwards.
//
// Every object's keys are unique, as stage B leaves them (it keeps the
// first member of a key and reports RZ-CFG-002). Apply holds to that for
// any input: an overlay member whose key an earlier member of its object
// had is skipped, so the first one applies and no other member changes,
// and a base object's first member of a key is the one merged into.
//
// The result holds the base resources in their input order (a patched one
// keeps its Start, a deleted one is dropped) followed by the resources the
// overlay adds, in (kind, name) order. Identities are taken from
// tree.Resource.ID as the loader set them; documents that failed stage C
// (envelope and identity) must not be passed. Apply does not look for
// $patch in base documents, which the pipeline checks with CheckBase,
// except where the merge changes an object's dispatch: a base $patch
// that CheckBase exempted as JSON Schema document data and that the
// merged object reads outside such a document is RZ-CFG-006 (01 req 25).
// An overlay spec.type holding a ${VAR} expression selects no branch, so
// it is such a change, whatever the variable would name (see the package
// doc).
//
// ctx is checked between resources; on cancellation Apply returns
// ctx.Err() and no result (01 req 53).
func Apply(ctx context.Context, base, patches []*tree.Resource, opts Options) ([]*tree.Resource, diag.List, error) {
	return apply(ctx, base, patches, opts, nil)
}

// apply is Apply, recording how its work is spread in p when p is not nil
// (tests).
func apply(ctx context.Context, base, patches []*tree.Resource, opts Options, p *probe) ([]*tree.Resource, diag.List, error) {
	a := newApplier(opts)
	a.setProbe(p)
	out := make([]*tree.Resource, 0, len(base)+len(patches))
	byID := make(map[tree.ID]int, len(base))
	for _, r := range base {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		a.between()
		if a.full {
			break
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
		a.between()
		if a.full {
			break
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
				Message: "kind " + strconv.Quote(clip(string(p.ID.Kind))) + " is not served by apiVersion " + strconv.Quote(clip(p.APIVersion)),
			})
			continue
		}
		if action == PatchReplace {
			m.replaceSpec(p.Root, root)
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
				a.remove(b.Root, "spec")
			}
			b.Root = m.mergeMap(b.Root, p.Root, root)
		default:
			p.Root = m.element(p.Root, root)
			added = append(added, p)
		}
	}
	out = deleteFunc(a, out, func(r *tree.Resource) bool { return r == nil })
	sortStable(a, added, func(x, y *tree.Resource) int {
		return cmp.Or(cmp.Compare(x.ID.Kind, y.ID.Kind), cmp.Compare(x.ID.Name, y.ID.Name))
	})
	for _, r := range added {
		a.tick()
		out = append(out, r)
	}
	return out, a.diags, nil
}

// CheckBase reports every $patch member of res, a base document, as
// RZ-CFG-006 ("valid only in overlays", 01 req 25), except inside the
// value of the JSONSchemaDocument definition, which is data. It reads
// only res and opts, so the loader runs it per resource in its worker
// pool (01 req 53), each worker passing Options with its own Yield. A
// resource whose apiVersion or kind has no schema is skipped: stage C and
// Apply report it (RZ-CFG-007). At most opts.MaxDiagnostics+1
// diagnostics are returned.
//
// A dispatch member holding a ${VAR} expression selects no if/then
// branch, so a $patch inside a Policy config.schema under spec.type
// ${TYPE} is outside a JSON Schema document here, and RZ-CFG-006, even
// when the variable would name validation.json-schema (see the package
// doc).
func CheckBase(res *tree.Resource, opts Options) diag.List { return checkBase(res, opts, nil) }

// checkBase is CheckBase, recording how its work is spread in p when p is
// not nil (tests).
func checkBase(res *tree.Resource, opts Options, p *probe) diag.List {
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
	a := newApplier(opts)
	a.setProbe(p)
	a.merger(res.ID).checkBase(res.Root, root)
	return a.diags
}

// applier holds the state of one Apply or CheckBase call.
type applier struct {
	opts  Options
	diags diag.List
	// max is the diagnostic budget; full is set once the diagnostic
	// beyond it was recorded, and every walk then stops.
	max  int
	full bool
	// quiet drops every diagnostic while settle normalizes a copy of an
	// overlay value whose diagnostics the merge reports afterwards.
	quiet bool
	// units counts the work units since the last Yield call.
	units int
	// probe, when set, records how the work is spread (tests).
	probe *probe
}

func newApplier(opts Options) *applier {
	a := &applier{opts: opts, max: opts.MaxDiagnostics}
	if a.max <= 0 {
		a.max = DefaultMaxDiagnostics
	}
	return a
}

// between marks the boundary between two resources: Yield is called and
// the unit count starts again.
func (a *applier) between() {
	a.units = 0
	a.yield()
}

func (a *applier) loc(p tree.Pos) diag.Location {
	if a.opts.Files == nil {
		return diag.Location{}
	}
	return a.opts.Files.Location(p)
}

// report records d unless the budget is spent or settle's trial turned
// diagnostics off; the diagnostic beyond the budget is the last one
// recorded and sets full.
func (a *applier) report(d diag.Diagnostic) {
	if a.full || a.quiet {
		return
	}
	if d.Severity == 0 {
		d.Severity = diag.SeverityError
	}
	a.diags = append(a.diags, d)
	a.full = len(a.diags) > a.max
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
		Code: CodeAPIVersion, Location: a.loc(a.memberPos(r, "apiVersion")), Resource: r.ID.ResourceID(),
		Path:    diag.Path{diag.Field("apiVersion")},
		Message: "apiVersion " + strconv.Quote(clip(r.APIVersion)) + " is not served",
	})
	return nil, false
}

// unique orders patches by file, line and column and drops every identity
// declared by two or more of them, reporting RZ-CFG-008 at each later
// declaration (01 req 24): the result never depends on file order. The
// first declaration's name position is read once per identity, however
// many later declarations relate it.
func (a *applier) unique(patches []*tree.Resource) []*tree.Resource {
	var sorted []*tree.Resource
	for _, r := range patches {
		a.tick()
		if r != nil {
			sorted = append(sorted, r)
		}
	}
	sortStable(a, sorted, func(x, y *tree.Resource) int {
		lx, ly := a.loc(x.Start), a.loc(y.Start)
		return cmp.Or(
			cmp.Compare(lx.File, ly.File), cmp.Compare(x.Start.File, y.Start.File),
			cmp.Compare(x.Start.Line, y.Start.Line), cmp.Compare(x.Start.Column, y.Start.Column),
		)
	})
	type declared struct {
		// at is the first declaration's name position, read when a later
		// one is reported; read tells whether it was.
		at   tree.Pos
		read bool
		dup  bool
		r    *tree.Resource
	}
	first := map[tree.ID]int{}
	var decls []declared
	for _, p := range sorted {
		a.tick()
		i, seen := first[p.ID]
		if !seen {
			first[p.ID] = len(decls)
			decls = append(decls, declared{r: p})
			continue
		}
		f := &decls[i]
		f.dup = true
		if a.full {
			continue
		}
		if !f.read {
			f.at, f.read = a.namePos(f.r), true
		}
		a.report(diag.Diagnostic{
			Code: CodeDuplicate, Location: a.loc(a.namePos(p)), Resource: p.ID.ResourceID(),
			Path:    diag.Path{diag.Field("metadata"), diag.Field("name")},
			Message: p.ID.String() + " is declared twice in the overlay; results never depend on file order",
			Related: []diag.Related{{Location: a.loc(f.at), Message: "first declared in"}},
		})
	}
	return deleteFunc(a, sorted, func(r *tree.Resource) bool { return decls[first[r.ID]].dup })
}

// mismatch reports RZ-CFG-030: an overlay document patching base under
// another apiVersion (01 req 24).
func (a *applier) mismatch(p, base *tree.Resource) {
	a.report(diag.Diagnostic{
		Code: CodeVersionMismatch, Location: a.loc(a.memberPos(p, "apiVersion")), Resource: p.ID.ResourceID(),
		Path: diag.Path{diag.Field("apiVersion")},
		Message: "overlay apiVersion " + strconv.Quote(clip(p.APIVersion)) + " differs from the base resource's " +
			strconv.Quote(clip(base.APIVersion)),
		Hint:    "patch a resource with the apiVersion of its base document",
		Related: []diag.Related{{Location: a.loc(a.memberPos(base, "apiVersion")), Message: "base resource declared in"}},
	})
}

// memberPos returns the position of the root member key's value, or the
// document start, counting the members it reads.
func (a *applier) memberPos(r *tree.Resource, key string) tree.Pos {
	if v, ok := a.get(r.Root, key); ok && v.Pos.Known() {
		return v.Pos
	}
	return r.Start
}

// namePos returns the position of metadata.name, or the document start,
// counting the members it reads.
func (a *applier) namePos(r *tree.Resource) tree.Pos {
	md, _ := a.get(r.Root, "metadata")
	if v, ok := a.get(md, "name"); ok && v.Pos.Known() {
		return v.Pos
	}
	return r.Start
}
