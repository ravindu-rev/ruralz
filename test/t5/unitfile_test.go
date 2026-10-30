// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package t5

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// toolTimeout bounds one run of a systemd tool.
const toolTimeout = 2 * time.Minute

// systemdTool returns the path of the systemd tool name, skipping t when
// the host has none: the checks need systemd's own tools, which every CI
// stage 8 leg has.
func systemdTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("no %s on PATH: this check needs systemd's tools (%v)", name, err)
	}
	return p
}

// unitCopy writes, into a new directory, a copy of ruralzd.service whose
// /usr/local/bin/ paths name that directory, which holds the shipped
// helper and an empty executable standing in for ruralzd, as
// systemd-analyze verify checks that every command exists and is
// executable. It returns the copy's path; extra is appended to the copy.
func unitCopy(t *testing.T, root, extra string) string {
	t.Helper()
	dir := t.TempDir()
	unit := string(shipped(t, root, unitFile))
	if !strings.Contains(unit, "ExecStart="+installBin+"ruralzd") || !strings.Contains(unit, "ExecReload="+installBin+helperFile) {
		t.Fatalf("%s/%s no longer runs %sruralzd and %s%s: update this check", systemdDir, unitFile, installBin, installBin, helperFile)
	}
	unit = strings.ReplaceAll(unit, installBin, dir+"/") + extra
	//nolint:gosec // G306: ExecStart= and ExecReload= must name executable files.
	if err := os.WriteFile(filepath.Join(dir, "ruralzd"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: ExecStart= and ExecReload= must name executable files.
	if err := os.WriteFile(filepath.Join(dir, helperFile), shipped(t, root, helperFile), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, unitFile)
	if err := os.WriteFile(p, []byte(unit), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// verifyFindings runs systemd-analyze verify on the unit at path and
// returns the output lines about the unit and its exit error: verify
// exits 0 after warnings such as an unknown directive, so a line naming
// the unit is a finding too. Lines about other units, which the host's
// own files can produce, are not.
func verifyFindings(t *testing.T, analyze, path string) ([]string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), toolTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, analyze, "verify", path).CombinedOutput() //nolint:gosec // G204: systemd-analyze on a unit this test wrote
	var findings []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if strings.Contains(line, "ruralzd") {
			findings = append(findings, line)
		} else if line != "" {
			t.Logf("systemd-analyze verify, not about the unit: %s", line)
		}
	}
	return findings, err
}

// TestUnitVerifies runs systemd-analyze verify on a copy of the shipped
// unit whose /usr/local/bin/ paths name the helper and a stand-in
// ruralzd: no error and no warning about it.
func TestUnitVerifies(t *testing.T) {
	analyze := systemdTool(t, "systemd-analyze")
	root := repoRoot(t)

	t.Run("shipped", func(t *testing.T) {
		findings, err := verifyFindings(t, analyze, unitCopy(t, root, ""))
		if err != nil || len(findings) > 0 {
			t.Errorf("systemd-analyze verify %s: %v\n%s", unitFile, err, strings.Join(findings, "\n"))
		}
	})
	// A copy with an unknown directive is reported, so the check above
	// reads the unit and catches warnings that leave the exit status 0.
	t.Run("unknown directive reported", func(t *testing.T) {
		findings, err := verifyFindings(t, analyze, unitCopy(t, root, "\n[Service]\nRuralzT5Unknown=1\n"))
		if err == nil && len(findings) == 0 {
			t.Error("systemd-analyze verify reported nothing for an unknown directive")
		}
	})
}

// TestSysusersEntry runs systemd-sysusers with the shipped sysusers.d
// entry against a temporary root: it creates the system user and group
// ruralz with an automatic system ID, home /var/lib/ruralz and no login
// shell.
func TestSysusersEntry(t *testing.T) {
	sysusers := systemdTool(t, "systemd-sysusers")
	root := repoRoot(t)
	target := t.TempDir()
	conf := filepath.Join(target, "etc", "sysusers.d")
	if err := os.MkdirAll(conf, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conf, "ruralzd.conf"), shipped(t, root, sysusersFile), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), toolTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, sysusers, "--root="+target).CombinedOutput() //nolint:gosec // G204: systemd-sysusers on a temporary root
	if err != nil {
		t.Fatalf("systemd-sysusers --root: %v\n%s", err, out)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, "ruralzd.conf") {
			t.Errorf("systemd-sysusers warned about the entry: %s", line)
		}
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(target, "etc", name)) //nolint:gosec // G304: a file systemd-sysusers wrote under the temporary root
		if err != nil {
			t.Fatalf("systemd-sysusers wrote no /etc/%s: %v\n%s", name, err, out)
		}
		return b
	}
	passwd, group := read("passwd"), read("group")
	// The temporary root has no login.defs, so the defaults bound the
	// system IDs systemd-sysusers allocates there.
	uidMax, gidMax := sysIDMax(nil)
	for _, p := range checkSystemUser(passwd, group, uidMax, gidMax) {
		t.Error(p)
	}
}
