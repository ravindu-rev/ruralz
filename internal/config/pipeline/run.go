// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Request names one run: a Bundle and the Environment it renders for.
type Request struct {
	// Source is the Bundle: a directory rooted at ruralz.yaml, or one file
	// holding a one-file Bundle such as a rendered Bundle.
	Source loader.Source
	// Environments is the --environments file; the zero Source is none.
	Environments loader.Source
	// Environment is the Environment to render (--env): its spec.overlay
	// and spec.variables, never Variables. It needs Environments. ""
	// renders without an overlay, with Variables.
	Environment string
	// Variables supplies ${VAR} values when Environment is "": the process
	// environment of the CLI or of file-mode ruralzd, read by the binary's
	// entry point (subst.Environ). Nil defines no variable.
	Variables subst.Source
	// Render keeps the resources as stage E leaves them, for the rendered
	// Bundle (Result.Rendered, WriteRendered); without it a run holds no
	// copy of them.
	Render bool
	// Online runs the online stage after the digest (Online): ruralz
	// bundle build unless --offline, ruralz bundle validate --online.
	Online bool
}

// Result is one run of the pipeline.
type Result struct {
	// Files is the file table every source position of the run refers to.
	Files *tree.FileTable
	// Environment names the Environment rendered; "" without one.
	Environment string
	// Overlay names the overlay applied; "" when none was.
	Overlay string
	// Diagnostics are every finding of the run in diag.List.Sort order: at
	// most Limits.MaxDiagnostics, then one RZ-CFG-001 "too many
	// diagnostics" when there were more, which stops the run.
	Diagnostics diag.List
	// Stopped reports a run that ended before its last stage: at a limit
	// (RZ-CFG-001), the diagnostics cap, the substitution output limit or
	// an Environment that cannot be used.
	Stopped bool
	// Validated is the pipeline's output when stages A to M found no error,
	// warnings aside: the hub Bundle, every Route's effective Filter
	// Chain, the Revision, every secretRef use and every CEL expression
	// with its site. It is nil otherwise, as an invalid Bundle has no
	// Revision. Errors of the online stage, which follows the digest,
	// leave it set.
	Validated *hub.Validated
	// Rendered holds, for Request.Render, the resources of a run with
	// Validated as stage E left them: the overlay merged and ${VAR}
	// substituted, nothing defaulted or normalized. WriteRendered writes
	// them as the rendered Bundle.
	Rendered []*tree.Resource
}

// HasErrors reports an error among the diagnostics.
func (r *Result) HasErrors() bool { return r.Diagnostics.HasErrors() }

// Run runs stages A to M on one Bundle for one Environment
// (docs/architecture/02-configuration-model.md, Figure 3), and the online
// stage when the request asks for it. Each stage runs on what passed the
// stages before it: stage G on the resources stage F accepted and stages H
// to K on the Bundle of the resources stage G built, leaving out a finding
// that only restates a failure of stage F or G; a limit, the diagnostics
// cap, the substitution output limit or an Environment that cannot be used
// ends the run there. Stages L and M run only when no stage found an
// error, so every error of the Bundle comes back in one run.
//
// The error is loader.ErrOptions for an Environment without an
// environments file or the reverse, loader.ErrSource for a source that
// cannot be read (the CLI exits 2), ctx's, or a fault of the release such
// as ErrPlace; a panic in a stage reaches the caller on its goroutine,
// once every goroutine of the run has stopped.
func (p *Pipeline) Run(ctx context.Context, req Request) (*Result, error) {
	lres, err := p.loader.Load(ctx, loader.Request{
		Source: req.Source, Environments: req.Environments, Environment: req.Environment, Variables: req.Variables,
	})
	if err != nil {
		return nil, err
	}
	return p.afterLoad(ctx, lres, nil, input{render: req.Render, online: req.Online})
}

