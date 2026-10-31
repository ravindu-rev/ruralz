// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build t5 && linux

package t5

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The helper's documented contract (docs/operations/01-deployment-topologies.md
// "T5 and T6", the ruralzd-handover row): `ruralzd-handover BINARY
// [TIMEOUT_SECONDS]` exits 0 once holder.json names BINARY's process, 1 if
// that process exits first, 1 after the timeout, 75 s (target) by default,
// once it stopped the process with SIGTERM, then SIGKILL 10 s later, and 2
// on a usage error; it checks every 0.2 s and counts those sleeps.
const (
	helperUsage        = "usage: ruralzd-handover BINARY [TIMEOUT_SECONDS]"
	helperTook         = "holds the lock: handover complete"
	helperDefaultWait  = 75 // seconds (target)
	helperStopGrace    = 10 // seconds from SIGTERM to SIGKILL
	helperPollsPerSec  = 5  // one check every 0.2 s
	helperPollInterval = "0.2"
)

// helperBound bounds one helper run in real time: its default wait and
// stop grace, with a margin.
const helperBound = 2 * time.Minute

// holderRecord is a holder.json as ruralzd writes it, %d standing for the
// process ID (docs/reference/01-cli-and-api-surface.md, holder.json).
const holderRecord = `{"format":"ruralz.holder.v1","pid":%d,"startTime":4242,"nodeId":"01JBZ7Q4Y5X2W3V1T0S9R8P7N6","version":"0.1.0"}`

// helperCopy returns an executable copy of the shipped helper, mode 0755
// as the T5 install steps leave it.
func helperCopy(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), helperFile)
	writeExecutable(t, p, shipped(t, repoRoot(t), helperFile))
	return p
}

// writeExecutable writes an executable script with mode 0755 while it
// holds syscall.ForkLock: a child that a parallel test forks while the
// file is open for writing keeps it open until its own exec, and an exec
// of the file in that time fails with "text file busy".
func writeExecutable(t *testing.T, name string, data []byte) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	//nolint:gosec // G306: the file is an executable script.
	if err := os.WriteFile(name, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

// helperCheck is one run of the helper with this test binary as BINARY:
// the data directory the helper reads holder.json from and the stand-in's
// log and readiness files.
type helperCheck struct {
	t      *testing.T
	helper string
	dir    string
	data   string
	log    string
	ready  string
	env    []string
}

// newHelperCheck returns a check whose stand-in runs mode; extra adds to
// the environment the helper and the stand-in get.
func newHelperCheck(t *testing.T, mode string, extra ...string) *helperCheck {
	t.Helper()
	dir := t.TempDir()
	h := &helperCheck{
		t: t, helper: helperCopy(t), dir: dir, data: filepath.Join(dir, "data"),
		log: filepath.Join(dir, "standin.log"), ready: filepath.Join(dir, "standin.ready"),
	}
	if err := os.Mkdir(h.data, 0o700); err != nil {
		t.Fatal(err)
	}
	h.env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		envDataDir + "=" + h.data,
		envConfig + "=" + filepath.Join(dir, "bundle"),
		envStandIn + "=" + mode,
		envStandInLog + "=" + h.log,
		envStandInReady + "=" + h.ready,
	}, extra...)
	return h
}

