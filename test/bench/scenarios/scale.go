// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package scenarios

import "time"

// Scale sets a driver's sizes: Full is the Latency job's on RH-1, with the
// targets of docs/architecture/12-performance-budgets-and-benchmarking.md;
// Functional is the nightly Functional job's reduced scale on the loopback
// profile, which keeps every fixed ceiling (20,000 in-flight units, 8,192
// State Store calls, 2,000 pre-routing writes, 512 MiB of buffered bytes,
// the balancer budget, the deadlines) and lowers only the rates and counts
// that are not ceilings, reaching the plateaus with longer injected
// delays; Smoke is the pr-full smoke run's: every scenario at 100 rps for
// 5 s, judged on no numeric criterion.
type Scale struct {
	// Name is "full", "functional" or "smoke".
	Name string
	// Functional judges only the criteria that hold on any hardware; the
	// others are recorded as skipped.
	Functional bool
	// Warmup and Duration are a single run's warm-up and measured phase;
	// zero takes the benchmark lengths, 60 s extended by the settle rule
	// and 180 s.
	Warmup, Duration time.Duration
	// Rate is the catalog scenarios' offered rate at a reduced scale; 0
	// keeps each scenario's own.
	Rate float64

	O1        O1Scale
	O2        O2Scale
	C1        C1Scale
	C2        C2Scale
	Mem       MemScale
	R1        R1Scale
	G1        G1Scale
	PB10      PB10Scale
	Constants ConstantsScale
}

// O1Scale sizes O1: S3's chain on Connections of Streams, O1a offering
// ARate with every admitted request held ADelay at the mock, O1b BRate
// without a delay (0 skips O1b).
type O1Scale struct {
	Dir                  string
	Connections, Streams int
	ARate                float64
	ADelay               time.Duration
	BRate                float64
}

// O2Scale sizes O2: S5 on Connections at Rate, every State Store round
// trip delayed Delay through the fault proxy, half each way, the requests
// cycling Keys x-bench-key values; 0 keeps S5's 100,000.
type O2Scale struct {
	Dir         string
	Connections int
	Rate        float64
	Delay       time.Duration
	Keys        int
}

// C1Scale sizes C1: Connections idle connections per run on S3's Bundle
// in Dir, opened at OpenRate per second and held for Hold once all are
// set up.
type C1Scale struct {
	Dir         string
	Connections int
	OpenRate    float64
	Hold        time.Duration
}

// C2Scale sizes C2: S3's load at Rate, then Burst new TLS connections
// within Within, on S3's Bundle in Dir.
type C2Scale struct {
	Dir    string
	Rate   float64
	Burst  int
	Within time.Duration
}

// MemScale sizes the GOMEMLIMIT run: Connections idle TLS connections (C1
// over TLS) plus S2's load at Rate in a container of Container bytes with
// GOMEMLIMIT at 90% of it (targets), on S3's Bundle in Dir.
type MemScale struct {
	Dir         string
	Connections int
	Rate        float64
	Container   int64
}

// R1Scale sizes R1: the ladder's Route counts; per size, the idle Hot
// Reloads and the idle settle before PB-6's RSS (120 s, PBB "Memory
// budget"); and the loaded run's rate, warm-up and length with a Hot
// Reload every LoadedEvery.
type R1Scale struct {
	Sizes          []int
	Reloads        int
	IdleSettle     time.Duration
	LoadedRate     float64
	LoadedWarmup   time.Duration
	LoadedDuration time.Duration
	LoadedEvery    time.Duration
}

// G1Scale sizes G1: open-loop steps of Step calls per second from From,
// StepDuration each, at most Steps; the variant emulates Nodes Nodes of 10
// connections each.
type G1Scale struct {
	From, Step   float64
	StepDuration time.Duration
	Steps        int
	Nodes        int
}

// PB10Scale sizes PB-10's telemetry-off S2: its rate.
type PB10Scale struct {
	Rate float64
}

// ConstantsScale sizes the constants runs.
type ConstantsScale struct {
	// BackgroundRate is S1's background traffic during a flood.
	BackgroundRate float64
	// PreRoutingClients never read their RZ-RT-001 responses; above the
	// 2,000 pre-routing writes, so every scale keeps 2,500.
	PreRoutingClients int
	// FloodConnections hold a slow header block each.
	FloodConnections int
	// Bodies stream BodyBytes each into the gated Route at once, together
	// above the 512 MiB budget.
	Bodies    int
	BodyBytes int64
	// ReloadRate activations per second for ReloadDuration under S2 at
	// ReloadLoad.
	ReloadRate     float64
	ReloadDuration time.Duration
	ReloadLoad     float64
	// QuotaKeys denied Quota keys offered at QuotaRate.
	QuotaKeys int
	QuotaRate float64
	// CacheKeys entries read twice in a row at CacheRate, of CacheBody
	// bytes each.
	CacheKeys int
	CacheRate float64
	CacheBody int64
	// RingSizes are the ring-hash ladder's rungs: Upstreams of
	// RingEndpoints Endpoints each.
	RingSizes     []int
	RingEndpoints int
	// TraceRate is S2's rate at both trace caps, half of it with a sampled
	// traceparent; each half stays above its cap (1,000 and 500 sampled
	// traces per second).
	TraceRate float64
}

