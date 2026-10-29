// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// gitRunner runs git in a directory and returns its standard output; tests
// replace it.
type gitRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// runGit is the gitRunner that executes git.
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	//nolint:gosec // Arguments are fixed or validated refs, passed after --end-of-options where git reads them.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// checkRef rejects a ref that git could read as an option; refs can come
// from workflow inputs.
func checkRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\n\x00") {
		return fmt.Errorf("%q is not a Git ref", ref)
	}
	return nil
}

// resolveCommit returns the full commit a ref names.
func resolveCommit(ctx context.Context, git gitRunner, dir, ref string) (string, error) {
	if err := checkRef(ref); err != nil {
		return "", err
	}
	out, err := git(ctx, dir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(out))
	if err := checkCommit(commit); err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", ref, err)
	}
	return commit, nil
}

// commitTime returns a commit's committer time in UTC, the timestamp of
// every archive entry, so a rebuild of the commit reproduces the archives.
func commitTime(ctx context.Context, git gitRunner, dir, commit string) (time.Time, error) {
	out, err := git(ctx, dir, "show", "-s", "--format=%ct", "--end-of-options", commit)
	if err != nil {
		return time.Time{}, err
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, fmt.Errorf("commit time of %s: %q is not a Unix time", commit, out)
	}
	return time.Unix(secs, 0).UTC(), nil
}
