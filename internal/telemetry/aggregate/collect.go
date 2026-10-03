// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"context"
	"fmt"
	"runtime/metrics"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Collection timing (spec 09 req 51).
const (
	// ReuseWindow is how long one collection is shared by the readers that
	// arrive after it (1 s, target).
	ReuseWindow = time.Second
	// AbandonAfter is how long a structure may stay held by a reader that
	// never released it before it is dropped instead of reused (30 s).
	AbandonAfter = 30 * time.Second
	// keepFree is how many released structures wait for reuse.
	keepFree = 2
	// keepHeld is how many held structures are tracked for release.
	keepHeld = 8
)

// Producer exports the registry as metricdata (sdkmetric.Producer). It is
// registered on the OTLP PeriodicReader (sdkmetric.WithProducer) and on
// the Prometheus exporter (prometheus.WithProducer), so each operation
// aggregates once and feeds both (OQ-observability-16 (a)).
//
// At most one collection is in flight; readers arriving meanwhile wait for
// it, and a result is reused for up to ReuseWindow. Results are pooled: a
// reader that knows when it stops reading the data (the OTLP exporter
// wrapper after Export, the /metrics handler after Gather) passes it back
// with Release, and the next collection reuses that structure. A result
// never released is abandoned after AbandonAfter, never reused while it
// may still be read.
type Producer struct {
	r   *Registry
	sem chan struct{}

	mu   sync.Mutex
	gen  uint64
	cur  *collection
	held []*collection
	free []*collection
}

var _ sdkmetric.Producer = (*Producer)(nil)

// collection is one pooled metricdata structure.
type collection struct {
	gen     uint64
	at      time.Time
	refs    int
	fams    []famData
	metrics []metricdata.Metrics
	scopes  []metricdata.ScopeMetrics
	buckets []uint64
	exs     []metricdata.Exemplar[float64]
	ids     []byte
	codes   []*codeEntry
}

// famData holds one family's reusable data point slices.
type famData struct {
	sums   []metricdata.DataPoint[float64]
	gauges []metricdata.DataPoint[int64]
	hists  []metricdata.HistogramDataPoint[float64]
}

// Producer returns the registry's producer.
func (r *Registry) Producer() *Producer { return r.producer }

// Produce implements sdkmetric.Producer. The returned data is shared with
// other readers and must be treated as read-only.
func (p *Producer) Produce(ctx context.Context) ([]metricdata.ScopeMetrics, error) {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("aggregate: collection: %w", ctx.Err())
	}
	defer func() { <-p.sem }()
	now := p.r.clk.Now()
	p.mu.Lock()
	if c := p.cur; c != nil && !now.Before(c.at) && now.Sub(c.at) < ReuseWindow {
		c.refs++
		p.mu.Unlock()
		return c.scopes, nil
	}
	// Structures held longer than AbandonAfter are dropped, never reused.
	p.held = slices.DeleteFunc(p.held, func(c *collection) bool { return now.Sub(c.at) >= AbandonAfter })
	// A current structure no reader holds is refilled in place: readers
	// arriving meanwhile wait on the semaphore, so nobody can see it
	// half-written.
	c := p.cur
	if c != nil && c.refs <= 0 {
		p.cur = nil
	} else {
		c = p.take()
	}
	p.mu.Unlock()

	p.r.collect(c, now)

	p.mu.Lock()
	defer p.mu.Unlock()
	if old := p.cur; old != nil {
		p.park(old)
	}
	p.gen++
	c.gen, c.at, c.refs = p.gen, now, 1
	p.cur = c
	return c.scopes, nil
}

// take returns a free structure, or a new one. Callers hold p.mu.
func (p *Producer) take() *collection {
	if n := len(p.free); n > 0 {
		c := p.free[n-1]
		p.free[n-1] = nil
		p.free = p.free[:n-1]
		return c
	}
	return &collection{fams: make([]famData, len(p.r.fams))}
}

