// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package basic

import (
	"context"
	"encoding/hex"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// drain makes UserBurst failed attempts of username, each from its own
// source, leaving the username's bucket empty.
func (fx *fixture) drain(t *testing.T, username string) {
	t.Helper()
	for i := range UserBurst {
		x := login(srcIP(i), username, "wrong")
		wantInvalid(t, fx.handle(x), x)
	}
}

// TestSuccessCacheHit checks "A hit skips throttle and hash": a verified
// pair authenticates again without a hash, even from a source whose
// failure bucket is empty while the hash queue is full.
func TestSuccessCacheHit(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("alice", "pw", 0)))
	x := login("192.0.2.1", "alice", "pw")
	wantAllow(t, fx.handle(x), x, "acme")
	for i := range SourceFailures {
		x := login("192.0.2.1", "guess-"+strconv.Itoa(i), "x")
		wantInvalid(t, fx.handle(x), x)
	}
	hashes := fx.hashes.count()
	fx.throttle.slots <- struct{}{}
	fx.throttle.queued.Store(fx.throttle.maxQueued)
	x = login("192.0.2.1", "alice", "pw")
	wantAllow(t, fx.handle(x), x, "acme")
	if fx.hashes.count() != hashes || fx.refused() != [NumRefusalCauses]uint64{} {
		t.Fatalf("the hit hashed or was refused: %d hashes, refusals %+v", fx.hashes.count()-hashes, fx.refused())
	}
	fx.throttle.queued.Store(0)
	<-fx.throttle.slots
}

// TestSuccessCacheIdle checks "An entry lasts 60 s after its last hit":
// each hit within CacheIdle extends the entry, and the first request
// CacheIdle after the last hit is verified again.
func TestSuccessCacheIdle(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("alice", "pw", 0)))
	x := login("192.0.2.1", "alice", "pw")
	wantAllow(t, fx.handle(x), x, "acme")
	for range 3 {
		fx.clock.Advance(CacheIdle - time.Nanosecond)
		x := login("192.0.2.1", "alice", "pw")
		wantAllow(t, fx.handle(x), x, "acme")
	}
	if fx.hashes.count() != 1 {
		t.Fatalf("hashes = %d within the idle bound, want 1", fx.hashes.count())
	}
	fx.clock.Advance(CacheIdle)
	x = login("192.0.2.1", "alice", "pw")
	wantAllow(t, fx.handle(x), x, "acme")
	if fx.hashes.count() != 2 {
		t.Fatalf("hashes = %d after an idle minute, want 2", fx.hashes.count())
	}
}

// TestSuccessCacheMaxAge checks "at most 15 minutes from verification,
// less up to 20% jitter": an entry hit every 30 s serves until its
// maximum age and is verified again at it.
func TestSuccessCacheMaxAge(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("alice", "pw", 0)))
	age := maxAge(fx.throttle.mac([]byte("alice:pw")))
	x := login("192.0.2.1", "alice", "pw")
	wantAllow(t, fx.handle(x), x, "acme")
	for fx.clock.Now().Add(CacheIdle / 2).Before(t0.Add(age)) {
		fx.clock.Advance(CacheIdle / 2)
		x := login("192.0.2.1", "alice", "pw")
		wantAllow(t, fx.handle(x), x, "acme")
	}
	fx.clock.Set(t0.Add(age - time.Nanosecond))
	x = login("192.0.2.1", "alice", "pw")
	wantAllow(t, fx.handle(x), x, "acme")
	if fx.hashes.count() != 1 {
		t.Fatalf("hashes = %d before the maximum age, want 1", fx.hashes.count())
	}
	fx.clock.Set(t0.Add(age))
	x = login("192.0.2.1", "alice", "pw")
	wantAllow(t, fx.handle(x), x, "acme")
	if fx.hashes.count() != 2 {
		t.Fatalf("hashes = %d at the maximum age, want 2", fx.hashes.count())
	}
}

