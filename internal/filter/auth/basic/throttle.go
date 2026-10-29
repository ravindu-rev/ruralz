// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package basic

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"math/bits"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// ComponentName is the Throttle's filter.Component name.
const ComponentName = "auth.basic throttle"

// Success cache (Security and identity, "Pre-authentication throttling").
const (
	// CacheEntries is the success cache capacity per Node (target).
	CacheEntries = 10_000
	// CacheIdle is how long an entry lasts after its last hit (target).
	CacheIdle = 60 * time.Second
	// CacheMaxAge is the longest an entry lasts from its verification,
	// before jitter (target).
	CacheMaxAge = 15 * time.Minute
	// CacheJitterPercent is the largest share of CacheMaxAge an entry's
	// jitter takes off (target).
	CacheJitterPercent = 20
)

// Username buckets (target).
const (
	// UserBuckets is the capacity of the username bucket LRU per Node.
	UserBuckets = 100_000
	// UserBurst is the tokens of a username bucket.
	UserBurst = 10
	// UserRefill is the time one username token takes to refill.
	UserRefill = time.Second
	// CreatesPerSecond caps the username buckets a Node creates per
	// second.
	CreatesPerSecond = 100
	// CreateBurst is the username buckets the Node may create at once
	// before CreatesPerSecond paces creations: one second's worth.
	CreateBurst = 100
)

// Hash queue and delay (target).
const (
	// QueueWaitersPerSlot is the hash-queue waiters per slot.
	QueueWaitersPerSlot = 4
	// QueueDeadline is the longest an attempt waits for a hash slot.
	QueueDeadline = time.Second
	// DelayWaitersPerBucket is the attempts an empty username bucket holds
	// waiting for a token.
	DelayWaitersPerBucket = 2
	// DelayWaiters is the attempts the Node holds waiting for a username
	// token.
	DelayWaiters = 1_024
	// MaxDelay is the longest an attempt waits for a username token.
	MaxDelay = 2 * time.Second
)

// Source key: failures per source.ip, an IPv6 address counted as its /64
// (target).
const (
	// SourceFailures is the failed verifications a source may make per
	// SourceWindow.
	SourceFailures = 10
	// SourceWindow is the window of SourceFailures.
	SourceWindow = time.Minute
	// SourcePrefixBits is the IPv6 prefix length one source key covers.
	SourcePrefixBits = 64
	// SourceBuckets is the capacity of the source bucket LRU per Node, in
	// Shards shards like the username buckets. A source whose bucket was
	// evicted starts again with a full bucket.
	SourceBuckets = 100_000
)

// Refusal causes: the indexes of Options.Refusals, in the label order of
// ruralz_auth_basic_refusals_total (catalog.BasicRefusalCauses). Every
// refusal is a 429 RZ-AUTH-007 before any hash.
const (
	// RefusalQueue: the hash queue was full, or no slot freed within
	// QueueDeadline.
	RefusalQueue = iota
	// RefusalCreate: the bucket-creation cap refused a new username's
	// bucket.
	RefusalCreate
	// RefusalDelay: the username's bucket was empty and could hold no
	// further waiter within MaxDelay.
	RefusalDelay
	// RefusalSource: the source's failure bucket was empty.
	RefusalSource
	// NumRefusalCauses is the number of causes.
	NumRefusalCauses
)

// Verdicts of an authentication attempt.
type verdict uint8

const (
	// verdictValid: the password matches the credential.
	verdictValid verdict = iota + 1
	// verdictInvalid: a wrong password, or a username without a usable
	// credential (after the dummy hash).
	verdictInvalid
	// verdictThrottled: refused before any hash.
	verdictThrottled
	// verdictAborted: the request ended while waiting, or the hash failed.
	verdictAborted
)

// outcome is the result of one attempt.
type outcome struct {
	verdict verdict
	// retry is the Retry-After delay of verdictThrottled, and cause its
	// Refusal* cause.
	retry time.Duration
	cause int
	// err is the cause of verdictAborted.
	err error
}

// refusal returns the outcome of an attempt refused for cause, which can
// retry after retry.
func refusal(cause int, retry time.Duration) outcome {
	return outcome{verdict: verdictThrottled, retry: retry, cause: cause}
}

