// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Tests for spec 08 req 68 and resolution R-49 (entry MAC over
// "rz1" ‖ key ‖ 0x00 ‖ field ‖ 0x00 ‖ entry) and the MAC row of the 08
// section 6.1 unit table: tampered entry, missing tag and wrong key are
// misses; without a key entries are stored and accepted untagged.

func macKey(b byte) []byte { return bytes.Repeat([]byte{b}, MinMACKeySize) }

func macFixture() (key, field []byte) {
	ck := statestore.CacheKey{URI: statestore.DigestOf("u"), Partition: statestore.DigestOf("p")}
	return AppendCachePartition(nil, ck), AppendField(nil, FieldEntry, statestore.DigestOf("v"))
}

func TestNewMAC(t *testing.T) {
	if m, err := NewMAC(nil); m != nil || err != nil {
		t.Fatalf("NewMAC(nil) = %v, %v; want nil, nil", m, err)
	}
	if m, err := NewMAC(make([]byte, MinMACKeySize-1)); m != nil || !errors.Is(err, ErrMACKeyTooShort) {
		t.Fatalf("NewMAC(31 bytes) = %v, %v; want ErrMACKeyTooShort", m, err)
	}
	m, err := NewMAC(macKey(1))
	if err != nil || !m.Enabled() {
		t.Fatalf("NewMAC(32 bytes) = %v, %v", m, err)
	}
	var none *MAC
	if none.Enabled() {
		t.Fatal("nil MAC reports enabled")
	}
	// Every method is safe on a nil MAC: AppendTag appends nothing.
	dst := []byte("prefix")
	if got := none.AppendTag(dst, []byte("k"), []byte("f"), []byte("entry")); string(got) != "prefix" {
		t.Fatalf("nil MAC AppendTag = %q, want dst unchanged", got)
	}
}

func TestMACLayout(t *testing.T) {
	// The tag is HMAC-SHA-256 over exactly "rz1" ‖ key ‖ 0 ‖ field ‖ 0 ‖
	// entry, appended after the entry.
	k := macKey(7)
	m, err := NewMAC(k)
	if err != nil {
		t.Fatal(err)
	}
	key, field := macFixture()
	entry := []byte("entry bytes")
	h := hmac.New(sha256.New, k)
	h.Write([]byte("rz1"))
	h.Write(key)
	h.Write([]byte{0})
	h.Write(field)
	h.Write([]byte{0})
	h.Write(entry)
	want := append(bytes.Clone(entry), h.Sum(nil)...)
	got := m.Seal(nil, key, field, entry)
	if !bytes.Equal(got, want) {
		t.Fatalf("Seal = %x, want %x", got, want)
	}
	if len(got) != len(entry)+TagSize {
		t.Fatalf("stored length %d, want %d", len(got), len(entry)+TagSize)
	}
}

func TestMACOpen(t *testing.T) {
	m, err := NewMAC(macKey(1))
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewMAC(macKey(2))
	if err != nil {
		t.Fatal(err)
	}
	key, field := macFixture()
	entry := []byte("cached response")
	stored := m.Seal(nil, key, field, entry)
	otherKey := AppendCachePartition(nil, statestore.CacheKey{URI: statestore.DigestOf("u2")})
	otherField := AppendField(nil, FieldEntry, statestore.DigestOf("v2"))

	flip := func(i int) []byte {
		b := bytes.Clone(stored)
		b[i] ^= 1
		return b
	}
	cases := []struct {
		name         string
		m            *MAC
		key, field   []byte
		stored, want []byte
		ok           bool
	}{
		{"intact", m, key, field, stored, entry, true},
		{"tampered entry", m, key, field, flip(0), nil, false},
		{"tampered tag", m, key, field, flip(len(stored) - 1), nil, false},
		{"missing tag", m, key, field, bytes.Clone(entry), nil, false},
		{"shorter than a tag", m, key, field, []byte("x"), nil, false},
		{"empty", m, key, field, nil, nil, false},
		{"wrong key", other, key, field, stored, nil, false},
		{"moved to another partition", m, otherKey, field, stored, nil, false},
		{"moved to another field", m, key, otherField, stored, nil, false},
		{"no key accepts tagged bytes as the entry", nil, key, field, stored, stored, true},
		{"no key accepts untagged", nil, key, field, entry, entry, true},
	}
	for _, c := range cases {
		got, ok := c.m.Open(c.key, c.field, c.stored)
		if ok != c.ok || !bytes.Equal(got, c.want) {
			t.Errorf("%s: Open = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
	// Without a key Seal stores the entry untagged.
	var none *MAC
	if got := none.Seal(nil, key, field, entry); !bytes.Equal(got, entry) {
		t.Fatalf("nil MAC Seal = %q, want the entry", got)
	}
	// An empty entry still carries a tag.
	empty := m.Seal(nil, key, field, nil)
	if got, ok := m.Open(key, field, empty); !ok || len(got) != 0 {
		t.Fatalf("empty entry: Open = %q, %v", got, ok)
	}
}

func TestMACConcurrent(t *testing.T) {
	m, err := NewMAC(macKey(3))
	if err != nil {
		t.Fatal(err)
	}
	key, field := macFixture()
	var wg sync.WaitGroup
	for g := range byte(16) {
		wg.Go(func() {
			entry := bytes.Repeat([]byte{g}, 100+int(g))
			for range 200 {
				stored := m.Seal(nil, key, field, entry)
				got, ok := m.Open(key, field, stored)
				if !ok || !bytes.Equal(got, entry) {
					t.Errorf("goroutine %d: round trip failed", g)
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestMACDoesNotAllocate(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items under the race detector")
	}
	m, err := NewMAC(macKey(4))
	if err != nil {
		t.Fatal(err)
	}
	key, field := macFixture()
	entry := bytes.Repeat([]byte{'x'}, 1024)
	buf := make([]byte, 0, 2048)
	if n := testing.AllocsPerRun(100, func() { buf = m.Seal(buf[:0], key, field, entry) }); n != 0 {
		t.Fatalf("Seal allocates %v times", n)
	}
}

func BenchmarkMACOpen(b *testing.B) {
	m, err := NewMAC(macKey(5))
	if err != nil {
		b.Fatal(err)
	}
	key, field := macFixture()
	stored := m.Seal(nil, key, field, bytes.Repeat([]byte{'x'}, 16<<10))
	b.ReportAllocs()
	b.SetBytes(int64(len(stored)))
	for b.Loop() {
		if _, ok := m.Open(key, field, stored); !ok {
			b.Fatal("open failed")
		}
	}
}
