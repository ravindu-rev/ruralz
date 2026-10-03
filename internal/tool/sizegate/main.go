// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command sizegate is the stage 9 size and idle RSS gate
// (docs/architecture/12-performance-budgets-and-benchmarking.md, "Memory
// budget" and "Regression policy and gates"; 11 req 68):
//
//	sizegate -binary PATH [-binary PATH ...] [-max-size 167772160]
//	         [-idle-rss [-rss-binary PATH] [-max-rss 93323264] [-settle 120s]
//	         [-window 10s] [-interval 1s] [-env KEY=VALUE ...]]
//	         [-summary FILE] [-override] [-annotate]
//
// Each -binary is a ruralzd built with the release flags plus -s -w; the
// gate fails when one is larger than -max-size (160 MiB). With -idle-rss it
// starts the binary built for the host platform (or -rss-binary) with
// telemetry defaults, RURALZ_CONFIG naming an empty directory (no Revision)
// and no connections, samples VmRSS from /proc/<pid>/status every -interval
// over the last -window of the -settle period, and fails when the maximum
// exceeds -max-rss (89 MiB). The M0 ruralzd stub, which exits 2 at once
// with exactly "Ruralz Gateway is not implemented yet; it is Planned (M1)",
// is skipped with a line until WP-76 wires the data plane; -annotate (set by
// the Makefile under GitHub Actions) also prints the skip as a ::warning
// workflow command. Both values go to the report and to -summary
// ($GITHUB_STEP_SUMMARY). -override (a perf-override label applied by a
// maintainer, 11 req 66) reports failures and passes. Exit status: 0 pass,
// 1 gate failure, 2 tool error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, clock.Real(), os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// listFlag collects a repeated string flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// run parses args, runs the gate and returns the exit status.
func run(ctx context.Context, clk clock.Clock, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sizegate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var binaries, env listFlag
	fs.Var(&binaries, "binary", "stripped ruralzd to size-check; repeat per platform")
	maxSize := fs.Int64("max-size", defaultMaxSize, "largest allowed binary size in bytes")
	idle := fs.Bool("idle-rss", false, "also measure the idle RSS of the host platform's binary")
	rssBinary := fs.String("rss-binary", "", "binary for the idle RSS run; default the -binary built for the host platform")
	maxRSS := fs.Int64("max-rss", defaultMaxRSS, "largest allowed idle VmRSS in bytes")
	settle := fs.Duration("settle", defaultSettle, "idle settle period")
	window := fs.Duration("window", defaultWindow, "final part of the settle period that is sampled")
	interval := fs.Duration("interval", defaultInterval, "sampling interval")
	fs.Var(&env, "env", "extra KEY=VALUE setting for the idle run; repeatable")
	procRoot := fs.String("proc", "/proc", "proc file system root")
	summary := fs.String("summary", "", "append a Markdown report to this file, such as $GITHUB_STEP_SUMMARY")
	override := fs.Bool("override", false, "report failures but exit 0 (perf-override approved by a maintainer)")
	annotate := fs.Bool("annotate", false, "also print a skipped idle RSS measurement as a GitHub Actions ::warning workflow command")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch {
	case fs.NArg() > 0:
		_, _ = fmt.Fprintf(stderr, "sizegate: unexpected arguments %q\n", fs.Args())
		return 2
	case len(binaries) == 0 && (!*idle || *rssBinary == ""):
		_, _ = fmt.Fprintln(stderr, "sizegate: at least one -binary is required")
		return 2
	case *maxSize <= 0 || *maxRSS <= 0:
		_, _ = fmt.Fprintln(stderr, "sizegate: -max-size and -max-rss must be positive")
		return 2
	}
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			_, _ = fmt.Fprintf(stderr, "sizegate: -env %q is not KEY=VALUE\n", kv)
			return 2
		}
	}

	failed := false
	var sizes []sizeResult
	for _, b := range binaries {
		r, err := checkSize(b, *maxSize)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "sizegate:", err)
			return 2
		}
		sizes = append(sizes, r)
		failed = failed || r.failed()
		_, _ = fmt.Fprintf(stdout, "sizegate: size of %s (%s): %s (%d bytes), limit %s (%d bytes): %s\n",
			r.path, orUnknown(r.platform), mib(r.size), r.size, mib(r.limit), r.limit, passFail(r.failed()))
	}

	var rss *rssResult
	if *idle {
		bin := *rssBinary
		if bin == "" {
			host := runtime.GOOS + "/" + runtime.GOARCH
			for _, r := range sizes {
				if r.platform == host {
					bin = r.path
					break
				}
			}
			if bin == "" {
				_, _ = fmt.Fprintf(stderr, "sizegate: -idle-rss: no -binary is built for %s; pass -rss-binary\n", host)
				return 2
			}
		}
		r, err := measureIdle(ctx, clk, idleOptions{
			binary: bin, env: env, procRoot: *procRoot,
			settle: *settle, window: *window, interval: *interval, limit: *maxRSS,
		})
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "sizegate: idle RSS:", err)
			return 2
		}
		rss = &r
		failed = failed || r.failed()
		if r.skipped != "" {
			_, _ = fmt.Fprintf(stdout, "sizegate: skip idle RSS: %s\n", r.skipped)
			if *annotate {
				_, _ = fmt.Fprintf(stdout, "::warning title=Idle RSS gate skipped::%s; the idle RSS limit is not enforced until the data plane replaces the stub\n",
					escapeWorkflowData(r.skipped))
			}
		} else {
			_, _ = fmt.Fprintf(stdout, "sizegate: idle RSS of %s after %v: max %s (%d bytes) over %d samples, limit %s (%d bytes): %s\n",
				r.path, r.settle, mib(r.max), r.max, len(r.samples), mib(r.limit), r.limit, passFail(r.failed()))
		}
	}

	if *summary != "" {
		if err := appendSummary(*summary, sizes, rss, failed && *override); err != nil {
			_, _ = fmt.Fprintln(stderr, "sizegate:", err)
			return 2
		}
	}
	switch {
	case failed && *override:
		_, _ = fmt.Fprintln(stdout, "sizegate: failures accepted: the perf-override label was applied by a maintainer")
		return 0
	case failed:
		_, _ = fmt.Fprintln(stderr, "sizegate: size or idle RSS gate failed")
		return 1
	}
	return 0
}

