// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// DefaultSampleEvery is the period of a Sampler's scrapes.
const DefaultSampleEvery = time.Second

// ScrapeTarget is one Node's /metrics.
type ScrapeTarget struct {
	// Name names the Node, such as "node-1".
	Name string
	// URL is its /metrics URL on the admin port.
	URL string
	// Token is its admin metrics token.
	Token string
}

// Series is one sampled series.
type Series struct {
	Name   string
	Labels promtext.Labels
}

// Sample is one scrape of one Node: when it was taken, in real and Node
// time, and the kept series' values; Err reports a scrape that failed, as
// a stopped or killed Node's does.
type Sample struct {
	Real, Node time.Time
	Err        error
	values     map[int]float64
}

// Sampler scrapes every Node's /metrics on a period and keeps the series
// of the metric families a run asserts (docs/engineering/03-testing-and-quality-strategy.md
// "Chaos testing": a degraded state without a metric fails its scenario).
// Its scrapes bypass the harness's capture store, which would keep every
// body for the leak scan; a run captures one /metrics per Node and phase
// through the harness instead. Its methods are safe for concurrent use.
type Sampler struct {
	client  *http.Client
	targets func() []ScrapeTarget
	every   time.Duration
	keep    []string
	real    clock.Clock
	node    func() time.Time

	mu      sync.Mutex
	series  []Series
	ids     map[string]int
	samples map[string][]Sample

	stop chan struct{}
	done chan struct{}
}

// SamplerConfig configures a Sampler.
type SamplerConfig struct {
	// Client fetches the scrapes, trusting the admin TLS.
	Client *http.Client
	// Targets returns the Nodes to scrape; it is called before every
	// round, so Nodes added meanwhile join.
	Targets func() []ScrapeTarget
	// Every is the period; default DefaultSampleEvery.
	Every time.Duration
	// Keep lists the metric family names (or name prefixes ending in '_')
	// whose series are kept.
	Keep []string
	// Real is the real clock; default clock.Real().
	Real clock.Clock
	// Node returns the Nodes' clock reading, zero without a controllable
	// clock; nil is none.
	Node func() time.Time
}

// StartSampler starts scraping until Stop or the end of ctx.
func StartSampler(ctx context.Context, c SamplerConfig) *Sampler {
	if c.Every <= 0 {
		c.Every = DefaultSampleEvery
	}
	if c.Real == nil {
		c.Real = clock.Real()
	}
	if c.Node == nil {
		c.Node = func() time.Time { return time.Time{} }
	}
	s := &Sampler{
		client: c.Client, targets: c.Targets, every: c.Every, keep: c.Keep, real: c.Real, node: c.Node,
		ids: map[string]int{}, samples: map[string][]Sample{},
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go s.loop(ctx)
	return s
}

func (s *Sampler) loop(ctx context.Context) {
	defer close(s.done)
	timer := s.real.NewTimer(s.every)
	defer timer.Stop()
	for {
		s.Round(ctx)
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-timer.C():
			timer.Reset(s.every)
		}
	}
}

// Stop stops the sampler and waits for its round in progress.
func (s *Sampler) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}

// Round scrapes every Node once, together, and records the samples.
func (s *Sampler) Round(ctx context.Context) {
	targets := s.targets()
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Go(func() {
			sctx, cancel := context.WithTimeout(ctx, max(s.every, time.Second))
			defer cancel()
			real, node := s.real.Now(), s.node()
			set, err := promtext.Scrape(sctx, s.client, t.URL, http.Header{"Authorization": {"Bearer " + t.Token}})
			s.record(t.Name, real, node, set, err)
		})
	}
	wg.Wait()
}

// record keeps one scrape.
func (s *Sampler) record(node string, real, nodeTime time.Time, set promtext.Set, err error) {
	smp := Sample{Real: real, Node: nodeTime, Err: err}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		smp.values = map[int]float64{}
		for name, fam := range set.Families {
			if !s.kept(name) {
				continue
			}
			for _, x := range fam.Samples {
				smp.values[s.idLocked(x.Name, x.Labels)] = x.Value
			}
		}
	}
	s.samples[node] = append(s.samples[node], smp)
}

