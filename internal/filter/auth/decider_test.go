// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tests for spec 06 requirements 3 (Security rule 2: a second successful
// authentication is 401 RZ-AUTH-002, whether or not either bound a
// Consumer), 7 (problem documents with registry statuses and no detail), 8
// (WWW-Authenticate on every 401 of a type with a challenge; Retry-After in
// integer seconds, at least 1, on every 429) and 10 (every auth and authz
// decision counts ruralz_auth_decisions_total with the RZ code on deny).

// decisions records AuthDecisions calls.
type decisions struct {
	mu     sync.Mutex
	allows int
	denies []string
	stripe emit.Stripe
}

func (d *decisions) Allow(s emit.Stripe) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allows++
	d.stripe = s
}

func (d *decisions) Deny(s emit.Stripe, code string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.denies = append(d.denies, code)
	d.stripe = s
}

func metricsWith(d *decisions) *emit.PolicyMetrics { return &emit.PolicyMetrics{Auth: d} }

func TestReq7And8DenyAnswers(t *testing.T) {
	tests := []struct {
		name      string
		challenge string
		authz     bool
		code      string
		status    int
		header    http.Header
	}{
		{"jwt 401 carries Bearer", ChallengeJWT, false, CodeInvalid, 401, http.Header{"Www-Authenticate": {`Bearer realm="ruralz"`}}},
		{"basic 401 carries Basic", ChallengeBasic, false, CodeMissing, 401, http.Header{"Www-Authenticate": {`Basic realm="ruralz", charset="UTF-8"`}}},
		{"api-key 401", APIKeyChallenge("x-api-key"), false, CodeClaims, 401, http.Header{"Www-Authenticate": {`APIKey header="x-api-key"`}}},
		{"mtls 401 has no challenge", "", false, CodeRevoked, 401, nil},
		{"421 has no challenge", ChallengeJWT, false, CodeNoCertRequest, 421, nil},
		{"429 carries Retry-After", ChallengeBasic, false, CodeThrottled, 429, http.Header{"Retry-After": {"1"}}},
		{"authz 403", "", true, "RZ-AUTH-010", 403, nil},
		{"authz ip 403", "", true, "RZ-AUTH-013", 403, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &decisions{}
			d := NewAuth(metricsWith(rec), tt.challenge)
			if tt.authz {
				d = NewAuthz(metricsWith(rec))
			}
			x := &filtertest.Exchange{StripeV: 3}
			r := d.Deny(x, tt.code)
			if r.Outcome != filter.Respond || r.Response == nil {
				t.Fatalf("Deny outcome %v", r.Outcome)
			}
			resp := r.Response
			if resp.Status != tt.status || resp.Code != tt.code || resp.Body != nil || resp.Detail != "" {
				t.Fatalf("response = %+v; want %d %s, no body, no detail", resp, tt.status, tt.code)
			}
			if len(resp.Header) != len(tt.header) {
				t.Fatalf("header = %v, want %v", resp.Header, tt.header)
			}
			for k, v := range tt.header {
				if got := resp.Header.Values(k); len(got) != 1 || got[0] != v[0] {
					t.Fatalf("header %s = %v, want %v", k, got, v)
				}
			}
			if len(rec.denies) != 1 || rec.denies[0] != tt.code || rec.allows != 0 || rec.stripe != 3 {
				t.Fatalf("recorded %+v", rec)
			}
		})
	}
}

// Every rejection gets its own header map, so a later onResponse edit of
// one generated response never leaks into another.
func TestReq8DenyHeadersAreNotShared(t *testing.T) {
	d := NewAuth(nil, ChallengeJWT)
	x := &filtertest.Exchange{}
	a := d.Deny(x, CodeInvalid).Response
	a.Header.Set("Access-Control-Allow-Origin", "*")
	if b := d.Deny(x, CodeInvalid).Response; b.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("rejections share a header map")
	}
}

func TestReq8ThrottleRetryAfter(t *testing.T) {
	tests := []struct {
		delay time.Duration
		want  string
	}{
		{0, "1"},
		{-time.Second, "1"},
		{time.Millisecond, "1"},
		{time.Second, "1"},
		{time.Second + time.Nanosecond, "2"},
		{1500 * time.Millisecond, "2"},
		{2 * time.Second, "2"},
		{90 * time.Second, "90"},
		{time.Duration(1<<63 - 1), "9223372037"},
	}
	for _, tt := range tests {
		if got := RetryAfter(tt.delay); got != tt.want {
			t.Errorf("RetryAfter(%v) = %s, want %s", tt.delay, got, tt.want)
		}
	}
	rec := &decisions{}
	r := NewAuth(metricsWith(rec), ChallengeBasic).Throttle(&filtertest.Exchange{}, 2500*time.Millisecond)
	if r.Response.Status != 429 || r.Response.Code != CodeThrottled || r.Response.Header.Get("Retry-After") != "3" {
		t.Fatalf("Throttle = %+v", r.Response)
	}
	if r.Response.Header.Get("Www-Authenticate") != "" {
		t.Fatal("a 429 carries a challenge")
	}
	if len(rec.denies) != 1 || rec.denies[0] != CodeThrottled {
		t.Fatalf("recorded %+v", rec)
	}
}

