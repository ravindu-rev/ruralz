// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// CodeRejected is the code of a request target that [NormalizePath]
// rejects: 400, "Request target or framing rejected by request hardening"
// (04 req 23, OQ-security-and-identity-21 (a)).
const CodeRejected = "RZ-RT-017"

// CodeNoRoute is the code of a request that matches no Route: 404, "No
// Route matched on this listener". [NormalizePath] gives it to the
// asterisk-form request target (04 req 23).
const CodeNoRoute = "RZ-RT-001"

// ErrRejected matches (with errors.Is) every request-hardening rejection
// [NormalizePath] returns: a [*PathError] carrying [CodeRejected]
// (RZ-RT-017). The one other error NormalizePath returns, for asterisk-form,
// is [ErrAsteriskForm] and does not match ErrRejected.
var ErrRejected = errors.New("routematch: request target rejected")

// ErrAsteriskForm matches (with errors.Is) the error [NormalizePath] returns
// for the asterisk-form request target "*" (as in "OPTIONS *"). It carries
// [CodeNoRoute] (RZ-RT-001, 404), not RZ-RT-017: asterisk-form is not
// rejected, it matches no Route (04 req 23).
var ErrAsteriskForm = errors.New("routematch: asterisk-form request target matches no Route")

// RejectReason says why [NormalizePath] rejected a path.
type RejectReason uint8

// Reasons for rejecting a request path (04 req 23 step 1).
const (
	// RejectEncodedNUL is a %00 escape.
	RejectEncodedNUL RejectReason = iota + 1
	// RejectBackslash is a raw "\".
	RejectBackslash
	// RejectEncodedBackslash is a %5C or %5c escape.
	RejectEncodedBackslash
	// RejectControlByte is a raw byte below 0x20 or 0x7F.
	RejectControlByte
	// RejectEncodedControlByte is the escape of a control byte other than
	// NUL: %01 to %1F or %7F, in either hex case.
	RejectEncodedControlByte
	// RejectBadEscape is a "%" not followed by two hex digits.
	RejectBadEscape
	// RejectNotOriginForm is a non-empty path that does not start with "/".
	RejectNotOriginForm
)

// String returns a short description of the reason.
func (r RejectReason) String() string {
	switch r {
	case RejectEncodedNUL:
		return "encoded NUL"
	case RejectBackslash:
		return "backslash"
	case RejectEncodedBackslash:
		return "encoded backslash"
	case RejectControlByte:
		return "control byte"
	case RejectEncodedControlByte:
		return "encoded control byte"
	case RejectBadEscape:
		return "invalid percent escape"
	case RejectNotOriginForm:
		return "path does not start with /"
	default:
		return "reason " + strconv.Itoa(int(r))
	}
}

// PathError describes a rejected path. It never holds the path itself, so
// it is safe to log.
type PathError struct {
	// Reason is the rule the path broke.
	Reason RejectReason
	// Offset is the byte offset of the offending byte or escape.
	Offset int
}

// Error returns "routematch: request target rejected: <reason> at byte <n>".
func (e *PathError) Error() string {
	return ErrRejected.Error() + ": " + e.Reason.String() + " at byte " + strconv.Itoa(e.Offset)
}

// Is reports whether target is [ErrRejected].
func (e *PathError) Is(target error) bool { return target == ErrRejected }

func reject(r RejectReason, off int) error {
	return errcode.Wrap(CodeRejected, &PathError{Reason: r, Offset: off})
}

// Character sets as bit masks over ASCII: lo covers bytes 0-63, hi 64-127.
const (
	// unreserved = ALPHA / DIGIT / "-" / "." / "_" / "~" (RFC 3986 2.3).
	unreservedLo uint64 = 1<<'-' | 1<<'.' | (1<<10-1)<<'0'
	unreservedHi uint64 = (1<<26-1)<<('A'-64) | 1<<('_'-64) | (1<<26-1)<<('a'-64) | 1<<('~'-64)
	// pathRaw is every byte a normalized path holds unescaped: unreserved,
	// sub-delims, ":", "@" (pchar, RFC 3986 3.3) and "/".
	pathRawLo = unreservedLo | 1<<'!' | 1<<'$' | 1<<'&' | 1<<'\'' | 1<<'(' | 1<<')' |
		1<<'*' | 1<<'+' | 1<<',' | 1<<';' | 1<<'=' | 1<<':' | 1<<'/'
	pathRawHi = unreservedHi | 1<<('@'-64)
)

const upperHex = "0123456789ABCDEF"

func inSet(c byte, lo, hi uint64) bool {
	switch {
	case c < 64:
		return lo&(1<<c) != 0
	case c < 128:
		return hi&(1<<(c-64)) != 0
	default:
		return false
	}
}