// Options configure a Throttle.
type Options struct {
	// Clock drives the buckets, the waits and the success cache ages; nil
	// uses clock.Real().
	Clock clock.Clock
	// Rand supplies the per-process HMAC key; nil uses crypto/rand.
	Rand io.Reader
	// Refusals are the ruralz_auth_basic_refusals_total handles by
	// Refusal* cause (RefusalCounters looks them up); a nil handle
	// records nothing.
	Refusals [NumRefusalCauses]emit.Counter
}

// RefusalCounters returns the Options.Refusals handles from counter, the
// telemetry registry's lookup of a Node-wide counter label set
// (aggregate.Registry.Counter): one per cause, in the label order of
// catalog.BasicRefusalCauses.
func RefusalCounters(counter func(name string, values ...string) (emit.Counter, error)) ([NumRefusalCauses]emit.Counter, error) {
	var out [NumRefusalCauses]emit.Counter
	causes := catalog.BasicRefusalCauses()
	if len(causes) != NumRefusalCauses {
		return out, fmt.Errorf("basic: %s has %d causes, the throttle %d", catalog.AuthBasicRefusalsTotal, len(causes), NumRefusalCauses)
	}
	for i, cause := range causes {
		c, err := counter(catalog.AuthBasicRefusalsTotal, cause)
		if err != nil {
			return out, fmt.Errorf("basic: %s cause %s: %w", catalog.AuthBasicRefusalsTotal, cause, err)
		}
		out[i] = c
	}
	return out, nil
}

// nopCounter records nothing: an Options.Refusals handle left nil.
type nopCounter struct{}

// Add does nothing.
func (nopCounter) Add(emit.Stripe, uint64) {}

// Throttle is the Node-wide pre-authentication throttle of auth.basic
// (package documentation "Throttle"): the success cache, the username and
// source buckets, the bucket-creation cap, the delay waiters and the hash
// queue, all keyed by HMAC-SHA-256 under a per-process key, so neither
// usernames, passwords nor addresses are kept. It outlives snapshots, so
// a Hot Reload keeps every entry; every auth.basic Policy of the Node
// shares it. It is safe for concurrent use.
type Throttle struct {
	clock clock.Clock
	// macs pools HMAC-SHA-256 states under the per-process key.
	macs sync.Pool

	cache   table[cacheEntry]
	users   table[bucket]
	sources table[bucket]

	userRate, sourceRate, createRate gcra

	createMu  sync.Mutex
	createTAT time.Time // guarded by createMu

	// slots holds one value per hash in progress (capacity: the slots).
	slots     chan struct{}
	queued    atomic.Int64 // attempts waiting for a slot
	maxQueued int64
	delayed   atomic.Int64 // attempts waiting for a username token

	// refusals counts ruralz_auth_basic_refusals_total by cause.
	refusals [NumRefusalCauses]emit.Counter

	// hash derives a 32-byte PBKDF2-HMAC-SHA-256 key; tests replace it.
	hash func(password string, salt []byte, iterations int) ([]byte, error)
}

var _ filter.Component = (*Throttle)(nil)

// cacheEntry is one success cache entry: a verified username and password
// pair (its key), the credential digest it matched, and its ages.
type cacheEntry struct {
	digest   [32]byte
	verified time.Time
	lastHit  time.Time
	maxAge   time.Duration
}

// bucket is a username or source bucket.
type bucket struct {
	tat time.Time
	// waiters are the attempts holding a reserved token (usernames only).
	waiters int
}

// NewThrottle returns the Node's throttle with max(1, GOMAXPROCS/4) hash
// slots; the supervisor runs it as a filter.Component. It fails only when
// the per-process key cannot be read from o.Rand.
func NewThrottle(o Options) (*Throttle, error) {
	return newThrottle(o, slotsFor(runtime.GOMAXPROCS(0)))
}

// slotsFor returns the hash slots for procs: max(1, procs/4), so hashing
// uses at most a quarter of the cores, or one core below four.
func slotsFor(procs int) int { return max(1, procs/4) }

