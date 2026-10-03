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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// listEvents renders `go test -json -list` events: one output event per
// name, then the package's ok line, as test2json emits them.
func listEvents(pkgs map[string][]string) string {
	names := make([]string, 0, len(pkgs))
	for p := range pkgs {
		names = append(names, p)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, p := range names {
		fmt.Fprintf(&b, `{"Action":"start","Package":%q}`+"\n", p)
		for _, n := range pkgs[p] {
			fmt.Fprintf(&b, `{"Action":"output","Package":%q,"Output":%q}`+"\n", p, n+"\n")
		}
		fmt.Fprintf(&b, `{"Action":"output","Package":%q,"Output":%q}`+"\n", p, "ok  \t"+p+"\t0.002s\n")
		fmt.Fprintf(&b, `{"Action":"pass","Package":%q,"Elapsed":0.002}`+"\n", p)
	}
	return b.String()
}

// TestParseList covers `go test -list` parsing (11 test plan item 5):
// sorting by package and name, packages without tests, duplicates and
// non-JSON lines.
func TestParseList(t *testing.T) {
	in := listEvents(map[string][]string{
		"example.com/m/b": {"FuzzZeta", "FuzzAlpha"},
		"example.com/m/a": {"FuzzLoad", "FuzzLoad"},
	}) +
		`{"Action":"output","Package":"example.com/m/c","Output":"?   \texample.com/m/c\t[no test files]\n"}` + "\n" +
		`{"Action":"skip","Package":"example.com/m/c"}` + "\n" +
		`{"Action":"output","Package":"example.com/m/d","Output":"FuzzyNotATarget is fine\n"}` + "\n" +
		"go: downloading example.com/x v1.0.0\n"
	got, err := parseList([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []target{
		{"example.com/m/a", "FuzzLoad"},
		{"example.com/m/b", "FuzzAlpha"},
		{"example.com/m/b", "FuzzZeta"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("parseList = %v, want %v", got, want)
	}
}

// TestParseListFailure: a package that fails to build is an error that
// names it, never a silently shorter plan.
func TestParseListFailure(t *testing.T) {
	in := listEvents(map[string][]string{"example.com/m/a": {"FuzzA"}}) +
		`{"Action":"output","Package":"example.com/m/broken","Output":"broken.go:3:1: syntax error\n"}` + "\n" +
		`{"Action":"fail","Package":"example.com/m/broken","Elapsed":0,"FailedBuild":"example.com/m/broken"}` + "\n"
	_, err := parseList([]byte(in))
	if !errors.Is(err, errList) || !strings.Contains(err.Error(), "example.com/m/broken") || !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("parseList = %v", err)
	}
	if _, err := parseList([]byte("{not json\n")); !errors.Is(err, errList) {
		t.Errorf("bad JSON: %v", err)
	}
}

// TestParseListBuildOutput: since Go 1.24 compiler messages are
// build-output events keyed by ImportPath, not output events keyed by
// Package; the error keeps them with the failed package (fail event
// FailedBuild, or the ImportPath without its " [pkg.test]" suffix), and
// build output no failed package names goes to a general section, so a
// broken nightly fuzz plan names the compiler error (11 req 28).
func TestParseListBuildOutput(t *testing.T) {
	const (
		p   = "example.com/m/p"
		ip  = p + " [" + p + ".test]"
		msg = `p/p.go:3:23: cannot use "s" (untyped string constant) as int value in return statement` + "\n"
	)
	buildOutput := func(importPath, out string) string {
		return fmt.Sprintf(`{"ImportPath":%q,"Action":"build-output","Output":%q}`, importPath, out) + "\n"
	}
	failEvents := func(pkg, failedBuild string) string {
		return fmt.Sprintf(`{"Action":"start","Package":%q}`, pkg) + "\n" +
			fmt.Sprintf(`{"Action":"output","Package":%q,"Output":%q}`, pkg, "FAIL\t"+pkg+" [build failed]\n") + "\n" +
			fmt.Sprintf(`{"Action":"fail","Package":%q,"Elapsed":0,"FailedBuild":%q}`, pkg, failedBuild) + "\n"
	}
	for _, tc := range []struct {
		name string
		in   string
		want []string // substrings of the error, in order
	}{
		{
			name: "named by FailedBuild",
			in: buildOutput(ip, "# "+ip+"\n") + buildOutput(ip, msg) +
				`{"ImportPath":"` + ip + `","Action":"build-fail"}` + "\n" +
				failEvents(p, ip) + listEvents(map[string][]string{"example.com/m/q": {"FuzzB"}}),
			want: []string{"1 package(s)", "\n" + p + ":\n# " + ip + "\n" + msg + "FAIL\t" + p + " [build failed]"},
		},
		{
			name: "matched by ImportPath without FailedBuild",
			in:   buildOutput(ip, msg) + failEvents(p, ""),
			want: []string{p + ":\n" + msg},
		},
		{
			name: "unclaimed build output in a general section",
			in:   buildOutput("example.com/m/dep", "dep/dep.go:1:1: syntax error\n") + failEvents(p, ""),
			want: []string{p + ":\nFAIL", "\nbuild output:\ndep/dep.go:1:1: syntax error"},
		},
		{
			name: "a dependency's build is reported once",
			in: buildOutput("example.com/m/dep", "dep/dep.go:1:1: syntax error\n") +
				failEvents(p, "example.com/m/dep") + failEvents("example.com/m/r", "example.com/m/dep"),
			want: []string{"2 package(s)", p + ":\ndep/dep.go:1:1: syntax error", "example.com/m/r:\nFAIL"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseList([]byte(tc.in))
			if !errors.Is(err, errList) {
				t.Fatalf("parseList error = %v, want errList", err)
			}
			msg := err.Error()
			at := 0
			for _, w := range tc.want {
				i := strings.Index(msg[at:], w)
				if i < 0 {
					t.Fatalf("error lacks %q after offset %d:\n%s", w, at, msg)
				}
				at += i + len(w)
			}
			if strings.Count(msg, "syntax error") > 1 {
				t.Errorf("build output repeated:\n%s", msg)
			}
		})
	}
}

func manyTargets(n int) []target {
	ts := make([]target, n)
	for i := range ts {
		ts[i] = target{Package: fmt.Sprintf("example.com/m/p%02d", i/3), Name: fmt.Sprintf("Fuzz%02d", i)}
	}
	return ts
}

// TestSharding: shards of eight in sorted order, deterministic for the same
// input whatever its order (11 req 28; test plan item 5).
func TestSharding(t *testing.T) {
	ts := manyTargets(17)
	shuffled := slices.Clone(ts)
	slices.Reverse(shuffled)
	sortTargets(shuffled)
	if !slices.Equal(ts, shuffled) {
		t.Fatal("sortTargets is not deterministic")
	}
	if n := shardCount(len(ts), 8); n != 3 {
		t.Fatalf("shardCount(17, 8) = %d, want 3", n)
	}
	var all []target
	for i := range 3 {
		s := shardOf(ts, 8, i)
		if want := []int{8, 8, 1}[i]; len(s) != want {
			t.Errorf("shard %d has %d targets, want %d", i, len(s), want)
		}
		if !slices.Equal(s, shardOf(ts, 8, i)) {
			t.Errorf("shard %d differs between calls", i)
		}
		all = append(all, s...)
	}
	if !slices.Equal(all, ts) {
		t.Error("the shards do not partition the sorted targets")
	}
	if s := shardOf(ts, 8, 3); s != nil {
		t.Errorf("shard past the end = %v, want empty", s)
	}
	if s := shardOf(ts, 8, -1); s != nil {
		t.Errorf("negative shard = %v, want empty", s)
	}
	for _, tc := range []struct {
		n, per, shards int
		ok             bool
	}{{17, 8, 0, true}, {17, 8, 3, true}, {17, 8, 5, true}, {17, 8, 2, false}, {0, 8, 1, true}, {16, 8, 2, true}} {
		if err := checkShards(tc.n, tc.per, tc.shards); (err == nil) != tc.ok {
			t.Errorf("checkShards(%d, %d, %d) = %v", tc.n, tc.per, tc.shards, err)
		}
	}
}

// fakeGo stands in for the go command.
type fakeGo struct {
	list    string                                    // go test -json -list output
	listErr error                                     // error of the list command
	dirs    map[string]string                         // import path -> directory
	fuzz    func(pkg, name string, w io.Writer) error // one fuzz run
	ran     []string
}

func (f *fakeGo) output(_ context.Context, _ string, args ...string) ([]byte, error) {
	switch args[0] {
	case "test":
		return []byte(f.list), f.listErr
	case "list":
		var b strings.Builder
		for _, p := range args[3:] {
			if d, ok := f.dirs[p]; ok {
				fmt.Fprintf(&b, "%s\t%s\n", p, d)
			}
		}
		return []byte(b.String()), nil
	}
	return nil, fmt.Errorf("unexpected go %v", args)
}

func (f *fakeGo) stream(_ context.Context, _ string, w io.Writer, args ...string) error {
	// test -run ^$ -fuzz ^Name$ -fuzztime T pkg
	name := strings.TrimSuffix(strings.TrimPrefix(args[4], "^"), "$")
	pkg := args[len(args)-1]
	f.ran = append(f.ran, pkg+" "+name+" "+args[6])
	if f.fuzz == nil {
		return nil
	}
	return f.fuzz(pkg, name, w)
}

// TestRunCommands covers list, matrix and shard output.
func TestRunCommands(t *testing.T) {
	f := &fakeGo{list: listEvents(map[string][]string{"example.com/m/a": {"FuzzB", "FuzzA"}, "example.com/m/b": {"FuzzC"}})}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"list"}, "example.com/m/a FuzzA\nexample.com/m/a FuzzB\nexample.com/m/b FuzzC\n"},
		{[]string{"matrix"}, "[0]\n"},
		{[]string{"matrix", "-per", "2"}, "[0,1]\n"},
		{[]string{"shard", "-per", "2", "-index", "1"}, "example.com/m/b FuzzC\n"},
		{[]string{"shard", "-per", "2", "-index", "0", "-corpus"}, "example.com/m/a/FuzzA\nexample.com/m/a/FuzzB\n"},
		{[]string{"shard", "-per", "2", "-shards", "4", "-index", "3"}, ""},
		{[]string{"shard"}, "example.com/m/a FuzzA\nexample.com/m/a FuzzB\nexample.com/m/b FuzzC\n"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), f, clock.Real(), tc.args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit %d\n%s", tc.args, code, stderr.String())
		}
		if stdout.String() != tc.want {
			t.Errorf("%v:\ngot  %q\nwant %q", tc.args, stdout.String(), tc.want)
		}
	}
	empty := &fakeGo{list: ""}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), empty, clock.Real(), []string{"matrix"}, &stdout, &stderr); code != 0 || stdout.String() != "[]\n" {
		t.Errorf("no targets: exit %d, output %q", code, stdout.String())
	}
}

