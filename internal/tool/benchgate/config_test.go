// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"testing"
)

// TestParseConfigDefaults: the defaults of 11 req 64 (10 runs, 3%,
// GOMAXPROCS 4, 2000x) and an explicit zero threshold.
func TestParseConfigDefaults(t *testing.T) {
	c, err := parseConfig([]byte(`{"benchmarks":[{"package":"./internal/gateway/router","pattern":"^BenchmarkMatch$","maxAllocs":30,"owner":"WP-42","budget":"PB-8","note":"S1 pass-through"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Runs != 10 || c.threshold() != 0.03 || c.GOMAXPROCS != 4 {
		t.Errorf("defaults = runs %d, threshold %v, gomaxprocs %d", c.Runs, c.threshold(), c.GOMAXPROCS)
	}
	e := c.Benchmarks[0]
	if e.Benchtime != "2000x" || e.MaxAllocs == nil || *e.MaxAllocs != 30 || e.Owner != "WP-42" || e.Budget != "PB-8" || e.Note == "" {
		t.Errorf("entry = %+v", e)
	}
	c, err = parseConfig([]byte(`{"runs":5,"threshold":0,"gomaxprocs":2,"benchmarks":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Runs != 5 || c.threshold() != 0 || c.GOMAXPROCS != 2 {
		t.Errorf("explicit = runs %d, threshold %v, gomaxprocs %d", c.Runs, c.threshold(), c.GOMAXPROCS)
	}
}

func TestParseConfigErrors(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"not JSON", `{`},
		{"trailing data", `{"benchmarks":[]} {}`},
		{"unknown field", `{"benchmarks":[{"package":"./p","pattern":".","maxAlloc":30}]}`},
		{"negative runs", `{"runs":-1}`},
		{"negative gomaxprocs", `{"gomaxprocs":-2}`},
		{"negative threshold", `{"threshold":-0.1}`},
		{"package without ./", `{"benchmarks":[{"package":"internal/p","pattern":"."}]}`},
		{"package pattern", `{"benchmarks":[{"package":"./internal/...","pattern":"."}]}`},
		{"package escapes", `{"benchmarks":[{"package":"./../p","pattern":"."}]}`},
		{"empty pattern", `{"benchmarks":[{"package":"./p","pattern":""}]}`},
		{"bad pattern", `{"benchmarks":[{"package":"./p","pattern":"("}]}`},
		{"duration benchtime", `{"benchmarks":[{"package":"./p","pattern":".","benchtime":"1s"}]}`},
		{"zero benchtime", `{"benchmarks":[{"package":"./p","pattern":".","benchtime":"0x"}]}`},
		{"negative cap", `{"benchmarks":[{"package":"./p","pattern":".","maxAllocs":-1}]}`},
		{"duplicate", `{"benchmarks":[{"package":"./p","pattern":"."},{"package":"./p","pattern":"."}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseConfig([]byte(tc.in)); !errors.Is(err, errConfig) {
				t.Errorf("parseConfig(%s) = %v, want errConfig", tc.in, err)
			}
		})
	}
}
