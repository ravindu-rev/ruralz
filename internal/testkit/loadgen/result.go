// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"
)

type admitResult int

const (
	admitGo admitResult = iota
	admitPause
	admitEnd
)

// phases decides where the warm-up ends and where the schedule ends. Its
// fields belong to the scheduler goroutine.
type phases struct {
	cfg *Config
	// checkpoints are the candidate warm-up ends: Warmup, then every
	// Settle.Step up to Settle.Max.
	checkpoints []time.Duration
	reached     int      // checkpoints[:reached] have been passed
	seqAt       []uint64 // first request at or past each passed checkpoint
	next        int      // index of the checkpoint awaiting a settle decision
	decided     bool
	boundary    time.Duration // the warm-up's end once decided
	settled     bool
	end         time.Duration // the schedule's end once admitEnd was returned
}

func newPhases(c *Config) *phases {
	cps := []time.Duration{c.Warmup}
	if s := c.Settle; s != nil {
		for cp := c.Warmup + s.Step; cp <= s.Max; cp += s.Step {
			cps = append(cps, cp)
		}
	}
	p := &phases{cfg: c, checkpoints: cps, seqAt: make([]uint64, len(cps))}
	if c.Settle == nil {
		p.decided, p.boundary, p.settled = true, c.Warmup, true
	}
	return p
}

// segmentStarts returns the offsets that split the run into segments, so
// the measured phase is a union of whole segments whichever checkpoint
// ends the warm-up.
func (p *phases) segmentStarts() []time.Duration { return slices.Clone(p.checkpoints) }

// measuringAt reports whether a request at off is in the measured phase.
func (p *phases) measuringAt(off time.Duration) bool { return p.decided && off >= p.boundary }

// admit decides whether request seq at off may be dispatched.
func (p *phases) admit(seq uint64, off time.Duration) admitResult {
	for p.reached < len(p.checkpoints) && off >= p.checkpoints[p.reached] {
		p.seqAt[p.reached] = seq
		p.reached++
	}
	bi := p.next // the boundary's checkpoint, decided or tentative
	b := p.checkpoints[bi]
	if p.decided {
		b = p.boundary
		bi = slices.Index(p.checkpoints, b)
	}
	var over bool
	switch {
	case p.cfg.Duration > 0:
		over = off >= b+p.cfg.Duration
	case p.cfg.Requests > 0:
		over = bi >= 0 && bi < p.reached && seq >= p.seqAt[bi]+p.cfg.Requests
	}
	switch {
	case !over:
		return admitGo
	case !p.decided:
		return admitPause
	}
	p.end = off
	if p.cfg.Duration > 0 {
		p.end = b + p.cfg.Duration
	}
	return admitEnd
}

// decide takes the settle decision for the pending checkpoint once every
// request before it has been dispatched (off, the next request's offset,
// is past it) and has ended.
func (p *phases) decide(g *Generator, off time.Duration) {
	if p.decided {
		return
	}
	c := p.checkpoints[p.next]
	if off < c {
		return
	}
	g.pending.Store(int64(c))
	s := p.cfg.Settle
	from := c - time.Duration(s.Windows)*s.Window
	if !g.rec.complete(from, c) {
		return
	}
	p99s := g.rec.p99s(from, c, s.Window)
	switch {
	case Settled(p99s, s.MaxSpread):
		p.decided, p.boundary, p.settled = true, c, true
	case p.next == len(p.checkpoints)-1:
		p.decided, p.boundary, p.settled = true, c, false
	default:
		p.next++
	}
	g.pending.Store(-1)
}

// stop ends the schedule at off; an undecided warm-up ends there too,
// unsettled, leaving the measured phase empty.
func (p *phases) stop(off time.Duration) {
	if !p.decided {
		p.decided, p.boundary, p.settled = true, off, false
	}
	p.end = off
}

// recorder aggregates finished requests by window and by segment.
type recorder struct {
	window      time.Duration
	segStarts   []time.Duration
	maxFailures int

	mu          sync.Mutex
	segs        []*Stats // len(segStarts)+1 segments
	wins        []*winRec
	failures    []Sample
	failDropped int64
}

type winRec struct {
	Stats
	dispatched, finished int64
}

func newRecorder(c *Config, segStarts []time.Duration) *recorder {
	r := &recorder{window: c.Window, segStarts: segStarts, maxFailures: c.MaxFailures}
	r.segs = make([]*Stats, len(segStarts)+1)
	for i := range r.segs {
		r.segs[i] = newStats(DefaultDigits, true)
	}
	return r
}

func (r *recorder) seg(off time.Duration) *Stats {
	return r.segs[sort.Search(len(r.segStarts), func(i int) bool { return r.segStarts[i] > off })]
}

// win returns off's window, creating the windows up to it.
func (r *recorder) win(off time.Duration) *winRec {
	i := int(off / r.window)
	for len(r.wins) <= i {
		r.wins = append(r.wins, nil)
	}
	if r.wins[i] == nil {
		r.wins[i] = &winRec{Stats: *newStats(WindowDigits, false)}
	}
	return r.wins[i]
}

