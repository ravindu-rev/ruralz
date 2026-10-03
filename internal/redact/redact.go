// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package redact holds the credential-header set of a snapshot and the
// helpers that keep credentials out of /tap and process logs (spec 06
// requirement 92, spec 04 requirement 75, observability O4). The set is
// authorization, proxy-authorization, cookie, response set-cookie, every
// auth.api-key Policy's config.header in the Revision, and the headers
// upstream-auth Filters set; the snapshot compiler builds one Set per
// Revision with New. It is the one list the M2 Plugin host reuses for the
// credentials.read Capability.
//
// /tap and logs also redact every header whose name contains token,
// secret, key, password, session or auth (case-insensitive). Redacted
// values are replaced by secret.Redacted ("[REDACTED]", R-9); query
// strings, URL user information and bodies are dropped, never redacted in
// place. Set.ReplaceAttr is the slog hook the process logger installs
// (spec 06 section 4); Headers, LogHeaders and HeaderAttr build the /tap
// and log views of a header map; Path and URL strip request targets and
// configured URLs.
package redact

import (
	"log/slog"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/secret"
)

// Marker replaces every redacted value.
const Marker = secret.Redacted

// The fixed credential headers (spec 06 requirement 92; Plugin system
// "Credentials", OQ-wasm-plugin-system-3 (b)).
const (
	// Authorization carries client credentials.
	Authorization = "authorization"
	// ProxyAuthorization carries proxy credentials.
	ProxyAuthorization = "proxy-authorization"
	// Cookie carries session cookies.
	Cookie = "cookie"
	// SetCookie is the response header that sets them.
	SetCookie = "set-cookie"
)

// FixedHeaders returns the credential headers every Revision has, in
// lowercase.
func FixedHeaders() []string {
	return []string{Authorization, Cookie, ProxyAuthorization, SetCookie}
}

// Set is the credential-header set of one snapshot. It is immutable and
// safe for concurrent use; a nil *Set holds the fixed headers only.
type Set struct {
	// names is the sorted, deduplicated lowercase set.
	names []string
	// index holds each name in lowercase and canonical form, so the
	// names net/http delivers hit without folding.
	index map[string]struct{}
}

// New returns the set of the fixed headers plus extra: every
// auth.api-key config.header of the Revision and every header an
// upstream-auth Filter sets. Names match case-insensitively; empty names
// are ignored and duplicates collapse.
func New(extra ...string) *Set {
	names := FixedHeaders()
	for _, n := range extra {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, strings.ToLower(n))
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	s := &Set{names: names, index: make(map[string]struct{}, 2*len(names))}
	for _, n := range names {
		s.index[n] = struct{}{}
		s.index[textproto.CanonicalMIMEHeaderKey(n)] = struct{}{}
	}
	return s
}

// Default returns the set of the fixed headers, for use before the first
// Revision.
func Default() *Set { return New() }

// Names returns the lowercase names of the set in byte order.
func (s *Set) Names() []string {
	if s == nil {
		names := FixedHeaders()
		slices.Sort(names)
		return names
	}
	return slices.Clone(s.names)
}

