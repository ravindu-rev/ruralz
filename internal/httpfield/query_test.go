// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package httpfield

import (
	"slices"
	"strings"
	"testing"
)

// Tests for 07 req 48 (order-preserving query edits) with the T1 Query
// cases and property P8 of spec 07 section 6.

func TestParseQueryRoundTripReq48(t *testing.T) {
	for _, raw := range []string{
		"", "a=1", "a=1&b=2&a=3", "x=%7e", "a=1;b=2", "k", "=v", "a=1&&b=2",
		"&", "&&", "a+b=c%20d", "%zz=1", "q=%E2%9C%93&lang=en-US",
	} {
		q := ParseQuery(raw)
		if got := string(q.AppendRaw(nil)); got != raw {
			t.Errorf("ParseQuery(%q).AppendRaw = %q", raw, got)
		}
		if got := q.String(); got != raw {
			t.Errorf("ParseQuery(%q).String = %q", raw, got)
		}
	}
}

func TestQueryEditsReq48(t *testing.T) {
	type op struct {
		del         bool
		name, value string
	}
	tests := []struct {
		name string
		raw  string
		ops  []op
		want string
	}{
		{"set replaces the first and drops later ones", "a=1&b=2&a=3", []op{{name: "a", value: "9"}}, "a=9&b=2"},
		{"set appends an absent key escaped", "a=1&b=2&a=3", []op{{name: "c", value: "x y"}}, "a=1&b=2&a=3&c=x+y"},
		{"del drops every occurrence", "a=1&b=2&a=3", []op{{del: true, name: "a"}}, "b=2"},
		{"key a+b equals a b", "a+b=1&c=2", []op{{name: "a b", value: "3"}}, "a+b=3&c=2"},
		{"key a%20b equals a b", "a%20b=1", []op{{del: true, name: "a b"}}, ""},
		{"key a%2Bb equals a+b", "a%2Bb=1&a+b=2", []op{{del: true, name: "a+b"}}, "a+b=2"},
		{"untouched escape stays %7e", "x=%7e&y=1", []op{{name: "y", value: "2"}}, "x=%7e&y=2"},
		{"semicolon is data", "a=1;b=2&c=3", []op{{name: "c", value: "4"}}, "a=1;b=2&c=4"},
		{"semicolon key is not split", "a=1;b=2", []op{{del: true, name: "b"}}, "a=1;b=2"},
		{"empty query set", "", []op{{name: "a", value: "1"}}, "a=1"},
		{"empty query del", "", []op{{del: true, name: "a"}}, ""},
		{"bare key replaced", "k&x=1", []op{{name: "k", value: "v"}}, "k=v&x=1"},
		{"bare key deleted", "k&x=1", []op{{del: true, name: "k"}}, "x=1"},
		{"=v has the empty key and matches no name", "=v&a=1", []op{{del: true, name: "v"}, {name: "a", value: ""}}, "=v&a="},
		{"empty segments kept", "a=1&&b=2", []op{{name: "b", value: "3"}}, "a=1&&b=3"},
		{"invalid escape compares raw", "%zz=1&%41=2", []op{{del: true, name: "%zz"}, {name: "A", value: "3"}}, "%41=3"},
		{"new key and value escaped", "", []op{{name: "a&b=c", value: "1&2=3%"}}, "a%26b%3Dc=1%262%3D3%25"},
		{"value decodes to the set value", "a=1", []op{{name: "a", value: "x+y/z"}}, "a=x%2By%2Fz"},
		{"case-sensitive keys", "A=1", []op{{name: "a", value: "2"}}, "A=1&a=2"},
		{"del then set appends", "a=1&b=2", []op{{del: true, name: "a"}, {name: "a", value: "3"}}, "b=2&a=3"},
		{"truncated escape compares raw", "a%4=1", []op{{del: true, name: "a%4"}}, ""},
		{"escape hex digits in either case", "%7e=1&%7E=2&%7a=3", []op{{del: true, name: "~"}}, "%7a=3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := ParseQuery(tt.raw)
			for _, o := range tt.ops {
				if o.del {
					q.Del(o.name)
				} else {
					q.Set(o.name, o.value)
				}
			}
			if got := q.String(); got != tt.want {
				t.Errorf("result = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQueryGetReq48(t *testing.T) {
	q := ParseQuery("a=1&b=x+y%21&a=2&k&c=%zz&d=a%2")
	tests := []struct {
		name, want string
		ok         bool
	}{
		{"a", "1", true},
		{"b", "x y!", true},
		{"k", "", true},
		{"c", "%zz", true},
		{"d", "a%2", true},
		{"missing", "", false},
	}
	for _, tt := range tests {
		got, ok := q.Get(tt.name)
		if got != tt.want || ok != tt.ok {
			t.Errorf("Get(%q) = %q, %v, want %q, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestQueryResetReusesStorage(t *testing.T) {
	var q Query
	q.Reset("a=1&b=2&c=3")
	q.Reset("x=1")
	if got := q.String(); got != "x=1" {
		t.Fatalf("after Reset = %q", got)
	}
	allocs := testing.AllocsPerRun(100, func() {
		q.Reset("a=1&b=2")
		if _, ok := q.Get("b"); !ok {
			t.Fatal("b missing")
		}
		q.Del("a")
	})
	if allocs != 0 {
		t.Errorf("Reset, Get and Del allocations = %v, want 0", allocs)
	}
	q.Reset("")
	if got := q.String(); got != "" {
		t.Errorf("after Reset(\"\") = %q", got)
	}
}

// TestQueryPropertyP8 checks property P8 over a fixed set of queries and
// names: untouched pairs keep their bytes and order, and after Set(k, v)
// exactly one pair has key k and it decodes to v.
func TestQueryPropertyP8(t *testing.T) {
	queries := []string{"", "a=1&b=2&a=3", "a+b=1&a%20b=2&c", "x=%7e;y&&=v", "%zz&%zz=2"}
	names := []string{"a", "a b", "c", "x", "%zz", "new key"}
	values := []string{"", "9", "x y", "%&=+"}
	for _, raw := range queries {
		for _, name := range names {
			for _, value := range values {
				checkSet(t, raw, name, value)
				checkDel(t, raw, name)
			}
		}
	}
}

// matching returns the segments of q whose key equals name.
func matching(q *Query, name string) (match, other []string) {
	for _, s := range q.segs {
		key := s
		if i := strings.IndexByte(s, '='); i >= 0 {
			key = s[:i]
		}
		if s != "" && keyEquals(key, name) {
			match = append(match, s)
		} else {
			other = append(other, s)
		}
	}
	return match, other
}

func checkSet(t *testing.T, raw, name, value string) {
	t.Helper()
	before := ParseQuery(raw)
	_, untouched := matching(&before, name)
	q := ParseQuery(raw)
	q.Set(name, value)
	match, other := matching(&q, name)
	if len(match) != 1 {
		t.Errorf("%q Set(%q, %q): %d pairs with the key, want 1", raw, name, value, len(match))
		return
	}
	if got, ok := q.Get(name); !ok || got != value {
		t.Errorf("%q Set(%q, %q): Get = %q, %v", raw, name, value, got, ok)
	}
	if !slices.Equal(other, untouched) {
		t.Errorf("%q Set(%q, %q): untouched pairs %q, want %q", raw, name, value, other, untouched)
	}
	// Reparsing the output gives the same answer.
	re := ParseQuery(q.String())
	if got, ok := re.Get(name); !ok || got != value {
		t.Errorf("%q Set(%q, %q): reparsed Get = %q, %v", raw, name, value, got, ok)
	}
}

func checkDel(t *testing.T, raw, name string) {
	t.Helper()
	before := ParseQuery(raw)
	_, untouched := matching(&before, name)
	q := ParseQuery(raw)
	q.Del(name)
	match, other := matching(&q, name)
	if len(match) != 0 {
		t.Errorf("%q Del(%q): %q remain", raw, name, match)
	}
	if !slices.Equal(other, untouched) {
		t.Errorf("%q Del(%q): untouched pairs %q, want %q", raw, name, other, untouched)
	}
}
