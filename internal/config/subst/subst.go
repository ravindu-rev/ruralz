// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package subst is stage E of the configuration pipeline: ${VAR} and
// ${VAR:-default} substitution in string scalars, run exactly once after
// overlay merge and before schema validation
// (docs/architecture/02-configuration-model.md "Environment
// substitution"; spec 01 requirements 26 to 31).
//
// Substitution rewrites the text of string scalars only, so it can never
// inject structure: a value holding a newline, ": ", "- " or "{" stays one
// scalar, and substituted text is never scanned again. A scalar that is
// exactly one expression is re-typed by the rendered schema at its
// position (an integer port, a boolean flag). $${ is a literal ${, and
// Escape writes a literal ${ back as $${ for rendering.
//
// Positions are read from the schema through internal/config/schemaidx
// (architecture R-3) with if/then dispatch applied to each object after
// its dispatch members (a Policy spec.type) were substituted. Mapping
// keys, apiVersion, kind, metadata.name and every x-ruralz-ref,
// x-ruralz-secret (with its subtree) and x-ruralz-cel position are
// forbidden: an unescaped ${ there is RZ-CFG-011 and nothing else is
// reported for that scalar, while $${ is still read as a literal ${
// (01 req 28), so a renderer escapes every literal ${ (01 req 31).
//
// The package also owns both RZ-CFG-013 warnings (architecture 3.1): a
// variable whose name looks secret, and a credential-shaped literal in
// any string outside an x-ruralz-secret subtree.
package subst

import (
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Configuration codes this package raises (all registered in
// internal/errcode).
const (
	// CodeMalformed: a ${ that is not ${NAME} or ${NAME:-default}, or a
	// default holding ${.
	CodeMalformed = "RZ-CFG-005"
	// CodeAPIVersion: a resource whose apiVersion or kind has no schema.
	// Stage C reports and drops such documents first; the check keeps
	// substitution from running without forbidden positions.
	CodeAPIVersion = "RZ-CFG-007"
	// CodeUndefined: an undefined variable without a default, one per
	// occurrence.
	CodeUndefined = "RZ-CFG-010"
	// CodeForbidden: ${ in a mapping key or a forbidden position.
	CodeForbidden = "RZ-CFG-011"
	// CodeWarning: the RZ-CFG-013 warning for a secret-like variable name
	// or a credential-shaped literal; it never blocks a Revision.
	CodeWarning = "RZ-CFG-013"
)

// Source supplies variable values: an Environment's spec.variables in
// the CLI, the process environment in file-mode ruralzd. It must be safe
// for concurrent reads.
type Source interface {
	// Lookup returns the value of name and whether it is set.
	Lookup(name string) (string, bool)
}

// Map is a Source over a fixed set of variables, such as an Environment's
// spec.variables.
type Map map[string]string

// Lookup returns the value of name.
func (m Map) Lookup(name string) (string, bool) {
	v, ok := m[name]
	return v, ok
}

// Environ returns the variables of an os.Environ snapshot taken by the
// binary's entry point (this package never reads the process
// environment). Entries without "=" or with an empty name (the Windows
// "=C:" drive entries) are skipped; the first of two equal names wins, as
// os.Getenv reads it.
func Environ(environ []string) Map {
	m := make(Map, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if _, dup := m[k]; !dup {
			m[k] = v
		}
	}
	return m
}

// Options configure a Substituter.
type Options struct {
	// Schemas holds the schema index of every served apiVersion.
	Schemas *schemaidx.Set
	// Files converts tree positions to diagnostic locations; nil leaves
	// locations empty.
	Files *tree.FileTable
	// Variables supplies the values; nil defines no variable.
	Variables Source
	// HintNames are the names an RZ-CFG-010 hint may suggest: the
	// Environment's spec.variables keys. Leave it nil for the process
	// environment, whose names are never suggested (01 req 27).
	HintNames []string
}

// Substituter applies substitution to resources. It is immutable and safe
// for concurrent use, so one Substituter serves every loader worker.
type Substituter struct {
	opts      Options
	detectors []detector
}

// New returns a Substituter; it compiles the credential detectors once.
func New(opts Options) *Substituter {
	return &Substituter{opts: opts, detectors: newDetectors()}
}

// Apply substitutes every string scalar of res in place and returns the
// stage E diagnostics (01 req 26 to 30): RZ-CFG-005 for a malformed
// expression, RZ-CFG-010 per undefined variable, RZ-CFG-011 in forbidden
// positions and RZ-CFG-013 warnings. A scalar with an error keeps its
// authored text. Each substituted scalar records its variables in
// tree.Node.Vars, so schema validation names them when a re-typed value
// does not fit.
//
// Diagnostic paths name list entries by their authored key or element
// values (spec.listeners[name="${NAME}"].port), as the overlay stage
// does, never by substituted text: a stage E diagnostic never reveals a
// variable's value, a secret-like one included. The location (file,
// line, column) identifies the node in both the authored and the
// substituted tree; later stages name entries by their substituted keys.
func (s *Substituter) Apply(res *tree.Resource) diag.List {
	if res == nil || res.Root == nil || res.Root.Kind != tree.KindMap {
		return nil
	}
	w := &walker{s: s, id: res.ID}
	var root *schemaidx.Node
	if s.opts.Schemas != nil {
		if x, ok := s.opts.Schemas.Index(res.APIVersion); ok {
			root, _ = x.Resource(string(res.ID.Kind))
		}
	}
	if root == nil {
		if w.envelope(res.Root) {
			return w.diags
		}
		at := res.Start
		if v, found := res.Root.Get("apiVersion"); found && v.Pos.Known() {
			at = v.Pos
		}
		w.report(diag.Diagnostic{
			Code: CodeAPIVersion, Location: w.loc(at), Path: diag.Path{diag.Field("apiVersion")},
			Message: "no schema for " + string(res.ID.Kind) + " in apiVersion " + res.APIVersion,
		})
		return w.diags
	}
	w.visitMap(res.Root, root, place{}, edge{})
	return w.diags
}

// Escape writes every literal ${ in s as $${, the form substitution reads
// back as ${ (01 req 31): substituting Escape(s) yields s.
func Escape(s string) string { return strings.ReplaceAll(s, "${", "$${") }
