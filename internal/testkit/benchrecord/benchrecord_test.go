// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package benchrecord

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// Tests for the result record (Identity, Environment, Load, Latency,
// Resources, Validity and budgets; format ruralz.bench.v1) and the schema
// check the harness dry run relies on to validate records.

func validMacro() *Record {
	return &Record{
		Format: Format,
		Identity: Identity{
			Scenario: "S2", Kind: KindMacro, Commit: strings.Repeat("ab", 20), Version: "0.1.0",
			Baseline: strings.Repeat("cd", 20), RevisionDigest: "sha256:" + strings.Repeat("0f", 32),
			Toolchain: "go1.27.1", StartedAt: time.Date(2026, 9, 26, 1, 2, 3, 0, time.UTC),
		},
		Environment: Environment{
			HardwareProfile: "RH-1", Microarchitecture: "znver4", Kernel: "6.18.44", Cpuset: "0-3",
			NIC: "mtu=1500 offloads=on", GOMAXPROCS: 4, GOGC: "100", GOMEMLIMIT: "",
			Sysctls: map[string]string{"net.ipv4.ip_local_port_range": "1024 65535"},
		},
		Load: Load{
			Tool: "oha", Version: "1.16.0", CommandLine: "oha -q 16000 --latency-correction",
			OfferedRate: 16000, AchievedRate: 15990, Connections: 256, KeyCardinality: 100000,
			KeyDistribution: "uniform", InjectedDelay: "none",
		},
		Latency: Latency{
			External:       Percentiles{P50: 120 * time.Microsecond, P90: 300 * time.Microsecond, P99: 840 * time.Microsecond, P999: 2 * time.Millisecond, Max: 9 * time.Millisecond},
			Internal:       Percentiles{P50: 100 * time.Microsecond, P99: 700 * time.Microsecond},
			HistogramFiles: []string{"hist/s2-external.hlog"},
		},
		Resources: Resources{
			CPUPerRequest: 31 * time.Microsecond, GCCPUShare: 0.07, RSSLoadBytes: 200 << 20,
			RSSIdleBytes: 60 << 20, LiveHeapBytes: 30 << 20, AllocsPerOp: 27, CPUProfileFiles: []string{"cpu.pprof"},
		},
		Validity: Validity{RateError: 0.000625, GeneratorCPU: 0.4, MockCPU: 0.3, NoiseCV: 0.015, Verdict: VerdictValid},
		Budgets: []BudgetResult{
			{ID: "PB-2", Value: "<= 1ms (target)", Result: "0.84ms", SLO: "SLO-GW-2", Verdict: BudgetPass},
			{ID: "PB-3", Value: "<= 150us (target)", Result: "120us", Verdict: BudgetPass},
			{ID: "PB-10", Value: "<= 5% (target)", Verdict: BudgetSkipped},
		},
	}
}

func validComponent() *Record {
	return &Record{
		Format: Format,
		Identity: Identity{
			Scenario: "BenchmarkMatch", Kind: KindComponent, Commit: "0123abc", Version: "0.1.0-dev",
			Toolchain: "go1.27.1", StartedAt: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC),
		},
		Environment: Environment{Kernel: "6.18", GOMAXPROCS: 4, GOGC: "off"},
		Resources:   Resources{AllocsPerOp: 3},
		Validity:    Validity{Verdict: VerdictValid},
	}
}

// validSummary is an S2 comparison summary: pooled head and baseline
// percentiles, the median-of-runs p99 values, four slower pairs of five
// and a quiet baseline, so the comparison fails; its budgets are pooled.
func validSummary() *Record {
	r := validMacro()
	r.Identity.Kind = KindSummary
	r.Validity = Validity{Verdict: VerdictValid}
	r.Latency.HistogramFiles = nil
	r.Resources.CPUProfileFiles = nil
	r.Summary = &Summary{
		Of:           OfComparison,
		Runs:         []string{"S2-head-1.json", "S2-base-1.json"},
		HeadRuns:     5,
		BaselineRuns: 5,
		Head: Views{
			External: Percentiles{P50: 130 * time.Microsecond, P99: 900 * time.Microsecond, Max: 3 * time.Millisecond},
			Internal: Percentiles{P50: 110 * time.Microsecond, P99: 760 * time.Microsecond},
		},
		Baseline: Views{
			External: Percentiles{P50: 120 * time.Microsecond, P99: 840 * time.Microsecond},
			Internal: Percentiles{P50: 100 * time.Microsecond, P99: 700 * time.Microsecond},
		},
		HeadMedianP99:     PerView{External: 905_000, Internal: 765_000},
		BaselineMedianP99: PerView{External: 845_000, Internal: 705_000},
		SlowerPairs:       PerView{External: 4, Internal: 5},
		BaselineNoise:     PerView{External: 0.011, Internal: 0.009},
		Verdict:           ABFail,
	}
	return r
}

