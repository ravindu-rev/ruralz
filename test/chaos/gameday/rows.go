// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package gameday

import (
	"slices"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// FC-16's key-set timeline (docs/architecture/08-security-and-identity.md
// "JWT and OIDC"): the mock IdP answers with Cache-Control max-age=300,
// the floor of the 5 minutes to 6 hours clamp, so a key set lives
// FC16Lifetime and is refreshed at its half-life, FC16HalfLife; a failed
// fetch is retried after FC16Retry, which also spaces the refetches an
// unknown kid or an issuer without usable keys triggers; and a fetch ends
// within FC16FetchBound, the bound of a compile's prefetch (target).
const (
	FC16CacheControl = "max-age=300"
	FC16Lifetime     = 5 * time.Minute
	FC16HalfLife     = FC16Lifetime / 2
	FC16Retry        = 30 * time.Second
	FC16FetchBound   = 5 * time.Second
)

// FC16Fetched returns the times the mock IdP recorded the JWKS fetches it
// answered with status, recorded in [from, to), oldest first. The IdP
// records a fetch once it has answered it.
func FC16Fetched(fetches []mockidp.Fetch, status int, from, to time.Time) []time.Time {
	var out []time.Time
	for _, f := range fetches {
		if f.Status == status && !f.Time.Before(from) && f.Time.Before(to) {
			out = append(out, f.Time)
		}
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out
}

// FC16Expiry returns the Node times between which the key sets fetched by
// the answers the IdP recorded at the real instants fetched expire, each
// lifetime after the Node read its clock on parsing the answer. A Node may
// have parsed an answer up to one clock advance before the IdP recorded
// it, so the earliest expiry counts from the first fetch's cause span
// (chaos.Run.CauseSpan); it parsed it at the latest FC16FetchBound after,
// so the latest counts from the last fetch's Node span then
// (chaos.Run.NodeSpan). ok is false without a fetch.
func FC16Expiry(fetched []time.Time, cause, span func(time.Time) (lo, hi time.Time), lifetime time.Duration) (earliest, latest time.Time, ok bool) {
	if len(fetched) == 0 {
		return time.Time{}, time.Time{}, false
	}
	first, last := slices.MinFunc(fetched, time.Time.Compare), slices.MaxFunc(fetched, time.Time.Compare)
	lo, _ := cause(first)
	_, hi := span(last.Add(FC16FetchBound))
	return lo.Add(lifetime), hi.Add(lifetime), true
}

// FC16Counts sorts requests by outcome: admitted (2xx), answered with the
// code asked for, or any other outcome, a transport failure included.
type FC16Counts struct {
	Admitted, Code, Other int64
}

// FC16Decided counts the requests of reqs a Node surely decided at Node
// times within [from, to): those it may have read its clock for no
// earlier than from when they were sent, and no later than before to when
// they completed (span maps a real instant to the Node times a Node may
// have read then).
func FC16Decided(reqs []chaos.Request, span func(time.Time) (lo, hi time.Time), from, to time.Time, code string) FC16Counts {
	var c FC16Counts
	for _, q := range reqs {
		if q.Sent.IsZero() || q.Done.IsZero() {
			continue
		}
		if lo, _ := span(q.Sent); lo.Before(from) {
			continue
		}
		if _, hi := span(q.Done); !hi.Before(to) {
			continue
		}
		switch {
		case chaos.Admitted(q):
			c.Admitted++
		case code != "" && q.Code == code:
			c.Code++
		default:
			c.Other++
		}
	}
	return c
}

// FC16Switch reads, per Node, the switch of a key's tokens from passing
// to failing with code: when the first request a Node answered with code
// completed, and how many requests a Node admitted that were sent after
// that, which a key set that dropped the key never does. node maps a
// request to the Node its connection reached; requests it maps to "" are
// skipped.
func FC16Switch(reqs []chaos.Request, node func(chaos.Request) string, code string) (first map[string]time.Time, admittedAfter int64) {
	first = map[string]time.Time{}
	for _, q := range reqs {
		if q.Code != code {
			continue
		}
		n := node(q)
		if n == "" {
			continue
		}
		if t, ok := first[n]; !ok || q.Done.Before(t) {
			first[n] = q.Done
		}
	}
	for _, q := range reqs {
		if !chaos.Admitted(q) {
			continue
		}
		if t, ok := first[node(q)]; ok && q.Sent.After(t) {
			admittedAfter++
		}
	}
	return first, admittedAfter
}

// FC20Rose reports whether a sampled counter rose after real instant
// since: it grew from its value then, or its series first appeared after
// since with a positive value, as a counter does whose label set, such as
// one rejection code, is exported only from its first increment.
func FC20Rose(trace []chaos.Point, since time.Time) bool {
	if len(trace) == 0 {
		return false
	}
	if trace[0].Real.After(since) {
		return trace[len(trace)-1].Value > 0
	}
	return chaos.Increase(trace, since, trace[len(trace)-1].Real) > 0
}

// FC20OffByOne returns the Nodes, sorted, whose count of activations
// differs from want: FC-20's recovery is one Hot Reload per Node.
func FC20OffByOne(activations map[string]float64, want float64) []string {
	var out []string
	for n, v := range activations {
		if v != want {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}
