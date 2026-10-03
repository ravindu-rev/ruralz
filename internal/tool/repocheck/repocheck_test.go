// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// badCatalog is the catalog fixture of the testdata/bad repository (spec 09
// test 19): one family per catalog gate case, matched against
// testdata/bad/docs/architecture/10-observability.md.
func badCatalog() catalogView {
	cv := linkedCatalog()
	route, status := catalog.Label{Name: "route"}, catalog.Label{Name: "status_class", Values: []string{"2xx"}}
	listener := catalog.Label{Name: "listener"}
	cv.families = []catalog.Family{
		{Name: "ruralz_fixture_requests_total", Kind: catalog.Counter, Unit: "{requests}", Labels: []catalog.Label{route, status}},
		// A counter without _total (req 77 (d)); its OBS row matches.
		{Name: "ruralz_x_count", Kind: catalog.Counter, Unit: "{requests}"},
		{Name: "ruralz_fixture_latency_seconds", Kind: catalog.Histogram, Bounds: catalog.BoundsFast, Unit: "s", Labels: []catalog.Label{listener}},
		{Name: "ruralz_fixture_open_connections", Kind: catalog.Gauge, Unit: "{connections}", Labels: []catalog.Label{listener}},
		{Name: "ruralz_fixture_queue_items", Kind: catalog.Gauge, Unit: "{items}"},
		{Name: "ruralz_fixture_errors_total", Kind: catalog.Counter, Unit: "{errors}", Labels: []catalog.Label{{Name: "code", Code: true}}},
		{Name: "ruralz_fixture_missing_total", Kind: catalog.Counter, Unit: "{requests}"},
		// Multi-name rows: an ambiguous Type cell, a "<value> for <word>"
		// exception, an ambiguous Unit cell.
		{Name: "ruralz_fixture_a_items", Kind: catalog.Gauge, Unit: "{items}"},
		{Name: "ruralz_fixture_b_items", Kind: catalog.Gauge, Unit: "{items}"},
		{Name: "ruralz_fixture_c_total", Kind: catalog.Counter, Unit: "{items}"},
		{Name: "ruralz_fixture_d_items", Kind: catalog.Gauge, Unit: "{items}"},
		{Name: "ruralz_fixture_e_items", Kind: catalog.Gauge, Unit: "{items}"},
		{Name: "ruralz_fixture_h_total", Kind: catalog.Counter, Unit: "{items}"},
		{Name: "ruralz_fixture_f_items", Kind: catalog.Gauge, Unit: "{items}"},
		{Name: "ruralz_fixture_g_total", Kind: catalog.Counter, Unit: "{items}"},
		// Listed only in the AI metrics table.
		{Name: "ruralz_fixture_ai_total", Kind: catalog.Counter, Unit: "{requests}"},
	}
	cv.reasons = []string{"fixture_ok", "fixture_later", "fixture_missing", "fixture_elsewhere"}
	return cv
}

