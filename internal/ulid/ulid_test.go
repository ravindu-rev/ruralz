// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ulid

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"
)

// fixedReader is an entropy source repeating one byte.
type fixedReader byte

func (r fixedReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

// seqReader returns 0, 1, 2, ... so every byte of the random part is
// distinguishable.
type seqReader struct{ n byte }

func (r *seqReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.n
		r.n++
	}
	return len(p), nil
}

// TestNewDeterministic covers spec 04 requirement 6 and test plan item 2
// "deterministic output with fixed entropy": 48-bit big-endian ms plus 80
// bits of entropy, 26 uppercase Crockford characters.
func TestNewDeterministic(t *testing.T) {
	cases := []struct {
		name    string
		ms      int64
		entropy io.Reader
		want    string
	}{
		{"epoch zero entropy", 0, fixedReader(0), "00000000000000000000000000"},
		{"epoch ones entropy", 0, fixedReader(0xff), "0000000000ZZZZZZZZZZZZZZZZ"},
		{"max time ones", MaxTime, fixedReader(0xff), "7ZZZZZZZZZZZZZZZZZZZZZZZZZ"},
		{"known vector", 1469918176385, fixedReader(0), "01ARYZ6S410000000000000000"},
		{"sequence entropy", 1, &seqReader{}, "0000000001000G40R40M30E209"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := New(time.UnixMilli(tc.ms), tc.entropy)
			if err != nil {
				t.Fatal(err)
			}
			if got := u.String(); got != tc.want {
				t.Errorf("String() = %s, want %s", got, tc.want)
			}
			if got := u.Timestamp(); got != uint64(tc.ms) { //nolint:gosec // G115: test values are non-negative.
				t.Errorf("Timestamp() = %d, want %d", got, tc.ms)
			}
		})
	}
}

// TestLayout checks the byte layout against an independent big.Int
// encoding (spec 04 requirement 6).
func TestLayout(t *testing.T) {
	u, err := New(time.UnixMilli(0x0123456789ab), &seqReader{n: 0xa0})
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9}
	if !bytes.Equal(u[:], wantBytes) {
		t.Fatalf("bytes = %x, want %x", u[:], wantBytes)
	}
	if got, want := u.String(), bigEncode(u); got != want {
		t.Errorf("String() = %s, big.Int reference %s", got, want)
	}
}

// bigEncode is a reference encoder: the 128-bit value in base 32, padded
// to 26 digits.
func bigEncode(u ULID) string {
	n := new(big.Int).SetBytes(u[:])
	var b [EncodedLen]byte
	thirtyTwo := big.NewInt(32)
	mod := new(big.Int)
	for i := EncodedLen - 1; i >= 0; i-- {
		n.DivMod(n, thirtyTwo, mod)
		b[i] = alphabet[mod.Int64()]
	}
	return string(b[:])
}

// TestTimeRoundTrip covers test plan item 2 "time round trip".
func TestTimeRoundTrip(t *testing.T) {
	for _, tm := range []time.Time{
		time.Unix(0, 0),
		time.Date(2026, 9, 26, 12, 34, 56, 789_000_000, time.UTC),
		time.Date(2026, 9, 26, 12, 34, 56, 789_999_999, time.FixedZone("x", 3600)),
		time.UnixMilli(MaxTime),
	} {
		u, err := New(tm, fixedReader(7))
		if err != nil {
			t.Fatal(err)
		}
		want := tm.Truncate(time.Millisecond).UTC()
		if got := u.Time(); !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("Time() = %v, want %v in UTC", got, want)
		}
	}
}

// TestNewErrors covers the 48-bit range and entropy failures.
func TestNewErrors(t *testing.T) {
	cases := []struct {
		name    string
		now     time.Time
		entropy io.Reader
		want    error
	}{
		{"before epoch", time.UnixMilli(-1), fixedReader(0), ErrTimeRange},
		{"after max", time.UnixMilli(MaxTime + 1), fixedReader(0), ErrTimeRange},
		{"short entropy", time.UnixMilli(1), bytes.NewReader(make([]byte, 9)), ErrEntropy},
		{"failing entropy", time.UnixMilli(1), errReader{}, ErrEntropy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := New(tc.now, tc.entropy)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !u.IsZero() {
				t.Errorf("u = %s, want zero on error", u)
			}
		})
	}
	if _, err := New(time.UnixMilli(1), errReader{}); !errors.Is(err, errBoom) {
		t.Errorf("err = %v, want the reader's error wrapped", err)
	}
}

