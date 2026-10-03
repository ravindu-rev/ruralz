// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package statestoretest_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest"
)

// Tests of the test kit itself (spec 08 section 6): the failure-injecting
// Fake follows the pre-call order of spec 08 req 33 and the RZ-STS
// failures of section 2.4, the Model follows reqs 41 to 43, and the
// conformance suite passes through a Fake wrapping the memory driver.

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

func TestFakeDefaults(t *testing.T) {
	// With no Inner every consumptive call is allowed, lookups miss and
	// writes apply; equal digests merge into a script_multi round trip.
	clk := clocktest.New(start())
	f := &statestoretest.Fake{Clock: clk}
	ctx := context.Background()
	d1, d2 := statestore.DigestOf("a"), statestore.DigestOf("b")
	g, q, other := gcra(d1, limit), quota(d1), quota(d2)
	if n := f.Consume(ctx, budget(), []*statestore.Call{g, q, other}); n != 2 {
		t.Fatalf("merged %d calls, want 2 sharing a digest", n)
	}
	if !g.Done || !g.GCRA.Allowed || g.GCRA.Denied != -1 || !q.Done || !q.Quota.Allowed || q.Trip != statestore.TripScriptMulti || q.Batch != 0 {
		t.Fatalf("default answers: %+v %+v", g, q)
	}
	if !q.Quota.WindowStart.Equal(start().Truncate(time.Hour)) {
		t.Fatalf("window start %v", q.Quota.WindowStart)
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
	trips := f.Trips()
	if len(trips) != 5 || trips[0].Method != statestoretest.MethodConsume || len(trips[0].Ops) != 2 || trips[2].Method != statestoretest.MethodRead {
		t.Fatalf("trips %+v", trips)
	}
	f.ResetTrips()
	if len(f.Trips()) != 0 {
		t.Fatal("ResetTrips kept trips")
	}
	if f.Consume(ctx, budget(), nil) != 0 {
		t.Fatal("Consume(nil) took calls")
	}
	f.Read(ctx, budget(), nil)
	f.Write(ctx, nil)
	if !f.AdmitStoreBytes(statestore.CacheKey{}, 1) || !f.RoundTrips() || !f.Supports(statestore.CapScripts) || f.Supports(statestore.CapVectorSets) {
		t.Fatal("default capabilities")
	}
}

func TestFakePreCallOrder(t *testing.T) {
	// Spec 08 req 33 and the section 6.1 "Pre-call order" row:
	// unsupported beats a spent budget, a spent budget beats an injected
	// breaker, which beats a later in-flight rule.
	ctx := context.Background()
	clk := clocktest.New(start())
	spent := func() *statestore.RequestBudget {
		rb := statestore.NewRequestBudget(time.Millisecond, 0)
		rb.CallTimeout(ctx, clk.Now(), time.Millisecond)
		return &rb
	}
	cases := []struct {
		name  string
		caps  statestore.Capability
		rb    func() *statestore.RequestBudget
		rules []*statestore.Error
		want  *statestore.Error
	}{
		{"unsupported beats spent budget", statestore.CapVectorSets, spent, nil, statestore.ErrUnsupported},
		{"spent budget beats breaker", 0, spent, []*statestore.Error{statestore.ErrBreakerOpen}, statestore.ErrNotAttempted},
		{"breaker beats in-flight", 0, budget, []*statestore.Error{statestore.ErrBreakerOpen, statestore.ErrNotAttempted}, statestore.ErrBreakerOpen},
		{"in-flight", 0, budget, []*statestore.Error{statestore.ErrNotAttempted}, statestore.ErrNotAttempted},
		{"error reply", 0, budget, []*statestore.Error{statestore.ErrFailed}, statestore.ErrFailed},
		{"answered", 0, budget, nil, nil},
	}
	for _, c := range cases {
		f := &statestoretest.Fake{Clock: clk, Caps: c.caps}
		rb := c.rb()
		clk.Advance(time.Second)
		for _, e := range c.rules {
			f.Inject(statestoretest.Rule{Err: e})
		}
		call := quota(statestore.DigestOf("k"))
		f.Consume(ctx, rb, []*statestore.Call{call})
		if call.Err != c.want {
			t.Errorf("%s: Err %v, want %v", c.name, call.Err, c.want)
		}
	}
}

func TestFakeLatencyAndTimeout(t *testing.T) {
	// A latency under the timeout is reported and advances the clock; at
	// or above it the call fails with RZ-STS-001 at the timeout, and with
	// Apply the store still charges it (spec 08 req 34).
	ctx := context.Background()
	clk := clocktest.New(start())
	mem, err := memory.New(ctx, statestore.Config{}, statestore.Deps{Clock: clk}, memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close(ctx) //nolint:errcheck // test cleanup
	f := &statestoretest.Fake{Inner: mem, Clock: clk, Advance: clk.Advance}
	d := statestore.DigestOf("k")

	slow := f.Inject(statestoretest.Rule{Method: statestoretest.MethodConsume, Latency: 10 * time.Millisecond, Times: 1})
	c := quota(d)
	f.Consume(ctx, budget(), []*statestore.Call{c})
	if c.Err != nil || c.Elapsed != 10*time.Millisecond || !clk.Now().Equal(start().Add(10*time.Millisecond)) {
		t.Fatalf("slow call: Err %v Elapsed %v now %v", c.Err, c.Elapsed, clk.Now())
	}
	if f.Hits(slow) != 1 {
		t.Fatalf("rule hits %d", f.Hits(slow))
	}

	f.Inject(statestoretest.Rule{Op: statestore.OpQuota, Latency: time.Second, Apply: true, Times: 1})
	c = quota(d)
	f.Consume(ctx, budget(), []*statestore.Call{c})
	if c.Err != statestore.ErrTimeout || c.Done || c.Elapsed != 50*time.Millisecond {
		t.Fatalf("timed-out call: Err %v Done %v Elapsed %v", c.Err, c.Done, c.Elapsed)
	}
	f.Inject(statestoretest.Rule{Err: statestore.ErrBreakerOpen, Latency: time.Second, Times: 1})
	c = quota(d)
	f.Consume(ctx, budget(), []*statestore.Call{c})
	if c.Err != statestore.ErrBreakerOpen || c.Elapsed != 0 {
		t.Fatalf("skipped call: Err %v Elapsed %v", c.Err, c.Elapsed)
	}
	f.Inject(statestoretest.Rule{Err: statestore.ErrTimeout, Times: 1})
	c = quota(d)
	f.Consume(ctx, budget(), []*statestore.Call{c})
	if c.Err != statestore.ErrTimeout {
		t.Fatalf("injected timeout: %v", c.Err)
	}
	c = quota(d)
	f.Consume(ctx, budget(), []*statestore.Call{c})
	if c.Err != nil || c.Quota.Used != 3 {
		t.Fatalf("after an applied timeout: Err %v used %d, want 3 (over-charged)", c.Err, c.Quota.Used)
	}
	// Inner answers GCRA decisions and store admission.
	g := gcra(d, statestore.GCRALimit{Requests: 1, Window: time.Hour})
	f.Consume(ctx, budget(), []*statestore.Call{g})
	g2 := gcra(d, statestore.GCRALimit{Requests: 1, Window: time.Hour})
	f.Consume(ctx, budget(), []*statestore.Call{g2})
	if !g.GCRA.Allowed || g2.GCRA.Allowed {
		t.Fatal("Inner GCRA decisions not passed through")
	}
	if !f.AdmitStoreBytes(statestore.CacheKey{}, 10) {
		t.Fatal("Inner admission not consulted")
	}
	f.Admit = func(statestore.CacheKey, int) bool { return false }
	if f.AdmitStoreBytes(statestore.CacheKey{}, 10) {
		t.Fatal("Admit override ignored")
	}
}

func TestFakeAnswersAndWrites(t *testing.T) {
	ctx := context.Background()
	clk := clocktest.New(start())
	f := &statestoretest.Fake{Clock: clk}
	// A scripted deny.
	f.Inject(statestoretest.Rule{Op: statestore.OpGCRA, Times: 1, Answer: func(c *statestore.Call) {
		c.GCRA.Allowed, c.GCRA.Denied, c.GCRA.RetryAfter = false, 0, 250*time.Millisecond
	}})
	g := gcra(statestore.DigestOf("k"), limit)
	f.Consume(ctx, budget(), []*statestore.Call{g})
	if !g.Done || g.GCRA.Allowed || g.GCRA.RetryAfter != 250*time.Millisecond {
		t.Fatalf("scripted deny: %+v", g.GCRA)
	}
	f.Inject(statestoretest.Rule{Method: statestoretest.MethodRead, Times: 1, Answer: func(c *statestore.Call) {
		c.Lookup.Found, c.Lookup.Entry = true, []byte("hit")
	}})
	l := lookup()
	f.Read(ctx, budget(), []*statestore.Call{l})
	if !l.Done || !l.Lookup.Found {
		t.Fatalf("scripted hit: %+v", l.Lookup)
	}
	f.Inject(statestoretest.Rule{Method: statestoretest.MethodRead, Err: statestore.ErrBreakerOpen, Times: 1})
	a, b := lookup(), lookup()
	f.Read(ctx, budget(), []*statestore.Call{a, b})
	if a.Err != statestore.ErrBreakerOpen || b.Err != statestore.ErrBreakerOpen || b.Batch != 0 {
		t.Fatalf("breaker on a lookup batch: %v %v", a.Err, b.Err)
	}

	// Writes: per-write rules, scripted answers, timeouts, zero timeouts.
	f.Inject(statestoretest.Rule{Op: statestore.OpCacheInvalidate, Err: statestore.ErrFailed, Times: 1})
	f.Inject(statestoretest.Rule{Op: statestore.OpCacheSet, AnswerWrite: func(w *statestore.Write) { w.Err = statestore.ErrFailed }, Times: 1})
	f.Inject(statestoretest.Rule{Op: statestore.OpRefund, AnswerWrite: func(*statestore.Write) {}, Times: 1})
	f.Inject(statestoretest.Rule{Method: statestoretest.MethodWrite, Latency: time.Hour, Times: 1})
	inv := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: time.Second}
	set := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second}
	ref := &statestore.Write{Kind: statestore.OpRefund, Timeout: time.Second}
	slow := &statestore.Write{Kind: statestore.OpRefund, Timeout: time.Second}
	zero := &statestore.Write{Kind: statestore.OpRefund}
	ok := &statestore.Write{Kind: statestore.OpRefund, Timeout: time.Second}
	f.Write(ctx, []*statestore.Write{inv, set, ref, slow, zero, ok})
	switch {
	case inv.Err != statestore.ErrFailed || inv.Applied:
		t.Fatalf("injected write failure: %+v", inv)
	case set.Err != statestore.ErrFailed || set.Applied:
		t.Fatalf("scripted write failure: %+v", set)
	case ref.Err != nil || !ref.Applied:
		t.Fatalf("scripted write success: %+v", ref)
	case slow.Err != statestore.ErrTimeout:
		t.Fatalf("slow write: %+v", slow)
	case zero.Err != statestore.ErrNotAttempted:
		t.Fatalf("zero-timeout write: %+v", zero)
	case !ok.Applied:
		t.Fatalf("plain write: %+v", ok)
	}
	// Rules persist without Times until Clear.
	r := f.Inject(statestoretest.Rule{Err: statestore.ErrFailed})
	for range 3 {
		c := quota(statestore.DigestOf("k"))
		f.Consume(ctx, budget(), []*statestore.Call{c})
		if c.Err != statestore.ErrFailed {
			t.Fatal("persistent rule stopped applying")
		}
	}
	if f.Hits(r) != 3 {
		t.Fatalf("hits %d", f.Hits(r))
	}
	f.Clear()
	c := quota(statestore.DigestOf("k"))
	f.Consume(ctx, budget(), []*statestore.Call{c})
	if c.Err != nil {
		t.Fatal("Clear kept rules")
	}
	// Closed: every call fails with RZ-STS-002.
	if err := f.Close(ctx); err != nil || !f.Closed() {
		t.Fatal("Close")
	}
	c = quota(statestore.DigestOf("k"))
	f.Consume(ctx, budget(), []*statestore.Call{c})
	w := &statestore.Write{Kind: statestore.OpRefund, Timeout: time.Second}
	f.Write(ctx, []*statestore.Write{w})
	if c.Err != statestore.ErrFailed || w.Err != statestore.ErrFailed {
		t.Fatalf("closed fake: %v, %v", c.Err, w.Err)
	}
	f.SetStatus(statestore.Status{BreakerNotClosed: true})
	if !f.Status().BreakerNotClosed {
		t.Fatal("SetStatus ignored")
	}
	local := &statestoretest.Fake{Local: true, Caps: statestore.CapScripts | statestore.CapVectorSets}
	if local.RoundTrips() || !local.Supports(statestore.CapVectorSets) {
		t.Fatal("Local or Caps ignored")
	}
}

