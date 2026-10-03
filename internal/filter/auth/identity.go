// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/identity"
)

// NewIdentity returns the identity of a successful auth.api-key or
// auth.basic authentication (spec 06 requirement 6): method is
// filter.MethodAPIKey or filter.MethodBasic, policy the Policy's name, c
// the bound Consumer (both types always bind one), claims the empty map
// and the principal the Consumer's name (requirement 22).
func NewIdentity(method, policy string, c *expr.Consumer) *filter.Identity {
	return &filter.Identity{
		Method:    method,
		Policy:    policy,
		Claims:    EmptyClaims(),
		Consumer:  c,
		Principal: identity.ConsumerPrincipal(c),
	}
}

// NewJWTIdentity returns the identity of a verified token: claims is the
// verified payload (the empty map when nil), c the bound Consumer or nil,
// sub the string claim sub ("" when absent or not a string), and the
// principal the Consumer's name, else "jwt:<iss>#<sub>" (with "%" and "#"
// in iss percent-encoded), else "" for an unbound token without a sub: no
// partition key, so a shared cache neither stores nor serves for it
// (identity.JWTPrincipal).
func NewJWTIdentity(policy string, c *expr.Consumer, claims expr.Value, iss, sub string) *filter.Identity {
	if claims == nil {
		claims = EmptyClaims()
	}
	return &filter.Identity{
		Method:    filter.MethodJWT,
		Policy:    policy,
		Claims:    claims,
		Consumer:  c,
		Principal: identity.JWTPrincipal(c, iss, sub),
	}
}

// NewMTLSIdentity returns the identity of a verified client certificate:
// subject is the leaf's RFC 4514 subject, c the bound Consumer or nil, and
// the principal the Consumer's name, else "mtls:<subject>", else "" for an
// unbound leaf with an empty subject (identity.MTLSPrincipal).
func NewMTLSIdentity(policy string, c *expr.Consumer, subject string) *filter.Identity {
	return &filter.Identity{
		Method:      filter.MethodMTLS,
		Policy:      policy,
		Claims:      EmptyClaims(),
		Consumer:    c,
		CertSubject: subject,
		Principal:   identity.MTLSPrincipal(c, subject),
	}
}

// EmptyClaims returns auth.claims for the methods other than jwt: an empty
// map (spec 06 requirement 6), never nil. It allocates nothing.
func EmptyClaims() expr.Value { return emptyClaims{} }

// emptyClaims is the JSON object {}.
type emptyClaims struct{}

// IsNull reports false: {} is not null.
func (emptyClaims) IsNull() bool { return false }

// AppendBody appends {}.
func (emptyClaims) AppendBody(dst []byte) ([]byte, error) { return append(dst, '{', '}'), nil }

// Native returns an empty map[string]any.
func (emptyClaims) Native() (any, error) { return map[string]any{}, nil }
