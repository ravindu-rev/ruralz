// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Environment variables of an InNetNS child.
const (
	// EnvInNetNS is "1" in the child InNetNS starts.
	EnvInNetNS = "RURALZ_TEST_IN_NETNS"
	// EnvNetNSSysctls carries the requested sysctls as a JSON object.
	EnvNetNSSysctls = "RURALZ_TEST_NETNS_SYSCTLS"
)

// ErrChildFailed reports that the InNetNS child test failed, ran no test
// or reported no result.
var ErrChildFailed = errors.New("proc: network namespace child failed")

// ErrChildSkipped reports that the InNetNS child test skipped; the error
// carries the child's skip reason, so the parent can pass it to t.Skip
// instead of reporting a pass for a run that never happened.
var ErrChildSkipped = errors.New("proc: network namespace child skipped")

// noTests is what a test binary prints when -test.run matched nothing.
const noTests = "testing: warning: no tests to run"

// Bounds of the child output testResult keeps.
const (
	maxResultLine  = 4 << 10
	maxReasonLines = 8
)

// NetNSOptions configures RunInNetNS.
type NetNSOptions struct {
	// TestName is the test to run in the child, as t.Name() reports it.
	TestName string
	// Sysctls are written under /proc/sys in the child's namespace, for
	// example {"net.ipv4.tcp_migrate_req": "1"}.
	Sysctls map[string]string
	// Args are extra test flags for the child, for example
	// -test.timeout=2m. -test.v is always true, because the verbose result
	// line tells a pass from a skip: it is passed before Args, so a "--"
	// or a positional argument in Args does not keep it from being
	// parsed, and a -test.v entry in Args is dropped.
	Args []string
	// Env is appended to the child's environment (it inherits the
	// parent's, unlike Start).
	Env []string
	// Output receives the child's combined output as it runs; the last
	// 64 KiB are also kept for the error of a failed child.
	Output io.Writer
}

// InNetNS re-executes the running test binary with -test.run matching
// only testName inside a new network namespace (CLONE_NEWNET) and returns
// its exit status. The child sees RURALZ_TEST_IN_NETNS=1 and calls
// NetNSChild, which brings the loopback interface up and applies sysctls
// there. Success means exit 0 and a "--- PASS" line for testName. A child
// that skips returns ErrChildSkipped with its reason; a child that fails,
// runs no test or prints no result line returns ErrChildFailed with its
// output tail. It needs root (or CAP_SYS_ADMIN) and Linux.
//
//	func TestHandover(t *testing.T) {
//		if proc.NetNSChild(t) {
//			runHandover(t) // inside the namespace
//			return
//		}
//		_, err := proc.InNetNS(ctx, t.Name(), map[string]string{"net.ipv4.tcp_migrate_req": "1"})
//		if errors.Is(err, proc.ErrChildSkipped) {
//			t.Skip(err)
//		}
//		if err != nil {
//			t.Fatal(err)
//		}
//	}
func InNetNS(ctx context.Context, testName string, sysctls map[string]string) (int, error) {
	return RunInNetNS(ctx, NetNSOptions{TestName: testName, Sysctls: sysctls})
}

// NetNSChild reports whether this process is an InNetNS child. In the
// child it brings the loopback interface up and applies the parent's
// sysctls (idempotent), failing t when that fails.
func NetNSChild(t testing.TB) bool {
	if os.Getenv(EnvInNetNS) != "1" {
		return false
	}
	t.Helper()
	sysctls, err := decodeSysctls(os.Getenv(EnvNetNSSysctls))
	if err != nil {
		t.Fatal(err)
	}
	if err := SetupNetNS(sysctls); err != nil {
		t.Fatalf("proc: network namespace setup: %v", err)
	}
	return true
}

// RunPattern returns the -test.run value selecting exactly the test or
// subtest named name.
func RunPattern(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = "^" + regexp.QuoteMeta(p) + "$"
	}
	return strings.Join(parts, "/")
}

