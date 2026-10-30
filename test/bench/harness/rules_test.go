// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// TestCheckFacts covers the pre-run host check of one host: the
// performance governor at a fixed 3.0 GHz or more, and throttle counters
// that did not move since the previous check.
func TestCheckFacts(t *testing.T) {
	good := func(cpu int) lab.CPUFacts {
		return lab.CPUFacts{CPU: cpu, Governor: "performance", MinKHz: 3_200_000, MaxKHz: 3_200_000, CurKHz: 3_200_000}
	}
	cur := lab.HostFacts{CPUs: []lab.CPUFacts{good(0), good(1), good(2)}}
	if got := CheckFacts(nil, cur, nil); len(got) != 0 {
		t.Errorf("good host = %q", got)
	}
	bad := cur
	bad.CPUs = []lab.CPUFacts{good(0), good(1), good(2)}
	bad.CPUs[0].Governor = "powersave"
	bad.CPUs[1].MinKHz = 1_200_000
	bad.CPUs[2].MinKHz, bad.CPUs[2].MaxKHz = 2_800_000, 2_800_000
	if got := CheckFacts(nil, bad, nil); len(got) != 3 {
		t.Errorf("bad host = %q, want three reasons", got)
	}
	if got := CheckFacts(nil, bad, []int{1}); len(got) != 1 || !strings.Contains(got[0], "cpu1 frequency not fixed") {
		t.Errorf("cpuset {1} = %q, want only cpu1's reason", got)
	}
	moved := lab.HostFacts{CPUs: []lab.CPUFacts{good(0), good(1), good(2)}}
	moved.CPUs[1].PackageThrottle = 3
	if got := CheckFacts(&cur, moved, nil); len(got) != 1 || !strings.Contains(got[0], "throttle") {
		t.Errorf("moved throttle = %q", got)
	}
	if got := CheckFacts(nil, lab.HostFacts{}, nil); len(got) != 1 {
		t.Errorf("no cpufreq = %q", got)
	}
}

func TestSettingsFromEnv(t *testing.T) {
	env := map[string]string{EnvRecords: "/r", EnvOha: "/o", EnvRotationAll: "true", EnvAccept: "S2=abcdef0"}
	s, err := SettingsFromEnv(func(k string) string { return env[k] })
	if err != nil || !s.RotationAll || s.Accept == nil || s.Accept.Scenario != "S2" || s.Records != "/r" {
		t.Errorf("SettingsFromEnv = %+v, %v", s, err)
	}
	for name, e := range map[string]map[string]string{
		"no records":      {},
		"rotation":        {EnvRecords: "/r", EnvRotationAll: "yes"},
		"acceptance":      {EnvRecords: "/r", EnvAccept: "S2"},
		"bad commit":      {EnvRecords: "/r", EnvAccept: "S2=HEAD"},
		"lonely baseline": {EnvRecords: "/r", EnvBaselineBin: "/b"},
	} {
		if _, err := SettingsFromEnv(func(k string) string { return e[k] }); err == nil {
			t.Errorf("SettingsFromEnv(%s) = nil error", name)
		}
	}
}

func TestGoModToolchain(t *testing.T) {
	if got := goModToolchain([]byte("module m\n\ngo 1.26.0\n\ntoolchain go1.27.1\n")); got != "go1.27.1" {
		t.Errorf("toolchain line = %q", got)
	}
	if got := goModToolchain([]byte("module m\ngo 1.26.0\n")); got != "go1.26.0" {
		t.Errorf("go line only = %q", got)
	}
}

