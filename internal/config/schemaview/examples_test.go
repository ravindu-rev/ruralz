// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/defaults"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/overlay"
	"github.com/ravindu-rev/ruralz/internal/config/precedence"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Codes the Bundle check reports for the rules of stages C and H that it
// applies itself (docs/architecture/02-configuration-model.md "Error
// codes"); every other code comes from the stage package that raises it.
const (
	codeUnserved     = "RZ-CFG-007"
	codeDuplicate    = "RZ-CFG-008"
	codeMissing      = "RZ-CFG-009"
	codeGateway      = "RZ-CFG-016"
	codeControlPlane = "RZ-CFG-017"
	codePromotion    = "RZ-CFG-022"
)

// bundleRoot is the file that marks a Bundle root and holds its Gateway
// (CM "Bundle layout and merge").
const bundleRoot = "ruralz.yaml"

// exampleWorkers is the worker count of the stages that take one; their
// results are the same for every count.
const exampleWorkers = 4

// stages are the configuration stages that exist as packages, built once
// and shared by every Bundle the examples tests load: B (profile),
// D (overlay), E (subst), F (this package), G (defaults), I (precedence),
// J's compiler (cel) and L and M (canonical), with the Policy type
// registry they share.
type stages struct {
	indexes  *schemaidx.Set
	views    *Set
	registry *registry.Registry
	defaults *defaults.Stage
	resolver *precedence.Resolver
	cel      *cel.Compiler
	encoder  *canonical.Encoder
}

// hubIndex returns the schema index of the hub representation.
func (st *stages) hubIndex() *schemaidx.Index { return st.defaults.HubIndex() }

// exampleStages builds the stages once for all tests.
var exampleStages = sync.OnceValues(func() (*stages, error) {
	indexes, err := schemaidx.Embedded()
	if err != nil {
		return nil, err
	}
	views, err := Embedded(indexes)
	if err != nil {
		return nil, err
	}
	reg := registry.New()
	g, err := defaults.New(defaults.Options{Schemas: indexes, Registry: reg, Converters: convert.Default()})
	if err != nil {
		return nil, err
	}
	compiler, err := cel.New()
	if err != nil {
		return nil, err
	}
	// The CEL compiler is the body oracle every binary passes to stage I.
	resolver, err := precedence.New(reg, precedence.Options{Body: compiler})
	if err != nil {
		return nil, err
	}
	return &stages{
		indexes:  indexes,
		views:    views,
		registry: reg,
		defaults: g,
		resolver: resolver,
		cel:      compiler,
		encoder:  canonical.NewEncoder(g.HubIndex()),
	}, nil
})

// mustStages returns the shared stages, failing t when they do not build.
func mustStages(t testing.TB) *stages {
	t.Helper()
	st, err := exampleStages()
	if err != nil {
		t.Fatalf("build the configuration stages: %v", err)
	}
	return st
}

// render is one load of a Bundle: without --env, with no overlay and the
// variables of the process environment, or for one Environment, with its
// overlay and its spec.variables (CM "Environment substitution").
type render struct {
	// env is the Environment name; "" renders without --env.
	env string
	// overlay is the selected overlay; its Name is "" without --env.
	overlay overlay.Selection
	// vars are the substitution variables.
	vars subst.Map
	// hints are the names an RZ-CFG-010 hint may suggest: the
	// Environment's variable names, none for the process environment.
	hints []string
}

// loaded is one render of a Bundle through the stages.
type loaded struct {
	files  *tree.FileTable
	bundle *hub.Bundle
	chains map[string]*hub.Chain
	rev    revision.Revision
}

// configFiles walks dir of fsys and returns the slash-separated paths,
// relative to dir and sorted bytewise, of the files the loader reads there:
// .yaml, .yml and .json names, case-sensitive (profile.FormatOf), no hidden
// path segment, and none that rules ignores, ruralz.yaml excepted. A nil
// rules ignores nothing.
func configFiles(fsys fs.FS, dir string, rules ignoreRules) ([]string, error) {
	var out []string
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
		if d.IsDir() || rel == "" {
			return nil
		}
		if _, ok := profile.FormatOf(d.Name()); !ok {
			return nil
		}
		if rel != bundleRoot && rules.ignored(rel) {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	slices.Sort(out)
	return out, err
}

// bundleFiles returns the files of the Bundle rooted at dir of fsys
// (CM "Discovery and base merge"): base, the base files in lexical byte
// order of their Bundle-relative paths, without overlays/, hidden paths and
// what .ruralzignore matches, ruralz.yaml always included; and overlays,
// the files of each overlays/<name>/ directory, discovered the same way.
func bundleFiles(fsys fs.FS, dir string) (base []string, overlays map[string][]string, err error) {
	var rules ignoreRules
	if data, err := fs.ReadFile(fsys, path.Join(dir, ".ruralzignore")); err == nil {
		if rules, err = parseIgnore(string(data)); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path.Join(dir, ".ruralzignore"), err)
		}
	}
	all, err := configFiles(fsys, dir, rules)
	if err != nil {
		return nil, nil, err
	}
	overlays = map[string][]string{}
	for _, rel := range all {
		rest, ok := strings.CutPrefix(rel, overlay.Root)
		if !ok {
			base = append(base, rel)
			continue
		}
		if name, _, nested := strings.Cut(rest, "/"); nested {
			overlays[name] = append(overlays[name], rel)
		}
	}
	return base, overlays, nil
}

// parseFiles parses each file of names (Bundle-relative paths under dir of
// fsys) with the restricted profile into resources, recording the files in
// files with role, and returns the stage B findings.
func parseFiles(ctx context.Context, fsys fs.FS, dir string, names []string, role tree.Role, files *tree.FileTable) ([]*tree.Resource, diag.List, error) {
	var rs []*tree.Resource
	var ds diag.List
	for _, name := range names {
		data, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}
		format, _ := profile.FormatOf(name)
		id := files.Add(tree.File{Path: name, Role: role})
		docs, found, err := profile.Parse(ctx, data, id, name, format, profile.Options{})
		if err != nil {
			return nil, nil, err
		}
		ds = append(ds, found...)
		for _, doc := range docs {
			res := resourceOf(doc.Root)
			res.Start = doc.Start
			rs = append(rs, res)
		}
	}
	return rs, ds, nil
}

