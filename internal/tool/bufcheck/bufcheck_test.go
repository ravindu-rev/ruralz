// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The test binary doubles as a fake buf: with BUFCHECK_FAKE_BUF set, it acts
// as buf instead of running the tests. BUFCHECK_FAKE_BUF_LOG names a file
// that gets one JSON line per call; BUFCHECK_FAKE_BUF_TREE, a directory that
// buf generate copies to its --output directory; BUFCHECK_FAKE_BUF_EXIT, the
// exit status of buf breaking and buf generate.
func TestMain(m *testing.M) {
	if os.Getenv("BUFCHECK_FAKE_BUF") != "" {
		os.Exit(fakeBuf(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeCall is one recorded fake buf call.
type fakeCall struct {
	Args []string `json:"args"`
	// Against maps each file of the --against directory, slash-separated
	// and relative to it, to its content.
	Against map[string]string `json:"against,omitempty"`
}

func fakeBuf(args []string) int {
	call := fakeCall{Args: args}
	switch {
	case len(args) == 3 && args[0] == "breaking" && args[1] == "--against":
		call.Against = map[string]string{}
		files, err := listFiles(args[2], false)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "fake buf:", err)
			return 1
		}
		for _, f := range files {
			data, err := os.ReadFile(filepath.Join(args[2], filepath.FromSlash(f))) //nolint:gosec // The fake buf reads the directory bufcheck passed.
			if err != nil {
				_, _ = fmt.Fprintln(os.Stderr, "fake buf:", err)
				return 1
			}
			call.Against[f] = string(data)
		}
	case len(args) == 3 && args[0] == "generate" && args[1] == "--output":
		if err := copyTree(os.Getenv("BUFCHECK_FAKE_BUF_TREE"), args[2]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "fake buf:", err)
			return 1
		}
	default:
		_, _ = fmt.Fprintf(os.Stderr, "fake buf: unexpected arguments %q\n", args)
		return 1
	}
	if logPath := os.Getenv("BUFCHECK_FAKE_BUF_LOG"); logPath != "" {
		line, err := json.Marshal(call)
		if err != nil {
			return 1
		}
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // The log path comes from the test.
		if err != nil {
			return 1
		}
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
	code := 0
	if s := os.Getenv("BUFCHECK_FAKE_BUF_EXIT"); s != "" {
		code, _ = strconv.Atoi(s)
	}
	if code == bufFindingsStatus {
		_, _ = fmt.Fprintln(os.Stdout, "api/proto/x/v1/x.proto:1:1:fake breaking change")
	}
	return code
}

func copyTree(src, dst string) error {
	files, err := listFiles(src, false)
	if err != nil {
		return err
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(f))) //nolint:gosec // A fixture tree of the test.
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil { //nolint:gosec // A path below the output directory bufcheck passed.
			return err
		}
		if err := os.WriteFile(target, data, 0o600); err != nil { //nolint:gosec // A path below the output directory bufcheck passed.
			return err
		}
	}
	return nil
}

// fakeBufPath configures the fake buf and returns its path and call log.
func fakeBufPath(t *testing.T, exitCode int, tree string) (string, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "calls.jsonl")
	t.Setenv("BUFCHECK_FAKE_BUF", "1")
	t.Setenv("BUFCHECK_FAKE_BUF_LOG", logPath)
	t.Setenv("BUFCHECK_FAKE_BUF_EXIT", strconv.Itoa(exitCode))
	t.Setenv("BUFCHECK_FAKE_BUF_TREE", tree)
	return exe, logPath
}

func readCalls(t *testing.T, logPath string) []fakeCall {
	t.Helper()
	data, err := os.ReadFile(logPath) //nolint:gosec // The test's own log file.
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []fakeCall
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var c fakeCall
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	return calls
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func runTool(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestReleaseBaselines(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want []string
	}{
		{"no tags", nil, nil},
		{"one release", []string{"v0.1.0"}, []string{"v0.1.0"}},
		{"newest patch of the line", []string{"v0.1.0", "v0.1.2", "v0.1.1"}, []string{"v0.1.2"}},
		{"lines N and N-1", []string{"v0.1.0", "v0.1.1", "v0.2.0"}, []string{"v0.2.0", "v0.1.1"}},
		{"only the two newest lines", []string{"v0.1.3", "v0.2.1", "v0.3.0", "v0.2.0"}, []string{"v0.3.0", "v0.2.1"}},
		{"a major release", []string{"v0.9.4", "v1.0.0", "v0.9.5"}, []string{"v1.0.0", "v0.9.5"}},
		{"numeric order", []string{"v0.9.0", "v0.10.0"}, []string{"v0.10.0", "v0.9.0"}},
		{"only vX.Y.Z tags", []string{"v0.2.0-rc.1", "v1.2", "release-1", "v01.2.3", "v0.1.0", "x", "sdk/go/v0.3.0"}, []string{"v0.1.0"}},
		{"numbers too large", []string{"v99999999999999999999.0.0", "v0.1.0"}, []string{"v0.1.0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := releaseBaselines(tt.tags); !slices.Equal(got, tt.want) {
				t.Errorf("releaseBaselines(%q) = %q, want %q", tt.tags, got, tt.want)
			}
		})
	}
}

