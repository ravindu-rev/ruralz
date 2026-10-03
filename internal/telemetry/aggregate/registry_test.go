// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Spec 09 req 45: every enumeration label set an alert rule references
// exists at 0 from process start, before any Revision.
func TestNodeSeriesPrecreatedAtZero_Req45(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	pts := collectNow(t, r, clk)

	for _, reason := range catalog.ReasonNames() {
		p := mustGet(t, pts, fmt.Sprintf(`%s{reason=%q}`, catalog.NodeDegradedInfo, reason))
		if p.kind != "gauge" || p.value != 0 {
			t.Errorf("degraded %s = %+v, want gauge 0", reason, p)
		}
	}
	for _, hop := range catalog.HopNames() {
		mustGet(t, pts, fmt.Sprintf(`%s{hop=%q}`, catalog.SecurityCleartextHops, hop))
	}
	for _, kind := range catalog.WriteKinds() {
		mustGet(t, pts, fmt.Sprintf(`%s{kind=%q}`, catalog.StateWritesDroppedTotal, kind))
	}
	for _, reason := range []string{"rate_cap_root", "rate_cap_parent"} {
		mustGet(t, pts, fmt.Sprintf(`%s{reason=%q}`, catalog.TelemetryTracesUnsampledTotal, reason))
	}
	foldable := 0
	for _, f := range catalog.Families() {
		if f.Class == catalog.ClassListener {
			continue
		}
		foldable++
		p := mustGet(t, pts, fmt.Sprintf(`%s{instrument=%q}`, catalog.TelemetryFoldedLabelSets, f.Name))
		if p.value != 0 {
			t.Errorf("folded %s = %v, want 0", f.Name, p.value)
		}
	}
	if got := len(familySeries(pts, catalog.TelemetryFoldedLabelSets)); got != foldable {
		t.Errorf("folded label set series = %d, want %d foldable families", got, foldable)
	}
	if got := len(familySeries(pts, catalog.StateCallsTotal)); got != emit.NumStateOps*emit.NumStateResults {
		t.Errorf("state calls series = %d, want %d", got, emit.NumStateOps*emit.NumStateResults)
	}
	if got := len(familySeries(pts, catalog.ConfigActivationDurationSeconds)); got != 15 {
		t.Errorf("activation duration label sets = %d, want 15", got)
	}
	mustGet(t, pts, catalog.ConfigActivationsTotal+`{code="",result="activated"}`)
	// Label sets with a code dimension are created on first record.
	for _, name := range []string{catalog.HTTPNodeResponsesTotal, catalog.ConfigRevisionInfo} {
		if got := familySeries(pts, name); len(got) != 0 {
			t.Errorf("%s pre-created %v, want none", name, got)
		}
	}
	for _, name := range []string{catalog.HTTPGatewayDurationSkippedTotal, catalog.TapEventsDroppedTotal, catalog.NodeBufferedBytes} {
		if len(familySeries(pts, name)) != 1 {
			t.Errorf("%s not pre-created", name)
		}
	}
	// No Revision yet: no listener or resource series.
	for _, name := range []string{catalog.HTTPRequestsTotal, catalog.HTTPListenerRequestsTotal, catalog.FilterDurationSeconds} {
		if got := familySeries(pts, name); len(got) != 0 {
			t.Errorf("%s has %d series before any Revision", name, len(got))
		}
	}
}

// Spec 09 req 46: runtime metrics come from runtime/metrics at collection.
func TestRuntimeMetrics_Req46(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	pts := collectNow(t, r, clk)
	if p := mustGet(t, pts, catalog.RuntimeGoroutines+"{}"); p.kind != "gauge" || p.value < 1 {
		t.Errorf("goroutines = %+v", p)
	}
	if p := mustGet(t, pts, catalog.RuntimeHeapBytes+"{}"); p.kind != "gauge" {
		t.Errorf("heap = %+v", p)
	}
	runtime.GC()
	if p := mustGet(t, collectNow(t, r, clk), catalog.RuntimeGCCyclesTotal+"{}"); p.kind != "sum" || p.value < 1 {
		t.Errorf("gc cycles = %+v, want a counter of at least 1", p)
	}
}