// TestCatalog checks the M1 open-loop scenarios against PBB: rates,
// connections, tools, saturations and budgets.
func TestCatalog(t *testing.T) {
	cat := Catalog()
	want := map[string]struct {
		tool  Tool
		rate  float64
		conns int
		sat   float64
	}{
		"S1": {ToolOha, 40000, 256, 50000}, "S2": {ToolLoadgen, 16000, 256, 32000},
		"S3": {ToolLoadgen, 14000, 64, 28000}, "S5": {ToolLoadgen, 20000, 256, 40000}, "S5x": {ToolLoadgen, 20000, 256, 40000},
	}
	if len(cat) != len(want) {
		t.Fatalf("catalog has %d scenarios, want %d", len(cat), len(want))
	}
	for _, sc := range cat {
		w, ok := want[sc.ID]
		if !ok {
			t.Errorf("unexpected scenario %s", sc.ID)
			continue
		}
		if sc.Workload.Tool != w.tool || sc.Workload.Rate != w.rate || sc.Workload.Connections != w.conns || sc.Saturation != w.sat {
			t.Errorf("%s = %+v", sc.ID, sc)
		}
		if !strings.HasPrefix(sc.Bundle, ScenarioRoot+"/") || len(sc.Budgets) == 0 {
			t.Errorf("%s bundle %q budgets %d", sc.ID, sc.Bundle, len(sc.Budgets))
		}
		seen := map[string]bool{}
		for _, b := range sc.Budgets {
			if b.ID == "" || b.Value == "" || seen[b.ID] {
				t.Errorf("%s budget %+v", sc.ID, b)
			}
			seen[b.ID] = true
		}
	}
	s2, _ := Find(cat, "S2")
	if s2.Workload.Tokens != 10000 || s2.Workload.Keys.N != 100000 || s2.Workload.KeyHeader != "x-bench-key" {
		t.Errorf("S2 workload = %+v", s2.Workload)
	}
	s3, _ := Find(cat, "S3")
	if s3.Workload.Port != 8443 || s3.Workload.Protocol != "h2" || s3.Workload.Streams != 64 {
		t.Errorf("S3 workload = %+v", s3.Workload)
	}
	s5x, _ := Find(cat, "S5x")
	if s5x.Proxy == nil || s5x.Proxy.Mean != 500*time.Microsecond || s5x.Proxy.StdDev != 100*time.Microsecond || !s5x.StateStore {
		t.Errorf("S5x = %+v", s5x)
	}
	s5, _ := Find(cat, "S5")
	z := s5.Zipf()
	if z.ID != "S5-zipf" || z.Workload.Rate != 20000 || z.Workload.Keys.Kind != lab.KeysZipf || z.Workload.Keys.Exponent != 2 || s5.Workload.Keys.Kind != lab.KeysCycle {
		t.Errorf("Zipf = %+v (S5 keys %+v)", z.Workload, s5.Workload.Keys)
	}
	s1, _ := Find(cat, "S1")
	if v := s1.VegetaCrossCheck(); v.Workload.Tool != ToolVegeta || v.Workload.Rate != 40000 || v.ID != "S1-vegeta" {
		t.Errorf("cross-check = %+v", v)
	}
	if r := s2.AtRate(19200, pb15()); r.Workload.Rate != 19200 || len(r.Budgets) != 1 || r.Budgets[0].ID != "PB-15" {
		t.Errorf("AtRate = %+v", r)
	}
	if _, ok := Find(cat, "S9"); ok {
		t.Error("Find(S9) found a scenario")
	}
}

// TestSoakDrift covers the soak rule: the median of the last 10 minutes
// against that of minutes 10 to 20, at most 2% RSS drift, goroutines flat
// within 1% or 50.
func TestSoakDrift(t *testing.T) {
	var points []SoakPoint
	for at := time.Duration(0); at <= SoakDuration; at += SoakSample {
		rss, gr := int64(100<<20), int64(1000)
		if at >= SoakDuration-10*time.Minute {
			rss, gr = 101<<20, 1040
		}
		points = append(points, SoakPoint{At: at, RSS: rss, Goroutines: gr})
	}
	so := SoakDrift(points, SoakDuration)
	if so.RSSEarlyBytes != 100<<20 || so.RSSLateBytes != 101<<20 || math.Abs(so.RSSDrift-0.01) > 1e-9 || !so.GoroutinesFlat {
		t.Errorf("SoakDrift = %+v, want 1%% drift and flat goroutines", so)
	}
	if bs := soakBudgets(so); bs[0].Verdict != benchrecord.BudgetPass || bs[1].Verdict != benchrecord.BudgetPass {
		t.Errorf("soak budgets = %+v", bs)
	}
	for i := range points {
		if points[i].At >= SoakDuration-10*time.Minute {
			points[i].RSS, points[i].Goroutines = 103<<20, 1051
		}
	}
	so = SoakDrift(points, SoakDuration)
	if bs := soakBudgets(so); bs[0].Verdict != benchrecord.BudgetFail || bs[1].Verdict != benchrecord.BudgetFail {
		t.Errorf("3%% drift and 51 more goroutines = %+v, want both failed", bs)
	}
	short := SoakDrift([]SoakPoint{{At: 15 * time.Second, RSS: 10, Goroutines: 10}, {At: 115 * time.Second, RSS: 10, Goroutines: 10}}, 2*time.Minute)
	if short.RSSEarlyBytes != 10 || short.RSSLateBytes != 10 {
		t.Errorf("a short soak scales its windows: %+v", short)
	}
	if bs := soakBudgets(benchrecord.Soak{}); bs[0].Verdict != benchrecord.BudgetFail {
		t.Errorf("a soak without samples = %+v", bs)
	}
}

