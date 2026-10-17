// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package precedence is stage I of the configuration pipeline: it computes
// the effective Filter Chain (hub.Chain) of every Route from the Gateway,
// Route and Upstream attachments of a validated Bundle, and it is the only
// stage that raises RZ-CFG-018, RZ-CFG-019, RZ-CFG-020, RZ-CFG-029 and
// RZ-CFG-038 (architecture R-46; spec 02 section 2.7, requirements 35 to
// 42; foundation pack section 8.12).
//
// For each Route the chain follows the resolution rules of
// docs/architecture/02-configuration-model.md "Attachment and precedence":
//
//  1. The Gateway's spec.policies minus the Route's spec.excludePolicies;
//     excluding a Gateway Policy with overridable: false is RZ-CFG-019.
//  2. Plus the Route's spec.policies: a Route Policy in the slot of a
//     remaining Gateway Policy replaces it (RZ-CFG-019 when that Policy has
//     overridable: false), other slots stack.
//  3. One Policy per slot per scope: two entries of one spec.policies list
//     naming distinct Policies with equal slots are RZ-CFG-018 at the
//     second, with the first as a related "first entry at" location (01
//     req 39). The rule applies to the Gateway list once, to each Route
//     list and to each Upstream list. An entry naming the same Policy as
//     an earlier entry of its list, which stage G reports as a duplicate
//     key (RZ-CFG-005, 01 req 34), is skipped silently, in spec.policies
//     and in spec.excludePolicies alike.
//  4. Upstream-scoped Policies form one set per Upstream, run only in
//     upstream-leg Phases and take no part in slot comparison. A type at a
//     disallowed scope is RZ-CFG-020 at the attaching entry
//     (registry.Registry.Phases decides, including Phase selection for
//     authz.cel through the injected body oracle, headers, transform.* and
//     plugin).
//  5. Within a Phase: Filter class, then scope (Gateway, Route, Upstream),
//     then zero-based position in that scope's spec.policies. The response
//     Phases onUpstreamResponseHeaders, onUpstreamResponseBody and
//     onResponse, and onChunk on the client leg and on legs (spec 02 R-12),
//     run in exact reverse; every other Phase, onRoute and onLog included,
//     in request order.
//
// A chain that holds a cache Policy and an onRequestBody Policy of class
// authz or validation, or a plugin Policy of class auth or authz in
// onRequestBody, is RZ-CFG-038 at the cache attachment
// (OQ-traffic-management-and-resilience-21 (a), spec 02 req 39). A Route's
// legs are its Upstreams in order of first appearance in spec.upstreams
// (by name, as map sorting leaves them) or spec.composition.steps
// (authored order), each once (req 41). failureMode open on a closed-only
// Policy is RZ-CFG-029, raised once per Policy whether attached or not
// (CheckPolicies, req 37).
//
// References that did not resolve (a missing Policy, Plugin or Upstream:
// RZ-CFG-009, raised at stage H), a reference longer than any resource
// name (63 bytes), and resources without a typed view, are skipped
// without further diagnostics and take no memory of their own, and every
// other attachment and Route is still processed, so one run reports every
// error (req 40). The
// chains are the single source of the data plane's per-Phase arrays and
// of render --effective (Resolver.Rows), so the two cannot diverge (req
// 42, 04 req 40).
//
// A Resolver holds only process-wide inputs (the registry, the body
// oracle, the diagnostic cap and the yield clock), so one Resolver serves
// every load of a process (spec 02 section 3: built once per CLI
// invocation or Node boot and passed explicitly); each call takes the
// load's own tree.FileTable. It is immutable after New and safe for
// concurrent use. Resolve fans work out to a bounded worker pool that it
// owns and waits for; no goroutine outlives the call, a panic or a
// runtime.Goexit in a work unit reaches the caller as one *unitPanic
// whatever the worker count, each worker yields the processor once
// Options.YieldAfter of work has passed (01 req 53), and a call reports
// at most Options.MaxDiagnostics+1 findings, holding fewer than twice
// that plus one per worker at any time, all workers together (01 req
// 50).
package precedence

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// RZ codes this stage raises (architecture R-46); every one is registered
// in internal/errcode and defined in docs/architecture/02-configuration-model.md.
const (
	// CodeSlot is RZ-CFG-018: two Policies in one slot at one scope.
	CodeSlot = "RZ-CFG-018"
	// CodeOverridable is RZ-CFG-019: excluding or replacing a Gateway
	// Policy with overridable: false.
	CodeOverridable = "RZ-CFG-019"
	// CodeScope is RZ-CFG-020: a Policy type, or a Plugin's Phases, not
	// allowed at the attaching scope.
	CodeScope = registry.CodeScope
	// CodeFailureMode is RZ-CFG-029: failureMode open on a closed-only
	// Policy.
	CodeFailureMode = registry.CodeFailureMode
	// CodeCacheGuardrail is RZ-CFG-038: a cache Policy combined with an
	// onRequestBody authorization, validation or Plugin auth or authz
	// Policy in one effective chain.
	CodeCacheGuardrail = "RZ-CFG-038"
)

