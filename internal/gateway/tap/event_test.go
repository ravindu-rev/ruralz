// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tap

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/redact"
)

// decodeLine decodes one encoded line as the CLI does.
func decodeLine(t *testing.T, line []byte) adminapi.TapEvent {
	t.Helper()
	if !bytes.HasSuffix(line, []byte("\n")) || bytes.Count(line, []byte("\n")) != 1 {
		t.Fatalf("line %q is not one NDJSON line", line)
	}
	var e adminapi.TapEvent
	if err := json.Unmarshal(line, &e); err != nil {
		t.Fatalf("line %q: %v", line, err)
	}
	return e
}

// TestLineIsTheAdminTapEvent checks that a line is the adminapi.TapEvent
// wire form, byte for byte: the documented members in their documented
// order, time in RFC 3339 UTC, the trace ID in lowercase hex, durationMs
// in milliseconds and empty optional members omitted.
func TestLineIsTheAdminTapEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func() (Event, adminapi.TapEvent)
	}{
		{"full", fullEvent},
		{"sparse", sparseEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, want := tc.make()
			b, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			b = append(b, '\n')
			if got := appendLine(nil, &ev); !bytes.Equal(got, b) {
				t.Fatalf("appendLine =\n%s\nwant\n%s", got, b)
			}
		})
	}
}

// TestLineRedactsCredentials checks O4 on /tap: every value of a fixed
// credential header, of a Revision credential header and of a header
// whose name contains token, secret, key, password, session or auth reads
// [REDACTED], in any case; other headers are copied; names are lowercase.
func TestLineRedactsCredentials(t *testing.T) {
	ev, _ := sparseEvent()
	ev.Credentials = redact.New("X-Shop-Credential")
	ev.RequestHeaders = http.Header{
		"Authorization":       {"Bearer abc.def"},
		"Proxy-Authorization": {"Basic Zm9vOmJhcg=="},
		"Cookie":              {"sid=1", "theme=dark"},
		"X-Shop-Credential":   {"shop-value"},
		"X-Api-Key":           {"k1"},
		"X-Upstream-TOKEN":    {"t1"},
		"X-Client-Secret":     {"s1"},
		"X-Password":          {"p1"},
		"X-Session-Id":        {"42"},
		"X-Auth-User":         {"alice"},
		"Accept":              {"text/plain"},
	}
	ev.ResponseHeaders = http.Header{"Set-Cookie": {"sid=2"}, "Content-Type": {"text/plain"}}
	got := decodeLine(t, appendLine(nil, &ev))
	r := []string{redact.Marker}
	wantReq := map[string][]string{
		"authorization":       r,
		"proxy-authorization": r,
		"cookie":              {redact.Marker, redact.Marker},
		"x-shop-credential":   r,
		"x-api-key":           r,
		"x-upstream-token":    r,
		"x-client-secret":     r,
		"x-password":          r,
		"x-session-id":        r,
		"x-auth-user":         r,
		"accept":              {"text/plain"},
	}
	if !reflect.DeepEqual(got.RequestHeaders, wantReq) {
		t.Errorf("requestHeaders = %v, want %v", got.RequestHeaders, wantReq)
	}
	wantResp := map[string][]string{"set-cookie": r, "content-type": {"text/plain"}}
	if !reflect.DeepEqual(got.ResponseHeaders, wantResp) {
		t.Errorf("responseHeaders = %v, want %v", got.ResponseHeaders, wantResp)
	}
	for _, secret := range []string{"abc.def", "Zm9vOmJhcg", "sid=", "shop-value", "alice"} {
		if line := appendLine(nil, &ev); bytes.Contains(line, []byte(secret)) {
			t.Errorf("line carries %q: %s", secret, line)
		}
	}
}

// TestLineWithoutRevisionSet checks that an exchange without a snapshot
// credential set still redacts the fixed credential headers.
func TestLineWithoutRevisionSet(t *testing.T) {
	ev, _ := sparseEvent()
	ev.RequestHeaders = http.Header{"Authorization": {"Bearer abc.def"}, "X-Shop-Credential": {"v"}}
	got := decodeLine(t, appendLine(nil, &ev))
	want := map[string][]string{"authorization": {redact.Marker}, "x-shop-credential": {"v"}}
	if !reflect.DeepEqual(got.RequestHeaders, want) {
		t.Errorf("requestHeaders = %v, want %v", got.RequestHeaders, want)
	}
}

