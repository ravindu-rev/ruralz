// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command bufcheck runs the two buf rules checks that take more than one buf
// command (docs/engineering/02-repository-layout-and-conventions.md, "buf
// rules"): make lint runs both, make generate the second with -write.
//
//	bufcheck breaking -buf BUF [-main REF] [-C DIR]
//	    buf breaking against REF (default origin/main) and against the
//	    newest vX.Y.Z tag of each of the two newest release lines, the
//	    minors N and N-1 of the skew window. A baseline whose tree holds no
//	    buf.yaml holds no protos, so every proto is new against it and
//	    nothing can break: that baseline passes, as main does for the change
//	    that adds the first protos.
//	bufcheck generate -buf BUF [-out DIR]... [-write] [-C DIR]
//	    buf generate into a temporary directory, which the -out directories
//	    (default internal/gen/proto) must equal: a generated file that is
//	    missing or differs there, a file there that buf generate does not
//	    write, and a generated file outside them are findings. Test files
//	    and testdata/ directories are not generated output. With -write,
//	    the -out directories are made equal to it instead.
//
// Exit status: 0 pass, 1 findings, 2 usage, git, buf or I/O error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usage = `usage: bufcheck breaking -buf BUF [-main REF] [-C DIR]
       bufcheck generate -buf BUF [-out DIR]... [-write] [-C DIR]`

// Exit codes of the checker, as every internal/tool checker uses them.
const (
	exitPass     = 0
	exitFindings = 1
	exitError    = 2
)

// stringList is a repeatable flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitError
	}
	switch args[0] {
	case "breaking":
		return runBreaking(ctx, args[1:], stdout, stderr)
	case "generate":
		return runGenerate(ctx, args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "bufcheck: unknown command %q\n%s\n", args[0], usage)
		return exitError
	}
}

func runBreaking(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bufcheck breaking", flag.ContinueOnError)
	fs.SetOutput(stderr)
	buf := fs.String("buf", "", "the buf binary")
	mainRef := fs.String("main", "origin/main", "the main branch baseline")
	dir := fs.String("C", ".", "repository root")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *buf == "" || *mainRef == "" || fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitError
	}
	c := checker{root: *dir, buf: *buf, stdout: stdout, stderr: stderr}
	ok, err := c.breaking(ctx, *mainRef)
	return finish(stderr, "bufcheck breaking", ok, err,
		"A breaking change needs ruralz.control.v2 or ruralz.plugin.v2 (docs/engineering/04-release-versioning-and-compatibility.md).")
}

func runGenerate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bufcheck generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	buf := fs.String("buf", "", "the buf binary")
	var outs stringList
	fs.Var(&outs, "out", "a directory buf generate writes, relative to -C (repeatable; default internal/gen/proto)")
	write := fs.Bool("write", false, "make the -out directories equal to buf generate's output")
	dir := fs.String("C", ".", "repository root")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *buf == "" || fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitError
	}
	if len(outs) == 0 {
		outs = stringList{"internal/gen/proto"}
	}
	for i, o := range outs {
		clean := filepath.ToSlash(filepath.Clean(o))
		if clean == "." || filepath.IsAbs(o) || clean == ".." || strings.HasPrefix(clean, "../") {
			_, _ = fmt.Fprintf(stderr, "bufcheck generate: -out %q is not a directory below the repository root\n", o)
			return exitError
		}
		outs[i] = clean
	}
	c := checker{root: *dir, buf: *buf, stdout: stdout, stderr: stderr}
	ok, err := c.generate(ctx, outs, *write)
	return finish(stderr, "bufcheck generate", ok, err,
		"Regenerate the protobuf code with make generate, then commit it.")
}

// finish maps a check's result to its exit status.
func finish(stderr io.Writer, name string, ok bool, err error, hint string) int {
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitError
	case !ok:
		_, _ = fmt.Fprintln(stderr, hint)
		return exitFindings
	default:
		return exitPass
	}
}

// checker runs buf and git in the repository at root.
type checker struct {
	root, buf      string
	stdout, stderr io.Writer
}

// errBuf is a buf failure that is not a finding.
var errBuf = errors.New("buf failed")

// bufFindingsStatus is the exit status buf ends with when it reports
// findings; any other non-zero status is an error.
const bufFindingsStatus = 100
