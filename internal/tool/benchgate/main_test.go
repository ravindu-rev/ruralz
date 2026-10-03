// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs git in dir with a fixed identity and fails the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=bench", "-c", "user.email=bench@example.com", "-c", "commit.gpgsign=false"}, args...)
	out, err := exec.CommandContext(t.Context(), "git", full...).CombinedOutput() //nolint:gosec // Test runs git with fixed arguments in a temporary repository.
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// allocSource is a package whose benchmark makes n heap allocations per
// operation.
func allocSource(n int) string {
	return fmt.Sprintf(`package p

var sink *int

// N is the number of allocations per operation.
const N = %d

// Alloc allocates N times.
func Alloc() {
	for range N {
		sink = new(int)
	}
}
`, n)
}

const benchSource = `package p

import "testing"

func BenchmarkAlloc(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Alloc()
	}
}
`

// newRepo creates a module repository whose committed base allocates
// baseAllocs times per operation; the working tree is the head.
func newRepo(t *testing.T, baseAllocs int) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "--initial-branch=main")
	write(t, filepath.Join(dir, "go.mod"), "module example.com/bg\n\ngo 1.26\n")
	write(t, filepath.Join(dir, "p", "p.go"), allocSource(baseAllocs))
	write(t, filepath.Join(dir, "p", "p_test.go"), benchSource)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "--quiet", "-m", "base")
	return dir
}

func writeGateList(t *testing.T, dir, body string) {
	t.Helper()
	write(t, filepath.Join(dir, "test", "bench", "allocgate.json"), body)
}

// assertClean checks that no worktree and no temporary directory survives a
// run (11 test plan item 1: worktree cleanup on every exit path).
func assertClean(t *testing.T, dir, tmp string) {
	t.Helper()
	list := git(t, dir, "worktree", "list", "--porcelain")
	if n := strings.Count(list, "worktree "); n != 1 {
		t.Errorf("worktrees after the run: %d\n%s", n, list)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "benchgate-") {
			t.Errorf("temporary directory %s left behind", e.Name())
		}
	}
}

// TestRunEndToEnd builds and runs real test binaries from a base worktree
// and the working tree (11 req 64, 65, 66; test plan item 1).
func TestRunEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds test binaries")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	const list = `{"runs":2,"benchmarks":[{"package":"./p","pattern":"^BenchmarkAlloc$","benchtime":"50x"}]}`
	const capped = `{"runs":2,"benchmarks":[{"package":"./p","pattern":"^BenchmarkAlloc$","benchtime":"50x","maxAllocs":30}]}`
	for _, tc := range []struct {
		name       string
		headAllocs int
		gateList   string
		headSource string // replaces p.go when set
		args       []string
		wantCode   int
		wantOut    []string
	}{
		{name: "unchanged passes", headAllocs: 30, gateList: list, args: []string{"-base", "main"}, wantCode: 0, wantOut: []string{"BenchmarkAlloc", "30", "pass"}},
		{name: "one extra allocation fails", headAllocs: 31, gateList: list, args: []string{"-base", "main"}, wantCode: 1, wantOut: []string{"+3.3%", "FAIL: regression"}},
		{name: "override passes", headAllocs: 31, gateList: list, args: []string{"-base", "main", "-override"}, wantCode: 0, wantOut: []string{"FAIL: regression", "Failures accepted"}},
		{name: "no base checks the cap", headAllocs: 31, gateList: capped, args: []string{"-base", ""}, wantCode: 1, wantOut: []string{"absolute caps only", "FAIL: above cap"}},
		{name: "head build failure is a tool error", headSource: "package p\n\nfunc Alloc() { undefined() }\n", gateList: list, args: []string{"-base", "main"}, wantCode: 2},
		{name: "unknown base is a tool error", headAllocs: 30, gateList: list, args: []string{"-base", "no-such-ref"}, wantCode: 2},
		{name: "missing gate list skips", headAllocs: 30, args: []string{"-base", "main"}, wantCode: 0, wantOut: []string{"skip"}},
		{name: "empty gate list skips", headAllocs: 30, gateList: `{"benchmarks":[]}`, args: []string{"-base", "main"}, wantCode: 0, wantOut: []string{"skip"}},
		{name: "bad gate list is a tool error", headAllocs: 30, gateList: `{"benchmarks":[{"package":"p"}]}`, args: []string{"-base", "main"}, wantCode: 2},
		{name: "missing package is a tool error", headAllocs: 30, gateList: `{"runs":1,"benchmarks":[{"package":"./nothere","pattern":"."}]}`, args: []string{"-base", "main"}, wantCode: 2},
		{name: "pattern matching nothing is a tool error", headAllocs: 30, gateList: `{"runs":1,"benchmarks":[{"package":"./p","pattern":"^BenchmarkNone$","benchtime":"1x"}]}`, args: []string{"-base", "main"}, wantCode: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newRepo(t, 30)
			src := tc.headSource
			if src == "" {
				src = allocSource(tc.headAllocs)
			}
			write(t, filepath.Join(dir, "p", "p.go"), src)
			if tc.gateList != "" {
				writeGateList(t, dir, tc.gateList)
			}
			summary := filepath.Join(t.TempDir(), "summary.md")
			var stdout, stderr bytes.Buffer
			args := append([]string{"-C", dir, "-summary", summary}, tc.args...)
			code := run(context.Background(), args, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.wantCode, stdout.String(), stderr.String())
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
				}
			}
			if tc.wantCode != 2 && !strings.Contains(stdout.String(), "skip") {
				md, err := os.ReadFile(summary) //nolint:gosec // Test reads its own temporary file.
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(md), "| `./p` | `BenchmarkAlloc` |") {
					t.Errorf("summary lacks the table:\n%s", md)
				}
			}
			assertClean(t, dir, tmp)
		})
	}
}

