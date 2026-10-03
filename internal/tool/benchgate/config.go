// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Defaults of the gate (docs/architecture/12-performance-budgets-and-benchmarking.md,
// "Regression policy and gates"; 11 req 64).
const (
	defaultRuns       = 10
	defaultThreshold  = 0.03
	defaultGOMAXPROCS = 4
	defaultBenchtime  = "2000x"
)

// config is the gate list test/bench/allocgate.json. The list is data, so
// adding a benchmark never edits the tool (11 req 62).
type config struct {
	// Runs is the number of interleaved runs per side (default 10).
	Runs int `json:"runs"`
	// Threshold is the allowed relative growth of the median allocs/op
	// (default 0.03); nil selects the default, so 0 can be configured.
	Threshold *float64 `json:"threshold"`
	// GOMAXPROCS is the fixed GOMAXPROCS and -test.cpu value (default 4).
	GOMAXPROCS int `json:"gomaxprocs"`
	// Benchmarks lists the gated benchmark sets.
	Benchmarks []entry `json:"benchmarks"`
}

// entry is one gated benchmark set: every benchmark of Package whose name
// matches Pattern.
type entry struct {
	// Package is one package directory relative to the module root, such
	// as "./internal/gateway/router" (no "/..." patterns).
	Package string `json:"package"`
	// Pattern is the -test.bench regular expression.
	Pattern string `json:"pattern"`
	// Benchtime is the fixed iteration count "<N>x" (default 2000x).
	Benchtime string `json:"benchtime,omitempty"`
	// MaxAllocs is an absolute allocs/op cap, such as PB-8's 30.
	MaxAllocs *int64 `json:"maxAllocs,omitempty"`
	// Owner names the area or work package that owns the benchmark.
	Owner string `json:"owner,omitempty"`
	// Budget names the budget the cap enforces, such as "PB-8".
	Budget string `json:"budget,omitempty"`
	// Note is free text for reviewers.
	Note string `json:"note,omitempty"`
}

// threshold returns the configured threshold or the default.
func (c *config) threshold() float64 {
	if c.Threshold == nil {
		return defaultThreshold
	}
	return *c.Threshold
}

var errConfig = errors.New("invalid gate configuration")

// parseConfig decodes and validates a gate configuration, filling defaults.
// Unknown fields are rejected, so a misspelled cap never disables a gate.
func parseConfig(data []byte) (*config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%w: %w", errConfig, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data after the JSON object", errConfig)
	}
	if c.Runs == 0 {
		c.Runs = defaultRuns
	}
	if c.GOMAXPROCS == 0 {
		c.GOMAXPROCS = defaultGOMAXPROCS
	}
	switch {
	case c.Runs < 1:
		return nil, fmt.Errorf("%w: runs must be at least 1, got %d", errConfig, c.Runs)
	case c.GOMAXPROCS < 1:
		return nil, fmt.Errorf("%w: gomaxprocs must be at least 1, got %d", errConfig, c.GOMAXPROCS)
	case c.threshold() < 0:
		return nil, fmt.Errorf("%w: threshold must not be negative, got %g", errConfig, c.threshold())
	}
	benchtime := regexp.MustCompile(`^[1-9][0-9]*x$`)
	seen := map[string]bool{}
	for i := range c.Benchmarks {
		e := &c.Benchmarks[i]
		if e.Benchtime == "" {
			e.Benchtime = defaultBenchtime
		}
		switch {
		case !strings.HasPrefix(e.Package, "./") || strings.Contains(e.Package, "..") || strings.HasSuffix(e.Package, "/..."):
			return nil, fmt.Errorf("%w: benchmarks[%d]: package %q must be one package path starting with ./", errConfig, i, e.Package)
		case e.Pattern == "":
			return nil, fmt.Errorf("%w: benchmarks[%d]: pattern is required", errConfig, i)
		case !benchtime.MatchString(e.Benchtime):
			return nil, fmt.Errorf("%w: benchmarks[%d]: benchtime %q must be a fixed iteration count such as 2000x", errConfig, i, e.Benchtime)
		case e.MaxAllocs != nil && *e.MaxAllocs < 0:
			return nil, fmt.Errorf("%w: benchmarks[%d]: maxAllocs must not be negative", errConfig, i)
		}
		if _, err := regexp.Compile(e.Pattern); err != nil {
			return nil, fmt.Errorf("%w: benchmarks[%d]: pattern: %w", errConfig, i, err)
		}
		key := e.Package + " " + e.Pattern
		if seen[key] {
			return nil, fmt.Errorf("%w: benchmarks[%d]: %s %s is listed twice", errConfig, i, e.Package, e.Pattern)
		}
		seen[key] = true
	}
	return &c, nil
}
