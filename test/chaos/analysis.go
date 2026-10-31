// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// MaxRateError is the timeline's validity rule: the baseline achieves its
// offered rate within 0.5% (target).
const MaxRateError = loadgen.DefaultMaxRateError

// Unattributed names the failed requests no Node's connection carried in
// a record's attribution.
const Unattributed = "unattributed"

// Stats sums the requests of a stream whose intended send time lies in a
// phase.
type Stats struct {
	// Offered counts the requests due in the phase, missed ones included.
	Offered int64
	// Achieved counts successes and counted codes.
	Achieved int64
	// Failed counts the rest: transport failures, missed and canceled
	// requests and uncounted error responses.
	Failed int64
	// Missed counts the requests the load generator never sent.
	Missed int64
	// Codes counts the responses by problem code.
	Codes map[string]int64
	// Statuses counts the responses by status.
	Statuses map[int]int64
	// Outcomes counts the requests by outcome.
	Outcomes map[loadgen.Outcome]int64
}

// Add merges o into s.
func (s *Stats) Add(o Stats) {
	s.Offered += o.Offered
	s.Achieved += o.Achieved
	s.Failed += o.Failed
	s.Missed += o.Missed
	for k, v := range o.Codes {
		s.codes()[k] += v
	}
	for k, v := range o.Statuses {
		s.statuses()[k] += v
	}
	for k, v := range o.Outcomes {
		s.outcomes()[k] += v
	}
}

func (s *Stats) codes() map[string]int64 {
	if s.Codes == nil {
		s.Codes = map[string]int64{}
	}
	return s.Codes
}

func (s *Stats) statuses() map[int]int64 {
	if s.Statuses == nil {
		s.Statuses = map[int]int64{}
	}
	return s.Statuses
}

func (s *Stats) outcomes() map[loadgen.Outcome]int64 {
	if s.Outcomes == nil {
		s.Outcomes = map[loadgen.Outcome]int64{}
	}
	return s.Outcomes
}

// RateShortfall is how far the achieved count falls short of the offered
// count, as a fraction of it; 1 when nothing was offered.
func (s Stats) RateShortfall() float64 {
	if s.Offered == 0 {
		return 1
	}
	return float64(s.Offered-s.Achieved) / float64(s.Offered)
}

// PhaseStats sums a stream's requests due in [from, to).
func PhaseStats(s *Stream, from, to time.Time) Stats {
	return statsOf(s, s.Requests(), from, to)
}

func statsOf(s *Stream, reqs []Request, from, to time.Time) Stats {
	var st Stats
	for _, r := range reqs {
		if r.Intended.Before(from) || !r.Intended.Before(to) {
			continue
		}
		st.Offered++
		st.outcomes()[r.Outcome]++
		if r.Status != 0 {
			st.statuses()[r.Status]++
		}
		if r.Code != "" {
			st.codes()[r.Code]++
		}
		switch {
		case r.Outcome == loadgen.Missed:
			st.Missed++
			st.Failed++
		case s.Failed(r):
			st.Failed++
		default:
			st.Achieved++
		}
	}
	return st
}

// LoadStats sums every stream of l over [from, to).
func LoadStats(l *Load, from, to time.Time) Stats {
	var st Stats
	for _, s := range l.Streams {
		st.Add(PhaseStats(s, from, to))
	}
	return st
}

// BaselineCriteria are the timeline's validity rule over the baseline
// phase, as criteria of kind k: zero errors, no missed request and the
// offered rate achieved within MaxRateError, else the run is invalid.
func BaselineCriteria(l *Load, p Phase, k Kind) []Criterion {
	st := LoadStats(l, p.Start, p.End)
	return []Criterion{
		Check("baseline errors", k, float64(st.Failed-st.Missed), EQ, 0),
		Check("baseline missed requests", k, float64(st.Missed), EQ, 0),
		Check("baseline achieved-rate shortfall", k, st.RateShortfall(), LE, MaxRateError),
	}
}

// Attribution counts the failed requests of l due in [from, to) per Node
// their connection reached, as the balancer attributed it.
func Attribution(l *Load, from, to time.Time) map[string]int64 {
	out := map[string]int64{}
	for _, s := range l.Streams {
		for _, r := range s.Requests() {
			if r.Intended.Before(from) || !r.Intended.Before(to) || !s.Failed(r) {
				continue
			}
			n := r.Node
			if n == "" {
				n = Unattributed
			}
			out[n]++
		}
	}
	return out
}

// InFlightFailures checks CE-1's error rule over the failed requests of l
// due at or after from: each must have been in flight on the victim, on a
// connection the L4 balancer attributes to it, and sent before the client
// saw that connection's reset or EOF, the first failure the client saw on
// it. It returns the failures attributed to another Node or to none, and
// those sent after their connection had failed.
func InFlightFailures(l *Load, victim string, from time.Time) (elsewhere, late int64) {
	for _, s := range l.Streams {
		reqs := s.Requests()
		seen := map[string]time.Time{} // connection to the first failure it showed
		for _, r := range reqs {
			if r.Intended.Before(from) || !s.Failed(r) || r.Local == "" {
				continue
			}
			if t, ok := seen[r.Local]; !ok || r.Done.Before(t) {
				seen[r.Local] = r.Done
			}
		}
		for _, r := range reqs {
			if r.Intended.Before(from) || !s.Failed(r) {
				continue
			}
			switch {
			case r.Node != victim:
				elsewhere++
			case r.Sent.After(seen[r.Local]):
				late++
			}
		}
	}
	return elsewhere, late
}