// validLadder is an S1 rate ladder summary without a baseline.
func validLadder() *Record {
	r := validSummary()
	r.Identity.Scenario = "S1"
	r.Summary.Of = OfLadder
	r.Summary.BaselineRuns = 0
	r.Summary.Verdict = ABNone
	r.Summary.Ladder = Ladder{
		Steps: []LadderStep{
			{OfferedRate: 25000, AchievedRate: 25000, P99: time.Millisecond, Saturated: true},
			{OfferedRate: 52500, AchievedRate: 52490, RateError: 0.0002, P99: 4 * time.Millisecond, Saturated: true},
			{OfferedRate: 55000, AchievedRate: 50000, RateError: 0.09, ErrorRate: 0.01, P99: 40 * time.Millisecond},
		},
		Saturation: 52500,
	}
	r.Budgets = []BudgetResult{{ID: "PB-4", Value: ">= 50000 rps (target)", Result: "52500 rps", Verdict: BudgetPass}}
	return r
}

func TestWriteDecodeRoundTrip(t *testing.T) {
	for name, rec := range map[string]*Record{
		"macro": validMacro(), "component": validComponent(), "summary": validSummary(), "ladder": validLadder(),
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Write(&buf, rec); err != nil {
				t.Fatal(err)
			}
			if !bytes.HasSuffix(buf.Bytes(), []byte("}\n")) {
				t.Fatal("record does not end with a newline")
			}
			got, err := Decode(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("Decode: %v\n%s", err, buf.String())
			}
			// Write materializes nil collections; compare through JSON.
			a, _ := json.Marshal(normalized(rec))
			b, _ := json.Marshal(normalized(got))
			if !bytes.Equal(a, b) {
				t.Fatalf("round trip changed the record:\n%s\n%s", a, b)
			}
			// Writing twice is byte-identical (map keys are sorted).
			var again bytes.Buffer
			if err := Write(&again, got); err != nil || !bytes.Equal(again.Bytes(), buf.Bytes()) {
				t.Fatalf("second write differs: %v", err)
			}
		})
	}
}

func normalized(r *Record) *Record {
	out := materialized(r)
	return &out
}

func TestWriteMaterializesEmptyCollections(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, validComponent()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"budgets": []`, `"sysctls": {}`, `"histogramFiles": []`, `"cpuProfileFiles": []`, `"reasons": []`,
		`"p50Ns": 0`, `"format": "ruralz.bench.v1"`, `"stateStoreCpuShare": 0`, `"evalshaUsecPerSec": 0`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("record lacks %s:\n%s", want, buf.String())
		}
	}
	// Only a summary record carries a summary member.
	if strings.Contains(buf.String(), `"summary"`) {
		t.Errorf("component record has a summary member:\n%s", buf.String())
	}
	s := validSummary()
	s.Summary.Ladder.Steps = nil
	buf.Reset()
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"steps": []`) {
		t.Errorf("summary lacks an empty ladder step list:\n%s", buf.String())
	}
}

// TestCommittedSchemaIsCurrent checks that test/bench/record.schema.json,
// the copy go generate writes, equals the embedded schema (stage 3 diffs
// it too).
func TestCommittedSchemaIsCurrent(t *testing.T) {
	got, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "bench", "record.schema.json"))
	if err != nil {
		t.Fatalf("read the published schema (run go generate ./internal/testkit/benchrecord): %v", err)
	}
	if !bytes.Equal(got, Schema()) {
		t.Fatal("test/bench/record.schema.json differs from the embedded schema: run go generate ./internal/testkit/benchrecord")
	}
}