// ErrOptions marks every error New returns.
var ErrOptions = errors.New("precedence: invalid options")

// Options configures a Resolver. Every option is process-wide, so one
// Resolver serves every load of a process.
type Options struct {
	// Body decides whether an authz.cel rule references request.body,
	// which places the Policy in onRequestBody instead of onRequestHeaders
	// (02 req 13a). Every binary passes its CEL compiler (expr.Compiler
	// implements registry.BodyOracle). When Body is nil and an attached
	// authz.cel Policy has a rule, Resolve fails with
	// registry.ErrNoBodyOracle (a programming error, never a guess).
	//
	// Body must be safe for concurrent use: Resolve calls it from up to
	// workers goroutines. An expr.Compiler is safe for concurrent use.
	// Body takes no context: Resolve observes the end of its ctx only
	// between work units, so a Body call in flight when ctx ends always
	// runs to completion and Resolve waits for it before it returns.
	// Resolve calls Body only inside work units, never on the calling
	// goroutine: a panic in it, or a runtime.Goexit (t.FailNow in a test
	// oracle, say), ends that unit and reaches the caller as a work unit's
	// failure (see Resolve), never as a result without the Policy.
	Body registry.BodyOracle
	// MaxDiagnostics is the cap of a run (01 req 50): the pipeline passes
	// its Limits.MaxDiagnostics. Run, Resolve and CheckPolicies each
	// return at most MaxDiagnostics+1 diagnostics, the first in
	// diag.List.Sort order of all their findings (ties in report order,
	// see Resolve). The call keeps them in one collector that all its
	// workers share, which holds at most MaxDiagnostics+1 at any time,
	// and a worker merges what it finds into it at once while it is not
	// full, then in batches of at most 64 and at most
	// (MaxDiagnostics+1)/workers (at least 1), and at the end of each
	// work unit. Once it is full, every worker rejects a finding that
	// sorts after its last kept one on location and code before building
	// it. That bounds the number of findings a run holds at once by
	// fewer than 2(MaxDiagnostics+1)+workers, whatever the input and the
	// worker count, and a worker builds a finding only when it sorts no
	// later than the last one the collector keeps at that moment, which
	// every worker's merges move. The size of one finding is bounded
	// by the texts it quotes: Policy, Plugin and resource names (63
	// characters for a Bundle stage F accepted, and stage I runs only
	// after it), Policy types, scopes, Filter classes and Phases (fixed
	// sets), file names shared with the tree.FileTable, and at most 64
	// code points of a slot, whose length no schema rule bounds. The
	// first MaxDiagnostics+1 hold every stage I finding a diagnostic
	// collector of that cap can keep, whatever earlier stages reported,
	// and MaxDiagnostics+1 of them tell it the cap was reached: stage I
	// never appends RZ-CFG-001 itself. Zero means DefaultMaxDiagnostics; a
	// negative value is an error.
	MaxDiagnostics int
	// Now is the clock of the yield timer (01 req 53 and 55): the pipeline
	// passes the Now its caller supplies (clock.Real().Now in every
	// binary), tests a fake. Every worker reads it before each work unit
	// and every 256 units of work inside one, and calls runtime.Gosched
	// once YieldAfter has passed since its last yield, so stage I shares
	// the processor with live traffic during activation. Workers call it
	// concurrently. Nil never yields and reads no clock. Now must not
	// panic; a panic in it reaches the caller in the calling goroutine
	// for every worker count, raised again as a work unit's panic (see
	// Resolve) when a worker read the clock for a unit. A runtime.Goexit
	// in it is a unit's failure the same way when a worker read the clock
	// for a unit; a read on the calling goroutine outside every unit
	// (worker 0's creation, the Gateway list, the sequential steps
	// between the pools) ends the calling goroutine, as the call itself
	// would.
	Now func() time.Time
	// YieldAfter is the work a worker does before it yields; zero means
	// DefaultYieldAfter (100 µs), and a negative value is an error.
	YieldAfter time.Duration
}

