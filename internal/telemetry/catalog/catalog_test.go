// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Tests for the telemetry catalog: naming, units, label values, histogram
// bounds, the M1 families, degraded reasons and hops, and the static half
// of the catalog gates (name grammar, suffixes, unique names, span names).
// The catalog has no Validate(): these tests hold the name checks for the
// catalog itself, and repocheck holds the checks of code outside it.

// family is one row: labels are "name", "name=v1|v2" for an enumeration
// or "name!" for an RZ code label.
type family struct {
	name    string
	kind    Kind
	bounds  Bounds
	unit    string
	labels  []string
	class   Class
	striped bool
}

const (
	sc    = "status_class=1xx|2xx|3xx|4xx|5xx"
	proto = "protocol=http1|http2"
	ph    = "phase=onRequestHeaders|onRequestBody|onRoute|onUpstreamRequest|onUpstreamResponseHeaders|onUpstreamResponseBody|onResponse|onLog|onChunk"
	ops   = "gcra|quota|cache_get|script_multi|pipeline|cache_set|cache_invalidate|refund"
	// singleOps is the `kind` of ruralz_state_ops_total, "any
	// single-operation op" in Observability: ops without the round-trip
	// labels.
	singleOps = "gcra|quota|cache_get|cache_set|cache_invalidate|refund"
)

// contractDeviations lists the families whose committed labels differ
// from the M1 family table; each value is the committed label set. An
// entry is removed once its deviation is settled.
func contractDeviations() map[string][]string {
	return map[string][]string{
		// The catalog gives `kind` the full Ops() list, so the
		// script_multi and pipeline round-trip labels are pre-created too;
		// emit.StateMetrics.Ops keeps one slot per StateOp* index either
		// way. Settling it means restricting `kind` to the single-operation
		// ops or recording the deviation in Observability.
		StateOpsTotal: {"kind=" + ops},
	}
}

