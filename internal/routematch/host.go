// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"net/netip"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// CodeInvalid is the code of an invalid match criterion found at
// validation: a host pattern or a path template that breaks the rules of
// this package (RZ-CFG-005).
const CodeInvalid = "RZ-CFG-005"

// Host name limits (RFC 1035 2.3.4).
const (
	maxHostLen  = 253
	maxLabelLen = 63
)

// NormalizeHost returns the request host the Router matches (04 req 24):
// r.Host (HTTP/1.1 Host, HTTP/2 :authority) lower-cased (ASCII), without
// its port, and without one trailing dot. An IPv6 literal keeps its
// brackets and takes the canonical RFC 5952 text of [netip.Addr.String], as
// [ParseHostPattern] gives it, so every spelling of one address matches the
// same entry: "[2001:DB8:0::1]:8080" becomes "[2001:db8::1]". A bracketed
// literal that is not an IPv6 address without a zone is only lower-cased,
// as is a host with more than one colon and no brackets, which has no port
// to strip. It returns h itself, allocating nothing, when h is already
// normal.
func NormalizeHost(h string) string {
	if strings.HasPrefix(h, "[") {
		if i := strings.IndexByte(h, ']'); i >= 0 {
			return canonicalIPv6(h[:i+1])
		}
	} else if i := strings.IndexByte(h, ':'); i >= 0 && strings.IndexByte(h[i+1:], ':') < 0 {
		h = h[:i]
	}
	h = strings.TrimSuffix(h, ".")
	return lowerASCII(h)
}

// maxIPv6Text is the length of the longest bracketed canonical IPv6 text.
const maxIPv6Text = len("[ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff]")

// canonicalIPv6 returns the bracketed literal b ("[...]") with the
// canonical text of its IPv6 address, or b lower-cased when it holds no
// IPv6 address without a zone. It allocates only when the text changes.
func canonicalIPv6(b string) string {
	a, err := netip.ParseAddr(b[1 : len(b)-1])
	if err != nil || !a.Is6() || a.Zone() != "" {
		return lowerASCII(b)
	}
	var buf [maxIPv6Text]byte
	out := append(buf[:0], '[')
	out = a.AppendTo(out)
	out = append(out, ']')
	if string(out) == b {
		return b
	}
	return string(out)
}

