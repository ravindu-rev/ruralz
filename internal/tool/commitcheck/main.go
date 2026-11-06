// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command commitcheck checks the DCO sign-off of the commits a change adds
// (docs/engineering/02-repository-layout-and-conventions.md, "DCO
// sign-off"), the check of `make dco` and the PR pipeline's ci job:
//
//	commitcheck dco -range BASE..HEAD   every non-merge commit of the range
//	                                     carries a Signed-off-by matching its
//	                                     author (DCO 1.1); Dependabot's
//	                                     commits are exempt
//
// Exit status: 0 pass, 1 findings, 2 usage or git error.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: commitcheck dco -range BASE..HEAD [-C DIR]")
		return 2
	}
	if args[0] != "dco" {
		fmt.Fprintf(os.Stderr, "commitcheck: unknown command %q; want dco\n", args[0])
		return 2
	}
	fs := flag.NewFlagSet("commitcheck dco", flag.ContinueOnError)
	rng := fs.String("range", "", "git revision range, such as origin/main..HEAD")
	dir := fs.String("C", ".", "git repository directory")
	if err := fs.Parse(args[1:]); err != nil || *rng == "" {
		fmt.Fprintln(os.Stderr, "commitcheck dco: -range is required")
		return 2
	}
	commits, err := gitCommits(ctx, *dir, *rng)
	if err != nil {
		fmt.Fprintln(os.Stderr, "commitcheck dco:", err)
		return 2
	}
	problems := checkDCO(commits)
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, p)
	}
	if len(problems) > 0 {
		fmt.Fprintln(os.Stderr, "Sign off with `git commit -s`; repair a branch with `git rebase --signoff`.")
		return 1
	}
	_, _ = fmt.Fprintf(os.Stdout, "commitcheck dco: %d commit(s) signed off\n", len(commits))
	return 0
}
