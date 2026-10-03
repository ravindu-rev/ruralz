// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command benchgate is the stage 9 alloc/op regression gate
// (docs/architecture/12-performance-budgets-and-benchmarking.md, "Regression
// policy and gates" and "Statistics and runners"):
//
//	benchgate [-C DIR] [-base REF] [-config FILE] [-summary FILE] [-override]
//
// For every entry of the gate list (default test/bench/allocgate.json) it
// builds the package's test binary twice with the release toolchain, from
// the working tree (head) and from the merge base of HEAD and REF checked
// out in a temporary git worktree (base), then runs them alternately A, B,
// A, B ... with GOGC=off, a fixed GOMAXPROCS and -test.cpu, a fixed
// -test.benchtime <N>x and -test.benchmem. A benchmark fails when its head
// median allocs/op exceeds the base median by more than the threshold (3%)
// with at least four of five interleaved pairs slower, when the head median
// exceeds the entry's maxAllocs cap (PB-8's 30), or when the base has it and
// the head does not. A benchmark without a base counterpart passes and is
// reported. An empty -base checks the caps only (a first release).
//
// B/op is reported, not gated. -override (a perf-override label applied by
// a maintainer, 11 req 66) reports failures and passes. A missing or empty
// gate list prints a skip line. Exit status: 0 pass, 1 gate failure, 2 tool
// error; the worktree is removed on every exit path.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run parses args, runs the gate and returns the exit status.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("benchgate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.dir, "C", ".", "repository root (the module root)")
	fs.StringVar(&o.base, "base", "origin/main", "base ref whose merge base with HEAD is the baseline; empty checks the caps only")
	fs.StringVar(&o.config, "config", "test/bench/allocgate.json", "gate list")
	fs.StringVar(&o.summary, "summary", "", "append a Markdown report to this file, such as $GITHUB_STEP_SUMMARY")
	fs.BoolVar(&o.override, "override", false, "report failures but exit 0 (perf-override approved by a maintainer)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "benchgate: unexpected arguments %q\n", fs.Args())
		return 2
	}
	if strings.HasPrefix(o.base, "-") {
		// The base can come from a workflow input; git must never read it
		// as an option.
		_, _ = fmt.Fprintf(stderr, "benchgate: -base %q is not a ref\n", o.base)
		return 2
	}
	res, err := gate(ctx, o, stdout)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "benchgate:", err)
		return 2
	}
	if res == outcomeFail {
		_, _ = fmt.Fprintln(stderr, "benchgate: alloc/op gate failed")
		return 1
	}
	return 0
}
