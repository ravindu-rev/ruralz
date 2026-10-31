// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/problem"
	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// Environment of a component benchmark worker: the harness re-runs its own
// test binary in ruralzd's cpuset with these set (TestComponentWorker).
const (
	EnvComponent    = "RURALZ_BENCH_COMPONENT"
	EnvComponentOut = "RURALZ_BENCH_COMPONENT_OUT"
)

// Component is one component benchmark (PBB "Suite layers"): each call is
// timed into an HdrHistogram inside ruralzd's cpuset.
type Component struct {
	Name string
	// Calls are timed per run, after Warm untimed calls, spread evenly
	// over Parallel goroutines (1 when 0) that call at once, under
	// GOMAXPROCS Procs (the worker's when 0): the metric aggregates at
	// 4 P and 32 P.
	Calls, Warm int
	Parallel    int
	Procs       int
	// TestCalls is the timed calls of the package's own quick run; 0 is
	// 1,000. The loader ladder's larger rungs, whose one call takes
	// seconds, time one.
	TestCalls int
	// Setup returns the call, which takes its goroutine's index below
	// workers, and a function releasing what it holds (nil for none).
	Setup func(ctx context.Context, workers int) (call func(worker int) error, release func(), err error)
	// Budgets are the targets the documents tie to the benchmark; other
	// per-stage values are reported.
	Budgets []Budget
}

// celBudget is the CEL target: typical match and key expressions under
// 2 µs at p99 (target).
func celBudget() Budget {
	return Budget{ID: "cel-p99", Value: "< 2µs p99 (target)", Kind: InternalP99, Limit: float64(2*time.Microsecond - 1)}
}

// Components returns the component benchmarks.
func Components() []Component {
	return append([]Component{
		{
			Name: "cel-match", Calls: 1_000_000, Warm: 100_000, Budgets: []Budget{celBudget()},
			Setup: celCall(expr.PlaceRouteMatchWhen, `request.headers["x-tenant"] == "acme"`, true),
		},
		{
			Name: "cel-key-header", Calls: 1_000_000, Warm: 100_000, Budgets: []Budget{celBudget()},
			Setup: celCall(expr.PlaceRateLimitKey, `request.headers["x-bench-key"]`, false),
		},
		{
			Name: "cel-key-consumer", Calls: 1_000_000, Warm: 100_000, Budgets: []Budget{celBudget()},
			Setup: celCall(expr.PlaceRateLimitKey, `consumer == null ? source.ip : consumer.name`, false),
		},
		{Name: "rz-rt-005-rejection", Calls: 200_000, Warm: 20_000, Setup: rejectionCall, Budgets: []Budget{{
			ID: "rz-rt-005-mean-cpu", Value: "<= 16µs mean CPU (target)", Kind: MaxMeanCPU, Limit: float64(16 * time.Microsecond),
		}}},
	}, m1Components()...)
}

// FindComponent returns the component name.
func FindComponent(name string) (Component, bool) {
	for _, c := range Components() {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}

// benchVars are the variables of a typical S2 request.
func benchVars() *expr.Vars {
	req := &expr.Request{
		Method: "GET", Scheme: "http", Host: "gateway.bench.example", Path: "/bench/1k",
		Header: http.Header{"X-Tenant": {"acme"}, "X-Bench-Key": {"key-4711"}, "Authorization": {"Bearer x"}},
	}
	return &expr.Vars{
		Request: req, Source: &expr.Source{IP: netip.MustParseAddr("192.0.2.1"), Port: 44321},
		Now: time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC),
	}
}

// celCall compiles src at place under the example input caps and returns
// one evaluation.
func celCall(place expr.PlaceID, src string, isBool bool) func(context.Context, int) (func(int) error, func(), error) {
	return func(ctx context.Context, _ int) (func(int) error, func(), error) {
		c, err := cel.New()
		if err != nil {
			return nil, nil, err
		}
		caps := expr.InputCaps{RequestHeaderBytes: 64 << 10, ClaimsBytes: 64 << 10}
		prg, iss := c.NewBuilder(nil, caps).Compile(expr.Site{Place: place}, src)
		if prg == nil {
			return nil, nil, fmt.Errorf("harness: compile %q: %v", src, iss)
		}
		v := benchVars()
		if isBool {
			return func(int) error { _, err := prg.EvalBool(ctx, v); return err }, nil, nil
		}
		return func(int) error { _, err := prg.EvalString(ctx, v); return err }, nil, nil
	}
}

