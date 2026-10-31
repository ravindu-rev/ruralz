// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// Tests for the Runtime part of the process harness: Stop through the
// Drain on the clock, SIGKILL after KillAfter, Kill and Start, Hot Reload
// by file swap or SIGHUP, and the handover to a successor.

// Stop sends SIGTERM and advances the clock through the Drain in whole
// DrainSteps until the Node exits: the stand-in stops accepting after its
// 5 s accept window and exits 0, releasing the lock.
func TestStopDrainsThroughTheClock(t *testing.T) {
	topo := standinTopology(t)
	n := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	if _, err := n.Holder(); err != nil {
		t.Fatalf("holder.json before the Drain: %v", err)
	}
	before := topo.Now()
	code, err := n.Stop(t.Context())
	if err != nil || code != 0 {
		t.Fatalf("Stop = %d, %v; want exit 0", code, err)
	}
	moved := topo.Now().Sub(before)
	if moved < standinAcceptWindow || moved > DefaultKillAfter || moved%DrainStep != 0 {
		t.Errorf("Stop moved the clock %v, want whole %v steps from the %v accept window to %v", moved, DrainStep, standinAcceptWindow, DefaultKillAfter)
	}
	out, _ := n.Process().Output()
	if !bytes.Contains(out, []byte("standin: draining\n")) || !bytes.Contains(out, []byte("standin: stop accepting\n")) {
		t.Errorf("the Node's output lacks its Drain:\n%s", out)
	}
	if _, err := n.Holder(); !errors.Is(err, nodedir.ErrNoHolder) {
		t.Errorf("holder.json after the Drain: %v, want none", err)
	}
	if code, err := n.Stop(t.Context()); err != nil || code != 0 {
		t.Errorf("Stop of a stopped Node = %d, %v", code, err)
	}
}

// A Node still running KillAfter of real time after SIGTERM gets SIGKILL,
// as systemd's TimeoutStopSec does.
func TestStopKillsAfterKillAfter(t *testing.T) {
	const killAfter = 1500 * time.Millisecond
	topo := standinTopology(t, func(c *Config) {
		c.KillAfter = killAfter
		c.Env = append(c.Env, standinStubbornEnv+"=1")
	})
	n := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	before := topo.Now()
	start := time.Now()
	code, err := n.Stop(t.Context())
	if err != nil || code != -1 {
		t.Fatalf("Stop = %d, %v; want -1, ended by a signal", code, err)
	}
	if took := time.Since(start); took < killAfter {
		t.Errorf("SIGKILL came after %v, want %v", took, killAfter)
	}
	if moved := topo.Now().Sub(before); moved > killAfter+DrainStep {
		t.Errorf("Stop moved the clock %v, want at most KillAfter", moved)
	}
	if sig, ok := n.Process().Signaled(); ok && sig.String() != "killed" {
		t.Errorf("ended by %v, want SIGKILL", sig)
	}
}

// Kill ends the Node at once; Start runs it again on the same directory,
// ports and render, appending to its logs.
func TestKillAndStartAgain(t *testing.T) {
	topo := standinTopology(t)
	n := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	first := n.Process()
	port, _ := n.Port("http")
	if err := n.Kill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if first.State() == nil || first.State().ExitCode() != -1 {
		t.Fatalf("after Kill the process state is %v", first.State())
	}
	if err := n.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n.Process() == first || len(n.Processes()) != 2 {
		t.Fatal("Start did not start a new process")
	}
	if p, _ := n.Port("http"); p != port {
		t.Errorf("the restarted Node listens on %d, want its port %d", p, port)
	}
	resp := get(t, topo.HTTPClient(), n.URL("http")+"/")
	if pid := resp.Header.Get("X-Standin-Pid"); pid != strconv.Itoa(n.Process().PID()) {
		t.Errorf("served by pid %s, want the new process %d", pid, n.Process().PID())
	}
	log, err := os.ReadFile(filepath.Join(n.LogDir(), n.Name()+".out"))
	if err != nil || bytes.Count(log, []byte("standin: ready ready")) != 2 {
		t.Errorf("the log holds %d boots (%v), want both", bytes.Count(log, []byte("standin: ready ready")), err)
	}
	if err := n.Start(t.Context()); err == nil {
		t.Error("Start of a running Node succeeded")
	}
	if err := n.Restart(t.Context()); err != nil {
		t.Fatalf("Restart = %v", err)
	}
	if len(n.Processes()) != 3 {
		t.Errorf("Restart left %d processes, want 3", len(n.Processes()))
	}
}

