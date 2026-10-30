// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// Tool is a load tool (PBB "Open-loop load and coordinated omission").
type Tool string

// Load tools: loadgen drives every scenario whose requests rotate keys or
// bearer tokens, oha S1 and its ladder, vegeta S1's cross-check.
const (
	ToolLoadgen Tool = "loadgen"
	ToolOha     Tool = "oha"
	ToolVegeta  Tool = "vegeta"
)

// Workload is what a scenario offers.
type Workload struct {
	Tool Tool
	// Method and Path are the request line: GET /bench/1k.
	Method, Path string
	// Port is the Node listener the load targets: 8080 (http) or 8443
	// (https).
	Port int
	// Protocol is "http1", "h2c" or "h2" (HTTP/2 over TLS).
	Protocol string
	// Connections and Streams shape the connections: S1's 256 keep-alive
	// HTTP/1.1 connections, S3's 64 connections of up to 64 streams.
	Connections, Streams int
	// Rate is the fixed offered rate.
	Rate float64
	// KeyHeader and Keys add a per-request key: S2's x-bench-key over
	// 100,000 values.
	KeyHeader string
	Keys      *lab.KeySpec
	// Tokens is the count of bearer tokens rotated, signed in turn by two
	// RS256 keys the mock IdP publishes; 0 sends none.
	Tokens int
	// Count names the RZ codes that count as achieved (RZ-RT-005 in O1).
	Count []string
	// BodySize and MockDelay set the mock Upstream; MockHeader adds
	// "Name: value" response headers to every mock response.
	BodySize   int64
	MockDelay  time.Duration
	MockHeader []string
	// MaxInFlight bounds the generator's requests in flight; 0 is
	// loadgen's 4,096, and O1 raises it to its 120 connections of 250
	// streams.
	MaxInFlight int
	// Header adds fixed request headers, such as the constants runs'
	// x-api-key.
	Header map[string][]string
}

// Delay is a normal delay distribution each way through the fault proxy.
type Delay struct {
	Mean, StdDev time.Duration
	// PassFirst starts the proxy forwarding unchanged until the driver
	// switches it to the delay (Lab.ProxyMode), so the Node's connection
	// setup is not delayed and only later replies are: O2.
	PassFirst bool
}

// String returns the record's injected delay distribution.
func (d *Delay) String() string {
	if d == nil {
		return "none"
	}
	return fmt.Sprintf("normal mean=%s stddev=%s each way", d.Mean, d.StdDev)
}

// Scenario is one benchmark scenario: its Bundle, its workload, its
// expected saturation and its budgets.
type Scenario struct {
	// ID is the scenario's identifier, such as S2.
	ID string
	// Bundle is the scenario's Bundle directory under
	// test/bench/scenarios, relative to the repository root.
	Bundle string
	// Environments is the file holding the profiles' Environment
	// resources, relative to the repository root; each profile renders
	// with --env named after it.
	Environments string
	Workload     Workload
	// Saturation is the expected saturation the ladder climbs to.
	Saturation float64
	// StateStore starts Valkey on RH-1S for the run; Proxy adds the fault
	// proxy in front of it.
	StateStore bool
	Proxy      *Delay
	Budgets    []Budget
}

// ScenarioRoot is the directory of the scenario Bundles.
const ScenarioRoot = "test/bench/scenarios"

// IdP and token settings of the scenario Bundles (S2 "jwt-bench").
const (
	BenchIssuer   = "https://idp.bench.example"
	BenchAudience = "bench"
	// BenchTokens is S2's rotated token count, from two keys (target).
	BenchTokens = 10000
	// BenchKeyCardinality is S2's x-bench-key cardinality (target).
	BenchKeyCardinality = 100000
)

// budgets of the catalog (PBB "Budget catalog and SLO ties", "Resources
// per core").
func pb1() Budget {
	return Budget{ID: "PB-1", Value: "<= 1ms gateway-added p99 at 40,000 rps (target)", SLO: "SLO-GW-2", Kind: InternalP99, Limit: float64(time.Millisecond)}
}

func pb2() Budget {
	return Budget{ID: "PB-2", Value: "<= 1ms gateway-added p99 at half saturation (target)", SLO: "SLO-GW-2", Kind: InternalP99, Limit: float64(time.Millisecond)}
}

func pb3() Budget {
	return Budget{ID: "PB-3", Value: "<= 150µs gateway-added p50 at half saturation (target)", SLO: "SLO-GW-3", Kind: InternalP50, Limit: float64(150 * time.Microsecond)}
}

func pb4() Budget {
	return Budget{ID: "PB-4", Value: ">= 50,000 rps S1 saturation on 4 vCPU (target)", Kind: MinSaturation, Limit: 50000}
}

func gcraBudget(id, zone, value, slo string, limit time.Duration) Budget {
	return Budget{
		ID: id, Value: value, SLO: slo, Kind: MetricP99, Limit: float64(limit),
		Metric: "ruralz_state_call_duration_seconds", Labels: map[string]string{"op": "gcra", "zone": zone},
	}
}

func cpuBudget(id string, vcpu float64) Budget {
	return Budget{ID: id, Value: fmt.Sprintf("<= %s vCPU per 10,000 rps (target)", formatSig(vcpu, 3)), Kind: MaxCPUPer10k, Limit: vcpu}
}

func gcShareBudget() Budget {
	return Budget{ID: "gc-cpu-share", Value: "<= 10% of Node CPU at GOGC=100 (target)", Kind: MaxGCShare, Limit: 0.10}
}

