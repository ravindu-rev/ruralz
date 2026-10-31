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
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
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
	// the suite then also checks the literal numbers. Without it,
	// outcomes are checked against the Model at each reply's ServerNow,
	// and case timings are scaled up tenfold.
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
	// OpenPeer returns a second driver on the store driver d uses, with
	// its own options and the time d sees, as another Node of the Cell
	// opens it; nil skips the cases that need two drivers on one store,
	// such as keyed and key-less Nodes sharing it. A driver whose store
	// lives in its own process, as memory's does, has no peer. The suite
	// closes the driver.
	OpenPeer func(t *testing.T, d statestore.Driver, o OpenOptions) statestore.Driver
}

// OpenOptions configure one driver.
type OpenOptions struct {
	// MACKey is statestore.Deps.MACKey.
	MACKey []byte
}

// KeyKind is the kind of stored key Inspect reads.
type KeyKind uint8

// Key kinds.
const (
	// KeyRateLimit is a GCRA TAT: Value is the TAT, microseconds since the
	// 2026 epoch, as text a float64 parse reads exactly.
	KeyRateLimit KeyKind = iota + 1
	// KeyQuota is a Quota counter: Value is the decimal count.
	KeyQuota
	// KeyCacheGeneration is a generation key: Value is its token, 32
	// lowercase hex characters.
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
	// Name, Window or Month, WindowStart and Digest name a KeyQuota.
	Name        string
	Window      time.Duration
	Month       bool
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

// callTimeout is every conformance call's Policy timeout: far above any
// round trip, even to a live server on a loaded machine, so a call times
// out only when the driver hangs.
const callTimeout = 10 * time.Second

// Real-time cases. On real time a case that needs its operations to run
// within a span of store time (a GCRA emission interval, a lease, one
// Quota window) reads the span from the store's own times and checks its
// outcomes only when the span held (suite.timed); a time to live is
// checked within the store time measured since it was set (suite.ttlNear).
// No result then depends on the machine's speed.
const (
	// timedAttempts bounds the attempts of a span on real time.
	timedAttempts = 3
	// probeLimit is the limit of the Quota calls that read the store's
	// time, never reached.
	probeLimit = 1 << 40
)

// RunConformance runs the driver conformance suite, including the
// consumptive grouping, deadline and entry MAC cases, against h.
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

// peer returns the case's view of a second driver on s's store
// (Harness.OpenPeer) opened with o: the same nonce, model and time, so
// its keys and fixtures are the case's own.
func (s *suite) peer(o OpenOptions) *suite {
	s.t.Helper()
	d := s.h.OpenPeer(s.t, s.d, o)
	s.t.Cleanup(func() {
		if err := d.Close(context.Background()); err != nil {
			s.t.Errorf("Close peer: %v", err)
		}
	})
	p := *s
	p.d = d
	return &p
}

func (s *suite) digest(name string) statestore.Digest {
	return statestore.DigestOf(s.nonce + "/" + name)
}

func (s *suite) name(base string) string { return base + "-" + s.nonce }

func (s *suite) advance(by time.Duration) { s.h.Advance(s.t, s.d, by) }

// advancePast moves time past a RetryAfter so the next call sees the
// instant it named: exactly by it on a fake clock. Against real time the
// store stamps its calls with the wall clock, rounded to the millisecond,
// while the sleep runs on the monotonic one, so a wall-clock correction
// can give the store less than was slept: it also waits a millisecond per
// case unit, far below every interval.
func (s *suite) advancePast(retry time.Duration) {
	if !s.h.Exact {
		retry += time.Millisecond * s.unit
	}
	s.advance(retry)
}

func (s *suite) now() time.Time { return s.h.Now(s.d) }

// tryTag tells the keys of a timed attempt apart: "" for the first, which
// a fake clock always settles, so its keys are the case's own.
func tryTag(try int) string {
	if try == 0 {
		return ""
	}
	return "-try" + strconv.Itoa(try)
}

// serverNow returns the store's time: the fake clock's when Exact, else
// the ServerNow of a Quota call on a probe key of the case's own, which
// orders after every operation the case made before it.
func (s *suite) serverNow() time.Time {
	s.t.Helper()
	if s.h.Exact {
		return s.now()
	}
	c := quotaCall(s.name("clock-probe"), time.Hour, probeLimit, s.digest("clock-probe"))
	s.consume(c)
	s.answered("store time probe", c)
	return c.Quota.ServerNow
}

// timed runs the part of a case whose checks hold only if its operations
// ran within a span of store time, such as a GCRA emission interval, a
// lease or one Quota window. attempt runs the operations on keys of its
// own (tryTag) and returns their checks and whether the span held, read
// from the store's times. With a fake clock time moves only through
// Advance, so the span holds and the checks run at once. On real time a
// loaded machine can stretch the operations past the span, and their
// outcomes then prove nothing either way: timed tries again, up to
// timedAttempts, and skips the case when no attempt held its span, so
// the result never depends on the machine's speed. The model checks of
// every answered call run in either case.
func (s *suite) timed(attempt func(try int) (check func(), held bool)) {
	s.t.Helper()
	for try := range timedAttempts {
		check, held := attempt(try)
		if held {
			check()
			return
		}
		if s.h.Exact {
			s.t.Fatal("statestoretest: a span failed to hold on a fake clock")
		}
	}
	s.t.Skipf("statestoretest: none of %d attempts ran within its span of store time; the machine is too slow for this real-time case", timedAttempts)
}

// sameWindow reports whether every Quota answer charged one window.
func sameWindow(qs ...*statestore.Quota) bool {
	for _, q := range qs[1:] {
		if !q.WindowStart.Equal(qs[0].WindowStart) {
			return false
		}
	}
	return true
}

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

// quotaCostCall returns a Quota call reserving cost units of window, or
// of the UTC calendar month with month.
func quotaCostCall(name string, window time.Duration, month bool, limit, cost int64, d statestore.Digest) *statestore.Call {
	c := quotaCall(name, window, limit, d)
	c.Quota.Month, c.Quota.Cost = month, cost
	return c
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

// checkGCRA compares a decided GCRA call with the Model at its
// ServerNow.
func (s *suite) checkGCRA(what string, c *statestore.Call) {
	s.t.Helper()
	s.answered(what, c)
	g := &c.GCRA
	if s.h.Exact {
		if want := s.now().Truncate(time.Microsecond); !g.ServerNow.Equal(want) {
			s.t.Fatalf("%s: ServerNow %v, want the store time %v", what, g.ServerNow, want)
		}
	}
	want := statestore.GCRA{Policy: g.Policy, Digest: g.Digest, Limits: g.Limits, Cost: g.Cost, Out: make([]statestore.GCRAOutcome, len(g.Limits))}
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
	return s.gcraCost(what, policy, d, 0, limits...)
}

// gcraCost runs one GCRA call charging cost and checks it against the
// model.
func (s *suite) gcraCost(what, policy string, d statestore.Digest, cost int64, limits ...statestore.GCRALimit) *statestore.GCRA {
	s.t.Helper()
	c := gcraCall(policy, d, limits...)
	c.GCRA.Cost = cost
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
	want := statestore.Quota{Name: q.Name, Window: q.Window, Month: q.Month, Limit: q.Limit, Cost: q.Cost, Digest: q.Digest}
	if ok, skew := s.model.Quota(q.ServerNow, q.ServerNow, &want); !ok || skew {
		s.t.Fatalf("%s: the model rejects the call", what)
	}
	if q.Allowed != want.Allowed || q.Used != want.Used || q.RetryAfter != want.RetryAfter ||
		!q.WindowStart.Equal(want.WindowStart) || !q.WindowEnd.Equal(want.WindowEnd) || !q.ServerNow.Equal(want.ServerNow) {
		s.t.Fatalf("%s: got %+v, model %+v", what, *q, want)
	}
}

func (s *suite) quota(what, name string, window time.Duration, limit int64, d statestore.Digest) *statestore.Quota {
	s.t.Helper()
	return s.quotaCost(what, name, window, false, limit, 0, d)
}

// quotaCost runs one Quota call reserving cost units of window, or of the
// calendar month with month, and checks it against the model.
func (s *suite) quotaCost(what, name string, window time.Duration, month bool, limit, cost int64, d statestore.Digest) *statestore.Quota {
	s.t.Helper()
	c := quotaCostCall(name, window, month, limit, cost, d)
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

func (s *suite) invalidate(what string, uri statestore.Digest, bump statestore.Token) {
	s.t.Helper()
	w := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: callTimeout, Invalidate: uri, Bump: bump}
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

// ttlNear checks a TTL against want, the TTL the store set at store time
// ref or later: exact with a fake clock, else within the store time
// spent since ref, read after the TTL, and the millisecond the store
// rounds to.
func (s *suite) ttlNear(what string, got, want time.Duration, ref time.Time) {
	s.t.Helper()
	slack := time.Duration(0)
	if !s.h.Exact {
		slack = s.serverNow().Sub(ref) + time.Millisecond
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

func needSkew(h Harness) string {
	if h.SkewNode == nil {
		return "harness cannot skew the Node clock"
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

// gcraCases cover the GCRA algorithm.
func gcraCases() []conformanceCase {
	return []conformanceCase{
		{name: "gcra/burst-from-idle", run: func(s *suite) {
			// requests 10, window 1 s, burst 10 (T = 100 ms, τ = 1 s): 11
			// calls at one instant are admitted, the 12th is denied with
			// RetryAfter = T, and one more is admitted T later. The 12
			// calls must span less than T.
			w := time.Second * s.unit
			l := statestore.GCRALimit{Requests: 10, Window: w, Burst: 10}
			s.timed(func(try int) (func(), bool) {
				p, d := s.name("rl"+tryTag(try)), s.digest("k"+tryTag(try))
				var first *statestore.GCRA
				for i := range 11 {
					g := s.gcra("call "+strconv.Itoa(i+1), p, d, l)
					if !g.Allowed {
						s.t.Fatalf("call %d denied from idle", i+1)
					}
					if first == nil {
						first = g
					}
				}
				g := s.gcra("call 12", p, d, l)
				return func() {
					if g.Allowed || g.Denied != 0 {
						s.t.Fatalf("call 12 allowed %v denied %d; want denied by limit 0", g.Allowed, g.Denied)
					}
					if s.h.Exact && g.RetryAfter != 100*time.Millisecond {
						s.t.Fatalf("RetryAfter %v, want T = 100ms", g.RetryAfter)
					}
					s.advancePast(g.RetryAfter)
					if g := s.gcra("after RetryAfter", p, d, l); !g.Allowed {
						s.t.Fatal("denied once RetryAfter passed")
					}
				}, g.ServerNow.Sub(first.ServerNow) < w/10
			})
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
			// burst 0: one admitted at once, then one per T. The first two
			// calls must span less than T.
			w := time.Second * s.unit
			l := statestore.GCRALimit{Requests: 10, Window: w, Burst: 0}
			s.timed(func(try int) (func(), bool) {
				p, d := s.name("rl"+tryTag(try)), s.digest("k"+tryTag(try))
				first := s.gcra("first", p, d, l)
				if !first.Allowed || first.Out[0].Remaining != 0 {
					s.t.Fatalf("first call allowed %v remaining %d, want 0 left", first.Allowed, first.Out[0].Remaining)
				}
				g := s.gcra("second", p, d, l)
				return func() {
					if g.Allowed {
						s.t.Fatal("second call at once admitted with burst 0")
					}
					if s.h.Exact && g.RetryAfter != 100*time.Millisecond {
						s.t.Fatalf("RetryAfter %v, want 100ms", g.RetryAfter)
					}
					s.advancePast(g.RetryAfter)
					if g := s.gcra("third", p, d, l); !g.Allowed {
						s.t.Fatal("not admitted one T later")
					}
				}, g.ServerNow.Sub(first.ServerNow) < w/10
			})
		}},
		{name: "gcra/multi-limit", run: func(s *suite) {
			// Limits 2/1s and 3/1m: a deny by one limit leaves every TAT
			// unchanged (all or nothing); remaining and reset per limit.
			// The calls up to the denial must span less than the first
			// limit's emission interval.
			l1 := statestore.GCRALimit{Requests: 2, Window: time.Second * s.unit, Burst: 2}
			l2 := statestore.GCRALimit{Requests: 3, Window: time.Minute * s.unit, Burst: 3}
			s.timed(func(try int) (func(), bool) {
				p, d := s.name("rl"+tryTag(try)), s.digest("k"+tryTag(try))
				var first, g *statestore.GCRA
				for i := range 10 {
					g = s.gcra("call "+strconv.Itoa(i+1), p, d, l1, l2)
					if first == nil {
						first = g
					}
					if !g.Allowed {
						break
					}
				}
				return func() {
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
					// The denial reserved nothing: the second limit still
					// admits once the first recovers.
					s.advancePast(g.RetryAfter)
					if g := s.gcra("after recovery", p, d, l1, l2); !g.Allowed {
						s.t.Fatal("denied after the denying limit recovered")
					}
				}, g.ServerNow.Sub(first.ServerNow) < l1.Window/time.Duration(l1.Requests)
			})
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
			s.ttlNear("GCRA key", st.TTL, want, g.ServerNow)
		}},
		{name: "gcra/cost", run: func(s *suite) {
			// requests 10, window 1 s, burst 10 (T = 100 ms, τ = 1 s), cost
			// 4: slack τ - 3T = 700 ms. Two calls admit (TAT now + 400 ms,
			// then + 800 ms), the third is denied though 3 units remain,
			// with RetryAfter 100 ms, and a cost-3 call (slack 800 ms) then
			// admits. The four calls must span less than T.
			w := time.Second * s.unit
			l := statestore.GCRALimit{Requests: 10, Window: w, Burst: 10}
			s.timed(func(try int) (func(), bool) {
				p, d := s.name("rl"+tryTag(try)), s.digest("k"+tryTag(try))
				first := s.gcraCost("first", p, d, 4, l)
				second := s.gcraCost("second", p, d, 4, l)
				third := s.gcraCost("third", p, d, 4, l)
				fourth := s.gcraCost("cost 3", p, d, 3, l)
				return func() {
					if !first.Allowed || !second.Allowed || third.Allowed || !fourth.Allowed {
						s.t.Fatalf("allowed %v, %v, %v, %v; want cost 4 twice, then denied, then cost 3",
							first.Allowed, second.Allowed, third.Allowed, fourth.Allowed)
					}
					if third.Denied != 0 || third.Out[0].Remaining == 0 {
						s.t.Fatalf("third call denied by %d with %d left; want limit 0 with units left", third.Denied, third.Out[0].Remaining)
					}
					if !s.h.Exact {
						return
					}
					for _, c := range []struct {
						what string
						got  statestore.GCRAOutcome
						want statestore.GCRAOutcome
					}{
						{"first", first.Out[0], statestore.GCRAOutcome{Remaining: 7, ResetAfter: 400 * time.Millisecond}},
						{"second", second.Out[0], statestore.GCRAOutcome{Remaining: 3, ResetAfter: 800 * time.Millisecond}},
						{"third", third.Out[0], statestore.GCRAOutcome{Remaining: 3, ResetAfter: 800 * time.Millisecond}},
						{"cost 3", fourth.Out[0], statestore.GCRAOutcome{Remaining: 0, ResetAfter: 1100 * time.Millisecond}},
					} {
						if c.got != c.want {
							s.t.Fatalf("%s outcome %+v, want %+v", c.what, c.got, c.want)
						}
					}
					if third.RetryAfter != 100*time.Millisecond {
						s.t.Fatalf("RetryAfter %v, want 100ms", third.RetryAfter)
					}
				}, fourth.ServerNow.Sub(first.ServerNow) < w/10
			})
		}},
		{name: "gcra/cost-above-tolerance", run: func(s *suite) {
			// burst 0 (τ = 0) and cost 2: the slack is -T, so the call never
			// conforms, from idle or after its RetryAfter of T.
			l := statestore.GCRALimit{Requests: 10, Window: time.Second * s.unit, Burst: 0}
			p, d := s.name("rl"), s.digest("k")
			g := s.gcraCost("from idle", p, d, 2, l)
			if g.Allowed || g.Denied != 0 {
				s.t.Fatalf("cost 2 with burst 0 allowed %v denied %d; want denied", g.Allowed, g.Denied)
			}
			if s.h.Exact && g.RetryAfter != 100*time.Millisecond {
				s.t.Fatalf("RetryAfter %v, want T = 100ms", g.RetryAfter)
			}
			s.advance(g.RetryAfter)
			if g := s.gcraCost("after RetryAfter", p, d, 2, l); g.Allowed {
				s.t.Fatal("cost 2 with burst 0 admitted after RetryAfter")
			}
			if g := s.gcraCost("cost 1", p, d, 1, l); !g.Allowed {
				s.t.Fatal("cost 1 denied on an idle key")
			}
		}},
		{name: "gcra/cost-all-or-nothing", run: func(s *suite) {
			// Limits 10/1s (burst 10) and 4/1m (burst 0, T = 15 s), cost 2:
			// the second limit never conforms, so the call is denied by it
			// and the first limit's TAT is not advanced.
			l1 := statestore.GCRALimit{Requests: 10, Window: time.Second * s.unit, Burst: 10}
			l2 := statestore.GCRALimit{Requests: 4, Window: time.Minute * s.unit, Burst: 0}
			p, d := s.name("rl"), s.digest("k")
			g := s.gcraCost("cost 2", p, d, 2, l1, l2)
			if g.Allowed || g.Denied != 1 {
				s.t.Fatalf("allowed %v denied %d; want denied by limit 1", g.Allowed, g.Denied)
			}
			if _, held := s.model.TAT(p, l1, d); held {
				s.t.Fatal("a denied call advanced the first limit's TAT")
			}
			if s.h.Inspect != nil {
				if _, ok := s.inspect(Key{Kind: KeyRateLimit, Policy: p, Limit: l1, Digest: d}); ok {
					s.t.Fatal("a denied call wrote the first limit's TAT")
				}
			}
		}},
		{name: "gcra/cost-zero-is-one", skip: needExact, run: func(s *suite) {
			// Cost 0 charges one emission interval, as cost 1 does.
			l := statestore.GCRALimit{Requests: 10, Window: time.Second, Burst: 10}
			zero := s.gcraCost("cost 0", s.name("rl0"), s.digest("k0"), 0, l)
			one := s.gcraCost("cost 1", s.name("rl1"), s.digest("k1"), 1, l)
			if zero.Out[0] != one.Out[0] || zero.Allowed != one.Allowed {
				s.t.Fatalf("cost 0 outcome %+v, cost 1 %+v; want equal", zero.Out[0], one.Out[0])
			}
		}},
		{name: "gcra/negative-cost", run: func(s *suite) {
			// A negative cost is an error reply: RZ-STS-002.
			c := gcraCall(s.name("rl"), s.digest("k"), statestore.GCRALimit{Requests: 1, Window: time.Second, Burst: 1})
			c.GCRA.Cost = -1
			s.consume(c)
			if c.Err != statestore.ErrFailed || c.Done {
				s.t.Fatalf("negative cost: Done %v Err %v, want RZ-STS-002", c.Done, c.Err)
			}
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

// quotaCases cover Quota reserve and refund, clock skew included.
func quotaCases() []conformanceCase {
	return append(quotaWindowCases(), quotaCostCases()...)
}

// quotaCostCases cover cost units, refunded units and calendar months.
func quotaCostCases() []conformanceCase {
	return []conformanceCase{
		{name: "quota/cost", run: func(s *suite) {
			// Cost units are reserved all or none: a cost above the units
			// left denies, the count unchanged, with RetryAfter to the
			// window end; 0 counts as 1. The calls must charge one window.
			s.timed(func(try int) (func(), bool) {
				n, d := s.name("q"+tryTag(try)), s.digest("k"+tryTag(try))
				first := s.quotaCost("cost 3", n, time.Hour, false, 5, 3, d)
				over := s.quotaCost("cost 3 past the units left", n, time.Hour, false, 5, 3, d)
				zero := s.quotaCost("cost 0", n, time.Hour, false, 5, 0, d)
				last := s.quotaCost("the last unit", n, time.Hour, false, 5, 1, d)
				full := s.quotaCost("exhausted", n, time.Hour, false, 5, 1, d)
				return func() {
					if !first.Allowed || first.Used != 3 {
						s.t.Fatalf("cost 3: allowed %v used %d", first.Allowed, first.Used)
					}
					if over.Allowed || over.Used != 3 || over.RetryAfter != over.WindowEnd.Sub(over.ServerNow) {
						s.t.Fatalf("cost 3 past the units left: allowed %v used %d retry %v", over.Allowed, over.Used, over.RetryAfter)
					}
					if !zero.Allowed || zero.Used != 4 {
						s.t.Fatalf("cost 0: allowed %v used %d, want one unit", zero.Allowed, zero.Used)
					}
					if !last.Allowed || last.Used != 5 || full.Allowed || full.Used != 5 {
						s.t.Fatalf("last unit %+v, exhausted %+v", *last, *full)
					}
				}, sameWindow(first, over, zero, last, full)
			})
		}},
		{name: "quota/cost-above-limit", run: func(s *suite) {
			// A cost above the limit, the largest included, denies and
			// reserves nothing.
			n, d := s.name("q"), s.digest("k")
			for _, cost := range []int64{6, math.MaxInt64} {
				if q := s.quotaCost("cost above the limit", n, time.Hour, false, 5, cost, d); q.Allowed || q.Used != 0 {
					s.t.Fatalf("cost %d: allowed %v used %d", cost, q.Allowed, q.Used)
				}
			}
			if q := s.quotaCost("after", n, time.Hour, false, 5, 5, d); !q.Allowed || q.Used != 5 {
				s.t.Fatalf("after denied costs: allowed %v used %d", q.Allowed, q.Used)
			}
		}},
		{name: "quota/negative-cost", run: func(s *suite) {
			// A negative cost is an error reply (RZ-STS-002) charging nothing.
			n, d := s.name("q"), s.digest("k")
			c := quotaCostCall(n, time.Hour, false, 5, -1, d)
			s.consume(c)
			if c.Done || c.Err != statestore.ErrFailed {
				s.t.Fatalf("negative cost: Done %v Err %v, want RZ-STS-002", c.Done, c.Err)
			}
			if q := s.quota("after", n, time.Hour, 5, d); !q.Allowed || q.Used != 1 {
				s.t.Fatalf("after a negative cost: allowed %v used %d", q.Allowed, q.Used)
			}
		}},
		{name: "quota/refund-units", run: func(s *suite) {
			// A refund returns its units, at most the count, so the count
			// stops at 0. The reservations must charge one window.
			s.timed(func(try int) (func(), bool) {
				n, d := s.name("q"+tryTag(try)), s.digest("k"+tryTag(try))
				q := s.quotaCost("reserve 4", n, time.Hour, false, 10, 4, d)
				ref := statestore.Refund{Name: n, Window: time.Hour, WindowStart: q.WindowStart, Units: 3, Digest: d}
				var applied []bool
				refund := func(what string, units int64) {
					ref.Units = units
					ok := s.refund(what, ref)
					if ok {
						s.model.Refund(ref)
					}
					applied = append(applied, ok)
				}
				refund("refund 3", 3)
				mid := s.quotaCost("reserve 1", n, time.Hour, false, 10, 1, d)
				refund("refund 10", 10)
				refund("refund at 0", 10)
				after := s.quotaCost("after", n, time.Hour, false, 10, 1, d)
				return func() {
					if !applied[0] || mid.Used != 2 {
						s.t.Fatalf("refund 3 of 4: applied %v, then used %d; want 2", applied[0], mid.Used)
					}
					if !applied[1] || applied[2] || after.Used != 1 {
						s.t.Fatalf("refund 10 of 2: applied %v, at 0 applied %v, then used %d; want 1", applied[1], applied[2], after.Used)
					}
				}, sameWindow(q, mid, after)
			})
		}},
		{name: "quota/refund-negative-units", run: func(s *suite) {
			// Negative units are an error reply (RZ-STS-002).
			w := &statestore.Write{Kind: statestore.OpRefund, Timeout: callTimeout, Refund: statestore.Refund{
				Name: s.name("q"), Window: time.Hour, WindowStart: time.UnixMilli(0), Units: -1, Digest: s.digest("k"),
			}}
			s.write(w)
			if w.Err != statestore.ErrFailed || w.Applied {
				s.t.Fatalf("negative units: Err %v Applied %v, want RZ-STS-002", w.Err, w.Applied)
			}
		}},
		{name: "quota/month", run: func(s *suite) {
			// A calendar quota charges the UTC month holding the store's
			// time, from 00:00 UTC on its first day to the next month's,
			// on a counter of its own that lives a month past the month's
			// end; a refund names that month. The calls must charge one
			// month.
			s.timed(func(try int) (func(), bool) {
				n, d := s.name("q"+tryTag(try)), s.digest("k"+tryTag(try))
				q := s.quotaCost("month", n, 0, true, 10, 2, d)
				win := s.quotaCost("a window of the same name", n, 720*time.Hour, false, 10, 1, d)
				ws, we := q.WindowStart.UTC(), q.WindowEnd.UTC()
				var st Stored
				var held bool
				if s.h.Inspect != nil {
					st, held = s.inspect(Key{Kind: KeyQuota, Name: n, Month: true, WindowStart: ws, Digest: d})
				}
				ref := statestore.Refund{Name: n, Month: true, WindowStart: ws, Units: 2, Digest: d}
				refunded := s.refund("month refund", ref)
				if refunded {
					s.model.Refund(ref)
				}
				after := s.quotaCost("after the refund", n, 0, true, 10, 1, d)
				return func() {
					if ws.Day() != 1 || ws.Hour() != 0 || ws.Minute() != 0 || ws.Second() != 0 || ws.Nanosecond() != 0 ||
						!we.Equal(ws.AddDate(0, 1, 0)) || q.ServerNow.Before(ws) || !q.ServerNow.Before(we) {
						s.t.Fatalf("month [%v, %v) for store time %v", ws, we, q.ServerNow)
					}
					if !q.Allowed || q.Used != 2 || !win.Allowed || win.Used != 1 {
						s.t.Fatalf("month %+v, window %+v: want separate counters", *q, *win)
					}
					if !refunded || after.Used != 1 {
						s.t.Fatalf("month refund applied %v, then used %d; want 1", refunded, after.Used)
					}
					if s.h.Inspect != nil {
						if !held || st.Value != "2" {
							s.t.Fatalf("month counter %q, %v; want 2", st.Value, held)
						}
						s.ttlNear("month counter", st.TTL, we.Add(we.Sub(ws)).Sub(q.ServerNow), q.ServerNow)
					}
				}, q.WindowStart.Equal(after.WindowStart)
			})
		}},
		{name: "quota/month-skew", skip: needSkew, run: func(s *suite) {
			// A Node clock 70 days ahead offers two months, its own and
			// the nearer neighbor, neither holding the store's time:
			// RZ-STS-002, charging nothing.
			n, d := s.name("q"), s.digest("k")
			s.h.SkewNode(s.t, s.d, 70*24*time.Hour)
			c := quotaCostCall(n, 0, true, 10, 1, d)
			s.consume(c)
			if c.Done || c.Err != statestore.ErrFailed {
				s.t.Fatalf("70 days ahead: Done %v Err %v, want RZ-STS-002", c.Done, c.Err)
			}
			if !s.h.Exact {
				return
			}
			// A minute before the store's month ends, a Node clock two
			// minutes ahead lies early in the next month, whose nearer
			// neighbor is the store's month: that month is charged.
			s.h.SkewNode(s.t, s.d, 0)
			now := s.now()
			_, end := QuotaWindowAt(now, 0, true)
			s.advance(time.UnixMilli(end).Sub(now) - time.Minute)
			s.h.SkewNode(s.t, s.d, 2*time.Minute)
			q := s.quotaCost("store month", n, 0, true, 10, 1, d)
			if !q.Allowed || q.Used != 1 || !q.WindowEnd.Equal(time.UnixMilli(end)) {
				s.t.Fatalf("Node in the next month: %+v, want the store's month ending %v", *q, time.UnixMilli(end).UTC())
			}
		}},
	}
}

// quotaWindowCases cover window reservations and refunds, clock skew
// included.
func quotaWindowCases() []conformanceCase {
	return []conformanceCase{
		{name: "quota/limit", run: func(s *suite) {
			// Reserve up to the limit, then deny with RetryAfter to the
			// window end; a raised limit admits at once. The calls must
			// charge one window.
			s.timed(func(try int) (func(), bool) {
				n, d := s.name("q"+tryTag(try)), s.digest("k"+tryTag(try))
				var qs []*statestore.Quota
				for range 3 {
					qs = append(qs, s.quota("reserve", n, time.Hour, 3, d))
				}
				q := s.quota("exhausted", n, time.Hour, 3, d)
				raised := s.quota("raised", n, time.Hour, 4, d)
				return func() {
					for i, r := range qs {
						if !r.Allowed || r.Used != int64(i+1) {
							s.t.Fatalf("reserve %d: allowed %v used %d", i+1, r.Allowed, r.Used)
						}
					}
					if q.Allowed || q.Used != 3 {
						s.t.Fatalf("exhausted: allowed %v used %d", q.Allowed, q.Used)
					}
					if end := q.WindowStart.Add(time.Hour); q.RetryAfter != end.Sub(q.ServerNow) {
						s.t.Fatalf("RetryAfter %v, want %v to the window end", q.RetryAfter, end.Sub(q.ServerNow))
					}
					if !raised.Allowed || raised.Used != 4 {
						s.t.Fatalf("raised limit: allowed %v used %d", raised.Allowed, raised.Used)
					}
				}, sameWindow(append(qs, q, raised)...)
			})
		}},
		{name: "quota/limit-zero", run: func(s *suite) {
			if q := s.quota("zero", s.name("q"), time.Hour, 0, s.digest("k")); q.Allowed || q.Used != 0 {
				s.t.Fatalf("limit 0: allowed %v used %d", q.Allowed, q.Used)
			}
		}},
		{name: "quota/refund", run: func(s *suite) {
			// A refund decrements the charged window only, never below 0.
			// The first two calls must charge one window.
			s.timed(func(try int) (func(), bool) {
				n, d := s.name("q"+tryTag(try)), s.digest("k"+tryTag(try))
				first := s.quota("first", n, time.Hour, 10, d)
				q := s.quota("second", n, time.Hour, 10, d)
				ref := statestore.Refund{Name: n, Window: time.Hour, WindowStart: q.WindowStart, Digest: d}
				other := ref
				other.WindowStart = q.WindowStart.Add(-time.Hour)
				otherApplied := s.refund("other window", other)
				if otherApplied {
					s.model.Refund(other)
				}
				var applied []bool
				for range 3 {
					ok := s.refund("refund", ref)
					if ok {
						s.model.Refund(ref)
					}
					applied = append(applied, ok)
				}
				after := s.quota("after refunds", n, time.Hour, 10, d)
				return func() {
					if otherApplied {
						s.t.Fatal("refund of an uncharged window applied")
					}
					for i, want := range []bool{true, true, false} {
						if applied[i] != want {
							s.t.Fatalf("refund %d applied %v, want %v", i+1, applied[i], want)
						}
					}
					if after.Used != 1 {
						s.t.Fatalf("used %d after refunding to 0, want 1", after.Used)
					}
				}, sameWindow(first, q)
			})
		}},
		{name: "quota/refund-after-expiry", skip: needInspect, run: func(s *suite) {
			// A refund after the window's key expired creates no key. The
			// window scales with real time like every case's, as half of
			// it is the time the reserve may take before the store refuses
			// its candidate windows as skewed.
			w := time.Second * s.unit
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
			s.ttlNear("quota key", st.TTL, q.WindowStart.Add(48*time.Hour).Sub(q.ServerNow), q.ServerNow)
		}},
		{name: "quota/skew", skip: func(h Harness) string {
			if h.SkewNode == nil {
				return "harness cannot skew the Node clock"
			}
			return ""
		}, run: func(s *suite) {
			// Node clock 0.6 of a window ahead near the end of the server
			// window: RZ-STS-002; 0.4 ahead: the server's window. The 0.6
			// call must reach the store before its window ends.
			w := time.Second * s.unit
			s.timed(func(try int) (func(), bool) {
				n, d := s.name("q"+tryTag(try)), s.digest("k"+tryTag(try))
				s.h.SkewNode(s.t, s.d, 0)
				probe := s.quota("probe", s.name("probe"+tryTag(try)), w, 1000, s.digest("probe"+tryTag(try)))
				pos := probe.ServerNow.Sub(probe.WindowStart)
				end := probe.WindowStart.Add(w)
				if pos > w*95/100 {
					end = end.Add(w)
				}
				s.advance((w + w*95/100 - pos) % w)
				s.h.SkewNode(s.t, s.d, w*6/10)
				c := quotaCall(n, w, 10, d)
				s.consume(c)
				if c.Done && c.Err == nil {
					// Too late: the server window rolled over. Keep the
					// model in step with the charge.
					s.checkQuota("0.6 window skew", c)
				}
				held := s.serverNow().Before(end)
				s.h.SkewNode(s.t, s.d, w*4/10)
				q := s.quota("0.4 window skew", n, w, 10, d)
				return func() {
					if c.Err != statestore.ErrFailed || c.Done {
						s.t.Fatalf("0.6 window skew: Done %v Err %v, want RZ-STS-002", c.Done, c.Err)
					}
					if !q.Allowed || q.Used != 1 {
						s.t.Fatalf("0.4 window skew: allowed %v used %d", q.Allowed, q.Used)
					}
				}, held
			})
		}},
	}
}

// consumeCases cover consumptive calls: call timeouts, chain order and
// slot grouping, the pre-call check order, the shared failure of a merged
// round trip and script_multi.
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
			// (RZ-STS-004) and change nothing. The first call must answer
			// within the budget, and the fresh request charge its window.
			budget := 10 * time.Millisecond
			if !s.h.Exact {
				budget = time.Second
			}
			s.timed(func(try int) (func(), bool) {
				n, d := s.name("q"+tryTag(try)), s.digest("k"+tryTag(try))
				start := s.serverNow()
				rb := statestore.NewRequestBudget(budget, 0)
				first := quotaCall(n, time.Hour, 5, d)
				s.d.Consume(context.Background(), &rb, []*statestore.Call{first})
				inBudget := s.serverNow().Before(start.Add(budget))
				if first.Done && first.Err == nil {
					s.checkQuota("first", first)
				}
				s.advance(2 * budget)
				late := quotaCall(n, time.Hour, 5, d)
				s.d.Consume(context.Background(), &rb, []*statestore.Call{late})
				fresh := quotaCall(n, time.Hour, 5, d)
				s.consume(fresh)
				if first.Done && first.Err == nil {
					// A first call that timed out may still have charged,
					// which the model cannot know: then only the span,
					// which it missed, decides.
					s.checkQuota("fresh request", fresh)
				}
				return func() {
					s.answered("first", first)
					s.answered("fresh request", fresh)
					if late.Err != statestore.ErrNotAttempted || late.Done {
						s.t.Fatalf("spent budget: Done %v Err %v, want RZ-STS-004", late.Done, late.Err)
					}
					if fresh.Quota.Used != 2 {
						s.t.Fatalf("used %d, want 2: the late call must not charge", fresh.Quota.Used)
					}
				}, inBudget && (!first.Done || sameWindow(&first.Quota, &fresh.Quota))
			})
		}},
		{name: "consume/zero-timeout", run: func(s *suite) {
			// A resolved timeout of 0 is never attempted.
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
			// The merged round trip's timeout is the smallest of its calls',
			// so a zero timeout anywhere in the group leaves every call not
			// attempted (RZ-STS-004) and charges nothing, in either order.
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

// cacheFixture is one cache case's keys, entry TTL and body.
type cacheFixture struct {
	k    statestore.CacheKey
	v    statestore.Digest
	ttl  time.Duration
	body []byte
}

// fix returns the cache keys tagged tag (tryTag) of a case.
func (s *suite) fix(tag string) cacheFixture {
	return cacheFixture{
		k:   statestore.CacheKey{URI: s.digest("u" + tag), Partition: s.digest("p" + tag)},
		v:   s.digest("v" + tag),
		ttl: time.Hour, body: []byte("entry-" + s.nonce + tag),
	}
}

// token returns the case's generation token named name: never zero.
func (s *suite) token(name string) statestore.Token {
	var t statestore.Token
	d := s.digest("token/" + name)
	copy(t[:], d[:])
	t[0] |= 1
	return t
}

// put stores body as variant v of f's partition the way the cache Filter
// does: under the generation token a lookup reads, or, when the URI has
// no generation key, creating it with s.token(f's URI); it returns
// whether the store applied and the token it wrote under.
func (s *suite) put(what string, f cacheFixture, names string, v statestore.Digest, lease uint64) (bool, statestore.Token) {
	s.t.Helper()
	l := s.lookup(what+": lookup", f.k, v)
	cs := statestore.CacheStore{Key: f.k, Names: names, Variant: v, TTL: f.ttl, Entry: f.body, Token: lease, Generation: l.Token}
	if !l.HasToken {
		cs.Create, cs.Generation = true, s.token("gen")
	}
	return s.store(what, cs), cs.Generation
}

// cacheCases cover Response Cache storage, generation tokens and the entry
// MAC.
func cacheCases() []conformanceCase {
	// fillLease outlasts the 1 s fill lease; revalidation is the 5 s
	// revalidation lease.
	fillLease := time.Second + 10*time.Millisecond
	const revalidation = 5 * time.Second
	macKey := bytes.Repeat([]byte{0x5a}, 32)
	return []conformanceCase{
		{name: "cache/miss", run: func(s *suite) {
			f := s.fix("")
			l := s.lookup("miss", f.k, f.v)
			if l.Found || l.HasToken || !l.Token.IsZero() || l.Partition || l.Names != "" || l.Resident != 0 {
				s.t.Fatalf("empty partition: %+v", *l)
			}
		}},
		{name: "cache/store-creates-generation", run: func(s *suite) {
			// The first store of a URI creates its generation key holding
			// the token the Node passes, and the lookup reads it back.
			f := s.fix("")
			tok := s.token("first")
			if !s.store("create", statestore.CacheStore{Key: f.k, Names: "accept", Variant: f.v, TTL: f.ttl, Entry: f.body, Token: 1, Generation: tok, Create: true}) {
				s.t.Fatal("store creating the generation key not applied")
			}
			l := s.lookup("hit", f.k, f.v)
			if !l.HasToken || l.Token != tok || !l.Found || !bytes.Equal(l.Entry, f.body) || !l.Partition || l.Names != "accept" {
				s.t.Fatalf("lookup after the store: %+v, want token %x", *l, tok)
			}
		}},
		{name: "cache/create-loses-to-existing-key", run: func(s *suite) {
			// A store whose lookup found no generation key writes nothing
			// when one appeared meanwhile, such as an invalidation's: a
			// store racing an invalidation never hits.
			f := s.fix("")
			bump := s.token("bump")
			s.invalidate("bump", f.k.URI, bump)
			if s.store("create", statestore.CacheStore{Key: f.k, Names: "n", Variant: f.v, TTL: f.ttl, Entry: f.body, Token: 1, Generation: s.token("late"), Create: true}) {
				s.t.Fatal("store creating an existing generation key applied")
			}
			l := s.lookup("after", f.k, f.v)
			if l.Token != bump || l.Found || l.Partition {
				s.t.Fatalf("losing create wrote something: %+v", *l)
			}
			if !s.store("under the key's token", statestore.CacheStore{Key: f.k, Names: "n", Variant: f.v, TTL: f.ttl, Entry: f.body, Token: 2, Generation: bump}) {
				s.t.Fatal("store under the token the key holds not applied")
			}
		}},
		{name: "cache/store-needs-current-token", run: func(s *suite) {
			// A store writes only while the generation key still holds the
			// token its lookup read: after a bump it writes nothing, not
			// even the fill lease, so a store under the new token goes on.
			f := s.fix("")
			_, old := s.put("first", f, "n", f.v, 1)
			bump := s.token("bump")
			s.invalidate("bump", f.k.URI, bump)
			s.advance(fillLease)
			v2 := s.digest("v2")
			if s.store("stale token", statestore.CacheStore{Key: f.k, Names: "n", Variant: v2, TTL: f.ttl, Entry: f.body, Token: 2, Generation: old}) {
				s.t.Fatal("store under a replaced token applied")
			}
			if s.lookup("unwritten", f.k, v2).Found {
				s.t.Fatal("store under a replaced token wrote its variant")
			}
			if !s.store("current token", statestore.CacheStore{Key: f.k, Names: "n", Variant: v2, TTL: f.ttl, Entry: f.body, Token: 3, Generation: bump}) {
				s.t.Fatal("store under the current token blocked by the refused one")
			}
		}},
		{name: "cache/invalidation-sets-token", run: func(s *suite) {
			// A bump sets the generation key to exactly the token the Node
			// passes, replacing any other, whether or not the key existed.
			f := s.fix("")
			first, second := s.token("one"), s.token("two")
			s.invalidate("on a missing key", f.k.URI, first)
			if l := s.lookup("first", f.k, f.v); !l.HasToken || l.Token != first {
				s.t.Fatalf("token %x, want %x", l.Token, first)
			}
			s.invalidate("on the key", f.k.URI, second)
			if l := s.lookup("second", f.k, f.v); l.Token != second {
				s.t.Fatalf("token %x, want %x", l.Token, second)
			}
		}},
		{name: "cache/store-needs-lease", run: func(s *suite) {
			// A store takes the 1 s fill lease; a second store within 1 s
			// is not applied.
			s.timed(func(try int) (func(), bool) {
				f := s.fix(tryTag(try))
				start := s.serverNow()
				ok, tok := s.put("first", f, "accept", f.v, 1)
				if !ok {
					s.t.Fatal("first store not applied")
				}
				l := s.lookup("hit", f.k, f.v)
				if !l.Found || !bytes.Equal(l.Entry, f.body) || l.Names != "accept" || l.Token != tok {
					s.t.Fatalf("lookup after store: %+v", *l)
				}
				v2 := s.digest("v2" + tryTag(try))
				second := s.store("second", statestore.CacheStore{Key: f.k, Names: "accept", Variant: v2, TTL: f.ttl, Entry: f.body, Token: 2, Generation: tok})
				found := s.lookup("second variant", f.k, v2).Found
				held := s.serverNow().Before(start.Add(time.Second))
				return func() {
					if second {
						s.t.Fatal("second store within the fill lease applied")
					}
					if found {
						s.t.Fatal("unapplied store wrote its variant")
					}
					s.advance(fillLease)
					if !s.store("after lease", statestore.CacheStore{Key: f.k, Names: "accept", Variant: v2, TTL: f.ttl, Entry: f.body, Token: 3, Generation: tok}) {
						s.t.Fatal("store after the fill lease not applied")
					}
				}, held
			})
		}},
		{name: "cache/names-reset", run: func(s *suite) {
			// A store under other Vary names drops the partition first.
			f := s.fix("")
			s.put("names a", f, "a", f.v, 1)
			s.advance(fillLease)
			v2 := s.digest("v2")
			if ok, _ := s.put("names b", f, "b", v2, 2); !ok {
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
			f := s.fix("")
			vs := make([]statestore.Digest, 9)
			for i := range vs {
				vs[i] = s.digest("v" + strconv.Itoa(i))
				if ok, _ := s.put("variant", f, "n", vs[i], uint64(i+1)); !ok {
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
		{name: "cache/store-time-less-receipt-delay", skip: needExact, run: func(s *suite) {
			// A store records t:<V> as its TIME minus the time the write
			// waited since the response arrived (on the Node clock), and a
			// refresh does the same; the resident time a lookup reports is
			// the shard's TIME minus t:<V>.
			f := s.fix("")
			tok := s.token("gen")
			if !s.store("store", statestore.CacheStore{
				Key: f.k, Names: "n", Variant: f.v, TTL: f.ttl, Entry: f.body, Token: 1,
				Generation: tok, Create: true, Received: s.now().Add(-2 * time.Second),
			}) {
				s.t.Fatal("store not applied")
			}
			if l := s.lookup("stored", f.k, f.v); l.Resident != 2*time.Second {
				s.t.Fatalf("resident %v after a 2 s receipt delay, want 2s", l.Resident)
			}
			s.advance(3 * time.Second)
			if l := s.lookup("later", f.k, f.v); l.Resident != 5*time.Second {
				s.t.Fatalf("resident %v 3 s later, want 5s", l.Resident)
			}
			if !s.leaseOp("take", statestore.CacheLease{Key: f.k, Mode: statestore.LeaseTake, Token: 7}) {
				s.t.Fatal("lease not taken")
			}
			if !s.leaseOp("refresh", statestore.CacheLease{
				Key: f.k, Variant: f.v, Mode: statestore.LeaseRefresh, Token: 7,
				Generation: tok, Entry: f.body, TTL: f.ttl, Received: s.now().Add(-1500 * time.Millisecond),
			}) {
				s.t.Fatal("refresh not applied")
			}
			if l := s.lookup("refreshed", f.k, f.v); l.Resident != 1500*time.Millisecond {
				s.t.Fatalf("resident %v after a refresh with a 1.5 s receipt delay, want 1.5s", l.Resident)
			}
		}},
		{name: "cache/skip-token", run: func(s *suite) {
			// A lookup with SkipToken keeps the token the Node's hot-entry
			// layer holds and still reads the partition.
			f := s.fix("")
			_, tok := s.put("store", f, "n", f.v, 1)
			held := s.token("held")
			c := lookupCall(f.k, f.v)
			c.Lookup.SkipToken, c.Lookup.Token, c.Lookup.HasToken = true, held, true
			s.read(c)
			s.answered("lookup", c)
			if l := &c.Lookup; l.Token != held || !l.HasToken || !l.Found || !bytes.Equal(l.Entry, f.body) {
				s.t.Fatalf("skip-token lookup: %+v, want the held token %x (the key holds %x)", *l, held, tok)
			}
		}},
		{name: "cache/revalidation-lease", run: func(s *suite) {
			// The revalidation lease lasts 5 s (SET NX PX 5000); a second
			// taker fails. CacheLease.TTL does not change it: the take
			// sends no TTL.
			s.timed(func(try int) (func(), bool) {
				f := s.fix(tryTag(try))
				take := func(token uint64) bool {
					return s.leaseOp("take", statestore.CacheLease{Key: f.k, Variant: f.v, Mode: statestore.LeaseTake, Token: token, TTL: time.Second})
				}
				start := s.serverNow()
				if !take(1) {
					s.t.Fatal("lease not taken")
				}
				twice := take(2)
				s.advance(4 * time.Second)
				lapsed := take(2)
				held := s.serverNow().Before(start.Add(revalidation))
				return func() {
					if twice {
						s.t.Fatal("held lease taken twice")
					}
					if lapsed {
						s.t.Fatal("lease lapsed before 5 s")
					}
					s.advance(time.Second + 10*time.Millisecond)
					if !take(2) {
						s.t.Fatal("lease not free after 5 s")
					}
				}, held
			})
		}},
		{name: "cache/refresh-and-end-stale", run: func(s *suite) {
			// Refresh (304 or replacement) and end-stale act only with the
			// lease token and while the generation key holds the entry's
			// token, all within the 5 s revalidation lease.
			s.timed(func(try int) (func(), bool) {
				f := s.fix(tryTag(try))
				_, tok := s.put("store", f, "n", f.v, 1)
				s.advance(fillLease)
				start := s.serverNow()
				taken := s.leaseOp("take", statestore.CacheLease{Key: f.k, Variant: f.v, Mode: statestore.LeaseTake, Token: 7})
				fresh := []byte("refreshed-" + s.nonce + tryTag(try))
				refresh := statestore.CacheLease{Key: f.k, Variant: f.v, Mode: statestore.LeaseRefresh, Token: 8, Generation: tok, Entry: fresh, TTL: f.ttl}
				wrongRefresh := s.leaseOp("refresh, wrong lease token", refresh)
				refresh.Token, refresh.Generation = 7, s.token("other")
				wrongGen := s.leaseOp("refresh, wrong generation token", refresh)
				unrefreshed := s.lookup("unrefreshed", f.k, f.v)
				refresh.Generation = tok
				refreshed := s.leaseOp("refresh", refresh)
				afterRefresh := s.lookup("refreshed", f.k, f.v)
				end := statestore.CacheLease{Key: f.k, Variant: f.v, Mode: statestore.LeaseEndStale, Token: 8, Generation: tok}
				wrongEnd := s.leaseOp("end, wrong lease token", end)
				end.Token, end.Generation = 7, s.token("other")
				wrongEndGen := s.leaseOp("end, wrong generation token", end)
				stillStored := s.lookup("still stored", f.k, f.v).Found
				end.Generation = tok
				ended := s.leaseOp("end", end)
				endedFound := s.lookup("ended", f.k, f.v).Found
				held := s.serverNow().Before(start.Add(revalidation))
				return func() {
					if !taken {
						s.t.Fatal("revalidation lease not taken")
					}
					if wrongRefresh || wrongGen {
						s.t.Fatalf("refresh applied without the lease token (%v) or the generation token (%v)", wrongRefresh, wrongGen)
					}
					if !bytes.Equal(unrefreshed.Entry, f.body) {
						s.t.Fatal("refused refresh changed the entry")
					}
					if !refreshed {
						s.t.Fatal("refresh with the lease and generation tokens not applied")
					}
					if !afterRefresh.Found || !bytes.Equal(afterRefresh.Entry, fresh) {
						s.t.Fatalf("refreshed entry %q, want %q", afterRefresh.Entry, fresh)
					}
					if wrongEnd || wrongEndGen {
						s.t.Fatalf("end-stale applied without the lease token (%v) or the generation token (%v)", wrongEnd, wrongEndGen)
					}
					if !stillStored {
						s.t.Fatal("refused end-stale deleted the variant")
					}
					if !ended {
						s.t.Fatal("end-stale with the lease and generation tokens not applied")
					}
					if endedFound {
						s.t.Fatal("end-stale kept the variant")
					}
				}, held
			})
		}},
		{name: "cache/lease-writes-after-bump", run: func(s *suite) {
			// After an invalidation a revalidation of the old entry
			// changes nothing: its refresh and end-stale name a token the
			// generation key no longer holds, so a variant stored under the
			// new token survives them.
			s.timed(func(try int) (func(), bool) {
				f := s.fix(tryTag(try))
				_, old := s.put("store", f, "n", f.v, 1)
				bump := s.token("bump")
				s.invalidate("bump", f.k.URI, bump)
				s.advance(fillLease)
				fresh := []byte("new-" + s.nonce + tryTag(try))
				if !s.store("store under the new token", statestore.CacheStore{Key: f.k, Names: "n", Variant: f.v, TTL: f.ttl, Entry: fresh, Token: 2, Generation: bump}) {
					s.t.Fatal("store under the new token not applied")
				}
				s.advance(fillLease)
				start := s.serverNow()
				taken := s.leaseOp("take", statestore.CacheLease{Key: f.k, Mode: statestore.LeaseTake, Token: 9})
				refreshed := s.leaseOp("refresh of the old entry", statestore.CacheLease{Key: f.k, Variant: f.v, Mode: statestore.LeaseRefresh, Token: 9, Generation: old, Entry: f.body, TTL: f.ttl})
				ended := s.leaseOp("end of the old entry", statestore.CacheLease{Key: f.k, Variant: f.v, Mode: statestore.LeaseEndStale, Token: 9, Generation: old})
				l := s.lookup("after", f.k, f.v)
				held := s.serverNow().Before(start.Add(revalidation))
				return func() {
					if !taken || refreshed || ended {
						s.t.Fatalf("take %v, refresh %v, end %v; want the take only", taken, refreshed, ended)
					}
					if !l.Found || !bytes.Equal(l.Entry, fresh) {
						s.t.Fatalf("variant %q after the old entry's lease writes, want %q", l.Entry, fresh)
					}
				}, held
			})
		}},
		{name: "cache/ttls", skip: needInspect, run: func(s *suite) {
			// Partition: the entry TTL (at most 25 h); generation 26 h
			// from its creation, a bump and every store under its token;
			// fill lease 1 s; revalidation lease 5 s. Each lease is read
			// before it ends.
			var f cacheFixture
			var tok statestore.Token
			s.timed(func(try int) (func(), bool) {
				f = s.fix(tryTag(try))
				f.ttl = 30 * time.Hour
				ref := s.serverNow()
				_, tok = s.put("store", f, "n", f.v, 1)
				part, partOK := s.inspect(Key{Kind: KeyCachePartition, Cache: f.k})
				lease, leaseOK := s.inspect(Key{Kind: KeyCacheLease, Cache: f.k})
				gen, genOK := s.inspect(Key{Kind: KeyCacheGeneration, Cache: f.k})
				held := s.serverNow().Before(ref.Add(time.Second))
				return func() {
					if !partOK || part.Value != "n" {
						s.t.Fatalf("partition %+v, %v", part, partOK)
					}
					s.ttlNear("partition capped at 25 h", part.TTL, 25*time.Hour, ref)
					if !leaseOK || lease.Value != "1" {
						s.t.Fatalf("fill lease %+v, %v", lease, leaseOK)
					}
					s.ttlNear("fill lease", lease.TTL, time.Second, ref)
					if !genOK || gen.Value != tokenText(tok) {
						s.t.Fatalf("generation key %+v, %v; want the token %s", gen, genOK, tokenText(tok))
					}
					s.ttlNear("generation created by a store", gen.TTL, 26*time.Hour, ref)
				}, held
			})
			if s.h.Exact {
				// A later store under the token renews the 26 h.
				s.advance(time.Hour)
				ref := s.serverNow()
				f2 := f
				if ok, _ := s.put("store renewing the generation", f2, "n", s.digest("v-renew"), 2); !ok {
					s.t.Fatal("second store not applied")
				}
				gen, _ := s.inspect(Key{Kind: KeyCacheGeneration, Cache: f.k})
				s.ttlNear("generation renewed by a store", gen.TTL, 26*time.Hour, ref)
			}
			ref := s.serverNow()
			bump := s.token("bump")
			s.invalidate("bump", f.k.URI, bump)
			gen, ok := s.inspect(Key{Kind: KeyCacheGeneration, Cache: f.k})
			if !ok || gen.Value != tokenText(bump) {
				s.t.Fatalf("generation key after the bump %+v, %v; want %s", gen, ok, tokenText(bump))
			}
			s.ttlNear("generation", gen.TTL, 26*time.Hour, ref)
			s.advance(fillLease)
			s.timed(func(try int) (func(), bool) {
				k := f.k
				if try > 0 {
					k = s.fix(tryTag(timedAttempts + try)).k
				}
				ref := s.serverNow()
				taken := s.leaseOp("take", statestore.CacheLease{Key: k, Variant: f.v, Mode: statestore.LeaseTake, Token: 9})
				lease, leaseOK := s.inspect(Key{Kind: KeyCacheLease, Cache: k})
				held := s.serverNow().Before(ref.Add(revalidation))
				return func() {
					if !taken || !leaseOK {
						s.t.Fatalf("revalidation lease taken %v, read %v", taken, leaseOK)
					}
					s.ttlNear("revalidation lease", lease.TTL, revalidation, ref)
				}, held
			})
		}},
		{name: "cache/mac-tamper", opts: OpenOptions{MACKey: macKey}, skip: func(h Harness) string {
			if h.Tamper == nil {
				return "harness cannot tamper with entries"
			}
			return ""
		}, run: func(s *suite) {
			// With a MAC key, a tampered entry or a missing tag is a miss,
			// never an error.
			f := s.fix("")
			put := func(token uint64) {
				s.advance(fillLease)
				if ok, _ := s.put("store", f, "n", f.v, token); !ok {
					s.t.Fatal("store not applied")
				}
				if l := s.lookup("intact", f.k, f.v); !l.Found || !bytes.Equal(l.Entry, f.body) {
					s.t.Fatalf("intact tagged entry: %+v", *l)
				}
			}
			put(1)
			s.h.Tamper(s.t, s.d, f.k, f.v, func(b []byte) []byte {
				if len(b) < 2 || b[0] != keys.MarkTagged {
					s.t.Fatalf("stored entry %x not marked tagged", b)
				}
				b = bytes.Clone(b)
				b[1] ^= 0xff
				return b
			})
			if l := s.lookup("tampered", f.k, f.v); l.Found {
				s.t.Fatal("tampered entry served")
			}
			put(2)
			s.h.Tamper(s.t, s.d, f.k, f.v, func([]byte) []byte { return append([]byte{keys.MarkUntagged}, f.body...) })
			if l := s.lookup("untagged", f.k, f.v); l.Found {
				s.t.Fatal("entry without a tag served")
			}
			put(3)
			s.h.Tamper(s.t, s.d, f.k, f.v, func(b []byte) []byte {
				b = bytes.Clone(b)
				b[0] = keys.MarkUntagged
				return b
			})
			if l := s.lookup("relabeled untagged", f.k, f.v); l.Found {
				s.t.Fatal("tagged entry relabeled untagged served")
			}
		}},
		{name: "cache/no-mac-key", run: func(s *suite) {
			// Without a key entries are stored and read untagged.
			f := s.fix("")
			s.put("store", f, "n", f.v, 1)
			if l := s.lookup("untagged", f.k, f.v); !l.Found || !bytes.Equal(l.Entry, f.body) {
				s.t.Fatalf("untagged entry: %+v", *l)
			}
		}},
		{name: "cache/mac-mark-without-key", skip: func(h Harness) string {
			if h.Tamper == nil {
				return "harness cannot tamper with entries"
			}
			return ""
		}, run: func(s *suite) {
			// Every stored entry records whether it carries a tag, so a
			// Node without the key reads an entry marked tagged as a miss
			// (Security, "Secrets" rule 9), as it does an unknown mark.
			f := s.fix("")
			s.put("store", f, "n", f.v, 1)
			for _, c := range []struct {
				name string
				edit func([]byte) []byte
			}{
				{"marked tagged", func(b []byte) []byte {
					b = append([]byte{keys.MarkTagged}, b[1:]...)
					return append(b, make([]byte, keys.TagSize)...)
				}},
				{"unknown mark", func(b []byte) []byte { return append([]byte{2}, b[1:]...) }},
				{"empty", func([]byte) []byte { return []byte{} }},
			} {
				s.h.Tamper(s.t, s.d, f.k, f.v, func(b []byte) []byte {
					if len(b) == 0 || b[0] != keys.MarkUntagged {
						s.t.Fatalf("%s: stored entry %x not marked untagged", c.name, b)
					}
					return c.edit(bytes.Clone(b))
				})
				if l := s.lookup(c.name, f.k, f.v); l.Found {
					s.t.Fatalf("%s: entry served by a Node without the key", c.name)
				}
				s.h.Tamper(s.t, s.d, f.k, f.v, func([]byte) []byte { return append([]byte{keys.MarkUntagged}, f.body...) })
				if l := s.lookup(c.name+": restored", f.k, f.v); !l.Found || !bytes.Equal(l.Entry, f.body) {
					s.t.Fatalf("%s: restored untagged entry: %+v", c.name, *l)
				}
			}
		}},
		{name: "cache/mac-mixed-nodes", opts: OpenOptions{MACKey: macKey}, skip: func(h Harness) string {
			if h.OpenPeer == nil {
				return "harness opens no second driver on one store"
			}
			return ""
		}, run: func(s *suite) {
			// A Cell's keyed and key-less Nodes on one State Store never
			// serve each other's entries, in either direction, while
			// Nodes holding the same key share theirs.
			keyless := s.peer(OpenOptions{})
			sameKey := s.peer(OpenOptions{MACKey: macKey})
			f := s.fix("keyed")
			if ok, _ := s.put("keyed store", f, "n", f.v, 1); !ok {
				s.t.Fatal("keyed store not applied")
			}
			if l := sameKey.lookup("same key", f.k, f.v); !l.Found || !bytes.Equal(l.Entry, f.body) {
				s.t.Fatalf("a Node with the same key misses the entry: %+v", *l)
			}
			if l := keyless.lookup("key-less reads tagged", f.k, f.v); l.Found {
				s.t.Fatal("a key-less Node served a keyed Node's entry")
			}
			g := s.fix("keyless")
			if ok, _ := keyless.put("key-less store", g, "n", g.v, 1); !ok {
				s.t.Fatal("key-less store not applied")
			}
			if l := keyless.lookup("key-less reads its own", g.k, g.v); !l.Found || !bytes.Equal(l.Entry, g.body) {
				s.t.Fatalf("a key-less Node misses its own entry: %+v", *l)
			}
			for _, keyed := range []*suite{s, sameKey} {
				if l := keyed.lookup("keyed reads untagged", g.k, g.v); l.Found {
					s.t.Fatal("a keyed Node served a key-less Node's entry")
				}
			}
		}},
		{name: "read/pipeline", run: func(s *suite) {
			// Independent lookups share one pipelined batch.
			f := s.fix("")
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

// tokenText is a generation key's value for t: 32 lowercase hex
// characters.
func tokenText(t statestore.Token) string { return hex.EncodeToString(t[:]) }
