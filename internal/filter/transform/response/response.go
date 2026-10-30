// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package response is the transform.response Policy type: it rewrites the
// response body and headers with CEL and regular expressions
// (docs/architecture/03-data-plane.md "Transform Policies"). It runs in
// onResponse at Gateway and Route scope, after any composition merge and
// never on a generated response, and in onUpstreamResponseBody at
// Upstream scope, on each leg's response; its CEL fields read response
// with its body, and upstream at Upstream scope. Package transform holds
// the compiler, the offline check and the runtime it shares with
// transform.request, and documents the rules.
package response

import (
	"context"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/transform"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeTransformResponse

// Factory builds transform.response Filters (filter.Factory).
type Factory struct{}

var _ filter.Factory = Factory{}

// Build compiles a transform.response config (transform.Build): a config
// of another type is RZ-CFG-005.
func (Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.TransformResponseConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(transform.CodeConfig, "%s: config is %T, want *v1alpha1.TransformResponseConfig", Type, env.Config)
	}
	f, err := transform.Build(ctx, env, transform.ResponseSpec(cfg))
	if err != nil {
		return nil, err
	}
	return f, nil
}

var _ hub.PolicyCheck = Check

// Check is the offline check of a transform.response Policy (a
// hub.PolicyCheck run at stage H by every binary, transform.Report). A
// resource whose Config is not a decoded TransformResponseConfig yields
// nothing.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.TransformResponseConfig)
	if !ok || cfg == nil {
		return
	}
	transform.Report(r, transform.ResponseSpec(cfg), loc, add)
}