func TestFakeAnswerDenyStopsGroup(t *testing.T) {
	// Spec 08 reqs 29, 38 and 44: a scripted deny ends a merged group as
	// on the real drivers; later calls are not answered and stay !Done
	// with a nil Err. A scripted allow answers the whole group.
	ctx := context.Background()
	d := statestore.DigestOf("k")
	cases := []struct {
		name     string
		answer   func(c *statestore.Call)
		answered int
		done     []bool
	}{
		{"deny on the first call", func(c *statestore.Call) {
			c.GCRA.Allowed, c.GCRA.Denied, c.GCRA.RetryAfter = false, 0, time.Second
		}, 1, []bool{true, false}},
		{"deny on the second call", func(c *statestore.Call) {
			c.GCRA.Allowed, c.GCRA.Denied = true, -1
			c.Quota.Allowed, c.Quota.Used = false, 5
		}, 2, []bool{true, true}},
		{"allow both", func(c *statestore.Call) {
			c.GCRA.Allowed, c.GCRA.Denied = true, -1
			c.Quota.Allowed, c.Quota.Used = true, 1
		}, 2, []bool{true, true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &statestoretest.Fake{Clock: clocktest.New(start())}
			answered := 0
			f.Inject(statestoretest.Rule{Op: statestore.OpGCRA, Answer: func(call *statestore.Call) {
				answered++
				c.answer(call)
			}})
			g, q := gcra(d, limit), quota(d)
			q.Done, q.Err = true, statestore.ErrFailed // stale state from an earlier use
			if n := f.Consume(ctx, budget(), []*statestore.Call{g, q}); n != 2 {
				t.Fatalf("Consume took %d calls, want 2", n)
			}
			if answered != c.answered {
				t.Fatalf("Answer called %d times, want %d", answered, c.answered)
			}
			for i, call := range []*statestore.Call{g, q} {
				if call.Done != c.done[i] || call.Err != nil {
					t.Fatalf("call %d: Done %v Err %v, want Done %v and no error", i, call.Done, call.Err, c.done[i])
				}
			}
		})
	}
}

