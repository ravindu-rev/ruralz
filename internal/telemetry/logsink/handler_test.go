// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/slogtest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

func testIDs() traceIDs {
	return traceIDs{
		trace: [16]byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
		span:  [8]byte{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7},
	}
}

// 09 req 63, test plan 6.1 item 16: keys and their order.
func TestProcessKeysOrder_Req63(t *testing.T) {
	s, out := newSink(t, Options{NodeID: "01J9Z8Q4W6X3V5T7R2N0M1K8H4", TraceContext: traceFromContext})
	s.SetRevision("rev-0123456789ab")
	ctx := withTrace(t.Context(), testIDs())
	log := s.Logger("gateway")
	log.ErrorContext(ctx, "activation failed",
		slog.String("policy", "jwt-default"),
		slog.Any(catalog.KeyError, errors.New("boom")),
		slog.String(catalog.KeyCode, "RZ-CFG-026"),
		slog.Int("line", 7),
	)
	drain(s)
	lines := out.lines(t)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), lines)
	}
	want := []string{"time", "level", "msg", "component", "node_id", "revision", "trace_id", "span_id", "code", "error", "policy", "line"}
	if got := keys(t, lines[0]); !slices.Equal(got, want) {
		t.Fatalf("keys = %v\nwant   %v", got, want)
	}
	m := object(t, lines[0])
	for k, v := range map[string]string{
		"level":     "ERROR",
		"msg":       "activation failed",
		"component": "gateway",
		"node_id":   "01J9Z8Q4W6X3V5T7R2N0M1K8H4",
		"revision":  "rev-0123456789ab",
		"trace_id":  "4bf92f3577b34da6a3ce929d0e0e4736",
		"span_id":   "00f067aa0ba902b7",
		"code":      "RZ-CFG-026",
		"error":     "boom",
	} {
		if m[k] != v {
			t.Errorf("%s = %v, want %q", k, m[k], v)
		}
	}
}

// 09 req 63: revision is absent before the first activation; trace IDs
// only with a valid span context; component and node_id omitted when empty.
func TestProcessOptionalKeys_Req63(t *testing.T) {
	s, out := newSink(t, Options{TraceContext: traceFromContext})
	if err := s.Handler("").Handle(context.Background(), slog.NewRecord(time.Unix(1, 0), slog.LevelInfo, "started", 0)); err != nil {
		t.Fatal(err)
	}
	s.SetRevision("rev-aaaaaaaaaaaa")
	s.SetRevision("")
	s.Logger("x").InfoContext(t.Context(), "cleared")
	drain(s)
	lines := out.lines(t)
	if got := keys(t, lines[0]); !slices.Equal(got, []string{"time", "level", "msg"}) {
		t.Errorf("keys = %v", got)
	}
	if got := keys(t, lines[1]); !slices.Equal(got, []string{"time", "level", "msg", "component"}) {
		t.Errorf("keys = %v", got)
	}
}

