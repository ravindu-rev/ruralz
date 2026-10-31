// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestBudgetStopsYAML covers a Budget on a YAML file: the node past it
// stops the file with RZ-CFG-001 at that node, in the Budget's words, and
// the file yields no document.
func TestBudgetStopsYAML(t *testing.T) {
	// The root mapping, four keys and four values: 9 nodes; the tenth is
	// the fifth key.
	src := []byte("a: 1\nb: 2\nc: 3\nd: 4\ne: 5\n")
	b := NewBudget(9, "the load holds more than 9 nodes")
	docs, diags, err := Parse(t.Context(), src, 0, "f.yaml", FormatYAML, Options{Budget: b})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 || codes(diags) != "RZ-CFG-001@5:1" || diags[0].Message != "the load holds more than 9 nodes" {
		t.Errorf("docs %d, %s %v", len(docs), codes(diags), diags)
	}
	if !b.Exceeded() || b.Taken() != 9 {
		t.Errorf("Exceeded %v, Taken %d", b.Exceeded(), b.Taken())
	}
}

// TestBudgetStopsJSON covers a Budget on a JSON file, whose values no
// other node limit bounds: the value past it stops the file at that value.
func TestBudgetStopsJSON(t *testing.T) {
	src := []byte(`{"a": [1, 2, 3], "b": 4}`)
	if _, diags, _ := Parse(t.Context(), src, 0, "f.json", FormatJSON, Options{MaxDocumentTokens: 1}); len(diags) != 0 {
		t.Errorf("without a Budget: %s", codes(diags))
	}
	b := NewBudget(4, "over budget")
	docs, diags, err := Parse(t.Context(), src, 0, "f.json", FormatJSON, Options{Budget: b})
	if err != nil {
		t.Fatal(err)
	}
	// Values: the object, the list, 1, 2; the fifth is 3.
	if len(docs) != 0 || codes(diags) != "RZ-CFG-001@1:14" || diags[0].Message != "over budget" {
		t.Errorf("docs %d, %s %v", len(docs), codes(diags), diags)
	}
	b = NewBudget(6, "over budget")
	if docs, diags, _ := Parse(t.Context(), src, 0, "f.json", FormatJSON, Options{Budget: b}); len(docs) != 1 || len(diags) != 0 || b.Taken() != 6 {
		t.Errorf("at the budget: docs %d, %s, Taken %d", len(docs), codes(diags), b.Taken())
	}
}

// TestBudgetReplacesFileBound covers a Budget in place of the per-file
// bound of five quarters of MaxDocumentTokens: one file may then hold as
// many nodes as the Budget, while each document keeps its own bound.
func TestBudgetReplacesFileBound(t *testing.T) {
	// Three documents of 5 nodes: over the file bound of 12 for
	// MaxDocumentTokens 10.
	src := []byte("a: 1\nb: 2\n---\na: 1\nb: 2\n---\na: 1\nb: 2\n")
	if _, diags, _ := Parse(t.Context(), src, 0, "f.yaml", FormatYAML, Options{MaxDocumentTokens: 10}); codes(diags) != "RZ-CFG-001@7:4" {
		t.Fatalf("without a Budget: %s", codes(diags))
	}
	docs, diags, _ := Parse(t.Context(), src, 0, "f.yaml", FormatYAML, Options{MaxDocumentTokens: 10, Budget: NewBudget(100, "x")})
	if len(docs) != 3 || len(diags) != 0 {
		t.Errorf("with a Budget: %d documents, %s", len(docs), codes(diags))
	}
	_, diags, _ = Parse(t.Context(), []byte("a: 1\nb: 2\nc: 3\nd: 4\ne: 5\n"), 0, "f.yaml", FormatYAML, Options{MaxDocumentTokens: 10, Budget: NewBudget(100, "x")})
	if codes(diags) != "RZ-CFG-001@5:4" || diags[0].Message != "document has more than 10 nodes" {
		t.Errorf("a document past its own bound: %s %v", codes(diags), diags)
	}
}

