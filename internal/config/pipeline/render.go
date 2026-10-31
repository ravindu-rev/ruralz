// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"
	"io"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/precedence"
	"github.com/ravindu-rev/ruralz/internal/config/render"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// WriteRendered writes the rendered Bundle of res to w in format f
// (render.FormatYAML, the default of ruralz bundle render, or
// render.FormatJSON): the resources as stage E left them, in canonical
// order (docs/architecture/02-configuration-model.md "Rendered Bundle").
// A secret field that holds anything but a reference refuses the whole
// render with RZ-CFG-012 and nothing is written (render.WriteBundle). res
// must come from a run with Request.Render that reached its digest; any
// other is ErrInput.
func (p *Pipeline) WriteRendered(ctx context.Context, w io.Writer, res *Result, f render.Format) (diag.List, error) {
	if res == nil || res.Validated == nil || res.Rendered == nil {
		return nil, fmt.Errorf("%w: WriteRendered needs a run with Request.Render that reached its digest", ErrInput)
	}
	return render.WriteBundle(ctx, w, res.Rendered, f, render.Options{Schemas: p.schemas, Files: res.Files, MaxDiagnostics: p.limits.MaxDiagnostics})
}

// BuildFunc returns the render.BuildFunc of the Bundle src with the
// environments file envs: what ruralz bundle build --env does, offline,
// for the Environment it is called with, which ruralz bundle render
// --api-version runs on the source and the converted tree to compare their
// Revisions (render.VerifyConversion). It returns the digest of a run that
// reached it, else the zero digest, with the run's diagnostics; its error
// is Run's.
func (p *Pipeline) BuildFunc(src, envs loader.Source) render.BuildFunc {
	return func(ctx context.Context, env string) (revision.Digest, diag.List, error) {
		res, err := p.Run(ctx, Request{Source: src, Environments: envs, Environment: env})
		if err != nil {
			return revision.Digest{}, nil, err
		}
		if res.Validated == nil {
			return revision.Digest{}, res.Diagnostics, nil
		}
		return res.Validated.Revision.Digest, res.Diagnostics, nil
	}
}

// Effective returns the rows of the effective Filter Chain of route in the
// run res, the chain the data plane builds its per-Phase arrays from, as
// ruralz bundle render --effective prints them (render.Effective). A route
// the Bundle lacks is render.ErrUnknownRoute with the nearest name as a
// hint; a run without a digest is ErrInput.
func (p *Pipeline) Effective(res *Result, route string) ([]precedence.Row, error) {
	if res == nil || res.Validated == nil {
		return nil, fmt.Errorf("%w: Effective needs a run that reached its digest", ErrInput)
	}
	return render.Effective(res.Validated, route, p.resolver)
}
