// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

// Tests for the encoder: 07 req 60 (compact, ascending byte order of
// names, minimal escaping without HTML escaping, numbers verbatim, no
// trailing newline, deterministic), 02 req 24 and 25 (RFC 8785 form, no
// HTML escaping), 07 test plan P2 (determinism and round trip).

func mustDecode(t testing.TB, s string) any {
	t.Helper()
	v, _, err := Decode([]byte(s), Options{MaxDepth: MaxDepthLimit})
	if err != nil {
		t.Fatalf("Decode(%q): %v", s, err)
	}
	return v
}

func TestAppendTransformForm(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{"members sorted by bytes", `{"b":1,"a":{"d":[true,null],"c":"x"},"B":2}`, `{"B":2,"a":{"c":"x","d":[true,null]},"b":1}`},
		{"no whitespace", " [ 1 , { \"a\" : [ ] } , \"\" ] ", `[1,{"a":[]},""]`},
		{"numbers verbatim", `[1.50E+02,-0,1e400,12345678901234567890123]`, `[1.50E+02,-0,1e400,12345678901234567890123]`},
		{"no HTML escaping", `"<a href=\"x\">&amp;</a>"`, `"<a href=\"x\">&amp;</a>"`},
		{"minimal escaping", `"\u0000\u001f\b\t\n\f\r\"\\\/\u007f\u2028\u2029\u00e9\ud83d\ude00"`, "\"\\u0000\\u001f\\b\\t\\n\\f\\r\\\"\\\\/\x7f  é\U0001F600\""},
		{"escaped names", `{"\n":1,"\"":2}`, `{"\n":1,"\"":2}`},
		{"scalars", `true`, `true`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := mustDecode(t, c.input)
			got, err := Append(nil, v)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Fatalf("Append = %s, want %s", got, c.want)
			}
			// The map form writes the same bytes.
			m, _, _ := Decode([]byte(c.input), Options{MapObjects: true})
			got2, err := Append([]byte("prefix:"), m)
			if err != nil || string(got2) != "prefix:"+c.want {
				t.Fatalf("Append(map) = %s, %v", got2, err)
			}
		})
	}
}

func TestAppendOrders(t *testing.T) {
	v := mustDecode(t, `{"z":1,"\ufb33":2,"\ud83d\ude00":3,"a":{"y":1,"x":2}}`)
	cases := []struct {
		o    EncodeOptions
		want string
	}{
		{EncodeOptions{Order: OrderBytes}, "{\"a\":{\"x\":2,\"y\":1},\"z\":1,\"דּ\":2,\"\U0001F600\":3}"},
		{EncodeOptions{Order: OrderUTF16}, "{\"a\":{\"x\":2,\"y\":1},\"z\":1,\"\U0001F600\":3,\"דּ\":2}"},
		{EncodeOptions{Order: OrderInsertion}, "{\"z\":1,\"דּ\":2,\"\U0001F600\":3,\"a\":{\"y\":1,\"x\":2}}"},
	}
	for _, c := range cases {
		e := Encoder{Options: c.o}
		got, err := e.Append(nil, v)
		if err != nil || string(got) != c.want {
			t.Fatalf("%+v: Append = %s, %v; want %s", c.o, got, err, c.want)
		}
	}
	// A map has no order of its own: OrderInsertion falls back to bytes.
	e := Encoder{Options: EncodeOptions{Order: OrderInsertion}}
	got, err := e.Append(nil, map[string]any{"b": 1, "a": 2})
	if err != nil || string(got) != `{"a":2,"b":1}` {
		t.Fatalf("map with OrderInsertion = %s, %v", got, err)
	}
}

