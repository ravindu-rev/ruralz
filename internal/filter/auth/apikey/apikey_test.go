// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package apikey

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestKnownKeyBindsConsumer checks "API keys by digest": the key's SHA-256
// digest binds the Consumer holding it, and the identity carries method
// api-key, the Policy, the snapshot's Consumer view, empty claims and the
// Consumer's name as principal.
func TestKnownKeyBindsConsumer(t *testing.T) {
	env := buildEnv(nil, nil, consumer("acme", "gold", literal("primary", keyAcme)), consumer("globex", "", literal("primary", keyGlobex)))
	f := build(t, env)
	x := withKey(keyAcme)
	id := wantAllow(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, "acme")
	if id.Method != filter.MethodAPIKey || id.Policy != policy || id.Principal != "acme" || id.CertSubject != "" {
		t.Fatalf("identity = %+v", id)
	}
	if id.Consumer != env.Consumers["acme"] {
		t.Fatal("the identity does not carry the snapshot's Consumer view")
	}
	if id.Claims == nil || id.Claims.IsNull() {
		t.Fatal("auth.claims is not the empty map")
	}
	if b, err := id.Claims.AppendBody(nil); err != nil || string(b) != "{}" {
		t.Fatalf("auth.claims = %s, %v; want {}", b, err)
	}
	x = withKey(keyGlobex)
	wantAllow(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, "globex")
}

// TestMissingKey checks RZ-AUTH-001 "credential missing": no field line of
// the header, or a field line with an empty value, with the challenge
// naming the header.
func TestMissingKey(t *testing.T) {
	f := build(t, buildEnv(nil, nil, consumer("acme", "", literal("primary", keyAcme))))
	for _, h := range []http.Header{nil, {"X-Api-Key": {""}}, {"Authorization": {"Bearer " + keyAcme}}, {"X-Api-Keys": {keyAcme}}} {
		x := exchange(h)
		wantDeny(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeMissing, defaultChallenge)
	}
}

// TestTwoFieldLines checks that more than one field line of the header, in
// any case, is RZ-AUTH-002 even when each line holds a valid key.
func TestTwoFieldLines(t *testing.T) {
	f := build(t, buildEnv(nil, nil, consumer("acme", "", literal("primary", keyAcme))))
	for _, h := range []http.Header{
		{"X-Api-Key": {keyAcme, keyAcme}},
		{"X-Api-Key": {keyAcme}, "x-api-key": {keyAcme}},
		{"X-API-KEY": {keyAcme}, "x-api-key": {keyAcme}},
	} {
		x := exchange(h)
		wantDeny(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid, defaultChallenge)
	}
}

// TestUnknownKey checks that a key no Consumer holds is RZ-AUTH-002,
// including a held key that differs in case or by a space: the digest is
// over the exact bytes of the field value.
func TestUnknownKey(t *testing.T) {
	f := build(t, buildEnv(nil, nil, consumer("acme", "", literal("primary", keyAcme))))
	for _, key := range []string{"guess-0123456789abcdefghijklmno", "ACME-0123456789abcdefghijklmnopq", keyAcme + " ", " " + keyAcme, hashOf(keyAcme)} {
		x := withKey(key)
		wantDeny(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid, defaultChallenge)
	}
}

// TestConfiguredHeader checks config.header: the configured name is read
// case-insensitively, the default header is then ignored, and the
// challenge names the header as configured.
func TestConfiguredHeader(t *testing.T) {
	name := "X-Partner-Key"
	f := build(t, buildEnv(&v1alpha1.AuthAPIKeyConfig{Header: &name}, nil, consumer("acme", "", literal("primary", keyAcme))))
	if f.Challenge() != `APIKey header="X-Partner-Key"` {
		t.Fatalf("Challenge = %q", f.Challenge())
	}
	for _, h := range []http.Header{{"X-Partner-Key": {keyAcme}}, {"x-partner-key": {keyAcme}}, {"X-PARTNER-KEY": {keyAcme}}} {
		x := exchange(h)
		wantAllow(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, "acme")
	}
	x := withKey(keyAcme)
	wantDeny(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeMissing, f.Challenge())
}

