// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Unit tests for spec 08 section 2.9 (reqs 58 to 60), the memory side of
// section 2.5 (reqs 41 to 46), req 53 (store admission), req 68 (MAC),
// req 64 (metrics labels) and the lifecycle of section 2.10.

func TestOpen(t *testing.T) {
	// Open is the Opener the Manager receives (resolution R-54).
	var opener statestore.Opener = Open
	ctx := context.Background()
	d, err := opener(ctx, statestore.Config{Driver: statestore.DriverMemory, URL: "redis://ignored:6379", Topology: statestore.TopologyCluster}, statestore.Deps{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Spec 08 req 58: memory reports noeviction and no degraded concern.
	if st := d.Status(); st != (statestore.Status{EvictionPolicy: statestore.Yes}) {
		t.Fatalf("Status = %+v", st)
	}
	if d.RoundTrips() {
		t.Fatal("memory reports round trips; its time must stay gateway-added (req 59)")
	}
	if !d.Supports(statestore.CapScripts) || d.Supports(statestore.CapVectorSets) || d.Supports(statestore.CapValkeySearch) {
		t.Fatal("memory supports scripts only")
	}
	if err := d.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if _, err := Open(ctx, statestore.Config{Driver: statestore.DriverRedis}, statestore.Deps{}); !errors.Is(err, ErrWrongDriver) {
		t.Fatalf("Open(redis config) = %v, want ErrWrongDriver", err)
	}
	if _, err := Open(ctx, statestore.Config{}, statestore.Deps{MACKey: []byte("short")}); !errors.Is(err, keys.ErrMACKeyTooShort) {
		t.Fatalf("Open(short MAC key) = %v, want ErrMACKeyTooShort", err)
	}
}

func TestCloseStopsDriver(t *testing.T) {
	// Spec 08 req 63: the sweeper has an owner that cancels and waits.
	clk := clocktest.New(start())
	d, err := New(context.Background(), statestore.Config{}, statestore.Deps{Clock: clk}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.done:
	default:
		t.Fatal("sweeper still running after Close")
	}
	c := gcraCall("p", statestore.DigestOf("k"), statestore.GCRALimit{Requests: 1, Window: time.Second, Burst: 1})
	d.Consume(context.Background(), budget(0), []*statestore.Call{c})
	if c.Err != statestore.ErrFailed {
		t.Fatalf("call after Close: %v, want RZ-STS-002", c.Err)
	}
	l := lookupCall(statestore.CacheKey{}, statestore.Digest{})
	d.Read(context.Background(), budget(0), []*statestore.Call{l})
	if l.Err != statestore.ErrFailed {
		t.Fatalf("lookup after Close: %v", l.Err)
	}
	w := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: time.Second}
	d.Write(context.Background(), []*statestore.Write{w})
	if w.Err != statestore.ErrFailed {
		t.Fatalf("write after Close: %v", w.Err)
	}
	// A Close whose context ends first reports it.
	d2 := &Driver{stop: func() {}, done: make(chan struct{})} // a sweeper that never exits
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d2.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close with an ended context = %v", err)
	}
}

func TestSweeperReclaims(t *testing.T) {
	// Spec 08 req 58: expired keys are reclaimed by the sweeper within
	// 10 s of expiry, and on access.
	f := newFixture(t, Options{}, nil)
	d := statestore.DigestOf("k")
	f.consume(gcraCall("p", d, statestore.GCRALimit{Requests: 10, Window: time.Second, Burst: 10}))
	f.consume(quotaCall("q", time.Second, 5, d))
	ck := statestore.CacheKey{URI: d, Partition: d}
	f.store(ck, "n", d, []byte("entry"), 1)
	f.write(&statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: time.Second, Invalidate: d})
	if n := f.keys(); n != 5 {
		t.Fatalf("%d keys, want 5", n)
	}
	// Everything but the generation (26 h) and partition (1 h) expires
	// within 3 s; the sweeper runs every 5 s.
	f.clk.Advance(5 * time.Second)
	f.eventually(t, func() bool { return f.keys() == 2 })
	f.clk.Advance(2 * time.Hour)
	f.eventually(t, func() bool { return f.keys() == 1 && f.d.bytes.Load() == 0 })
	f.clk.Advance(26 * time.Hour)
	f.eventually(t, func() bool { return f.keys() == 0 })
}

