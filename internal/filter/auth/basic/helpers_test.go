// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package basic

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"math/big"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// t0 is the start of the fixture clock.
var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// policy is the name of the fixture Policy.
const policy = "partner-basic"

// fakeKey is the fixture's stand-in for PBKDF2: SHA-256 over the
// password, the salt and the iteration count, so tests hash in
// nanoseconds; TestRealPBKDF2 covers the real derivation.
func fakeKey(password string, salt []byte, iterations int) []byte {
	h := sha256.New()
	_, _ = h.Write([]byte(password))
	_, _ = h.Write(salt)
	_, _ = h.Write(strconv.AppendInt(nil, int64(iterations), 10))
	return h.Sum(nil)
}

// credential returns a credentials.basic entry of username whose stored
// key is fakeKey of password; iterations 0 leaves the default.
func credential(username, password string, iterations int) v1alpha1.BasicCredential {
	s := sha256.Sum256([]byte("salt:" + username))
	salt := s[:16]
	n := identity.DefaultIterations
	c := v1alpha1.BasicCredential{Username: username}
	if iterations != 0 {
		n = iterations
		it := int32(iterations) //nolint:gosec // test iteration counts fit in int32
		c.Iterations = &it
	}
	enc := base64.RawURLEncoding
	c.Hash = identity.BasicHashPrefix + enc.EncodeToString(salt) + ":" + enc.EncodeToString(fakeKey(password, salt, n))
	return c
}

// consumer returns a Consumer named name holding creds.
func consumer(name string, creds ...v1alpha1.BasicCredential) *v1alpha1.Consumer {
	return &v1alpha1.Consumer{Metadata: v1alpha1.ObjectMeta{Name: name}, Spec: v1alpha1.ConsumerSpec{Credentials: v1alpha1.Credentials{Basic: creds}}}
}

// hashCall is one recorded derivation: never the password.
type hashCall struct {
	salt       string
	iterations int
}

// hasher replaces the throttle's derivation with fakeKey and records each
// call. With gate set, each call waits for a value on it.
type hasher struct {
	mu    sync.Mutex
	calls []hashCall
	gate  chan struct{}
}

func (h *hasher) hash(password string, salt []byte, iterations int) ([]byte, error) {
	h.mu.Lock()
	h.calls = append(h.calls, hashCall{salt: string(salt), iterations: iterations})
	gate := h.gate
	h.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return fakeKey(password, salt, iterations), nil
}

// count returns the number of derivations started.
func (h *hasher) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

// last returns the latest derivation.
func (h *hasher) last() hashCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[len(h.calls)-1]
}

// decisions records ruralz_auth_decisions_total.
type decisions struct {
	mu    sync.Mutex
	allow int
	deny  map[string]int
}

func (d *decisions) Allow(emit.Stripe) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allow++
}

func (d *decisions) Deny(_ emit.Stripe, code string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deny == nil {
		d.deny = map[string]int{}
	}
	d.deny[code]++
}

// revoker revokes the listed usernames and counts its calls.
type revoker struct {
	mu        sync.Mutex
	usernames map[string]bool
	calls     int
}

func (r *revoker) RevokedBasic(u string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.usernames[u]
}
func (*revoker) RevokedJWT(string, string, string, string, time.Time) bool { return false }
func (*revoker) RevokedKey(string, string) bool                            { return false }
func (*revoker) RevokedAPIKey([32]byte) bool                               { return false }
func (*revoker) RevokedCert([]byte, *big.Int) bool                         { return false }

// counter records an emit.Counter: the total and the last stripe.
type counter struct {
	mu     sync.Mutex
	n      uint64
	stripe emit.Stripe
}

func (c *counter) Add(s emit.Stripe, n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += n
	c.stripe = s
}

// fixture is a Filter over a throttle on a fake clock with a fast hash.
type fixture struct {
	t         testing.TB
	clock     *clocktest.Fake
	throttle  *Throttle
	hashes    *hasher
	decisions *decisions
	revoker   *revoker
	refusals  [NumRefusalCauses]*counter
	f         *Filter
}

// newFixture builds a throttle with one hash slot, as on a Node with
// fewer than eight cores, and a Filter over consumers.
func newFixture(t testing.TB, consumers ...*v1alpha1.Consumer) *fixture {
	t.Helper()
	fx := &fixture{t: t, clock: clocktest.New(t0), hashes: &hasher{}, decisions: &decisions{}, revoker: &revoker{usernames: map[string]bool{}}}
	o := Options{Clock: fx.clock, Rand: strings.NewReader(strings.Repeat("k", 32))}
	for i := range fx.refusals {
		fx.refusals[i] = &counter{}
		o.Refusals[i] = fx.refusals[i]
	}
	th, err := newThrottle(o, 1)
	if err != nil {
		t.Fatal(err)
	}
	th.hash = fx.hashes.hash
	fx.throttle = th
	fx.f = fx.build(t, nil, consumers...)
	return fx
}

// refused returns ruralz_auth_basic_refusals_total by Refusal* cause.
func (fx *fixture) refused() [NumRefusalCauses]uint64 {
	var out [NumRefusalCauses]uint64
	for i, c := range fx.refusals {
		c.mu.Lock()
		out[i] = c.n
		c.mu.Unlock()
	}
	return out
}