// envelopeRules applies stage C's envelope rules to the resources of one file
// set (CM "Discovery and base merge"): an apiVersion that is not served or
// a kind its schema does not dispatch is RZ-CFG-007, an Environment or a
// Cluster is RZ-CFG-017 unless controlPlane allows them, and a second
// document of one identity is RZ-CFG-008. It returns the resources that
// pass. A document without metadata.name is left to stage F.
func envelopeRules(st *stages, rs []*tree.Resource, files *tree.FileTable, controlPlane bool) ([]*tree.Resource, diag.List) {
	var kept []*tree.Resource
	var ds diag.List
	first := map[tree.ID]*tree.Resource{}
	for _, res := range rs {
		at := files.Location(res.Start)
		idx, ok := st.indexes.Index(res.APIVersion)
		if !ok {
			ds = append(ds, diag.Diagnostic{
				Code: codeUnserved, Severity: diag.SeverityError, Location: at, Resource: res.ID.ResourceID(),
				Message: fmt.Sprintf("apiVersion %q is not served", res.APIVersion),
			})
			continue
		}
		if !slices.Contains(idx.Kinds(), string(res.ID.Kind)) {
			ds = append(ds, diag.Diagnostic{
				Code: codeUnserved, Severity: diag.SeverityError, Location: at, Resource: res.ID.ResourceID(),
				Message: fmt.Sprintf("unknown kind %q", res.ID.Kind),
			})
			continue
		}
		cp := res.ID.Kind == v1alpha1.KindEnvironment || res.ID.Kind == v1alpha1.KindCluster
		if cp != controlPlane {
			msg := fmt.Sprintf("%s is a control-plane kind and never inside a Bundle", res.ID.Kind)
			if controlPlane {
				msg = fmt.Sprintf("%s is a Bundle kind; control/ holds only Environment and Cluster resources", res.ID.Kind)
			}
			ds = append(ds, diag.Diagnostic{Code: codeControlPlane, Severity: diag.SeverityError, Location: at, Resource: res.ID.ResourceID(), Message: msg})
			continue
		}
		if res.ID.Name != "" {
			if prev, dup := first[res.ID]; dup {
				ds = append(ds, diag.Diagnostic{
					Code: codeDuplicate, Severity: diag.SeverityError, Location: at, Resource: res.ID.ResourceID(),
					Message: fmt.Sprintf("duplicate identity %s", res.ID),
					Related: []diag.Related{{Location: files.Location(prev.Start), Message: "first defined at"}},
				})
				continue
			}
			first[res.ID] = res
		}
		kept = append(kept, res)
	}
	return kept, ds
}

// gatewayRule is RZ-CFG-016 (CM "Scope of kinds"): a Bundle has exactly one
// Gateway, in ruralz.yaml.
func gatewayRule(rs []*tree.Resource, files *tree.FileTable) diag.List {
	var ds diag.List
	var gateways []*tree.Resource
	for _, res := range rs {
		if res.ID.Kind != v1alpha1.KindGateway {
			continue
		}
		at := files.Location(res.Start)
		switch {
		case at.File != bundleRoot:
			ds = append(ds, diag.Diagnostic{Code: codeGateway, Severity: diag.SeverityError, Location: at, Resource: res.ID.ResourceID(), Message: "the Gateway must be in " + bundleRoot})
		case len(gateways) > 0:
			ds = append(ds, diag.Diagnostic{
				Code: codeGateway, Severity: diag.SeverityError, Location: at, Resource: res.ID.ResourceID(), Message: "a Bundle has exactly one Gateway",
				Related: []diag.Related{{Location: files.Location(gateways[0].Start), Message: "first Gateway at"}},
			})
		}
		gateways = append(gateways, res)
	}
	if len(gateways) == 0 {
		ds = append(ds, diag.Diagnostic{Code: codeGateway, Severity: diag.SeverityError, Location: diag.Location{File: bundleRoot}, Message: "a Bundle has exactly one Gateway, in " + bundleRoot})
	}
	return ds
}

// loadBundle renders the Bundle rooted at dir of fsys for r through every
// stage that exists as a package, in pipeline order, plus the rules of
// stages C, H and J that the check applies itself (envelopeRules,
// gatewayRule, references, compileCEL): it parses (B), checks envelopes
// (C), merges the selected overlay (D), substitutes (E), validates against
// the rendered view (F), materializes defaults and builds the hub (G),
// resolves references (H), computes the effective Filter Chains (I),
// compiles every CEL expression at its sites (J) and encodes the Revision
// (L, M). Stage G runs only when the stages before it found no error and
// stage L only on a Bundle without errors, as each assumes what the stages
// before it check; every other stage runs, so one render reports every
// finding its stages can reach. Every diagnostic names the render's
// Environment.
func loadBundle(ctx context.Context, st *stages, fsys fs.FS, dir string, r render) (*loaded, diag.List, error) {
	out := &loaded{files: &tree.FileTable{}}
	var ds diag.List
	report := func(l diag.List) bool {
		for _, d := range l {
			d.Environment = r.env
			ds = append(ds, d)
		}
		return l.HasErrors()
	}
	baseNames, overlays, err := bundleFiles(fsys, dir)
	if err != nil {
		return nil, nil, err
	}
	base, found, err := parseFiles(ctx, fsys, dir, baseNames, tree.RoleBase, out.files)
	if err != nil {
		return nil, nil, err
	}
	report(found)
	base, found = envelopeRules(st, base, out.files, false)
	report(found)
	report(gatewayRule(base, out.files))
	opts := overlay.Options{Schemas: st.indexes, Files: out.files}
	for _, res := range base {
		report(overlay.CheckBase(res, opts))
	}
	rs := base
	if r.overlay.Name != "" {
		names, ok := overlays[r.overlay.Name]
		if !ok {
			report(r.overlay.Absent(slices.Sorted(maps.Keys(overlays))))
		}
		patches, found, err := parseFiles(ctx, fsys, dir, names, tree.RoleOverlay, out.files)
		if err != nil {
			return nil, nil, err
		}
		report(found)
		patches, found = envelopeRules(st, patches, out.files, false)
		report(found)
		if rs, found, err = overlay.Apply(ctx, base, patches, opts); err != nil {
			return nil, nil, err
		}
		report(found)
	}
	s := subst.New(subst.Options{Schemas: st.indexes, Files: out.files, Variables: r.vars, HintNames: r.hints})
	for _, res := range rs {
		report(s.Apply(res))
	}
	for _, res := range rs {
		found, _ := st.views.Validate(res, out.files)
		report(found)
	}
	if ds.HasErrors() {
		return out, ds, nil
	}
	if out.bundle, found, err = st.defaults.Run(ctx, rs, out.files, exampleWorkers); err != nil {
		return nil, nil, err
	}
	if report(found) || out.bundle == nil {
		return out, ds, nil
	}
	report(references(st.hubIndex(), out.bundle, out.files))
	if out.chains, found, err = st.resolver.Run(ctx, out.bundle, out.files, exampleWorkers); err != nil {
		return nil, nil, err
	}
	report(found)
	found, err = compileCEL(st, out.bundle, out.chains, out.files)
	if err != nil {
		return nil, nil, err
	}
	report(found)
	if ds.HasErrors() {
		return out, ds, nil
	}
	if out.rev, err = st.encoder.Encode(ctx, out.bundle, canonical.Options{}); err != nil {
		return nil, nil, err
	}
	return out, ds, nil
}

