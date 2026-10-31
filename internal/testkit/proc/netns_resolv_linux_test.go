// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Tests of NetNSOptions.ResolvConf, which the FC-19 drill uses to point
// its Nodes at the harness DNS server: the child sees the file at
// /etc/resolv.conf while the host's stays as it was.

// requireMountNS skips unless this process can create a network and a
// mount namespace together, probed with a real child (this test binary
// running no test).
func requireMountNS(t *testing.T) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$", "-test.count=1") //nolint:gosec // G204: this test binary
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET | syscall.CLONE_NEWNS}
	if err := cmd.Start(); err != nil {
		t.Skipf("network and mount namespaces unavailable: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("network and mount namespace probe: %v", err)
	}
	if _, err := os.Stat(ResolvConfPath); err != nil {
		t.Skipf("%s, the bind mount's target, is missing: %v", ResolvConfPath, err)
	}
}

// TestInNetNSResolvConfChild runs inside the namespaces RunInNetNS
// creates with ResolvConf: /etc/resolv.conf holds that file's content.
func TestInNetNSResolvConfChild(t *testing.T) {
	if !NetNSChild(t) {
		t.Skip("run by TestInNetNSResolvConf")
	}
	src := os.Getenv(EnvNetNSResolvConf)
	want, err := os.ReadFile(src) //nolint:gosec // G304: the file the parent test wrote
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(ResolvConfPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want the content of %s, %q", ResolvConfPath, got, src, want)
	}
}

func TestInNetNSResolvConf(t *testing.T) { // the child resolves with the file it was given
	requireMountNS(t)
	host, err := os.ReadFile(ResolvConfPath)
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(t.TempDir(), "resolv.conf")
	content := fmt.Sprintf("nameserver 192.0.2.53\n# %s %d\n", t.Name(), time.Now().UnixNano())
	if err := os.WriteFile(conf, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if code, err := RunInNetNS(ctx, NetNSOptions{TestName: "TestInNetNSResolvConfChild", ResolvConf: conf}); err != nil || code != 0 {
		t.Fatalf("RunInNetNS(ResolvConf) = %d, %v", code, err)
	}
	after, err := os.ReadFile(ResolvConfPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, host) {
		t.Fatalf("the host's %s changed to %q", ResolvConfPath, after)
	}
}

// TestRunInNetNSResolvConfRefusals checks that a ResolvConf that is not an
// absolute path of a regular file is refused before any child starts.
func TestRunInNetNSResolvConfRefusals(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"resolv.conf", filepath.Join(dir, "missing"), dir} {
		code, err := RunInNetNS(t.Context(), NetNSOptions{TestName: "TestInNetNSResolvConfChild", ResolvConf: p})
		if err == nil || code != -1 || errors.Is(err, ErrChildFailed) || errors.Is(err, ErrNamespace) {
			t.Errorf("RunInNetNS(ResolvConf %q) = %d, %v; want a refusal", p, code, err)
		}
	}
}

// TestNamespaceErr checks which start failures report ErrNamespace: the
// errors clone(2) returns for missing privilege, kind or room.
func TestNamespaceErr(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no privilege", &os.SyscallError{Syscall: "fork/exec", Err: syscall.EPERM}, true},
		{"no namespace kind", fmt.Errorf("fork/exec x: %w", syscall.EINVAL), true},
		{"namespace limit", syscall.ENOSPC, true},
		{"user namespace limit", syscall.EUSERS, true},
		{"missing executable", fmt.Errorf("fork/exec x: %w", syscall.ENOENT), false},
		{"other", errors.New("exec: no command"), false},
	} {
		if got := namespaceErr(tc.err); got != tc.want {
			t.Errorf("%s: namespaceErr(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
