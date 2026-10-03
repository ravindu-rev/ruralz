// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package httpfield

import (
	"net/http"
	"net/textproto"
	"reflect"
	"strings"
	"testing"
)

// Fuzz targets F3 FuzzQueryEdit and F4 FuzzFieldValue of spec 07 section 6
// (oracle: no panic, no hang, bounded memory, plus the properties below).

// refValueByte is the RFC 9110 section 5.5 field-vchar / SP / HTAB table
// spelled out byte by byte, independent of isValueByte.
func refValueByte(c byte) bool {
	switch {
	case c == 0x09, c == 0x20:
		return true
	case 0x21 <= c && c <= 0x7e: // VCHAR
		return true
	case 0x80 <= c: // obs-text
		return true
	default:
		return false
	}
}

func refValidValue(v string) bool {
	for i := range len(v) {
		if !refValueByte(v[i]) {
			return false
		}
	}
	return v == "" || (v[0] != ' ' && v[0] != '\t' && v[len(v)-1] != ' ' && v[len(v)-1] != '\t')
}

func refValidName(n string) bool {
	if n == "" || len(n) > MaxNameBytes {
		return false
	}
	for i := range len(n) {
		if !refIsTchar(n[i]) {
			return false
		}
	}
	return true
}

// FuzzFieldValue is F4: the validators agree with byte-table references,
// NormalizeValue returns a valid value or an error, and Protect agrees with
// a lowercase lookup (07 req 21, 22, 23, 27).
func FuzzFieldValue(f *testing.F) {
	for _, s := range []string{
		"", "v", " v ", "a\r\nb", "\x00", "\x7f", "a\tb", "\x80\xff", "Traceparent",
		"X-Forwarded-For", ":authority", "Keep-alive", strings.Repeat("v", MaxValueBytes+1),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		if got, want := ValidValue(v), refValidValue(v); got != want {
			t.Fatalf("ValidValue(%q) = %v, reference %v", v, got, want)
		}
		if got, want := CheckValue(v) == nil, refValidValue(v) && len(v) <= MaxValueBytes; got != want {
			t.Fatalf("CheckValue(%q) == nil is %v, reference %v", v, got, want)
		}
		if got, want := ValidName(v), refValidName(v); got != want {
			t.Fatalf("ValidName(%q) = %v, reference %v", v, got, want)
		}
		if got, want := CheckName(v) == nil, refValidName(v); got != want {
			t.Fatalf("CheckName(%q) == nil is %v, reference %v", v, got, want)
		}
		n, err := NormalizeValue(v)
		trimmed := strings.Trim(v, " \t")
		switch {
		case err == nil && (!refValidValue(n) || len(n) > MaxComputedValueBytes || n != trimmed):
			t.Fatalf("NormalizeValue(%q) = %q, not the valid trimmed value", v, n)
		case err != nil && refValidValue(trimmed) && len(trimmed) <= MaxComputedValueBytes:
			t.Fatalf("NormalizeValue(%q) failed on a valid value: %v", v, err)
		}
		if got, want := Protect(v), refProtect(v); got != want {
			t.Fatalf("Protect(%q) = %v, reference %v", v, got, want)
		}
	})
}

// refProtect is 07 req 22 as a lookup on the ASCII-lowercased name.
func refProtect(name string) Protection {
	if strings.HasPrefix(name, ":") {
		return Pseudo
	}
	lower := []byte(name)
	for i, c := range lower {
		if 'A' <= c && c <= 'Z' {
			lower[i] = c + 'a' - 'A'
		}
	}
	switch string(lower) {
	case "host":
		return HostField
	case "connection", "keep-alive", "proxy-connection", "te", "trailer",
		"transfer-encoding", "upgrade", "proxy-authenticate", "proxy-authorization":
		return HopByHop
	case "content-length":
		return Framing
	case "expect":
		return ExpectField
	case "traceparent", "tracestate":
		return TraceContext
	default:
		return Unprotected
	}
}

// FuzzQueryEdit is F3 with property P8: parsing is lossless, Set leaves
// exactly one pair with the key and it decodes to the value, Del leaves
// none, and every other segment keeps its bytes and order (07 req 48).
func FuzzQueryEdit(f *testing.F) {
	f.Add("a=1&b=2&a=3", "a", "9")
	f.Add("a+b=1&x=%7e", "a b", "x y")
	f.Add("k&=v&&;", "k", "")
	f.Add("%zz=1&%41=2", "%zz", "%")
	f.Add("", "new", "v&w")
	f.Fuzz(func(t *testing.T, raw, name, value string) {
		q := ParseQuery(raw)
		if got := q.String(); got != raw {
			t.Fatalf("ParseQuery(%q) round trip = %q", raw, got)
		}
		if name == "" {
			return // query names are never empty (07 req 40)
		}
		checkSet(t, raw, name, value)
		checkDel(t, raw, name)
	})
}

