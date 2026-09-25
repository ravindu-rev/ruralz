// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package cli implements the ruralz command-line interface. Commands follow
// the `ruralz <noun> <verb>` form of docs/reference/01-cli-and-api-surface.md;
// `ruralz version` and `ruralz completion` are the only exceptions.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ravindu-rev/ruralz/internal/buildinfo"
)

// Exit codes from the CLI and API surface reference. They never change once
// released.
const (
	// ExitOK is success with nothing to report.
	ExitOK = 0
	// ExitNegative is a negative result, such as an invalid Bundle.
	ExitNegative = 1
	// ExitNoResult is a usage error or unreadable input.
	ExitNoResult = 2
)

const usage = `Usage: ruralz <command> [flags]

Commands:
  version    Print version, commit, build flavor and served apiVersions

Run 'ruralz <command> --help' for the flags of one command.
`

// Run executes the ruralz CLI with args (without the program name) and
// returns the process exit code. Data goes to stdout; usage and errors go to
// stderr.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return ExitNoResult
	}
	switch args[0] {
	case "version":
		return runVersion(ctx, args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		_, _ = io.WriteString(stdout, usage)
		return ExitOK
	default:
		_, _ = fmt.Fprintf(stderr, "ruralz: unknown command %q\n\n%s", args[0], usage)
		return ExitNoResult
	}
}

func runVersion(_ context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ruralz version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	output := fs.String("output", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitNoResult
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "ruralz version: unexpected argument %q\n", fs.Arg(0))
		return ExitNoResult
	}
	info := buildinfo.Get()
	var err error
	switch *output {
	case "text":
		err = info.WriteText(stdout)
	case "json":
		err = info.WriteJSON(stdout)
	default:
		_, _ = fmt.Fprintf(stderr, "ruralz version: unsupported --output %q (want text or json)\n", *output)
		return ExitNoResult
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ruralz version: %v\n", err)
		return ExitNoResult
	}
	return ExitOK
}