// references applies the reference rule of stage H (CM "Identity and
// references"): every x-ruralz-ref value names a resource of the target
// kind in the rendered Bundle, and every Route spec.listeners entry names
// a listener of the Gateway; a missing target is RZ-CFG-009, hinting the
// nearest name of that kind.
func references(idx *schemaidx.Index, b *hub.Bundle, files *tree.FileTable) diag.List {
	names := map[v1alpha1.Kind][]string{}
	for _, r := range b.Resources() {
		names[r.Kind] = append(names[r.Kind], r.Name)
	}
	var listeners []string
	if gw := b.Gateway(); gw != nil {
		for _, l := range gw.Spec.Listeners {
			listeners = append(listeners, l.Name)
		}
	}
	var ds diag.List
	missing := func(r *hub.Resource, at tree.Pos, p diag.Path, what, name string, candidates []string) {
		d := diag.Diagnostic{
			Code: codeMissing, Severity: diag.SeverityError, Location: files.Location(at), Resource: r.ResourceID(), Path: p,
			Message: fmt.Sprintf("%s %q not found", what, name),
		}
		if near, ok := diag.Nearest(name, candidates); ok {
			d.Hint = fmt.Sprintf("did you mean %q?", near)
		}
		ds = append(ds, d)
	}
	for _, r := range b.Resources() {
		_ = idx.Walk(string(r.Kind), r.Tree, func(c *schemaidx.Cursor) error {
			kind := v1alpha1.Kind(c.Info.Keywords().Ref)
			if kind == "" || c.Node.Kind != tree.KindString {
				return nil
			}
			if _, ok := b.Get(hub.ID{Kind: kind, Name: c.Node.Text}); !ok {
				missing(r, c.Node.Pos, c.Path(), string(kind), c.Node.Text, names[kind])
			}
			return nil
		})
		route, ok := hub.Object[v1alpha1.Route](r)
		if !ok {
			continue
		}
		for _, l := range route.Spec.Listeners {
			if slices.Contains(listeners, l) {
				continue
			}
			p := diag.Path{diag.Field("spec"), diag.Field("listeners"), diag.Item(l)}
			at := r.Tree.Pos
			if n, ok := r.Tree.At(p); ok {
				at = n.Pos
			}
			missing(r, at, p, "Gateway listener", l, listeners)
		}
	}
	return ds
}

// celPlaces maps each x-ruralz-cel field, as "Definition.property", to its
// place (CM "Allowed places"); the headers places name the shared set[]
// definition of each side, which covers its add[] entries too.
func celPlaces() map[string]expr.PlaceID {
	return map[string]expr.PlaceID{
		"RouteMatch.when":                      expr.PlaceRouteMatchWhen,
		"PolicySpec.when":                      expr.PlacePolicyWhen,
		"CompositionStep.pathExpression":       expr.PlaceStepPathExpression,
		"CompositionStep.when":                 expr.PlaceStepWhen,
		"AuthzCELConfig.rule":                  expr.PlaceAuthzCELRule,
		"RateLimitConfig.key":                  expr.PlaceRateLimitKey,
		"RateLimitConfig.cost":                 expr.PlaceRateLimitCost,
		"RateLimit.when":                       expr.PlaceRateLimitWhen,
		"QuotaConfig.key":                      expr.PlaceQuotaKey,
		"QuotaConfig.cost":                     expr.PlaceQuotaCost,
		"HeaderRequestSet.valueExpression":     expr.PlaceHeadersRequestValue,
		"HeaderResponseSet.valueExpression":    expr.PlaceHeadersResponseValue,
		"CacheConfig.key":                      expr.PlaceCacheKey,
		"TransformRequestConfig.body":          expr.PlaceTransformRequestBody,
		"TransformRequestSet.valueExpression":  expr.PlaceTransformRequestValue,
		"TransformResponseConfig.body":         expr.PlaceTransformResponseBody,
		"TransformResponseSet.valueExpression": expr.PlaceTransformResponseValue,
		"LoadBalancing.hashKey":                expr.PlaceHashKey,
		"Retries.retryOn":                      expr.PlaceRetryOn,
		"CircuitBreaker.failureWhen":           expr.PlaceFailureWhen,
		"AccessLog.when":                       expr.PlaceAccessLogWhen,
		"AISemanticCacheConfig.key":            expr.PlaceSemanticCacheKey,
		"AIModelCandidate.when":                expr.PlaceCandidateWhen,
		"Messaging.key":                        expr.PlaceMessagingKey,
	}
}

