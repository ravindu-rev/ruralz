// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the credential headers (authorization, proxy-authorization,
// cookie, response set-cookie, every auth.api-key config.header of the
// active Revision, headers set by upstream-auth Filters; /tap replaces
// their values and drops query strings and bodies), the names /tap also
// redacts (matching (?i)(token|secret|key|password|session|auth)), the
// [REDACTED] marker and observability O4 (no credential reaches a log).

const canary = "canary-0f1e2d3c4b5a69788796a5b4c3d2e1f0"

func strPtr(s string) *string { return &s }

// revisionHeaders plays the snapshot compiler: the config.header of every
// auth.api-key Policy of a Revision (the default x-api-key materialized)
// plus the headers upstream-auth Filters set.
func revisionHeaders() []string {
	policies := []v1alpha1.AuthAPIKeyConfig{
		{Header: strPtr("x-api-key")},
		{Header: strPtr("X-Partner-Credential")},
		{Header: strPtr("x-client-id")},
		{Header: strPtr("X-Client-ID")}, // same header, other spelling
	}
	var names []string
	for _, p := range policies {
		names = append(names, *p.Header)
	}
	return append(names, "Authorization", "x-upstream-signature")
}

func TestSetCoversEveryCredentialHeader(t *testing.T) {
	set := New(revisionHeaders()...)
	want := []string{"authorization", "cookie", "proxy-authorization", "set-cookie", "x-api-key", "x-client-id", "x-partner-credential", "x-upstream-signature"}
	if got := set.Names(); !slices.Equal(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
	for _, n := range want {
		for _, spelling := range []string{n, strings.ToUpper(n), http.CanonicalHeaderKey(n), mixed(n)} {
			if !set.Credential(spelling) || !set.Redacts(spelling) {
				t.Errorf("%q is not a credential header", spelling)
			}
		}
	}
	for _, n := range []string{"content-type", "x-request-id", "accept", "x-client", "x-api-keys", ""} {
		if set.Credential(n) {
			t.Errorf("%q is a credential header", n)
		}
	}

	// The /tap view of a request carrying every credential header of the
	// Revision holds none of their values.
	h := http.Header{}
	for i, n := range want {
		h.Add(n, fmt.Sprintf("%s-%d", canary, i))
		h.Add(n, canary)
	}
	h.Set("Content-Type", "application/json")
	view := set.Headers(h)
	for _, n := range want {
		vs, ok := view[n]
		if !ok || len(vs) != 2 || vs[0] != Marker || vs[1] != Marker {
			t.Errorf("view[%q] = %v, want two %s", n, vs, Marker)
		}
	}
	if got := view["content-type"]; len(got) != 1 || got[0] != "application/json" {
		t.Errorf("content-type = %v", got)
	}
	if s := fmt.Sprint(view); strings.Contains(s, canary) {
		t.Fatalf("the /tap view leaks the canary: %s", s)
	}
}

// mixed alternates the case of the letters of s.
func mixed(s string) string {
	b := []byte(s)
	for i := range b {
		if i%2 == 0 && b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

func TestFixedHeadersAndNilSet(t *testing.T) {
	var nilSet *Set
	for _, s := range []*Set{nilSet, Default(), New()} {
		if got := s.Names(); !slices.Equal(got, []string{"authorization", "cookie", "proxy-authorization", "set-cookie"}) {
			t.Fatalf("Names = %v", got)
		}
		for _, n := range []string{"Authorization", "PROXY-AUTHORIZATION", "cookie", "Set-Cookie"} {
			if !s.Credential(n) {
				t.Errorf("%T %q is not a credential header", s, n)
			}
		}
		if s.Credential("x-api-key") {
			t.Error("x-api-key is a credential header without a Revision naming it")
		}
		if !s.Redacts("x-api-key") {
			t.Error("x-api-key is not redacted by name")
		}
	}
	if got := FixedHeaders(); len(got) != 4 {
		t.Fatalf("FixedHeaders = %v", got)
	}
	// Names returns a copy.
	s := New("x-a")
	n := s.Names()
	n[0] = "mutated"
	if s.Names()[0] == "mutated" {
		t.Fatal("Names exposes the set")
	}
	// Blank names are ignored.
	if got := New("", "  ", " X-Key-Header ").Names(); !slices.Contains(got, "x-key-header") || len(got) != 5 {
		t.Fatalf("Names = %v", got)
	}
}

var sensitiveRE = regexp.MustCompile(`(?i)(token|secret|key|password|session|auth)`)

func TestSensitive(t *testing.T) {
	tests := map[string]bool{
		"x-auth-token":        true,
		"X-Session-Id":        true,
		"x-client-secret":     true,
		"X-API-KEY":           true,
		"sec-websocket-key":   true,
		"www-authenticate":    true,
		"Proxy-Authenticate":  true,
		"x-db-password":       true,
		"X-Csrf-Token":        true,
		"content-type":        false,
		"cookie":              false, // a credential header, not a sensitive name
		"keep-alive":          false,
		"x-request-id":        false,
		"traceparent":         false,
		"x-forwarded-for":     false,
		"":                    false,
		"ke":                  false,
		"tokenizer-version":   true,
		"x-autho":             true,
		"X-Secre":             false,
		"x-sessio":            false,
		"passwor":             false,
		"accept-encoding-key": true,
	}
	for name, want := range tests {
		if got := Sensitive(name); got != want {
			t.Errorf("Sensitive(%q) = %v, want %v", name, got, want)
		}
		if got := sensitiveRE.MatchString(name); got != want {
			t.Errorf("the oracle disagrees on %q", name)
		}
	}
	h := http.Header{"X-Auth-Token": {canary}, "X-Trace": {"t1"}}
	view := New().Headers(h)
	if view["x-auth-token"][0] != Marker || view["x-trace"][0] != "t1" {
		t.Fatalf("view = %v", view)
	}
}

func TestValue(t *testing.T) {
	s := New("x-partner")
	if s.Value("X-Partner", canary) != Marker || s.Value("Authorization", canary) != Marker || s.Value("x-secret-hint", canary) != Marker {
		t.Fatal("a credential value was returned")
	}
	if s.Value("Accept", "text/plain") != "text/plain" {
		t.Fatal("a plain value was redacted")
	}
	// One marker everywhere.
	if got := s.Value("Cookie", "sid=1"); got != "[REDACTED]" || Marker != secret.Redacted {
		t.Fatalf("marker = %q", got)
	}
}

func TestHeadersEdgeCases(t *testing.T) {
	s := New()
	if s.Headers(nil) != nil || s.Headers(http.Header{}) != nil {
		t.Fatal("an empty header gives a non-nil view")
	}
	// The view never aliases the input.
	h := http.Header{"Accept": {"a", "b"}}
	view := s.Headers(h)
	view["accept"][0] = "changed"
	if h.Get("Accept") != "a" {
		t.Fatal("the view aliases the input")
	}
	// A header without values stays, empty.
	if v, ok := s.Headers(http.Header{"X-Empty": {}})["x-empty"]; !ok || len(v) != 0 {
		t.Fatalf("x-empty = %v, %v", v, ok)
	}
	// Two spellings of one name merge deterministically, credentials
	// redacted whatever the spelling.
	hand := http.Header{"x-api-key": {canary}, "X-Api-Key": {canary}, "accept": {"1"}, "Accept": {"2"}, "ACCEPT": {"3"}}
	for range 20 {
		view := s.Headers(hand)
		if got := view["accept"]; !slices.Equal(got, []string{"3", "2", "1"}) {
			t.Fatalf("accept = %v", got)
		}
		if got := view["x-api-key"]; !slices.Equal(got, []string{Marker, Marker}) {
			t.Fatalf("x-api-key = %v", got)
		}
	}
}

func TestPathDropsQuery(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/orders?api_key=" + canary, "/orders"},
		{"/orders#frag", "/orders"},
		{"/orders", "/orders"},
		{"/a?b#c", "/a"},
		{"?only", ""},
		{"", ""},
		{"/search%3Fq=1?q=" + canary, "/search%3Fq=1"},
	}
	for _, tt := range tests {
		if got := Path(tt.in); got != tt.want {
			t.Errorf("Path(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"rediss://:" + canary + "@redis.internal:6380/0", "rediss://redis.internal:6380/0"},
		{"redis://user:" + canary + "@10.0.0.5:6379", "redis://10.0.0.5:6379"},
		{"https://idp.example/jwks.json?client_secret=" + canary, "https://idp.example/jwks.json"},
		{"https://idp.example/token#" + canary, "https://idp.example/token"},
		{"https://idp.example/token?", "https://idp.example/token"},
		{"http://[::1]:8080/p", "http://[::1]:8080/p"},
		{"//user:" + canary + "@host/path", "//host/path"},
		{"user:" + canary + "@host", Marker},
		{"http://h/%zz" + canary, Marker},
		{"localhost:6379", Marker},
		{"", ""},
		// An unencoded '/', '?' or '#' in user information moves part of
		// it into the host, path, query or fragment: fail closed (O4).
		{"rediss://:1234/x@host:6380", Marker},
		{"redis://user:1234#secret@host:6379", Marker},
		{"redis://user:1234?secret@host:6379", Marker},
		{"rediss://:" + canary + "/b64+pad==@redis.internal:6380/0", Marker},
		{"redis://u:p@1:2/ss@host:6379", Marker},
		{"redis://u:p@1:2#ss@host", Marker},
		{"//u:pw/x@host/path", Marker},
		{"/p@" + canary, Marker},
		{"http:/x//u:" + canary + "@h", Marker},
		// A '@' outside user information also fails closed.
		{"https://idp.example/users/a@b", Marker},
		{"https://idp.example/jwks?email=a@b", Marker},
		// Several '@' inside the authority are user information.
		{"redis://u@x:" + canary + "@host:6379/1", "redis://host:6379/1"},
		// User information without a host.
		{"//@//0", Marker},
		{"redis://:" + canary + "@/0", Marker},
	}
	for _, tt := range tests {
		if got := URL(tt.in); got != tt.want {
			t.Errorf("URL(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if tt.want == Marker || !strings.Contains(tt.in, "@") {
			continue
		}
		if strings.Contains(URL(tt.in), canary) {
			t.Errorf("URL(%q) leaks the canary", tt.in)
		}
	}
}

// TestURLKeepsPathEscapes: a path holding a byte that needs escaping, such
// as a space or a non-ASCII byte, makes net/url escape the decoded path
// again, which writes '@', '/' and ':' unescaped where the input escaped
// them. URL keeps the input's escapes in every URL shape, so the result
// holds no '@' and every segment stays as written, while user information,
// the query and the fragment are still dropped and an opaque URL still
// gives Marker.
func TestURLKeepsPathEscapes(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"empty user information without scheme", "//@0/%40 ", "//0/%40%20"},
		{"empty user information with scheme", "rediss://@redis.internal:6380/%40 ", "rediss://redis.internal:6380/%40%20"},
		{"user information without scheme", "//u:" + canary + "@host/a%40b c", "//host/a%40b%20c"},
		{"user information with scheme", "rediss://:" + canary + "@redis.internal:6380/0%40\xc3\xa9", "rediss://redis.internal:6380/0%40%C3%A9"},
		{"escaped '@' in user information", "redis://u%40x:" + canary + "%40@host/%40 ", "redis://host/%40%20"},
		{"no user information", "https://idp.example/users/a%40b c", "https://idp.example/users/a%40b%20c"},
		{"no authority", "/a%40b c", "/a%40b%20c"},
		{"relative path", "a%40b%3Ac d", "a%40b%3Ac%20d"},
		{"escaped slash", "https://idp.example/a%2Fb c", "https://idp.example/a%2Fb%20c"},
		{"escapes that would read as an authority", "http:/%2F%40host/x y", "http:/%2F%40host/x%20y"},
		{"sub-delimiters", "https://idp.example/%40!$&'()*+,;=: x", "https://idp.example/%40!$&'()*+,;=:%20x"},
		{"bytes a path cannot hold unescaped", "https://idp.example/%40\"<>\\^`{|}", "https://idp.example/%40%22%3C%3E%5C%5E%60%7B%7C%7D"},
		{"query", "https://idp.example/p%40 ?q=%40 ", "https://idp.example/p%40%20"},
		{"fragment", "https://idp.example/p%40 #%40 ", "https://idp.example/p%40%20"},
		{"opaque", "mailto:a%40b c", Marker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := URL(tt.in); got != tt.want {
				t.Errorf("URL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestEscapePathMatchesNetURL checks escapePath byte by byte against
// net/url: a byte stays exactly when net/url accepts it unescaped in an
// escaped path and it is not '@', any other byte becomes its uppercase
// escape, and EscapedPath keeps the result, so URL.String writes it as is.
func TestEscapePathMatchesNetURL(t *testing.T) {
	for c := range 256 {
		b := byte(c)
		if b == '%' {
			// A '%' in a parsed path always starts an escape.
			continue
		}
		p := string([]byte{'/', b})
		want := p
		// EscapedPath returns RawPath only when net/url accepts it.
		if (&url.URL{Path: p, RawPath: p}).EscapedPath() != p || b == '@' {
			want = fmt.Sprintf("/%%%02X", b)
		}
		got := escapePath(p)
		if got != want {
			t.Errorf("escapePath(%q) = %q, want %q", p, got, want)
		}
		if kept := (&url.URL{Path: p, RawPath: got}).EscapedPath(); kept != got {
			t.Errorf("EscapedPath discards escapePath(%q) = %q for %q", p, got, kept)
		}
	}
	// The escapes the path holds stay as written, in either case.
	if got, want := escapePath("/a%2fb%40c d"), "/a%2fb%40c%20d"; got != want {
		t.Errorf("escapePath(%q) = %q, want %q", "/a%2fb%40c d", got, want)
	}
}

// headerAttr builds an attribute keyed by a header name, the keys a
// header-map group holds (deliberately not snake_case log keys).
func headerAttr(name string, v any) slog.Attr { return slog.Attr{Key: name, Value: slog.AnyValue(v)} }

// replaceRecord logs one record through handler with set.ReplaceAttr.
func replaceRecord(t *testing.T, set *Set, text bool, attrs ...any) string {
	t.Helper()
	var buf bytes.Buffer
	o := &slog.HandlerOptions{ReplaceAttr: set.ReplaceAttr}
	var h slog.Handler = slog.NewJSONHandler(&buf, o)
	if text {
		h = slog.NewTextHandler(&buf, o)
	}
	slog.New(h).Info("record", attrs...)
	return buf.String()
}

// TestReplaceAttrO4 covers the slog ReplaceAttr hook and observability O4
// with the JSON and text handlers.
func TestReplaceAttrO4(t *testing.T) {
	set := New("x-partner-credential")
	h := http.Header{"Authorization": {"Bearer " + canary}, "X-Session": {canary}, "Accept": {"text/plain"}}
	tests := []struct {
		name  string
		attrs []any
		want  []string // JSON fragments
	}{
		{
			name:  "credential header keys at any depth and spelling",
			attrs: []any{"authorization", canary, "Proxy_Authorization", canary, slog.Group("upstream", "set_cookie", canary, "x_partner_credential", canary)},
			want:  []string{`"authorization":"[REDACTED]"`, `"Proxy_Authorization":"[REDACTED]"`, `"set_cookie":"[REDACTED]"`, `"x_partner_credential":"[REDACTED]"`},
		},
		{
			name:  "sensitive names inside a header group",
			attrs: []any{slog.Group("request_headers", headerAttr("x-auth-token", canary), headerAttr("x-request-id", "r1")), slog.Group("upstream_headers", headerAttr("x-db-password", canary))},
			want:  []string{`"x-auth-token":"[REDACTED]"`, `"x-request-id":"r1"`, `"x-db-password":"[REDACTED]"`},
		},
		{
			name:  "flat OpenTelemetry header keys",
			attrs: []any{"http.request.header.x-api-key", canary, "http.response.header.set-cookie", canary, "http.request.header.accept", "a/b"},
			want:  []string{`"http.request.header.x-api-key":"[REDACTED]"`, `"http.response.header.set-cookie":"[REDACTED]"`, `"http.request.header.accept":"a/b"`},
		},
		{
			name:  "header maps as values",
			attrs: []any{"headers", h, "view", map[string][]string{"Cookie": {canary}, "Accept": {"x"}}},
			want:  []string{`"headers":{"accept":["text/plain"],"authorization":["[REDACTED]"],"x-session":["[REDACTED]"]}`, `"view":{"accept":["x"],"cookie":["[REDACTED]"]}`},
		},
		{
			name:  "sensitive words outside header maps pass",
			attrs: []any{"auth_method", "jwt", "input_tokens", 12, "session_count", 3, "header", "x", "keyed", true},
			want:  []string{`"auth_method":"jwt"`, `"input_tokens":12`, `"session_count":3`, `"header":"x"`, `"keyed":true`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := replaceRecord(t, set, false, tt.attrs...)
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("JSON lacks %s:\n%s", w, out)
				}
			}
			for _, text := range []bool{false, true} {
				if out := replaceRecord(t, set, text, tt.attrs...); strings.Contains(out, canary) {
					t.Fatalf("text=%v leaks the canary:\n%s", text, out)
				}
			}
		})
	}
	// A nil set uses the fixed headers.
	var nilSet *Set
	if out := replaceRecord(t, nilSet, true, "proxy_authorization", canary, "COOKIE", canary, "x_other", "v"); strings.Contains(out, canary) || !strings.Contains(out, "x_other=v") {
		t.Fatalf("nil set:\n%s", out)
	}
	// A key longer than any name and a key ending in "header." pass.
	long := strings.Repeat("a_", 100)
	if a := set.ReplaceAttr(nil, slog.String(long, "v")); a.Value.String() != "v" {
		t.Fatalf("long key = %v", a)
	}
	if a := set.ReplaceAttr(nil, headerAttr("http.request.header.", "v")); a.Value.String() != "v" {
		t.Fatalf("empty flat header = %v", a)
	}
	// Header groups are recognized in any case.
	if a := set.ReplaceAttr([]string{"upstream", "Request-HEADERS"}, headerAttr("X-Session-Id", canary)); a.Value.String() != Marker {
		t.Fatalf("mixed-case header group = %v", a)
	}
	// A []string value keeps its shape.
	if a := set.ReplaceAttr([]string{"headers"}, headerAttr("x-api-key", []string{canary, canary})); fmt.Sprint(a.Value.Any()) != "[[REDACTED] [REDACTED]]" {
		t.Fatalf("[]string value = %v", a.Value)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_ = set.ReplaceAttr([]string{"upstream"}, slog.String("proxy_authorization", "x"))
		_ = set.ReplaceAttr([]string{"headers"}, headerAttr("x-request-id", "x"))
	}); allocs != 0 {
		t.Fatalf("ReplaceAttr allocates %v times", allocs)
	}
}

func TestSlogHelpersO4(t *testing.T) {
	set := New("x-partner-credential")
	h := http.Header{
		"Authorization":        {"Bearer " + canary},
		"Cookie":               {"sid=" + canary},
		"X-Partner-Credential": {canary},
		"X-Session":            {canary},
		"Accept":               {"application/json"},
	}
	var buf bytes.Buffer
	for _, handler := range []slog.Handler{slog.NewJSONHandler(&buf, nil), slog.NewTextHandler(&buf, nil)} {
		log := slog.New(handler)
		log.Info("upstream request", set.HeaderAttr("headers", h))
		log.Info("upstream request", slog.Any("headers", set.LogHeaders(h)))
	}
	out := buf.String()
	if strings.Contains(out, canary) {
		t.Fatalf("a log record leaks the canary:\n%s", out)
	}
	for _, want := range []string{`"authorization":["[REDACTED]"]`, `"accept":["application/json"]`, `headers.cookie=[[REDACTED]]`} {
		if !strings.Contains(out, want) {
			t.Errorf("log output lacks %s:\n%s", want, out)
		}
	}
	// The group lists names in byte order.
	g := set.LogHeaders(h).LogValue().Group()
	var names []string
	for _, a := range g {
		names = append(names, a.Key)
	}
	if !slices.IsSorted(names) || len(names) != len(h) {
		t.Fatalf("group names = %v", names)
	}
}

func TestSetIsConcurrencySafe(t *testing.T) {
	set := New(revisionHeaders()...)
	h := http.Header{"X-Api-Key": {canary}, "Accept": {"x"}}
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 1000 {
				if set.Headers(h)["x-api-key"][0] != Marker || !set.Credential("x-client-id") {
					t.Error("redaction failed")
					return
				}
			}
		}()
	}
	for range 8 {
		<-done
	}
}

func TestNoAllocationsOnNameChecks(t *testing.T) {
	set := New(revisionHeaders()...)
	allocs := testing.AllocsPerRun(100, func() {
		_ = set.Redacts("X-Client-Id")
		_ = set.Redacts("x-CLIENT-id")
		_ = set.Redacts("Content-Type")
		_ = Sensitive("X-Request-Id")
		_ = set.Value("Accept", "x")
	})
	if allocs != 0 {
		t.Fatalf("name checks allocate %v times", allocs)
	}
}

// FuzzURL: URL never panics, never returns user information, a query, a
// fragment or a '@', and never returns any part of the user information of
// its input: when the input holds '@', the result is Marker or carries a
// host parsed from after the input's last '@'. The path of the result is
// the input's, changed only by escaping bytes, never by decoding an escape.
func FuzzURL(f *testing.F) {
	for _, s := range []string{
		"rediss://:pw@h:6380/0", "https://h/p?q=1#f", "user:pw@h", "//u:p@h", "%zz", "http://[::1]/",
		"rediss://:1234/x@host:6380", "redis://user:1234#secret@host:6379", "redis://u:p@1:2/ss@host:6379", "//@//0",
		"https://h/a%2Fb c", "http:/%2F%40h/x y",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got := URL(raw)
		if got == Marker {
			return
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("URL(%q) = %q does not parse: %v", raw, got, err)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("URL(%q) = %q keeps user information, query or fragment", raw, got)
		}
		if strings.Contains(got, "@") {
			t.Fatalf("URL(%q) = %q keeps a '@'", raw, got)
		}
		in, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("URL(%q) = %q for an input that does not parse: %v", raw, got, err)
		}
		if p, inPath := writtenPath(u), writtenPath(in); !escapesOnly(inPath, p) {
			t.Fatalf("URL(%q) = %q: path %q is not %q with bytes escaped", raw, got, p, inPath)
		}
		at := strings.LastIndexByte(raw, '@')
		if at < 0 {
			return
		}
		// The host must come from after the last '@': no byte between
		// "//" and that '@' may become the host, path, query or fragment.
		hostPart := raw[at+1:]
		if i := strings.IndexAny(hostPart, "/?#"); i >= 0 {
			hostPart = hostPart[:i]
		}
		want, err := url.Parse("//" + hostPart)
		if err != nil || want.Host != u.Host {
			t.Fatalf("URL(%q) = %q: host %q, want the host after the last '@' %q (%v)", raw, got, u.Host, hostPart, err)
		}
		if in.User != nil {
			if !strings.HasPrefix(raw[at+1:], hostPart) || strings.Contains(raw[at+1:], "@") {
				t.Fatalf("URL(%q): '@' after the authority", raw)
			}
		}
	})
}

// writtenPath returns the path of u as the string u was parsed from wrote
// it: RawPath, or the default escaping of Path when RawPath is empty.
func writtenPath(u *url.URL) string {
	if u.RawPath != "" {
		return u.RawPath
	}
	return u.EscapedPath()
}

// escapesOnly reports whether out is in with some bytes other than '%'
// replaced by their escapes, in either case of hexadecimal digit.
func escapesOnly(in, out string) bool {
	for i := range len(in) {
		c := in[i]
		switch {
		case out != "" && out[0] == c:
			out = out[1:]
		case c != '%' && len(out) >= 3 && out[0] == '%' && strings.EqualFold(out[1:3], fmt.Sprintf("%02X", c)):
			out = out[3:]
		default:
			return false
		}
	}
	return out == ""
}

// FuzzSensitive checks Sensitive against the regular expression sensitiveRE.
func FuzzSensitive(f *testing.F) {
	for _, s := range []string{"x-auth-token", "Keep-Alive", "SESSION", "passwor"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if got, want := Sensitive(name), sensitiveRE.MatchString(name); got != want {
			// The regexp folds Unicode (the Kelvin sign K matches k); header
			// names are ASCII tokens, so only ASCII inputs must agree.
			for i := 0; i < len(name); i++ {
				if name[i] >= 0x80 {
					return
				}
			}
			t.Fatalf("Sensitive(%q) = %v, regexp %v", name, got, want)
		}
	})
}

func BenchmarkRedacts(b *testing.B) {
	set := New(revisionHeaders()...)
	names := []string{"Content-Type", "Authorization", "X-Client-Id", "x-request-id", "Traceparent", "X-Api-Key"}
	b.ReportAllocs()
	for b.Loop() {
		for _, n := range names {
			_ = set.Redacts(n)
		}
	}
}

func BenchmarkTapHeaders(b *testing.B) {
	set := New(revisionHeaders()...)
	h := http.Header{
		"Accept": {"application/json"}, "Authorization": {"Bearer x"}, "Content-Type": {"application/json"},
		"User-Agent": {"bench"}, "X-Api-Key": {"k"}, "X-Request-Id": {"r"}, "Traceparent": {"00-1-2-01"},
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = set.Headers(h)
	}
}