// build builds another Filter of the fixture Policy over the fixture's
// throttle, as the next snapshot's compile does, with previous as
// BuildEnv.Previous.
func (fx *fixture) build(t testing.TB, previous filter.Filter, consumers ...*v1alpha1.Consumer) *Filter {
	t.Helper()
	fac, err := NewFactory(fx.throttle, fx.revoker)
	if err != nil {
		t.Fatal(err)
	}
	env := buildEnv(consumers...)
	env.Metrics = &emit.PolicyMetrics{Auth: fx.decisions}
	env.Previous = previous
	flt, err := fac.Build(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := flt.(*Filter)
	if !ok {
		t.Fatalf("Build returned %T", flt)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// buildEnv returns the BuildEnv of the fixture Policy over consumers.
func buildEnv(consumers ...*v1alpha1.Consumer) filter.BuildEnv {
	rs := make([]*hub.Resource, 0, len(consumers))
	for _, c := range consumers {
		rs = append(rs, &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindConsumer, Name: c.Metadata.Name}, Object: c})
	}
	return filter.BuildEnv{
		Policy:    &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: policy}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeAuthBasic}},
		Config:    &v1alpha1.AuthBasicConfig{},
		Bundle:    hub.NewBundle(rs),
		Consumers: identity.CompileConsumers(consumers).Map(),
	}
}

// basicValue returns the Authorization value of username and password.
func basicValue(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

// exchange returns a request from src carrying Authorization values.
func exchange(src string, values ...string) *filtertest.Exchange {
	x := &filtertest.Exchange{Start: t0, Hdr: http.Header{}}
	if src != "" {
		x.SourceV.IP = netip.MustParseAddr(src)
	}
	for _, v := range values {
		x.Hdr.Add("Authorization", v)
	}
	return x
}

// login returns a request from src with username and password.
func login(src, username, password string) *filtertest.Exchange {
	return exchange(src, basicValue(username, password))
}

// handle runs the Filter in onRequestHeaders and returns its result; an
// attempt that unexpectedly waits on the fake clock fails the test
// instead of hanging it.
func (fx *fixture) handle(x *filtertest.Exchange) filter.Result {
	return result(fx.t, fx.async(fx.t.Context(), x))
}

// result waits for an asynchronous Handle; the deadline only bounds a
// broken test.
func result(t testing.TB, ch <-chan filter.Result) filter.Result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("the attempt did not finish: it waits on the fake clock")
		return filter.Result{}
	}
}

// wantDeny checks a deny with code at status: the registered status, the
// Basic challenge on a 401, Retry-After on a 429, no body or detail, and no
// identity.
func wantDeny(t *testing.T, r filter.Result, x *filtertest.Exchange, code string, status int) {
	t.Helper()
	if r.Outcome != filter.Respond || r.Response == nil || r.Response.Code != code || r.Response.Status != status {
		t.Fatalf("result = %+v (response %+v), want a %d deny with %s", r, r.Response, status, code)
	}
	switch status {
	case http.StatusUnauthorized:
		if got := r.Response.Header.Get("WWW-Authenticate"); got != auth.ChallengeBasic {
			t.Fatalf("WWW-Authenticate = %q, want %q", got, auth.ChallengeBasic)
		}
	case http.StatusTooManyRequests:
		if r.Response.Header.Get("Retry-After") == "" {
			t.Fatal("a 429 without Retry-After")
		}
	}
	if r.Response.Body != nil || r.Response.Detail != "" {
		t.Fatal("a rejection carries a body or detail")
	}
	if x.Ident != nil {
		t.Fatal("a rejected request has an identity")
	}
}

// wantInvalid checks a 401 RZ-AUTH-002.
func wantInvalid(t *testing.T, r filter.Result, x *filtertest.Exchange) {
	t.Helper()
	wantDeny(t, r, x, auth.CodeInvalid, http.StatusUnauthorized)
}

// wantThrottled checks a 429 RZ-AUTH-007 with Retry-After retry.
func wantThrottled(t *testing.T, r filter.Result, x *filtertest.Exchange, retry string) {
	t.Helper()
	wantDeny(t, r, x, auth.CodeThrottled, http.StatusTooManyRequests)
	if got := r.Response.Header.Get("Retry-After"); got != retry {
		t.Fatalf("Retry-After = %q, want %q", got, retry)
	}
}

// wantAllow checks Continue with an identity bound to the Consumer named
// consumer and returns the identity.
func wantAllow(t *testing.T, r filter.Result, x *filtertest.Exchange, consumer string) *filter.Identity {
	t.Helper()
	if r.Outcome != filter.Continue || x.Ident == nil {
		t.Fatalf("result = %+v (response %+v), identity %v, want Continue with an identity", r, r.Response, x.Ident)
	}
	if x.Ident.Consumer == nil || x.Ident.Consumer.Name != consumer {
		t.Fatalf("identity = %+v, want Consumer %q", x.Ident, consumer)
	}
	return x.Ident
}

// waitFor polls cond until it holds; the deadline only bounds a broken
// test, the condition never depends on time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

// async runs the Filter on x in a goroutine; the result arrives on the
// returned channel.
func (fx *fixture) async(ctx context.Context, x *filtertest.Exchange) <-chan filter.Result {
	ch := make(chan filter.Result, 1)
	go func() { ch <- fx.f.Handle(ctx, phase.OnRequestHeaders, x) }()
	return ch
}

// userTAT returns the theoretical arrival time of username's bucket and
// whether the bucket exists.
func (fx *fixture) userTAT(username string) (time.Time, bool) {
	k := fx.throttle.mac([]byte(username))
	s := fx.throttle.users.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.m[k]
	if n == nil {
		return time.Time{}, false
	}
	return n.val.tat, true
}

// srcIP returns the n-th address of 10.0.0.0/8, a distinct source per n.
func srcIP(n int) string {
	return "10." + strconv.Itoa(n>>16&255) + "." + strconv.Itoa(n>>8&255) + "." + strconv.Itoa(n&255)
}
