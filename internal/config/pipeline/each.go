// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
)

// EachRequest names a Bundle and an environments file to run for every
// Environment the file declares: ruralz bundle validate with
// --environments and no --env (docs/reference/01-cli-and-api-surface.md
// "Global flags").
type EachRequest struct {
	// Source is the Bundle.
	Source loader.Source
	// Environments is the --environments file. Required.
	Environments loader.Source
	// Render keeps each run's rendered resources (Request.Render).
	Render bool
	// Online runs the online stage of each run (Request.Online).
	Online bool
}

// EachResult is one run per Environment of a Bundle.
type EachResult struct {
	// Shared holds the findings every Environment shares, sorted and
	// capped, naming no Environment: those of the environments file and of
	// stages A to C of the Bundle, reported once.
	Shared diag.List
	// Stopped reports that stages A to C of the Bundle stopped the load,
	// at a limit or the diagnostics cap: no Environment was rendered.
	Stopped bool
	// Environments are the Environments that passed their checks, in the
	// order the file declares them. An environments file that declares
	// none, and has no error in Shared, is a usage error of the CLI.
	Environments []string
	// Runs holds one Result per Environment of Environments, in the same
	// order. Each run's diagnostics are its findings of stages D to M, and
	// of the online stage, each naming its Environment.
	Runs []*Result
}

// HasErrors reports an error among the shared findings or in any run.
func (r *EachResult) HasErrors() bool {
	if r.Shared.HasErrors() {
		return true
	}
	for _, run := range r.Runs {
		if run.HasErrors() {
			return true
		}
	}
	return false
}

// RunEach runs the Bundle once for every Environment of the environments
// file, in file order: the file and the Bundle's stages A to C once, then
// stages D to M, and the online stage when asked, per Environment, each
// as Run would for that Environment. The error is loader.ErrOptions
// without an environments file, loader.ErrSource for a source that cannot
// be read, ctx's or a fault of the release.
func (p *Pipeline) RunEach(ctx context.Context, req EachRequest) (*EachResult, error) {
	if req.Environments.IsZero() {
		return nil, fmt.Errorf("%w: RunEach needs an environments file", loader.ErrOptions)
	}
	envs, err := p.loader.ReadEnvironments(ctx, req.Environments)
	if err != nil {
		return nil, err
	}
	b, err := p.loader.Parse(ctx, req.Source)
	if err != nil {
		return nil, err
	}
	own := b.Diagnostics()
	keep := newKeeper(p.limits.MaxDiagnostics)
	keep.add(stageLoad, 0, envs.Diagnostics())
	keep.add(stageLoad, 1, own)
	out := &EachResult{Shared: keep.result(), Stopped: b.Stopped(), Environments: envs.FileOrder()}
	if out.Stopped {
		return out, nil
	}
	for _, name := range out.Environments {
		env, _ := envs.Select(name)
		lres, err := b.Render(ctx, env, nil)
		if err != nil {
			return nil, err
		}
		res, err := p.afterLoad(ctx, lres, own, input{render: req.Render, online: req.Online})
		if err != nil {
			return nil, err
		}
		for i := range res.Diagnostics {
			res.Diagnostics[i].Environment = name
		}
		out.Runs = append(out.Runs, res)
	}
	return out, nil
}
