// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// RZ codes stage H raises; every one is registered in internal/errcode and
// defined in docs/architecture/02-configuration-model.md. RZ-CFG-035 and
// RZ-CFG-036 come from internal/identity and RZ-CFG-037 from the injected
// Policy checks, under the same codes.
const (
	// CodeSchema is RZ-CFG-005: a value the schema admits that a semantic
	// rule refuses, as a Node would refuse or ignore it at activation.
	CodeSchema = "RZ-CFG-005"
	// CodeReference is RZ-CFG-009: a missing or wrong-kind reference
	// target.
	CodeReference = "RZ-CFG-009"
	// CodePlugin is RZ-CFG-021: a Plugin image without an @sha256 digest.
	CodePlugin = "RZ-CFG-021"
	// CodeDuplicateMatch is RZ-CFG-023: two Routes with identical match
	// criteria on a common listener.
	CodeDuplicateMatch = "RZ-CFG-023"
	// CodeSteps is RZ-CFG-031: more composition steps than
	// limits.maxCompositionSteps.
	CodeSteps = "RZ-CFG-031"
	// CodeStepBodies is RZ-CFG-032: step maxBodyBytes values that exceed
	// limits.maxResponseBodyBytes or leave no default share.
	CodeStepBodies = "RZ-CFG-032"
	// CodeDestination is RZ-CFG-041: one secretRef used with two
	// destinations (secret-to-destination binding).
	CodeDestination = "RZ-CFG-041"
)

// DefaultMaxDiagnostics is the cap of one run when Options.MaxDiagnostics
// is zero: the loader's Limits.MaxDiagnostics default (10,000 per run), as
// in stages G and I.
const DefaultMaxDiagnostics = 10_000

// ErrOptions marks every error New returns.
var ErrOptions = errors.New("validate: invalid options")

// errGoexit is the value of the unitPanic of a worker that ended while it
// validated a resource without returning and without a panic to recover:
// something it called, such as an injected Policy check, ran
// runtime.Goexit.
var errGoexit = errors.New("validate: runtime.Goexit ended the worker while stage H validated the resource")

// Options configure a Validator. Every option is process-wide, so one
// Validator serves every load of a process.
type Options struct {
	// Schemas indexes the rendered schema of every served apiVersion; it
	// must serve convert.HubAPIVersion, whose index drives the reference
	// and secret walk and gives the Gateway defaults.
	Schemas *schemaidx.Set
	// Registry is the Policy type registry (registry.New); it checks
	// spec.filterClass.
	Registry *registry.Registry
	// Checks are the offline Policy checks: each binary passes
	// internal/filter/builtin/checks.Table(), so internal/config never
	// imports internal/filter. A type without a check is absent. Run calls
	// a check on every Policy of its type whose config is decoded, from up
	// to workers goroutines at once, so a check must be safe for
	// concurrent use. New copies the map.
	Checks hub.Checks
	// MaxDiagnostics is the cap of the run: the pipeline passes its
	// Limits.MaxDiagnostics, or what earlier stages left of it. Run
	// returns at most MaxDiagnostics+1 diagnostics, so a diag.Collector of
	// the same cap sees that it was reached and appends its one
	// RZ-CFG-001. Zero means DefaultMaxDiagnostics; a negative value is an
	// error.
	MaxDiagnostics int
	// Now is the clock of the yield timer: the pipeline passes
	// clock.Real().Now, tests a fake. Workers call it concurrently. Nil
	// never yields.
	Now func() time.Time
	// YieldAfter is the work a worker does before it yields; zero means
	// DefaultYieldAfter, and a negative value is an error.
	YieldAfter time.Duration
}

// schemaDefaults are the Gateway defaults stage H applies when the
// Gateway, or the object holding the field, is absent: stage G
// materializes defaults only inside objects that are present.
type schemaDefaults struct {
	adminPort   int64
	maxSteps    int64
	maxResponse int64
}

// Validator runs stage H. It is immutable after New and safe for
// concurrent use; each call takes the load's own tree.FileTable.
type Validator struct {
	hub            *schemaidx.Index
	reg            *registry.Registry
	checks         hub.Checks
	maxDiagnostics int
	now            func() time.Time
	yieldAfter     time.Duration
	defaults       schemaDefaults
	// stopped is a test hook, nil otherwise: Run calls it with the index
	// of the first resource whose panic it recorded.
	stopped func(index int)
}

// New returns a Validator, or an error wrapping ErrOptions when a required
// option is missing, a limit is negative, a check is nil, or the hub
// schema lacks a Gateway default stage H applies.
func New(o Options) (*Validator, error) {
	if o.Schemas == nil || o.Registry == nil {
		return nil, fmt.Errorf("%w: Schemas and Registry are required", ErrOptions)
	}
	if o.MaxDiagnostics < 0 || o.YieldAfter < 0 {
		return nil, fmt.Errorf("%w: MaxDiagnostics %d and YieldAfter %v must not be negative", ErrOptions, o.MaxDiagnostics, o.YieldAfter)
	}
	idx, ok := o.Schemas.Index(convert.HubAPIVersion)
	if !ok {
		return nil, fmt.Errorf("%w: no schema for the hub apiVersion %s", ErrOptions, convert.HubAPIVersion)
	}
	for t, c := range o.Checks {
		if c == nil {
			return nil, fmt.Errorf("%w: the check of Policy type %q is nil", ErrOptions, t)
		}
	}
	d, err := gatewayDefaults(idx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOptions, err)
	}
	return &Validator{
		hub: idx, reg: o.Registry, checks: maps.Clone(o.Checks),
		maxDiagnostics: cmp.Or(o.MaxDiagnostics, DefaultMaxDiagnostics),
		now:            o.Now, yieldAfter: cmp.Or(o.YieldAfter, DefaultYieldAfter),
		defaults: d,
	}, nil
}