// TestDefaultHeader checks the schema default: an absent config, a nil
// config pointer, an absent header and an empty header all read
// x-api-key.
func TestDefaultHeader(t *testing.T) {
	empty := ""
	for _, cfg := range []any{nil, (*v1alpha1.AuthAPIKeyConfig)(nil), &v1alpha1.AuthAPIKeyConfig{}, v1alpha1.AuthAPIKeyConfig{}, &v1alpha1.AuthAPIKeyConfig{Header: &empty}} {
		f := build(t, buildEnv(cfg, nil, consumer("acme", "", literal("primary", keyAcme))))
		if f.Challenge() != defaultChallenge {
			t.Fatalf("config %#v: Challenge = %q", cfg, f.Challenge())
		}
		x := exchange(http.Header{"x-api-key": {keyAcme}})
		wantAllow(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, "acme")
	}
}

// TestStripsCredential checks "auth.api-key and auth.basic strip their
// credential": every field line of the header, in any case, is removed
// from the live request headers, whatever the decision, and no other
// field is touched.
func TestStripsCredential(t *testing.T) {
	f := build(t, buildEnv(nil, nil, consumer("acme", "", literal("primary", keyAcme))))
	tests := []struct {
		name string
		h    http.Header
	}{
		{"authenticated", http.Header{"X-Api-Key": {keyAcme}}},
		{"non-canonical key", http.Header{"x-api-key": {keyAcme}}},
		{"unknown key", http.Header{"X-Api-Key": {"guess-0123456789abcdefghijklmno"}}},
		{"two lines", http.Header{"X-Api-Key": {keyAcme}, "X-API-Key": {keyAcme}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.h.Set("Authorization", "Bearer opaque")
			tt.h.Set("X-Request-Id", "r1")
			x := exchange(tt.h)
			f.Handle(t.Context(), phase.OnRequestHeaders, x)
			for k := range x.Hdr {
				if http.CanonicalHeaderKey(k) == "X-Api-Key" {
					t.Fatalf("field %q left on the request", k)
				}
			}
			if x.Hdr.Get("Authorization") != "Bearer opaque" || x.Hdr.Get("X-Request-Id") != "r1" || len(x.Hdr) != 2 {
				t.Fatalf("other fields changed: %v", x.Hdr)
			}
		})
	}
}

// TestAmbiguousKey checks "two matching Consumers are 401 RZ-AUTH-002":
// a digest two Consumers hold, literally or through a secretRef, binds
// neither.
func TestAmbiguousKey(t *testing.T) {
	st := newStore(map[secret.Ref]string{refPrevious: keyGlobex})
	f := build(t, buildEnv(nil, st,
		consumer("acme", "", literal("primary", keyAcme), literal("shared", keyGlobex)),
		consumer("initech", "", literal("primary", keyAcme)),
		consumer("globex", "", held(refPrevious)),
	))
	for _, key := range []string{keyAcme, keyGlobex} {
		x := withKey(key)
		wantDeny(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid, defaultChallenge)
	}
}

// TestSameConsumerTwice checks that one Consumer holding the same key
// twice, literally and through a secretRef, still binds: two entries of
// one Consumer are not two matching Consumers.
func TestSameConsumerTwice(t *testing.T) {
	st := newStore(map[secret.Ref]string{refPrevious: keyAcme})
	f := build(t, buildEnv(nil, st, consumer("acme", "", literal("primary", keyAcme), held(refPrevious))))
	x := withKey(keyAcme)
	wantAllow(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, "acme")
}