// TestBudgetShared covers one Budget over several files: each takes from
// what the others left, and Taken counts every node admitted.
func TestBudgetShared(t *testing.T) {
	b := NewBudget(12, "the load holds more than 12 nodes")
	if _, diags, _ := Parse(t.Context(), []byte("a: 1\nb: 2\n"), 0, "a.yaml", FormatYAML, Options{Budget: b}); len(diags) != 0 || b.Taken() != 5 {
		t.Fatalf("first file: %s, Taken %d", codes(diags), b.Taken())
	}
	if _, diags, _ := Parse(t.Context(), []byte(`{"x": [1, 2]}`), 1, "b.json", FormatJSON, Options{Budget: b}); len(diags) != 0 || b.Taken() != 9 {
		t.Fatalf("second file: %s, Taken %d", codes(diags), b.Taken())
	}
	_, diags, _ := Parse(t.Context(), []byte("a: 1\nb: 2\n"), 2, "c.yaml", FormatYAML, Options{Budget: b})
	if codes(diags) != "RZ-CFG-001@2:1" || !b.Exceeded() || b.Taken() != 12 {
		t.Errorf("third file: %s, Exceeded %v, Taken %d", codes(diags), b.Exceeded(), b.Taken())
	}
	if _, diags, _ := Parse(t.Context(), []byte("a: 1\n"), 3, "d.yaml", FormatYAML, Options{Budget: b}); codes(diags) != "RZ-CFG-001@1:1" {
		t.Errorf("a file after the Budget is spent: %s", codes(diags))
	}
}

// TestBudgetConcurrent covers a Budget shared by Parse calls on many
// goroutines: together they never admit more than its limit, and the race
// detector finds no unsynchronized state.
func TestBudgetConcurrent(t *testing.T) {
	var src strings.Builder
	for i := range 100 {
		src.WriteString("k" + strconv.Itoa(i) + ": [1, 2, 3]\n")
	}
	b := NewBudget(1000, "over")
	var wg sync.WaitGroup
	var mu sync.Mutex
	stopped := 0
	for i := range 8 {
		wg.Go(func() {
			_, diags, _ := Parse(t.Context(), []byte(src.String()), 0, "f"+strconv.Itoa(i)+".yaml", FormatYAML, Options{Budget: b})
			if len(diags) > 0 {
				mu.Lock()
				stopped++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	// Each file holds 1 + 100 keys + 100 lists + 300 items = 501 nodes, so
	// at most one file fits and the others stop.
	if b.Taken() != 1000 || !b.Exceeded() || stopped < 7 {
		t.Errorf("Taken %d, Exceeded %v, %d files stopped", b.Taken(), b.Exceeded(), stopped)
	}
}

// TestNewBudgetFloor covers a limit of 0 or below, which admits no node:
// what earlier files of a load leave can be exactly nothing, and the next
// file's first node is then past the budget.
func TestNewBudgetFloor(t *testing.T) {
	for _, limit := range []int{0, -5} {
		b := NewBudget(limit, "x")
		if b.take() || b.Taken() != 0 || !b.Exceeded() {
			t.Errorf("NewBudget(%d): Taken %d, Exceeded %v", limit, b.Taken(), b.Exceeded())
		}
	}
	if _, diags, _ := Parse(t.Context(), []byte("a: 1\n"), 0, "f.yaml", FormatYAML, Options{Budget: NewBudget(0, "spent")}); codes(diags) != "RZ-CFG-001@1:1" {
		t.Errorf("a file with nothing left: %s", codes(diags))
	}
	if docs, diags, _ := Parse(t.Context(), []byte("# only a comment\n"), 0, "f.yaml", FormatYAML, Options{Budget: NewBudget(0, "spent")}); len(docs) != 0 || len(diags) != 0 {
		t.Errorf("an empty file with nothing left: %d documents, %s", len(docs), codes(diags))
	}
}
