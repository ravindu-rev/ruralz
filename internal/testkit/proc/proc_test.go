// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Tests for 11 test plan item 11 (proc: start, signal, wait, output
// bounds, /proc sampling) using this test binary as the child process
// (TestHelperProcess), so no external program is needed.

const helperEnv = "RURALZ_PROC_HELPER"

// helper returns a Spec running TestHelperProcess in mode.
func helper(mode string, env ...string) Spec {
	return Spec{
		Name: "helper-" + mode,
		Path: os.Args[0],
		Args: []string{"-test.run=^TestHelperProcess$", "-test.count=1"},
		Env:  append([]string{helperEnv + "=" + mode}, env...),
	}
}

// TestHelperProcess is the child process of these tests.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("child process of the proc tests")
	}
	switch mode {
	case "echo":
		_, _ = os.Stdout.WriteString("hello stdout\n")
		_, _ = os.Stderr.WriteString("hello stderr\n")
		code, _ := strconv.Atoi(os.Getenv("EXIT_CODE"))
		os.Exit(code)
	case "cat":
		_, _ = io.Copy(os.Stdout, os.Stdin)
		os.Exit(0)
	case "env":
		_, _ = os.Stdout.WriteString(strings.Join(os.Environ(), "\n") + "\n")
		os.Exit(0)
	case "chatty":
		// One write per line, as a logging Node does.
		n, _ := strconv.Atoi(os.Getenv("CHATTY_LINES"))
		for i := range n {
			_, _ = fmt.Fprintf(os.Stdout, "%08d %s\n", i, strings.Repeat("x", 90))
		}
		os.Exit(0)
	case "spam":
		n, _ := strconv.Atoi(os.Getenv("SPAM_BYTES"))
		line := []byte("0123456789abcdef")
		for written := 0; written < n; written += len(line) {
			_, _ = os.Stdout.Write(line)
		}
		os.Exit(0)
	case "trap":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		_, _ = os.Stdout.WriteString("ready\n")
		<-ch
		_, _ = os.Stdout.WriteString("got TERM\n")
		os.Exit(3)
	case "sleep":
		_, _ = os.Stdout.WriteString("ready\n")
		time.Sleep(time.Hour)
		os.Exit(0)
	case "spin":
		_, _ = os.Stdout.WriteString("ready\n")
		deadline := time.Now().Add(time.Hour)
		x := 0
		for time.Now().Before(deadline) {
			x++
		}
		os.Exit(x % 2)
	case "spawn":
		// Start a grandchild in this process group (no Setpgid, no
		// Pdeathsig) and report its PID: only the group kill reaches it.
		gc := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestHelperProcess$") //nolint:gosec // G204: this test binary
		gc.Env = []string{helperEnv + "=sleep"}
		if err := gc.Start(); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(2)
		}
		_, _ = fmt.Fprintf(os.Stdout, "grandchild %d\n", gc.Process.Pid)
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(99)
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return c
}

func TestStartWaitOutput(t *testing.T) { // 11 test plan item 11: start, wait, output
	dir := t.TempDir()
	s := helper("echo", "EXIT_CODE=7")
	s.Name = "node-1"
	s.LogDir = dir
	p, err := Start(ctx(t), s)
	if err != nil {
		t.Fatal(err)
	}
	if p.PID() <= 0 || p.Name() != "node-1" {
		t.Fatalf("PID %d name %q", p.PID(), p.Name())
	}
	code, err := p.Wait(ctx(t))
	if err != nil || code != 7 {
		t.Fatalf("Wait = %d, %v; want 7", code, err)
	}
	out, errb := p.Output()
	if !bytes.Contains(out, []byte("hello stdout\n")) || !bytes.Contains(errb, []byte("hello stderr\n")) {
		t.Fatalf("output = %q / %q", out, errb)
	}
	if p.State() == nil || p.State().ExitCode() != 7 {
		t.Fatalf("State = %v", p.State())
	}
	if _, ok := p.Signaled(); ok {
		t.Fatal("Signaled after a normal exit")
	}
	o, e := p.LogPaths()
	if o != filepath.Join(dir, "node-1.out") || e != filepath.Join(dir, "node-1.err") {
		t.Fatalf("LogPaths = %s %s", o, e)
	}
	logOut, err := os.ReadFile(o) //nolint:gosec // G304: test log file
	if err != nil || !bytes.Contains(logOut, []byte("hello stdout")) {
		t.Fatalf("stdout log = %q, %v", logOut, err)
	}
	// A restart with the same name appends to the logs.
	p2, err := Start(ctx(t), s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p2.Wait(ctx(t)); err != nil {
		t.Fatal(err)
	}
	logOut, _ = os.ReadFile(o) //nolint:gosec // G304: test log file
	if bytes.Count(logOut, []byte("hello stdout")) != 2 {
		t.Fatalf("log not appended: %q", logOut)
	}
	if err := p.Signal(syscall.SIGTERM); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Signal after exit = %v", err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill after exit = %v", err)
	}
}

