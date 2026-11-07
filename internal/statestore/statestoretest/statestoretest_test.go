// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package statestoretest_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest"
)

// Tests of the test kit itself: the Fake answers every call it attempts
// and fails the others with their RZ-STS codes, and the Model follows the
// GCRA, Quota reserve and Quota refund rules.

func start() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

func gcra(d statestore.Digest, l statestore.GCRALimit) *statestore.Call {
	return &statestore.Call{Kind: statestore.OpGCRA, Timeout: 50 * time.Millisecond, GCRA: statestore.GCRA{
		Policy: "p", Digest: d, Limits: []statestore.GCRALimit{l}, Out: make([]statestore.GCRAOutcome, 1),
	}}
}

// quota reserves one unit of a 5-unit hourly window.
func quota(d statestore.Digest) *statestore.Call {
	return &statestore.Call{Kind: statestore.OpQuota, Timeout: 50 * time.Millisecond, Quota: statestore.Quota{
		Name: "q", Window: time.Hour, Limit: 5, Digest: d,
	}}
}

func lookup() *statestore.Call {
	return &statestore.Call{Kind: statestore.OpCacheGet, Timeout: 50 * time.Millisecond}
}

func budget() *statestore.RequestBudget {
	rb := statestore.NewRequestBudget(time.Second, 0)
	return &rb
}

var limit = statestore.GCRALimit{Requests: 10, Window: time.Second, Burst: 10}

// TestFakeDefaults checks the Fake's answers: every consumptive call is
// allowed, equal digests merging into a script_multi round trip and a
// Quota window starting on its boundary; lookups miss; writes apply; a
// lookup sent to Consume fails; and it supports scripts alone.
func TestFakeDefaults(t *testing.T) {
	f := &statestoretest.Fake{}
	ctx := context.Background()
	d1, d2 := statestore.DigestOf("a"), statestore.DigestOf("b")
	g, q, other := gcra(d1, limit), quota(d1), quota(d2)
	before := time.Now()
	if n := f.Consume(ctx, budget(), []*statestore.Call{g, q, other}); n != 2 {
		t.Fatalf("merged %d calls, want 2 sharing a digest", n)
	}
	if !g.Done || !g.GCRA.Allowed || g.GCRA.Denied != -1 || !q.Done || !q.Quota.Allowed || q.Trip != statestore.TripScriptMulti || q.Batch != 0 {
		t.Fatalf("default answers: %+v %+v", g, q)
	}
	if ws := q.Quota.WindowStart; ws.After(q.Quota.ServerNow) || ws.Before(before.Truncate(time.Hour)) || !ws.Equal(ws.Truncate(time.Hour)) {
		t.Fatalf("window start %v for server time %v, want its hour boundary", ws, q.Quota.ServerNow)
	}
	if other.Done {
		t.Fatal("call with another digest answered in the merged trip")
	}
	f.Consume(ctx, budget(), []*statestore.Call{other})
	if other.Trip != statestore.TripSingle || other.Batch != -1 {
		t.Fatalf("single trip labeled %d/%d", other.Trip, other.Batch)
	}
	l := lookup()
	l.Lookup.Found = true
	f.Read(ctx, budget(), []*statestore.Call{l})
	if !l.Done || l.Lookup.Found || l.Trip != statestore.TripPipeline {
		t.Fatalf("default lookup: %+v", l)
	}
	w := &statestore.Write{Kind: statestore.OpRefund, Timeout: time.Second}
	f.Write(ctx, []*statestore.Write{w})
	if !w.Applied || w.Err != nil {
		t.Fatalf("default write: %+v", w)
	}
	bad := lookup()
	f.Consume(ctx, budget(), []*statestore.Call{bad})
	if bad.Err != statestore.ErrFailed {
		t.Fatalf("lookup in Consume: %v", bad.Err)
	}
	if n := f.Consume(ctx, budget(), []*statestore.Call{lookup(), lookup()}); n != 1 {
		t.Fatalf("lookups merged: %d", n)
	}
	if f.Consume(ctx, budget(), nil) != 0 {
		t.Fatal("Consume(nil) took calls")
	}
	f.Read(ctx, budget(), nil)
	f.Write(ctx, nil)
	if !f.AdmitStoreBytes(statestore.CacheKey{}, 1) || !f.RoundTrips() || !f.Supports(statestore.CapScripts) || f.Supports(statestore.CapVectorSets) {
		t.Fatal("default capabilities")
	}
	if s := f.Status(); s != (statestore.Status{}) {
		t.Fatalf("Status = %+v, want the zero Status", s)
	}
}