// keys counts the keys held, expired ones included.
func (f *fixture) keys() int {
	n := 0
	for i := range f.d.shards {
		s := &f.d.shards[i]
		s.mu.Lock()
		n += s.keys()
		s.mu.Unlock()
	}
	return n
}

// eventually waits for cond, which the sweeper goroutine makes true after
// the fake clock fires its timer; each poll advances one sweep interval,
// so a sweep that ran before the last Advance finished is retried.
func (f *fixture) eventually(t *testing.T, cond func() bool) {
	t.Helper()
	for range 2000 {
		if cond() {
			return
		}
		f.clk.Advance(DefaultSweepInterval)
		if err := clock.Real().Sleep(context.Background(), time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("condition not reached")
}

func TestEntryBound(t *testing.T) {
	// Spec 08 req 58: a shard full of unexpired keys refuses a new key
	// with RZ-STS-002, like noeviction; existing keys keep working and
	// expired ones are reclaimed to make room.
	f := newFixture(t, Options{Shards: 1, MaxEntries: 3}, nil)
	l := statestore.GCRALimit{Requests: 10, Window: time.Second, Burst: 10}
	for i := range 3 {
		c := gcraCall("p", statestore.DigestOf(string(rune('a'+i))), l)
		f.consume(c)
		if c.Err != nil {
			t.Fatalf("key %d: %v", i, c.Err)
		}
	}
	full := []*statestore.Call{
		gcraCall("p", statestore.DigestOf("x"), l),
		quotaCall("q", time.Hour, 5, statestore.DigestOf("x")),
	}
	for _, c := range full {
		f.consume(c)
		if c.Err != statestore.ErrFailed {
			t.Fatalf("%v on a full shard: Err %v, want RZ-STS-002", c.Kind, c.Err)
		}
	}
	if f.m.calls[emit.StateOpGCRA][emit.StateResultError].total() != 1 {
		t.Fatal("refusal not counted as an error result")
	}
	// A denial writes nothing, so it needs no room: with key "a"
	// exhausted, a call adding a fresh second limit is denied, not refused.
	for range 11 {
		f.consume(gcraCall("p", statestore.DigestOf("a"), l))
	}
	mixed := gcraCall("p", statestore.DigestOf("a"), l, statestore.GCRALimit{Requests: 7, Window: time.Second, Burst: 7})
	f.consume(mixed)
	if mixed.Err != nil || mixed.GCRA.Allowed {
		t.Fatalf("denial on a full shard: Err %v allowed %v", mixed.Err, mixed.GCRA.Allowed)
	}
	existing := gcraCall("p", statestore.DigestOf("b"), l)
	f.consume(existing)
	if existing.Err != nil || !existing.GCRA.Allowed {
		t.Fatalf("existing key on a full shard: %v", existing.Err)
	}
	// Cache writes refuse too.
	ck := statestore.CacheKey{URI: statestore.DigestOf("u")}
	if w := f.store(ck, "n", ck.URI, []byte("e"), 1); w.Err != statestore.ErrFailed || w.Applied {
		t.Fatalf("store on a full shard: %+v", *w)
	}
	inv := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: time.Second, Invalidate: ck.URI}
	take := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Lease: statestore.CacheLease{Key: ck, Mode: statestore.LeaseTake, Token: 1}}
	f.write(inv, take)
	if inv.Err != statestore.ErrFailed || take.Err != statestore.ErrFailed {
		t.Fatalf("invalidate %v, lease %v on a full shard", inv.Err, take.Err)
	}
	// Once the keys expire, the room is reclaimed on demand.
	f.clk.Advance(3 * time.Second)
	again := gcraCall("p", statestore.DigestOf("x"), l)
	f.consume(again)
	if again.Err != nil {
		t.Fatalf("after expiry: %v", again.Err)
	}
}