// TestRevokedKey checks Figure 2's order for API keys: a key the lookup
// verified is checked against the revocation list before binding (401
// RZ-AUTH-004, ambiguous keys included), and a key no Consumer holds is
// RZ-AUTH-002 without consulting the list.
func TestRevokedKey(t *testing.T) {
	rv := &revoker{digests: map[[32]byte]bool{identity.DigestString(keyAcme): true, identity.DigestString(keyOld): true}}
	env := buildEnv(nil, nil,
		consumer("acme", "", literal("primary", keyAcme)),
		consumer("globex", "", literal("primary", keyGlobex), literal("shared", keyOld)),
		consumer("initech", "", literal("shared", keyOld)),
	)
	flt, err := NewFactory(rv).Build(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	f := flt.(*Filter)
	defer func() { _ = f.Close() }()
	x := withKey(keyAcme)
	wantDeny(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeRevoked, defaultChallenge)
	x = withKey(keyOld)
	wantDeny(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeRevoked, defaultChallenge)
	x = withKey(keyGlobex)
	wantAllow(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, "globex")
	calls := rv.calls
	x = withKey(keyNew)
	wantDeny(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid, defaultChallenge)
	if rv.calls != calls {
		t.Fatal("the Revoker was consulted for a key no Consumer holds")
	}
}

// TestSecondAuthentication checks Security rule 2: when an earlier
// auth-class Policy authenticated the request, a valid key is 401
// RZ-AUTH-002 and the first identity stays.
func TestSecondAuthentication(t *testing.T) {
	f := build(t, buildEnv(nil, nil, consumer("acme", "", literal("primary", keyAcme))))
	x := withKey(keyAcme)
	first := &filter.Identity{Method: filter.MethodJWT, Policy: "jwt-default"}
	x.Ident = first
	r := f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if r.Outcome != filter.Respond || r.Response.Code != auth.CodeInvalid || r.Response.Status != http.StatusUnauthorized || x.Ident != first {
		t.Fatalf("second authentication = %+v (response %+v)", r, r.Response)
	}
}

// TestSecretRefKeyRotation checks "secretRef-held keys are hashed at
// load": the Filter binds the current value of the reference, and after
// the resolver publishes a rotation the new value binds and the old one is
// RZ-AUTH-002, with no new Revision.
func TestSecretRefKeyRotation(t *testing.T) {
	st := newStore(map[secret.Ref]string{refPrevious: keyOld})
	f := build(t, buildEnv(nil, st, consumer("acme", "", held(refPrevious))))
	x := withKey(keyOld)
	wantAllow(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, "acme")

	st.rotate(refPrevious, keyNew)
	x = withKey(keyNew)
	wantAllow(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, "acme")
	x = withKey(keyOld)
	wantDeny(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid, defaultChallenge)
}

// TestRemovalInFileMode checks the M1 revocation of an API key in file
// mode ("Removed; CI and Hot Reload"): the Filter built for the Revision
// without the key, carried over from the previous one with an unchanged
// Policy, rejects the removed literal and secretRef-held keys with
// RZ-AUTH-002 and binds the remaining key, while the previous snapshot's
// Filter keeps serving its in-flight requests until it is closed.
func TestRemovalInFileMode(t *testing.T) {
	st := newStore(map[secret.Ref]string{refPrevious: keyOld})
	before := build(t, buildEnv(nil, st, consumer("acme", "", literal("primary", keyAcme), held(refPrevious), literal("next", keyNew))))
	envAfter := buildEnv(nil, st, consumer("acme", "", literal("next", keyNew)))
	envAfter.Previous = before
	after := build(t, envAfter)

	for _, key := range []string{keyAcme, keyOld} {
		x := withKey(key)
		wantDeny(t, after.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid, defaultChallenge)
		x = withKey(key)
		wantAllow(t, before.Handle(t.Context(), phase.OnRequestHeaders, x), x, "acme")
	}
	x := withKey(keyNew)
	wantAllow(t, after.Handle(t.Context(), phase.OnRequestHeaders, x), x, "acme")

	// The previous snapshot retires: its secretRef watch stops, and the
	// new Filter, which holds no secretRef key, is unaffected.
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}
	if st.live() != 0 {
		t.Fatalf("watches = %d after the previous snapshot retired, want 0", st.live())
	}
	x = withKey(keyNew)
	wantAllow(t, after.Handle(t.Context(), phase.OnRequestHeaders, x), x, "acme")
}

