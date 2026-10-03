// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sfv

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Tests for the RFC 9651 section 4.1 serializer that writes the RateLimit
// fields of 05 req 67 and 68 (R-18). Examples marked "RFC 9651 3.x" are the
// field values of that RFC section.

// golden returns testdata/name without its final newline.
func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: the test reads its own golden files.
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(b, []byte("\n"))
}

func p(key string, v BareItem) Param { return Param{Key: key, Value: v} }

func it(v BareItem, params ...Param) Item { return Item{Value: v, Params: params} }

// allTypesList is testdata/list-all-types.golden as a List: every bare item
// type, parameters and inner lists, including RFC 9651 examples.
func allTypesList() List {
	return List{
		ItemMember(Integer(42)),                                             // RFC 9651 3.3.1
		ItemMember(Integer(MinInteger)),                                     // lower bound
		ItemMember(Decimal(4.5)),                                            // RFC 9651 3.3.2
		ItemMember(Decimal(-0.25)),                                          // negative
		ItemMember(Decimal(0)),                                              // at least one fractional digit
		ItemMember(String(`hello "world" \ !`)),                             // escapes
		ItemMember(Token("foo123/456")),                                     // RFC 9651 3.3.4
		ItemMember(Token("*tok:en")),                                        // "*" start, ":" inside
		ItemMember(ByteSequence([]byte("pretend this is binary content."))), // RFC 9651 3.3.5
		ItemMember(ByteSequence(nil)),                                       // empty
		ItemMember(Boolean(true)),                                           // RFC 9651 3.3.6
		ItemMember(Boolean(false)),                                          // false
		ItemMember(Date(1659578233)),                                        // RFC 9651 3.3.7
		ItemMember(Date(-1)),                                                // before the epoch
		ItemMember(DisplayString("füü % \" \n")),                            // RFC 9651 4.1.11 escaping
		InnerListMember([]Item{it(String("foo"), p("a", Integer(1)), p("b", Integer(2)))}, p("lvl", Integer(5))), // RFC 9651 3.1.1
		InnerListMember([]Item{it(String("bar")), it(String("baz"))}, p("lvl", Integer(1))),
		InnerListMember(nil, p("empty", Boolean(true))),
		ItemMember(Token("abc"), p("a", Integer(1)), p("b", Integer(2)), p("cde_456", Boolean(true))), // RFC 9651 3.1.2
		InnerListMember([]Item{it(Token("ghi"), p("jk", Integer(4))), it(Token("l"))}, p("q", String("9")), p("r", Token("w"))),
		ItemMember(Integer(1), p("a", Boolean(true)), p("b", Boolean(false))), // RFC 9651 3.1.2
		ItemMember(Integer(5), p("foo", Token("bar")), p("*", Date(0)), p("x.y-z_9", DisplayString("é"))),
	}
}

func TestAppendListGoldenAllTypes(t *testing.T) {
	got, err := AppendList(nil, allTypesList())
	if err != nil {
		t.Fatal(err)
	}
	if want := golden(t, "list-all-types.golden"); !bytes.Equal(got, want) {
		t.Fatalf("AppendList =\n%s\nwant\n%s", got, want)
	}
}

