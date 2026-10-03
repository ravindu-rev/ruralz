// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"strings"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Principal prefixes of unbound identities (spec 06 requirement 22). A
// Consumer name is an RFC 1123 label and never contains ":", so the three
// forms cannot collide.
//
// An unbound identity without a subject (a token whose sub claim is absent,
// not a string or empty; a certificate with an empty subject) has no
// principal: "". Such identities share no key that tells their clients
// apart (client-credentials tokens differing only by client_id or azp,
// SPIFFE leaves identified only by a URI SAN), so a shared cache must
// neither store nor serve responses for an empty principal on an
// authenticated request. Falling back to client_id or azp instead would
// merge every user of one client application whose IdP omits sub.
const (
	// PrincipalJWTPrefix starts the principal of an unbound token.
	PrincipalJWTPrefix = "jwt:"
	// PrincipalMTLSPrefix starts the principal of an unbound certificate.
	PrincipalMTLSPrefix = "mtls:"
)

// ConsumerPrincipal returns the principal of an identity bound to c: its
// name; "" when c is nil.
func ConsumerPrincipal(c *expr.Consumer) string {
	if c == nil {
		return ""
	}
	return c.Name
}

// JWTPrincipal returns the non-secret cache partition key of a verified
// token (spec 06 requirement 22): the bound Consumer's name, else
// "jwt:" + iss + "#" + sub, else "" (no principal) when sub is empty. "%"
// and "#" in iss are percent-encoded so that distinct (iss, sub) pairs
// never share a key.
func JWTPrincipal(c *expr.Consumer, iss, sub string) string {
	if c != nil {
		return c.Name
	}
	if sub == "" {
		return ""
	}
	if strings.ContainsAny(iss, "%#") {
		iss = strings.NewReplacer("%", "%25", "#", "%23").Replace(iss)
	}
	return PrincipalJWTPrefix + iss + "#" + sub
}

// MTLSPrincipal returns the partition key of a verified client
// certificate: the bound Consumer's name, else "mtls:" + the leaf's RFC
// 4514 subject, else "" (no principal) when the subject is empty.
func MTLSPrincipal(c *expr.Consumer, subject string) string {
	if c != nil {
		return c.Name
	}
	if subject == "" {
		return ""
	}
	return PrincipalMTLSPrefix + subject
}
