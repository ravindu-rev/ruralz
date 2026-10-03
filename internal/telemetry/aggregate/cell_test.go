// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// allBounds are the five histogram bound sets.
func allBounds() []catalog.Bounds {
	return []catalog.Bounds{catalog.BoundsFast, catalog.BoundsRequest, catalog.BoundsControl, catalog.BoundsBytes, catalog.BoundsRatio}
}

// Spec 09 req 42 and test 11: bounds in integers equal the bounds in
// seconds times the scale exactly, and bucket choice at an exact bound is
// inclusive.
func TestHistogramBounds_Req42(t *testing.T) {
	scales := map[catalog.Bounds]float64{
		catalog.BoundsFast: 1e9, catalog.BoundsRequest: 1e9, catalog.BoundsControl: 1e9,
		catalog.BoundsBytes: 1, catalog.BoundsRatio: 1e6,
	}
	for _, set := range allBounds() {
		b, err := newHistBounds(set)
		if err != nil {
			t.Fatal(err)
		}
		for i, s := range set.Seconds() {
			if float64(b.ints[i]) != math.Round(s*scales[set]) {
				t.Errorf("set %d bound %d = %d, want %v", set, i, b.ints[i], s*scales[set])
			}
			if got := b.bucket(b.ints[i]); got != i {
				t.Errorf("set %d: value %d in bucket %d, want %d", set, b.ints[i], got, i)
			}
			if got := b.bucket(b.ints[i] + 1); got != i+1 {
				t.Errorf("set %d: value %d in bucket %d, want %d", set, b.ints[i]+1, got, i+1)
			}
			if i > 0 {
				if got := b.bucket(b.ints[i-1] + 1); got != i {
					t.Errorf("set %d: just above bound %d in bucket %d", set, i-1, got)
				}
			}
		}
		if b.bucket(0) != 0 || b.bucket(math.MaxUint64) != numBounds {
			t.Errorf("set %d: 0 or max in the wrong bucket", set)
		}
	}
	fast, _ := newHistBounds(catalog.BoundsFast)
	// le="0.00015" (index 4) receives 150,000 ns, not 150,001 ns.
	if fast.bucket(150_000) != 4 || fast.bucket(150_001) != 5 {
		t.Errorf("150,000 ns in %d, 150,001 ns in %d", fast.bucket(150_000), fast.bucket(150_001))
	}
	if _, err := newHistBounds(catalog.BoundsNone); err == nil {
		t.Error("BoundsNone accepted")
	}
}

// Spec 09 req 42: _sum is nanoseconds for fast, microseconds rounded to
// the nearest for request and control, bytes, or millionths for ratio.
func TestHistogramSumUnits_Req42(t *testing.T) {
	tests := []struct {
		set   catalog.Bounds
		in    uint64
		units uint64
		base  float64
	}{
		{catalog.BoundsFast, 1234, 1234, 1234e-9},
		{catalog.BoundsRequest, 1499, 1, 1e-6},
		{catalog.BoundsRequest, 1500, 2, 2e-6},
		{catalog.BoundsRequest, 499, 0, 0},
		{catalog.BoundsControl, 2_000_000_000, 2_000_000, 2},
		{catalog.BoundsControl, math.MaxUint64, math.MaxUint64/1000 + 1, float64(math.MaxUint64/1000+1) * 1e-6},
		{catalog.BoundsBytes, 4096, 4096, 4096},
		{catalog.BoundsRatio, 1_010_000, 1_010_000, 1.01},
	}
	for _, tt := range tests {
		b, _ := newHistBounds(tt.set)
		got := b.sumUnits(tt.in)
		if got != tt.units {
			t.Errorf("set %d sumUnits(%d) = %d, want %d", tt.set, tt.in, got, tt.units)
		}
		if !approx(float64(got)/b.sumDiv, tt.base) {
			t.Errorf("set %d base %v, want %v", tt.set, float64(got)/b.sumDiv, tt.base)
		}
	}
}

// Spec 09 test 11: statuses 100 to 599 map to their class; anything else
// (no response, client abort codes outside the range) counts as 5xx.
func TestStatusClass(t *testing.T) {
	tests := map[int]int{
		100: 0, 199: 0, 200: 1, 204: 1, 304: 2, 399: 2, 400: 3, 404: 3, 499: 3, 500: 4, 599: 4,
		0: 4, 99: 4, 600: 4, 1000: 4, -1: 4, -200: 4,
	}
	for in, want := range tests {
		if got := statusClass(in); got != want {
			t.Errorf("statusClass(%d) = %d, want %d", in, got, want)
		}
	}
	for s := 100; s < 600; s++ {
		if got := statusClass(s); got != s/100-1 {
			t.Fatalf("statusClass(%d) = %d", s, got)
		}
	}
}

