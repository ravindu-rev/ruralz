// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package ulid implements Universally Unique Lexicographically Sortable
// Identifiers on crypto/rand (OQ-tech-stack-and-libraries-15, decided: own
// code, no library). A ULID is 128 bits: a 48-bit big-endian Unix time in
// milliseconds followed by 80 random bits, written as 26 characters of
// Crockford base32. The Node identity node.id (spec 04 requirement 6) and,
// from M2, Ruralz Control's Rollout IDs are ULIDs.
//
// The text form is uppercase; Parse accepts either case and rejects the
// letters I, L, O and U, which Crockford's decoder would otherwise map to
// digits, so every accepted string has exactly one canonical spelling.
package ulid

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// ULID is a 128-bit identifier: bytes 0 to 5 hold the big-endian Unix time
// in milliseconds, bytes 6 to 15 the random part. The zero value is the
// ULID "00000000000000000000000000". Byte order equals lexical order of the
// text form, so Compare and string comparison agree.
type ULID [16]byte

// EncodedLen is the length of the text form.
const EncodedLen = 26

// MaxTime is the largest encodable timestamp in Unix milliseconds
// (2^48 - 1, in the year 10889).
const MaxTime = 1<<48 - 1

// alphabet is Crockford's base32 alphabet in value order.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Errors returned by New and Parse.
var (
	// ErrTimeRange reports a time before the Unix epoch or after MaxTime.
	ErrTimeRange = errors.New("ulid: time outside the 48-bit millisecond range")
	// ErrEntropy reports a failed read of the random part.
	ErrEntropy = errors.New("ulid: reading entropy failed")
	// ErrLength reports a text form that is not 26 characters long.
	ErrLength = errors.New("ulid: text form is not 26 characters long")
	// ErrInvalidChar reports a character outside the Crockford alphabet,
	// including I, L, O and U.
	ErrInvalidChar = errors.New("ulid: invalid character")
	// ErrOverflow reports a text form above the 128-bit range (first
	// character above 7).
	ErrOverflow = errors.New("ulid: value overflows 128 bits")
)

// New returns a ULID for now with 80 bits read from entropy. A nil entropy
// selects crypto/rand.Reader, the production source (spec 04 requirement
// 6); tests pass a fixed reader for deterministic output. Two ULIDs made
// in the same millisecond are not ordered among themselves.
func New(now time.Time, entropy io.Reader) (ULID, error) {
	var u ULID
	ms := now.UnixMilli()
	if ms < 0 || ms > MaxTime {
		return ULID{}, fmt.Errorf("%w: %s", ErrTimeRange, now.UTC().Format(time.RFC3339Nano))
	}
	putTime(&u, uint64(ms))
	if entropy == nil {
		entropy = rand.Reader
	}
	if _, err := io.ReadFull(entropy, u[6:]); err != nil {
		return ULID{}, fmt.Errorf("%w: %w", ErrEntropy, err)
	}
	return u, nil
}

// putTime writes the low 48 bits of ms big-endian into u[0:6].
func putTime(u *ULID, ms uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], ms)
	copy(u[:6], b[2:])
}

// Timestamp returns the Unix time in milliseconds.
func (u ULID) Timestamp() uint64 {
	return uint64(u[0])<<40 | uint64(u[1])<<32 | uint64(u[2])<<24 |
		uint64(u[3])<<16 | uint64(u[4])<<8 | uint64(u[5])
}

// Time returns the timestamp as a time.Time in UTC with millisecond
// precision.
func (u ULID) Time() time.Time {
	return time.UnixMilli(int64(u.Timestamp())).UTC() //nolint:gosec // G115: Timestamp is at most 2^48-1.
}

// IsZero reports whether u is the zero ULID.
func (u ULID) IsZero() bool { return u == ULID{} }

// Compare returns -1, 0 or +1 as u sorts before, equal to or after v; the
// order is the lexical order of the text forms.
func (u ULID) Compare(v ULID) int {
	for i := range u {
		switch {
		case u[i] < v[i]:
			return -1
		case u[i] > v[i]:
			return 1
		}
	}
	return 0
}

// String returns the 26-character uppercase Crockford base32 form.
func (u ULID) String() string {
	var b [EncodedLen]byte
	u.encode(&b)
	return string(b[:])
}

// AppendText appends the text form to b (encoding.TextAppender).
func (u ULID) AppendText(b []byte) ([]byte, error) {
	var e [EncodedLen]byte
	u.encode(&e)
	return append(b, e[:]...), nil
}

// MarshalText returns the text form (encoding.TextMarshaler), so a ULID
// encodes as a JSON string.
func (u ULID) MarshalText() ([]byte, error) {
	return u.AppendText(make([]byte, 0, EncodedLen))
}

// UnmarshalText parses the text form with Parse (encoding.TextUnmarshaler).
func (u *ULID) UnmarshalText(b []byte) error {
	v, err := parse(b)
	if err != nil {
		return err
	}
	*u = v
	return nil
}

// encode writes the 130-bit big-endian base32 form of u (two leading zero
// bits) into b, five bits per character from the right.
func (u ULID) encode(b *[EncodedLen]byte) {
	hi := binary.BigEndian.Uint64(u[:8])
	lo := binary.BigEndian.Uint64(u[8:])
	for i := EncodedLen - 1; i >= 0; i-- {
		b[i] = alphabet[lo&0x1f]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
}

// Parse decodes a 26-character text form. Letters match in either case;
// I, L, O and U are rejected, and so is a first character above 7, whose
// value does not fit in 128 bits (spec 04 requirement 6).
func Parse(s string) (ULID, error) {
	return parse(s)
}

// parse decodes s; it is generic so UnmarshalText decodes without copying.
func parse[T string | []byte](s T) (ULID, error) {
	if len(s) != EncodedLen {
		return ULID{}, fmt.Errorf("%w: got %d", ErrLength, len(s))
	}
	var hi, lo uint64
	for i := range EncodedLen {
		v := decodeChar(s[i])
		if v < 0 {
			return ULID{}, fmt.Errorf("%w %q at offset %d", ErrInvalidChar, rune(s[i]), i)
		}
		if i == 0 && v > 7 {
			return ULID{}, fmt.Errorf("%w: first character %q", ErrOverflow, rune(s[0]))
		}
		hi = hi<<5 | lo>>59
		lo = lo<<5 | uint64(v)
	}
	var u ULID
	binary.BigEndian.PutUint64(u[:8], hi)
	binary.BigEndian.PutUint64(u[8:], lo)
	return u, nil
}

// decodeChar returns the value of one Crockford character (either case),
// or -1 for any other byte, including I, L, O and U.
func decodeChar(c byte) int {
	if c >= 'a' && c <= 'z' {
		c -= 'a' - 'A'
	}
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'H':
		return int(c-'A') + 10
	case c == 'J' || c == 'K':
		return int(c-'J') + 18
	case c == 'M' || c == 'N':
		return int(c-'M') + 20
	case c >= 'P' && c <= 'T':
		return int(c-'P') + 22
	case c >= 'V' && c <= 'Z':
		return int(c-'V') + 27
	default:
		return -1
	}
}