// TestMaxAgeJitter checks the jitter bounds: every entry's maximum age is
// more than CacheMaxAge less CacheJitterPercent and at most CacheMaxAge,
// and entries spread over that range.
func TestMaxAgeJitter(t *testing.T) {
	fx := newFixture(t)
	floor := CacheMaxAge - CacheMaxAge/100*CacheJitterPercent
	lo, hi := CacheMaxAge, time.Duration(0)
	for i := range 10_000 {
		a := maxAge(fx.throttle.mac([]byte("user-" + strconv.Itoa(i) + ":pw")))
		if a <= floor || a > CacheMaxAge {
			t.Fatalf("maxAge = %v, outside (%v, %v]", a, floor, CacheMaxAge)
		}
		lo, hi = min(lo, a), max(hi, a)
	}
	if lo > floor+5*time.Second || hi < CacheMaxAge-5*time.Second {
		t.Fatalf("maxAge spans [%v, %v], want close to (%v, %v]", lo, hi, floor, CacheMaxAge)
	}
	var top [32]byte
	copy(top[8:16], strings.Repeat("\xff", 8))
	if maxAge([32]byte{}) != CacheMaxAge || maxAge(top) <= floor {
		t.Fatalf("extremes: %v, %v", maxAge([32]byte{}), maxAge(top))
	}
}

// TestSnapshotSwapEvictsChangedOnly checks "A snapshot swap evicts only
// entries of changed or removed credentials": after a swap an unchanged
// credential still hits, a changed one is verified against its new hash,
// and a removed username is refused after a dummy hash, its entry gone.
func TestSnapshotSwapEvictsChangedOnly(t *testing.T) {
	bob := credential("bob", "b1", 0)
	fx := newFixture(t, consumer("acme", credential("alice", "a1", 0)), consumer("globex", bob))
	for _, p := range [][2]string{{"alice", "a1"}, {"bob", "b1"}} {
		x := login("192.0.2.1", p[0], p[1])
		r := fx.handle(x)
		if r.Outcome != filter.Continue {
			t.Fatalf("%s: %+v", p[0], r)
		}
	}
	next := fx.build(t, fx.f, consumer("acme", credential("alice", "a2", 0)), consumer("globex", bob))
	run := func(f *Filter, user, pw string) (*filtertest.Exchange, filter.Result) {
		x := login("192.0.2.1", user, pw)
		return x, f.Handle(t.Context(), phase.OnRequestHeaders, x)
	}
	x, r := run(next, "bob", "b1")
	wantAllow(t, r, x, "globex")
	if fx.hashes.count() != 2 {
		t.Fatal("the unchanged credential was verified again after the swap")
	}
	x, r = run(next, "alice", "a1")
	wantInvalid(t, r, x)
	x, r = run(next, "alice", "a2")
	wantAllow(t, r, x, "acme")
	if fx.hashes.count() != 4 {
		t.Fatalf("hashes = %d, want the changed credential verified twice", fx.hashes.count())
	}

	last := fx.build(t, next, consumer("acme", credential("alice", "a2", 0)))
	x, r = run(last, "bob", "b1")
	wantInvalid(t, r, x)
	if fx.hashes.last().iterations != identity.DefaultIterations {
		t.Fatal("the removed username was not given the dummy hash")
	}
	if n := fx.throttle.cache.len(); n != 1 {
		t.Fatalf("cache entries = %d, want only alice's current pair", n)
	}
}

// TestSourceKey checks the source key: at most SourceFailures failed
// verifications per SourceWindow per source.ip, an IPv4-mapped address
// counted as IPv4 and an IPv6 address as its /64; an empty bucket refuses
// every uncached attempt with 429 RZ-AUTH-007 before any hash, while a
// success cache hit from it passes and other sources are unaffected.
func TestSourceKey(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("erin", "right", 0)))
	x := login("198.51.100.1", "erin", "right")
	wantAllow(t, fx.handle(x), x, "acme")
	// Each failure uses a new username, so only the source key limits.
	guesses := 0
	fail := func(src string, n int) {
		t.Helper()
		for range n {
			guesses++
			x := login(src, "guess-"+strconv.Itoa(guesses), "x")
			wantInvalid(t, fx.handle(x), x)
		}
	}
	fail("192.0.2.1", SourceFailures)
	hashes := fx.hashes.count()
	for _, src := range []string{"192.0.2.1", "::ffff:192.0.2.1"} {
		x := login(src, "guess-more", "x")
		wantThrottled(t, fx.handle(x), x, "6")
	}
	if fx.hashes.count() != hashes || fx.refused()[RefusalSource] != 2 {
		t.Fatalf("refused attempts hashed (%d) or were not counted (%+v)", fx.hashes.count()-hashes, fx.refused())
	}
	if _, ok := fx.userTAT("guess-more"); ok {
		t.Fatal("a refused attempt created a username bucket")
	}
	fail("192.0.2.2", 1)
	x = login("192.0.2.1", "erin", "right")
	wantAllow(t, fx.handle(x), x, "acme")

	fail("2001:db8::1", SourceFailures)
	x = login("2001:db8::ffff:1", "guess-v6", "x")
	wantThrottled(t, fx.handle(x), x, "6")
	fail("2001:db8:0:1::1", 1)

	// One failure refills every SourceWindow / SourceFailures.
	fx.clock.Advance(SourceWindow / SourceFailures)
	fail("192.0.2.1", 1)
	x = login("192.0.2.1", "guess-again", "x")
	wantThrottled(t, fx.handle(x), x, "6")
}

