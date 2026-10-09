// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"testing"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Tests for 03 C requirement 19: header lookup semantics equal
// expr.JoinedHeader, without allocating the canonical key, with Host hidden
// in request.headers only and a deterministic choice among non-canonical
// keys.

// TestReq19LookupMatchesJoinedHeader compares joinedHeader with the
// contract helper over canonical maps and names in every case.
func TestReq19LookupMatchesJoinedHeader(t *testing.T) {
	h := http.Header{}
	h.Add("X-Tenant", "acme")
	h.Add("accept", "a")
	h.Add("Accept", "b")
	h.Add("x-multi-word-name", "v")
	h.Add("Www-Authenticate", "Basic")
	h.Add("X_Under", "u")
	h.Add("Te", "trailers")
	for _, name := range []string{
		"x-tenant", "X-TENANT", "X-Tenant", "accept", "ACCEPT", "x-multi-word-name",
		"X-MULTI-WORD-NAME", "www-authenticate", "WWW-Authenticate", "x_under", "X_UNDER",
		"te", "TE", "missing", "", "x tenant", "x-tenant ", "-", strings.Repeat("a", maxStackKey+1),
	} {
		got, ok := joinedHeader(h, name)
		want, wantOK := expr.JoinedHeader(h, name)
		if got != want || ok != wantOK {
			t.Errorf("joinedHeader(%q) = %q, %v; expr.JoinedHeader = %q, %v", name, got, ok, want, wantOK)
		}
	}
	if got, _ := joinedHeader(h, "accept"); got != "a, b" {
		t.Errorf("repeated field joined = %q, want \"a, b\"", got)
	}
}

// headerMap is the mapSource method set without comparable, so the header
// sources can share a test table.
type headerMap interface {
	lookup(name string) (ref.Val, bool)
	sortedKeys() []string
	size() int
	raw() any
}

// TestReq19Host checks that Host is hidden in request.headers only: it is
// request.host there, while response and step maps show every field
// received, Host included (03 req 19).
func TestReq19Host(t *testing.T) {
	h := http.Header{"Host": {"example.com"}, "host": {"x"}, "A": {"1"}}
	tests := []struct {
		name      string
		src       headerMap
		wantHost  bool
		wantNames []string
	}{
		{"request", requestHeaderSource{&h}, false, []string{"a"}},
		{"response or step", headerSource{&h}, true, []string{"a", "host"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"host", "Host", "HOST"} {
				v, ok := tc.src.lookup(name)
				if ok != tc.wantHost {
					t.Errorf("lookup(%q) found = %v, want %v", name, ok, tc.wantHost)
				}
				if ok && v != types.String("example.com") {
					t.Errorf("lookup(%q) = %v, want the canonical key's value", name, v)
				}
			}
			if n := tc.src.size(); n != len(tc.wantNames) {
				t.Errorf("size = %d, want %d", n, len(tc.wantNames))
			}
			if names := tc.src.sortedKeys(); !slices.Equal(names, tc.wantNames) {
				t.Errorf("sortedKeys = %v, want %v", names, tc.wantNames)
			}
			if raw := tc.src.raw().(map[string]string); len(raw) != len(tc.wantNames) {
				t.Errorf("raw = %v, want keys %v", raw, tc.wantNames)
			}
			if _, ok := tc.src.lookup("a"); !ok {
				t.Error("lookup(a) not found")
			}
		})
	}
	// joinedHeader itself shows Host, as expr.JoinedHeader does.
	if got, ok := joinedHeader(h, "host"); !ok || got != "example.com" {
		t.Errorf("joinedHeader(host) = %q, %v", got, ok)
	}
	// Absent pointers are empty maps.
	for _, src := range []headerMap{headerSource{}, requestHeaderSource{}} {
		if _, ok := src.lookup("a"); ok || src.size() != 0 || len(src.sortedKeys()) != 0 {
			t.Errorf("%T with a nil pointer is not empty", src)
		}
	}
}

