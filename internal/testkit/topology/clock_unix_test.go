// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package topology

import (
	"syscall"
	"testing"
)

// A Node stopped with SIGSTOP misses advances, which do not wait for it,
// and catches up when SIGCONT resumes it.
func TestClockHoldsAStoppedNode(t *testing.T) {
	topo := standinTopology(t)
	nodes := addNodes(t, topo, 2, nodeOptions(t, topo))
	if err := nodes[0].Signal(t.Context(), syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	if err := topo.Advance(t.Context(), standinTimer); err != nil {
		t.Fatalf("Advance with a stopped Node = %v", err)
	}
	client := topo.HTTPClient()
	if resp := get(t, client, nodes[1].URL("http")+"/"); resp.Header.Get("X-Standin-Timer") != "fired" {
		t.Error("the running Node's timer did not fire")
	}
	if err := nodes[0].Signal(t.Context(), syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	resp := get(t, client, nodes[0].URL("http")+"/")
	if got := clockReading(t, resp); !got.Equal(topo.Now()) || resp.Header.Get("X-Standin-Timer") != "fired" {
		t.Errorf("the resumed Node reads %s with its timer %s, want %s and fired", format(got), resp.Header.Get("X-Standin-Timer"), format(topo.Now()))
	}
}