// escapeWorkflowData escapes the message of a GitHub Actions workflow
// command, so a line break or percent sign cannot end or alter it.
func escapeWorkflowData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

func passFail(failed bool) string {
	if failed {
		return "FAIL"
	}
	return "pass"
}

func orUnknown(s string) string {
	if s == "" {
		return "platform unknown"
	}
	return s
}

// appendSummary appends both values to the job summary (11 req 68).
func appendSummary(path string, sizes []sizeResult, rss *rssResult, overridden bool) error {
	var b strings.Builder
	b.WriteString("### Size and idle RSS gate\n\n| Check | Binary | Value | Limit | Result |\n|---|---|---|---|---|\n")
	for _, r := range sizes {
		fmt.Fprintf(&b, "| Stripped size (%s) | `%s` | %s (%d bytes) | %s | %s |\n",
			orUnknown(r.platform), r.path, mib(r.size), r.size, mib(r.limit), passFail(r.failed()))
	}
	if rss != nil {
		if rss.skipped != "" {
			fmt.Fprintf(&b, "| Idle RSS | `%s` | skipped: %s | %s | skip |\n", rss.path, rss.skipped, mib(rss.limit))
		} else {
			fmt.Fprintf(&b, "| Idle RSS after %v (max of %d samples) | `%s` | %s (%d bytes) | %s | %s |\n",
				rss.settle, len(rss.samples), rss.path, mib(rss.max), rss.max, mib(rss.limit), passFail(rss.failed()))
		}
	}
	if overridden {
		b.WriteString("\nFailures accepted: the perf-override label was applied by a maintainer (11 req 66).\n")
	}
	b.WriteString("\n")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // The summary path is a command-line argument ($GITHUB_STEP_SUMMARY).
	if err != nil {
		return err
	}
	_, werr := io.WriteString(f, b.String())
	return errors.Join(werr, f.Close())
}
