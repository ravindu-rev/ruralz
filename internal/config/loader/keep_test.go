// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// finding returns a test diagnostic in file f at line n.
func findingAt(f string, n int, sev diag.Severity) diag.Diagnostic {
	return diag.Diagnostic{Code: CodeUnserved, Severity: sev, Location: diag.Location{File: f, Line: n, Column: 1}, Message: "m" + strconv.Itoa(n)}
}

// TestKeeperOrderIndependent covers the keeper's contract: whatever the
// order and batches the diagnostics come in, it keeps the first of them
// in sort order, and past the cap it ends like a diag.Collector of the
// cap: the first cap and one RZ-CFG-001.
func TestKeeperOrderIndependent(t *testing.T) {
	var all diag.List
	for i := range 50 {
		all = append(all, findingAt("f"+strconv.Itoa(i%7)+".yaml", i, diag.SeverityError))
	}
	want := func(maxDiagnostics int) diag.List {
		c := diag.NewCollector(maxDiagnostics)
		sorted := slices.Clone(all)
		sorted.Sort()
		c.AddAll(sorted)
		return c.List()
	}
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: a seeded order makes the property reproducible.
	for _, maxDiagnostics := range []int{3, 10, 49, 50, 80} {
		w := texts(want(maxDiagnostics))
		for round := range 20 {
			shuffled := slices.Clone(all)
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			k := newKeeper(maxDiagnostics)
			for i := 0; i < len(shuffled); {
				n := min(1+rng.IntN(7), len(shuffled)-i)
				k.add(stepParse, i, shuffled[i:i+n])
				i += n
			}
			if len(k.list) > 2*k.max+7 {
				t.Errorf("cap %d: the keeper held %d", maxDiagnostics, len(k.list))
			}
			if got := texts(k.result()); !slices.Equal(got, w) {
				t.Fatalf("cap %d round %d:\n%q\nwant\n%q", maxDiagnostics, round, got, w)
			}
			if k.full() != (len(all) > maxDiagnostics) {
				t.Errorf("cap %d: full = %v", maxDiagnostics, k.full())
			}
		}
	}
}

// TestKeeperErrors covers hasErrors: warnings alone are no error; an error
// is, and so is a passed cap, which the result reports as RZ-CFG-001.
func TestKeeperErrors(t *testing.T) {
	k := newKeeper(5)
	k.add(stepParse, 0, diag.List{findingAt("a", 1, diag.SeverityWarning)})
	if k.hasErrors() {
		t.Error("a warning counts as an error")
	}
	k.add(stepParse, 1, diag.List{findingAt("a", 2, diag.SeverityError)})
	if !k.hasErrors() {
		t.Error("an error does not count")
	}
	w := newKeeper(1)
	w.add(stepParse, 0, diag.List{findingAt("a", 1, diag.SeverityWarning), findingAt("a", 2, diag.SeverityWarning)})
	if !w.hasErrors() || !w.result().HasErrors() {
		t.Error("a passed cap counts as no error")
	}
}

// TestKeeperMergeAndClone covers merge and clone: a merged keeper keeps
// the sort keys and counts of both, and a clone is independent.
func TestKeeperMergeAndClone(t *testing.T) {
	a, b := newKeeper(3), newKeeper(3)
	a.add(stepParse, 0, diag.List{findingAt("b", 1, diag.SeverityError)})
	b.add(stepParse, 0, diag.List{findingAt("a", 1, diag.SeverityWarning), findingAt("c", 1, diag.SeverityWarning)})
	c := a.clone()
	c.merge(b)
	if got := texts(c.result()); len(got) != 3 || c.seen != 3 || c.errs != 1 {
		t.Errorf("merged: %q, seen %d, errs %d", got, c.seen, c.errs)
	}
	if len(a.list) != 1 {
		t.Error("merging into a clone changed the original")
	}
	c.merge(b)
	if !c.full() || len(c.result()) != 4 {
		t.Errorf("past the cap: %q", texts(c.result()))
	}
}
