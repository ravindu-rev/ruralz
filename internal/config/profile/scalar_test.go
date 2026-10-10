// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// TestResolvePlain covers the YAML 1.2 core schema typing of plain
// scalars (01 req 12, 11 req 15: yes, on, 0777, 0b1, 1_000, 2026-09-23).
func TestResolvePlain(t *testing.T) {
	tests := []struct {
		in   string
		kind tree.Kind
		text string
		b    bool
		code string
	}{
		{in: "", kind: tree.KindNull},
		{in: "~", kind: tree.KindNull},
		{in: "null", kind: tree.KindNull},
		{in: "Null", kind: tree.KindNull},
		{in: "NULL", kind: tree.KindNull},
		{in: "nULL", kind: tree.KindString, text: "nULL"},
		{in: "true", kind: tree.KindBool, b: true},
		{in: "True", kind: tree.KindBool, b: true},
		{in: "TRUE", kind: tree.KindBool, b: true},
		{in: "false", kind: tree.KindBool},
		{in: "False", kind: tree.KindBool},
		{in: "FALSE", kind: tree.KindBool},
		{in: "tRUE", kind: tree.KindString, text: "tRUE"},
		{in: "yes", kind: tree.KindString, text: "yes"},
		{in: "Yes", kind: tree.KindString, text: "Yes"},
		{in: "on", kind: tree.KindString, text: "on"},
		{in: "off", kind: tree.KindString, text: "off"},
		{in: "y", kind: tree.KindString, text: "y"},
		{in: "0777", kind: tree.KindInt, text: "777"},
		{in: "0", kind: tree.KindInt, text: "0"},
		{in: "-0", kind: tree.KindInt, text: "0"},
		{in: "+0", kind: tree.KindInt, text: "0"},
		{in: "000", kind: tree.KindInt, text: "0"},
		{in: "+5", kind: tree.KindInt, text: "5"},
		{in: "-007", kind: tree.KindInt, text: "-7"},
		{in: "0o17", kind: tree.KindInt, text: "15"},
		{in: "0o0", kind: tree.KindInt, text: "0"},
		{in: "0x1F", kind: tree.KindInt, text: "31"},
		{in: "0x1f", kind: tree.KindInt, text: "31"},
		{in: "0x7FFFFFFFFFFFFFFF", kind: tree.KindInt, text: "9223372036854775807"},
		{in: "0x8000000000000000", code: codeSchema},
		{in: "0o777777777777777777777", kind: tree.KindInt, text: "9223372036854775807"},
		{in: "0o1000000000000000000000", code: codeSchema},
		{in: "9223372036854775807", kind: tree.KindInt, text: "9223372036854775807"},
		{in: "-9223372036854775808", kind: tree.KindInt, text: "-9223372036854775808"},
		{in: "9223372036854775808", code: codeSchema},
		{in: "-9223372036854775809", code: codeSchema},
		{in: "00000000000000000000000000001", kind: tree.KindInt, text: "1"},
		{in: "123456789012345678901234567890", code: codeSchema},
		{in: "0o", kind: tree.KindString, text: "0o"},
		{in: "0x", kind: tree.KindString, text: "0x"},
		{in: "0o8", kind: tree.KindString, text: "0o8"},
		{in: "0xG", kind: tree.KindString, text: "0xG"},
		{in: "-0o7", kind: tree.KindString, text: "-0o7"},
		{in: "0b1", kind: tree.KindString, text: "0b1"},
		{in: "1_000", kind: tree.KindString, text: "1_000"},
		{in: "2026-09-23", kind: tree.KindString, text: "2026-09-23"},
		{in: "1e3", kind: tree.KindFloat, text: "1e3"},
		{in: "1E+3", kind: tree.KindFloat, text: "1E+3"},
		{in: "1.5", kind: tree.KindFloat, text: "1.5"},
		{in: ".5", kind: tree.KindFloat, text: "0.5"},
		{in: "-.5", kind: tree.KindFloat, text: "-0.5"},
		{in: "+1.5", kind: tree.KindFloat, text: "1.5"},
		{in: "1.", kind: tree.KindFloat, text: "1"},
		{in: "-1.", kind: tree.KindFloat, text: "-1"},
		{in: "01.5", kind: tree.KindFloat, text: "1.5"},
		{in: "-00.5", kind: tree.KindFloat, text: "-0.5"},
		{in: "00.0", kind: tree.KindFloat, text: "0.0"},
		{in: "1.e3", kind: tree.KindFloat, text: "1e3"},
		{in: "+.5E-3", kind: tree.KindFloat, text: "0.5E-3"},
		{in: "0010e05", kind: tree.KindFloat, text: "10e05"},
		{in: "1e400", kind: tree.KindFloat, text: "1e400"},
		{in: ".", kind: tree.KindString, text: "."},
		{in: "+", kind: tree.KindString, text: "+"},
		{in: "-", kind: tree.KindString, text: "-"},
		{in: "e3", kind: tree.KindString, text: "e3"},
		{in: "1e", kind: tree.KindString, text: "1e"},
		{in: "1e+", kind: tree.KindString, text: "1e+"},
		{in: "1.5.5", kind: tree.KindString, text: "1.5.5"},
		{in: ".e3", kind: tree.KindString, text: ".e3"},
		{in: "1.3.0", kind: tree.KindString, text: "1.3.0"},
		{in: ".inf", code: codeSchema},
		{in: "-.Inf", code: codeSchema},
		{in: "+.INF", code: codeSchema},
		{in: ".nan", code: codeSchema},
		{in: ".NaN", code: codeSchema},
		{in: ".NAN", code: codeSchema},
		{in: "-.nan", kind: tree.KindString, text: "-.nan"},
		{in: ".iNf", kind: tree.KindString, text: ".iNf"},
		{in: "inf", kind: tree.KindString, text: "inf"},
		{in: "hello world", kind: tree.KindString, text: "hello world"},
	}
	for _, tt := range tests {
		got := resolvePlain(tt.in)
		if got.code != tt.code {
			t.Errorf("resolvePlain(%q) code = %q (%s), want %q", tt.in, got.code, got.msg, tt.code)
			continue
		}
		if tt.code != "" {
			if got.msg == "" {
				t.Errorf("resolvePlain(%q): no message", tt.in)
			}
			continue
		}
		if got.kind != tt.kind || got.text != tt.text || got.b != tt.b {
			t.Errorf("resolvePlain(%q) = (%d, %q, %v), want (%d, %q, %v)", tt.in, got.kind, got.text, got.b, tt.kind, tt.text, tt.b)
		}
	}
}

