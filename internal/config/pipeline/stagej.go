// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// CodeCEL is RZ-CFG-014: a CEL syntax, type or unavailable-variable error,
// the code stage J gives a finding the compiler reports without one.
const CodeCEL = "RZ-CFG-014"

// relatedAttached is the message of the related location of each
// attaching PolicyRef whose site yields a CEL finding.
const relatedAttached = "attached at"

// ErrPlace marks an x-ruralz-cel field that maps to no place of
// expr.Places: the schema and the CEL contract disagree, a fault of the
// release, never of the Bundle.
var ErrPlace = errors.New("pipeline: an x-ruralz-cel field maps to no CEL place")

// attachment is one PolicyRef that attaches a Policy some Route's
// effective chain runs: the Gateway, Route or Upstream whose spec.policies
// holds it, its position there, its location and the Phases the Policy
// runs in at its scope.
type attachment struct {
	holder tree.ID
	index  int
	ref    diag.Location
	scope  phase.Scope
	phases phase.Set
}

// compareAttachments orders attachments canonically: by the holder's kind
// (hub.KindOrder) and name, then by position in its list.
func compareAttachments(a, b attachment) int {
	return cmp.Or(
		cmp.Compare(hub.KindOrder(a.holder.Kind), hub.KindOrder(b.holder.Kind)),
		cmp.Compare(a.holder.Name, b.holder.Name),
		cmp.Compare(a.index, b.index),
	)
}

// attachmentsOf returns, by Policy name, the attaching PolicyRefs of every
// Policy that some Route's effective chain runs, in canonical order: a
// Gateway PolicyRef once, however many Routes inherit it.
func attachmentsOf(b *hub.Bundle, chains map[string]*hub.Chain) map[string][]attachment {
	var gateway tree.ID
	for _, r := range b.Resources() {
		if r.Kind == v1alpha1.KindGateway {
			gateway = r.ID
			break
		}
	}
	type key struct {
		holder tree.ID
		index  int
	}
	seen := map[key]bool{}
	out := map[string][]attachment{}
	add := func(e hub.Entry, holder tree.ID) {
		k := key{holder: holder, index: e.Position}
		if seen[k] {
			return
		}
		seen[k] = true
		out[e.Policy] = append(out[e.Policy], attachment{holder: holder, index: e.Position, ref: e.Ref, scope: e.Scope, phases: e.Phases})
	}
	for _, name := range slices.Sorted(maps.Keys(chains)) {
		c := chains[name]
		if c == nil {
			continue
		}
		route := tree.ID{Kind: v1alpha1.KindRoute, Name: name}
		for p := range phase.Count {
			for _, e := range c.Client[p] {
				if e.Scope == phase.ScopeGateway {
					add(e, gateway)
				} else {
					add(e, route)
				}
			}
		}
		for _, l := range c.Legs {
			up := tree.ID{Kind: v1alpha1.KindUpstream, Name: l.Upstream}
			for p := range phase.Count {
				for _, e := range l.Phases[p] {
					add(e, up)
				}
			}
		}
	}
	for _, as := range out {
		slices.SortFunc(as, compareAttachments)
	}
	return out
}

// siteGroup is one site of a Policy's fields: a scope, the first Phase the
// Policy runs in there and whether it gates the request body there, with
// the attaching PolicyRefs that share it.
type siteGroup struct {
	scope     phase.Scope
	first     phase.Phase
	gatesBody bool
	refs      []attachment
}

// site returns the group's site of the Policy field at place whose result
// is result: the result a transform set[] valueExpression takes by its
// entry's target (entryResult), else 0 for the place's own.
func (g siteGroup) site(place expr.PlaceID, t v1alpha1.PolicyType, result expr.Result) expr.Site {
	s := expr.Site{Place: place, Scope: g.scope, PolicyType: t, Result: result}
	if place == expr.PlacePolicyWhen {
		s.FirstPhase, s.GatesBody = g.first, g.gatesBody
	}
	return s
}

// celStage is stage J of one run: it compiles every CEL expression of the
// Bundle stage G built at each of its sites and checks its cost
// (docs/architecture/02-configuration-model.md "Allowed places",
// "Variables", "Limits"). It is immutable but for its cache once built.
type celStage struct {
	p        *Pipeline
	b        *hub.Bundle
	files    *tree.FileTable
	text     func(tree.FileID) ([]byte, bool)
	attached map[string][]attachment
	sites    sites
	cache    sync.Map
}