// binary returns the test binary, the stand-in BINARY.
func (h *helperCheck) binary() string {
	p, err := os.Executable()
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// helperResult is one finished helper run.
type helperResult struct {
	code           int
	stdout, stderr string
	elapsed        time.Duration
	// started is the process the helper reported starting, while it ran
	// when the helper exited; zero otherwise.
	started proc
	// startedPID is the PID the helper reported starting; 0 for none.
	startedPID int
}

// startedRE matches the helper's report of the process it started.
func startedRE() *regexp.Regexp { return regexp.MustCompile(`started .* as process ([0-9]+)`) }

// run runs the helper with args and h's environment until it exits. Its
// output goes to files, so a process it started and left running holds no
// pipe the wait depends on. The process the helper left running ends with
// the test.
func (h *helperCheck) run(args ...string) helperResult {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), helperBound)
	defer cancel()
	open := func(name string) *os.File {
		f, err := os.Create(filepath.Join(h.dir, name)) //nolint:gosec // G304: a file in the test's directory
		if err != nil {
			h.t.Fatal(err)
		}
		return f
	}
	stdout, stderr := open("helper.stdout"), open("helper.stderr")
	cmd := exec.CommandContext(ctx, h.helper, args...) //nolint:gosec // G204: the shipped helper with the check's arguments
	cmd.Env, cmd.Stdout, cmd.Stderr, cmd.Dir = h.env, stdout, stderr, h.dir
	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	_ = stdout.Close()
	_ = stderr.Close()
	r := helperResult{elapsed: elapsed}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	default:
		h.t.Fatalf("running %s: %v", h.helper, err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(h.dir, name)) //nolint:gosec // G304: a file in the test's directory
		if err != nil {
			h.t.Fatal(err)
		}
		return string(b)
	}
	r.stdout, r.stderr = read("helper.stdout"), read("helper.stderr")
	if m := startedRE().FindStringSubmatch(r.stdout); m != nil {
		r.startedPID, _ = strconv.Atoi(m[1])
		if p, ok := lookupProc(r.startedPID); ok {
			r.started = p
			h.t.Cleanup(func() { p.kill(syscall.SIGKILL) })
		}
	}
	return r
}

// records returns the stand-in's records.
func (h *helperCheck) records() []string {
	h.t.Helper()
	b, err := os.ReadFile(h.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// hasRecord reports whether the stand-in recorded a line starting with
// prefix.
func (h *helperCheck) hasRecord(prefix string) bool {
	return slices.ContainsFunc(h.records(), func(r string) bool { return strings.HasPrefix(r, prefix) })
}

// TestHelperUsage checks the bounds of the arguments: a timeout of one to
// four digits without a leading zero and an executable regular file;
// anything else is a usage error, exit 2, that starts nothing. The
// helper's unit tests in internal/tool/releasekit cover the other usage
// errors and its exit paths.
func TestHelperUsage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		args func(bin string) []string
		want string
	}{
		{"timeout leading zero", func(bin string) []string { return []string{bin, "075"} }, helperUsage},
		{"timeout negative", func(bin string) []string { return []string{bin, "-1"} }, helperUsage},
		{"timeout five digits", func(bin string) []string { return []string{bin, "10000"} }, helperUsage},
		{"binary a directory", func(string) []string { return []string{dir} }, "is not an executable file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHelperCheck(t, modeExit, envStandInExit+"=0")
			r := h.run(tc.args(h.binary())...)
			if r.code != 2 || !strings.Contains(r.stderr, tc.want) {
				t.Errorf("ruralzd-handover %s = exit %d, stderr %q; want 2 and %q", tc.name, r.code, r.stderr, tc.want)
			}
			if h.hasRecord(recordStart) || r.startedPID != 0 {
				t.Errorf("ruralzd-handover started a process on a usage error: %q", r.stdout)
			}
		})
	}
	// The largest timeout, four digits, is no usage error.
	h := newHelperCheck(t, modeHolder, envStandInHolder+"="+holderRecord)
	if r := h.run(h.binary(), "9999"); r.code != 0 {
		t.Errorf("ruralzd-handover with timeout 9999 = exit %d, stderr %q; want 0", r.code, r.stderr)
	}
}

// TestHelperTakesLock checks that the helper exits 0 once holder.json names
// the process it started, by that process's ID, leaves it running, and
// starts it with the unit's environment and standard input /dev/null.
func TestHelperTakesLock(t *testing.T) {
	t.Parallel()
	h := newHelperCheck(t, modeHolder, envStandInHolder+"="+holderRecord)
	r := h.run(h.binary(), "30")
	if r.code != 0 || !strings.Contains(r.stdout, helperTook) {
		t.Fatalf("ruralzd-handover = exit %d, stdout %q, stderr %q; want 0 and %q", r.code, r.stdout, r.stderr, helperTook)
	}
	if !r.started.alive() {
		t.Errorf("the process %d the helper started does not run after the helper exited", r.startedPID)
	}
	if pid, err := holderPID(h.data); err != nil || pid != r.startedPID {
		t.Errorf("holder.json names %d (%v), want the started process %d", pid, err, r.startedPID)
	}
	want := recordStart + " pid=" + strconv.Itoa(r.startedPID) + " stdin-null=true config=" + filepath.Join(h.dir, "bundle")
	if recs := h.records(); len(recs) == 0 || recs[0] != want {
		t.Errorf("stand-in records %q, want first %q: the environment and /dev/null standard input", recs, want)
	}
}