func TestReq3SecondAuthenticationFails(t *testing.T) {
	acme := &expr.Consumer{Name: "acme"}
	tests := []struct {
		name          string
		first, second *filter.Identity
	}{
		{"both bound a Consumer", NewIdentity(filter.MethodAPIKey, "keys", acme), NewIdentity(filter.MethodBasic, "basic", acme)},
		{"neither bound a Consumer", NewJWTIdentity("jwt", nil, nil, "i", "s"), NewMTLSIdentity("mtls", nil, "CN=x")},
		{"only the first bound", NewIdentity(filter.MethodAPIKey, "keys", acme), NewJWTIdentity("jwt", nil, nil, "i", "s")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec1, rec2 := &decisions{}, &decisions{}
			x := &filtertest.Exchange{}
			if r := NewAuth(metricsWith(rec1), ChallengeJWT).Authenticated(x, tt.first); r.Outcome != filter.Continue {
				t.Fatalf("first authentication: %+v", r)
			}
			if x.Ident != tt.first || rec1.allows != 1 {
				t.Fatalf("first identity not recorded: %+v, %+v", x.Ident, rec1)
			}
			r := NewAuth(metricsWith(rec2), ChallengeBasic).Authenticated(x, tt.second)
			if r.Outcome != filter.Respond || r.Response.Status != 401 || r.Response.Code != CodeInvalid {
				t.Fatalf("second authentication = %+v, want 401 %s", r.Response, CodeInvalid)
			}
			if r.Response.Header.Get("Www-Authenticate") != ChallengeBasic {
				t.Fatal("the rule 2 rejection has no challenge")
			}
			if x.Ident != tt.first {
				t.Fatal("the second identity replaced the first")
			}
			if rec2.allows != 0 || len(rec2.denies) != 1 || rec2.denies[0] != CodeInvalid {
				t.Fatalf("second Policy recorded %+v", rec2)
			}
		})
	}
}

// legView refuses identities, as a leg view does.
type legView struct{ filtertest.Exchange }

func (*legView) SetIdentity(*filter.Identity) error { return filter.ErrNotAvailable }

// Requirement 8 on a closed failure: an auth Decider that cannot decide
// returns CannotDecide (never Respond), with a code whose registered status
// is 401, and exposes as Challenger the value the executor adds as
// WWW-Authenticate to that 401; authz and auth.mtls expose none.
func TestReq8UndecidedLeavesTheChallengeToTheExecutor(t *testing.T) {
	type jwtFilter struct{ *Decider }
	cause := errors.New("jwks unavailable")
	tests := []struct {
		name      string
		d         *Decider
		in, code  string
		status    int
		challenge string
	}{
		{"jwt default", NewAuth(nil, ChallengeJWT), "", CodeInvalid, 401, ChallengeJWT},
		{"jwt no keys", NewAuth(nil, ChallengeJWT), CodeNoKeys, CodeNoKeys, 401, ChallengeJWT},
		{"basic", NewAuth(nil, ChallengeBasic), CodeInvalid, CodeInvalid, 401, ChallengeBasic},
		{"api-key", NewAuth(nil, APIKeyChallenge("x-key")), CodeInvalid, CodeInvalid, 401, `APIKey header="x-key"`},
		{"mtls", NewAuth(nil, ""), CodeInvalid, CodeInvalid, 401, ""},
		{"authz default", NewAuthz(nil), "", CodeAuthzUndecided, 403, ""},
	}
	for _, tt := range tests {
		r := tt.d.Undecided(&filtertest.Exchange{}, tt.in, cause)
		if r.Outcome != filter.CannotDecide || r.Response != nil || r.Code != tt.code || !errors.Is(r.Err, cause) {
			t.Errorf("%s: Undecided = %+v, want CannotDecide %s", tt.name, r, tt.code)
		}
		if got := tt.d.Status(r.Code); got != tt.status {
			t.Errorf("%s: Status(%s) = %d, want %d", tt.name, r.Code, got, tt.status)
		}
		var f any = jwtFilter{tt.d}
		ch, ok := f.(Challenger)
		if !ok || ch.Challenge() != tt.challenge {
			t.Errorf("%s: Challenger = %v, %q; want %q", tt.name, ok, ch.Challenge(), tt.challenge)
		}
	}
}