// Spec 09 req 50 and test 11: collection adds each delta since the last
// collection modulo 2^64 to a float64 total, so a stripe wrapping near
// 2^64 never makes the total go backwards; gauges sum their stripes.
func TestCounterWrapAndGaugeStripes_Req50(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	c, err := r.Counter(catalog.TelemetrySpansTotal)
	if err != nil {
		t.Fatal(err)
	}
	key := catalog.TelemetrySpansTotal + "{}"
	// Put the word and the collector's state where a counter that already
	// counted 2^64-10 events stands; the float total restarts at 100 so
	// small deltas stay visible.
	g := r.nodeGroup(catalog.TelemetrySpansTotal)
	g.cell.v[0].Store(math.MaxUint64 - 9)
	g.col[0] = colState{last: math.MaxUint64 - 9, tot: 100}
	c.Add(0, 20) // the stripe word wraps to 10
	first := mustGet(t, collectNow(t, r, clk), key).value
	if first != 120 {
		t.Errorf("total after a wrapping add = %v, want 120", first)
	}
	c.Add(1, 5)
	if second := mustGet(t, collectNow(t, r, clk), key).value; second != 125 {
		t.Errorf("total = %v, want 125", second)
	}
	c.Add(0, math.MaxUint64-4) // stripe 0 wraps to 5, the sum over stripes to 10
	if third := mustGet(t, collectNow(t, r, clk), key).value; third < 125 {
		t.Errorf("total went backwards: %v", third)
	}

	// Striped gauge: Adds on several stripes sum; Set keeps concurrent Adds.
	pl, bind := admitBind(t, r, emit.Shape{Listeners: []string{"public"}})
	defer bind.Release()
	active := pl.Listener("public").Active
	for s := range emit.Stripe(8) {
		active.Add(s, 2)
	}
	active.Add(3, -5)
	gkey := catalog.HTTPActiveRequests + `{listener="public"}`
	if got := mustGet(t, collectNow(t, r, clk), gkey).value; got != 11 {
		t.Errorf("active = %v, want 11", got)
	}
	active.Set(4)
	active.Add(2, 1)
	if got := mustGet(t, collectNow(t, r, clk), gkey).value; got != 5 {
		t.Errorf("active after Set(4)+1 = %v, want 5", got)
	}
	active.Set(-3)
	if got := mustGet(t, collectNow(t, r, clk), gkey).value; got != -3 {
		t.Errorf("active after Set(-3) = %v", got)
	}
	q := r.Node().State.QueueItems // unstriped
	q.Add(0, 3)
	q.Set(9)
	q.Add(1, -2)
	if got := mustGet(t, collectNow(t, r, clk), catalog.StateWriteQueueItems+"{}").value; got != 7 {
		t.Errorf("queue items = %v, want 7", got)
	}
}

// Spec 09 req 49: a striped cell's blocks never share a cache line.
func TestStripedCellLayout_Req49(t *testing.T) {
	var tab [256]uint8
	for i := range tab {
		tab[i] = uint8(i % 8)
	}
	for _, words := range []int{1, 5, 9, 16, 30, 32, 240} {
		c := newCell(&tab, words, 8, true)
		if c.stride%lineWords != 0 || c.stride < words {
			t.Errorf("words %d: stride %d", words, c.stride)
		}
		if len(c.v) != c.stride*8 {
			t.Errorf("words %d: len %d", words, len(c.v))
		}
	}
	if c := newCell(&tab, 5, 8, false); c.stride != 0 || len(c.v) != 5 {
		t.Errorf("unstriped cell stride %d len %d", c.stride, len(c.v))
	}
	if c := newCell(&tab, 5, 1, true); c.stride != 0 {
		t.Errorf("one stripe still striped")
	}
}

// Spec 09 req 52: exemplars go through a sequence lock; readers never see
// a torn exemplar, even with concurrent writers.
func TestExemplarSequenceLock_Req52(t *testing.T) {
	var e exemplarSlot
	if _, ok := e.load(); ok {
		t.Fatal("empty slot loaded")
	}
	mk := func(v byte) emit.Exemplar {
		var ex emit.Exemplar
		for k := range ex.TraceID {
			ex.TraceID[k] = v
		}
		for k := range ex.SpanID {
			ex.SpanID[k] = v
		}
		ex.Time = time.Unix(int64(v), 0)
		return ex
	}
	ex := mk(7)
	e.store(7, &ex)
	x, ok := e.load()
	if !ok || x.val != 7 || x.trace != ex.TraceID || x.span != ex.SpanID || !x.time(time.Time{}).Equal(ex.Time) {
		t.Fatalf("load = %+v %v", x, ok)
	}
	var zero emit.Exemplar
	e.store(1, &zero)
	x, _ = e.load()
	if fb := time.Unix(99, 0); !x.time(fb).Equal(fb) {
		t.Errorf("zero exemplar time = %v, want the fallback", x.time(fb))
	}

	e = exemplarSlot{}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := range byte(4) {
		wg.Go(func() {
			for v := w + 1; ; v += 4 {
				select {
				case <-stop:
					return
				default:
				}
				ex := mk(v)
				e.store(uint64(v), &ex)
			}
		})
	}
	for range 20000 {
		x, ok := e.load()
		if !ok {
			continue
		}
		for _, tb := range x.trace {
			if uint64(tb) != x.val {
				t.Fatalf("torn exemplar %+v", x)
			}
		}
		if uint64(x.span[7]) != x.val || x.at != int64(x.span[0])*1e9 {
			t.Fatalf("torn exemplar %+v", x)
		}
	}
	close(stop)
	wg.Wait()
}

