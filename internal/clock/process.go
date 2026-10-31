// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !ruralz_testclock

package clock

// TestClockBuild reports whether this binary is the controllable-clock
// test build, built with the ruralz_testclock tag; it is false in every
// other build, the released ones included.
const TestClockBuild = false

// Process returns the clock a process runs on: Real in every build but
// the controllable-clock one (process_testclock.go). ruralzd calls it once
// at start and passes the clock to every timer-driven component.
func Process() (Clock, error) { return Real(), nil }
