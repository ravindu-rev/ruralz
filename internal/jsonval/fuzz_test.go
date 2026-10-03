// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"testing"
	"unicode/utf8"
)

// Fuzz targets (07 test plan F5; WP-02 FuzzDecode). Seeds are the JSON
// conformance corpus and the benchmark document; the oracle is: no panic,
// agreement with encoding/json on inputs without duplicate names, typed
// errors for the rest, exact cost accounting and round trips.

// v1Decode decodes data with encoding/json into its UseNumber data model.
func v1Decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

func addSeeds(f *testing.F) {
	for _, c := range conformanceCases() {
		if len(c.input) < 1024 {
			f.Add([]byte(c.input))
		}
	}
	b, err := readOrdersFile()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b)
	f.Add([]byte(`{"a":{"a":1},"b":[{"a":1,"a":2}]}`))
	f.Add([]byte(`{"k0":0,"k1":1,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9,"k10":10,"k11":11,"k12":12,"k13":13,"k14":14,"k15":15,"k16":16,"k3":0}`))
}

// checkRejection checks that a rejection is typed and justified.
func checkRejection(t *testing.T, data []byte, err error) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not *Error", err)
	}
	if e.Offset < 0 || e.Offset > len(data) {
		t.Fatalf("offset %d outside input of %d bytes", e.Offset, len(data))
	}
	switch {
	case errors.Is(e.Kind, ErrSyntax):
		if json.Valid(data) {
			t.Fatalf("syntax error %v, encoding/json accepts %q", err, data)
		}
	case errors.Is(e.Kind, ErrInvalidUTF8):
		if utf8.Valid(data) {
			t.Fatalf("UTF-8 error %v on valid UTF-8 %q", err, data)
		}
	case errors.Is(e.Kind, ErrSurrogate):
		if !bytes.Contains(data[e.Offset:], []byte(`\u`)) {
			t.Fatalf("surrogate error %v without an escape at the offset", err)
		}
	case errors.Is(e.Kind, ErrDepth):
		if data[e.Offset] != '[' && data[e.Offset] != '{' {
			t.Fatalf("depth error %v not at a bracket", err)
		}
	case errors.Is(e.Kind, ErrDuplicateName):
		if e.Other < 0 || e.Other >= e.Offset {
			t.Fatalf("duplicate error %v with first offset %d", err, e.Other)
		}
		a, b := NewScanner(data[e.Other:], ScanOptions{}), NewScanner(data[e.Offset:], ScanOptions{})
		ta, errA := a.Next()
		tb, errB := b.Next()
		if errA != nil || errB != nil || a.Text(ta) != b.Text(tb) {
			t.Fatalf("duplicate error %v on different names", err)
		}
	default:
		t.Fatalf("unexpected error kind %v", e.Kind)
	}
}

func FuzzDecode(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		v, cost, err := Decode(data, Options{})
		verr := Validate(data, ScanOptions{})
		if (err == nil) != (verr == nil) || (err != nil && err.Error() != verr.Error()) {
			t.Fatalf("Decode error %v, Validate error %v", err, verr)
		}
		if lw, _, lwErr := Decode(data, Options{AllowDuplicateNames: true, MapObjects: true}); lwErr == nil {
			// Last wins agrees with encoding/json, which keeps the last
			// value of a duplicate in a map (07 req 58; 03 req 25).
			want, v1err := v1Decode(data)
			if v1err != nil || !reflect.DeepEqual(lw, want) {
				t.Fatalf("last-wins decode %#v, encoding/json %#v (%v)", lw, want, v1err)
			}
		} else if !errors.Is(lwErr, ErrDuplicateName) {
			checkRejection(t, data, lwErr)
		}
		if err != nil {
			checkRejection(t, data, err)
			return
		}
		if !json.Valid(data) {
			t.Fatalf("accepted input encoding/json rejects: %q", data)
		}
		// The map form is exactly the encoding/json data model. It has no
		// *Object name indexes, so it never costs more.
		m, mcost, err := Decode(data, Options{MapObjects: true})
		if err != nil || mcost != Cost(m) || mcost > cost {
			t.Fatalf("map decode: cost %d, Cost %d, object form %d, %v", mcost, Cost(m), cost, err)
		}
		if want, err := v1Decode(data); err != nil || !reflect.DeepEqual(m, want) {
			t.Fatalf("map decode %#v, encoding/json %#v (%v)", m, want, err)
		}
		if !Equal(v, m) {
			t.Fatal("object and map forms differ")
		}
		// Cost is exact, bounded per input byte, and the budget threshold
		// sits at it (07 req 58; 03 req 25).
		if Cost(v) != cost {
			t.Fatalf("cost %d, Cost %d", cost, Cost(v))
		}
		if cost > MaxCostPerByte*int64(len(data)) {
			t.Fatalf("cost %d over MaxCostPerByte for %d bytes", cost, len(data))
		}
		if _, _, err := Decode(data, Options{MaxCost: cost}); err != nil {
			t.Fatalf("budget equal to the cost: %v", err)
		}
		if _, _, err := Decode(data, Options{MaxCost: cost - 1}); cost > 1 && !errors.Is(err, ErrTooLarge) {
			t.Fatalf("budget below the cost: %v", err)
		}
		// Cost grows with the document.
		wrapped := append(append([]byte("["), data...), ","...)
		wrapped = append(append(wrapped, data...), ']')
		if _, wcost, err := Decode(wrapped, Options{}); err == nil && wcost != 2*cost+CostValue+CostArray {
			t.Fatalf("wrapped cost %d, want %d", wcost, 2*cost+CostValue+CostArray)
		} else if err != nil && !errors.Is(err, ErrDepth) {
			t.Fatalf("wrapped: %v", err)
		}
		// Round trip through the encoder (07 P2) in every order.
		for _, e := range []Encoder{{}, {Options: EncodeOptions{Order: OrderInsertion}}, {Options: EncodeOptions{Order: OrderUTF16}}} {
			b, err := e.Append(nil, v)
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			w, _, err := Decode(b, Options{})
			if err != nil || !Equal(v, w) {
				t.Fatalf("round trip %q -> %q: %v", data, b, err)
			}
			// The output limit holds exactly at the output length.
			e.Options.MaxBytes = len(b)
			if _, err := e.Append(nil, v); err != nil {
				t.Fatalf("MaxBytes %d: %v", len(b), err)
			}
			e.Options.MaxBytes = len(b) - 1
			if _, err := e.Append(nil, v); len(b) > 1 && !errors.Is(err, ErrOutputTooLarge) {
				t.Fatalf("MaxBytes %d: %v", len(b)-1, err)
			}
		}
		// The canonical form exists unless a literal overflows a double,
		// and it is idempotent (02 req 26): parse, canonicalize, parse again
		// gives identical bytes, including for float literals such as 11e17
		// whose canonical form is an integer beyond ±(2^53−1).
		if c, err := AppendCanonical(nil, v); err == nil {
			c2, err := AppendCanonical(nil, mustDecode(t, string(c)))
			if err != nil || !bytes.Equal(c, c2) {
				t.Fatalf("canonical %q then %q: %v", c, c2, err)
			}
		} else if !errors.Is(err, ErrNumberRange) || !hasOverflowingNumber(t, data) {
			t.Fatalf("AppendCanonical: %v", err)
		}
	})
}

