// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package apikey

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// codeSchema rejects a config that is not an auth.api-key config.
const codeSchema = "RZ-CFG-005"

// Factory builds auth.api-key Filters.
type Factory struct {
	revoker identity.Revoker
}

var _ filter.Factory = (*Factory)(nil)

// NewFactory returns the auth.api-key Factory. revoker is consulted after
// every successful lookup, before the Consumer binds (nil means
// identity.NopRevoker, the M1 wiring).
func NewFactory(revoker identity.Revoker) *Factory {
	if revoker == nil {
		revoker = identity.NopRevoker{}
	}
	return &Factory{revoker: revoker}
}

// Build compiles one auth.api-key Policy: the header it reads and the
// snapshot's credential index. It acquires the index even for a Policy
// that is unchanged since the previous snapshot, because Consumers change
// without the Policy changing; that is how a removed key leaves service.
// A config of another type, or a config.header that cannot carry keys
// (checkHeader, the rule Check applies offline), is RZ-CFG-005; an index
// error keeps its RZ-CFG code (RZ-CFG-026 for a secretRef-held key that is
// unresolved or too short). A ctx that has ended is its error, unchanged.
func (f *Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := configOf(env.Config)
	if err != nil {
		return nil, err
	}
	if err := checkHeader(cfg.Header); err != nil {
		return nil, errcode.Wrap(codeSchema, fmt.Errorf("auth.api-key: config.header: %w", err))
	}
	ix, err := auth.AcquireIndex(env)
	if err != nil {
		return nil, err
	}
	header := auth.APIKeyHeader(cfg.Header)
	flt := &Filter{
		Decider:   auth.NewAuth(env.Metrics, auth.APIKeyChallenge(header)),
		canonical: http.CanonicalHeaderKey(header),
		index:     ix,
		revoker:   f.revoker,
	}
	if env.Policy != nil {
		flt.policy = env.Policy.Metadata.Name
	}
	return flt, nil
}

// configOf returns the typed config. config.header has a schema default
// that the loader materializes only inside a present config, so an absent
// config (nil, or a nil pointer) reads as the default header.
func configOf(cfg any) (*v1alpha1.AuthAPIKeyConfig, error) {
	switch c := cfg.(type) {
	case nil:
		return &v1alpha1.AuthAPIKeyConfig{}, nil
	case *v1alpha1.AuthAPIKeyConfig:
		if c == nil {
			return &v1alpha1.AuthAPIKeyConfig{}, nil
		}
		return c, nil
	case v1alpha1.AuthAPIKeyConfig:
		return &c, nil
	default:
		return nil, errcode.Errorf(codeSchema, "auth.api-key: config is %T, not an auth.api-key config", cfg)
	}
}

// Filter is a compiled auth.api-key Policy. It is immutable after Build
// and safe for concurrent use; Close releases its credential index.
type Filter struct {
	*auth.Decider
	policy string
	// canonical is config.header in http.CanonicalHeaderKey form.
	canonical string
	index     *identity.Index
	revoker   identity.Revoker
	closed    atomic.Bool
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
//   - no field line of config.header, or an empty value: 401 RZ-AUTH-001;
//   - more than one field line, in any case: 401 RZ-AUTH-002;
//   - a key whose SHA-256 digest no Consumer holds: 401 RZ-AUTH-002;
//   - a held key the Revoker lists: 401 RZ-AUTH-004;
//   - a key two Consumers hold, or an earlier Policy already authenticated
//     the request: 401 RZ-AUTH-002.
//
// Otherwise the request identity is set: method api-key, the bound
// Consumer, its name as principal. Every field line of config.header is
// removed from the live request headers before the decision, so the key
// never reaches the Upstream ("Request hardening").
func (f *Filter) Handle(_ context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != phase.OnRequestHeaders {
		return filter.Next()
	}
	h := x.Header()
	key, lines := auth.Single(h, f.canonical)
	auth.Strip(h, f.canonical)
	switch {
	case lines == 0 || (lines == 1 && key == ""):
		return f.Deny(x, auth.CodeMissing)
	case lines > 1:
		return f.Deny(x, auth.CodeInvalid)
	}
	digest := identity.DigestString(key)
	c, ambiguous := f.index.APIKey(digest)
	if c == nil && !ambiguous {
		return f.Deny(x, auth.CodeInvalid)
	}
	if f.revoker.RevokedAPIKey(digest) {
		return f.Deny(x, auth.CodeRevoked)
	}
	if ambiguous {
		return f.Deny(x, auth.CodeInvalid)
	}
	return f.Authenticated(x, auth.NewIdentity(filter.MethodAPIKey, f.policy, c))
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