// 09 req 63: time is RFC 3339 UTC with microseconds; level spellings.
func TestProcessTimeAndLevels_Req63(t *testing.T) {
	s, out := newSink(t, Options{Level: slog.LevelDebug})
	zone := time.FixedZone("x", 2*3600)
	at := time.Date(2026, 9, 26, 9, 4, 5, 123456789, zone)
	h := s.Handler("")
	for _, l := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		if err := h.Handle(t.Context(), slog.NewRecord(at, l, "m", 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Handle(t.Context(), slog.NewRecord(time.Time{}, slog.LevelInfo, "no time", 0)); err != nil {
		t.Fatal(err)
	}
	drain(s)
	lines := out.lines(t)
	for i, want := range []string{"DEBUG", "INFO", "WARN", "ERROR"} {
		m := object(t, lines[i])
		if m["time"] != "2026-09-26T07:04:05.123456Z" {
			t.Errorf("time = %v", m["time"])
		}
		if m["level"] != want {
			t.Errorf("level = %v, want %s", m["level"], want)
		}
	}
	if _, ok := object(t, lines[4])["time"]; ok {
		t.Errorf("zero time written: %s", lines[4])
	}
}

// Framing keys given as attributes fill their slot once, handler values
// first and record values overriding; time, level and msg are renamed.
func TestProcessSlotAttributes_Req63(t *testing.T) {
	s, out := newSink(t, Options{NodeID: "node", TraceContext: traceFromContext})
	s.SetRevision("rev-active000000")
	h := s.Handler("loader").WithAttrs([]slog.Attr{
		slog.String(catalog.KeyCode, "RZ-CFG-001"),
		slog.String("policy", "p"),
		{Key: "", Value: slog.GroupValue(slog.String(catalog.KeyComponent, "inline"))},
	})
	rec := slog.NewRecord(time.Unix(0, 0), slog.LevelWarn, "candidate rejected", 0)
	rec.AddAttrs(
		slog.String(catalog.KeyRevision, "rev-candidate000"),
		slog.String(catalog.KeyCode, "RZ-CFG-002"),
		slog.Attr{Key: slog.MessageKey, Value: slog.StringValue("spoof")},
		slog.Attr{Key: slog.TimeKey, Value: slog.IntValue(1)},
		slog.String(catalog.KeyCode, "RZ-CFG-003"),
	)
	if err := h.Handle(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	drain(s)
	line := out.lines(t)[0]
	want := []string{"time", "level", "msg", "component", "node_id", "revision", "code", "policy", "attr_msg", "attr_time"}
	if got := keys(t, line); !slices.Equal(got, want) {
		t.Fatalf("keys = %v\nwant   %v", got, want)
	}
	m := object(t, line)
	if m["component"] != "inline" || m["revision"] != "rev-candidate000" || m["code"] != "RZ-CFG-003" || m["msg"] != "candidate rejected" {
		t.Errorf("unexpected slots: %s", line)
	}
}

// Groups follow the slog handler rules: nesting, empty groups omitted,
// inline groups, handler attributes before record attributes.
func TestProcessGroups(t *testing.T) {
	s, out := newSink(t, Options{})
	base := s.Handler("c")
	h := base.WithAttrs([]slog.Attr{slog.Int("a", 1)}).WithGroup("g").WithAttrs([]slog.Attr{slog.Int("b", 2)}).WithGroup("h")
	ctx := t.Context()
	logger := slog.New(h)
	logger.InfoContext(ctx, "one", slog.Int("c", 3))
	logger.InfoContext(ctx, "two")
	logger.InfoContext(ctx, "three", slog.Group("empty"), slog.Group("", slog.Int("d", 4)))
	slog.New(base.WithGroup("unused")).InfoContext(ctx, "four")
	drain(s)
	lines := out.lines(t)
	want := []string{
		`"component":"c","a":1,"g":{"b":2,"h":{"c":3}}}`,
		`"component":"c","a":1,"g":{"b":2}}`,
		`"component":"c","a":1,"g":{"b":2,"h":{"d":4}}}`,
		`"component":"c"}`,
	}
	for i, w := range want {
		if !strings.HasSuffix(lines[i], w) {
			t.Errorf("line %d = %s\nwant suffix %s", i, lines[i], w)
		}
	}
}

// The handler satisfies the log/slog handler contract.
func TestProcessHandlerContract(t *testing.T) {
	var s *Sink
	var out *syncBuffer
	newHandler := func(t *testing.T) slog.Handler {
		s, out = newSink(t, Options{})
		return s.Handler("")
	}
	result := func(t *testing.T) map[string]any {
		drain(s)
		lines := out.lines(t)
		if len(lines) != 1 {
			t.Fatalf("got %d lines", len(lines))
		}
		return object(t, lines[0])
	}
	slogtest.Run(t, newHandler, result)
}

// 09 req 4 and test plan 6.1 item 16: secret.Value logs [REDACTED]; the
// handler never calls Reveal.
func TestProcessSecretRedaction_Req4(t *testing.T) {
	const canary = "canary-7f3a9c"
	v := secret.NewValue([]byte(canary))
	type holder struct {
		Key secret.Value `json:"key"`
	}
	s, out := newSink(t, Options{})
	ctx := t.Context()
	log := slog.New(s.Handler("secret").WithAttrs([]slog.Attr{slog.Any("with", v)}))
	log.WarnContext(ctx, "rotation",
		slog.Any("value", v),
		slog.Any("pointer", &v),
		slog.Group("grp", slog.Any("nested", v)),
		slog.Any("struct", holder{Key: v}),
		slog.String("formatted", fmt.Sprintf("%v %s %q %#v %x", v, v, v, v, v)),
		slog.Any(catalog.KeyError, fmt.Errorf("resolve: %v", v)),
	)
	drain(s)
	got := out.String()
	if strings.Contains(got, canary) {
		t.Fatalf("secret leaked: %s", got)
	}
	if n := strings.Count(got, secret.Redacted); n < 6 {
		t.Fatalf("want [REDACTED] for every form, got %d in %s", n, got)
	}
}

// 06 req 92, 04 req 75: credential headers and sensitive header names are
// redacted in header maps and as attribute keys; others are kept.
func TestProcessCredentialHeaders_Req92(t *testing.T) {
	s, out := newSink(t, Options{})
	s.SetCredentialHeaders([]string{"X-Tenant-Credential"})
	h := http.Header{
		"Authorization":       {"Bearer abc.def"},
		"Proxy-Authorization": {"Basic Zm9vOmJhcg=="},
		"Cookie":              {"sid=s3cr3t"},
		"Set-Cookie":          {"sid=s3cr3t2"},
		"X-Api-Key":           {"k-0123456789012345678901"},
		"X-Tenant-Credential": {"tc-value"},
		"X-Session-Id":        {"session-value"},
		"Accept":              {"application/json"},
	}
	ctx := t.Context()
	log := s.Logger("admin")
	log.InfoContext(ctx, "request",
		slog.Any("headers", h),
		slog.Any("plain", map[string][]string{"x-auth-token": {"tok-value"}}),
		slog.String("authorization", "Bearer attr-value"),
		slog.Attr{Key: http.CanonicalHeaderKey("set-cookie"), Value: slog.StringValue("cookie-attr")}, // header spelling as a key
		slog.String("x_tenant_credential", "tc-attr"),
		slog.String("accept", "text/plain"),
	)
	drain(s)
	got := out.String()
	for _, leak := range []string{"abc.def", "Zm9vOmJhcg", "s3cr3t", "k-0123", "tc-value", "session-value", "tok-value", "attr-value", "cookie-attr", "tc-attr"} {
		if strings.Contains(got, leak) {
			t.Errorf("credential %q leaked: %s", leak, got)
		}
	}
	for _, keep := range []string{"application/json", "text/plain"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q missing: %s", keep, got)
		}
	}
}

// 09 req 4: query strings and user information never reach a log line,
// through URLs, requests or url.Error chains.
func TestProcessURLRedaction_Req4(t *testing.T) {
	u, err := url.Parse("https://user:pw-secret@idp.example/token?client_secret=q-secret#frag")
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://gw.example/orders?api_key=q2-secret", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	uerr := &url.Error{Op: "Post", URL: u.String(), Err: errors.New("connection refused")}
	s, out := newSink(t, Options{})
	ctx := t.Context()
	s.Logger("egress").ErrorContext(ctx, "fetch failed",
		slog.Any("url", u),
		slog.Any("url_value", *u),
		slog.Any("request", req),
		slog.Any(catalog.KeyError, fmt.Errorf("refresh: %w", uerr)),
		slog.Any("joined", errors.Join(errors.New("first"), uerr)),
		slog.Any("nil_request", (*http.Request)(nil)),
		slog.Any("query", url.Values{"token": {"q3-secret"}}),
		slog.Any("errs", []error{uerr, nil}),
	)
	drain(s)
	got := out.String()
	for _, leak := range []string{"pw-secret", "q-secret", "q2-secret", "q3-secret", "frag", "user:"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q leaked: %s", leak, got)
		}
	}
	m := object(t, out.lines(t)[0])
	if m["url"] != "https://idp.example/token" {
		t.Errorf("url = %v", m["url"])
	}
	if m["error"] != `refresh: Post "https://idp.example/token": connection refused` {
		t.Errorf("error = %v", m["error"])
	}
	r, _ := m["request"].(map[string]any)
	if r["method"] != "GET" || r["host"] != "gw.example" || r["path"] != "/orders" {
		t.Errorf("request = %v", m["request"])
	}
	if m["nil_request"] != "<nil>" {
		t.Errorf("nil_request = %v", m["nil_request"])
	}
	if m["query"] != "[REDACTED]" {
		t.Errorf("query = %v", m["query"])
	}
	if errs, _ := m["errs"].([]any); len(errs) != 2 || errs[0] != `Post "https://idp.example/token": connection refused` || errs[1] != nil {
		t.Errorf("errs = %v", m["errs"])
	}
}

// 09 req 4, 06 req 92: a *url.Error prints its URL with %q, so a URL whose
// query holds a quote, a backslash or a control byte appears escaped in
// the text; the query never reaches the line, for the error alone,
// wrapped, joined, wrapped twice, or printed by a wrapper itself.
func TestProcessURLErrorQuoting_Req4(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"quote", `http://up.example/p?token=abc"def`, "http://up.example/p"},
		{"backslash", `http://up.example/p?token=abc\def`, "http://up.example/p"},
		{"del byte", "http://up.example/p?token=abc\x7fdef", "http://up.example/p"},
		{"control byte", "http://up.example/p?token=abc\x01def", "http://up.example/p"},
		{"userinfo and quote", `http://u:pw-leak@up.example/p?token="abc"`, "http://up.example/p"},
		{"unicode", "http://up.example/\u00e9?token=abc\u2028def", "http://up.example/%C3%A9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uerr := &url.Error{Op: "Get", URL: tt.raw, Err: errors.New("refused")}
			errs := map[string]error{
				"direct":      uerr,
				"wrapped":     fmt.Errorf("fwd: %w", uerr),
				"joined":      errors.Join(errors.New("first"), uerr),
				"nested":      fmt.Errorf("leg: %w", fmt.Errorf("fwd: %w", uerr)),
				"printed raw": fmt.Errorf("fwd %s: %w", tt.raw, uerr),
				"printed %q":  fmt.Errorf("fwd %q: %w", tt.raw, uerr),
			}
			for form, err := range errs {
				s, out := newSink(t, Options{})
				s.Logger("egress").ErrorContext(t.Context(), "upstream failed", slog.Any(catalog.KeyError, err))
				drain(s)
				line := out.String()
				for _, leak := range []string{"token", "abc", "pw-leak"} {
					if strings.Contains(line, leak) {
						t.Errorf("%s: %q leaked: %s", form, leak, line)
					}
				}
				got, _ := object(t, out.lines(t)[0])[catalog.KeyError].(string)
				if want := `Get "` + tt.want + `": refused`; !strings.Contains(got, want) {
					t.Errorf("%s: error = %q, want it to contain %q", form, got, want)
				}
			}
		})
	}
}

