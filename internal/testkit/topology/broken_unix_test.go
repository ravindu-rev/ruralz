// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package topology

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A running Node that does not answer a command in time breaks the clock
// for good: its stream is out of step, so later commands fail too instead
// of reading a late answer. A Node stopped behind the harness's back, not
// through Node.Signal, shows it.
func TestClockBreaksOnAnUnansweredCommand(t *testing.T) {
	topo := standinTopology(t, func(c *Config) { c.KillAfter = time.Second })
	p := addNodes(t, topo, 1, nodeOptions(t, topo))[0].Process()
	if err := p.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Signal(syscall.SIGCONT) }()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := topo.Advance(ctx, time.Second)
	if err == nil || !strings.Contains(err.Error(), "clock broken") {
		t.Fatalf("Advance past a frozen Node = %v, want the clock broken", err)
	}
	if again := topo.Advance(t.Context(), time.Second); again == nil || again.Error() != err.Error() {
		t.Errorf("the next Advance = %v, want the same breakage", again)
	}
}