func TestRunUsage(t *testing.T) {
	f := &fakeGo{list: listEvents(map[string][]string{"example.com/m/a": {"FuzzA", "FuzzB", "FuzzC"}})}
	for _, args := range [][]string{
		{},
		{"nope"},
		{"list", "-nope"},
		{"shard", "-per", "0"},
		{"shard", "-index", "-2"},
		{"shard", "-shards", "2", "-index", "2"},
		{"run", "-per", "1", "-shards", "2", "-index", "0"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), f, clock.Real(), args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
	}
	bad := &fakeGo{list: `{"Action":"fail","Package":"example.com/m/x"}` + "\n", listErr: errors.New("exit status 1")}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), bad, clock.Real(), []string{"list"}, &stdout, &stderr); code != 2 {
		t.Errorf("failed listing: exit %d, want 2", code)
	}
	if code := run(context.Background(), &fakeGo{listErr: errors.New("go: not found")}, clock.Real(), []string{"list"}, &stdout, &stderr); code != 2 {
		t.Errorf("go command error: exit %d, want 2", code)
	}
}

// TestRunShard runs a shard with a fake go command: a crasher records the
// new testdata/fuzz input, a failure without one is "fail", the shard keeps
// going after failures, and the report is written (11 req 28).
func TestRunShard(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "a")
	if err := os.MkdirAll(filepath.Join(pkgDir, "testdata", "fuzz", "FuzzCrash"), 0o750); err != nil {
		t.Fatal(err)
	}
	// A committed seed exists before the run and is not a crasher.
	if err := os.WriteFile(filepath.Join(pkgDir, "testdata", "fuzz", "FuzzCrash", "seed"), []byte("go test fuzz v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := clocktest.New(time.Unix(0, 0))
	f := &fakeGo{
		list: listEvents(map[string][]string{"example.com/m/a": {"FuzzCrash", "FuzzOK", "FuzzSeedFails"}}),
		dirs: map[string]string{"example.com/m/a": pkgDir},
		fuzz: func(_, name string, w io.Writer) error {
			clk.Advance(15 * time.Minute)
			switch name {
			case "FuzzCrash":
				_, _ = io.WriteString(w, "--- FAIL: FuzzCrash\n    Failing input written to testdata/fuzz/FuzzCrash/771e938e4458e983\n")
				return errors.Join(os.WriteFile(filepath.Join(pkgDir, "testdata", "fuzz", "FuzzCrash", "771e938e4458e983"), []byte("go test fuzz v1\n[]byte(\"boom\")\n"), 0o600), errors.New("exit status 1"))
			case "FuzzSeedFails":
				_, _ = io.WriteString(w, "--- FAIL: FuzzSeedFails (0.00s)\n")
				return errors.New("exit status 1")
			}
			return nil
		},
	}
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), f, clk, []string{"run", "-C", root, "-index", "0", "-fuzztime", "15m", "-report", reportPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, stdout.String(), stderr.String())
	}
	if want := []string{"example.com/m/a FuzzCrash 15m", "example.com/m/a FuzzOK 15m", "example.com/m/a FuzzSeedFails 15m"}; !slices.Equal(f.ran, want) {
		t.Errorf("ran %v, want %v", f.ran, want)
	}
	data, err := os.ReadFile(reportPath) //nolint:gosec // Test reads its own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	var rep report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Format != "ruralz.fuzzplan.v1" || rep.Shard != 0 || rep.Shards != 1 || rep.FuzzTime != "15m" || len(rep.Targets) != 3 {
		t.Fatalf("report = %+v", rep)
	}
	crash, ok, seed := rep.Targets[0], rep.Targets[1], rep.Targets[2]
	if crash.Result != resultCrasher || !slices.Equal(crash.Crashers, []string{"a/testdata/fuzz/FuzzCrash/771e938e4458e983"}) || !strings.Contains(crash.Output, "Failing input") {
		t.Errorf("crasher = %+v", crash)
	}
	if crash.Seconds != 900 {
		t.Errorf("crasher seconds = %v, want 900", crash.Seconds)
	}
	if ok.Result != resultPass || ok.Output != "" {
		t.Errorf("pass = %+v", ok)
	}
	if seed.Result != resultFail || len(seed.Crashers) != 0 {
		t.Errorf("seed failure = %+v", seed)
	}
	if !strings.Contains(stderr.String(), "FuzzCrash: crasher") {
		t.Errorf("stderr = %s", stderr.String())
	}
}

