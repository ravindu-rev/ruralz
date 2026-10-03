// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
)

// Tests for the RFC 8785 number form: 02 req 16 (literals, I-JSON integer
// range, non-finite numbers) and 02 req 24 (ECMAScript
// Number.prototype.toString), 02 test plan item 3, RFC 8785 Appendix B.

func TestAppendFloatRFC8785AppendixB(t *testing.T) {
	cases := []struct {
		bits uint64
		want string
	}{
		{0x0000000000000000, "0"},
		{0x8000000000000000, "0"},
		{0x0000000000000001, "5e-324"},
		{0x8000000000000001, "-5e-324"},
		{0x7fefffffffffffff, "1.7976931348623157e+308"},
		{0xffefffffffffffff, "-1.7976931348623157e+308"},
		{0x4340000000000000, "9007199254740992"},
		{0xc340000000000000, "-9007199254740992"},
		{0x4430000000000000, "295147905179352830000"},
		{0x44b52d02c7e14af5, "9.999999999999997e+22"},
		{0x44b52d02c7e14af6, "1e+23"},
		{0x44b52d02c7e14af7, "1.0000000000000001e+23"},
		{0x444b1ae4d6e2ef4e, "999999999999999700000"},
		{0x444b1ae4d6e2ef4f, "999999999999999900000"},
		{0x444b1ae4d6e2ef50, "1e+21"},
		{0x3eb0c6f7a0b5ed8c, "9.999999999999997e-7"},
		{0x3eb0c6f7a0b5ed8d, "0.000001"},
		{0x41b3de4355555553, "333333333.3333332"},
		{0x41b3de4355555554, "333333333.33333325"},
		{0x41b3de4355555555, "333333333.3333333"},
		{0x41b3de4355555556, "333333333.3333334"},
		{0x41b3de4355555557, "333333333.33333343"},
		{0xbecbf647612f3696, "-0.0000033333333333333333"},
		{0x43143ff3c1cb0959, "1424953923781206.2"},
	}
	for _, c := range cases {
		got, err := AppendFloat(nil, math.Float64frombits(c.bits))
		if err != nil || string(got) != c.want {
			t.Fatalf("%#016x: AppendFloat = %s, %v; want %s", c.bits, got, err, c.want)
		}
	}
	for _, bits := range []uint64{0x7fffffffffffffff, 0x7ff0000000000000, 0xfff0000000000000} {
		if _, err := AppendFloat(nil, math.Float64frombits(bits)); !errors.Is(err, ErrNumberRange) {
			t.Fatalf("%#016x: err = %v, want ErrNumberRange", bits, err)
		}
	}
}

func TestAppendFloatForms(t *testing.T) {
	// The four ECMAScript branches around their boundaries.
	for f, want := range map[float64]string{
		1:        "1",
		-1:       "-1",
		1e20:     "100000000000000000000",
		1e21:     "1e+21",
		1.5e21:   "1.5e+21",
		123.456:  "123.456",
		0.1:      "0.1",
		1e-6:     "0.000001",
		1.5e-6:   "0.0000015",
		1e-7:     "1e-7",
		1.25e-7:  "1.25e-7",
		-2.5e-10: "-2.5e-10",
		5e300:    "5e+300",
	} {
		got, err := AppendFloat([]byte("x"), f)
		if err != nil || string(got) != "x"+want {
			t.Fatalf("AppendFloat(%v) = %s, %v; want x%s", f, got, err, want)
		}
	}
}

func TestAppendFloatRoundTrip(t *testing.T) {
	// Shortest digits always read back to the same double.
	r := rand.New(rand.NewPCG(8785, 1)) //nolint:gosec // G404: deterministic test data
	for range 100000 {
		f := math.Float64frombits(r.Uint64())
		if math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		b, err := AppendFloat(nil, f)
		if err != nil {
			t.Fatal(err)
		}
		g, err := strconv.ParseFloat(string(b), 64)
		if err != nil || (g != f && !(f == 0 && g == 0)) {
			t.Fatalf("%v -> %s -> %v, %v", f, b, g, err)
		}
		if !ValidNumber(string(b)) {
			t.Fatalf("%s is not a JSON number", b)
		}
	}
}