// TestSourceCountsOnlyFailures checks that the source key counts failed
// verifications: successful ones from the same source never empty it.
func TestSourceCountsOnlyFailures(t *testing.T) {
	var cs []*v1alpha1.Consumer
	for i := range 2 * SourceFailures {
		cs = append(cs, consumer("c"+strconv.Itoa(i), credential("ok-"+strconv.Itoa(i), "pw", 0)))
	}
	fx := newFixture(t, cs...)
	for i := range 2 * SourceFailures {
		x := login("192.0.2.1", "ok-"+strconv.Itoa(i), "pw")
		wantAllow(t, fx.handle(x), x, "c"+strconv.Itoa(i))
	}
	x := login("192.0.2.1", "ok-0", "wrong")
	wantInvalid(t, fx.handle(x), x)
}

// TestUsernameBucket checks "burst 10, refill 1 per second": UserBurst
// uncached attempts of one username pass at once, from any sources, and
// one more passes without waiting once a token refilled.
func TestUsernameBucket(t *testing.T) {
	fx := newFixture(t)
	fx.drain(t, "frank")
	if tat, _ := fx.userTAT("frank"); !tat.Equal(t0.Add(UserBurst * UserRefill)) {
		t.Fatalf("bucket at %v after the burst", tat)
	}
	fx.clock.Advance(UserRefill)
	x := login("192.0.2.200", "frank", "wrong")
	wantInvalid(t, result(t, fx.async(t.Context(), x)), x)
	if fx.clock.Pending() != 0 {
		t.Fatal("an attempt with a refilled token waited")
	}
}

// TestDelayWaiters checks "An empty bucket holds at most 2 waiters ... for
// up to 2 s": the next two attempts of an empty bucket wait for the tokens
// refilling after 1 s and 2 s and are then verified, and a third is
// refused at once with Retry-After the time its token would take.
func TestDelayWaiters(t *testing.T) {
	fx := newFixture(t)
	fx.drain(t, "dave")
	first, second := login("192.0.2.100", "dave", "wrong"), login("192.0.2.101", "dave", "wrong")
	r1 := fx.async(t.Context(), first)
	waitFor(t, "the first waiter", func() bool { return fx.clock.Pending() == 1 })
	r2 := fx.async(t.Context(), second)
	waitFor(t, "the second waiter", func() bool { return fx.clock.Pending() == 2 })

	x := login("192.0.2.102", "dave", "wrong")
	wantThrottled(t, fx.handle(x), x, "3")
	if fx.refused()[RefusalDelay] != 1 || fx.hashes.count() != UserBurst {
		t.Fatalf("refusals %+v, hashes %d", fx.refused(), fx.hashes.count())
	}
	fx.clock.Advance(time.Second)
	wantInvalid(t, result(t, r1), first)
	fx.clock.Advance(time.Second)
	wantInvalid(t, result(t, r2), second)
	if fx.hashes.count() != UserBurst+2 || fx.throttle.delayed.Load() != 0 {
		t.Fatalf("hashes %d, delayed %d", fx.hashes.count(), fx.throttle.delayed.Load())
	}
}