// attachment is one scope a Policy is attached at, with the Phases it runs
// in there.
type attachment struct {
	scope  phase.Scope
	phases phase.Set
}

// attachments returns the attachments of every Policy that some Route's
// effective chain runs, by Policy name, in scope order.
func attachments(chains map[string]*hub.Chain) map[string][]attachment {
	out := map[string][]attachment{}
	add := func(e hub.Entry) {
		for _, a := range out[e.Policy] {
			if a.scope == e.Scope {
				return
			}
		}
		out[e.Policy] = append(out[e.Policy], attachment{scope: e.Scope, phases: e.Phases})
	}
	for _, name := range slices.Sorted(maps.Keys(chains)) {
		c := chains[name]
		for _, es := range c.Client {
			for _, e := range es {
				add(e)
			}
		}
		for _, l := range c.Legs {
			for _, es := range l.Phases {
				for _, e := range es {
					add(e)
				}
			}
		}
	}
	for _, as := range out {
		slices.SortFunc(as, func(a, b attachment) int { return int(a.scope) - int(b.scope) })
	}
	return out
}

// sites returns the sites of a CEL field of r at place (CM "Variables":
// each occurrence refines its place's variables by attachment). A
// composition field takes its Route's mode and the names of the earlier
// steps; a Policy field takes one site per attachment, or one without a
// scope for a Policy no Route runs, with the Policy type, its first Phase
// there and whether it gates the request body there: a Policy that runs in
// onRequestBody at that attachment, or transform.request, which reads the
// request body at every scope (CM "Allowed places"). Every other field has
// the bare site of its place.
func sites(place expr.PlaceID, r *hub.Resource, p diag.Path, attached map[string][]attachment) []expr.Site {
	if route, ok := hub.Object[v1alpha1.Route](r); ok && route.Spec.Composition != nil &&
		(place == expr.PlaceStepPathExpression || place == expr.PlaceStepWhen) {
		// The path names the step as its one keyed element:
		// spec.composition.steps[name=<step>].<field>.
		step := ""
		for _, e := range p {
			if e.Kind == diag.ElemKeyed {
				step = e.Key
			}
		}
		site := expr.Site{Place: place, Mode: route.Spec.Composition.Mode}
		for _, s := range route.Spec.Composition.Steps {
			if s.Name == step {
				break
			}
			site.EarlierSteps = append(site.EarlierSteps, s.Name)
		}
		return []expr.Site{site}
	}
	policy, ok := hub.Object[v1alpha1.Policy](r)
	if !ok {
		return []expr.Site{{Place: place}}
	}
	as := attached[r.Name]
	if len(as) == 0 {
		as = []attachment{{}}
	}
	out := make([]expr.Site, 0, len(as))
	for _, a := range as {
		site := expr.Site{Place: place, Scope: a.scope, PolicyType: policy.Spec.Type}
		if place == expr.PlacePolicyWhen {
			site.FirstPhase, _ = a.phases.First()
			site.GatesBody = a.phases.Has(phase.OnRequestBody) || policy.Spec.Type == v1alpha1.PolicyTypeTransformRequest
		}
		out = append(out, site)
	}
	return out
}

// compileCEL applies stage J's compile and cost rules (CM "Limits"): every
// CEL expression of the Bundle compiles at each of its sites (sites) within
// the cost ceiling, else RZ-CFG-014 or RZ-CFG-015 at the expression. An
// x-ruralz-cel field that maps to no place is an error of the check itself.
func compileCEL(st *stages, b *hub.Bundle, chains map[string]*hub.Chain, files *tree.FileTable) (diag.List, error) {
	places := celPlaces()
	attached := attachments(chains)
	var ds diag.List
	var unknown error
	for _, r := range b.Resources() {
		var defs []string
		_ = st.hubIndex().Walk(string(r.Kind), r.Tree, func(c *schemaidx.Cursor) error {
			defs = append(defs[:c.Depth()], c.Info.Node.Def())
			if c.Info.Keywords().CEL == nil || c.Node.Kind != tree.KindString || c.Node.Text == "" {
				return nil
			}
			last, _ := c.Last()
			field := defs[c.Depth()-1] + "." + last.Name
			place, ok := places[field]
			if !ok {
				unknown = errors.Join(unknown, fmt.Errorf("x-ruralz-cel field %s maps to no place", field))
				return nil
			}
			seen := map[string]bool{}
			for _, site := range sites(place, r, c.Path(), attached) {
				for _, is := range st.cel.Check(site, c.Node.Text) {
					if seen[is.Code+is.Message] {
						continue
					}
					seen[is.Code+is.Message] = true
					ds = append(ds, diag.Diagnostic{
						Code: is.Code, Severity: diag.SeverityError, Location: files.Location(c.Node.Pos),
						Resource: r.ResourceID(), Path: c.Path(), Message: is.Message, Hint: is.Hint,
					})
				}
			}
			return nil
		})
	}
	return ds, unknown
}

// controlDir is the directory beside the Bundles under examples/ that holds
// the Environment and Cluster files (CM "Bundle layout and merge": control/
// sits outside the Bundle).
const controlDir = "control"

// environment is one Environment of control/: the overlay it selects and
// the variables it substitutes (CM "Environment").
type environment struct {
	name    string
	overlay overlay.Selection
	vars    subst.Map
}

// render returns the render of the Environment.
func (e environment) render() render {
	return render{env: e.name, overlay: e.overlay, vars: e.vars, hints: slices.Sorted(maps.Keys(e.vars))}
}

// renderOf returns the renders of a Bundle: without --env, then for each
// Environment of envs.
func renderOf(envs []environment) []render {
	out := []render{{}}
	for _, e := range envs {
		out = append(out, e.render())
	}
	return out
}

// stringAt returns the string at the path of fields below n.
func stringAt(n *tree.Node, fields ...string) (*tree.Node, bool) {
	for _, f := range fields {
		var ok bool
		if n, ok = n.Get(f); !ok {
			return nil, false
		}
	}
	return n, n.Kind == tree.KindString
}

