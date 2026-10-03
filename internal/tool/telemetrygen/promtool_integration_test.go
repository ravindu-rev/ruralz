// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPromtoolRules is spec 09 test 43 (09 req 72): the generated rules pass
// "promtool check rules" (every expression parses with the Prometheus PromQL
// parser) and "promtool test rules" with testdata/ruralz.rules.test.yaml,
// which pre-creates every degraded reason at 0 and holds series that already
// count an event at the first scrape. Every dashboard panel expression must
// parse too: each becomes a recording rule of a scratch rule file. `make
// integration` puts the pinned promtool of bin/ on PATH; without it the test
// is skipped.
func TestPromtoolRules(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("promtool is not installed (make tools installs the pinned release into bin/)")
	}
	files, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, f := range files {
		if f.path == rulesFile {
			if err := os.WriteFile(filepath.Join(dir, filepath.Base(rulesFile)), f.data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	tests, err := os.ReadFile(filepath.Join("testdata", "ruralz.rules.test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ruralz.rules.test.yaml"), tests, 0o600); err != nil { //nolint:gosec // G703: a fixed name under t.TempDir.
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dashboards.rules.yaml"), dashboardRecordingRules(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"check", "rules", filepath.Base(rulesFile)},
		{"test", "rules", "ruralz.rules.test.yaml"},
		{"check", "rules", "dashboards.rules.yaml"},
	} {
		cmd := exec.CommandContext(t.Context(), promtool, args...) //nolint:gosec // G204: promtool from PATH with fixed arguments.
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("promtool %v: %v\n%s", args, err, out)
		}
	}
}

// dashboardRecordingRules turns every panel expression of the pack into a
// recording rule, with Grafana's $__rate_interval replaced by a duration,
// so promtool parses it.
func dashboardRecordingRules() []byte {
	var b bytes.Buffer
	b.WriteString("groups:\n  - name: dashboards\n    rules:\n")
	n := 0
	for _, d := range dashboards() {
		for _, p := range d.panels {
			for _, t := range p.targets {
				expr := strings.ReplaceAll(t.expr, "$__rate_interval", "5m")
				fmt.Fprintf(&b, "      - record: dashboard:expr_%d\n        expr: %s\n", n, yamlString(expr))
				n++
			}
		}
	}
	return b.Bytes()
}