func TestFullShardScansOnlyAfterAnExpiry(t *testing.T) {
	// Spec 08 req 58: a shard full of live keys refuses a new key without
	// scanning its maps; the on-demand reclaim runs only once the server
	// time passed the shard's lower bound of key expiries, which the scan
	// then recomputes, so a flood of new keys scans at most once per
	// server millisecond. A key planted as expired behind the bound
	// reveals whether a scan ran: only a scan deletes it.
	f := newFixture(t, Options{Shards: 1, MaxEntries: 3, SweepInterval: time.Hour}, nil)
	l := statestore.GCRALimit{Requests: 10, Window: time.Second, Burst: 10} // TAT keys live 1,100 ms
	s := &f.d.shards[0]
	t0 := instantOf(f.clk.Now()).ms
	key := func(name string) rlKey { return rlKey{"p", l.Requests, l.Window, statestore.DigestOf(name)} }
	allow := func(name string) *statestore.Call {
		t.Helper()
		c := gcraCall("p", statestore.DigestOf(name), l)
		f.consume(c)
		return c
	}
	state := func() (next int64, keys int) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.next - t0, s.keys()
	}
	// plant expires key "a" behind the bound and returns whether it
	// survived, restoring it.
	plant := func() func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		e := s.rl[key("a")]
		old := e.exp
		e.exp = instantOf(f.clk.Now()).ms - 1
		s.rl[key("a")] = e
		return func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			e, ok := s.rl[key("a")]
			if ok {
				e.exp = old
				s.rl[key("a")] = e
			}
			return ok
		}
	}
	for _, n := range []string{"a", "b", "c"} {
		if c := allow(n); c.Err != nil {
			t.Fatalf("key %s: %v", n, c.Err)
		}
	}
	if next, _ := state(); next != 1100 {
		t.Fatalf("next %d ms after the start, want 1100", next)
	}
	// Extending the keys to t0 + 1,600 leaves the bound low (stale, safe).
	f.clk.Advance(500 * time.Millisecond)
	for _, n := range []string{"a", "b", "c"} {
		allow(n)
	}
	steps := []struct {
		name     string
		advance  time.Duration
		plant    bool // plant an expired key; it must survive (no scan)
		admitted bool
		next     int64 // ms after t0
		keys     int
	}{
		{"bound in the future: no scan", 0, true, false, 1100, 3},
		{"bound passed: one scan frees nothing and recomputes the bound", 601 * time.Millisecond, false, false, 1600, 3},
		{"same millisecond: no second scan", 0, true, false, 1600, 3},
		{"keys expired: the scan reclaims them", 500 * time.Millisecond, false, true, 1601 + 1100, 1},
	}
	for _, st := range steps {
		f.clk.Advance(st.advance)
		var survived func() bool
		if st.plant {
			survived = plant()
		}
		c := allow("new")
		if got := c.Err == nil; got != st.admitted {
			t.Fatalf("%s: new key admitted %v (Err %v), want %v", st.name, got, c.Err, st.admitted)
		}
		if survived != nil && !survived() {
			t.Fatalf("%s: room scanned the shard", st.name)
		}
		if next, keys := state(); next != st.next || keys != st.keys {
			t.Fatalf("%s: next %d keys %d, want %d and %d", st.name, next, keys, st.next, st.keys)
		}
	}
}

func TestDroppedVariantReleased(t *testing.T) {
	// Spec 08 req 58 (SG-7): a variant dropped by end-stale, a re-store
	// or the eighth-variant bound leaves no reference in the partition's
	// backing array, so memory the byte bound no longer counts is freed.
	f := newFixture(t, Options{SweepInterval: time.Hour}, nil)
	ck := statestore.CacheKey{URI: statestore.DigestOf("u")}
	v := statestore.DigestOf("v")
	big := bytes.Repeat([]byte{1}, 100_000)
	tail := func(what string) {
		t.Helper()
		s := f.d.shardOf(int(keys.PartitionSlot(ck)))
		s.mu.Lock()
		defer s.mu.Unlock()
		p := s.part[ck]
		if p == nil {
			t.Fatalf("%s: partition gone", what)
		}
		for i, x := range p.variants[len(p.variants):cap(p.variants)] {
			if x.entry != nil {
				t.Fatalf("%s: slot len+%d still holds a %d-byte entry", what, i, len(x.entry))
			}
		}
	}
	if w := f.store(ck, "n", v, big, 1); !w.Applied {
		t.Fatal("store not applied")
	}
	f.clk.Advance(2 * time.Second)
	take := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Lease: statestore.CacheLease{Key: ck, Variant: v, Mode: statestore.LeaseTake, Token: 2}}
	end := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Lease: statestore.CacheLease{Key: ck, Variant: v, Mode: statestore.LeaseEndStale, Token: 2}}
	f.write(take, end)
	if !take.Applied || !end.Applied || f.d.bytes.Load() != 0 {
		t.Fatalf("take %v end %v bytes %d", take.Applied, end.Applied, f.d.bytes.Load())
	}
	tail("end-stale of the only variant")
	var token uint64 = 2
	for i := range maxVariants + 1 {
		f.clk.Advance(6 * time.Second)
		token++
		f.store(ck, "n", statestore.DigestOf(strconv.Itoa(i)), big, token)
	}
	tail("ninth variant evicting the oldest")
	f.clk.Advance(6 * time.Second)
	f.store(ck, "n", statestore.DigestOf("3"), []byte("small"), token+1)
	tail("re-store of a middle variant")
}