// TestUnchangedPolicyBindsNewConsumers checks that a Filter carried over
// with an unchanged Policy binds the new snapshot's Consumer views, and
// keeps following the carried-over secretRef key, rotations included.
func TestUnchangedPolicyBindsNewConsumers(t *testing.T) {
	st := newStore(map[secret.Ref]string{refPrevious: keyOld})
	before := build(t, buildEnv(nil, st, consumer("acme", "silver", held(refPrevious))))
	envAfter := buildEnv(nil, st, consumer("acme", "gold", held(refPrevious)))
	envAfter.Previous = before
	after := build(t, envAfter)
	if after.CredentialIndex().KeyIndex() != before.CredentialIndex().KeyIndex() {
		t.Fatal("the secretRef key index was not carried over")
	}
	_ = before.Close()
	st.rotate(refPrevious, keyNew)
	x := withKey(keyNew)
	id := wantAllow(t, after.Handle(t.Context(), phase.OnRequestHeaders, x), x, "acme")
	if id.Consumer.Tier != "gold" || id.Consumer != envAfter.Consumers["acme"] {
		t.Fatalf("bound Consumer view %+v, want the new snapshot's", id.Consumer)
	}
}

// TestCloseReleasesIndex checks that Close drops the Filter's reference on
// the credential index, stopping its secretRef watches once no Filter
// holds it, and is idempotent.
func TestCloseReleasesIndex(t *testing.T) {
	st := newStore(map[secret.Ref]string{refPrevious: keyOld})
	env := buildEnv(nil, st, consumer("acme", "", held(refPrevious)))
	a := build(t, env)
	b := build(t, env)
	if a.CredentialIndex() != b.CredentialIndex() {
		t.Fatal("the snapshot's Filters do not share one index")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil || st.live() != 1 {
		t.Fatalf("watches = %d after one holder closed twice, want 1", st.live())
	}
	if err := b.Close(); err != nil || st.live() != 0 {
		t.Fatalf("watches = %d after every holder closed, want 0", st.live())
	}
}

// TestBuildErrors checks Build's rejections: a config of another type is
// RZ-CFG-005, a secretRef-held key the secret table lacks is RZ-CFG-026,
// and an ended compile context is its error, without a code.
func TestBuildErrors(t *testing.T) {
	_, err := NewFactory(nil).Build(t.Context(), buildEnv(&v1alpha1.AuthBasicConfig{}, nil))
	if code, _ := errcode.CodeOf(err); code != "RZ-CFG-005" {
		t.Fatalf("wrong config type: %v, want RZ-CFG-005", err)
	}
	_, err = NewFactory(nil).Build(t.Context(), buildEnv(nil, newStore(map[secret.Ref]string{}), consumer("acme", "", held(refPrevious))))
	if code, _ := errcode.CodeOf(err); code != "RZ-CFG-026" {
		t.Fatalf("unresolved secretRef key: %v, want RZ-CFG-026", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = NewFactory(nil).Build(ctx, buildEnv(nil, nil))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled compile: %v", err)
	}
	if _, ok := errcode.CodeOf(err); ok {
		t.Fatal("a canceled compile carries an RZ code")
	}
}

// TestOtherPhasesContinue checks that the Filter acts only in
// onRequestHeaders: elsewhere it neither authenticates nor strips.
func TestOtherPhasesContinue(t *testing.T) {
	f := build(t, buildEnv(nil, nil, consumer("acme", "", literal("primary", keyAcme))))
	for _, p := range []phase.Phase{phase.OnRequestBody, phase.OnRoute, phase.OnResponse, phase.OnLog} {
		x := withKey(keyAcme)
		if r := f.Handle(t.Context(), p, x); r.Outcome != filter.Continue || x.Ident != nil || x.Hdr.Get("X-Api-Key") != keyAcme {
			t.Fatalf("Handle(%v) = %+v", p, r)
		}
	}
}

// TestDecisionsCounted checks that ruralz_auth_decisions_total counts an
// allow per authenticated request and a deny per rejection by code.
func TestDecisionsCounted(t *testing.T) {
	d := &decisions{}
	env := buildEnv(nil, nil, consumer("acme", "", literal("primary", keyAcme)))
	env.Metrics = &emit.PolicyMetrics{Auth: d}
	f := build(t, env)
	for _, x := range []*filtertest.Exchange{withKey(keyAcme), withKey(keyNew), exchange(nil), withKey(keyAcme)} {
		f.Handle(t.Context(), phase.OnRequestHeaders, x)
	}
	if d.allow != 2 || d.deny[auth.CodeInvalid] != 1 || d.deny[auth.CodeMissing] != 1 {
		t.Fatalf("decisions = %d allow, %v deny", d.allow, d.deny)
	}
}
