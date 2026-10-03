// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sfv

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

// Fuzz targets for spec 05 test plan item 28 ("sfv serializer: output
// reparses per RFC 9651 grammar"); oracle: no panic, no hang, and the
// round-trip properties below.

// gen builds structured values from fuzz bytes; an exhausted input yields
// zeros.
type gen struct{ b []byte }

func (g *gen) byte() byte {
	if len(g.b) == 0 {
		return 0
	}
	c := g.b[0]
	g.b = g.b[1:]
	return c
}

func (g *gen) bytes(maxLen int) []byte {
	n := min(int(g.byte())%(maxLen+1), len(g.b))
	out := g.b[:n]
	g.b = g.b[n:]
	return out
}

func (g *gen) int64() int64 {
	var buf [8]byte
	copy(buf[:], g.bytes(8))
	n := int64(binary.LittleEndian.Uint64(buf[:])) //nolint:gosec // G115: fuzz input, any bit pattern is wanted.
	if g.byte()%2 == 0 {
		n %= 2_000_000_000_000_000 // mostly near the valid range
	}
	return n
}

func (g *gen) bare() BareItem {
	switch g.byte() % 10 {
	case 0:
		return Integer(g.int64())
	case 1:
		f := math.Float64frombits(uint64(g.int64())) //nolint:gosec // G115: fuzz input, any bit pattern is wanted.
		if g.byte()%2 == 0 {
			f = float64(g.int64()%1_000_000_000_000_000) / 1000
		}
		return Decimal(f)
	case 2:
		return String(string(g.bytes(12)))
	case 3:
		return Token(string(g.bytes(12)))
	case 4:
		return ByteSequence(g.bytes(12))
	case 5:
		return Boolean(g.byte()%2 == 0)
	case 6:
		return Date(g.int64())
	case 7:
		return DisplayString(string(g.bytes(12)))
	case 8:
		return BareItem{}
	default:
		return String(string(g.bytes(4)))
	}
}

func (g *gen) params() []Param {
	var ps []Param
	for range g.byte() % 4 {
		ps = append(ps, Param{Key: string(g.bytes(4)), Value: g.bare()})
	}
	return ps
}

func (g *gen) list() List {
	var l List
	for range g.byte() % 5 {
		if g.byte()%3 == 0 {
			var items []Item
			for range g.byte() % 4 {
				items = append(items, Item{Value: g.bare(), Params: g.params()})
			}
			l = append(l, InnerListMember(items, g.params()...))
			continue
		}
		l = append(l, ItemMember(g.bare(), g.params()...))
	}
	return l
}

// sameBare reports whether a parsed item equals the serialized one;
// decimals compare after the serializer's rounding.
func sameBare(t *testing.T, want, got BareItem) bool {
	t.Helper()
	if want.kind != KindDecimal {
		return want == got
	}
	a, errA := AppendBareItem(nil, want)
	b, errB := AppendBareItem(nil, got)
	return got.kind == KindDecimal && errA == nil && errB == nil && string(a) == string(b)
}

func sameParams(t *testing.T, want, got []Param) bool {
	t.Helper()
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i].Key != got[i].Key || !sameBare(t, want[i].Value, got[i].Value) {
			return false
		}
	}
	return true
}

func sameList(t *testing.T, want, got List) bool {
	t.Helper()
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		w, g := &want[i], &got[i]
		if w.Inner != g.Inner || !sameParams(t, w.Params, g.Params) {
			return false
		}
		if !w.Inner {
			if !sameBare(t, w.Value, g.Value) {
				return false
			}
			continue
		}
		if len(w.Items) != len(g.Items) {
			return false
		}
		for j := range w.Items {
			if !sameBare(t, w.Items[j].Value, g.Items[j].Value) || !sameParams(t, w.Items[j].Params, g.Items[j].Params) {
				return false
			}
		}
	}
	return true
}

