// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"crypto/sha256"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// URIDigest returns U, the SHA-256 of the request URI
// (docs/architecture/09-traffic-management-and-resilience.md "Response
// caching", URI row) in the form scheme "://" host [":" port] path ["?"
// query]: scheme as given; host, the Router's normalized host (lower
// case, no trailing dot, an IPv6 literal in canonical form), with the
// port of authority, the request's Host or :authority as sent, unless it
// is the scheme's default; path, the Router's normalized path; and the
// raw query with its escapes normalized (AppendQuery), an empty query
// equal to none. The normalized path keeps "?" escaped, so the form is
// unambiguous.
func URIDigest(scheme, host, authority, path, rawQuery string) statestore.Digest {
	var buf [256]byte
	return sha256.Sum256(AppendURI(buf[:0], scheme, host, authority, path, rawQuery))
}

// AppendURI appends the form URIDigest hashes to dst.
func AppendURI(dst []byte, scheme, host, authority, path, rawQuery string) []byte {
	dst = append(dst, scheme...)
	dst = append(dst, "://"...)
	dst = append(dst, host...)
	if p := port(authority); p != "" && p != defaultPort(scheme) {
		dst = append(dst, ':')
		dst = append(dst, p...)
	}
	dst = append(dst, path...)
	if rawQuery != "" {
		dst = append(dst, '?')
		dst = AppendQuery(dst, rawQuery)
	}
	return dst
}

// port returns the port of an authority, without leading zeros ("0" for
// all zeros), or "" when it names none: host, [v6], host: or [v6]:. A
// host with more than one colon and no brackets has no port.
func port(authority string) string {
	var p string
	if strings.HasPrefix(authority, "[") {
		i := strings.IndexByte(authority, ']')
		if i < 0 || i+1 >= len(authority) || authority[i+1] != ':' {
			return ""
		}
		p = authority[i+2:]
	} else {
		i := strings.IndexByte(authority, ':')
		if i < 0 || strings.IndexByte(authority[i+1:], ':') >= 0 {
			return ""
		}
		p = authority[i+1:]
	}
	if p == "" {
		return ""
	}
	if t := strings.TrimLeft(p, "0"); t != "" {
		return t
	}
	return "0"
}

// defaultPort returns the default port of scheme.
func defaultPort(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// AppendQuery appends rawQuery with its escapes normalized: an escape of
// an unreserved byte (RFC 3986 section 2.3) is decoded and every other
// escape keeps its byte with upper-case hex digits; a raw byte a query may
// not hold unescaped (RFC 3986 section 3.4: pchar, "/" and "?") is
// escaped, and so is a "%" that starts no valid escape. Parameters keep
// their order and are never decoded otherwise.
func AppendQuery(dst []byte, rawQuery string) []byte {
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(rawQuery); i++ {
		c := rawQuery[i]
		switch {
		case c == '%' && i+2 < len(rawQuery) && isHex(rawQuery[i+1]) && isHex(rawQuery[i+2]):
			v := unhex(rawQuery[i+1])<<4 | unhex(rawQuery[i+2])
			if isUnreserved(v) {
				dst = append(dst, v)
			} else {
				dst = append(dst, '%', hex[v>>4], hex[v&0xf])
			}
			i += 2
		case c != '%' && isQueryRaw(c):
			dst = append(dst, c)
		default:
			dst = append(dst, '%', hex[c>>4], hex[c&0xf])
		}
	}
	return dst
}

// isHex reports a hex digit.
func isHex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

// unhex returns the value of the hex digit c.
func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	default:
		return c - 'a' + 10
	}
}

// isUnreserved reports ALPHA, DIGIT, "-", ".", "_" or "~".
func isUnreserved(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	default:
		return c == '-' || c == '.' || c == '_' || c == '~'
	}
}

// isQueryRaw reports a byte a query holds unescaped: unreserved,
// sub-delims, ":", "@", "/" and "?".
func isQueryRaw(c byte) bool {
	if isUnreserved(c) {
		return true
	}
	return strings.IndexByte("!$&'()*+,;=:@/?", c) >= 0
}

// PartitionDigest returns P, the SHA-256 of the Route name, a NUL byte and
// the partition value ("Response caching", Partition row), so Routes never
// share entries while an invalidation by U reaches each of them.
func PartitionDigest(route, value string) statestore.Digest {
	var buf [256]byte
	b := append(buf[:0], route...)
	b = append(b, 0)
	b = append(b, value...)
	return sha256.Sum256(b)
}