// TestDelayNodeCap checks "1,024 per Node": with DelayWaiters attempts
// already waiting, an attempt of another empty bucket is refused at once.
func TestDelayNodeCap(t *testing.T) {
	fx := newFixture(t)
	fx.drain(t, "dave")
	fx.throttle.delayed.Store(DelayWaiters)
	x := login("192.0.2.100", "dave", "wrong")
	wantThrottled(t, fx.handle(x), x, "1")
	if fx.clock.Pending() != 0 || fx.refused()[RefusalDelay] != 1 || fx.throttle.delayed.Load() != DelayWaiters {
		t.Fatalf("pending %d, refusals %+v, delayed %d", fx.clock.Pending(), fx.refused(), fx.throttle.delayed.Load())
	}
	fx.throttle.delayed.Store(0)
}

// TestAbandonedWaitReturnsTokens checks that a request ending while it
// waits for a token cannot decide (the auth class default, 401
// RZ-AUTH-002) and returns the username and source tokens it took,
// without a hash.
func TestAbandonedWaitReturnsTokens(t *testing.T) {
	fx := newFixture(t)
	fx.drain(t, "dave")
	before, _ := fx.userTAT("dave")
	ctx, cancel := context.WithCancel(t.Context())
	x := login("192.0.2.100", "dave", "wrong")
	ch := fx.async(ctx, x)
	waitFor(t, "the waiter", func() bool { return fx.clock.Pending() == 1 })
	cancel()
	r := result(t, ch)
	if r.Outcome != filter.CannotDecide || r.Code != auth.CodeInvalid || !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("abandoned attempt = %+v", r)
	}
	if after, _ := fx.userTAT("dave"); !after.Equal(before) {
		t.Fatalf("username bucket at %v, want %v: the reserved token was kept", after, before)
	}
	if fx.hashes.count() != UserBurst || fx.throttle.delayed.Load() != 0 {
		t.Fatalf("hashes %d, delayed %d", fx.hashes.count(), fx.throttle.delayed.Load())
	}
	for range SourceFailures {
		x := login("192.0.2.100", "other-"+strconv.Itoa(fx.hashes.count()), "x")
		wantInvalid(t, fx.handle(x), x)
	}
}

// TestBucketCreationCap checks "A bucket is created only after the hash
// queue admits an attempt, at most 100 per second per Node": past the cap
// a new username is refused before its hash, an existing username still
// passes, and the cap refills one creation per 10 ms.
func TestBucketCreationCap(t *testing.T) {
	fx := newFixture(t)
	for i := range CreateBurst {
		x := login(srcIP(i), "user-"+strconv.Itoa(i), "x")
		wantInvalid(t, fx.handle(x), x)
	}
	x := login(srcIP(1000), "user-new", "x")
	wantThrottled(t, fx.handle(x), x, "1")
	if fx.refused()[RefusalCreate] != 1 || fx.hashes.count() != CreateBurst || fx.throttle.users.len() != CreateBurst {
		t.Fatalf("refusals %+v, hashes %d, buckets %d", fx.refused(), fx.hashes.count(), fx.throttle.users.len())
	}
	x = login(srcIP(1001), "user-0", "x")
	wantInvalid(t, fx.handle(x), x)
	fx.clock.Advance(time.Second / CreatesPerSecond)
	x = login(srcIP(1002), "user-new", "x")
	wantInvalid(t, fx.handle(x), x)
	if fx.throttle.users.len() != CreateBurst+1 {
		t.Fatalf("buckets = %d", fx.throttle.users.len())
	}
}

