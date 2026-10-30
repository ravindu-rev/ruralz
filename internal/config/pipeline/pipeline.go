// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/defaults"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/precedence"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/validate"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Errors of this package.
var (
	// ErrOptions marks Options New cannot build a Pipeline from.
	ErrOptions = errors.New("pipeline: invalid options")
	// ErrInput marks a call the pipeline cannot serve with what it was
	// given, such as WriteRendered for a run that kept no rendered
	// resources: a programming error, never a configuration finding.
	ErrInput = errors.New("pipeline: invalid input")
)

// Options configure a Pipeline. Every option is process-wide, so one
// Pipeline serves every load of a process: build it once per CLI
// invocation or Node boot.
type Options struct {
	// Checks are the offline checks of each Policy type: each binary
	// passes internal/filter/builtin/checks.Table(), so internal/config
	// never imports internal/filter (docs/engineering/02-repository-
	// layout-and-conventions.md "Package layers"). Required: a binary
	// that left them out would pass Bundles the others refuse. A type
	// without a check is absent from the map; an empty map has none.
	Checks hub.Checks
	// CEL is the CEL compiler (internal/cel in every binary): stage I asks
	// it which authz.cel rules read the request body, and stage J compiles
	// every expression with it. Required.
	CEL expr.Compiler
	// Schemas indexes the rendered schema of every served apiVersion; nil
	// indexes the schemas embedded in api/schema.
	Schemas *schemaidx.Set
	// Views compiles the same schemas for stage F; nil compiles the
	// embedded views of Schemas.
	Views *schemaview.Set
	// Converters is the conversion registry; nil means convert.Default().
	Converters *convert.Registry
	// Registry is the Policy type registry; nil means registry.New().
	Registry *registry.Registry
	// Limits bound each run: the loader's source, file, resource, nesting,
	// node and ignore-pattern limits, and the diagnostics cap of every
	// stage (loader.Limits). A zero field takes its default.
	Limits loader.Limits
	// Workers is the most goroutines a run uses at once in each stage;
	// zero means max(1, GOMAXPROCS/2), as a Node's activation runs, and the
	// CLI passes GOMAXPROCS. Results never depend on it.
	Workers int
	// Now is the clock of the workers' yield timers: the binaries pass
	// clock.Real().Now, tests a fake. Nil never yields.
	Now func() time.Time
	// YieldAfter is the work a worker does before it yields the
	// processor; zero means loader.DefaultYieldAfter (100 µs).
	YieldAfter time.Duration
}

// Pipeline is the configuration pipeline facade: stages A to M of
// docs/architecture/02-configuration-model.md (Figure 3) over the stage
// packages, and the online stage. It is immutable and safe for
// concurrent use.
type Pipeline struct {
	schemas    *schemaidx.Set
	views      *schemaview.Set
	hub        *schemaidx.Index
	reg        *registry.Registry
	conv       *convert.Registry
	cel        expr.Compiler
	loader     *loader.Loader
	defaults   *defaults.Stage
	validator  *validate.Validator
	resolver   *precedence.Resolver
	encoder    *canonical.Encoder
	decoder    *canonical.Decoder
	limits     loader.Limits
	workers    int
	now        func() time.Time
	yieldAfter time.Duration
}

// New returns a Pipeline. It compiles the embedded schemas when o leaves
// them nil, which takes a while, and builds every stage once. It fails
// with an error wrapping ErrOptions for a missing CEL compiler or Checks,
// a nil check, a negative limit, worker count or yield time, and options
// the stages refuse together.
func New(o Options) (*Pipeline, error) {
	switch {
	case o.CEL == nil:
		return nil, fmt.Errorf("%w: CEL is required: every binary passes its CEL compiler", ErrOptions)
	case o.Checks == nil:
		return nil, fmt.Errorf("%w: Checks is required: every binary passes checks.Table()", ErrOptions)
	case o.Workers < 0 || o.YieldAfter < 0:
		return nil, fmt.Errorf("%w: Workers %d and YieldAfter %v must not be negative", ErrOptions, o.Workers, o.YieldAfter)
	}
	p := &Pipeline{
		schemas: o.Schemas, views: o.Views, reg: o.Registry, conv: o.Converters, cel: o.CEL,
		workers: o.Workers, now: o.Now, yieldAfter: cmp.Or(o.YieldAfter, loader.DefaultYieldAfter),
	}
	if p.workers == 0 {
		p.workers = max(1, runtime.GOMAXPROCS(0)/2)
	}
	if p.reg == nil {
		p.reg = registry.New()
	}
	if p.conv == nil {
		p.conv = convert.Default()
	}
	conv := p.conv
	var err error
	if p.schemas == nil {
		if p.schemas, err = schemaidx.Embedded(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrOptions, err)
		}
	}
	if p.views == nil {
		if p.views, err = schemaview.Embedded(p.schemas); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrOptions, err)
		}
	}
	if p.loader, err = loader.New(loader.Options{
		Schemas: p.schemas, Views: p.views, Converters: conv, Limits: o.Limits,
		Workers: p.workers, Now: o.Now, YieldAfter: p.yieldAfter,
	}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOptions, err)
	}
	p.limits = p.loader.Limits()
	maxDiags := p.limits.MaxDiagnostics
	if p.defaults, err = defaults.New(defaults.Options{
		Schemas: p.schemas, Registry: p.reg, Converters: conv, MaxDiagnostics: maxDiags, Now: o.Now, YieldAfter: p.yieldAfter,
	}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOptions, err)
	}
	p.hub = p.defaults.HubIndex()
	if p.validator, err = validate.New(validate.Options{
		Schemas: p.schemas, Registry: p.reg, Checks: o.Checks, MaxDiagnostics: maxDiags, Now: o.Now, YieldAfter: p.yieldAfter,
	}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOptions, err)
	}
	if p.resolver, err = precedence.New(p.reg, precedence.Options{
		Body: o.CEL, MaxDiagnostics: maxDiags, Now: o.Now, YieldAfter: p.yieldAfter,
	}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOptions, err)
	}
	p.encoder = canonical.NewEncoder(p.hub)
	p.decoder = canonical.NewDecoder(p.hub, canonical.DecoderOptions{
		Workers: p.workers, MaxDiagnostics: maxDiags, MaxResources: p.limits.MaxResources, Now: o.Now, YieldAfter: p.yieldAfter,
	})
	return p, nil
}

