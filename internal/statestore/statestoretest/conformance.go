// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package statestoretest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Harness adapts one driver to RunConformance. Open and Advance are
// required; the other hooks enable the cases that need them.
type Harness struct {
	// Open returns a driver on empty state, or on state no other case
	// touches: every case derives its digests from a fresh random nonce,
	// so cases may share one server. The suite closes the driver.
	Open func(t *testing.T, o OpenOptions) statestore.Driver
	// Advance moves the time driver d and its store see by the given
	// duration: a fake clock's Advance, or a real sleep against a live
	// server.
	Advance func(t *testing.T, d statestore.Driver, by time.Duration)
	// Exact is true when time moves only through Advance (a fake clock):
	// the suite then also checks the literal numbers of spec 08 section
	// 6.2. Without it, outcomes are checked against the Model at each
	// reply's ServerNow, and case timings are scaled up tenfold.
	Exact bool
	// Now returns the store time of driver d; required when Exact.
	Now func(d statestore.Driver) time.Time
	// Parallel runs the cases in parallel; set it when Open gives each
	// case its own clock or a real-time store.
	Parallel bool
	// DigestSlot returns the Redis Cluster slot of every key tagged with a
	// digest (keys.DigestSlot); nil skips the cross-slot grouping case.
	DigestSlot func(d statestore.Digest) uint16
	// SkewNode sets the Node clock the driver reads to the store's time
	// plus offset (the offset replaces any earlier one); nil skips the
	// clock-skew cases.
	SkewNode func(t *testing.T, d statestore.Driver, offset time.Duration)
	// Inspect reads one stored key; nil skips the raw value and TTL cases.
	Inspect func(t *testing.T, d statestore.Driver, k Key) (Stored, bool)
	// Tamper rewrites the stored entry of one cache variant, as a
	// compromised State Store could; nil skips the MAC cases.
	Tamper func(t *testing.T, d statestore.Driver, k statestore.CacheKey, v statestore.Digest, edit func(stored []byte) []byte)
}

// OpenOptions configure one driver.
type OpenOptions struct {
	// MACKey is statestore.Deps.MACKey.
	MACKey []byte
}

// KeyKind is the kind of stored key Inspect reads.
type KeyKind uint8

// Key kinds of spec 08 req 48.
const (
	// KeyRateLimit is a GCRA TAT: Value is the TAT, microseconds since the
	// 2026 epoch, as text a float64 parse reads exactly.
	KeyRateLimit KeyKind = iota + 1
	// KeyQuota is a Quota counter: Value is the decimal count.
	KeyQuota
	// KeyCacheGeneration is a generation: Value is the decimal generation.
	KeyCacheGeneration
	// KeyCachePartition is a partition hash: Value is its names field.
	KeyCachePartition
	// KeyCacheLease is a partition lease: Value is the decimal token.
	KeyCacheLease
)

// Key names one stored key in driver-neutral terms.
type Key struct {
	Kind KeyKind
	// Policy, Limit and Digest name a KeyRateLimit.
	Policy string
	Limit  statestore.GCRALimit
	// Name, Window, WindowStart and Digest name a KeyQuota.
	Name        string
	Window      time.Duration
	WindowStart time.Time
	Digest      statestore.Digest
	// Cache names the cache keys (Cache.URI alone for a generation).
	Cache statestore.CacheKey
}

// Stored is one key's value and remaining time to live (PTTL).
type Stored struct {
	Value string
	TTL   time.Duration
}

// callTimeout is every conformance call's Policy timeout, generous for a
// live server.
const callTimeout = 2 * time.Second

// RunConformance runs the driver conformance suite of spec 08 section 6.2
// (and the grouping, deadline and MAC rows of section 6.1) against h.
func RunConformance(t *testing.T, h Harness) {
	t.Helper()
	if h.Open == nil || h.Advance == nil {
		t.Fatal("statestoretest: Harness.Open and Harness.Advance are required")
	}
	if h.Exact && h.Now == nil {
		t.Fatal("statestoretest: an Exact harness needs Now")
	}
	for _, c := range conformanceCases() {
		t.Run(c.name, func(t *testing.T) {
			if h.Parallel {
				t.Parallel()
			}
			if c.skip != nil {
				if why := c.skip(h); why != "" {
					t.Skip(why)
				}
			}
			s := newSuite(t, h, c.opts)
			c.run(s)
		})
	}
}

type conformanceCase struct {
	name string
	opts OpenOptions
	skip func(h Harness) string
	run  func(s *suite)
}

// suite is one case's driver, model and key material.
type suite struct {
	t     *testing.T
	h     Harness
	d     statestore.Driver
	nonce string
	model *Model
	// unit is the case time unit: 1 with a fake clock, 10 against real
	// time, so scheduling noise stays far below every interval.
	unit time.Duration
}

