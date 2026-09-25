// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command commitcheck enforces the commit rules of CI stage 1
// (docs/engineering/02-repository-layout-and-conventions.md):
//
//	commitcheck dco -range BASE..HEAD   every non-merge commit carries a
//	                                     Signed-off-by matching its author (DCO 1.1)
//	commitcheck title [-title TITLE]    a pull request title, default $PR_TITLE, follows
//	                                     Conventional Commits
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
		fmt.Fprintln(os.Stderr, "usage: commitcheck dco -range BASE..HEAD | commitcheck title -title TITLE")
		return 2
	}
	fs := flag.NewFlagSet("commitcheck "+args[0], flag.ContinueOnError)
	switch args[0] {
	case "dco":
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
	case "title":
		title := fs.String("title", "", "pull request title; default $PR_TITLE")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *title == "" {
			*title = os.Getenv("PR_TITLE")
		}
		if *title == "" {
			fmt.Fprintln(os.Stderr, "commitcheck title: -title or PR_TITLE is required")
			return 2
		}
		if err := checkTitle(*title); err != nil {
			fmt.Fprintf(os.Stderr, "commitcheck title: %q: %v\n", *title, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "commitcheck: unknown command %q\n", args[0])
		return 2
	}
}