// Limits returns the limits each run applies, defaults filled in.
func (p *Pipeline) Limits() loader.Limits { return p.limits }

// Workers returns the most goroutines a stage of a run uses.
func (p *Pipeline) Workers() int { return p.workers }

// Schemas returns the rendered schema index of every served apiVersion,
// which the rendered Bundle's secret check reads (render.Options.Schemas).
func (p *Pipeline) Schemas() *schemaidx.Set { return p.schemas }

// HubIndex returns the schema index of the hub representation, which the
// diff engine reads (diff.New).
func (p *Pipeline) HubIndex() *schemaidx.Index { return p.hub }

// Registry returns the Policy type registry the stages share.
func (p *Pipeline) Registry() *registry.Registry { return p.reg }

// Converters returns the conversion registry the stages share, which
// ruralz bundle render --api-version converts source files with
// (render.Convert).
func (p *Pipeline) Converters() *convert.Registry { return p.conv }

// Parse runs stages A to C on a Bundle under the run's limits: its files
// and base union, which ruralz bundle render --api-version reads file by
// file (loader.Bundle.ParseFile) to convert. The error is loader.ErrSource
// for a source that cannot be read, or ctx's.
func (p *Pipeline) Parse(ctx context.Context, src loader.Source) (*loader.Bundle, error) {
	return p.loader.Parse(ctx, src)
}

// ReadEnvironments reads and checks an --environments file under the
// run's limits (loader.Loader.ReadEnvironments): the CLI reads its
// Environments in file order (loader.Environments.FileOrder) to run each,
// and refuses a file that declares none. The error is loader.ErrSource for
// a file that cannot be read, or ctx's.
func (p *Pipeline) ReadEnvironments(ctx context.Context, src loader.Source) (*loader.Environments, error) {
	return p.loader.ReadEnvironments(ctx, src)
}

// Resolver returns stage I, whose Rows give the effective Filter Chain
// table of render --effective from the chains of a run.
func (p *Pipeline) Resolver() *precedence.Resolver { return p.resolver }

// Decoder returns the ruralz.canonical.v1 decoder of the hub schema,
// under the run's limits, for callers that read a /config/dump document
// themselves (canonical.Decoder.ReadDump) before FromResources.
func (p *Pipeline) Decoder() *canonical.Decoder { return p.decoder }

// PolicySite returns the site of the field at place and path (key-aware,
// under the resource, as hub.CELUse.Path) of the Policy r (a Policy
// hub.Resource) attached at scope, where it runs in phases
// (hub.Entry.Phases), exactly as stage J compiles it: the snapshot
// compiler passes the same sites to expr.Builder.Compile, through
// filter.BuildEnv.Site. For Policy.spec.when the site holds the first
// Phase there, which declares response when it is a response Phase, and
// whether the Policy gates the request body there, which lets the
// condition read request.body (configuration model, "Variables"). For a
// transform set[] valueExpression it holds the result the entry's target
// takes (expr.TransformValueResult); a nil path names no entry and keeps
// the place's result, which a transform Filter narrows itself by the
// entry it compiles. r must hold a *v1alpha1.Policy; for anything else the
// site is the bare place.
func (p *Pipeline) PolicySite(r *hub.Resource, place expr.PlaceID, path diag.Path, scope phase.Scope, phases phase.Set) expr.Site {
	pol, ok := hub.Object[v1alpha1.Policy](r)
	if !ok || pol == nil {
		return expr.Site{Place: place}
	}
	s := sites{cel: p.cel, reg: p.reg}
	return s.policySite(r, pol, place, path, scope, phases)
}