// gitRepo is a temporary repository for the breaking tests.
type gitRepo struct {
	t   *testing.T
	dir string
}

func newGitRepo(t *testing.T) gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("no git to run the breaking check: %v", err)
	}
	r := gitRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

// git runs git in the repository, failing the test when it fails.
func (r gitRepo) git(args ...string) {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), "git", append([]string{"-C", r.dir}, args...)...) //nolint:gosec // The test runs git with fixed arguments in a temporary repository.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Jane Doe", "GIT_AUTHOR_EMAIL=jane@example.com",
		"GIT_COMMITTER_NAME=Jane Doe", "GIT_COMMITTER_EMAIL=jane@example.com",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// commit writes files and commits every change.
func (r gitRepo) commit(msg string, files map[string]string) {
	r.t.Helper()
	writeFiles(r.t, r.dir, files)
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
}

const bufYAML = "version: v2\nmodules:\n  - path: api/proto\n"

func TestBreakingPassesAgainstABaselineWithoutProtos(t *testing.T) {
	r := newGitRepo(t)
	r.commit("feat: before the protos", map[string]string{"README.md": "readme\n"})
	r.git("tag", "v0.1.0")
	// The change that adds the first protos, as a working tree change.
	writeFiles(t, r.dir, map[string]string{"buf.yaml": bufYAML, "api/proto/x/v1/x.proto": "syntax = \"proto3\";\n"})
	buf, logPath := fakeBufPath(t, 0, "")
	code, stdout, stderr := runTool(t, "breaking", "-buf", buf, "-main", "main", "-C", r.dir)
	if code != exitPass {
		t.Fatalf("breaking = %d, want %d; stderr:\n%s", code, exitPass, stderr)
	}
	if calls := readCalls(t, logPath); len(calls) != 0 {
		t.Errorf("buf ran %d time(s) against baselines without buf.yaml, want 0: %+v", len(calls), calls)
	}
	if !strings.Contains(stdout, "main, v0.1.0") || !strings.Contains(stdout, "has no buf.yaml") {
		t.Errorf("stdout = %q, want main and v0.1.0 named as one baseline without buf.yaml", stdout)
	}
}

func TestBreakingAgainstMainAndReleaseLines(t *testing.T) {
	r := newGitRepo(t)
	r.commit("feat: first protos", map[string]string{
		"buf.yaml": bufYAML, "api/proto/x/v1/x.proto": "v0.1\n", "docs/notes.md": "not a proto\n",
	})
	r.git("tag", "v0.1.0")
	r.commit("fix: second", map[string]string{"api/proto/x/v1/x.proto": "v0.1.1\n"})
	r.git("tag", "v0.1.1")
	r.commit("feat: third", map[string]string{"api/proto/x/v1/x.proto": "v0.2\n", "api/proto/y/v1/y.proto": "y\n"})
	r.git("tag", "v0.2.0")
	r.commit("feat: unreleased", map[string]string{"api/proto/x/v1/x.proto": "main\n"})
	buf, logPath := fakeBufPath(t, 0, "")
	code, stdout, stderr := runTool(t, "breaking", "-buf", buf, "-main", "main", "-C", r.dir)
	if code != exitPass {
		t.Fatalf("breaking = %d, want %d; stderr:\n%s", code, exitPass, stderr)
	}
	calls := readCalls(t, logPath)
	want := []map[string]string{
		{"buf.yaml": bufYAML, "api/proto/x/v1/x.proto": "main\n", "api/proto/y/v1/y.proto": "y\n"},
		{"buf.yaml": bufYAML, "api/proto/x/v1/x.proto": "v0.2\n", "api/proto/y/v1/y.proto": "y\n"},
		{"buf.yaml": bufYAML, "api/proto/x/v1/x.proto": "v0.1.1\n"},
	}
	if len(calls) != len(want) {
		t.Fatalf("buf ran %d time(s), want %d (main, v0.2.0, v0.1.1): %+v", len(calls), len(want), calls)
	}
	for i, c := range calls {
		if c.Args[0] != "breaking" || len(c.Against) != len(want[i]) {
			t.Errorf("call %d = %+v, want buf breaking against %v", i, c, want[i])
			continue
		}
		for f, content := range want[i] {
			if got, ok := c.Against[f]; !ok || got != content {
				t.Errorf("call %d: against %s = %q (present %t), want %q", i, f, got, ok, content)
			}
		}
	}
	for _, name := range []string{"main", "v0.2.0", "v0.1.1"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("stdout = %q, want baseline %s named", stdout, name)
		}
	}
}

