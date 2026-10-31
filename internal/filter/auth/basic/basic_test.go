// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package basic

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestValidCredentialBindsConsumer checks "credentials.basic by
// username": the right password binds the Consumer declaring the
// username, and the identity carries method basic, the Policy, the
// snapshot's Consumer view, empty claims and the Consumer's name as
// principal.
func TestValidCredentialBindsConsumer(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("acme-batch", "s3cret", 0)), consumer("globex", credential("globex-ci", "hunter2", 700_000)))
	x := login("192.0.2.1", "acme-batch", "s3cret")
	id := wantAllow(t, fx.handle(x), x, "acme")
	if id.Method != filter.MethodBasic || id.Policy != policy || id.Principal != "acme" || id.CertSubject != "" {
		t.Fatalf("identity = %+v", id)
	}
	if b, err := id.Claims.AppendBody(nil); err != nil || string(b) != "{}" {
		t.Fatalf("auth.claims = %s, %v; want {}", b, err)
	}
	if fx.f.CredentialIndex() == nil {
		t.Fatal("no credential index")
	}
	if c, _ := fx.f.CredentialIndex().Consumer("acme"); id.Consumer != c {
		t.Fatal("the identity does not carry the snapshot's Consumer view")
	}
	x = login("192.0.2.1", "globex-ci", "hunter2")
	wantAllow(t, fx.handle(x), x, "globex")
	if got := fx.hashes.last().iterations; got != 700_000 {
		t.Fatalf("verified at %d iterations, want the credential's 700000", got)
	}
}

// TestMissingCredential checks RZ-AUTH-001 "credential missing", as
// auth.jwt reads it: no Authorization field, an empty one, or a scheme
// other than Basic, each with the Basic challenge and no hash.
func TestMissingCredential(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("acme-batch", "s3cret", 0)))
	tok := strings.TrimPrefix(basicValue("acme-batch", "s3cret"), "Basic ")
	for _, values := range [][]string{nil, {""}, {"Bearer " + tok}, {"Digest " + tok}, {"Basic" + tok}} {
		x := exchange("", values...)
		wantDeny(t, fx.handle(x), x, auth.CodeMissing, http.StatusUnauthorized)
	}
	if fx.hashes.count() != 0 {
		t.Fatal("a request without Basic credentials was hashed")
	}
}

// TestMalformedValue checks "a malformed value is RZ-AUTH-002": a Basic
// value that is not padded base64 of user-id ":" password is rejected
// before the throttle, with no hash, bucket or source token.
func TestMalformedValue(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("acme-batch", "s3cret", 0)))
	enc := base64.StdEncoding.EncodeToString
	for _, v := range []string{
		"Basic",
		"Basic    ",
		"Basic !!!!",
		"Basic " + enc([]byte("no-colon")),
		"Basic " + strings.TrimRight(enc([]byte("acme-batch:s3cre")), "="),
		"Basic " + base64.URLEncoding.EncodeToString([]byte("acme-batch:s3cret>>?")),
		"Basic " + enc([]byte("acme-batch:")) + " " + enc([]byte("s3cret")),
		`Basic realm="ruralz"`,
	} {
		x := exchange("", v)
		wantInvalid(t, fx.handle(x), x)
	}
	if fx.hashes.count() != 0 || fx.throttle.users.len() != 0 || fx.throttle.sources.len() != 0 {
		t.Fatalf("malformed values reached the throttle: %d hashes, %d buckets, %d sources",
			fx.hashes.count(), fx.throttle.users.len(), fx.throttle.sources.len())
	}
}

// TestSchemeAndSpacing checks that the scheme name is matched
// case-insensitively and that spaces around the value and after the scheme
// are ignored, and that a password may hold colons: the user-id ends at the
// first one (RFC 7617).
func TestSchemeAndSpacing(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("acme-batch", "pa:ss:word", 0)))
	tok := strings.TrimPrefix(basicValue("acme-batch", "pa:ss:word"), "Basic ")
	for _, v := range []string{"basic " + tok, "BASIC " + tok, " Basic   " + tok + " ", "\tBasic " + tok} {
		x := exchange("", v)
		wantAllow(t, fx.handle(x), x, "acme")
	}
}