// TestReq19NonCanonicalKeys checks maps built by direct assignment: lookup
// prefers the exact canonical key, then the smallest key, every time.
func TestReq19NonCanonicalKeys(t *testing.T) {
	h := http.Header{"x-a": {"lower"}, "X-a": {"mixed"}, "X-B": {"canonical"}, "x-b": {"lower"}}
	for range 50 {
		if got, _ := joinedHeader(h, "x-a"); got != "mixed" {
			t.Fatalf("x-a = %q, want the smallest key's value", got)
		}
		if got, _ := joinedHeader(h, "X-B"); got != "canonical" {
			t.Fatalf("x-b = %q, want the canonical key's value", got)
		}
	}
	for _, hideHost := range []bool{false, true} {
		if n := headerSize(h, hideHost); n != 2 {
			t.Errorf("headerSize(%v) = %d, want 2 distinct names", hideHost, n)
		}
		if names := headerNames(h, hideHost); !slices.Equal(names, []string{"x-a", "x-b"}) {
			t.Errorf("headerNames(%v) = %v", hideHost, names)
		}
	}
	raw := headerSource{&h}.raw().(map[string]string)
	if len(raw) != 2 || raw["x-a"] != "mixed" || raw["x-b"] != "canonical" {
		t.Errorf("raw = %v", raw)
	}
	// Long names, canonicalized off the stack, still try the exact
	// http.CanonicalHeaderKey key first: the canonical key of a token, and
	// name itself when it is not a token, before a smaller folded key.
	long := strings.Repeat("a", maxStackKey+10)
	canon := textproto.CanonicalMIMEHeaderKey(long)
	h2 := http.Header{canon: {"c"}, strings.ToUpper(long): {"u"}}
	if got, _ := joinedHeader(h2, long); got != "c" {
		t.Errorf("long name = %q, want the canonical key's value", got)
	}
	longSpace := "a " + strings.Repeat("b", maxStackKey-1)
	h3 := http.Header{longSpace: {"exact"}, "A " + strings.Repeat("b", maxStackKey-1): {"upper"}}
	for range 50 {
		if got, _ := joinedHeader(h3, longSpace); got != "exact" {
			t.Fatalf("long name that is not a token = %q, want the exact key's value", got)
		}
	}
	if got, ok := joinedHeader(h3, strings.ToUpper(longSpace)); !ok || got != "upper" {
		t.Errorf("long name with no exact key = %q, %v; want the smallest folded key's value", got, ok)
	}
	checkHeaderLookup(t, h3, longSpace)
	if got, ok := joinedHeader(http.Header{}, "a"); ok || got != "" {
		t.Errorf("empty header found %q", got)
	}
}

// TestCanonicalKeyMatchesTextproto checks the stack canonicalization
// against net/textproto.
func TestCanonicalKeyMatchesTextproto(t *testing.T) {
	for _, s := range []string{
		"", "a", "A", "x-foo", "X-FOO", "content-type", "-x-", "x--y", "1abc", "ab_cd", "a.b",
		"a b", "a:b", "ümlaut", "x-\xff", "WWW-AUTHENTICATE", "te", "!#$%&'*+-.^_`|~",
	} {
		if got, want := string(canonicalKey(nil, s)), textproto.CanonicalMIMEHeaderKey(s); got != want {
			t.Errorf("canonicalKey(%q) = %q, want %q", s, got, want)
		}
		if got, want := isCanonical(s), s != "" && validName(s) && textproto.CanonicalMIMEHeaderKey(s) == s; got != want {
			t.Errorf("isCanonical(%q) = %v, want %v", s, got, want)
		}
	}
}

// validName reports a token of header name bytes.
func validName(s string) bool {
	for i := range len(s) {
		if !isTokenByte(s[i]) {
			return false
		}
	}
	return true
}

// TestASCIIFolding checks that folding is ASCII only.
func TestASCIIFolding(t *testing.T) {
	if asciiEqualFold("\u212aey", "Key") || asciiEqualFold("ab", "abc") || !asciiEqualFold("X-Ab", "x-aB") {
		t.Error("asciiEqualFold")
	}
	if asciiLower("X-Ab") != "x-ab" || asciiLower("x-ab") != "x-ab" || asciiLower("\u00c9A") != "\u00c9a" {
		t.Error("asciiLower")
	}
}
