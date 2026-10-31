// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// gitRunner runs git in a directory and returns its standard output; tests
// replace it.
type gitRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// runGit is the gitRunner that executes git.
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	//nolint:gosec // Arguments are fixed, a validated commit, or a path after --.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// commitPattern matches a full lower-case SHA-1 commit, as GitHub names
// commits.
func commitPattern() *regexp.Regexp { return regexp.MustCompile(`^[0-9a-f]{40}$`) }