// FuzzStripHopByHop removes every hop-by-hop and Connection-named field,
// keeps every other field, and never panics (07 req 33).
func FuzzStripHopByHop(f *testing.F) {
	f.Add("keep-alive, x-foo", "x-foo", "trailers", true)
	f.Add("", "Upgrade", "gzip", false)
	f.Add(",,te,", "Te", "trailers, deflate", true)
	f.Fuzz(func(t *testing.T, conn, extra, te string, toUpstream bool) {
		d := TowardClient
		if toUpstream {
			d = TowardUpstream
		}
		h := map[string][]string{
			"Connection": {conn}, "Te": {te}, "Expect": {"100-continue"},
			"Accept": {"*/*"},
		}
		if key := textproto.CanonicalMIMEHeaderKey(extra); key != "" && h[key] == nil {
			h[key] = []string{"x"}
		}
		before := make(map[string]bool, len(h))
		for k := range h {
			before[k] = true
		}
		StripHopByHop(h, d)
		opts := ConnectionOptions(map[string][]string{"Connection": {conn}})
		for k := range before {
			_, kept := h[k]
			named := false
			for _, o := range opts {
				named = named || lowerEq(k, o)
			}
			drop := IsHopByHop(k) || named || (d == TowardUpstream && k == "Expect")
			switch {
			case k == "Te" && d == TowardUpstream && listHas([]string{te}, "trailers"):
				if !kept || len(h[k]) != 1 || h[k][0] != "trailers" {
					t.Fatalf("TE = %q, want trailers", h[k])
				}
			case drop && kept:
				t.Fatalf("%s kept (Connection %q, direction %d)", k, conn, d)
			case !drop && !kept:
				t.Fatalf("%s removed (Connection %q, direction %d)", k, conn, d)
			}
		}
	})
}

// refToken reports whether s is an RFC 9110 token, from the tchar string
// of the RFC rather than the bit masks.
func refToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !refIsTchar(s[i]) {
			return false
		}
	}
	return true
}

// refLower lowercases the ASCII letters of s only.
func refLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// refElements splits comma-separated lists into elements trimmed of SP and
// HTAB, empty elements included.
func refElements(values []string) []string {
	var out []string
	for _, v := range values {
		for _, e := range strings.Split(v, ",") {
			out = append(out, strings.Trim(e, " \t"))
		}
	}
	return out
}

// refStrip is the hop-by-hop rule of 07 req 33, 04 req 22 and 05 req 28
// and 30 as a quadratic model over keys of any spelling: Connection and TE
// are read under every key that equals them ignoring ASCII case, and a
// token key is named when its lowercase form is a lowercased token option.
// admission selects the StripConnectionOptions rule instead.
func refStrip(h map[string][]string, d Direction, admission bool) map[string][]string {
	named := map[string]bool{}
	keepTE := false
	for k, vs := range h {
		switch refLower(k) {
		case "connection":
			for _, e := range refElements(vs) {
				if refToken(e) && (!admission || refProtect(e) != HopByHop) {
					named[refLower(e)] = true
				}
			}
		case "te":
			for _, e := range refElements(vs) {
				keepTE = keepTE || refLower(e) == "trailers"
			}
		}
	}
	out := map[string][]string{}
	for k, vs := range h {
		lower := refLower(k)
		drop := refToken(k) && named[lower]
		if admission {
			drop = drop || lower == "connection"
		} else {
			drop = drop || refProtect(k) == HopByHop || (d != TowardClient && lower == "expect")
		}
		if !drop {
			out[k] = vs
		}
	}
	if keepTE && d == TowardUpstream && !admission {
		out["Te"] = []string{"trailers"}
	}
	return out
}

// FuzzStripHopByHopSpelling checks StripHopByHop, for every Direction
// value, and StripConnectionOptions against refStrip on header maps whose
// keys keep the fuzzer's spelling, so both the canonical lookup path and
// the non-canonical set path run (07 req 33).
func FuzzStripHopByHopSpelling(f *testing.F) {
	f.Add("x-foo, TE", "connection", "X-Foo", "te", "trailers", uint8(1))
	f.Add("X-FOO,,x-foo", "Connection", "x-foo", "Te", "gzip, Trailers", uint8(2))
	f.Add("expect", "CONNECTION", "Expect", "EXPECT", "x", uint8(0))
	f.Add("a,b,a", "Connection", "A", "x y", "b", uint8(3))
	f.Fuzz(func(t *testing.T, conn, k1, k2, k3, v3 string, d uint8) {
		build := func() http.Header {
			h := http.Header{"Connection": {conn}, "Accept": {"*/*"}}
			h[k1] = []string{"x-foo, x-bar"}
			h[k2] = []string{"2"}
			h[k3] = []string{v3}
			return h
		}
		dir := Direction(d % 4)
		h := build()
		want := refStrip(build(), dir, false)
		StripHopByHop(h, dir)
		if !reflect.DeepEqual(map[string][]string(h), want) {
			t.Fatalf("StripHopByHop(%q, %d) = %q, reference %q", build(), dir, h, want)
		}
		h = build()
		want = refStrip(build(), dir, true)
		StripConnectionOptions(h)
		if !reflect.DeepEqual(map[string][]string(h), want) {
			t.Fatalf("StripConnectionOptions(%q) = %q, reference %q", build(), h, want)
		}
	})
}

// FuzzCanonicalKey checks the canonical form StripHopByHop looks options
// up by against net/textproto, which net/http stores keys with.
func FuzzCanonicalKey(f *testing.F) {
	for _, s := range []string{"x-foo", "X-FOO", "x foo", "content-md5", "\x80", "-a-", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		canon := textproto.CanonicalMIMEHeaderKey(s)
		if got, ok := appendCanonicalToken(nil, s); ok != refToken(s) || (ok && string(got) != canon) {
			t.Fatalf("appendCanonicalToken(%q) = %q, %v; textproto %q", s, got, ok, canon)
		}
		if got, want := isCanonicalKey(s), canon == s; got != want {
			t.Fatalf("isCanonicalKey(%q) = %v, want %v", s, got, want)
		}
	})
}
