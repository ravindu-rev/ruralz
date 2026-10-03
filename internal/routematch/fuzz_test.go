// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// referenceNormalize is an independent, allocation-heavy reading of 04 req
// 23 used as the oracle of FuzzNormalizePath: it rejects by substring and
// byte scans, rewrites escapes byte by byte, and removes dot segments with
// the literal RFC 3986 section 5.2.4 loop over strings.
func referenceNormalize(p string) (string, bool) {
	if p == "" {
		return "/", true
	}
	if p[0] != '/' || strings.ContainsRune(p, '\\') {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c == 0x7F {
			return "", false
		}
		if c != '%' {
			if strings.IndexByte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~!$&'()*+,;=:@/", c) >= 0 {
				b.WriteByte(c)
			} else {
				b.WriteString("%" + strings.ToUpper(hex2(c)))
			}
			continue
		}
		if i+2 >= len(p) || !isHex(p[i+1]) || !isHex(p[i+2]) {
			return "", false
		}
		esc := strings.ToUpper(p[i+1 : i+3])
		v := hexVal(esc[0])<<4 | hexVal(esc[1])
		if esc == "00" || esc == "5C" || v < 0x20 || v == 0x7F {
			return "", false
		}
		if strings.IndexByte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~", v) >= 0 {
			b.WriteByte(v)
		} else {
			b.WriteString("%" + esc)
		}
		i += 2
	}
	return rfcRemoveDotSegments(b.String()), true
}

func hex2(c byte) string { return string([]byte{"0123456789abcdef"[c>>4], "0123456789abcdef"[c&15]}) }
func isHex(c byte) bool  { return strings.IndexByte("0123456789abcdefABCDEF", c) >= 0 }
func hexVal(c byte) byte {
	v, _, _ := unhex(c)
	return v
}
func segCount(s string) int { return strings.Count(s, "/") }

// asciiUpper upper-cases ASCII letters byte by byte, leaving invalid UTF-8
// as it is.
func asciiUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// rfcRemoveDotSegments is RFC 3986 section 5.2.4 as written.
func rfcRemoveDotSegments(in string) string {
	out := ""
	for in != "" {
		switch {
		case strings.HasPrefix(in, "../"):
			in = in[3:]
		case strings.HasPrefix(in, "./"):
			in = in[2:]
		case strings.HasPrefix(in, "/./"):
			in = in[2:]
		case in == "/.":
			in = "/"
		case strings.HasPrefix(in, "/../"):
			in = in[3:]
			out = out[:max(strings.LastIndexByte(out, '/'), 0)]
		case in == "/..":
			in = "/"
			out = out[:max(strings.LastIndexByte(out, '/'), 0)]
		case in == "." || in == "..":
			in = ""
		default:
			j := strings.IndexByte(in[1:], '/')
			if j < 0 {
				out += in
				in = ""
			} else {
				out += in[:j+1]
				in = in[j+1:]
			}
		}
	}
	return out
}

// encodesControl reports whether s holds the escape of a control byte
// (%00 to %1F or %7F, either hex case).
func encodesControl(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '%' && isHex(s[i+1]) && isHex(s[i+2]) {
			if v := hexVal(s[i+1])<<4 | hexVal(s[i+2]); v < 0x20 || v == 0x7F {
				return true
			}
		}
	}
	return false
}

