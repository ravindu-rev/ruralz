// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package errcode

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

// Tests for architecture section 2.3 (WP-01) and section 0 convention 5:
// Error carries a code through %w chains, CodeOf finds the outermost one,
// Status maps registered codes; plus the M1 code rows (R-22).

func TestErrorFormat(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.7:8080: connection refused")
	err := Wrap("RZ-UP-001", cause)
	if err.Error() != "RZ-UP-001: dial tcp 10.0.0.7:8080: connection refused" {
		t.Fatalf("Error = %q", err.Error())
	}
	if !errors.Is(err, cause) || !errors.Is(errors.Unwrap(err), cause) {
		t.Fatal("Wrap lost the cause")
	}
	// A nil cause still yields an error that reports only the code.
	bare := Wrap("RZ-RT-001", nil)
	if bare == nil || bare.Error() != "RZ-RT-001" || errors.Unwrap(bare) != nil {
		t.Fatalf("Wrap(code, nil) = %v", bare)
	}
}

func TestErrorf(t *testing.T) {
	inner := io.ErrUnexpectedEOF
	err := Errorf("RZ-CFG-001", "parse %s: %w", "ruralz.yaml", inner)
	if err.Error() != "RZ-CFG-001: parse ruralz.yaml: unexpected EOF" {
		t.Fatalf("Error = %q", err.Error())
	}
	if !errors.Is(err, inner) {
		t.Fatal("Errorf did not wrap its %w argument")
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != "RZ-CFG-001" {
		t.Fatalf("errors.As = %+v", e)
	}
}

func TestCodeOf(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
		ok   bool
	}{
		{"nil", nil, "", false},
		{"plain", errors.New("x"), "", false},
		{"direct", Wrap("RZ-RT-005", nil), "RZ-RT-005", true},
		{"through fmt %w", fmt.Errorf("handler: %w", Wrap("RZ-RT-004", io.EOF)), "RZ-RT-004", true},
		{"outermost wins", Wrap("RZ-RT-015", fmt.Errorf("step: %w", Wrap("RZ-UP-003", nil))), "RZ-RT-015", true},
		{"through errors.Join", errors.Join(errors.New("a"), Wrap("RZ-AUTH-002", nil)), "RZ-AUTH-002", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, ok := CodeOf(tt.err)
			if code != tt.code || ok != tt.ok {
				t.Fatalf("CodeOf = %q, %v; want %q, %v", code, ok, tt.code, tt.ok)
			}
		})
	}
}

func TestStatus(t *testing.T) {
	tests := []struct {
		code string
		want int
	}{
		{"RZ-RT-001", 404},
		{"RZ-RT-017", 400},
		{"RZ-RT-018", 504},
		{"RZ-RT-019", 503},
		{"RZ-UP-011", 502},
		{"RZ-AUTH-008", 421},
		{"RZ-RL-003", 429},
		{"RZ-RT-014", 0},  // StatusNote: the caller decides
		{"RZ-RT-016", 0},  // StatusNote: the caller decides
		{"RZ-CFG-040", 0}, // configuration codes have no status
		{"RZ-STS-001", 0},
		{"", 0},
		{"RZ-RT-999", 0}, // unregistered
	}
	for _, tt := range tests {
		if got := Status(tt.code); got != tt.want {
			t.Errorf("Status(%q) = %d, want %d", tt.code, got, tt.want)
		}
	}
}

func TestM1Codes(t *testing.T) {
	// Architecture 2.3: the rows WP-01 registers, with their statuses.
	tests := []struct {
		id         string
		area       Area
		status     int
		statusNote bool
	}{
		{"RZ-CFG-038", AreaCFG, 0, false},
		{"RZ-CFG-039", AreaCFG, 0, false},
		{"RZ-CFG-040", AreaCFG, 0, false},
		{"RZ-CFG-041", AreaCFG, 0, false},
		{"RZ-RT-016", AreaRT, 0, true},
		{"RZ-RT-017", AreaRT, 400, false},
		{"RZ-RT-018", AreaRT, 504, false},
		{"RZ-RT-019", AreaRT, 503, false},
		{"RZ-UP-011", AreaUP, 502, false},
		{"RZ-AUTH-008", AreaAUTH, 421, false},
	}
	for _, tt := range tests {
		c, ok := Lookup(tt.id)
		if !ok {
			t.Errorf("%s is not registered", tt.id)
			continue
		}
		if c.Area != tt.area || c.Status != tt.status || (c.StatusNote != "") != tt.statusNote || c.Meaning == "" {
			t.Errorf("%s = %+v", tt.id, c)
		}
	}
}