func m1Families() []family {
	L, R, U, P := ClassListener, ClassRoute, ClassUpstream, ClassPolicy
	return []family{
		{HTTPRequestsTotal, Counter, BoundsNone, "{requests}", []string{"route", sc}, R, true},
		{HTTPListenerRequestsTotal, Counter, BoundsNone, "{requests}", []string{"listener", proto, sc, "origin=upstream|node|dependency"}, L, true},
		{HTTPNodeResponsesTotal, Counter, BoundsNone, "{responses}", []string{"code!"}, L, false},
		{HTTPRequestDurationSeconds, Histogram, BoundsRequest, "s", []string{"route"}, R, true},
		{HTTPGatewayDurationSeconds, Histogram, BoundsFast, "s", []string{"listener"}, L, true},
		{HTTPGatewayDurationSkippedTotal, Counter, BoundsNone, "{requests}", []string{"reason=clock_anomaly"}, L, false},
		{HTTPRequestBodyBytes, Histogram, BoundsBytes, "By", []string{"listener"}, L, true},
		{HTTPResponseBodyBytes, Histogram, BoundsBytes, "By", []string{"listener"}, L, true},
		{HTTPActiveRequests, Gauge, BoundsNone, "{requests}", []string{"listener"}, L, true},
		{ListenerOpenConnections, Gauge, BoundsNone, "{connections}", []string{"listener", proto}, L, false},
		{ListenerConnectionsTotal, Counter, BoundsNone, "{connections}", []string{"listener", proto, "result=accepted|tls_failure|refused"}, L, false},
		{ListenerTLSHandshakeDurationSeconds, Histogram, BoundsRequest, "s", []string{"listener"}, L, false},
		{FilterDurationSeconds, Histogram, BoundsFast, "s", []string{"policy", ph}, P, true},
		{FilterShortCircuitsTotal, Counter, BoundsNone, "{responses}", []string{"policy", ph, sc}, P, true},
		{FilterFailuresTotal, Counter, BoundsNone, "{failures}", []string{"policy", ph, "mode=open|closed"}, P, false},
		{AuthDecisionsTotal, Counter, BoundsNone, "{decisions}", []string{"policy", "result=allow|deny", "code!"}, P, true},
		{AuthJWKSAgeSeconds, Gauge, BoundsNone, "s", []string{"policy"}, P, false},
		{AuthUpstreamTokenAgeSeconds, Gauge, BoundsNone, "s", []string{"policy"}, P, false},
		{AuthUpstreamRefreshFailuresTotal, Counter, BoundsNone, "{failures}", []string{"policy"}, P, false},
		{AuthBasicRefusalsTotal, Counter, BoundsNone, "{refusals}", []string{"cause=queue|create|delay|source"}, L, false},
		{SecurityCleartextHops, Gauge, BoundsNone, "{hops}", []string{"hop=client|upstream|state_store|telemetry|admin"}, L, false},
		{RateLimitDecisionsTotal, Counter, BoundsNone, "{decisions}", []string{"policy", "result=allow|deny_local|deny_global|fail_open|fail_closed|allow_local"}, P, true},
		{RateLimitBucketEvictionsTotal, Counter, BoundsNone, "{entries}", []string{"policy", "segment=main|local_only"}, P, false},
		{QuotaDecisionsTotal, Counter, BoundsNone, "{decisions}", []string{"policy", "result=allow|deny|no_quota|fail_open|fail_closed"}, P, true},
		{CacheRequestsTotal, Counter, BoundsNone, "{requests}", []string{"route", "result=hit|miss|bypass|stale|stale_error|coalesced"}, R, false},
		{CacheStoreSkippedTotal, Counter, BoundsNone, "{stores}", []string{"policy", "reason=size_limit|buffer_budget|memory"}, P, false},
		{CacheRevalidationsTotal, Counter, BoundsNone, "{revalidations}", []string{"route", "result=refreshed|ended|kept|skipped"}, R, false},
		{CacheRevalidationDropsTotal, Counter, BoundsNone, "{revalidations}", []string{"reason=queue_full|queue_wait|partitions_full"}, L, false},
		{UpstreamAttemptsTotal, Counter, BoundsNone, "{attempts}", []string{"upstream", sc, "error=none|connect|timeout|reset|tls"}, U, true},
		{UpstreamAttemptDurationSeconds, Histogram, BoundsRequest, "s", []string{"upstream"}, U, true},
		{UpstreamRetriesTotal, Counter, BoundsNone, "{retries}", []string{"upstream"}, U, false},
		{UpstreamRetryBudgetExhaustedTotal, Counter, BoundsNone, "{retries}", []string{"upstream"}, U, false},
		{UpstreamBreakerStateInfo, Gauge, BoundsNone, "1", []string{"upstream", "state=closed|open|half_open"}, U, false},
		{UpstreamEjectionsTotal, Counter, BoundsNone, "{ejections}", []string{"upstream", "reason=passive|active"}, U, false},
		{UpstreamHealthyEndpoints, Gauge, BoundsNone, "{endpoints}", []string{"upstream"}, U, false},
		{UpstreamProbesSkippedTotal, Counter, BoundsNone, "{probes}", []string{"upstream"}, U, false},
		{UpstreamDegradedInfo, Gauge, BoundsNone, "1", []string{"upstream", "reason=panic|discovery_stale|balancer_budget"}, U, false},
		{UpstreamCELErrorsTotal, Counter, BoundsNone, "{errors}", []string{"upstream", "field=hashKey|retryOn|failureWhen"}, U, false},
		{UpstreamPoolConnections, Gauge, BoundsNone, "{connections}", []string{"upstream", "state=idle|active"}, U, false},
		{StateCallDurationSeconds, Histogram, BoundsFast, "s", []string{"op=" + ops, "zone=same|cross|unknown"}, L, true},
		{StateCallsTotal, Counter, BoundsNone, "{calls}", []string{"op=" + ops, "result=ok|error|timeout|skipped"}, L, true},
		{StateOpsTotal, Counter, BoundsNone, "{operations}", []string{"kind=" + singleOps}, L, true},
		{StateWritesDroppedTotal, Counter, BoundsNone, "{writes}", []string{"kind=refund|cache_set|cache_invalidate"}, L, false},
		{StateWriteQueueItems, Gauge, BoundsNone, "{items}", nil, L, false},
		{ConfigRevisionInfo, Gauge, BoundsNone, "1", []string{"revision", "role=active|lkg"}, L, false},
		{ConfigActivationsTotal, Counter, BoundsNone, "{activations}", []string{"result=activated|rejected", "code!"}, L, false},
		{ConfigActivationDurationSeconds, Histogram, BoundsControl, "s", []string{"stage=verify|plugin_compile|compile|swap|total", "size_class=le1000|le10000|gt10000"}, L, false},
		{ConfigRetiredSnapshots, Gauge, BoundsNone, "{snapshots}", nil, L, false},
		{SnapshotRetirementEndedTotal, Counter, BoundsNone, "{requests}", nil, L, false},
		{ConfigSecretRotationFailuresTotal, Counter, BoundsNone, "{failures}", []string{"provider=env|file|kubernetes|vault"}, L, false},
		{NodeDegradedInfo, Gauge, BoundsNone, "1", []string{"reason=" + strings.Join(ReasonNames(), "|")}, L, false},
		{NodeBufferedBytes, Gauge, BoundsNone, "By", nil, L, false},
		{RuntimeGoroutines, Gauge, BoundsNone, "{goroutines}", nil, L, false},
		{RuntimeHeapBytes, Gauge, BoundsNone, "By", nil, L, false},
		{RuntimeGCCyclesTotal, Counter, BoundsNone, "{cycles}", nil, L, false},
		{RuntimeGCCPUSecondsTotal, Counter, BoundsNone, "s", nil, L, false},
		{RuntimeAllocationsTotal, Counter, BoundsNone, "{objects}", nil, L, false},
		{TelemetrySpansTotal, Counter, BoundsNone, "{spans}", nil, L, false},
		{TelemetrySpansDroppedTotal, Counter, BoundsNone, "{spans}", []string{"reason=queue_full|export_error"}, L, false},
		{TelemetryTracesUnsampledTotal, Counter, BoundsNone, "{traces}", []string{"reason=rate_cap_root|rate_cap_parent"}, L, false},
		{TelemetryLogsTotal, Counter, BoundsNone, "{records}", []string{"stream=access|process"}, L, false},
		{TelemetryLogsDroppedTotal, Counter, BoundsNone, "{records}", []string{"stream=access|process", "sink=stdout|otlp", "reason=queue_full|export_error"}, L, false},
		{TelemetryAccessLogCELErrorsTotal, Counter, BoundsNone, "{errors}", nil, L, false},
		{TelemetryFoldedLabelSets, Gauge, BoundsNone, "{label_sets}", []string{"instrument"}, L, false},
		{TelemetrySeries, Gauge, BoundsNone, "{series}", []string{"state=live|retiring"}, L, false},
		{TapEventsDroppedTotal, Counter, BoundsNone, "{events}", nil, L, false},
	}
}

