// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package basic

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// headerAuthorization is the credential field, in canonical form.
const headerAuthorization = "Authorization"

// codeSchema rejects a config that is not an auth.basic config.
const codeSchema = "RZ-CFG-005"

// ErrNoThrottle reports a Factory built without the Node's throttle.
var ErrNoThrottle = errors.New("basic: no throttle")

// Factory builds auth.basic Filters over the Node's throttle.
type Factory struct {
	throttle *Throttle
	revoker  identity.Revoker
}

var _ filter.Factory = (*Factory)(nil)

// NewFactory returns the auth.basic Factory. throttle is the Node's
// throttle, which the ruralzd supervisor runs as a filter.Component;
// revoker is consulted after every verification and on every success
// cache hit (nil means identity.NopRevoker, the M1 wiring).
func NewFactory(throttle *Throttle, revoker identity.Revoker) (*Factory, error) {
	if throttle == nil {
		return nil, ErrNoThrottle
	}
	if revoker == nil {
		revoker = identity.NopRevoker{}
	}
	return &Factory{throttle: throttle, revoker: revoker}, nil
}

// Build compiles one auth.basic Policy over the snapshot's credential
// index, which it acquires even for a Policy unchanged since the previous
// snapshot, because Consumers change without the Policy changing. A config
// of another type is RZ-CFG-005; an index error keeps its RZ-CFG code
// (RZ-CFG-005 for a malformed hash, RZ-CFG-036 for iterations out of
// range, when validation was skipped). A ctx that has ended is its error,
// unchanged.
func (f *Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch c := env.Config.(type) {
	case nil, *v1alpha1.AuthBasicConfig, v1alpha1.AuthBasicConfig:
	default:
		return nil, errcode.Errorf(codeSchema, "auth.basic: config is %T, not an auth.basic config", c)
	}
	ix, err := auth.AcquireIndex(env)
	if err != nil {
		return nil, err
	}
	flt := &Filter{
		Decider:  auth.NewAuth(env.Metrics, auth.ChallengeBasic),
		index:    ix,
		throttle: f.throttle,
		revoker:  f.revoker,
	}
	if env.Policy != nil {
		flt.policy = env.Policy.Metadata.Name
	}
	return flt, nil
}

// Filter is a compiled auth.basic Policy. It is immutable after Build and
// safe for concurrent use; Close releases its credential index, never the
// Node's throttle.
type Filter struct {
	*auth.Decider
	policy   string
	index    *identity.Index
	throttle *Throttle
	revoker  identity.Revoker
	closed   atomic.Bool
}

var (
	_ filter.Filter    = (*Filter)(nil)
	_ filter.Closer    = (*Filter)(nil)
	_ auth.Challenger  = (*Filter)(nil)
	_ auth.IndexHolder = (*Filter)(nil)
)

// Handle authenticates the client request in onRequestHeaders and
// continues in every other Phase. The decision follows Security and
// identity, Figure 2:
//
//   - no Authorization field, or a scheme other than Basic: 401
//     RZ-AUTH-001;
//   - more than one Authorization line, in any case, or a Basic value that
//     is not base64 of user-id ":" password: 401 RZ-AUTH-002;
//   - a success cache miss the throttle refuses: 429 RZ-AUTH-007 with
//     Retry-After, before any hash;
//   - a wrong password, or a username no Consumer (or two) declares, after
//     a hash: 401 RZ-AUTH-002;
//   - a verified or cached pair whose username the Revoker lists: 401
//     RZ-AUTH-004;
//   - an earlier Policy already authenticated the request: 401
//     RZ-AUTH-002.
//
// A request that ends while waiting in the throttle is CannotDecide with
// the class default, 401 RZ-AUTH-002. Otherwise the request identity is
// set: method basic, the bound Consumer, its name as principal. Every
// Authorization line is removed from the live request headers before the
// decision, so the password never reaches the Upstream ("Request
// hardening").
func (f *Filter) Handle(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != phase.OnRequestHeaders {
		return filter.Next()
	}
	h := x.Header()
	value, lines := auth.Single(h, headerAuthorization)
	auth.Strip(h, headerAuthorization)
	switch {
	case lines == 0:
		return f.Deny(x, auth.CodeMissing)
	case lines > 1:
		return f.Deny(x, auth.CodeInvalid)
	}
	token, basic := basicToken(value)
	if !basic {
		return f.Deny(x, auth.CodeMissing)
	}
	decoded, colon, ok := decodeUserPass(token)
	if !ok {
		return f.Deny(x, auth.CodeInvalid)
	}
	defer clear(decoded)
	username := string(decoded[:colon])
	cred, ambiguous := f.index.Basic(username)
	if ambiguous {
		cred = nil
	}
	o := f.throttle.authenticate(ctx, x.Stripe(), x.Source().IP, decoded, colon, cred)
	switch o.verdict {
	case verdictValid:
	case verdictThrottled:
		return f.Throttle(x, o.retry)
	case verdictAborted:
		return f.Undecided(x, "", o.err)
	default:
		return f.Deny(x, auth.CodeInvalid)
	}
	if f.revoker.RevokedBasic(username) {
		return f.Deny(x, auth.CodeRevoked)
	}
	return f.Authenticated(x, auth.NewIdentity(filter.MethodBasic, f.policy, cred.Consumer))
}

// basicToken returns the credentials of an Authorization value whose
// scheme is Basic, matched case-insensitively (RFC 9110 section 11.1),
// read as auth.jwt reads Bearer: spaces and tabs around the value and
// spaces after the scheme are ignored. basic is false for another scheme.
func basicToken(v string) (token string, basic bool) {
	scheme, rest, _ := strings.Cut(strings.Trim(v, " \t"), " ")
	if !strings.EqualFold(scheme, "Basic") {
		return "", false
	}
	return strings.TrimLeft(rest, " "), true
}

// decodeUserPass decodes RFC 7617 credentials: padded base64 (RFC 4648
// section 4) of user-id ":" password, split at the first colon, since a
// user-id holds none. ok is false for an empty token, invalid base64 or no
// colon; the caller clears decoded after use.
func decodeUserPass(token string) (decoded []byte, colon int, ok bool) {
	if token == "" {
		return nil, 0, false
	}
	b, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return nil, 0, false
	}
	colon = bytes.IndexByte(b, ':')
	if colon < 0 {
		clear(b)
		return nil, 0, false
	}
	return b, colon, true
}

// CredentialIndex returns the credential index the Filter binds with
// (auth.IndexHolder), so the next snapshot carries its KeyIndex over.
func (f *Filter) CredentialIndex() *identity.Index { return f.index }

// Close releases the Filter's credential index; it is idempotent.
func (f *Filter) Close() error {
	if !f.closed.CompareAndSwap(false, true) {
		return nil
	}
	if f.index != nil {
		f.index.Release()
	}
	return nil
}