// Result is what stage H hands to the later stages besides diagnostics.
type Result struct {
	// Secrets lists every secretRef use of the Bundle (hub.Validated.Secrets)
	// in canonical resource order, each resource's in document order, with
	// the kind of value it holds and its destination.
	Secrets []hub.SecretUse
}

// Run is stage H on b, the Bundle stage G built: the per-resource rules
// with at most workers goroutines (fewer than 1 means 1), created and
// joined within the call, then the cross-resource rules (credentials,
// match criteria, secret destinations). files is the tree.FileTable of
// the load that built b; nil reports positions in a resource's own file
// with hub.Resource.Source.File and others at the resource start.
//
// Every resource is checked and every rule runs, so one run reports every
// error. The diagnostics are the first MaxDiagnostics+1 of all findings in
// diag.List.Sort order, ties in report order (resource order, then the
// cross-resource passes), so the result is the same for every worker
// count. The error is non-nil only when ctx ends; then the result is
// empty. A panic, or a runtime.Goexit, in a check stops the dispatch of
// further resources; Run waits for the started ones and raises the first
// again in the calling goroutine, as an error value that wraps the panic
// value when it is an error and holds the worker's stack, so no goroutine
// outlives the call.
func (v *Validator) Run(ctx context.Context, b *hub.Bundle, files *tree.FileTable, workers int) (Result, diag.List, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, nil, err
	}
	if b == nil {
		return Result{}, nil, nil
	}
	main := v.newWork()
	ix := v.index(b, files, main)
	rs := ix.resources
	keep := newKeeper(v.maxDiagnostics + 1)
	secrets := make([][]hub.SecretUse, len(rs))
	var st runState
	var next atomic.Int64
	var wg sync.WaitGroup
	for range max(1, min(workers, len(rs))) {
		wg.Go(func() {
			w := v.newWork()
			for {
				if ctx.Err() != nil || st.stop.Load() {
					return
				}
				i := int(next.Add(1) - 1)
				if i >= len(rs) {
					return
				}
				if !v.runOne(ix, i, files, w, keep, secrets, &st) {
					return
				}
				w.check()
			}
		})
	}
	wg.Wait()
	if st.panicked != nil {
		panic(st.panicked)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, nil, err
	}
	var all []hub.SecretUse
	for _, s := range secrets {
		all = append(all, s...)
	}
	l := locator{files: files}
	// The cross-resource passes report after every resource, each as one
	// more work unit.
	passUnit := uint64(len(rs))
	for _, pass := range []func(*sink){
		func(out *sink) { v.checkCredentials(b, l, main, out) },
		func(out *sink) { v.checkMatches(ix, l, main, out) },
		func(out *sink) { checkSecrets(all, main, out) },
	} {
		if err := ctx.Err(); err != nil {
			return Result{}, nil, err
		}
		pass(&sink{k: keep, unit: passUnit})
		passUnit++
	}
	return Result{Secrets: all}, keep.list(), nil
}

// runState is what the workers of one Run share about a stop: the stop
// flag, which ends the dispatch, and the first worker failure recorded.
type runState struct {
	stop     atomic.Bool
	mu       sync.Mutex
	panicked *unitPanic
}

// runOne validates resource i of ix on worker w and records its secret
// uses; it returns false, after recording the stop in st, when the
// resource panicked. A runtime.Goexit never returns here: the deferred
// function records the stop, and the worker's goroutine then ends.
func (v *Validator) runOne(ix *bundleIndex, i int, files *tree.FileTable, w *work, keep *keeper, secrets [][]hub.SecretUse, st *runState) (ok bool) {
	done := false
	defer func() {
		if done {
			return
		}
		p := &unitPanic{value: recover(), index: i, stack: debug.Stack()}
		if p.value == nil {
			p.value, p.goexit = errGoexit, true
		}
		v.halt(st, p)
	}()
	r := ix.resources[i]
	if r != nil && inBundle(r.Kind) {
		l := locator{files: files}
		out := sink{k: keep, unit: uint64(i)} //nolint:gosec // G115: i is a resource index, never negative.
		u := &unit{v: v, ix: ix, l: l, w: w, r: r, idx: i, f: l.newFinder(r, w), out: out}
		u.validate()
		secrets[i] = u.secrets
	}
	done = true
	return true
}

// halt stops the dispatch of a Run and records p when it is the first
// failure; the test hook stopped sees the first one's index.
func (v *Validator) halt(st *runState, p *unitPanic) {
	st.stop.Store(true)
	st.mu.Lock()
	first := st.panicked == nil
	if first {
		st.panicked = p
	}
	st.mu.Unlock()
	if first && v.stopped != nil {
		v.stopped(p.index)
	}
}

// unitPanic is a panic raised, or a runtime.Goexit called (value
// errGoexit), while Run validated one resource, carried to the goroutine
// that called Run. It is an error that wraps the panic value when that
// value is an error, so errors.Is and errors.As reach it, and its text
// holds the value and the worker's stack.
type unitPanic struct {
	// value is what was passed to panic, or errGoexit.
	value any
	// index is the resource's index in canonical order.
	index int
	// stack is the worker's stack at the panic or the Goexit.
	stack []byte
	// goexit reports a runtime.Goexit.
	goexit bool
}

// Error returns the panic value, the resource index and the stack.
func (p *unitPanic) Error() string {
	what := "panicked"
	if p.goexit {
		what = "stopped"
	}
	return fmt.Sprintf("validate: stage H %s on resource %d: %v\n\n%s", what, p.index, p.value, p.stack)
}

// Unwrap returns the panic value when it is an error, else nil.
func (p *unitPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}
