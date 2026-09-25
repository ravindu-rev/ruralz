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
//     identifier, string or hostname outside the reviewed allowlist.
//
// The metric and span name checks start with internal/telemetry (M1), and the
// Ruralz Console placeholder check with internal/console (M2).
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
	findings, err := run(*root, *allow)
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
