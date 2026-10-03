// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sfv

import (
	"errors"
	"reflect"
	"testing"
)

// Tests for the RFC 9651 section 4.2 parser: the oracle that serialized
// RateLimit fields reparse (05 test plan item 28). Examples marked "RFC
// 9651 3.x" are the field values of that RFC section, including their
// non-canonical whitespace.

func TestParseListRFCExamples(t *testing.T) {
	tests := []struct {
		name, in, canonical string
	}{
		{"3.1 tokens", "sugar, tea, rum", "sugar, tea, rum"},
		{"3.1.1 inner lists", `("foo" "bar"), ("baz"), ("bat" "one"), ()`, `("foo" "bar"), ("baz"), ("bat" "one"), ()`},
		{"3.1.1 inner list parameters", `("foo"; a=1;b=2);lvl=5, ("bar" "baz");lvl=1`, `("foo";a=1;b=2);lvl=5, ("bar" "baz");lvl=1`},
		{"3.1.2 parameters", `abc;a=1;b=2; cde_456, (ghi;jk=4 l);q="9";r=w`, `abc;a=1;b=2;cde_456, (ghi;jk=4 l);q="9";r=w`},
		{"3.1.2 boolean parameters", "1; a; b=?0", "1;a;b=?0"},
		{"3.3 item parameters", "5; foo=bar", "5;foo=bar"},
		{"3.3.1 integer", "42", "42"},
		{"3.3.2 decimal", "4.5", "4.5"},
		{"3.3.3 string", `"hello world"`, `"hello world"`},
		{"3.3.4 token", "foo123/456", "foo123/456"},
		{"3.3.5 byte sequence", ":cHJldGVuZCB0aGlzIGlzIGJpbmFyeSBjb250ZW50Lg==:", ":cHJldGVuZCB0aGlzIGlzIGJpbmFyeSBjb250ZW50Lg==:"},
		{"3.3.6 boolean", "?1", "?1"},
		{"3.3.7 date", "@1659578233", "@1659578233"},
		{"display string", `%"f%c3%bc%c3%bc"`, `%"f%c3%bc%c3%bc"`},
		{
			"05 req 67 policy", `"ratelimit-global";q=1000;w=1, "ratelimit-gold.1";q=100;w=1, "ratelimit-gold.2";q=5000;w=60`,
			`"ratelimit-global";q=1000;w=1, "ratelimit-gold.1";q=100;w=1, "ratelimit-gold.2";q=5000;w=60`,
		},
		{"leading and trailing SP", "  a , b  ", "a, b"},
		{"OWS around commas", "a\t,\tb", "a, b"},
		{"trailing HTAB in a list", "a\t", "a"},
		{"inner list extra SP", "(  a   b  )", "(a b)"},
		{"empty", "", ""},
		{"only SP", "   ", ""},
		{"missing padding", ":aGk:", ":aGk=:"},
		{"non-zero pad bits", ":aGl=:", ":aGk=:"},
		{"duplicate parameter overwrites in place", "a;x=1;y=2;x=3", "a;x=3;y=2"},
		{"decimal canonical", "-01.500", "-1.5"},
		{"negative zero integer", "-0", "0"},
		{"escapes", `"a\"b\\c"`, `"a\"b\\c"`},
		{"escape first", `"\"x"`, `"\"x"`},
		{"many escapes", `"\\\\\"\\"`, `"\\\\\"\\"`},
		{"15 digit integer", "-999999999999999", "-999999999999999"},
		{"12.3 decimal", "123456789012.345", "123456789012.345"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, err := ParseList(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := AppendList(nil, l)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.canonical {
				t.Errorf("ParseList(%q) serializes as %q, want %q", tt.in, got, tt.canonical)
			}
		})
	}
}

func TestParseListStructure(t *testing.T) {
	l, err := ParseList(`("foo"; a=1;b=2);lvl=5, ("bar" "baz");lvl=1, 4.5;x, :aGk=:, ?0, @-5, %"%c3%a9", tok`)
	if err != nil {
		t.Fatal(err)
	}
	want := List{
		InnerListMember([]Item{it(String("foo"), p("a", Integer(1)), p("b", Integer(2)))}, p("lvl", Integer(5))),
		InnerListMember([]Item{it(String("bar")), it(String("baz"))}, p("lvl", Integer(1))),
		ItemMember(Decimal(4.5), p("x", Boolean(true))),
		ItemMember(ByteSequence([]byte("hi"))),
		ItemMember(Boolean(false)),
		ItemMember(Date(-5)),
		ItemMember(DisplayString("é")),
		ItemMember(Token("tok")),
	}
	if !reflect.DeepEqual(l, want) {
		t.Errorf("ParseList =\n%#v\nwant\n%#v", l, want)
	}
}

func TestParseGoldenAllTypesRoundTrip(t *testing.T) {
	in := golden(t, "list-all-types.golden")
	l, err := ParseList(string(in))
	if err != nil {
		t.Fatal(err)
	}
	got, err := AppendList(nil, l)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(in) {
		t.Errorf("round trip =\n%s\nwant\n%s", got, in)
	}
	want := allTypesList()
	if len(l) != len(want) {
		t.Fatalf("parsed %d members, want %d", len(l), len(want))
	}
	for i := range want {
		if l[i].Inner != want[i].Inner || l[i].Value != want[i].Value || len(l[i].Params) != len(want[i].Params) {
			t.Errorf("member %d = %#v, want %#v", i, l[i], want[i])
		}
	}
}

func TestParseListFailures(t *testing.T) {
	for _, in := range []string{
		"a,", ",a", "a,,b", "a b", "a;", "a;A=1", "a;=1", "a;b=", "a;b=\x01",
		"\ta", "(", "(a", "(a ", "(a b", "(a,b)", "(a)b", "((a))", "(a;b=?2)",
		"-", "--1", "-a", "1234567890123456", "1234567890123.4", "123456789012.3456",
		"1.2345", "1.", "1.2.3",
		`"abc`, `"a\b"`, `"a\`, "\"a\x01\"", "\"a\x7f\"", "\"café\"",
		":aGk", ":a*:", ":a:", ":a=b:",
		"?", "?2", "@", "@1.5", "@a",
		`%`, `%x`, `%"`, `%"a`, `%"%C3%BC"`, `%"%c3"`, `%"%g0"`, `%"%c"`, "%\"\x01\"", `%"%c3%bc`,
		"&", "=", "café",
	} {
		if l, err := ParseList(in); !errors.Is(err, ErrParse) {
			t.Errorf("ParseList(%q) = %#v, %v; want ErrParse", in, l, err)
		}
	}
}

func TestParseItem(t *testing.T) {
	tests := []struct {
		in   string
		want Item
	}{
		{"42", it(Integer(42))},
		{"  5; foo=bar  ", it(Integer(5), p("foo", Token("bar")))},
		{`"ratelimit-gold.1";r=0;t=1`, it(String("ratelimit-gold.1"), p("r", Integer(0)), p("t", Integer(1)))},
	}
	for _, tt := range tests {
		got, err := ParseItem(tt.in)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseItem(%q) = %#v, %v; want %#v", tt.in, got, err, tt.want)
		}
	}
	// An Item field is not a List, and only SP may surround it.
	for _, in := range []string{"", "a, b", "a\t", "é", "(a)", "a;B"} {
		if got, err := ParseItem(in); !errors.Is(err, ErrParse) {
			t.Errorf("ParseItem(%q) = %#v, %v; want ErrParse", in, got, err)
		}
	}
}
