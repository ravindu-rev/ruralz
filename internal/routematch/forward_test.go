// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"errors"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// TestCheckForwardPath checks the hardening of a path Ruralz sends in
// place of the client's (docs/architecture/03-data-plane.md "Composition
// engine"): it starts with "/" and holds no "." or ".." segment in any
// spelling, %00, raw or encoded backslash, other raw or encoded control
// byte or invalid escape; other bytes pass, and the error carries no code.
func TestCheckForwardPath(t *testing.T) {
	tests := []struct {
		path string
		want RejectReason // 0 passes
	}{
		{"/", 0},
		{"/orders/42", 0},
		{"/orders/a%2Fb", 0},
		{"/a//b/", 0},
		{"/orders/...", 0},
		{"/orders/.a", 0},
		{"/orders/a.", 0},
		{"/orders/%2E%41", 0},
		{"/a b/é[x]", 0},
		{"/orders/%7e", 0},
		{"", RejectNotOriginForm},
		{"orders/1", RejectNotOriginForm},
		{"*", RejectNotOriginForm},
		{"/orders/.", RejectDotSegment},
		{"/orders/..", RejectDotSegment},
		{"/./orders", RejectDotSegment},
		{"/orders/../admin", RejectDotSegment},
		{"/orders/%2E", RejectDotSegment},
		{"/orders/%2e%2E", RejectDotSegment},
		{"/orders/.%2e/x", RejectDotSegment},
		{"/orders/%2E./x", RejectDotSegment},
		{"/orders/%00", RejectEncodedNUL},
		{"/orders\\1", RejectBackslash},
		{"/orders/%5C", RejectEncodedBackslash},
		{"/orders/%5c", RejectEncodedBackslash},
		{"/orders/\x01", RejectControlByte},
		{"/orders/\x7f", RejectControlByte},
		{"/orders/%0A", RejectEncodedControlByte},
		{"/orders/%7F", RejectEncodedControlByte},
		{"/orders/%zz", RejectBadEscape},
		{"/orders/%2", RejectBadEscape},
	}
	for _, tt := range tests {
		err := CheckForwardPath(tt.path)
		var pe *PathError
		switch {
		case tt.want == 0 && err != nil:
			t.Errorf("CheckForwardPath(%q) = %v, want nil", tt.path, err)
		case tt.want != 0 && (!errors.As(err, &pe) || pe.Reason != tt.want):
			t.Errorf("CheckForwardPath(%q) = %v, want %v", tt.path, err, tt.want)
		}
		if _, coded := errcode.CodeOf(err); coded {
			t.Errorf("CheckForwardPath(%q) = %v carries a code; the caller wraps its own", tt.path, err)
		}
	}
}

// TestCheckForwardQuery checks that a step's raw query holds no raw
// control byte, which no request line can carry; escapes and other bytes
// pass as they are.
func TestCheckForwardQuery(t *testing.T) {
	for _, q := range []string{"", "a=1&b=%00", "q=a b", "x=é#y"} {
		if err := CheckForwardQuery(q); err != nil {
			t.Errorf("CheckForwardQuery(%q) = %v, want nil", q, err)
		}
	}
	for _, q := range []string{"a=\r\n", "\x00", "a=\x7f", "a=\tb"} {
		var pe *PathError
		if err := CheckForwardQuery(q); !errors.As(err, &pe) || pe.Reason != RejectControlByte {
			t.Errorf("CheckForwardQuery(%q) = %v, want a control byte", q, err)
		}
	}
}

// TestRejectDotSegmentString checks the reason's description.
func TestRejectDotSegmentString(t *testing.T) {
	if got := RejectDotSegment.String(); got != "dot segment" {
		t.Errorf("String() = %q", got)
	}
}