// Full returns the Latency job's scale.
func Full() Scale {
	return Scale{
		Name: "full",
		O1:   O1Scale{Dir: DirS3, Connections: 120, Streams: 250, ARate: 30000, ADelay: time.Second, BRate: 42000},
		O2:   O2Scale{Dir: DirO2, Connections: 12000, Rate: 24000, Delay: 500 * time.Millisecond},
		C1:   C1Scale{Dir: DirS3, Connections: 20000, OpenRate: 2000, Hold: 30 * time.Second},
		C2:   C2Scale{Dir: DirS3, Rate: 14000, Burst: 10000, Within: 2 * time.Second},
		Mem:  MemScale{Dir: DirS3, Connections: 20000, Rate: 16000, Container: MemContainerBytes},
		R1: R1Scale{
			Sizes: []int{100, 1000, 5000, 10000, 17000}, Reloads: 5, IdleSettle: 120 * time.Second,
			LoadedRate: 25000, LoadedWarmup: 10 * time.Second, LoadedDuration: 30 * time.Second, LoadedEvery: 5 * time.Second,
		},
		G1:   G1Scale{From: 50000, Step: 5000, StepDuration: 60 * time.Second, Steps: 13, Nodes: 1000},
		PB10: PB10Scale{Rate: 16000},
		Constants: ConstantsScale{
			BackgroundRate: 1000, PreRoutingClients: 2500, FloodConnections: 20000, Bodies: 64, BodyBytes: 10<<20 - 1<<10,
			ReloadRate: 10, ReloadDuration: 60 * time.Second, ReloadLoad: 16000,
			QuotaKeys: 100000, QuotaRate: 10000, CacheKeys: 4096, CacheRate: 2000, CacheBody: 64 << 10,
			RingSizes: []int{1000, 4000}, RingEndpoints: 64, TraceRate: 16000,
		},
	}
}

// Functional returns the nightly Functional job's reduced scale.
func Functional() Scale {
	return Scale{
		Name: "functional", Functional: true, Warmup: 10 * time.Second, Duration: 20 * time.Second, Rate: FunctionalRate,
		O1:  O1Scale{Dir: DirO1Functional, Connections: 120, Streams: 250, ARate: 3000, ADelay: 10 * time.Second},
		O2:  O2Scale{Dir: DirO2Functional, Connections: 12000, Rate: 3600, Delay: 3 * time.Second, Keys: FirstSeenBurst},
		C1:  C1Scale{Dir: DirS3, Connections: 2000, OpenRate: 1000, Hold: 10 * time.Second},
		C2:  C2Scale{Dir: DirS3, Rate: 300, Burst: 1000, Within: 2 * time.Second},
		Mem: MemScale{Dir: DirS3, Connections: 1000, Rate: 300, Container: MemContainerBytes},
		R1: R1Scale{
			Sizes: []int{100, 1000}, Reloads: 3, IdleSettle: 2 * time.Second,
			LoadedRate: 300, LoadedWarmup: 2 * time.Second, LoadedDuration: 10 * time.Second, LoadedEvery: 2 * time.Second,
		},
		G1:   G1Scale{From: 2000, Step: 1000, StepDuration: 5 * time.Second, Steps: 3, Nodes: 20},
		PB10: PB10Scale{Rate: 300},
		Constants: ConstantsScale{
			BackgroundRate: 100, PreRoutingClients: 2500, FloodConnections: 500, Bodies: 64, BodyBytes: 10<<20 - 1<<10,
			ReloadRate: 10, ReloadDuration: 10 * time.Second, ReloadLoad: 300,
			QuotaKeys: 2000, QuotaRate: 500, CacheKeys: 200, CacheRate: 200, CacheBody: 64 << 10,
			RingSizes: []int{1000, 4000}, RingEndpoints: 64, TraceRate: 3000,
		},
	}
}

// Smoke returns the pr-full smoke run's scale (PBB "Suite layers"): every
// M1 scenario on the loopback profile at 100 rps for 5 s, the drivers'
// counts cut to what starts, serves and writes a valid record in seconds,
// judged on no numeric criterion; the ceilings and real-time deadlines
// the functional variants reach stay out of it.
func Smoke() Scale {
	return Scale{
		Name: "smoke", Functional: true, Warmup: -1, Duration: 5 * time.Second, Rate: SmokeRate,
		O1:   O1Scale{Dir: DirS3, Connections: 4, Streams: 4, ARate: SmokeRate},
		O2:   O2Scale{Dir: DirO2, Connections: 16, Rate: SmokeRate, Delay: 2 * time.Millisecond},
		C1:   C1Scale{Dir: DirS3, Connections: 20, OpenRate: SmokeRate},
		C2:   C2Scale{Dir: DirS3, Rate: SmokeRate, Burst: 20, Within: 2 * time.Second},
		Mem:  MemScale{Dir: DirS3, Connections: 20, Rate: SmokeRate, Container: MemContainerBytes},
		R1:   R1Scale{Sizes: []int{100}, Reloads: 1, LoadedRate: SmokeRate, LoadedDuration: 5 * time.Second, LoadedEvery: 2 * time.Second},
		G1:   G1Scale{From: SmokeRate, Step: SmokeRate, StepDuration: 5 * time.Second, Steps: 1, Nodes: 2},
		PB10: PB10Scale{Rate: SmokeRate},
	}
}

// FirstSeenBurst is the ratelimit Policy's first-seen budget in file
// mode: 200 keys per second, a bucket of 200 that starts full
// (docs/architecture/09-traffic-management-and-resilience.md "Decision
// path"). O2's functional variant cycles this many keys and primes them
// before its delay starts, so every key has full buckets and every request
// calls GCRA; a key past the budget decides on a local-only entry for 10 s
// without a call, which would keep most of S5's 100,000 keys off the State
// Store for longer than the run (the full scale's warm-up is
// OQ-performance-budgets-and-benchmarking-15).
const FirstSeenBurst = 200

// FunctionalRate is the functional variants' rate for the catalog
// scenarios, far below saturation on any host.
const FunctionalRate = 300

// SmokeRate is the smoke run's offered rate: 100 rps (target).
const SmokeRate = 100
