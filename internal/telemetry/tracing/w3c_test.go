// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func mustHex16(tb testing.TB, s string) [16]byte {
	tb.Helper()
	var b [16]byte
	if _, err := hex.Decode(b[:], []byte(s)); err != nil {
		tb.Fatal(err)
	}
	return b
}

func mustHex8(tb testing.TB, s string) [8]byte {
	tb.Helper()
	var b [8]byte
	if _, err := hex.Decode(b[:], []byte(s)); err != nil {
		tb.Fatal(err)
	}
	return b
}

// TestParseTraceparent is spec 09 test 7 (req 27).
func TestParseTraceparent(t *testing.T) {
	tid, pid := testTraceIDHex, testParentIDHex
	zeroTID := strings.Repeat("0", 32)
	zeroPID := strings.Repeat("0", 16)
	for _, tc := range []struct {
		name  string
		in    string
		ok    bool
		flags byte
	}{
		{"version 00 sampled", "00-" + tid + "-" + pid + "-01", true, 0x01},
		{"version 00 unsampled", "00-" + tid + "-" + pid + "-00", true, 0x00},
		{"version 00 unknown flags kept", "00-" + tid + "-" + pid + "-09", true, 0x09},
		{"leading and trailing OWS", " \t00-" + tid + "-" + pid + "-01\t ", true, 0x01},
		{"uppercase trace ID", "00-" + strings.ToUpper(tid) + "-" + pid + "-01", false, 0},
		{"uppercase parent ID", "00-" + tid + "-" + strings.ToUpper("00F067AA0BA902B7") + "-01", false, 0},
		{"uppercase flags", "00-" + tid + "-" + pid + "-0A", false, 0},
		{"uppercase version", "0A-" + tid + "-" + pid + "-01", false, 0},
		{"all-zero trace ID", "00-" + zeroTID + "-" + pid + "-01", false, 0},
		{"all-zero parent ID", "00-" + tid + "-" + zeroPID + "-01", false, 0},
		{"version ff", "ff-" + tid + "-" + pid + "-01", false, 0},
		{"length 54", "00-" + tid + "-" + pid + "-1", false, 0},
		{"length 56", "00-" + tid + "-" + pid + "-011", false, 0},
		{"version 00 with a suffix", "00-" + tid + "-" + pid + "-01-future", false, 0},
		{"version 01 with a suffix", "01-" + tid + "-" + pid + "-01-what-the-future-will-be-like", true, 0x01},
		{"version cc exactly 55", "cc-" + tid + "-" + pid + "-01", true, 0x01},
		{"version cc suffix without dash", "cc-" + tid + "-" + pid + "-01.what", false, 0},
		{"non-hex trace ID", "00-" + strings.Replace(tid, "4", "g", 1) + "-" + pid + "-01", false, 0},
		{"non-hex parent ID", "00-" + tid + "-" + strings.Replace(pid, "f", "x", 1) + "-01", false, 0},
		{"non-hex flags", "00-" + tid + "-" + pid + "-0.", false, 0},
		{"non-hex version", ".0-" + tid + "-" + pid + "-01", false, 0},
		{"version too long", "000-" + tid + "-" + pid + "-01", false, 0},
		{"version too short", "0-" + tid + "-" + pid + "-01", false, 0},
		{"trace ID too long", "00-" + tid + "0-" + pid + "-01", false, 0},
		{"trace ID too short", "00-" + tid[1:] + "-" + pid + "-01", false, 0},
		{"parent ID too long", "00-" + tid + "-" + pid + "0-01", false, 0},
		{"parent ID too short", "00-" + tid + "-" + pid[1:] + "-01", false, 0},
		{"wrong separators", "00_" + tid + "_" + pid + "_01", false, 0},
		{"empty", "", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotTID, gotPID, flags, ok := ParseTraceparent(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				if gotTID != ([16]byte{}) || gotPID != ([8]byte{}) || flags != 0 {
					t.Fatalf("invalid input returned non-zero IDs %x %x %x", gotTID, gotPID, flags)
				}
				return
			}
			if gotTID != mustHex16(t, tid) || gotPID != mustHex8(t, pid) || flags != tc.flags {
				t.Fatalf("got %x %x %02x, want %s %s %02x", gotTID, gotPID, flags, tid, pid, tc.flags)
			}
		})
	}
}