// lowerASCII lower-cases the ASCII letters of s, allocating only when s
// holds an upper-case letter.
func lowerASCII(s string) string {
	i := 0
	for i < len(s) && (s[i] < 'A' || s[i] > 'Z') {
		i++
	}
	if i == len(s) {
		return s
	}
	b := []byte(s)
	for ; i < len(b); i++ {
		if c := b[i]; 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// HostTier is precedence rank 1 (04 req 31): the kind of host entry that
// matched.
type HostTier uint8

// Host tiers, highest precedence first.
const (
	// HostExact is an exact host entry.
	HostExact HostTier = iota
	// HostWildcard is a "*." entry; a longer suffix ranks higher.
	HostWildcard
	// HostAny is a Route without hosts.
	HostAny
)

// HostPattern is one parsed match.hosts (or listener hostnames) entry. The
// zero value matches any host and stands for a Route without hosts.
type HostPattern struct {
	// Exact is the lower-cased host of an exact entry.
	Exact string
	// Suffix is ".shop.example" for "*.shop.example".
	Suffix string
}

// ParseHostPattern parses and canonicalizes a host entry: lower-cased, one
// trailing dot removed, either an exact host name, a bracketed IPv6 literal
// (kept in the canonical text of [netip.Addr.String], as [NormalizeHost]
// gives it), or "*." followed by a host name (OQ-data-plane-2 (a)). A "*" anywhere
// else, an empty label, a port, a character outside letters, digits, "-" and
// "_", or a name longer than 253 bytes is an error carrying [CodeInvalid]
// (RZ-CFG-005).
func ParseHostPattern(s string) (HostPattern, error) {
	h := lowerASCII(strings.TrimSuffix(s, "."))
	if h == "" {
		return HostPattern{}, errcode.Errorf(CodeInvalid, "host %q is empty", s)
	}
	if rest, ok := strings.CutPrefix(h, "*."); ok {
		if err := checkHostName(s, rest); err != nil {
			return HostPattern{}, err
		}
		return HostPattern{Suffix: h[1:]}, nil
	}
	if strings.HasPrefix(h, "[") {
		inner, ok := strings.CutSuffix(h[1:], "]")
		a, err := netip.ParseAddr(inner)
		if !ok || err != nil || !a.Is6() || a.Zone() != "" {
			return HostPattern{}, errcode.Errorf(CodeInvalid,
				"host %q is not a bracketed IPv6 address without zone or port", s)
		}
		return HostPattern{Exact: "[" + a.String() + "]"}, nil
	}
	if err := checkHostName(s, h); err != nil {
		return HostPattern{}, err
	}
	return HostPattern{Exact: h}, nil
}

// checkHostName checks the lower-cased host name h of entry s.
func checkHostName(s, h string) error {
	if len(h) > maxHostLen {
		return errcode.Errorf(CodeInvalid, "host %q is longer than %d bytes", s, maxHostLen)
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" {
			return errcode.Errorf(CodeInvalid, "host %q has an empty label", s)
		}
		if len(label) > maxLabelLen {
			return errcode.Errorf(CodeInvalid, "host %q has a label longer than %d bytes", s, maxLabelLen)
		}
		for i := 0; i < len(label); i++ {
			switch c := label[i]; {
			case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
			case c == '*':
				return errcode.Errorf(CodeInvalid,
					`host %q: "*" is allowed only as one whole leading label, as in "*.example.com"`, s)
			case c == ':':
				return errcode.Errorf(CodeInvalid, "host %q has a port; hosts match without ports", s)
			default:
				return errcode.Errorf(CodeInvalid,
					"host %q has a character other than letters, digits, '-' and '_' (use the A-label of an internationalized name)", s)
			}
		}
	}
	return nil
}

// CheckHost reports whether s is a valid host entry (see [ParseHostPattern]).
func CheckHost(s string) error {
	_, err := ParseHostPattern(s)
	return err
}

// IsAny reports whether p is the zero pattern, which matches every host.
func (p HostPattern) IsAny() bool { return p.Exact == "" && p.Suffix == "" }

// Tier returns the precedence tier of p.
func (p HostPattern) Tier() HostTier {
	switch {
	case p.Suffix != "":
		return HostWildcard
	case p.Exact != "":
		return HostExact
	default:
		return HostAny
	}
}

// Match reports whether host, normalized by [NormalizeHost], matches p. A
// wildcard matches one or more leading labels and never the bare suffix:
// "*.shop.example" matches "eu.shop.example" and "a.b.shop.example", not
// "shop.example" or "xshop.example" (04 req 27).
func (p HostPattern) Match(host string) bool {
	switch {
	case p.Suffix != "":
		return len(host) > len(p.Suffix) && strings.HasSuffix(host, p.Suffix)
	case p.Exact != "":
		return host == p.Exact
	default:
		return true
	}
}

// String returns the canonical entry: the host, "*" plus the suffix, or ""
// for any host.
func (p HostPattern) String() string {
	if p.Suffix != "" {
		return "*" + p.Suffix
	}
	return p.Exact
}

// HostTable maps host patterns to per-host entries of type V (a Router keeps
// one path index per entry): an exact-host map, a wildcard map keyed by
// suffix, and one any-host entry (04 req 27). The zero value is empty and
// ready to use. Build it with [HostTable.Entry]; once built, [HostTable.Lookup]
// is safe for concurrent use.
type HostTable[V any] struct {
	exact    map[string]*V
	wildcard map[string]*V
	anyHost  *V
	// minSuffix and maxSuffix bound the wildcard suffix lengths, so a
	// lookup skips suffixes no entry can match.
	minSuffix, maxSuffix int
}

// Entry returns the entry for p, creating a zero V on first use.
func (t *HostTable[V]) Entry(p HostPattern) *V {
	switch p.Tier() {
	case HostExact:
		if t.exact == nil {
			t.exact = make(map[string]*V)
		}
		v := t.exact[p.Exact]
		if v == nil {
			v = new(V)
			t.exact[p.Exact] = v
		}
		return v
	case HostWildcard:
		if t.wildcard == nil {
			t.wildcard = make(map[string]*V)
		}
		v := t.wildcard[p.Suffix]
		if v == nil {
			v = new(V)
			t.wildcard[p.Suffix] = v
			n := len(p.Suffix)
			if t.minSuffix == 0 || n < t.minSuffix {
				t.minSuffix = n
			}
			t.maxSuffix = max(t.maxSuffix, n)
		}
		return v
	default:
		if t.anyHost == nil {
			t.anyHost = new(V)
		}
		return t.anyHost
	}
}

// Len returns the number of entries.
func (t *HostTable[V]) Len() int {
	n := len(t.exact) + len(t.wildcard)
	if t.anyHost != nil {
		n++
	}
	return n
}

// Lookup calls yield for every entry whose pattern matches host (normalized
// by [NormalizeHost]) in precedence rank 1 order: the exact entry, then
// wildcard entries from the longest suffix to the shortest, then the
// any-host entry. It stops when yield returns false. The pattern passed to
// yield may share memory with host. Lookup allocates nothing.
func (t *HostTable[V]) Lookup(host string, yield func(p HostPattern, v *V) bool) {
	if v := t.exact[host]; v != nil {
		if !yield(HostPattern{Exact: host}, v) {
			return
		}
	}
	if len(t.wildcard) > 0 {
		// Suffixes start at a dot after at least one byte of host, longest
		// first, so a wildcard never matches its bare suffix.
		for i := 1; i < len(host); i++ {
			if host[i] != '.' {
				continue
			}
			n := len(host) - i
			if n < t.minSuffix {
				break
			}
			if n > t.maxSuffix {
				continue
			}
			if v := t.wildcard[host[i:]]; v != nil {
				if !yield(HostPattern{Suffix: host[i:]}, v) {
					return
				}
			}
		}
	}
	if t.anyHost != nil {
		yield(HostPattern{}, t.anyHost)
	}
}