func TestFakeMergedTimeoutIsMin(t *testing.T) {
	// Spec 08 req 29 and the section 6.1 grouping row: the merged round
	// trip's timeout is the smallest of its calls', clamped by the
	// per-request deadline (req 28); a zero anywhere leaves the group not
	// attempted (RZ-STS-004) and charges nothing.
	ctx := context.Background()
	clk := clocktest.New(start())
	mem, err := memory.New(ctx, statestore.Config{}, statestore.Deps{Clock: clk}, memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close(ctx) //nolint:errcheck // test cleanup
	f := &statestoretest.Fake{Inner: mem, Clock: clk, Slot: keys.SlotOf}
	cases := []struct {
		name         string
		gcra, quota  time.Duration
		budget       time.Duration
		wantTimeout  time.Duration
		wantErr      *statestore.Error
		wantCharged  bool
		reverseOrder bool
	}{
		{"zero second", 50 * time.Millisecond, 0, time.Second, 0, statestore.ErrNotAttempted, false, false},
		{"zero first", 50 * time.Millisecond, 0, time.Second, 0, statestore.ErrNotAttempted, false, true},
		{"smaller second", 50 * time.Millisecond, 30 * time.Millisecond, time.Second, 30 * time.Millisecond, nil, true, false},
		{"smaller first", 50 * time.Millisecond, 30 * time.Millisecond, time.Second, 30 * time.Millisecond, nil, true, true},
		{"clamped by the deadline", 50 * time.Millisecond, 30 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond, nil, true, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.ResetTrips()
			d := statestore.DigestOf("k" + strconv.Itoa(i))
			g, q := gcra(d, statestore.GCRALimit{Requests: 1, Window: time.Hour}), quota(d)
			g.Timeout, q.Timeout = c.gcra, c.quota
			calls := []*statestore.Call{g, q}
			if c.reverseOrder {
				calls = []*statestore.Call{q, g}
			}
			rb := statestore.NewRequestBudget(c.budget, 0)
			if n := f.Consume(ctx, &rb, calls); n != 2 {
				t.Fatalf("Consume took %d calls, want 2", n)
			}
			trips := f.Trips()
			if len(trips) != 1 || trips[0].Timeout != c.wantTimeout || trips[0].Err != c.wantErr {
				t.Fatalf("trips %+v, want one with timeout %v and Err %v", trips, c.wantTimeout, c.wantErr)
			}
			for _, call := range calls {
				if call.Err != c.wantErr || call.Done != (c.wantErr == nil) {
					t.Fatalf("%v call: Done %v Err %v", call.Kind, call.Done, call.Err)
				}
			}
			after := quota(d)
			f.Consume(ctx, budget(), []*statestore.Call{after})
			if want := map[bool]int64{false: 1, true: 2}[c.wantCharged]; after.Quota.Used != want {
				t.Fatalf("quota used %d after the group, want %d", after.Quota.Used, want)
			}
		})
	}
}