// park moves a replaced current structure to the free or held list.
// Callers hold p.mu.
func (p *Producer) park(c *collection) {
	if c.refs <= 0 {
		if len(p.free) < keepFree {
			p.free = append(p.free, c)
		}
		return
	}
	if len(p.held) >= keepHeld {
		p.held = slices.Delete(p.held, 0, 1)
	}
	p.held = append(p.held, c)
}

// Release returns data obtained from Produce once the caller no longer
// reads it; each result is released at most once. scopes may also carry
// other producers' scopes, which are ignored.
func (p *Producer) Release(scopes []metricdata.ScopeMetrics) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range scopes {
		ms := scopes[i].Metrics
		if len(ms) == 0 {
			continue
		}
		if c := p.cur; c != nil && len(c.metrics) > 0 && &c.metrics[0] == &ms[0] {
			c.refs = max(c.refs-1, 0)
			continue
		}
		for k, c := range p.held {
			if len(c.metrics) == 0 || &c.metrics[0] != &ms[0] {
				continue
			}
			c.refs--
			if c.refs <= 0 {
				p.held = slices.Delete(p.held, k, k+1)
				if len(p.free) < keepFree {
					p.free = append(p.free, c)
				}
			}
			break
		}
	}
}

// ReleaseResource releases the Ruralz scope of rm (see Release).
func (p *Producer) ReleaseResource(rm *metricdata.ResourceMetrics) {
	if rm != nil {
		p.Release(rm.ScopeMetrics)
	}
}

// famSnap is one family's exported groups at the start of a collection,
// with whether each was retiring then.
type famSnap struct {
	groups   []*group
	retiring []bool
	folded   int
}

// snapshot copies, under the registry mutex, what the collection reads.
// The retiring flags are taken at the same instant as the group lists, so
// ruralz_telemetry_series{state="retiring"} counts one consistent state
// that the retiring ceiling held (spec 09 req 56), however lifecycle calls
// interleave with the rest of the collection.
func (r *Registry) snapshot() ([]famSnap, map[funcKey]func() int64, [2]revisionInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]famSnap, len(r.fams))
	total := 0
	for _, f := range r.fams {
		if f.dirty {
			sorted := make([]*group, 0, len(f.fixed)+len(f.groups))
			sorted = append(sorted, f.fixed...)
			for _, g := range f.groups {
				sorted = append(sorted, g)
			}
			slices.SortFunc(sorted, compareGroups)
			f.sorted = sorted
			f.dirty = false
		}
		total += len(f.sorted)
	}
	if cap(r.retiringBuf) < total {
		r.retiringBuf = make([]bool, 0, total+total/4)
	}
	buf := r.retiringBuf[:0]
	for _, f := range r.fams {
		at := len(buf)
		for _, g := range f.sorted {
			buf = append(buf, g.state == stRetiring)
		}
		out[f.idx] = famSnap{groups: f.sorted, retiring: buf[at:len(buf):len(buf)], folded: f.activeFolded + f.ceilingFolded}
	}
	r.retiringBuf = buf
	return out, r.funcs, r.rev
}

// seriesCount accumulates the exported series by state.
type seriesCount struct{ live, retiring int }

func (s *seriesCount) add(retiring bool, n int) {
	if retiring {
		s.retiring += n
		return
	}
	s.live += n
}