// TestTwoAuthorizationLines checks that more than one Authorization field
// line, in any case, is RZ-AUTH-002 even when each is valid.
func TestTwoAuthorizationLines(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("acme-batch", "s3cret", 0)))
	v := basicValue("acme-batch", "s3cret")
	x := exchange("", v, v)
	wantInvalid(t, fx.handle(x), x)
	x = exchange("", v)
	x.Hdr["authorization"] = []string{v}
	wantInvalid(t, fx.handle(x), x)
}

// TestWrongPassword checks that a wrong password for a declared username
// is RZ-AUTH-002 after one hash with the credential's salt and iteration
// count.
func TestWrongPassword(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("acme-batch", "s3cret", 800_000)))
	x := login("192.0.2.1", "acme-batch", "S3cret")
	wantInvalid(t, fx.handle(x), x)
	if fx.hashes.count() != 1 || fx.hashes.last().iterations != 800_000 {
		t.Fatalf("hashes = %+v, want one at 800000 iterations", fx.hashes.calls)
	}
}

// TestUndeclaredUsername checks "Undeclared usernames": a username no
// Consumer declares gets the same answer as a wrong password, after a
// dummy hash at the default iteration count through the same queue, and a
// bucket like any presented username.
func TestUndeclaredUsername(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("acme-batch", "s3cret", 900_000)))
	x := login("192.0.2.1", "mallory", "s3cret")
	wantInvalid(t, fx.handle(x), x)
	if fx.hashes.count() != 1 || fx.hashes.last().iterations != identity.DefaultIterations {
		t.Fatalf("hashes = %+v, want one dummy hash at %d iterations", fx.hashes.calls, identity.DefaultIterations)
	}
	if _, ok := fx.userTAT("mallory"); !ok {
		t.Fatal("no bucket for the undeclared username")
	}
}

// TestAmbiguousUsername checks that a username two Consumers declare
// (which RZ-CFG-035 rejects at validation) binds neither: it is verified
// like an undeclared one and is RZ-AUTH-002 even with the right password.
func TestAmbiguousUsername(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("shared", "s3cret", 0)), consumer("globex", credential("shared", "s3cret", 0)))
	x := login("192.0.2.1", "shared", "s3cret")
	wantInvalid(t, fx.handle(x), x)
	if fx.hashes.count() != 1 || fx.hashes.last().iterations != identity.DefaultIterations {
		t.Fatalf("hashes = %+v, want one dummy hash", fx.hashes.calls)
	}
}

// TestRevokedUsername checks Figure 2's order: a verified pair whose
// username is on the revocation list is RZ-AUTH-004, on a success cache
// hit too ("A hit still passes the revocation check"), while a wrong
// password is RZ-AUTH-002 without consulting the list.
func TestRevokedUsername(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("acme-batch", "s3cret", 0)))
	x := login("192.0.2.1", "acme-batch", "s3cret")
	wantAllow(t, fx.handle(x), x, "acme")

	fx.revoker.usernames["acme-batch"] = true
	x = login("192.0.2.1", "acme-batch", "s3cret")
	wantDeny(t, fx.handle(x), x, auth.CodeRevoked, http.StatusUnauthorized)
	if fx.hashes.count() != 1 {
		t.Fatal("the revoked pair was hashed again instead of hitting the success cache")
	}
	calls := fx.revoker.calls
	x = login("192.0.2.1", "acme-batch", "wrong")
	wantInvalid(t, fx.handle(x), x)
	if fx.revoker.calls != calls {
		t.Fatal("the Revoker was consulted for a wrong password")
	}

	// A revoked pair verified on a miss is refused too.
	fy := newFixture(t, consumer("acme", credential("acme-batch", "s3cret", 0)))
	fy.revoker.usernames["acme-batch"] = true
	x = login("192.0.2.1", "acme-batch", "s3cret")
	wantDeny(t, fy.handle(x), x, auth.CodeRevoked, http.StatusUnauthorized)
}