// FuzzNormalizePath checks 04 req 23 (test plan: FuzzNormalizePath): the
// result equals an independent reference; it is idempotent; it never holds
// a "." or ".." segment; it never decodes %2F (no slash is added); it holds
// only canonical escapes; NUL, backslash and encoded control forms are
// rejected with RZ-RT-017; asterisk-form is 404 RZ-RT-001.
func FuzzNormalizePath(f *testing.F) {
	for _, tc := range normalizeCases {
		f.Add(tc.in)
	}
	for _, s := range []string{"/a/%2E%2e/%2f/../b", "/%2e", "/..%2F..", "/%41%2F%5c", "/ /\xff", "/./.././../", "*", "/%0d%0A", "/%7F"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got, err := NormalizePath(in)
		if in == "*" {
			if code, _ := errcode.CodeOf(err); got != "" || !errors.Is(err, ErrAsteriskForm) || code != CodeNoRoute {
				t.Fatalf(`NormalizePath("*") = %q, %v; want ErrAsteriskForm`, got, err)
			}
			return
		}
		want, ok := referenceNormalize(in)
		if ok != (err == nil) {
			t.Fatalf("NormalizePath(%q) = %q, %v; reference ok=%v %q", in, got, err, ok, want)
		}
		upper := asciiUpper(in)
		mustReject := strings.Contains(upper, "%5C") || strings.ContainsRune(in, '\\') || encodesControl(in)
		if err != nil {
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("error %v does not match ErrRejected", err)
			}
			if code, _ := errcode.CodeOf(err); code != CodeRejected {
				t.Fatalf("error %v has code %q", err, code)
			}
			return
		}
		if mustReject {
			t.Fatalf("NormalizePath(%q) = %q; NUL, backslash or control form not rejected", in, got)
		}
		if got != want {
			t.Fatalf("NormalizePath(%q) = %q; reference %q", in, got, want)
		}
		if again, err := NormalizePath(got); err != nil || again != got {
			t.Fatalf("not idempotent: %q -> %q -> %q, %v", in, got, again, err)
		}
		if !strings.HasPrefix(got, "/") {
			t.Fatalf("%q does not start with /", got)
		}
		for i := 1; i <= len(got); {
			seg, next := NextSegment(got, i)
			if seg == "." || seg == ".." {
				t.Fatalf("%q holds a dot segment", got)
			}
			i = next
		}
		if in != "" && segCount(got) > segCount(in) {
			t.Fatalf("%q -> %q added a slash (decoded %%2F?)", in, got)
		}
		for i := 0; i < len(got); i++ {
			c := got[i]
			switch {
			case c == '%':
				e := got[i+1 : i+3]
				if e != strings.ToUpper(e) || isUnreserved(hexVal(e[0])<<4|hexVal(e[1])) {
					t.Fatalf("%q holds a non-canonical escape %q", got, e)
				}
				i += 2
			case !isPathRaw(c):
				t.Fatalf("%q holds raw byte %#x", got, c)
			}
		}
	})
}

