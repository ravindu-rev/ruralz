// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"math"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// finding returns an error diagnostic in file at line.
func finding(file string, line int, sev diag.Severity) diag.Diagnostic {
	return diag.Diagnostic{Code: "RZ-CFG-005", Severity: sev, Location: diag.Location{File: file, Line: line}, Message: "m" + strconv.Itoa(line)}
}

// TestKeeperCap covers the run's cap: up to the cap the keeper returns
// every finding in sort order; past it the first findings in sort order,
// whatever order they were offered in, and one RZ-CFG-001; full and
// hasErrors tell the run to stop and that it has no Revision, counting
// findings the cap dropped.
func TestKeeperCap(t *testing.T) {
	k := newKeeper(3)
	k.add(stageH, 0, diag.List{finding("b.yaml", 1, diag.SeverityError)})
	k.add(stageF, 0, diag.List{finding("a.yaml", 2, diag.SeverityWarning), finding("a.yaml", 1, diag.SeverityWarning)})
	if k.full() || !k.hasErrors() {
		t.Errorf("3 of 3: full %v, hasErrors %v", k.full(), k.hasErrors())
	}
	wantTexts(t, "at the cap", k.result(),
		"a.yaml:1 warning RZ-CFG-005: m1",
		"a.yaml:2 warning RZ-CFG-005: m2",
		"b.yaml:1 error RZ-CFG-005: m1",
	)
	k.add(stageJ, 0, diag.List{finding("a.yaml", 3, diag.SeverityWarning)})
	if !k.full() || !k.hasErrors() {
		t.Errorf("4 of 3: full %v, hasErrors %v", k.full(), k.hasErrors())
	}
	wantTexts(t, "past the cap", k.result(),
		"- error RZ-CFG-001: too many diagnostics; stopped after 3",
		"a.yaml:1 warning RZ-CFG-005: m1",
		"a.yaml:2 warning RZ-CFG-005: m2",
		"a.yaml:3 warning RZ-CFG-005: m3",
	)
	w := newKeeper(10)
	w.add(stageLoad, 0, diag.List{finding("a.yaml", 1, diag.SeverityWarning)})
	if w.hasErrors() || w.full() {
		t.Errorf("warnings only: hasErrors %v, full %v", w.hasErrors(), w.full())
	}
	if big := newKeeper(math.MaxInt); big.max != math.MaxInt || big.trimAt() != math.MaxInt {
		t.Errorf("an uncapped keeper: max %d, trimAt %d", big.max, big.trimAt())
	}
}

// TestKeeperTotalOrder covers ties of the sort key: equal findings keep
// stage, item and position order, so the kept set never depends on the
// order a pool offers them in.
func TestKeeperTotalOrder(t *testing.T) {
	d := finding("a.yaml", 1, diag.SeverityError)
	a, b := d, d
	a.Hint, b.Hint = "first", "second"
	k := newKeeper(1)
	k.add(stageJ, 2, diag.List{b})
	k.add(stageJ, 1, diag.List{a})
	if got := k.result(); len(got) != 2 || got[1].Hint != "first" {
		t.Errorf("result %+v, want the finding of the earlier item kept", got)
	}
	for range 3 {
		k.add(stageK, 0, diag.List{d, d})
	}
	if len(k.list) > 2*k.max {
		t.Errorf("the keeper holds %d findings, more than twice its %d", len(k.list), k.max)
	}
}
