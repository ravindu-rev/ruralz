// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwt/jwks"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// headerAuthorization is the credential field, in canonical form.
const headerAuthorization = "Authorization"

// errNoKeys is the cause of RZ-AUTH-006: the issuer has no usable keys,
// never fetched or expired after failed refreshes.
var errNoKeys = errors.New("jwt: no usable keys for the issuer")

// keySource is what the Filter needs of one issuer's key set: a
// *jwks.Handle.
type keySource interface {
	Keys() *jwks.Keys
	Nudge(now time.Time)
	Release()
}

// issuer is one config.issuers entry.
type issuer struct {
	name      string
	audiences []string
	keys      keySource
}

// Filter is a compiled auth.jwt Policy. It is immutable after Build and
// safe for concurrent use; Close releases its key sets and credential
// index.
type Filter struct {
	*auth.Decider
	policy  string
	issuers map[string]*issuer
	order   []*issuer
	index   *identity.Index
	revoker identity.Revoker
	codec   *codec
	// age is the Policy's ruralz_auth_jwks_age_seconds; nil records
	// nothing. ageAt is the Unix second of the last update.
	age    emit.Gauge
	ageAt  atomic.Int64
	closed atomic.Bool
}

var (
	_ filter.Filter    = (*Filter)(nil)
	_ filter.Closer    = (*Filter)(nil)
	_ auth.Challenger  = (*Filter)(nil)
	_ auth.IndexHolder = (*Filter)(nil)
	_ keySource        = (*jwks.Handle)(nil)
)

// Handle authenticates the client request in onRequestHeaders and
// continues in every other Phase. The decision follows Security and
// identity, Figure 2:
//
//   - no Authorization field, or a scheme other than Bearer: 401
//     RZ-AUTH-001;
//   - more than one Authorization line, a token that is not a JWS compact
//     token of at most MaxTokenBytes, an alg other than RS256, PS256, ES256
//     or EdDSA, a crit header, a payload that is not a JSON object: 401
//     RZ-AUTH-002;
//   - an iss naming no configured issuer: 401 RZ-AUTH-003;
//   - the issuer has no usable keys: CannotDecide with RZ-AUTH-006, which
//     failureMode closed answers with 401 RZ-AUTH-006, and a fetch is
//     nudged;
//   - no key of the token's kid and algorithm family, or, for a token
//     without a kid, no key of that family: 401 RZ-AUTH-005, and a fetch
//     is nudged (at most one per issuer per jwks.RefreshInterval);
//   - a key whose alg differs from the token's, or a signature that does
//     not verify; for a token without a kid, which tries every key of its
//     family (at most jwks.MaxKeys verifications), none that verifies: 401
//     RZ-AUTH-002;
//   - exp absent, passed or too far ahead, nbf ahead, aud not accepted:
//     401 RZ-AUTH-003;
//   - revoked (identity.Revoker): 401 RZ-AUTH-004;
//   - two Consumers bound, or an earlier Policy already authenticated
//     the request: 401 RZ-AUTH-002.
//
// Otherwise the request identity is set: method jwt, the verified claims,
// the bound Consumer or none. The Authorization field is forwarded
// upstream unchanged (Security and identity, "Request hardening").
func (f *Filter) Handle(_ context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != phase.OnRequestHeaders {
		return filter.Next()
	}
	value, lines := auth.Single(x.Header(), headerAuthorization)
	switch {
	case lines == 0:
		return f.Deny(x, auth.CodeMissing)
	case lines > 1:
		return f.Deny(x, auth.CodeInvalid)
	}
	raw, bearer := bearerToken(value)
	if !bearer {
		return f.Deny(x, auth.CodeMissing)
	}
	s := f.codec.getScratch()
	defer f.codec.putScratch(s)
	t, err := f.codec.parse(raw, s)
	if err != nil {
		return f.Deny(x, auth.CodeInvalid)
	}
	return f.verify(x, &t, s)
}

