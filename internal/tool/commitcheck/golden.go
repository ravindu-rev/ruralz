// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// corpusDir is the golden corpus of the configuration conformance suite
// (docs/engineering/03-testing-and-quality-strategy.md "Golden tests"):
// its entries are append-only, and every release reproduces the files the
// newest release tag holds, byte for byte.
const corpusDir = "test/conformance/config"

// releaseVersion is a parsed release tag, vMAJOR.MINOR.PATCH.
type releaseVersion struct {
	tag                 string
	major, minor, patch int
}

// newestRelease returns the newest release tag among tags: the highest
// vMAJOR.MINOR.PATCH without leading zeros, never a release candidate's
// (vX.Y.Z-rc.N); "" when there is none. Patch releases are tagged on
// release-X.Y branches, so the newest tag need not be reachable from the
// range under check; the corpus regeneration refuses the same files
// (internal/testkit/corpus Held).
func newestRelease(tags []string) string {
	re := regexp.MustCompile(`^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)
	var best releaseVersion
	for _, t := range tags {
		m := re.FindStringSubmatch(strings.TrimSpace(t))
		if m == nil {
			continue
		}
		v := releaseVersion{tag: m[0]}
		v.major, _ = strconv.Atoi(m[1])
		v.minor, _ = strconv.Atoi(m[2])
		v.patch, _ = strconv.Atoi(m[3])
		if best.tag == "" || cmp.Or(cmp.Compare(v.major, best.major), cmp.Compare(v.minor, best.minor), cmp.Compare(v.patch, best.patch)) > 0 {
			best = v
		}
	}
	return best.tag
}

// splitRange splits a revision range BASE..HEAD or BASE...HEAD into its
// two revisions; both are required.
func splitRange(rng string) (base, head string, err error) {
	sep := ".."
	if strings.Contains(rng, "...") {
		sep = "..."
	}
	base, head, ok := strings.Cut(rng, sep)
	if !ok || base == "" || head == "" || strings.Contains(head, "..") {
		return "", "", fmt.Errorf("range %q is not BASE..HEAD", rng)
	}
	return base, head, nil
}

// corpusChange is one corpus file a range changes: its git status letter
// (A added, M modified, D deleted, T type changed) and its slash path from
// the repository root.
type corpusChange struct {
	status byte
	path   string
}

// gitOutput runs git in dir and returns its standard output; a failure
// carries git's standard error.
func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	//nolint:gosec // The arguments are built here and passed without a shell.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// gitHeld returns the newest release tag of the repository at dir and the
// corpus files it holds, by slash path from the repository root; before
// the first release the tag is "" and no file is held.
func gitHeld(ctx context.Context, dir string) (string, map[string]bool, error) {
	out, err := gitOutput(ctx, dir, "tag", "--list", "v*")
	if err != nil {
		return "", nil, err
	}
	tag := newestRelease(strings.Split(string(out), "\n"))
	held := map[string]bool{}
	if tag == "" {
		return "", held, nil
	}
	out, err = gitOutput(ctx, dir, "ls-tree", "-r", "-z", "--name-only", "refs/tags/"+tag, "--", corpusDir+"/")
	if err != nil {
		return "", nil, err
	}
	for name := range strings.SplitSeq(string(out), "\x00") {
		if name != "" {
			held[name] = true
		}
	}
	return tag, held, nil
}

// gitCorpusChanges returns the corpus files that head changes since its
// merge base with base, the changes a pull request makes, with renames
// reported as a deletion and an addition.
func gitCorpusChanges(ctx context.Context, dir, base, head string) ([]corpusChange, error) {
	out, err := gitOutput(ctx, dir, "diff", "--no-renames", "--no-ext-diff", "--name-status", "-z", base+"..."+head, "--", corpusDir+"/")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(fields) == 1 && fields[0] == "" {
		return nil, nil
	}
	if len(fields)%2 != 0 {
		return nil, errors.New("git diff: unexpected --name-status output")
	}
	changes := make([]corpusChange, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		if fields[i] == "" {
			return nil, errors.New("git diff: empty status")
		}
		changes = append(changes, corpusChange{status: fields[i][0], path: fields[i+1]})
	}
	return changes, nil
}

// checkGolden returns one problem per change that modifies, deletes or
// changes the type of a file the release tag holds; adding files, entries
// and schema levels is how the corpus grows.
func checkGolden(tag string, held map[string]bool, changes []corpusChange) []string {
	var problems []string
	for _, c := range changes {
		if !held[c.path] {
			continue
		}
		var what string
		switch c.status {
		case 'A':
			continue
		case 'D':
			what = "deletes"
		case 'T':
			what = "changes the type of"
		default:
			what = "changes"
		}
		problems = append(problems, fmt.Sprintf("%s: the range %s it, but release %s holds it", c.path, what, tag))
	}
	return problems
}