// TestFakeSpentBudgetAndClose checks the calls the Fake does not attempt:
// a spent per-request budget fails them with RZ-STS-004, a write with no
// timeout too, and once closed every call fails with RZ-STS-002.
func TestFakeSpentBudgetAndClose(t *testing.T) {
	ctx := context.Background()
	f := &statestoretest.Fake{}
	// A budget started an hour ago with a second to spend.
	spent := statestore.NewRequestBudget(time.Second, 0)
	spent.CallTimeout(ctx, time.Now().Add(-time.Hour), time.Second)
	c, l := quota(statestore.DigestOf("k")), lookup()
	f.Consume(ctx, &spent, []*statestore.Call{c})
	f.Read(ctx, &spent, []*statestore.Call{l})
	if c.Err != statestore.ErrNotAttempted || c.Done || l.Err != statestore.ErrNotAttempted || l.Done {
		t.Fatalf("spent budget: Consume %v, Read %v; want RZ-STS-004", c.Err, l.Err)
	}
	untimed := &statestore.Write{Kind: statestore.OpRefund}
	f.Write(ctx, []*statestore.Write{untimed})
	if untimed.Err != statestore.ErrNotAttempted || untimed.Applied {
		t.Fatalf("write without a timeout: %+v", untimed)
	}

	if err := f.Close(ctx); err != nil {
		t.Fatal(err)
	}
	c, l = quota(statestore.DigestOf("k")), lookup()
	w := &statestore.Write{Kind: statestore.OpRefund, Timeout: time.Second}
	f.Consume(ctx, budget(), []*statestore.Call{c})
	f.Read(ctx, budget(), []*statestore.Call{l})
	f.Write(ctx, []*statestore.Write{w})
	if c.Err != statestore.ErrFailed || l.Err != statestore.ErrFailed || w.Err != statestore.ErrFailed || w.Applied {
		t.Fatalf("after Close: Consume %v, Read %v, Write %v; want RZ-STS-002", c.Err, l.Err, w.Err)
	}
}

func TestFakeContextAndNoBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &statestoretest.Fake{}
	c := quota(statestore.DigestOf("k"))
	f.Consume(ctx, nil, []*statestore.Call{c})
	if c.Err != nil {
		t.Fatalf("no budget, real clock: %v", c.Err)
	}
	cancel()
	c = quota(statestore.DigestOf("k"))
	f.Consume(ctx, nil, []*statestore.Call{c})
	w := &statestore.Write{Kind: statestore.OpRefund, Timeout: time.Second}
	f.Write(ctx, []*statestore.Write{w})
	if c.Err != statestore.ErrNotAttempted || w.Err != statestore.ErrNotAttempted {
		t.Fatalf("ended context: %v, %v", c.Err, w.Err)
	}
	dl, cancel2 := context.WithTimeout(context.Background(), time.Hour)
	defer cancel2()
	c = quota(statestore.DigestOf("k"))
	f.Consume(dl, nil, []*statestore.Call{c})
	if c.Err != nil {
		t.Fatalf("context deadline an hour away: %v", c.Err)
	}
}

// TestFakeConcurrent checks that the Fake is safe for concurrent use:
// calls of every kind race a Close under the race detector, each call
// answered or failed with RZ-STS-002.
func TestFakeConcurrent(t *testing.T) {
	f := &statestoretest.Fake{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				c, l := quota(statestore.DigestOf("k")), lookup()
				w := &statestore.Write{Kind: statestore.OpRefund, Timeout: time.Second}
				f.Consume(context.Background(), budget(), []*statestore.Call{c})
				f.Read(context.Background(), budget(), []*statestore.Call{l})
				f.Write(context.Background(), []*statestore.Write{w})
				for _, err := range []*statestore.Error{c.Err, l.Err, w.Err} {
					if err != nil && err != statestore.ErrFailed {
						t.Errorf("concurrent call failed with %v, want an answer or RZ-STS-002", err)
					}
				}
			}
		})
	}
	wg.Go(func() { _ = f.Close(context.Background()) })
	wg.Wait()
}