func TestCacheByteBound(t *testing.T) {
	// Spec 08 req 58: 64 MiB of cache entry bytes (here 100); a store
	// beyond the bound is refused and writes nothing.
	f := newFixture(t, Options{MaxCacheBytes: 100}, nil)
	a := statestore.CacheKey{URI: statestore.DigestOf("a")}
	b := statestore.CacheKey{URI: statestore.DigestOf("b")}
	v := statestore.DigestOf("v")
	if w := f.store(a, "n", v, bytes.Repeat([]byte{1}, 60), 1); !w.Applied {
		t.Fatalf("first store: %+v", *w)
	}
	w := f.store(b, "n", v, bytes.Repeat([]byte{1}, 60), 1)
	if w.Err != statestore.ErrFailed || w.Applied {
		t.Fatalf("store over the byte bound: Err %v Applied %v", w.Err, w.Applied)
	}
	if f.lookup(b, v).Lookup.Found {
		t.Fatal("refused store wrote its entry")
	}
	if l, _ := inspectLease(f.d, b); l {
		t.Fatal("refused store took the fill lease")
	}
	// Replacing a variant counts only the difference.
	f.clk.Advance(2 * time.Second)
	if w := f.store(a, "n", v, bytes.Repeat([]byte{2}, 90), 2); !w.Applied || w.Err != nil {
		t.Fatalf("replacing store: %+v", *w)
	}
	if got := f.d.bytes.Load(); got != 90 {
		t.Fatalf("bytes %d, want 90", got)
	}
	// A refresh that grows past the bound is refused.
	f.clk.Advance(2 * time.Second)
	take := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Lease: statestore.CacheLease{Key: a, Variant: v, Mode: statestore.LeaseTake, Token: 5}}
	grow := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Lease: statestore.CacheLease{Key: a, Variant: v, Mode: statestore.LeaseRefresh, Token: 5, Entry: bytes.Repeat([]byte{3}, 120)}}
	f.write(take, grow)
	if !take.Applied || grow.Err != statestore.ErrFailed {
		t.Fatalf("take %+v, grow %+v", *take, *grow)
	}
	// A names change frees the old partition's bytes first.
	f.clk.Advance(6 * time.Second)
	if w := f.store(a, "other", v, bytes.Repeat([]byte{4}, 95), 6); !w.Applied {
		t.Fatalf("names change store: %+v", *w)
	}
}

func inspectLease(d *Driver, k statestore.CacheKey) (bool, uint64) {
	s := d.shardOf(int(keys.PartitionSlot(k)))
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.lease[k]
	return ok && !expired(l.exp, instantOf(d.server.Now()).ms), l.token
}