func newThrottle(o Options, slots int) (*Throttle, error) {
	clk := o.Clock
	if clk == nil {
		clk = clock.Real()
	}
	src := o.Rand
	if src == nil {
		src = rand.Reader
	}
	key := make([]byte, sha256.Size)
	if _, err := io.ReadFull(src, key); err != nil {
		return nil, fmt.Errorf("basic: throttle key: %w", err)
	}
	t := &Throttle{
		clock:      clk,
		userRate:   newGCRA(UserRefill, UserBurst),
		sourceRate: newGCRA(SourceWindow/SourceFailures, SourceFailures),
		createRate: newGCRA(time.Second/CreatesPerSecond, CreateBurst),
		slots:      make(chan struct{}, slots),
		maxQueued:  int64(QueueWaitersPerSlot * slots),
		refusals:   o.Refusals,
		hash:       derive,
	}
	for i, c := range t.refusals {
		if c == nil {
			t.refusals[i] = nopCounter{}
		}
	}
	t.macs.New = func() any { return hmac.New(sha256.New, key) }
	t.cache.init(CacheEntries)
	t.users.init(UserBuckets)
	t.sources.init(SourceBuckets)
	return t, nil
}

// derive is PBKDF2-HMAC-SHA-256 with a 32-byte output.
func derive(password string, salt []byte, iterations int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
}

// Name implements filter.Component.
func (*Throttle) Name() string { return ComponentName }

// Run implements filter.Component: it returns nil once ctx is done. The
// throttle starts no goroutine: attempts wait on their own request
// goroutines under their request contexts, and entries expire when read
// or give way to newer ones in their LRU, so there is nothing to stop.
func (*Throttle) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// authenticate decides one attempt of username decoded[:colon] with
// password decoded[colon+1:] against cred, the username's credential in
// the snapshot, or nil when the username is undeclared or ambiguous. A
// success cache hit decides at once; otherwise the attempt passes the
// source key, the username bucket and the hash queue before one hash. A
// refusal is counted by cause on the request's stripe s.
func (t *Throttle) authenticate(ctx context.Context, s emit.Stripe, src netip.Addr, decoded []byte, colon int, cred *identity.BasicCredential) outcome {
	key := t.mac(decoded)
	if t.hit(key, cred, t.clock.Now()) {
		return outcome{verdict: verdictValid}
	}
	o := t.attempt(ctx, src, decoded[:colon], func() (bool, error) { return t.verify(decoded[colon+1:], cred) })
	switch o.verdict {
	case verdictValid:
		t.remember(key, cred.Digest, t.clock.Now())
	case verdictThrottled:
		t.refusals[o.cause].Add(s, 1)
	case verdictInvalid, verdictAborted:
	}
	return o
}

// verify checks password against cred, or runs the dummy hash, at the
// default iteration count through the same queue, when cred is nil, so
// neither timing nor status tells whether a username exists. It reports a
// match, comparing the derived key in constant time.
func (t *Throttle) verify(password []byte, cred *identity.BasicCredential) (bool, error) {
	var dummy [16]byte
	salt, iterations := dummy[:], identity.DefaultIterations
	if cred != nil {
		salt, iterations = cred.Salt[:], cred.Iterations
	}
	out, err := t.hash(string(password), salt, iterations)
	if err != nil {
		return false, fmt.Errorf("basic: verify: %w", err)
	}
	defer clear(out)
	return cred != nil && subtle.ConstantTimeCompare(out, cred.Key[:]) == 1, nil
}

// attempt runs one uncached verification behind the throttle. In order:
//
//  1. Source key: the source's failure bucket must hold a token, else the
//     attempt is refused (RefusalSource). The token is taken and returned
//     unless the hash fails, so a source makes at most SourceFailures
//     failed verifications per SourceWindow, concurrent ones included.
//  2. Username bucket, when the username has one: a token is taken, or,
//     when the bucket is empty, reserved by one of at most
//     DelayWaitersPerBucket waiters (DelayWaiters per Node) that sleeps
//     until it refills, at most MaxDelay; else refused (RefusalDelay).
//  3. Hash queue: a free slot, or a wait of at most QueueDeadline as one
//     of QueueWaitersPerSlot × slots waiters; else refused (RefusalQueue).
//  4. A username without a bucket gets one only now, after the queue
//     admitted the attempt and within the bucket-creation cap; else
//     refused (RefusalCreate). The attempt takes one of its tokens.
//  5. One hash, holding the slot.
//
// A refusal or an ended request returns every token taken, and nothing
// is hashed.
func (t *Throttle) attempt(ctx context.Context, src netip.Addr, username []byte, check func() (bool, error)) outcome {
	sk := sourceKey(src)
	srcNode, retry := t.takeSource(t.mac(sk[:]))
	if srcNode == nil {
		return refusal(RefusalSource, retry)
	}
	failed := false
	defer func() {
		if !failed {
			t.refundSource(srcNode)
		}
	}()

	uk := t.mac(username)
	user, o := t.takeUser(ctx, uk)
	if o.verdict != 0 {
		return o
	}
	if o := t.acquire(ctx); o.verdict != 0 {
		if user != nil {
			t.refundUser(user)
		}
		return o
	}
	defer t.release()
	if user == nil {
		if user, o = t.createUser(uk); user == nil {
			return o
		}
	}
	ok, err := check()
	switch {
	case err != nil:
		return outcome{verdict: verdictAborted, err: err}
	case !ok:
		failed = true
		return outcome{verdict: verdictInvalid}
	}
	return outcome{verdict: verdictValid}
}