// collect fills c from the aggregates at time now. It runs single-flight.
func (r *Registry) collect(c *collection, now time.Time) {
	snaps, funcs, rev := r.snapshot()
	metrics.Read(r.rtSamples)
	c.metrics = c.metrics[:0]
	c.buckets = c.buckets[:0]
	c.exs = c.exs[:0]
	c.ids = c.ids[:0]
	var sc seriesCount
	seriesIdx := -1
	for _, f := range r.fams {
		fd := &c.fams[f.idx]
		fd.sums, fd.gauges, fd.hists = fd.sums[:0], fd.gauges[:0], fd.hists[:0]
		switch f.layout {
		case layoutRevisionInfo:
			for i := range rev {
				if rev[i].display != "" {
					fd.gauges = append(fd.gauges, metricdata.DataPoint[int64]{
						Attributes: rev[i].attrs, StartTime: rev[i].start, Time: now, Value: 1,
					})
					sc.live++
				}
			}
		case layoutComputed:
			if f.cat.Name == catalog.TelemetrySeries {
				seriesIdx = f.idx
				continue
			}
			r.collectComputed(f, fd, snaps, now, &sc)
		case layoutValues, layoutStatus, layoutListenerRequests, layoutAttempts, layoutResultCode, layoutCode:
			fs := snaps[f.idx]
			for i, g := range fs.groups {
				r.collectGroup(c, fd, g, fs.retiring[i], funcs, now, &sc)
			}
		}
	}
	if seriesIdx >= 0 {
		f := r.fams[seriesIdx]
		fd := &c.fams[seriesIdx]
		for i, v := range []int{sc.live, sc.retiring} {
			fd.gauges = append(fd.gauges, metricdata.DataPoint[int64]{
				Attributes: attribute.NewSet(attribute.String(labelState, f.cat.Labels[0].Values[i])),
				StartTime:  r.start, Time: now, Value: int64(v),
			})
		}
	}
	for _, f := range r.fams {
		fd := &c.fams[f.idx]
		m := metricdata.Metrics{Name: f.cat.Name, Unit: f.cat.Unit}
		switch {
		case f.hist:
			if len(fd.hists) == 0 {
				continue
			}
			m.Data = metricdata.Histogram[float64]{DataPoints: fd.hists, Temporality: metricdata.CumulativeTemporality}
		case f.gauge:
			if len(fd.gauges) == 0 {
				continue
			}
			m.Data = metricdata.Gauge[int64]{DataPoints: fd.gauges}
		default:
			if len(fd.sums) == 0 {
				continue
			}
			m.Data = metricdata.Sum[float64]{
				DataPoints: fd.sums, Temporality: metricdata.CumulativeTemporality, IsMonotonic: true,
			}
		}
		c.metrics = append(c.metrics, m)
	}
	if len(c.scopes) == 0 {
		c.scopes = make([]metricdata.ScopeMetrics, 1)
	}
	c.scopes[0] = metricdata.ScopeMetrics{
		Scope:   instrumentation.Scope{Name: ScopeName, Version: r.scope},
		Metrics: c.metrics,
	}
	r.lastCollect = now
}

// collectGroup appends one group's data points.
func (r *Registry) collectGroup(c *collection, fd *famData, g *group, retiring bool, funcs map[funcKey]func() int64, now time.Time, sc *seriesCount) {
	f := g.fam
	switch {
	case f.hist:
		for i := range g.n {
			fd.hists = append(fd.hists, r.histPoint(c, g, i, now))
		}
		sc.add(retiring, g.n*histSeries)
		return
	case f.gauge:
		for i := range g.n {
			v := i64(g.slots[i].sum())
			if fn := funcs[funcKey{f.idx, i}]; fn != nil && f.role == roleNode {
				v = fn()
			}
			fd.gauges = append(fd.gauges, metricdata.DataPoint[int64]{
				Attributes: g.attrs[i], StartTime: g.start, Time: now, Value: v,
			})
		}
		sc.add(retiring, g.n)
		return
	}
	for i := range g.n {
		fd.sums = append(fd.sums, metricdata.DataPoint[float64]{
			Attributes: g.attrs[i], StartTime: g.start, Time: now, Value: g.col[i].add(g.slots[i].sum()),
		})
	}
	sc.add(retiring, g.n)
	if g.code == nil {
		return
	}
	c.codes = g.code.table.entries(c.codes[:0])
	slices.SortFunc(c.codes, func(a, b *codeEntry) int { return strings.Compare(a.code, b.code) })
	for _, e := range c.codes {
		u := e.n.Load()
		if !e.seen {
			if u == 0 {
				continue
			}
			e.seen = true
			e.start = r.lastCollect
			if e.start.Before(g.start) {
				e.start = g.start
			}
			e.attrs = attribute.NewSet(slices.Concat(g.code.base, []attribute.KeyValue{attribute.String(labelCode, e.code)})...)
		}
		e.total += float64(u - e.last)
		e.last = u
		fd.sums = append(fd.sums, metricdata.DataPoint[float64]{
			Attributes: e.attrs, StartTime: e.start, Time: now, Value: e.total,
		})
		sc.add(retiring, 1)
	}
}