// kept reports whether family name is kept.
func (s *Sampler) kept(name string) bool {
	for _, k := range s.keep {
		if name == k || (strings.HasSuffix(k, "_") && strings.HasPrefix(name, k)) {
			return true
		}
	}
	return false
}

// idLocked interns a series.
func (s *Sampler) idLocked(name string, l promtext.Labels) int {
	key := name + l.String()
	id, ok := s.ids[key]
	if !ok {
		id = len(s.series)
		s.series = append(s.series, Series{Name: name, Labels: l})
		s.ids[key] = id
	}
	return id
}

// Nodes returns the scraped Nodes' names.
func (s *Sampler) Nodes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.samples))
	for n := range s.samples {
		out = append(out, n)
	}
	return out
}

// Samples returns a Node's samples, oldest first.
func (s *Sampler) Samples(node string) []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sample(nil), s.samples[node]...)
}

// Sum returns, for one sample, the sum of the series named name whose
// labels include match; ok is false for a failed scrape.
func (s *Sampler) Sum(smp Sample, name string, match promtext.Labels) (float64, bool) {
	if smp.Err != nil || smp.values == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0.0
	for id, v := range smp.values {
		if sr := s.series[id]; sr.Name == name && matches(sr.Labels, match) {
			total += v
		}
	}
	return total, true
}

// matches reports whether l holds every label of match; a match value ""
// matches an absent label.
func matches(l, match promtext.Labels) bool {
	for k, v := range match {
		if l[k] != v {
			return false
		}
	}
	return true
}

// Point is one value of a series sum over time.
type Point struct {
	Real, Node time.Time
	Value      float64
}

// Trace returns a Node's sum of name over match at every successful
// sample, oldest first.
func (s *Sampler) Trace(node, name string, match promtext.Labels) []Point {
	var out []Point
	for _, smp := range s.Samples(node) {
		if v, ok := s.Sum(smp, name, match); ok {
			out = append(out, Point{Real: smp.Real, Node: smp.Node, Value: v})
		}
	}
	return out
}

// Groups returns the distinct values of the labels by among the series
// named name, each as a label set, sorted by its exposition form.
func (s *Sampler) Groups(name string, by ...string) []promtext.Labels {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]promtext.Labels{}
	for _, sr := range s.series {
		if sr.Name != name {
			continue
		}
		g := promtext.Labels{}
		for _, b := range by {
			g[b] = sr.Labels[b]
		}
		seen[g.String()] = g
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]promtext.Labels, len(keys))
	for i, k := range keys {
		out[i] = seen[k]
	}
	return out
}

// Totals returns, per group of the labels by, how much the counter name
// grew on every Node together over (from, to].
func (s *Sampler) Totals(name string, from, to time.Time, by ...string) map[string]float64 {
	out := map[string]float64{}
	for _, g := range s.Groups(name, by...) {
		total := 0.0
		for _, n := range s.Nodes() {
			total += Increase(s.Trace(n, name, g), from, to)
		}
		if total > 0 {
			out[name+g.String()] = total
		}
	}
	return out
}

// Rises returns the samples at which a counter trace grew, each the
// later sample of the pair: the observed times of a counter's increases.
func Rises(trace []Point) []Point {
	var out []Point
	for i := 1; i < len(trace); i++ {
		if trace[i].Value > trace[i-1].Value {
			out = append(out, trace[i])
		}
	}
	return out
}

// Between returns the points with a real time in [from, to).
func Between(trace []Point, from, to time.Time) []Point {
	var out []Point
	for _, p := range trace {
		if !p.Real.Before(from) && p.Real.Before(to) {
			out = append(out, p)
		}
	}
	return out
}

// Increase returns how much a counter trace grew over (from, to]: from
// the last value at or before from, or from the first value after it when
// none precedes it, to the last value at or before to. A counter that
// reset, as a restarted Node's does, counts from 0 again.
func Increase(trace []Point, from, to time.Time) float64 {
	total, prev, have := 0.0, 0.0, false
	for _, p := range trace {
		if p.Real.After(to) {
			break
		}
		switch {
		case !p.Real.After(from), !have:
			prev, have = p.Value, true
		case p.Value >= prev:
			total += p.Value - prev
			prev = p.Value
		default:
			total += p.Value
			prev = p.Value
		}
	}
	return total
}
