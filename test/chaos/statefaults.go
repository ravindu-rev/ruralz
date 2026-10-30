// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

// StateStoreTimeout is the chaos Bundle's stateStoreTimeout.
const StateStoreTimeout = 50 * time.Millisecond

// BreakerReason is the degraded reason of an open State client breaker.
const BreakerReason = "state_store_breaker_open"

// State Store failure codes of closed Policies
// (docs/architecture/11-scalability-and-distributed-state.md "State
// client and RZ-STS error codes").
const (
	CodeSTSTimeout = "RZ-STS-001"
	CodeSTSError   = "RZ-STS-002"
	CodeSTSOpen    = "RZ-STS-003"
	CodeSTSSkipped = "RZ-STS-004"
)

// STSCodes returns the codes a closed Policy answers while the State
// Store fails.
func STSCodes() []string {
	return []string{CodeSTSTimeout, CodeSTSError, CodeSTSOpen, CodeSTSSkipped}
}

// SetStoreMode sets the fault proxies every Node reaches the State Store
// named store through to m.
func (r *Run) SetStoreMode(store string, m faultproxy.Mode) error {
	for _, n := range r.Top.Nodes() {
		ps, ok := n.StateStoreProxy(store)
		if !ok {
			return fmt.Errorf("chaos: %s has no proxy to State Store %s", n.Name(), store)
		}
		for _, p := range ps {
			if err := p.SetMode(m); err != nil {
				return fmt.Errorf("chaos: %s: %w", n.Name(), err)
			}
		}
	}
	return nil
}

// LimitPlan is the load and limits of a run that checks per-key
// admission bounds: each key is offered twice the rate its bound admits,
// so the bound decides admission.
type LimitPlan struct {
	// Rates of the streams: plain, limited (declared ceiling), closed
	// (/limited-closed) and undeclared.
	Plain, Limited, Closed, Undeclared float64
	// Ceiling is /limited's per-Node ceiling and Requests its Cell-wide
	// limit per second, N × Ceiling; Undeclared is /undeclared's limit.
	Ceiling, Requests, UndeclaredRequests int
	// UndeclaredKeys are the key header values of /undeclared.
	UndeclaredKeys []string
}

// PlanLimits splits rate over the four streams, 30% plain, 30% limited,
// 20% closed and 20% undeclared, and sizes the limits for nodes Nodes so
// that every key is offered at least twice its fail-open bound.
func PlanLimits(rate float64, nodes int) LimitPlan {
	p := LimitPlan{Plain: rate * 0.3, Limited: rate * 0.3, Closed: rate * 0.2, Undeclared: rate * 0.2}
	keys := float64(len(Consumers()))
	n := float64(max(1, nodes))
	p.Ceiling = max(1, int(p.Limited/keys/(2*n)))
	p.Requests = p.Ceiling * max(1, nodes)
	p.UndeclaredRequests = max(1, int(p.Undeclared/keys/(2*n)))
	for i := range len(Consumers()) {
		p.UndeclaredKeys = append(p.UndeclaredKeys, fmt.Sprintf("undeclared-%d", i+1))
	}
	return p
}

// Apply sets the plan's limits on the Bundle parameters.
func (p LimitPlan) Apply(b *BundleParams) {
	b.Limited = Limit{Requests: p.Requests, Ceiling: p.Ceiling}
	b.Undeclared = p.UndeclaredRequests
}

// Streams returns the plan's four streams; the closed stream counts the
// State Store codes as achieved, so the fault phase's expected 503s are
// not errors (CodesIn tells them apart).
func (p LimitPlan) Streams() []StreamSpec {
	return []StreamSpec{
		{Name: "plain", Path: PathPlain, Rate: p.Plain},
		{Name: "limited", Path: PathLimited, Rate: p.Limited, APIKeys: true, Counted: Decisions()},
		{Name: "closed", Path: PathLimitedClosed, Rate: p.Closed, APIKeys: true, Counted: append(Decisions(), STSCodes()...)},
		{Name: "undeclared", Path: PathUndeclared, Rate: p.Undeclared, KeyHeader: KeyHeader, Keys: listKeys(p.UndeclaredKeys), Counted: Decisions()},
	}
}

// StreamNamed returns the stream named name.
func (l *Load) StreamNamed(name string) *Stream {
	for _, s := range l.Streams {
		if s.Spec.Name == name {
			return s
		}
	}
	return nil
}