// hasOverflowingNumber reports whether valid JSON data holds a number
// literal that overflows a double, the only literal without a canonical
// form.
func hasOverflowingNumber(t *testing.T, data []byte) bool {
	t.Helper()
	s := NewScanner(data, ScanOptions{MaxDepth: MaxDepthLimit, AllowDuplicateNames: true})
	for {
		tk, err := s.Next()
		if err != nil {
			return false
		}
		if tk.Kind == KindNumber {
			if _, ok := finiteDouble(string(s.Bytes(tk))); !ok {
				return true
			}
		}
	}
}

func FuzzAppendFloat(f *testing.F) {
	for _, x := range []float64{0, 1, -1, 0.1, 1e21, 1e-7, 5e-324, math.MaxFloat64, 333333333.33333325} {
		f.Add(math.Float64bits(x))
	}
	f.Fuzz(func(t *testing.T, bits uint64) {
		x := math.Float64frombits(bits)
		b, err := AppendFloat(nil, x)
		if math.IsNaN(x) || math.IsInf(x, 0) {
			if !errors.Is(err, ErrNumberRange) {
				t.Fatalf("%v: err = %v", x, err)
			}
			return
		}
		if err != nil || !ValidNumber(string(b)) {
			t.Fatalf("%v: %q, %v", x, b, err)
		}
		y, err := strconv.ParseFloat(string(b), 64)
		if err != nil || (y != x && !(x == 0 && y == 0)) {
			t.Fatalf("%v -> %s -> %v", x, b, y)
		}
		// A float literal gets the same canonical form.
		lit := strconv.FormatFloat(x, 'e', -1, 64)
		c, err := AppendCanonicalNumber(nil, json.Number(lit))
		if err != nil || !bytes.Equal(c, b) {
			t.Fatalf("AppendCanonicalNumber(%s) = %s, %v; AppendFloat %s", lit, c, err, b)
		}
	})
}

func FuzzCompareUTF16(f *testing.F) {
	f.Add("a", "b")
	f.Add("\U0001F600", "דּ")
	f.Add("x\xff", "x\xfe")
	f.Fuzz(func(t *testing.T, a, b string) {
		ab, ba := CompareUTF16(a, b), CompareUTF16(b, a)
		if ab != -ba || CompareUTF16(a, a) != 0 {
			t.Fatalf("not antisymmetric: %d %d", ab, ba)
		}
		if (ab == 0) != (a == b) {
			t.Fatalf("CompareUTF16(%q, %q) = 0 for different strings", a, b)
		}
		if utf8.ValidString(a) && utf8.ValidString(b) {
			if ref := referenceCompareUTF16(a, b); ab != ref {
				t.Fatalf("CompareUTF16(%q, %q) = %d, reference %d", a, b, ab, ref)
			}
		}
	})
}