// TestHelperReadsOnlyPID checks that the helper reads only the pid member
// of holder.json, wherever it stands and whatever other members the record
// holds.
func TestHelperReadsOnlyPID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, record string }{
		{"pid last", `{"format":"ruralz.holder.v1","nodeId":"01JBZ7Q4Y5X2W3V1T0S9R8P7N6","version":"0.1.0","pid":%d}`},
		{"unknown members", `{"format":"ruralz.holder.v1","future":{"list":[1,2,3],"name":"x"},"pid":%d,"later":true}`},
		{"spacing and lines", "{\n  \"format\" : \"ruralz.holder.v1\",\n  \"pid\" :\n    %d\n}\n"},
		{"pidNamespace first", `{"format":"ruralz.holder.v1","pidNamespace":"pid:[4026531836]","pid":%d}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHelperCheck(t, modeHolder, envStandInHolder+"="+tc.record)
			if r := h.run(h.binary(), "30"); r.code != 0 || !strings.Contains(r.stdout, helperTook) {
				t.Errorf("ruralzd-handover with holder.json %q = exit %d, stderr %q; want 0", tc.record, r.code, r.stderr)
			}
		})
	}
}

// sleepShim is a sleep(1) the helper finds first on PATH: it appends each
// call's arguments to $RURALZ_T5_SLEEP_LOG and returns at once once the
// stand-in is ready, so a run takes no time per check.
const sleepShim = `#!/usr/bin/env bash
printf '%s\n' "$*" >>"$RURALZ_T5_SLEEP_LOG"
while [[ ! -e $RURALZ_T5_STANDIN_READY ]]; do command -p sleep 0.01; done
`

// TestHelperCountsSleeps checks that the helper waits by counting its
// 0.2 s sleeps, five a second, so a clock step never shortens a wait: with
// a sleep that returns at once, the timeout is still that many sleeps, and
// so is the stop grace. Without a timeout argument the wait is 75 s
// (target), 375 sleeps.
func TestHelperCountsSleeps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		args    []string
		timeout int
	}{
		{"timeout 2 s", []string{"2"}, 2},
		{"default timeout", nil, helperDefaultWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shim := t.TempDir()
			writeExecutable(t, filepath.Join(shim, "sleep"), []byte(sleepShim))
			sleeps := filepath.Join(shim, "sleeps.log")
			h := newHelperCheck(t, modeIgnoreTerm, "RURALZ_T5_SLEEP_LOG="+sleeps)
			h.env[0] = "PATH=" + shim + string(os.PathListSeparator) + os.Getenv("PATH")
			r := h.run(append([]string{h.binary()}, tc.args...)...)
			if r.code != 1 || !strings.Contains(r.stderr, "killing it") {
				t.Fatalf("ruralzd-handover = exit %d, stderr %q; want 1 after SIGKILL", r.code, r.stderr)
			}
			b, err := os.ReadFile(sleeps) //nolint:gosec // G304: a file in the test's directory
			if err != nil {
				t.Fatal(err)
			}
			calls := strings.Split(strings.TrimSpace(string(b)), "\n")
			want := (tc.timeout + helperStopGrace) * helperPollsPerSec
			if len(calls) != want {
				t.Errorf("the helper slept %d times, want %d: %d s of checks and the %d s stop grace at %d a second",
					len(calls), want, tc.timeout, helperStopGrace, helperPollsPerSec)
			}
			for i, c := range calls {
				if c != helperPollInterval {
					t.Errorf("sleep call %d took %q, want %s", i, c, helperPollInterval)
					break
				}
			}
		})
	}
}

// TestHelperHandsOverRuralzd runs the helper as ExecReload= does, outside
// systemd: started beside a ruralzd that holds the lock, with the same
// environment, it starts the same binary, which hands over and takes the
// lock; the helper exits 0, the old process exits 0 after its Drain, and
// the new one serves.
func TestHelperHandsOverRuralzd(t *testing.T) {
	t.Parallel()
	bin := releaseRuralzd(t)
	ctx := t.Context()
	up := startUpstream(t)
	d := newNodeDir(t, up.Listener.Addr().String())
	old := startNode(t, bin, d.env(), filepath.Join(d.dir, "old.log"))
	if _, err := waitReady(ctx, d.adminURL()); err != nil {
		t.Fatalf("the holder: %v\n%s", err, old.output())
	}
	h := &helperCheck{t: t, helper: helperCopy(t), dir: d.dir, data: d.data, env: d.env()}
	r := h.run(bin, "30")
	if r.code != 0 || !strings.Contains(r.stdout, helperTook) {
		t.Fatalf("ruralzd-handover = exit %d\nstdout %s\nstderr %s\nholder:\n%s", r.code, r.stdout, r.stderr, old.output())
	}
	if pid, err := holderPID(d.data); err != nil || pid != r.startedPID || pid == old.pid() {
		t.Errorf("holder.json names %d (%v), want the new process %d", pid, err, r.startedPID)
	}
	if code, err := old.waitExit(); err != nil || code != 0 {
		t.Errorf("the old process: exit %d, %v; want 0 within %v of the release\n%s", code, err, exitAfterReleaseBound, old.output())
	}
	if _, err := waitReady(ctx, d.adminURL()); err != nil {
		t.Errorf("the new process: %v", err)
	}
	if err := get(ctx, d.webURL()); err != nil {
		t.Errorf("GET / on the new process: %v", err)
	}
	// A second stop signal jumps to the Drain deadline.
	r.started.kill(syscall.SIGTERM)
	r.started.kill(syscall.SIGTERM)
	if err := waitFor(ctx, exitAfterReleaseBound, func() (bool, string) { return !r.started.alive(), "the new process runs" }); err != nil {
		t.Errorf("the new process after SIGTERM: %v", err)
	}
}

// TestHelperRefusedHandover runs the helper with the refused-handover
// variant: the holder refuses its unknown handover marker, the variant
// exits 1 holding no listening socket, the helper exits 1, and the holder
// keeps the lock and serves.
func TestHelperRefusedHandover(t *testing.T) {
	t.Parallel()
	bin, variant := releaseRuralzd(t), refusedRuralzd(t)
	ctx := t.Context()
	up := startUpstream(t)
	d := newNodeDir(t, up.Listener.Addr().String())
	holder := startNode(t, bin, d.env(), filepath.Join(d.dir, "holder.log"))
	if _, err := waitReady(ctx, d.adminURL()); err != nil {
		t.Fatalf("the holder: %v\n%s", err, holder.output())
	}
	h := &helperCheck{t: t, helper: helperCopy(t), dir: d.dir, data: d.data, env: d.env()}
	r := h.run(variant, "30")
	if r.code != 1 || !strings.Contains(r.stderr, "exited with status 1 before taking the lock") {
		t.Fatalf("ruralzd-handover with the variant = exit %d\nstdout %s\nstderr %s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout+r.stderr, "unknown_marker") || !strings.Contains(holder.output(), "unknown_marker") {
		t.Errorf("no unknown_marker refusal logged\nvariant:\n%s\n%s\nholder:\n%s", r.stdout, r.stderr, holder.output())
	}
	if holder.exited() {
		t.Fatalf("the holder exited after refusing\n%s", holder.output())
	}
	if pid, err := holderPID(d.data); err != nil || pid != holder.pid() {
		t.Errorf("holder.json names %d (%v), want the holder %d", pid, err, holder.pid())
	}
	if code, v, err := readyz(ctx, d.adminURL()); err != nil || code != http.StatusOK {
		t.Errorf("/readyz of the holder = %d %+v, %v; want 200", code, v, err)
	}
	if err := get(ctx, d.webURL()); err != nil {
		t.Errorf("GET / on the holder after the refusal: %v", err)
	}
}
