// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Attribute keys the redaction helpers produce for a logged *http.Request.
const (
	keyMethod = "method"
	keyHost   = "host"
)

// builtinCredentialHeaders are the credential headers of Security and
// identity "Secrets" rule 2 and 06 req 92 that apply to every Revision:
// authorization, proxy-authorization, cookie, set-cookie, and the default
// auth.api-key header x-api-key (R-21). Each Revision adds its auth.api-key
// header names through Sink.SetCredentialHeaders.
func builtinCredentialHeaders() []string {
	return []string{"authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key"}
}

// sensitiveFragments mark a header name as sensitive in a logged header
// map, as the /tap redaction does (04 req 75).
func sensitiveFragments() []string {
	return []string{"token", "secret", "key", "password", "session", "auth"}
}

// credSet is an immutable set of credential header names, normalized to
// lower case with '-' spelled '_' so the header "X-Api-Key" and the
// attribute key "x_api_key" match the same entry.
type credSet struct{ names map[string]struct{} }

func newCredSet(extra []string) *credSet {
	c := &credSet{names: make(map[string]struct{})}
	for _, n := range slices.Concat(builtinCredentialHeaders(), extra) {
		if n == "" {
			continue
		}
		c.names[strings.Map(normalizeRune, n)] = struct{}{}
	}
	return c
}

func normalizeRune(r rune) rune {
	switch {
	case r == '-':
		return '_'
	case r >= 'A' && r <= 'Z':
		return r + ('a' - 'A')
	default:
		return r
	}
}

// name reports whether key names a credential header, ignoring case and
// the '-'/'_' spelling. It does not allocate for keys up to 128 bytes.
func (c *credSet) name(key string) bool {
	if c == nil || key == "" {
		return false
	}
	var buf [128]byte
	if len(key) > len(buf) {
		_, ok := c.names[strings.Map(normalizeRune, key)]
		return ok
	}
	for i := 0; i < len(key); i++ {
		b := key[i]
		switch {
		case b == '-':
			b = '_'
		case b >= 'A' && b <= 'Z':
			b += 'a' - 'A'
		}
		buf[i] = b
	}
	_, ok := c.names[string(buf[:len(key)])]
	return ok
}

// header reports whether a header of a logged header map is redacted: a
// credential header, or a name containing a sensitive fragment.
func (c *credSet) header(name string) bool {
	if c.name(name) {
		return true
	}
	lower := strings.ToLower(name)
	for _, f := range sensitiveFragments() {
		if strings.Contains(lower, f) {
			return true
		}
	}
	return false
}

// redactHeader returns a copy of h whose sensitive values are [REDACTED].
func redactHeader(h http.Header, c *credSet) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, vs := range h {
		if c.header(k) {
			out[k] = []string{secret.Redacted}
			continue
		}
		out[k] = slices.Clone(vs)
	}
	return out
}

// redactURL renders u without user information, query or fragment:
// query strings never reach a signal (09 req 4).
func redactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.User = nil
	c.RawQuery = ""
	c.ForceQuery = false
	c.Fragment = ""
	c.RawFragment = ""
	return c.String()
}