// sysctlPath returns the /proc/sys file of a sysctl name.
func sysctlPath(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
		return "", fmt.Errorf("proc: bad sysctl name %q", name)
	}
	for i := range len(name) {
		if !sysctlByte(name[i]) {
			return "", fmt.Errorf("proc: bad sysctl name %q", name)
		}
	}
	return "/proc/sys/" + strings.ReplaceAll(name, ".", "/"), nil
}

// sysctlByte reports whether c may appear in a sysctl name.
func sysctlByte(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') || c == '_' || c == '-' || c == '.'
}

func encodeSysctls(m map[string]string) (string, error) {
	for name := range m {
		if _, err := sysctlPath(name); err != nil {
			return "", err
		}
	}
	if m == nil {
		m = map[string]string{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("proc: encode sysctls: %w", err)
	}
	return string(b), nil
}

func decodeSysctls(s string) (map[string]string, error) {
	m := map[string]string{}
	if s == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("proc: %s: %w", EnvNetNSSysctls, err)
	}
	return m, nil
}

// testResult reads a test binary's -test.v output for the result line of
// one test and the log lines that precede it (a skip reason).
type testResult struct {
	name    string
	cur     []byte // the line being read, cut at maxResultLine
	passed  bool
	skipped bool
	noTests bool
	reason  []string // the latest indented log lines, at most maxReasonLines
}

func newTestResult(name string) *testResult { return &testResult{name: name} }

// Write splits p into lines; it never fails.
func (r *testResult) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			r.add(p)
			break
		}
		r.add(p[:i])
		r.line(string(r.cur))
		r.cur = r.cur[:0]
		p = p[i+1:]
	}
	return n, nil
}

func (r *testResult) add(b []byte) {
	if room := maxResultLine - len(r.cur); room > 0 {
		r.cur = append(r.cur, b[:min(len(b), room)]...)
	}
}

// finish reads an unterminated last line.
func (r *testResult) finish() {
	if len(r.cur) > 0 {
		r.line(string(r.cur))
		r.cur = r.cur[:0]
	}
}

func (r *testResult) line(l string) {
	l = strings.TrimSuffix(l, "\r")
	t := strings.TrimLeft(l, " \t")
	switch {
	case strings.HasPrefix(t, noTests):
		r.noTests = true
	case strings.HasPrefix(t, "--- PASS: "+r.name+" ("):
		r.passed = true
	case strings.HasPrefix(t, "--- SKIP: "+r.name+" ("):
		r.skipped = true
	case t == "=== RUN   "+r.name:
		r.reason = r.reason[:0]
	case strings.HasPrefix(t, "=== "), strings.HasPrefix(t, "--- "):
		// Framing of other tests.
	case t != "" && t != l:
		// An indented log line, such as a t.Skip message.
		if len(r.reason) == maxReasonLines {
			r.reason = append(r.reason[:0], r.reason[1:]...)
		}
		r.reason = append(r.reason, t)
	}
}

// outcome maps the child's exit (err from exec.Cmd.Run) and its output to
// RunInNetNS's result.
func (r *testResult) outcome(err error, tail []byte) (int, error) {
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return ee.ExitCode(), fmt.Errorf("%w: %s exited %d:\n%s", ErrChildFailed, r.name, ee.ExitCode(), tail)
	case err != nil:
		return -1, fmt.Errorf("proc: run %s in a new network namespace: %w", r.name, err)
	case r.noTests:
		return 0, fmt.Errorf("%w: no test matched %s", ErrChildFailed, r.name)
	case r.skipped:
		reason := strings.Join(r.reason, "\n")
		if reason == "" {
			reason = "no reason given"
		}
		return 0, fmt.Errorf("%w: %s: %s", ErrChildSkipped, r.name, reason)
	case !r.passed:
		return 0, fmt.Errorf("%w: %s printed no PASS line:\n%s", ErrChildFailed, r.name, tail)
	}
	return 0, nil
}