// histPoint builds the data point of histogram label set i of g.
func (r *Registry) histPoint(c *collection, g *group, i int, now time.Time) metricdata.HistogramDataPoint[float64] {
	b := g.fam.bounds
	start := len(c.buckets)
	var count uint64
	for k := range numBuckets {
		v := g.cell.sum(i*histWords + k)
		c.buckets = append(c.buckets, v)
		count += v
	}
	sum := g.col[i].add(g.cell.sum(i*histWords + sumWord))
	dp := metricdata.HistogramDataPoint[float64]{
		Attributes:   g.attrs[i],
		StartTime:    g.start,
		Time:         now,
		Count:        count,
		Bounds:       b.floats,
		BucketCounts: c.buckets[start:len(c.buckets):len(c.buckets)],
		Sum:          sum / b.sumDiv,
	}
	if x, ok := g.ex[i].load(); ok {
		ids := len(c.ids)
		c.ids = append(c.ids, x.trace[:]...)
		c.ids = append(c.ids, x.span[:]...)
		ex := len(c.exs)
		c.exs = append(c.exs, metricdata.Exemplar[float64]{
			Time:    x.time(now),
			Value:   float64(x.val) / b.valDiv,
			TraceID: c.ids[ids : ids+16 : ids+16],
			SpanID:  c.ids[ids+16 : ids+24 : ids+24],
		})
		dp.Exemplars = c.exs[ex : ex+1 : ex+1]
	}
	return dp
}

// collectComputed fills the families computed at collection: runtime
// metrics read from runtime/metrics (spec 09 req 46) and the folded label
// set counts per foldable family (req 56).
func (r *Registry) collectComputed(f *family, fd *famData, snaps []famSnap, now time.Time, sc *seriesCount) {
	switch f.cat.Name {
	case catalog.RuntimeGoroutines, catalog.RuntimeHeapBytes, catalog.RuntimeGCCyclesTotal:
		i := slices.IndexFunc(r.rtSamples, func(s metrics.Sample) bool { return runtimeFamily(s.Name) == f.cat.Name })
		if i < 0 {
			return
		}
		var v uint64
		if r.rtSamples[i].Value.Kind() == metrics.KindUint64 {
			v = r.rtSamples[i].Value.Uint64()
		}
		if f.gauge {
			fd.gauges = append(fd.gauges, metricdata.DataPoint[int64]{StartTime: r.start, Time: now, Value: i64(v)})
		} else {
			fd.sums = append(fd.sums, metricdata.DataPoint[float64]{StartTime: r.start, Time: now, Value: float64(v)})
		}
		sc.live++
	case catalog.TelemetryFoldedLabelSets:
		for _, o := range r.fams {
			if !o.foldable() {
				continue
			}
			fd.gauges = append(fd.gauges, metricdata.DataPoint[int64]{
				Attributes: attribute.NewSet(attribute.String(labelInstrument, o.cat.Name)),
				StartTime:  r.start, Time: now, Value: int64(snaps[o.idx].folded),
			})
			sc.live++
		}
	}
}

// runtimeFamily maps a runtime/metrics name to its catalog family.
func runtimeFamily(name string) string {
	switch name {
	case "/sched/goroutines:goroutines":
		return catalog.RuntimeGoroutines
	case "/gc/heap/live:bytes":
		return catalog.RuntimeHeapBytes
	case "/gc/cycles/total:gc-cycles":
		return catalog.RuntimeGCCyclesTotal
	default:
		return ""
	}
}