func TestValidateViolations(t *testing.T) { // Schema check
	tests := []struct {
		name   string
		mutate func(r *Record)
		path   string
	}{
		{"format", func(r *Record) { r.Format = "ruralz.bench.v2" }, "format"},
		{"scenario", func(r *Record) { r.Identity.Scenario = "" }, "identity.scenario"},
		{"kind", func(r *Record) { r.Identity.Kind = "micro" }, "identity.kind"},
		{"commit short", func(r *Record) { r.Identity.Commit = "abc" }, "identity.commit"},
		{"commit upper", func(r *Record) { r.Identity.Commit = "ABCDEF0" }, "identity.commit"},
		{"version", func(r *Record) { r.Identity.Version = "" }, "identity.version"},
		{"digest form", func(r *Record) { r.Identity.RevisionDigest = "rev-162af81f5de4" }, "identity.revisionDigest"},
		{"digest macro", func(r *Record) { r.Identity.RevisionDigest = "" }, "identity.revisionDigest"},
		{"toolchain", func(r *Record) { r.Identity.Toolchain = "" }, "identity.toolchain"},
		{"startedAt", func(r *Record) { r.Identity.StartedAt = time.Time{} }, "identity.startedAt"},
		{"gomaxprocs", func(r *Record) { r.Environment.GOMAXPROCS = 0 }, "environment.gomaxprocs"},
		{"gogc", func(r *Record) { r.Environment.GOGC = "" }, "environment.gogc"},
		{"kernel", func(r *Record) { r.Environment.Kernel = "" }, "environment.kernel"},
		{"tool", func(r *Record) { r.Load.Tool = "" }, "load.tool"},
		{"offered zero", func(r *Record) { r.Load.OfferedRate = 0 }, "load.offeredRate"},
		{"offered NaN", func(r *Record) { r.Load.OfferedRate = math.NaN() }, "load.offeredRate"},
		{"achieved negative", func(r *Record) { r.Load.AchievedRate = -1 }, "load.achievedRate"},
		{"connections", func(r *Record) { r.Load.Connections = 0 }, "load.connections"},
		{"connections negative", func(r *Record) { r.Load.Connections = -1 }, "load.connections"},
		{"key cardinality", func(r *Record) { r.Load.KeyCardinality = -1 }, "load.keyCardinality"},
		{"percentile order", func(r *Record) { r.Latency.External.P99 = time.Microsecond }, "latency.external.p99Ns"},
		{"percentile negative", func(r *Record) { r.Latency.Internal.P50 = -1 }, "latency.internal.p50Ns"},
		{"histogram abs", func(r *Record) { r.Latency.HistogramFiles = []string{"/tmp/x"} }, "latency.histogramFiles[0]"},
		{"histogram escape", func(r *Record) { r.Latency.HistogramFiles = []string{"../x"} }, "latency.histogramFiles[0]"},
		{"histogram inner dotdot", func(r *Record) { r.Latency.HistogramFiles = []string{"a/../b"} }, "latency.histogramFiles[0]"},
		{"histogram trailing dotdot", func(r *Record) { r.Latency.HistogramFiles = []string{"ok", "a/.."} }, "latency.histogramFiles[1]"},
		{"histogram backslash", func(r *Record) { r.Latency.HistogramFiles = []string{`hist\s2.hlog`} }, "latency.histogramFiles[0]"},
		{"profile backslash escape", func(r *Record) { r.Resources.CPUProfileFiles = []string{`..\cpu.pprof`} }, "resources.cpuProfileFiles[0]"},
		{"profile empty", func(r *Record) { r.Resources.CPUProfileFiles = []string{""} }, "resources.cpuProfileFiles[0]"},
		{"cpu per request", func(r *Record) { r.Resources.CPUPerRequest = -1 }, "resources.cpuPerRequestNs"},
		{"gc share", func(r *Record) { r.Resources.GCCPUShare = 1.5 }, "resources.gcCpuShare"},
		{"rss", func(r *Record) { r.Resources.RSSLoadBytes = -1 }, "resources.rssLoadBytes"},
		{"heap", func(r *Record) { r.Resources.LiveHeapBytes = -1 }, "resources.liveHeapBytes"},
		{"allocs", func(r *Record) { r.Resources.AllocsPerOp = math.Inf(1) }, "resources.allocsPerOp"},
		{"rate error", func(r *Record) { r.Validity.RateError = -0.1 }, "validity.rateError"},
		{"generator cpu", func(r *Record) { r.Validity.GeneratorCPU = 2 }, "validity.generatorCpu"},
		{"mock cpu", func(r *Record) { r.Validity.MockCPU = -1 }, "validity.mockCpu"},
		{"noise", func(r *Record) { r.Validity.NoiseCV = math.NaN() }, "validity.noiseCv"},
		{"drops", func(r *Record) { r.Validity.Drops = -1 }, "validity.drops"},
		{"verdict", func(r *Record) { r.Validity.Verdict = "ok" }, "validity.verdict"},
		{"budget id", func(r *Record) { r.Budgets[0].ID = "PB 2" }, "budgets[0].id"},
		{"budget id dup", func(r *Record) { r.Budgets[1].ID = "PB-2" }, "budgets[1].id"},
		{"budget value", func(r *Record) { r.Budgets[0].Value = "" }, "budgets[0].value"},
		{"budget result", func(r *Record) { r.Budgets[1].Result = "" }, "budgets[1].result"},
		{"budget verdict", func(r *Record) { r.Budgets[2].Verdict = "maybe" }, "budgets[2].verdict"},
		{"state store share", func(r *Record) { r.Resources.StateStoreCPUShare = 1.2 }, "resources.stateStoreCpuShare"},
		{"evalsha", func(r *Record) { r.Resources.EvalshaMicrosPerSecond = -3 }, "resources.evalshaUsecPerSec"},
		{"mock excess", func(r *Record) { r.Validity.MockExcessP99 = -1 }, "validity.mockExcessP99Ns"},
		{"invalid without reason", func(r *Record) { r.Validity.Verdict = VerdictInvalid }, "validity.reasons"},
		{"inconclusive without reason", func(r *Record) { r.Validity.Verdict = VerdictInconclusive }, "validity.reasons"},
		{"blank reason", func(r *Record) { r.Validity.Reasons = []string{" "} }, "validity.reasons[0]"},
		{"summary on a macro run", func(r *Record) { r.Summary = validSummary().Summary }, "summary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validMacro()
			tt.mutate(r)
			err := r.Validate()
			if err == nil || !strings.Contains(err.Error(), "benchrecord: "+tt.path+":") {
				t.Fatalf("Validate() = %v, want a violation at %s", err, tt.path)
			}
			if err := Write(&bytes.Buffer{}, r); err == nil {
				t.Fatal("Write accepted an invalid record")
			}
		})
	}
	if err := validMacro().Validate(); err != nil {
		t.Fatalf("valid macro record: %v", err)
	}
	if err := validComponent().Validate(); err != nil {
		t.Fatalf("valid component record: %v", err)
	}
	reasoned := validMacro()
	reasoned.Validity.Verdict, reasoned.Validity.Reasons = VerdictInvalid, []string{"achieved rate 1.2% short of the offered rate"}
	if err := reasoned.Validate(); err != nil {
		t.Fatalf("invalid run with a reason: %v", err)
	}
}