// R-56: the State Store handles are fixed arrays indexed by emit.StateOp*,
// emit.StateResult* and emit.Write*, each index labeled with the catalog
// value at that position.
func TestStateMetricsIndexes_R56(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	st := r.Node().State
	results := []string{"ok", "error", "timeout", "skipped"}
	for op := range emit.NumStateOps {
		for res := range emit.NumStateResults {
			st.Calls[op][res].Add(0, uint64(1+op*10+res))
		}
		st.Ops[op].Add(1, uint64(100+op))
		st.CallDuration[op].Record(2, uint64(op+1)*1000)
	}
	for k := range emit.NumWriteKinds {
		st.WritesDropped[k].Add(3, uint64(1000+k))
	}
	st.QueueItems.Set(7)
	pts := collectNow(t, r, clk)
	for op, opName := range catalog.Ops() {
		for res, resName := range results {
			key := fmt.Sprintf(`%s{op=%q,result=%q}`, catalog.StateCallsTotal, opName, resName)
			if got := mustGet(t, pts, key).value; got != float64(1+op*10+res) {
				t.Errorf("%s = %v, want %d", key, got, 1+op*10+res)
			}
		}
		key := fmt.Sprintf(`%s{kind=%q}`, catalog.StateOpsTotal, opName)
		if got := mustGet(t, pts, key).value; got != float64(100+op) {
			t.Errorf("%s = %v", key, got)
		}
		key = fmt.Sprintf(`%s{op=%q}`, catalog.StateCallDurationSeconds, opName)
		if p := mustGet(t, pts, key); p.count != 1 || !approx(p.sum, float64(op+1)*1e-6) {
			t.Errorf("%s = count %d sum %v", key, p.count, p.sum)
		}
	}
	for k, kind := range catalog.WriteKinds() {
		key := fmt.Sprintf(`%s{kind=%q}`, catalog.StateWritesDroppedTotal, kind)
		if got := mustGet(t, pts, key).value; got != float64(1000+k) {
			t.Errorf("%s = %v", key, got)
		}
	}
	if got := mustGet(t, pts, catalog.StateWriteQueueItems+"{}").value; got != 7 {
		t.Errorf("queue items = %v", got)
	}
}

// Spec 09 req 45 and test 12: a registered code creates its series on
// first record; an unregistered one creates none and logs once at DEBUG.
func TestNodeResponsesCodes_Req45(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r, clk := newTestRegistry(t, func(o *Options) { o.Logger = logger })
	nr := r.Node().NodeResponses
	nr.Inc(0, "RZ-RT-005")
	nr.Inc(1, "RZ-RT-005")
	nr.Inc(2, "RZ-RT-001")
	nr.Inc(0, "RZ-XX-999")
	nr.Inc(0, "RZ-XX-998")
	pts := familySeries(collectNow(t, r, clk), catalog.HTTPNodeResponsesTotal)
	want := map[string]float64{
		catalog.HTTPNodeResponsesTotal + `{code="RZ-RT-001"}`: 1,
		catalog.HTTPNodeResponsesTotal + `{code="RZ-RT-005"}`: 2,
	}
	if len(pts) != len(want) {
		t.Fatalf("series = %v, want %v", pts, want)
	}
	for k, v := range want {
		if pts[k].value != v {
			t.Errorf("%s = %v, want %v", k, pts[k].value, v)
		}
	}
	if n := strings.Count(logs.String(), "metric code label not recorded"); n != 1 {
		t.Errorf("drop logged %d times, want once: %s", n, logs.String())
	}
	// Keys are catalog constants (spec 09 req 63): metric, code and reason.
	for _, want := range []string{
		"level=DEBUG", catalog.KeyMetric + "=" + catalog.HTTPNodeResponsesTotal, catalog.KeyCode + "=RZ-XX-999",
		catalog.KeyReason + "=" + dropUnregistered,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q lacks %q", logs.String(), want)
		}
	}
	if strings.Contains(logs.String(), labelInstrument+"=") {
		t.Errorf("log %q uses a key outside the catalog", logs.String())
	}
}

// A code table that is full drops further codes with reason table_full.
func TestCodeTableFullLogsReason(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tab := newCodeTable(catalog.AuthDecisionsTotal, []string{"RZ-RT-001"}, 1, func(string) bool { return true }, logger)
	tab.inc("RZ-RT-002")
	tab.inc("RZ-RT-003")
	if n := strings.Count(logs.String(), "metric code label not recorded"); n != 1 {
		t.Fatalf("drop logged %d times, want once: %s", n, logs.String())
	}
	if !strings.Contains(logs.String(), catalog.KeyReason+"="+dropTableFull) ||
		!strings.Contains(logs.String(), catalog.KeyMetric+"="+catalog.AuthDecisionsTotal) {
		t.Errorf("log = %s", logs.String())
	}
}