// mac returns HMAC-SHA-256 of b under the per-process key.
func (t *Throttle) mac(b []byte) [32]byte {
	h := t.macs.Get().(hash.Hash)
	h.Reset()
	_, _ = h.Write(b)
	var sum [32]byte
	h.Sum(sum[:0])
	t.macs.Put(h)
	return sum
}

// sourceKey returns the bytes a source bucket is keyed by: the address
// family, then the IPv4 address or the IPv6 /64 prefix. An IPv4-mapped
// address counts as IPv4; an invalid address is one shared source.
func sourceKey(ip netip.Addr) [17]byte {
	var k [17]byte
	ip = ip.Unmap().WithZone("")
	switch {
	case ip.Is4():
		a := ip.As4()
		k[0] = 4
		copy(k[1:], a[:])
	case ip.Is6():
		p, _ := ip.Prefix(SourcePrefixBits)
		a := p.Addr().As16()
		k[0] = 6
		copy(k[1:], a[:])
	}
	return k
}

// takeSource takes a failure token from the source bucket of key,
// creating the bucket when absent: the bucket's node, or nil and the wait
// until a token refills when it is empty.
func (t *Throttle) takeSource(key [32]byte) (*node[bucket], time.Duration) {
	s := t.sources.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := t.clock.Now()
	n := s.get(key)
	if n == nil {
		n = s.add(key)
	}
	if w := t.sourceRate.wait(n.val.tat, now); w > 0 {
		return nil, w
	}
	t.sourceRate.take(&n.val.tat, now)
	return n, 0
}

// refundSource returns the token taken from n.
func (t *Throttle) refundSource(n *node[bucket]) {
	s := t.sources.shard(n.key)
	s.mu.Lock()
	t.sourceRate.refund(&n.val.tat)
	s.mu.Unlock()
}

// takeUser takes a token from the username bucket of key: the bucket's
// node, or nil when the username has no bucket (it is created after the
// queue admits the attempt). An empty bucket makes the attempt wait for a
// reserved token, within the delay bounds, or refuses it.
func (t *Throttle) takeUser(ctx context.Context, key [32]byte) (*node[bucket], outcome) {
	s := t.users.shard(key)
	s.mu.Lock()
	now := t.clock.Now()
	n := s.get(key)
	if n == nil {
		s.mu.Unlock()
		return nil, outcome{}
	}
	w := t.userRate.wait(n.val.tat, now)
	if w == 0 {
		t.userRate.take(&n.val.tat, now)
		s.mu.Unlock()
		return n, outcome{}
	}
	if n.val.waiters >= DelayWaitersPerBucket || w > MaxDelay || !t.reserveDelay() {
		s.mu.Unlock()
		return nil, refusal(RefusalDelay, w)
	}
	n.val.waiters++
	t.userRate.take(&n.val.tat, now)
	s.mu.Unlock()

	err := t.clock.Sleep(ctx, w)
	t.delayed.Add(-1)
	s.mu.Lock()
	n.val.waiters--
	if err != nil {
		t.userRate.refund(&n.val.tat)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, outcome{verdict: verdictAborted, err: err}
	}
	return n, outcome{}
}