// WindowCount is one key's admissions in one aligned window.
type WindowCount struct {
	Key string
	// Window is the window's start, in Node time.
	Window time.Time
	// Count is the least number of admissions the window must hold:
	// those surely inside it, plus the boundary requests no assignment
	// can move out of it.
	Count int64
}

// AdmissionBound checks that no key admits more than bound requests in
// any window-aligned interval of Node time within [from, to) (by intended
// send time): the fail-open and local-only bounds hold per key and
// aligned window. Each admitted request was decided at a Node time
// between the earliest Node time a Node may have read when it was sent
// and the latest when it completed (span); a request whose two readings
// fall in adjacent windows may count in either, and the check assigns
// such requests to windows the way that keeps the largest window
// smallest. It returns the fullest window, and whether every window
// holds the bound.
func AdmissionBound(reqs []Request, admitted func(Request) bool, span func(time.Time) (lo, hi time.Time), window time.Duration, from, to time.Time, bound int64) (WindowCount, bool) {
	type key = string
	definite := map[key]map[int64]int64{}
	straddle := map[key]map[int64]int64{} // requests between window w and w+1, keyed by w
	for _, r := range reqs {
		if r.Intended.Before(from) || !r.Intended.Before(to) || !admitted(r) {
			continue
		}
		sent, _ := span(r.Sent)
		_, done := span(r.Done)
		ws := sent.UnixNano() / int64(window)
		wd := done.UnixNano() / int64(window)
		switch {
		case ws == wd:
			add(definite, r.Key, ws)
		case wd == ws+1:
			add(straddle, r.Key, ws)
		default:
			// A request that outlived a whole window was admitted in one
			// of them; it can only raise the window it was sent in.
			add(definite, r.Key, ws)
		}
	}
	worst, ok := WindowCount{}, true
	keys := map[key]bool{}
	for k := range definite {
		keys[k] = true
	}
	for k := range straddle {
		keys[k] = true
	}
	for k := range keys {
		var ws []int64
		for w := range definite[k] {
			ws = append(ws, w)
		}
		for w := range straddle[k] {
			ws = append(ws, w, w+1)
		}
		slices.Sort(ws)
		ws = slices.Compact(ws)
		carry := int64(0) // straddling requests the previous window could not take
		for _, w := range ws {
			n := definite[k][w] + carry
			carry = 0
			if s := straddle[k][w]; s > 0 {
				// Fill this window up to the bound; the rest go to the next.
				room := max(0, bound-n)
				take := min(room, s)
				n += take
				carry = s - take
			}
			if n > worst.Count {
				worst = WindowCount{Key: k, Window: time.Unix(0, w*int64(window)), Count: n}
			}
			if n > bound {
				ok = false
			}
		}
	}
	return worst, ok
}

// Interval is a Node time known only within [Lo, Hi], such as when a
// Node decided a request it got at one real instant and answered at a
// later one.
type Interval struct{ Lo, Hi time.Time }

// BucketLaw checks the local bucket law, summed over a run's Nodes, on
// the admitted requests of reqs due in [from, to) (by intended send
// time): over any span of Node time, at most capacity + span × rate
// admissions, the Nodes' full buckets plus their refill, which is how a
// localOnly Policy admits N_serving × ceiling per window beyond the
// buckets it starts with (docs/architecture/09-traffic-management-and-resilience.md
// "Per-Node ceiling"). Each admission was decided at a Node time between
// the earliest reading when it was sent and the latest when it completed
// (span). It returns BucketLawOf's excess and slack.
func BucketLaw(reqs []Request, admitted func(Request) bool, span func(time.Time) (lo, hi time.Time), capacity, rate float64, from, to time.Time) (excess float64, slack time.Duration) {
	var ds []Interval
	for _, r := range reqs {
		if r.Intended.Before(from) || !r.Intended.Before(to) || !admitted(r) {
			continue
		}
		lo, _ := span(r.Sent)
		_, hi := span(r.Done)
		ds = append(ds, Interval{lo, hi})
	}
	return BucketLawOf(ds, capacity, rate)
}

// BucketLawOf checks that events, each at a Node time known within its
// interval, keep a token bucket's law: over any span, at most capacity +
// span × rate events. It orders the events by their latest time and
// widens capacity by rate × the widest interval, so it never fails events
// whose true times keep the law. It returns how many events exceed the
// law at its worst, 0 when it holds, and that widest interval.
func BucketLawOf(events []Interval, capacity, rate float64) (excess float64, slack time.Duration) {
	if len(events) == 0 {
		return 0, 0
	}
	ds := slices.Clone(events)
	for _, d := range ds {
		slack = max(slack, d.Hi.Sub(d.Lo))
	}
	slices.SortFunc(ds, func(a, b Interval) int { return a.Hi.Compare(b.Hi) })
	// A virtual bucket of the widened capacity, refilled at rate and
	// starting full, takes one token per event: it runs dry exactly when
	// some span holds more events than the law allows.
	full := capacity + rate*slack.Seconds()
	level, worst := full, 0.0
	prev := ds[0].Hi
	for _, d := range ds {
		level = min(full, level+rate*d.Hi.Sub(prev).Seconds()) - 1
		prev = d.Hi
		worst = min(worst, level)
	}
	return -worst, slack
}

