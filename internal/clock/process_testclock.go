// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build ruralz_testclock

package clock

// TestClockBuild reports whether this binary is the controllable-clock
// test build, built with the ruralz_testclock tag, as this one is
// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
// scope"). internal/testkit/binaries sets the tag; release tooling refuses
// every binary built with a tag.
const TestClockBuild = true

// Process returns the clock a process runs on: in this build a Commanded
// clock on file descriptor CommandFD that waits for the harness's first
// command, start, starts at the time it names and moves only on the
// harness's commands (OpenCommanded). ruralzd calls it once at start,
// before anything opens a descriptor or reads the time, and exits 2 when
// it returns ErrNoCommandDescriptor, a controllable-clock build started
// without its command socket, and 1 when the stream ends or begins
// without start (ErrNoStart).
func Process() (Clock, error) {
	c, err := OpenCommanded(CommandFD)
	if err != nil {
		return nil, err
	}
	return c, nil
}
