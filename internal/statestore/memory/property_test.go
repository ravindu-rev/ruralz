// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest"
)

// Property and fuzz tests of spec 08 section 6.3 and 05 tests 23 and 25:
// the memory driver equals the reference model (spec 08 req 46), GCRA
// admits at most requests + burst in any window of one limit and
// converges to requests per window, and concurrent Quota reservations
// never exceed the limit.

// gcraScenario is one random limit set and arrival sequence.
type gcraScenario struct {
	limits []statestore.GCRALimit
	gaps   []time.Duration // time before each arrival
}

// newScenario draws a scenario: 1 to 3 limits with T of at least 1 µs,
// bursts from 0 to 2 × requests, arrivals in bursts and gaps.
func newScenario(r *rand.Rand, arrivals int) gcraScenario {
	var sc gcraScenario
	windows := []time.Duration{time.Millisecond, 10 * time.Millisecond, 100 * time.Millisecond, time.Second, 1500 * time.Millisecond, time.Minute}
	for range 1 + r.IntN(3) {
		w := windows[r.IntN(len(windows))]
		req := 1 + r.Int64N(min(50, w.Microseconds()))
		sc.limits = append(sc.limits, statestore.GCRALimit{Requests: req, Window: w, Burst: r.Int64N(2*req + 1)})
	}
	for range arrivals {
		switch r.IntN(4) {
		case 0:
			sc.gaps = append(sc.gaps, 0) // same instant
		case 1:
			sc.gaps = append(sc.gaps, time.Duration(r.Int64N(1000))*time.Microsecond)
		default:
			w := sc.limits[r.IntN(len(sc.limits))].Window
			sc.gaps = append(sc.gaps, time.Duration(r.Int64N(int64(w)/4+1)))
		}
	}
	return sc
}

