// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canary

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Tests for 11 test plan item 10 (canary: every encoding found at the right
// offset; no false positives on random data over 10,000 cases) and the
// encodings 11 req 39 lists: raw, standard and URL-safe base64, hex,
// URL-encoded and JSON-escaped.

// fixedValue is a canary value that is stable across runs.
const fixedValue = Prefix + "0123456789abcdef0123456789abcdef"

func TestNewFormat(t *testing.T) { // 11 req 39: rzcanary-<32 hex from crypto/rand>
	re := regexp.MustCompile(`^rzcanary-[0-9a-f]{32}$`)
	seen := map[string]bool{}
	for range 100 {
		c, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(c.Value) {
			t.Fatalf("New() = %q, want rzcanary-<32 hex>", c.Value)
		}
		if seen[c.Value] {
			t.Fatalf("New() repeated %q", c.Value)
		}
		seen[c.Value] = true
	}
}

// percentAll percent-encodes every byte.
func percentAll(s string, upper bool) string {
	var b strings.Builder
	for i := range len(s) {
		if upper {
			fmt.Fprintf(&b, "%%%02X", s[i])
		} else {
			fmt.Fprintf(&b, "%%%02x", s[i])
		}
	}
	return b.String()
}

// jsonEscapeAll \u-escapes every byte.
func jsonEscapeAll(s string) string {
	var b strings.Builder
	for i := range len(s) {
		fmt.Fprintf(&b, `\u%04x`, s[i])
	}
	return b.String()
}

func TestScanEncodingsAtOffsets(t *testing.T) { // 11 test plan item 10; 11 req 39
	c := Canary{Value: fixedValue}
	v := c.Value
	mixedHex := []byte(hex.EncodeToString([]byte(v)))
	for i := range mixedHex {
		if i%2 == 0 {
			mixedHex[i] = strings.ToUpper(string(mixedHex[i]))[0]
		}
	}
	halfJSON := v[:10] + jsonEscapeAll(v[10:12]) + v[12:]
	halfURL := v[:5] + percentAll(v[5:6], true) + v[6:]
	tests := []struct {
		name    string
		encoded string
		enc     string
		skip    int // offset of the pattern inside encoded
	}{
		{"raw", v, EncodingRaw, 0},
		{"hex lower", hex.EncodeToString([]byte(v)), EncodingHex, 0},
		{"hex upper", strings.ToUpper(hex.EncodeToString([]byte(v))), EncodingHex, 0},
		{"hex mixed", string(mixedHex), EncodingHex, 0},
		{"url every byte upper", percentAll(v, true), EncodingURL, 0},
		{"url every byte lower", percentAll(v, false), EncodingURL, 0},
		{"url one byte", halfURL, EncodingURL, 0},
		{"json every byte", jsonEscapeAll(v), EncodingJSON, 0},
		{"json two bytes", halfJSON, EncodingJSON, 0},
		{"base64 std padded aligned", base64.StdEncoding.EncodeToString([]byte(v)), EncodingBase64, 0},
		{"base64 url raw aligned", base64.RawURLEncoding.EncodeToString([]byte(v)), EncodingBase64, 0},
		{"base64 std shifted 1", base64.StdEncoding.EncodeToString([]byte("u:" + v + "!")), EncodingBase64, 3},
		{"base64 std shifted 2", base64.StdEncoding.EncodeToString([]byte("u" + v)), EncodingBase64, 2},
		{"base64 basic auth", base64.StdEncoding.EncodeToString([]byte("user:" + v)), EncodingBase64, 7},
	}
	for _, tt := range tests {
		for _, prefix := range []string{"", "x", "log line: ", strings.Repeat("-", 1000)} {
			t.Run(fmt.Sprintf("%s/prefix%d", tt.name, len(prefix)), func(t *testing.T) {
				data := []byte(prefix + tt.encoded + " trailer")
				got := c.Scan("src", data)
				want := Finding{Source: "src", Encoding: tt.enc, Offset: len(prefix) + tt.skip}
				found := false
				for _, f := range got {
					if f == want {
						found = true
					} else if f.Encoding == EncodingRaw && tt.enc != EncodingRaw {
						// Escaped forms that leave a raw run are reported
						// once as raw only when the whole value is literal.
						t.Errorf("unexpected raw finding %v", f)
					}
				}
				if !found {
					t.Fatalf("Scan(%q) = %v, want %v", tt.encoded, got, want)
				}
			})
		}
	}
}

func TestScanBase64EveryAlignment(t *testing.T) { // 11 req 39: base64 embedded in longer values
	c := Canary{Value: fixedValue}
	skip := [3]int{0, 2, 3}
	for k := range 3 {
		for tail := range 3 {
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding} {
				plain := strings.Repeat("\xff", k) + c.Value + strings.Repeat("\xfe", tail)
				data := []byte("[" + enc.EncodeToString([]byte(plain)) + "]")
				got := c.Scan("s", data)
				want := Finding{Source: "s", Encoding: EncodingBase64, Offset: 1 + skip[k]}
				if len(got) != 1 || got[0] != want {
					t.Fatalf("k=%d tail=%d: Scan = %v, want [%v]", k, tail, got, want)
				}
			}
		}
	}
}

