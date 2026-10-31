// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mtls

import (
	"context"
	"crypto/x509"
	"slices"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// Filter is a compiled auth.mtls Policy for one snapshot. It is immutable
// after Build and safe for concurrent use; Close releases its trust holder
// and credential index.
type Filter struct {
	*auth.Decider
	policy  string
	rules   rules
	index   *identity.Index
	revoker identity.Revoker
	trust   *holder
	closed  atomic.Bool
}

var (
	_ filter.Filter    = (*Filter)(nil)
	_ filter.Closer    = (*Filter)(nil)
	_ auth.Challenger  = (*Filter)(nil)
	_ auth.IndexHolder = (*Filter)(nil)
)

// rules are the compiled config.subjects; both sets empty admit any
// verified leaf.
type rules struct {
	subjects map[string]struct{}
	uris     map[string]struct{}
}

// admit reports whether a leaf with this subject and these URI SANs
// matches a rule exactly.
func (r rules) admit(subject string, uris []string) bool {
	if len(r.subjects) == 0 && len(r.uris) == 0 {
		return true
	}
	if _, ok := r.subjects[subject]; ok {
		return true
	}
	for _, u := range uris {
		if _, ok := r.uris[u]; ok {
			return true
		}
	}
	return false
}

// verdict is the outcome of verifying one connection's client chain for
// one Filter (one Policy in one snapshot) under one trust state. It is
// immutable; the connection's filter.ConnCache keeps it under the Filter.
type verdict struct {
	ts   *trustState
	leaf *x509.Certificate
	// code is "" when the chain is accepted, else the RZ-AUTH code of the
	// rejection.
	code string
	// from and until bound an accepted verdict: the validity of the chain
	// it accepted.
	from, until time.Time
	// retry ends a rejected verdict: the earliest NotBefore of a presented
	// certificate after the verification time, at which the chain is
	// verified again; zero when the rejection does not depend on time.
	retry time.Time
	// subject and consumer are the accepted leaf's RFC 4514 subject and
	// bound Consumer (nil when unbound).
	subject  string
	consumer *expr.Consumer
}

// holds reports whether v still decides a request at now.
func (v *verdict) holds(now time.Time) bool {
	if v.code == "" {
		return !now.Before(v.from) && !now.After(v.until)
	}
	return v.retry.IsZero() || now.Before(v.retry)
}

// Handle authenticates the client request in onRequestHeaders and
// continues in every other Phase. The decision follows Security and
// identity, Figure 2:
//
//   - a cleartext connection, or one whose handshake requested no client
//     certificate: 421 RZ-AUTH-008;
//   - no client certificate presented: 401 RZ-AUTH-001;
//   - a leaf without the clientAuth extended key usage, or a chain that
//     does not verify against config.caCertificate at the request time:
//     401 RZ-AUTH-002;
//   - every verified chain holds a certificate a CRL of config.crl lists,
//     or identity.Revoker revokes the leaf: 401 RZ-AUTH-004;
//   - a leaf matching no config.subjects rule, two Consumers bound, or an
//     earlier Policy already authenticated the request: 401 RZ-AUTH-002.
//
// Otherwise the request identity is set: method mtls, the leaf's RFC 4514
// subject, the bound Consumer or none. Rejections carry no
// WWW-Authenticate challenge and never echo certificate content.
func (f *Filter) Handle(_ context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != phase.OnRequestHeaders {
		return filter.Next()
	}
	conn := x.TLS()
	if conn == nil || !conn.ClientCertRequested {
		return f.Deny(x, auth.CodeNoCertRequest)
	}
	certs := conn.PeerCertificates()
	if len(certs) == 0 || certs[0] == nil {
		return f.Deny(x, auth.CodeMissing)
	}
	v := f.decide(conn.Cache, certs, x.Now())
	if v.code != "" {
		return f.Deny(x, v.code)
	}
	if leaf := certs[0]; f.revoker.RevokedCert(leaf.RawIssuer, leaf.SerialNumber) {
		return f.Deny(x, auth.CodeRevoked)
	}
	return f.Authenticated(x, auth.NewMTLSIdentity(f.policy, v.consumer, v.subject))
}

// decide returns the verdict on certs at now: the one the connection's
// cache keeps for this Filter when it was reached under the current trust
// state for the same leaf and still holds, else a new one, which replaces
// it.
func (f *Filter) decide(cache *filter.ConnCache, certs []*x509.Certificate, now time.Time) *verdict {
	ts := f.trust.state()
	if cache != nil {
		if c, ok := cache.Get(f); ok {
			if v, ok := c.(*verdict); ok && v.ts == ts && v.leaf == certs[0] && v.holds(now) {
				return v
			}
		}
	}
	v := f.verify(ts, certs, now)
	if cache != nil {
		cache.Put(f, v)
	}
	return v
}

// verify checks a presented chain (leaf first, then the intermediates the
// client sent) under ts at now.
func (f *Filter) verify(ts *trustState, certs []*x509.Certificate, now time.Time) *verdict {
	leaf := certs[0]
	v := &verdict{ts: ts, leaf: leaf}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		v.code = auth.CodeInvalid
		return v
	}
	var inter *x509.CertPool
	if len(certs) > 1 {
		inter = x509.NewCertPool()
		for _, c := range certs[1:] {
			if c != nil {
				inter.AddCert(c)
			}
		}
	}
	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots:         ts.roots,
		Intermediates: inter,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		v.code, v.retry = auth.CodeInvalid, retryAt(certs, now)
		return v
	}
	chain := bestUnrevoked(ts, chains)
	if chain == nil {
		v.code = auth.CodeRevoked
		return v
	}
	subject, uris := Subject(leaf), uriSANs(leaf)
	if !f.rules.admit(subject, uris) {
		v.code = auth.CodeInvalid
		return v
	}
	c, ambiguous := f.index.Certificate(subject, uris)
	if ambiguous {
		v.code = auth.CodeInvalid
		return v
	}
	v.subject, v.consumer = subject, c
	v.from, v.until = window(chain)
	return v
}

