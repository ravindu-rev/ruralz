// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Tests for Decode: 07 req 56 (mutable tree, numbers keep their literal
// text), 07 req 58 and 69 (strict decode, cost reported and bounded by the
// budget), 07 req 80 (duplicates rejected for validation), 03 req 25 (last
// wins for CEL and transforms), 02 req 16 (numbers carried as literals).

func TestDecodeValues(t *testing.T) {
	obj := func(ms ...Member) *Object { return &Object{members: ms} }
	cases := []struct {
		name  string
		input string
		want  any
	}{
		{"null", `null`, nil},
		{"true", ` true `, true},
		{"false", "false\n", false},
		{"integer keeps its text", `-0`, json.Number("-0")},
		{"float keeps its text", `1.50E+02`, json.Number("1.50E+02")},
		{"big integer keeps its digits", `123456789012345678901234567890`, json.Number("123456789012345678901234567890")},
		{"string", `"a\u00e9\ud83d\ude00\n"`, "aé\U0001F600\n"},
		{"empty array", `[]`, []any{}},
		{"empty object", `{}`, obj()},
		{"order kept", `{"b":1,"a":[true,null],"c":{"z":"x","y":"w"}}`, obj(
			Member{"b", json.Number("1")},
			Member{"a", []any{true, nil}},
			Member{"c", obj(Member{"z", "x"}, Member{"y", "w"})},
		)},
		{"escaped names", `{"a":1,"b\"":2}`, obj(Member{"a", json.Number("1")}, Member{"b\"", json.Number("2")})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, cost, err := Decode([]byte(c.input), Options{})
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(v, c.want) {
				t.Fatalf("Decode = %#v, want %#v", v, c.want)
			}
			if cost != Cost(v) {
				t.Fatalf("cost %d, Cost %d", cost, Cost(v))
			}
		})
	}
}

func TestDecodeMemberOrder(t *testing.T) {
	// The tree keeps input member order (WP-02 scope, 05 req 47 merge).
	v, _, err := Decode([]byte(`{"z":1,"a":2,"m":3,"b":4,"y":5,"c":6,"x":7,"d":8,"w":9,"e":10}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	o := v.(*Object)
	var names []string
	for name := range o.All() {
		names = append(names, name)
	}
	if got := strings.Join(names, ","); got != "z,a,m,b,y,c,x,d,w,e" {
		t.Fatalf("order = %s", got)
	}
	if x, ok := o.Get("x"); !ok || x != json.Number("7") {
		t.Fatalf("Get(x) = %v, %v", x, ok)
	}
}

func TestDecodeDuplicates(t *testing.T) {
	in := []byte(`{"a":1,"b":2,"a":{"c":3}}`)
	// Default and validation (07 req 80; 06 req 25): rejected.
	if _, _, err := Decode(in, Options{}); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("default: err = %v, want ErrDuplicateName", err)
	}
	if _, _, err := Decode(in, Options{MapObjects: true}); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("map objects: err = %v, want ErrDuplicateName", err)
	}
	// Transforms and CEL (07 req 58; 03 req 25): last wins, first position.
	v, _, err := Decode(in, Options{AllowDuplicateNames: true})
	if err != nil {
		t.Fatal(err)
	}
	o := v.(*Object)
	if o.Len() != 2 || o.At(0).Name != "a" || o.At(1).Name != "b" {
		t.Fatalf("members = %v", o.members)
	}
	if !Equal(o.At(0).Value, &Object{members: []Member{{"c", json.Number("3")}}}) {
		t.Fatalf("a = %v, want the last value", o.At(0).Value)
	}
	m, _, err := Decode(in, Options{AllowDuplicateNames: true, MapObjects: true})
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(m, v) {
		t.Fatalf("map form %v differs from %v", m, v)
	}
	// Many duplicates in a large object take the indexed path.
	var b strings.Builder
	b.WriteString("{")
	for i := range 50 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"k`)
		b.WriteByte(byte('a' + i%10))
		b.WriteString(`":`)
		b.WriteString(strings.Repeat("1", i%3+1))
	}
	b.WriteString("}")
	v, _, err = Decode([]byte(b.String()), Options{AllowDuplicateNames: true})
	if err != nil {
		t.Fatal(err)
	}
	if o := v.(*Object); o.Len() != 10 {
		t.Fatalf("Len = %d, want 10", o.Len())
	}
}

