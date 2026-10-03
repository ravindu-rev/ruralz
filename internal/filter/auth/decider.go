// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Decider records the decisions of one auth- or authz-class Policy and
// builds its answers (spec 06 requirements 3, 7, 8 and 10). Build one per
// Filter at compile time; it is immutable and safe for concurrent use.
// Statuses come from the errcode registry, resolved once here, never from
// literals, so a registry amendment (such as OQ-security-and-identity-9)
// needs no Filter change.
type Decider struct {
	decisions emit.AuthDecisions
	challenge string
	// undecided is the class default code of CannotDecide under closed
	// (auth and authz are closed only).
	undecided string
	// statuses holds the registered status of every code that has a
	// single one, resolved at construction so the request path never
	// calls errcode (Lookup rebuilds the registry per call).
	statuses map[string]int
}

// NewAuth returns the Decider of an auth-class Policy: its 401s carry
// challenge ("" for none, auth.mtls), and CannotDecide fails closed with
// 401 RZ-AUTH-002. m is the Policy's handles (BuildEnv.Metrics); nil
// records nothing.
func NewAuth(m *emit.PolicyMetrics, challenge string) *Decider {
	return newDecider(m, challenge, CodeInvalid)
}

// NewAuthz returns the Decider of an authz-class Policy: no challenge, and
// CannotDecide fails closed with 403 RZ-AUTH-015.
func NewAuthz(m *emit.PolicyMetrics) *Decider {
	return newDecider(m, "", CodeAuthzUndecided)
}

func newDecider(m *emit.PolicyMetrics, challenge, undecided string) *Decider {
	d := &Decider{decisions: nopDecisions{}, challenge: challenge, undecided: undecided, statuses: map[string]int{}}
	if m != nil && m.Auth != nil {
		d.decisions = m.Auth
	}
	for _, c := range errcode.All() {
		if c.Status != 0 {
			d.statuses[c.ID] = c.Status
		}
	}
	return d
}

// Challenge returns the Policy's WWW-Authenticate value (Challenger).
func (d *Decider) Challenge() string { return d.challenge }

// Status returns the registered status of code; 500 for a code without a
// single registered status. It reads only the table built by NewAuth or
// NewAuthz and allocates nothing.
func (d *Decider) Status(code string) int {
	if s, ok := d.statuses[code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Allow counts an allow decision and returns Continue (an authz Policy
// that admits the request).
func (d *Decider) Allow(x filter.Exchange) filter.Result {
	d.decisions.Allow(x.Stripe())
	return filter.Next()
}

// Deny counts a deny with code and returns the rejection: a problem
// document for code at its registered status (the handler writes it with a
// generic title and no detail), carrying the challenge on a 401 and
// Retry-After: 1 on a 429.
func (d *Decider) Deny(x filter.Exchange, code string) filter.Result {
	d.decisions.Deny(x.Stripe(), code)
	status := d.Status(code)
	var h http.Header
	switch status {
	case http.StatusUnauthorized:
		if d.challenge != "" {
			h = http.Header{headerWWWAuthenticate: {d.challenge}}
		}
	case http.StatusTooManyRequests:
		h = http.Header{headerRetryAfter: {"1"}}
	}
	return filter.Deny(status, code, h)
}

// Throttle counts a deny and returns 429 RZ-AUTH-007 with Retry-After in
// whole seconds, rounded up and at least 1 (spec 06 requirement 8).
func (d *Decider) Throttle(x filter.Exchange, retryAfter time.Duration) filter.Result {
	d.decisions.Deny(x.Stripe(), CodeThrottled)
	return filter.Deny(d.Status(CodeThrottled), CodeThrottled, http.Header{headerRetryAfter: {RetryAfter(retryAfter)}})
}

// RetryAfter formats a Retry-After delay in integer seconds, rounded up
// and at least 1.
func RetryAfter(delay time.Duration) string {
	s := int64(1)
	if delay > time.Second {
		s = int64(delay / time.Second)
		if delay%time.Second != 0 {
			s++
		}
	}
	return strconv.FormatInt(s, 10)
}

// Authenticated records a successful authentication: it sets the request
// identity and returns Continue with an allow counted. Security rule 2
// (spec 06 requirement 3): when an earlier auth-class Policy already
// authenticated the request, SetIdentity refuses and the request fails with
// 401 RZ-AUTH-002, whether or not either bound a Consumer. On a leg view,
// where no identity can be set, the Policy cannot decide.
func (d *Decider) Authenticated(x filter.Exchange, id *filter.Identity) filter.Result {
	err := x.SetIdentity(id)
	switch {
	case err == nil:
		d.decisions.Allow(x.Stripe())
		return filter.Next()
	case errors.Is(err, filter.ErrSecondBinding):
		return d.Deny(x, CodeInvalid)
	default:
		return d.Undecided(x, "", err)
	}
}

// Undecided counts the closed failure and returns CannotDecide with code,
// or the class default (401 RZ-AUTH-002 for auth, 403 RZ-AUTH-015 for
// authz) when code is "". err is the cause the executor logs and puts on
// the span; it must not contain credential material.
//
// The result stays CannotDecide, not a Respond, so the Filter Chain
// executor applies the closed failure itself: it maps the SPI sentinels
// (filter.ErrBudget, filter.ErrTooLarge), counts
// ruralz_filter_failures_total and lists the Policy in the access log's
// failure_modes (spec 04 requirements 43 and 44). The executor therefore
// supplies the WWW-Authenticate of the closed failure's 401 (spec 06
// requirement 8): for an auth-class Policy in a request Phase it adds the
// Policy's Challenger.Challenge(), which a Filter embedding this Decider
// provides, as it does for the 401 RZ-AUTH-001 of Security rule 1.
func (d *Decider) Undecided(x filter.Exchange, code string, err error) filter.Result {
	if code == "" {
		code = d.undecided
	}
	d.decisions.Deny(x.Stripe(), code)
	return filter.Undecided(code, err)
}

// nopDecisions records nothing (tests, Policies without handles).
type nopDecisions struct{}

func (nopDecisions) Allow(emit.Stripe)        {}
func (nopDecisions) Deny(emit.Stripe, string) {}
