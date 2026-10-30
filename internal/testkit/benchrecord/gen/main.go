// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command gen writes the JSON Schema internal/testkit/benchrecord embeds to
// test/bench/record.schema.json, the published copy stage 3 diffs
// (docs/architecture/12-performance-budgets-and-benchmarking.md "Result
// record"). It runs from the //go:generate line of the benchrecord
// package:
//
//	gen -out FILE
//
// It exits 1 on any error.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run writes the schema to the -out file.
func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "file to write the record schema to")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *out == "" || fs.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "gen: usage: gen -out FILE")
		return 1
	}
	// The published schema is world-readable documentation.
	if err := os.WriteFile(*out, benchrecord.Schema(), 0o644); err != nil { //nolint:gosec // G306: a published, world-readable schema file.
		_, _ = fmt.Fprintln(stderr, "gen:", err)
		return 1
	}
	return 0
}