func TestDecodeMapObjectsMatchesEncodingJSON(t *testing.T) {
	// MapObjects yields exactly the encoding/json UseNumber data model, the
	// form JSON Schema validation and JWT claims consume (07 req 80).
	for _, c := range conformanceCases() {
		if !c.accept || c.flag != "" {
			continue
		}
		v, _, err := Decode([]byte(c.input), Options{MapObjects: true})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		dec := json.NewDecoder(strings.NewReader(c.input))
		dec.UseNumber()
		var want any
		if err := dec.Decode(&want); err != nil {
			t.Fatalf("%s: encoding/json: %v", c.name, err)
		}
		if !reflect.DeepEqual(v, want) {
			t.Fatalf("%s: Decode = %#v, encoding/json = %#v", c.name, v, want)
		}
	}
}

func TestDecodeCost(t *testing.T) {
	// 07 req 58, 69; 03 req 25: the cost of every value is charged before
	// it is built; a budget equal to the cost passes, one byte less fails.
	cases := map[string]int64{
		`null`:        CostValue,
		`true`:        CostValue,
		`"abc"`:       CostValue + CostString + 3,
		`12.5`:        CostValue + CostString + 4,
		`[]`:          CostValue + CostArray,
		`[1,2]`:       CostValue + CostArray + 2*(CostValue+CostString+1),
		`{}`:          CostValue + CostObject,
		`{"ab":null}`: CostValue + CostObject + CostMember + 2 + CostValue,
		`"\u00e9"`:    CostValue + CostString + 2,
	}
	// An *Object of more than 8 members pays for its name index; the map
	// form has none.
	nine := `{"a":0,"b":0,"c":0,"d":0,"e":0,"f":0,"g":0,"h":0,"i":0}`
	nineMembers := int64(CostValue + CostObject + 9*(CostMember+1+CostValue+CostString+1))
	for _, c := range []struct {
		o    Options
		want int64
	}{
		{Options{}, nineMembers + 9*CostIndexEntry},
		{Options{AllowDuplicateNames: true}, nineMembers + 9*CostIndexEntry},
		{Options{MapObjects: true}, nineMembers},
	} {
		v, cost, err := Decode([]byte(nine), c.o)
		if err != nil || cost != c.want || Cost(v) != c.want {
			t.Fatalf("%+v: cost %d, Cost %d, err %v; want %d", c.o, cost, Cost(v), err, c.want)
		}
	}
	// A duplicate is charged for its name and value but adds no member.
	v, cost, err := Decode([]byte(nine[:len(nine)-1]+`,"a":1}`), Options{AllowDuplicateNames: true})
	if err != nil || Cost(v) != nineMembers+9*CostIndexEntry || cost != Cost(v)+CostMember+1+CostValue+CostString+1 {
		t.Fatalf("duplicate: cost %d, Cost %d, err %v", cost, Cost(v), err)
	}
	// The index charge is taken when the ninth member arrives, at its name.
	_, _, err = Decode([]byte(nine), Options{MaxCost: nineMembers + 8*CostIndexEntry})
	var e *Error
	if !errors.As(err, &e) || !errors.Is(e.Kind, ErrTooLarge) || e.Offset != strings.Index(nine, `"i"`) {
		t.Fatalf("index charge: err = %v", err)
	}
	for in, want := range cases {
		for _, mapObjects := range []bool{false, true} {
			v, cost, err := Decode([]byte(in), Options{MapObjects: mapObjects})
			if err != nil || cost != want || Cost(v) != want {
				t.Fatalf("%s: cost %d, Cost %d, err %v; want %d", in, cost, Cost(v), err, want)
			}
		}
	}
	orders := readOrders(t)
	v, cost, err = Decode(orders, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if cost != Cost(v) {
		t.Fatalf("cost %d, Cost %d", cost, Cost(v))
	}
	if _, _, err := Decode(orders, Options{MaxCost: cost}); err != nil {
		t.Fatalf("budget equal to the cost: %v", err)
	}
	_, spent, err := Decode(orders, Options{MaxCost: cost - 1})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("budget one below the cost: err = %v", err)
	}
	if spent <= cost-1 || spent > cost {
		t.Fatalf("spent %d with budget %d", spent, cost-1)
	}
}

