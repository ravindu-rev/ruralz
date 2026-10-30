// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// TestComponentWorker is the component benchmark worker: the harness
// re-runs this test binary in ruralzd's cpuset with RURALZ_BENCH_COMPONENT
// naming the component and RURALZ_BENCH_COMPONENT_OUT the result file.
func TestComponentWorker(t *testing.T) {
	name := os.Getenv(EnvComponent)
	if name == "" {
		t.Skip("runs only as a component benchmark worker (" + EnvComponent + ")")
	}
	res, err := RunComponent(t.Context(), name, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(EnvComponentOut), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestComponents runs every component benchmark briefly: each sets up,
// times every call into its histogram and measures its CPU.
func TestComponents(t *testing.T) {
	for _, c := range Components() {
		t.Run(c.Name, func(t *testing.T) {
			res, err := RunComponent(t.Context(), c.Name, 1000)
			if err != nil {
				t.Fatalf("RunComponent(%q) = %v", c.Name, err)
			}
			h, err := lab.DecodeHistogram(res.Histogram)
			if err != nil {
				t.Fatal(err)
			}
			if res.Calls != 1000 || h.Count() != 1000 {
				t.Errorf("RunComponent(%q) timed %d calls into %d samples, want 1000", c.Name, res.Calls, h.Count())
			}
			if res.CPUSeconds < 0 || res.WallSeconds <= 0 {
				t.Errorf("RunComponent(%q) CPU %v s over %v s", c.Name, res.CPUSeconds, res.WallSeconds)
			}
			if len(c.Budgets) == 0 {
				t.Errorf("component %q ties no target", c.Name)
			}
		})
	}
	if _, err := RunComponent(t.Context(), "nonexistent", 10); err == nil {
		t.Error("RunComponent(nonexistent) = nil error")
	}
}

// TestComponentTargets checks the targets the documents tie to the
// component benchmarks: CEL under 2 µs at p99, the RZ-RT-005 rejection's
// 16 µs mean CPU.
func TestComponentTargets(t *testing.T) {
	for _, c := range Components() {
		for _, b := range c.Budgets {
			switch b.ID {
			case "cel-p99":
				if b.Kind != InternalP99 || time.Duration(b.Limit) >= 2*time.Microsecond {
					t.Errorf("%s cel-p99 = %+v, want under 2µs at p99", c.Name, b)
				}
			case "rz-rt-005-mean-cpu":
				if b.Kind != MaxMeanCPU || time.Duration(b.Limit) != 16*time.Microsecond {
					t.Errorf("%s = %+v, want a 16µs mean CPU", c.Name, b)
				}
			default:
				t.Errorf("%s budget %q is not a documented target", c.Name, b.ID)
			}
		}
	}
	m := &Measures{CPUPerRequest: 17 * time.Microsecond}
	r := Budget{ID: "x", Value: "v", Kind: MaxMeanCPU, Limit: float64(16 * time.Microsecond)}.Evaluate(m)
	if r.Verdict != "fail" || r.Result != "17.0µs" {
		t.Errorf("mean CPU 17µs against 16µs = %+v, want a failure", r)
	}
}