// input is what the stages after the load need beside the resources.
type input struct {
	// resources are the trees stage F validates; files and text locate
	// their positions.
	resources []*tree.Resource
	files     *tree.FileTable
	text      func(tree.FileID) ([]byte, bool)
	render    bool
	online    bool
	// loadFailed holds the resources an error of stages A to E names,
	// which stage F does not validate.
	loadFailed map[tree.ID]bool
	// canonical, for canonical re-entry, is the content the resources were
	// decoded from: the recomputed bytes must equal it (RZ-CFG-027), at
	// positions counted in canonicalText.
	canonical     []byte
	canonicalText []byte
	isCanonical   bool
}

// afterLoad turns the loader's result into a run's: the findings of
// stages A to E, less shared ones a multi-Environment run reports once,
// then stages F to M on the resources unless the load stopped.
func (p *Pipeline) afterLoad(ctx context.Context, lres *loader.Result, shared diag.List, in input) (*Result, error) {
	res := &Result{Files: lres.Files, Environment: lres.Environment, Overlay: lres.Overlay}
	ds := lres.Diagnostics
	if shared != nil {
		ds = subtract(ds, shared)
	}
	if lres.Stopped {
		// The loader's list already holds its cap and RZ-CFG-001.
		res.Diagnostics, res.Stopped = ds, true
		return res, nil
	}
	keep := newKeeper(p.limits.MaxDiagnostics)
	keep.add(stageLoad, 0, ds)
	in.resources, in.files, in.text = lres.Resources, lres.Files, lres.Text
	in.loadFailed = loadFailures(lres.Diagnostics)
	return res, p.finish(ctx, res, keep, in)
}

// loadFailures returns the resources that failed stages A to E: those an
// error of the load names. Stage F runs on what passed the stages before
// it, so a value an error left unsubstituted, or a $patch a base document
// may not hold, is reported once, by the stage that found it. Two errors
// name a resource that stays sound: RZ-CFG-008 names the identity of a
// later declaration the load dropped, whose first declaration stays, and
// RZ-CFG-016 a Gateway the Bundle holds in the wrong place or one too
// many times.
func loadFailures(ds diag.List) map[tree.ID]bool {
	var out map[tree.ID]bool
	for _, d := range ds {
		if d.Severity == diag.SeverityWarning || d.Resource == nil || d.Code == codeDuplicate || d.Code == codeGateway {
			continue
		}
		if out == nil {
			out = map[tree.ID]bool{}
		}
		out[tree.ID{Kind: v1alpha1.Kind(d.Resource.Kind), Name: d.Resource.Name}] = true
	}
	return out
}

// Codes of the load that name a resource without failing it.
const (
	// codeDuplicate is RZ-CFG-008: a duplicate identity.
	codeDuplicate = "RZ-CFG-008"
	// codeGateway is RZ-CFG-016: not exactly one Gateway, or a Gateway
	// outside ruralz.yaml.
	codeGateway = "RZ-CFG-016"
)