// bestUnrevoked returns, of the verified chains, the one without a revoked
// certificate that stays valid longest; nil when every chain holds one.
func bestUnrevoked(ts *trustState, chains [][]*x509.Certificate) []*x509.Certificate {
	var best []*x509.Certificate
	var bestUntil time.Time
	for _, ch := range chains {
		if len(ch) == 0 || ts.revoked(ch) {
			continue
		}
		if _, until := window(ch); best == nil || until.After(bestUntil) {
			best, bestUntil = ch, until
		}
	}
	return best
}

// window returns the span in which every certificate of chain is valid.
func window(chain []*x509.Certificate) (from, until time.Time) {
	for i, c := range chain {
		if i == 0 || c.NotBefore.After(from) {
			from = c.NotBefore
		}
		if i == 0 || c.NotAfter.Before(until) {
			until = c.NotAfter
		}
	}
	return from, until
}

// retryAt returns the earliest NotBefore of certs after now, zero when
// none: a chain refused because a certificate was not valid yet is
// verified again from then on, on the same connection.
func retryAt(certs []*x509.Certificate, now time.Time) time.Time {
	var at time.Time
	for _, c := range certs {
		if c != nil && c.NotBefore.After(now) && (at.IsZero() || c.NotBefore.Before(at)) {
			at = c.NotBefore
		}
	}
	return at
}

// CredentialIndex returns the credential index the Filter binds with
// (auth.IndexHolder), so the next snapshot carries its KeyIndex over.
func (f *Filter) CredentialIndex() *identity.Index { return f.index }

// Close releases the Filter's trust holder, whose watches stop with its
// last Filter, and its credential index; it is idempotent.
func (f *Filter) Close() error {
	if !f.closed.CompareAndSwap(false, true) {
		return nil
	}
	f.trust.release()
	if f.index != nil {
		f.index.Release()
	}
	return nil
}