var errBoom = errors.New("boom")

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errBoom }

// TestNewCryptoRand covers the production entropy source (nil selects
// crypto/rand.Reader): distinct values, valid alphabet.
func TestNewCryptoRand(t *testing.T) {
	now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	seen := map[ULID]bool{}
	for range 1000 {
		u, err := New(now, nil)
		if err != nil {
			t.Fatal(err)
		}
		if seen[u] {
			t.Fatalf("duplicate ULID %s", u)
		}
		seen[u] = true
		assertAlphabet(t, u.String())
		if !u.Time().Equal(now) {
			t.Fatalf("Time() = %v, want %v", u.Time(), now)
		}
	}
}

// assertAlphabet covers test plan item 2 "26-char alphabet".
func assertAlphabet(t *testing.T, s string) {
	t.Helper()
	if len(s) != EncodedLen {
		t.Fatalf("len(%q) = %d, want 26", s, len(s))
	}
	for _, c := range s {
		if !strings.ContainsRune(alphabet, c) {
			t.Fatalf("%q contains %q outside the Crockford alphabet", s, c)
		}
	}
}

// TestLexicalOrderEqualsTimeOrder covers test plan item 2 "lexical order
// equals time order for distinct ms".
func TestLexicalOrderEqualsTimeOrder(t *testing.T) {
	var ids []ULID
	for ms := int64(0); ms < 2000; ms += 7 {
		// Descending entropy so the random part alone would sort the
		// other way.
		u, err := New(time.UnixMilli(ms*1_000_003), fixedReader(byte(255-ms%256)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, u)
	}
	strs := make([]string, len(ids))
	for i, u := range ids {
		strs[i] = u.String()
	}
	if !sort.StringsAreSorted(strs) {
		t.Fatal("text forms are not in time order")
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1].Compare(ids[i]) != -1 || ids[i].Compare(ids[i-1]) != 1 {
			t.Fatalf("Compare disagrees with time order at %d", i)
		}
		if self := ids[i]; ids[i].Compare(self) != 0 {
			t.Fatal("Compare(self) != 0")
		}
	}
}