// TestRemovedBenchmark: a benchmark the base has and the head lost fails
// until the gate list stops matching it (11 req 65).
func TestRemovedBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("builds test binaries")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir := newRepo(t, 3)
	write(t, filepath.Join(dir, "p", "p_test.go"), strings.Replace(benchSource, "BenchmarkAlloc", "BenchmarkRenamed", 1))
	writeGateList(t, dir, `{"runs":1,"benchmarks":[{"package":"./p","pattern":".","benchtime":"10x"}]}`)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-C", dir, "-base", "main"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"BenchmarkAlloc", "FAIL: removed", "BenchmarkRenamed", "new (no base)"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
	writeGateList(t, dir, `{"runs":1,"benchmarks":[{"package":"./p","pattern":"^BenchmarkRenamed$","benchtime":"10x"}]}`)
	stdout.Reset()
	if code := run(context.Background(), []string{"-C", dir, "-base", "main"}, &stdout, &stderr); code != 0 {
		t.Fatalf("after the gate list change: exit %d, want 0\n%s%s", code, stdout.String(), stderr.String())
	}
	assertClean(t, dir, tmp)
}

// TestWorktreeRemoveAfterCancel: cleanup runs even when the run's context
// is already canceled (an interrupted gate).
func TestWorktreeRemoveAfterCancel(t *testing.T) {
	dir := newRepo(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	wt, err := addWorktree(ctx, dir, filepath.Join(t.TempDir(), "base"), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := wt.remove(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt.dir); !os.IsNotExist(err) {
		t.Errorf("worktree directory still exists: %v", err)
	}
	if n := strings.Count(git(t, dir, "worktree", "list", "--porcelain"), "worktree "); n != 1 {
		t.Errorf("worktrees after remove: %d", n)
	}
}

// TestWorktreeRemoveFallback: when git cannot remove the worktree (its
// directory was deleted behind git's back), remove prunes it.
func TestWorktreeRemoveFallback(t *testing.T) {
	dir := newRepo(t, 1)
	wt, err := addWorktree(context.Background(), dir, filepath.Join(t.TempDir(), "base"), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wt.dir); err != nil {
		t.Fatal(err)
	}
	if err := wt.remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(git(t, dir, "worktree", "list", "--porcelain"), "worktree "); n != 1 {
		t.Errorf("worktrees after remove: %d", n)
	}
}

func TestRunUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("unknown flag: exit %d, want 2", code)
	}
	if code := run(context.Background(), []string{"extra"}, &stdout, &stderr); code != 2 {
		t.Errorf("extra argument: exit %d, want 2", code)
	}
	if code := run(context.Background(), []string{"-base", "--output=x"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "is not a ref") {
		t.Errorf("option-like base: exit %d, want 2\n%s", code, stderr.String())
	}
}