// TestInconclusiveWithoutBaselineRuns checks that a comparison whose
// baseline refused the head's Revision, so no baseline run exists, is a
// valid inconclusive summary (PBB "Reproducibility").
func TestInconclusiveWithoutBaselineRuns(t *testing.T) {
	r := validSummary()
	r.Summary.Verdict, r.Summary.BaselineRuns = ABInconclusive, 0
	r.Summary.Baseline = Views{}
	r.Summary.BaselineMedianP99, r.Summary.SlowerPairs, r.Summary.BaselineNoise = PerView{}, PerView{}, PerView{}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for an inconclusive comparison without baseline runs", err)
	}
}

func TestValidateSummaryViolations(t *testing.T) { // Summary rules
	tests := []struct {
		name   string
		mutate func(r *Record)
		path   string
	}{
		{"summary missing", func(r *Record) { r.Summary = nil }, "summary"},
		{"of", func(r *Record) { r.Summary.Of = "trend" }, "summary.of"},
		{"no runs", func(r *Record) { r.Summary.Runs = nil }, "summary.runs"},
		{"run path", func(r *Record) { r.Summary.Runs = []string{"../x.json"} }, "summary.runs[0]"},
		{"negative runs", func(r *Record) { r.Summary.HeadRuns = -1 }, "summary.headRuns"},
		{"head order", func(r *Record) { r.Summary.Head.External.P99 = time.Microsecond }, "summary.head.external.p99Ns"},
		{"baseline order", func(r *Record) { r.Summary.Baseline.Internal.P99 = time.Microsecond }, "summary.baseline.internal.p99Ns"},
		{"median", func(r *Record) { r.Summary.HeadMedianP99.External = -1 }, "summary.headMedianP99Ns.external"},
		{"slower pairs", func(r *Record) { r.Summary.SlowerPairs.Internal = math.NaN() }, "summary.slowerPairs.internal"},
		{"noise", func(r *Record) { r.Summary.BaselineNoise.External = math.Inf(1) }, "summary.baselineNoise.external"},
		{"verdict", func(r *Record) { r.Summary.Verdict = "ok" }, "summary.verdict"},
		{"A/B verdict without baseline", func(r *Record) { r.Summary.BaselineRuns = 0 }, "summary.verdict"},
		{"saturation", func(r *Record) { r.Summary.Ladder.Saturation = -1 }, "summary.ladder.saturation"},
		{"step order", func(r *Record) {
			r.Summary.Ladder.Steps = []LadderStep{{OfferedRate: 2}, {OfferedRate: 1}}
		}, "summary.ladder.steps[1].offeredRate"},
		{"step error rate", func(r *Record) { r.Summary.Ladder.Steps = []LadderStep{{ErrorRate: 2}} }, "summary.ladder.steps[0].errorRate"},
		{"soak negative", func(r *Record) { r.Summary.Soak.Samples = -1 }, "summary.soak"},
		{"soak drift", func(r *Record) { r.Summary.Soak.RSSDrift = math.NaN() }, "summary.soak.rssDrift"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validSummary()
			tt.mutate(r)
			err := r.Validate()
			if err == nil || !strings.Contains(err.Error(), "benchrecord: "+tt.path+":") {
				t.Fatalf("Validate() = %v, want a violation at %s", err, tt.path)
			}
		})
	}
	for name, r := range map[string]*Record{"comparison": validSummary(), "ladder": validLadder()} {
		if err := r.Validate(); err != nil {
			t.Errorf("valid %s summary: %v", name, err)
		}
	}
}