func newSuite(t *testing.T, h Harness, o OpenOptions) *suite {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	d := h.Open(t, o)
	t.Cleanup(func() {
		if err := d.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	unit := time.Duration(1)
	if !h.Exact {
		unit = 10
	}
	return &suite{t: t, h: h, d: d, nonce: hex.EncodeToString(b[:]), model: NewModel(), unit: unit}
}

func (s *suite) digest(name string) statestore.Digest {
	return statestore.DigestOf(s.nonce + "/" + name)
}

func (s *suite) name(base string) string { return base + "-" + s.nonce }

func (s *suite) advance(by time.Duration) { s.h.Advance(s.t, s.d, by) }

func (s *suite) now() time.Time { return s.h.Now(s.d) }

// consume runs calls as one request (a fresh per-request budget).
func (s *suite) consume(calls ...*statestore.Call) int {
	rb := statestore.NewRequestBudget(2*callTimeout, 0)
	return s.d.Consume(context.Background(), &rb, calls)
}

func (s *suite) read(calls ...*statestore.Call) {
	rb := statestore.NewRequestBudget(2*callTimeout, 0)
	s.d.Read(context.Background(), &rb, calls)
}

func (s *suite) write(ws ...*statestore.Write) {
	s.d.Write(context.Background(), ws)
}

func gcraCall(policy string, d statestore.Digest, limits ...statestore.GCRALimit) *statestore.Call {
	return &statestore.Call{Kind: statestore.OpGCRA, Timeout: callTimeout, GCRA: statestore.GCRA{
		Policy: policy, Digest: d, Limits: limits, Out: make([]statestore.GCRAOutcome, len(limits)),
	}}
}

func quotaCall(name string, window time.Duration, limit int64, d statestore.Digest) *statestore.Call {
	return &statestore.Call{Kind: statestore.OpQuota, Timeout: callTimeout, Quota: statestore.Quota{
		Name: name, Window: window, Limit: limit, Digest: d,
	}}
}

func lookupCall(k statestore.CacheKey, v statestore.Digest) *statestore.Call {
	return &statestore.Call{Kind: statestore.OpCacheGet, Timeout: callTimeout, Lookup: statestore.CacheLookup{Key: k, Variant: v}}
}

// answered fails the case unless c was decided without error.
func (s *suite) answered(what string, c *statestore.Call) {
	s.t.Helper()
	if c.Err != nil || !c.Done {
		s.t.Fatalf("%s: Done %v, Err %v; want an answer", what, c.Done, c.Err)
	}
}

// checkGCRA compares a decided GCRA call with the Model at its ServerNow
// (spec 08 req 46).
func (s *suite) checkGCRA(what string, c *statestore.Call) {
	s.t.Helper()
	s.answered(what, c)
	g := &c.GCRA
	if s.h.Exact {
		if want := s.now().Truncate(time.Microsecond); !g.ServerNow.Equal(want) {
			s.t.Fatalf("%s: ServerNow %v, want the store time %v", what, g.ServerNow, want)
		}
	}
	want := statestore.GCRA{Policy: g.Policy, Digest: g.Digest, Limits: g.Limits, Out: make([]statestore.GCRAOutcome, len(g.Limits))}
	if !s.model.GCRA(g.ServerNow, &want) {
		s.t.Fatalf("%s: the model rejects the call", what)
	}
	if g.Allowed != want.Allowed || g.Denied != want.Denied || g.RetryAfter != want.RetryAfter {
		s.t.Fatalf("%s: allowed %v denied %d retry %v; model allowed %v denied %d retry %v",
			what, g.Allowed, g.Denied, g.RetryAfter, want.Allowed, want.Denied, want.RetryAfter)
	}
	for i := range want.Out {
		if g.Out[i] != want.Out[i] {
			s.t.Fatalf("%s: limit %d outcome %+v, model %+v", what, i, g.Out[i], want.Out[i])
		}
	}
}

// gcra runs one GCRA call and checks it against the model.
func (s *suite) gcra(what, policy string, d statestore.Digest, limits ...statestore.GCRALimit) *statestore.GCRA {
	s.t.Helper()
	c := gcraCall(policy, d, limits...)
	if n := s.consume(c); n != 1 {
		s.t.Fatalf("%s: Consume took %d calls, want 1", what, n)
	}
	if c.Trip != statestore.TripSingle || c.Batch != -1 {
		s.t.Fatalf("%s: single call labeled trip %d batch %d", what, c.Trip, c.Batch)
	}
	s.checkGCRA(what, c)
	return &c.GCRA
}

// checkQuota compares a decided Quota call with the Model at its
// ServerNow, the Node clock agreeing with the store.
func (s *suite) checkQuota(what string, c *statestore.Call) {
	s.t.Helper()
	s.answered(what, c)
	q := &c.Quota
	want := statestore.Quota{Name: q.Name, Window: q.Window, Limit: q.Limit, Digest: q.Digest}
	if ok, skew := s.model.Quota(q.ServerNow, q.ServerNow, &want); !ok || skew {
		s.t.Fatalf("%s: the model rejects the call", what)
	}
	if q.Allowed != want.Allowed || q.Used != want.Used || q.RetryAfter != want.RetryAfter ||
		!q.WindowStart.Equal(want.WindowStart) || !q.ServerNow.Equal(want.ServerNow) {
		s.t.Fatalf("%s: got %+v, model %+v", what, *q, want)
	}
}

func (s *suite) quota(what, name string, window time.Duration, limit int64, d statestore.Digest) *statestore.Quota {
	s.t.Helper()
	c := quotaCall(name, window, limit, d)
	if n := s.consume(c); n != 1 {
		s.t.Fatalf("%s: Consume took %d calls, want 1", what, n)
	}
	s.checkQuota(what, c)
	return &c.Quota
}

func (s *suite) lookup(what string, k statestore.CacheKey, v statestore.Digest) *statestore.CacheLookup {
	s.t.Helper()
	c := lookupCall(k, v)
	s.read(c)
	s.answered(what, c)
	if c.Trip != statestore.TripPipeline {
		s.t.Fatalf("%s: lookup trip %d, want pipeline", what, c.Trip)
	}
	return &c.Lookup
}

func (s *suite) store(what string, cs statestore.CacheStore) bool {
	s.t.Helper()
	w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTimeout, Cache: cs}
	s.write(w)
	if w.Err != nil {
		s.t.Fatalf("%s: store failed: %v", what, w.Err)
	}
	return w.Applied
}

func (s *suite) leaseOp(what string, l statestore.CacheLease) bool {
	s.t.Helper()
	w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTimeout, Lease: l}
	s.write(w)
	if w.Err != nil {
		s.t.Fatalf("%s: lease failed: %v", what, w.Err)
	}
	return w.Applied
}

func (s *suite) invalidate(what string, uri statestore.Digest) {
	s.t.Helper()
	w := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: callTimeout, Invalidate: uri}
	s.write(w)
	if w.Err != nil || !w.Applied {
		s.t.Fatalf("%s: invalidation Err %v Applied %v", what, w.Err, w.Applied)
	}
}

