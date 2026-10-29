// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/overlay"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Result is a rendered Bundle: the resources stage F validates.
type Result struct {
	// Files is the file table every position of Resources refers to.
	Files *tree.FileTable
	// Resources is the base union with the overlay merged and ${VAR}
	// substituted: the root file's resources, then the other base files'
	// in path order, then the resources the overlay adds, in kind and name
	// order. It holds what passed stages A to E; it is nil when the load
	// stopped.
	Resources []*tree.Resource
	// Diagnostics are the findings of the run, sorted: at most
	// Limits.MaxDiagnostics, then one RZ-CFG-001 "too many diagnostics"
	// when there were more, as a diag.Collector of that cap holds them.
	Diagnostics diag.List
	// Stopped reports a load that did not reach the end of stage E: a
	// limit, the diagnostics cap, the substitution output limit or an
	// Environment that cannot be used.
	Stopped bool
	// Environment names the Environment rendered; "" without one.
	Environment string
	// Overlay names the overlay applied; "" when none was.
	Overlay string
}

// HasErrors reports an error among the diagnostics: such a load yields no
// Revision.
func (r *Result) HasErrors() bool { return r.Diagnostics.HasErrors() }

// Request names one load: a Bundle and the Environment it renders for.
type Request struct {
	// Source is the Bundle.
	Source Source
	// Environments is the --environments file; the zero Source is none.
	Environments Source
	// Environment is the Environment to render (--env); it needs
	// Environments. "" renders without an overlay, with Variables.
	Environment string
	// Variables supplies ${VAR} values when Environment is "": the process
	// environment (subst.Environ) of the CLI or of file-mode ruralzd. It is
	// never read for an Environment. Nil defines no variable.
	Variables subst.Source
}

// Load runs stages A to E for one Environment: it reads the environments
// file and selects the Environment when the request names one, parses the
// Bundle and renders it. The diagnostics hold the environments file's
// findings too. An Environment that cannot be used (RZ-CFG-009, or one
// that failed its checks) stops the load after the Bundle's stages A to C.
// The error is ErrOptions for an Environment without an environments file
// or the reverse, ErrSource for a source that cannot be read, or ctx's.
func (l *Loader) Load(ctx context.Context, req Request) (*Result, error) {
	switch {
	case req.Environment != "" && req.Environments.IsZero():
		return nil, fmt.Errorf("%w: Environment %s needs an environments file; reading Environments from Ruralz Control is Planned (M2)", ErrOptions, strconv.Quote(req.Environment))
	case req.Environment == "" && !req.Environments.IsZero():
		return nil, fmt.Errorf("%w: an environments file needs an Environment name: load once per Environment", ErrOptions)
	}
	var (
		env   *Environment
		extra *keeper
	)
	if req.Environment != "" {
		envs, err := l.ReadEnvironments(ctx, req.Environments)
		if err != nil {
			return nil, err
		}
		extra = envs.keep.clone()
		var ds diag.List
		env, ds = envs.Select(req.Environment)
		extra.add(stepSelect, 0, ds)
	}
	b, err := l.Parse(ctx, req.Source)
	if err != nil {
		return nil, err
	}
	if req.Environment != "" && env == nil {
		keep := b.keep.clone()
		keep.merge(extra)
		return &Result{Files: b.files, Environment: req.Environment, Diagnostics: keep.result(), Stopped: true}, nil
	}
	return b.render(ctx, env, req.Variables, extra, true)
}

// Render runs stages D and E on a copy of the base union. With env, it
// applies the overlay env selects (none for a one-file Bundle) and
// substitutes env's variables, never vars; without env, it applies no
// overlay and substitutes vars. The Bundle is not changed, so renders for
// several Environments may run concurrently. The diagnostics hold the
// Bundle's own. The error is ctx's.
func (b *Bundle) Render(ctx context.Context, env *Environment, vars subst.Source) (*Result, error) {
	return b.render(ctx, env, vars, nil, false)
}