// panicWrapper wraps an error and panics in Error.
type panicWrapper struct{ err error }

func (p panicWrapper) Error() string { panic("boom") }
func (p panicWrapper) Unwrap() error { return p.err }

// A nil *url.Error, or an error holding a *url.Error whose Error panics,
// never crashes the logging goroutine and never leaks the query.
func TestProcessURLErrorPanics(t *testing.T) {
	s, out := newSink(t, Options{})
	var ne *nilError
	s.Logger("x").ErrorContext(t.Context(), "m",
		slog.Any("nil_url_error", error((*url.Error)(nil))),
		slog.Any("panicking", panicWrapper{err: &url.Error{Op: "Get", URL: "http://h/?q=leak", Err: errors.New("x")}}),
		slog.Any("nil_inner", &url.Error{Op: "Get", URL: "http://h/?q=leak", Err: ne}),
	)
	drain(s)
	line := out.lines(t)[0]
	if strings.Contains(line, "leak") {
		t.Fatalf("query leaked: %s", line)
	}
	m := object(t, line)
	if m["nil_url_error"] != "<nil>" || m["panicking"] != "!PANIC: boom" || m["nil_inner"] != `Get "http://h/": <nil>` {
		t.Errorf("line = %s", line)
	}
}

// 09 req 4 and 65: a typed-nil error whose Unwrap dereferences its
// receiver neither panics in Handle or WithAttrs nor breaks the
// accounting; it is written as slog.JSONHandler writes it.
func TestProcessTypedNilErrors(t *testing.T) {
	s, out := newSink(t, Options{})
	s.Logger("x").ErrorContext(t.Context(), "m",
		slog.Any("path_error", error((*fs.PathError)(nil))),
		slog.Any("op_error", error((*net.OpError)(nil))),
		slog.Any("joined", errors.Join(errors.New("a"), (*fs.PathError)(nil))),
	)
	s.Logger("x").With(slog.Any("with_error", error((*fs.PathError)(nil)))).InfoContext(t.Context(), "w")
	drain(s)
	lines := out.lines(t)
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	m := object(t, lines[0])
	if m["path_error"] != "<nil>" || m["op_error"] != "<nil>" {
		t.Errorf("line = %s", lines[0])
	}
	if j, _ := m["joined"].(string); !strings.HasPrefix(j, "!PANIC:") {
		t.Errorf("joined = %v", m["joined"])
	}
	if w := object(t, lines[1]); w["with_error"] != "<nil>" {
		t.Errorf("line = %s", lines[1])
	}
	if st := s.Stats(); st.Produced != uint64(len(lines))+st.QueueFull {
		t.Errorf("stats = %+v, lines = %d", st, len(lines))
	}
}

