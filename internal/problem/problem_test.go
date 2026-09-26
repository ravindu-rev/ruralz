// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package problem

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Tests for architecture section 2.3 (WP-01) and section 0 convention 5:
// RFC 9457 documents with members title, status, code, requestId, detail
// (R-27), escaping, Write headers, and the Document decoder of spec 10
// section 2.2. FuzzProblemAppend is spec 04 section 6's target.

const traceID = "0af7651916cd43dd8448eb211c80319c"

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: the test reads its own golden files.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAppendGolden(t *testing.T) {
	// The detail member comes last (R-27); RZ-RT-009 names a schema keyword
	// location only.
	p := New("RZ-RT-009", "4bf92f3577b34da6a3ce929d0e0e4736")
	p.Detail = "/properties/items/minItems"
	if got, want := Append(nil, p), golden(t, "rt-009-detail.json"); !bytes.Equal(got, want) {
		t.Fatalf("Append =\n%s\nwant\n%s", got, want)
	}
}

// dumpWrite renders what Write sent in the golden format: the status line,
// every header as "Name: value" sorted by name, a blank line, the body and
// a final newline.
func dumpWrite(rec *httptest.ResponseRecorder) []byte {
	var b bytes.Buffer
	b.WriteString(strconv.Itoa(rec.Code))
	b.WriteByte('\n')
	h := rec.Header()
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		for _, v := range h[k] {
			b.WriteString(k + ": " + v + "\n")
		}
	}
	b.WriteByte('\n')
	b.Write(rec.Body.Bytes())
	b.WriteByte('\n')
	return b.Bytes()
}

// TestWriteGolden is spec 04 section 6 test item 4: golden bytes, with
// Write's header set (application/problem+json, no-store, exact
// Content-Length), for RZ-RT-001 to 007, 011, 012, 014, 016 and 017.
// RZ-RT-014 and RZ-RT-016 register only a StatusNote ("503 before
// commit"), so the handler sets 503 explicitly (04 req 38).
func TestWriteGolden(t *testing.T) {
	status503 := func(code string) Problem {
		p := New(code, traceID)
		p.Status = http.StatusServiceUnavailable
		return p
	}
	tests := []struct {
		file string
		p    Problem
	}{
		{"rt-001.http", New("RZ-RT-001", traceID)},
		{"rt-002.http", New("RZ-RT-002", traceID)},
		{"rt-003.http", New("RZ-RT-003", traceID)},
		{"rt-004.http", New("RZ-RT-004", traceID)},
		{"rt-005.http", New("RZ-RT-005", traceID)},
		{"rt-006.http", New("RZ-RT-006", traceID)},
		{"rt-007.http", New("RZ-RT-007", traceID)},
		{"rt-011.http", New("RZ-RT-011", traceID)},
		{"rt-012.http", New("RZ-RT-012", traceID)},
		{"rt-014.http", status503("RZ-RT-014")},
		{"rt-016.http", status503("RZ-RT-016")},
		{"rt-017.http", New("RZ-RT-017", traceID)},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, tt.p, nil)
			want := golden(t, tt.file)
			if got := dumpWrite(rec); !bytes.Equal(got, want) {
				t.Fatalf("Write =\n%s\nwant\n%s", got, want)
			}
			// The golden body is exactly what Append produces.
			_, body, ok := bytes.Cut(want, []byte("\n\n"))
			if !ok || !bytes.Equal(Append(nil, tt.p), bytes.TrimSuffix(body, []byte("\n"))) {
				t.Fatalf("Append = %s, want the golden body %s", Append(nil, tt.p), body)
			}
		})
	}
}

func TestMemberOrder(t *testing.T) {
	// R-27: title, status, code, requestId, then detail last; no type member.
	b := Append(nil, Problem{Title: "t", Status: 503, Code: "RZ-RT-005", RequestID: traceID, Detail: "d"})
	dec := json.NewDecoder(bytes.NewReader(b))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"title", "status", "code", "requestId", "detail"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("members %v, want %v", keys, want)
	}
}

func TestAppendReusesBuffer(t *testing.T) {
	prefix := []byte("x")
	b := Append(prefix, New("RZ-RT-001", traceID))
	if !bytes.HasPrefix(b, []byte(`x{"title":`)) {
		t.Fatalf("Append did not append to dst: %s", b)
	}
	// Nothing on the request path allocates by itself (architecture 2).
	buf := make([]byte, 0, 512)
	p := New("RZ-RT-005", traceID)
	if n := testing.AllocsPerRun(100, func() { buf = Append(buf[:0], p) }); n != 0 {
		t.Fatalf("Append into a large enough buffer allocates %v times", n)
	}
}