// FuzzSerializeList: whatever the serializer accepts reparses to the same
// List and serializes to the same bytes; whatever it refuses leaves dst
// unchanged.
func FuzzSerializeList(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{3, 1, 2, 5, 'h', 'e', 'l', 'l', 'o', 1, 1, 'q', 0, 8})
	f.Add([]byte{4, 0, 3, 7, 3, 0xc3, 0xa9, 'x', 2, 1, 'a', 5})
	f.Fuzz(func(t *testing.T, data []byte) {
		g := &gen{b: data}
		l := g.list()
		out, err := AppendList([]byte("pre"), l)
		if err != nil {
			if !errors.Is(err, ErrSerialize) || string(out) != "pre" {
				t.Fatalf("failure = %q, %v", out, err)
			}
			return
		}
		out = out[len("pre"):]
		back, err := ParseList(string(out))
		if err != nil {
			t.Fatalf("serialized %q does not reparse: %v", out, err)
		}
		if !sameList(t, l, back) {
			t.Fatalf("%q reparses to %#v, want %#v", out, back, l)
		}
		again, err := AppendList(nil, back)
		if err != nil || string(again) != string(out) {
			t.Fatalf("reserialized %q = %q, %v", out, again, err)
		}
	})
}

// FuzzParseList: parsing never panics; whatever parses serializes, and the
// serialization is a fixed point of parse then serialize.
func FuzzParseList(f *testing.F) {
	for _, s := range []string{
		"", "sugar, tea, rum", `("foo" "bar"), ("baz"), ()`, `abc;a=1;b=2; cde_456, (ghi;jk=4 l);q="9";r=w`,
		"4.5, -0.0, 999999999999.999", `"a\"b\\c"`, ":aGk:", "?1;x", "@-1", `%"f%c3%bc"`,
		`"ratelimit-global";q=1000;w=1, "ratelimit-gold.1";q=100;w=1`, "a;x=1;x=2", "1.", "(a", "%\"%C3\"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		l, err := ParseList(in)
		if err != nil {
			if !errors.Is(err, ErrParse) {
				t.Fatalf("ParseList(%q) error %v does not wrap ErrParse", in, err)
			}
			return
		}
		out, err := AppendList(nil, l)
		if err != nil {
			t.Fatalf("ParseList(%q) = %#v, which does not serialize: %v", in, l, err)
		}
		back, err := ParseList(string(out))
		if err != nil || !sameList(t, l, back) {
			t.Fatalf("%q -> %q reparses to %#v, %v", in, out, back, err)
		}
		again, _ := AppendList(nil, back)
		if string(again) != string(out) {
			t.Fatalf("%q -> %q -> %q is not a fixed point", in, out, again)
		}
		if _, err := ParseItem(in); err == nil && len(l) != 1 {
			t.Fatalf("%q parses as an Item and as a %d-member List", in, len(l))
		}
	})
}

// FuzzRateLimit: the RateLimit helpers either fail leaving dst unchanged
// or append one member that parses back to the name and both parameters
// (05 req 67, 68).
func FuzzRateLimit(f *testing.F) {
	f.Add("ratelimit-gold.1", int64(100), int64(1), false)
	f.Add("café", int64(-1), int64(MaxInteger+1), true)
	f.Add(`q"\`, int64(0), int64(0), true)
	f.Fuzz(func(t *testing.T, name string, a, b int64, policy bool) {
		fn, k1, k2 := AppendRateLimit, "r", "t"
		if policy {
			fn, k1, k2 = AppendRateLimitPolicy, "q", "w"
		}
		prefix := []byte(`"x";q=1;w=1`)
		out, err := fn(prefix, name, a, b)
		if err != nil {
			if !errors.Is(err, ErrSerialize) || string(out) != `"x";q=1;w=1` {
				t.Fatalf("failure = %q, %v", out, err)
			}
			return
		}
		l, err := ParseList(string(out))
		if err != nil || len(l) != 2 {
			t.Fatalf("%q reparses to %#v, %v", out, l, err)
		}
		want := ItemMember(String(name), Param{Key: k1, Value: Integer(a)}, Param{Key: k2, Value: Integer(b)})
		if !sameList(t, List{want}, l[1:]) {
			t.Fatalf("%q member = %#v, want %#v", out, l[1], want)
		}
	})
}