// dispatched counts a request whose intended time came.
func (r *recorder) dispatched(off time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.win(off)
	w.dispatched++
	w.Offered++
	r.seg(off).Offered++
}

// add records a finished request.
func (r *recorder) add(s Sample, off time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.win(off)
	w.finished++
	w.add(s)
	r.seg(off).add(s)
	if s.Outcome != Success {
		if len(r.failures) < r.maxFailures {
			r.failures = append(r.failures, s)
		} else {
			r.failDropped++
		}
	}
}

// complete reports whether every request of the windows in [from, to)
// has ended.
func (r *recorder) complete(from, to time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := int(max(from, 0) / r.window); i < int(to/r.window) && i < len(r.wins); i++ {
		if w := r.wins[i]; w != nil && w.dispatched != w.finished {
			return false
		}
	}
	return true
}

// p99s returns the p99 of each span of length span in [from, to).
func (r *recorder) p99s(from, to, span time.Duration) []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []time.Duration
	for a := from; a+span <= to; a += span {
		h := mustHistogram(WindowDigits)
		for i := int(max(a, 0) / r.window); i < int((a+span)/r.window) && i < len(r.wins); i++ {
			if w := r.wins[i]; w != nil {
				_ = h.Merge(w.Latency) // same settings
			}
		}
		out = append(out, h.ValueAtPercentile(99))
	}
	return out
}

// result assembles the Result once every request has ended.
func (r *recorder) result(start time.Time, p *phases, end time.Duration, rate float64) *Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := &Result{
		Rate:            rate,
		Window:          r.window,
		Start:           start,
		Settled:         p.settled,
		Failures:        r.failures,
		FailuresDropped: r.failDropped,
	}
	res.Warmup = *newStats(DefaultDigits, true)
	res.Measured = *newStats(DefaultDigits, true)
	bi := slices.Index(r.segStarts, p.boundary)
	for i, s := range r.segs {
		if bi >= 0 && i > bi {
			res.Measured.merge(s)
		} else {
			res.Warmup.merge(s)
		}
	}
	res.MeasureStart = start.Add(p.boundary)
	res.MeasureEnd = start.Add(max(end, p.boundary))
	res.Windows = make([]Window, 0, len(r.wins))
	for i, w := range r.wins {
		ws := Window{Start: start.Add(time.Duration(i) * r.window)}
		if w != nil {
			ws.Stats = w.Stats
		} else {
			ws.Stats = *newStats(WindowDigits, false)
		}
		res.Windows = append(res.Windows, ws)
	}
	return res
}

// Stats aggregates requests.
type Stats struct {
	// Offered counts requests whose intended time came, Missed ones
	// included.
	Offered int64
	// Outcomes counts requests by Outcome.
	Outcomes [NumOutcomes]int64
	// Statuses counts responses by HTTP status.
	Statuses map[int]int64
	// Codes counts error responses by the RZ code of their problem
	// document.
	Codes map[string]int64
	// NewConns counts requests that opened a connection.
	NewConns int64
	// Bytes counts response body bytes.
	Bytes int64
	// Latency holds every sent request's latency from its intended send
	// time, failures included and Canceled requests excluded.
	Latency *Histogram
	// Service holds the same requests' time from the actual send; nil in
	// Windows.
	Service *Histogram
}

func newStats(digits int, service bool) *Stats {
	s := &Stats{Latency: mustHistogram(digits)}
	if service {
		s.Service = mustHistogram(digits)
	}
	return s
}

func (s *Stats) add(x Sample) {
	s.Outcomes[x.Outcome]++
	if x.Outcome != Missed && x.Outcome != Canceled {
		s.Latency.Record(x.Latency())
		if s.Service != nil {
			s.Service.Record(x.Service())
		}
	}
	if x.Status != 0 {
		if s.Statuses == nil {
			s.Statuses = map[int]int64{}
		}
		s.Statuses[x.Status]++
	}
	if x.Code != "" && x.Outcome != Success {
		if s.Codes == nil {
			s.Codes = map[string]int64{}
		}
		s.Codes[x.Code]++
	}
	if x.Conn.Local != "" && !x.Conn.Reused {
		s.NewConns++
	}
	s.Bytes += x.Bytes
}

// merge adds o to s; both histograms come from this package.
func (s *Stats) merge(o *Stats) {
	s.Offered += o.Offered
	for i, n := range o.Outcomes {
		s.Outcomes[i] += n
	}
	if len(o.Statuses) > 0 {
		if s.Statuses == nil {
			s.Statuses = map[int]int64{}
		}
		for k, n := range o.Statuses {
			s.Statuses[k] += n
		}
	}
	if len(o.Codes) > 0 {
		if s.Codes == nil {
			s.Codes = map[string]int64{}
		}
		for k, n := range o.Codes {
			s.Codes[k] += n
		}
	}
	s.NewConns += o.NewConns
	s.Bytes += o.Bytes
	_ = s.Latency.Merge(o.Latency) // same or narrower settings
	if s.Service != nil && o.Service != nil {
		_ = s.Service.Merge(o.Service)
	}
}

