// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// baseline is one commit buf breaking compares against.
type baseline struct {
	names  []string // the refs that name it, such as origin/main and v0.2.0
	commit string
}

func (b baseline) String() string {
	short := b.commit
	if len(short) > 12 {
		short = short[:12]
	}
	return strings.Join(b.names, ", ") + " (" + short + ")"
}

// breaking runs buf breaking against every baseline and reports whether
// none found a breaking change.
func (c checker) breaking(ctx context.Context, mainRef string) (bool, error) {
	bases, err := c.baselines(ctx, mainRef)
	if err != nil {
		return false, err
	}
	ok := true
	for _, b := range bases {
		pass, err := c.breakingAgainst(ctx, b)
		if err != nil {
			return false, fmt.Errorf("against %s: %w", b, err)
		}
		ok = ok && pass
	}
	return ok, nil
}

// baselines resolves the main ref and the release tags to commits, merging
// refs that name one commit, main first.
func (c checker) baselines(ctx context.Context, mainRef string) ([]baseline, error) {
	commit, err := c.resolve(ctx, mainRef)
	if err != nil {
		return nil, fmt.Errorf("baseline %s: %w; fetch it or name another with -main", mainRef, err)
	}
	bases := []baseline{{names: []string{mainRef}, commit: commit}}
	out, err := c.git(ctx, "tag", "--list")
	if err != nil {
		return nil, err
	}
	for _, tag := range releaseBaselines(strings.Fields(string(out))) {
		commit, err := c.resolve(ctx, tag)
		if err != nil {
			return nil, fmt.Errorf("baseline %s: %w", tag, err)
		}
		if i := slices.IndexFunc(bases, func(b baseline) bool { return b.commit == commit }); i >= 0 {
			bases[i].names = append(bases[i].names, tag)
			continue
		}
		bases = append(bases, baseline{names: []string{tag}, commit: commit})
	}
	return bases, nil
}

// release is a parsed vX.Y.Z release tag.
type release struct {
	tag                 string
	major, minor, patch int
}

// releaseBaselines picks, from a repository's tags, the newest vX.Y.Z tag
// of the newest release line and of the line before it: the minors N and
// N-1 of the skew window (docs/engineering/04-release-versioning-and-compatibility.md
// "Control Stream versioning"). Only vX.Y.Z tags are releases.
func releaseBaselines(tags []string) []string {
	re := regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	var rels []release
	for _, t := range tags {
		m := re.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		r := release{tag: t}
		var errs [3]error
		r.major, errs[0] = strconv.Atoi(m[1])
		r.minor, errs[1] = strconv.Atoi(m[2])
		r.patch, errs[2] = strconv.Atoi(m[3])
		if errors.Join(errs[:]...) != nil {
			continue // a number too large for an int is no release
		}
		rels = append(rels, r)
	}
	// Newest first.
	slices.SortFunc(rels, func(a, b release) int {
		if a.major != b.major {
			return b.major - a.major
		}
		if a.minor != b.minor {
			return b.minor - a.minor
		}
		return b.patch - a.patch
	})
	var out []string
	for i, r := range rels {
		if i > 0 && r.major == rels[i-1].major && r.minor == rels[i-1].minor {
			continue // an older patch of a line already taken
		}
		out = append(out, r.tag)
		if len(out) == 2 {
			break
		}
	}
	return out
}

// breakingAgainst runs buf breaking against one baseline; a baseline without
// buf.yaml has no protos and passes.
func (c checker) breakingAgainst(ctx context.Context, b baseline) (bool, error) {
	files, err := c.git(ctx, "ls-tree", "-r", "-z", "--name-only", b.commit)
	if err != nil {
		return false, err
	}
	var paths []string
	hasConfig := false
	for _, p := range strings.Split(string(files), "\x00") {
		switch {
		case p == "buf.yaml":
			hasConfig = true
			paths = append(paths, p)
		case p == "buf.lock" || strings.HasSuffix(p, ".proto"):
			paths = append(paths, p)
		}
	}
	if !hasConfig {
		_, _ = fmt.Fprintf(c.stdout, "bufcheck breaking: %s has no buf.yaml, so it has no protos: every proto is new against it\n", b)
		return true, nil
	}
	dir, err := os.MkdirTemp("", "bufcheck-baseline-")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := c.exportBlobs(ctx, b.commit, paths, dir); err != nil {
		return false, fmt.Errorf("export the baseline: %w", err)
	}
	var out bytes.Buffer
	//nolint:gosec // buf is the pinned binary make passes, run with fixed arguments.
	cmd := exec.CommandContext(ctx, c.buf, "breaking", "--against", dir)
	cmd.Dir = c.root
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		_, _ = fmt.Fprintf(c.stdout, "bufcheck breaking: no breaking change against %s\n", b)
		return true, nil
	case errors.As(err, &ee) && ee.ExitCode() == bufFindingsStatus:
		_, _ = fmt.Fprintf(c.stderr, "bufcheck breaking: breaking changes against %s:\n%s", b, out.String())
		return false, nil
	default:
		return false, fmt.Errorf("%w: buf breaking: %w: %s", errBuf, err, strings.TrimSpace(out.String()))
	}
}

// resolve names the commit a ref points at.
func (c checker) resolve(ctx context.Context, ref string) (string, error) {
	out, err := c.git(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("no commit %q", ref)
	}
	return strings.TrimSpace(string(out)), nil
}

// git runs git in the repository and returns its standard output.
func (c checker) git(ctx context.Context, args ...string) ([]byte, error) {
	//nolint:gosec // The arguments are fixed git subcommands, refs and paths, passed without a shell.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", c.root}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// exportBlobs writes the committed bytes of paths at commit below dir. It
// reads the blobs with git cat-file, which, unlike a checkout or git
// archive, converts no line endings.
func (c checker) exportBlobs(ctx context.Context, commit string, paths []string, dir string) error {
	var in bytes.Buffer
	for _, p := range paths {
		if !filepath.IsLocal(filepath.FromSlash(p)) || strings.ContainsAny(p, "\n\r") {
			return fmt.Errorf("path %q of %s leaves the tree", p, commit)
		}
		in.WriteString(commit + ":" + p + "\n")
	}
	//nolint:gosec // A fixed git subcommand; the object names go to its standard input.
	cmd := exec.CommandContext(ctx, "git", "-C", c.root, "cat-file", "--batch")
	cmd.Stdin = &in
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	r := bufio.NewReader(bytes.NewReader(out))
	for _, p := range paths {
		header, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("git cat-file: no answer for %s", p)
		}
		// Each answer is "<object> blob <size>\n<content>\n".
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[1] != "blob" {
			return fmt.Errorf("git cat-file: %s: %q", p, strings.TrimSpace(header))
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 {
			return fmt.Errorf("git cat-file: %s: size %q", p, fields[2])
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return fmt.Errorf("git cat-file: %s: %w", p, err)
		}
		if b, err := r.ReadByte(); err != nil || b != '\n' {
			return fmt.Errorf("git cat-file: %s: no newline after the content", p)
		}
		target := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}