func TestDecodeStrict(t *testing.T) { // Schema check on read
	var buf bytes.Buffer
	if err := Write(&buf, validMacro()); err != nil {
		t.Fatal(err)
	}
	good := buf.String()
	tests := []struct {
		name, in, want string
	}{
		{"unknown member", strings.Replace(good, `"format"`, `"extra": 1, "format"`, 1), "unknown field"},
		{"missing member", strings.Replace(good, `"kernel": "6.18.44",`, "", 1), "environment.kernel: required member missing"},
		{"missing nested", strings.Replace(good, `"p999Ns": 2000000,`, "", 1), "latency.external.p999Ns: required member missing"},
		{"missing budget member", strings.Replace(good, `"slo": "SLO-GW-2",`, "", 1), "budgets[0].slo: required member missing"},
		{"trailing data", good + "{}", "trailing data"},
		{"not json", "{", "decode"},
		{"wrong type", strings.Replace(good, `"gomaxprocs": 4`, `"gomaxprocs": "4"`, 1), "decode"},
		{"invalid value", strings.Replace(good, `"verdict": "valid"`, `"verdict": "nope"`, 1), "validity.verdict"},
		// The schema's relativePath rejects inner ".." elements and
		// backslashes on every platform.
		{"inner dotdot path", strings.Replace(good, `"hist/s2-external.hlog"`, `"hist/../s2.hlog"`, 1), "latency.histogramFiles[0]"},
		{"backslash path", strings.Replace(good, `"cpu.pprof"`, `"prof\\cpu.pprof"`, 1), "resources.cpuProfileFiles[0]"},
		// No schema type admits null, though Go reads null as a zero value.
		{"null budgets", withNull(t, good, "budgets"), "budgets: null is not allowed"},
		{"null sysctls", withNull(t, good, "environment", "sysctls"), "environment.sysctls: null is not allowed"},
		{"null histogram files", withNull(t, good, "latency", "histogramFiles"), "latency.histogramFiles: null is not allowed"},
		{"null profile files", withNull(t, good, "resources", "cpuProfileFiles"), "resources.cpuProfileFiles: null is not allowed"},
		{"null nested object", withNull(t, good, "latency", "internal"), "latency.internal: null is not allowed"},
		{"null string", withNull(t, good, "identity", "baseline"), "identity.baseline: null is not allowed"},
		{"null number", withNull(t, good, "validity", "noiseCv"), "validity.noiseCv: null is not allowed"},
		{"null array item", strings.Replace(good, `"cpu.pprof"`, `null`, 1), "resources.cpuProfileFiles[0]: null is not allowed"},
		{"null sysctl value", strings.Replace(good, `"1024 65535"`, `null`, 1), "environment.sysctls.net.ipv4.ip_local_port_range: null is not allowed"},
		{"null budget member", strings.Replace(good, `"slo": "SLO-GW-2"`, `"slo": null`, 1), "budgets[0].slo: null is not allowed"},
		{"null record", "null", "$: null is not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.in == good {
				t.Fatal("mutation did not apply")
			}
			_, err := Decode(strings.NewReader(tt.in))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Decode err = %v, want %q", err, tt.want)
			}
		})
	}
	big := bytes.Repeat([]byte(" "), MaxRecordBytes+1)
	if _, err := Decode(bytes.NewReader(big)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized record err = %v", err)
	}
}