// readControl reads the files of dir under fsys, the Environments the CLI
// reads with --environments and the Clusters Ruralz Control reads: each
// document passes the envelope rules (only Environment and Cluster) and
// the rendered view; a promotion.from or a Cluster spec.environment that
// names no Environment there is RZ-CFG-009, and a promotion.from chain
// that returns to its Environment is RZ-CFG-022. It returns the
// Environments in name order; none when dir does not exist.
func readControl(ctx context.Context, st *stages, fsys fs.FS, dir string) ([]environment, diag.List, error) {
	names, err := configFiles(fsys, dir, nil)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for i, name := range names {
		names[i] = path.Join(dir, name)
	}
	files := &tree.FileTable{}
	rs, ds, err := parseFiles(ctx, fsys, ".", names, tree.RoleEnvironments, files)
	if err != nil {
		return nil, nil, err
	}
	rs, found := envelopeRules(st, rs, files, true)
	ds = append(ds, found...)
	for _, res := range rs {
		found, _ := st.views.Validate(res, files)
		ds = append(ds, found...)
	}
	envs := map[string]environment{}
	from := map[string]*tree.Node{}
	for _, res := range rs {
		if res.ID.Kind != v1alpha1.KindEnvironment {
			continue
		}
		e := environment{name: res.ID.Name, vars: subst.Map{}}
		e.overlay = overlay.Selection{Name: e.name, Environment: e.name}
		if n, ok := stringAt(res.Root, "spec", "overlay"); ok {
			e.overlay.Name, e.overlay.Explicit, e.overlay.Location = n.Text, true, files.Location(n.Pos)
		}
		if vars, ok := res.Root.Get("spec"); ok {
			if vars, ok = vars.Get("variables"); ok {
				for _, m := range vars.Members {
					e.vars[m.Key] = m.Value.Text
				}
			}
		}
		if n, ok := stringAt(res.Root, "spec", "promotion", "from"); ok {
			from[e.name] = n
		}
		envs[e.name] = e
	}
	missing := func(res *tree.Resource, n *tree.Node, p diag.Path) {
		ds = append(ds, diag.Diagnostic{
			Code: codeMissing, Severity: diag.SeverityError, Location: files.Location(n.Pos), Resource: res.ID.ResourceID(), Path: p,
			Message: fmt.Sprintf("Environment %q not found", n.Text),
		})
	}
	for _, res := range rs {
		switch res.ID.Kind {
		case v1alpha1.KindEnvironment:
			n, ok := from[res.ID.Name]
			if !ok {
				continue
			}
			p := diag.Path{diag.Field("spec"), diag.Field("promotion"), diag.Field("from")}
			if _, ok := envs[n.Text]; !ok {
				missing(res, n, p)
				continue
			}
			// Follow the chain: it either ends or returns to this Environment.
			seen := map[string]bool{res.ID.Name: true}
			for next := n.Text; ; {
				if next == res.ID.Name {
					ds = append(ds, diag.Diagnostic{
						Code: codePromotion, Severity: diag.SeverityError, Location: files.Location(n.Pos), Resource: res.ID.ResourceID(), Path: p,
						Message: "promotion.from chain returns to Environment " + res.ID.Name,
					})
					break
				}
				if seen[next] {
					break
				}
				seen[next] = true
				f, ok := from[next]
				if !ok {
					break
				}
				next = f.Text
			}
		case v1alpha1.KindCluster:
			if n, ok := stringAt(res.Root, "spec", "environment"); ok {
				if _, ok := envs[n.Text]; !ok {
					missing(res, n, diag.Path{diag.Field("spec"), diag.Field("environment")})
				}
			}
		default:
		}
	}
	out := make([]environment, 0, len(envs))
	for _, name := range slices.Sorted(maps.Keys(envs)) {
		out = append(out, envs[name])
	}
	return out, ds, nil
}

// bundleRoots returns the directories of fsys that hold a ruralz.yaml,
// hidden ones excepted, sorted.
func bundleRoots(fsys fs.FS) ([]string, error) {
	var roots []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if p != "." && strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		if _, err := fs.Stat(fsys, path.Join(p, bundleRoot)); err == nil {
			roots = append(roots, p)
		}
		return nil
	})
	slices.Sort(roots)
	return roots, err
}

// checkBundles renders every Bundle of an examples/ tree (bundleRoots)
// without --env and for each Environment of control/ (readControl) through
// loadBundle, and returns one problem per diagnostic, warnings included, as
// "<Bundle root>: <diagnostic text>", with the diagnostics of control/
// itself, and one problem per overlays/<name>/ directory that no
// Environment selects, which no render would check. renders counts the
// Bundle renders.
func checkBundles(ctx context.Context, st *stages, fsys fs.FS) (problems []string, renders int, err error) {
	envs, ds, err := readControl(ctx, st, fsys, controlDir)
	if err != nil {
		return nil, 0, err
	}
	for _, d := range ds {
		problems = append(problems, controlDir+": "+strings.TrimSpace(texts(diag.List{d})))
	}
	roots, err := bundleRoots(fsys)
	if err != nil {
		return nil, 0, err
	}
	selected := map[string]bool{}
	for _, e := range envs {
		selected[e.overlay.Name] = true
	}
	for _, root := range roots {
		_, overlays, err := bundleFiles(fsys, root)
		if err != nil {
			return nil, 0, err
		}
		for _, name := range slices.Sorted(maps.Keys(overlays)) {
			if !selected[name] {
				problems = append(problems, fmt.Sprintf("%s: overlays/%s/ is selected by no Environment of %s/, so no render checks it", root, name, controlDir))
			}
		}
		for _, r := range renderOf(envs) {
			renders++
			_, ds, err := loadBundle(ctx, st, fsys, root, r)
			if err != nil {
				return nil, 0, fmt.Errorf("%s [environment=%s]: %w", root, r.env, err)
			}
			ds.Sort()
			prev := ""
			for _, d := range ds {
				// overlay.CheckBase and stage F report a base $patch with
				// one text; the pipeline reports a finding once.
				line := root + ": " + strings.TrimSpace(texts(diag.List{d}))
				if line != prev {
					problems = append(problems, line)
				}
				prev = line
			}
		}
	}
	return problems, renders, nil
}

