// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package coalesce

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// waitLimit bounds every wait for an event a test caused; reaching it
// fails the test instead of hanging it. No assertion depends on timing.
const waitLimit = 10 * time.Second

// names is the Vary names of the published fixture response.
const names = "accept-encoding"

// key returns the flight key of URI n at generation gen.
func key(n string, gen int64) Key {
	return Key{Cache: statestore.CacheKey{URI: statestore.DigestOf(n), Partition: statestore.DigestOf("anonymous")}, Generation: gen}
}

// digest returns the variant digest of selected values v.
func digest(v string) statestore.Digest { return statestore.DigestOf(v) }

// selector returns a fixed variant and counts its calls.
type selector struct {
	v     statestore.Digest
	calls atomic.Int32
	names atomic.Value
}

// Variant implements Selector.
func (s *selector) Variant(n string) statestore.Digest {
	s.calls.Add(1)
	s.names.Store(n)
	return s.v
}

// newTable returns a Table of strings on a fake clock.
func newTable(o Options) (*Table[string], *clocktest.Fake) {
	clk := clocktest.New(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	o.Clock = clk
	return NewTable[string](o), clk
}

// waited is a follower's result.
type waited struct {
	v string
	o Outcome
}

// waitAsync runs Wait on a new goroutine; the result arrives on the
// returned channel.
func waitAsync(ctx context.Context, tk Ticket[string], sel Selector) <-chan waited {
	ch := make(chan waited, 1)
	go func() {
		v, o := tk.Wait(ctx, sel)
		ch <- waited{v, o}
	}()
	return ch
}

// await returns a waitAsync result, failing the test when it does not
// arrive.
func await(t *testing.T, ch <-chan waited) waited {
	t.Helper()
	select {
	case w := <-ch:
		return w
	case <-time.After(waitLimit):
		t.Fatal("Wait did not return")
		return waited{}
	}
}

// TestLeadThenFollow checks that the first GET miss of a key leads and
// later misses follow, a HEAD miss included.
func TestLeadThenFollow(t *testing.T) {
	tab, _ := newTable(Options{})
	k := key("/v1/catalog", 1)
	if r := tab.Lead(k).Role(); r != Leader {
		t.Fatalf("first Lead = %v, want leader", r)
	}
	if r := tab.Lead(k).Role(); r != Follower {
		t.Errorf("second Lead = %v, want follower", r)
	}
	if r := tab.Follow(k).Role(); r != Follower {
		t.Errorf("Follow = %v, want follower", r)
	}
	if n := tab.Len(); n != 1 {
		t.Errorf("Len() = %d, want 1", n)
	}
}

// TestFollowNeverLeads checks that a HEAD miss without a flight fetches
// alone and starts none, since a HEAD response has no body for GET
// followers.
func TestFollowNeverLeads(t *testing.T) {
	tab, _ := newTable(Options{})
	k := key("/v1/catalog", 1)
	if r := tab.Follow(k).Role(); r != Alone {
		t.Errorf("Follow without a flight = %v, want alone", r)
	}
	if r := tab.Lead(k).Role(); r != Leader {
		t.Errorf("Lead after Follow = %v, want leader", r)
	}
}

// TestGenerationSeparatesFlights checks that a miss that read another
// generation never joins an older fetch.
func TestGenerationSeparatesFlights(t *testing.T) {
	tab, _ := newTable(Options{})
	if r := tab.Lead(key("/v1/catalog", 1)).Role(); r != Leader {
		t.Fatalf("Lead = %v, want leader", r)
	}
	if r := tab.Lead(key("/v1/catalog", 2)).Role(); r != Leader {
		t.Errorf("Lead at a newer generation = %v, want leader", r)
	}
	if r := tab.Lead(key("/v1/other", 1)).Role(); r != Leader {
		t.Errorf("Lead of another URI = %v, want leader", r)
	}
}

// TestFollowerReuse checks the reuse rule: a follower reuses the
// published response when its Vary-selected values under the published
// names match the leader's, and fetches alone otherwise.
func TestFollowerReuse(t *testing.T) {
	tests := []struct {
		name  string
		names string
		sel   Selector
		want  waited
		calls int32
	}{
		{name: "matching values", names: names, sel: &selector{v: digest("gzip")}, want: waited{"resp", Reuse}, calls: 1},
		{name: "other values", names: names, sel: &selector{v: digest("br")}, want: waited{"", Mismatch}, calls: 1},
		{name: "no selector", names: names, sel: nil, want: waited{"", Mismatch}},
		{name: "no Vary", names: "", sel: &selector{v: digest("br")}, want: waited{"resp", Reuse}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tab, _ := newTable(Options{})
			k := key("/v1/catalog", 1)
			lead := tab.Lead(k)
			f := tab.Lead(k)
			ch := waitAsync(t.Context(), f, tt.sel)
			lead.Publish(tt.names, digest("gzip"), "resp")
			if got := await(t, ch); got != tt.want {
				t.Errorf("Wait = %+v, want %+v", got, tt.want)
			}
			if s, ok := tt.sel.(*selector); ok {
				if n := s.calls.Load(); n != tt.calls {
					t.Errorf("selector called %d times, want %d", n, tt.calls)
				}
				if n, _ := s.names.Load().(string); tt.calls > 0 && n != names {
					t.Errorf("selector got names %q, want %q", n, names)
				}
			}
		})
	}
}

