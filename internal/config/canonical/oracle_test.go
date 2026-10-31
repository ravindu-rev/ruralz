// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build go1.27

package canonical

import (
	"bytes"
	"encoding/json/jsontext"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// Differential tests against the standard library's RFC 8785
// implementation, jsontext.Value.Canonicalize. Production code cannot
// import jsontext under go 1.26.0, so it serves only as this go1.27
// oracle; the Go 1.26 floor job skips the file.

// oracle canonicalizes data with jsontext.
func oracle(data []byte) ([]byte, error) {
	v := jsontext.Value(bytes.Clone(data))
	if err := v.Canonicalize(); err != nil {
		return nil, err
	}
	return v, nil
}

// TestOracleGolden: every golden canonical document is a fixed point of
// jsontext's canonicalization.
func TestOracleGolden(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("testdata", "golden", "*.json"))
	if err != nil || len(names) == 0 {
		t.Fatalf("golden files: %v %v", names, err)
	}
	for _, name := range names {
		data, err := os.ReadFile(name) //nolint:gosec // G304: a golden file under testdata.
		if err != nil {
			t.Fatal(err)
		}
		want, err := oracle(data)
		if err != nil {
			t.Fatal(err)
		}
		sameBytes(t, name, data, want)
	}
}

// randomValue writes a random JSON value without null and without
// integer literals beyond 2^53, which jsontext and RFC 8785 round alike
// but which authored configuration refuses.
func randomValue(b *strings.Builder, r *rand.Rand, depth int) {
	switch k := r.IntN(8); {
	case depth > 4 || k < 2:
		quote(b, randomString(r), r)
	case k == 2:
		b.WriteString(strconv.FormatBool(r.IntN(2) == 0))
	case k == 3:
		b.WriteString(strconv.FormatInt(r.Int64N(1<<53)-1<<52, 10))
	case k == 4:
		f := math.Float64frombits(r.Uint64())
		if math.IsNaN(f) || math.IsInf(f, 0) {
			f = r.NormFloat64() * math.Pow(10, float64(r.IntN(40)-20))
		}
		b.WriteString(strconv.FormatFloat(f, []byte("eEfg")[r.IntN(4)], -1, 64))
	case k == 5:
		b.WriteByte('[')
		for i := range r.IntN(4) {
			if i > 0 {
				b.WriteByte(',')
			}
			randomValue(b, r, depth+1)
		}
		b.WriteByte(']')
	default:
		b.WriteByte('{')
		seen := map[string]bool{}
		for i := range r.IntN(5) {
			name := randomString(r)
			if seen[name] {
				continue
			}
			seen[name] = true
			if i > 0 && len(seen) > 1 {
				b.WriteByte(',')
			}
			quote(b, name, r)
			b.WriteByte(':')
			randomValue(b, r, depth+1)
		}
		b.WriteByte('}')
	}
}

// quote writes s as a JSON string, escaping what JSON requires and,
// at random, other characters as \uXXXX in either hex case (surrogate
// pairs for supplementary characters), so the canonical form must undo
// every escape.
func quote(b *strings.Builder, s string, r *rand.Rand) {
	hex := func(u rune) {
		f := "%04x"
		if r.IntN(2) == 0 {
			f = "%04X"
		}
		fmt.Fprintf(b, "\\u"+f, u)
	}
	b.WriteByte('"')
	for _, c := range s {
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		case c < 0x20 || r.IntN(4) == 0:
			if c >= 0x10000 {
				hi, lo := utf16.EncodeRune(c)
				hex(hi)
				hex(lo)
			} else {
				hex(c)
			}
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
}

// randomString returns a short string over a mix of ASCII, controls,
// BMP characters around the surrogate range and supplementary characters.
func randomString(r *rand.Rand) string {
	alphabet := []rune{'a', 'b', 'Z', '0', '/', '"', '\\', '<', '&', 0, 0x1f, 0x7f, 0xe9, 0x2028, 0x20ac, 0xd7ff, 0xe000, 0xfb33, 0xfffd, 0x10000, 0x1f600, 0x10ffff}
	var b strings.Builder
	for range r.IntN(6) {
		c := alphabet[r.IntN(len(alphabet))]
		if utf8.ValidRune(c) {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// TestOracleRandom: AppendNode equals jsontext on 10,000 generated
// values.
func TestOracleRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(39, 8785)) //nolint:gosec // G404: deterministic test data
	compared := 0
	for i := range 10_000 {
		var b strings.Builder
		randomValue(&b, r, 0)
		data := []byte(b.String())
		if compareOracle(t, data) {
			compared++
		}
		if t.Failed() {
			t.Fatalf("value %d: %s", i, data)
		}
	}
	if compared < 9_900 {
		t.Errorf("compared %d of 10,000 values; the generator writes invalid JSON", compared)
	}
}

// compareOracle checks AppendNode against jsontext on one JSON text that
// both accept and reports whether it compared them; texts holding null,
// which the canonical form never writes, must fail with ErrEncode.
func compareOracle(t *testing.T, data []byte) bool {
	t.Helper()
	want, err := oracle(data)
	if err != nil {
		return false
	}
	n, err := jsonNode(data)
	if err != nil {
		return false
	}
	got, err := AppendNode(nil, n)
	if err != nil {
		if !bytes.Contains(want, []byte("null")) && !strings.Contains(err.Error(), "RFC 8785 form") {
			t.Errorf("AppendNode(%q) error = %v; jsontext gives %q", data, err, want)
		}
		return true
	}
	if !bytes.Equal(got, want) {
		t.Errorf("AppendNode(%q) = %q, jsontext %q", data, got, want)
	}
	return true
}

// FuzzJCSDifferential compares AppendNode with jsontext on arbitrary
// JSON.
func FuzzJCSDifferential(f *testing.F) {
	addGolden(f, false)
	for _, s := range []string{`{"b":[1.0,1e21,5e-324],"a":"< >"}`, `[123456789012345678901,-0,1E-7]`, `"😀é"`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		compareOracle(t, data)
	})
}