// celResult is what stage J found in one resource.
type celResult struct {
	ds   diag.List
	uses []hub.CELUse
	err  error
}

// compileCEL runs stage J on b, whose effective chains are chains, on the
// worker pool: one unit per resource. It returns every expression with its
// site (hub.Validated.CEL), the findings of each resource in canonical
// order, and an error for ctx or a fault of the release (ErrPlace, or a
// Policy type the registry does not list).
func (p *Pipeline) compileCEL(ctx context.Context, b *hub.Bundle, chains map[string]*hub.Chain, files *tree.FileTable, text func(tree.FileID) ([]byte, bool)) ([]hub.CELUse, []diag.List, error) {
	j := &celStage{p: p, b: b, files: files, text: text, attached: attachmentsOf(b, chains)}
	j.sites = sites{cel: p.cel, reg: p.reg, cache: &j.cache}
	rs := b.Resources()
	out := make([]celResult, len(rs))
	if err := p.parallel(ctx, len(rs), func(w *worker, i int) { out[i] = j.resource(w, rs[i]) }); err != nil {
		return nil, nil, err
	}
	var uses []hub.CELUse
	ds := make([]diag.List, len(rs))
	for i := range out {
		if out[i].err != nil {
			return nil, nil, out[i].err
		}
		uses = append(uses, out[i].uses...)
		ds[i] = out[i].ds
	}
	return uses, ds, nil
}

// frame is what the walk of one resource knows about a position: the
// nearest position at or above it that no default produced.
type frame struct {
	best      tree.Pos
	defaulted bool
}

// resource compiles the expressions of r, a unit of the pool. A resource
// without a tree or a typed view, which stage G never hands over, has no
// site to compile at.
func (j *celStage) resource(w *worker, r *hub.Resource) celResult {
	var out celResult
	if r == nil || r.Tree == nil || r.Object == nil {
		return out
	}
	policy, _ := hub.Object[v1alpha1.Policy](r)
	route, _ := hub.Object[v1alpha1.Route](r)
	if (r.Kind == v1alpha1.KindPolicy && policy == nil) || (r.Kind == v1alpha1.KindRoute && route == nil) {
		return out
	}
	var (
		ptype    v1alpha1.PolicyType
		groups   []siteGroup
		attached bool
		grouped  bool
	)
	if policy != nil {
		ptype = policy.Spec.Type
	}
	stack := make([]frame, 0, 16)
	err := j.p.hub.Walk(string(r.Kind), r.Tree, func(c *schemaidx.Cursor) error {
		d := c.Depth()
		stack = stack[:min(d, len(stack))]
		var fr frame
		if d > 0 && len(stack) > 0 {
			fr = stack[len(stack)-1]
		}
		if c.Node != nil {
			fr.defaulted = fr.defaulted || c.Node.Style == tree.StyleDefaulted
			if !fr.defaulted && c.Node.Pos.Known() {
				fr.best = c.Node.Pos
			}
		}
		stack = append(stack, fr)
		if c.Info.Node == nil {
			// Free content holds no x-ruralz-cel field.
			return schemaidx.ErrSkipChildren
		}
		if c.Info.Keywords().CEL == nil || c.Node == nil || c.Node.Kind != tree.KindString || c.Node.Text == "" {
			return nil
		}
		path := c.Path()
		place, ok := PlaceOf(r.Kind, ptype, path)
		if !ok {
			return fmt.Errorf("%w: %s %s", ErrPlace, r.ID, path)
		}
		f := field{r: r, path: path, node: c.Node, loc: j.location(r, fr.best)}
		if policy == nil {
			site := expr.Site{Place: place}
			if route != nil {
				site = RouteSite(route, place, stepOf(path))
			}
			j.single(w, &out, f, site)
			return nil
		}
		if !grouped {
			var err error
			if groups, attached, err = j.groups(r, policy); err != nil {
				return err
			}
			grouped = true
		}
		if attached {
			j.perAttachment(w, &out, f, groups, place, ptype)
		} else {
			j.everyScope(w, &out, f, groups, place, ptype)
		}
		return nil
	})
	if err != nil {
		out.err = err
	}
	return out
}

// field is one CEL expression: its resource, path, node and the location
// of the field, or of its nearest authored ancestor when a default wrote
// it.
type field struct {
	r    *hub.Resource
	path diag.Path
	node *tree.Node
	loc  diag.Location
}