// 06 req 92, 09 req 4 and 61: one pointer to a header map, byte slice or
// URL is redacted and copied like the value itself; other values are
// queued by reference (documented in the package comment).
func TestProcessPointerValues_Req92(t *testing.T) {
	s, out := newSink(t, Options{})
	h := http.Header{"Authorization": {"Bearer leak-ptr"}, "Accept": {"a"}}
	m := map[string][]string{"Cookie": {"leak-map-ptr"}}
	b := []byte("before")
	u, err := url.Parse("https://idp.example/token?client_secret=leak-url-ptr")
	if err != nil {
		t.Fatal(err)
	}
	var nilHeader *http.Header
	var nilBytes *[]byte
	var nilURL **url.URL
	s.Logger("x").InfoContext(t.Context(), "m",
		slog.Any("h", &h), slog.Any("m", &m), slog.Any("b", &b), slog.Any("u", &u),
		slog.Any("nh", nilHeader), slog.Any("nb", nilBytes), slog.Any("nu", nilURL))
	h.Set("Accept", "changed")
	copy(b, "AFTER!")
	drain(s)
	line := out.lines(t)[0]
	if strings.Contains(line, "leak") {
		t.Fatalf("pointer value leaked: %s", line)
	}
	got := object(t, line)
	if fmt.Sprint(got["h"]) != "map[Accept:[a] Authorization:[[REDACTED]]]" || got["b"] != "YmVmb3Jl" ||
		got["u"] != "https://idp.example/token" || got["nh"] != nil || got["nb"] != nil || got["nu"] != nil {
		t.Errorf("line = %s", line)
	}
}

