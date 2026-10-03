// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// options are the command-line settings of one gate run.
type options struct {
	dir      string // repository (module) root
	base     string // base ref; empty checks the absolute caps only
	config   string // gate list, relative to dir unless absolute
	summary  string // optional Markdown summary file, appended to
	override bool   // report failures but pass (perf-override approved)
}

// outcome is the result of a gate run.
type outcome int

const (
	outcomePass outcome = iota
	outcomeFail
	outcomeSkip
)

// command runs one external program and returns its standard output; a
// non-zero exit becomes an error carrying the tail of standard error.
func command(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // Runs git, go and built test binaries with arguments the gate composes.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w\n%s%s", name, strings.Join(args, " "), err,
			tail(stdout.Bytes(), 4096), tail(stderr.Bytes(), 4096))
	}
	return stdout.Bytes(), nil
}

// tail returns at most the last n bytes of b.
func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// worktree is a temporary git worktree of the merge base.
type worktree struct {
	repo, dir string
}

// addWorktree checks out rev in a new detached worktree at dir.
func addWorktree(ctx context.Context, repo, dir, rev string) (*worktree, error) {
	if _, err := command(ctx, repo, nil, "git", "worktree", "add", "--detach", "--quiet", dir, rev); err != nil {
		return nil, err
	}
	return &worktree{repo: repo, dir: dir}, nil
}

// remove deletes the worktree and its administrative files. It runs on a
// context detached from cancellation, so an interrupted run still cleans up.
func (w *worktree) remove(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	_, err := command(ctx, w.repo, nil, "git", "worktree", "remove", "--force", w.dir)
	if err != nil {
		rmErr := os.RemoveAll(w.dir)
		_, pruneErr := command(ctx, w.repo, nil, "git", "worktree", "prune")
		if rmErr == nil && pruneErr == nil {
			return nil
		}
		return errors.Join(err, rmErr, pruneErr)
	}
	return nil
}

// buildTest compiles the test binary of pkg inside tree with the release
// flags and toolchain. It returns "" when the tree has no such package or
// the package has no test files.
func buildTest(ctx context.Context, tree, pkg, out, toolchain string) (string, error) {
	if _, err := os.Stat(filepath.Join(tree, filepath.FromSlash(pkg))); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	env := []string{"CGO_ENABLED=0", "GOTOOLCHAIN=" + toolchain}
	if _, err := command(ctx, tree, env, "go", "test", "-c", "-trimpath", "-o", out, pkg); err != nil {
		return "", err
	}
	if _, err := os.Stat(out); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return out, nil
}

// runBench runs one benchmark pass of a test binary in its package directory
// with GOGC=off and a fixed GOMAXPROCS (11 req 64).
func runBench(ctx context.Context, bin, pkgDir string, e entry, cpu int) ([]byte, error) {
	env := []string{"GOGC=off", "GOMAXPROCS=" + strconv.Itoa(cpu)}
	return command(ctx, pkgDir, env, bin,
		"-test.run", "^$", "-test.bench", e.Pattern, "-test.benchtime", e.Benchtime,
		"-test.benchmem", "-test.cpu", strconv.Itoa(cpu), "-test.count", "1")
}