// use returns the hub.CELUse of f at site.
func (f field) use(site expr.Site) hub.CELUse {
	return hub.CELUse{Site: site, Expr: f.node.Text, Resource: *f.r.ResourceID(), Path: f.path, Loc: f.loc}
}

// issueKey identifies a distinct issue of an expression: one diagnostic
// reports it however many sites raise it.
type issueKey struct {
	code, message, hint  string
	offset, line, column int
}

// keyOf returns the key of is.
func keyOf(is expr.Issue) issueKey {
	return issueKey{code: is.Code, message: is.Message, hint: is.Hint, offset: is.Offset, line: is.Line, column: is.Column}
}

// single compiles a field that has one site: every field outside a
// Policy.
func (j *celStage) single(w *worker, out *celResult, f field, site expr.Site) {
	seen := map[issueKey]bool{}
	for _, is := range j.check(w, site, f.node.Text) {
		k := keyOf(is)
		if seen[k] {
			continue
		}
		seen[k] = true
		out.ds = append(out.ds, j.diagnostic(f, is, nil))
	}
	out.uses = append(out.uses, f.use(site))
}

// perAttachment compiles a field of a Policy some Route's chain runs once
// per site of its attachments, and reports each distinct issue once, with
// each attaching PolicyRef whose site yields it as a related location in
// canonical order (configuration model, "Diagnostics and source map").
func (j *celStage) perAttachment(w *worker, out *celResult, f field, groups []siteGroup, place expr.PlaceID, t v1alpha1.PolicyType) {
	type found struct {
		is   expr.Issue
		refs []attachment
	}
	var all []found
	index := map[issueKey]int{}
	result := entryResult(f.r, place, f.path)
	for _, g := range groups {
		site := g.site(place, t, result)
		for _, is := range j.check(w, site, f.node.Text) {
			k := keyOf(is)
			if at, ok := index[k]; ok {
				all[at].refs = append(all[at].refs, g.refs...)
				continue
			}
			index[k] = len(all)
			all = append(all, found{is: is, refs: slices.Clone(g.refs)})
		}
		out.uses = append(out.uses, f.use(site))
	}
	for _, fd := range all {
		slices.SortFunc(fd.refs, compareAttachments)
		fd.refs = slices.CompactFunc(fd.refs, func(a, b attachment) bool { return compareAttachments(a, b) == 0 })
		related := make([]diag.Related, len(fd.refs))
		for i, a := range fd.refs {
			related[i] = diag.Related{Location: a.ref, Message: relatedAttached}
		}
		out.ds = append(out.ds, j.diagnostic(f, fd.is, related))
	}
}

// everyScope compiles a field of a Policy no Route's chain runs at every
// scope its type allows, and reports only the issues every one of those
// sites raises, so an expression some attachment would accept is never
// refused (configuration model, "Variables"). Such a field never runs, so
// it is not listed in hub.Validated.CEL.
func (j *celStage) everyScope(w *worker, out *celResult, f field, groups []siteGroup, place expr.PlaceID, t v1alpha1.PolicyType) {
	var common []expr.Issue
	result := entryResult(f.r, place, f.path)
	for i, g := range groups {
		iss := j.check(w, g.site(place, t, result), f.node.Text)
		if i == 0 {
			seen := map[issueKey]bool{}
			for _, is := range iss {
				if k := keyOf(is); !seen[k] {
					seen[k] = true
					common = append(common, is)
				}
			}
			continue
		}
		raised := make(map[issueKey]bool, len(iss))
		for _, is := range iss {
			raised[keyOf(is)] = true
		}
		common = slices.DeleteFunc(common, func(is expr.Issue) bool { return !raised[keyOf(is)] })
		if len(common) == 0 {
			return
		}
	}
	for _, is := range common {
		out.ds = append(out.ds, j.diagnostic(f, is, nil))
	}
}

