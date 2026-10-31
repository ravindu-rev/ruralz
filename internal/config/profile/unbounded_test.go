// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"math"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// TestFileNodeBudgetSaturates covers a per-document node limit too large
// for five quarters of it to fit an int, such as math.MaxInt for no
// practical limit: the per-file budget is math.MaxInt instead of a wrapped
// negative number that refused every file.
func TestFileNodeBudgetSaturates(t *testing.T) {
	for in, want := range map[int]int{
		1_000_000:       1_250_000,
		12:              15,
		math.MaxInt:     math.MaxInt,
		math.MaxInt / 4: math.MaxInt/4 + math.MaxInt/16,
	} {
		if got := maxFileNodes(in); got != want {
			t.Errorf("maxFileNodes(%d) = %d, want %d", in, got, want)
		}
	}
	_, diags, err := Parse(t.Context(), []byte("a: 1\n---\nb: [1, 2]\n"), 0, "f.yaml", FormatYAML, Options{MaxDocumentTokens: math.MaxInt})
	if err != nil || len(diags) > 0 {
		t.Errorf("MaxDocumentTokens math.MaxInt: %q, %v", codes(diags), err)
	}
}

// TestDepthCappedAtJSONLimit covers a MaxDepth above jsonval's limit: the
// YAML parser stops at jsonval.MaxDepthLimit, as the JSON scanner does, so
// its recursion stays bounded however large the option.
func TestDepthCappedAtJSONLimit(t *testing.T) {
	nested := func(lists int) []byte {
		return []byte("a: " + strings.Repeat("[", lists) + strings.Repeat("]", lists) + "\n")
	}
	opts := Options{MaxDepth: math.MaxInt}
	// The root mapping is depth 1, so jsonval.MaxDepthLimit-1 lists fit.
	if _, diags, _ := Parse(t.Context(), nested(jsonval.MaxDepthLimit-1), 0, "f.yaml", FormatYAML, opts); len(diags) > 0 {
		t.Errorf("depth %d: %q", jsonval.MaxDepthLimit, codes(diags))
	}
	_, diags, _ := Parse(t.Context(), nested(jsonval.MaxDepthLimit), 0, "f.yaml", FormatYAML, opts)
	if len(diags) != 1 || diags[0].Code != codeParse || !strings.Contains(diags[0].Message, "10000") {
		t.Errorf("depth %d: %q", jsonval.MaxDepthLimit+1, codes(diags))
	}
}