// withNull returns the record JSON with the member at path set to null.
func withNull(t *testing.T, record string, path ...string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(record), &doc); err != nil {
		t.Fatal(err)
	}
	m := doc
	for _, name := range path[:len(path)-1] {
		m = m[name].(map[string]any)
	}
	if _, ok := m[path[len(path)-1]]; !ok {
		t.Fatalf("no member %v", path)
	}
	m[path[len(path)-1]] = nil
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRelativePathRule(t *testing.T) { // The schema's relativePath, platform-independent
	tests := map[string]bool{
		"cpu.pprof": true, "hist/s2.hlog": true, "./a": true, "a/./b": true, "a..b/c": true, "..a": true, "a/": true,
		"": false, "/a": false, "..": false, "../a": false, "a/..": false, "a/../b": false, `a\b`: false, `..\a`: false, `C:\a`: false,
	}
	// The schema states the same rule as a minLength and a "not" pattern.
	var root struct {
		Defs map[string]struct {
			MinLength int `json:"minLength"`
			Not       struct {
				Pattern string `json:"pattern"`
			} `json:"not"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(Schema(), &root); err != nil {
		t.Fatal(err)
	}
	def := root.Defs["relativePath"]
	notRE, err := regexp.Compile(def.Not.Pattern)
	if err != nil || def.MinLength != 1 {
		t.Fatalf("relativePath schema: minLength %d, pattern %q: %v", def.MinLength, def.Not.Pattern, err)
	}
	for in, want := range tests {
		if got := isRelativePath(in); got != want {
			t.Errorf("isRelativePath(%q) = %v, want %v", in, got, want)
		}
		if schema := len(in) >= def.MinLength && !notRE.MatchString(in); schema != want {
			t.Errorf("schema relativePath(%q) = %v, want %v", in, schema, want)
		}
	}
}

func TestSchemaAdmitsNoNull(t *testing.T) { // checkRequired rejects every null; the schema must agree
	if bytes.Contains(Schema(), []byte(`"null"`)) {
		t.Fatal(`record.schema.json admits null somewhere; checkRequired rejects every null`)
	}
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "S2-a-1.json")
	if err := WriteFile(path, validMacro()); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity.Scenario != "S2" || got.Budgets[0].Result != "0.84ms" {
		t.Fatalf("ReadFile = %+v", got.Identity)
	}
	bad := validMacro()
	bad.Format = ""
	if err := WriteFile(filepath.Join(dir, "bad.json"), bad); err == nil {
		t.Fatal("WriteFile accepted an invalid record")
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.json")); !os.IsNotExist(err) {
		t.Fatalf("invalid record left a file: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
	if _, err := ReadFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("ReadFile on a missing file: want error")
	}
	if err := WriteFile(filepath.Join(dir, "no", "such", "dir.json"), validMacro()); err == nil {
		t.Fatal("WriteFile into a missing directory: want error")
	}
}

// schemaDoc is the part of the schema document the consistency test reads.
type schemaDoc struct {
	Schema               string                `json:"$schema"`
	Type                 string                `json:"type"`
	AdditionalProperties json.RawMessage       `json:"additionalProperties"`
	Required             []string              `json:"required"`
	Properties           map[string]*schemaDoc `json:"properties"`
	Items                *schemaDoc            `json:"items"`
	Ref                  string                `json:"$ref"`
	Defs                 map[string]*schemaDoc `json:"$defs"`
}

func TestSchemaMatchesRecordType(t *testing.T) { // record.schema.json and the Go type agree
	var root schemaDoc
	if err := json.Unmarshal(Schema(), &root); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if root.Schema != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("$schema = %q", root.Schema)
	}
	var walk func(path string, s *schemaDoc, typ reflect.Type)
	walk = func(path string, s *schemaDoc, typ reflect.Type) {
		if s.Ref != "" {
			name := strings.TrimPrefix(s.Ref, "#/$defs/")
			if root.Defs[name] == nil {
				t.Fatalf("%s: dangling $ref %s", path, s.Ref)
			}
			s = root.Defs[name]
		}
		switch {
		case typ == reflect.TypeFor[time.Time]() || typ == reflect.TypeFor[time.Duration]():
			return
		case typ.Kind() == reflect.Slice:
			if s.Items == nil {
				t.Fatalf("%s: array without items", path)
			}
			walk(path+"[]", s.Items, typ.Elem())
			return
		case typ.Kind() == reflect.Pointer:
			walk(path, s, typ.Elem())
			return
		case typ.Kind() != reflect.Struct:
			return
		}
		if string(s.AdditionalProperties) != "false" {
			t.Errorf("%s: additionalProperties must be false", path)
		}
		var fields, mandatory []string
		for f := range typ.Fields() {
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			fields = append(fields, name)
			// An omitempty member (the summary) is optional; the schema's
			// if/then rule ties it to the kind, checked below.
			if opts != "omitempty" {
				mandatory = append(mandatory, name)
			}
			child := s.Properties[name]
			if child == nil {
				t.Errorf("%s: member %q missing from the schema", path, name)
				continue
			}
			walk(path+"."+name, child, f.Type)
		}
		var props []string
		for name := range s.Properties {
			props = append(props, name)
		}
		slices.Sort(fields)
		slices.Sort(props)
		req := slices.Sorted(slices.Values(s.Required))
		if !slices.Equal(fields, props) {
			t.Errorf("%s: Go members %v, schema properties %v", path, fields, props)
		}
		slices.Sort(mandatory)
		if !slices.Equal(mandatory, req) {
			t.Errorf("%s: every member but an omitempty one is required: Go %v, schema required %v", path, mandatory, req)
		}
	}
	walk("$", &root, reflect.TypeFor[Record]())
	// The summary member is required exactly for kind summary.
	var rule struct {
		AllOf []struct {
			If struct {
				Properties struct {
					Identity struct {
						Properties struct {
							Kind struct {
								Const string `json:"const"`
							} `json:"kind"`
						} `json:"properties"`
					} `json:"identity"`
				} `json:"properties"`
			} `json:"if"`
			Then struct {
				Required []string `json:"required"`
			} `json:"then"`
			Else struct {
				Not struct {
					Required []string `json:"required"`
				} `json:"not"`
			} `json:"else"`
		} `json:"allOf"`
	}
	if err := json.Unmarshal(Schema(), &rule); err != nil || len(rule.AllOf) != 1 {
		t.Fatalf("schema allOf: %v, %d rules", err, len(rule.AllOf))
	}
	r0 := rule.AllOf[0]
	if r0.If.Properties.Identity.Properties.Kind.Const != KindSummary ||
		!slices.Equal(r0.Then.Required, []string{"summary"}) || !slices.Equal(r0.Else.Not.Required, []string{"summary"}) {
		t.Errorf("schema does not tie the summary member to kind summary: %+v", r0)
	}
	// Schema returns a copy.
	s := Schema()
	s[0] = 'x'
	if Schema()[0] != '{' {
		t.Fatal("Schema() exposes the embedded bytes")
	}
}

func FuzzDecode(f *testing.F) {
	var buf bytes.Buffer
	if err := Write(&buf, validMacro()); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	var sum bytes.Buffer
	if err := Write(&sum, validLadder()); err != nil {
		f.Fatal(err)
	}
	f.Add(sum.Bytes())
	f.Add([]byte(`{"format":"ruralz.bench.v1"}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		rec, err := Decode(bytes.NewReader(data))
		if err != nil {
			return
		}
		// Whatever decodes is valid and writes back to a decodable record.
		var out bytes.Buffer
		if err := Write(&out, rec); err != nil {
			t.Fatalf("decoded record does not write: %v", err)
		}
		if _, err := Decode(&out); err != nil {
			t.Fatalf("written record does not decode: %v", err)
		}
	})
}