// TestManyFollowers checks that every follower of a flight gets the one
// published value.
func TestManyFollowers(t *testing.T) {
	tab, _ := newTable(Options{})
	k := key("/v1/catalog", 1)
	lead := tab.Lead(k)
	const n = 64
	chans := make([]<-chan waited, n)
	for i := range n {
		chans[i] = waitAsync(t.Context(), tab.Lead(k), &selector{v: digest("gzip")})
	}
	lead.Publish(names, digest("gzip"), "resp")
	for _, ch := range chans {
		if got := await(t, ch); got != (waited{"resp", Reuse}) {
			t.Errorf("Wait = %+v, want resp, reuse", got)
		}
	}
}

// TestAbandonReleasesFollowers checks that an abandoned flight sends every
// follower to fetch alone.
func TestAbandonReleasesFollowers(t *testing.T) {
	tab, _ := newTable(Options{})
	k := key("/v1/catalog", 1)
	lead := tab.Lead(k)
	ch := waitAsync(t.Context(), tab.Lead(k), &selector{v: digest("gzip")})
	lead.Abandon()
	if got := await(t, ch); got.o != Abandoned {
		t.Errorf("Wait = %+v, want abandoned", got)
	}
}

// TestResolutionFreesKey checks that once a flight resolves, the next miss
// of its key leads a new flight.
func TestResolutionFreesKey(t *testing.T) {
	for _, publish := range []bool{true, false} {
		tab, _ := newTable(Options{})
		k := key("/v1/catalog", 1)
		lead := tab.Lead(k)
		if publish {
			lead.Publish("", digest(""), "resp")
		} else {
			lead.Abandon()
		}
		if n := tab.Len(); n != 0 {
			t.Errorf("publish %v: Len() = %d after resolution, want 0", publish, n)
		}
		if r := tab.Lead(k).Role(); r != Leader {
			t.Errorf("publish %v: Lead after resolution = %v, want leader", publish, r)
		}
	}
}

// TestResolveOnce checks that a flight resolves exactly once, that only
// its leader resolves it, and that Finish may call Abandon whatever came
// before.
func TestResolveOnce(t *testing.T) {
	t.Run("abandon after publish", func(t *testing.T) {
		tab, _ := newTable(Options{})
		k := key("/v1/catalog", 1)
		lead := tab.Lead(k)
		f := tab.Lead(k)
		lead.Publish("", digest(""), "resp")
		lead.Abandon()
		lead.Streaming(nil)
		if got, o := f.Wait(t.Context(), nil); o != Reuse || got != "resp" {
			t.Errorf("Wait = %q, %v; want resp, reuse", got, o)
		}
	})
	t.Run("publish after abandon", func(t *testing.T) {
		tab, _ := newTable(Options{})
		k := key("/v1/catalog", 1)
		lead := tab.Lead(k)
		f := tab.Lead(k)
		lead.Abandon()
		lead.Publish("", digest(""), "resp")
		if _, o := f.Wait(t.Context(), nil); o != Abandoned {
			t.Errorf("Wait = %v, want abandoned", o)
		}
	})
	t.Run("followers cannot resolve", func(t *testing.T) {
		tab, _ := newTable(Options{})
		k := key("/v1/catalog", 1)
		lead := tab.Lead(k)
		f := tab.Lead(k)
		f.Abandon()
		f.Publish("", digest(""), "forged")
		f.Streaming(nil)
		var alone Ticket[string]
		alone.Abandon()
		alone.Publish("", digest(""), "forged")
		alone.Streaming(nil)
		if n := tab.Len(); n != 1 {
			t.Fatalf("Len() = %d, want the flight still open", n)
		}
		lead.Publish("", digest(""), "resp")
		if got, o := f.Wait(t.Context(), nil); o != Reuse || got != "resp" {
			t.Errorf("Wait = %q, %v; want resp, reuse", got, o)
		}
	})
}