// TestResolveTagged covers core tags forcing a type and mismatches being
// RZ-CFG-001 (01 req 12).
func TestResolveTagged(t *testing.T) {
	tests := []struct {
		tag, in string
		kind    tree.Kind
		text    string
		code    string
	}{
		{tag: "!!str", in: "0777", kind: tree.KindString, text: "0777"},
		{tag: "!!str", in: "true", kind: tree.KindString, text: "true"},
		{tag: "!!str", in: "", kind: tree.KindString, text: ""},
		{tag: "!!int", in: "12", kind: tree.KindInt, text: "12"},
		{tag: "!!int", in: "0x10", kind: tree.KindInt, text: "16"},
		{tag: "!!int", in: "1.5", code: codeParse},
		{tag: "!!int", in: "abc", code: codeParse},
		{tag: "!!int", in: "99999999999999999999", code: codeSchema},
		{tag: "!!float", in: "1", kind: tree.KindFloat, text: "1"},
		{tag: "!!float", in: "007", kind: tree.KindFloat, text: "7"},
		{tag: "!!float", in: ".5", kind: tree.KindFloat, text: "0.5"},
		{tag: "!!float", in: ".inf", code: codeSchema},
		{tag: "!!float", in: ".nan", code: codeSchema},
		{tag: "!!float", in: "0x1F", code: codeParse},
		{tag: "!!bool", in: "true", kind: tree.KindBool},
		{tag: "!!bool", in: "yes", code: codeParse},
		{tag: "!!null", in: "", kind: tree.KindNull},
		{tag: "!!null", in: "~", kind: tree.KindNull},
		{tag: "!!null", in: "a", code: codeParse},
		{tag: "!!map", in: "a", code: codeParse},
		{tag: "!!seq", in: "a", code: codeParse},
	}
	for _, tt := range tests {
		got := resolveTagged(tt.tag, tt.in)
		if got.code != tt.code {
			t.Errorf("resolveTagged(%s, %q) code = %q (%s), want %q", tt.tag, tt.in, got.code, got.msg, tt.code)
			continue
		}
		if tt.code == "" && (got.kind != tt.kind || got.text != tt.text) {
			t.Errorf("resolveTagged(%s, %q) = (%d, %q), want (%d, %q)", tt.tag, tt.in, got.kind, got.text, tt.kind, tt.text)
		}
	}
}