// groups returns the site groups of Policy r. attached is true when some
// Route's effective chain runs it: one group per distinct site of its
// attachments, in the canonical order of their first PolicyRef. Otherwise
// there is one group per scope its type allows, with the Phases it would
// run in there (its declared Phases when its config subscribes it to
// none), and a single group without a scope when no scope allows it, as
// for a plugin Policy whose Plugin's Phases fit no leg.
func (j *celStage) groups(r *hub.Resource, p *v1alpha1.Policy) (gs []siteGroup, attached bool, err error) {
	add := func(scope phase.Scope, phases phase.Set, ref *attachment) {
		first, _ := phases.First()
		gb := j.sites.gatesBody(r, p, scope, phases)
		for i := range gs {
			if gs[i].scope == scope && gs[i].first == first && gs[i].gatesBody == gb {
				if ref != nil {
					gs[i].refs = append(gs[i].refs, *ref)
				}
				return
			}
		}
		g := siteGroup{scope: scope, first: first, gatesBody: gb}
		if ref != nil {
			g.refs = []attachment{*ref}
		}
		gs = append(gs, g)
	}
	if as := j.attached[p.Metadata.Name]; len(as) > 0 {
		for i := range as {
			add(as[i].scope, as[i].phases, &as[i])
		}
		return gs, true, nil
	}
	e, ok := j.p.reg.Lookup(p.Spec.Type)
	if !ok {
		return nil, false, fmt.Errorf("pipeline: Policy %s: %w %q", p.Metadata.Name, registry.ErrUnknownType, p.Spec.Type)
	}
	var pluginPhases []v1alpha1.Phase
	if p.Spec.Type == v1alpha1.PolicyTypePlugin {
		if pl, ok := j.b.Plugin(p.Spec.Plugin); ok {
			pluginPhases = pl.Spec.Phases
		}
	}
	for _, scope := range []phase.Scope{phase.ScopeGateway, phase.ScopeRoute, phase.ScopeUpstream} {
		if !e.Scopes.Has(scope) {
			continue
		}
		phases, err := j.p.reg.Phases(registry.Attachment{Policy: p, Config: r.Config, Scope: scope, PluginPhases: pluginPhases}, j.p.cel)
		var se *registry.ScopeError
		switch {
		case errors.As(err, &se):
			continue
		case err != nil:
			return nil, false, fmt.Errorf("pipeline: Policy %s at %s scope: %w", p.Metadata.Name, scope, err)
		}
		if phases.Empty() {
			phases = e.Phases
			if scope == phase.ScopeUpstream {
				phases = e.UpstreamPhases
			}
		}
		add(scope, phases, nil)
	}
	if len(gs) == 0 {
		gs = []siteGroup{{}}
	}
	return gs, false, nil
}

// check compiles src at site, once per run for each distinct site and
// source, and yields the worker after the compile.
func (j *celStage) check(w *worker, site expr.Site, src string) []expr.Issue {
	key := siteKey(site) + src
	if v, ok := j.cache.Load(key); ok {
		iss, _ := v.([]expr.Issue)
		return iss
	}
	iss := j.p.cel.Check(site, src)
	j.cache.Store(key, iss)
	w.yield()
	return iss
}

// diagnostic returns the finding of issue is of field f. An issue with a
// position in the expression sits at the file position of its character
// when the field is a plain scalar or a quoted scalar on one line
// (scalarPos); otherwise at the field, and its message then ends with
// "at L:C", the line and column within the expression. An issue without a
// position, such as the cost ceiling, sits at the field.
func (j *celStage) diagnostic(f field, is expr.Issue, related []diag.Related) diag.Diagnostic {
	d := diag.Diagnostic{
		Code: cmp.Or(is.Code, CodeCEL), Severity: diag.SeverityError, Location: f.loc,
		Resource: f.r.ResourceID(), Path: f.path, Message: is.Message, Hint: is.Hint, Related: related,
	}
	if is.Offset < 0 {
		return d
	}
	if j.text != nil {
		if text, ok := j.text(f.node.Pos.File); ok {
			if pos, ok := scalarPos(f.node, is.Offset, text); ok {
				d.Location = j.location(f.r, pos)
				return d
			}
		}
	}
	if is.Line > 0 {
		d.Message += " at " + strconv.Itoa(is.Line) + ":" + strconv.Itoa(is.Column)
	}
	return d
}

// location converts pos, a position in r's tree, to a location: through
// the run's FileTable, else with r's own file name when pos lies in the
// file of r's root, otherwise, and for an unknown position, r's start.
func (j *celStage) location(r *hub.Resource, pos tree.Pos) diag.Location {
	return locate(j.files, r, pos)
}

// locate is celStage.location for any stage of this package.
func locate(files *tree.FileTable, r *hub.Resource, pos tree.Pos) diag.Location {
	if !pos.Known() {
		return r.Source.Start
	}
	if files != nil {
		if loc := files.Location(pos); loc.File != "" {
			return loc
		}
	}
	if r.Tree != nil && pos.File == r.Tree.Pos.File && r.Source.File != "" {
		return diag.Location{File: r.Source.File, Line: int(pos.Line), Column: int(pos.Column)}
	}
	return r.Source.Start
}
