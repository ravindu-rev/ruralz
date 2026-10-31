// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// fakeSystemdRun is a systemd-run that records its arguments into
// $XDG_RUNTIME_DIR/scopes, one line per call, then executes the command
// after "--" in its own place, as systemd-run --scope does once the user
// manager has made the scope.
const fakeSystemdRun = `#!/bin/sh
printf '%s\n' "$*" >>"$XDG_RUNTIME_DIR/scopes"
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
exec "$@"
`

// TestNodesInOneCpuset: with Config.CPUs every Node process, a handover
// successor included, starts through systemd-run in a user manager scope
// on the cpuset, which reaches the user manager through the harness's
// XDG_RUNTIME_DIR; systemd-run executes the Node in place, so the process
// the harness signals and serves through is the Node's, with its clock
// socket; and GOMAXPROCS divides the cpuset's CPUs, not the harness's,
// among the Nodes.
func TestNodesInOneCpuset(t *testing.T) {
	bin, runtimeDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "systemd-run"), []byte(fakeSystemdRun), 0o700); err != nil { //nolint:gosec // G306: the fake must be executable by its owner
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	// More CPUs than this host has: the fake applies no cpuset, and the
	// rule must divide the cpuset's count, which differs from the host's.
	cpus := fmt.Sprintf("0-%d", 2*runtime.NumCPU()+1)
	topo := standinTopology(t, func(c *Config) { c.CPUs = cpus })
	nodes := addNodes(t, topo, 2, nodeOptions(t, topo))
	if m := waitLine(t, nodes[0], `standin: env GOMAXPROCS=(\S*) procs=(\d+) `); m[1] != strconv.Itoa(runtime.NumCPU()+1) {
		t.Errorf("GOMAXPROCS = %q, want %d: the cpuset's %d CPUs over two Nodes", m[1], runtime.NumCPU()+1, 2*runtime.NumCPU()+2)
	}
	for _, n := range nodes {
		resp := get(t, topo.HTTPClient(), n.URL("http")+"/")
		if pid := resp.Header.Get("X-Standin-Pid"); pid != strconv.Itoa(n.Process().PID()) {
			t.Errorf("%s served by pid %s, want the process the harness started, %d", n.Name(), pid, n.Process().PID())
		}
	}
	if reuseport.SharedPorts() {
		n := nodes[0]
		old := n.Process()
		succ, err := n.Handover(t.Context(), HandoverOptions{})
		if err != nil {
			t.Fatal(err)
		}
		waitOutput(t, succ, `standin: listening as successor\n`)
		waitOutput(t, old, `standin: lingering\n`)
		if err := topo.Advance(t.Context(), standinLinger); err != nil {
			t.Fatal(err)
		}
		if err := n.WaitHolder(t.Context(), succ); err != nil {
			t.Fatal(err)
		}
		if code, err := old.Wait(t.Context()); err != nil || code != 0 {
			t.Fatalf("the old process ended %d, %v; want its Drain to exit 0", code, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(runtimeDir, "scopes")) //nolint:gosec // G304: the fake's record in the test's directory
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(ScopeArgs(cpus, exe), " ")
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	starts := len(nodes)
	if reuseport.SharedPorts() {
		starts++
	}
	if len(lines) != starts {
		t.Fatalf("systemd-run ran %d times, want %d (each Node and the successor): %q", len(lines), starts, lines)
	}
	for _, l := range lines {
		if l != want {
			t.Errorf("systemd-run %s, want %s", l, want)
		}
	}
	code, err := nodes[1].Stop(t.Context())
	if err != nil || code != 0 {
		t.Errorf("Stop = %d, %v; want the Node's own Drain to exit 0", code, err)
	}
}

// TestNodesInOneCpusetSystemd runs a Node in a real user manager scope on
// CPU 0 and reads the CPUs its process may run on. It skips where
// systemd-run --user cannot apply a cpuset: no systemd user manager, or
// no cpuset controller delegated to it.
func TestNodesInOneCpusetSystemd(t *testing.T) {
	run, err := exec.LookPath("systemd-run")
	if err != nil {
		t.Skip("no systemd-run")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, run, ScopeArgs("0", "grep", "Cpus_allowed_list", "/proc/self/status")...).Output() //nolint:gosec // G204: the test's own probe
	if err != nil || strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "Cpus_allowed_list:")) != "0" {
		t.Skipf("systemd-run --user cannot confine a scope to CPU 0 here: %q, %v", out, err)
	}
	topo := standinTopology(t, func(c *Config) { c.CPUs = "0" })
	n := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(n.Process().PID()), "status"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
			if list, ok := proc.ParseCPUList(v); !ok || len(list) != 1 || list[0] != 0 {
				t.Errorf("the Node may run on CPUs %q, want 0", strings.TrimSpace(v))
			}
			return
		}
	}
	t.Errorf("no Cpus_allowed_list in the Node's status")
}