func TestStdin(t *testing.T) {
	s := helper("cat")
	s.Stdin = strings.NewReader("from stdin\n")
	p, err := Start(ctx(t), s)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := p.Wait(ctx(t)); err != nil || code != 0 {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	if out, _ := p.Output(); string(out) != "from stdin\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestEnvironmentNotInherited(t *testing.T) { // 11 section 3: nothing is inherited implicitly
	t.Setenv("RURALZ_PROC_LEAK", "leak")
	p, err := Start(ctx(t), helper("env", "ONLY=this"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(ctx(t)); err != nil {
		t.Fatal(err)
	}
	out, _ := p.Output()
	if bytes.Contains(out, []byte("RURALZ_PROC_LEAK")) || !bytes.Contains(out, []byte("ONLY=this")) {
		t.Fatalf("child environment:\n%s", out)
	}
}

func TestSignalAndWaitOutput(t *testing.T) { // 11 test plan item 11: signal
	p, err := Start(ctx(t), helper("trap"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.WaitOutput(ctx(t), regexp.MustCompile(`(?m)^ready$`)); err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	m, err := p.WaitOutput(ctx(t), regexp.MustCompile(`got (TERM)`))
	if err != nil || len(m) != 2 || m[1] != "TERM" {
		t.Fatalf("WaitOutput = %v, %v", m, err)
	}
	if code, err := p.Wait(ctx(t)); err != nil || code != 3 {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("Done not closed after Wait")
	}
}

func TestKillAndContext(t *testing.T) {
	p, err := Start(ctx(t), helper("sleep"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.WaitOutput(ctx(t), regexp.MustCompile("ready")); err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	if code, err := p.Wait(ctx(t)); err != nil || code != -1 {
		t.Fatalf("Wait after Kill = %d, %v", code, err)
	}
	if sig, ok := p.Signaled(); !ok || sig != syscall.SIGKILL {
		t.Fatalf("Signaled = %v, %v", sig, ok)
	}
	// Ending the Start context kills the process.
	c, cancel := context.WithCancel(context.Background())
	q, err := Start(c, helper("sleep"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.WaitOutput(ctx(t), regexp.MustCompile("ready")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if code, err := q.Wait(ctx(t)); err != nil || code != -1 {
		t.Fatalf("Wait after cancel = %d, %v", code, err)
	}
	// Wait itself honors its context.
	r, err := Start(ctx(t), helper("sleep"))
	if err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if _, err := r.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait with an expired context = %v", err)
	}
	if r.State() != nil {
		t.Fatal("State before exit")
	}
	if _, err := r.WaitOutput(short, regexp.MustCompile("never")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitOutput with an expired context = %v", err)
	}
	_ = r.Kill()
	if _, err := r.WaitOutput(ctx(t), regexp.MustCompile("never")); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("WaitOutput after exit = %v", err)
	}
}

func TestKillReachesProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process groups are killed on Linux")
	}
	p, err := Start(ctx(t), helper("spawn"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := p.WaitOutput(ctx(t), regexp.MustCompile(`grandchild (\d+)`))
	if err != nil {
		t.Fatal(err)
	}
	gc, _ := strconv.Atoi(m[1])
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(ctx(t)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		// The grandchild is gone or a zombie awaiting its reaper.
		st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", gc))
		if err != nil || bytes.Contains(st, []byte(") Z ")) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived Kill: %s", gc, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOutputBounds(t *testing.T) { // 11 test plan item 11: output bounds
	s := helper("spam", "SPAM_BYTES=1048576")
	s.RingBytes = 64 << 10
	s.LogDir = t.TempDir()
	p, err := Start(ctx(t), s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(ctx(t)); err != nil {
		t.Fatal(err)
	}
	out, _ := p.Output()
	if len(out) != 64<<10 {
		t.Fatalf("retained %d bytes, want %d", len(out), 64<<10)
	}
	if !bytes.Equal(out, bytes.Repeat([]byte("0123456789abcdef"), (64<<10)/16)) {
		t.Fatal("retained bytes are not the tail")
	}
	if w, _ := p.Written(); w < 1<<20 {
		t.Fatalf("Written = %d", w)
	}
	o, _ := p.LogPaths()
	if st, err := os.Stat(o); err != nil || st.Size() < 1<<20 {
		t.Fatalf("log file holds %v bytes: %v", st, err)
	}
}

func TestRing(t *testing.T) {
	tests := []struct {
		name   string
		size   int
		writes []string
		want   string
	}{
		{"empty", 4, nil, ""},
		{"fits", 8, []string{"ab", "cd"}, "abcd"},
		{"exact", 4, []string{"ab", "cd"}, "abcd"},
		{"wrap", 4, []string{"abc", "de"}, "bcde"},
		{"wrap twice", 4, []string{"abc", "def", "g"}, "defg"},
		{"oversized write", 4, []string{"ab", "cdefghij"}, "ghij"},
		{"many small", 3, []string{"a", "b", "c", "d", "e"}, "cde"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRing(tt.size)
			total := 0
			for _, w := range tt.writes {
				n, err := r.Write([]byte(w))
				if err != nil || n != len(w) {
					t.Fatalf("Write = %d, %v", n, err)
				}
				total += len(w)
			}
			if got := string(r.bytes()); got != tt.want {
				t.Fatalf("bytes = %q, want %q", got, tt.want)
			}
			if r.written() != int64(total) {
				t.Fatalf("written = %d", r.written())
			}
		})
	}
	// A failing log file never fails the write.
	r := newRing(8)
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	r.file = f
	if n, err := r.Write([]byte("abc")); err != nil || n != 3 || r.fileErr == nil {
		t.Fatalf("Write with a closed file = %d, %v (fileErr %v)", n, err, r.fileErr)
	}
	ch := r.changed()
	_, _ = r.Write([]byte("x"))
	select {
	case <-ch:
	default:
		t.Fatal("changed not closed by a write")
	}
	r.finish()
	select {
	case <-r.changed():
	default:
		t.Fatal("changed not closed after finish")
	}
}

func TestSampleLive(t *testing.T) { // 11 test plan item 11: /proc sampling; 11 req 61
	if runtime.GOOS != "linux" {
		t.Skip("/proc sampling is Linux only")
	}
	p, err := Start(ctx(t), helper("spin"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Kill() }()
	if _, err := p.WaitOutput(ctx(t), regexp.MustCompile("ready")); err != nil {
		t.Fatal(err)
	}
	a, err := p.Sample()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	b, err := p.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if b.RSSBytes <= 0 || b.Threads < 1 || b.UserCPU+b.SysCPU <= 0 {
		t.Fatalf("sample = %+v", b)
	}
	if share := CPUShare(a, b); share <= 0.1 || share > float64(runtime.NumCPU())+0.5 {
		t.Fatalf("CPU share of a spinning process = %.2f", share)
	}
	_ = p.Kill()
	if _, err := p.Wait(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Sample(); err == nil {
		t.Fatal("Sample after exit: want error")
	}
}

func TestSampleProcFakeTree(t *testing.T) { // 11 req 68: RSS from a fake /proc tree
	root := t.TempDir()
	dir := filepath.Join(root, "42")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	stat := "42 (ruralzd (x) y) S 1 42 42 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 9 0 12345 1000000 2000 18446744073709551615\n"
	status := "Name:\truralzd\nVmPeak:\t  200000 kB\nVmRSS:\t   91136 kB\nThreads:\t9\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := SampleProc(root, 42)
	if err != nil {
		t.Fatal(err)
	}
	if u.UserCPU != 2500*time.Millisecond || u.SysCPU != 500*time.Millisecond || u.RSSBytes != 91136*1024 || u.Threads != 9 {
		t.Fatalf("Usage = %+v", u)
	}
	if _, err := SampleProc(root, 43); err == nil {
		t.Fatal("missing pid: want error")
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte("Name: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SampleProc(root, 42); err == nil {
		t.Fatal("status without Threads: want error")
	}
	if err := os.Remove(filepath.Join(dir, "status")); err != nil {
		t.Fatal(err)
	}
	if _, err := SampleProc(root, 42); err == nil {
		t.Fatal("missing status: want error")
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("42 (x) S 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SampleProc(root, 42); err == nil {
		t.Fatal("short stat: want error")
	}
}

func TestParseProcFiles(t *testing.T) {
	good := "1 (a) S " + strings.Repeat("0 ", 10) + "7 8 0 0 0 0 3 0\n"
	if u, s, th, err := ParseStat([]byte(good)); err != nil || u != 7 || s != 8 || th != 3 {
		t.Fatalf("ParseStat = %d %d %d %v", u, s, th, err)
	}
	statErrs := []string{
		"no parens",
		"1 (a) S " + strings.Repeat("0 ", 10) + "x 8 0 0 0 0 3 0",
		"1 (a) S " + strings.Repeat("0 ", 10) + "7 x 0 0 0 0 3 0",
		"1 (a) S " + strings.Repeat("0 ", 10) + "7 8 0 0 0 0 x 0",
	}
	for _, s := range statErrs {
		if _, _, _, err := ParseStat([]byte(s)); err == nil {
			t.Errorf("ParseStat(%q): want error", s)
		}
	}
	if rss, th, err := ParseStatus([]byte("State:\tZ (zombie)\nThreads:\t1\n")); err != nil || rss != 0 || th != 1 {
		t.Fatalf("zombie status = %d %d %v", rss, th, err)
	}
	for _, s := range []string{"VmRSS:\tx kB\nThreads:\t1\n", "VmRSS:\t10 MB\nThreads:\t1\n", "Threads:\tx\n"} {
		if _, _, err := ParseStatus([]byte(s)); err == nil {
			t.Errorf("ParseStatus(%q): want error", s)
		}
	}
	a := Usage{UserCPU: time.Second, Time: time.Unix(0, 0)}
	b := Usage{UserCPU: 2 * time.Second, SysCPU: time.Second, Time: time.Unix(4, 0)}
	if got := CPUShare(a, b); got != 0.5 {
		t.Fatalf("CPUShare = %v", got)
	}
	if got := CPUShare(b, b); got != 0 {
		t.Fatalf("CPUShare without elapsed time = %v", got)
	}
}

func TestStartErrors(t *testing.T) {
	if _, err := Start(ctx(t), Spec{}); err == nil {
		t.Fatal("empty Path: want error")
	}
	if _, err := Start(ctx(t), Spec{Path: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing binary: want error")
	}
	s := helper("echo")
	s.LogDir = filepath.Join(t.TempDir(), "no", "such")
	if _, err := Start(ctx(t), s); err == nil {
		t.Fatal("missing LogDir: want error")
	}
	// The stderr log failing to open closes the stdout log.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "x.err"), 0o750); err != nil {
		t.Fatal(err)
	}
	s = helper("echo")
	s.Name, s.LogDir = "x", dir
	if _, err := Start(ctx(t), s); err == nil {
		t.Fatal("stderr log is a directory: want error")
	}
	s.LogDir = t.TempDir()
	s.Path = filepath.Join(dir, "missing")
	if _, err := Start(ctx(t), s); err == nil {
		t.Fatal("missing binary with logs: want error")
	}
	p, err := Start(ctx(t), Spec{Path: os.Args[0], Args: []string{"-test.run=^TestHelperProcess$"}, Env: []string{helperEnv + "=echo"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != filepath.Base(os.Args[0]) {
		t.Fatalf("default name = %q", p.Name())
	}
	_, _ = p.Wait(ctx(t))
}

func TestRingGrowsLazily(t *testing.T) { // review finding: no eager 2 x 8 MiB per Start
	r := newRing(DefaultRingBytes)
	if len(r.buf) != ringInitialBytes {
		t.Fatalf("initial ring = %d bytes, want %d", len(r.buf), ringInitialBytes)
	}
	if small := newRing(100); len(small.buf) != 100 {
		t.Fatalf("small ring = %d bytes, want its bound", len(small.buf))
	}
	var all []byte
	chunk := make([]byte, 10007)
	for i := 0; len(all) < 3*DefaultRingBytes; i++ {
		for j := range chunk {
			chunk[j] = byte(i + j)
		}
		_, _ = r.Write(chunk)
		all = append(all, chunk...)
		want := all[max(0, len(all)-DefaultRingBytes):]
		if len(r.buf) > DefaultRingBytes || len(r.buf) < len(want) {
			t.Fatalf("after %d bytes the ring holds %d in %d", len(all), len(want), len(r.buf))
		}
		if i%97 == 0 || len(all) >= 3*DefaultRingBytes {
			if got := r.bytes(); !bytes.Equal(got, want) {
				t.Fatalf("after %d bytes the ring lost data (len %d, want %d)", len(all), len(got), len(want))
			}
		}
	}
	if len(r.buf) != DefaultRingBytes {
		t.Fatalf("full ring = %d bytes, want %d", len(r.buf), DefaultRingBytes)
	}
	// One write larger than the bound grows straight to it.
	big := newRing(1 << 20)
	_, _ = big.Write(bytes.Repeat([]byte("z"), 3<<20))
	if len(big.buf) != 1<<20 || big.n != 1<<20 {
		t.Fatalf("oversized first write: buf %d, held %d", len(big.buf), big.n)
	}
}

func TestRingSince(t *testing.T) {
	r := newRing(4)
	check := func(off int64, want string, wantFrom, wantEnd int64) {
		t.Helper()
		got, from, end := r.since([]byte("^"), off)
		if string(got) != "^"+want || from != wantFrom || end != wantEnd {
			t.Fatalf("since(%d) = %q, %d, %d; want %q, %d, %d", off, got, from, end, "^"+want, wantFrom, wantEnd)
		}
	}
	check(0, "", 0, 0)
	_, _ = r.Write([]byte("ab"))
	check(0, "ab", 0, 2)
	check(1, "b", 1, 2)
	check(2, "", 2, 2)
	_, _ = r.Write([]byte("cdef"))
	check(1, "cdef", 2, 6) // "ab" were dropped
	check(4, "ef", 4, 6)
	_, _ = r.Write([]byte("g")) // wraps: start 1
	check(3, "defg", 3, 7)
	check(5, "fg", 5, 7)
	check(7, "", 7, 7)
}

func TestLineMatcher(t *testing.T) { // review finding: WaitOutput matches line by line
	tests := []struct {
		name   string
		limit  int
		writes []string
		re     string
		want   []string // nil: no match after any write
		at     int      // the write after which the match appears
	}{
		{"line split across writes", 1 << 10, []string{"rea", "dy 42\n"}, `ready (\d+)\n`, []string{"ready 42\n", "42"}, 1},
		{"unterminated line matches", 1 << 10, []string{"x\nprompt> "}, `^prompt> $`, []string{"prompt> "}, 0},
		{"growing last line", 1 << 10, []string{"pro", "mpt> "}, `prompt> `, []string{"prompt> "}, 1},
		{"first line wins", 1 << 10, []string{"k=1\nk=2\n"}, `k=(\d)`, []string{"k=1", "1"}, 0},
		{"anchors per line", 1 << 10, []string{"x\nready\n"}, `^ready\n$`, []string{"ready\n"}, 0},
		{"multiline flag", 1 << 10, []string{"a\nready\nb\n"}, `(?m)^ready$`, []string{"ready"}, 0},
		{"no match across lines", 1 << 10, []string{"a\n", "b\n"}, `a\nb`, nil, 0},
		{"no match across lines in one write", 1 << 10, []string{"a\nb\n"}, `a\nb`, nil, 0},
		// The ring dropped "XX" between scans: the held "ab" must not be
		// joined to "c\n" into a false "abc".
		{"dropped bytes split the line", 2, []string{"ab", "XXc\n"}, `^abc`, nil, 0},
		{"no drop joins the line", 8, []string{"ab", "XXc\n"}, `^abXXc`, []string{"abXXc"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRing(tt.limit)
			m := &lineMatcher{r: r, re: regexp.MustCompile(tt.re)}
			for i, w := range tt.writes {
				_, _ = r.Write([]byte(w))
				got := m.scan()
				if tt.want != nil && i == tt.at {
					if strings.Join(got, "|") != strings.Join(tt.want, "|") {
						t.Fatalf("after write %d: %q, want %q", i, got, tt.want)
					}
					return
				}
				if got != nil {
					t.Fatalf("after write %d: unexpected match %q", i, got)
				}
			}
			if tt.want != nil {
				t.Fatal("no match")
			}
		})
	}
}

func TestLineMatcherScansEachByteOnce(t *testing.T) { // review finding: cost follows the output, not the ring size
	r := newRing(DefaultRingBytes)
	m := &lineMatcher{r: r, re: regexp.MustCompile(`never matches`)}
	line := []byte(strings.Repeat("y", 99) + "\n")
	for range 20000 {
		_, _ = r.Write(line)
		if m.scan() != nil {
			t.Fatal("unexpected match")
		}
	}
	if m.scanned != r.written() {
		t.Fatalf("scanned %d bytes for %d written, want each byte once", m.scanned, r.written())
	}
	// A long line without a newline is matched again as it grows, but
	// one scan never reads more than the new bytes plus MaxLineBytes.
	r = newRing(DefaultRingBytes)
	m = &lineMatcher{r: r, re: regexp.MustCompile(`never matches`)}
	piece := bytes.Repeat([]byte("z"), 4<<10)
	for i := range 64 { // 256 KiB, one line
		_, _ = r.Write(piece)
		before := m.scanned
		m.scan()
		if d := m.scanned - before; d > int64(len(piece)+MaxLineBytes) {
			t.Fatalf("scan %d read %d bytes for %d new, want at most %d", i, d, len(piece), len(piece)+MaxLineBytes)
		}
		if len(m.line) >= MaxLineBytes {
			t.Fatalf("held %d bytes of the unterminated line", len(m.line))
		}
	}
}

func TestWaitOutputChattyProcess(t *testing.T) { // review finding: a waiter does not burden a chatty Node (GD-2; 11 req 44-46)
	const lines = 20000
	p, err := Start(ctx(t), helper("chatty", fmt.Sprintf("CHATTY_LINES=%d", lines)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.WaitOutput(ctx(t), regexp.MustCompile(`never matches`))
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("WaitOutput = %v, want the exit error", err)
	}
	out, errb := p.Written()
	written := out + errb
	if out < lines*100 {
		t.Fatalf("child wrote %d bytes, want at least %d", out, lines*100)
	}
	// Each byte is matched about once (a line split by a pipe read is
	// matched again when complete). Rescanning the ring on every write,
	// as before, costs hundreds of times the output.
	if scanned := p.scanned.Load(); scanned < written || scanned > 2*written {
		t.Fatalf("WaitOutput matched %d bytes for %d written, want between 1x and 2x", scanned, written)
	}
	// A match is still found late in the output.
	q, err := Start(ctx(t), helper("chatty", fmt.Sprintf("CHATTY_LINES=%d", lines)))
	if err != nil {
		t.Fatal(err)
	}
	m, err := q.WaitOutput(ctx(t), regexp.MustCompile(fmt.Sprintf(`^(%08d) x+\n$`, lines-1)))
	if err != nil || m[1] != fmt.Sprintf("%08d", lines-1) {
		t.Fatalf("WaitOutput = %v, %v", m, err)
	}
	if _, err := q.Wait(ctx(t)); err != nil {
		t.Fatal(err)
	}
}
