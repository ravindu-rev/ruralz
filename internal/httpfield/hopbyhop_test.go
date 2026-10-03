// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package httpfield

import (
	"net/http"
	"net/textproto"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Tests for 07 req 33, 04 req 22 and 05 req 28 and 30: hop-by-hop fields
// and every field Connection names are removed in both directions, Expect
// is dropped toward Upstreams and "TE: trailers" is kept toward them
// (RFC 9110 section 7.6.1). Spec 07 integration case I4 ("Connection:
// x-foo" with X-Foo removed both ways) is covered at the predicate level.

// longName is a token over MaxNameBytes, so it does not fit the stack
// buffer StripHopByHop canonicalizes options in.
const longName = "x-" + "abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFGHIJKLMNOPQRSTUVWXYZ"

func TestConnectionOptionsReq33(t *testing.T) {
	tests := []struct {
		name string
		h    http.Header
		want []string
	}{
		{"none", http.Header{"X-Foo": {"1"}}, nil},
		{"one", http.Header{"Connection": {"close"}}, []string{"close"}},
		{"lowercased and trimmed", http.Header{"Connection": {" Keep-Alive ,\tX-Foo "}}, []string{"keep-alive", "x-foo"}},
		{"several lines", http.Header{"Connection": {"te", "X-Bar, x-baz"}}, []string{"te", "x-bar", "x-baz"}},
		{"empty elements", http.Header{"Connection": {",, x-foo ,,"}}, []string{"x-foo"}},
		{"non-token skipped", http.Header{"Connection": {"x foo, x-bar, (x)"}}, []string{"x-bar"}},
		{"empty field", http.Header{"Connection": {""}}, nil},
		{"consecutive duplicates listed once", http.Header{"Connection": {"A, a,A", "a, b, A"}}, []string{"a", "b", "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ConnectionOptions(tt.h); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ConnectionOptions = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStripHopByHopReq33(t *testing.T) {
	tests := []struct {
		name string
		dir  Direction
		in   http.Header
		want http.Header
	}{
		{
			name: "fixed list toward upstream",
			dir:  TowardUpstream,
			in: http.Header{
				"Connection": {"keep-alive"}, "Keep-Alive": {"timeout=5"},
				"Proxy-Connection": {"keep-alive"}, "Te": {"gzip"}, "Trailer": {"X-Sum"},
				"Transfer-Encoding": {"chunked"}, "Upgrade": {"websocket"},
				"Proxy-Authorization": {"Basic eA=="}, "Proxy-Authenticate": {"Basic"},
				"Content-Type": {"application/json"},
			},
			want: http.Header{"Content-Type": {"application/json"}},
		},
		{
			name: "fixed list toward client",
			dir:  TowardClient,
			in: http.Header{
				"Connection": {"close"}, "Keep-Alive": {"timeout=5"},
				"Transfer-Encoding": {"chunked"}, "Upgrade": {"h2c"},
				"Proxy-Authenticate": {"Basic"}, "Trailer": {"X-Sum"},
				"Content-Length": {"12"}, "Set-Cookie": {"a=1", "b=2"},
			},
			want: http.Header{"Content-Length": {"12"}, "Set-Cookie": {"a=1", "b=2"}},
		},
		{
			// Spec 07 test I4: Connection: x-foo removes X-Foo both ways.
			name: "Connection-named field toward upstream",
			dir:  TowardUpstream,
			in:   http.Header{"Connection": {"x-foo, X-Bar"}, "X-Foo": {"1"}, "X-Bar": {"2"}, "X-Baz": {"3"}},
			want: http.Header{"X-Baz": {"3"}},
		},
		{
			name: "Connection-named field toward client",
			dir:  TowardClient,
			in:   http.Header{"Connection": {"X-Foo"}, "X-Foo": {"1"}, "X-Baz": {"3"}},
			want: http.Header{"X-Baz": {"3"}},
		},
		{
			name: "Connection may name an end-to-end field",
			dir:  TowardUpstream,
			in:   http.Header{"Connection": {"authorization"}, "Authorization": {"Bearer x"}, "Accept": {"*/*"}},
			want: http.Header{"Accept": {"*/*"}},
		},
		{
			// Found by FuzzStripHopByHop: a non-token element names no field.
			name: "non-token Connection element names nothing",
			dir:  TowardClient,
			in:   http.Header{"Connection": {"\x10, x y"}, "\x10": {"1"}, "x y": {"2"}},
			want: http.Header{"\x10": {"1"}, "x y": {"2"}},
		},
		{
			name: "Expect dropped toward upstream",
			dir:  TowardUpstream,
			in:   http.Header{"Expect": {"100-continue"}, "Accept": {"*/*"}},
			want: http.Header{"Accept": {"*/*"}},
		},
		{
			name: "Expect kept toward client",
			dir:  TowardClient,
			in:   http.Header{"Expect": {"100-continue"}},
			want: http.Header{"Expect": {"100-continue"}},
		},
		{
			name: "TE trailers kept toward upstream",
			dir:  TowardUpstream,
			in:   http.Header{"Connection": {"TE"}, "Te": {"trailers"}},
			want: http.Header{"Te": {"trailers"}},
		},
		{
			name: "TE trailers member among others becomes exactly trailers",
			dir:  TowardUpstream,
			in:   http.Header{"Te": {"deflate;q=0.5, Trailers"}},
			want: http.Header{"Te": {"trailers"}},
		},
		{
			name: "TE trailers on a second line",
			dir:  TowardUpstream,
			in:   http.Header{"Te": {"gzip", " trailers "}},
			want: http.Header{"Te": {"trailers"}},
		},
		{
			name: "TE without trailers dropped",
			dir:  TowardUpstream,
			in:   http.Header{"Te": {"gzip, trailersx"}},
			want: http.Header{},
		},
		{
			name: "TE dropped toward client",
			dir:  TowardClient,
			in:   http.Header{"Te": {"trailers"}},
			want: http.Header{},
		},
		{
			name: "non-canonical keys matched ignoring case",
			dir:  TowardUpstream,
			in:   http.Header{"keep-alive": {"1"}, "TRANSFER-ENCODING": {"chunked"}, "expect": {"100-continue"}, "x-ok": {"1"}},
			want: http.Header{"x-ok": {"1"}},
		},
		{
			// A Connection field under another spelling still names fields.
			name: "non-canonical Connection key names a canonical field",
			dir:  TowardUpstream,
			in:   http.Header{"connection": {"x-foo"}, "X-Foo": {"1"}, "X-Baz": {"3"}},
			want: http.Header{"X-Baz": {"3"}},
		},
		{
			name: "Connection names a non-canonical key",
			dir:  TowardClient,
			in:   http.Header{"Connection": {"X-Foo"}, "x-FOO": {"1"}, "x-baz": {"3"}, "X-Qux": {"4"}},
			want: http.Header{"x-baz": {"3"}, "X-Qux": {"4"}},
		},
		{
			name: "Connection under two spellings",
			dir:  TowardClient,
			in:   http.Header{"Connection": {"x-a"}, "CONNECTION": {"x-b"}, "X-A": {"1"}, "x-b": {"2"}, "X-C": {"3"}},
			want: http.Header{"X-C": {"3"}},
		},
		{
			name: "non-canonical TE trailers kept toward upstream",
			dir:  TowardUpstream,
			in:   http.Header{"TE": {"trailers"}, "Connection": {"te"}},
			want: http.Header{"Te": {"trailers"}},
		},
		{
			name: "non-token key is never named",
			dir:  TowardClient,
			in:   http.Header{"Connection": {"x-foo"}, "x foo": {"1"}, "X-Foo": {"2"}},
			want: http.Header{"x foo": {"1"}},
		},
		{
			name: "non-token option with a non-canonical key",
			dir:  TowardClient,
			in:   http.Header{"Connection": {"x y, keep-alive"}, "x-foo": {"1"}},
			want: http.Header{"x-foo": {"1"}},
		},
		{
			name: "Connection names TE, trailers kept",
			dir:  TowardUpstream,
			in:   http.Header{"Connection": {"TE, X-Foo"}, "Te": {"trailers"}, "X-Foo": {"1"}},
			want: http.Header{"Te": {"trailers"}},
		},
		{
			name: "Connection names Expect toward client",
			dir:  TowardClient,
			in:   http.Header{"Connection": {"expect"}, "Expect": {"100-continue"}},
			want: http.Header{},
		},
		{
			name: "repeated and mixed-case options",
			dir:  TowardClient,
			in:   http.Header{"Connection": {"x-foo, X-FOO,x-Foo", "x-foo, x-bar"}, "X-Foo": {"1"}, "X-Bar": {"2"}, "X-Baz": {"3"}},
			want: http.Header{"X-Baz": {"3"}},
		},
		{
			name: "option longer than MaxNameBytes",
			dir:  TowardClient,
			in:   http.Header{"Connection": {"x-ok, " + longName}, textproto.CanonicalMIMEHeaderKey(longName): {"1"}, "X-Ok": {"2"}, "X-Baz": {"3"}},
			want: http.Header{"X-Baz": {"3"}},
		},
		{
			// The zero Direction takes the stricter rule of each: Expect is
			// dropped and TE is not kept.
			name: "zero Direction drops Expect and TE",
			dir:  Direction(0),
			in:   http.Header{"Expect": {"100-continue"}, "Te": {"trailers"}, "Connection": {"x-foo"}, "X-Foo": {"1"}, "Accept": {"*/*"}},
			want: http.Header{"Accept": {"*/*"}},
		},
		{
			name: "undefined Direction drops Expect and TE",
			dir:  Direction(7),
			in:   http.Header{"Expect": {"100-continue"}, "Te": {"trailers"}, "Accept": {"*/*"}},
			want: http.Header{"Accept": {"*/*"}},
		},
		{
			name: "forwarding and trace fields untouched",
			dir:  TowardUpstream,
			in:   http.Header{"X-Forwarded-For": {"1.2.3.4"}, "Forwarded": {"for=1.2.3.4"}, "Traceparent": {"00-x"}},
			want: http.Header{"X-Forwarded-For": {"1.2.3.4"}, "Forwarded": {"for=1.2.3.4"}, "Traceparent": {"00-x"}},
		},
		{
			name: "empty header",
			dir:  TowardClient,
			in:   http.Header{},
			want: http.Header{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			StripHopByHop(tt.in, tt.dir)
			if !reflect.DeepEqual(tt.in, tt.want) {
				t.Errorf("StripHopByHop = %q, want %q", tt.in, tt.want)
			}
		})
	}
}

func TestStripHopByHopDoesNotAllocate(t *testing.T) {
	h := http.Header{
		"Content-Type": {"application/json"}, "Accept": {"*/*"},
		"User-Agent": {"test"}, "X-Forwarded-For": {"1.2.3.4"},
	}
	// The second named field is over 32 bytes, the stack buffer size of a
	// string conversion that escapes nowhere.
	const longField = "X-A-Rather-Long-Connection-Named-Field"
	conn, foo := []string{"keep-alive, X-Foo, " + longField}, []string{"1"}
	allocs := testing.AllocsPerRun(100, func() {
		h["Connection"], h["X-Foo"], h[longField] = conn, foo, foo
		StripHopByHop(h, TowardUpstream)
		if _, ok := h["X-Foo"]; ok || len(h) != 4 {
			t.Fatalf("StripHopByHop left %q", h)
		}
	})
	if allocs != 0 {
		t.Errorf("allocations = %v, want 0", allocs)
	}
}

// TestStripConnectionOptionsReq22 covers the admission strip of 04 req 22
// and 05 req 28: Connection and the end-to-end fields it names go, the
// fixed hop-by-hop fields stay for StripHopByHop at leg build, which then
// keeps "TE: trailers" although "Connection: TE" named it (RFC 9110
// section 10.1.4).
func TestStripConnectionOptionsReq22(t *testing.T) {
	tests := []struct {
		name string
		in   http.Header
		want http.Header
	}{
		{
			name: "no Connection",
			in:   http.Header{"Keep-Alive": {"1"}, "Accept": {"*/*"}},
			want: http.Header{"Keep-Alive": {"1"}, "Accept": {"*/*"}},
		},
		{
			name: "named end-to-end fields removed, fixed fields kept",
			in: http.Header{
				"Connection": {"TE, keep-alive, X-Shop-Consumer, upgrade"}, "Te": {"trailers"},
				"Keep-Alive": {"timeout=5"}, "Upgrade": {"websocket"}, "X-Shop-Consumer": {"gold"},
				"Accept": {"*/*"},
			},
			want: http.Header{"Te": {"trailers"}, "Keep-Alive": {"timeout=5"}, "Upgrade": {"websocket"}, "Accept": {"*/*"}},
		},
		{
			name: "Expect named",
			in:   http.Header{"Connection": {"expect"}, "Expect": {"100-continue"}},
			want: http.Header{},
		},
		{
			name: "non-canonical spellings",
			in:   http.Header{"connection": {"x-a, TE"}, "CONNECTION": {"X-B"}, "x-a": {"1"}, "X-B": {"2"}, "te": {"trailers"}, "X-C": {"3"}},
			want: http.Header{"te": {"trailers"}, "X-C": {"3"}},
		},
		{
			name: "non-token elements name nothing",
			in:   http.Header{"Connection": {"x y, (x)"}, "x y": {"1"}},
			want: http.Header{"x y": {"1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			StripConnectionOptions(tt.in)
			if !reflect.DeepEqual(tt.in, tt.want) {
				t.Errorf("StripConnectionOptions = %q, want %q", tt.in, tt.want)
			}
		})
	}
}

// TestStripAtAdmissionKeepsPolicyFields is the 05 req 28 order: the client
// names a field in Connection, the admission strip removes it, a headers
// Policy then sets the same field, and the leg build strip keeps the
// Policy's value and "TE: trailers".
func TestStripAtAdmissionKeepsPolicyFields(t *testing.T) {
	h := http.Header{"Connection": {"TE, X-Shop-Consumer"}, "Te": {"trailers"}, "X-Shop-Consumer": {"forged"}}
	StripConnectionOptions(h)
	h["X-Shop-Consumer"] = []string{"gold"} // the request Phases
	StripHopByHop(h, TowardUpstream)
	want := http.Header{"Te": {"trailers"}, "X-Shop-Consumer": {"gold"}}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("header = %q, want %q", h, want)
	}
}

// TestStripConnectionOptionsDoesNotAllocate keeps the admission strip of
// 04 req 22 free of allocations on a canonical header.
func TestStripConnectionOptionsDoesNotAllocate(t *testing.T) {
	h := http.Header{"Content-Type": {"application/json"}, "Accept": {"*/*"}, "Keep-Alive": {"1"}}
	conn, foo := []string{"keep-alive, X-Foo"}, []string{"1"}
	allocs := testing.AllocsPerRun(100, func() {
		h["Connection"], h["X-Foo"] = conn, foo
		StripConnectionOptions(h)
		if _, ok := h["X-Foo"]; ok || len(h) != 3 {
			t.Fatalf("StripConnectionOptions left %q", h)
		}
	})
	if allocs != 0 {
		t.Errorf("allocations = %v, want 0", allocs)
	}
}

// adversarialHeader is the review case for 07 req 33: n fields and a
// Connection value of size bytes made of short options that name none of
// them, "a,b,a,b,..." so that no option repeats the one before it.
func adversarialHeader(n, size int) (http.Header, []string) {
	h := make(http.Header, n+1)
	for i := range n {
		h["X-Field-"+strconv.Itoa(i)] = []string{"v"}
	}
	conn := strings.Repeat("a,b,", size/4)
	return h, []string{conn}
}

// TestStripHopByHopLongConnectionDoesNotAllocate checks that a long
// Connection value against 250 fields costs no allocation per option (07
// req 33); field C shares the options' length, so every option is looked
// up. The time bound is BenchmarkStripHopByHopLongConnection.
func TestStripHopByHopLongConnectionDoesNotAllocate(t *testing.T) {
	h, conn := adversarialHeader(250, 64<<10)
	h["C"] = []string{"v"}
	allocs := testing.AllocsPerRun(10, func() {
		h["Connection"] = conn
		StripHopByHop(h, TowardUpstream)
		if len(h) != 251 {
			t.Fatalf("StripHopByHop left %d fields, want 251", len(h))
		}
	})
	if allocs != 0 {
		t.Errorf("allocations = %v, want 0", allocs)
	}
}

// TestCanonicalKeyMatchesTextproto checks that the form StripHopByHop
// looks Connection options up by is the one net/http stores keys in, so
// that "Connection: x-foo" finds X-Foo (07 req 33, spec 07 test I4).
func TestCanonicalKeyMatchesTextproto(t *testing.T) {
	for _, s := range []string{
		"", "a", "A", "x-foo", "X-FOO", "x--foo", "-x", "x-", "content-md5", "X_foo-bar",
		"WWW-Authenticate", "x foo", "X Foo", "\x80", "x:y", "1-a", "a-1b", longName,
	} {
		canon := textproto.CanonicalMIMEHeaderKey(s)
		got, ok := appendCanonicalToken(nil, s)
		if ok != IsToken(s) {
			t.Errorf("appendCanonicalToken(%q) ok = %v, want %v", s, ok, IsToken(s))
		}
		if ok && string(got) != canon {
			t.Errorf("appendCanonicalToken(%q) = %q, textproto %q", s, got, canon)
		}
		if got, want := isCanonicalKey(s), canon == s; got != want {
			t.Errorf("isCanonicalKey(%q) = %v, want %v", s, got, want)
		}
	}
}
