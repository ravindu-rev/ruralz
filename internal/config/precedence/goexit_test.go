// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// goexitOracle ends its goroutine with runtime.Goexit, as a test oracle's
// t.FailNow does, for a rule naming exit, and answers like bodyOracle
// otherwise.
type goexitOracle struct{}

func (goexitOracle) ReferencesRequestBody(src string) (bool, error) {
	if strings.Contains(src, "exit") {
		runtime.Goexit()
	}
	return strings.Contains(src, "request.body"), nil
}

// goexitClock is an Options.Now that counts its reads and ends its
// goroutine with runtime.Goexit from read fail on (0 never does).
type goexitClock struct {
	fail  int64
	reads atomic.Int64
}

func (c *goexitClock) now() time.Time {
	if n := c.reads.Add(1); c.fail > 0 && n >= c.fail {
		runtime.Goexit()
	}
	return time.Unix(0, 0)
}

// runCatching runs f on a goroutine of its own and reports what a
// recover there saw and whether f returned; f ending its goroutine with
// runtime.Goexit gives nil and false.
func runCatching(f func()) (v any, returned bool) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { v = recover() }()
		f()
		returned = true
	}()
	<-done
	return v, returned
}

// wantGoexit fails t unless v is the *unitPanic of a runtime.Goexit.
func wantGoexit(t *testing.T, what string, v any, returned bool) {
	t.Helper()
	up, ok := v.(*unitPanic)
	switch {
	case returned:
		t.Errorf("%s: Run returned as if every unit had run", what)
	case !ok:
		t.Errorf("%s: recovered %T %v, want the *unitPanic of the Goexit", what, v, v)
	case !up.goexit || !errors.Is(up, errGoexit) || !strings.Contains(up.Error(), "stopped"):
		t.Errorf("%s: %v, want a Goexit wrapping errGoexit", what, up)
	default:
	}
}

// TestGoexitInUnitRaised covers the WP-38 S2 round-2 minor that a work
// unit ending through runtime.Goexit (an injected Options.Body or
// Options.Now calling t.FailNow, say) made the pooled path return a wrong
// result silently: recover saw no panic, the worker's goroutine ended and
// the others drained, so an authz.cel Policy vanished from the chain with
// its RZ-CFG-038, while inline the same call ended the caller's
// goroutine. The class, one row each, on 1, 2 and 4 workers: Body in
// selectAll (the reviewer's Bundle), and Now at every read before a unit
// of Phase selection, of the Upstream legs and of the Routes (where a
// lost unit left an empty leg, or a nil chain that crashed the indexing
// of the result). Every one reaches the caller's recover as the
// *unitPanic of the Goexit, wrapping errGoexit, never as a result.
func TestGoexitInUnitRaised(t *testing.T) {
	b := newBuilder()
	b.policy("az-exit", v1alpha1.PolicyTypeAuthzCEL, authzCfg("exit && request.body.x"))
	b.policy("cache", v1alpha1.PolicyTypeCache, nil)
	b.policy("cors", v1alpha1.PolicyTypeCORS, nil)
	b.policy("rl", v1alpha1.PolicyTypeRateLimit, nil)
	b.gateway("cors", "rl")
	b.upstream("u")
	b.route("r", []string{"az-exit", "cache"}, nil, []string{"u"})
	oracleBundle := b.bundle()

	c := newBuilder()
	for i := range 4 {
		p, h := fmt.Sprintf("p%d", i), fmt.Sprintf("h%d", i)
		c.policy(p, v1alpha1.PolicyTypeRateLimit, nil)
		c.policy(h, v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
		c.upstream(fmt.Sprintf("u%d", i), h)
		c.route(fmt.Sprintf("r%d", i), []string{p}, nil, []string{fmt.Sprintf("u%d", i)})
	}
	clockBundle := c.bundle()

	for _, workers := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			v, returned := runCatching(func() {
				_, _, _ = mustNew(t, nil, Options{Body: goexitOracle{}}).Run(t.Context(), oracleBundle, nil, workers)
			})
			wantGoexit(t, "Body in selectAll", v, returned)

			clean := &goexitClock{}
			if v, returned := runCatching(func() {
				_, _, _ = mustNew(t, nil, Options{Now: clean.now}).Run(t.Context(), clockBundle, nil, workers)
			}); v != nil || !returned {
				t.Fatalf("a clean run: %v, returned %v", v, returned)
			}
			// One read per worker created, then one before each unit: 8
			// attachKeys, then 4 Upstreams, then 4 Routes.
			created := int64(workers)
			if total := clean.reads.Load(); total != created+16 {
				t.Fatalf("%d clock reads, want %d", total, created+16)
			}
			for fail := created + 1; fail <= created+16; fail++ {
				stage := map[bool]string{true: "Phase selection", false: "Upstream legs"}[fail <= created+8]
				if fail > created+12 {
					stage = "Routes"
				}
				clock := &goexitClock{fail: fail}
				v, returned := runCatching(func() {
					_, _, _ = mustNew(t, nil, Options{Now: clock.now}).Run(t.Context(), clockBundle, nil, workers)
				})
				wantGoexit(t, fmt.Sprintf("Now at read %d (%s)", fail, stage), v, returned)
			}
		})
	}
}

// TestGoexitOutsideUnit pins what Options.Now documents for a read on the
// calling goroutine, outside every work unit (the Gateway list checked on
// worker 0): a runtime.Goexit there ends the calling goroutine, as the
// call itself would, for every worker count, and Run never returns.
func TestGoexitOutsideUnit(t *testing.T) {
	b := newBuilder()
	var gw []string
	for i := range 2 * yieldCheck {
		gw = append(gw, fmt.Sprintf("p%04d", i))
		b.policy(gw[i], v1alpha1.PolicyTypeRateLimit, nil)
	}
	b.gateway(gw...)
	bundle := b.bundle()
	for _, workers := range []int{1, 4} {
		clean := &goexitClock{}
		r := mustNew(t, nil, Options{Now: clean.now})
		s := newState(r, bundle, nil, workers)
		if err := s.selectAll(t.Context()); err != nil {
			t.Fatal(err)
		}
		before := clean.reads.Load()
		s.attachGateway()
		if clean.reads.Load() == before {
			t.Fatalf("workers %d: attachGateway read no clock", workers)
		}
		clock := &goexitClock{fail: before + 1}
		v, returned := runCatching(func() {
			s := newState(mustNew(t, nil, Options{Now: clock.now}), bundle, nil, workers)
			if err := s.selectAll(t.Context()); err != nil {
				t.Error(err)
				return
			}
			s.attachGateway()
		})
		if v != nil || returned {
			t.Errorf("workers %d: recovered %v, returned %v; want the calling goroutine ended", workers, v, returned)
		}
	}
}