func isUnreserved(c byte) bool { return inSet(c, unreservedLo, unreservedHi) }

func isPathRaw(c byte) bool { return inSet(c, pathRawLo, pathRawHi) }

// unhex returns the value of hex digit c and whether it is a lowercase
// letter digit.
func unhex(c byte) (v byte, lower, ok bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', false, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, false, true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true, true
	default:
		return 0, false, false
	}
}

func isDotSegment(s string) bool { return s == "." || s == ".." }

// NormalizePath returns the normalized form of escaped, the request path
// as r.URL.EscapedPath() returns it (never the decoded r.URL.Path), by the
// steps of 04 req 23:
//
//  1. Reject, with an error that matches [ErrRejected] and carries
//     [CodeRejected] (RZ-RT-017), a path holding %00, a raw "\", %5C or %5c
//     (OQ-security-and-identity-21 (a)), any other control byte (below 0x20,
//     or 0x7F), raw or percent-encoded, an invalid percent escape, or a
//     non-empty path that does not start with "/" other than "*".
//  2. Decode the escapes of unreserved characters (A-Z a-z 0-9 - . _ ~) and
//     upper-case the hex digits of every other escape; %2F stays encoded
//     and never splits a segment. A raw byte a path may not hold unescaped
//     (RFC 3986 pchar, such as a space, "[" or a byte above 0x7F) is
//     percent-encoded, so equivalent spellings share one form.
//  3. Remove dot segments per RFC 3986 section 5.2.4; a path that climbs
//     above the root stays at "/".
//  4. Keep repeated slashes and a trailing slash.
//
// An empty path (absolute-form without a path) normalizes to "/".
//
// The asterisk-form target "*" (as in "OPTIONS *", for which [RequestPath]
// returns "*") matches no Route (04 req 23): NormalizePath returns an error
// that matches [ErrAsteriskForm] and carries [CodeNoRoute] (RZ-RT-001, 404).
// The caller therefore answers an error with the code it carries
// (errcode.CodeOf, errcode.Status), never with a fixed RZ-RT-017.
//
// The caller must also answer CONNECT and HTTP/2 extended CONNECT with 404
// RZ-RT-001 before calling NormalizePath (04 req 22), which cannot tell them
// apart: net/http leaves a CONNECT (authority-form) an empty path, which
// normalizes to "/" and would match a prefix "/" Route, and an extended
// CONNECT carries an ordinary :path.
//
// NormalizePath is idempotent and returns escaped itself, allocating
// nothing, when it is already normal.
func NormalizePath(escaped string) (string, error) {
	if escaped == "" {
		return "/", nil
	}
	if escaped[0] != '/' {
		if escaped == "*" {
			return "", errcode.Wrap(CodeNoRoute, ErrAsteriskForm)
		}
		return "", reject(RejectNotOriginForm, 0)
	}
	rewrite := false
	grow := 0
	seg := 1
	for i := 1; i < len(escaped); i++ {
		c := escaped[i]
		switch {
		case c == '/':
			if isDotSegment(escaped[seg:i]) {
				rewrite = true
			}
			seg = i + 1
		case c == '%':
			if i+2 >= len(escaped) {
				return "", reject(RejectBadEscape, i)
			}
			hi, lowerHi, ok1 := unhex(escaped[i+1])
			lo, lowerLo, ok2 := unhex(escaped[i+2])
			if !ok1 || !ok2 {
				return "", reject(RejectBadEscape, i)
			}
			switch v := hi<<4 | lo; {
			case v == 0:
				return "", reject(RejectEncodedNUL, i)
			case v == '\\':
				return "", reject(RejectEncodedBackslash, i)
			case v < 0x20 || v == 0x7F:
				return "", reject(RejectEncodedControlByte, i)
			case lowerHi || lowerLo || isUnreserved(v):
				rewrite = true
			}
			i += 2
		case isPathRaw(c):
		case c < 0x20 || c == 0x7F:
			return "", reject(RejectControlByte, i)
		case c == '\\':
			return "", reject(RejectBackslash, i)
		default:
			rewrite = true
			grow += 2
		}
	}
	if isDotSegment(escaped[seg:]) {
		rewrite = true
	}
	if !rewrite {
		return escaped, nil
	}
	return rewritePath(escaped, grow), nil
}

// rewritePath applies steps 2 and 3 to a path that passed step 1.
func rewritePath(p string, grow int) string {
	b := make([]byte, 0, len(p)+grow)
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '%':
			hi, _, _ := unhex(p[i+1])
			lo, _, _ := unhex(p[i+2])
			if v := hi<<4 | lo; isUnreserved(v) {
				b = append(b, v)
			} else {
				b = append(b, '%', upperHex[hi], upperHex[lo])
			}
			i += 2
		case isPathRaw(c):
			b = append(b, c)
		default:
			b = append(b, '%', upperHex[c>>4], upperHex[c&0x0F])
		}
	}
	return string(removeDotSegments(b))
}