// TestBadRepo checks every testdata/bad case, including the telemetry name
// checks and catalog gates of spec 09 tests 2 and 19 (09 req 74, 75, 77).
func TestBadRepo(t *testing.T) {
	findings, err := run(filepath.Join("testdata", "bad"), "allow.txt", badCatalog())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.String())
	}
	want := []string{
		"cmd/app: cmd/<binary> holds only main.go",
		"cmd/app/main.go:3: cmd/ holds wiring only: move declarations",
		"cmd/app/main.go:7: cmd/ holds wiring only: only func main",
		`internal/a/a.go:3: import "C" is forbidden`,
		"internal/a/a.go:7: RZ-RT-" + "999 is not registered", // split so repocheck does not flag this file
		`internal/gate/gate.go:3: no-license-check: "licensekey"`,
		`internal/gate/gate.go:5: no-license-check: "revington.co"`,
		`pkg/p/p.go:6: public package imports "github.com/example/thirdparty"`,
		`pkg/p/p.go:7: public package imports "github.com/ravindu-rev/ruralz/internal/errcode"`,
		"console/app.ts:1: missing license header",
		// 09 req 77 (a) to (c); names_test.go and the catalog raise nothing.
		`internal/telemetry/names.go:5: metric name "ruralz_http_requests_total": use the internal/telemetry/catalog constant`,
		`internal/telemetry/names.go:7: span name "ruralz.filter.x"`,
		`internal/telemetry/names.go:9: span started with the literal name "custom"`,
		// 09 req 77 (d).
		"internal/telemetry/catalog/catalog.go: counter ruralz_x_count must end in _total",
		// 09 req 74: the code gate, against the "Ruralz Gateway metrics" and
		// "Degraded states" tables only.
		`docs/architecture/10-observability.md:9: ruralz_fixture_latency_seconds uses the fast bounds in the catalog, "histogram, request" here`,
		"docs/architecture/10-observability.md:10: label protocol of ruralz_fixture_open_connections is not in the catalog",
		`docs/architecture/10-observability.md:11: ruralz_fixture_queue_items has unit {items} (items) in the catalog, "entries" here`,
		`docs/architecture/10-observability.md:12: ruralz_fixture_errors_total is a counter in the catalog, "gauge" here`,
		"docs/architecture/10-observability.md: catalog family ruralz_fixture_missing_total is missing from the metrics catalog tables",
		`docs/architecture/10-observability.md:14: the Type cell "Gauge; Counter" gives neither one value per name nor a value with "<value> for <word>" exceptions`,
		`docs/architecture/10-observability.md:15: ruralz_fixture_d_items is a gauge in the catalog, "counter" here`,
		`docs/architecture/10-observability.md:16: the Unit cell "items, cycles, bytes" gives neither one value per name`,
		`docs/architecture/10-observability.md:22: catalog family ruralz_fixture_ai_total is listed under "AI metrics", not in the "Ruralz Gateway metrics" table`,
		"docs/architecture/10-observability.md:29: degraded reason fixture_later is registered in M1 but tagged Planned (M2) here",
		"docs/architecture/10-observability.md: degraded reason fixture_missing is missing from the degraded states table",
		"docs/architecture/10-observability.md: degraded reason fixture_elsewhere is missing from the degraded states table",
		// 09 req 75: the doc gate.
		"docs/architecture/02-other.md:3: ruralz_bogus_total is not in the metrics catalog",
		"docs/architecture/02-other.md:5: degraded reason bogus_reason is not in the degraded states table",
		"docs/architecture/02-other.md:8: degraded reason fixture_unknown is not in the degraded states table",
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if strings.HasPrefix(g, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing finding %q", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d findings, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
}

// TestRepositoryIsClean runs every check, the telemetry catalog gates
// included, on the repository itself.
func TestRepositoryIsClean(t *testing.T) {
	findings, err := run(filepath.Join("..", "..", ".."), filepath.FromSlash("internal/tool/repocheck/nolicensecheck.allow"), linkedCatalog())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}

func TestAllowlistFormat(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{
		"internal/ entitlement",             // no reason
		"internal/ entitlement #",           // empty reason
		"internal/ notaterm # reason",       // unknown term
		"internal/ entitlement extra # why", // too many fields
	} {
		p := filepath.Join(dir, "allow")
		if err := os.WriteFile(p, []byte(bad+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAllowlist(p); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	if a, err := loadAllowlist(filepath.Join(dir, "missing")); err != nil || a != nil {
		t.Errorf("missing allowlist: %v, %v", a, err)
	}
}

func TestNormalize(t *testing.T) {
	for _, s := range []string{"licenseKey", "license_key", "LICENSE-KEY", "license key"} {
		if !strings.Contains(normalize(s), "licensekey") {
			t.Errorf("normalize(%q) = %q", s, normalize(s))
		}
	}
}