func (s *suite) refund(what string, r statestore.Refund) bool {
	s.t.Helper()
	w := &statestore.Write{Kind: statestore.OpRefund, Timeout: callTimeout, Refund: r}
	s.write(w)
	if w.Err != nil {
		s.t.Fatalf("%s: refund failed: %v", what, w.Err)
	}
	return w.Applied
}

// inspect reads a key; it fails the case when the harness has no Inspect.
func (s *suite) inspect(k Key) (Stored, bool) {
	s.t.Helper()
	return s.h.Inspect(s.t, s.d, k)
}

// ttlNear checks a TTL against want: exact with a fake clock, else within
// the time the case may have spent since.
func (s *suite) ttlNear(what string, got, want time.Duration) {
	s.t.Helper()
	slack := time.Duration(0)
	if !s.h.Exact {
		slack = 500 * time.Millisecond
	}
	if got > want || got < want-slack {
		s.t.Fatalf("%s: TTL %v, want %v (slack %v)", what, got, want, slack)
	}
}

func needExact(h Harness) string {
	if !h.Exact {
		return "needs a fake clock"
	}
	return ""
}

func needInspect(h Harness) string {
	if h.Inspect == nil {
		return "harness has no Inspect"
	}
	return ""
}

func conformanceCases() []conformanceCase {
	var cs []conformanceCase
	cs = append(cs, gcraCases()...)
	cs = append(cs, quotaCases()...)
	cs = append(cs, consumeCases()...)
	cs = append(cs, cacheCases()...)
	return cs
}

// gcraCases cover spec 08 req 41 and the GCRA bullet of section 6.2.
func gcraCases() []conformanceCase {
	return []conformanceCase{
		{name: "gcra/burst-from-idle", run: func(s *suite) {
			// requests 10, window 1 s, burst 10 (T = 100 ms, τ = 1 s): 11
			// calls at one instant are admitted, the 12th is denied with
			// RetryAfter = T, and one more is admitted T later.
			w := time.Second * s.unit
			l := statestore.GCRALimit{Requests: 10, Window: w, Burst: 10}
			p, d := s.name("rl"), s.digest("k")
			for i := range 11 {
				if g := s.gcra("call "+strconv.Itoa(i+1), p, d, l); !g.Allowed {
					s.t.Fatalf("call %d denied from idle", i+1)
				}
			}
			g := s.gcra("call 12", p, d, l)
			if g.Allowed || g.Denied != 0 {
				s.t.Fatalf("call 12 allowed %v denied %d; want denied by limit 0", g.Allowed, g.Denied)
			}
			if s.h.Exact && g.RetryAfter != 100*time.Millisecond {
				s.t.Fatalf("RetryAfter %v, want T = 100ms", g.RetryAfter)
			}
			s.advance(g.RetryAfter)
			if g := s.gcra("after RetryAfter", p, d, l); !g.Allowed {
				s.t.Fatal("denied once RetryAfter passed")
			}
		}},
		{name: "gcra/first-window", skip: needExact, run: func(s *suite) {
			// Over the first window exactly requests + burst = 20 are
			// admitted.
			l := statestore.GCRALimit{Requests: 10, Window: time.Second, Burst: 10}
			p, d := s.name("rl"), s.digest("k")
			admitted := 0
			for step := time.Duration(0); step < time.Second; step += 10 * time.Millisecond {
				for {
					if g := s.gcra("window", p, d, l); !g.Allowed {
						break
					}
					admitted++
				}
				s.advance(10 * time.Millisecond)
			}
			if admitted != 20 {
				s.t.Fatalf("%d admitted in the first window, want 20", admitted)
			}
		}},
		{name: "gcra/burst-zero", run: func(s *suite) {
			// burst 0: one admitted at once, then one per T.
			w := time.Second * s.unit
			l := statestore.GCRALimit{Requests: 10, Window: w, Burst: 0}
			p, d := s.name("rl"), s.digest("k")
			if g := s.gcra("first", p, d, l); !g.Allowed || g.Out[0].Remaining != 0 {
				s.t.Fatalf("first call allowed %v remaining %d, want 0 left", g.Allowed, g.Out[0].Remaining)
			}
			g := s.gcra("second", p, d, l)
			if g.Allowed {
				s.t.Fatal("second call at once admitted with burst 0")
			}
			if s.h.Exact && g.RetryAfter != 100*time.Millisecond {
				s.t.Fatalf("RetryAfter %v, want 100ms", g.RetryAfter)
			}
			s.advance(g.RetryAfter)
			if g := s.gcra("third", p, d, l); !g.Allowed {
				s.t.Fatal("not admitted one T later")
			}
		}},
		{name: "gcra/multi-limit", run: func(s *suite) {
			// Limits 2/1s and 3/1m: a deny by one limit leaves every TAT
			// unchanged (all or nothing); remaining and reset per limit.
			l1 := statestore.GCRALimit{Requests: 2, Window: time.Second * s.unit, Burst: 2}
			l2 := statestore.GCRALimit{Requests: 3, Window: time.Minute * s.unit, Burst: 3}
			p, d := s.name("rl"), s.digest("k")
			var g *statestore.GCRA
			for i := range 10 {
				if g = s.gcra("call "+strconv.Itoa(i+1), p, d, l1, l2); !g.Allowed {
					break
				}
			}
			if g.Allowed || g.Denied != 0 {
				s.t.Fatalf("denied %v by limit %d, want limit 0 first", !g.Allowed, g.Denied)
			}
			if g.Out[0].Remaining != 0 || g.Out[1].Remaining == 0 {
				s.t.Fatalf("remaining %d, %d; want 0 for the denying limit only", g.Out[0].Remaining, g.Out[1].Remaining)
			}
			if s.h.Inspect != nil {
				for _, l := range []statestore.GCRALimit{l1, l2} {
					st, ok := s.inspect(Key{Kind: KeyRateLimit, Policy: p, Limit: l, Digest: d})
					want, _ := s.model.TAT(p, l, d)
					got, err := strconv.ParseFloat(st.Value, 64)
					if !ok || err != nil || got != want {
						s.t.Fatalf("TAT of %d/%v is %q (%v), model %v", l.Requests, l.Window, st.Value, err, want)
					}
				}
			}
			// The denial reserved nothing: the second limit still admits
			// once the first recovers.
			s.advance(g.RetryAfter)
			if g := s.gcra("after recovery", p, d, l1, l2); !g.Allowed {
				s.t.Fatal("denied after the denying limit recovered")
			}
		}},
		{name: "gcra/remaining-reset", skip: needExact, run: func(s *suite) {
			l := statestore.GCRALimit{Requests: 10, Window: time.Second, Burst: 10}
			g := s.gcra("first", s.name("rl"), s.digest("k"), l)
			if g.Out[0] != (statestore.GCRAOutcome{Remaining: 10, ResetAfter: 100 * time.Millisecond}) {
				s.t.Fatalf("first outcome %+v, want remaining 10, reset 100ms", g.Out[0])
			}
		}},
		{name: "gcra/ttl", skip: needInspect, run: func(s *suite) {
			// The key's PTTL is TAT - now + τ, rounded up to the millisecond.
			l := statestore.GCRALimit{Requests: 10, Window: time.Second * s.unit, Burst: 10}
			p, d := s.name("rl"), s.digest("k")
			g := s.gcra("first", p, d, l)
			tat, _ := s.model.TAT(p, l, d)
			_, tau, _ := GCRAParams(l)
			want := time.Duration(max(1, int64(math.Ceil((tat-GCRAMicros(g.ServerNow)+tau)/1000)))) * time.Millisecond
			st, ok := s.inspect(Key{Kind: KeyRateLimit, Policy: p, Limit: l, Digest: d})
			if !ok {
				s.t.Fatal("GCRA key missing")
			}
			s.ttlNear("GCRA key", st.TTL, want)
		}},
		{name: "gcra/invalid-limit", run: func(s *suite) {
			// A limit the script rejects is an error reply: RZ-STS-002.
			c := gcraCall(s.name("rl"), s.digest("k"), statestore.GCRALimit{Requests: 0, Window: time.Second})
			s.consume(c)
			if c.Err != statestore.ErrFailed || c.Done {
				s.t.Fatalf("invalid limit: Done %v Err %v, want RZ-STS-002", c.Done, c.Err)
			}
		}},
	}
}

