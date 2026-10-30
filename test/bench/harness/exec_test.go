// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// TestHelperLabhost is a helper process: labhost with the arguments in
// HARNESS_TEST_LABHOST, so the runner tests start real lab services
// without building the binary.
func TestHelperLabhost(t *testing.T) {
	args := os.Getenv("HARNESS_TEST_LABHOST")
	if args == "" {
		t.Skip("helper process")
	}
	os.Exit(lab.Main(context.Background(), strings.Fields(args), os.Stdin, os.Stdout, os.Stderr))
}

func helperArgv() []string { return []string{os.Args[0], "-test.run", "^TestHelperLabhost$"} }

// TestRunnerStartAndStop starts a lab service through the local runner,
// drives it over its standard input and output and stops it by closing
// standard input, reading its final response.
func TestRunnerStartAndStop(t *testing.T) {
	p := Loopback(t.TempDir())
	t.Setenv("HARNESS_TEST_LABHOST", "echo -listen 127.0.0.1:0")
	proc, err := p.Runner(RoleUpstream).Start(t.Context(), "echo", helperArgv(), "")
	if err != nil {
		t.Fatal(err)
	}
	if proc.Ready.Service != "echo" || proc.Ready.Addr == "" {
		t.Errorf("Ready = %+v", proc.Ready)
	}
	if _, err := proc.Do(lab.Request{Op: lab.OpReset}); err != nil {
		t.Errorf("reset: %v", err)
	}
	if _, err := proc.Do(lab.Request{Op: "nope"}); err == nil {
		t.Error("an unknown operation answered OK")
	}
	last, err := proc.Stop()
	if err != nil || !last.OK {
		t.Errorf("Stop = %+v, %v", last, err)
	}
	t.Setenv("HARNESS_TEST_LABHOST", "nonexistent")
	if _, err := p.Runner(RoleUpstream).Start(t.Context(), "bad", helperArgv(), ""); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("a service that never becomes ready = %v", err)
	}
}

// TestRunnerOutputAndPut runs a one-shot command and copies a file on the
// local host.
func TestRunnerOutputAndPut(t *testing.T) {
	p := Loopback(t.TempDir())
	r := p.Runner(RoleLoad)
	t.Setenv("HARNESS_TEST_LABHOST", "facts -root "+t.TempDir())
	out, err := r.Output(t.Context(), helperArgv(), nil)
	if err != nil || !strings.Contains(string(out), `"hostname"`) {
		t.Errorf("Output = %q, %v", out, err)
	}
	t.Setenv("HARNESS_TEST_LABHOST", "nonexistent")
	if _, err := r.Output(t.Context(), helperArgv(), nil); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("a failing command = %v, want its standard error", err)
	}
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(p.Hosts[RoleLoad].Dir, "bin", "tool")
	if err := r.Put(t.Context(), src, dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("Put = %v, %v", fi, err)
	}
}