func TestAppendBareItem(t *testing.T) {
	tests := []struct {
		name string
		v    BareItem
		want string
	}{
		{"integer max", Integer(MaxInteger), "999999999999999"},
		{"integer min", Integer(MinInteger), "-999999999999999"},
		{"integer zero", Integer(0), "0"},
		{"decimal rounds", Decimal(1.23456), "1.235"},
		{"decimal half to even", Decimal(0.0625), "0.062"},
		{"decimal half to even up", Decimal(0.1875), "0.188"},
		{"decimal trailing zeros", Decimal(100), "100.0"},
		{"decimal one fraction digit", Decimal(1.5), "1.5"},
		{"decimal 12 integer digits", Decimal(999999999999.999), "999999999999.999"},
		{"decimal negative", Decimal(-1.5), "-1.5"},
		{"decimal negative zero", Decimal(math.Copysign(0, -1)), "0.0"},
		{"decimal rounds to zero", Decimal(-0.0001), "0.0"},
		// Ties that float64 cannot hold round by the binary value, as the
		// Decimal doc states: 0.0025 is stored just above the tie and
		// 9.9995 just below it, unlike the httpwg decimal-text cases.
		{"decimal binary tie above", Decimal(0.0025), "0.003"},
		{"decimal binary tie below", Decimal(9.9995), "9.999"},
		{"string empty", String(""), `""`},
		{"string printable", String(" ~"), `" ~"`},
		{"string escapes", String(`a"b\c`), `"a\"b\\c"`},
		{"token single", Token("a"), "a"},
		{"token star", Token("*"), "*"},
		{"token tchar", Token("A!#$%&'*+-.^_`|~9:/"), "A!#$%&'*+-.^_`|~9:/"},
		{"bytes padding", ByteSequence([]byte("hi")), ":aGk=:"},
		{"date zero", Date(0), "@0"},
		{"display string ASCII", DisplayString("abc"), `%"abc"`},
		{"display string DEL", DisplayString("\x7f"), `%"%7f"`},
		{"display string empty", DisplayString(""), `%""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AppendBareItem(nil, tt.v)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("AppendBareItem = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestAppendBareItemFailures(t *testing.T) {
	tests := []struct {
		name string
		v    BareItem
	}{
		{"zero item", BareItem{}},
		{"integer over", Integer(MaxInteger + 1)},
		{"integer under", Integer(MinInteger - 1)},
		{"decimal NaN", Decimal(math.NaN())},
		{"decimal +Inf", Decimal(math.Inf(1))},
		{"decimal -Inf", Decimal(math.Inf(-1))},
		{"decimal 13 integer digits", Decimal(1e12)},
		{"decimal rounds to 13 integer digits", Decimal(999999999999.9995)},
		{"decimal negative 13 digits", Decimal(-1e12)},
		{"string DEL", String("a\x7f")},
		{"string HTAB", String("a\tb")},
		{"string LF", String("a\nb")},
		{"string non-ASCII", String("café")},
		{"token empty", Token("")},
		{"token digit start", Token("1abc")},
		{"token space", Token("foo bar")},
		{"token quote", Token(`a"`)},
		{"token non-ASCII", Token("a\u00e9")},
		{"date over", Date(MaxInteger + 1)},
		{"display string invalid UTF-8", DisplayString("\xff")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := []byte("prefix")
			got, err := AppendBareItem(dst, tt.v)
			if !errors.Is(err, ErrSerialize) {
				t.Fatalf("AppendBareItem = %q, %v; want ErrSerialize", got, err)
			}
			if string(got) != "prefix" {
				t.Errorf("dst = %q after a failure, want it unchanged", got)
			}
		})
	}
}

func TestAppendKey(t *testing.T) {
	for _, key := range []string{"a", "*", "a_b-c.d*9", "q", "lvl"} {
		if got, err := AppendKey(nil, key); err != nil || string(got) != key {
			t.Errorf("AppendKey(%q) = %q, %v", key, got, err)
		}
	}
	for _, key := range []string{"", "A", "1a", "_a", "aB", "a b", "a=b", "é"} {
		if got, err := AppendKey([]byte("x"), key); !errors.Is(err, ErrSerialize) || string(got) != "x" {
			t.Errorf("AppendKey(%q) = %q, %v; want ErrSerialize and dst unchanged", key, got, err)
		}
	}
}

func TestAppendParams(t *testing.T) {
	got, err := AppendParams(nil, []Param{p("a", Boolean(true)), p("b", Boolean(false)), p("c", String("x"))})
	if err != nil || string(got) != `;a;b=?0;c="x"` {
		t.Errorf("AppendParams = %q, %v", got, err)
	}
	for name, params := range map[string][]Param{
		"duplicate key": {p("a", Integer(1)), p("b", Integer(2)), p("a", Integer(3))},
		"bad key":       {p("A", Integer(1))},
		"bad value":     {p("a", Integer(MaxInteger+1))},
	} {
		if got, err := AppendParams([]byte("x"), params); !errors.Is(err, ErrSerialize) || string(got) != "x" {
			t.Errorf("%s: AppendParams = %q, %v; want ErrSerialize and dst unchanged", name, got, err)
		}
	}
}