// gate runs the alloc/op gate and writes its report to w.
func gate(ctx context.Context, o options, w io.Writer) (res outcome, err error) {
	cfgPath := o.config
	if !filepath.IsAbs(cfgPath) {
		cfgPath = filepath.Join(o.dir, cfgPath)
	}
	data, err := os.ReadFile(cfgPath) //nolint:gosec // The gate list path is a command-line argument.
	if errors.Is(err, fs.ErrNotExist) {
		_, _ = fmt.Fprintf(w, "benchgate: skip: %s is not present yet\n", o.config)
		return outcomeSkip, nil
	}
	if err != nil {
		return outcomePass, err
	}
	cfg, err := parseConfig(data)
	if err != nil {
		return outcomePass, fmt.Errorf("%s: %w", o.config, err)
	}
	if len(cfg.Benchmarks) == 0 {
		_, _ = fmt.Fprintf(w, "benchgate: skip: %s lists no benchmarks yet\n", o.config)
		return outcomeSkip, nil
	}
	gover, err := command(ctx, o.dir, nil, "go", "env", "GOVERSION")
	if err != nil {
		return outcomePass, err
	}
	toolchain := strings.TrimSpace(string(gover))

	tmp, err := os.MkdirTemp("", "benchgate-")
	if err != nil {
		return outcomePass, err
	}
	defer func() {
		if rmErr := os.RemoveAll(tmp); rmErr != nil && err == nil {
			err = rmErr
		}
	}()

	var baseTree, baseRev string
	if o.base != "" {
		wt, rev, werr := checkoutBase(ctx, o.dir, o.base, filepath.Join(tmp, "base"))
		if werr != nil {
			return outcomePass, werr
		}
		defer func() {
			if rmErr := wt.remove(ctx); rmErr != nil && err == nil {
				err = fmt.Errorf("removing the base worktree: %w", rmErr)
			}
		}()
		baseTree, baseRev = wt.dir, rev
	}

	var results []result
	for i, e := range cfg.Benchmarks {
		rs, err := measure(ctx, cfg, i, e, o.dir, baseTree, filepath.Join(tmp, "bin"), toolchain)
		if err != nil {
			return outcomePass, fmt.Errorf("%s %s: %w", e.Package, e.Pattern, err)
		}
		results = append(results, rs...)
	}
	failed := false
	for _, r := range results {
		failed = failed || r.verdict.failed()
	}

	header := "benchgate: no base, absolute caps only"
	if o.base != "" {
		header = fmt.Sprintf("benchgate: head vs merge base %.12s of %s, %d interleaved runs each, GOGC=off, GOMAXPROCS=%d, threshold %.1f%%",
			baseRev, o.base, cfg.Runs, cfg.GOMAXPROCS, cfg.threshold()*100)
	}
	_, _ = fmt.Fprintln(w, header)
	if err := writeTable(w, results); err != nil {
		return outcomePass, err
	}
	footer := ""
	switch {
	case failed && o.override:
		footer = "Failures accepted: the perf-override label was applied by a maintainer (11 req 66)."
	case failed:
		footer = "A median allocs/op above the base by more than the threshold with four of five pairs slower, a cap exceeded, or a removed benchmark fails the gate; a removal passes once allocgate.json no longer matches it."
	}
	if footer != "" {
		_, _ = fmt.Fprintln(w, footer)
	}
	if o.summary != "" {
		if err := appendSummary(o.summary, results, header, footer); err != nil {
			return outcomePass, err
		}
	}
	if failed && !o.override {
		return outcomeFail, nil
	}
	return outcomePass, nil
}

// checkoutBase checks out the merge base of HEAD and base in a worktree at
// dir and returns it with the merge base commit.
func checkoutBase(ctx context.Context, repo, base, dir string) (*worktree, string, error) {
	out, err := command(ctx, repo, nil, "git", "merge-base", "HEAD", base)
	if err != nil {
		return nil, "", fmt.Errorf("merge base with %s (fetch it, for example actions/checkout with fetch-depth: 0): %w", base, err)
	}
	rev := strings.TrimSpace(string(out))
	wt, err := addWorktree(ctx, repo, dir, rev)
	if err != nil {
		return nil, "", err
	}
	return wt, rev, nil
}

// measure builds and runs one gated set, alternating base and head runs.
func measure(ctx context.Context, cfg *config, i int, e entry, headTree, baseTree, binDir, toolchain string) ([]result, error) {
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		return nil, err
	}
	headBin, err := buildTest(ctx, headTree, e.Package, filepath.Join(binDir, fmt.Sprintf("%03d-head.test", i)), toolchain)
	if err != nil {
		return nil, err
	}
	var baseBin string
	if baseTree != "" {
		baseBin, err = buildTest(ctx, baseTree, e.Package, filepath.Join(binDir, fmt.Sprintf("%03d-base.test", i)), toolchain)
		if err != nil {
			return nil, err
		}
	}
	if headBin == "" && baseBin == "" {
		return nil, errors.New("no test binary: the package does not exist or has no test files")
	}
	base, head := map[string][]sample{}, map[string][]sample{}
	for range cfg.Runs {
		for _, side := range []struct {
			bin, tree string
			into      map[string][]sample
		}{{baseBin, baseTree, base}, {headBin, headTree, head}} {
			if side.bin == "" {
				continue
			}
			out, err := runBench(ctx, side.bin, filepath.Join(side.tree, filepath.FromSlash(e.Package)), e, cfg.GOMAXPROCS)
			if err != nil {
				return nil, err
			}
			if err := parseBench(out, cfg.GOMAXPROCS, side.into); err != nil {
				return nil, err
			}
		}
	}
	if len(base) == 0 && len(head) == 0 {
		return nil, errors.New("the pattern matches no benchmark in the base or the head")
	}
	return evaluate(e.Package, base, head, e.MaxAllocs, cfg.threshold(), baseTree != ""), nil
}

// appendSummary appends the Markdown report to path ($GITHUB_STEP_SUMMARY).
func appendSummary(path string, rs []result, header, footer string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // The summary path is a command-line argument ($GITHUB_STEP_SUMMARY).
	if err != nil {
		return err
	}
	werr := writeMarkdown(f, strings.TrimPrefix(header, "benchgate: "), rs, footer)
	return errors.Join(werr, f.Close())
}