// quotaCases cover spec 08 reqs 42 and 43 and the Quota and Skew bullets
// of section 6.2.
func quotaCases() []conformanceCase {
	return []conformanceCase{
		{name: "quota/limit", run: func(s *suite) {
			// Reserve up to the limit, then deny with RetryAfter to the
			// window end; a raised limit admits at once.
			n, d := s.name("q"), s.digest("k")
			for i := range 3 {
				if q := s.quota("reserve", n, time.Hour, 3, d); !q.Allowed || q.Used != int64(i+1) {
					s.t.Fatalf("reserve %d: allowed %v used %d", i+1, q.Allowed, q.Used)
				}
			}
			q := s.quota("exhausted", n, time.Hour, 3, d)
			if q.Allowed || q.Used != 3 {
				s.t.Fatalf("exhausted: allowed %v used %d", q.Allowed, q.Used)
			}
			if end := q.WindowStart.Add(time.Hour); q.RetryAfter != end.Sub(q.ServerNow) {
				s.t.Fatalf("RetryAfter %v, want %v to the window end", q.RetryAfter, end.Sub(q.ServerNow))
			}
			if q := s.quota("raised", n, time.Hour, 4, d); !q.Allowed || q.Used != 4 {
				s.t.Fatalf("raised limit: allowed %v used %d", q.Allowed, q.Used)
			}
		}},
		{name: "quota/limit-zero", run: func(s *suite) {
			if q := s.quota("zero", s.name("q"), time.Hour, 0, s.digest("k")); q.Allowed || q.Used != 0 {
				s.t.Fatalf("limit 0: allowed %v used %d", q.Allowed, q.Used)
			}
		}},
		{name: "quota/refund", run: func(s *suite) {
			// A refund decrements the charged window only, never below 0.
			n, d := s.name("q"), s.digest("k")
			s.quota("first", n, time.Hour, 10, d)
			q := s.quota("second", n, time.Hour, 10, d)
			ref := statestore.Refund{Name: n, Window: time.Hour, WindowStart: q.WindowStart, Digest: d}
			other := ref
			other.WindowStart = q.WindowStart.Add(-time.Hour)
			if s.refund("other window", other) {
				s.t.Fatal("refund of an uncharged window applied")
			}
			for i, want := range []bool{true, true, false} {
				if got := s.refund("refund", ref); got != want {
					s.t.Fatalf("refund %d applied %v, want %v", i+1, got, want)
				}
				if want {
					s.model.Refund(ref)
				}
			}
			if q := s.quota("after refunds", n, time.Hour, 10, d); q.Used != 1 {
				s.t.Fatalf("used %d after refunding to 0, want 1", q.Used)
			}
		}},
		{name: "quota/refund-after-expiry", skip: needInspect, run: func(s *suite) {
			// A refund after the window's key expired creates no key.
			w := time.Second
			n, d := s.name("q"), s.digest("k")
			q := s.quota("reserve", n, w, 10, d)
			s.advance(q.WindowStart.Add(2*w).Sub(q.ServerNow) + 50*time.Millisecond*s.unit)
			ref := statestore.Refund{Name: n, Window: w, WindowStart: q.WindowStart, Digest: d}
			if s.refund("expired", ref) {
				s.t.Fatal("refund of an expired window applied")
			}
			if _, ok := s.inspect(Key{Kind: KeyQuota, Name: n, Window: w, WindowStart: q.WindowStart, Digest: d}); ok {
				s.t.Fatal("refund recreated an expired key")
			}
		}},
		{name: "quota/utc-midnight", run: func(s *suite) {
			// 24h windows start at midnight UTC; the counter lives until
			// the window start plus two windows.
			n, d := s.name("q"), s.digest("k")
			q := s.quota("daily", n, 24*time.Hour, 10, d)
			if ws := q.WindowStart.UTC(); ws.Hour() != 0 || ws.Minute() != 0 || ws.Second() != 0 || ws.Nanosecond() != 0 {
				s.t.Fatalf("24h window starts at %v, not midnight UTC", ws)
			}
			if q.ServerNow.Sub(q.WindowStart) >= 24*time.Hour || q.ServerNow.Before(q.WindowStart) {
				s.t.Fatalf("window %v does not hold now %v", q.WindowStart, q.ServerNow)
			}
			if s.h.Inspect == nil {
				return
			}
			st, ok := s.inspect(Key{Kind: KeyQuota, Name: n, Window: 24 * time.Hour, WindowStart: q.WindowStart, Digest: d})
			if !ok || st.Value != "1" {
				s.t.Fatalf("counter %q, %v; want 1", st.Value, ok)
			}
			s.ttlNear("quota key", st.TTL, q.WindowStart.Add(48*time.Hour).Sub(q.ServerNow))
		}},
		{name: "quota/skew", skip: func(h Harness) string {
			if h.SkewNode == nil {
				return "harness cannot skew the Node clock"
			}
			return ""
		}, run: func(s *suite) {
			// Node clock 0.6 of a window ahead near the end of the server
			// window: RZ-STS-002; 0.4 ahead: the server's window.
			w := time.Second * s.unit
			n, d := s.name("q"), s.digest("k")
			probe := s.quota("probe", s.name("probe"), w, 1000, s.digest("probe"))
			pos := probe.ServerNow.Sub(probe.WindowStart)
			s.advance((w + w*95/100 - pos) % w)
			s.h.SkewNode(s.t, s.d, w*6/10)
			c := quotaCall(n, w, 10, d)
			s.consume(c)
			if c.Err != statestore.ErrFailed || c.Done {
				s.t.Fatalf("0.6 window skew: Done %v Err %v, want RZ-STS-002", c.Done, c.Err)
			}
			s.h.SkewNode(s.t, s.d, w*4/10)
			q := s.quota("0.4 window skew", n, w, 10, d)
			if !q.Allowed || q.Used != 1 {
				s.t.Fatalf("0.4 window skew: allowed %v used %d", q.Allowed, q.Used)
			}
		}},
	}
}