// TestAppendTraceparent checks the injected format (spec 09 req 31).
func TestAppendTraceparent(t *testing.T) {
	tid, pid := mustHex16(t, testTraceIDHex), mustHex8(t, testParentIDHex)
	if got := string(AppendTraceparent(nil, tid, pid, true)); got != tpSampled {
		t.Errorf("sampled = %q, want %q", got, tpSampled)
	}
	if got := string(AppendTraceparent([]byte("x"), tid, pid, false)); got != "x"+tpUnsampled {
		t.Errorf("unsampled = %q", got)
	}
	gotTID, gotPID, flags, ok := ParseTraceparent(string(AppendTraceparent(nil, tid, pid, true)))
	if !ok || gotTID != tid || gotPID != pid || flags != FlagSampled {
		t.Errorf("round trip failed: %x %x %x %v", gotTID, gotPID, flags, ok)
	}
}

// TestTraceIDFormats covers requestId and log IDs (spec 09 req 32).
func TestTraceIDFormats(t *testing.T) {
	tid, sid := mustHex16(t, testTraceIDHex), mustHex8(t, testParentIDHex)
	if got := FormatTraceID(tid); got != testTraceIDHex {
		t.Errorf("FormatTraceID = %q", got)
	}
	if got := string(AppendTraceID([]byte("id="), tid)); got != "id="+testTraceIDHex {
		t.Errorf("AppendTraceID = %q", got)
	}
	if got := string(AppendSpanID(nil, sid)); got != testParentIDHex {
		t.Errorf("AppendSpanID = %q", got)
	}
}

// members returns n list members "k0=v0,...".
func members(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "k" + strconv.Itoa(i) + "=v"
	}
	return strings.Join(parts, ",")
}

// TestValidTracestate is spec 09 test 7 (tracestate half, req 27) plus the
// W3C list-member grammar.
func TestValidTracestate(t *testing.T) {
	allKeyChars := "abcdefghijklmnopqrstuvwxyz0123456789_-*/"
	var allValueChars strings.Builder
	for c := byte(0x20); c <= 0x7e; c++ {
		if c != ',' && c != '=' {
			allValueChars.WriteByte(c)
		}
	}
	allValue := allValueChars.String() // ends in '~', not a space
	for _, tc := range []struct {
		name string
		in   string
		ok   bool
	}{
		{"one member", "rojo=00f067aa0ba902b7", true},
		{"two members", "rojo=00f067aa0ba902b7,congo=t61rcWkgMzE", true},
		{"OWS around members", "foo=1 \t , \t bar=2", true},
		{"empty members", "foo=1,,bar=2,", true},
		{"only empty members", " , ,", false},
		{"empty", "", false},
		{"all key and value characters", allKeyChars + "=" + allValue, true},
		{"multi-tenant key", "fw529a3039@dt=abc", true},
		{"multi-tenant key starting with digit", "1tenant@sys=1", true},
		{"key starting with digit", "1abc=1", false},
		{"uppercase key", "FOO=1", false},
		{"key with dot", "foo.bar=1", false},
		{"space before =", "foo =1", false},
		{"empty value", "foo=,bar=3", false},
		{"value with =", "foo=bar=baz", false},
		{"value ending in space", "foo=bar ,x=1", true}, // trailing OWS belongs to the list
		{"value with control character", "foo=a\x01b", false},
		{"value with non-ASCII", "foo=caf\xc3\xa9", false},
		{"no =", "foo", false},
		{"empty tenant", "@sys=1", false},
		{"empty system", "tenant@=1", false},
		{"double @", "foo@@bar=1", false},
		{"two @", "foo@bar@baz=1", false},
		{"system starting with digit", "tenant@1sys=1", false},
		{"duplicate keys", "foo=1,foo=1", false},
		{"duplicate keys different values", "foo=1,bar=2,foo=2", false},
		{"32 members", members(32), true},
		{"33 members", members(33), false},
		{"key 256", "k" + strings.Repeat("a", 255) + "=1", true},
		{"key 257", "k" + strings.Repeat("a", 256) + "=1", false},
		{"tenant 241 system 14", "t" + strings.Repeat("a", 240) + "@s" + strings.Repeat("b", 13) + "=1", true},
		{"tenant 242", "t" + strings.Repeat("a", 241) + "@s=1", false},
		{"system 15", "t@s" + strings.Repeat("b", 14) + "=1", false},
		{"value 256", "k=" + strings.Repeat("v", 256), true},
		{"value 257", "k=" + strings.Repeat("v", 257), false},
		{"512 bytes", "k=" + strings.Repeat("v", 254) + ",j=" + strings.Repeat("w", 253), true},
		{"513 bytes", "k=" + strings.Repeat("v", 254) + ",j=" + strings.Repeat("w", 254), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidTracestate(tc.in); got != tc.ok {
				t.Fatalf("ValidTracestate(%q) = %v, want %v", tc.in, got, tc.ok)
			}
		})
	}
}