func loadedRSSBudget() Budget {
	return Budget{ID: "loaded-rss", Value: "<= 256 MiB with 256 keep-alive connections (target)", Kind: MaxLoadedRSS, Limit: 256 << 20}
}

// s1Workload is S1's: GET /bench/1k, a 1 KiB response, 256 keep-alive
// HTTP/1.1 connections on 8080, 40,000 rps (target).
func s1Workload() Workload {
	return Workload{Tool: ToolOha, Method: "GET", Path: "/bench/1k", Port: 8080, Protocol: "http1", Connections: 256, Rate: 40000, BodySize: 1024}
}

// s2Workload is S2's: S1's shape with 10,000 rotated tokens and 100,000
// uniformly cycled keys, 16,000 rps (target).
func s2Workload() Workload {
	w := s1Workload()
	w.Tool, w.Rate, w.Tokens, w.KeyHeader = ToolLoadgen, 16000, BenchTokens, "x-bench-key"
	w.Keys = &lab.KeySpec{Kind: lab.KeysCycle, Prefix: "key-", N: BenchKeyCardinality}
	return w
}

// Catalog returns the M1 open-loop scenarios with the workloads, expected
// saturations and budgets of PBB "Scenarios" and "Throughput targets".
// Their Bundles live under ScenarioRoot, one directory per scenario named
// after its lower-case ID, with the profiles' Environments in
// environments.yaml there.
func Catalog() []Scenario {
	env := filepath.ToSlash(filepath.Join(ScenarioRoot, "environments.yaml"))
	dir := func(id string) string { return filepath.ToSlash(filepath.Join(ScenarioRoot, strings.ToLower(id))) }
	s3 := s2Workload()
	s3.Port, s3.Protocol, s3.Connections, s3.Streams, s3.Rate = 8443, "h2", 64, 64, 14000
	s5 := s1Workload()
	s5.Tool, s5.Rate, s5.KeyHeader = ToolLoadgen, 20000, "x-bench-key"
	s5.Keys = &lab.KeySpec{Kind: lab.KeysCycle, Prefix: "key-", N: BenchKeyCardinality}
	return []Scenario{
		{
			ID: "S1", Bundle: dir("S1"), Environments: env, Workload: s1Workload(), Saturation: 50000,
			Budgets: []Budget{pb1(), cpuBudget("cpu-per-10k-rps", 0.8), gcShareBudget(), loadedRSSBudget()},
		},
		{
			ID: "S2", Bundle: dir("S2"), Environments: env, Workload: s2Workload(), Saturation: 32000,
			Budgets: []Budget{pb2(), pb3(), cpuBudget("cpu-per-10k-rps", 1.25), gcShareBudget()},
		},
		{
			ID: "S3", Bundle: dir("S3"), Environments: env, Workload: s3, Saturation: 28000,
			Budgets: []Budget{{ID: "S3-1ms", Value: "<= 1ms gateway-added p99 at half saturation (target)", Kind: InternalP99, Limit: float64(time.Millisecond)}, cpuBudget("cpu-per-10k-rps", 1.42)},
		},
		{
			ID: "S5", Bundle: dir("S5"), Environments: env, Workload: s5, Saturation: 40000, StateStore: true,
			Budgets: []Budget{gcraBudget("PB-9", "same", "<= 1ms same-zone GCRA round trip p99 (hypothesis)", "SLO-GW-5", time.Millisecond)},
		},
		{
			ID: "S5x", Bundle: dir("S5x"), Environments: env, Workload: s5, Saturation: 40000, StateStore: true,
			Proxy:   &Delay{Mean: 500 * time.Microsecond, StdDev: 100 * time.Microsecond},
			Budgets: []Budget{gcraBudget("PB-14", "cross", "<= 2ms cross-zone GCRA round trip p99 (hypothesis)", "", 2*time.Millisecond)},
		},
	}
}

// Find returns the catalog scenario id.
func Find(cat []Scenario, id string) (Scenario, bool) {
	for _, s := range cat {
		if s.ID == id {
			return s, true
		}
	}
	return Scenario{}, false
}

// AtRate returns s offered at rate, with budgets replaced when given: the
// PB-15 and PB-16 runs are S2 at 19,200 and 25,600 rps (target).
func (s Scenario) AtRate(rate float64, budgets ...Budget) Scenario {
	s.Workload.Rate = rate
	if len(budgets) > 0 {
		s.Budgets = budgets
	}
	return s
}

// Zipf returns S5's Zipf variant: half saturation, 20,000 rps, keys over
// S2's 100,000 values with exponent 2, judged on S5's absolute criteria.
func (s Scenario) Zipf() Scenario {
	s.ID += "-zipf"
	s.Workload.Rate = 20000
	s.Workload.Keys = &lab.KeySpec{Kind: lab.KeysZipf, Prefix: "key-", N: BenchKeyCardinality, Exponent: 2, Seed: 1}
	return s
}

// VegetaCrossCheck returns S1's cross-check: one vegeta run at 40,000 rps.
func (s Scenario) VegetaCrossCheck() Scenario {
	s.ID += "-vegeta"
	s.Workload.Tool = ToolVegeta
	s.Workload.Rate = 40000
	return s
}

// PB15 and PB16 are S2 at 60% and 80% of saturation (target).
func pb15() Budget {
	return Budget{ID: "PB-15", Value: "<= 1ms gateway-added p99 at 60% of saturation (target)", SLO: "SLO-GW-2", Kind: InternalP99, Limit: float64(time.Millisecond)}
}

func pb16() Budget {
	return Budget{ID: "PB-16", Value: "<= 2ms gateway-added p99 at 80% of saturation (target)", Kind: InternalP99, Limit: float64(2 * time.Millisecond)}
}