// TestSecondAuthentication checks Security rule 2: when an earlier
// auth-class Policy authenticated the request, valid credentials are 401
// RZ-AUTH-002 and the first identity stays.
func TestSecondAuthentication(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("acme-batch", "s3cret", 0)))
	x := login("192.0.2.1", "acme-batch", "s3cret")
	first := &filter.Identity{Method: filter.MethodAPIKey, Policy: "partner-keys"}
	x.Ident = first
	r := fx.handle(x)
	if r.Outcome != filter.Respond || r.Response.Code != auth.CodeInvalid || r.Response.Status != http.StatusUnauthorized || x.Ident != first {
		t.Fatalf("second authentication = %+v (response %+v)", r, r.Response)
	}
}

// TestStripsAuthorization checks "auth.api-key and auth.basic strip their
// credential": every Authorization line, in any case, is removed from the
// live request headers whatever the decision, and no other field is
// touched.
func TestStripsAuthorization(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("acme-batch", "s3cret", 0)))
	tests := []struct {
		name string
		h    http.Header
	}{
		{"authenticated", http.Header{"Authorization": {basicValue("acme-batch", "s3cret")}}},
		{"non-canonical key", http.Header{"authorization": {basicValue("acme-batch", "s3cret")}}},
		{"wrong password", http.Header{"Authorization": {basicValue("acme-batch", "nope")}}},
		{"another scheme", http.Header{"Authorization": {"Bearer opaque"}}},
		{"two lines", http.Header{"Authorization": {"Basic a"}, "AUTHORIZATION": {"Basic b"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.h.Set("X-Api-Key", "kept")
			x := exchange("192.0.2.1")
			x.Hdr = tt.h
			fx.handle(x)
			for k := range x.Hdr {
				if http.CanonicalHeaderKey(k) == "Authorization" {
					t.Fatalf("field %q left on the request", k)
				}
			}
			if x.Hdr.Get("X-Api-Key") != "kept" || len(x.Hdr) != 1 {
				t.Fatalf("other fields changed: %v", x.Hdr)
			}
		})
	}
}

// TestOtherPhasesContinue checks that the Filter acts only in
// onRequestHeaders: elsewhere it neither authenticates, hashes nor
// strips.
func TestOtherPhasesContinue(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("acme-batch", "s3cret", 0)))
	for _, p := range []phase.Phase{phase.OnRequestBody, phase.OnRoute, phase.OnResponse, phase.OnLog} {
		x := login("192.0.2.1", "acme-batch", "s3cret")
		if r := fx.f.Handle(t.Context(), p, x); r.Outcome != filter.Continue || x.Ident != nil || x.Hdr.Get("Authorization") == "" {
			t.Fatalf("Handle(%v) = %+v", p, r)
		}
	}
	if fx.hashes.count() != 0 {
		t.Fatal("a Phase other than onRequestHeaders hashed")
	}
}

// TestDecisionsCounted checks that ruralz_auth_decisions_total counts an
// allow per authenticated request and a deny per rejection by code,
// throttled attempts included.
func TestDecisionsCounted(t *testing.T) {
	fx := newFixture(t, consumer("acme", credential("acme-batch", "s3cret", 0)))
	fx.handle(login("192.0.2.1", "acme-batch", "s3cret"))
	fx.handle(login("192.0.2.1", "acme-batch", "s3cret"))
	fx.handle(exchange("192.0.2.1"))
	for i := range SourceFailures + 1 {
		fx.handle(login("192.0.2.9", "guess", strings.Repeat("x", i)))
	}
	d := fx.decisions
	if d.allow != 2 || d.deny[auth.CodeMissing] != 1 || d.deny[auth.CodeInvalid] != SourceFailures || d.deny[auth.CodeThrottled] != 1 {
		t.Fatalf("decisions = %d allow, %v deny", d.allow, d.deny)
	}
}