// 09 req 61: the byte bound counts what freeze copies. A large byte slice
// is dropped with queue_full before it is cloned; slices that fit reserve
// their full length; an error's redacted text is trued up after freezing.
func TestQueueBytesCountCopies_Req61(t *testing.T) {
	s, out := newSink(t, Options{})
	log := s.Logger("x")
	ctx := t.Context()
	log.InfoContext(ctx, "huge", slog.Any("b", make([]byte, 4<<20)))
	if st := s.Stats(); st.QueueFull != 1 || s.bytes.Load() != 0 {
		t.Fatalf("stats = %+v reserved %d, want the 4 MiB slice dropped", st, s.bytes.Load())
	}
	for range 3 {
		log.InfoContext(ctx, "big", slog.Any("b", make([]byte, 400<<10)))
	}
	if st := s.Stats(); st.QueueFull != 2 || s.bytes.Load() < 800<<10 {
		t.Fatalf("stats = %+v reserved %d, want two 400 KiB slices queued", st, s.bytes.Load())
	}
	drain(s)
	if s.bytes.Load() != 0 || len(out.lines(t)) != 2 {
		t.Fatalf("reserved %d after drain", s.bytes.Load())
	}

	s2, _ := newSink(t, Options{})
	long := &url.Error{Op: "Get", URL: "http://h/" + strings.Repeat("p", 600<<10) + "?q=1", Err: errors.New("x")}
	for range 2 {
		s2.Logger("x").ErrorContext(ctx, "m", slog.Any(catalog.KeyError, long))
	}
	if st := s2.Stats(); st.QueueFull != 1 || s2.bytes.Load() < 600<<10 {
		t.Fatalf("stats = %+v reserved %d, want the frozen text counted", st, s2.bytes.Load())
	}
	drain(s2)
	if s2.bytes.Load() != 0 {
		t.Fatalf("reserved %d after drain", s2.bytes.Load())
	}
}

