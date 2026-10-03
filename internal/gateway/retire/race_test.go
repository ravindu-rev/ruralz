// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package retire

import (
	"context"
	"io"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Race test for spec 04 reqs 50 and 51 (test plan item 12): requests pin
// and unpin concurrently with continuous Hot Reloads; each freed snapshot
// sets a poisoned flag (from Binding.Release) and closes its resources,
// and no pinned request ever observes a poisoned snapshot or a closed
// resource. Resources carried over between snapshots close exactly once,
// after the last snapshot sharing them is freed. Reloads continue until
// enough requests ran and one was ended (req 52), paced by request
// progress so the outcome does not depend on scheduling or GOMAXPROCS.

// raceRes is a resource that must never be closed while a pinned request
// can reach it.
type raceRes struct{ closes atomic.Int32 }

func (r *raceRes) Close() error { r.closes.Add(1); return nil }

// poisonBinding poisons its snapshot on Release.
type poisonBinding struct {
	retired  atomic.Bool
	poisoned atomic.Bool
}

func (b *poisonBinding) Retire()  { b.retired.Store(true) }
func (b *poisonBinding) Release() { b.poisoned.Store(true) }

// observeLive reports a pinned request that can reach a freed snapshot or
// a closed resource.
func observeLive(t *testing.T, s *snapshot.Snapshot) {
	if s.Binding.(*poisonBinding).poisoned.Load() {
		t.Error("a pinned request observed a freed snapshot")
	}
	for _, r := range s.Resources {
		if r.(*raceRes).closes.Load() != 0 {
			t.Error("a pinned request observed a closed resource")
		}
	}
}

// held is one request the race test keeps pinned across retirements.
type held struct {
	s      *snapshot.Snapshot
	stripe emit.Stripe
	rec    *snapshot.PinnedRequest
}

func TestReq50Req51NoRequestObservesAClosedResource(t *testing.T) {
	const (
		workers  = 16
		slow     = 4    // workers holding their pin across several retirements
		reloads  = 200  // at least this many Hot Reloads
		minPins  = 2000 // and at least this many completed requests
		carryFor = 5    // a shared pool is carried over by 5 consecutive snapshots
		bound    = 30 * time.Second
	)
	h := New(Config{
		Clock:        clock.Real(),
		Stripes:      8,
		Grace:        5 * time.Millisecond,
		EndingBound:  20 * time.Millisecond,
		PollInterval: time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- h.Run(ctx) }()
	var runErr error
	stopRun := sync.OnceFunc(func() {
		cancel()
		runErr = <-runDone
	})
	defer stopRun()

	var all []*raceRes
	pools := map[int]*raceRes{}
	mk := func(i int) *snapshot.Snapshot {
		own := &raceRes{}
		pool, ok := pools[i/carryFor]
		if !ok {
			pool = &raceRes{}
			pools[i/carryFor] = pool
			all = append(all, pool)
		}
		all = append(all, own)
		s := newSnap("snapshot-"+strconv.Itoa(i), nil, own, pool)
		s.Binding = &poisonBinding{}
		return s
	}
	if _, err := h.Publish(mk(0)); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var pinsDone, endedN atomic.Int64
	progress := make(chan struct{}, 1) // a request finished since the last drain
	var started, wg sync.WaitGroup
	started.Add(workers)
	stopWorkers := sync.OnceFunc(func() {
		stop.Store(true)
		wg.Wait()
	})
	defer stopWorkers()
	for w := range emit.Stripe(workers) {
		wg.Go(func() {
			markStarted := sync.OnceFunc(started.Done)
			defer markStarted()
			rec := &snapshot.PinnedRequest{Cancel: func() {}}
			stripe := w
			for !stop.Load() {
				rec.Mu.Lock()
				rec.Ended, rec.Committed = snapshot.EndNone, false
				rec.Mu.Unlock()
				s := h.Pin(stripe, rec)
				if s == nil {
					t.Error("Pin returned nil while a snapshot is published")
					return
				}
				markStarted()
				for range 3 {
					observeLive(t, s)
					if w < slow {
						time.Sleep(3 * time.Millisecond)
					} else {
						runtime.Gosched()
					}
				}
				if ended(rec) == snapshot.EndGrace {
					endedN.Add(1)
				}
				h.Unpin(s, stripe, rec)
				pinsDone.Add(1)
				select {
				case progress <- struct{}{}:
				default:
				}
			}
		})
	}
	allStarted := make(chan struct{})
	go func() { started.Wait(); close(allStarted) }() // ends once every worker started or returned
	select {
	case <-allStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("a worker never pinned")
	}

	deadline := time.Now().Add(bound)
	i := 0
	publish := func() {
		t.Helper()
		i++
		wctx, wcancel := context.WithDeadline(context.Background(), deadline)
		err := h.WaitActivate(wctx)
		wcancel()
		if err != nil {
			t.Fatalf("reload %d: WaitActivate: %v", i, err)
		}
		if _, err := h.Publish(mk(i)); err != nil {
			t.Fatalf("reload %d: Publish: %v", i, err)
		}
		if c := h.Counts(); c.Retired > DefaultK || c.Closing+c.Ending > 1 {
			t.Fatalf("reload %d: retirement bound broken: %+v", i, c)
		}
	}
	// holdAcrossRetirements pins K + 1 consecutive snapshots with dedicated
	// requests, so the first one becomes closing at the (K + 1)th
	// retirement whatever the workers do, and ending after grace: its
	// request is ended with RZ-RT-014 while its snapshot is neither freed
	// nor closed (req 52 "never freed early").
	holdAcrossRetirements := func() {
		t.Helper()
		var hs []held
		for j := range DefaultK + 1 {
			hd := held{stripe: emit.Stripe(j), rec: &snapshot.PinnedRequest{Cancel: func() {}}}
			hd.s = h.Pin(hd.stripe, hd.rec)
			hs = append(hs, hd)
			publish()
		}
		waitFor(t, "the held request ended at grace end", func() bool { return ended(hs[0].rec) == snapshot.EndGrace })
		observeLive(t, hs[0].s)
		endedN.Add(1)
		for _, hd := range hs {
			h.Unpin(hd.s, hd.stripe, hd.rec)
		}
	}

	for i < reloads || pinsDone.Load() < minPins || endedN.Load() == 0 {
		if time.Now().After(deadline) {
			switch {
			case pinsDone.Load() < minPins:
				t.Fatalf("only %d requests ran in %v, want %d", pinsDone.Load(), bound, minPins)
			case endedN.Load() == 0:
				t.Fatal("no request was ended; the test never reached an ending snapshot")
			default:
				t.Fatalf("only %d reloads in %v, want %d", i, bound, reloads)
			}
		}
		if i == reloads/2 {
			holdAcrossRetirements()
			continue
		}
		// Pace on progress: at least one request finishes after each
		// reload, so the reloads never outrun the workers.
		select {
		case <-progress:
		default:
		}
		publish()
		select {
		case <-progress:
		case <-time.After(time.Second):
		}
	}
	stopWorkers()
	t.Logf("%d reloads, %d requests, %d ended at grace end", i, pinsDone.Load(), endedN.Load())

	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	if err := h.Close(cctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stopRun()
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	for i, r := range all {
		if n := r.closes.Load(); n != 1 {
			t.Fatalf("resource %d closed %d times, want exactly 1", i, n)
		}
	}
}

// TestReq50Req51ConcurrentUnpinsFreeOnce pins many requests on one
// snapshot, retires it and unpins from many goroutines: it is freed once,
// only after the last unpin.
func TestReq50Req51ConcurrentUnpinsFreeOnce(t *testing.T) {
	x := newHarness(t, Config{}, true)
	pool := &fakeCloser{name: "pool"}
	a := newSnap("a", x.log, pool)
	x.publish(a)
	var ps []pinned
	for i := range 64 {
		ps = append(ps, x.pin(emit.Stripe(i)))
	}
	x.publish(newSnap("b", x.log))
	var wg sync.WaitGroup
	for _, p := range ps[1:] {
		wg.Go(func() { x.unpin(p) })
	}
	wg.Wait()
	x.idle()
	if pool.isClosed() || bindingOf(a).releases.Load() != 0 {
		t.Fatal("the snapshot was freed with one request still pinned")
	}
	x.unpin(ps[0])
	x.waitState(a, freed)
	waitFor(t, "pool closed", pool.isClosed)
	x.idle()
	if n := pool.closed.Load(); n != 1 {
		t.Fatalf("pool closed %d times, want 1", n)
	}
	if n := bindingOf(a).releases.Load(); n != 1 {
		t.Fatalf("Release called %d times, want 1", n)
	}
}

var _ io.Closer = (*raceRes)(nil)