// Resolver computes effective chains. It is immutable after New and safe
// for concurrent use, and it keeps nothing of one call for the next: the
// tree.FileTable of a load is an argument of each call, so one Resolver
// serves every load of a process. Build it with New. The zero value, like
// a nil *Resolver, behaves as New(nil, Options{}) returns: every call
// builds its own registry.New(), with the default cap and yield time, no
// body oracle and no clock.
type Resolver struct {
	reg            *registry.Registry
	body           registry.BodyOracle
	maxDiagnostics int
	now            func() time.Time
	yieldAfter     time.Duration
}

// New returns a Resolver over reg; a nil reg uses registry.New(). Every
// call shares reg, which a registry.Registry allows: it is immutable and
// safe for concurrent use. Resolve and Rows take every Filter class from
// reg, so live and removed rows of one Resolver always agree. New fails,
// with an error wrapping ErrOptions, when MaxDiagnostics or YieldAfter is
// negative.
func New(reg *registry.Registry, o Options) (*Resolver, error) {
	if o.MaxDiagnostics < 0 || o.YieldAfter < 0 {
		return nil, fmt.Errorf("%w: MaxDiagnostics %d and YieldAfter %v must not be negative", ErrOptions, o.MaxDiagnostics, o.YieldAfter)
	}
	return newResolver(reg, o), nil
}

// newResolver is New for options already checked.
func newResolver(reg *registry.Registry, o Options) *Resolver {
	if reg == nil {
		reg = registry.New()
	}
	return &Resolver{
		reg: reg, body: o.Body, maxDiagnostics: cmp.Or(o.MaxDiagnostics, DefaultMaxDiagnostics),
		now: o.Now, yieldAfter: cmp.Or(o.YieldAfter, DefaultYieldAfter),
	}
}

// usable returns r, or for a nil or zero Resolver (New always sets a
// registry) what New(nil, Options{}) returns, so the exported methods
// never dereference a missing registry or keep a cap of zero.
func (r *Resolver) usable() *Resolver {
	if r != nil && r.reg != nil {
		return r
	}
	return newResolver(nil, Options{})
}

// Run is stage I: Resolve and CheckPolicies, with one diagnostic list in
// diag.List.Sort order, at most Options.MaxDiagnostics+1 long: the first
// of all their findings, CheckPolicies' first among findings the sort
// leaves equal. files is the tree.FileTable of the load that built b
// (see Resolve). The error is non-nil only for a programming error (see
// Resolve) or when ctx ends.
func (r *Resolver) Run(ctx context.Context, b *hub.Bundle, files *tree.FileTable, workers int) (map[string]*hub.Chain, diag.List, error) {
	s, ks, err := r.usable().run(ctx, b, files, workers)
	if err != nil {
		return nil, nil, err
	}
	return s.chains, diagnostics(ks), nil
}