// TestLinePathHasNoQuery checks that a query string or fragment never
// reaches /tap, even when the handler passes one.
func TestLinePathHasNoQuery(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/orders", "/orders"},
		{"/orders?api_key=k&x=1", "/orders"},
		{"/orders#frag", "/orders"},
		{"/a?b#c", "/a"},
	} {
		ev, _ := sparseEvent()
		ev.Path = tc.in
		if got := decodeLine(t, appendLine(nil, &ev)).Path; got != tc.want {
			t.Errorf("path of %q = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLineEscapesEveryString checks that no member can break the line:
// quotes, backslashes, control characters and newlines are escaped, and
// invalid UTF-8 becomes U+FFFD, as encoding/json reads it.
func TestLineEscapesEveryString(t *testing.T) {
	ev, _ := sparseEvent()
	ev.Host = "a\"b\\c"
	ev.Route = "line\nbreak\r\ttab\x01\x1f"
	ev.Consumer = "bad\xffutf8\xc3"
	ev.Upstream = "ünïcode ✓"
	ev.RequestHeaders = http.Header{"X-Raw": {"v\n2", "\x00"}, "X-Bad\xff": {"ok"}}
	got := decodeLine(t, appendLine(nil, &ev))
	if got.Host != ev.Host || got.Route != ev.Route || got.Upstream != ev.Upstream {
		t.Errorf("decoded host %q route %q upstream %q", got.Host, got.Route, got.Upstream)
	}
	if want := "bad�utf8�"; got.Consumer != want {
		t.Errorf("consumer = %q, want %q", got.Consumer, want)
	}
	want := map[string][]string{"x-raw": {"v\n2", "\x00"}, "x-bad�": {"ok"}}
	if !reflect.DeepEqual(got.RequestHeaders, want) {
		t.Errorf("requestHeaders = %q, want %q", got.RequestHeaders, want)
	}
}

// TestLineHeaderOrder checks that header members are sorted by lowercase
// name, beyond the names sorted on the stack too, and that spellings of
// one name merge into one member in the byte order of the spellings.
func TestLineHeaderOrder(t *testing.T) {
	ev, _ := sparseEvent()
	h := http.Header{"x-a": {"lower"}, "X-A": {"upper"}, "B": {"b"}, "a-z": {"az"}}
	for i := range stackNames + 8 {
		h["X-Many-"+strconv.Itoa(100+i)] = []string{strconv.Itoa(i)}
	}
	ev.RequestHeaders = h
	line := appendLine(nil, &ev)
	got := decodeLine(t, line)
	if want := []string{"upper", "lower"}; !reflect.DeepEqual(got.RequestHeaders["x-a"], want) {
		t.Errorf("x-a = %v, want %v", got.RequestHeaders["x-a"], want)
	}
	if len(got.RequestHeaders) != len(h)-1 {
		t.Errorf("%d members, want %d", len(got.RequestHeaders), len(h)-1)
	}
	// Member names appear in byte order of their lowercase form.
	obj := line[bytes.Index(line, []byte(`"requestHeaders":{`))+len(`"requestHeaders":`):]
	dec := json.NewDecoder(bytes.NewReader(obj))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < len(names); i++ {
		if strings.Compare(names[i-1], names[i]) >= 0 {
			t.Fatalf("member names out of order: %v", names)
		}
	}
}

// TestLineDuration checks durationMs: milliseconds with the shortest
// decimal digits.
func TestLineDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want float64
	}{
		{0, 0},
		{time.Nanosecond, 0.000001},
		{1250 * time.Microsecond, 1.25},
		{3 * time.Second, 3000},
	} {
		ev, _ := sparseEvent()
		ev.Duration = tc.d
		if got := decodeLine(t, appendLine(nil, &ev)).DurationMs; got != tc.want {
			t.Errorf("durationMs of %v = %v, want %v", tc.d, got, tc.want)
		}
	}
}

// TestDroppedNotice checks the {"dropped":N} line, up to the longest N.
func TestDroppedNotice(t *testing.T) {
	for _, n := range []uint64{1, 3, 1<<64 - 1} {
		got := appendDropped(nil, n)
		if len(got) > maxNotice {
			t.Fatalf("notice %q longer than %d", got, maxNotice)
		}
		var d struct {
			Dropped uint64 `json:"dropped"`
		}
		if err := json.Unmarshal(got, &d); err != nil || d.Dropped != n {
			t.Fatalf("notice %q = %+v, %v; want %d", got, d, err, n)
		}
	}
	// The CLI reads the notice as adminapi.TapDropped.
	var d adminapi.TapDropped
	if err := json.Unmarshal(appendDropped(nil, 3), &d); err != nil || d.Dropped != 3 {
		t.Fatalf("TapDropped = %+v, %v; want 3", d, err)
	}
}