// 09 req 63: a top-level attribute with a slot key fills its slot even
// when its value is a group, and is not written again as an attribute.
func TestProcessSlotGroupNotRepeated_Req63(t *testing.T) {
	s, out := newSink(t, Options{})
	s.Logger("c").InfoContext(t.Context(), "m", slog.Group(catalog.KeyError, slog.String("kind", "k")), slog.Int("n", 1))
	drain(s)
	line := out.lines(t)[0]
	if got := keys(t, line); !slices.Equal(got, []string{"time", "level", "msg", "component", "error", "n"}) {
		t.Fatalf("keys = %v in %s", got, line)
	}
}

// 09 req 61: the record is frozen when queued, so a caller reusing a
// header map or byte slice cannot change or race the queued record.
func TestProcessFreezesMutableValues_Req61(t *testing.T) {
	s, out := newSink(t, Options{})
	h := http.Header{"Accept": {"a"}}
	b := []byte("before")
	s.Logger("x").InfoContext(t.Context(), "m", slog.Any("h", h), slog.Any("b", b))
	h.Set("Accept", "changed")
	copy(b, "AFTER!")
	drain(s)
	m := object(t, out.lines(t)[0])
	if got := fmt.Sprint(m["h"]); got != "map[Accept:[a]]" {
		t.Errorf("h = %s", got)
	}
	if m["b"] != "YmVmb3Jl" { // base64 of "before", as encoding/json writes []byte
		t.Errorf("b = %v", m["b"])
	}
}

// The ReplaceAttr hook (internal/redact) runs for every non-group
// attribute with its groups, and for slot values without renaming them.
func TestProcessReplaceAttr(t *testing.T) {
	var seen []string
	replace := func(groups []string, a slog.Attr) slog.Attr {
		seen = append(seen, strings.Join(append(slices.Clone(groups), a.Key), "."))
		switch a.Key {
		case "drop":
			return slog.Attr{}
		case "token":
			return slog.String("token", "[MASKED]")
		case catalog.KeyCode:
			return slog.String("renamed", "RZ-RT-005")
		}
		return a
	}
	s, out := newSink(t, Options{ReplaceAttr: replace})
	h := s.Handler("c").WithGroup("g")
	logger := slog.New(h)
	logger.InfoContext(t.Context(), "m", slog.String("token", "abc"), slog.String("drop", "x"), slog.Group("in", slog.Int("n", 1)))
	slog.New(s.Handler("c")).InfoContext(t.Context(), "m", slog.String(catalog.KeyCode, "RZ-RT-001"))
	drain(s)
	lines := out.lines(t)
	if !strings.HasSuffix(lines[0], `"g":{"token":"[MASKED]","in":{"n":1}}}`) {
		t.Errorf("line = %s", lines[0])
	}
	if m := object(t, lines[1]); m["code"] != "RZ-RT-005" {
		t.Errorf("slot = %s", lines[1])
	}
	for _, want := range []string{"g.token", "g.drop", "g.in.n", "code"} {
		if !slices.Contains(seen, want) {
			t.Errorf("ReplaceAttr not called for %s (saw %v)", want, seen)
		}
	}
}

