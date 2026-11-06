// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"net/http"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// CodeUpstreamReset is the code of a leg whose attempt was reset before a
// response and not retried, answered 502. A retry the retry budget refuses
// answers it, since over budget the original failure returns
// (docs/architecture/09-traffic-management-and-resilience.md "Retry
// budget").
const CodeUpstreamReset = "RZ-UP-004"

// ResetFailures are the failed requests of a phase in which an Endpoint
// resets connections before responses, split by the retry budget rule:
// idempotent requests succeed on retries while the budget has room, and a
// retry it refuses answers 502 RZ-UP-004
// (docs/engineering/03-testing-and-quality-strategy.md "Chaos testing").
type ResetFailures struct {
	// Resets counts the failed requests answered 502 RZ-UP-004.
	Resets int64
	// Other counts every other failed request: other statuses and codes,
	// transport failures, missed and canceled requests.
	Other int64
	// Last is when the latest of the Resets answers completed; zero
	// without one.
	Last time.Time
}

// PhaseResetFailures splits the failed requests of st due in phase p,
// counted from the phase's recorded start: the phase's fault is injected
// after it, and a request due in between can still meet the fault.
func PhaseResetFailures(st *Stream, p Phase) ResetFailures {
	var f ResetFailures
	for _, q := range st.Requests() {
		if q.Intended.Before(p.Start) || !q.Intended.Before(p.End) || !st.Failed(q) {
			continue
		}
		if q.Status != http.StatusBadGateway || q.Code != CodeUpstreamReset {
			f.Other++
			continue
		}
		f.Resets++
		if q.Done.After(f.Last) {
			f.Last = q.Done
		}
	}
	return f
}

// Criteria returns the phase's retry budget criteria, named after prefix:
// no failed request but those answered 502 RZ-UP-004, and no more of those
// than refusals, the retries the Nodes' retry budget refused over the
// phase (Run.RetryBudgetRefusals).
func (f ResetFailures) Criteria(prefix string, refusals float64) []Criterion {
	return []Criterion{
		Check(prefix+": failures other than 502 RZ-UP-004", Functional, float64(f.Other), EQ, 0),
		Check(prefix+": 502 RZ-UP-004 answers, at most the retry budget's refusals", Functional, float64(f.Resets), LE, refusals),
	}
}

// RetryBudgetRefusals returns the retries the retry budget refused on
// every Node together, ruralz_upstream_retry_budget_exhausted_total over
// match, over phase p, whose failed requests are f: from each Node's last
// sample answered by the phase's start to its first sample begun once the
// phase had ended and its last 502 RZ-UP-004 answer had arrived
// (IncreaseCovering), so the count holds every refusal behind those
// answers, a Node counting a refusal before its leg answers.
func (r *Run) RetryBudgetRefusals(match promtext.Labels, p Phase, f ResetFailures) float64 {
	to := p.End
	if f.Last.After(to) {
		to = f.Last
	}
	total := 0.0
	for _, n := range r.Sampler.Nodes() {
		total += IncreaseCovering(r.Sampler.Trace(n, catalog.UpstreamRetryBudgetExhaustedTotal, match), p.Start, to)
	}
	return total
}

// IncreaseCovering returns how much a counter grew between two samples
// that bracket every increment made in [from, to]: the last sample
// answered by from, which holds none made after from, and the first sample
// begun at or after to, which holds every one made before to. Increase
// starts from the last sample begun by from instead, which, answered after
// from, can hold an increment made after it. A counter reset counts from
// 0. Without a sample answered by from the count starts from 0, and
// without one begun at or after to it ends at the last sample.
func IncreaseCovering(trace []Point, from, to time.Time) float64 {
	if len(trace) == 0 {
		return 0
	}
	end := len(trace) - 1
	for i, p := range trace {
		if !p.Real.Before(to) {
			end = i
			break
		}
	}
	base, prev := -1, 0.0
	for i, p := range trace[:end+1] {
		if !p.Done.After(from) {
			base, prev = i, p.Value
		}
	}
	total := 0.0
	for _, p := range trace[base+1 : end+1] {
		if p.Value >= prev {
			total += p.Value - prev
		} else {
			total += p.Value
		}
		prev = p.Value
	}
	return total
}