// verify selects the issuer and key of a parsed token, checks its
// signature, claims and revocation, and binds it.
func (f *Filter) verify(x filter.Exchange, t *parsed, s *scratch) filter.Result {
	iss, _ := t.claims.StringClaim("iss")
	is := f.issuers[iss]
	if is == nil {
		return f.Deny(x, auth.CodeClaims)
	}
	now := x.Now()
	f.recordAge(now)
	ks := is.keys.Keys()
	if ks == nil || ks.Expired(now) {
		is.keys.Nudge(now)
		return f.Undecided(x, auth.CodeNoKeys, errNoKeys)
	}
	k, code := f.signer(ks, t, s)
	switch code {
	case "":
	case auth.CodeUnknownKID:
		is.keys.Nudge(now)
		return f.Deny(x, code)
	default:
		return f.Deny(x, code)
	}
	if !t.claims.valid(is.audiences, now) {
		return f.Deny(x, auth.CodeClaims)
	}
	sub, _ := t.claims.StringClaim("sub")
	jti, _ := t.claims.StringClaim("jti")
	if f.revoker.RevokedJWT(iss, k.ID, jti, sub, t.claims.issuedAt()) {
		return f.Deny(x, auth.CodeRevoked)
	}
	c, ambiguous := f.index.JWT(iss, t.claims)
	if ambiguous {
		return f.Deny(x, auth.CodeInvalid)
	}
	return f.Authenticated(x, auth.NewJWTIdentity(f.policy, c, t.claims, iss, sub))
}

// signer returns the key of ks whose signature check passes for t, or the
// code of the failure. A token with a kid checks the key of that kid and
// its algorithm family: none is RZ-AUTH-005. A token without a kid tries
// every key of its family in document order, so at most jwks.MaxKeys
// verifications, and the first that verifies wins: no key of the family
// is RZ-AUTH-005. A key whose JWK names another alg is never used. No key
// that verifies is RZ-AUTH-002.
func (f *Filter) signer(ks *jwks.Keys, t *parsed, s *scratch) (*jwks.Key, string) {
	if t.kid != "" {
		k, ok := ks.Lookup(t.kid, t.family)
		switch {
		case !ok:
			return nil, auth.CodeUnknownKID
		case f.verifies(k, t, s):
			return k, ""
		default:
			return nil, auth.CodeInvalid
		}
	}
	keys := ks.OfFamily(t.family)
	if len(keys) == 0 {
		return nil, auth.CodeUnknownKID
	}
	for _, k := range keys {
		if f.verifies(k, t, s) {
			return k, ""
		}
	}
	return nil, auth.CodeInvalid
}

// verifies reports whether k may verify t, its JWK naming no alg or t's,
// and t's signature verifies with it.
func (f *Filter) verifies(k *jwks.Key, t *parsed, s *scratch) bool {
	if k.Alg != "" && k.Alg != t.alg {
		return false
	}
	return f.codec.verifier(t.alg).Verify(k.Public, s.input(t), s.sig) == nil
}

// recordAge sets ruralz_auth_jwks_age_seconds to the age of the Policy's
// oldest key set at now, at most once per second; issuers never fetched
// are left out.
func (f *Filter) recordAge(now time.Time) {
	if f.age == nil {
		return
	}
	sec := now.Unix()
	if f.ageAt.Load() == sec {
		return
	}
	f.ageAt.Store(sec)
	oldest := time.Duration(-1)
	for _, is := range f.order {
		if ks := is.keys.Keys(); ks != nil {
			oldest = max(oldest, now.Sub(ks.Fetched()))
		}
	}
	if oldest >= 0 {
		f.age.Set(int64(oldest / time.Second))
	}
}

// CredentialIndex returns the credential index the Filter binds with
// (auth.IndexHolder), so the next snapshot carries its KeyIndex over.
func (f *Filter) CredentialIndex() *identity.Index { return f.index }

// Close releases the Filter's key sets and credential index; it is
// idempotent.
func (f *Filter) Close() error {
	if !f.closed.CompareAndSwap(false, true) {
		return nil
	}
	for _, is := range f.order {
		is.keys.Release()
	}
	if f.index != nil {
		f.index.Release()
	}
	return nil
}