type nilError struct{}

func (*nilError) Error() string { panic("nil receiver") }

type marshalFail struct{}

func (marshalFail) MarshalJSON() ([]byte, error) { return nil, errors.New("no") }

// Values encode like slog.JSONHandler, with fixed time formatting and
// strings for non-finite floats; encoding never fails a record.
func TestProcessValueKinds(t *testing.T) {
	s, out := newSink(t, Options{})
	var ne *nilError
	at := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
	s.Logger("").InfoContext(t.Context(), "kinds",
		slog.Int64("i", -3), slog.Uint64("u", math.MaxUint64), slog.Float64("f", 1.5),
		slog.Float64("small", 1e-7), slog.Float64("big", 1e21), slog.Float64("nan", math.NaN()),
		slog.Float64("inf", math.Inf(1)), slog.Float64("ninf", math.Inf(-1)),
		slog.Bool("b", true), slog.Duration("d", 1500*time.Millisecond), slog.Time("t", at),
		slog.Any("nil", nil), slog.Any("nilerr", error(ne)), slog.Any("fail", marshalFail{}),
		slog.Any("slice", []int{1, 2}), slog.String("esc", "a\"\\\n\r\t\x01\u2028<>&\xff"),
	)
	drain(s)
	line := out.lines(t)[0]
	for _, want := range []string{
		`"i":-3`, `"u":18446744073709551615`, `"f":1.5`, `"small":1e-7`, `"big":1e+21`,
		`"nan":"NaN"`, `"inf":"+Inf"`, `"ninf":"-Inf"`, `"b":true`, `"d":1500000000`,
		`"t":"2026-01-02T03:04:05.000006Z"`, `"nil":null`, `"nilerr":"<nil>"`,
		`"fail":"!ERROR:json: error calling MarshalJSON`,
		`"slice":[1,2]`, `"esc":"a\"\\\n\r\t\u0001\u2028<>&\ufffd"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %s in %s", want, line)
		}
	}
	if !json.Valid([]byte(line)) {
		t.Fatalf("invalid JSON: %s", line)
	}
}

// 09 req 64, test plan 6.1 item 16: at info, debug records cost nothing.
func TestProcessDisabledZeroAllocs_Req64(t *testing.T) {
	s, _ := newSink(t, Options{Level: slog.LevelInfo})
	log := s.Logger("gateway")
	ctx := t.Context()
	allocs := testing.AllocsPerRun(1000, func() {
		log.DebugContext(ctx, "request")
		log.LogAttrs(ctx, slog.LevelDebug, "request", slog.String("route", "orders"), slog.Int("status", 200))
	})
	if allocs != 0 {
		t.Fatalf("disabled records allocate %v times, want 0", allocs)
	}
	if st := s.Stats(); st.Produced != 0 {
		t.Fatalf("disabled records counted as produced: %+v", st)
	}
}

// 09 req 61, WP-11 done-when: no allocation per record at steady state,
// for queueing on the caller and for encoding on the worker.
func TestProcessSteadyStateZeroAllocs_Req61(t *testing.T) {
	skipUnderRace(t)
	s, out := newSink(t, Options{NodeID: "node", TraceContext: traceFromContext})
	s.SetRevision("rev-0123456789ab")
	log := s.Logger("gateway")
	ctx := withTrace(t.Context(), testIDs())
	logOne := func() {
		log.LogAttrs(ctx, slog.LevelWarn, "upstream degraded",
			slog.String(catalog.KeyUpstream, "orders"), slog.String(catalog.KeyCode, "RZ-UP-001"),
			slog.Int("attempt", 2), slog.Bool("retry", true))
	}
	for range 100 {
		logOne()
		drain(s)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		logOne()
		drain(s)
	}); allocs != 0 {
		t.Fatalf("log and encode allocate %v times per record, want 0", allocs)
	}
	if out.Writes() == 0 {
		t.Fatal("nothing written")
	}
}