// run is Run, returning the call's state and its findings in compareKept
// order: CheckPolicies' and Resolve's, kept by one collector.
func (r *Resolver) run(ctx context.Context, b *hub.Bundle, files *tree.FileTable, workers int) (*state, []kept, error) {
	s, err := r.resolve(ctx, b, files, workers, true)
	if err != nil {
		return nil, nil, err
	}
	return s, s.found, nil
}

// CheckPolicies reports failureMode open on every closed-only Policy of b
// as RZ-CFG-029 at spec.failureMode, once per Policy whether it is
// attached or not (02 req 37; foundation pack section 8.10): auth.*
// (auth.upstream-* included), authz.* and a plugin Policy whose
// filterClass is auth or authz. files locates the findings as in Resolve.
// The list is sorted and holds at most Options.MaxDiagnostics+1
// findings, the first in diag.List.Sort order (Bundle order among
// findings the sort leaves equal). A Policy whose Object is not a
// non-nil *v1alpha1.Policy is skipped (see Resolve).
func (r *Resolver) CheckPolicies(b *hub.Bundle, files *tree.FileTable) diag.List {
	r = r.usable()
	out := newCollector(keepOf(r.maxDiagnostics), 1)
	w := r.newWorker(out)
	r.checkPolicies(w, b, files)
	return diagnostics(out.sorted(w))
}

// checkPolicies is CheckPolicies on worker w, whose collector keeps the
// findings.
func (r *Resolver) checkPolicies(w *worker, b *hub.Bundle, files *tree.FileTable) {
	if b == nil {
		return
	}
	loc := locator{files: files}
	w.begin(groupPolicies, 0)
	for _, res := range b.Resources() {
		w.charge(sitePolicyCheck, 1)
		if res.Kind != v1alpha1.KindPolicy {
			continue
		}
		p, ok := object[v1alpha1.Policy](res)
		if !ok {
			continue
		}
		d, bad := r.reg.CheckFailureMode(p)
		if !bad {
			continue
		}
		at := loc.path(w, res, d.Path, false)
		if !w.admits(at, d.Code) {
			continue
		}
		d.Resource, d.Location = res.ResourceID(), at
		w.offer(d)
	}
	w.flush()
}