// Credential reports whether name is a credential header of the set,
// ignoring case. It does not allocate.
func (s *Set) Credential(name string) bool {
	if s == nil {
		return fixed(name)
	}
	if _, ok := s.index[name]; ok {
		return true
	}
	for _, n := range s.names {
		if len(n) == len(name) && strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// fixed reports whether name is one of the fixed credential headers.
func fixed(name string) bool {
	return strings.EqualFold(name, Authorization) || strings.EqualFold(name, ProxyAuthorization) ||
		strings.EqualFold(name, Cookie) || strings.EqualFold(name, SetCookie)
}

// Sensitive reports whether name contains token, secret, key, password,
// session or auth in any case. It does not allocate.
func Sensitive(name string) bool {
	// Spec 04 requirement 75: (?i)(token|secret|key|password|session|auth).
	for _, w := range [...]string{"token", "secret", "key", "password", "session", "auth"} {
		if containsFold(name, w) {
			return true
		}
	}
	return false
}

// containsFold reports whether s contains the lowercase ASCII word w,
// ignoring the case of s.
func containsFold(s, w string) bool {
	for i := 0; i+len(w) <= len(s); i++ {
		j := 0
		for ; j < len(w); j++ {
			c := s[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != w[j] {
				break
			}
		}
		if j == len(w) {
			return true
		}
	}
	return false
}

// Redacts reports whether /tap and logs replace the values of header name:
// a credential header of the set or a sensitive name.
func (s *Set) Redacts(name string) bool { return s.Credential(name) || Sensitive(name) }

// Value returns value, or Marker when the header name is redacted.
func (s *Set) Value(name, value string) string {
	if s.Redacts(name) {
		return Marker
	}
	return value
}

// Headers returns the /tap view of h (adminapi.TapEvent RequestHeaders and
// ResponseHeaders): names in lowercase, each value of a redacted header
// replaced by Marker, other values copied. It returns nil for an empty h
// and never retains h.
func (s *Set) Headers(h http.Header) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for name, vs := range h {
		lower := strings.ToLower(name)
		if _, dup := out[lower]; dup {
			// Two spellings of one name (only in a hand-built Header):
			// merge them in spelling order so the view is deterministic.
			return s.mergedHeaders(h)
		}
		out[lower] = s.values(name, vs)
	}
	return out
}

// mergedHeaders is Headers over the names of h in byte order.
func (s *Set) mergedHeaders(h http.Header) map[string][]string {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make(map[string][]string, len(h))
	for _, name := range names {
		lower := strings.ToLower(name)
		out[lower] = append(out[lower], s.values(name, h[name])...)
	}
	return out
}

// values copies vs, replacing each value by Marker when name is redacted.
func (s *Set) values(name string, vs []string) []string {
	dst := make([]string, len(vs))
	if s.Redacts(name) {
		for i := range dst {
			dst[i] = Marker
		}
	} else {
		copy(dst, vs)
	}
	return dst
}

// LogHeaders returns a slog.LogValuer for h: a group of the /tap view in
// name order, resolved only when a record is written. It keeps h, so the
// caller must not change h before the record is handled.
func (s *Set) LogHeaders(h http.Header) slog.LogValuer { return headerValuer{set: s, h: h} }

// HeaderAttr returns slog.Any(key, s.LogHeaders(h)).
func (s *Set) HeaderAttr(key string, h http.Header) slog.Attr {
	return slog.Any(key, s.LogHeaders(h))
}

// headerValuer is the LogValuer of LogHeaders.
type headerValuer struct {
	set *Set
	h   http.Header
}

// LogValue implements slog.LogValuer.
func (v headerValuer) LogValue() slog.Value {
	view := v.set.Headers(v.h)
	names := make([]string, 0, len(view))
	for n := range view {
		names = append(names, n)
	}
	slices.Sort(names)
	attrs := make([]slog.Attr, len(names))
	for i, n := range names {
		attrs[i] = slog.Any(n, view[n])
	}
	return slog.GroupValue(attrs...)
}

// Path returns a request target without its query string and fragment,
// the only form /tap and logs carry.
func Path(target string) string {
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		return target[:i]
	}
	return target
}

// URL returns raw without user information, query string and fragment,
// for logging a JWKS, token, Endpoint or State Store URL. It fails closed:
// a string that does not parse, an opaque URL, and any URL whose '@' does
// not end user information inside the authority (an unencoded '/', '?' or
// '#' in a password moves part of it into the host, path, query or
// fragment), as well as user information without a host, are returned as
// Marker, since they may hold a credential in an unknown place. A '@'
// anywhere in a URL without user information therefore also gives Marker.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		// scheme:opaque forms (such as "user:pass@host" without //)
		// cannot be split safely.
		return Marker
	}
	if at := strings.LastIndexByte(raw, '@'); at >= 0 && (u.User == nil || u.Host == "" || !inAuthority(raw, at)) {
		return Marker
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// inAuthority reports whether byte i of raw lies in its authority: after
// the first "//" and before the first '/', '?' or '#' that follows it, the
// split net/url makes. A scheme never contains '/', so for a URL that
// parsed with user information the first "//" starts the authority.
func inAuthority(raw string, i int) bool {
	start := strings.Index(raw, "//")
	if start < 0 || start+2 > i {
		return false
	}
	end := strings.IndexAny(raw[start+2:], "/?#")
	return end < 0 || start+2+end > i
}

// ReplaceAttr is a slog.HandlerOptions.ReplaceAttr hook (and the
// logsink.Options.ReplaceAttr redaction hook, spec 06 section 4; O4) for
// process logs. It replaces with Marker the value of
//
//   - an attribute named like a credential header of s, at any depth, in
//     any case and with '_' for '-' (a snake_case log key such as
//     proxy_authorization);
//   - an attribute redacted by name (Redacts) inside a group named like a
//     header map (a group whose name contains "header", such as headers or
//     request_headers), or whose key is an OpenTelemetry-style flat header
//     key ("http.request.header.<name>");
//
// and turns an http.Header or map[string][]string value into the
// redacted group LogHeaders writes. Other attributes pass unchanged: a
// sensitive word alone (auth_method, input_tokens, session_count) does not
// make a log key a credential outside a header map. Groups are never
// passed to the hook; their members are, with the group names.
func (s *Set) ReplaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindAny {
		switch h := a.Value.Any().(type) {
		case http.Header:
			return slog.Attr{Key: a.Key, Value: s.LogHeaders(h).LogValue()}
		case map[string][]string:
			return slog.Attr{Key: a.Key, Value: s.LogHeaders(h).LogValue()}
		}
	}
	if s.logCredential(a.Key) {
		return redacted(a)
	}
	if name, ok := headerKey(a.Key); ok && s.Redacts(name) {
		return redacted(a)
	}
	for _, g := range groups {
		if containsFold(g, "header") {
			if s.Redacts(a.Key) {
				return redacted(a)
			}
			break
		}
	}
	return a
}