func TestBreakingComparesTheCommittedBytes(t *testing.T) {
	r := newGitRepo(t)
	// A checkout, or git archive, of this tree writes CRLF line endings; the
	// baseline is the committed blobs, byte for byte.
	r.commit("feat: protos", map[string]string{
		".gitattributes": "*.proto text eol=crlf\n", "buf.yaml": bufYAML, "api/proto/x/v1/x.proto": "a\nb\n",
	})
	r.commit("feat: change", map[string]string{"api/proto/x/v1/x.proto": "a\nb\nc\n"})
	buf, logPath := fakeBufPath(t, 0, "")
	if code, _, stderr := runTool(t, "breaking", "-buf", buf, "-main", "main~1", "-C", r.dir); code != exitPass {
		t.Fatalf("breaking = %d, want %d; stderr:\n%s", code, exitPass, stderr)
	}
	calls := readCalls(t, logPath)
	if len(calls) != 1 || calls[0].Against["api/proto/x/v1/x.proto"] != "a\nb\n" {
		t.Errorf("buf calls = %+v, want one against the committed bytes %q", calls, "a\nb\n")
	}
}

func TestBreakingMergesRefsOfOneCommit(t *testing.T) {
	r := newGitRepo(t)
	r.commit("feat: protos", map[string]string{"buf.yaml": bufYAML, "api/proto/x/v1/x.proto": "x\n"})
	r.git("tag", "v0.1.0")
	buf, logPath := fakeBufPath(t, 0, "")
	code, stdout, stderr := runTool(t, "breaking", "-buf", buf, "-main", "main", "-C", r.dir)
	if code != exitPass {
		t.Fatalf("breaking = %d, want %d; stderr:\n%s", code, exitPass, stderr)
	}
	if calls := readCalls(t, logPath); len(calls) != 1 {
		t.Errorf("buf ran %d time(s), want 1 for main and v0.1.0 on one commit", len(calls))
	}
	if !strings.Contains(stdout, "main, v0.1.0") {
		t.Errorf("stdout = %q, want main and v0.1.0 as one baseline", stdout)
	}
}

func TestBreakingReportsBufFindings(t *testing.T) {
	r := newGitRepo(t)
	r.commit("feat: protos", map[string]string{"buf.yaml": bufYAML, "api/proto/x/v1/x.proto": "x\n"})
	buf, _ := fakeBufPath(t, bufFindingsStatus, "")
	code, _, stderr := runTool(t, "breaking", "-buf", buf, "-main", "main", "-C", r.dir)
	if code != exitFindings {
		t.Fatalf("breaking = %d, want %d; stderr:\n%s", code, exitFindings, stderr)
	}
	if !strings.Contains(stderr, "fake breaking change") || !strings.Contains(stderr, "ruralz.control.v2") {
		t.Errorf("stderr = %q, want buf's findings and the versioning hint", stderr)
	}
}

func TestBreakingBufErrorIsNoFinding(t *testing.T) {
	r := newGitRepo(t)
	r.commit("feat: protos", map[string]string{"buf.yaml": bufYAML, "api/proto/x/v1/x.proto": "x\n"})
	buf, _ := fakeBufPath(t, 1, "")
	if code, _, stderr := runTool(t, "breaking", "-buf", buf, "-main", "main", "-C", r.dir); code != exitError {
		t.Errorf("breaking with buf failing = %d, want %d; stderr:\n%s", code, exitError, stderr)
	}
}

