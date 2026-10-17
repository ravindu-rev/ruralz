// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// failingClock is an Options.Now that counts its reads and panics with
// "clock failure" from read fail on (0 never panics).
type failingClock struct {
	fail  int64
	reads atomic.Int64
}

func (c *failingClock) now() time.Time {
	if n := c.reads.Add(1); c.fail > 0 && n >= c.fail {
		panic("clock failure")
	}
	return time.Unix(0, 0)
}

// TestNowPanicRecovered covers the WP-38 S2 round-4 minor that the pooled
// loop read the yield timer outside the recovered region, so a panicking
// Options.Now crashed the process on 2 or more workers while the inline
// path let it reach the caller unwrapped: one member of the class "a
// panic surfaces differently inline and pooled". For 1, 2 and 4 workers
// and every clock read after the workers' creation (the first read
// before a unit of Phase selection, of the Upstream legs and of the
// Routes, whichever goroutine makes it), the caller's recover sees the
// same *unitPanic carrying "clock failure".
func TestNowPanicRecovered(t *testing.T) {
	b := newBuilder()
	for i := range 3 {
		p, h := fmt.Sprintf("p%d", i), fmt.Sprintf("h%d", i)
		b.policy(p, v1alpha1.PolicyTypeRateLimit, nil)
		b.policy(h, v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
		b.upstream(fmt.Sprintf("u%d", i), h)
		b.route(fmt.Sprintf("r%d", i), []string{p}, nil, nil)
	}
	bundle := b.bundle()
	run := func(workers int, c *failingClock) (v any) {
		defer func() { v = recover() }()
		_, _, _ = mustNew(t, nil, Options{Now: c.now}).Run(t.Context(), bundle, nil, workers)
		return nil
	}
	for _, workers := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			clean := &failingClock{}
			if v := run(workers, clean); v != nil {
				t.Fatalf("a clean run panicked: %v", v)
			}
			// One read per worker created, then one before each unit:
			// 6 attachKeys, 3 Upstreams and 3 Routes.
			total := clean.reads.Load()
			if want := int64(workers + 12); total != want {
				t.Fatalf("%d clock reads, want %d", total, want)
			}
			for fail := int64(workers + 1); fail <= total; fail++ {
				v := run(workers, &failingClock{fail: fail})
				up, ok := v.(*unitPanic)
				if !ok || up.value != "clock failure" {
					t.Errorf("clock panicking from read %d: recovered %T %v, want a *unitPanic of the clock failure", fail, v, v)
				}
			}
		})
	}
}
