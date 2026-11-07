// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
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

// Tests that a RunInNetNS child sees only lo (skipped without root), and
// for the namespace setup the security suite relies on (loopback brought
// up with SIOCSIFFLAGS).

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

// TestInNetNSChild runs inside the namespace TestInNetNS creates.
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
	if err := SetupNetNS(); err != nil {
		t.Fatal(err)
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

func TestInNetNS(t *testing.T) { // a RunInNetNS child sees only lo
	requireNetNS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code, err := RunInNetNS(ctx, NetNSOptions{TestName: "TestInNetNSChild"})
	if err != nil || code != 0 {
		t.Fatalf("RunInNetNS = %d, %v", code, err)
	}
	code, err = RunInNetNS(ctx, NetNSOptions{TestName: "TestInNetNSChildFails", Args: []string{"-test.v=true"}})
	if !errors.Is(err, ErrChildFailed) || code != 1 || !strings.Contains(err.Error(), "intentional failure") {
		t.Fatalf("failing child = %d, %v", code, err)
	}
	if !strings.Contains(err.Error(), "=== RUN   TestInNetNSChildFails") {
		t.Fatalf("failing child's error %q lacks its output tail", err)
	}
	if _, err := RunInNetNS(ctx, NetNSOptions{TestName: "TestNoSuchTest"}); !errors.Is(err, ErrChildFailed) {
		t.Fatalf("unmatched test = %v, want ErrChildFailed", err)
	}
	// A skipping child is not a pass (no false green). -test.v=false
	// in Args does not hide the result line.
	code, err = RunInNetNS(ctx, NetNSOptions{TestName: "TestInNetNSChildSkips", Args: []string{"-test.v=false"}})
	if !errors.Is(err, ErrChildSkipped) || errors.Is(err, ErrChildFailed) || code != 0 ||
		!strings.Contains(err.Error(), "intentional skip: precondition missing") {
		t.Fatalf("skipping child = %d, %v; want ErrChildSkipped with the reason", code, err)
	}
	if _, err := RunInNetNS(ctx, NetNSOptions{TestName: "TestInNetNSChildSkipsSub/sub"}); !errors.Is(err, ErrChildSkipped) ||
		!strings.Contains(err.Error(), "intentional subtest skip") {
		t.Fatalf("skipping subtest = %v, want ErrChildSkipped", err)
	}
	if _, err := RunInNetNS(ctx, NetNSOptions{TestName: "TestInNetNSChildSkipsSub"}); err != nil {
		t.Fatalf("parent of a skipped subtest = %v, want a pass", err)
	}
	// Flag parsing stops at "--" or a positional argument in Args; the
	// verbose flag comes before them, so a passing child is still a pass.
	if _, err := RunInNetNS(ctx, NetNSOptions{TestName: "TestInNetNSChildSkipsSub", Args: []string{"-test.v=false", "--", "positional"}}); err != nil {
		t.Fatalf("passing child with \"--\" in Args = %v, want a pass", err)
	}
	if _, err := RunInNetNS(ctx, NetNSOptions{}); err == nil {
		t.Fatal("empty test name: want error")
	}
}

// TestInNetNSUserChild runs inside the user and network namespace
// RunInNetNS with User creates: the caller's user is root there and the
// loopback interface is up.
func TestInNetNSUserChild(t *testing.T) {
	if !NetNSChild(t) {
		t.Skip("run by TestInNetNSUser")
	}
	if uid, gid := os.Getuid(), os.Getgid(); uid != 0 || gid != 0 {
		t.Fatalf("uid %d gid %d in the user namespace, want 0 and 0", uid, gid)
	}
	lo, err := net.InterfaceByName("lo")
	if err != nil || lo.Flags&net.FlagUp == 0 {
		t.Fatalf("lo = %+v, %v; want up", lo, err)
	}
}

func TestInNetNSUser(t *testing.T) { // a user namespace maps the caller to root
	probe := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$", "-test.count=1") //nolint:gosec // G204: this test binary
	probe.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWNET | syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	if err := probe.Run(); err != nil {
		t.Skipf("user namespaces unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code, err := RunInNetNS(ctx, NetNSOptions{TestName: "TestInNetNSUserChild", User: true})
	if errors.Is(err, ErrChildSkipped) {
		t.Skip(err)
	}
	if err != nil || code != 0 {
		t.Fatalf("RunInNetNS(User) = %d, %v", code, err)
	}
}

// TestNetNSChildSkipsWithoutCapability: a child of RunInNetNS with User
// whose user namespace grants no capability over the network namespace,
// as Ubuntu's AppArmor leaves every unprivileged one, skips naming
// ErrNamespace instead of failing. The child here stays in the test's own
// namespaces without privilege, where bringing the loopback interface up
// fails with the same EPERM.
func TestNetNSChildSkipsWithoutCapability(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root brings the loopback interface up in any network namespace")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestNetNSChildSkipsWithoutCapabilityChild$", "-test.count=1", "-test.v=true") //nolint:gosec // G204: this test binary
	cmd.Env = append(os.Environ(), EnvInNetNS+"=1", EnvNetNSUser+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- SKIP: TestNetNSChildSkipsWithoutCapabilityChild") || !strings.Contains(string(out), ErrNamespace.Error()) {
		t.Fatalf("child without the capability = %v:\n%s\nwant a skip naming %q", err, out, ErrNamespace)
	}
}

// TestNetNSChildSkipsWithoutCapabilityChild is the child of
// TestNetNSChildSkipsWithoutCapability.
func TestNetNSChildSkipsWithoutCapabilityChild(t *testing.T) {
	if !NetNSChild(t) {
		t.Skip("run by TestNetNSChildSkipsWithoutCapability")
	}
	t.Fatal("NetNSChild set up a network namespace without the capability")
}

func TestIsVerboseFlag(t *testing.T) {
	for a, want := range map[string]bool{
		"-test.v": true, "--test.v": true, "-test.v=false": true, "--test.v=test2json": true,
		"test.v": false, "---test.v": false, "-test.vet=off": false, "-test.timeout=2m": false,
		"--": false, "-": false, "": false, "positional": false,
	} {
		if got := isVerboseFlag(a); got != want {
			t.Errorf("isVerboseFlag(%q) = %v, want %v", a, got, want)
		}
	}
}

func TestInNetNSNested(t *testing.T) {
	t.Setenv(EnvInNetNS, "1")
	if _, err := RunInNetNS(context.Background(), NetNSOptions{TestName: "TestX"}); err == nil {
		t.Fatal("RunInNetNS inside a child: want error")
	}
}