// TestTracestateHeader covers extraction: field lines combined, limits on
// the combined value, the raw value kept byte-exact (spec 09 req 27).
func TestTracestateHeader(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"absent", nil, ""},
		{"kept byte-exact", []string{"foo=1 ,\tbar=2"}, "foo=1 ,\tbar=2"},
		{"outer OWS trimmed", []string{"  foo=1  "}, "foo=1"},
		{"lines combined", []string{"foo=1,bar=2", "rojo=1,congo=2", "baz=3"}, "foo=1,bar=2,rojo=1,congo=2,baz=3"},
		{"duplicate across lines", []string{"foo=1", "foo=2"}, ""},
		{"combined over 512", []string{"k=" + strings.Repeat("v", 255), "j=" + strings.Repeat("w", 255)}, ""},
		{"invalid dropped", []string{"FOO=1"}, ""},
		{"over 32 members dropped", []string{members(20), "z1=1,z2=2,z3=3,z4=4,z5=5,z6=6,z7=7,z8=8,z9=9,y1=1,y2=2,y3=3,y4=4"}, ""},
		{"32 members across lines kept", []string{members(20), "z1=1,z2=2,z3=3,z4=4,z5=5,z6=6,z7=7,z8=8,z9=9,y1=1,y2=2,y3=3"}, members(20) + ",z1=1,z2=2,z3=3,z4=4,z5=5,z6=6,z7=7,z8=8,z9=9,y1=1,y2=2,y3=3"},
		{"invalid second line", []string{"foo=1", "BAR=2"}, ""},
		{"empty lines only", []string{"", " ,\t"}, ""},
		{"empty line kept in place", []string{" foo=1", "", "bar=2 "}, "foo=1,,bar=2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.lines != nil {
				h[HeaderTracestate] = tc.lines
			}
			if got := tracestate(h); got != tc.want {
				t.Fatalf("tracestate = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTracestateHeaderAllocs: extraction allocates only the combined value
// of several field lines that are valid together, never for a single line
// or for an invalid combination a client sends (spec 09 reqs 27 and 38).
func TestTracestateHeaderAllocs(t *testing.T) {
	skipUnderRace(t)
	for _, tc := range []struct {
		name  string
		lines []string
		want  float64
	}{
		{"one line", []string{"rojo=00f067aa0ba902b7,congo=t61rcWkgMzE"}, 0},
		{"invalid lines", []string{"foo=1", "FOO=2"}, 0},
		{"duplicate across lines", []string{"foo=1", "foo=2"}, 0},
		{"over 512 across lines", []string{"k=" + strings.Repeat("v", 255), "j=" + strings.Repeat("w", 255)}, 0},
		{"valid lines", []string{"foo=1", "bar=2"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{HeaderTracestate: tc.lines}
			if got := testing.AllocsPerRun(100, func() { _ = tracestate(h) }); got != tc.want {
				t.Fatalf("allocs = %v, want %v", got, tc.want)
			}
		})
	}
}