func TestUndecided(t *testing.T) {
	rec := &decisions{}
	x := &legView{}
	r := NewAuth(metricsWith(rec), ChallengeJWT).Authenticated(x, NewIdentity(filter.MethodBasic, "p", nil))
	if r.Outcome != filter.CannotDecide || r.Code != CodeInvalid || !errors.Is(r.Err, filter.ErrNotAvailable) {
		t.Fatalf("leg view result = %+v", r)
	}
	cause := errors.New("jwks unavailable")
	tests := []struct {
		name  string
		d     *Decider
		code  string
		want  string
		class string
	}{
		{"auth default", NewAuth(metricsWith(rec), ChallengeJWT), "", CodeInvalid, "auth"},
		{"auth explicit", NewAuth(metricsWith(rec), ChallengeJWT), CodeNoKeys, CodeNoKeys, "auth"},
		{"authz default", NewAuthz(metricsWith(rec)), "", CodeAuthzUndecided, "authz"},
	}
	for _, tt := range tests {
		rec.denies = nil
		r := tt.d.Undecided(&filtertest.Exchange{}, tt.code, cause)
		if r.Outcome != filter.CannotDecide || r.Code != tt.want || !errors.Is(r.Err, cause) {
			t.Errorf("%s: Undecided = %+v", tt.name, r)
		}
		if len(rec.denies) != 1 || rec.denies[0] != tt.want {
			t.Errorf("%s: recorded %v", tt.name, rec.denies)
		}
	}
}

func TestReq10Allow(t *testing.T) {
	rec := &decisions{}
	if r := NewAuthz(metricsWith(rec)).Allow(&filtertest.Exchange{StripeV: 5}); r.Outcome != filter.Continue {
		t.Fatalf("Allow = %+v", r)
	}
	if rec.allows != 1 || len(rec.denies) != 0 || rec.stripe != 5 {
		t.Fatalf("recorded %+v", rec)
	}
	// Without handles nothing is recorded and nothing panics.
	for _, m := range []*emit.PolicyMetrics{nil, {}} {
		d := NewAuth(m, "")
		x := &filtertest.Exchange{}
		d.Allow(x)
		d.Deny(x, CodeMissing)
		d.Throttle(x, time.Second)
		d.Undecided(x, "", nil)
	}
}

// Statuses come from the registry (OQ-security-and-identity-9: never a
// literal), resolved at construction for every area, so Status never calls
// errcode on the request path.
func TestReq7StatusFromRegistry(t *testing.T) {
	d := NewAuth(nil, "")
	for _, c := range errcode.All() {
		if c.Status == 0 {
			continue
		}
		if got := d.Status(c.ID); got != c.Status {
			t.Errorf("Status(%s) = %d, want %d", c.ID, got, c.Status)
		}
	}
	if d.Status("RZ-RT-008") != 403 {
		t.Error("non-AUTH code status not resolved")
	}
	for _, code := range []string{CodeInvalid, "RZ-RT-008", "RZ-AUTH-016"} {
		if n := testing.AllocsPerRun(100, func() { d.Status(code) }); n != 0 {
			t.Errorf("Status(%s) allocates %v times", code, n)
		}
	}
	if d.Status("RZ-AUTH-016") != 500 || d.Status("RZ-NOPE-001") != 500 {
		t.Error("a code without a single status must be 500")
	}
	// The Code constants are registered, with the documented statuses.
	for code, status := range map[string]int{
		CodeMissing: 401, CodeInvalid: 401, CodeClaims: 401, CodeRevoked: 401, CodeUnknownKID: 401,
		CodeNoKeys: 401, CodeThrottled: 429, CodeNoCertRequest: 421, CodeAuthzUndecided: 403,
	} {
		if c, ok := errcode.Lookup(code); !ok || c.Status != status {
			t.Errorf("%s registered as %+v, want status %d", code, c, status)
		}
	}
}

// A Filter embedding *Decider is a Challenger, which is how the executor
// finds the challenges of Security rule 1.
func TestReq2EmbeddedDeciderIsChallenger(t *testing.T) {
	type jwtFilter struct{ *Decider }
	var f any = jwtFilter{NewAuth(nil, ChallengeJWT)}
	ch, ok := f.(Challenger)
	if !ok || ch.Challenge() != ChallengeJWT {
		t.Fatal("embedded Decider does not provide the challenge")
	}
	if NewAuthz(nil).Challenge() != "" {
		t.Fatal("authz has a challenge")
	}
}

// BenchmarkAuthenticated: the success path of every auth Filter.
func BenchmarkAuthenticated(b *testing.B) {
	d := NewAuth(nil, ChallengeJWT)
	id := NewIdentity(filter.MethodAPIKey, "keys", &expr.Consumer{Name: "acme"})
	x := &filtertest.Exchange{}
	b.ReportAllocs()
	for b.Loop() {
		x.Ident = nil
		d.Authenticated(x, id)
	}
}