// Count returns the requests that ended with o.
func (s *Stats) Count(o Outcome) int64 {
	if o >= NumOutcomes {
		return 0
	}
	return s.Outcomes[o]
}

// Sent returns the requests actually sent: Offered minus Missed.
func (s *Stats) Sent() int64 { return s.Offered - s.Outcomes[Missed] }

// Achieved returns the requests that count toward the achieved rate:
// Success, plus error responses carrying one of the counted codes (PBB
// counts RZ-RT-005 in O1).
func (s *Stats) Achieved(counted ...string) int64 {
	n := s.Outcomes[Success]
	for _, c := range slices.Compact(slices.Sorted(slices.Values(counted))) {
		n += s.Codes[c]
	}
	return n
}

// Errors returns the requests that did not achieve: Offered minus
// Achieved, so Missed, Canceled, failed and uncounted error responses.
func (s *Stats) Errors(counted ...string) int64 { return s.Offered - s.Achieved(counted...) }

// ErrorRate returns Errors over Offered; 0 when nothing was offered.
func (s *Stats) ErrorRate(counted ...string) float64 {
	if s.Offered == 0 {
		return 0
	}
	return float64(s.Errors(counted...)) / float64(s.Offered)
}

// CodeNames returns the counted RZ codes in order.
func (s *Stats) CodeNames() []string { return slices.Sorted(maps.Keys(s.Codes)) }

// Window is one slice of the run by intended send time.
type Window struct {
	// Start is the window's first intended send time.
	Start time.Time
	Stats
}

// Result is a finished run.
type Result struct {
	// Rate is the offered rate in requests per second.
	Rate float64
	// Window is the length of each of Windows.
	Window time.Duration
	// Start is the intended send time of request 0.
	Start time.Time
	// MeasureStart and MeasureEnd bound the measured phase by intended
	// send time.
	MeasureStart, MeasureEnd time.Time
	// Settled reports a warm-up that settled; always true without Settle.
	// A Stop or context end before the settle decision leaves it false.
	Settled bool
	// Warmup and Measured aggregate the two phases.
	Warmup, Measured Stats
	// Windows is the whole run, warm-up included, by Window of intended
	// send time.
	Windows []Window
	// Failures holds the first MaxFailures requests that did not end in
	// Success; FailuresDropped counts the rest.
	Failures        []Sample
	FailuresDropped int64
}

// MeasuredDuration returns the measured phase's length.
func (r *Result) MeasuredDuration() time.Duration { return r.MeasureEnd.Sub(r.MeasureStart) }

// AchievedRate returns the measured phase's achieved requests per second
// (see Stats.Achieved); 0 for an empty phase.
func (r *Result) AchievedRate(counted ...string) float64 {
	d := r.MeasuredDuration()
	if d <= 0 {
		return 0
	}
	return float64(r.Measured.Achieved(counted...)) / d.Seconds()
}

// RateError returns how far the achieved count of the measured phase falls
// short of the offered count, as a fraction of the offered count; 1 when
// nothing was offered.
func (r *Result) RateError(counted ...string) float64 {
	if r.Measured.Offered == 0 {
		return 1
	}
	return float64(r.Measured.Errors(counted...)) / float64(r.Measured.Offered)
}

// Validity is PBB's rule for a valid run.
type Validity struct {
	// MaxRateError bounds RateError; 0 means DefaultMaxRateError.
	MaxRateError float64
	// Count names the RZ codes counted as achieved, such as RZ-RT-005 in
	// O1.
	Count []string
}

// Valid checks the run against v: a measured phase that offered requests,
// a settled warm-up, no request canceled by the context, and an achieved
// rate within MaxRateError of the offered rate (PBB "Open-loop load and
// coordinated omission"). The other rules, generator and mock CPU and the
// mock's own latency, belong to the harness measuring them.
func (r *Result) Valid(v Validity) error {
	if v.MaxRateError == 0 {
		v.MaxRateError = DefaultMaxRateError
	}
	var errs []error
	if r.Measured.Offered == 0 || r.MeasuredDuration() <= 0 {
		errs = append(errs, errors.New("loadgen: the measured phase offered no requests"))
	}
	if !r.Settled {
		errs = append(errs, errors.New("loadgen: the warm-up did not settle"))
	}
	if n := r.Measured.Count(Canceled); n > 0 {
		errs = append(errs, fmt.Errorf("loadgen: %d measured requests were canceled", n))
	}
	if e := r.RateError(v.Count...); r.Measured.Offered > 0 && e > v.MaxRateError {
		errs = append(errs, fmt.Errorf("loadgen: achieved rate %.4f%% short of the offered rate, above %.4f%%",
			100*e, 100*v.MaxRateError))
	}
	return errors.Join(errs...)
}