// finish runs stages F to M, and the online stage, on in.resources,
// offering every finding to keep, and fills res.
func (p *Pipeline) finish(ctx context.Context, res *Result, keep *keeper, in input) error {
	rs := in.resources

	// Stage F: the rendered JSON Schema of each resource's apiVersion, on
	// the resources that passed stages A to E.
	found := make([]diag.List, len(rs))
	served := make([]bool, len(rs))
	if err := p.parallel(ctx, len(rs), func(w *worker, i int) {
		if rs[i] == nil || in.loadFailed[rs[i].ID] {
			return
		}
		found[i], served[i] = p.views.ValidateWith(rs[i], in.files, schemaview.Options{MaxDiagnostics: p.limits.MaxDiagnostics, Yield: w.yield})
	}); err != nil {
		return err
	}
	var failed failures
	accepted := make([]*tree.Resource, 0, len(rs))
	for i, r := range rs {
		keep.add(stageF, i, found[i])
		// A resource without a schema view had RZ-CFG-007 at stage C.
		if r == nil || !served[i] || found[i].HasErrors() {
			if r != nil {
				failed.add(r)
			}
			continue
		}
		accepted = append(accepted, r)
	}
	if keep.full() {
		res.stop(keep)
		return nil
	}
	// Stage G rewrites the trees in place: the rendered Bundle keeps the
	// trees stage E left, which only a run without error writes.
	var rendered []*tree.Resource
	if in.render && !keep.hasErrors() {
		rendered = cloneResources(rs)
	}

	// Stage G: defaults, hub conversion, normalization and typed decode of
	// the resources stage F accepted.
	built := make([]*hub.Resource, len(accepted))
	found = make([]diag.List, len(accepted))
	if err := p.parallel(ctx, len(accepted), func(_ *worker, i int) {
		built[i], found[i] = p.defaults.Resource(accepted[i], in.files)
	}); err != nil {
		return err
	}
	ok := make([]*hub.Resource, 0, len(built))
	for i, h := range built {
		keep.add(stageG, i, found[i])
		if h == nil {
			failed.add(accepted[i])
			continue
		}
		ok = append(ok, h)
	}
	if keep.full() {
		res.stop(keep)
		return nil
	}
	b := hub.NewBundle(ok)

	// Stage H: references, cross-resource rules, secret destinations and
	// the injected Policy checks.
	vres, hds, err := p.validator.Run(ctx, b, in.files, p.workers)
	if err != nil {
		return err
	}
	keep.add(stageH, 0, failed.filter(hds, b, p.hub))
	if keep.full() {
		res.stop(keep)
		return nil
	}

	// Stage I: effective Filter Chains, slots and guardrails.
	chains, ids, err := p.resolver.Run(ctx, b, in.files, p.workers)
	if err != nil {
		return err
	}
	keep.add(stageI, 0, ids)
	if keep.full() {
		res.stop(keep)
		return nil
	}

	// Stage J: CEL compile and cost at every site.
	text := in.text
	if in.isCanonical {
		text = canonicalText(rs, in.canonicalText)
	}
	uses, jds, err := p.compileCEL(ctx, b, chains, in.files, text)
	if err != nil {
		return err
	}
	for i, ds := range jds {
		keep.add(stageJ, i, ds)
	}
	if keep.full() {
		res.stop(keep)
		return nil
	}

	// Stage K: effective-chain rules.
	keep.add(stageK, 0, chainRules(b, chains, in.files))
	if keep.full() {
		res.stop(keep)
		return nil
	}
	if keep.hasErrors() {
		res.Diagnostics = keep.result()
		return nil
	}

	// Stages L and M: the ruralz.canonical.v1 content and its digest.
	rev, err := p.encoder.Encode(ctx, b, canonical.Options{Level: p.hub.Level(), Workers: p.workers, Now: p.now, YieldAfter: p.yieldAfter})
	if err != nil {
		return fmt.Errorf("pipeline: stage L: %w", err)
	}
	if in.isCanonical {
		if d, differs := recomputed(rev.Content, in.canonical, in.canonicalText, rs, in.files); differs {
			keep.add(stageL, 0, diag.List{d})
			res.Diagnostics = keep.result()
			return nil
		}
	}
	res.Validated = &hub.Validated{Bundle: b, Chains: chains, Revision: rev, Secrets: vres.Secrets, CEL: uses}
	res.Rendered = rendered
	if in.online {
		ds, err := p.Online(ctx, res.Validated)
		if err != nil {
			return err
		}
		keep.add(stageOnline, 0, ds)
	}
	res.Diagnostics = keep.result()
	return nil
}

// stop ends a run at the diagnostics cap: the kept findings and their one
// RZ-CFG-001, and no later stage.
func (r *Result) stop(keep *keeper) {
	r.Diagnostics, r.Stopped = keep.result(), true
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

// subtract returns the diagnostics of all that shared does not hold,
// counting duplicates: a Render's list repeats the findings of stages A to
// C, which a multi-Environment run reports once.
func subtract(all, shared diag.List) diag.List {
	if len(shared) == 0 {
		return all
	}
	count := make(map[string]int, len(shared))
	for _, d := range shared {
		count[string(d.AppendText(nil))]++
	}
	out := make(diag.List, 0, len(all))
	for _, d := range all {
		k := string(d.AppendText(nil))
		if count[k] > 0 {
			count[k]--
			continue
		}
		out = append(out, d)
	}
	return out
}