// TestWaitOnNonFollower checks that only a follower waits: a leader's or
// an Alone Ticket returns at once.
func TestWaitOnNonFollower(t *testing.T) {
	tab, _ := newTable(Options{})
	lead := tab.Lead(key("/v1/catalog", 1))
	var alone Ticket[string]
	for _, tk := range []Ticket[string]{lead, alone} {
		if got, o := tk.Wait(t.Context(), nil); o != Abandoned || got != "" {
			t.Errorf("%v Ticket: Wait = %q, %v; want abandoned", tk.Role(), got, o)
		}
	}
}

// TestTableBound checks MaxKeys (target): past it a GET miss fetches
// alone, misses of keys with a flight still follow, and a resolution makes
// room.
func TestTableBound(t *testing.T) {
	tab, _ := newTable(Options{MaxKeys: 2})
	a, b, c := key("/a", 1), key("/b", 1), key("/c", 1)
	la := tab.Lead(a)
	tab.Lead(b)
	if r := tab.Lead(c).Role(); r != Alone {
		t.Errorf("Lead past MaxKeys = %v, want alone", r)
	}
	if r := tab.Lead(a).Role(); r != Follower {
		t.Errorf("Lead of a key with a flight = %v, want follower", r)
	}
	la.Abandon()
	if r := tab.Lead(c).Role(); r != Leader {
		t.Errorf("Lead after a resolution = %v, want leader", r)
	}
	if def := NewTable[string](Options{}); def.maxKeys != MaxKeys || def.bodyWait != BodyWait {
		t.Errorf("defaults %d, %v; want %d, %v", def.maxKeys, def.bodyWait, MaxKeys, BodyWait)
	}
}

// TestStreamingDetaches checks that once the leader's body copy starts, a
// later miss leads a new flight, while the flight's followers still get
// its result.
func TestStreamingDetaches(t *testing.T) {
	tab, _ := newTable(Options{})
	k := key("/v1/catalog", 1)
	lead := tab.Lead(k)
	early := waitAsync(t.Context(), tab.Lead(k), nil)
	lead.Streaming(nil)
	if n := tab.Len(); n != 0 {
		t.Errorf("Len() after Streaming = %d, want 0", n)
	}
	lead2 := tab.Lead(k)
	if lead2.Role() != Leader {
		t.Fatalf("Lead after Streaming = %v, want leader", lead2.Role())
	}
	if r := tab.Follow(k).Role(); r != Follower {
		t.Errorf("Follow after Streaming = %v, want a follower of the new flight", r)
	}
	lead.Publish("", digest(""), "first")
	if got := await(t, early); got != (waited{"first", Reuse}) {
		t.Errorf("early follower got %+v, want the first flight's response", got)
	}
	lead2.Abandon()
}

// TestBodyWait checks the follower waits: unbounded for the leader's
// response headers, at most BodyWait (target) for its body after
// Streaming.
func TestBodyWait(t *testing.T) {
	t.Run("headers wait has no bound", func(t *testing.T) {
		tab, clk := newTable(Options{})
		k := key("/v1/catalog", 1)
		lead := tab.Lead(k)
		ch := waitAsync(t.Context(), tab.Lead(k), nil)
		clk.Advance(time.Hour)
		lead.Publish("", digest(""), "resp")
		if got := await(t, ch); got.o != Reuse {
			t.Errorf("Wait = %+v, want reuse", got)
		}
	})
	t.Run("body completes in time", func(t *testing.T) {
		tab, clk := newTable(Options{})
		k := key("/v1/catalog", 1)
		lead := tab.Lead(k)
		ch := waitAsync(t.Context(), tab.Lead(k), nil)
		lead.Streaming(nil)
		clk.Advance(BodyWait - time.Nanosecond)
		lead.Publish("", digest(""), "resp")
		if got := await(t, ch); got.o != Reuse {
			t.Errorf("Wait = %+v, want reuse", got)
		}
	})
	t.Run("body too slow", func(t *testing.T) {
		tab, clk := newTable(Options{})
		k := key("/v1/catalog", 1)
		lead := tab.Lead(k)
		f := tab.Lead(k)
		lead.Streaming(nil)
		clk.Advance(BodyWait)
		if got, o := f.Wait(t.Context(), nil); o != TimedOut || got != "" {
			t.Errorf("Wait = %q, %v; want timed_out", got, o)
		}
		lead.Abandon()
	})
	t.Run("body too slow for a waiting follower", func(t *testing.T) {
		tab, clk := newTable(Options{BodyWait: 3 * time.Second})
		k := key("/v1/catalog", 1)
		lead := tab.Lead(k)
		ch := waitAsync(t.Context(), tab.Lead(k), nil)
		lead.Streaming(nil)
		waitTimer(t, clk)
		clk.Advance(3 * time.Second)
		if got := await(t, ch); got.o != TimedOut {
			t.Errorf("Wait = %+v, want timed_out", got)
		}
		lead.Abandon()
	})
}