// TestRunShardEmpty: an index past the needed shards runs nothing and
// passes, writing an empty report.
func TestRunShardEmpty(t *testing.T) {
	f := &fakeGo{list: listEvents(map[string][]string{"example.com/m/a": {"FuzzA"}})}
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), f, clock.Real(), []string{"run", "-shards", "3", "-index", "2", "-report", reportPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "skip") || len(f.ran) != 0 {
		t.Errorf("stdout %q, ran %v", stdout.String(), f.ran)
	}
	data, err := os.ReadFile(reportPath) //nolint:gosec // Test reads its own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"targets": []`) {
		t.Errorf("report = %s", data)
	}
}

func TestRunShardErrors(t *testing.T) {
	f := &fakeGo{list: listEvents(map[string][]string{"example.com/m/a": {"FuzzA"}})} // no directory for the package
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), f, clock.Real(), []string{"run"}, &stdout, &stderr); code != 2 {
		t.Errorf("missing package directory: exit %d, want 2", code)
	}
	f.dirs = map[string]string{"example.com/m/a": t.TempDir()}
	if code := run(context.Background(), f, clock.Real(), []string{"run", "-report", filepath.Join(t.TempDir(), "no", "such", "dir", "r.json")}, &stdout, &stderr); code != 2 {
		t.Errorf("unwritable report: exit %d, want 2", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.fuzz = func(string, string, io.Writer) error { cancel(); return context.Canceled }
	if code := run(ctx, f, clock.Real(), []string{"run"}, &stdout, &stderr); code != 2 {
		t.Errorf("canceled: exit %d, want 2", code)
	}
}

// TestRealGo lists and fuzzes a temporary module with the real go command:
// one target finds a crasher within a few hundred executions, one passes
// (11 req 28, test plan item 5).
func TestRealGo(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go fuzzer")
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/fz\n\ngo 1.26\n",
		"p/p_test.go": `package p

import "testing"

func FuzzCrash(f *testing.F) {
	f.Add([]byte("a"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 2 {
			t.Fatal("boom")
		}
	})
}

func FuzzOK(f *testing.F) {
	f.Add(1)
	f.Fuzz(func(t *testing.T, n int) {})
}
`,
		"q/q.go": "package q\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), goRunner{}, clock.Real(), []string{"list", "-C", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("list: exit %d\n%s", code, stderr.String())
	}
	if want := "example.com/fz/p FuzzCrash\nexample.com/fz/p FuzzOK\n"; stdout.String() != want {
		t.Fatalf("list = %q, want %q", stdout.String(), want)
	}
	reportPath := filepath.Join(t.TempDir(), "report.json")
	stdout.Reset()
	stderr.Reset()
	code := run(context.Background(), goRunner{}, clock.Real(), []string{"run", "-C", dir, "-fuzztime", "500x", "-report", reportPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run: exit %d, want 1\n%s%s", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(reportPath) //nolint:gosec // Test reads its own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	var rep report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Targets) != 2 || rep.Targets[0].Result != resultCrasher || rep.Targets[1].Result != resultPass {
		t.Fatalf("report = %s", data)
	}
	if c := rep.Targets[0].Crashers; len(c) != 1 || !strings.HasPrefix(c[0], "p/testdata/fuzz/FuzzCrash/") {
		t.Errorf("crashers = %v", c)
	}

	// A package that does not compile fails the plan with the
	// compiler's message (Go 1.24+ build-output events).
	broken := filepath.Join(dir, "q", "broken.go")
	if err := os.WriteFile(broken, []byte("package q\n\nfunc X() int { return \"s\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), goRunner{}, clock.Real(), []string{"list", "-C", dir}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "cannot use") {
		t.Errorf("list of a broken package: exit %d, want 2 with the compiler message\n%s", code, errOut.String())
	}
}