func TestMetrics(t *testing.T) {
	// Spec 08 reqs 59 and 64 (R-56): calls and ops count on the request's
	// stripe by OpKind.Label and RoundTrip.Label; memory records no
	// duration.
	f := newFixture(t, Options{}, nil)
	ctx := context.Background()
	d := statestore.DigestOf("k")
	l := statestore.GCRALimit{Requests: 10, Window: time.Second, Burst: 10}

	f.d.Consume(ctx, budget(3), []*statestore.Call{gcraCall("p", d, l)})
	f.d.Consume(ctx, budget(3), []*statestore.Call{gcraCall("p", d, l), quotaCall("q", time.Hour, 5, d)})
	f.d.Read(ctx, budget(3), []*statestore.Call{lookupCall(statestore.CacheKey{}, d), lookupCall(statestore.CacheKey{URI: d}, d)})
	late := quotaCall("q", time.Hour, 5, d)
	late.Timeout = 0
	f.d.Consume(ctx, budget(3), []*statestore.Call{late})
	bad := gcraCall("p", d, statestore.GCRALimit{})
	f.d.Consume(ctx, budget(3), []*statestore.Call{bad})
	f.write(
		&statestore.Write{Kind: statestore.OpRefund, Timeout: time.Second},
		&statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: time.Second, Invalidate: d},
	)
	f.write(&statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: time.Second, Invalidate: d})

	calls := func(op, r int) uint64 { return f.m.calls[op][r].total() }
	stripe3 := func(op, r int) uint64 { return f.m.calls[op][r].n[3].Load() }
	cases := []struct {
		name string
		got  uint64
		want uint64
	}{
		{"gcra ok on stripe 3", stripe3(emit.StateOpGCRA, emit.StateResultOK), 1},
		{"script_multi ok", calls(emit.StateOpScriptMulti, emit.StateResultOK), 1},
		{"pipeline ok on stripe 3", stripe3(emit.StateOpPipeline, emit.StateResultOK), 1},
		{"quota skipped", calls(emit.StateOpQuota, emit.StateResultSkipped), 1},
		{"gcra error", calls(emit.StateOpGCRA, emit.StateResultError), 1},
		{"write batch as pipeline", calls(emit.StateOpPipeline, emit.StateResultOK), 2},
		{"single write by kind", calls(emit.StateOpCacheInvalidate, emit.StateResultOK), 1},
		{"gcra ops", f.m.ops[emit.StateOpGCRA].total(), 3},
		{"quota ops", f.m.ops[emit.StateOpQuota].total(), 1},
		{"cache_get ops (2 per lookup)", f.m.ops[emit.StateOpCacheGet].total(), 4},
		{"refund ops", f.m.ops[emit.StateOpRefund].total(), 1},
		{"cache_invalidate ops", f.m.ops[emit.StateOpCacheInvalidate].total(), 2},
		{"durations", f.m.durations(), 0},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	// Nil metrics and nil handles are tolerated.
	d2, err := New(ctx, statestore.Config{}, statestore.Deps{Clock: f.clk, Metrics: &emit.StateMetrics{}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeDriver(t, d2)
	c := gcraCall("p", d, l)
	d2.Consume(ctx, nil, []*statestore.Call{c})
	if c.Err != nil || !c.Done {
		t.Fatalf("nil budget and handles: %v", c.Err)
	}
}

func TestPrecheck(t *testing.T) {
	// Spec 08 reqs 28, 33 and 60: memory never reports breaker, in-flight
	// or timeout failures, but a spent per-request deadline, a zero
	// timeout or an ended context is not attempted (RZ-STS-004).
	f := newFixture(t, Options{}, nil)
	d := statestore.DigestOf("k")
	cases := []struct {
		name string
		ctx  func(context.Context) (context.Context, context.CancelFunc)
		rb   *statestore.RequestBudget
		tmo  time.Duration
		want *statestore.Error
	}{
		{"budget", context.WithCancel, budget(0), time.Millisecond, nil},
		{"no budget", context.WithCancel, nil, time.Millisecond, nil},
		{"zero timeout", context.WithCancel, budget(0), 0, statestore.ErrNotAttempted},
		{"zero timeout without budget", context.WithCancel, nil, 0, statestore.ErrNotAttempted},
		{"zero budget", context.WithCancel, &statestore.RequestBudget{}, time.Second, statestore.ErrNotAttempted},
		{"canceled", func(parent context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(parent)
			cancel()
			return ctx, cancel
		}, budget(0), time.Second, statestore.ErrNotAttempted},
		{"deadline passed without budget", func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithDeadline(parent, start().Add(-time.Second))
		}, nil, time.Second, statestore.ErrNotAttempted},
	}
	for _, c := range cases {
		ctx, cancel := c.ctx(context.Background())
		q := quotaCall("q", time.Hour, 5, d)
		q.Timeout = c.tmo
		f.d.Consume(ctx, c.rb, []*statestore.Call{q})
		cancel()
		if q.Err != c.want {
			t.Errorf("%s: Err %v, want %v", c.name, q.Err, c.want)
		}
		if c.want != nil && (q.Done || q.Elapsed != 0) {
			t.Errorf("%s: not-attempted call Done %v Elapsed %v", c.name, q.Done, q.Elapsed)
		}
	}
	// The spent per-request deadline: 1 s from the first call.
	rb := budget(0)
	f.d.Consume(context.Background(), rb, []*statestore.Call{quotaCall("q", time.Hour, 5, d)})
	f.clk.Advance(time.Second)
	late := quotaCall("q", time.Hour, 5, d)
	f.d.Consume(context.Background(), rb, []*statestore.Call{late})
	if late.Err != statestore.ErrNotAttempted {
		t.Fatalf("spent budget: %v", late.Err)
	}
	// A memory call never has round-trip time.
	c := gcraCall("p", d, statestore.GCRALimit{Requests: 1, Window: time.Second})
	f.consume(c)
	if c.Elapsed != 0 {
		t.Fatalf("Elapsed %v on memory", c.Elapsed)
	}
}

func TestWrongKinds(t *testing.T) {
	// A call a script does not accept is an error reply (RZ-STS-002).
	f := newFixture(t, Options{}, nil)
	ctx := context.Background()
	if n := f.d.Consume(ctx, budget(0), nil); n != 0 {
		t.Fatalf("Consume(nil) = %d", n)
	}
	f.d.Read(ctx, budget(0), nil)
	f.d.Write(ctx, nil)

	get := lookupCall(statestore.CacheKey{}, statestore.Digest{})
	if n := f.d.Consume(ctx, budget(0), []*statestore.Call{get, get}); n != 1 || get.Err != statestore.ErrFailed {
		t.Fatalf("lookup in Consume: n %d Err %v", n, get.Err)
	}
	g := gcraCall("p", statestore.DigestOf("k"), statestore.GCRALimit{Requests: 1, Window: time.Second})
	ok := lookupCall(statestore.CacheKey{}, statestore.Digest{})
	f.d.Read(ctx, budget(0), []*statestore.Call{g, ok})
	if g.Err != statestore.ErrFailed || ok.Err != nil || !ok.Done {
		t.Fatalf("GCRA in Read: %v; lookup %v", g.Err, ok.Err)
	}
	short := gcraCall("p", statestore.DigestOf("k"), statestore.GCRALimit{Requests: 1, Window: time.Second})
	short.GCRA.Out = nil
	f.consume(short)
	if short.Err != statestore.ErrFailed {
		t.Fatalf("GCRA without Out: %v", short.Err)
	}
	for _, l := range []statestore.GCRALimit{
		{Requests: 0, Window: time.Second},
		{Requests: 1, Window: 0},
		{Requests: 1, Window: time.Nanosecond},
		{Requests: 1, Window: time.Second, Burst: -1},
	} {
		c := gcraCall("p", statestore.DigestOf("k"), l)
		f.consume(c)
		if c.Err != statestore.ErrFailed {
			t.Errorf("limit %+v: %v, want RZ-STS-002", l, c.Err)
		}
	}
	q := quotaCall("q", time.Microsecond, 5, statestore.DigestOf("k"))
	f.consume(q)
	if q.Err != statestore.ErrFailed {
		t.Fatalf("sub-millisecond quota window: %v", q.Err)
	}
	unknown := &statestore.Write{Kind: statestore.OpGCRA, Timeout: time.Second}
	badMode := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Lease: statestore.CacheLease{Mode: 9}}
	f.write(unknown, badMode)
	if unknown.Err != statestore.ErrFailed || badMode.Err != statestore.ErrFailed {
		t.Fatalf("unknown write %v, lease mode %v", unknown.Err, badMode.Err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	w := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: time.Second}
	f.d.Write(canceled, []*statestore.Write{w})
	if w.Err != statestore.ErrNotAttempted {
		t.Fatalf("write on an ended context: %v", w.Err)
	}
}

func TestReserveWindows(t *testing.T) {
	// Spec 08 req 42 against the model: the Node's candidates A and B,
	// the server's window, skew beyond half a window, TTL ws + 2W.
	const w = time.Minute
	cases := []struct {
		name    string
		pos     time.Duration // server position in its window
		skew    time.Duration // Node clock minus server clock
		wantErr bool
	}{
		{"no skew", 30 * time.Second, 0, false},
		{"node 0.4 ahead late in window", 57 * time.Second, 24 * time.Second, false},
		{"node 0.4 behind early in window", 3 * time.Second, -24 * time.Second, false},
		{"node 0.6 ahead late in window", 57 * time.Second, 36 * time.Second, true},
		{"node 0.6 behind early in window", 3 * time.Second, -36 * time.Second, true},
		{"node 0.6 ahead early in window", 3 * time.Second, 36 * time.Second, false},
		{"node a window ahead", 30 * time.Second, w, true},
		{"node just under half ahead", 59 * time.Second, 29*time.Second + 999*time.Millisecond, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := clocktest.New(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC).Add(c.pos))
			node := &skewClock{base: server}
			node.off.Store(int64(c.skew))
			logs := &logRecorder{}
			d, err := New(context.Background(), statestore.Config{}, statestore.Deps{Clock: node, Logger: newLogger(logs)}, Options{ServerClock: server})
			if err != nil {
				t.Fatal(err)
			}
			defer closeDriver(t, d)
			q := quotaCall("q", w, 5, statestore.DigestOf("k"))
			d.Consume(context.Background(), budget(0), []*statestore.Call{q})
			if c.wantErr {
				if q.Err != statestore.ErrFailed {
					t.Fatalf("Err %v, want RZ-STS-002 for skew", q.Err)
				}
				recs := logs.records()
				if len(recs) != 1 || attr(recs[0], "code") != statestore.ErrFailed.Code() || attr(recs[0], "quota") != "q" {
					t.Fatalf("skew WARN records %v", recs)
				}
				// Rate-limited: a second failure within 10 s logs nothing.
				d.Consume(context.Background(), budget(0), []*statestore.Call{quotaCall("q", w, 5, statestore.DigestOf("k"))})
				if len(logs.records()) != 1 {
					t.Fatal("skew WARN not rate-limited")
				}
				return
			}
			if q.Err != nil || !q.Quota.Allowed {
				t.Fatalf("Err %v allowed %v", q.Err, q.Quota.Allowed)
			}
			ws := server.Now().Truncate(w)
			if !q.Quota.WindowStart.Equal(ws) {
				t.Fatalf("WindowStart %v, want the server window %v", q.Quota.WindowStart, ws)
			}
			st, ok := inspectQuota(d, "q", w, ws, statestore.DigestOf("k"))
			if !ok || st != ws.Add(2*w).UnixMilli() {
				t.Fatalf("expiry %d, want %d (ws + 2W)", st, ws.Add(2*w).UnixMilli())
			}
		})
	}
}