// TestParse covers test plan item 2: lowercase accepted, I L O U
// rejected, overflow first char > 7 rejected, wrong length rejected.
func TestParse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // canonical form; empty when an error is expected
		err  error
	}{
		{"upper", "01ARYZ6S41TSV4RRFFQ69G5FAV", "01ARYZ6S41TSV4RRFFQ69G5FAV", nil},
		{"lower", "01aryz6s41tsv4rrffq69g5fav", "01ARYZ6S41TSV4RRFFQ69G5FAV", nil},
		{"mixed", "01aRyZ6S41tSv4RRFfq69G5FAv", "01ARYZ6S41TSV4RRFFQ69G5FAV", nil},
		{"zero", "00000000000000000000000000", "00000000000000000000000000", nil},
		{"max", "7ZZZZZZZZZZZZZZZZZZZZZZZZZ", "7ZZZZZZZZZZZZZZZZZZZZZZZZZ", nil},
		{"overflow 8", "8ZZZZZZZZZZZZZZZZZZZZZZZZZ", "", ErrOverflow},
		{"overflow Z", "Z0000000000000000000000000", "", ErrOverflow},
		{"letter I", "01ARYZ6S41TSV4RRFFQ69G5FAI", "", ErrInvalidChar},
		{"letter i", "01ARYZ6S41TSV4RRFFQ69G5FAi", "", ErrInvalidChar},
		{"letter L", "01ARYZ6S41TSV4RRFFQ69G5FAL", "", ErrInvalidChar},
		{"letter l", "0lARYZ6S41TSV4RRFFQ69G5FAV", "", ErrInvalidChar},
		{"letter O", "O1ARYZ6S41TSV4RRFFQ69G5FAV", "", ErrInvalidChar},
		{"letter o", "01ARYZ6S41TSV4RRFFQ69G5FAo", "", ErrInvalidChar},
		{"letter U", "01ARYZ6S41TSV4RRFFQ69G5FAU", "", ErrInvalidChar},
		{"letter u", "01ARYZ6S41TSV4RRFFQ69G5FAu", "", ErrInvalidChar},
		{"hyphen", "01ARYZ6S41TSV4RR-FQ69G5FAV", "", ErrInvalidChar},
		{"non-ASCII", "01ARYZ6S41TSV4RRFFQ69G5F\xc3\x80", "", ErrInvalidChar},
		{"NUL", "01ARYZ6S41TSV4RRFFQ69G5FA\x00", "", ErrInvalidChar},
		{"short", "01ARYZ6S41TSV4RRFFQ69G5FA", "", ErrLength},
		{"long", "01ARYZ6S41TSV4RRFFQ69G5FAVV", "", ErrLength},
		{"empty", "", "", ErrLength},
		{"trailing newline", "01ARYZ6S41TSV4RRFFQ69G5FAV\n", "", ErrLength},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := Parse(tc.in)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("Parse(%q) err = %v, want %v", tc.in, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if got := u.String(); got != tc.want {
				t.Errorf("Parse(%q).String() = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestEveryCharacter decodes each alphabet character in both cases at
// every position and rejects every other byte.
func TestEveryCharacter(t *testing.T) {
	for v := range 32 {
		for _, c := range []byte{alphabet[v], lower(alphabet[v])} {
			if got := decodeChar(c); got != v {
				t.Errorf("decodeChar(%q) = %d, want %d", c, got, v)
			}
		}
	}
	for c := range 256 {
		b := byte(c)
		if strings.IndexByte(alphabet, upper(b)) >= 0 {
			continue
		}
		if got := decodeChar(b); got != -1 {
			t.Errorf("decodeChar(%q) = %d, want -1", b, got)
		}
	}
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func upper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}

// TestTextAndJSON covers the TextMarshaler, TextAppender and JSON forms.
func TestTextAndJSON(t *testing.T) {
	u, err := Parse("01ARYZ6S41TSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatal(err)
	}
	b, err := u.AppendText([]byte("id="))
	if err != nil || string(b) != "id=01ARYZ6S41TSV4RRFFQ69G5FAV" {
		t.Fatalf("AppendText = %q, %v", b, err)
	}
	j, err := json.Marshal(struct{ ID ULID }{u})
	if err != nil {
		t.Fatal(err)
	}
	if string(j) != `{"ID":"01ARYZ6S41TSV4RRFFQ69G5FAV"}` {
		t.Fatalf("json = %s", j)
	}
	var back struct{ ID ULID }
	if err := json.Unmarshal([]byte(`{"ID":"01aryz6s41tsv4rrffq69g5fav"}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != u {
		t.Errorf("round trip = %s, want %s", back.ID, u)
	}
	var bad ULID
	if err := bad.UnmarshalText([]byte("01ARYZ6S41TSV4RRFFQ69G5FAI")); !errors.Is(err, ErrInvalidChar) {
		t.Errorf("UnmarshalText err = %v, want ErrInvalidChar", err)
	}
	if !bad.IsZero() {
		t.Error("UnmarshalText modified the receiver on error")
	}
}

// FuzzParse checks that every accepted string round-trips to its
// uppercase form and that every ULID re-parses to itself.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"01ARYZ6S41TSV4RRFFQ69G5FAV", "01aryz6s41tsv4rrffq69g5fav", "7ZZZZZZZZZZZZZZZZZZZZZZZZZ",
		"8ZZZZZZZZZZZZZZZZZZZZZZZZZ", "01ARYZ6S41TSV4RRFFQ69G5FAI", "", "0",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, err := Parse(s)
		if err != nil {
			if !u.IsZero() {
				t.Fatalf("Parse(%q) returned %s with error %v", s, u, err)
			}
			return
		}
		if got := u.String(); got != strings.ToUpper(s) {
			t.Fatalf("Parse(%q).String() = %s", s, got)
		}
		again, err := Parse(u.String())
		if err != nil || again != u {
			t.Fatalf("re-parse of %s = %s, %v", u, again, err)
		}
		if bigEncode(u) != u.String() {
			t.Fatalf("encoding of %x disagrees with the reference", u[:])
		}
	})
}

// BenchmarkNew measures one node.id-style generation on crypto/rand.
func BenchmarkNew(b *testing.B) {
	now := time.Now()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := New(now, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkString measures the encoder.
func BenchmarkString(b *testing.B) {
	u, err := New(time.UnixMilli(1469918176385), fixedReader(0xa5))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = u.String()
	}
}

// BenchmarkParse measures the decoder.
func BenchmarkParse(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse("01ARYZ6S41TSV4RRFFQ69G5FAV"); err != nil {
			b.Fatal(err)
		}
	}
}
