// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Configuration codes of this package. The static checks raise the first
// three at validation; compiling an index raises CodeSchema and
// CodeIterations only for a Revision that skipped validation, and
// CodeUnresolved for a secretRef-held API key that cannot be used.
const (
	// CodeDuplicate: a basic username, certificate subject or uriSan
	// declared by two Consumers.
	CodeDuplicate = "RZ-CFG-035"
	// CodeIterations: basic iterations outside 600,000 to 1,000,000.
	CodeIterations = "RZ-CFG-036"
	// CodeSchema: a malformed credential, a credentials.jwt entry that binds
	// nothing, or an oauthClients list on a Consumer without a jwt issuer.
	CodeSchema = "RZ-CFG-005"
	// CodeUnresolved: a secretRef-held API key that is not resolved or is
	// shorter than MinKeyBytes (interim code, OQ-security-and-identity-23).
	CodeUnresolved = "RZ-CFG-026"
)

// Locator maps a key-aware path inside r to its source position; the
// configuration validator passes one over the resource's positioned tree.
type Locator func(r *hub.Resource, p diag.Path) diag.Location

// first is where a credential value was first declared.
type first struct {
	consumer string
	loc      diag.Location
}

// Check runs the static Consumer checks every binary's validator runs at
// stage H (spec 06 requirement 20; Security and identity, "Basic and mTLS
// schemas"; OQ-security-and-identity-24 (a)):
//
//   - RZ-CFG-035: a credentials.basic username, or a credentials.certificates
//     subject or uriSan, declared by two Consumers, so each credential binds
//     at most one Consumer. It is reported at every later declaration, in
//     canonical Consumer order, with the first one as related location.
//   - RZ-CFG-036: credentials.basic[].iterations outside 600,000 to
//     1,000,000 (an absent value is the default 600,000).
//   - RZ-CFG-005: credentials.oauthClients on a Consumer without
//     credentials.jwt: a client identifier binds only through the
//     Consumer's own jwt issuers.
//   - RZ-CFG-005: a credentials.jwt entry with an empty subject and no
//     claims (`claims: {}` or `subject: ""`). The schema rejects both
//     forms (minLength 1, minProperties 1); this check covers callers that
//     skip schema validation. Such an entry binds no token: the index
//     never reads it as "every token of the issuer".
//
// Duplicate API key hashes and duplicate (issuer, subject) or (issuer,
// clientId) pairs have no static code: the runtime answers them with 401
// RZ-AUTH-002 (spec 06 requirement 18; architecture R-22). A nil loc
// places findings at the resource start.
func Check(b *hub.Bundle, loc Locator, add func(diag.Diagnostic)) {
	if b == nil {
		return
	}
	at := func(r *hub.Resource, p diag.Path) diag.Location {
		if loc == nil {
			return r.Source.Start
		}
		return loc(r, p)
	}
	usernames := map[string]first{}
	subjects := map[string]first{}
	uris := map[string]first{}
	dup := func(seen map[string]first, r *hub.Resource, p diag.Path, what, value string) {
		l := at(r, p)
		f, ok := seen[value]
		switch {
		case !ok:
			seen[value] = first{consumer: r.Name, loc: l}
		case f.consumer != r.Name:
			add(diag.Diagnostic{
				Code: CodeDuplicate, Severity: diag.SeverityError, Location: l,
				Resource: r.ResourceID(), Path: p,
				Message: what + " " + strconv.Quote(value) + " is already declared by Consumer " + strconv.Quote(f.consumer),
				Related: []diag.Related{{Location: f.loc, Message: "declared in"}},
			})
		}
	}
	for _, r := range b.Resources() {
		if r.Kind != v1alpha1.KindConsumer {
			continue
		}
		c, ok := hub.Object[v1alpha1.Consumer](r)
		if !ok {
			continue
		}
		creds := diag.Path{diag.Field("spec"), diag.Field("credentials")}
		cr := &c.Spec.Credentials
		for _, bc := range cr.Basic {
			entry := creds.Append(diag.Field("basic"), diag.Keyed("username", bc.Username))
			dup(usernames, r, entry.Append(diag.Field("username")), "basic username", bc.Username)
			if n := Iterations(bc.Iterations); !ValidIterations(n) {
				p := entry.Append(diag.Field("iterations"))
				add(diag.Diagnostic{
					Code: CodeIterations, Severity: diag.SeverityError, Location: at(r, p),
					Resource: r.ResourceID(), Path: p,
					Message: "iterations " + strconv.Itoa(n) + " is outside 600000 to 1000000",
				})
			}
		}
		for _, cc := range cr.Certificates {
			entry := creds.Append(diag.Field("certificates"), diag.Keyed("name", cc.Name))
			if cc.Subject != "" {
				dup(subjects, r, entry.Append(diag.Field("subject")), "certificate subject", cc.Subject)
			}
			if cc.URISAN != "" {
				dup(uris, r, entry.Append(diag.Field("uriSan")), "certificate uriSan", cc.URISAN)
			}
		}
		for _, j := range cr.JWT {
			if j.Subject != "" || len(j.Claims) > 0 {
				continue
			}
			entry := creds.Append(diag.Field("jwt"), diag.Keyed("issuer", j.Issuer))
			p, what := entry.Append(diag.Field("subject")), "an empty subject"
			if j.Claims != nil {
				p, what = entry.Append(diag.Field("claims")), "an empty claims map"
			}
			add(diag.Diagnostic{
				Code: CodeSchema, Severity: diag.SeverityError, Location: at(r, p),
				Resource: r.ResourceID(), Path: p,
				Message: "jwt binding for issuer " + strconv.Quote(j.Issuer) + " has " + what + " and binds no token: set subject or at least one claim",
			})
		}
		if len(cr.OAuthClients) > 0 && len(cr.JWT) == 0 {
			p := creds.Append(diag.Field("oauthClients"))
			add(diag.Diagnostic{
				Code: CodeSchema, Severity: diag.SeverityError, Location: at(r, p),
				Resource: r.ResourceID(), Path: p,
				Message: "oauthClients needs a credentials.jwt entry: a client identifier binds only through the Consumer's jwt issuers",
			})
		}
	}
}

// Diagnostics runs Check and returns its findings in order.
func Diagnostics(b *hub.Bundle, loc Locator) diag.List {
	var out diag.List
	Check(b, loc, func(d diag.Diagnostic) { out = append(out, d) })
	return out
}