func labelSpec(l Label) string {
	switch {
	case l.Code:
		return l.Name + "!"
	case l.Values != nil:
		return l.Name + "=" + strings.Join(l.Values, "|")
	default:
		return l.Name
	}
}

func TestFamiliesMatchSpec(t *testing.T) {
	want := m1Families()
	got := Families()
	if len(got) != len(want) {
		t.Fatalf("%d families, want the %d M1 families", len(got), len(want))
	}
	deviations := contractDeviations()
	for i, w := range want {
		g := got[i]
		var labels []string
		for _, l := range g.Labels {
			labels = append(labels, labelSpec(l))
		}
		if dev, ok := deviations[w.name]; ok {
			// The committed labels are the recorded deviation, which
			// really differs from the spec row.
			if !slices.Equal(labels, dev) || slices.Equal(dev, w.labels) {
				t.Errorf("family %s labels %v; recorded deviation %v from spec %v", w.name, labels, dev, w.labels)
			}
			w.labels = dev
			delete(deviations, w.name)
		}
		if g.Name != w.name || g.Kind != w.kind || g.Bounds != w.bounds || g.Unit != w.unit ||
			g.Class != w.class || g.Striped != w.striped || !slices.Equal(labels, w.labels) {
			t.Errorf("family %d = %s %d %d %s %v class %d striped %v;\nwant %s %d %d %s %v class %d striped %v",
				i, g.Name, g.Kind, g.Bounds, g.Unit, labels, g.Class, g.Striped,
				w.name, w.kind, w.bounds, w.unit, w.labels, w.class, w.striped)
		}
	}
	for name := range deviations {
		t.Errorf("recorded deviation for %s matches no M1 family", name)
	}
}