func TestEscaping(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "abc", `"abc"`},
		{"quote and backslash", `a"b\c`, `"a\"b\\c"`},
		{"control bytes", "a\x00b\nc\td\x1f", `"a\u0000b\u000ac\u0009d\u001f"`},
		{"DEL is not escaped", "\x7f", "\"\x7f\""},
		{"no HTML escaping", "<a href='x'>&</a>", `"<a href='x'>&</a>"`},
		{"valid multibyte kept", "héllo, 世界 😀", `"héllo, 世界 😀"`},
		{"invalid UTF-8 replaced", "a\xffb", `"a` + "\uFFFD" + `b"`},
		{"each invalid byte replaced", "\xc3\x28", `"` + "\uFFFD(" + `"`},
		{"truncated rune", "ok\xe4\xb8", `"ok` + "\uFFFD\uFFFD" + `"`},
		{"line separators kept", "\u2028\u2029", "\"\u2028\u2029\""},
		{"empty", "", `""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(appendString(nil, tt.in))
			if got != tt.want {
				t.Fatalf("appendString(%q) = %s, want %s", tt.in, got, tt.want)
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("appendString(%q) is not valid JSON: %s", tt.in, got)
			}
		})
	}
}

func TestWrite(t *testing.T) {
	tests := []struct {
		name  string
		p     Problem
		extra http.Header
	}{
		{"plain", New("RZ-RT-001", traceID), nil},
		{"challenge", New("RZ-AUTH-001", traceID), http.Header{"Www-Authenticate": {`Bearer realm="api"`}}},
		{"retry after", New("RZ-RT-005", traceID), http.Header{"Retry-After": {"1"}}},
		// A document larger than Write's 256-byte stack buffer.
		{"long detail", Problem{Title: "Request body failed validation", Status: 400, Code: "RZ-RT-009", RequestID: traceID, Detail: strings.Repeat("/properties/x", 40)}, nil},
		// extra cannot override the document headers.
		{"extra conflicts", New("RZ-RT-004", traceID), http.Header{
			"Content-Type":   {"text/html"},
			"Cache-Control":  {"public"},
			"Content-Length": {"1"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, tt.p, tt.extra)
			res := rec.Result()
			defer func() { _ = res.Body.Close() }()
			body := rec.Body.Bytes()
			if res.StatusCode != tt.p.Status {
				t.Errorf("status %d, want %d", res.StatusCode, tt.p.Status)
			}
			if got := res.Header.Get("Content-Type"); got != ContentType {
				t.Errorf("Content-Type %q, want %q", got, ContentType)
			}
			if got := res.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control %q, want no-store", got)
			}
			if got := res.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
				t.Errorf("Content-Length %q, body has %d bytes", got, len(body))
			}
			if !bytes.Equal(body, Append(nil, tt.p)) {
				t.Errorf("body %s, want %s", body, Append(nil, tt.p))
			}
			for k, vs := range tt.extra {
				switch k {
				case "Content-Type", "Cache-Control", "Content-Length":
					continue
				}
				if got := res.Header.Values(k); strings.Join(got, ",") != strings.Join(vs, ",") {
					t.Errorf("header %s = %v, want %v", k, got, vs)
				}
			}
		})
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		code   string
		status int
		title  string
	}{
		{"RZ-RT-001", 404, "No matching Route"},
		{"RZ-RT-016", 500, "Node draining"},                  // StatusNote: the caller sets 503
		{"RZ-RT-014", 500, "Configuration snapshot retired"}, // StatusNote: the caller sets 503
		{"RZ-RT-019", 503, "Tap subscriber limit reached"},
		{"RZ-AUTH-001", 401, "Unauthorized"}, // generic by status
		{"RZ-AUTH-008", 421, "Misdirected Request"},
		{"RZ-AUTH-013", 403, "Forbidden"},
		{"RZ-UP-003", 504, "Gateway Timeout"},
		{"RZ-UP-011", 502, "Bad Gateway"},
		{"RZ-RL-002", 429, "Too Many Requests"},
		{"RZ-CFG-026", 500, "Internal Server Error"}, // no single status
		{"not-a-code", 500, "Internal Server Error"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			p := New(tt.code, traceID)
			if p.Status != tt.status || p.Title != tt.title || p.Code != tt.code || p.RequestID != traceID || p.Detail != "" {
				t.Fatalf("New = %+v, want status %d title %q", p, tt.status, tt.title)
			}
		})
	}
}

func TestTitle(t *testing.T) {
	// Every RZ-RT code that produces a document has its own title;
	// RZ-RT-013 ends a stream and never produces one.
	for _, c := range errcode.All() {
		if c.Area != errcode.AreaRT || c.ID == "RZ-RT-013" {
			continue
		}
		title := Title(c.ID, c.Status)
		if title == "" || title == http.StatusText(c.Status) || title == "Error" {
			t.Errorf("%s has no dedicated title (got %q)", c.ID, title)
		}
	}
	if got := Title("RZ-RT-013", 0); got != "Error" {
		t.Errorf("Title(RZ-RT-013, 0) = %q, want Error", got)
	}
	if got := Title("RZ-AUTH-010", 403); got != "Forbidden" {
		t.Errorf("Title(RZ-AUTH-010, 403) = %q, want Forbidden", got)
	}
	if got := Title("", 599); got != "Error" {
		t.Errorf("Title for an unknown status = %q, want Error", got)
	}
}

func TestDecode(t *testing.T) {
	docs := []Problem{
		New("RZ-RT-001", traceID),
		{Title: "Request body failed validation", Status: 400, Code: "RZ-RT-009", RequestID: traceID, Detail: "a\"b\\c\n\x01<&>é"},
	}
	for _, p := range docs {
		d, err := Decode(Append(nil, p))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		want := Document{Title: p.Title, Status: p.Status, Code: p.Code, RequestID: p.RequestID, Detail: p.Detail}
		if d != want {
			t.Fatalf("Decode = %+v, want %+v", d, want)
		}
	}

	// Documents from other servers: type is read, unknown members ignored.
	d, err := Decode([]byte(`{"type":"about:blank","title":"Not Found","status":404,"instance":"/x","extra":{"a":[1]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if d != (Document{Type: "about:blank", Title: "Not Found", Status: 404}) {
		t.Fatalf("Decode = %+v", d)
	}

	for _, bad := range []string{"", "{", `{"status":"404"}`, "[]", "null x"} {
		d, err := Decode([]byte(bad))
		if err == nil {
			t.Errorf("Decode(%q) succeeded", bad)
			continue
		}
		if !strings.HasPrefix(err.Error(), "problem document: ") {
			t.Errorf("Decode(%q) error %q lacks its prefix", bad, err)
		}
		var syn *json.SyntaxError
		var typ *json.UnmarshalTypeError
		if !errors.As(err, &syn) && !errors.As(err, &typ) {
			t.Errorf("Decode(%q) error %v does not wrap the JSON error", bad, err)
		}
		if d != (Document{}) {
			t.Errorf("Decode(%q) returned %+v with its error", bad, d)
		}
	}
}

// FuzzProblemAppend (spec 04 section 6): the document is always valid JSON,
// decodes to the same members (invalid UTF-8 replaced byte by byte) and
// keeps its member order.
func FuzzProblemAppend(f *testing.F) {
	f.Add("No matching Route", 404, "RZ-RT-001", traceID, "")
	f.Add("t\x00\"\\", 503, "RZ-RT-004", "x\xff", "detail\n\u2028<>&")
	f.Add("", 0, "", "", "\xed\xa0\x80")
	f.Fuzz(func(t *testing.T, title string, status int, code, requestID, detail string) {
		p := Problem{Title: title, Status: status, Code: code, RequestID: requestID, Detail: detail}
		b := Append(nil, p)
		if !json.Valid(b) {
			t.Fatalf("invalid JSON: %q", b)
		}
		if !bytes.HasPrefix(b, []byte(`{"title":`)) {
			t.Fatalf("title is not the first member: %q", b)
		}
		d, err := Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		fix := func(s string) string { return string([]rune(s)) }
		want := Document{Title: fix(title), Status: status, Code: fix(code), RequestID: fix(requestID), Detail: fix(detail)}
		if d != want {
			t.Fatalf("round trip %+v, want %+v", d, want)
		}
	})
}

func BenchmarkWrite(b *testing.B) {
	p := New("RZ-RT-005", traceID)
	w := discardWriter{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		Write(w, p, nil)
	}
}

type discardWriter struct{ h http.Header }

func (w discardWriter) Header() http.Header       { return w.h }
func (discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (discardWriter) WriteHeader(int)             {}