// Spec 09 test 23 (property): cumulative buckets are non-decreasing,
// _count equals the +Inf bucket and _sum is exact, for random streams on
// random stripes of every bound set.
func TestHistogramInvariants_Test23(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	rng := testRand(23, 42)
	type ref struct {
		count   uint64
		units   uint64
		buckets [numBuckets]uint64
	}
	names := map[catalog.Bounds]string{}
	for _, f := range r.fams {
		if f.hist && f.role == roleNode {
			names[f.cat.Bounds] = f.cat.Name
		}
	}
	pl, bind := admitBind(t, r, emit.Shape{Listeners: []string{"l"}})
	defer bind.Release()
	hists := map[string]emit.Histogram{
		fmt.Sprintf(`%s{op="gcra"}`, catalog.StateCallDurationSeconds):                                r.Node().State.CallDuration[emit.StateOpGCRA],
		fmt.Sprintf(`%s{size_class="le1000",stage="total"}`, catalog.ConfigActivationDurationSeconds): r.Node().Config.ActivationDuration("total", "le1000"),
		fmt.Sprintf(`%s{listener="l"}`, catalog.HTTPRequestBodyBytes):                                 pl.Listener("l").RequestBody,
		fmt.Sprintf(`%s{listener="l"}`, catalog.ListenerTLSHandshakeDurationSeconds):                  pl.Listener("l").TLSHandshake,
	}
	bounds := map[string]*histBounds{}
	for k := range hists {
		name, _, _ := strings.Cut(k, "{")
		bounds[k] = r.byName[name].bounds
	}
	refs := map[string]*ref{}
	for k := range hists {
		refs[k] = &ref{}
	}
	for round := range 5 {
		for k, h := range hists {
			b := bounds[k]
			rf := refs[k]
			for range 500 {
				v := rng.Uint64N(b.ints[numBounds-1] * 2)
				if rng.IntN(10) == 0 {
					v = b.ints[rng.IntN(numBounds)]
				}
				h.Record(stripe(rng.IntN(256)), v)
				rf.count++
				rf.units += b.sumUnits(v)
				rf.buckets[b.bucket(v)]++
			}
		}
		pts := collectNow(t, r, clk)
		for k := range hists {
			p := mustGet(t, pts, k)
			rf := refs[k]
			var cum uint64
			for i, n := range p.buckets {
				if n != rf.buckets[i] {
					t.Fatalf("round %d %s bucket %d = %d, want %d", round, k, i, n, rf.buckets[i])
				}
				next := cum + n
				if next < cum {
					t.Fatalf("%s cumulative bucket overflow", k)
				}
				cum = next
			}
			if p.count != cum || p.count != rf.count {
				t.Fatalf("%s count %d, +Inf %d, want %d", k, p.count, cum, rf.count)
			}
			if want := float64(rf.units) / bounds[k].sumDiv; !approx(p.sum, want) {
				t.Fatalf("%s sum %v, want %v", k, p.sum, want)
			}
		}
	}
}

// Spec 09 req 52 and test 45: only RecordExemplar touches the exemplar
// slot; Record, the unsampled path, never does.
func TestUnsampledNeverTouchesExemplar_Test45(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	pl, bind := admitBind(t, r, emit.Shape{Routes: []string{"orders"}})
	defer bind.Release()
	d := pl.Route("orders").Duration
	for i := range 1000 {
		d.Record(emit.Stripe(i), uint64(i)*1000)
	}
	g := r.byName[catalog.HTTPRequestDurationSeconds].groups[groupKey{res: "orders", phase: -1}]
	if seq := g.ex[0].seq.Load(); seq != 0 {
		t.Fatalf("exemplar sequence = %d after unsampled records", seq)
	}
	p := mustGet(t, collectNow(t, r, clk), catalog.HTTPRequestDurationSeconds+`{route="orders"}`)
	if len(p.exemplars) != 0 {
		t.Fatalf("exemplars %v", p.exemplars)
	}
	ex := emit.Exemplar{TraceID: [16]byte{1, 2, 3}, SpanID: [8]byte{9}, Time: epoch().Add(time.Second)}
	d.RecordExemplar(1, 2_500_000, ex)
	p = mustGet(t, collectNow(t, r, clk), catalog.HTTPRequestDurationSeconds+`{route="orders"}`)
	if len(p.exemplars) != 1 {
		t.Fatalf("exemplars %v", p.exemplars)
	}
	e := p.exemplars[0]
	if e.Value != 0.0025 || string(e.TraceID) != string(ex.TraceID[:]) || string(e.SpanID) != string(ex.SpanID[:]) || !e.Time.Equal(ex.Time) {
		t.Errorf("exemplar = %+v", e)
	}
	if p.count != 1001 {
		t.Errorf("count = %d", p.count)
	}
}