// Resolve computes the effective chain of every Route of b, keyed by
// Route name, with the RZ-CFG-018, RZ-CFG-019, RZ-CFG-020 and RZ-CFG-038
// findings, sorted. The Gateway list and every Upstream list are checked
// once, whether or not a Route reaches them; each Route list once per
// Route.
//
// The findings are at most Options.MaxDiagnostics+1: the first in
// diag.List.Sort order, findings the sort leaves equal (such as findings
// without a position) in report order, the Gateway list's first, then
// the Upstream lists' and the Routes', each in Bundle order. The same
// findings come back for every worker count, and the call holds fewer
// than 2(MaxDiagnostics+1)+workers of them at any time, all workers
// together (see Options.MaxDiagnostics).
//
// files is the tree.FileTable of the load that built b: it maps tree
// positions to file names for diagnostic locations and hub.Entry.Ref.
// When nil, a position in a resource's own file is reported with
// hub.Resource.Source.File and any other position (an overlay-added node)
// at the resource's start. A FileTable indexes one load's files only, so
// each call passes its own; the Resolver keeps none.
//
// Chains share slices and must be treated as read-only: every Route
// reaching an Upstream shares that leg's Phase slices, and a client Phase
// a Route changes nothing in (no removed Gateway Policy and no Route
// Policy runs in it) shares the Gateway's slice, as a Phase changed only
// by removals shares the slice of every Route removing the same Gateway
// Policies from it. Every shared slice has a capacity equal to its
// length, so an append copies it. Sharing is by content, so a Phase whose
// content no other Route has costs that Route a copy of every remaining
// Gateway entry of the Phase, O(Gateway Policies) per Route: a Route that
// adds a Policy to a Phase holding Gateway Policies, a Route whose
// exclusions remove a set of Gateway Policies from a Phase that no other
// Route removes, and a Route whose replacing Policy runs in another Phase
// than the replaced one, which removes it from its Phase the same way.
// Chain memory is therefore linear in the input only while Routes share
// their removal sets; bounding the rest is a contract request on hub.Chain
// (WP-38 S2 chain-size findings of rounds 2 and 3, pinned by
// TestScaleSharedLinear).
//
// A Policy's spec.slot is read once per attachment scope, when Phase
// selection interns it: lists compare slots by interned id, and a finding
// quotes at most 64 code points of a slot, so neither the work nor the
// findings of a Route grow with the length of the slots it attaches (no
// schema rule bounds that length). A reference (spec.policies,
// spec.excludePolicies, spec.upstreams, a composition step's upstream or
// a plugin Policy's spec.plugin) longer than 63 bytes, the longest
// metadata.name stage F accepts, is never hashed or compared: it is
// skipped as unresolved after its length is read, so no work grows with
// the length of a reference either (no schema rule bounds it). A
// reference that resolves to no Policy takes no memory beyond its list
// entry.
//
// Work is split over at most workers goroutines (one unit per attachment
// selection, Upstream or Route; at least one goroutine, which the call
// starts and joins, so no unit runs on the calling goroutine), and the
// result never depends on scheduling. Each worker reads the yield timer
// of Options.Now between units and every 256 units of work inside one
// (01 req 53), and every loop of a unit counts its elements as it reads
// them, so the time between two reads stays bounded. Those goroutines
// read b, call Options.Body and read files concurrently; b must not be
// modified until Resolve returns. ctx is checked between work units: once
// it ends, no further unit starts, every unit already started (an
// Options.Body call in flight included) runs to completion, and Resolve
// returns ctx.Err() after all of them have. Once a work unit has failed,
// by a panic (in Options.Body, say) or by a runtime.Goexit, which ends its
// goroutine without a panic to recover, no further unit starts and
// dispatch ends; units started before run to completion, as may the
// failing unit's own deferred calls, which run before the failure is
// recorded. Once every started unit has returned, it is raised again in
// the calling goroutine, with the same value and the work unit's stack
// whatever the worker count (an error that wraps the panic value, which
// errors.Is and errors.As reach; for a Goexit, an error that says the
// unit stopped), so a caller's recover sees one behavior for every worker
// count and no unit is ever left out of a result.
//
// A Gateway, Route, Upstream, Policy or Plugin whose hub.Resource.Object
// is not a non-nil pointer to its kind's v1alpha1 type (a nil Object, a
// typed nil or another type, which stage G never produces) is treated as
// absent, like an unresolved reference: Resolve, CheckPolicies and Rows
// skip it the same way for every worker count and never dereference it.
//
// Resolve fails, with no result, when ctx ends (ctx.Err()) or on a
// programming error that earlier stages exclude: a Policy type the
// registry does not list (registry.ErrUnknownType), an authz.cel rule
// without Options.Body (registry.ErrNoBodyOracle) or a
// hub.Resource.Config whose type is not the Policy type's config type.
func (r *Resolver) Resolve(ctx context.Context, b *hub.Bundle, files *tree.FileTable, workers int) (map[string]*hub.Chain, diag.List, error) {
	s, err := r.usable().resolve(ctx, b, files, workers, false)
	if err != nil {
		return nil, nil, err
	}
	return s.chains, diagnostics(s.found), nil
}

// resolve runs Resolve and returns its state; a nil b has no Routes.
// With policies set it first runs CheckPolicies on worker 0 into the same
// collector, as Run does.
func (r *Resolver) resolve(ctx context.Context, b *hub.Bundle, files *tree.FileTable, workers int, policies bool) (*state, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b == nil {
		b = hub.NewBundle(nil)
	}
	s := newState(r, b, files, workers)
	if policies {
		r.checkPolicies(s.pool.worker(0), b, files)
	}
	if err := s.run(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
