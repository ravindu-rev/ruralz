// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// carryKey identifies a Filter across snapshots: the Policy's name and
// type and the attachment's scope and Phase set.
type carryKey struct {
	policy string
	typ    v1alpha1.PolicyType
	scope  phase.Scope
	phases phase.Set
}

// carried is a Filter of the previous snapshot with the canonical config
// of its Policy there.
type carried struct {
	filter filter.Filter
	config string
}

// carryOver is what the previous snapshot hands the compile: its Filters
// by identity and config (filter.BuildEnv.Previous), and every Filter it
// holds, which a discarded compile must not close.
type carryOver struct {
	byKey map[carryKey]carried
	held  map[any]bool
}

// carryOverOf indexes the Filters of prev, the active snapshot, whose
// Policies' canonical configs come from its Revision.
func carryOverOf(prev *snapshot.Snapshot) carryOver {
	co := carryOver{byKey: map[carryKey]carried{}, held: map[any]bool{}}
	if prev == nil {
		return co
	}
	configs := map[string]string{}
	if prev.Validated != nil && prev.Validated.Bundle != nil {
		for _, r := range prev.Validated.Bundle.Resources() {
			if r.Kind == v1alpha1.KindPolicy {
				configs[r.Name] = configOf(r)
			}
		}
	}
	add := func(p *snapshot.Policy) {
		if p == nil || p.Filter == nil {
			return
		}
		if isComparable(p.Filter) {
			co.held[p.Filter] = true
		}
		if p.Phases.Empty() {
			// An invalidation entry shares its cache Policy's Filter.
			return
		}
		k := carryKey{policy: p.Name, typ: p.Type, scope: p.Scope, phases: p.Phases}
		if _, dup := co.byKey[k]; dup {
			return
		}
		if cfg, ok := configs[p.Name]; ok {
			co.byKey[k] = carried{filter: p.Filter, config: cfg}
		}
	}
	for _, rt := range prev.Routes {
		if rt == nil {
			continue
		}
		for _, p := range rt.Chain.Policies {
			add(p)
		}
	}
	for _, lc := range prev.LegChains {
		if lc == nil {
			continue
		}
		for ph := range phase.Count {
			for _, p := range lc.Phases[ph] {
				add(p)
			}
		}
	}
	return co
}

// previous returns the Filter of the previous snapshot built for a's
// Policy, type, scope and Phase set with the same canonical config, or
// nil.
func (co carryOver) previous(a *attachment) filter.Filter {
	c, ok := co.byKey[carryKey{policy: a.key.policy, typ: a.pol.Spec.Type, scope: a.key.scope, phases: a.key.phases}]
	if !ok || c.config != configOf(a.res) {
		return nil
	}
	return c.filter
}

// configOf returns the canonical bytes of a Policy's spec.config, "" when
// it has none.
func configOf(r *hub.Resource) string {
	n, ok := r.Tree.At(diag.Path{diag.Field("spec"), diag.Field("config")})
	if !ok {
		return ""
	}
	out, err := canonical.AppendNode(nil, n)
	if err != nil {
		return ""
	}
	return string(out)
}

// isComparable reports whether v can be compared, so used as a map key.
func isComparable(v any) bool { return reflect.ValueOf(v).Comparable() }

// buildFilters builds the Filter of every attachment on the worker pool,
// with its spec.when and its body needs, and records each refusal. ctx's
// error, or a fault (a Factory missing for a served type, an error
// without an RZ code), ends the compile.
func (b *build) buildFilters(ctx context.Context) error {
	// However the builds end, ctx and a panic included, every Filter they
	// built is the compile's to close, but one the previous snapshot
	// holds.
	defer func() {
		for _, c := range b.closers() {
			if !b.carry.held[asKey(c)] {
				b.opened.filters = append(b.opened.filters, c)
			}
		}
	}()
	err := parallel(ctx, len(b.atts), b.c.workers, b.c.clk.Now, func(i int) { b.buildOne(ctx, b.atts[i]) })
	if err != nil {
		return err
	}
	for _, a := range b.atts {
		if a.failed != nil {
			if err := b.refuse(*a.failed); err != nil {
				return err
			}
		}
	}
	return nil
}