func TestScanBase64URLDistinct(t *testing.T) {
	// A value whose base64 uses '+' and '/' in the standard alphabet is
	// reported as base64url when encoded with the URL-safe alphabet.
	c := Canary{Value: "\xfb\xff\xbf" + "secretsecret"}
	if std, u := base64.StdEncoding.EncodeToString([]byte(c.Value)), base64.URLEncoding.EncodeToString([]byte(c.Value)); std == u {
		t.Fatal("test value does not exercise the URL-safe alphabet")
	}
	data := []byte(base64.URLEncoding.EncodeToString([]byte(c.Value)))
	got := c.Scan("s", data)
	if len(got) != 1 || got[0].Encoding != EncodingBase64URL || got[0].Offset != 0 {
		t.Fatalf("Scan = %v, want one base64url finding at 0", got)
	}
}

func TestScanMultipleAndOrdered(t *testing.T) {
	c := Canary{Value: fixedValue}
	data := []byte(c.Value + " " + hex.EncodeToString([]byte(c.Value)) + " " + c.Value)
	got := c.Scan("s", data)
	if len(got) != 3 {
		t.Fatalf("Scan = %v, want 3 findings", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Offset > got[i].Offset {
			t.Fatalf("findings out of order: %v", got)
		}
	}
	if got[0].Encoding != EncodingRaw || got[1].Encoding != EncodingHex || got[2].Encoding != EncodingRaw {
		t.Fatalf("encodings = %v", got)
	}
}

func TestScanJSONMarshal(t *testing.T) {
	// encoding/json leaves the canary alphabet literal: found as raw.
	c := Canary{Value: fixedValue}
	b, err := json.Marshal(map[string]string{"secret": c.Value})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Scan("s", b); len(got) != 1 || got[0].Encoding != EncodingRaw {
		t.Fatalf("Scan = %v", got)
	}
	// url.QueryEscape likewise.
	if got := c.Scan("s", []byte("a="+url.QueryEscape(c.Value))); len(got) != 1 || got[0].Encoding != EncodingRaw {
		t.Fatalf("Scan = %v", got)
	}
}

func TestScanJSONSurrogatesAndShortEscapes(t *testing.T) {
	c := Canary{Value: "a\U0001F600b/c\n"}
	data := []byte(`"a😀b\/c\n"`)
	got := c.Scan("s", data)
	if len(got) != 1 || got[0].Encoding != EncodingJSON || got[0].Offset != 1 {
		t.Fatalf("Scan = %v", got)
	}
	// Truncated and invalid escapes pass through without a match or panic.
	for _, s := range []string{`\`, `\u12`, `\uzzzz`, `\ud83d`, `\ud83dA`, `%`, `%4`, `%zz`} {
		if got := c.Scan("s", []byte(s)); len(got) != 0 {
			t.Fatalf("Scan(%q) = %v", s, got)
		}
	}
}

func TestScanEmptyValue(t *testing.T) {
	if got := (Canary{}).Scan("s", []byte("anything")); got != nil {
		t.Fatalf("Scan with empty value = %v", got)
	}
}

func TestNoFalsePositives(t *testing.T) { // 11 test plan item 10: 10,000 random cases
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	other, err := New()
	if err != nil {
		t.Fatal(err)
	}
	src := rand.NewChaCha8([32]byte{1})
	rng := rand.New(src) //nolint:gosec // G404: deterministic test data
	alphabet := []byte("0123456789abcdefABCDEF%\\u-rzcanary")
	for i := range 10000 {
		n := 64 + rng.IntN(512)
		data := make([]byte, n)
		switch i % 3 {
		case 0: // arbitrary bytes
			_, _ = src.Read(data)
		case 1: // bytes from the alphabets the decoders act on
			for j := range data {
				data[j] = alphabet[rng.IntN(len(alphabet))]
			}
		default: // a different canary in every encoding
			data = append(data, other.Value...)
			data = append(data, base64.StdEncoding.EncodeToString([]byte(other.Value))...)
			data = append(data, hex.EncodeToString([]byte(other.Value))...)
			data = append(data, percentAll(other.Value, true)...)
			data = append(data, jsonEscapeAll(other.Value)...)
		}
		if got := c.Scan("random", data); len(got) != 0 {
			t.Fatalf("case %d: false positive %v", i, got)
		}
	}
}

func TestScanFileAndDir(t *testing.T) {
	c := Canary{Value: fixedValue}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clean.log"), []byte("nothing here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	leak := filepath.Join(dir, "sub", "node.err")
	if err := os.WriteFile(leak, []byte("x="+hex.EncodeToString([]byte(c.Value))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(leak, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := c.ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Finding{Source: "sub/node.err", Encoding: EncodingHex, Offset: 2}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("ScanDir = %v, want [%v] (symbolic links are not followed)", got, want)
	}
	if !strings.Contains(got[0].String(), "sub/node.err: canary (hex) at byte 2") || strings.Contains(got[0].String(), c.Value) {
		t.Fatalf("String() = %q", got[0].String())
	}
	found, err := c.ScanFile(leak)
	if err != nil || len(found) != 1 || found[0].Source != leak {
		t.Fatalf("ScanFile = %v, %v", found, err)
	}
	if _, err := c.ScanFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("ScanFile on a missing file: want error")
	}
	if _, err := c.ScanDir(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("ScanDir on a missing directory: want error")
	}
}

func FuzzScan(f *testing.F) {
	c := Canary{Value: fixedValue}
	f.Add([]byte(c.Value), 0)
	f.Add([]byte(percentAll(c.Value, false)), 3)
	f.Add([]byte(jsonEscapeAll(c.Value)), 1)
	f.Add([]byte(`😀%%\u`), 2)
	f.Fuzz(func(t *testing.T, data []byte, at int) {
		// Scanning never panics, and a raw canary inserted anywhere is
		// always found at its offset.
		_ = c.Scan("fuzz", data)
		if at < 0 {
			at = -at
		}
		at %= len(data) + 1
		in := append(append(append([]byte{}, data[:at]...), c.Value...), data[at:]...)
		for _, fd := range c.Scan("fuzz", in) {
			if fd.Encoding == EncodingRaw && fd.Offset == at {
				return
			}
		}
		t.Fatalf("raw canary at %d not found", at)
	})
}

func BenchmarkScan(b *testing.B) {
	c := Canary{Value: fixedValue}
	line := []byte(`{"level":"INFO","msg":"request","path":"/orders/42?x=%2F","ua":"curl\/8.5"}` + "\n")
	var data []byte
	for len(data) < 1<<20 {
		data = append(data, line...)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if got := c.Scan("bench", data); len(got) != 0 {
			b.Fatal(got)
		}
	}
}