func TestAppendCanonicalNumber(t *testing.T) {
	// 02 test plan item 3 (forms) and 02 req 26: the canonical form is total
	// over finite literals, reading integer literals beyond ±(2^53−1) as
	// doubles as encoding/json/jsontext does, and canonicalizing its output
	// gives the same bytes.
	cases := []struct {
		lit  string
		want string
		err  error
	}{
		{"0", "0", nil},
		{"-0", "0", nil},
		{"1.0", "1", nil},
		{"100", "100", nil},
		{"1E2", "100", nil},
		{"0.05", "0.05", nil},
		{"0.000001", "0.000001", nil},
		{"1e-7", "1e-7", nil},
		{"1e21", "1e+21", nil},
		{"1e20", "100000000000000000000", nil},
		{"333333333.3333333", "333333333.3333333", nil},
		{"5e-324", "5e-324", nil},
		{"1.7976931348623157e308", "1.7976931348623157e+308", nil},
		{"1.2345678901234568e20", "123456789012345680000", nil},
		{"-0.0", "0", nil},
		{"1e-400", "0", nil},
		{"9007199254740991", "9007199254740991", nil},
		{"-9007199254740991", "-9007199254740991", nil},
		{"9007199254740992", "9007199254740992", nil},
		{"-9007199254740992", "-9007199254740992", nil},
		{"9007199254740993", "9007199254740992", nil},
		{"-9007199254740993", "-9007199254740992", nil},
		{"123456789012345678901", "123456789012345680000", nil},
		{"18446744073709551616", "18446744073709552000", nil},
		{"-0000", "", ErrInvalidNumber},
		{"1" + strings.Repeat("0", 400), "", ErrNumberRange},
		{"1e400", "", ErrNumberRange},
		{"-1e400", "", ErrNumberRange},
		{"NaN", "", ErrInvalidNumber},
		{"Infinity", "", ErrInvalidNumber},
		{"01", "", ErrInvalidNumber},
		{"1.", "", ErrInvalidNumber},
		{"+1", "", ErrInvalidNumber},
		{"", "", ErrInvalidNumber},
		{"1 ", "", ErrInvalidNumber},
	}
	for _, c := range cases {
		got, err := AppendCanonicalNumber(nil, json.Number(c.lit))
		if !errors.Is(err, c.err) || (err == nil && string(got) != c.want) {
			t.Fatalf("AppendCanonicalNumber(%q) = %q, %v; want %q, %v", c.lit, got, err, c.want, c.err)
		}
		if err != nil {
			continue
		}
		again, err := AppendCanonicalNumber(nil, json.Number(got))
		if err != nil || string(again) != string(got) {
			t.Fatalf("AppendCanonicalNumber(%q) = %q, %v; not idempotent", got, again, err)
		}
	}
}

func TestCheckNumber(t *testing.T) {
	// 02 req 16 and 02 test plan item 3: integer literals outside ±(2^53−1)
	// and overflowing literals are refused (stage G reports RZ-CFG-005);
	// float literals whose canonical form is a large integer are fine.
	cases := []struct {
		lit string
		err error
	}{
		{"0", nil},
		{"-0", nil},
		{"9007199254740991", nil},
		{"-9007199254740991", nil},
		{"9007199254740992", ErrNumberRange},
		{"-9007199254740992", ErrNumberRange},
		{"123456789012345678901", ErrNumberRange},
		{"18446744073709551616", ErrNumberRange},
		{"1.2345678901234568e20", nil},
		{"9007199254740993.0", nil},
		{"1e21", nil},
		{"1.7976931348623157e308", nil},
		{"1e-400", nil},
		{"1e400", ErrNumberRange},
		{"-1e400", ErrNumberRange},
		{"1" + strings.Repeat("0", 400) + ".0", ErrNumberRange},
		{"01", ErrInvalidNumber},
		{"NaN", ErrInvalidNumber},
		{"", ErrInvalidNumber},
	}
	for _, c := range cases {
		if err := CheckNumber(json.Number(c.lit)); !errors.Is(err, c.err) {
			t.Fatalf("CheckNumber(%q) = %v, want %v", c.lit, err, c.err)
		}
		// Whatever CheckNumber accepts has a canonical form.
		if c.err == nil {
			if _, err := AppendCanonicalNumber(nil, json.Number(c.lit)); err != nil {
				t.Fatalf("AppendCanonicalNumber(%q): %v", c.lit, err)
			}
		}
	}
}

func TestValidNumber(t *testing.T) {
	for s, ok := range map[string]bool{
		"0": true, "-0": true, "12": true, "1.5": true, "1e5": true, "1E+5": true, "-1.5e-5": true,
		"": false, "-": false, "01": false, "1.": false, ".5": false, "1e": false, "1e+": false,
		"+1": false, "0x1": false, "1_000": false, " 1": false, "1 ": false, "NaN": false,
	} {
		if ValidNumber(s) != ok {
			t.Fatalf("ValidNumber(%q) = %v", s, !ok)
		}
	}
}