// render is Render; extra holds findings to report beside the Bundle's,
// and inPlace renders the Bundle's own base trees, which only Load may do.
func (b *Bundle) render(ctx context.Context, env *Environment, vars subst.Source, extra *keeper, inPlace bool) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keep := b.keep.clone()
	if extra != nil {
		keep.merge(extra)
	}
	res := &Result{Files: b.files}
	if env != nil {
		res.Environment = env.Name
	}
	if b.stopped || keep.full() {
		return res.stop(keep), nil
	}

	set := b.selectOverlay(env, res, keep)
	var patches []*tree.Resource
	if set != nil {
		keep.add(stepOverlayParse, -1, set.findings)
		// The overlay's files have what the base files left of the load's
		// node budget.
		parsed, _, over, err := b.l.parse(ctx, set.sources, keep, stepOverlayParse, b.l.limits.MaxNodes()-b.nodes, b.l.nodeMessage(bundleFiles))
		if err != nil {
			return nil, err
		}
		var ok bool
		if over {
			return res.stop(keep), nil
		}
		if patches, ok = b.overlayDocs(parsed, keep); !ok {
			return res.stop(keep), nil
		}
	}
	base := b.base
	if !inPlace {
		base = cloneResources(b.base)
	}
	removal := b.gatewayRemoval(patches)
	w := b.l.newWorker()
	merged, ds, err := overlay.Apply(ctx, base, patches, overlay.Options{
		Schemas: b.l.schemas, Files: b.files, MaxDiagnostics: b.l.limits.MaxDiagnostics, Yield: w.yield,
	})
	if err != nil {
		return nil, err
	}
	keep.add(stepApply, 0, ds)
	merged, ok := b.afterMerge(merged, removal, keep)
	if !ok || keep.full() {
		return res.stop(keep), nil
	}
	if spent, err := b.substitute(ctx, merged, env, vars, keep); err != nil {
		return nil, err
	} else if spent || keep.full() {
		return res.stop(keep), nil
	}
	res.Resources = merged
	res.Diagnostics = keep.result()
	return res, nil
}

// stop finishes a render that stopped: no resources.
func (r *Result) stop(keep *keeper) *Result {
	r.Resources, r.Stopped, r.Diagnostics = nil, true, keep.result()
	return r
}

// selectOverlay returns the overlay env selects. A selected directory that
// does not exist is no overlay, and RZ-CFG-009 when spec.overlay names it
// (overlay.Selection.Absent). A one-file Bundle has no overlays.
func (b *Bundle) selectOverlay(env *Environment, res *Result, keep *keeper) *overlaySet {
	if env == nil || b.oneFile {
		return nil
	}
	if set, ok := b.overlays[env.Overlay]; ok && overlay.ValidName(env.Overlay) {
		res.Overlay = env.Overlay
		return set
	}
	sel := overlay.Selection{Name: env.Overlay, Explicit: env.OverlayExplicit, Environment: env.Name, Location: env.OverlayLocation}
	keep.add(stepOverlayParse, -2, sel.Absent(b.overlayNames))
	return nil
}

// overlayDocs runs the envelope checks of stage C on the documents of the
// selected overlay and returns those that pass: Apply reports duplicates.
// ok is false when the overlay passes the resource limit, which stops the
// load.
func (b *Bundle) overlayDocs(parsed []parsed, keep *keeper) (patches []*tree.Resource, ok bool) {
	item := 0
	for _, p := range parsed {
		for _, doc := range p.docs {
			item++
			r, _, ds := b.l.envelope(doc, b.files)
			if len(ds) == 0 {
				if d, ok := controlKind(r, b.files); ok {
					ds = diag.List{d}
				}
			}
			if len(ds) > 0 {
				keep.add(stepOverlayUnion, item, ds)
				continue
			}
			if len(patches) == b.l.limits.MaxResources {
				keep.add(stepOverlayUnion, item, diag.List{{
					Code: CodeLimit, Severity: diag.SeverityError, Location: b.files.Location(r.Start),
					Message: "the overlay holds more than " + strconv.Itoa(b.l.limits.MaxResources) + " documents",
				}})
				return nil, false
			}
			patches = append(patches, r)
		}
	}
	return patches, true
}

