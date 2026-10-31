// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clock

import "testing"

// Every build but the controllable-clock one runs on Real.
func TestProcessPicksTheBuildClock(t *testing.T) {
	if TestClockBuild {
		t.Skip("internal/testkit/binaries runs the controllable-clock build's Process with and without its command socket")
	}
	c, err := Process()
	if err != nil {
		t.Fatalf("Process() = %v", err)
	}
	if _, ok := c.(realClock); !ok {
		t.Fatalf("Process() = %T, want the real clock", c)
	}
}
