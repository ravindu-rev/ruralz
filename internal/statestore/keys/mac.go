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

// Entry MAC constants (spec 08 req 68, resolution R-49).
const (
	// TagSize is the length of an entry tag (HMAC-SHA-256).
	TagSize = sha256.Size
	// MinMACKeySize is the shortest accepted MAC key
	// (RURALZ_STATE_STORE_MAC_KEY_FILE holds at least 32 bytes).
	MinMACKeySize = 32
	// MACDomain opens every tagged message, naming the layout version.
	MACDomain = "rz1"
)

// ErrMACKeyTooShort is returned by NewMAC for a key under MinMACKeySize.
var ErrMACKeyTooShort = errors.New("statestore: the entry MAC key is shorter than 32 bytes")

// MAC tags and checks the opaque entries the State Store keeps for others
// (M1: Response Cache entries): HMAC-SHA-256 over
// "rz1" ‖ full key ‖ 0x00 ‖ field name ‖ 0x00 ‖ entry. A stored value is
// the entry followed by its TagSize-byte tag. Counters are not tagged:
// Lua has no HMAC and a deletion cannot be prevented anyway.
//
// A nil *MAC is valid and means no key is configured: Seal stores entries
// untagged, AppendTag appends nothing and Open accepts every stored value
// as its entry. A MAC is safe for concurrent use; its HMAC states are
// pooled, so tagging allocates nothing once warm.
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

// Seal appends the stored form of entry under key and field: the entry,
// then its tag when m is not nil. dst must not overlap entry.
func (m *MAC) Seal(dst, key, field, entry []byte) []byte {
	dst = append(dst, entry...)
	if m == nil {
		return dst
	}
	return m.AppendTag(dst, key, field, entry)
}

// Open checks a stored value read from key and field and returns its
// entry, a sub-slice of stored. A missing or wrong tag gives ok false: the
// caller treats it as a miss, never as an error (spec 08 req 68). A nil m
// returns stored unchanged.
func (m *MAC) Open(key, field, stored []byte) (entry []byte, ok bool) {
	if m == nil {
		return stored, true
	}
	if len(stored) < TagSize {
		return nil, false
	}
	body, tag := stored[:len(stored)-TagSize], stored[len(stored)-TagSize:]
	var buf [TagSize]byte
	want := m.AppendTag(buf[:0], key, field, body)
	if !hmac.Equal(want, tag) {
		return nil, false
	}
	return body, true
}