// runScenario plays sc against a memory driver and the model, failing on
// the first difference, and returns the admission times.
func runScenario(t testing.TB, sc gcraScenario) []time.Time {
	t.Helper()
	clk := clocktest.New(start())
	d, err := New(context.Background(), statestore.Config{}, statestore.Deps{Clock: clk}, Options{SweepInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer closeDriver(t, d)
	model := statestoretest.NewModel()
	dg := statestore.DigestOf("user")
	var admitted []time.Time
	for i, gap := range sc.gaps {
		clk.Advance(gap)
		c := gcraCall("p", dg, sc.limits...)
		d.Consume(context.Background(), nil, []*statestore.Call{c})
		if c.Err != nil || !c.Done {
			t.Fatalf("arrival %d: Err %v", i, c.Err)
		}
		want := statestore.GCRA{Policy: "p", Digest: dg, Limits: sc.limits, Out: make([]statestore.GCRAOutcome, len(sc.limits))}
		if !model.GCRA(clk.Now(), &want) {
			t.Fatalf("arrival %d: model rejects %+v", i, sc.limits)
		}
		g := &c.GCRA
		if g.Allowed != want.Allowed || g.Denied != want.Denied || g.RetryAfter != want.RetryAfter ||
			!slices.Equal(g.Out, want.Out) || !g.ServerNow.Equal(want.ServerNow) {
			t.Fatalf("arrival %d limits %+v: memory %+v, model %+v", i, sc.limits, *g, want)
		}
		for _, l := range sc.limits {
			st, ok := inspect(d, statestoretest.Key{Kind: statestoretest.KeyRateLimit, Policy: "p", Limit: l, Digest: dg})
			mt, mok := model.TAT("p", l, dg)
			if ok && (!mok || st.Value != strconv.FormatFloat(mt, 'g', 17, 64)) {
				t.Fatalf("arrival %d: TAT %s, model %v", i, st.Value, mt)
			}
		}
		if g.Allowed {
			// Windows are measured on the scripts' time base: server TIME
			// in whole microseconds.
			admitted = append(admitted, g.ServerNow)
		}
	}
	return admitted
}

// checkWindows asserts that no half-open window [t, t + W) of any limit
// holds more than requests + burst admissions (spec 08 req 41), time
// being the server's microseconds.
func checkWindows(t testing.TB, sc gcraScenario, admitted []time.Time) {
	t.Helper()
	for _, l := range sc.limits {
		bound := int(l.Requests + l.Burst)
		j := 0
		for i := range admitted {
			for j < len(admitted) && admitted[j].Sub(admitted[i]) < l.Window {
				j++
			}
			if n := j - i; n > bound {
				t.Fatalf("limit %+v admitted %d in one window from %v, bound %d", l, n, admitted[i], bound)
			}
		}
	}
}

func TestGCRAProperty(t *testing.T) {
	// Done when: "GCRA property test with clocktest.Fake".
	r := newRand(1, 2)
	for i := range 300 {
		sc := newScenario(r, 400)
		admitted := runScenario(t, sc)
		checkWindows(t, sc, admitted)
		if t.Failed() {
			t.Fatalf("scenario %d", i)
		}
	}
}

func TestGCRAConverges(t *testing.T) {
	// Sustained demand converges to requests per window: over k windows
	// the admissions lie within [k × requests, k × requests + burst + 1].
	for _, l := range []statestore.GCRALimit{
		{Requests: 10, Window: time.Second, Burst: 10},
		{Requests: 10, Window: time.Second, Burst: 0},
		{Requests: 3, Window: time.Second, Burst: 3},
		{Requests: 7, Window: 100 * time.Millisecond, Burst: 2},
	} {
		sc := gcraScenario{limits: []statestore.GCRALimit{l}}
		step := l.Window / time.Duration(4*l.Requests)
		const k = 20
		for range int(k * l.Window / step) {
			sc.gaps = append(sc.gaps, step)
		}
		admitted := runScenario(t, sc)
		lo, hi := k*int(l.Requests), k*int(l.Requests)+int(l.Burst)+1
		if n := len(admitted); n < lo-1 || n > hi {
			t.Errorf("limit %+v: %d admitted over %d windows, want [%d, %d]", l, n, k, lo-1, hi)
		}
		checkWindows(t, sc, admitted)
	}
}

func FuzzGCRAModel(f *testing.F) {
	// 05 test 23: random arrivals, limits and bursts on a deterministic
	// clock; memory equals the reference model and respects the window
	// bound.
	for _, s := range []uint64{0, 1, 42, 1 << 40} {
		f.Add(s, uint16(64))
	}
	f.Fuzz(func(t *testing.T, seed uint64, n uint16) {
		sc := newScenario(newRand(seed, seed^0x9e3779b97f4a7c15), int(n%512))
		checkWindows(t, sc, runScenario(t, sc))
	})
}

func TestQuotaModel(t *testing.T) {
	// Spec 08 reqs 42, 43 and 46: random reservations, refunds and skews
	// on split Node and server clocks equal the model.
	r := newRand(3, 4)
	server := clocktest.New(start())
	node := &skewClock{base: server}
	d, err := New(context.Background(), statestore.Config{}, statestore.Deps{Clock: node}, Options{ServerClock: server, SweepInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer closeDriver(t, d)
	model := statestoretest.NewModel()
	windows := []time.Duration{time.Second, time.Minute, time.Hour, 24 * time.Hour}
	var charged []statestore.Refund
	for i := range 5000 {
		switch r.IntN(10) {
		case 0:
			server.Advance(time.Duration(r.Int64N(int64(2 * time.Minute))))
		case 1:
			node.off.Store(int64(time.Duration(r.Int64N(int64(time.Minute))) - 30*time.Second))
		case 2:
			if len(charged) == 0 {
				continue
			}
			ref := charged[r.IntN(len(charged))]
			w := &statestore.Write{Kind: statestore.OpRefund, Timeout: time.Second, Refund: ref}
			d.Write(context.Background(), []*statestore.Write{w})
			// The model keeps expired counters; only compare live windows.
			if server.Now().Before(ref.WindowStart.Add(2 * ref.Window)) {
				if want := model.Refund(ref); w.Applied != want {
					t.Fatalf("op %d: refund applied %v, model %v", i, w.Applied, want)
				}
			}
		default:
			w := windows[r.IntN(len(windows))]
			q := quotaCall("q", w, r.Int64N(5), statestore.DigestOf(strconv.Itoa(r.IntN(3))))
			d.Consume(context.Background(), nil, []*statestore.Call{q})
			want := statestore.Quota{Name: "q", Window: w, Limit: q.Quota.Limit, Digest: q.Quota.Digest}
			ok, skew := model.Quota(node.Now(), server.Now(), &want)
			if skew {
				if q.Err != statestore.ErrFailed {
					t.Fatalf("op %d: skew not reported: %v", i, q.Err)
				}
				continue
			}
			if !ok || q.Err != nil {
				t.Fatalf("op %d: Err %v", i, q.Err)
			}
			// A counter that expired in memory restarts; so does the model's
			// view of a window it has not seen since.
			got := q.Quota
			if got.Allowed != want.Allowed || got.Used != want.Used || got.RetryAfter != want.RetryAfter ||
				!got.WindowStart.Equal(want.WindowStart) || !got.ServerNow.Equal(want.ServerNow) {
				t.Fatalf("op %d: memory %+v, model %+v", i, got, want)
			}
			if got.Allowed {
				charged = append(charged, statestore.Refund{Name: "q", Window: w, WindowStart: got.WindowStart, Digest: got.Digest})
			}
		}
	}
}

func FuzzQuotaNoOverAdmission(f *testing.F) {
	// 05 test 25: concurrent reservations on the memory driver never
	// exceed the limit per window.
	f.Add(uint8(10), uint8(8), uint8(5))
	f.Add(uint8(0), uint8(4), uint8(3))
	f.Add(uint8(100), uint8(16), uint8(10))
	f.Fuzz(func(t *testing.T, limit, workers, each uint8) {
		clk := clocktest.New(start())
		d, err := New(context.Background(), statestore.Config{}, statestore.Deps{Clock: clk}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer closeDriver(t, d)
		workers = max(workers%32, 1)
		dg := statestore.DigestOf("consumer")
		var allowed atomic.Int64
		var mu sync.Mutex
		used := map[int64]bool{}
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				for range each {
					q := quotaCall("q", time.Hour, int64(limit), dg)
					d.Consume(context.Background(), nil, []*statestore.Call{q})
					if q.Err != nil {
						t.Errorf("Err %v", q.Err)
						return
					}
					if q.Quota.Allowed {
						allowed.Add(1)
						mu.Lock()
						if used[q.Quota.Used] {
							t.Errorf("count %d handed out twice", q.Quota.Used)
						}
						used[q.Quota.Used] = true
						mu.Unlock()
					}
				}
			})
		}
		wg.Wait()
		total := int64(workers) * int64(each)
		if got, want := allowed.Load(), min(int64(limit), total); got != want {
			t.Fatalf("%d admitted of %d requests with limit %d, want %d", got, total, limit, want)
		}
	})
}

func TestConcurrentMixedOps(t *testing.T) {
	// Every operation is safe for concurrent use; byte accounting and the
	// per-shard bound hold afterwards.
	clk := clocktest.New(start())
	d, err := New(context.Background(), statestore.Config{}, statestore.Deps{Clock: clk},
		Options{Shards: 4, MaxEntries: 400, MaxCacheBytes: 4096, SweepInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := range uint64(8) {
		wg.Go(func() {
			r := newRand(g, 7)
			for range 500 {
				dg := statestore.DigestOf(strconv.Itoa(r.IntN(40)))
				ck := statestore.CacheKey{URI: dg, Partition: statestore.DigestOf(strconv.Itoa(r.IntN(3)))}
				v := statestore.DigestOf(strconv.Itoa(r.IntN(10)))
				switch r.IntN(8) {
				case 0:
					d.Consume(context.Background(), nil, []*statestore.Call{
						gcraCall("p", dg, statestore.GCRALimit{Requests: 5, Window: time.Second, Burst: 5}),
						quotaCall("q", time.Minute, 20, dg),
					})
				case 1:
					d.Read(context.Background(), nil, []*statestore.Call{lookupCall(ck, v)})
				case 2:
					d.Write(context.Background(), []*statestore.Write{{Kind: statestore.OpCacheSet, Timeout: time.Second, Cache: statestore.CacheStore{
						Key: ck, Names: strconv.Itoa(r.IntN(2)), Variant: v, TTL: time.Minute, Entry: make([]byte, r.IntN(300)), Token: r.Uint64(),
					}}})
				case 3:
					modes := []statestore.LeaseMode{statestore.LeaseTake, statestore.LeaseRefresh, statestore.LeaseEndStale}
					mode := modes[r.IntN(len(modes))]
					d.Write(context.Background(), []*statestore.Write{{Kind: statestore.OpCacheSet, Timeout: time.Second, Lease: statestore.CacheLease{
						Key: ck, Variant: v, Mode: mode, Token: r.Uint64N(2), Entry: make([]byte, r.IntN(300)), TTL: time.Minute,
					}}})
				case 4:
					d.Write(context.Background(), []*statestore.Write{{Kind: statestore.OpCacheInvalidate, Timeout: time.Second, Invalidate: dg}})
				case 5:
					d.AdmitStoreBytes(ck, r.IntN(1000))
				case 6:
					clk.Advance(time.Duration(r.Int64N(int64(500 * time.Millisecond))))
				default:
					d.Write(context.Background(), []*statestore.Write{{Kind: statestore.OpRefund, Timeout: time.Second, Refund: statestore.Refund{
						Name: "q", Window: time.Minute, WindowStart: clk.Now().Truncate(time.Minute), Digest: dg,
					}}})
				}
			}
		})
	}
	wg.Wait()
	if err := d.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkAccounting(t, d)
}