func inspectQuota(d *Driver, name string, w time.Duration, ws time.Time, dg statestore.Digest) (int64, bool) {
	s := d.shardOf(int(keys.DigestSlot(dg)))
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.qt[qtKey{name, w, ws.UnixMilli(), dg}]
	return e.exp, ok
}

func TestMACAtDriver(t *testing.T) {
	// Spec 08 req 68: entries carry a tag under the configured key; an
	// entry read under another key is a miss, logged rate-limited, never
	// an error.
	key := bytes.Repeat([]byte{1}, 32)
	f := newFixture(t, Options{}, key)
	ck := statestore.CacheKey{URI: statestore.DigestOf("u"), Partition: statestore.DigestOf("p")}
	v := statestore.DigestOf("v")
	entry := []byte("response")
	f.store(ck, "n", v, entry, 1)
	c := f.lookup(ck, v)
	if !c.Lookup.Found || !bytes.Equal(c.Lookup.Entry, entry) {
		t.Fatalf("tagged entry: %+v", c.Lookup)
	}
	if got := f.d.bytes.Load(); got != int64(len(entry)+keys.TagSize) {
		t.Fatalf("stored %d bytes, want entry plus tag", got)
	}
	// The same state read under another key (a rotation) misses.
	other, err := keys.NewMAC(bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.d.mac = other
	for range 3 {
		c := f.lookup(ck, v)
		if c.Lookup.Found || c.Err != nil || len(c.Lookup.Entry) != 0 {
			t.Fatalf("entry under a rotated key: %+v, %v", c.Lookup, c.Err)
		}
	}
	if n := len(f.logs.records()); n != 1 {
		t.Fatalf("%d MAC warnings, want 1 (rate-limited)", n)
	}
	f.clk.Advance(11 * time.Second)
	f.lookup(ck, v)
	if n := len(f.logs.records()); n != 2 {
		t.Fatalf("%d MAC warnings after 11 s, want 2", n)
	}
}

func TestLookupReusesEntryBuffer(t *testing.T) {
	// The entry is copied into the Call's storage and stays valid until
	// the Call is reused, even when the store changes.
	f := newFixture(t, Options{}, nil)
	ck := statestore.CacheKey{URI: statestore.DigestOf("u")}
	v := statestore.DigestOf("v")
	f.store(ck, "n", v, []byte("first"), 1)
	c := lookupCall(ck, v)
	c.Lookup.Entry = make([]byte, 0, 64)
	buf := c.Lookup.Entry[:1]
	f.d.Read(context.Background(), budget(0), []*statestore.Call{c})
	if &c.Lookup.Entry[0] != &buf[0] {
		t.Fatal("lookup did not reuse the Call's entry storage")
	}
	f.clk.Advance(2 * time.Second)
	f.store(ck, "n", v, []byte("second"), 2)
	if string(c.Lookup.Entry) != "first" {
		t.Fatalf("entry changed under the caller: %q", c.Lookup.Entry)
	}
}

func TestExpiredOnAccess(t *testing.T) {
	// Expired keys read as absent before the sweeper runs (spec 08 req
	// 50): TATs, counters, generations, partitions and leases.
	f := newFixture(t, Options{SweepInterval: time.Hour}, nil)
	d := statestore.DigestOf("k")
	ck := statestore.CacheKey{URI: d, Partition: d}
	l := statestore.GCRALimit{Requests: 1, Window: time.Second, Burst: 0}
	f.consume(gcraCall("p", d, l))
	f.store(ck, "n", d, []byte("e"), 1)
	f.write(&statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: time.Second, Invalidate: d})
	f.clk.Advance(27 * time.Hour)
	c := gcraCall("p", d, l)
	f.consume(c)
	if !c.GCRA.Allowed || c.GCRA.Out[0].ResetAfter != time.Second {
		t.Fatalf("expired TAT still counted: %+v", c.GCRA)
	}
	lk := f.lookup(ck, d)
	if lk.Lookup.Found || lk.Lookup.Generation != 0 || lk.Lookup.Names != "" {
		t.Fatalf("expired cache keys read: %+v", lk.Lookup)
	}
	if f.d.bytes.Load() != 0 {
		t.Fatal("expired partition's bytes not reclaimed on access")
	}
	// Spec 08 req 45: the revalidation lease is fixed at 5 s; the take
	// ignores CacheLease.TTL like the script, which takes no TTL.
	take := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Lease: statestore.CacheLease{Key: ck, Mode: statestore.LeaseTake, Token: 2, TTL: 2 * time.Second}}
	f.write(take)
	if !take.Applied {
		t.Fatal("expired lease blocked a take")
	}
	if held, tok := inspectLease(f.d, ck); !held || tok != 2 {
		t.Fatal("lease not taken")
	}
	f.clk.Advance(5 * time.Second)
	if held, _ := inspectLease(f.d, ck); !held {
		t.Fatal("lease TTL taken from the call, not fixed at 5 s")
	}
	f.clk.Advance(time.Millisecond)
	if held, _ := inspectLease(f.d, ck); held {
		t.Fatal("revalidation lease outlived 5 s")
	}
}

func TestManyLimits(t *testing.T) {
	// More limits than the stack buffer still decide all or nothing.
	f := newFixture(t, Options{}, nil)
	var ls []statestore.GCRALimit
	for i := range stackLimits + 4 {
		ls = append(ls, statestore.GCRALimit{Requests: int64(100 + i), Window: time.Second, Burst: int64(100 + i)})
	}
	ls = append(ls, statestore.GCRALimit{Requests: 1, Window: time.Second, Burst: 0})
	d := statestore.DigestOf("k")
	first := gcraCall("p", d, ls...)
	f.consume(first)
	if !first.GCRA.Allowed {
		t.Fatal("first call denied")
	}
	second := gcraCall("p", d, ls...)
	f.consume(second)
	if second.GCRA.Allowed || second.GCRA.Denied != len(ls)-1 {
		t.Fatalf("second call: allowed %v denied %d", second.GCRA.Allowed, second.GCRA.Denied)
	}
	for i := range ls[:len(ls)-1] {
		if second.GCRA.Out[i].Remaining != first.GCRA.Out[i].Remaining {
			t.Fatalf("limit %d changed on a denial", i)
		}
	}
}
