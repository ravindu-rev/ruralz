// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package harness

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// freePort returns a loopback port free at the time of the call.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// writeScenario writes a one-Route Bundle like S1's, with OTLP export to
// the sink, and the loopback Environment, on the profile's ports.
func writeScenario(t *testing.T, dir string, p *Profile, listen int) Scenario {
	t.Helper()
	bundle := fmt.Sprintf(`apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: bench
spec:
  listeners:
    - name: http
      protocol: http
      port: %d
  admin:
    port: %d
  telemetry:
    otlp: {endpoint: "https://%s"}
    traceSampling: 0.01
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: mock
spec:
  protocol: http
  endpoints:
    - address: "%s"
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: bench-1k
spec:
  match:
    path: {exact: /bench/1k}
    methods: [GET]
  upstreams:
    - name: mock
`, listen, p.adminPort(), p.Services.Sink, p.Services.Mock)
	envs := "apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: loopback\nspec:\n  variables:\n    BENCH: \"1\"\n"
	bdir := filepath.Join(dir, "s1")
	if err := os.MkdirAll(bdir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bdir, "ruralz.yaml"), []byte(bundle), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "environments.yaml"), []byte(envs), 0o600); err != nil {
		t.Fatal(err)
	}
	return Scenario{
		ID: "S1", Bundle: bdir, Environments: filepath.Join(dir, "environments.yaml"), Saturation: 200,
		Workload: Workload{Tool: ToolLoadgen, Method: "GET", Path: "/bench/1k", Port: listen, Protocol: "http1", Connections: 2, Rate: 100, BodySize: 1024},
		Budgets:  []Budget{pb1(), pb4()},
	}
}

// TestLoopbackRun drives the harness end to end on the loopback profile:
// the release ruralzd on the head's Revision behind the lab services, a
// run writing a schema-valid record with both views and its histogram
// logs, a one-pair comparison and a two-step ladder writing their
// summaries. Benchmark runs use real time by design.
func TestLoopbackRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	commit, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	// Benchmark runs use the release build: a prebuilt controllable-clock
	// build (RURALZ_TEST_BIN_DIR) is rebuilt as the release one.
	set, err := binaries.Build(ctx, root, filepath.Join(work, "bin"), binaries.BuildOptions{})
	if err == nil && set.TestClock {
		set, err = binaries.Build(ctx, root, filepath.Join(work, "bin"), binaries.BuildOptions{Commit: commit})
	}
	if err != nil {
		t.Fatal(err)
	}
	tc, err := goVersion(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	head := Build{Ruralzd: set.Ruralzd, Ruralz: set.Ruralz, Commit: commit, Version: binaries.DefaultVersion, Toolchain: tc}
	p := Loopback(work)
	port := func() string { return "127.0.0.1:" + strconv.Itoa(freePort(t)) }
	p.Services = Services{Mock: port(), IdP: port(), Sink: port(), StateStore: port(), FaultProxy: port(), EchoPort: freePort(t)}
	p.AdminPort = freePort(t)
	sc := writeScenario(t, filepath.Join(work, "scenarios"), p, freePort(t))
	st := Settings{Records: filepath.Join(work, "records")}
	s, err := NewSession(ctx, p, st, root, filepath.Join(work, "session"))
	if err != nil {
		t.Fatal(err)
	}
	rev, err := Render(ctx, head.Ruralz, root, sc, p)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Run(ctx, RunSpec{Scenario: sc, Build: head, Revision: rev, Name: "run-1-head", Warmup: time.Second, Duration: 3 * time.Second, NoSettle: true})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Rec.ReadRecord("S1", out.Path)
	if err != nil {
		t.Fatalf("the run's record does not read back: %v", err)
	}
	t.Logf("run: %s, reasons %q", rec.Validity.Verdict, rec.Validity.Reasons)
	if rec.Identity.RevisionDigest != rev.Digest || rec.Load.OfferedRate != 100 || rec.Environment.HardwareProfile != "loopback" {
		t.Errorf("record = %+v", rec.Identity)
	}
	if out.Measures.External.Count() < 250 {
		t.Errorf("external view holds %d requests, want about 300", out.Measures.External.Count())
	}
	if out.Measures.Internal == nil || out.Measures.Internal.Count() == 0 {
		t.Errorf("internal view is empty: no access record folded (reasons %q)", rec.Validity.Reasons)
	}
	for _, f := range rec.Latency.HistogramFiles {
		if _, err := os.Stat(filepath.Join(st.Records, "S1", f)); err != nil {
			t.Errorf("histogram file %s: %v", f, err)
		}
	}
	if rec.Resources.CPUPerRequest <= 0 || rec.Resources.RSSLoadBytes <= 0 {
		t.Errorf("resources = %+v", rec.Resources)
	}
	cmp, err := s.Compare(ctx, CompareSpec{Scenario: sc, Head: head, Baseline: &head, Revision: rev, Warmup: time.Second, Duration: 2 * time.Second, NoSettle: true, Pairs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Summary.Summary.Of != benchrecord.OfComparison || len(cmp.Summary.Summary.Runs) != 2 || !cmp.Requeue {
		t.Errorf("one-pair comparison = %+v: fewer than five pairs is inconclusive", cmp.Summary.Summary)
	}
	if _, err := s.Rec.ReadRecord("S1", cmp.Path); err != nil {
		t.Errorf("the summary does not read back: %v", err)
	}
	lad, err := s.Ladder(ctx, LadderSpec{Scenario: sc, Build: head, Revision: rev, Ladder: &loadgen.Ladder{Expected: 200, From: 0.5, To: 0.6, Step: 0.1, StepDuration: 2 * time.Second, Warmup: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if steps := lad.Summary.Ladder.Steps; len(steps) != 2 || steps[0].OfferedRate != 100 || steps[1].OfferedRate != 120 {
		t.Errorf("ladder steps = %+v", steps)
	}
}
