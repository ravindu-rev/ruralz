// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// maxShown bounds, in bytes, the text from the content that one
// diagnostic carries in each place: a quoted value in a message, a
// resource name or kind, a member name, a list key or a set element of a
// path. Each finding thus stays short however large the content, and the
// diagnostics of a decode print in space linear in their count.
const maxShown = 64

// clip shortens s to at most maxShown bytes, cut before a rune so a
// multi-byte character is never split, and marks a cut with "...". The
// result is always a copy: s is usually a substring of the parser's one
// copy of the content, and a diagnostic that kept it would keep the whole
// content alive for as long as the error is kept.
func clip(s string) string {
	if len(s) <= maxShown {
		return strings.Clone(s)
	}
	cut := maxShown
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// shown returns a resource kind or name read from the content as a
// diagnostic shows it: clipped (a copy), Go-quoted
// (strconv.QuoteToGraphic) when it holds a character that is not graphic
// (a control or format character such as ESC, a C1 control or U+202E), a
// quote, a backslash or invalid UTF-8, and "" when it is empty, so an
// identity is never blank. diag.AppendText prints a resource identity as
// it is, so text output then carries no terminal control sequence; a name
// that needs no escape is shown unquoted, and deciding that allocates
// nothing.
//
// The member names, keys and set elements of a path are only clipped:
// diag quotes them as JSON (diag.Path.String), which escapes the C0
// controls, the quote and the backslash but leaves C1 controls and format
// characters such as U+202E as they are, so a path does not yet escape
// what an identity does; escaping every rune strconv.IsGraphic rejects as
// \uXXXX would take a change to internal/config/diag.
func shown(s string) string {
	if s == "" {
		return `""`
	}
	c := clip(s)
	if needsQuote(c) {
		return strconv.QuoteToGraphic(c)
	}
	return c
}

// needsQuote reports whether strconv.QuoteToGraphic would escape a
// character of s: one that is not graphic, a quote, a backslash or a byte
// of invalid UTF-8.
func needsQuote(s string) bool {
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c < 0x20 || c == 0x7f || c == '"' || c == '\\' {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || !strconv.IsGraphic(r) {
			return true
		}
		i += size
	}
	return false
}

// resourceID returns the identity diagnostics carry for a resource of the
// content (see shown).
func resourceID(kind, name string) *diag.ResourceID {
	return &diag.ResourceID{Kind: shown(kind), Name: shown(name)}
}

// boundElem bounds the content text of path element e of a finding: a
// member name, a keyed entry's key and a string set element are clipped
// (diag quotes them as JSON), and a set element whose canonical JSON is
// longer than maxShown is named by its index i, as schemaidx.ItemElem
// names an element it cannot key; canonical content keeps a set sorted,
// so the index names one element. Every text is a copy (clip), never a
// substring of the content: schemaidx.ItemElem returns the content's own
// key, string and number texts.
func boundElem(e diag.PathElem, i int) diag.PathElem {
	switch e.Kind {
	case diag.ElemField:
		e.Name = clip(e.Name)
	case diag.ElemKeyed:
		e.Key = clip(e.Key)
	case diag.ElemItem:
		switch v := e.Item.(type) {
		case string:
			e.Item = clip(v)
		case json.Number:
			if len(v) > maxShown {
				return diag.Index(i)
			}
			e.Item = json.Number(clip(string(v)))
		case json.RawMessage:
			if len(v) > maxShown {
				return diag.Index(i)
			}
		default:
		}
	case diag.ElemIndex:
	}
	return e
}

// boundPath clips the member names and keys of p in place and returns it.
// It bounds the paths of the encoder's failures, which hold member names
// and indexes only: in ErrEncode messages (encodeError) and in the
// decoder's re-encoding findings (Decoder.form).
func boundPath(p diag.Path) diag.Path {
	for j := range p {
		switch p[j].Kind {
		case diag.ElemField:
			p[j].Name = clip(p[j].Name)
		case diag.ElemKeyed:
			p[j].Key = clip(p[j].Key)
		case diag.ElemIndex, diag.ElemItem:
		}
	}
	return p
}

// idText returns a resource identity for an error message, its kind and
// name shown as diagnostics show them.
func idText(id tree.ID) string {
	return shown(string(id.Kind)) + "/" + shown(id.Name)
}
