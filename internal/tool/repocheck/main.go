// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command repocheck runs the repository checks no linter covers (CI stage 1,
// docs/engineering/02-repository-layout-and-conventions.md):
//
//   - cmd/ holds wiring only: one main.go per binary with only func main;
//   - no file imports "C";
//   - every RZ-<AREA>-<NNN> string literal is registered in internal/errcode;
//   - pkg/ and api/schema import only the standard library and pkg/;
//   - proto and TypeScript files carry the license header;
//   - the no-license-check scan (ADR-0002, SM-1) finds no denylisted
//     identifier, string or hostname outside the reviewed allowlist;
//   - metric and span names come from internal/telemetry/catalog: no Go
//     string literal outside it (test files and testdata aside) is a metric
//     name or starts with a span prefix, no span starts with a literal name,
//     and the catalog itself follows the foundation pack grammar (spec 09
//     requirement 77);
//   - docs/architecture/10-observability.md lists every catalog family with
//     the same type, histogram set, unit and labels, and every degraded
//     reason (requirement 74), and every ruralz_* name and degraded reason
//     under docs/architecture/ is in its catalog (requirement 75).
//
// The Ruralz Console placeholder check starts with internal/console (M2).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	root := flag.String("root", ".", "repository root")
	allow := flag.String("allowlist", filepath.FromSlash("internal/tool/repocheck/nolicensecheck.allow"),
		"no-license-check allowlist, relative to -root")
	flag.Parse()
	findings, err := run(*root, *allow, linkedCatalog())
	if err != nil {
		fmt.Fprintln(os.Stderr, "repocheck:", err)
		os.Exit(2)
	}
	for _, f := range findings {
		fmt.Fprintln(os.Stderr, f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "repocheck: %d finding(s)\n", len(findings))
		os.Exit(1)
	}
}
