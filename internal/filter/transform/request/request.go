// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package request is the transform.request Policy type: it rewrites the
// forwarded request body, headers, query string and path with CEL and
// regular expressions (docs/architecture/03-data-plane.md "Transform
// Policies"). It runs in onRequestBody at Gateway and Route scope, once on
// the client request, and in onUpstreamRequest at Upstream scope, on each
// attempt's outgoing request from the same buffered input; its CEL fields
// read request.body, and upstream at Upstream scope. Package transform
// holds the compiler, the offline check and the runtime it shares with
// transform.response, and documents the rules.
package request

import (
	"context"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/transform"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeTransformRequest

// Factory builds transform.request Filters (filter.Factory).
type Factory struct{}

var _ filter.Factory = Factory{}

// Build compiles a transform.request config (transform.Build): a config of
// another type is RZ-CFG-005.
func (Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.TransformRequestConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(transform.CodeConfig, "%s: config is %T, want *v1alpha1.TransformRequestConfig", Type, env.Config)
	}
	f, err := transform.Build(ctx, env, transform.RequestSpec(cfg))
	if err != nil {
		return nil, err
	}
	return f, nil
}

var _ hub.PolicyCheck = Check

// Check is the offline check of a transform.request Policy (a
// hub.PolicyCheck run at stage H by every binary, transform.Report). A
// resource whose Config is not a decoded TransformRequestConfig yields
// nothing.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.TransformRequestConfig)
	if !ok || cfg == nil {
		return
	}
	transform.Report(r, transform.RequestSpec(cfg), loc, add)
}

// maxContentLength is the longest Content-Length value the Node writes
// when it replaces a body: the digits of the largest int64.
const maxContentLength = 19

// AddedRequestBytes is the most bytes cfg adds to a request's header
// block, query and path as CEL reads them, a transform.request Policy's
// share of expr.InputCaps.AddedHeaderBytes: each set entry with target
// header counts its name, httpfield.MaxComputedValueBytes and 4 bytes for
// the separators of its field line, one with target query its name,
// httpfield.MaxQueryValueBytes and 4 bytes, and one with target path
// transform.MaxPathBytes and 4 bytes. A config that may rewrite the body
// also counts the Content-Type line it may write, contentType or
// application/json, and the Content-Length line the Node sets on the new
// body. A set entry counts whole although it replaces what it names, and
// a remove entry counts nothing.
func AddedRequestBytes(cfg *v1alpha1.TransformRequestConfig) int64 {
	if cfg == nil {
		return 0
	}
	var n int64
	for _, e := range cfg.Set {
		switch e.Target {
		case v1alpha1.TransformRequestTargetHeader:
			n += int64(len(e.Name)) + httpfield.MaxComputedValueBytes + 4
		case v1alpha1.TransformRequestTargetQuery:
			n += int64(len(e.Name)) + httpfield.MaxQueryValueBytes + 4
		case v1alpha1.TransformRequestTargetPath:
			n += transform.MaxPathBytes + 4
		default:
		}
	}
	if rewritesBody(cfg) {
		ct := len(transform.DefaultContentType)
		if cfg.ContentType != nil {
			ct = max(ct, len(*cfg.ContentType))
		}
		n += int64(len("Content-Type")+ct+4) + int64(len("Content-Length")+maxContentLength+4)
	}
	return n
}

// rewritesBody reports whether cfg may rewrite the body: it sets body or
// holds an arrayOps or replace entry, or a set or remove entry with target
// body.
func rewritesBody(cfg *v1alpha1.TransformRequestConfig) bool {
	if cfg.Body != "" || len(cfg.ArrayOps) > 0 || len(cfg.Replace) > 0 {
		return true
	}
	for _, e := range cfg.Set {
		if e.Target == v1alpha1.TransformRequestTargetBody {
			return true
		}
	}
	for _, e := range cfg.Remove {
		if e.Target == v1alpha1.TransformRequestTargetBody {
			return true
		}
	}
	return false
}
