// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Tests for the entry MAC over "rz1" ‖ key ‖ 0x00 ‖ field ‖ 0x00 ‖ entry
// and the tagged-entry mark that opens every stored value: tampered
// entry, missing tag and wrong key are misses; without a key entries are
// stored and accepted marked untagged, and an entry marked for the other
// key setting is a miss either way.

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
	// A keyed Node stores the mark 1, the entry, then the tag:
	// HMAC-SHA-256 over exactly "rz1" ‖ key ‖ 0 ‖ field ‖ 0 ‖ entry.
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
	want := append(append([]byte{MarkTagged}, entry...), h.Sum(nil)...)
	got := m.Seal(nil, key, field, entry)
	if !bytes.Equal(got, want) {
		t.Fatalf("Seal = %x, want %x", got, want)
	}
	if len(got) != 1+len(entry)+TagSize || m.SealedLen(len(entry)) != len(got) {
		t.Fatalf("stored length %d, SealedLen %d; want %d", len(got), m.SealedLen(len(entry)), 1+len(entry)+TagSize)
	}
}

func TestMACLayoutUntagged(t *testing.T) {
	// A key-less Node stores the mark 0, then the entry, without a tag.
	var none *MAC
	key, field := macFixture()
	entry := []byte("entry bytes")
	want := append([]byte{MarkUntagged}, entry...)
	if got := none.Seal(nil, key, field, entry); !bytes.Equal(got, want) {
		t.Fatalf("nil MAC Seal = %x, want %x", got, want)
	}
	if n := none.SealedLen(len(entry)); n != len(want) {
		t.Fatalf("nil MAC SealedLen = %d, want %d", n, len(want))
	}
	if MarkUntagged == MarkTagged {
		t.Fatal("the two marks are equal")
	}
}

func TestMACMixedKeySettings(t *testing.T) {
	// Keyed and key-less Nodes on one State Store never serve each
	// other's entries (Security, "Secrets" rule 9): the key-less Node
	// reads a tagged entry as a miss, the keyed Node an untagged one.
	m, err := NewMAC(macKey(8))
	if err != nil {
		t.Fatal(err)
	}
	var none *MAC
	key, field := macFixture()
	entry := []byte("cached response")
	tagged, untagged := m.Seal(nil, key, field, entry), none.Seal(nil, key, field, entry)
	if got, ok := none.Open(key, field, tagged); ok {
		t.Errorf("key-less Open(tagged) = %q, true; want a miss", got)
	}
	if got, ok := m.Open(key, field, untagged); ok {
		t.Errorf("keyed Open(untagged) = %q, true; want a miss", got)
	}
	// A tagged entry relabeled untagged by a compromised State Store
	// reaches a key-less Node as untagged, which it cannot tell apart;
	// a keyed Node still refuses it.
	relabeled := append([]byte{MarkUntagged}, tagged[1:]...)
	if _, ok := m.Open(key, field, relabeled); ok {
		t.Error("keyed Open(tagged entry marked untagged) accepted")
	}
	if got := none.Refusal(tagged[0]); !strings.Contains(got, "no MAC key") {
		t.Errorf("key-less Refusal(MarkTagged) = %q, want the missing key named", got)
	}
	for _, c := range []struct {
		name string
		m    *MAC
		mark byte
	}{{"keyed, untagged", m, untagged[0]}, {"keyed, tagged", m, MarkTagged}, {"key-less, unknown mark", none, 2}} {
		if got := c.m.Refusal(c.mark); !strings.Contains(got, "failed its MAC check") {
			t.Errorf("%s: Refusal = %q, want a failed MAC check", c.name, got)
		}
	}
}

func TestMACOpenPayload(t *testing.T) {
	// A driver copies only the payload after the mark into its caller's
	// buffer: the entry OpenPayload returns starts where the payload does.
	m, err := NewMAC(macKey(9))
	if err != nil {
		t.Fatal(err)
	}
	var none *MAC
	key, field := macFixture()
	entry := []byte("cached response")
	for _, c := range []struct {
		name string
		m    *MAC
	}{{"keyed", m}, {"key-less", none}} {
		stored := c.m.Seal(nil, key, field, entry)
		payload := bytes.Clone(stored[1:])
		got, ok := c.m.OpenPayload(key, field, stored[0], payload)
		if !ok || !bytes.Equal(got, entry) || &got[0] != &payload[0] {
			t.Errorf("%s: OpenPayload = %q, %v; want the entry at the payload's start", c.name, got, ok)
		}
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
	var none *MAC
	untagged := none.Seal(nil, key, field, entry)
	otherKey := AppendCachePartition(nil, statestore.CacheKey{URI: statestore.DigestOf("u2")})
	otherField := AppendField(nil, FieldEntry, statestore.DigestOf("v2"))

	flip := func(b []byte, i int) []byte {
		b = bytes.Clone(b)
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
		{"tampered mark", m, key, field, flip(stored, 0), nil, false},
		{"tampered entry", m, key, field, flip(stored, 1), nil, false},
		{"tampered tag", m, key, field, flip(stored, len(stored)-1), nil, false},
		{"missing tag", m, key, field, append([]byte{MarkTagged}, entry...), nil, false},
		{"untagged entry", m, key, field, untagged, nil, false},
		{"shorter than a tag", m, key, field, []byte{MarkTagged, 'x'}, nil, false},
		{"mark only", m, key, field, []byte{MarkTagged}, nil, false},
		{"empty", m, key, field, nil, nil, false},
		{"wrong key", other, key, field, stored, nil, false},
		{"moved to another partition", m, otherKey, field, stored, nil, false},
		{"moved to another field", m, key, otherField, stored, nil, false},
		{"no key refuses a tagged entry", nil, key, field, stored, nil, false},
		{"no key accepts untagged", nil, key, field, untagged, entry, true},
		{"no key accepts an empty untagged entry", nil, key, field, []byte{MarkUntagged}, []byte{}, true},
		{"no key refuses an unknown mark", nil, key, field, append([]byte{2}, entry...), nil, false},
		{"no key refuses empty", nil, key, field, nil, nil, false},
	}
	for _, c := range cases {
		got, ok := c.m.Open(c.key, c.field, c.stored)
		if ok != c.ok || !bytes.Equal(got, c.want) {
			t.Errorf("%s: Open = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
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