// consumeCases cover spec 08 reqs 28, 29, 33, 38 and 44 and the Consume
// grouping and deadline rows of section 6.1.
func consumeCases() []conformanceCase {
	gl := func(s *suite) statestore.GCRALimit {
		return statestore.GCRALimit{Requests: 1, Window: time.Hour * s.unit, Burst: 0}
	}
	return []conformanceCase{
		{name: "consume/merged", run: func(s *suite) {
			// [GCRA k, Quota k] share a slot: one script_multi round trip.
			d := s.digest("k")
			g := gcraCall(s.name("rl"), d, gl(s))
			q := quotaCall(s.name("q"), time.Hour, 5, d)
			if n := s.consume(g, q); n != 2 {
				s.t.Fatalf("Consume took %d calls, want 2", n)
			}
			for _, c := range []*statestore.Call{g, q} {
				if c.Trip != statestore.TripScriptMulti || c.Batch != 0 {
					s.t.Fatalf("merged call labeled trip %d batch %d", c.Trip, c.Batch)
				}
			}
			s.checkGCRA("gcra", g)
			s.checkQuota("quota", q)
		}},
		{name: "consume/deny-stops", run: func(s *suite) {
			// A deny in the first call leaves the second !Done and its
			// counter unchanged.
			d := s.digest("k")
			s.gcra("exhaust", s.name("rl"), d, gl(s))
			g := gcraCall(s.name("rl"), d, gl(s))
			q := quotaCall(s.name("q"), time.Hour, 5, d)
			if n := s.consume(g, q); n != 2 {
				s.t.Fatalf("Consume took %d calls, want 2", n)
			}
			s.checkGCRA("gcra", g)
			if g.GCRA.Allowed {
				s.t.Fatal("exhausted limit allowed")
			}
			if q.Done || q.Err != nil {
				s.t.Fatalf("call after a deny: Done %v Err %v", q.Done, q.Err)
			}
			if got := s.quota("quota alone", s.name("q"), time.Hour, 5, d); got.Used != 1 {
				s.t.Fatalf("quota used %d after the skipped call, want 1", got.Used)
			}
		}},
		{name: "consume/other-slot", skip: func(h Harness) string {
			if h.DigestSlot == nil {
				return "harness has no DigestSlot"
			}
			return ""
		}, run: func(s *suite) {
			// [GCRA k1, Quota k2] in different slots: two single trips.
			d1 := s.digest("k1")
			var d2 statestore.Digest
			for i := 0; ; i++ {
				if d2 = s.digest("k2-" + strconv.Itoa(i)); s.h.DigestSlot(d2) != s.h.DigestSlot(d1) {
					break
				}
			}
			g := gcraCall(s.name("rl"), d1, gl(s))
			q := quotaCall(s.name("q"), time.Hour, 5, d2)
			calls := []*statestore.Call{g, q}
			if n := s.consume(calls...); n != 1 {
				s.t.Fatalf("first Consume took %d calls, want 1", n)
			}
			if q.Done {
				s.t.Fatal("call in another slot answered in the first round trip")
			}
			if n := s.consume(calls[1:]...); n != 1 {
				s.t.Fatalf("second Consume took %d calls, want 1", n)
			}
			s.checkGCRA("gcra", g)
			s.checkQuota("quota", q)
		}},
		{name: "consume/failure-applies-to-group", run: func(s *suite) {
			// Every call of a merged round trip gets the same failure.
			d := s.digest("k")
			g := gcraCall(s.name("rl"), d, statestore.GCRALimit{Requests: 0, Window: time.Second})
			q := quotaCall(s.name("q"), time.Hour, 5, d)
			if n := s.consume(g, q); n != 2 {
				s.t.Fatalf("Consume took %d calls, want 2", n)
			}
			for _, c := range []*statestore.Call{g, q} {
				if c.Err != statestore.ErrFailed || c.Done {
					s.t.Fatalf("merged failure: Done %v Err %v", c.Done, c.Err)
				}
			}
		}},
		{name: "consume/spent-budget", run: func(s *suite) {
			// After the per-request deadline, calls are not attempted
			// (RZ-STS-004) and change nothing.
			d := s.digest("k")
			rb := statestore.NewRequestBudget(10*time.Millisecond*s.unit, 0)
			first := quotaCall(s.name("q"), time.Hour, 5, d)
			s.d.Consume(context.Background(), &rb, []*statestore.Call{first})
			s.checkQuota("first", first)
			s.advance(20 * time.Millisecond * s.unit)
			late := quotaCall(s.name("q"), time.Hour, 5, d)
			s.d.Consume(context.Background(), &rb, []*statestore.Call{late})
			if late.Err != statestore.ErrNotAttempted || late.Done {
				s.t.Fatalf("spent budget: Done %v Err %v, want RZ-STS-004", late.Done, late.Err)
			}
			if q := s.quota("fresh request", s.name("q"), time.Hour, 5, d); q.Used != 2 {
				s.t.Fatalf("used %d, want 2: the late call must not charge", q.Used)
			}
		}},
		{name: "consume/zero-timeout", run: func(s *suite) {
			// A resolved timeout of 0 is never attempted (spec 08 req 6).
			c := quotaCall(s.name("q"), time.Hour, 5, s.digest("k"))
			c.Timeout = 0
			s.consume(c)
			if c.Err != statestore.ErrNotAttempted {
				s.t.Fatalf("zero timeout: Err %v, want RZ-STS-004", c.Err)
			}
			w := &statestore.Write{Kind: statestore.OpCacheInvalidate, Invalidate: s.digest("u")}
			s.write(w)
			if w.Err != statestore.ErrNotAttempted || w.Applied {
				s.t.Fatalf("zero-timeout write: Err %v Applied %v", w.Err, w.Applied)
			}
		}},
		{name: "consume/merged-timeout-min", run: func(s *suite) {
			// Spec 08 req 29 and the section 6.1 grouping row: the merged
			// round trip's timeout is the smallest of its calls', so a zero
			// timeout anywhere in the group leaves every call not attempted
			// (RZ-STS-004, req 6) and charges nothing, in either order.
			for i, zeroFirst := range []bool{false, true} {
				d := s.digest("k" + strconv.Itoa(i))
				g := gcraCall(s.name("rl"), d, gl(s))
				q := quotaCall(s.name("q"), time.Hour, 5, d)
				g.Timeout, q.Timeout = 50*time.Millisecond*s.unit, 0
				calls := []*statestore.Call{g, q}
				if zeroFirst {
					g.Timeout, q.Timeout = 0, 50*time.Millisecond*s.unit
					calls = []*statestore.Call{q, g}
				}
				if n := s.consume(calls...); n != 2 {
					s.t.Fatalf("zero first %v: Consume took %d calls, want 2", zeroFirst, n)
				}
				for _, c := range calls {
					if c.Err != statestore.ErrNotAttempted || c.Done {
						s.t.Fatalf("zero first %v: %v call Done %v Err %v, want RZ-STS-004", zeroFirst, c.Kind, c.Done, c.Err)
					}
				}
				if got := s.gcra("gcra after", s.name("rl"), d, gl(s)); !got.Allowed {
					s.t.Fatalf("zero first %v: the unattempted GCRA call charged its limit", zeroFirst)
				}
				if got := s.quota("quota after", s.name("q"), time.Hour, 5, d); got.Used != 1 {
					s.t.Fatalf("zero first %v: quota used %d, want 1: the unattempted call must not charge", zeroFirst, got.Used)
				}
			}
		}},
		{name: "consume/supports-scripts", run: func(s *suite) {
			if !s.d.Supports(statestore.CapScripts) {
				s.t.Fatal("driver lacks the script command set")
			}
		}},
	}
}