func TestAppendCanonical(t *testing.T) {
	// RFC 8785 section 3.2.2 example and 02 test plan items 3 and 4.
	in := `{
  "numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001],
  "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",
  "literals": [null, true, false]
}`
	want := `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"` + "€" + `$\u000f\nA'B\"\\\\\"/"}`
	got, err := AppendCanonical(nil, mustDecode(t, in))
	if err != nil || string(got) != want {
		t.Fatalf("AppendCanonical = %s, %v\nwant %s", got, err, want)
	}
	order := `{"\u20ac":"Euro Sign","\r":"Carriage Return","\ufb33":"Hebrew Letter Dalet With Dagesh","1":"One","\ud83d\ude00":"Emoji: Grinning Face","\u0080":"Control","\u00f6":"Latin Small Letter O With Diaeresis"}`
	want = `{"\r":"Carriage Return","1":"One","` + "\u0080" + `":"Control","` + "ö" + `":"Latin Small Letter O With Diaeresis","` + "€" + `":"Euro Sign","` + "\U0001F600" + `":"Emoji: Grinning Face","` + "דּ" + `":"Hebrew Letter Dalet With Dagesh"}`
	for _, mapObjects := range []bool{false, true} {
		v, _, err := Decode([]byte(order), Options{MapObjects: mapObjects})
		if err != nil {
			t.Fatal(err)
		}
		got, err := AppendCanonical(nil, v)
		if err != nil || string(got) != want {
			t.Fatalf("member order = %s, %v\nwant %s", got, err, want)
		}
	}
	// Canonicalization is idempotent: parse, canonicalize, parse again
	// gives identical bytes (02 req 26).
	again, err := AppendCanonical(nil, mustDecode(t, want))
	if err != nil || string(again) != want {
		t.Fatalf("second pass = %s, %v", again, err)
	}
}

func TestAppendNumbers(t *testing.T) {
	cases := []struct {
		v         any
		verbatim  string
		canonical string
		canonErr  error
	}{
		{json.Number("1.0"), "1.0", "1", nil},
		{json.Number("-0"), "-0", "0", nil},
		{json.Number("1E2"), "1E2", "100", nil},
		{json.Number("9007199254740992"), "9007199254740992", "9007199254740992", nil},
		{json.Number("9007199254740993"), "9007199254740993", "9007199254740992", nil},
		{json.Number("1e400"), "1e400", "", ErrNumberRange},
		{0, "0", "0", nil},
		{-5, "-5", "-5", nil},
		{int8(-8), "-8", "-8", nil},
		{int16(16), "16", "16", nil},
		{int32(-32), "-32", "-32", nil},
		{int64(MaxSafeInteger), "9007199254740991", "9007199254740991", nil},
		{int64(-MaxSafeInteger - 1), "-9007199254740992", "", ErrNumberRange},
		{uint(7), "7", "7", nil},
		{uint8(8), "8", "8", nil},
		{uint16(16), "16", "16", nil},
		{uint32(32), "32", "32", nil},
		{uint64(math.MaxUint64), "18446744073709551615", "", ErrNumberRange},
		{0.1, "0.1", "0.1", nil},
		{1e21, "1e+21", "1e+21", nil},
		{-1.5e-7, "-1.5e-7", "-1.5e-7", nil},
		{float32(0.1), "0.1", "0.1", nil},
	}
	for _, c := range cases {
		got, err := Append(nil, c.v)
		if err != nil || string(got) != c.verbatim {
			t.Fatalf("Append(%T %v) = %s, %v; want %s", c.v, c.v, got, err, c.verbatim)
		}
		got, err = AppendCanonical(nil, c.v)
		if !errors.Is(err, c.canonErr) || (err == nil && string(got) != c.canonical) {
			t.Fatalf("AppendCanonical(%T %v) = %s, %v; want %s, %v", c.v, c.v, got, err, c.canonical, c.canonErr)
		}
	}
}