func TestNaming(t *testing.T) {
	// Name grammar, unique names, kind suffixes and units.
	loose := regexp.MustCompile(`^ruralz_[a-z0-9_]+$`)
	grammar := regexp.MustCompile(`^ruralz_[a-z][a-z0-9]*(_[a-z0-9]+)+$`)
	noun := regexp.MustCompile(`^\{[a-z_]+\}$`)
	seen := map[string]bool{}
	for _, f := range Families() {
		n := f.Name
		if !loose.MatchString(n) || !grammar.MatchString(n) {
			t.Errorf("%s does not match the name grammar", n)
		}
		if seen[n] {
			t.Errorf("%s declared twice", n)
		}
		seen[n] = true
		switch f.Kind {
		case Counter:
			if !strings.HasSuffix(n, "_total") {
				t.Errorf("counter %s does not end _total", n)
			}
			if f.Bounds != BoundsNone {
				t.Errorf("counter %s has bounds", n)
			}
		case Histogram:
			if !strings.HasSuffix(n, "_seconds") && !strings.HasSuffix(n, "_bytes") && !strings.HasSuffix(n, "_ratio") {
				t.Errorf("histogram %s does not end _seconds, _bytes or _ratio", n)
			}
			if f.Bounds == BoundsNone {
				t.Errorf("histogram %s has no bound set", n)
			}
		case Gauge:
			if strings.HasSuffix(n, "_total") {
				t.Errorf("gauge %s ends _total", n)
			}
			if f.Bounds != BoundsNone {
				t.Errorf("gauge %s has bounds", n)
			}
		default:
			t.Errorf("%s has kind %d", n, f.Kind)
		}
		// The unit suffix precedes a counter's _total:
		// ruralz_<component>_<name>_<unit>, counters ending _total.
		base := n
		if f.Kind == Counter {
			base = strings.TrimSuffix(n, "_total")
		}
		switch f.Unit {
		case "s":
			if !strings.HasSuffix(base, "_seconds") {
				t.Errorf("%s has unit s but does not end _seconds", n)
			}
		case "By":
			if !strings.HasSuffix(base, "_bytes") {
				t.Errorf("%s has unit By but does not end _bytes", n)
			}
		case "1":
			if !strings.HasSuffix(n, "_info") && !strings.HasSuffix(n, "_ratio") {
				t.Errorf("%s has unit 1 but does not end _info or _ratio", n)
			}
		default:
			if !noun.MatchString(f.Unit) {
				t.Errorf("%s unit %q is not s, By, 1 or a {counted_noun}", n, f.Unit)
			}
			if f.Kind == Counter && !strings.HasSuffix(n, "_total") {
				t.Errorf("%s is a counted noun without _total", n)
			}
		}
		if f.Class < ClassListener || f.Class > ClassPolicy {
			t.Errorf("%s has class %d", n, f.Class)
		}
	}
}

