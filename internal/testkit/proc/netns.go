// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Environment variables of a RunInNetNS child.
const (
	// EnvInNetNS is "1" in the child RunInNetNS starts.
	EnvInNetNS = "RURALZ_TEST_IN_NETNS"
	// EnvNetNSUser is "1" in a child started with NetNSOptions.User and
	// empty otherwise.
	EnvNetNSUser = "RURALZ_TEST_NETNS_USER"
)

// ErrChildFailed reports that the RunInNetNS child test failed, ran no
// test or reported no result.
var ErrChildFailed = errors.New("proc: network namespace child failed")

// ErrChildSkipped reports that the RunInNetNS child test skipped; the
// error carries the child's skip reason, so the parent can pass it to
// t.Skip instead of reporting a pass for a run that never happened.
var ErrChildSkipped = errors.New("proc: network namespace child skipped")

// ErrNamespace reports that the child's namespaces could not be created:
// the process lacks the privilege, or the kernel the namespace kind, or
// the user namespaces it allows are used up. A test that needs them
// skips with this error as its reason.
var ErrNamespace = errors.New("proc: the namespaces could not be created")

// noTests is what a test binary prints when -test.run matched nothing.
const noTests = "testing: warning: no tests to run"

// Bounds of the child output testResult keeps.
const (
	maxResultLine  = 4 << 10
	maxReasonLines = 8
)

// NetNSOptions configures RunInNetNS, which re-executes the running test
// binary with -test.run matching only TestName inside a new network
// namespace (CLONE_NEWNET) and returns its exit status. The child sees
// RURALZ_TEST_IN_NETNS=1 and calls NetNSChild, which brings the loopback
// interface up there. Success means exit 0 and a "--- PASS" line for
// TestName. A child that skips returns ErrChildSkipped with its reason; a
// child that fails, runs no test or prints no result line returns
// ErrChildFailed with its output tail. It needs Linux, and root (or
// CAP_SYS_ADMIN) or, with User, unprivileged user namespaces.
type NetNSOptions struct {
	// TestName is the test to run in the child, as t.Name() reports it.
	TestName string
	// Args are extra test flags for the child, for example
	// -test.timeout=2m. -test.v is always true, because the verbose result
	// line tells a pass from a skip: it is passed before Args, so a "--"
	// or a positional argument in Args does not keep it from being
	// parsed, and a -test.v entry in Args is dropped.
	Args []string
	// Env is appended to the child's environment (it inherits the
	// parent's, unlike Start).
	Env []string
	// User also puts the child in a new user namespace (CLONE_NEWUSER)
	// that maps the caller's user and group to root, so a caller without
	// CAP_SYS_ADMIN gets CAP_NET_ADMIN over the new network namespace
	// where the kernel admits unprivileged user namespaces.
	User bool
}

// NetNSChild reports whether this process is a RunInNetNS child. In the
// child it brings the loopback interface up, failing t when that fails. A
// child in a user namespace whose root holds no capability over the
// network namespace, as Ubuntu's AppArmor leaves every unprivileged one
// (kernel.apparmor_restrict_unprivileged_userns=1), skips with
// ErrNamespace in its reason: it could not have the namespaces, as when
// clone(2) refuses them.
func NetNSChild(t testing.TB) bool {
	if os.Getenv(EnvInNetNS) != "1" {
		return false
	}
	t.Helper()
	if err := SetupNetNS(); err != nil {
		if os.Getenv(EnvNetNSUser) == "1" && errors.Is(err, os.ErrPermission) {
			t.Skipf("%v: the user namespace grants no capability over the network namespace: %v", ErrNamespace, err)
		}
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
