// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"context"
	"errors"
	"fmt"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// ErrNoEnvironment marks a conversion check without an Environment:
// render --api-version renders both trees for every Environment of its
// required --environments file, so a file without one checks nothing.
var ErrNoEnvironment = errors.New("render: no Environment to check the conversion with")

// BuildFunc builds the Revision of one tree, the source Bundle or the
// converted one, rendered for Environment env: what ruralz bundle build
// --env env does through the configuration pipeline facade, offline. It
// returns the digest, zero when the tree does not build, and the
// diagnostics; its error is reserved for failures that are not about the
// Bundle (unreadable files, a canceled ctx).
type BuildFunc func(ctx context.Context, env string) (revision.Digest, diag.List, error)

// Outcome is one tree's Revision for one Environment.
type Outcome struct {
	// Digest is the Revision digest; zero when the tree did not build.
	Digest revision.Digest
	// Diagnostics are the build's findings, warnings included.
	Diagnostics diag.List
}

// built reports whether the tree built: a digest and no error.
func (o Outcome) built() bool { return !o.Digest.IsZero() && !o.Diagnostics.HasErrors() }

// Comparison is the conversion check of one Environment.
type Comparison struct {
	// Environment is the Environment's metadata.name.
	Environment string
	// Source is the source Bundle's Revision.
	Source Outcome
	// Converted is the converted Bundle's Revision.
	Converted Outcome
}

// Equal reports whether both trees built and yield one Revision, which is
// when their diff is empty: conversion never changes a Revision
// (configuration model, "Hub-and-spoke conversion").
func (c Comparison) Equal() bool {
	return c.Source.built() && c.Converted.built() && c.Source.Digest == c.Converted.Digest
}

// VerifyConversion is the second half of ruralz bundle render
// --api-version once WriteTree has written the converted tree: for each
// Environment of envs, in order, it builds the source tree with source
// and the converted tree with converted and compares their Revisions. The
// command exits 0 when every Comparison is Equal (AllEqual) and 1
// otherwise. An empty envs is ErrNoEnvironment; an error from either
// BuildFunc, or the end of ctx, stops the check and is returned.
func VerifyConversion(ctx context.Context, envs []string, source, converted BuildFunc) ([]Comparison, error) {
	if len(envs) == 0 {
		return nil, ErrNoEnvironment
	}
	if source == nil || converted == nil {
		return nil, fmt.Errorf("%w: a nil BuildFunc", ErrOptions)
	}
	out := make([]Comparison, 0, len(envs))
	for _, env := range envs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c := Comparison{Environment: env}
		var err error
		if c.Source.Digest, c.Source.Diagnostics, err = source(ctx, env); err != nil {
			return nil, fmt.Errorf("render: build the source Bundle for Environment %s: %w", env, err)
		}
		if c.Converted.Digest, c.Converted.Diagnostics, err = converted(ctx, env); err != nil {
			return nil, fmt.Errorf("render: build the converted Bundle for Environment %s: %w", env, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// AllEqual reports whether every comparison is Equal, the condition for
// render --api-version to exit 0; it is false for no comparisons.
func AllEqual(cs []Comparison) bool {
	if len(cs) == 0 {
		return false
	}
	for _, c := range cs {
		if !c.Equal() {
			return false
		}
	}
	return true
}