func TestLabels(t *testing.T) {
	// Resource labels carry names, enumerations are fixed, codes are
	// registered RZ codes; names are snake_case.
	name := regexp.MustCompile(`^[a-z][a-z_]*$`)
	resource := []string{"route", "upstream", "policy", "listener", "revision", "instrument"}
	for _, f := range Families() {
		seen := map[string]bool{}
		for _, l := range f.Labels {
			if seen[l.Name] {
				t.Errorf("%s: label %s declared twice", f.Name, l.Name)
			}
			seen[l.Name] = true
			if !name.MatchString(l.Name) {
				t.Errorf("%s: label %q is not snake_case", f.Name, l.Name)
			}
			switch {
			case l.Code:
				if l.Name != "code" || l.Values != nil {
					t.Errorf("%s: code label %+v", f.Name, l)
				}
			case l.Values == nil:
				if !slices.Contains(resource, l.Name) {
					t.Errorf("%s: label %s has no values and is not a resource label", f.Name, l.Name)
				}
			default:
				vals := map[string]bool{}
				for _, v := range l.Values {
					if v == "" || vals[v] {
						t.Errorf("%s: label %s value %q empty or repeated", f.Name, l.Name, v)
					}
					vals[v] = true
				}
			}
		}
		// Resource classes carry their resource label first.
		want := map[Class]string{ClassRoute: "route", ClassUpstream: "upstream", ClassPolicy: "policy"}[f.Class]
		if want != "" && (len(f.Labels) == 0 || f.Labels[0].Name != want) {
			t.Errorf("%s: class %d family does not start with %s", f.Name, f.Class, want)
		}
	}
}

func TestBounds(t *testing.T) {
	// Five sets of 13 ascending bounds.
	sets := map[Bounds][]float64{
		BoundsFast:    {0.00001, 0.000025, 0.00005, 0.0001, 0.00015, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.1},
		BoundsRequest: {0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 60},
		BoundsControl: {0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 3600},
		BoundsBytes:   {64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864, 268435456, 1073741824},
		BoundsRatio:   {0.5, 0.75, 0.9, 0.95, 0.98, 0.99, 1, 1.01, 1.02, 1.05, 1.1, 1.25, 1.5},
	}
	for b, want := range sets {
		got := b.Seconds()
		if !slices.Equal(got, want) {
			t.Errorf("Bounds(%d) = %v, want %v", b, got, want)
		}
		if len(got) != 13 || !slices.IsSorted(got) {
			t.Errorf("Bounds(%d) is not 13 ascending values", b)
		}
	}
	if BoundsNone.Seconds() != nil || Bounds(99).Seconds() != nil {
		t.Error("BoundsNone has bounds")
	}
}

func TestLookup(t *testing.T) {
	for _, f := range Families() {
		got, ok := Lookup(f.Name)
		if !ok || got.Name != f.Name || got.Kind != f.Kind || len(got.Labels) != len(f.Labels) {
			t.Errorf("Lookup(%s) = %+v, %v", f.Name, got, ok)
		}
	}
	if _, ok := Lookup("ruralz_missing_total"); ok {
		t.Error("Lookup of an unknown family succeeded")
	}
	// Families returns a fresh table.
	fs := Families()
	fs[0].Name = "changed"
	if Families()[0].Name == "changed" {
		t.Error("Families returned shared state")
	}
}