// Reload swaps the test render atomically, keeping each listener's port
// and giving a new listener a new one; the Node activates it at its next
// watcher poll, one clock second away, or at once on SIGHUP.
func TestReloadKeepsPortsAndActivates(t *testing.T) {
	topo := standinTopology(t)
	n := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	old := n.Render()
	urlBefore, _ := n.SecretValue(refStoreURL)
	added := strings.Replace(testBundle, "  admin:\n", "    - name: extra\n      protocol: http\n      port: 9090\n  admin:\n", 1)
	r, err := n.Reload(t.Context(), []byte(added))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Listeners) != 3 || r.Listeners[0].Port != old.Listeners[0].Port || r.Listeners[1].Port != old.Listeners[1].Port ||
		r.AdminPort != old.AdminPort || r.Listeners[2].Name != "extra" || r.Listeners[2].Port == old.Listeners[0].Port {
		t.Fatalf("reload render %+v, want the old ports kept and a new one for extra", r)
	}
	if v, _ := n.SecretValue(refStoreURL); !bytes.Equal(v, urlBefore) {
		t.Error("a re-render changed a generated secret")
	}
	if err := topo.Advance(t.Context(), standinWatch); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitRevision(t.Context(), r.TestDigest); err != nil {
		t.Fatal(err)
	}
	if resp := get(t, topo.HTTPClient(), n.URL("extra")+"/"); resp.StatusCode != http.StatusOK {
		t.Errorf("the added listener answered %d", resp.StatusCode)
	}

	again := strings.Replace(added, "port: 9090", "port: 9191", 1)
	r2, err := n.Reload(t.Context(), []byte(again))
	if err != nil {
		t.Fatal(err)
	}
	if r2.Listeners[2].Port != r.Listeners[2].Port {
		t.Errorf("a changed authored port moved the test port from %d to %d", r.Listeners[2].Port, r2.Listeners[2].Port)
	}
	if err := n.Signal(t.Context(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitRevision(t.Context(), r2.TestDigest); err != nil {
		t.Fatalf("after SIGHUP: %v", err)
	}
}

// Handover starts a successor on the Node's directory, ports and clock;
// after the holder's linger the successor holds the lock and serves, and
// the old process drains away.
func TestHandoverMovesTheNodeToItsSuccessor(t *testing.T) {
	if !reuseport.SharedPorts() {
		t.Skip("two processes share a port with SO_REUSEPORT only on Linux and the BSDs")
	}
	topo := standinTopology(t)
	n := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	old := n.Process()
	succ, err := n.Handover(t.Context(), HandoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if succ.Name() != n.Name()+"-successor-1" {
		t.Errorf("successor name %s", succ.Name())
	}
	waitOutput(t, succ, `standin: listening as successor\n`)
	waitOutput(t, old, `standin: lingering\n`)
	if err := topo.Advance(t.Context(), standinLinger); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitHolder(t.Context(), succ); err != nil {
		t.Fatal(err)
	}
	if n.Process() != succ {
		t.Fatal("the Node's process is not the successor after WaitHolder")
	}
	if code, err := old.Wait(t.Context()); err != nil || code != 0 {
		t.Fatalf("the old process ended %d, %v; want a Drain to exit 0", code, err)
	}
	resp := get(t, topo.HTTPClient(), n.URL("http")+"/")
	if pid := resp.Header.Get("X-Standin-Pid"); pid != strconv.Itoa(succ.PID()) {
		t.Errorf("served by pid %s, want the successor %d", pid, succ.PID())
	}
	if got := clockReading(t, resp); !got.Equal(topo.Now()) {
		t.Errorf("the successor's clock reads %s, the topology %s", format(got), format(topo.Now()))
	}
	if err := n.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ps := n.Processes(); len(ps) != 2 || ps[0] != old || ps[1] != succ {
		t.Errorf("Processes = %v, want the old process and the successor", ps)
	}
}
