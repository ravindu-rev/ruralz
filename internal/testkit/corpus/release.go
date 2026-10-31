// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package corpus

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ErrNoGit reports that the repository's history cannot be read: git is
// not installed or the directory is not a Git work tree.
var ErrNoGit = errors.New("corpus: no Git history")

// version is a parsed release tag, vMAJOR.MINOR.PATCH.
type version struct {
	tag                 string
	major, minor, patch int
}

// parseRelease parses a release tag, vMAJOR.MINOR.PATCH without leading
// zeros; a release candidate's tag (vX.Y.Z-rc.N) and any other name is not
// one.
func parseRelease(tag string) (version, bool) {
	m := regexp.MustCompile(`^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`).FindStringSubmatch(tag)
	if m == nil {
		return version{}, false
	}
	v := version{tag: tag}
	v.major, _ = strconv.Atoi(m[1])
	v.minor, _ = strconv.Atoi(m[2])
	v.patch, _ = strconv.Atoi(m[3])
	return v, true
}

// NewestRelease returns the newest release tag among tags: the highest
// vMAJOR.MINOR.PATCH, never a release candidate's; "" when there is none.
// Release tags of patches sit on release-X.Y branches, so the newest one
// need not be reachable from the commit under test.
func NewestRelease(tags []string) string {
	var best version
	found := false
	for _, t := range tags {
		v, ok := parseRelease(strings.TrimSpace(t))
		if !ok {
			continue
		}
		if !found || cmp.Or(cmp.Compare(v.major, best.major), cmp.Compare(v.minor, best.minor), cmp.Compare(v.patch, best.patch)) > 0 {
			best, found = v, true
		}
	}
	return best.tag
}

// git runs git in dir and returns its standard output; a failure carries
// git's standard error.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	//nolint:gosec // The arguments are built here and passed without a shell.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.Is(err, exec.ErrNotFound) || errors.As(err, &ee) && strings.Contains(stderr.String(), "not a git repository") {
			return nil, fmt.Errorf("%w: %w: %s", ErrNoGit, err, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("corpus: git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Held returns the newest release tag of the repository at repo and the
// corpus files that tag holds, by their slash paths relative to the
// repository root (Dir/...): the files no regeneration may rewrite and no
// change may alter or delete (docs/engineering/03-testing-and-quality-
// strategy.md "Golden tests"). Before the first release the tag is "" and
// no file is held. The error wraps ErrNoGit when the history cannot be
// read.
func Held(ctx context.Context, repo string) (tag string, held map[string]bool, err error) {
	out, err := git(ctx, repo, "tag", "--list", "v*")
	if err != nil {
		return "", nil, err
	}
	tag = NewestRelease(strings.Split(string(out), "\n"))
	held = map[string]bool{}
	if tag == "" {
		return "", held, nil
	}
	out, err = git(ctx, repo, "ls-tree", "-r", "-z", "--name-only", tag, "--", Dir+"/")
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