// checkBase is the examples/ tree TestCheckBundles changes per case: a
// Bundle b that renders without findings without --env and for the
// Environment env1, whose variable PORT it substitutes and whose overlay
// it applies.
func checkBase() map[string]string {
	return map[string]string{
		"b/ruralz.yaml": `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: http
      protocol: http
      port: ${PORT:-8080}
  policies:
    - name: limit-global
`,
		"b/routes/orders.yaml": `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: orders
spec:
  listeners: [http]
  match:
    path: {prefix: /orders}
  upstreams:
    - name: orders
`,
		"b/upstreams.json": `{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "orders"},
 "spec": {"protocol": "http", "endpoints": [{"address": "orders.shop.svc:8080"}]}}
`,
		"b/policies.yaml": `apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: limit-global
spec:
  type: ratelimit
  overridable: false
  config:
    key: "source.ip"
    limits: [{requests: 10, window: 1s}]
`,
		"b/overlays/env1/upstreams.yaml": `apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: orders
spec:
  timeout: 2s
`,
		"b/.ruralzignore":  "drafts/\nruralz.yaml\n",
		"b/drafts/x.yaml":  "kind: [broken\n",
		"b/.hidden.yaml":   "kind: [broken\n",
		"b/.git/x.yaml":    "kind: [broken\n",
		"b/notes.md":       "kind: [broken\n",
		"b/README.YAML":    "kind: [broken\n",
		"control/envs.yml": "apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: env1\nspec:\n  variables:\n    PORT: \"8081\"\n",
	}
}

