// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package httpfield

import (
	"errors"
	"strings"
	"testing"
)

// Tests for 07 req 21 (field names are RFC 9110 tokens of 1 to 256 bytes)
// and the T1 name cases of spec 07 section 6.

// rfcTchar is tchar spelled out as RFC 9110 section 5.6.2 lists it; the
// reference the constant bit masks are checked against.
const rfcTchar = "!#$%&'*+-.^_`|~" +
	"0123456789" +
	"ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"abcdefghijklmnopqrstuvwxyz"

func refIsTchar(c byte) bool { return strings.IndexByte(rfcTchar, c) >= 0 }

func TestIsTokenCharMatchesRFC9110Req21(t *testing.T) {
	for c := range 256 {
		if got, want := IsTokenChar(byte(c)), refIsTchar(byte(c)); got != want {
			t.Errorf("IsTokenChar(%#02x) = %v, want %v", c, got, want)
		}
	}
}

func TestValidNameReq21(t *testing.T) {
	// Every tchar is a valid one-byte name.
	for i := range len(rfcTchar) {
		if name := rfcTchar[i : i+1]; !ValidName(name) {
			t.Errorf("ValidName(%q) = false, want true", name)
		}
	}
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"256 bytes", strings.Repeat("a", 256), true},
		{"257 bytes", strings.Repeat("a", 257), false},
		{"space", "x-a b", false},
		{"colon", "x:a", false},
		{"pseudo-header", ":authority", false},
		{"parenthesis", "x(a", false},
		{"non-ASCII", "x-café", false},
		{"obs-text byte", "x\x80", false},
		{"DEL", "x\x7f", false},
		{"NUL", "x\x00", false},
		{"HTAB", "x\ta", false},
		{"typical", "X-Shop-Consumer", true},
		{"every tchar", rfcTchar, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidName(tt.in); got != tt.want {
				t.Errorf("ValidName(%q) = %v, want %v", tt.in, got, tt.want)
			}
			if got := CheckName(tt.in) == nil; got != tt.want {
				t.Errorf("CheckName(%q) == nil is %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestCheckNameReasonsReq21(t *testing.T) {
	tests := []struct {
		in   string
		want error
		text string
	}{
		{"", ErrNameEmpty, "empty"},
		{strings.Repeat("a", 257), ErrNameTooLong, "257 bytes"},
		{"x y", ErrNameChar, "offset 1"},
		{":path", ErrNameChar, "offset 0"},
	}
	for _, tt := range tests {
		err := CheckName(tt.in)
		if !errors.Is(err, tt.want) {
			t.Errorf("CheckName(%q) = %v, want %v", tt.in, err, tt.want)
			continue
		}
		if !strings.Contains(err.Error(), tt.text) {
			t.Errorf("CheckName(%q) = %q, want it to mention %q", tt.in, err, tt.text)
		}
	}
}

func TestIsToken(t *testing.T) {
	if IsToken("") {
		t.Error(`IsToken("") = true`)
	}
	if !IsToken(strings.Repeat("a", 1000)) {
		t.Error("IsToken(1000 bytes) = false; IsToken has no length limit")
	}
}

func TestEqualFold(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"X-Foo", "x-foo", true},
		{"x-foo", "X-FOO", true},
		{"", "", true},
		{"x-foo", "x-fo", false},
		{"x-foo", "x-fop", false},
		// The Kelvin sign folds onto k in strings.EqualFold, never here.
		{"K", "k", false},
		{"[", "{", false}, // 0x5B and 0x7B differ only in bit 0x20
	}
	for _, tt := range tests {
		if got := equalFold(tt.a, tt.b); got != tt.want {
			t.Errorf("equalFold(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestNameAndValueChecksDoNotAllocate(t *testing.T) {
	name, value := "X-Shop-Consumer", "gold\tcustomer"
	allocs := testing.AllocsPerRun(100, func() {
		if !ValidName(name) || !ValidValue(value) || CheckName(name) != nil || CheckValue(value) != nil {
			t.Fatal("unexpected invalid input")
		}
		if _, err := NormalizeValue("  v  "); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("allocations = %v, want 0", allocs)
	}
}