// FuzzTemplate checks 04 req 29 (test plan: FuzzTemplate): parsing never
// panics; errors carry RZ-CFG-005; a valid template round-trips through
// String; it matches its own instances with the right captures; Match on
// any path agrees with the segment trie and captures non-empty segments
// without "/".
func FuzzTemplate(f *testing.F) {
	for _, tc := range templateCases {
		f.Add(tc.in, "/v1/orders/42")
	}
	f.Add("/v1/{id}/", "/v1/42/")
	f.Add("/{a}/b/{c}", "/x/b/y%2Fz")
	f.Add("/", "/")
	f.Fuzz(func(t *testing.T, s, path string) {
		tp, err := ParseTemplate(s)
		if err != nil {
			if code, _ := errcode.CodeOf(err); code != CodeInvalid {
				t.Fatalf("ParseTemplate(%q) error %v has code %q", s, err, code)
			}
			if CheckTemplate(s) == nil {
				t.Fatalf("CheckTemplate(%q) accepts what ParseTemplate rejects", s)
			}
			return
		}
		canon := tp.String()
		again, err := ParseTemplate(canon)
		if err != nil || !reflect.DeepEqual(again, tp) || again.String() != canon {
			t.Fatalf("round trip %q -> %q -> %+v, %v", s, canon, again, err)
		}
		// An instance replaces every parameter by a distinct value.
		var inst strings.Builder
		var want []string
		for i, seg := range tp.Segments {
			inst.WriteByte('/')
			if seg.Param != "" {
				v := "p" + string(rune('a'+i%26))
				want = append(want, v)
				inst.WriteString(v)
			} else {
				inst.WriteString(seg.Literal)
			}
		}
		if tp.TrailingSlash || len(tp.Segments) == 0 {
			inst.WriteByte('/')
		}
		norm, err := NormalizePath(inst.String())
		if err != nil || norm != inst.String() {
			t.Fatalf("instance %q of %q is not normal: %q, %v", inst.String(), canon, norm, err)
		}
		raw, ok := tp.Match(inst.String(), nil)
		if !ok || !reflect.DeepEqual(raw, want) && len(want) > 0 {
			t.Fatalf("%q does not match its instance %q: %q, %v", canon, inst.String(), raw, ok)
		}
		// Any path: Match agrees with the trie.
		var tr Trie[int]
		tr.InsertTemplate(tp, 1)
		raw, ok = tp.Match(path, nil)
		var trieRaw []string
		found := false
		tr.Lookup(path, nil, func(_ int, r []string) bool { found, trieRaw = true, append([]string(nil), r...); return true })
		if ok != found || ok && !reflect.DeepEqual(raw, trieRaw) && len(raw)+len(trieRaw) > 0 {
			t.Fatalf("%q on %q: Match %v %q, trie %v %q", canon, path, ok, raw, found, trieRaw)
		}
		if ok {
			if len(raw) != tp.Params() {
				t.Fatalf("%q on %q captured %d values", canon, path, len(raw))
			}
			for _, r := range raw {
				if r == "" || strings.Contains(r, "/") {
					t.Fatalf("%q on %q captured %q", canon, path, r)
				}
			}
		}
	})
}

// FuzzNormalizeHost checks 04 req 24 and 27: no upper-case ASCII in the
// result, idempotence unless a second trailing dot remains, and the host
// table yields exactly the matching patterns in rank 1 order.
func FuzzNormalizeHost(f *testing.F) {
	for _, s := range []string{"API.Shop.Example:8443", "[::1]:8080", "shop.example.", "a.b.shop.example", "xshop.example", "::1", "", ".", "a..shop.example", "[0:0::1]", "[::FFFF:10.0.0.1]:1", "[2001:db8::1%25x]"} {
		f.Add(s)
	}
	patterns := []string{"api.shop.example", "*.shop.example", "*.b.shop.example", "*.example", "[::1]"}
	var tab HostTable[string]
	var parsed []HostPattern
	for _, s := range patterns {
		p, err := ParseHostPattern(s)
		if err != nil {
			f.Fatal(err)
		}
		*tab.Entry(p) = s
		parsed = append(parsed, p)
	}
	*tab.Entry(HostPattern{}) = "any"
	parsed = append(parsed, HostPattern{})
	f.Fuzz(func(t *testing.T, in string) {
		h := NormalizeHost(in)
		if strings.IndexFunc(h, func(r rune) bool { return 'A' <= r && r <= 'Z' }) >= 0 {
			t.Fatalf("NormalizeHost(%q) = %q holds upper case", in, h)
		}
		if !strings.HasSuffix(h, ".") && NormalizeHost(h) != h {
			t.Fatalf("NormalizeHost not idempotent on %q: %q -> %q", in, h, NormalizeHost(h))
		}
		var got []HostPattern
		tab.Lookup(h, func(p HostPattern, _ *string) bool { got = append(got, p); return true })
		var ranks []Rank
		for _, p := range parsed {
			if p.Match(h) {
				ranks = append(ranks, Rank{Host: p})
			}
		}
		slices.SortFunc(ranks, Compare)
		want := make([]HostPattern, 0, len(ranks))
		for _, r := range ranks {
			want = append(want, r.Host)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("host %q: table %v; patterns %v", h, got, want)
		}
	})
}