func TestDecodeBudgetBoundsWork(t *testing.T) {
	// A body far over its budget stops at the budget plus one value, so
	// decoded memory stays bounded (07 req 58 "oversize at 4 times the raw
	// limit").
	raw := []byte("[" + strings.Repeat(`"xxxxxxxxxxxxxxxx",`, 100000) + `""]`)
	budget := int64(4 * 1024)
	_, spent, err := Decode(raw, Options{MaxCost: budget})
	var e *Error
	if !errors.As(err, &e) || !errors.Is(e.Kind, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if spent > budget+CostValue+CostString+16 {
		t.Fatalf("spent %d, budget %d", spent, budget)
	}
	if e.Offset <= 0 || e.Offset > 2048 {
		t.Fatalf("failure offset %d, want early in the input", e.Offset)
	}
	// A long string is charged before it is copied.
	long := []byte(`{"k":"` + strings.Repeat("y", 1<<20) + `"}`)
	if _, spent, err := Decode(long, Options{MaxCost: 1 << 10}); !errors.Is(err, ErrTooLarge) || spent < 1<<20 {
		t.Fatalf("long string: spent %d, err %v", spent, err)
	}
	// Long names too.
	long = []byte(`{"` + strings.Repeat("n", 4096) + `":1}`)
	if _, _, err := Decode(long, Options{MaxCost: 1024, MapObjects: true}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("long name: err %v", err)
	}
}

func TestDecodeErrors(t *testing.T) {
	cases := []struct {
		input string
		o     Options
		kind  error
		off   int
	}{
		{``, Options{}, ErrSyntax, 0},
		{`[1,2`, Options{}, ErrSyntax, 4},
		{`{"a":[1,}`, Options{}, ErrSyntax, 8},
		{`{"a":1} x`, Options{}, ErrSyntax, 8},
		{`[[[1]]]`, Options{MaxDepth: 2}, ErrDepth, 2},
		{`{"a":{"b":1,"b":2}}`, Options{MapObjects: true}, ErrDuplicateName, 12},
		{`["abc",[1,2,3]]`, Options{MaxCost: 80}, ErrTooLarge, 7},
		{`{"a":1,"b":true}`, Options{MaxCost: 100}, ErrTooLarge, 5},
		{"[\"\xc3\x28\"]", Options{}, ErrInvalidUTF8, 2},
		{`{"a":[1,}`, Options{MapObjects: true}, ErrSyntax, 8},
		{`{"a":1,`, Options{MapObjects: true}, ErrSyntax, 7},
		{`{"a"`, Options{MapObjects: true}, ErrSyntax, 4},
		{`{"a":1,"bb":2}`, Options{MapObjects: true, MaxCost: 120}, ErrTooLarge, 7},
		{`[{}]`, Options{MapObjects: true, MaxCost: 60}, ErrTooLarge, 1},
	}
	for _, c := range cases {
		v, _, err := Decode([]byte(c.input), c.o)
		var e *Error
		if v != nil || !errors.As(err, &e) || !errors.Is(e.Kind, c.kind) || e.Offset != c.off {
			t.Fatalf("Decode(%q) = %v, %v; want %v at %d", c.input, v, err, c.kind, c.off)
		}
	}
}

// manyMembers returns an object with n distinct members.
func manyMembers(n int) []byte {
	var b strings.Builder
	b.WriteByte('{')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%d":%d`, i, i%10)
	}
	b.WriteByte('}')
	return []byte(b.String())
}

