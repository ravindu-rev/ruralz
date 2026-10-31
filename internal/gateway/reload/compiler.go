// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

// Compiler compiles one Revision into an unpublished snapshot, running
// validation gate steps 7, 9 and 10 (CompilerOf adapts
// *compile.Compiler). It returns the Compiled snapshot; or the findings
// of the step that refused the Revision, every one an RZ-CFG error; or an
// error for ctx (the compile deadline) or a fault of the release.
type Compiler interface {
	Compile(ctx context.Context, in compile.Input) (Compiled, diag.List, error)
}

// Compiled is one compiled Revision before publication
// (*compile.Compiled).
type Compiled interface {
	// Snapshot is the compiled snapshot; the activation sets its Binding
	// and publishes it.
	Snapshot() *snapshot.Snapshot
	// Secrets is the Revision's resolved secret table, which the
	// listeners and telemetry settings are prepared with.
	Secrets() secret.Store
	// Warnings are the activation warnings, logged once at publication.
	Warnings() []compile.Warning
	// UpstreamConnections is the summed circuitBreaker.maxConnections of
	// the Upstreams some Route reaches, which the file descriptor warning
	// adds to the connection ceilings.
	UpstreamConnections() int
	// Activate switches the Node's secret resolver and Upstream runtimes
	// to the snapshot, right after the swap.
	Activate()
	// Discard releases a snapshot that is never published.
	Discard()
}

// CompilerOf adapts the snapshot compiler.
func CompilerOf(c *compile.Compiler) Compiler { return compilerOf{c: c} }

type compilerOf struct{ c *compile.Compiler }

func (a compilerOf) Compile(ctx context.Context, in compile.Input) (Compiled, diag.List, error) {
	c, ds, err := a.c.Compile(ctx, in)
	if c == nil {
		return nil, ds, err
	}
	return compiledOf{c: c}, ds, err
}

// compiledOf adapts *compile.Compiled.
type compiledOf struct{ c *compile.Compiled }

func (c compiledOf) Snapshot() *snapshot.Snapshot { return c.c.Snapshot }
func (c compiledOf) Secrets() secret.Store        { return c.c.Secrets }
func (c compiledOf) Warnings() []compile.Warning  { return c.c.Warnings }
func (c compiledOf) UpstreamConnections() int     { return c.c.UpstreamConnections }
func (c compiledOf) Activate()                    { c.c.Activate() }
func (c compiledOf) Discard()                     { c.c.Discard() }