// reserveDelay admits one more Node-wide delay waiter, if there is room.
func (t *Throttle) reserveDelay() bool {
	if t.delayed.Add(1) > DelayWaiters {
		t.delayed.Add(-1)
		return false
	}
	return true
}

// refundUser returns the token taken from n.
func (t *Throttle) refundUser(n *node[bucket]) {
	s := t.users.shard(n.key)
	s.mu.Lock()
	t.userRate.refund(&n.val.tat)
	s.mu.Unlock()
}

// createUser creates the username bucket of key within the creation cap
// and takes one of its tokens. A bucket another attempt created meanwhile
// is used instead, without waiting: the caller holds a hash slot.
func (t *Throttle) createUser(key [32]byte) (*node[bucket], outcome) {
	s := t.users.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := t.clock.Now()
	if n := s.get(key); n != nil {
		if w := t.userRate.wait(n.val.tat, now); w > 0 {
			return nil, refusal(RefusalDelay, w)
		}
		t.userRate.take(&n.val.tat, now)
		return n, outcome{}
	}
	if w := t.takeCreate(now); w > 0 {
		return nil, refusal(RefusalCreate, w)
	}
	n := s.add(key)
	t.userRate.take(&n.val.tat, now)
	return n, outcome{}
}

// takeCreate takes a bucket-creation token: 0, or the wait until one
// refills.
func (t *Throttle) takeCreate(now time.Time) time.Duration {
	t.createMu.Lock()
	defer t.createMu.Unlock()
	if w := t.createRate.wait(t.createTAT, now); w > 0 {
		return w
	}
	t.createRate.take(&t.createTAT, now)
	return 0
}

// acquire takes a hash slot: at once, or as one of at most maxQueued
// waiters for up to QueueDeadline.
func (t *Throttle) acquire(ctx context.Context) outcome {
	select {
	case t.slots <- struct{}{}:
		return outcome{}
	default:
	}
	if t.queued.Add(1) > t.maxQueued {
		t.queued.Add(-1)
		return refusal(RefusalQueue, 0)
	}
	defer t.queued.Add(-1)
	timer := t.clock.NewTimer(QueueDeadline)
	defer timer.Stop()
	select {
	case t.slots <- struct{}{}:
		return outcome{}
	case <-timer.C():
		return refusal(RefusalQueue, 0)
	case <-ctx.Done():
		return outcome{verdict: verdictAborted, err: ctx.Err()}
	}
}

// release frees a hash slot.
func (t *Throttle) release() { <-t.slots }

// hit reports whether the success cache holds key for cred at now and
// refreshes the entry's last hit. An entry past CacheIdle since its last
// hit or past its maximum age since verification, or recorded for another
// credential digest (the credential changed, or the username is no longer
// declared: cred nil), is removed and misses: a snapshot swap thereby
// evicts only the entries of changed or removed credentials.
func (t *Throttle) hit(key [32]byte, cred *identity.BasicCredential, now time.Time) bool {
	s := t.cache.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.get(key)
	if n == nil {
		return false
	}
	e := &n.val
	if cred == nil || e.digest != cred.Digest || now.Sub(e.lastHit) >= CacheIdle || now.Sub(e.verified) >= e.maxAge {
		s.remove(n)
		return false
	}
	e.lastHit = now
	return true
}

// remember records a verified pair under key with the credential digest
// it matched, replacing an older entry.
func (t *Throttle) remember(key, digest [32]byte, now time.Time) {
	s := t.cache.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.get(key)
	if n == nil {
		n = s.add(key)
	}
	n.val = cacheEntry{digest: digest, verified: now, lastHit: now, maxAge: maxAge(key)}
}

// maxAge returns the maximum age of the entry keyed key: CacheMaxAge less
// a jitter of up to CacheJitterPercent of it, so entries verified together
// expire apart. The jitter share comes from bytes 8 to 15 of the key, an
// HMAC output under the per-process key: uniform, fixed for a pair within
// the process, and unknown to clients.
func maxAge(key [32]byte) time.Duration {
	span := uint64(CacheMaxAge / 100 * CacheJitterPercent)
	jitter, _ := bits.Mul64(binary.BigEndian.Uint64(key[8:16]), span)
	return CacheMaxAge - time.Duration(jitter) //nolint:gosec // jitter < span, a few minutes in nanoseconds
}