// Without a registered code list, codes of the RZ shape are accepted.
func TestSyntacticCodes(t *testing.T) {
	r, clk := newTestRegistry(t, func(o *Options) { o.Codes = nil })
	r.Node().NodeResponses.Inc(0, "RZ-RT-005")
	r.Node().NodeResponses.Inc(0, "not-a-code")
	pts := familySeries(collectNow(t, r, clk), catalog.HTTPNodeResponsesTotal)
	if len(pts) != 1 {
		t.Fatalf("series = %v", pts)
	}
	tests := []struct {
		in   string
		want bool
	}{
		{"RZ-RT-005", true},
		{"RZ-AUTH-001", true},
		{"RZ-STS-004", true},
		{"RZ-A-001", false},
		{"RZ-ABCDE-001", false},
		{"RZ-rt-001", false},
		{"RZ-RT-01", false},
		{"RZ-RT-0a1", false},
		{"XZ-RT-001", false},
		{"RZ-RT001", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := syntacticCode(tt.in); got != tt.want {
			t.Errorf("syntacticCode(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// A code table never exceeds its capacity and never loses a code it
// holds; installation races resolve to one entry per code.
func TestCodeTableCapacityAndRaces(t *testing.T) {
	valid := func(string) bool { return true }
	tab := newCodeTable("test", []string{"A"}, 3, valid, nil)
	for _, c := range []string{"A", "B", "C", "D", "E"} {
		tab.inc(c)
	}
	var got []string
	for _, e := range tab.entries(nil) {
		got = append(got, fmt.Sprintf("%s=%d", e.code, e.n.Load()))
	}
	if len(got) != 3 {
		t.Fatalf("entries = %v, want 3 (capacity)", got)
	}
	race := newCodeTable("test", nil, 64, valid, nil)
	done := make(chan struct{})
	for w := range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range 1000 {
				race.inc(fmt.Sprintf("C%02d", (i+w)%32))
			}
		}()
	}
	for range 8 {
		<-done
	}
	var total uint64
	seen := map[string]bool{}
	for _, e := range race.entries(nil) {
		if seen[e.code] {
			t.Fatalf("duplicate entry %s", e.code)
		}
		seen[e.code] = true
		total += e.n.Load()
	}
	if len(seen) != 32 || total != 8000 {
		t.Errorf("entries %d total %d, want 32 and 8000", len(seen), total)
	}
}

func TestConfigMetrics(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	c := r.Node().Config
	c.Activated.Add(0, 2)
	c.Rejected.Inc(0, "RZ-CFG-005")
	c.ActivationDuration("compile", "le1000").Record(0, 1_500_000_000)
	c.ActivationDuration("bogus", "le1000").Record(0, 1)
	c.SecretRotationFail("file").Add(0, 1)
	c.SecretRotationFail("bogus").Add(0, 1)
	c.RetiredSnapshots.Set(2)
	c.RetirementEnded.Add(0, 3)
	c.RevisionInfo("active", "rev-000000000001")
	c.RevisionInfo("lkg", "rev-000000000001")
	c.RevisionInfo("active", "rev-000000000002")
	c.RevisionInfo("bogus", "rev-x")
	pts := collectNow(t, r, clk)
	checks := map[string]float64{
		catalog.ConfigActivationsTotal + `{code="",result="activated"}`:            2,
		catalog.ConfigActivationsTotal + `{code="RZ-CFG-005",result="rejected"}`:   1,
		catalog.ConfigSecretRotationFailuresTotal + `{provider="file"}`:            1,
		catalog.ConfigRetiredSnapshots + `{}`:                                      2,
		catalog.SnapshotRetirementEndedTotal + `{}`:                                3,
		catalog.ConfigRevisionInfo + `{revision="rev-000000000002",role="active"}`: 1,
		catalog.ConfigRevisionInfo + `{revision="rev-000000000001",role="lkg"}`:    1,
	}
	for k, v := range checks {
		if p := mustGet(t, pts, k); p.value != v {
			t.Errorf("%s = %v, want %v", k, p.value, v)
		}
	}
	if got := len(familySeries(pts, catalog.ConfigRevisionInfo)); got != 2 {
		t.Errorf("revision info series = %d, want at most 2", got)
	}
	h := mustGet(t, pts, catalog.ConfigActivationDurationSeconds+`{size_class="le1000",stage="compile"}`)
	if h.count != 1 || !approx(h.sum, 1.5) {
		t.Errorf("activation duration = %+v", h)
	}
	c.RevisionInfo("lkg", "")
	if got := len(familySeries(collectNow(t, r, clk), catalog.ConfigRevisionInfo)); got != 1 {
		t.Errorf("revision info after clearing lkg = %d series", got)
	}
}

func TestNodeAccessors(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	un, err := r.Counter(catalog.TelemetryTracesUnsampledTotal, "rate_cap_root")
	if err != nil {
		t.Fatal(err)
	}
	un.Add(0, 4)
	deg, err := r.Gauge(catalog.NodeDegradedInfo, catalog.ReasonLKGBoot.String())
	if err != nil {
		t.Fatal(err)
	}
	deg.Set(1)
	spans, err := r.Counter(catalog.TelemetrySpansTotal)
	if err != nil {
		t.Fatal(err)
	}
	spans.Add(1, 9)
	if err := r.GaugeFunc(catalog.NodeBufferedBytes, nil, func() int64 { return 4096 }); err != nil {
		t.Fatal(err)
	}
	pts := collectNow(t, r, clk)
	for k, v := range map[string]float64{
		catalog.TelemetryTracesUnsampledTotal + `{reason="rate_cap_root"}`: 4,
		catalog.NodeDegradedInfo + `{reason="lkg_boot"}`:                   1,
		catalog.TelemetrySpansTotal + `{}`:                                 9,
		catalog.NodeBufferedBytes + `{}`:                                   4096,
	} {
		if got := mustGet(t, pts, k).value; got != v {
			t.Errorf("%s = %v, want %v", k, got, v)
		}
	}
	if err := r.GaugeFunc(catalog.NodeBufferedBytes, nil, nil); err != nil {
		t.Fatal(err)
	}
	r.Node().BufferedBytes.Set(12)
	if got := mustGet(t, collectNow(t, r, clk), catalog.NodeBufferedBytes+`{}`).value; got != 12 {
		t.Errorf("buffered bytes after removing the function = %v, want 12", got)
	}

	errTests := []struct {
		name string
		call func() error
		want error
	}{
		{"unknown family", func() error { _, err := r.Counter("ruralz_bogus_total"); return err }, ErrUnknownFamily},
		{"listener family", func() error { _, err := r.Counter(catalog.ListenerConnectionsTotal, "x"); return err }, ErrNotNodeFamily},
		{"resource family", func() error { _, err := r.Counter(catalog.HTTPRequestsTotal); return err }, ErrNotNodeFamily},
		{"code family", func() error { _, err := r.Counter(catalog.HTTPNodeResponsesTotal); return err }, ErrNotNodeFamily},
		{"computed family", func() error { _, err := r.Gauge(catalog.TelemetrySeries, "live"); return err }, ErrNotNodeFamily},
		{"bad value", func() error { _, err := r.Gauge(catalog.NodeDegradedInfo, "bogus"); return err }, ErrLabelValues},
		{"value count", func() error { _, err := r.Gauge(catalog.NodeDegradedInfo); return err }, ErrLabelValues},
		{"gauge as counter", func() error { _, err := r.Counter(catalog.NodeDegradedInfo, "lkg_boot"); return err }, ErrNotNodeFamily},
		{"counter as gauge", func() error { _, err := r.Gauge(catalog.TelemetrySpansTotal); return err }, ErrNotNodeFamily},
		{"histogram as counter", func() error { _, err := r.Counter(catalog.StateCallDurationSeconds, "gcra"); return err }, ErrNotNodeFamily},
		{"func on counter", func() error { return r.GaugeFunc(catalog.TelemetrySpansTotal, nil, func() int64 { return 1 }) }, ErrNotNodeFamily},
		{"func bad value", func() error { return r.GaugeFunc(catalog.NodeDegradedInfo, []string{"x"}, nil) }, ErrLabelValues},
	}
	for _, tt := range errTests {
		if err := tt.call(); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}

// Spec 09 req 49: stripes are min(GOMAXPROCS, 8) and assigned round robin.
func TestStripes_Req49(t *testing.T) {
	r, _ := newTestRegistry(t, func(o *Options) { o.Stripes = 0 })
	if want := min(runtime.GOMAXPROCS(0), maxStripes); r.Stripes() != want {
		t.Errorf("default stripes = %d, want %d", r.Stripes(), want)
	}
	r, _ = newTestRegistry(t, func(o *Options) { o.Stripes = 20 })
	if r.Stripes() != maxStripes {
		t.Errorf("stripes = %d, want the cap 8", r.Stripes())
	}
	r, _ = newTestRegistry(t, func(o *Options) { o.Stripes = 3 })
	for i := range 9 {
		if got := r.NewStripe(); int(got) != i%3 {
			t.Fatalf("stripe %d = %d, want %d", i, got, i%3)
		}
	}
	for s := range 256 {
		if int(r.tab[s]) != s%3 {
			t.Fatalf("tab[%d] = %d", s, r.tab[s])
		}
	}
}

// The Policy type strings match pkg/config/v1alpha1, and the Phase
// indexes match internal/phase.
func TestTypeAndPhaseConstants(t *testing.T) {
	for got, want := range map[string]v1alpha1.PolicyType{
		typeAuthJWT:        v1alpha1.PolicyTypeAuthJWT,
		typeUpstreamOAuth2: v1alpha1.PolicyTypeAuthUpstreamOAuth2,
		typeRateLimit:      v1alpha1.PolicyTypeRateLimit,
		typeQuota:          v1alpha1.PolicyTypeQuota,
		typeCache:          v1alpha1.PolicyTypeCache,
	} {
		if got != string(want) {
			t.Errorf("type %q, want %q", got, want)
		}
	}
	if !strings.HasPrefix(string(v1alpha1.PolicyTypeAuthUpstreamSigV4), typeUpstreamPrefix) ||
		!strings.HasPrefix(string(v1alpha1.PolicyTypeAuthzCEL), typeAuthzPrefix) ||
		!strings.HasPrefix(string(v1alpha1.PolicyTypeAuthBasic), typeAuthPrefix) {
		t.Error("type prefixes do not match v1alpha1")
	}
	if phaseOnUpstreamRequest != int(phase.OnUpstreamRequest) || phaseOnLog != int(phase.OnLog) {
		t.Error("Phase indexes do not match internal/phase")
	}
	for p := phase.OnRequestHeaders; p < phase.Count; p++ {
		if p.CanShortCircuit() != (int(p) <= phaseOnUpstreamRequest) {
			t.Errorf("CanShortCircuit(%v) disagrees", p)
		}
	}
	f, _ := catalog.Lookup(catalog.FilterDurationSeconds)
	for i, l := range f.Labels[1].Values {
		if phase.Phase(i).String() != l {
			t.Errorf("phase label %d = %q, want %q", i, l, phase.Phase(i))
		}
	}
}

// Every catalog family is classified; every Policy family has a type rule.
func TestFamilyClassification(t *testing.T) {
	r, _ := newTestRegistry(t, nil)
	if len(r.fams) != len(catalog.Families()) {
		t.Fatalf("families = %d", len(r.fams))
	}
	for _, f := range r.fams {
		if f.role == rolePolicy {
			applies := false
			for _, typ := range []string{"auth.jwt", "auth.api-key", "authz.cel", "auth.upstream-oauth2", "ratelimit", "quota", "cache", "headers"} {
				applies = applies || policyApplies(f, typ)
			}
			if !applies {
				t.Errorf("Policy family %s applies to no type", f.cat.Name)
			}
		}
	}
	if _, err := newFamily(0, catalog.Family{Name: "x", Kind: catalog.Counter, Class: catalog.ClassListener, Labels: []catalog.Label{{Name: "free"}}}); !errors.Is(err, errCatalog) {
		t.Errorf("free label family: err = %v", err)
	}
	if _, err := newFamily(0, catalog.Family{Name: "x", Kind: catalog.Counter, Class: catalog.ClassRoute, Labels: []catalog.Label{{Name: "policy"}}}); !errors.Is(err, errCatalog) {
		t.Errorf("Route family without route label: err = %v", err)
	}
	if _, err := newFamily(0, catalog.Family{Name: "x", Kind: catalog.Counter, Class: 0}); !errors.Is(err, errCatalog) {
		t.Errorf("unknown class: err = %v", err)
	}
	if _, err := newFamily(0, catalog.Family{Name: "x", Kind: catalog.Histogram, Class: catalog.ClassListener}); !errors.Is(err, errBounds) {
		t.Errorf("histogram without bounds: err = %v", err)
	}
	if _, err := newFamily(0, catalog.Family{
		Name: catalog.HTTPRequestsTotal, Kind: catalog.Counter, Class: catalog.ClassRoute,
		Labels: []catalog.Label{{Name: "route"}},
	}); !errors.Is(err, errCatalog) {
		t.Errorf("status layout without status_class: err = %v", err)
	}
}