// rejectionCall is the RZ-RT-005 rejection's work: decode an S1 request's
// header block, fail the admission check of a full ceiling and write the
// 503 problem response.
func rejectionCall(context.Context, int) (func(int) error, func(), error) {
	raw := []byte("GET /bench/1k HTTP/1.1\r\nHost: gateway.bench.example\r\nUser-Agent: oha/1.16.0\r\nAccept: */*\r\n\r\n")
	units := admission.NewUnits(0)
	w := &discard{h: http.Header{}}
	br := bufio.NewReader(bytes.NewReader(raw))
	return func(int) error {
		br.Reset(bytes.NewReader(raw))
		req, err := http.ReadRequest(br)
		if err != nil {
			return err
		}
		if units.TryAcquire(1) {
			return errors.New("harness: a zero ceiling admitted")
		}
		clear(w.h)
		problem.Write(w, problem.New(admission.CodeFull, req.Header.Get("traceparent")), nil)
		return nil
	}, nil, nil
}

type discard struct{ h http.Header }

func (d *discard) Header() http.Header       { return d.h }
func (*discard) Write(b []byte) (int, error) { return len(b), nil }
func (*discard) WriteHeader(int)             {}

// ComponentResult is one worker run's measurement.
type ComponentResult struct {
	Name  string `json:"name"`
	Calls int    `json:"calls"`
	// Histogram is the per-call time, HdrHistogram V2, base64.
	Histogram string `json:"histogram"`
	// CPUSeconds is the process CPU of the timed calls.
	CPUSeconds float64 `json:"cpuSeconds"`
	// WallSeconds is the timed loop's length.
	WallSeconds float64 `json:"wallSeconds"`
}

// RunComponent runs one component in this process: Warm untimed calls,
// then Calls timed ones; calls, when positive, overrides both counts with
// calls timed calls after calls/10 untimed ones.
func RunComponent(ctx context.Context, name string, calls int) (*ComponentResult, error) {
	c, ok := FindComponent(name)
	if !ok {
		return nil, fmt.Errorf("harness: unknown component %q", name)
	}
	if calls > 0 {
		c.Calls, c.Warm = calls, calls/10
	}
	if c.Procs > 0 {
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(c.Procs))
	}
	workers := max(c.Parallel, 1)
	call, release, err := c.Setup(ctx, workers)
	if err != nil {
		return nil, err
	}
	if release != nil {
		defer release()
	}
	for i := range c.Warm {
		if err := call(i % workers); err != nil {
			return nil, err
		}
	}
	hs := make([]*loadgen.Histogram, workers)
	for i := range hs {
		if hs[i], err = loadgen.NewHistogram(loadgen.DefaultLowest, loadgen.DefaultHighest, loadgen.DefaultDigits); err != nil {
			return nil, err
		}
	}
	errs := make([]error, workers)
	before, _ := proc.SampleProc("/proc", os.Getpid())
	start := time.Now()
	var wg sync.WaitGroup
	for w := range workers {
		n := c.Calls / workers
		if w < c.Calls%workers {
			n++
		}
		wg.Go(func() {
			for range n {
				t0 := time.Now()
				err := call(w)
				hs[w].Record(time.Since(t0))
				if err != nil {
					errs[w] = err
					return
				}
			}
		})
	}
	wg.Wait()
	wall := time.Since(start)
	after, _ := proc.SampleProc("/proc", os.Getpid())
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	h := hs[0]
	for _, o := range hs[1:] {
		if err := h.Merge(o); err != nil {
			return nil, err
		}
	}
	return &ComponentResult{
		Name: name, Calls: c.Calls, Histogram: lab.EncodeHistogram(h),
		CPUSeconds: ((after.UserCPU + after.SysCPU) - (before.UserCPU + before.SysCPU)).Seconds(), WallSeconds: wall.Seconds(),
	}, nil
}

// BuildHarnessTest compiles this package's test binary from the tree at
// root, the worker of either side.
func BuildHarnessTest(ctx context.Context, root, out string) error {
	cmd := exec.CommandContext(ctx, "go", "test", "-c", "-trimpath", "-o", out, "./test/bench/harness") //nolint:gosec // fixed build command
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("harness: build the component worker in %s: %w: %s", root, err, b)
	}
	return nil
}