func TestReasons(t *testing.T) {
	// The M1 degraded reasons plus crl_stale and
	// cache_revalidation_saturated.
	want := []string{
		"lkg_boot", "lkg_write_failed", "state_store_memory_fallback", "state_store_unauthenticated",
		"state_store_breaker_open", "state_store_eviction_policy", "upstream_panic", "discovery_stale",
		"balancer_budget", "probes_skipped", "header_limit_capped", "snapshot_ending_overdue",
		"secret_rotation_failed", "jwks_stale", "cleartext_hop", "telemetry_export_failing", "crl_stale",
		"cache_revalidation_saturated",
	}
	if !slices.Equal(ReasonNames(), want) {
		t.Fatalf("ReasonNames = %v", ReasonNames())
	}
	if int(NumReasons)-1 != len(want) {
		t.Fatalf("NumReasons = %d for %d names", NumReasons, len(want))
	}
	seen := map[string]bool{}
	for r := ReasonLKGBoot; r < NumReasons; r++ {
		s := r.String()
		if s == "" || seen[s] || s != want[r-1] {
			t.Errorf("Reason(%d) = %q", r, s)
		}
		seen[s] = true
	}
	if Reason(0).String() != "" || NumReasons.String() != "" || Reason(200).String() != "" {
		t.Error("an out-of-range Reason has a name")
	}
	if ReasonCRLStale.String() != "crl_stale" || ReasonCleartextHop.String() != "cleartext_hop" ||
		ReasonCacheRevalidationSaturated.String() != "cache_revalidation_saturated" {
		t.Error("named reasons do not match their constants")
	}
}

// TestCacheEnumerations checks the label values of the Response Cache
// families: ruralz_cache_requests_total results with coalesced last, the
// Route-scoped ruralz_cache_revalidations_total results, and the Node-wide
// ruralz_cache_revalidation_drops_total reasons (the revalidation workers
// are shared by every cache Policy of the Node, so drops have no route).
func TestCacheEnumerations(t *testing.T) {
	tests := []struct {
		family, label string
		values        []string
		want          []string
		class         Class
	}{
		{CacheRequestsTotal, "result", CacheResults(), []string{"hit", "miss", "bypass", "stale", "stale_error", "coalesced"}, ClassRoute},
		{CacheRevalidationsTotal, "result", CacheRevalidationResults(), []string{"refreshed", "ended", "kept", "skipped"}, ClassRoute},
		{CacheRevalidationDropsTotal, "reason", CacheRevalidationDropReasons(), []string{"queue_full", "queue_wait", "partitions_full"}, ClassListener},
	}
	for _, tt := range tests {
		if !slices.Equal(tt.values, tt.want) {
			t.Errorf("%s %s values = %v, want %v", tt.family, tt.label, tt.values, tt.want)
		}
		f, ok := Lookup(tt.family)
		if !ok || f.Kind != Counter || f.Class != tt.class {
			t.Fatalf("family %+v", f)
		}
		last := f.Labels[len(f.Labels)-1]
		if last.Name != tt.label || !slices.Equal(last.Values, tt.want) {
			t.Errorf("%s label %+v, want %s=%v", tt.family, last, tt.label, tt.want)
		}
	}
}

func TestHops(t *testing.T) {
	// The hop kinds of ruralz_security_cleartext_hops.
	want := []string{"client", "upstream", "state_store", "telemetry", "admin"}
	if !slices.Equal(HopNames(), want) || int(NumHops) != len(want) {
		t.Fatalf("HopNames = %v, NumHops %d", HopNames(), NumHops)
	}
	for h := HopClient; h < NumHops; h++ {
		if h.String() != want[h] {
			t.Errorf("Hop(%d) = %q", h, h.String())
		}
	}
	if NumHops.String() != "" {
		t.Error("NumHops has a name")
	}
}

func TestOpsAndWriteKinds(t *testing.T) {
	// The M1 op enumeration: single ops first, then round-trip labels,
	// then post-commit write kinds.
	if got := strings.Join(Ops(), "|"); got != ops {
		t.Fatalf("Ops = %s", got)
	}
	for _, k := range WriteKinds() {
		if !slices.Contains(Ops(), k) {
			t.Errorf("write kind %s is not an op", k)
		}
	}
}