// Admitted reports a request the Route admitted: a 2xx response.
func Admitted(r Request) bool { return r.Status >= 200 && r.Status < 300 }

// AdmissionCheck is a per-key admission bound criterion over the requests
// of st due in [from, to), with windows of the Nodes' clock.
func (r *Run) AdmissionCheck(name string, st *Stream, window time.Duration, from, to time.Time, bound int64) Criterion {
	worst, _ := AdmissionBound(st.Requests(), Admitted, r.NodeSpan, window, from, to, bound)
	if worst.Key != "" {
		r.T.Logf("chaos: %s: fullest window %s at %s: %d admitted, bound %d", name, worst.Key, worst.Window.Format(time.RFC3339Nano), worst.Count, bound)
	}
	return Check(name, Functional, float64(worst.Count), LE, float64(bound))
}

// CodesIn counts the responses of st due in [from, to) carrying one of
// codes.
func CodesIn(st *Stream, from, to time.Time, codes ...string) int64 {
	var n int64
	for _, q := range st.Requests() {
		if !q.Intended.Before(from) && q.Intended.Before(to) && slices.Contains(codes, q.Code) {
			n++
		}
	}
	return n
}

// CodeOrder returns the codes among codes that responses of st due in
// [from, to) carried, in the order each first completed.
func CodeOrder(st *Stream, from, to time.Time, codes ...string) []string {
	first := map[string]time.Time{}
	for _, q := range st.Requests() {
		if q.Intended.Before(from) || !q.Intended.Before(to) || !slices.Contains(codes, q.Code) {
			continue
		}
		if t, ok := first[q.Code]; !ok || q.Done.Before(t) {
			first[q.Code] = q.Done
		}
	}
	out := make([]string, 0, len(first))
	for c := range first {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return first[out[i]].Before(first[out[j]]) })
	return out
}

// AdmittedAfter counts the admitted requests of st due in [from, to) that
// were sent after the first response carrying one of codes completed.
func AdmittedAfter(st *Stream, from, to time.Time, codes ...string) int64 {
	var first time.Time
	reqs := st.Requests()
	for _, q := range reqs {
		if !q.Intended.Before(from) && q.Intended.Before(to) && slices.Contains(codes, q.Code) && (first.IsZero() || q.Done.Before(first)) {
			first = q.Done
		}
	}
	if first.IsZero() {
		return 0
	}
	var n int64
	for _, q := range reqs {
		if !q.Intended.Before(from) && q.Intended.Before(to) && q.Sent.After(first) && Admitted(q) {
			n++
		}
	}
	return n
}

// ReasonClearedAt returns the real instant by which every Node of nodes
// had cleared reason for good after real instant from: the first sample
// of each Node after its last sample still raising it. ok is false when
// a Node still raised it at its last sample.
func (s *Sampler) ReasonClearedAt(nodes []string, reason string, from time.Time) (time.Time, bool) {
	var at time.Time
	for _, n := range nodes {
		tr := s.ReasonTrace(n, reason)
		cleared := from
		for i, p := range tr {
			if p.Real.Before(from) {
				continue
			}
			if p.Value > 0 {
				if i == len(tr)-1 {
					return time.Time{}, false
				}
				cleared = tr[i+1].Real
			}
		}
		if cleared.After(at) {
			at = cleared
		}
	}
	return at, true
}

// ReasonRaised returns, per Node of nodes, the first real instant in
// [from, to) a sample showed reason raised; Nodes that never raised it
// are absent.
func (s *Sampler) ReasonRaised(nodes []string, reason string, from, to time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	for _, n := range nodes {
		if p, ok := FirstAt(s.ReasonTrace(n, reason), from, func(v float64) bool { return v > 0 }); ok && p.Real.Before(to) {
			out[n] = p.Real
		}
	}
	return out
}

// ErrorsAfter counts the failed requests of l due in [from, to), the
// responses carrying one of codes included even where a stream counts
// them as achieved.
func ErrorsAfter(l *Load, from, to time.Time, codes ...string) int64 {
	var n int64
	for _, st := range l.Streams {
		for _, q := range st.Requests() {
			if q.Intended.Before(from) || !q.Intended.Before(to) {
				continue
			}
			if st.Failed(q) || slices.Contains(codes, q.Code) {
				n++
			}
		}
	}
	return n
}

