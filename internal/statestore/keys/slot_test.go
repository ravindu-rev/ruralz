// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Tests for spec 08 req 49 (CRC16-XMODEM hash slots of hash tags), req 26
// (keys of one command share a slot) and the "Slot" row of the 08 section
// 6.1 unit table, plus the Slot fuzz target of 08 section 6.3.

// refCRC16 is a bitwise CRC16-XMODEM, independent of the table.
func refCRC16(b []byte) uint16 {
	var crc uint16
	for _, c := range b {
		crc ^= uint16(c) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// refSlot follows the Redis Cluster specification's keyHashSlot text.
func refSlot(key string) uint16 {
	s := -1
	for i := range len(key) {
		if key[i] == '{' {
			s = i
			break
		}
	}
	if s == -1 {
		return refCRC16([]byte(key)) & 16383
	}
	e := -1
	for i := s + 1; i < len(key); i++ {
		if key[i] == '}' {
			e = i
			break
		}
	}
	if e == -1 || e == s+1 {
		return refCRC16([]byte(key)) & 16383
	}
	return refCRC16([]byte(key[s+1:e])) & 16383
}

func TestCRCTable(t *testing.T) {
	for i := range 256 {
		if got, want := crcTable[i], refCRC16([]byte{byte(i)}); got != want {
			t.Fatalf("crcTable[%d] = %#04x, want %#04x", i, got, want)
		}
	}
}

func TestSlot(t *testing.T) {
	// 08 section 6.1 "Slot" row.
	if got := CRC16([]byte("123456789")); got != 0x31C3 {
		t.Fatalf("CRC16(123456789) = %#04x, want 0x31C3", got)
	}
	if got := Slot([]byte("123456789")); got != 12739 {
		t.Fatalf("Slot(123456789) = %d, want 12739", got)
	}
	cases := []struct {
		key string
		tag string
	}{
		{"{user1000}.following", "user1000"},
		{"{user1000}.followers", "user1000"},
		{"foo{}{bar}", "foo{}{bar}"},
		{"foo{{bar}}zap", "{bar"},
		{"foo{bar}{zap}", "bar"},
		{"nobraces", "nobraces"},
		{"open{only", "open{only"},
		{"}{x}", "x"},
		{"", ""},
	}
	for _, c := range cases {
		if got := HashTag(c.key); got != c.tag {
			t.Errorf("HashTag(%q) = %q, want %q", c.key, got, c.tag)
		}
		want := CRC16([]byte(c.tag)) % Slots
		if got := Slot([]byte(c.key)); got != want {
			t.Errorf("Slot(%q) = %d, want %d", c.key, got, want)
		}
		if got := SlotString(c.key); got != want {
			t.Errorf("SlotString(%q) = %d, want %d", c.key, got, want)
		}
		if got := refSlot(c.key); got != want {
			t.Errorf("reference slot of %q = %d, want %d", c.key, got, want)
		}
	}
	if Slot([]byte("{user1000}.following")) != Slot([]byte("{user1000}.followers")) {
		t.Fatal("keys with one hash tag landed in different slots")
	}
}

func TestDigestSlots(t *testing.T) {
	// 08 req 49: consumptive keys of one config.key value share one slot,
	// so a ratelimit and a quota keyed alike merge; a partition hash and
	// its lease share one slot (08 req 26).
	d := statestore.DigestOf("user-42")
	want := DigestSlot(d)
	rl := AppendRateLimit(nil, "ratelimit-gold", statestore.GCRALimit{Requests: 100, Window: time.Second}, d)
	rl2 := AppendRateLimit(nil, "ratelimit-gold", statestore.GCRALimit{Requests: 5000, Window: time.Minute}, d)
	qt := AppendQuota(nil, "monthly-requests", 720*time.Hour, time.UnixMilli(1757376000000), d)
	gen := AppendCacheGeneration(nil, d)
	for name, key := range map[string][]byte{"ratelimit": rl, "ratelimit 1m": rl2, "quota": qt, "generation": gen} {
		if got := Slot(key); got != want {
			t.Errorf("%s key %q in slot %d, want DigestSlot %d", name, key, got, want)
		}
	}
	ck := statestore.CacheKey{URI: statestore.DigestOf("u"), Partition: statestore.DigestOf("p")}
	ps := PartitionSlot(ck)
	if got := Slot(AppendCachePartition(nil, ck)); got != ps {
		t.Errorf("partition slot %d, want PartitionSlot %d", got, ps)
	}
	if got := Slot(AppendCacheLease(nil, ck)); got != ps {
		t.Errorf("lease slot %d, want PartitionSlot %d", got, ps)
	}

	// SlotOf and WriteSlot pick the right tag per kind.
	calls := []struct {
		c    statestore.Call
		want int
	}{
		{statestore.Call{Kind: statestore.OpGCRA, GCRA: statestore.GCRA{Digest: d}}, int(want)},
		{statestore.Call{Kind: statestore.OpQuota, Quota: statestore.Quota{Digest: d}}, int(want)},
		{statestore.Call{Kind: statestore.OpCacheGet, Lookup: statestore.CacheLookup{Key: ck}}, -1},
		{statestore.Call{}, -1},
	}
	for i, c := range calls {
		if got := SlotOf(&c.c); got != c.want {
			t.Errorf("SlotOf case %d = %d, want %d", i, got, c.want)
		}
	}
	writes := []struct {
		w    statestore.Write
		want int
	}{
		{statestore.Write{Kind: statestore.OpRefund, Refund: statestore.Refund{Digest: d}}, int(want)},
		{statestore.Write{Kind: statestore.OpCacheSet, Cache: statestore.CacheStore{Key: ck}}, int(ps)},
		{statestore.Write{Kind: statestore.OpCacheSet, Lease: statestore.CacheLease{Key: ck, Mode: statestore.LeaseTake}}, int(ps)},
		{statestore.Write{Kind: statestore.OpCacheInvalidate, Invalidate: ck.URI}, int(DigestSlot(ck.URI))},
		{statestore.Write{Kind: statestore.OpGCRA}, -1},
	}
	for i, c := range writes {
		if got := WriteSlot(&c.w); got != c.want {
			t.Errorf("WriteSlot case %d = %d, want %d", i, got, c.want)
		}
	}
}

func TestDigestSlotDoesNotAllocate(t *testing.T) {
	d := statestore.DigestOf("user-42")
	c := &statestore.Call{Kind: statestore.OpGCRA, GCRA: statestore.GCRA{Digest: d}}
	if n := testing.AllocsPerRun(100, func() { _ = SlotOf(c) }); n != 0 {
		t.Fatalf("SlotOf allocates %v times", n)
	}
	key := []byte("rz:rl:ratelimit-gold:100/1s:{0123}")
	if n := testing.AllocsPerRun(100, func() { _ = Slot(key) }); n != 0 {
		t.Fatalf("Slot allocates %v times", n)
	}
}

func FuzzSlot(f *testing.F) {
	// 08 section 6.3: Slot against a reference implementation.
	for _, s := range []string{"", "123456789", "{user1000}.following", "foo{}{bar}", "foo{{bar}}zap", "foo{bar}{zap}", "{", "}", "{}", "a{b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, key string) {
		want := refSlot(key)
		if got := Slot([]byte(key)); got != want {
			t.Fatalf("Slot(%q) = %d, reference %d", key, got, want)
		}
		if got := SlotString(key); got != want {
			t.Fatalf("SlotString(%q) = %d, reference %d", key, got, want)
		}
		if tag := HashTag(key); !strings.Contains(key, tag) {
			t.Fatalf("HashTag(%q) = %q is not part of the key", key, tag)
		}
	})
}

func BenchmarkDigestSlot(b *testing.B) {
	d := statestore.DigestOf("user-42")
	b.ReportAllocs()
	for b.Loop() {
		_ = DigestSlot(d)
	}
}
