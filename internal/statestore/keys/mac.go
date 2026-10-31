// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"hash"
	"sync"
)

// Entry MAC constants.
const (
	// TagSize is the length of an entry tag (HMAC-SHA-256).
	TagSize = sha256.Size
	// MinMACKeySize is the shortest accepted MAC key
	// (RURALZ_STATE_STORE_MAC_KEY_FILE holds at least 32 bytes).
	MinMACKeySize = 32
	// MACDomain opens every tagged message, naming the layout version.
	MACDomain = "rz1"
)

// Stored entry marks: the first byte of every stored entry records
// whether a tag follows it, so a Node without the MAC key reads a tagged
// entry as a miss and a Node with the key reads an untagged one as a miss
// (docs/architecture/08-security-and-identity.md "Secrets" rule 9). Any
// other first byte is a miss for every Node.
const (
	// MarkUntagged opens an entry stored by a Node without a MAC key: the
	// entry follows to the end of the stored value.
	MarkUntagged byte = 0
	// MarkTagged opens an entry stored by a Node with a MAC key: the entry
	// follows, then its TagSize-byte tag.
	MarkTagged byte = 1
)

// ErrMACKeyTooShort is returned by NewMAC for a key under MinMACKeySize.
var ErrMACKeyTooShort = errors.New("statestore: the entry MAC key is shorter than 32 bytes")

// MAC tags and checks the opaque entries the State Store keeps for others
// (M1: Response Cache entries): HMAC-SHA-256 over
// "rz1" ‖ full key ‖ 0x00 ‖ field name ‖ 0x00 ‖ entry. A stored value is
// a mark byte, the entry and, when the mark is MarkTagged, the entry's
// TagSize-byte tag. Counters are not tagged: Lua has no HMAC and a
// deletion cannot be prevented anyway.
//
// A nil *MAC is valid and means no key is configured: Seal stores entries
// marked untagged, AppendTag appends nothing, and Open accepts only
// entries marked untagged, so a key-less Node never serves an entry a
// keyed Node stored, nor the reverse. A MAC is safe for concurrent use;
// its HMAC states are pooled, so tagging allocates nothing once warm.
type MAC struct {
	pool   sync.Pool
	domain []byte
	sep    []byte
}

// NewMAC returns the MAC for key (Deps.MACKey), or nil for an empty key.
// It copies key.
func NewMAC(key []byte) (*MAC, error) {
	if len(key) == 0 {
		return nil, nil //nolint:nilnil // a nil *MAC is the documented "no key" value
	}
	if len(key) < MinMACKeySize {
		return nil, ErrMACKeyTooShort
	}
	k := bytes.Clone(key)
	m := &MAC{domain: []byte(MACDomain), sep: []byte{0}}
	m.pool.New = func() any { return hmac.New(sha256.New, k) }
	return m, nil
}

// Enabled reports whether entries carry tags.
func (m *MAC) Enabled() bool { return m != nil }

// AppendTag appends the tag of entry stored under key and field. A nil m
// has no key and appends nothing: entries then carry no tag.
func (m *MAC) AppendTag(dst, key, field, entry []byte) []byte {
	if m == nil {
		return dst
	}
	h, _ := m.pool.Get().(hash.Hash)
	h.Reset()
	h.Write(m.domain)
	h.Write(key)
	h.Write(m.sep)
	h.Write(field)
	h.Write(m.sep)
	h.Write(entry)
	dst = h.Sum(dst)
	m.pool.Put(h)
	return dst
}

// SealedLen returns the length of the stored form of an entry of n bytes.
func (m *MAC) SealedLen(n int) int {
	if m == nil {
		return 1 + n
	}
	return 1 + n + TagSize
}

// Seal appends the stored form of entry under key and field: the mark,
// the entry and, when m is not nil, its tag. dst must not overlap entry.
func (m *MAC) Seal(dst, key, field, entry []byte) []byte {
	if m == nil {
		dst = append(dst, MarkUntagged)
		return append(dst, entry...)
	}
	dst = append(dst, MarkTagged)
	dst = append(dst, entry...)
	return m.AppendTag(dst, key, field, entry)
}

// Open checks a stored value read from key and field and returns its
// entry, a sub-slice of stored (see OpenPayload). An empty value is
// refused.
func (m *MAC) Open(key, field, stored []byte) (entry []byte, ok bool) {
	if len(stored) == 0 {
		return nil, false
	}
	return m.OpenPayload(key, field, stored[0], stored[1:])
}

// OpenPayload checks a stored value read from key and field given as its
// mark and its payload, everything after the mark, and returns its entry,
// a prefix of payload: a driver copies the payload alone into its
// caller's buffer, so the entry starts where the buffer does. With a key,
// only a value marked tagged whose tag matches is accepted; without one
// (a nil m), only a value marked untagged. Anything else gives ok false,
// which the caller treats as a miss, never as an error.
func (m *MAC) OpenPayload(key, field []byte, mark byte, payload []byte) (entry []byte, ok bool) {
	if m == nil {
		if mark != MarkUntagged {
			return nil, false
		}
		return payload, true
	}
	if mark != MarkTagged || len(payload) < TagSize {
		return nil, false
	}
	body, tag := payload[:len(payload)-TagSize], payload[len(payload)-TagSize:]
	var buf [TagSize]byte
	want := m.AppendTag(buf[:0], key, field, body)
	if !hmac.Equal(want, tag) {
		return nil, false
	}
	return body, true
}

// Refusal returns the warning a driver logs, rate-limited, for a stored
// value with this mark that Open refused: a key-less Node reading a value
// marked tagged shares its State Store with keyed Nodes, whose entries it
// never serves; any other refusal is a failed MAC check. Neither names
// the entry.
func (m *MAC) Refusal(mark byte) string {
	if m == nil && mark == MarkTagged {
		return "state store cache entry carries a MAC tag but this Node has no MAC key; served as a miss"
	}
	return "state store cache entry failed its MAC check; served as a miss"
}
