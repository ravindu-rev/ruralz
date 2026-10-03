// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command fuzzplan plans and runs the nightly Fuzz job of CI stage 11
// (docs/engineering/03-testing-and-quality-strategy.md, "Test pyramid" and
// "Fuzzing"; 11 req 28):
//
//	fuzzplan list   [-C DIR] [packages]
//	fuzzplan matrix [-C DIR] [-per 8] [packages]
//	fuzzplan shard  [-C DIR] [-per 8] [-shards N] -index I [-corpus] [packages]
//	fuzzplan run    [-C DIR] [-per 8] [-shards N] [-index I] [-fuzztime 15m] [-report FILE] [packages]
//
// The targets are listed with `go test -json -list '^Fuzz' <packages>`
// (default ./...), sorted by package and name, and cut into shards of -per
// (eight) in that order, so the same tree always yields the same shards.
// list prints "<package> <name>" lines; matrix prints the shard indexes as
// a JSON array for a GitHub Actions matrix; shard prints one shard's
// targets, or with -corpus their corpus directories relative to
// $GOCACHE/fuzz, which the workflow keeps between nights as an artifact.
// run fuzzes each target of shard -index (default -1: every target) in turn
// with `go test -run '^$' -fuzz '^<Name>$' -fuzztime <d> <package>`, keeps
// going after a failure, and writes a JSON report listing each target's
// result and the new testdata/fuzz inputs of each crasher, from which the
// workflow opens fuzz-crasher issues. -shards N fixes the matrix size: an
// index past the needed shards is empty, and too few shards is an error.
//
// Exit status: 0 pass (or nothing to run), 1 a target failed, 2 tool error.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, goRunner{}, clock.Real(), os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usage = "usage: fuzzplan list|matrix|shard|run [flags] [packages]"

// run dispatches a subcommand and returns the exit status.
func run(ctx context.Context, r runner, clk clock.Clock, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("fuzzplan "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("C", ".", "module root")
	per, shards, index, corpus := defaultPerShard, 0, -1, false
	fuzztime, reportPath := "15m", ""
	switch cmd {
	case "list":
	case "matrix":
		fs.IntVar(&per, "per", defaultPerShard, "targets per shard")
	case "shard", "run":
		fs.IntVar(&per, "per", defaultPerShard, "targets per shard")
		fs.IntVar(&shards, "shards", 0, "fixed number of shards (0: as many as needed)")
		fs.IntVar(&index, "index", -1, "shard index; -1 selects every target")
		if cmd == "shard" {
			fs.BoolVar(&corpus, "corpus", false, "print corpus directories relative to $GOCACHE/fuzz")
		} else {
			fs.StringVar(&fuzztime, "fuzztime", "15m", "fuzzing time per target")
			fs.StringVar(&reportPath, "report", "", "write the JSON report to this file")
		}
	default:
		_, _ = fmt.Fprintf(stderr, "fuzzplan: unknown command %q\n%s\n", cmd, usage)
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if per < 1 || shards < 0 || index < -1 || (shards > 0 && index >= shards) {
		_, _ = fmt.Fprintf(stderr, "fuzzplan %s: want -per >= 1, -shards >= 0 and -1 <= -index < -shards\n", cmd)
		return 2
	}
	patterns := fs.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	ts, err := listTargets(ctx, r, *dir, patterns)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "fuzzplan:", err)
		return 2
	}
	if err := checkShards(len(ts), per, shards); err != nil {
		_, _ = fmt.Fprintln(stderr, "fuzzplan:", err)
		return 2
	}
	selected := ts
	if index >= 0 {
		selected = shardOf(ts, per, index)
	}

	switch cmd {
	case "list":
		for _, t := range ts {
			_, _ = fmt.Fprintf(stdout, "%s %s\n", t.Package, t.Name)
		}
	case "matrix":
		idx := make([]int, shardCount(len(ts), per))
		for i := range idx {
			idx[i] = i
		}
		data, _ := json.Marshal(idx)
		_, _ = fmt.Fprintf(stdout, "%s\n", data)
	case "shard":
		for _, t := range selected {
			if corpus {
				_, _ = fmt.Fprintf(stdout, "%s/%s\n", t.Package, t.Name)
			} else {
				_, _ = fmt.Fprintf(stdout, "%s %s\n", t.Package, t.Name)
			}
		}
	case "run":
		return runShard(ctx, r, clk, *dir, selected, report{
			Format: "ruralz.fuzzplan.v1", Shard: index, Shards: max(shards, shardCount(len(ts), per)), FuzzTime: fuzztime,
		}, reportPath, stdout, stderr)
	}
	return 0
}

// runShard fuzzes the selected targets and writes the report.
func runShard(ctx context.Context, r runner, clk clock.Clock, dir string, ts []target, rep report, reportPath string, stdout, stderr io.Writer) int {
	if len(ts) == 0 {
		_, _ = fmt.Fprintln(stdout, "fuzzplan: skip: no fuzz targets in this shard")
	}
	results, err := runTargets(ctx, r, clk, dir, ts, rep.FuzzTime, stdout)
	rep.Targets = results
	if rep.Targets == nil {
		rep.Targets = []targetReport{}
	}
	if reportPath != "" {
		if werr := writeReport(reportPath, &rep); werr != nil {
			_, _ = fmt.Fprintln(stderr, "fuzzplan:", werr)
			return 2
		}
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "fuzzplan:", err)
		return 2
	}
	if rep.failed() {
		for _, t := range rep.Targets {
			if t.Result != resultPass {
				_, _ = fmt.Fprintf(stderr, "fuzzplan: %s %s: %s %v\n", t.Package, t.Name, t.Result, t.Crashers)
			}
		}
		return 1
	}
	return 0
}