// TestStateZones checks the zone enumeration of
// ruralz_state_call_duration_seconds: same, cross and unknown, in that
// label order, after op, on the listener-class family that never folds, so
// the SLO-GW-5 SLI (op="gcra", zone="same") is always exported.
func TestStateZones(t *testing.T) {
	want := []string{"same", "cross", "unknown"}
	if !slices.Equal(Zones(), want) {
		t.Fatalf("Zones = %v, want %v", Zones(), want)
	}
	f, ok := Lookup(StateCallDurationSeconds)
	if !ok || f.Kind != Histogram || f.Class != ClassListener || len(f.Labels) != 2 {
		t.Fatalf("family %+v", f)
	}
	if op, zone := f.Labels[0], f.Labels[1]; op.Name != "op" || zone.Name != "zone" || !slices.Equal(zone.Values, want) {
		t.Fatalf("labels %+v, want op then zone=%v", f.Labels, want)
	}
	// Only the duration carries the zone: the call counters stay by op.
	for _, name := range []string{StateCallsTotal, StateOpsTotal} {
		g, _ := Lookup(name)
		if slices.ContainsFunc(g.Labels, func(l Label) bool { return l.Name == "zone" }) {
			t.Errorf("%s carries a zone label", name)
		}
	}
	// Zones returns a fresh slice.
	Zones()[0] = "changed"
	if Zones()[0] != "same" {
		t.Error("Zones returned shared state")
	}
}

// TestBasicRefusalCauses checks the cause enumeration of
// ruralz_auth_basic_refusals_total, the auth.basic throttle's refusals
// before any hash: queue, create, delay and source, in that label order,
// on a Node-wide counter (the throttle is shared by every auth.basic
// Policy, so the family has no policy label).
func TestBasicRefusalCauses(t *testing.T) {
	want := []string{"queue", "create", "delay", "source"}
	if !slices.Equal(BasicRefusalCauses(), want) {
		t.Fatalf("BasicRefusalCauses = %v, want %v", BasicRefusalCauses(), want)
	}
	f, ok := Lookup(AuthBasicRefusalsTotal)
	if !ok || f.Kind != Counter || f.Class != ClassListener || len(f.Labels) != 1 ||
		f.Labels[0].Name != "cause" || !slices.Equal(f.Labels[0].Values, want) {
		t.Fatalf("family %+v", f)
	}
}

func TestSpansAndKeys(t *testing.T) {
	// Span names are ruralz.route.match or carry the filter and upstream
	// prefixes.
	if SpanRouteMatch != "ruralz.route.match" {
		t.Errorf("SpanRouteMatch = %s", SpanRouteMatch)
	}
	if got := FilterSpanName("auth-jwt"); got != "ruralz.filter.auth-jwt" {
		t.Errorf("FilterSpanName = %s", got)
	}
	if got := UpstreamSpanName("inventory"); got != "ruralz.upstream.inventory" {
		t.Errorf("UpstreamSpanName = %s", got)
	}
	attrs := []string{
		AttrRoute, AttrListener, AttrRevision, AttrConsumer, AttrTier, AttrCode, AttrPolicyType, AttrPhase,
		AttrOutcome, AttrFailureMode, AttrStateOp, AttrStateDuration, AttrStateBatch, AttrAttempt, AttrCompStep,
	}
	attr := regexp.MustCompile(`^ruralz\.[a-z_]+(\.[a-z_]+)*$`)
	for _, a := range attrs {
		if !attr.MatchString(a) {
			t.Errorf("attribute key %q", a)
		}
	}
	// Process log keys are snake_case and never the slog built-in keys
	// (sloglint forbidden-keys).
	keys := []string{
		KeyComponent, KeyNodeID, KeyRevision, KeyTraceID, KeySpanID, KeyCode, KeyError, KeyFile, KeyLine,
		KeyColumn, KeyResourceKind, KeyResourceName, KeyPath, KeyProvider, KeyReference, KeyShard, KeyPolicy,
		KeyUpstream, KeyListener, KeyReason, KeyPhase, KeyPanicType, KeyStack, KeyErrorType, KeyMetric,
	}
	snake := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	seen := map[string]bool{}
	for _, k := range keys {
		if !snake.MatchString(k) || seen[k] || slices.Contains([]string{"time", "level", "msg", "source"}, k) {
			t.Errorf("log key %q", k)
		}
		seen[k] = true
	}
}
