// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package keys is the State Store key layout (spec 08 section 2.6,
// docs/architecture/11-scalability-and-distributed-state.md "Distributed
// state catalog"): the rz:rl:, rz:qt: and rz:rc: prefixes with rzplg:
// reserved for Plugin state, the escaping of free-form names, the hash
// tags that keep one partition in one Redis Cluster slot, the CRC16 slot
// function, the Response Cache generation, partition and lease keys with
// their hash fields, and the entry MAC (spec 08 req 68, resolution R-49).
//
// Every builder appends to dst and allocates nothing when dst has the
// capacity; the layout is a compatibility surface (the release document's
// "State Store layout changes"), pinned by golden files under testdata/.
package keys

import (
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Key prefixes (spec 08 req 47). Another prefix needs a D11 catalog row.
const (
	// PrefixRateLimit holds Rate Limit TATs.
	PrefixRateLimit = "rz:rl:"
	// PrefixQuota holds Quota counters (and Token Budget counters from M3).
	PrefixQuota = "rz:qt:"
	// PrefixCache holds Response Cache generations, partitions and leases.
	PrefixCache = "rz:rc:"
	// PrefixPlugin is reserved for Plugin state (M2,
	// OQ-wasm-plugin-system-19 (a)); nothing in M1 writes under it.
	PrefixPlugin = "rzplg:"
)

// Prefixes returns every key prefix in catalog order, the reserved one
// last.
func Prefixes() []string {
	return []string{PrefixRateLimit, PrefixQuota, PrefixCache, PrefixPlugin}
}

// Reserved reports whether key lies in a namespace M1 never writes
// (rzplg:, Plugin state from M2).
func Reserved(key string) bool { return strings.HasPrefix(key, PrefixPlugin) }

// Response Cache key suffixes and partition hash fields (spec 08 req 48).
// A variant field is its prefix followed by the lowercase hex SHA-256 of
// the Vary-selected values (V).
const (
	// SuffixGeneration ends the per-URI generation key.
	SuffixGeneration = ":gen"
	// SuffixLease ends the per-partition fill and revalidation lease key.
	SuffixLease = ":lease"
	// FieldNames holds the Vary names the partition was stored under.
	FieldNames = "n"
	// FieldOrder holds the variant digests in store order (at most 8).
	FieldOrder = "o"
	// FieldGeneration prefixes a variant's generation (g:<V>).
	FieldGeneration = "g:"
	// FieldTime prefixes a variant's store time in server milliseconds (t:<V>).
	FieldTime = "t:"
	// FieldEntry prefixes a variant's opaque, MAC-tagged entry (e:<V>).
	FieldEntry = "e:"
)

// Lengths of the fixed-size keys and fields, for stack buffers.
const (
	// HexLen is the length of a lowercase hex SHA-256 digest.
	HexLen = 2 * len(statestore.Digest{})
	// CacheGenerationLen is the length of a generation key.
	CacheGenerationLen = len(PrefixCache) + 1 + HexLen + 1 + len(SuffixGeneration)
	// CachePartitionLen is the length of a partition hash key.
	CachePartitionLen = len(PrefixCache) + 1 + HexLen + 1 + HexLen + 1
	// CacheLeaseLen is the length of a partition lease key.
	CacheLeaseLen = CachePartitionLen + len(SuffixLease)
	// FieldLen is the length of a variant field name.
	FieldLen = 2 + HexLen
)

// upperHex is the digit set of %XX escapes; hexDigits that of digests.
const (
	upperHex  = "0123456789ABCDEF"
	hexDigits = "0123456789abcdef"
)

// AppendHex appends the lowercase hex encoding of d.
func AppendHex(dst []byte, d statestore.Digest) []byte { return hex.AppendEncode(dst, d[:]) }

// unreserved reports whether c stays unescaped in a key: [A-Za-z0-9._-].
func unreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

// AppendEscaped appends s with every byte outside [A-Za-z0-9._-] written
// as %XX in uppercase hex (spec 08 req 48), so '{', '}' and ':' never
// appear and distinct names give distinct keys. RFC 1123 labels, such as
// Policy names, are unchanged.
func AppendEscaped(dst []byte, s string) []byte {
	for i := range len(s) {
		c := s[i]
		if unreserved(c) {
			dst = append(dst, c)
			continue
		}
		dst = append(dst, '%', upperHex[c>>4], upperHex[c&0x0f])
	}
	return dst
}

// Unescape reverses AppendEscaped; ok is false for text AppendEscaped
// never produces (a reserved byte, a malformed or lowercase escape, or an
// escape of an unreserved byte).
func Unescape(s string) (string, bool) {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if unreserved(c) {
			b.WriteByte(c)
			continue
		}
		if c != '%' || i+2 >= len(s) {
			return "", false
		}
		hi, okHi := unhex(s[i+1])
		lo, okLo := unhex(s[i+2])
		if !okHi || !okLo {
			return "", false
		}
		v := hi<<4 | lo
		if unreserved(v) {
			return "", false
		}
		b.WriteByte(v)
		i += 2
	}
	return b.String(), true
}