// NodeNames returns the names of the topology's Nodes that serve.
func (r *Run) NodeNames() []string {
	var out []string
	for _, n := range r.Top.Nodes() {
		if n.Process() != nil {
			out = append(out, n.Name())
		}
	}
	return out
}

// CallsIncrease returns how much ruralz_state_calls_total grew over
// (from, to] on every Node together for the results listed, every op.
func (r *Run) CallsIncrease(from, to time.Time, results ...string) float64 {
	total := 0.0
	for k, v := range r.Sampler.Totals(catalog.StateCallsTotal, from, to, "result") {
		for _, res := range results {
			if strings.Contains(k, `result="`+res+`"`) {
				total += v
			}
		}
	}
	return total
}

// ErrorKinds counts the requests ErrorsAfter counts by problem code, or
// by outcome for those without one: the failure evidence of an error
// criterion.
func ErrorKinds(l *Load, from, to time.Time, codes ...string) map[string]int64 {
	out := map[string]int64{}
	for _, st := range l.Streams {
		for _, q := range st.Requests() {
			if q.Intended.Before(from) || !q.Intended.Before(to) || (!st.Failed(q) && !slices.Contains(codes, q.Code)) {
				continue
			}
			k := q.Code
			if k == "" {
				k = q.Outcome.String()
			}
			out[st.Spec.Name+" "+k]++
		}
	}
	return out
}

// ErrorSpan returns when the first and the last request ErrorsAfter
// counts were sent.
func ErrorSpan(l *Load, from, to time.Time, codes ...string) (first, last time.Time) {
	for _, st := range l.Streams {
		for _, q := range st.Requests() {
			if q.Intended.Before(from) || !q.Intended.Before(to) || (!st.Failed(q) && !slices.Contains(codes, q.Code)) {
				continue
			}
			if first.IsZero() || q.Sent.Before(first) {
				first = q.Sent
			}
			if q.Sent.After(last) {
				last = q.Sent
			}
		}
	}
	return first, last
}

// RecoveryCriteria adds the recovery rules of a State Store fault that
// ended at real instant end: every Node's breaker closes within the
// recovery phase rec, after which no request of l fails or gets a State
// Store code, and no degraded reason raised since the baseline base is
// left (docs/architecture/11-scalability-and-distributed-state.md "Chaos
// experiments": every degraded reason the fault raised clears and errors
// return to zero).
func (r *Run) RecoveryCriteria(l *Load, base, rec Phase, end time.Time) {
	r.T.Helper()
	nodes := r.NodeNames()
	if kinds := ErrorKinds(l, base.Start, base.End, STSCodes()...); len(kinds) > 0 {
		r.T.Logf("chaos: baseline errors and State Store codes: %v", kinds)
	}
	k := r.ValidityKind()
	closedAt, ok := r.Sampler.ReasonClearedAt(nodes, BreakerReason, end)
	if !ok || !closedAt.Before(rec.End) {
		r.Add(Holds("breakers closed within the recovery", k, false))
	} else {
		r.Breakdown("breakers_closed_after_fault_s", closedAt.Sub(end).Seconds())
		if kinds := ErrorKinds(l, closedAt, rec.End, STSCodes()...); len(kinds) > 0 {
			first, last := ErrorSpan(l, closedAt, rec.End, STSCodes()...)
			r.T.Logf("chaos: recovery errors after the breakers closed: %v, %v to %v after they closed", kinds, first.Sub(closedAt), last.Sub(closedAt))
		}
		r.Add(
			Holds("breakers closed within the recovery", k, true),
			Check("recovery errors after the breakers closed", k, float64(ErrorsAfter(l, closedAt, rec.End, STSCodes()...)), EQ, 0),
		)
	}
	r.StaleReasons(nodes, base.End, rec.End)
}

// StaleReasons adds the criterion, of the run's validity kind, that no
// Node of nodes still raises at real instant end a degraded reason it did
// not raise at real instant before, logging each one left.
func (r *Run) StaleReasons(nodes []string, before, end time.Time) {
	r.T.Helper()
	stale := r.Sampler.NewReasons(nodes, before, end)
	for n, rs := range stale {
		r.T.Logf("chaos: %s kept degraded reasons %v after the recovery", n, rs)
	}
	r.Add(Check("degraded reasons left after the recovery", r.ValidityKind(), float64(len(stale)), EQ, 0))
}