// cacheCases cover spec 08 req 45, req 68 and the Cache bullet of section
// 6.2.
func cacheCases() []conformanceCase {
	type fixture struct {
		k    statestore.CacheKey
		v    statestore.Digest
		ttl  time.Duration
		body []byte
	}
	fix := func(s *suite) fixture {
		return fixture{
			k:   statestore.CacheKey{URI: s.digest("u"), Partition: s.digest("p")},
			v:   s.digest("v"),
			ttl: time.Hour, body: []byte("entry-" + s.nonce),
		}
	}
	fillLease := time.Second + 10*time.Millisecond
	macKey := bytes.Repeat([]byte{0x5a}, 32)
	return []conformanceCase{
		{name: "cache/miss", run: func(s *suite) {
			f := fix(s)
			l := s.lookup("miss", f.k, f.v)
			if l.Found || l.Generation != 0 || l.Names != "" || l.EntryGen != 0 {
				s.t.Fatalf("empty partition: %+v", *l)
			}
		}},
		{name: "cache/store-needs-lease", run: func(s *suite) {
			// A store takes the 1 s fill lease; a second store within 1 s
			// is not applied.
			f := fix(s)
			if !s.store("first", statestore.CacheStore{Key: f.k, Names: "accept", Variant: f.v, TTL: f.ttl, Entry: f.body, Token: 1}) {
				s.t.Fatal("first store not applied")
			}
			l := s.lookup("hit", f.k, f.v)
			if !l.Found || !bytes.Equal(l.Entry, f.body) || l.Names != "accept" || l.EntryGen != l.Generation {
				s.t.Fatalf("lookup after store: %+v", *l)
			}
			v2 := s.digest("v2")
			if s.store("second", statestore.CacheStore{Key: f.k, Names: "accept", Variant: v2, TTL: f.ttl, Entry: f.body, Token: 2}) {
				s.t.Fatal("second store within the fill lease applied")
			}
			if s.lookup("second variant", f.k, v2).Found {
				s.t.Fatal("unapplied store wrote its variant")
			}
			s.advance(fillLease)
			if !s.store("after lease", statestore.CacheStore{Key: f.k, Names: "accept", Variant: v2, TTL: f.ttl, Entry: f.body, Token: 3}) {
				s.t.Fatal("store after the fill lease not applied")
			}
		}},
		{name: "cache/names-reset", run: func(s *suite) {
			// A store under other Vary names drops the partition first.
			f := fix(s)
			s.store("names a", statestore.CacheStore{Key: f.k, Names: "a", Variant: f.v, TTL: f.ttl, Entry: f.body, Token: 1})
			s.advance(fillLease)
			v2 := s.digest("v2")
			if !s.store("names b", statestore.CacheStore{Key: f.k, Names: "b", Variant: v2, TTL: f.ttl, Entry: f.body, Token: 2}) {
				s.t.Fatal("store under new names not applied")
			}
			if l := s.lookup("old variant", f.k, f.v); l.Found || l.Names != "b" {
				s.t.Fatalf("old variant survived a names change: %+v", *l)
			}
			if !s.lookup("new variant", f.k, v2).Found {
				s.t.Fatal("new variant missing")
			}
		}},
		{name: "cache/nine-variants", run: func(s *suite) {
			// At most 8 variants: the ninth evicts the oldest.
			f := fix(s)
			vs := make([]statestore.Digest, 9)
			for i := range vs {
				vs[i] = s.digest("v" + strconv.Itoa(i))
				if !s.store("variant", statestore.CacheStore{Key: f.k, Names: "n", Variant: vs[i], TTL: f.ttl, Entry: f.body, Token: uint64(i + 1)}) {
					s.t.Fatalf("store %d not applied", i+1)
				}
				s.advance(fillLease)
			}
			if s.lookup("oldest", f.k, vs[0]).Found {
				s.t.Fatal("ninth variant did not evict the oldest")
			}
			for i := 1; i < 9; i++ {
				if !s.lookup("kept", f.k, vs[i]).Found {
					s.t.Fatalf("variant %d evicted", i+1)
				}
			}
		}},
		{name: "cache/generation", run: func(s *suite) {
			// A hit needs the variant's generation to equal the current
			// one; an invalidation outdates every stored variant.
			f := fix(s)
			l := s.lookup("before", f.k, f.v)
			s.store("store", statestore.CacheStore{Key: f.k, Names: "n", Variant: f.v, Generation: l.Generation, TTL: f.ttl, Entry: f.body, Token: 1})
			if l := s.lookup("hit", f.k, f.v); !l.Found || l.EntryGen != l.Generation {
				s.t.Fatalf("stored variant not current: %+v", *l)
			}
			s.invalidate("bump", f.k.URI)
			l = s.lookup("after bump", f.k, f.v)
			if !l.Found || l.Generation == 0 || l.EntryGen == l.Generation {
				s.t.Fatalf("after invalidation: %+v", *l)
			}
			s.advance(fillLease)
			s.store("restore", statestore.CacheStore{Key: f.k, Names: "n", Variant: f.v, Generation: l.Generation, TTL: f.ttl, Entry: f.body, Token: 2})
			if l2 := s.lookup("current again", f.k, f.v); l2.EntryGen != l2.Generation || l2.Generation != l.Generation {
				s.t.Fatalf("restored variant: %+v", *l2)
			}
		}},
		{name: "cache/generation-monotonic", run: func(s *suite) {
			// Two bumps in one microsecond still increase: never reused.
			f := fix(s)
			s.invalidate("first", f.k.URI)
			g1 := s.lookup("first", f.k, f.v).Generation
			s.invalidate("second", f.k.URI)
			g2 := s.lookup("second", f.k, f.v).Generation
			if g2 <= g1 || (s.h.Exact && g2 != g1+1) {
				s.t.Fatalf("generations %d then %d", g1, g2)
			}
			if s.h.Exact {
				if want := s.now().UnixMicro(); g1 != want {
					s.t.Fatalf("generation %d, want the server time %d µs", g1, want)
				}
			}
		}},
		{name: "cache/revalidation-lease", run: func(s *suite) {
			// The revalidation lease lasts 5 s (SET NX PX 5000); a second
			// taker fails. CacheLease.TTL does not change it: the take
			// script has no TTL argument.
			f := fix(s)
			take := func(token uint64) bool {
				return s.leaseOp("take", statestore.CacheLease{Key: f.k, Variant: f.v, Mode: statestore.LeaseTake, Token: token, TTL: time.Second})
			}
			if !take(1) {
				s.t.Fatal("lease not taken")
			}
			if take(2) {
				s.t.Fatal("held lease taken twice")
			}
			s.advance(4 * time.Second)
			if take(2) {
				s.t.Fatal("lease lapsed before 5 s")
			}
			s.advance(time.Second + 10*time.Millisecond)
			if !take(2) {
				s.t.Fatal("lease not free after 5 s")
			}
		}},
		{name: "cache/refresh-and-end-stale", run: func(s *suite) {
			// Refresh (304) and end-stale act only with the lease token.
			f := fix(s)
			s.store("store", statestore.CacheStore{Key: f.k, Names: "n", Variant: f.v, TTL: f.ttl, Entry: f.body, Token: 1})
			s.advance(fillLease)
			if !s.leaseOp("take", statestore.CacheLease{Key: f.k, Variant: f.v, Mode: statestore.LeaseTake, Token: 7}) {
				s.t.Fatal("revalidation lease not taken")
			}
			fresh := []byte("refreshed-" + s.nonce)
			refresh := statestore.CacheLease{Key: f.k, Variant: f.v, Mode: statestore.LeaseRefresh, Token: 8, Entry: fresh, TTL: f.ttl}
			if s.leaseOp("refresh, wrong token", refresh) {
				s.t.Fatal("refresh without the lease token applied")
			}
			if l := s.lookup("unrefreshed", f.k, f.v); !bytes.Equal(l.Entry, f.body) {
				s.t.Fatal("wrong-token refresh changed the entry")
			}
			refresh.Token = 7
			if !s.leaseOp("refresh", refresh) {
				s.t.Fatal("refresh with the lease token not applied")
			}
			if l := s.lookup("refreshed", f.k, f.v); !l.Found || !bytes.Equal(l.Entry, fresh) {
				s.t.Fatalf("refreshed entry %q, want %q", l.Entry, fresh)
			}
			end := statestore.CacheLease{Key: f.k, Variant: f.v, Mode: statestore.LeaseEndStale, Token: 8}
			if s.leaseOp("end, wrong token", end) {
				s.t.Fatal("end-stale without the lease token applied")
			}
			if !s.lookup("still stored", f.k, f.v).Found {
				s.t.Fatal("wrong-token end-stale deleted the variant")
			}
			end.Token = 7
			if !s.leaseOp("end", end) {
				s.t.Fatal("end-stale with the lease token not applied")
			}
			if s.lookup("ended", f.k, f.v).Found {
				s.t.Fatal("end-stale kept the variant")
			}
		}},
		{name: "cache/ttls", skip: needInspect, run: func(s *suite) {
			// Partition: the entry TTL (at most 25 h); generation 26 h;
			// fill lease 1 s; revalidation lease 5 s.
			f := fix(s)
			s.store("store", statestore.CacheStore{Key: f.k, Names: "n", Variant: f.v, TTL: 30 * time.Hour, Entry: f.body, Token: 1})
			part, ok := s.inspect(Key{Kind: KeyCachePartition, Cache: f.k})
			if !ok || part.Value != "n" {
				s.t.Fatalf("partition %+v, %v", part, ok)
			}
			s.ttlNear("partition capped at 25 h", part.TTL, 25*time.Hour)
			lease, ok := s.inspect(Key{Kind: KeyCacheLease, Cache: f.k})
			if !ok || lease.Value != "1" {
				s.t.Fatalf("fill lease %+v, %v", lease, ok)
			}
			s.ttlNear("fill lease", lease.TTL, time.Second)
			s.invalidate("bump", f.k.URI)
			gen, ok := s.inspect(Key{Kind: KeyCacheGeneration, Cache: f.k})
			if !ok {
				s.t.Fatal("generation key missing")
			}
			s.ttlNear("generation", gen.TTL, 26*time.Hour)
			s.advance(fillLease)
			s.leaseOp("take", statestore.CacheLease{Key: f.k, Variant: f.v, Mode: statestore.LeaseTake, Token: 9})
			lease, _ = s.inspect(Key{Kind: KeyCacheLease, Cache: f.k})
			s.ttlNear("revalidation lease", lease.TTL, 5*time.Second)
		}},
		{name: "cache/mac-tamper", opts: OpenOptions{MACKey: macKey}, skip: func(h Harness) string {
			if h.Tamper == nil {
				return "harness cannot tamper with entries"
			}
			return ""
		}, run: func(s *suite) {
			// With a MAC key, a tampered entry or a missing tag is a miss,
			// never an error (spec 08 req 68).
			f := fix(s)
			put := func(token uint64) {
				s.advance(fillLease)
				if !s.store("store", statestore.CacheStore{Key: f.k, Names: "n", Variant: f.v, TTL: f.ttl, Entry: f.body, Token: token}) {
					s.t.Fatal("store not applied")
				}
				if l := s.lookup("intact", f.k, f.v); !l.Found || !bytes.Equal(l.Entry, f.body) {
					s.t.Fatalf("intact tagged entry: %+v", *l)
				}
			}
			put(1)
			s.h.Tamper(s.t, s.d, f.k, f.v, func(b []byte) []byte {
				b = bytes.Clone(b)
				b[0] ^= 0xff
				return b
			})
			if l := s.lookup("tampered", f.k, f.v); l.Found {
				s.t.Fatal("tampered entry served")
			}
			put(2)
			s.h.Tamper(s.t, s.d, f.k, f.v, func([]byte) []byte { return bytes.Clone(f.body) })
			if l := s.lookup("untagged", f.k, f.v); l.Found {
				s.t.Fatal("entry without a tag served")
			}
		}},
		{name: "cache/no-mac-key", run: func(s *suite) {
			// Without a key entries are stored and read untagged.
			f := fix(s)
			s.store("store", statestore.CacheStore{Key: f.k, Names: "n", Variant: f.v, TTL: f.ttl, Entry: f.body, Token: 1})
			if l := s.lookup("untagged", f.k, f.v); !l.Found || !bytes.Equal(l.Entry, f.body) {
				s.t.Fatalf("untagged entry: %+v", *l)
			}
		}},
		{name: "read/pipeline", run: func(s *suite) {
			// Independent lookups share one pipelined batch.
			f := fix(s)
			a, b := lookupCall(f.k, f.v), lookupCall(statestore.CacheKey{URI: s.digest("u2")}, f.v)
			s.read(a, b)
			for _, c := range []*statestore.Call{a, b} {
				s.answered("batched lookup", c)
				if c.Trip != statestore.TripPipeline || c.Batch != 0 {
					s.t.Fatalf("batched lookup labeled trip %d batch %d", c.Trip, c.Batch)
				}
			}
		}},
	}
}
