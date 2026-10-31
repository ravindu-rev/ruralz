// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"fmt"
	"math"
	"strconv"
	"time"

	telcatalog "github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// Metric names the criteria read (docs/architecture/10-observability.md
// "Ruralz Gateway metrics").
const (
	metricActive         = telcatalog.HTTPActiveRequests
	metricOpenConns      = telcatalog.ListenerOpenConnections
	metricConnsTotal     = telcatalog.ListenerConnectionsTotal
	metricHandshake      = telcatalog.ListenerTLSHandshakeDurationSeconds
	metricNodeResponses  = telcatalog.HTTPNodeResponsesTotal
	metricGoroutines     = telcatalog.RuntimeGoroutines
	metricHeap           = telcatalog.RuntimeHeapBytes
	metricPoolConns      = telcatalog.UpstreamPoolConnections
	metricStateInflight  = telcatalog.StateInflightCalls
	metricStateCalls     = telcatalog.StateCallsTotal
	metricRetired        = telcatalog.ConfigRetiredSnapshots
	metricActivations    = telcatalog.ConfigActivationsTotal
	metricActivationTime = telcatalog.ConfigActivationDurationSeconds
	metricBuffered       = telcatalog.NodeBufferedBytes
	metricDegraded       = telcatalog.NodeDegradedInfo
	metricUpDegraded     = telcatalog.UpstreamDegradedInfo
	metricLogsDropped    = telcatalog.TelemetryLogsDroppedTotal
	metricSpansDropped   = telcatalog.TelemetrySpansDroppedTotal
	metricUnsampled      = telcatalog.TelemetryTracesUnsampledTotal
	metricQuota          = telcatalog.QuotaDecisionsTotal
	metricCache          = telcatalog.CacheRequestsTotal
	metricRequests       = telcatalog.HTTPRequestsTotal
)

// Ceilings the criteria check (targets).
const (
	// InFlightUnits is the Node's in-flight unit ceiling (O1).
	InFlightUnits = 20000
	// StateInFlight is the Node's State Store call ceiling (O2).
	StateInFlight = 8192
	// ShardConnections is a Node's State Store connections per shard: 2
	// pipelined plus 8 dedicated.
	ShardConnections = 2 + 8
	// GoroutineReserve is O2's goroutine allowance beyond its connections
	// and call ceiling.
	GoroutineReserve = 1000
	// PreRoutingWrites is the Node's pre-routing write ceiling.
	PreRoutingWrites = 2000
	// RetiredSnapshots is K, the retired snapshots a Node pins, plus at
	// most one closing or ending.
	RetiredSnapshots = 2
	// BufferedBytes is limits.maxBufferedBytes' default, 512 MiB.
	BufferedBytes = 512 << 20
	// plateauTolerance is how close to its ceiling a gauge's highest
	// sample must come for a plateau: requests and calls end and start
	// between samples.
	plateauTolerance = 0.01
)

// inWindow returns the good samples taken in [start, end].
func inWindow(samples []harness.NodeSample, start, end time.Time) []harness.NodeSample {
	var out []harness.NodeSample
	for _, s := range samples {
		if s.Err == nil && !s.At.Before(start) && !s.At.After(end) {
			out = append(out, s)
		}
	}
	return out
}

// good returns the good samples.
func good(samples []harness.NodeSample) []harness.NodeSample {
	var out []harness.NodeSample
	for _, s := range samples {
		if s.Err == nil {
			out = append(out, s)
		}
	}
	return out
}