func TestAppendListFailuresLeaveDst(t *testing.T) {
	for name, l := range map[string]List{
		"bad item":                 {ItemMember(Integer(1)), ItemMember(Token("1"))},
		"bad item parameter":       {ItemMember(Integer(1), p("A", Integer(1)))},
		"bad inner item":           {InnerListMember([]Item{it(Integer(1)), it(String("\n"))})},
		"bad inner param":          {InnerListMember([]Item{it(Integer(1))}, p("", Integer(1)))},
		"bad inner item parameter": {InnerListMember([]Item{it(Integer(1), p("a", Decimal(math.NaN())))})},
	} {
		got, err := AppendList([]byte("keep"), l)
		if !errors.Is(err, ErrSerialize) || string(got) != "keep" {
			t.Errorf("%s: AppendList = %q, %v; want ErrSerialize and dst unchanged", name, got, err)
		}
	}
}

func TestAppendListEmptyAppendsNothing(t *testing.T) {
	// RFC 9651 4.1.1: an empty List is not serialized; the field is omitted.
	got, err := AppendList([]byte("x"), nil)
	if err != nil || string(got) != "x" {
		t.Errorf("AppendList(empty) = %q, %v", got, err)
	}
}

func TestAppendMemberBuildsList(t *testing.T) {
	var dst []byte
	var err error
	for _, m := range allTypesList() {
		if dst, err = AppendMember(dst, m); err != nil {
			t.Fatal(err)
		}
	}
	if want := golden(t, "list-all-types.golden"); !bytes.Equal(dst, want) {
		t.Fatalf("AppendMember sequence =\n%s\nwant\n%s", dst, want)
	}
	before := len(dst)
	if dst, err = AppendMember(dst, ItemMember(BareItem{})); !errors.Is(err, ErrSerialize) || len(dst) != before {
		t.Errorf("AppendMember(invalid) = %v, length %d, want ErrSerialize and length %d", err, len(dst), before)
	}
}

func TestAppendItem(t *testing.T) {
	got, err := AppendItem(nil, it(Integer(5), p("foo", Token("bar")))) // RFC 9651 3.3
	if err != nil || string(got) != "5;foo=bar" {
		t.Errorf("AppendItem = %q, %v", got, err)
	}
	if got, err := AppendItem([]byte("x"), it(Integer(5), p("foo", Token("")))); !errors.Is(err, ErrSerialize) || string(got) != "x" {
		t.Errorf("AppendItem(invalid parameter) = %q, %v", got, err)
	}
}

func TestKindAndAccessors(t *testing.T) {
	tests := []struct {
		v     BareItem
		kind  Kind
		name  string
		i     int64
		f     float64
		text  string
		bytes []byte
		b     bool
	}{
		{Integer(7), KindInteger, "Integer", 7, 0, "", nil, false},
		{Decimal(1.5), KindDecimal, "Decimal", 0, 1.5, "", nil, false},
		{String("s"), KindString, "String", 0, 0, "s", nil, false},
		{Token("t"), KindToken, "Token", 0, 0, "t", nil, false},
		{ByteSequence([]byte{1}), KindByteSequence, "Byte Sequence", 0, 0, "", []byte{1}, false},
		{Boolean(true), KindBoolean, "Boolean", 0, 0, "", nil, true},
		{Boolean(false), KindBoolean, "Boolean", 0, 0, "", nil, false},
		{Date(9), KindDate, "Date", 9, 0, "", nil, false},
		{DisplayString("d"), KindDisplayString, "Display String", 0, 0, "d", nil, false},
		{BareItem{}, 0, "Kind(0)", 0, 0, "", nil, false},
	}
	for _, tt := range tests {
		if tt.v.Kind() != tt.kind || tt.kind.String() != tt.name || tt.v.Int() != tt.i ||
			tt.v.Float() != tt.f || tt.v.Text() != tt.text || !bytes.Equal(tt.v.Bytes(), tt.bytes) || tt.v.Bool() != tt.b {
			t.Errorf("%s: accessors = %v %q %d %v %q %v %v", tt.name, tt.v.Kind(), tt.v.Kind(), tt.v.Int(),
				tt.v.Float(), tt.v.Text(), tt.v.Bytes(), tt.v.Bool())
		}
	}
}
