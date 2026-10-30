// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"errors"
	"strings"
)

// CheckForwardPath reports whether p, a path Ruralz sends to an Upstream
// in place of the client's (a composition step's literal path, or its
// pathExpression result up to the first "?";
// docs/architecture/03-data-plane.md "Composition engine"), passes the
// hardening of the client path: it starts with "/" and passes step 1 of
// [NormalizePath] (no %00, raw or encoded backslash, other raw or encoded
// control byte or invalid escape). Unlike a client path, whose dot
// segments normalization removes, it must hold no "." or ".." segment in
// any spelling, %2E and %2e included, so a computed path cannot climb out
// of the prefix it was built under. Other bytes a path may not hold
// unescaped, such as a space, pass: the Upstream layer escapes them.
//
// The error is a [*PathError] whose Reason names the rule; it carries no
// code, so each caller wraps its own: RZ-CFG-005 for a literal step path
// at validation, RZ-RT-015 for a computed one at runtime.
func CheckForwardPath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return &PathError{Reason: RejectNotOriginForm}
	}
	if _, err := NormalizePath(p); err != nil {
		if pe := (*PathError)(nil); errors.As(err, &pe) {
			return &PathError{Reason: pe.Reason, Offset: pe.Offset}
		}
		return err
	}
	for i := 1; i <= len(p); {
		seg, next := NextSegment(p, i)
		if dotSegment(seg) {
			return &PathError{Reason: RejectDotSegment, Offset: i}
		}
		i = next
	}
	return nil
}

// CheckForwardQuery reports whether q, the raw query a composition step
// sends after the first "?" of its path, can go on a request line: it
// holds no raw control byte (below 0x20, or 0x7F), which net/http refuses
// to write (docs/architecture/03-data-plane.md "Composition engine"). Its
// other bytes are sent as they are. The error is a [*PathError] with
// [RejectControlByte] at the byte's offset in q, without a code.
func CheckForwardQuery(q string) error {
	for i := range len(q) {
		if c := q[i]; c < 0x20 || c == 0x7F {
			return &PathError{Reason: RejectControlByte, Offset: i}
		}
	}
	return nil
}

// dotSegment reports whether segment s decodes to "." or "..": one or two
// characters, each "." or its escape %2E in either case.
func dotSegment(s string) bool {
	dots := 0
	for i := 0; i < len(s); dots++ {
		switch {
		case dots == 2:
			return false
		case s[i] == '.':
			i++
		case len(s)-i >= 3 && s[i] == '%' && s[i+1] == '2' && (s[i+2] == 'E' || s[i+2] == 'e'):
			i += 3
		default:
			return false
		}
	}
	return dots > 0
}
