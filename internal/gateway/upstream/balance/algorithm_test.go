// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"strings"
	"testing"
)

// TestAlgorithmNames covers the loadBalancing.algorithm spellings of 05 req
// 11 (d) and the least-request default of 05 req 4.
func TestAlgorithmNames(t *testing.T) {
	tests := []struct {
		in   string
		want Algorithm
	}{
		{"", LeastRequest},
		{"least-request", LeastRequest},
		{"round-robin", RoundRobin},
		{"ring-hash", RingHash},
		{"random", Random},
	}
	for _, tt := range tests {
		got, err := ParseAlgorithm(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ParseAlgorithm(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
		if tt.in != "" && got.String() != tt.in {
			t.Errorf("%v.String() = %q, want %q", got, got.String(), tt.in)
		}
		if !got.Valid() {
			t.Errorf("%v not valid", got)
		}
	}
	var zero Algorithm
	if zero != LeastRequest {
		t.Errorf("zero Algorithm = %v, want least-request (05 req 4 default)", zero)
	}
	for _, bad := range []string{"maglev", "Round-Robin", "least_request"} {
		if _, err := ParseAlgorithm(bad); err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("ParseAlgorithm(%q) error = %v", bad, err)
		}
	}
	if a := Algorithm(9); a.Valid() || a.String() != "Algorithm(9)" {
		t.Errorf("Algorithm(9): valid %v, string %q", a.Valid(), a.String())
	}
}
