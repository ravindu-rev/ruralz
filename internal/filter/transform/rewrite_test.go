// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"net/http"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
)

// Tests for the response body rules other Policies share with the
// transforms (rewrite.go), as auth.jwt-sign uses them.

// TestBodyRewritable: no body is rewritten on a response that cannot have
// content (HEAD, 1xx, 204, 304), on a 206 or on one with Content-Range,
// under any casing of its key; any other response may be.
func TestBodyRewritable(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		method string
		header http.Header
		want   bool
	}{
		{"200", http.StatusOK, http.MethodGet, nil, true},
		{"500", http.StatusInternalServerError, http.MethodPost, nil, true},
		{"204", http.StatusNoContent, http.MethodGet, nil, false},
		{"304", http.StatusNotModified, http.MethodGet, nil, false},
		{"100", http.StatusContinue, http.MethodGet, nil, false},
		{"206", http.StatusPartialContent, http.MethodGet, nil, false},
		{"Content-Range", http.StatusOK, http.MethodGet, http.Header{"Content-Range": {"bytes 0-1/2"}}, false},
		{"content-range in lower case", http.StatusOK, http.MethodGet, http.Header{"content-range": {"bytes 0-1/2"}}, false},
		{"HEAD", http.StatusOK, http.MethodHead, nil, false},
	} {
		m := &filtertest.Message{Hdr: c.header, Code: c.status}
		x := &filtertest.Exchange{MethodV: c.method, Msg: m}
		if got := BodyRewritable(x, m); got != c.want {
			t.Errorf("%s: BodyRewritable = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestJSONContent: exactly one Content-Type field line naming JSON.
func TestJSONContent(t *testing.T) {
	for _, c := range []struct {
		lines []string
		want  bool
	}{
		{[]string{"application/json"}, true},
		{[]string{"application/problem+json; charset=utf-8"}, true},
		{[]string{"text/plain"}, false},
		{[]string{"application/json", "application/json"}, false},
		{nil, false},
	} {
		h := http.Header{}
		if c.lines != nil {
			h["Content-Type"] = c.lines
		}
		if got := JSONContent(h); got != c.want {
			t.Errorf("JSONContent(%q) = %v, want %v", c.lines, got, c.want)
		}
	}
}

// TestMarkRewritten: strong ETags turn weak, weak ones stay, Accept-Ranges
// goes, lines under differently cased keys fold into the canonical key
// first, and a created body takes Content-Type application/json in place
// of any other line.
func TestMarkRewritten(t *testing.T) {
	h := http.Header{
		"Etag": {`"a"`}, "etag": {`W/"b"`, `"c"`}, "Accept-Ranges": {"bytes"}, "accept-ranges": {"none"},
		"content-type": {"text/plain"}, "X-Other": {"kept"},
	}
	MarkRewritten(h, false)
	if got := h["Etag"]; !slices.Equal(got, []string{`W/"a"`, `W/"b"`, `W/"c"`}) {
		t.Errorf("ETag = %q", got)
	}
	for _, k := range []string{"etag", "Accept-Ranges", "accept-ranges", "content-type"} {
		if _, ok := h[k]; ok {
			t.Errorf("key %q kept", k)
		}
	}
	if got := h["Content-Type"]; !slices.Equal(got, []string{"text/plain"}) || h.Get("X-Other") != "kept" {
		t.Errorf("Content-Type = %q, X-Other = %q; want them kept", got, h.Get("X-Other"))
	}
	created := http.Header{"content-type": {"text/plain"}}
	MarkRewritten(created, true)
	if got := created["Content-Type"]; !slices.Equal(got, []string{DefaultContentType}) || len(created) != 1 {
		t.Errorf("created body header = %v, want Content-Type application/json alone", created)
	}
}
