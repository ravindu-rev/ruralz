// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package httpfield

import (
	"errors"
	"strings"
	"testing"
)

// Tests for 07 req 23 (literal values), 07 req 27 (computed values) and the
// T1 value cases of spec 07 section 6.

func TestValidValueReq23(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", true},
		{"plain", "max-age=63072000; includeSubDomains", true},
		{"CR", "a\rb", false},
		{"LF", "a\nb", false},
		{"CRLF injection", "a\r\nX-Evil: 1", false},
		{"NUL", "a\x00b", false},
		{"0x01", "a\x01b", false},
		{"0x1F", "a\x1fb", false},
		{"DEL 0x7F", "a\x7fb", false},
		{"inner HTAB", "a\tb", true},
		{"inner SP", "a b", true},
		{"obs-text 0x80", "a\x80b", true},
		{"obs-text 0xFF", "a\xffb", true},
		{"UTF-8", "café", true},
		{"leading SP", " a", false},
		{"trailing SP", "a ", false},
		{"leading HTAB", "\ta", false},
		{"trailing HTAB", "a\t", false},
		{"only SP", " ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidValue(tt.in); got != tt.want {
				t.Errorf("ValidValue(%q) = %v, want %v", tt.in, got, tt.want)
			}
			if got := CheckValue(tt.in) == nil; got != tt.want {
				t.Errorf("CheckValue(%q) == nil is %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestCheckValueReasonsReq23(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"8 KiB passes", strings.Repeat("v", MaxValueBytes), nil},
		{"8 KiB + 1", strings.Repeat("v", MaxValueBytes+1), ErrValueTooLong},
		{"control", "secret\r\nvalue", ErrValueControl},
		{"whitespace", "secret ", ErrValueWhitespace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckValue(tt.in)
			if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
				t.Fatalf("CheckValue = %v, want %v", err, tt.want)
			}
			// Errors never echo the value (it may come from a request).
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Errorf("error %q echoes the value", err)
			}
		})
	}
}

func TestNormalizeValueReq27(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr error
	}{
		{"trimmed", "  v  ", "v", nil},
		{"trimmed HTAB", "\tv\t", "v", nil},
		{"inner whitespace kept", " a \t b ", "a \t b", nil},
		{"empty", "", "", nil},
		{"whitespace only sets an empty field", " \t ", "", nil},
		{"CRLF", "a\r\nb", "", ErrValueControl},
		{"NUL", "\x00", "", ErrValueControl},
		{"DEL", "a\x7f", "", ErrValueControl},
		{"8 KiB after trimming", "  " + strings.Repeat("v", MaxComputedValueBytes) + "  ", strings.Repeat("v", MaxComputedValueBytes), nil},
		{"8 KiB + 1", strings.Repeat("v", MaxComputedValueBytes+1), "", ErrValueTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeValue(tt.in)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) {
				t.Fatalf("NormalizeValue(%q) error = %v, want %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("NormalizeValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if err == nil && !ValidValue(got) {
				t.Errorf("NormalizeValue(%q) = %q, which ValidValue rejects", tt.in, got)
			}
		})
	}
}

func TestTrimOWS(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"a", "a"},
		{" \t a b\t ", "a b"},
		{"\t\t", ""},
		{"\na\n", "\na\n"}, // only SP and HTAB are OWS
	}
	for _, tt := range tests {
		if got := TrimOWS(tt.in); got != tt.want {
			t.Errorf("TrimOWS(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLimitsReq37(t *testing.T) {
	// 07 req 37: 32 entries x 8 KiB bound the bytes one Policy adds.
	if MaxValueBytes != 8192 || MaxComputedValueBytes != MaxValueBytes {
		t.Errorf("MaxValueBytes = %d, MaxComputedValueBytes = %d, want 8192", MaxValueBytes, MaxComputedValueBytes)
	}
	if 32*MaxValueBytes != 256<<10 {
		t.Errorf("32 x MaxValueBytes = %d, want 256 KiB", 32*MaxValueBytes)
	}
	if MaxNameBytes != 256 || MaxQueryNameBytes != 256 || MaxQueryValueBytes != 8192 {
		t.Errorf("limits = %d, %d, %d", MaxNameBytes, MaxQueryNameBytes, MaxQueryValueBytes)
	}
}