func TestLadderStep(t *testing.T) {
	c := loadgen.BenchmarkSaturation()
	ok := ladderStep(loadgen.StepSummary{Offered: 1000, RateError: 0.001, P99: 5 * time.Millisecond}, 999, c)
	slow := ladderStep(loadgen.StepSummary{Offered: 1000, RateError: 0.001, P99: 11 * time.Millisecond}, 999, c)
	if !ok.Saturated || slow.Saturated || ok.AchievedRate != 999 {
		t.Errorf("steps = %+v %+v", ok, slow)
	}
}

func TestRecorder(t *testing.T) {
	r := &Recorder{Dir: t.TempDir()}
	if _, err := r.WriteFile("S1", "../x", nil); err == nil {
		t.Error("WriteFile(../x) = nil error")
	}
	if _, err := r.WriteFile("../S1", "x", nil); err == nil {
		t.Error("WriteFile into ../S1 = nil error")
	}
	rec := validRecord()
	rec.Latency.HistogramFiles = []string{"run-1-head.external.hlog"}
	if _, err := r.Write("run-1-head", rec); err == nil || !strings.Contains(err.Error(), "run-1-head.external.hlog") {
		t.Errorf("Write with a missing histogram file = %v", err)
	}
	if _, err := r.WriteFile("S1", "run-1-head.external.hlog", []byte("#log\n")); err != nil {
		t.Fatal(err)
	}
	path, err := r.Write("run-1-head", rec)
	if err != nil || path != "run-1-head.json" {
		t.Fatalf("Write = %q, %v", path, err)
	}
	back, err := r.ReadRecord("S1", path)
	if err != nil || back.Identity.Commit != rec.Identity.Commit {
		t.Errorf("ReadRecord = %+v, %v", back, err)
	}
	rec.Identity.Commit = "HEAD"
	if _, err := r.Write("bad", rec); err == nil {
		t.Error("Write(invalid record) = nil error")
	}
}

// validRecord is a valid macro run record of S1.
func validRecord() *benchrecord.Record {
	return &benchrecord.Record{
		Format: benchrecord.Format,
		Identity: benchrecord.Identity{
			Scenario: "S1", Kind: benchrecord.KindMacro, Commit: "abcdef0", Version: "0.1.0",
			RevisionDigest: "sha256:" + strings.Repeat("a", 64), Toolchain: "go1.27.1", StartedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		},
		Environment: benchrecord.Environment{HardwareProfile: "loopback", Kernel: "6.1", GOMAXPROCS: 4, GOGC: "100"},
		Load:        benchrecord.Load{Tool: "oha", OfferedRate: 100, Connections: 2},
		Validity:    benchrecord.Validity{Verdict: benchrecord.VerdictValid},
	}
}

func TestIntervalLog(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	b, err := intervalLog(hist(t, time.Millisecond), start, start.Add(time.Second), "c")
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), "#") && !strings.HasPrefix(sc.Text(), "\"") {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("interval log has %d interval lines, want 1:\n%s", lines, b)
	}
	empty, err := intervalLog(nil, start, start, "c")
	if err != nil || strings.Count(string(empty), "\n") == 0 {
		t.Errorf("empty log = %q, %v", empty, err)
	}
}

func TestParseValkeyInfo(t *testing.T) {
	text := "# CPU\r\nused_cpu_sys:1.5\r\nused_cpu_sys_main_thread:0.75\r\nused_cpu_user_main_thread:1.25\r\n\n# Commandstats\r\ncmdstat_evalsha:calls=10,usec=1234,usec_per_call=123.40\r\ncmdstat_ping:calls=1,usec=1\r\n"
	v := ParseValkeyInfo(text)
	if v.MainThreadCPU != 2 || v.EvalshaMicros != 1234 {
		t.Errorf("ParseValkeyInfo = %+v", v)
	}
}

func TestReadReply(t *testing.T) {
	tests := []struct {
		in, want string
		err      bool
	}{
		{"+OK\r\n", "OK", false},
		{"$5\r\nhello\r\n", "hello", false},
		{"$-1\r\n", "", false},
		{"-WRONGPASS invalid\r\n", "", true},
		{":1\r\n", "", true},
		{"$x\r\n", "", true},
		{"\r\n", "", true},
	}
	for _, tt := range tests {
		got, err := readReply(bufio.NewReader(strings.NewReader(tt.in)))
		if got != tt.want || (err != nil) != tt.err {
			t.Errorf("readReply(%q) = %q, %v", tt.in, got, err)
		}
	}
}