// TestCheckBundles checks what checkBundles reports: nothing for the
// valid base tree, whose hidden, ignored, other-extension and upper-case
// extension files hold errors it must not read, and whose .ruralzignore
// cannot drop ruralz.yaml; for a finding in the Bundle, one problem per
// render that meets it, the render for an Environment naming it; for
// control/, one problem; and one for an overlay directory no Environment
// selects. Each case breaks one rule of one stage, so every check of
// loadBundle, readControl and checkBundles has a case that fails without
// it.
func TestCheckBundles(t *testing.T) {
	base := checkBase()
	route, policy, gateway, env := base["b/routes/orders.yaml"], base["b/policies.yaml"], base["b/ruralz.yaml"], base["control/envs.yml"]
	edit := func(s, old, repl string) string {
		if !strings.Contains(s, old) {
			t.Fatalf("%q is not in %q", old, s)
		}
		return strings.Replace(s, old, repl, 1)
	}
	// both lists findings in Bundle b in report order: each one in the
	// render without --env, then each one in the render for env1.
	both := func(texts ...string) []string {
		var out []string
		for _, env := range []string{"", " [environment=env1]"} {
			for _, text := range texts {
				out = append(out, "b: "+text+env)
			}
		}
		return out
	}
	const env2 = "---\napiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: env2\nspec:\n  promotion:\n    from: env1\n"
	// CEL sites (sites): a sequential step reads only earlier steps, an
	// aggregate step none.
	const steps = `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: orders
spec:
  match:
    path: {prefix: /orders}
  composition:
    mode: sequential
    steps:
      - name: a
        upstream: orders
        pathExpression: '"/a/" + string(steps.b.status)'
      - name: b
        upstream: orders
        pathExpression: '"/b/" + string(steps.a.status)'
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: parallel
spec:
  match:
    path: {prefix: /parallel}
  composition:
    mode: aggregate
    steps:
      - name: c
        upstream: orders
        pathExpression: '"/c/" + string(steps.c.status)'
`
	// Policy.spec.when reads response only from a response Phase, and
	// request.body only where the Policy gates the request body.
	const whens = `---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: hsts
spec:
  type: headers
  when: 'response.status == 200'
  config:
    response:
      set: [{name: strict-transport-security, value: "max-age=31536000"}]
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: body-check
spec:
  type: validation.json-schema
  when: 'has(request.body.id)'
  config:
    schema: {type: object}
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: limit-route
spec:
  type: ratelimit
  when: 'has(request.body.id)'
  config:
    key: "source.ip"
    limits: [{requests: 5, window: 1s}]
`
	// transform.request reads upstream only at Upstream scope.
	const stamp = `---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: stamp
spec:
  type: transform.request
  config:
    set: [{target: header, name: x-upstream, valueExpression: 'upstream.name'}]
`
	for _, tc := range []struct {
		name    string
		changes map[string]string
		renders int
		want    []string
	}{
		{"valid", nil, 2, nil},
		{
			"parse",
			map[string]string{"b/routes/orders.yaml": edit(route, "  match:", "  match: &m")},
			2,
			both("routes/orders.yaml:7:10 error RZ-CFG-003: anchors are not allowed (&m)"),
		},
		{
			"unserved apiVersion",
			map[string]string{"b/x.yaml": "apiVersion: ruralz/v9\nkind: Route\nmetadata:\n  name: x\nspec: {}\n"},
			2,
			both(`x.yaml:1:1 error RZ-CFG-007 Route/x: apiVersion "ruralz/v9" is not served`),
		},
		{
			"unknown kind",
			map[string]string{"b/x.yaml": "apiVersion: ruralz/v1alpha1\nkind: Router\nmetadata:\n  name: x\nspec: {}\n"},
			2,
			both(`x.yaml:1:1 error RZ-CFG-007 Router/x: unknown kind "Router"`),
		},
		{
			"Environment inside a Bundle",
			map[string]string{"b/x.yaml": "apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: x\nspec: {}\n"},
			2,
			both("x.yaml:1:1 error RZ-CFG-017 Environment/x: Environment is a control-plane kind and never inside a Bundle"),
		},
		{
			"duplicate identity",
			map[string]string{"b/x.yaml": edit(route, "  listeners: [http]\n", "")},
			2,
			both("x.yaml:1:1 error RZ-CFG-008 Route/orders: duplicate identity Route/orders (first defined at routes/orders.yaml:1:1)"),
		},
		{
			"Gateway outside ruralz.yaml",
			map[string]string{"b/ruralz.yaml": policy, "b/policies.yaml": "", "b/gateway.yaml": gateway},
			2,
			both("gateway.yaml:1:1 error RZ-CFG-016 Gateway/edge: the Gateway must be in ruralz.yaml"),
		},
		{
			"two Gateways",
			map[string]string{"b/ruralz.yaml": gateway + "---\n" + edit(gateway, "name: edge", "name: edge2")},
			2,
			both("ruralz.yaml:12:1 error RZ-CFG-016 Gateway/edge2: a Bundle has exactly one Gateway (first Gateway at ruralz.yaml:1:1)"),
		},
		{
			"no Gateway",
			map[string]string{"b/ruralz.yaml": policy, "b/policies.yaml": ""},
			2,
			both("ruralz.yaml error RZ-CFG-016: a Bundle has exactly one Gateway, in ruralz.yaml"),
		},
		{
			"$patch in a base document",
			map[string]string{"b/routes/orders.yaml": edit(route, "    - name: orders\n", "    - name: orders\n      $patch: delete\n")},
			2,
			both("routes/orders.yaml:11:7 error RZ-CFG-006 Route/orders spec.upstreams[name=orders].$patch: $patch is valid only in overlays"),
		},
		{
			"overlay parse",
			map[string]string{"b/overlays/env1/x.yaml": "a: &x 1\n"},
			2,
			[]string{"b: overlays/env1/x.yaml:1:4 error RZ-CFG-003: anchors are not allowed (&x) [environment=env1]"},
		},
		{
			"overlay directive",
			map[string]string{"b/overlays/env1/x.yaml": edit(route, "    - name: orders\n", "    - name: orders\n      $patch: bogus\n")},
			2,
			[]string{
				`b: overlays/env1/x.yaml:11:15 error RZ-CFG-005 Route/orders spec.upstreams[name=orders].$patch: unsupported $patch value "bogus" in a list entry ` +
					"(use $patch: delete, or {$patch: replace} as the first element of an orderedMap list) [environment=env1]",
			},
		},
		{
			"explicit overlay without its directory",
			map[string]string{"control/envs.yml": env + "  overlay: env9\n"},
			2,
			[]string{
				"b: overlays/env1/ is selected by no Environment of control/, so no render checks it",
				`b: control/envs.yml:8:12 error RZ-CFG-009 Environment/env1 spec.overlay: overlay directory "overlays/env9/" does not exist (did you mean "env1"?) [environment=env1]`,
			},
		},
		{
			"overlay no Environment selects",
			map[string]string{"b/overlays/qa/x.yaml": route},
			2,
			[]string{"b: overlays/qa/ is selected by no Environment of control/, so no render checks it"},
		},
		{
			"undefined variable",
			map[string]string{"b/routes/orders.yaml": edit(route, "prefix: /orders", `prefix: "${PREFIX}"`)},
			2,
			both(`routes/orders.yaml:8:20 error RZ-CFG-010 Route/orders spec.match.path.prefix: variable "PREFIX" is not defined and has no default`),
		},
		{
			"warnings are problems",
			map[string]string{"b/routes/orders.yaml": edit(route, "prefix: /orders", `prefix: "/${API_TOKEN:-x}"`)},
			2,
			both(`routes/orders.yaml:8:20 warning RZ-CFG-013 Route/orders spec.match.path.prefix: variable "API_TOKEN" looks like a secret ` +
				"(use a secretRef with provider: env instead of substitution)"),
		},
		{
			"schema",
			map[string]string{"b/routes/orders.yaml": route + "  timout: 1s\n"},
			2,
			both(`routes/orders.yaml:11:3 error RZ-CFG-006 Route/orders spec.timout: unknown field "timout" (did you mean "timeout"?)`),
		},
		{
			"stage G",
			map[string]string{"b/routes/orders.yaml": route + "  timeout: 9999999999h\n"},
			2,
			both(`routes/orders.yaml:11:12 error RZ-CFG-005 Route/orders spec.timeout: duration "9999999999h" is out of range: the largest is 2562047h47m16.854775807s`),
		},
		{
			"missing reference",
			map[string]string{"b/routes/orders.yaml": edit(route, "    - name: orders", "    - name: ordrs")},
			2,
			both(`routes/orders.yaml:10:13 error RZ-CFG-009 Route/orders spec.upstreams[name=ordrs].name: Upstream "ordrs" not found (did you mean "orders"?)`),
		},
		{
			"missing listener",
			map[string]string{"b/routes/orders.yaml": edit(route, "[http]", "[https]")},
			2,
			both(`routes/orders.yaml:6:15 error RZ-CFG-009 Route/orders spec.listeners[item=https]: Gateway listener "https" not found (did you mean "http"?)`),
		},
		{
			"effective chain",
			map[string]string{"b/routes/orders.yaml": route + "  excludePolicies: [{name: limit-global}]\n"},
			2,
			both("routes/orders.yaml:11:21 error RZ-CFG-019 Route/orders spec.excludePolicies[name=limit-global]: " +
				"cannot exclude a Policy with overridable: false (declared in policies.yaml:7:3)"),
		},
		{
			"CEL",
			map[string]string{"b/policies.yaml": edit(policy, `"source.ip"`, `"source.ip +"`)},
			2,
			both("policies.yaml:9:10 error RZ-CFG-014 Policy/limit-global spec.config.key: CEL ratelimit config.key: Syntax error: mismatched input '<EOF>' " +
				"expecting {'[', '{', '(', '.', '-', '!', 'true', 'false', 'null', NUM_FLOAT, NUM_INT, NUM_UINT, STRING, BYTES, IDENTIFIER}"),
		},
		{
			"CEL composition sites",
			map[string]string{"b/routes/orders.yaml": steps},
			2,
			both(
				`routes/orders.yaml:13:25 error RZ-CFG-014 Route/orders spec.composition.steps[name=a].pathExpression: `+
					`CEL Route.spec.composition.steps[].pathExpression: step "b" is not an earlier step of this composition `+
					`(steps holds the steps that ran before this one, in list order)`,
				`routes/orders.yaml:30:25 error RZ-CFG-014 Route/parallel spec.composition.steps[name=c].pathExpression: `+
					`CEL Route.spec.composition.steps[].pathExpression: steps is available only in sequential compositions (this composition is "aggregate")`,
			),
		},
		{
			"CEL Policy when sites",
			map[string]string{
				"b/policies.yaml":      edit(policy, "  overridable: false\n", "  overridable: false\n  when: 'response.status == 200'\n") + whens,
				"b/routes/orders.yaml": route + "  policies: [{name: hsts}, {name: body-check}, {name: limit-route}]\n",
			},
			2,
			both(
				"policies.yaml:8:9 error RZ-CFG-014 Policy/limit-global spec.when: CEL Policy.spec.when: undeclared reference to 'response' "+
					"(Policy.spec.when has request, source, route, consumer, auth, now)",
				"policies.yaml:40:9 error RZ-CFG-014 Policy/limit-route spec.when: CEL Policy.spec.when: request.body is not available here "+
					"(the request body is read only in body Phases and composition)",
			),
		},
		{
			"CEL transform sites",
			map[string]string{
				"b/policies.yaml":      policy + stamp,
				"b/routes/orders.yaml": route + "  policies: [{name: stamp}]\n",
				"b/upstreams.json":     edit(base["b/upstreams.json"], `"endpoints"`, `"policies": [{"name": "stamp"}], "endpoints"`),
			},
			2,
			both("policies.yaml:19:63 error RZ-CFG-014 Policy/stamp spec.config.set[0].valueExpression: " +
				"CEL transform.request config.set[].valueExpression: undeclared reference to 'upstream' " +
				"(transform.request config.set[].valueExpression has request, source, route, consumer, auth, now)"),
		},
		{
			"control schema",
			map[string]string{"control/envs.yml": env + "  variabels: {}\n"},
			2,
			[]string{`control: control/envs.yml:8:3 error RZ-CFG-006 Environment/env1 spec.variabels: unknown field "variabels" (did you mean "variables"?)`},
		},
		{
			"control Bundle kind",
			map[string]string{"control/routes.yaml": route},
			2,
			[]string{"control: control/routes.yaml:1:1 error RZ-CFG-017 Route/orders: Route is a Bundle kind; control/ holds only Environment and Cluster resources"},
		},
		{
			"control duplicate",
			map[string]string{"control/more.yaml": env},
			2,
			[]string{"control: control/more.yaml:1:1 error RZ-CFG-008 Environment/env1: duplicate identity Environment/env1 (first defined at control/envs.yml:1:1)"},
		},
		{
			"promotion target missing",
			map[string]string{"control/envs.yml": env + "  promotion: {from: staging}\n"},
			2,
			[]string{`control: control/envs.yml:8:21 error RZ-CFG-009 Environment/env1 spec.promotion.from: Environment "staging" not found`},
		},
		{
			"promotion cycle",
			map[string]string{"control/envs.yml": env + "  promotion: {from: env2}\n" + env2},
			3,
			[]string{
				"control: control/envs.yml:8:21 error RZ-CFG-022 Environment/env1 spec.promotion.from: promotion.from chain returns to Environment env1",
				"control: control/envs.yml:16:11 error RZ-CFG-022 Environment/env2 spec.promotion.from: promotion.from chain returns to Environment env2",
			},
		},
		{
			"Cluster Environment missing",
			map[string]string{"control/clusters.yaml": "apiVersion: ruralz/v1alpha1\nkind: Cluster\nmetadata:\n  name: c\nspec:\n  environment: prod\n"},
			2,
			[]string{`control: control/clusters.yaml:6:16 error RZ-CFG-009 Cluster/c spec.environment: Environment "prod" not found`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := checkBase()
			maps.Copy(files, tc.changes)
			fsys := fstest.MapFS{}
			for name, data := range files {
				if data != "" {
					fsys[name] = &fstest.MapFile{Data: []byte(data)}
				}
			}
			problems, renders, err := checkBundles(t.Context(), mustStages(t), fsys)
			if err != nil {
				t.Fatalf("checkBundles() error = %v", err)
			}
			if renders != tc.renders {
				t.Errorf("checkBundles() renders = %d, want %d", renders, tc.renders)
			}
			if !slices.Equal(problems, tc.want) {
				t.Errorf("checkBundles() problems =\n%s\nwant\n%s", strings.Join(problems, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// TestCELPlacesMatchSchema checks celPlaces, the table compileCEL reads,
// against the rendered view: every x-ruralz-cel property maps to a place,
// every entry names a property that carries x-ruralz-cel, and every place
// of expr.Places has exactly one entry.
func TestCELPlacesMatchSchema(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(schema.RenderedV1alpha1(), &doc); err != nil {
		t.Fatal(err)
	}
	annotated := map[string]bool{}
	defs, _ := doc["$defs"].(map[string]any)
	for def, d := range defs {
		props, _ := d.(map[string]any)["properties"].(map[string]any)
		for prop, p := range props {
			if _, ok := p.(map[string]any)["x-ruralz-cel"]; ok {
				annotated[def+"."+prop] = true
			}
		}
	}
	if len(annotated) == 0 {
		t.Fatal("the rendered view has no x-ruralz-cel property")
	}
	places := celPlaces()
	for field := range annotated {
		if _, ok := places[field]; !ok {
			t.Errorf("x-ruralz-cel field %s has no celPlaces entry", field)
		}
	}
	seen := map[expr.PlaceID]string{}
	for field, id := range places {
		if !annotated[field] {
			t.Errorf("celPlaces entry %s names no x-ruralz-cel field", field)
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("place %s has entries %s and %s", id, prev, field)
		}
		seen[id] = field
	}
	for _, p := range expr.Places() {
		if _, ok := seen[p.ID]; !ok {
			t.Errorf("place %s has no celPlaces entry", p.ID)
		}
	}
}
