// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestCheckOpenFiles covers the open-file check: a run within the limit,
// or on a host whose limit is unknown, passes; one past it fails naming
// the need and the limit, so a starved Node never reports its dial errors
// as the run's result.
func TestCheckOpenFiles(t *testing.T) {
	for _, c := range []struct {
		name    string
		limit   uint64
		need    int
		wantErr bool
	}{
		{"within", 65536, 21120, false},
		{"exactly", 21120, 21120, false},
		{"past", 20000, 21120, true},
		{"unknown limit", 0, 21120, false},
		{"no need", 1024, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &Session{OpenFiles: c.limit}
			err := s.CheckOpenFiles("O1", c.need)
			if (err != nil) != c.wantErr {
				t.Fatalf("CheckOpenFiles(%d) with limit %d = %v, want error %v", c.need, c.limit, err, c.wantErr)
			}
			if err != nil {
				for _, want := range []string{"O1", strconv.Itoa(c.need), strconv.FormatUint(c.limit, 10)} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("CheckOpenFiles error %q does not name %q", err, want)
					}
				}
			}
		})
	}
}

// openFilesHelperEnv runs TestOpenFilesHelper as the child process of
// TestInheritOpenFiles.
const openFilesHelperEnv = "RURALZ_HARNESS_OPENFILES_HELPER"

// TestInheritOpenFiles checks that a process the harness starts after
// InheritOpenFiles gets the harness's raised limit. The harness runs
// here as a child whose original soft limit is 256: Go raises its own
// soft limit at startup but gives the processes it starts the original
// one unless the program set the limit itself, which left a Node at the
// shell's soft limit, often 1,024, where C1, O1 and O2 hold tens of
// thousands of connections.
func TestInheritOpenFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads the limits with the Linux shell")
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", `ulimit -Sn 256 && exec "$0" -test.run='^TestOpenFilesHelper$' -test.count=1`, os.Args[0]) //nolint:gosec // G204: this test binary
	cmd.Env = append(os.Environ(), openFilesHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	fields := strings.Fields(lastLine(string(out)))
	if len(fields) != 2 {
		t.Fatalf("helper wrote %q, want the harness's limit and a started process's", out)
	}
	if fields[0] == "256" {
		t.Fatalf("InheritOpenFiles() = 256: the helper's limit was not raised")
	}
	if fields[1] != fields[0] && fields[1] != "unlimited" {
		t.Errorf("a started process's open-file limit = %s, want the harness's %s", fields[1], fields[0])
	}
}

// TestOpenFilesHelper is TestInheritOpenFiles's child: it calls
// InheritOpenFiles and writes the limit it returned and the one a process
// it starts reads.
func TestOpenFilesHelper(t *testing.T) {
	if os.Getenv(openFilesHelperEnv) == "" {
		t.Skip("child process of TestInheritOpenFiles")
	}
	limit, err := InheritOpenFiles()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), "sh", "-c", "ulimit -n").Output()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = os.Stdout.WriteString(strconv.FormatUint(limit, 10) + " " + strings.TrimSpace(string(out)) + "\n")
	os.Exit(0)
}

// lastLine returns the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