// runWorker runs one component in a worker of the test binary bin, in
// ruralzd's cpuset with the profile's GOMAXPROCS.
func (s *Session) runWorker(ctx context.Context, bin, name, out string) (*ComponentResult, error) {
	p := s.Profile
	argv := p.Runner(RoleGateway).Argv([]string{bin, "-test.run", "^TestComponentWorker$", "-test.count", "1"}, p.Hosts[RoleGateway].CPUs)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the harness's own test binary
	cmd.Env = append(os.Environ(), EnvComponent+"="+name, EnvComponentOut+"="+out)
	if p.GOMAXPROCS > 0 {
		cmd.Env = append(cmd.Env, "GOMAXPROCS="+strconv.Itoa(p.GOMAXPROCS))
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w: component %s: %w: %s", ErrRefused, name, err, b)
	}
	b, err := os.ReadFile(out) //nolint:gosec // G304: the worker's output file
	if err != nil {
		return nil, err
	}
	var r ComponentResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ComponentSpec compares one component between head and baseline workers.
type ComponentSpec struct {
	Component Component
	Head      Build
	HeadBin   string
	// Baseline and BaselineBin are nil and empty without a baseline.
	Baseline    *Build
	BaselineBin string
	Pairs       int
}

// CompareComponent runs five head and five baseline worker runs
// interleaved, writes a component record per run and the summary: it
// fails when a tied target is missed or the median p99 is more than 5%
// above the baseline's with four of five pairs slower.
func (s *Session) CompareComponent(ctx context.Context, cs ComponentSpec) (*CompareResult, error) {
	n := cs.Pairs
	if n == 0 {
		n = Pairs
	}
	scen := Scenario{ID: "component-" + cs.Component.Name, Budgets: cs.Component.Budgets}
	baseCommit := ""
	if cs.Baseline != nil {
		baseCommit = cs.Baseline.Commit
	}
	one := func(b Build, bin, name string) (*RunOutcome, error) {
		out := filepath.Join(s.Work, "components", cs.Component.Name+"-"+name+".json")
		if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
			return nil, err
		}
		started := time.Now().UTC().Truncate(time.Millisecond)
		r, err := s.runWorker(ctx, bin, cs.Component.Name, out)
		if err != nil {
			return nil, err
		}
		h, err := lab.DecodeHistogram(r.Histogram)
		if err != nil {
			return nil, err
		}
		m := Measures{Internal: h}
		if r.Calls > 0 {
			m.CPUPerRequest = time.Duration(r.CPUSeconds / float64(r.Calls) * float64(time.Second))
		}
		log, err := intervalLog(h, started, started.Add(time.Duration(r.WallSeconds*float64(time.Second))), "ruralz component "+cs.Component.Name)
		if err != nil {
			return nil, err
		}
		file, err := s.Rec.WriteFile(scen.ID, name+".internal.hlog", log)
		if err != nil {
			return nil, err
		}
		rec := s.baseRecord(scen, RunSpec{Build: b, Baseline: baseCommit}, started, benchrecord.KindComponent)
		rec.Load = benchrecord.Load{Tool: "harness", Version: s.ToolVersions[ToolLoadgen], CommandLine: "TestComponentWorker " + cs.Component.Name + " calls=" + strconv.Itoa(r.Calls)}
		rec.Latency = benchrecord.Latency{Internal: Percentiles(h), HistogramFiles: []string{file}}
		rec.Resources = benchrecord.Resources{CPUPerRequest: m.CPUPerRequest}
		rec.Validity = benchrecord.Validity{Verdict: benchrecord.VerdictValid}
		rec.Budgets = EvaluateAll(cs.Component.Budgets, &m)
		path, err := s.Rec.Write(name, rec)
		if err != nil {
			return nil, err
		}
		return &RunOutcome{Record: rec, Path: path, Measures: m, Valid: true}, nil
	}
	var pairs []Pair
	var reasons []string
	refused := false
	for i := 1; i <= n; i++ {
		var p Pair
		h, err := one(cs.Head, cs.HeadBin, "run-"+strconv.Itoa(i)+"-head")
		if err != nil {
			return nil, err
		}
		p.Head = h
		if cs.Baseline != nil && !refused {
			b, err := one(*cs.Baseline, cs.BaselineBin, "run-"+strconv.Itoa(i)+"-baseline")
			switch {
			case errors.Is(err, ErrRefused):
				refused = true
				reasons = append(reasons, "the baseline cannot run the component: "+err.Error())
			case err != nil:
				return nil, err
			default:
				p.Baseline = b
			}
		}
		pairs = append(pairs, p)
	}
	sum, err := Summarize(scen, pairs, cs.Baseline != nil, reasons)
	if err != nil {
		return nil, err
	}
	rec := s.baseRecord(scen, RunSpec{Build: cs.Head, Baseline: baseCommit}, time.Now().UTC().Truncate(time.Millisecond), benchrecord.KindSummary)
	sum.fill(rec)
	path, err := s.Rec.Write("summary-comparison", rec)
	if err != nil {
		return nil, err
	}
	return &CompareResult{
		Summary: rec, Path: path, Verdict: rec.Summary.Verdict, Requeue: rec.Summary.Verdict == benchrecord.ABInconclusive,
		Pass: (rec.Summary.Verdict == benchrecord.ABPass || rec.Summary.Verdict == benchrecord.ABNone) && !AnyFailed(rec.Budgets),
	}, nil
}
