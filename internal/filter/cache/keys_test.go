// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"crypto/sha256"
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// TestURIForm checks the URI row of "Response caching": the scheme, the
// normalized host with a non-default port, the normalized path and the
// query with its escapes normalized, an empty query equal to none.
func TestURIForm(t *testing.T) {
	tests := []struct {
		name                                    string
		scheme, host, authority, path, rawQuery string
		want                                    string
	}{
		{"plain", "https", "shop.example", "shop.example", "/v1/catalog", "page=2", "https://shop.example/v1/catalog?page=2"},
		{"default https port", "https", "shop.example", "shop.example:443", "/", "", "https://shop.example/"},
		{"default http port", "http", "shop.example", "shop.example:80", "/", "", "http://shop.example/"},
		{"other port", "https", "shop.example", "Shop.Example.:8443", "/", "", "https://shop.example:8443/"},
		{"port with leading zeros", "https", "shop.example", "shop.example:0443", "/", "", "https://shop.example/"},
		{"empty port", "https", "shop.example", "shop.example:", "/", "", "https://shop.example/"},
		{"IPv6 with port", "http", "[2001:db8::1]", "[2001:DB8::1]:8080", "/", "", "http://[2001:db8::1]:8080/"},
		{"IPv6 without port", "http", "[2001:db8::1]", "[2001:db8::1]", "/", "", "http://[2001:db8::1]/"},
		{"unreserved escapes decoded", "https", "h", "h", "/", "a=%41%7e%2d", "https://h/?a=A~-"},
		{"hex upper-cased", "https", "h", "h", "/", "a=%2f%3a", "https://h/?a=%2F%3A"},
		{"raw bytes escaped", "https", "h", "h", "/", "a=b c\x80", "https://h/?a=b%20c%80"},
		{"invalid escape", "https", "h", "h", "/", "a=%zz&b=%4", "https://h/?a=%25zz&b=%254"},
		{"order kept", "https", "h", "h", "/", "b=2&a=1", "https://h/?b=2&a=1"},
		{"sub-delims kept", "https", "h", "h", "/", "q=a+b;c=d,e/f?g", "https://h/?q=a+b;c=d,e/f?g"},
	}
	for _, tt := range tests {
		got := string(AppendURI(nil, tt.scheme, tt.host, tt.authority, tt.path, tt.rawQuery))
		if got != tt.want {
			t.Errorf("%s: AppendURI = %q, want %q", tt.name, got, tt.want)
		}
		if d := URIDigest(tt.scheme, tt.host, tt.authority, tt.path, tt.rawQuery); d != sha256.Sum256([]byte(tt.want)) {
			t.Errorf("%s: URIDigest is not the SHA-256 of %q", tt.name, tt.want)
		}
	}
}

// TestURIEquivalentSpellings checks that equivalent spellings share U and
// different URIs do not.
func TestURIEquivalentSpellings(t *testing.T) {
	a := URIDigest("https", "shop.example", "shop.example:443", "/p", "x=%7E&y=%2f")
	b := URIDigest("https", "shop.example", "shop.example", "/p", "x=~&y=%2F")
	if a != b {
		t.Error("equivalent spellings have different digests")
	}
	if a == URIDigest("https", "shop.example", "shop.example", "/p", "y=%2F&x=~") {
		t.Error("reordered parameters share a digest")
	}
	if URIDigest("https", "h", "h", "/p", "") == URIDigest("http", "h", "h", "/p", "") {
		t.Error("schemes share a digest")
	}
}

// TestPartitionDigest checks the Partition row: the SHA-256 of the Route
// name, a NUL byte and the value, so neither moves into the other.
func TestPartitionDigest(t *testing.T) {
	if got, want := PartitionDigest("catalog", "acme"), sha256.Sum256([]byte("catalog\x00acme")); got != want {
		t.Errorf("PartitionDigest = %x, want %x", got, want)
	}
	if PartitionDigest("a", "bc") == PartitionDigest("ab", "c") {
		t.Error("the NUL separator does not keep Route and value apart")
	}
}

// TestNames checks the n row of "Response Cache layout": members trimmed,
// lower-cased, deduplicated, sorted by bytes and joined by commas.
func TestNames(t *testing.T) {
	tests := []struct {
		h    http.Header
		want string
	}{
		{http.Header{}, ""},
		{http.Header{"Vary": {"Accept-Encoding"}}, "accept-encoding"},
		{http.Header{"Vary": {" Accept-Language ,accept-encoding", "ACCEPT-ENCODING, , X-Tier"}}, "accept-encoding,accept-language,x-tier"},
	}
	for _, tt := range tests {
		if got := Names(tt.h); got != tt.want {
			t.Errorf("Names(%v) = %q, want %q", tt.h, got, tt.want)
		}
	}
}

// TestVariant checks the V row: one record per name, "-" for a missing
// field, else "=" and its trimmed lines joined by ", ", each ended by a
// line feed; the SHA-256 of the empty string without names.
func TestVariant(t *testing.T) {
	h := http.Header{"Accept-Encoding": {" gzip ", "br\t"}, "x-tier": {"gold"}}
	tests := []struct {
		names, records string
	}{
		{"", ""},
		{"accept-encoding", "accept-encoding:=gzip, br\n"},
		{"accept-encoding,accept-language", "accept-encoding:=gzip, br\naccept-language:-\n"},
		{"x-tier", "x-tier:=gold\n"},
	}
	for _, tt := range tests {
		if got, want := Variant(h, tt.names), statestore.Digest(sha256.Sum256([]byte(tt.records))); got != want {
			t.Errorf("Variant(%q) is not the SHA-256 of %q", tt.names, tt.records)
		}
	}
}

// TestNameLRU checks the Vary-name LRU: the last names learned per URI,
// at most its bound of URIs, the least recently used evicted.
func TestNameLRU(t *testing.T) {
	l := newNameLRU(2)
	u := func(s string) statestore.Digest { return statestore.DigestOf(s) }
	l.learn(u("a"), "accept")
	l.learn(u("b"), "x")
	if l.get(u("a")) != "accept" {
		t.Fatal("learned names not returned")
	}
	l.learn(u("c"), "y")
	if l.get(u("b")) != "" || l.get(u("a")) != "accept" || l.get(u("c")) != "y" || l.len() != 2 {
		t.Errorf("after eviction: b %q, a %q, c %q, len %d", l.get(u("b")), l.get(u("a")), l.get(u("c")), l.len())
	}
	l.learn(u("a"), "")
	if l.get(u("a")) != "" || l.len() != 2 {
		t.Error("relearning a URI did not replace its names")
	}
}