// waitTimer waits until a follower armed its BodyWait timer.
func waitTimer(t *testing.T, clk *clocktest.Fake) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for clk.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no follower armed its body wait")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestCopyStopReleasesFollowers checks the stop signal of the leader's
// body copy (filter.Tee.Stopped): a follower stops waiting as soon as the
// copy stops without completing, long before BodyWait, whether it waited
// before the stop or came after it; a publish that came first still wins.
func TestCopyStopReleasesFollowers(t *testing.T) {
	t.Run("waiting follower", func(t *testing.T) {
		tab, _ := newTable(Options{})
		k := key("/v1/catalog", 1)
		lead := tab.Lead(k)
		ch := waitAsync(t.Context(), tab.Lead(k), nil)
		stop := make(chan struct{})
		lead.Streaming(stop)
		close(stop)
		if got := await(t, ch); got != (waited{"", Stopped}) {
			t.Errorf("Wait = %+v, want stopped", got)
		}
		lead.Abandon()
	})
	t.Run("follower after the stop", func(t *testing.T) {
		tab, _ := newTable(Options{})
		k := key("/v1/catalog", 1)
		lead := tab.Lead(k)
		f := tab.Lead(k)
		stop := make(chan struct{})
		lead.Streaming(stop)
		close(stop)
		if got, o := f.Wait(t.Context(), nil); o != Stopped || got != "" {
			t.Errorf("Wait = %q, %v; want stopped", got, o)
		}
		lead.Abandon()
	})
	t.Run("publish before the stop", func(t *testing.T) {
		tab, _ := newTable(Options{})
		k := key("/v1/catalog", 1)
		lead := tab.Lead(k)
		f := tab.Lead(k)
		stop := make(chan struct{})
		lead.Streaming(stop)
		lead.Publish("", digest(""), "resp")
		close(stop)
		if got, o := f.Wait(t.Context(), nil); o != Reuse || got != "resp" {
			t.Errorf("Wait = %q, %v; want resp, reuse", got, o)
		}
	})
}

// TestFollowerContextEnds checks that a follower whose request ends stops
// waiting, before and after Streaming.
func TestFollowerContextEnds(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		tab, _ := newTable(Options{})
		k := key("/v1/catalog", 1)
		lead := tab.Lead(k)
		f := tab.Follow(k)
		if streaming {
			lead.Streaming(nil)
		}
		ctx, cancel := context.WithCancel(t.Context())
		ch := waitAsync(ctx, f, nil)
		cancel()
		if got := await(t, ch); got.o != Canceled {
			t.Errorf("streaming %v: Wait = %+v, want canceled", streaming, got)
		}
		lead.Abandon()
	}
}

// TestConcurrentLeadAndResolve checks, under the race detector, that
// leads, waits and resolutions on one key from many goroutines agree: each
// follower gets its own flight's result.
func TestConcurrentLeadAndResolve(t *testing.T) {
	tab, _ := newTable(Options{})
	k := key("/v1/catalog", 1)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			tk := tab.Lead(k)
			switch tk.Role() {
			case Leader:
				if i%2 == 0 {
					tk.Streaming(nil)
				}
				tk.Publish("", digest(""), "resp")
			case Follower:
				if v, o := tk.Wait(t.Context(), nil); o != Reuse || v != "resp" {
					t.Errorf("Wait = %q, %v; want resp, reuse", v, o)
				}
			default:
				t.Errorf("Lead = %v", tk.Role())
			}
		})
	}
	wg.Wait()
	if n := tab.Len(); n != 0 {
		t.Errorf("Len() = %d after every flight resolved, want 0", n)
	}
}

// TestStrings checks the names of roles and outcomes.
func TestStrings(t *testing.T) {
	for r, want := range map[Role]string{Alone: "alone", Leader: "leader", Follower: "follower", 9: "unknown"} {
		if got := r.String(); got != want {
			t.Errorf("Role(%d).String() = %q, want %q", r, got, want)
		}
	}
	for o, want := range map[Outcome]string{
		Reuse: "reuse", Mismatch: "mismatch", Abandoned: "abandoned", TimedOut: "timed_out", Canceled: "canceled",
		Stopped: "stopped", 0: "unknown",
	} {
		if got := o.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q, want %q", o, got, want)
		}
	}
}