// unhex decodes one uppercase hex digit.
func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// AppendDuration appends d in the canonical form time.Duration.String
// gives it (1s, 1m0s, 24h0m0s, 1.5s, 100ms), without allocating.
func AppendDuration(dst []byte, d time.Duration) []byte {
	if d == 0 {
		return append(dst, "0s"...)
	}
	// Two's complement: negating the unsigned value is exact for every
	// negative d, including math.MinInt64.
	u := uint64(d) //nolint:gosec // reinterpreted, then negated below when d < 0
	if d < 0 {
		dst = append(dst, '-')
		u = -u
	}
	switch {
	case u < uint64(time.Microsecond):
		dst = strconv.AppendUint(dst, u, 10)
		return append(dst, "ns"...)
	case u < uint64(time.Millisecond):
		dst = appendDecimal(dst, u, uint64(time.Microsecond), 3)
		return append(dst, "µs"...)
	case u < uint64(time.Second):
		dst = appendDecimal(dst, u, uint64(time.Millisecond), 6)
		return append(dst, "ms"...)
	}
	h := u / uint64(time.Hour)
	u -= h * uint64(time.Hour)
	m := u / uint64(time.Minute)
	u -= m * uint64(time.Minute)
	if h > 0 {
		dst = strconv.AppendUint(dst, h, 10)
		dst = append(dst, 'h')
	}
	if h > 0 || m > 0 {
		dst = strconv.AppendUint(dst, m, 10)
		dst = append(dst, 'm')
	}
	dst = appendDecimal(dst, u, uint64(time.Second), 9)
	return append(dst, 's')
}

// appendDecimal appends v/unit with the remainder as a fraction of digits
// places, trailing zeros removed.
func appendDecimal(dst []byte, v, unit uint64, digits int) []byte {
	dst = strconv.AppendUint(dst, v/unit, 10)
	frac := v % unit
	if frac == 0 {
		return dst
	}
	var buf [9]byte
	for i := digits - 1; i >= 0; i-- {
		buf[i] = byte('0' + frac%10)
		frac /= 10
	}
	n := digits
	for n > 0 && buf[n-1] == '0' {
		n--
	}
	dst = append(dst, '.')
	return append(dst, buf[:n]...)
}

// appendTag appends {<hex d>}.
func appendTag(dst []byte, d statestore.Digest) []byte {
	dst = append(dst, '{')
	dst = AppendHex(dst, d)
	return append(dst, '}')
}

// AppendRateLimit appends the GCRA key of one limits[] entry (spec 08 req
// 48, ADR-0008 Keys row):
// rz:rl:<policy>:<requests>/<window>:{<hex of the config.key digest>}.
// Every limit of one Policy and key shares the hash tag, so one script
// updates them together; a changed requests or window is a new key.
func AppendRateLimit(dst []byte, policy string, l statestore.GCRALimit, d statestore.Digest) []byte {
	dst = append(dst, PrefixRateLimit...)
	dst = AppendEscaped(dst, policy)
	dst = append(dst, ':')
	dst = strconv.AppendInt(dst, l.Requests, 10)
	dst = append(dst, '/')
	dst = AppendDuration(dst, l.Window)
	dst = append(dst, ':')
	return appendTag(dst, d)
}

// AppendQuota appends the counter key of one Quota window (spec 08 req 48):
// rz:qt:<escaped consumerQuota>:<window>:<window start in Unix
// milliseconds>:{<hex of the config.key digest>}.
func AppendQuota(dst []byte, name string, window time.Duration, start time.Time, d statestore.Digest) []byte {
	dst = append(dst, PrefixQuota...)
	dst = AppendEscaped(dst, name)
	dst = append(dst, ':')
	dst = AppendDuration(dst, window)
	dst = append(dst, ':')
	dst = strconv.AppendInt(dst, start.UnixMilli(), 10)
	dst = append(dst, ':')
	return appendTag(dst, d)
}

// AppendCacheGeneration appends the generation key of a URI digest U:
// rz:rc:{<hex U>}:gen.
func AppendCacheGeneration(dst []byte, uri statestore.Digest) []byte {
	dst = append(dst, PrefixCache...)
	dst = appendTag(dst, uri)
	return append(dst, SuffixGeneration...)
}

// AppendCachePartition appends the partition hash key of k:
// rz:rc:{<hex U>:<hex P>}.
func AppendCachePartition(dst []byte, k statestore.CacheKey) []byte {
	dst = append(dst, PrefixCache...)
	dst = append(dst, '{')
	dst = AppendHex(dst, k.URI)
	dst = append(dst, ':')
	dst = AppendHex(dst, k.Partition)
	return append(dst, '}')
}

// AppendCacheLease appends the fill and revalidation lease key of k:
// rz:rc:{<hex U>:<hex P>}:lease.
func AppendCacheLease(dst []byte, k statestore.CacheKey) []byte {
	dst = AppendCachePartition(dst, k)
	return append(dst, SuffixLease...)
}

// AppendField appends a variant field name: prefix (FieldGeneration,
// FieldTime or FieldEntry) followed by the hex of the variant digest.
func AppendField(dst []byte, prefix string, variant statestore.Digest) []byte {
	dst = append(dst, prefix...)
	return AppendHex(dst, variant)
}