// removeDotSegments removes "." and ".." segments from b, which starts with
// "/", in place. It gives the result of RFC 3986 section 5.2.4 for an
// absolute path: a final dot segment leaves a trailing slash, and ".." at the
// root is dropped.
func removeDotSegments(b []byte) []byte {
	w := 0
	for r := 0; r < len(b); {
		end := r + 1
		for end < len(b) && b[end] != '/' {
			end++
		}
		seg := b[r+1 : end]
		last := end == len(b)
		dot := len(seg) == 1 && seg[0] == '.'
		dotdot := len(seg) == 2 && seg[0] == '.' && seg[1] == '.'
		switch {
		case dot || dotdot:
			if dotdot {
				for w > 0 {
					w--
					if b[w] == '/' {
						break
					}
				}
			}
			if last {
				b[w] = '/'
				w++
			}
		default:
			w += copy(b[w:], b[r:end])
		}
		r = end
	}
	return b[:w]
}

// RequestPath returns the request path of u exactly as the client sent it,
// still escaped: the input [NormalizePath] expects. It is u.RawPath when set
// and u.EscapedPath() otherwise. u.EscapedPath() alone is not enough:
// when the raw path holds a byte net/url does not accept unescaped (such as
// "{", "|", a double quote or a byte above 0x7F), it re-escapes the decoded
// u.Path, so "/a%2Fb/{x}" would come back as "/a/b/%7Bx%7D" and the encoded
// slash would split a segment (04 req 23). NormalizePath percent-encodes
// those bytes itself. RequestPath allocates nothing for a server request.
func RequestPath(u *url.URL) string {
	if u.RawPath != "" {
		return u.RawPath
	}
	return u.EscapedPath()
}

// NextSegment returns the path segment that starts at byte i of path, just
// after a "/", and the index where the following segment starts. The
// segments of a path are those of strings.Split(path[1:], "/"), so "/" has
// one empty segment and a trailing slash yields a final empty segment:
//
//	for i := 1; i <= len(path); {
//		seg, next := NextSegment(path, i)
//		// use seg
//		i = next
//	}
//
// next is len(path)+1 after the last segment. NextSegment allocates nothing.
func NextSegment(path string, i int) (seg string, next int) {
	j := strings.IndexByte(path[i:], '/')
	if j < 0 {
		return path[i:], len(path) + 1
	}
	return path[i : i+j], i + j + 1
}

// DecodeParam percent-decodes a template capture taken from a normalized
// path, so request.pathParams holds decoded values (04 req 29): "a%2Fb"
// becomes "a/b" and "%C3%A9" becomes "é". It returns raw itself, allocating
// nothing, when raw holds no escape; an invalid escape is kept as is.
func DecodeParam(raw string) string {
	n := strings.IndexByte(raw, '%')
	if n < 0 {
		return raw
	}
	b := make([]byte, 0, len(raw))
	b = append(b, raw[:n]...)
	for i := n; i < len(raw); i++ {
		c := raw[i]
		if c == '%' && i+2 < len(raw) {
			hi, _, ok1 := unhex(raw[i+1])
			lo, _, ok2 := unhex(raw[i+2])
			if ok1 && ok2 {
				b = append(b, hi<<4|lo)
				i += 2
				continue
			}
		}
		b = append(b, c)
	}
	return string(b)
}

// PrefixMatch reports whether path matches prefix on segment boundaries
// (04 req 28): path equals prefix, or starts with it and either prefix ends
// with "/" or the next byte of path is "/". "/" matches every path; an
// encoded slash (%2F) is never a boundary. Both are normalized paths.
func PrefixMatch(prefix, path string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	if len(path) == len(prefix) {
		return true
	}
	return strings.HasSuffix(prefix, "/") || path[len(prefix)] == '/'
}

// CheckPath reports whether s is a valid path.exact or path.prefix value: it
// begins with "/" and passes step 1 of [NormalizePath]. The Router matches
// the normalized form, NormalizePath(s). Errors carry [CodeInvalid]
// (RZ-CFG-005).
func CheckPath(s string) error {
	if !strings.HasPrefix(s, "/") {
		return errcode.Errorf(CodeInvalid, "path %q does not begin with /", s)
	}
	if _, err := NormalizePath(s); err != nil {
		return errcode.Errorf(CodeInvalid, "path %q is not a valid path (%s)", s, rejectReason(err))
	}
	return nil
}