// TestHashQueue checks "max(1, GOMAXPROCS/4) slots ...; 4 × slots
// waiters, 1 s deadline": with one slot busy, four attempts wait, a fifth
// is refused at once, the waiters are refused when the deadline passes,
// and no refused attempt hashes or creates a username bucket.
func TestHashQueue(t *testing.T) {
	var cs []*v1alpha1.Consumer
	for i := range QueueWaitersPerSlot + 2 {
		cs = append(cs, consumer("c"+strconv.Itoa(i), credential("user-"+strconv.Itoa(i), "pw", 0)))
	}
	fx := newFixture(t, cs...)
	gate := make(chan struct{})
	fx.hashes.gate = gate
	holder := login("192.0.2.1", "user-0", "pw")
	held := fx.async(t.Context(), holder)
	waitFor(t, "the first hash", func() bool { return fx.hashes.count() == 1 })

	waiters := make([]*filtertest.Exchange, QueueWaitersPerSlot)
	results := make([]<-chan filter.Result, QueueWaitersPerSlot)
	for i := range waiters {
		waiters[i] = login("192.0.2.1", "user-"+strconv.Itoa(i+1), "pw")
		results[i] = fx.async(t.Context(), waiters[i])
	}
	waitFor(t, "the queued attempts", func() bool { return fx.clock.Pending() == QueueWaitersPerSlot })

	x := login("192.0.2.1", "user-"+strconv.Itoa(QueueWaitersPerSlot+1), "pw")
	wantThrottled(t, fx.handle(x), x, "1")
	fx.clock.Advance(QueueDeadline)
	for i, ch := range results {
		wantThrottled(t, result(t, ch), waiters[i], "1")
	}
	gate <- struct{}{}
	wantAllow(t, result(t, held), holder, "c0")
	if fx.hashes.count() != 1 || fx.refused()[RefusalQueue] != QueueWaitersPerSlot+1 || fx.throttle.users.len() != 1 {
		t.Fatalf("hashes %d, refusals %+v, buckets %d", fx.hashes.count(), fx.refused(), fx.throttle.users.len())
	}
	if fx.throttle.queued.Load() != 0 || len(fx.throttle.slots) != 0 {
		t.Fatal("the queue did not empty")
	}
}