// redacted returns a with its value replaced by Marker. A []string value
// (a header's values, as LogHeaders writes them) keeps its shape: one
// Marker per value.
func redacted(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindAny {
		if vs, ok := a.Value.Any().([]string); ok {
			if !slices.ContainsFunc(vs, func(v string) bool { return v != Marker }) {
				return a
			}
			out := make([]string, len(vs))
			for i := range out {
				out[i] = Marker
			}
			return slog.Any(a.Key, out)
		}
	}
	return slog.String(a.Key, Marker)
}

// logCredential reports whether a log key names a credential header of
// s, ignoring ASCII case and reading '_' as '-'. It does not allocate.
func (s *Set) logCredential(key string) bool {
	if s.Credential(key) {
		return true
	}
	if strings.IndexByte(key, '_') < 0 {
		return false
	}
	if s == nil {
		for _, n := range [...]string{Authorization, Cookie, ProxyAuthorization, SetCookie} {
			if dashFold(n, key) {
				return true
			}
		}
		return false
	}
	for _, n := range s.names {
		if dashFold(n, key) {
			return true
		}
	}
	return false
}

// dashFold reports whether key equals the lowercase name n, ignoring the
// ASCII case of key and reading '_' in key as '-'.
func dashFold(n, key string) bool {
	if len(n) != len(key) {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := key[i]
		switch {
		case c == '_':
			c = '-'
		case 'A' <= c && c <= 'Z':
			c += 'a' - 'A'
		}
		if c != n[i] {
			return false
		}
	}
	return true
}

// headerKey returns the header name of an OpenTelemetry-style flat
// attribute key (http.request.header.<name>, http.response.header.<name>):
// the text after the last "header.".
func headerKey(key string) (string, bool) {
	const marker = "header."
	if i := strings.LastIndex(key, marker); i >= 0 && i+len(marker) < len(key) {
		return key[i+len(marker):], true
	}
	return "", false
}