// TestNormalizeFloatIsRFC8259 checks R-64 over a grid of core schema
// floats: the normalized text is an RFC 8259 number of the same exact
// value, so jsonval accepts every float the profile admits.
func TestNormalizeFloatIsRFC8259(t *testing.T) {
	signs := []string{"", "+", "-"}
	ints := []string{"", "0", "00", "1", "01", "007", "10", "123"}
	fracs := []string{"", ".", ".0", ".5", ".50", ".05"}
	exps := []string{"", "e3", "E-3", "e+03", "e0"}
	n := 0
	for _, s := range signs {
		for _, i := range ints {
			for _, f := range fracs {
				for _, e := range exps {
					in := s + i + f + e
					if !isCoreFloat(in) {
						continue
					}
					n++
					out := normalizeFloat(in)
					if !jsonval.ValidNumber(out) {
						t.Errorf("normalizeFloat(%q) = %q, not an RFC 8259 number", in, out)
						continue
					}
					if err := jsonval.CheckNumber(json.Number(out)); err != nil {
						t.Errorf("jsonval.CheckNumber(%q) = %v", out, err)
					}
					want, ok1 := new(big.Rat).SetString(strings.TrimPrefix(fixDot(in), "+"))
					got, ok2 := new(big.Rat).SetString(out)
					if !ok1 || !ok2 || want.Cmp(got) != 0 {
						t.Errorf("normalizeFloat(%q) = %q, value changed", in, out)
					}
				}
			}
		}
	}
	if n < 100 {
		t.Fatalf("checked %d floats, want a full grid", n)
	}
}

// fixDot makes Go's big.Rat parse YAML-only float shapes (".5", "1.").
func fixDot(s string) string {
	s = strings.Replace(s, ".e", "e", 1)
	s = strings.Replace(s, ".E", "E", 1)
	s = strings.TrimSuffix(s, ".")
	if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "-.") || strings.HasPrefix(s, "+.") {
		s = strings.Replace(s, ".", "0.", 1)
	}
	return s
}

// TestClip covers the shortening of text quoted in a message, which never
// splits a UTF-8 sequence (01 req 14: findings carry readable text).
func TestClip(t *testing.T) {
	long := strings.Repeat("é", 40) // 80 bytes
	got := clip(long)
	if !strings.HasSuffix(got, "...") || len(got) > 67 {
		t.Fatalf("clip = %q", got)
	}
	if strings.ContainsRune(strings.TrimSuffix(got, "..."), '�') {
		t.Fatalf("clip split a character: %q", got)
	}
	if clip("short") != "short" {
		t.Fatal("clip changed a short string")
	}
}