func add(m map[string]map[int64]int64, k string, w int64) {
	if m[k] == nil {
		m[k] = map[int64]int64{}
	}
	m[k][w]++
}

// Percentile returns the p-th percentile of durations by the nearest-rank
// rule; 0 for none.
func Percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	rank = max(1, min(rank, len(s)))
	return s[rank-1]
}

// Latencies returns the latency, from the intended send time, of every
// sent request of the streams due in [from, to).
func Latencies(streams []*Stream, from, to time.Time) []time.Duration {
	var out []time.Duration
	for _, s := range streams {
		for _, r := range s.Requests() {
			if r.Intended.Before(from) || !r.Intended.Before(to) || r.Outcome == loadgen.Missed || r.Outcome == loadgen.Canceled {
				continue
			}
			out = append(out, r.Done.Sub(r.Intended))
		}
	}
	return out
}

// WindowP99s returns the p99 latency of each 1 s window of [from, to) by
// intended send time, in order.
func WindowP99s(streams []*Stream, from, to time.Time) []time.Duration {
	n := int(to.Sub(from) / time.Second)
	buckets := make([][]time.Duration, n)
	for _, s := range streams {
		for _, r := range s.Requests() {
			if r.Intended.Before(from) || !r.Intended.Before(to) || r.Outcome == loadgen.Missed || r.Outcome == loadgen.Canceled {
				continue
			}
			i := int(r.Intended.Sub(from) / time.Second)
			if i < n {
				buckets[i] = append(buckets[i], r.Done.Sub(r.Intended))
			}
		}
	}
	out := make([]time.Duration, n)
	for i, b := range buckets {
		out[i] = Percentile(b, 99)
	}
	return out
}

// OutcomeTimeline counts the requests of st sent in [from, to) whose key
// is among keys (every key when keys is empty), per Node their
// connection reached (node maps a request to it) and per step-long slot
// of real time from from, by "status code" or outcome: the failure
// evidence of a criterion on what each Node did after a fault.
func OutcomeTimeline(st *Stream, node func(Request) string, keys []string, from, to time.Time, step time.Duration) map[string][]map[string]int {
	slots := int((to.Sub(from) + step - 1) / step)
	out := map[string][]map[string]int{}
	for _, q := range st.Requests() {
		if q.Sent.Before(from) || !q.Sent.Before(to) || (len(keys) > 0 && !slices.Contains(keys, q.Key)) {
			continue
		}
		n := node(q)
		if n == "" {
			n = Unattributed
		}
		if out[n] == nil {
			out[n] = make([]map[string]int, slots)
		}
		i := int(q.Sent.Sub(from) / step)
		if out[n][i] == nil {
			out[n][i] = map[string]int{}
		}
		k := q.Outcome.String()
		if q.Status != 0 {
			k = strconv.Itoa(q.Status)
			if q.Code != "" {
				k += " " + q.Code
			}
		}
		out[n][i][k]++
	}
	return out
}

// FormatTimeline writes an OutcomeTimeline one Node per line, Nodes in
// name order, each slot as its outcome counts in key order.
func FormatTimeline(tl map[string][]map[string]int, step time.Duration) string {
	var b strings.Builder
	for _, n := range slices.Sorted(maps.Keys(tl)) {
		fmt.Fprintf(&b, "\n  %s:", n)
		for i, slot := range tl[n] {
			fmt.Fprintf(&b, " [%v", time.Duration(i)*step)
			for _, k := range slices.Sorted(maps.Keys(slot)) {
				fmt.Fprintf(&b, " %s=%d", k, slot[k])
			}
			b.WriteString("]")
		}
	}
	return b.String()
}

// Transition is when a sampled gauge changed on one Node.
type Transition struct {
	Node       string
	Real, Time time.Time // Time is the Node time
	Value      float64
}

// FirstAt returns the first point of trace at or after real instant t
// whose value satisfies ok.
func FirstAt(trace []Point, t time.Time, ok func(float64) bool) (Point, bool) {
	i := sort.Search(len(trace), func(i int) bool { return !trace[i].Real.Before(t) })
	for ; i < len(trace); i++ {
		if ok(trace[i].Value) {
			return trace[i], true
		}
	}
	return Point{}, false
}

// LastAt returns the last point of trace before real instant t whose
// value satisfies ok.
func LastAt(trace []Point, t time.Time, ok func(float64) bool) (Point, bool) {
	for i := len(trace) - 1; i >= 0; i-- {
		if trace[i].Real.Before(t) && ok(trace[i].Value) {
			return trace[i], true
		}
	}
	return Point{}, false
}
