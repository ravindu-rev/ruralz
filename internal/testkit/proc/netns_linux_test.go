// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Tests for 11 test plan item 11 ("InNetNS child sees only lo", skipped
// without root) and the namespace setup 11 req 40 and 44 rely on (loopback
// brought up with SIOCSIFFLAGS; per-namespace net.ipv4.tcp_migrate_req).

// requireNetNS skips unless this process can create a network namespace.
// It probes with a real CLONE_NEWNET child (this test binary running no
// test): uid 0 without CAP_SYS_ADMIN, common in containers, fails there
// with EPERM.
func requireNetNS(t *testing.T) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$", "-test.count=1") //nolint:gosec // G204: this test binary
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
	if err := cmd.Start(); err != nil {
		t.Skipf("network namespaces unavailable: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("network namespace probe: %v", err)
	}
}

// TestInNetNSChild runs inside the namespace InNetNS creates.
func TestInNetNSChild(t *testing.T) {
	if !NetNSChild(t) {
		t.Skip("run by TestInNetNS")
	}
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	// lo is up; any other device is a fallback tunnel (tunl0, sit0, gre0
	// and so on, created in every namespace while their modules are
	// loaded), down and without addresses.
	loUp := false
	for _, ifc := range ifs {
		if ifc.Name == "lo" {
			loUp = ifc.Flags&net.FlagUp != 0
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil || ifc.Flags&net.FlagUp != 0 || len(addrs) > 0 {
			t.Fatalf("interface %+v (addresses %v, %v) in the namespace, want only lo up", ifc, addrs, err)
		}
	}
	if !loUp {
		t.Fatalf("interfaces = %+v, want lo up", ifs)
	}
	if v, err := ReadSysctl("net.ipv4.tcp_migrate_req"); err != nil || v != "1" {
		t.Fatalf("tcp_migrate_req = %q, %v", v, err)
	}
	// Loopback works; nothing else is reachable.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if _, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", "192.0.2.1:80"); err == nil {
		t.Fatal("an address outside the namespace is reachable")
	}
	// A second setup is harmless.
	if err := SetupNetNS(map[string]string{"net.ipv4.tcp_migrate_req": "0"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := ReadSysctl("net.ipv4.tcp_migrate_req"); v != "0" {
		t.Fatalf("tcp_migrate_req after reset = %q", v)
	}
}

// TestInNetNSChildFails fails on purpose inside the namespace.
func TestInNetNSChildFails(t *testing.T) {
	if !NetNSChild(t) {
		t.Skip("run by TestInNetNS")
	}
	t.Fatal("intentional failure")
}

// TestInNetNSChildSkips skips on purpose inside the namespace, as a child
// missing redis-server would.
func TestInNetNSChildSkips(t *testing.T) {
	if !NetNSChild(t) {
		t.Skip("run by TestInNetNS")
	}
	t.Skip("intentional skip: precondition missing")
}

// TestInNetNSChildSkipsSub passes while its subtest skips.
func TestInNetNSChildSkipsSub(t *testing.T) {
	if !NetNSChild(t) {
		t.Skip("run by TestInNetNS")
	}
	t.Run("sub", func(t *testing.T) { t.Skip("intentional subtest skip") })
}

func TestInNetNS(t *testing.T) { // 11 test plan item 11: InNetNS child sees only lo
	requireNetNS(t)
	host, err := ReadSysctl("net.ipv4.tcp_migrate_req")
	if err != nil {
		t.Skipf("tcp_migrate_req unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code, err := InNetNS(ctx, "TestInNetNSChild", map[string]string{"net.ipv4.tcp_migrate_req": "1"})
	if err != nil || code != 0 {
		t.Fatalf("InNetNS = %d, %v", code, err)
	}
	// The host namespace is untouched.
	if v, _ := ReadSysctl("net.ipv4.tcp_migrate_req"); v != host {
		t.Fatalf("host tcp_migrate_req changed from %q to %q", host, v)
	}
	var out bytes.Buffer
	code, err = RunInNetNS(ctx, NetNSOptions{TestName: "TestInNetNSChildFails", Output: &out, Args: []string{"-test.v=true"}})
	if !errors.Is(err, ErrChildFailed) || code != 1 || !strings.Contains(err.Error(), "intentional failure") {
		t.Fatalf("failing child = %d, %v", code, err)
	}
	if !strings.Contains(out.String(), "=== RUN   TestInNetNSChildFails") {
		t.Fatalf("child output not streamed: %q", out.String())
	}
	if _, err := InNetNS(ctx, "TestNoSuchTest", nil); !errors.Is(err, ErrChildFailed) {
		t.Fatalf("unmatched test = %v, want ErrChildFailed", err)
	}
	// A skipping child is not a pass (11 req 40, 44: no false green).
	// -test.v=false in Args does not hide the result line.
	code, err = RunInNetNS(ctx, NetNSOptions{TestName: "TestInNetNSChildSkips", Args: []string{"-test.v=false"}})
	if !errors.Is(err, ErrChildSkipped) || errors.Is(err, ErrChildFailed) || code != 0 ||
		!strings.Contains(err.Error(), "intentional skip: precondition missing") {
		t.Fatalf("skipping child = %d, %v; want ErrChildSkipped with the reason", code, err)
	}
	if _, err := InNetNS(ctx, "TestInNetNSChildSkipsSub/sub", nil); !errors.Is(err, ErrChildSkipped) ||
		!strings.Contains(err.Error(), "intentional subtest skip") {
		t.Fatalf("skipping subtest = %v, want ErrChildSkipped", err)
	}
	if _, err := InNetNS(ctx, "TestInNetNSChildSkipsSub", nil); err != nil {
		t.Fatalf("parent of a skipped subtest = %v, want a pass", err)
	}
	if _, err := InNetNS(ctx, "TestInNetNSChild", map[string]string{"bad/name": "1"}); err == nil {
		t.Fatal("bad sysctl name: want error")
	}
	if _, err := InNetNS(ctx, "", nil); err == nil {
		t.Fatal("empty test name: want error")
	}
	// A child whose setup fails reports it.
	if _, err := InNetNS(ctx, "TestInNetNSChild", map[string]string{"net.ipv4.no_such_sysctl": "1"}); !errors.Is(err, ErrChildFailed) {
		t.Fatalf("unknown sysctl in the child = %v", err)
	}
}

func TestInNetNSNested(t *testing.T) {
	t.Setenv(EnvInNetNS, "1")
	if _, err := InNetNS(context.Background(), "TestX", nil); err == nil {
		t.Fatal("InNetNS inside a child: want error")
	}
}

func TestSysctlNames(t *testing.T) {
	tests := map[string]string{
		"net.ipv4.tcp_migrate_req":      "/proc/sys/net/ipv4/tcp_migrate_req",
		"net.ipv4.ip_local_port_range":  "/proc/sys/net/ipv4/ip_local_port_range",
		"net.ipv6.conf.lo.disable_ipv6": "/proc/sys/net/ipv6/conf/lo/disable_ipv6",
	}
	for in, want := range tests {
		if got, err := sysctlPath(in); err != nil || got != want {
			t.Errorf("sysctlPath(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", ".x", "x.", "a..b", "a/b", "a b", "../etc"} {
		if _, err := sysctlPath(bad); err == nil {
			t.Errorf("sysctlPath(%q): want error", bad)
		}
		if err := WriteSysctl(bad, "1"); err == nil {
			t.Errorf("WriteSysctl(%q): want error", bad)
		}
		if _, err := ReadSysctl(bad); err == nil {
			t.Errorf("ReadSysctl(%q): want error", bad)
		}
	}
	if _, err := ReadSysctl("net.no.such"); err == nil {
		t.Fatal("ReadSysctl of a missing sysctl: want error")
	}
	if err := WriteSysctl("net.no.such", "1"); err == nil {
		t.Fatal("WriteSysctl of a missing sysctl: want error")
	}
}
