// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package render writes what ruralz bundle render prints
// (docs/architecture/02-configuration-model.md "Rendered Bundle",
// docs/reference/01-cli-and-api-surface.md "Bundle verbs in detail"): the
// rendered Bundle of one Environment, the effective Filter Chain table of
// one Route, and the apiVersion conversion of a source Bundle. It takes
// what the configuration pipeline already produced, the trees and types of
// internal/config/tree, hub and precedence, and runs no stage itself; the
// pipeline facade and the CLI call it after a run without errors.
//
// # Rendered Bundle
//
// Documents and WriteBundle write the resources of one Environment as
// stage E leaves them: the overlay merged, every ${VAR} substituted, a
// value that was exactly one expression re-typed by the rendered schema
// view, and the whole validated by the pipeline, but nothing defaulted or
// normalized. Stage G rewrites trees in place, so the pipeline hands over
// copies taken before it. The output holds the authored values only: no
// schema or registry default is written, so a rendered Bundle never
// freezes a default a later schema level changes, and an older Node never
// meets a field it does not serve because render wrote its default.
// Comments and file names are dropped.
//
// The output is one YAML stream that is itself a valid one-file Bundle
// (FormatYAML), or the same documents as one JSON array (FormatJSON). The
// documents are in canonical order, by kind (Gateway, Upstream, Plugin,
// Policy, Consumer, AIProvider, AIModel, Route) and then name bytewise,
// the order of the Revision, so moving a resource between files never
// changes the output; each starts with apiVersion, kind, metadata (name,
// labels, annotations) and spec, and every other member keeps its
// authored order. Scalars take the forms of
// internal/config/profile: a string is plain when the YAML 1.2 core schema
// reads it back as that string, and double-quoted otherwise.
//
// Render is idempotent: every literal ${ in a string value, which only a
// $${ escape can have left after substitution, is written $${, so loading
// the output substitutes it back and rendering it again yields the same
// Revision and the same bytes. Mapping keys and the envelope fields
// apiVersion, kind and metadata.name never hold ${ in a valid Bundle
// (RZ-CFG-011), and keys are written as they are.
//
// A secretRef is written as the reference itself, never as a value: only
// Nodes resolve secrets. Before it writes anything, render walks every
// x-ruralz-secret position with the rendered schema index of the
// resource's own apiVersion, and a value there that is not a well-formed
// reference (an undeclared member, a type the schema does not admit, a
// provider outside its enum) refuses the whole render with RZ-CFG-012 at
// the secret field, naming neither the value nor any member below the
// field, as stage F's own RZ-CFG-012 never does either.
//
// # Effective Filter Chain
//
// Effective returns the rows of one Route's effective Filter Chain from
// the chain the pipeline computed (precedence.Resolver.Rows), the source
// of the data plane's per-Phase arrays, and WriteEffective prints them as
// the worked example's table (PHASE, LEG, POLICY, FROM, REASON) or as a
// JSON array of precedence.Row. A Route the Bundle lacks is
// ErrUnknownRoute with the nearest name as a hint.
//
// # Conversion
//
// ruralz bundle render --api-version VERSION --output-dir DIR runs three
// calls. Convert rewrites every base and overlay file of the source Bundle
// under VERSION through the hub (convert.Registry), unmerged and
// unsubstituted, each in its own format; WriteTree writes the files into
// DIR, which must not exist or must be empty and must lie outside the
// source Bundle, all at once through a hidden directory renamed into
// place; and VerifyConversion builds both trees for every Environment of
// the --environments file through an injected BuildFunc and compares their
// Revisions. The command exits 0 only when every Comparison is Equal; the
// CLI prints each Comparison and the diff of each that is not.
//
// # Concurrency and effects
//
// Every function is safe for concurrent use and never changes its inputs.
// Documents, WriteBundle, Convert and VerifyConversion check ctx between
// units of work. Only WriteTree touches the file system, and nothing in
// the package logs, reads the clock or opens a connection.
package render