// TestBuild checks the Factory: a missing throttle is ErrNoThrottle, a
// config of another type is RZ-CFG-005, an absent or empty config is
// accepted, an ended compile context is its error without a code, and the
// Filter exposes the Basic challenge.
func TestBuild(t *testing.T) {
	if _, err := NewFactory(nil, nil); !errors.Is(err, ErrNoThrottle) {
		t.Fatalf("NewFactory(nil) = %v, want ErrNoThrottle", err)
	}
	fx := newFixture(t)
	fac, err := NewFactory(fx.throttle, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := buildEnv()
	env.Config = &v1alpha1.AuthAPIKeyConfig{}
	_, err = fac.Build(t.Context(), env)
	if code, _ := errcode.CodeOf(err); code != "RZ-CFG-005" {
		t.Fatalf("wrong config type: %v, want RZ-CFG-005", err)
	}
	for _, cfg := range []any{nil, (*v1alpha1.AuthBasicConfig)(nil), v1alpha1.AuthBasicConfig{}} {
		env.Config = cfg
		flt, err := fac.Build(t.Context(), env)
		if err != nil {
			t.Fatalf("config %#v: %v", cfg, err)
		}
		if flt.(*Filter).Challenge() != auth.ChallengeBasic {
			t.Fatal("the Filter does not expose the Basic challenge")
		}
		_ = flt.(*Filter).Close()
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = fac.Build(ctx, buildEnv())
	if _, coded := errcode.CodeOf(err); !errors.Is(err, context.Canceled) || coded {
		t.Fatalf("canceled compile: %v", err)
	}
}

// TestCloseReleasesIndex checks that Close drops the Filter's reference on
// the snapshot's credential index, stopping the secretRef watches once no
// Filter holds it, is idempotent, and leaves the Node's throttle serving.
func TestCloseReleasesIndex(t *testing.T) {
	ref := secret.Ref{Provider: v1alpha1.SecretProviderEnv, Name: "RURALZ_SECRET_KEY"}
	st := &store{values: map[secret.Ref]string{ref: "ZXhhbXBsZS1rZXktYWFhYWFhYWFhYWFh"}}
	c := consumer("acme", credential("acme-batch", "s3cret", 0))
	c.Spec.Credentials.APIKeys = []v1alpha1.APIKey{{Name: "k", SecretRef: &v1alpha1.SecretRef{Provider: ref.Provider, Name: ref.Name}}}
	fx := newFixture(t)
	fac, err := NewFactory(fx.throttle, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := buildEnv(c)
	env.Secrets = st
	flt, err := fac.Build(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	f := flt.(*Filter)
	if st.live() != 1 {
		t.Fatalf("watches = %d, want 1", st.live())
	}
	if err := f.Close(); err != nil || st.live() != 0 {
		t.Fatalf("watches = %d after Close (%v), want 0", st.live(), err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	g := fx.build(t, nil, consumer("acme", credential("acme-batch", "s3cret", 0)))
	x := login("192.0.2.1", "acme-batch", "s3cret")
	wantAllow(t, g.Handle(t.Context(), phase.OnRequestHeaders, x), x, "acme")
}

// store is a secret.Store over fixed values that counts live watches.
type store struct {
	mu      sync.Mutex
	values  map[secret.Ref]string
	watches int
}

func (s *store) Get(r secret.Ref) (secret.Value, bool) {
	v, ok := s.values[r]
	if !ok {
		return secret.Value{}, false
	}
	return secret.NewValue([]byte(v)), true
}

func (s *store) Watch(secret.Ref, func(secret.Value) error) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watches++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.watches--
	}
}

func (s *store) live() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watches
}