// gaugeMax returns a gauge's highest sum over its matching series among
// the samples, and how many samples it read.
func gaugeMax(samples []harness.NodeSample, name string, match promtext.Labels) (float64, int) {
	hi, n := math.Inf(-1), 0
	for _, s := range good(samples) {
		hi = math.Max(hi, s.Metrics.Sum(name, match))
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return hi, n
}

// counterDelta returns a counter's increase from the first to the last
// good sample.
func counterDelta(samples []harness.NodeSample, name string, match promtext.Labels) float64 {
	g := good(samples)
	if len(g) < 2 {
		return 0
	}
	return promtext.Delta(g[0].Metrics, g[len(g)-1].Metrics, name, match)
}

// fmtCount formats a count.
func fmtCount(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// plateau judges a gauge that reaches its ceiling, within the tolerance,
// and never passes it, over the samples of the measured window.
func plateau(id, value string, samples []harness.NodeSample, name string, ceiling float64) benchrecord.BudgetResult {
	hi, n := gaugeMax(samples, name, nil)
	if n == 0 {
		return judge(id, value, false, "no Node sample in the measured window")
	}
	ok := hi <= ceiling && hi >= ceiling*(1-plateauTolerance)
	return judge(id, value, ok, fmt.Sprintf("highest %s over %d samples", fmtCount(hi), n))
}

// accounting judges the load's admitted responses plus the counted codes
// against the offered requests: within 0.5% (target), loadgen's rate rule
// with the codes counting as achieved.
func accounting(id, value string, r *lab.LoadResult) benchrecord.BudgetResult {
	if r == nil || r.Offered == 0 {
		return judge(id, value, false, "no request offered")
	}
	return judge(id, value, r.RateError <= harness.MaxRateError,
		fmt.Sprintf("%d of %d offered requests admitted or counted, rate error %.4f; %s", r.Achieved, r.Offered, r.RateError, breakdown(r)))
}

// breakdown summarizes a load's answers: statuses, RZ codes, outcomes and
// its first failure.
func breakdown(r *lab.LoadResult) string {
	s := fmt.Sprintf("statuses %v, codes %v, outcomes %v", r.Statuses, r.Codes, r.Outcomes)
	if len(r.Failures) > 0 {
		s += ", first failure " + r.Failures[0]
	}
	return s
}

// noCode judges codeUpstream, a code the load must never see.
func noCode(id, value string, r *lab.LoadResult) benchrecord.BudgetResult {
	n := codesOf(r, codeUpstream)
	return judge(id, value, n == 0, fmt.Sprintf("%d responses with %s", n, codeUpstream))
}

// goroutineBound judges O2's goroutine rule on every sample: fewer
// goroutines than one per open client connection, one per request in
// flight (net/http's background read of an HTTP/1.1 connection whose
// handler runs, watching for the client closing), two per open upstream
// connection, the call ceiling and the reserve (target).
func goroutineBound(id, value string, samples []harness.NodeSample) benchrecord.BudgetResult {
	worst, n := math.Inf(1), 0
	var at string
	for _, s := range good(samples) {
		n++
		gr := s.Metrics.Sum(metricGoroutines, nil)
		bound := s.Metrics.Sum(metricOpenConns, nil) + s.Metrics.Sum(metricActive, nil) +
			2*s.Metrics.Sum(metricPoolConns, nil) + StateInFlight + GoroutineReserve
		if room := bound - gr; room < worst {
			worst, at = room, fmt.Sprintf("%s goroutines against a bound of %s", fmtCount(gr), fmtCount(bound))
		}
	}
	if n == 0 {
		return judge(id, value, false, "no Node sample in the measured window")
	}
	return judge(id, value, worst > 0, "tightest of "+strconv.Itoa(n)+" samples: "+at)
}

// histogramDelta returns a Node histogram's increase between the first
// and the last good sample.
func histogramDelta(samples []harness.NodeSample, name string, match promtext.Labels) (promtext.Histogram, bool) {
	g := good(samples)
	if len(g) < 2 {
		return promtext.Histogram{}, false
	}
	after, err := g[len(g)-1].Metrics.Histogram(name, match)
	if err != nil {
		return promtext.Histogram{}, false
	}
	before, err := g[0].Metrics.Histogram(name, match)
	if err != nil {
		before = promtext.Histogram{}
	}
	prev := map[float64]float64{}
	for _, b := range before.Buckets {
		prev[b.UpperBound] = b.Count
	}
	out := promtext.Histogram{Count: after.Count - before.Count, Sum: after.Sum - before.Sum}
	for _, b := range after.Buckets {
		out.Buckets = append(out.Buckets, promtext.Bucket{UpperBound: b.UpperBound, Count: math.Max(b.Count-prev[b.UpperBound], 0)})
	}
	return out, true
}

// quantileBound judges the p99 of the Node's TLS handshake histogram over
// the samples against limit.
func quantileBound(id, value string, samples []harness.NodeSample, match promtext.Labels, limit time.Duration) benchrecord.BudgetResult {
	h, ok := histogramDelta(samples, metricHandshake, match)
	if !ok || h.Count == 0 {
		return judge(id, value, false, "no observation of "+metricHandshake)
	}
	v := h.Quantile(0.99)
	if math.IsNaN(v) {
		return judge(id, value, false, "no quantile of "+metricHandshake)
	}
	d := time.Duration(v * float64(time.Second))
	return judge(id, value, d <= limit, fmt.Sprintf("%s over %s observations", d, fmtCount(h.Count)))
}

// interpolate returns the rate at which the measured script CPU share
// reaches G1ScriptCPU, linearly between the last step below it and the
// first at or above it; ok is false when no step reaches it or the first
// step already does.
func interpolate(rates, levels []float64) (float64, bool) {
	if len(levels) > 0 && levels[0] >= G1ScriptCPU {
		return rates[0], false
	}
	for i := 1; i < len(levels); i++ {
		if levels[i] < G1ScriptCPU {
			continue
		}
		r0, r1, l0, l1 := rates[i-1], rates[i], levels[i-1], levels[i]
		if l1 == l0 {
			return r1, true
		}
		return r0 + (G1ScriptCPU-l0)*(r1-r0)/(l1-l0), true
	}
	return 0, false
}