func TestFakeSlotMerging(t *testing.T) {
	// The Store contract merges adjacent calls by hash slot (spec 08 req
	// 29): with Slot set, calls with different digests in one slot share a
	// round trip as on the real drivers; without it only equal digests do.
	ctx := context.Background()
	a := statestore.DigestOf("a")
	var same, other statestore.Digest
	for i := 0; same == (statestore.Digest{}) || other == (statestore.Digest{}); i++ {
		d := statestore.DigestOf("b" + strconv.Itoa(i))
		switch {
		case keys.DigestSlot(d) == keys.DigestSlot(a) && same == (statestore.Digest{}):
			same = d
		case keys.DigestSlot(d) != keys.DigestSlot(a) && other == (statestore.Digest{}):
			other = d
		}
	}
	cases := []struct {
		name string
		slot func(*statestore.Call) int
		next statestore.Digest
		want int
	}{
		{"slot: same slot merges", keys.SlotOf, same, 2},
		{"slot: other slot splits", keys.SlotOf, other, 1},
		{"slot: equal digest merges", keys.SlotOf, a, 2},
		{"digest: same slot splits", nil, same, 1},
		{"digest: equal digest merges", nil, a, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &statestoretest.Fake{Slot: c.slot}
			calls := []*statestore.Call{gcra(a, limit), quota(c.next)}
			if n := f.Consume(ctx, budget(), calls); n != c.want {
				t.Fatalf("merged %d calls, want %d", n, c.want)
			}
			if trips := f.Trips(); len(trips) != 1 || len(trips[0].Ops) != c.want {
				t.Fatalf("trips %+v", trips)
			}
		})
	}
	// A call without a slot (a lookup) never merges.
	f := &statestoretest.Fake{Slot: keys.SlotOf}
	if n := f.Consume(ctx, budget(), []*statestore.Call{lookup(), lookup()}); n != 1 {
		t.Fatalf("lookups merged: %d", n)
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

func TestFakeConcurrent(t *testing.T) {
	f := &statestoretest.Fake{}
	f.Inject(statestoretest.Rule{Err: statestore.ErrFailed, Times: 50})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				c := quota(statestore.DigestOf("k"))
				f.Consume(context.Background(), budget(), []*statestore.Call{c})
			}
		})
	}
	wg.Wait()
	failed := 0
	for _, tr := range f.Trips() {
		if tr.Err != nil {
			failed++
		}
	}
	if failed != 50 {
		t.Fatalf("%d failed trips, want the rule's 50", failed)
	}
}

func TestConformanceThroughFake(t *testing.T) {
	// The suite passes through a Fake wrapping the memory driver: the
	// Fake is transparent when no rule matches.
	var mu sync.Mutex
	clocks := map[statestore.Driver]*clocktest.Fake{}
	statestoretest.RunConformance(t, statestoretest.Harness{
		Exact: true, Parallel: true, DigestSlot: keys.DigestSlot,
		Open: func(t *testing.T, o statestoretest.OpenOptions) statestore.Driver {
			clk := clocktest.New(start())
			mem, err := memory.New(context.Background(), statestore.Config{}, statestore.Deps{Clock: clk, MACKey: o.MACKey}, memory.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := mem.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			f := &statestoretest.Fake{Inner: mem, Clock: clk, Slot: keys.SlotOf}
			mu.Lock()
			clocks[f] = clk
			mu.Unlock()
			return f
		},
		Advance: func(_ *testing.T, d statestore.Driver, by time.Duration) {
			mu.Lock()
			clk := clocks[d]
			mu.Unlock()
			clk.Advance(by)
		},
		Now: func(d statestore.Driver) time.Time {
			mu.Lock()
			defer mu.Unlock()
			return clocks[d].Now()
		},
	})
}
