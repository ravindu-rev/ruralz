// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package statestoretest

import (
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// TestTimed checks the real-time rule of the suite: a span that held runs
// its checks once; on real time a span that failed is attempted again,
// each attempt numbered for keys of its own, and after timedAttempts the
// case skips rather than fails, so its result never depends on the
// machine's speed.
func TestTimed(t *testing.T) {
	type outcome struct {
		tries   []int
		checked int
		skipped bool
		passed  bool
	}
	run := func(name string, exact bool, held func(try int) bool) outcome {
		var o outcome
		o.passed = t.Run(name, func(t *testing.T) {
			s := &suite{t: t, h: Harness{Exact: exact}}
			defer func() { o.skipped = t.Skipped() }()
			s.timed(func(try int) (func(), bool) {
				o.tries = append(o.tries, try)
				return func() { o.checked++ }, held(try)
			})
		})
		return o
	}
	if o := run("fake clock", true, func(int) bool { return true }); len(o.tries) != 1 || o.checked != 1 || o.skipped || !o.passed {
		t.Errorf("fake clock: %+v, want one attempt, checked once", o)
	}
	if o := run("held at once", false, func(int) bool { return true }); len(o.tries) != 1 || o.checked != 1 || o.skipped || !o.passed {
		t.Errorf("held at once: %+v, want one attempt, checked once", o)
	}
	if o := run("held on the second attempt", false, func(try int) bool { return try == 1 }); len(o.tries) != 2 || o.checked != 1 || o.skipped || !o.passed {
		t.Errorf("held on the second attempt: %+v, want two attempts, checked once", o)
	}
	o := run("never held", false, func(int) bool { return false })
	if len(o.tries) != timedAttempts || o.checked != 0 || !o.skipped || !o.passed {
		t.Errorf("never held on real time: %+v, want %d attempts, no check, a skip", o, timedAttempts)
	}
	for i, try := range o.tries {
		if try != i {
			t.Errorf("attempt %d numbered %d", i, try)
		}
	}
	if tryTag(0) != "" || tryTag(1) == "" || tryTag(1) == tryTag(2) {
		t.Errorf("tryTag: %q %q %q, want the first empty and the others distinct", tryTag(0), tryTag(1), tryTag(2))
	}
}

// TestSameWindow checks the one-window precondition of the Quota cases.
func TestSameWindow(t *testing.T) {
	w := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	a, b, c := &statestore.Quota{WindowStart: w}, &statestore.Quota{WindowStart: w}, &statestore.Quota{WindowStart: w.Add(time.Hour)}
	if !sameWindow(a) || !sameWindow(a, b) || sameWindow(a, b, c) || sameWindow(c, a) {
		t.Error("sameWindow does not compare every window start with the first")
	}
}