// TestSlots checks the hash queue width: max(1, GOMAXPROCS/4) slots, so
// hashing uses at most a quarter of the cores, or one core below four.
func TestSlots(t *testing.T) {
	for procs, want := range map[int]int{0: 1, 1: 1, 3: 1, 4: 1, 7: 1, 8: 2, 16: 4, 64: 16} {
		if got := slotsFor(procs); got != want {
			t.Errorf("slotsFor(%d) = %d, want %d", procs, got, want)
		}
	}
	th, err := NewThrottle(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if cap(th.slots) != slotsFor(runtime.GOMAXPROCS(0)) || th.maxQueued != int64(QueueWaitersPerSlot*cap(th.slots)) {
		t.Fatalf("slots %d, waiters %d", cap(th.slots), th.maxQueued)
	}
}

// TestRealPBKDF2 checks the derivation against a credential produced by
// crypto/pbkdf2 at the default 600,000 iterations, and that it is
// PBKDF2-HMAC-SHA-256 (RFC 7914 section 11 test vector).
func TestRealPBKDF2(t *testing.T) {
	got, err := derive("passwd", []byte("salt"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := "55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc"; hex.EncodeToString(got) != want {
		t.Fatalf("derive = %x, want %s", got, want)
	}
	th, err := newThrottle(Options{Clock: clocktest.New(t0)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	fac, err := NewFactory(th, nil)
	if err != nil {
		t.Fatal(err)
	}
	cred := v1alpha1.BasicCredential{Username: "acme-batch", Hash: "pbkdf2-sha256:cnVyYWx6LXRlc3Qtc2FsdA:TjyQ0sKP8hANb2y2K7IOlIsqb2ECTHhGLF_GTWbXKzc"}
	flt, err := fac.Build(t.Context(), buildEnv(consumer("acme", cred)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = flt.(*Filter).Close() }()
	x := login("192.0.2.1", "acme-batch", "correct horse battery staple")
	wantAllow(t, flt.Handle(t.Context(), phase.OnRequestHeaders, x), x, "acme")
	x = login("192.0.2.1", "acme-batch", "Correct horse battery staple")
	wantInvalid(t, flt.Handle(t.Context(), phase.OnRequestHeaders, x), x)
}

// TestThrottleComponent checks the filter.Component contract: the name,
// and Run returning nil once its context is done.
func TestThrottleComponent(t *testing.T) {
	fx := newFixture(t)
	var c filter.Component = fx.throttle
	if c.Name() != ComponentName {
		t.Fatalf("Name = %q", c.Name())
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
}

// TestThrottleKey checks that the per-process key comes from Options.Rand
// and that a reader failure fails construction; two throttles with
// different keys key the same username differently.
func TestThrottleKey(t *testing.T) {
	if _, err := newThrottle(Options{Rand: iotest.ErrReader(errors.New("no entropy"))}, 1); err == nil {
		t.Fatal("a failing key reader was accepted")
	}
	a, err := newThrottle(Options{Rand: strings.NewReader(strings.Repeat("a", 32))}, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newThrottle(Options{Rand: strings.NewReader(strings.Repeat("b", 32))}, 1)
	if err != nil {
		t.Fatal(err)
	}
	first, again, other := a.mac([]byte("alice")), a.mac([]byte("alice")), b.mac([]byte("alice"))
	if first != again || first == other {
		t.Fatal("keys are not per process")
	}
}

// TestRefusalsCounted checks that ruralz_auth_basic_refusals_total counts
// every refusal by cause on the refused request's stripe, and nothing
// else: neither a verification, a failed one nor a success cache hit.
func TestRefusalsCounted(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("alice", "pw", 0)))
	x := login("192.0.2.1", "alice", "pw")
	wantAllow(t, fx.handle(x), x, "acme")
	for i := range SourceFailures {
		x := login("192.0.2.1", "guess-"+strconv.Itoa(i), "x")
		wantInvalid(t, fx.handle(x), x)
	}
	x = login("192.0.2.1", "alice", "pw")
	wantAllow(t, fx.handle(x), x, "acme")
	if got := fx.refused(); got != [NumRefusalCauses]uint64{} {
		t.Fatalf("refusals %v without a refusal", got)
	}
	x = login("192.0.2.1", "guess-more", "x")
	x.StripeV = 5
	wantThrottled(t, fx.handle(x), x, "6")
	want := [NumRefusalCauses]uint64{RefusalSource: 1}
	if got := fx.refused(); got != want || fx.refusals[RefusalSource].stripe != 5 {
		t.Fatalf("refusals %v on stripe %d, want %v on stripe 5", got, fx.refusals[RefusalSource].stripe, want)
	}
	if fx.decisions.deny[auth.CodeThrottled] != 1 {
		t.Fatalf("decisions %v, want one RZ-AUTH-007 deny", fx.decisions.deny)
	}
}

// TestRefusalCounters checks the wiring of the metric: RefusalCounters
// asks the registry for ruralz_auth_basic_refusals_total once per cause,
// in catalog label order, so that Refusal* indexes the label value of the
// same name, and returns a lookup failure.
func TestRefusalCounters(t *testing.T) {
	names := map[int]string{RefusalQueue: "queue", RefusalCreate: "create", RefusalDelay: "delay", RefusalSource: "source"}
	f, ok := catalog.Lookup(catalog.AuthBasicRefusalsTotal)
	if !ok || f.Kind != catalog.Counter || len(f.Labels) != 1 || f.Labels[0].Name != "cause" || len(f.Labels[0].Values) != NumRefusalCauses {
		t.Fatalf("catalog family %+v", f)
	}
	for i, v := range f.Labels[0].Values {
		if names[i] != v {
			t.Fatalf("cause %d is %q in the catalog, %q here", i, v, names[i])
		}
	}
	byCause := map[string]*counter{}
	got, err := RefusalCounters(func(name string, values ...string) (emit.Counter, error) {
		if name != catalog.AuthBasicRefusalsTotal || len(values) != 1 {
			t.Fatalf("lookup of %s %v", name, values)
		}
		c := &counter{}
		byCause[values[0]] = c
		return c, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		if got[i] != emit.Counter(byCause[name]) {
			t.Errorf("Refusals[%d] is not the %s handle", i, name)
		}
	}
	fail := errors.New("unknown family")
	if _, err := RefusalCounters(func(string, ...string) (emit.Counter, error) { return nil, fail }); !errors.Is(err, fail) {
		t.Fatalf("lookup failure = %v", err)
	}
}

// TestUnwiredRefusals checks that a throttle built without refusal
// handles records into no-op ones instead of failing on a refusal.
func TestUnwiredRefusals(t *testing.T) {
	th, err := newThrottle(Options{Clock: clocktest.New(t0)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range th.refusals {
		if c == nil {
			t.Fatalf("Refusals[%d] is nil", i)
		}
		c.Add(0, 1)
	}
}