func TestAppendErrors(t *testing.T) {
	self := &Object{}
	self.Set("self", self)
	loop := []any{nil}
	loop[0] = loop
	cyc := map[string]any{}
	cyc["m"] = cyc
	cases := []struct {
		name string
		v    any
		err  error
	}{
		{"invalid UTF-8 string", "a\xffb", ErrInvalidUTF8},
		{"invalid UTF-8 name", map[string]any{"\xff": 1}, ErrInvalidUTF8},
		{"invalid UTF-8 object name", &Object{members: []Member{{"\xc0", 1}}}, ErrInvalidUTF8},
		{"invalid literal", json.Number("01"), ErrInvalidNumber},
		{"empty literal", json.Number(""), ErrInvalidNumber},
		{"NaN", math.NaN(), ErrNumberRange},
		{"infinity", math.Inf(-1), ErrNumberRange},
		{"nested NaN", []any{1, map[string]any{"a": math.Inf(1)}}, ErrNumberRange},
		{"unsupported", struct{}{}, ErrUnsupportedType},
		{"unsupported element", []any{"ok", make(chan int)}, ErrUnsupportedType},
		{"cyclic object", self, ErrDepth},
		{"cyclic array", loop, ErrDepth},
		{"cyclic map", cyc, ErrDepth},
	}
	for _, c := range cases {
		if _, err := Append(nil, c.v); !errors.Is(err, c.err) {
			t.Fatalf("%s: err = %v, want %v", c.name, err, c.err)
		}
	}
	// Errors inside a sorted object leave the scratch stack balanced.
	e := Encoder{}
	bad := mustDecode(t, `{"b":1,"a":2}`).(*Object)
	bad.Set("c", math.NaN())
	for range 3 {
		if _, err := e.Append(nil, bad); !errors.Is(err, ErrNumberRange) {
			t.Fatal(err)
		}
		if len(e.idx) != 0 {
			t.Fatalf("scratch not unwound: %d", len(e.idx))
		}
	}
	badMap := map[string]any{"b": 1, "a": math.NaN()}
	if _, err := e.Append(nil, badMap); !errors.Is(err, ErrNumberRange) || len(e.keys) != 0 {
		t.Fatalf("map scratch not unwound: %v %d", err, len(e.keys))
	}
}

func TestAppendNullForms(t *testing.T) {
	for _, v := range []any{nil, (*Object)(nil), map[string]any(nil), []any(nil)} {
		got, err := Append(nil, v)
		if err != nil || string(got) != "null" {
			t.Fatalf("Append(%#v) = %s, %v", v, got, err)
		}
	}
	got, err := Append(nil, []any{false, &Object{}, map[string]any{}})
	if err != nil || string(got) != `[false,{},{}]` {
		t.Fatalf("Append = %s, %v", got, err)
	}
}

