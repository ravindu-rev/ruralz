// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/composition"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore/manager"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Errors of New and Compile.
var (
	// ErrDeps marks Deps New cannot build a Compiler from.
	ErrDeps = errors.New("compile: invalid dependencies")
	// ErrInput marks an Input no pipeline run produces, such as one
	// without a Revision or a Gateway: a programming error.
	ErrInput = errors.New("compile: invalid input")
)

// StateStores acquires the State Store handles of one snapshot, one new
// handle per role; *manager.Manager implements it.
type StateStores interface {
	Acquire(ctx context.Context, spec manager.Spec) (manager.Handles, error)
}

// Deps are the Node-wide parts the compiler uses for every Revision. The
// ruralzd wiring builds them once per process.
type Deps struct {
	// Pipeline is the configuration pipeline facade the Revisions come
	// from: its Registry runs the serve check, its HubIndex walks the
	// resources and its PolicySite gives each Policy field the site stage
	// J checked it at. Required.
	Pipeline *pipeline.Pipeline
	// CEL is the CEL compiler (internal/cel): its Builder compiles every
	// program of a snapshot. Required.
	CEL expr.Compiler
	// Filters are the Filter tables (internal/filter/builtin), injected so
	// the compiler never imports them. Required.
	Filters *filter.Registry
	// Secrets resolves every secretRef of a Revision
	// (internal/secret/resolver). Required.
	Secrets secret.Resolver
	// LookupEnv reads the process environment Secrets reads, which tells
	// whether RURALZ_STATE_STORE_URL is set for a Gateway without
	// stateStore; nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// StateStores acquires each snapshot's State Store handles
	// (internal/statestore/manager). Required.
	StateStores StateStores
	// Upstreams holds the Node's Upstream runtimes. Required.
	Upstreams *upstream.Manager
	// Composition is the composition engine. Required.
	Composition *composition.Engine
	// Meter admits each Revision's metric label sets. Required.
	Meter emit.Meter
	// Status receives the degraded reasons Filters raise
	// (filter.BuildEnv.Status); nil records none.
	Status emit.NodeStatus
	// Clock is the Node clock of the Filters and of the build workers'
	// yields; nil means clock.Real().
	Clock clock.Clock
	// Logger is the Filters' logger, and records a Filter whose Close
	// panics when a compile is discarded; nil discards.
	Logger *slog.Logger
	// Replayer revalidates Response Cache entries in the background
	// (internal/gateway/replay); nil revalidates every stale entry on the
	// request path.
	Replayer filter.Replayer
	// Rand draws the weighted picks of the plain-upstreams forwarders and
	// must be safe for concurrent use; nil means the math/rand/v2 runtime
	// generator.
	Rand rand.Source
	// Workers is the most Filter builds that run at once; zero means
	// Pipeline.Workers(), as the Node's activation runs the pipeline.
	Workers int
}

// Compiler is the snapshot compiler. It is immutable and safe for
// concurrent use, though a Node compiles one Revision at a time.
type Compiler struct {
	d         Deps
	reg       *registry.Registry
	workers   int
	clk       clock.Clock
	log       *slog.Logger
	lookupEnv func(string) (string, bool)
}