// checkNamesReleased fails when a reset nameSet keeps scratch over the
// bounds (07 req 73): records, nesting-level slices, or index maps holding
// room for more than maxScratchNames entries in total.
func checkNamesReleased(t *testing.T, ns *nameSet) {
	t.Helper()
	if ns.data != nil || cap(ns.recs) > maxScratchNames || cap(ns.objs) > maxScratchLevels || cap(ns.index) > maxScratchLevels {
		t.Fatalf("kept the input or records %d, levels %d, indexes %d", cap(ns.recs), cap(ns.objs), cap(ns.index))
	}
	if cap(ns.a) > maxScratch || cap(ns.b) > maxScratch {
		t.Fatalf("kept unescape buffers of %d and %d bytes", cap(ns.a), cap(ns.b))
	}
	total := 0
	for i, li := range ns.index {
		if li.m == nil {
			continue
		}
		if len(li.m) != 0 {
			t.Fatalf("level %d index not cleared", i)
		}
		total += li.room
	}
	if total > maxScratchNames {
		t.Fatalf("kept index room for %d names", total)
	}
}

func TestDecoderReuse(t *testing.T) {
	d := NewDecoder(Options{AllowDuplicateNames: true})
	for _, in := range []string{`{"a":1}`, `[{"a":"A","a":2}]`, `"x"`} {
		v, _, err := d.Decode([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		w, _, _ := Decode([]byte(in), Options{AllowDuplicateNames: true})
		if !Equal(v, w) {
			t.Fatalf("%s: reused decoder %v, fresh %v", in, v, w)
		}
	}
	// A decoder releases the input and drops scratch over 64 KiB (07 req
	// 73), so a pooled decoder never pins a large body.
	big := []byte(`"` + strings.Repeat(`\n`, 100<<10) + `"`)
	if _, _, err := d.Decode(big); err != nil {
		t.Fatal(err)
	}
	if d.buf != nil || d.s.data != nil || d.s.names.data != nil {
		t.Fatalf("decoder kept %d bytes of scratch or the input", cap(d.buf))
	}
	small := []byte(`"\n"`)
	if _, _, err := d.Decode(small); err != nil {
		t.Fatal(err)
	}
	if d.buf == nil || cap(d.buf) > maxScratch {
		t.Fatalf("small scratch not kept: %d", cap(d.buf))
	}
}

func TestDecoderReleasesNameScratch(t *testing.T) {
	// 07 req 73: a strict decoder (validation.json-schema, JWT claims)
	// checks duplicate names through records and per-level index maps; after
	// an object of 200,000 members it keeps neither the records nor the
	// grown index map, while small inputs keep their scratch for reuse.
	huge := manyMembers(200000)
	for _, o := range []Options{{MapObjects: true}, {}} {
		d := NewDecoder(o)
		if _, _, err := d.Decode(readOrders(t)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.Decode(huge); err != nil {
			t.Fatal(err)
		}
		ns := &d.s.names
		checkNamesReleased(t, ns)
		if ns.recs != nil || len(ns.index) > 0 && ns.index[0].m != nil {
			t.Fatalf("%+v: kept records %d or the level 0 index", o, cap(ns.recs))
		}
		if len(d.s.stack) != 0 || cap(d.s.stack) > MaxDepthLimit/64+1 {
			t.Fatalf("%+v: stack %d words", o, cap(d.s.stack))
		}
		// Reuse after the release still works and keeps small scratch.
		if _, _, err := d.Decode(manyMembers(100)); err != nil {
			t.Fatal(err)
		}
		if ns.recs == nil || ns.index[0].m == nil || ns.index[0].room != 100 {
			t.Fatalf("%+v: small scratch not kept", o)
		}
	}
}

// densityCases are bodies of one shape at about rawLimit bytes.
func densityCases(rawLimit int) map[string][]byte {
	fill := func(open, elem, sep, end string, n func(i int) string) []byte {
		var b strings.Builder
		b.WriteString(open)
		for i := 0; b.Len()+len(elem)+len(sep)+8 < rawLimit; i++ {
			if i > 0 {
				b.WriteString(sep)
			}
			fmt.Fprintf(&b, elem, n(i))
		}
		b.WriteString(end)
		return []byte(b.String())
	}
	none := func(int) string { return "" }
	return map[string][]byte{
		"one-digit numbers": fill("[", "0%s", ",", "]", none),
		"short members":     fill("{", `"k%s":1`, ",", "}", func(i int) string { return fmt.Sprint(i) }),
		"empty objects":     fill("[", "{}%s", ",", "]", none),
	}
}

func TestDecodeCostDensity(t *testing.T) {
	// 07 req 58, 03 req 25, 04 req 47: the oversize stop of 4 times the raw
	// limit and the cost model of built Go values. Cost per input byte
	// depends on density, so a dense body well inside its raw limit costs
	// over 4 times that limit; MaxCostPerByte times the limit admits every
	// body within it. The ratios here are the ones the MaxCost and
	// CostValue docs state.
	const rawLimit = 64 << 10
	ratio := func(data []byte) float64 {
		_, cost, err := Decode(data, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return float64(cost) / float64(len(data))
	}
	r := ratio(readOrders(t))
	t.Logf("orders: cost %.2f times the input bytes", r)
	if r < 2 || r > 4 {
		t.Fatalf("orders: cost ratio %.2f", r)
	}
	cases := densityCases(rawLimit)
	for name, want := range map[string][2]float64{
		"one-digit numbers": {16, 17},
		"short members":     {10, 12},
		"empty objects":     {21, 22},
	} {
		data := cases[name]
		if len(data) > rawLimit {
			t.Fatalf("%s: %d bytes over the raw limit", name, len(data))
		}
		r := ratio(data)
		t.Logf("%s: cost %.2f times the input bytes", name, r)
		if r < want[0] || r > want[1] {
			t.Fatalf("%s: cost ratio %.2f, want %v", name, r, want)
		}
		_, _, err := Decode(data, Options{MaxCost: 4 * rawLimit})
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("%s at 4 times the raw limit: err = %v, want ErrTooLarge", name, err)
		}
		if _, _, err := Decode(data, Options{MaxCost: MaxCostPerByte * rawLimit}); err != nil {
			t.Fatalf("%s at MaxCostPerByte times the raw limit: %v", name, err)
		}
	}
	// A dense numeric array already fails at a quarter of the raw limit.
	quarter := densityCases(rawLimit / 4)["one-digit numbers"]
	if _, _, err := Decode(quarter, Options{MaxCost: 4 * rawLimit}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("quarter-size dense array: err = %v", err)
	}
	// The bound is reached by a lone one-digit number and holds for the
	// densest shapes.
	for _, in := range []string{"0", "{}", "[]", `""`, "[0]", "[{}]", "[{},{}]", `{"":0}`, `{"":{}}`, "null"} {
		_, cost, err := Decode([]byte(in), Options{})
		if err != nil || cost > MaxCostPerByte*int64(len(in)) {
			t.Fatalf("%s: cost %d for %d bytes, %v", in, cost, len(in), err)
		}
		if in == "0" && cost != MaxCostPerByte {
			t.Fatalf("lone digit costs %d, want MaxCostPerByte", cost)
		}
	}
}

func TestDecodeValueGuard(t *testing.T) {
	// Tokens that never start a value are refused defensively.
	var d Decoder
	for _, k := range []Kind{KindInvalid, KindName, KindObjectEnd, KindArrayEnd} {
		if _, err := d.value(Token{Kind: k}); !errors.Is(err, ErrSyntax) {
			t.Fatalf("%v: err = %v", k, err)
		}
	}
}
