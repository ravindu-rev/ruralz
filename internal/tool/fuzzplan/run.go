// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// runner executes the go command; tests substitute it.
type runner interface {
	// output runs go with args in dir and returns its standard output.
	output(ctx context.Context, dir string, args ...string) ([]byte, error)
	// stream runs go with args in dir, copying its output to w.
	stream(ctx context.Context, dir string, w io.Writer, args ...string) error
}

// goRunner runs the real go command.
type goRunner struct{}

func (goRunner) output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // Runs the go command with arguments the plan composes.
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, tailOf(stderr.Bytes(), 8192))
	}
	return out, nil
}

func (goRunner) stream(ctx context.Context, dir string, w io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // Runs the go command with arguments the plan composes.
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = w, w
	cmd.WaitDelay = 10 * time.Second
	return cmd.Run()
}

func tailOf(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// listTargets lists the fuzz targets of the packages matched by patterns.
func listTargets(ctx context.Context, r runner, dir string, patterns []string) ([]target, error) {
	out, err := r.output(ctx, dir, append([]string{"test", "-json", "-list", "^Fuzz"}, patterns...)...)
	ts, perr := parseList(out)
	if perr != nil {
		return nil, errors.Join(perr, err)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errList, err)
	}
	return ts, nil
}

// Results of one target.
const (
	resultPass    = "pass"
	resultCrasher = "crasher" // the fuzzer found a failing input
	resultFail    = "fail"    // failed without a new input (seed failure, build error)
)

// targetReport is the outcome of one fuzz run.
type targetReport struct {
	Package  string   `json:"package"`
	Name     string   `json:"name"`
	Result   string   `json:"result"`
	Seconds  float64  `json:"seconds"`
	Crashers []string `json:"crashers,omitempty"` // repository-relative new corpus files
	Output   string   `json:"output,omitempty"`   // tail of the go test output on failure
}

// report is the JSON report of a shard run, read by the nightly workflow
// to open fuzz-crasher issues (11 req 28).
type report struct {
	Format   string         `json:"format"` // "ruralz.fuzzplan.v1"
	Shard    int            `json:"shard"`  // -1: every target
	Shards   int            `json:"shards"`
	FuzzTime string         `json:"fuzztime"`
	Targets  []targetReport `json:"targets"`
}

// failed reports whether any target failed.
func (r *report) failed() bool {
	return slices.ContainsFunc(r.Targets, func(t targetReport) bool { return t.Result != resultPass })
}

// syncWriter serializes writes from the go command's two output pipes.
type syncWriter struct {
	mu sync.Mutex
	w  []io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.w {
		if _, err := w.Write(p); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// ringBuffer keeps the last max bytes written to it.
type ringBuffer struct {
	buf []byte
	max int
}

func (b *ringBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

// corpusFiles lists the files of <pkgDir>/testdata/fuzz/<name>, relative
// to the repository root.
func corpusFiles(root, pkgDir, name string) ([]string, error) {
	dir := filepath.Join(pkgDir, "testdata", "fuzz", name)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		rel, err := filepath.Rel(root, filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}

// packageDirs maps import paths to directories with `go list`.
func packageDirs(ctx context.Context, r runner, dir string, ts []target) (map[string]string, error) {
	var pkgs []string
	for _, t := range ts {
		if !slices.Contains(pkgs, t.Package) {
			pkgs = append(pkgs, t.Package)
		}
	}
	out, err := r.output(ctx, dir, append([]string{"list", "-f", "{{.ImportPath}}\t{{.Dir}}"}, pkgs...)...)
	if err != nil {
		return nil, err
	}
	dirs := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if p, d, ok := strings.Cut(line, "\t"); ok {
			dirs[p] = d
		}
	}
	for _, p := range pkgs {
		if dirs[p] == "" {
			return nil, fmt.Errorf("go list returned no directory for %s", p)
		}
	}
	return dirs, nil
}

// runTargets fuzzes each target in turn for fuzztime (11 req 28:
// `go test -run '^$' -fuzz '^<Name>$' -fuzztime 15m <pkg>`), streaming the
// output to w. A failing target does not stop the shard; its new
// testdata/fuzz inputs are recorded as crashers.
func runTargets(ctx context.Context, r runner, clk clock.Clock, dir string, ts []target, fuzztime string, w io.Writer) ([]targetReport, error) {
	if len(ts) == 0 {
		return nil, nil
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	dirs, err := packageDirs(ctx, r, dir, ts)
	if err != nil {
		return nil, err
	}
	out := make([]targetReport, 0, len(ts))
	for i, t := range ts {
		before, err := corpusFiles(root, dirs[t.Package], t.Name)
		if err != nil {
			return out, err
		}
		_, _ = fmt.Fprintf(w, "fuzzplan: [%d/%d] %s %s for %s\n", i+1, len(ts), t.Package, t.Name, fuzztime)
		tail := &ringBuffer{max: 16 << 10}
		start := clk.Now()
		runErr := r.stream(ctx, dir, &syncWriter{w: []io.Writer{w, tail}},
			"test", "-run", "^$", "-fuzz", "^"+t.Name+"$", "-fuzztime", fuzztime, t.Package)
		tr := targetReport{Package: t.Package, Name: t.Name, Result: resultPass, Seconds: clk.Since(start).Seconds()}
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if runErr != nil {
			after, err := corpusFiles(root, dirs[t.Package], t.Name)
			if err != nil {
				return out, err
			}
			for _, f := range after {
				if !slices.Contains(before, f) {
					tr.Crashers = append(tr.Crashers, f)
				}
			}
			tr.Result = resultFail
			if len(tr.Crashers) > 0 {
				tr.Result = resultCrasher
			}
			tr.Output = string(tail.buf)
		}
		_, _ = fmt.Fprintf(w, "fuzzplan: %s %s: %s\n", t.Package, t.Name, tr.Result)
		out = append(out, tr)
	}
	return out, nil
}

// writeReport writes the shard report as indented JSON.
func writeReport(path string, r *report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