// redactURLString parses s as a URL and redacts it; an unparsable value
// is cut at the first '?' or '#' and loses anything before an '@'.
func redactURLString(s string) string {
	if u, err := url.Parse(s); err == nil {
		return redactURL(u)
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// hasURLError reports whether err's tree holds a *url.Error, without
// allocating.
func hasURLError(err error) bool {
	for err != nil {
		switch x := err.(type) { //nolint:errorlint // walks the tree by hand to stay allocation-free.
		case *url.Error:
			return true
		case interface{ Unwrap() []error }:
			for _, e := range x.Unwrap() {
				if hasURLError(e) {
					return true
				}
			}
			return false
		}
		err = errors.Unwrap(err)
	}
	return false
}

// redactErrorText returns err's text with the URL of every *url.Error in
// its tree replaced by its redacted form (09 req 4). A *url.Error prints
// its URL with %q, so a URL holding a quote, a backslash or a control byte
// appears escaped; see redactURLError. A panicking Error method gives
// "<nil>" for a nil receiver and a "!PANIC:" text otherwise, as
// slog.JSONHandler writes.
func redactErrorText(err error) (text string) {
	defer func() {
		if r := recover(); r != nil {
			if rv := reflect.ValueOf(err); rv.Kind() == reflect.Pointer && rv.IsNil() {
				text = "<nil>"
				return
			}
			text = fmt.Sprintf("!PANIC: %v", r)
		}
	}()
	text = err.Error()
	var walk func(error)
	walk = func(e error) {
		for e != nil {
			switch x := e.(type) { //nolint:errorlint // every node of the tree is visited.
			case *url.Error:
				if x == nil {
					return
				}
				if x.URL != "" {
					text = redactURLError(text, x)
				}
			case interface{ Unwrap() []error }:
				for _, c := range x.Unwrap() {
					walk(c)
				}
				return
			}
			e = errors.Unwrap(e)
		}
	}
	walk(err)
	return text
}

// redactURLError replaces, in text, every spelling of x's URL that a
// *url.Error or a wrapper prints: x's own segment, the %q form and the raw
// URL.
func redactURLError(text string, x *url.Error) string {
	red := redactURLString(x.URL)
	if red == x.URL {
		return text
	}
	clean := &url.Error{Op: x.Op, URL: red, Err: x.Err}
	text = strings.ReplaceAll(text, x.Error(), clean.Error())
	text = strings.ReplaceAll(text, strconv.Quote(x.URL), strconv.Quote(red))
	return strings.ReplaceAll(text, x.URL, red)
}

// mutable reports whether a value must be frozen before it is queued: a
// LogValuer (resolved on the calling goroutine, so secret.Value becomes
// [REDACTED] at once), a header map, request or URL (redacted), a byte
// slice (copied), a pointer to one of these, or an error carrying a URL.
func mutable(v slog.Value) bool {
	switch v.Kind() {
	case slog.KindLogValuer:
		return true
	case slog.KindGroup:
		for _, a := range v.Group() {
			if mutable(a.Value) {
				return true
			}
		}
		return false
	case slog.KindAny:
		switch x := v.Any().(type) {
		case http.Header, *http.Header, map[string][]string, *map[string][]string,
			*http.Request, url.URL, *url.URL, **url.URL, []byte, *[]byte:
			return true
		case error:
			return hasURLError(x)
		}
		return false
	default:
		return false
	}
}

// freeze returns an immutable, redacted form of v (see mutable). It runs
// on the logging goroutine, so the queue never holds a credential, a query
// string or memory the caller may reuse.
func freeze(v slog.Value, c *credSet) slog.Value {
	switch v.Kind() {
	case slog.KindLogValuer:
		return freeze(v.Resolve(), c)
	case slog.KindGroup:
		as := v.Group()
		out := make([]slog.Attr, len(as))
		for i, a := range as {
			out[i] = slog.Attr{Key: a.Key, Value: freeze(a.Value, c)}
		}
		return slog.GroupValue(out...)
	case slog.KindAny:
		return freezeAny(v, c)
	default:
		return v
	}
}

// freezeAny freezes a KindAny value; a nil pointer becomes JSON null.
func freezeAny(v slog.Value, c *credSet) slog.Value {
	switch x := v.Any().(type) {
	case http.Header:
		return slog.AnyValue(redactHeader(x, c))
	case *http.Header:
		if x == nil {
			return slog.AnyValue(nil)
		}
		return slog.AnyValue(redactHeader(*x, c))
	case map[string][]string:
		return slog.AnyValue(redactHeader(http.Header(x), c))
	case *map[string][]string:
		if x == nil {
			return slog.AnyValue(nil)
		}
		return slog.AnyValue(redactHeader(http.Header(*x), c))
	case *http.Request:
		if x == nil {
			return slog.StringValue("<nil>")
		}
		path := ""
		if x.URL != nil {
			path = x.URL.Path
		}
		return slog.GroupValue(
			slog.String(keyMethod, x.Method),
			slog.String(keyHost, x.Host),
			slog.String(catalog.KeyPath, path),
		)
	case url.URL:
		return slog.StringValue(redactURL(&x))
	case *url.URL:
		return slog.StringValue(redactURL(x))
	case **url.URL:
		if x == nil {
			return slog.AnyValue(nil)
		}
		return slog.StringValue(redactURL(*x))
	case []byte:
		return slog.AnyValue(bytes.Clone(x))
	case *[]byte:
		if x == nil {
			return slog.AnyValue(nil)
		}
		return slog.AnyValue(bytes.Clone(*x))
	case error:
		if hasURLError(x) {
			return slog.StringValue(redactErrorText(x))
		}
	}
	return v
}

// freezeRecord returns a copy of r safe to queue: a plain Clone when no
// attribute is mutable, otherwise a rebuilt record with frozen values.
func freezeRecord(r slog.Record, c *credSet, needed bool) slog.Record {
	if !needed {
		return r.Clone()
	}
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(slog.Attr{Key: a.Key, Value: freeze(a.Value, c)})
		return true
	})
	return out
}