func TestBreakingNeedsTheMainBaseline(t *testing.T) {
	r := newGitRepo(t)
	r.commit("feat: protos", map[string]string{"buf.yaml": bufYAML})
	buf, logPath := fakeBufPath(t, 0, "")
	code, _, stderr := runTool(t, "breaking", "-buf", buf, "-main", "origin/main", "-C", r.dir)
	if code != exitError || !strings.Contains(stderr, "origin/main") || !strings.Contains(stderr, "fetch it") {
		t.Errorf("breaking without origin/main = %d, stderr %q; want %d naming the baseline", code, stderr, exitError)
	}
	if calls := readCalls(t, logPath); len(calls) != 0 {
		t.Errorf("buf ran %d time(s) without a main baseline, want 0", len(calls))
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"lint"},
		{"breaking"},
		{"breaking", "-buf", "buf", "-main", ""},
		{"breaking", "-buf", "buf", "extra"},
		{"generate"},
		{"generate", "-buf", "buf", "-out", "../outside"},
		{"generate", "-buf", "buf", "-out", "."},
		{"generate", "-buf", "buf", "-out", filepath.Join(t.TempDir(), "abs")},
	} {
		if code, _, _ := runTool(t, args...); code != exitError {
			t.Errorf("run(%q) = %d, want %d", args, code, exitError)
		}
	}
}

// generatedTree is buf generate's output in the generate tests.
func generatedTree() map[string]string {
	return map[string]string{
		"internal/gen/proto/ruralz/x/v1/x.pb.go":                 "// Code generated by protoc-gen-go. DO NOT EDIT.\npackage xv1\n",
		"internal/gen/proto/ruralz/x/v1/xv1connect/x.connect.go": "// Code generated by protoc-gen-connect-go. DO NOT EDIT.\npackage xv1connect\n",
	}
}

// generateFixture returns a repository holding the given committed files
// and the fake buf's generated tree.
func generateFixture(t *testing.T, committed map[string]string) (repo, tree string) {
	t.Helper()
	repo, tree = t.TempDir(), t.TempDir()
	writeFiles(t, repo, committed)
	writeFiles(t, tree, generatedTree())
	return repo, tree
}

func TestGenerateCurrent(t *testing.T) {
	committed := generatedTree()
	committed["internal/gen/proto/ruralz/x/v1/x_test.go"] = "package xv1\n"
	committed["internal/gen/proto/ruralz/x/v1/testdata/seed"] = "seed\n"
	repo, tree := generateFixture(t, committed)
	buf, logPath := fakeBufPath(t, 0, tree)
	code, _, stderr := runTool(t, "generate", "-buf", buf, "-C", repo)
	if code != exitPass {
		t.Fatalf("generate = %d, want %d; stderr:\n%s", code, exitPass, stderr)
	}
	if calls := readCalls(t, logPath); len(calls) != 1 || calls[0].Args[0] != "generate" {
		t.Errorf("buf calls = %+v, want one buf generate", calls)
	}
}

func TestGenerateCatchesAStaleTree(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string]string)
		want string
	}{
		{
			name: "hand-edited file",
			edit: func(c map[string]string) { c["internal/gen/proto/ruralz/x/v1/x.pb.go"] += "// edited\n" },
			want: "internal/gen/proto/ruralz/x/v1/x.pb.go: differs from what buf generate writes",
		},
		{
			name: "file not committed",
			edit: func(c map[string]string) { delete(c, "internal/gen/proto/ruralz/x/v1/xv1connect/x.connect.go") },
			want: "internal/gen/proto/ruralz/x/v1/xv1connect/x.connect.go: generated by buf generate but not committed",
		},
		{
			name: "file of a deleted proto",
			edit: func(c map[string]string) { c["internal/gen/proto/ruralz/old/v1/old.pb.go"] = "package oldv1\n" },
			want: "internal/gen/proto/ruralz/old/v1/old.pb.go: not written by buf generate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			committed := generatedTree()
			tt.edit(committed)
			repo, tree := generateFixture(t, committed)
			buf, _ := fakeBufPath(t, 0, tree)
			code, _, stderr := runTool(t, "generate", "-buf", buf, "-C", repo)
			if code != exitFindings {
				t.Fatalf("generate = %d, want %d; stderr:\n%s", code, exitFindings, stderr)
			}
			if !strings.Contains(stderr, tt.want) || !strings.Contains(stderr, "make generate") {
				t.Errorf("stderr = %q, want %q and the make generate hint", stderr, tt.want)
			}
		})
	}
}