// New returns a Compiler over d. It fails with an error wrapping ErrDeps
// for a missing required dependency or a negative worker count.
func New(d Deps) (*Compiler, error) {
	missing := ""
	switch {
	case d.Pipeline == nil:
		missing = "Pipeline"
	case d.CEL == nil:
		missing = "CEL"
	case d.Filters == nil:
		missing = "Filters"
	case d.Secrets == nil:
		missing = "Secrets"
	case d.StateStores == nil:
		missing = "StateStores"
	case d.Upstreams == nil:
		missing = "Upstreams"
	case d.Composition == nil:
		missing = "Composition"
	case d.Meter == nil:
		missing = "Meter"
	}
	if missing != "" {
		return nil, fmt.Errorf("%w: %s is required", ErrDeps, missing)
	}
	if d.Workers < 0 {
		return nil, fmt.Errorf("%w: Workers %d is negative", ErrDeps, d.Workers)
	}
	c := &Compiler{d: d, reg: d.Pipeline.Registry(), workers: d.Workers, clk: d.Clock, log: d.Logger, lookupEnv: d.LookupEnv}
	if c.workers == 0 {
		c.workers = d.Pipeline.Workers()
	}
	if c.clk == nil {
		c.clk = clock.Real()
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.lookupEnv == nil {
		c.lookupEnv = os.LookupEnv
	}
	return c, nil
}

// Input is one Revision to compile.
type Input struct {
	// Validated is the pipeline's output for the Revision
	// (pipeline.Result.Validated). Required.
	Validated *hub.Validated
	// Files is the run's file table (pipeline.Result.Files), which locates
	// the findings; nil locates them by each resource's own source.
	Files *tree.FileTable
	// Previous is the active snapshot, whose Filters and CEL programs
	// carry over; nil before the first activation.
	Previous *snapshot.Snapshot
}

// Warning is one activation warning, which has no RZ code: the activation
// logs each once when it publishes the snapshot.
type Warning struct {
	// Resource names the resource the warning is about, when one is.
	Resource *diag.ResourceID
	// Message is one line.
	Message string
}

// Compiled is one compiled Revision before it is published: the
// activation publishes Snapshot (retire.Holder.Publish), then calls
// Activate, or calls Discard when it never publishes it.
type Compiled struct {
	// Snapshot is the compiled snapshot. Binding and Pins are the
	// activation's: Publish creates the pins.
	Snapshot *snapshot.Snapshot
	// Secrets is the Revision's resolved secret table, which the listener
	// Set prepares the listeners with.
	Secrets secret.Store
	// Upstreams is the snapshot's Upstream layer (also its Legs and one of
	// its Resources).
	Upstreams *upstream.Set
	// Warnings are the activation warnings, in a stable order.
	Warnings []Warning
	// UpstreamConnections is the summed circuitBreaker.maxConnections of
	// the Upstreams some Route reaches, which the file descriptor warning
	// of docs/architecture/03-data-plane.md "Bounded resources" adds to
	// the connection ceilings.
	UpstreamConnections int

	resolver secret.Resolver
	opened   resources
	// state is 0 until Activate (1) or Discard (2).
	state atomic.Uint32
}

// Activate switches the Node to the compiled snapshot at the swap: the
// secret resolver polls its secret table, and the Upstream runtimes take
// its sources, health thresholds, metric handles and balancer plan. The
// activation calls it right after Publish; later calls, and calls after
// Discard, do nothing.
func (c *Compiled) Activate() {
	if !c.state.CompareAndSwap(0, 1) {
		return
	}
	c.resolver.Activate(c.Secrets)
	c.Upstreams.Activate()
}

// Discard releases a compiled snapshot that is never published: it closes
// the Filters the compile built that the active snapshot does not share,
// closes the Upstream set and releases the State Store handles. After
// Activate it does nothing, as the retirer then owns the snapshot's
// resources; later calls do nothing either.
func (c *Compiled) Discard() {
	if !c.state.CompareAndSwap(0, 2) {
		return
	}
	c.opened.release()
}

// resources are what one compile opened: the Filters it built that the
// previous snapshot does not hold, the Upstream set and the State Store
// handles. release closes them, Filters first, as the retirer does.
type resources struct {
	filters []io.Closer
	set     *upstream.Set
	handles manager.Handles
	log     *slog.Logger
}

// release closes every Filter, the set and the handles. A Filter whose
// Close panics is logged, and the rest are still closed.
func (r *resources) release() {
	for i := len(r.filters) - 1; i >= 0; i-- {
		closeContained(r.log, r.filters[i])
	}
	r.filters = nil
	if r.set != nil {
		_ = r.set.Close()
		r.set = nil
	}
	r.handles.Release()
	r.handles = manager.Handles{}
}

// closeContained closes c, logging a panic instead of raising it, so one
// faulty Filter never keeps the others open.
func closeContained(log *slog.Logger, c io.Closer) {
	defer func() {
		if v := recover(); v != nil {
			log.Error("a Filter of a discarded snapshot panicked while closing", slog.Any("panic", v))
		}
	}()
	_ = c.Close()
}

// Compile compiles one Revision. It returns the Compiled snapshot; or,
// for a Revision a step of the validation gate refuses, that step's
// findings, sorted as diagnostics, every one an RZ-CFG error; or an error
// for ctx (the compile deadline), an Input without a Revision or a Gateway
// (ErrInput), or a fault of the release, such as an error without an RZ
// code. A panic reaches the caller on its goroutine. On every outcome but
// success the compile has closed what it opened, keeping the Filters the
// previous snapshot shares.
func (c *Compiler) Compile(ctx context.Context, in Input) (*Compiled, diag.List, error) {
	v := in.Validated
	if v == nil || v.Bundle == nil {
		return nil, nil, fmt.Errorf("%w: no validated Revision", ErrInput)
	}
	b, err := c.newBuild(in)
	if err != nil {
		return nil, nil, err
	}
	// Step 7: refuse every feature this release does not serve.
	if ds, err := c.serve(v, b.loc); err != nil || len(ds) > 0 {
		return nil, ds, err
	}
	done := false
	defer func() {
		if !done {
			b.opened.release()
		}
	}()
	b.collect()
	// Step 9: resolve every secretRef and open the State Store handles.
	if ds, err := b.resolve(ctx); err != nil || len(ds) > 0 {
		return nil, ds, err
	}
	if ds, err := b.stores(ctx); err != nil || len(ds) > 0 {
		return nil, ds, err
	}
	// Step 10: compile Filters, Upstreams, Routes and Routers.
	if ds, err := b.compile(ctx); err != nil || len(ds) > 0 {
		return nil, ds, err
	}
	out := b.result()
	done = true
	return out, nil, nil
}

// effectiveTimeout returns a Policy's effective stateStoreTimeout:
// spec.stateStoreTimeout, else the Gateway's stateStore.timeout default.
func effectiveTimeout(p *v1alpha1.Policy, def time.Duration) time.Duration {
	if p.Spec.StateStoreTimeout != nil {
		return time.Duration(*p.Spec.StateStoreTimeout)
	}
	return def
}