// closers returns the built Filters that hold resources
// (filter.Closer), each instance once, in attachment order. Instances are
// told apart by identity, as the retirer does: a Filter that cannot be
// compared counts as unshared, once per attachment.
func (b *build) closers() []io.Closer {
	var out []io.Closer
	seen := map[any]bool{}
	for _, a := range b.atts {
		c, ok := a.filter.(filter.Closer)
		if !ok {
			continue
		}
		if k := asKey(c); k != nil {
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		out = append(out, c)
	}
	return out
}

// asKey returns v as a map key, or nil for a value that cannot be one.
func asKey(v any) any {
	if !isComparable(v) {
		return nil
	}
	return v
}

// buildOne compiles one attachment: its spec.when at the attachment's
// site, its body needs and its Filter from the Policy type's Factory.
func (b *build) buildOne(ctx context.Context, a *attachment) {
	scope, phases := a.key.scope, a.key.phases
	site := func(place expr.PlaceID) expr.Site { return b.c.d.Pipeline.PolicySite(a.res, place, nil, scope, phases) }
	whenSite := site(expr.PlacePolicyWhen)
	a.gatesBody = whenSite.GatesBody
	a.metrics = b.plan.Policy(a.key.policy)
	if src := a.pol.Spec.When; src != "" {
		prg, iss := b.builder.Compile(whenSite, src)
		if prg == nil {
			a.failed = &refusal{res: a.res, path: specPath("when"), err: celError(iss)}
			return
		}
		a.when = prg
	}
	if a.pol.Spec.Type == v1alpha1.PolicyTypeTransformResponse && scope != phase.ScopeUpstream {
		needs, err := b.c.reg.TransformBody(registry.Attachment{Policy: a.pol, Config: a.res.Config, Scope: scope}, b.c.d.CEL)
		if err != nil {
			a.failed = &refusal{res: a.res, path: specPath("config"), err: fmt.Errorf("compile: body need of Policy %q: %w", a.key.policy, err)}
			return
		}
		a.readsResponse = needs || (a.when != nil && a.when.Refs().ResponseBody)
	}
	fac, ok := b.c.d.Filters.Factory(a.pol.Spec.Type)
	if !ok {
		a.failed = &refusal{res: a.res, path: specPath("type"), err: fmt.Errorf("compile: no Factory for the served Policy type %q", a.pol.Spec.Type)}
		return
	}
	hs := b.opened.handles
	env := filter.BuildEnv{
		Policy: a.pol, Config: a.res.Config, Scope: scope, Phases: phases,
		Bundle: b.b, Consumers: b.consumers,
		CEL: b.builder, Site: site,
		Secrets:    b.secrets,
		StateStore: hs.Main.Store(), Enqueuer: hs.Main.Enqueuer(),
		CacheStore: hs.Cache.Store(), CacheEnqueuer: hs.Cache.Enqueuer(),
		StateStoreTimeout: a.timeout,
		Limits: filter.Limits{
			MaxRequestBodyBytes: b.limits.MaxRequestBodyBytes, MaxResponseBodyBytes: b.limits.MaxResponseBodyBytes,
			MaxBufferedBytes: b.limits.MaxBufferedBytes,
		},
		Metrics: a.metrics, Status: b.c.d.Status, Clock: b.c.clk, Logger: b.c.log,
		Shared: b.shared, Replayer: b.c.d.Replayer,
		Previous: b.carry.previous(a),
	}
	f, err := fac.Build(ctx, env)
	switch {
	case err != nil:
		a.failed = &refusal{res: a.res, path: specPath("config"), err: err}
	case f == nil:
		a.failed = &refusal{res: a.res, path: specPath("config"), err: fmt.Errorf("compile: the %s Factory built no Filter for Policy %q", a.pol.Spec.Type, a.key.policy)}
	default:
		a.filter = f
		a.consumptive, _ = f.(filter.Consumptive)
	}
}

// refuse records a refusal as a finding when its error carries a
// registered RZ-CFG code (findingCode); ctx's error and any other error
// end the compile.
func (b *build) refuse(r refusal) error {
	if errors.Is(r.err, context.Canceled) || errors.Is(r.err, context.DeadlineExceeded) {
		return r.err
	}
	code, ok := findingCode(r.err)
	if !ok {
		return fmt.Errorf("compile: %s/%s %s: %w", r.res.Kind, r.res.Name, r.path, r.err)
	}
	b.findings = append(b.findings, b.loc.finding(code, r.res, r.path, r.err))
	return nil
}

// findingCode returns the code of err's outermost errcode.Error when it is
// a registered RZ-CFG code, the only codes a step of the validation gate
// rejects a Revision with (docs/architecture/02-configuration-model.md
// "Error codes"); an error without one is a fault of the release.
func findingCode(err error) (string, bool) {
	code, ok := errcode.CodeOf(err)
	if !ok {
		return "", false
	}
	c, ok := errcode.Lookup(code)
	return code, ok && c.Area == errcode.AreaCFG
}

// celError returns the error of a CEL field the Builder refused: the
// first Issue's code, RZ-CFG-014 or RZ-CFG-015, and message; RZ-CFG-014
// for an Issue without a registered RZ-CFG code, or for no Issue.
func celError(iss []expr.Issue) error {
	if len(iss) == 0 {
		return errcode.Errorf(codeCEL, "CEL did not compile")
	}
	code := iss[0].Code
	if c, ok := errcode.Lookup(code); !ok || c.Area != errcode.AreaCFG {
		code = codeCEL
	}
	return errcode.Errorf(code, "%s", iss[0].Message)
}

// codeCEL is RZ-CFG-014: a CEL syntax, type or unavailable-variable error.
const codeCEL = "RZ-CFG-014"