func TestGenerateRejectsOutputOutsideTheOutDirectories(t *testing.T) {
	repo, tree := generateFixture(t, generatedTree())
	writeFiles(t, tree, map[string]string{"sdk/rust/src/abi.rs": "// Code generated by pdkgen. DO NOT EDIT.\n"})
	buf, _ := fakeBufPath(t, 0, tree)
	code, _, stderr := runTool(t, "generate", "-buf", buf, "-C", repo)
	if code != exitError || !strings.Contains(stderr, "sdk/rust/src/abi.rs") {
		t.Errorf("generate = %d, stderr %q; want %d naming the file outside -out", code, stderr, exitError)
	}
	// Naming its directory takes it in.
	writeFiles(t, repo, map[string]string{"sdk/rust/src/abi.rs": "// Code generated by pdkgen. DO NOT EDIT.\n"})
	if code, _, stderr := runTool(t, "generate", "-buf", buf, "-C", repo, "-out", "internal/gen/proto", "-out", "sdk/rust/src"); code != exitPass {
		t.Errorf("generate with both -out directories = %d, want %d; stderr:\n%s", code, exitPass, stderr)
	}
}

func TestGenerateWriteMakesTheTreeCurrent(t *testing.T) {
	committed := generatedTree()
	committed["internal/gen/proto/ruralz/x/v1/x.pb.go"] = "stale\n"
	delete(committed, "internal/gen/proto/ruralz/x/v1/xv1connect/x.connect.go")
	committed["internal/gen/proto/ruralz/old/v1/old.pb.go"] = "package oldv1\n"
	committed["internal/gen/proto/ruralz/x/v1/x_test.go"] = "package xv1\n"
	committed["internal/gen/proto/ruralz/kept/v1/kept_test.go"] = "package keptv1\n"
	repo, tree := generateFixture(t, committed)
	buf, _ := fakeBufPath(t, 0, tree)
	if code, _, stderr := runTool(t, "generate", "-write", "-buf", buf, "-C", repo); code != exitPass {
		t.Fatalf("generate -write = %d, want %d; stderr:\n%s", code, exitPass, stderr)
	}
	for f, want := range generatedTree() {
		got, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(f))) //nolint:gosec // A file of the test's repository.
		if err != nil || string(got) != want {
			t.Errorf("after -write, %s = %q (%v), want %q", f, got, err, want)
		}
	}
	for _, gone := range []string{"internal/gen/proto/ruralz/old/v1/old.pb.go", "internal/gen/proto/ruralz/old"} {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(gone))); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("after -write, %s exists (%v), want it removed", gone, err)
		}
	}
	for _, kept := range []string{"internal/gen/proto/ruralz/x/v1/x_test.go", "internal/gen/proto/ruralz/kept/v1/kept_test.go"} {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(kept))); err != nil {
			t.Errorf("after -write, test file %s: %v, want it kept", kept, err)
		}
	}
	if code, _, stderr := runTool(t, "generate", "-buf", buf, "-C", repo); code != exitPass {
		t.Errorf("generate after -write = %d, want %d; stderr:\n%s", code, exitPass, stderr)
	}
}

func TestGenerateWriteKeepsTheOutDirectory(t *testing.T) {
	// Every generated file is stale, so the out directory empties.
	repo, tree := t.TempDir(), t.TempDir()
	writeFiles(t, repo, map[string]string{"internal/gen/proto/ruralz/old/v1/old.pb.go": "package oldv1\n"})
	buf, _ := fakeBufPath(t, 0, tree)
	if code, _, stderr := runTool(t, "generate", "-write", "-buf", buf, "-C", repo); code != exitPass {
		t.Fatalf("generate -write = %d, want %d; stderr:\n%s", code, exitPass, stderr)
	}
	if fi, err := os.Stat(filepath.Join(repo, "internal", "gen", "proto")); err != nil || !fi.IsDir() {
		t.Errorf("after -write, internal/gen/proto: %v, want the -out directory kept", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "internal", "gen", "proto", "ruralz")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after -write, internal/gen/proto/ruralz exists (%v), want the emptied directory removed", err)
	}
}

func TestGenerateBufErrorIsNoFinding(t *testing.T) {
	repo, tree := generateFixture(t, generatedTree())
	buf, _ := fakeBufPath(t, 1, tree)
	if code, _, stderr := runTool(t, "generate", "-buf", buf, "-C", repo); code != exitError {
		t.Errorf("generate with buf failing = %d, want %d; stderr:\n%s", code, exitError, stderr)
	}
}
