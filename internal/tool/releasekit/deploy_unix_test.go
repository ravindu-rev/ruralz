// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// fakeBinary writes an executable bash script standing in for a new
// ruralzd.
func fakeBinary(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "ruralzd")
	writeFile(t, p, "#!/usr/bin/env bash\n"+body)
	if err := os.Chmod(p, 0o700); err != nil { //nolint:gosec // The fake binary needs its owner's execute bit.
		t.Fatal(err)
	}
	return p
}

// runHelper runs deploy/systemd/ruralzd-handover with args and
// RURALZ_DATA_DIR=dataDir, returning its exit status and stderr.
func runHelper(t *testing.T, dataDir string, env []string, args ...string) (int, string) {
	t.Helper()
	helper := filepath.Join(repoRoot(), "deploy", "systemd", "ruralzd-handover")
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{helper}, args...)...) //nolint:gosec // The repository's helper with test arguments.
	cmd.Env = append(append(os.Environ(), "RURALZ_DATA_DIR="+dataDir), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &ee):
		return ee.ExitCode(), stderr.String()
	default:
		t.Fatal(err)
		return 0, ""
	}
}

// killRecorded kills the process whose PID the fake wrote to file.
func killRecorded(t *testing.T, file string) {
	t.Helper()
	data, err := os.ReadFile(file) //nolint:gosec // The test's own record.
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// TestHandoverHelper runs the systemd ExecReload helper against fake new
// binaries: it exits 0 once holder.json names the process it started,
// which keeps running with the unit's environment; 1 when that process
// exits first; and, after the timeout, stops the process and exits 1,
// with SIGKILL when SIGTERM is ignored. A holder.json naming another
// process never counts.
func TestHandoverHelper(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	t.Run("takes the lock", func(t *testing.T) {
		dir := t.TempDir()
		pidFile := filepath.Join(dir, "child.pid")
		t.Cleanup(func() { killRecorded(t, pidFile) })
		bin := fakeBinary(t, dir, `echo "$$" > "$RURALZ_DATA_DIR/child.pid"
printf '%s\n' "$RURALZ_CONFIG" > "$RURALZ_DATA_DIR/config"
sleep 0.3
printf '{"format":"ruralz.holder.v1","pid":%d,"startTime":1,"nodeId":"01ARYZ6S41TSV4RRFFQ69G5FAV","version":"0.1.0"}\n' "$$" > "$RURALZ_DATA_DIR/holder.json"
exec sleep 60
`)
		code, stderr := runHelper(t, dir, []string{"RURALZ_CONFIG=/etc/ruralzd/bundle"}, bin, "30")
		if code != 0 {
			t.Fatalf("exit %d, want 0:\n%s", code, stderr)
		}
		pid, _ := os.ReadFile(filepath.Join(dir, "child.pid")) //nolint:gosec // The fake's record.
		p, err := os.FindProcess(atoi(t, strings.TrimSpace(string(pid))))
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("the new process does not outlive the helper: %v", err)
		}
		if cfg, _ := os.ReadFile(filepath.Join(dir, "config")); strings.TrimSpace(string(cfg)) != "/etc/ruralzd/bundle" { //nolint:gosec // The fake's record.
			t.Errorf("the new process saw RURALZ_CONFIG %q", cfg)
		}
	})
	t.Run("exits first", func(t *testing.T) {
		dir := t.TempDir()
		bin := fakeBinary(t, dir, "exit 3\n")
		code, stderr := runHelper(t, dir, nil, bin, "30")
		if code != 1 || !strings.Contains(stderr, "exited with status 3 before taking the lock") {
			t.Errorf("exit %d, want 1 with the status:\n%s", code, stderr)
		}
	})
	t.Run("times out", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "holder.json"), `{"format":"ruralz.holder.v1","pid":1,"nodeId":"01ARYZ6S41TSV4RRFFQ69G5FAV"}`+"\n")
		bin := fakeBinary(t, dir, `trap 'echo stopped > "$RURALZ_DATA_DIR/stopped"; exit 0' TERM
while :; do sleep 0.1; done
`)
		code, stderr := runHelper(t, dir, nil, bin, "1")
		if code != 1 || !strings.Contains(stderr, "did not take the lock within 1 s: stopping it") {
			t.Errorf("exit %d, want 1 after the timeout:\n%s", code, stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, "stopped")); err != nil {
			t.Errorf("the new process was not stopped with SIGTERM: %v", err)
		}
	})
	t.Run("ignores SIGTERM", func(t *testing.T) {
		dir := t.TempDir()
		pidFile := filepath.Join(dir, "child.pid")
		t.Cleanup(func() { killRecorded(t, pidFile) })
		bin := fakeBinary(t, dir, `echo "$$" > "$RURALZ_DATA_DIR/child.pid"
trap '' TERM
while :; do sleep 0.1; done
`)
		code, stderr := runHelper(t, dir, nil, bin, "1")
		if code != 1 || !strings.Contains(stderr, "ignored SIGTERM for 10 s: killing it") {
			t.Errorf("exit %d, want 1 after SIGKILL:\n%s", code, stderr)
		}
	})
	t.Run("usage", func(t *testing.T) {
		dir := t.TempDir()
		bin := fakeBinary(t, dir, "exit 0\n")
		plain := filepath.Join(dir, "plain")
		writeFile(t, plain, "not executable")
		for name, args := range map[string][]string{
			"no binary": nil, "zero timeout": {bin, "0"}, "word timeout": {bin, "soon"},
			"three arguments": {bin, "5", "x"}, "not executable": {plain}, "missing": {filepath.Join(dir, "absent")},
		} {
			if code, stderr := runHelper(t, dir, nil, args...); code != 2 {
				t.Errorf("%s: exit %d, want 2:\n%s", name, code, stderr)
			}
		}
	})
}