func TestCgroupRSS(t *testing.T) {
	dir := t.TempDir()
	stat := filepath.Join(dir, "memory.stat")
	if err := os.WriteFile(stat, []byte("anon 1000\nfile 5000\nfile_mapped 200\nshmem 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := CgroupRSS(stat); err != nil || v != 1200 {
		t.Errorf("CgroupRSS = %d, %v; want anon + file_mapped = 1200", v, err)
	}
	if err := os.WriteFile(stat, []byte("anon 1000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CgroupRSS(stat); err == nil {
		t.Error("CgroupRSS without file_mapped = nil error")
	}
	procDir := filepath.Join(dir, "proc", "42")
	if err := os.MkdirAll(procDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "cgroup"), []byte("0::/user.slice/run-r1.scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cgroupOf(filepath.Join(dir, "proc"), 42); got != "/user.slice/run-r1.scope" {
		t.Errorf("cgroupOf = %q", got)
	}
	if got := cgroupOf(filepath.Join(dir, "proc"), 43); got != "" {
		t.Errorf("cgroupOf(missing) = %q", got)
	}
}

// TestFoldWindows checks the sink windows: equal whole seconds covering
// the span, at most otlpsink's 64.
func TestFoldWindows(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, span := range []time.Duration{5 * time.Second, 390 * time.Second, 2*time.Hour + 210*time.Second} {
		ws := foldWindows(start, span)
		if len(ws) == 0 || len(ws) > 64 {
			t.Errorf("span %v: %d windows", span, len(ws))
			continue
		}
		w := ws[0].end.Sub(ws[0].start)
		if w%time.Second != 0 || !ws[0].start.Equal(start) || ws[len(ws)-1].end.Before(start.Add(span)) {
			t.Errorf("span %v: width %v, first %v, last end %v", span, w, ws[0].start, ws[len(ws)-1].end)
		}
	}
}

// TestWindowSamples covers the samples bracketing the measured window and
// the measures between them.
func TestWindowSamples(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	set := func(requests, gc, allocs float64) promtext.Set {
		s, err := promtext.ParseBytes([]byte("# TYPE ruralz_http_requests_total counter\nruralz_http_requests_total{route=\"r\",status_class=\"2xx\"} " +
			formatSig(requests, 9) + "\n# TYPE ruralz_runtime_gc_cpu_seconds_total counter\nruralz_runtime_gc_cpu_seconds_total " + formatSig(gc, 6) +
			"\n# TYPE ruralz_runtime_allocations_total counter\nruralz_runtime_allocations_total " + formatSig(allocs, 9) + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	mk := func(sec int, cpu time.Duration, requests, gc float64) nodeSample {
		return nodeSample{at: t0.Add(time.Duration(sec) * time.Second), usage: usageOf(cpu), rss: int64(sec), metrics: set(requests, gc, requests*14)}
	}
	samples := []nodeSample{mk(0, 0, 0, 0), mk(10, time.Second, 1000, 0.01), mk(20, 3*time.Second, 21000, 0.21), mk(30, 4*time.Second, 22000, 0.25)}
	a, b, err := window(samples, t0.Add(5*time.Second), t0.Add(25*time.Second))
	if err != nil || !a.at.Equal(t0.Add(10*time.Second)) || !b.at.Equal(t0.Add(20*time.Second)) {
		t.Fatalf("window = %v, %v, %v", a, b, err)
	}
	m, requests, _ := windowMeasures(a, b, nil)
	if requests != 20000 || m.CPUPerRequest != 100*time.Microsecond || math.Abs(m.GCShare-0.1) > 1e-9 {
		t.Errorf("measures = %+v over %d requests", m, requests)
	}
	a, b, err = window(samples, t0.Add(12*time.Second), t0.Add(14*time.Second))
	if err != nil || !a.at.Equal(t0.Add(10*time.Second)) || !b.at.Equal(t0.Add(20*time.Second)) {
		t.Errorf("a short window brackets from outside: %v, %v, %v", a, b, err)
	}
	if _, _, err := window(samples[:1], t0, t0.Add(time.Second)); err == nil {
		t.Error("window with one sample = nil error")
	}
	if got := maxRSS(samples, t0.Add(5*time.Second), t0.Add(25*time.Second), 0); got != 20 {
		t.Errorf("maxRSS = %d, want 20", got)
	}
	if got := dedupe([]string{"a", "", "b", "a"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("dedupe = %q", got)
	}
}