func TestAppendDeepButFinite(t *testing.T) {
	in := strings.Repeat(`{"a":[`, 2000) + "1" + strings.Repeat("]}", 2000)
	got, err := Append(nil, mustDecode(t, in))
	if err != nil || string(got) != in {
		t.Fatalf("deep tree: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	// 07 P2: Decode(Append(t)) equals t, and encoding is idempotent.
	for _, c := range conformanceCases() {
		v, _, err := Decode([]byte(c.input), Options{MaxDepth: MaxDepthLimit, AllowDuplicateNames: true})
		if err != nil {
			continue
		}
		for _, e := range []Encoder{{}, {Options: EncodeOptions{Order: OrderInsertion}}} {
			b, err := e.Append(nil, v)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			w, _, err := Decode(b, Options{MaxDepth: MaxDepthLimit})
			if err != nil || !Equal(v, w) {
				t.Fatalf("%s: round trip %s: %v", c.name, b, err)
			}
			b2, _ := e.Append(nil, w)
			if string(b2) != string(b) {
				t.Fatalf("%s: second encoding %s differs from %s", c.name, b2, b)
			}
		}
	}
}

// shuffleObjects permutes the members of every object in v.
func shuffleObjects(r *rand.Rand, v any) {
	switch x := v.(type) {
	case *Object:
		r.Shuffle(len(x.members), func(i, j int) { x.members[i], x.members[j] = x.members[j], x.members[i] })
		x.index = nil
		for _, m := range x.members {
			shuffleObjects(r, m.Value)
		}
	case []any:
		for _, e := range x {
			shuffleObjects(r, e)
		}
	}
}

func TestPermutationInvariance(t *testing.T) {
	// 07 P2: permuting input member order gives identical output.
	orders := readOrders(t)
	want, err := Append(nil, mustDecode(t, string(orders)))
	if err != nil {
		t.Fatal(err)
	}
	wantJCS, _ := AppendCanonical(nil, mustDecode(t, string(orders)))
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: deterministic test data
	for range 50 {
		v := mustDecode(t, string(orders))
		shuffleObjects(r, v)
		got, err := Append(nil, v)
		if err != nil || string(got) != string(want) {
			t.Fatalf("permuted output differs:\n%s\n%s", got, want)
		}
		got, err = AppendCanonical(nil, v)
		if err != nil || string(got) != string(wantJCS) {
			t.Fatalf("permuted canonical output differs:\n%s\n%s", got, wantJCS)
		}
	}
}

func TestEncoderZeroAllocs(t *testing.T) {
	// A reused Encoder writing into a large enough buffer allocates
	// nothing after warm-up, sorted or not (07 req 89 "0 Ruralz-owned
	// allocations").
	v := mustDecode(t, string(readOrders(t)))
	m, _, _ := Decode(readOrders(t), Options{MapObjects: true})
	for _, o := range []EncodeOptions{{}, {Order: OrderUTF16, Numbers: NumbersCanonical}, {Order: OrderInsertion}} {
		for _, tree := range []any{v, m} {
			e := Encoder{Options: o}
			buf := make([]byte, 0, 4096)
			if _, err := e.Append(buf, tree); err != nil {
				t.Fatal(err)
			}
			allocs := testing.AllocsPerRun(20, func() {
				if _, err := e.Append(buf[:0], tree); err != nil {
					t.Fatal(err)
				}
			})
			if allocs != 0 {
				t.Fatalf("%+v %T: %v allocations", o, tree, allocs)
			}
		}
	}
}

func TestAppendString(t *testing.T) {
	for in, want := range map[string]string{
		"":          `""`,
		"plain":     `"plain"`,
		"\x01\x1f":  `"\u0001\u001f"`,
		"a\"b\\c":   `"a\"b\\c"`,
		"</script>": `"</script>"`,
		"é ":        "\"é \"",
	} {
		got, err := AppendString(nil, in)
		if err != nil || string(got) != want {
			t.Fatalf("AppendString(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
}

func TestAppendMaxBytes(t *testing.T) {
	// 07 req 70 and 05 req 47: the output cap is enforced while encoding.
	v := mustDecode(t, string(readOrders(t)))
	full, err := Append(nil, v)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("xyz")
	for _, o := range []EncodeOptions{{MaxBytes: len(full)}, {MaxBytes: len(full) + 1}, {}} {
		e := Encoder{Options: o}
		got, err := e.Append(prefix, v)
		if err != nil || string(got[len(prefix):]) != string(full) {
			t.Fatalf("MaxBytes %d: %v", o.MaxBytes, err)
		}
	}
	for _, max := range []int{1, 10, len(full) / 2, len(full) - 1} {
		e := Encoder{Options: EncodeOptions{MaxBytes: max}}
		got, err := e.Append(prefix, v)
		if !errors.Is(err, ErrOutputTooLarge) {
			t.Fatalf("MaxBytes %d: err = %v", max, err)
		}
		if len(got)-len(prefix) > max+64 {
			t.Fatalf("MaxBytes %d: wrote %d bytes before stopping", max, len(got)-len(prefix))
		}
	}
	// A long string is refused before it is copied.
	long := strings.Repeat("x", 1<<20)
	e := Encoder{Options: EncodeOptions{MaxBytes: 1024}}
	got, err := e.Append(nil, []any{long})
	if !errors.Is(err, ErrOutputTooLarge) || len(got) > 1 {
		t.Fatalf("long string: %d bytes, %v", len(got), err)
	}
	// Escaping growth is caught while the string is written.
	ctl := strings.Repeat("\x01", 200)
	if got, err := e.Append(nil, ctl); !errors.Is(err, ErrOutputTooLarge) || len(got) > 1024 {
		t.Fatalf("escaped growth: %d bytes, %v", len(got), err)
	}
	if got, err := e.Append(nil, ctl[:100]); err != nil || len(got) != 602 {
		t.Fatalf("escaped string under the limit: %d, %v", len(got), err)
	}
}

func TestAppendMaxBytesBeforeCopying(t *testing.T) {
	// 07 req 70 and 05 req 47: names, number literals and escaped strings
	// are refused before the output passes MaxBytes, so a refused Append
	// writes at most MaxBytes plus a token's worth, never the oversized
	// value.
	long := strings.Repeat("n", 1<<20)
	ctl := strings.Repeat("\x01", 1000)
	mixed := strings.Repeat("ab\n", 400)
	cases := []struct {
		name         string
		max          int
		v            any
		canonicalErr error // the error in the RFC 8785 form
	}{
		{"long name in an object", 10, &Object{members: []Member{{long, true}}}, ErrOutputTooLarge},
		{"long name in a map", 10, map[string]any{long: true}, ErrOutputTooLarge},
		{"long name after members", 64, mustDecode(t, `{"a":1,"b":[2,3]}`).(*Object).Clone(), ErrOutputTooLarge},
		// A 1 MiB integer literal overflows a double: no canonical form.
		{"long number", 10, []any{json.Number("1" + strings.Repeat("0", 1<<20))}, ErrNumberRange},
		{"long canonical number", 4, json.Number("1" + strings.Repeat("0", 300)), ErrOutputTooLarge},
		{"long control string", 1024, []any{ctl}, ErrOutputTooLarge},
		{"long control name", 1024, map[string]any{ctl: 1}, ErrOutputTooLarge},
		{"long mixed string", 1024, mixed, ErrOutputTooLarge},
		{"long escaped value after a name", 1024, map[string]any{"k": ctl}, ErrOutputTooLarge},
	}
	cases[2].v.(*Object).Set(long, 1)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, o := range []EncodeOptions{{MaxBytes: c.max}, {MaxBytes: c.max, Order: OrderUTF16, Numbers: NumbersCanonical}} {
				e := Encoder{Options: o}
				prefix := []byte("prefix")
				got, err := e.Append(prefix, c.v)
				want := ErrOutputTooLarge
				if o.Numbers == NumbersCanonical {
					want = c.canonicalErr
				}
				if !errors.Is(err, want) {
					t.Fatalf("%+v: err = %v, want %v", o, err, want)
				}
				if n := len(got) - len(prefix); n > c.max+32 {
					t.Fatalf("%+v: wrote %d bytes with MaxBytes %d", o, n, c.max)
				}
			}
		})
	}
}

func TestAppendStringLimit(t *testing.T) {
	// The bounded string writer stops before passing its limit and accepts
	// output that ends exactly at it (07 req 70).
	for _, s := range []string{"", "plain", "\x01\x1f", "a\"b\\c", "é\n\u2028", strings.Repeat("\x00x", 50)} {
		full, err := AppendString([]byte("xy"), s)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := appendString([]byte("xy"), s, len(full)); err != nil || string(got) != string(full) {
			t.Fatalf("%q at its exact length: %q, %v", s, got, err)
		}
		for limit := 2; limit < len(full); limit++ {
			got, err := appendString([]byte("xy"), s, limit)
			if !errors.Is(err, ErrOutputTooLarge) || len(got) > max(limit, 3) {
				t.Fatalf("%q with limit %d: %d bytes, %v", s, limit, len(got), err)
			}
		}
	}
}

func TestEncoderDropsLargeScratch(t *testing.T) {
	// 07 req 73: an Encoder keeps sort scratch for reuse up to about 64
	// KiB, so a pooled Encoder does not pin room for the largest object it
	// wrote.
	const n = 20000
	if n <= maxScratchIdx || n <= maxScratchKeys {
		t.Fatal("the test object does not pass the scratch bounds")
	}
	o := NewObject(n)
	m := make(map[string]any, n)
	for i := range n {
		name := fmt.Sprintf("k%06d", n-i) // descending, so the encoder sorts
		o.Set(name, i)
		m[name] = i
	}
	for _, v := range []any{o, m, []any{o, m}} {
		e := Encoder{}
		if _, err := e.Append(nil, readOrdersTree(t)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Append(nil, v); err != nil {
			t.Fatal(err)
		}
		if cap(e.idx) > maxScratchIdx || cap(e.keys) > maxScratchKeys {
			t.Fatalf("%T: kept %d positions and %d keys", v, cap(e.idx), cap(e.keys))
		}
		// Small scratch stays for reuse.
		small := map[string]any{"b": 1, "a": 2}
		if _, err := e.Append(nil, []any{small, &Object{members: []Member{{"b", 1}, {"a", 2}}}}); err != nil {
			t.Fatal(err)
		}
		if e.idx == nil || e.keys == nil || len(e.idx) != 0 || len(e.keys) != 0 {
			t.Fatalf("%T: small scratch not kept", v)
		}
	}
}

// readOrdersTree decodes the orders document.
func readOrdersTree(t *testing.T) any {
	t.Helper()
	return mustDecode(t, string(readOrders(t)))
}