// gatewayRemoval returns where the overlay deletes the Gateway, read
// before Apply rewrites the patches: the first patch of the Gateway's
// identity that carries ruralz.io/patch: delete.
func (b *Bundle) gatewayRemoval(patches []*tree.Resource) diag.Location {
	if !b.hasGateway {
		return diag.Location{}
	}
	for _, p := range patches {
		if p.ID != b.gateway {
			continue
		}
		md, _ := p.Root.Get("metadata")
		ann, _ := md.Get("annotations")
		if v, ok := ann.Get(overlay.PatchAnnotation); ok && v != nil && v.Kind == tree.KindString && v.Text == overlay.PatchDelete {
			return b.files.Location(v.Pos)
		}
	}
	return diag.Location{File: b.root}
}

// afterMerge checks the merged Bundle. It withholds an overlay addition
// whose identity may belong to a base document stage A to C lost, since
// it would only repeat that finding; an overlay can neither add nor
// remove the Gateway (RZ-CFG-016); and the rendered Bundle stays within
// the resource limit, past which the load stops (ok false).
func (b *Bundle) afterMerge(merged []*tree.Resource, removal diag.Location, keep *keeper) (out []*tree.Resource, ok bool) {
	out = make([]*tree.Resource, 0, len(merged))
	present := false
	for i, r := range merged {
		added := !b.baseIDs[r.ID]
		if added && (b.tainted || b.lost[r.ID]) {
			continue
		}
		if r.ID.Kind == v1alpha1.KindGateway {
			if added {
				keep.add(stepMerged, i, diag.List{b.gatewayOutside(r, "an overlay cannot add a Gateway: "+b.root+" declares the Bundle's only Gateway")})
			} else if b.hasGateway && r.ID == b.gateway {
				present = true
			}
		}
		out = append(out, r)
	}
	if b.hasGateway && !present {
		keep.add(stepMerged, len(merged), diag.List{{
			Code: CodeGateway, Severity: diag.SeverityError, Location: removal,
			Resource: resourceID(b.gateway),
			Message:  "an overlay cannot remove the Gateway: a Bundle has exactly one",
		}})
	}
	if len(out) > b.l.limits.MaxResources {
		keep.add(stepMerged, len(merged)+1, diag.List{{
			Code: CodeLimit, Severity: diag.SeverityError, Location: b.files.Location(out[b.l.limits.MaxResources].Start),
			Message: "the rendered Bundle holds more than " + strconv.Itoa(b.l.limits.MaxResources) + " resources",
		}})
		return nil, false
	}
	return out, true
}

// substitute runs stage E on rs in the worker pool. Once the output limit
// is spent, the render has failed: its one RZ-CFG-001 replaces stage E's
// other findings, and spent is true.
func (b *Bundle) substitute(ctx context.Context, rs []*tree.Resource, env *Environment, vars subst.Source, keep *keeper) (spent bool, err error) {
	opts := subst.Options{
		Schemas: b.l.schemas, Files: b.files, Variables: vars,
		MaxDiagnostics: b.l.limits.MaxDiagnostics, MaxOutputBytes: b.l.limits.MaxSourceBytes,
	}
	if env != nil {
		opts.Variables, opts.HintNames = env.Variables, env.variableNames()
	}
	s := subst.New(opts)
	stage := newKeeper(b.l.limits.MaxDiagnostics)
	if err := b.l.parallel(ctx, len(rs), func(w *worker, i int) {
		stage.add(stepSubstitute, i, s.ApplyWith(rs[i], w.yield))
	}); err != nil {
		return false, err
	}
	if s.Spent() {
		keep.add(stepSubstitute, 0, diag.List{s.LimitDiagnostic()})
		return true, nil
	}
	keep.merge(stage)
	return false, nil
}

// cloneResources returns deep copies of rs.
func cloneResources(rs []*tree.Resource) []*tree.Resource {
	out := make([]*tree.Resource, len(rs))
	for i, r := range rs {
		c := *r
		c.Root = r.Root.Clone()
		out[i] = &c
	}
	return out
}
