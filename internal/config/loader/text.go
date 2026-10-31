// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// maxShown bounds, in bytes, the authored text a diagnostic quotes in each
// place: a value in a message, a resource kind or name. A message stays
// short however long the value.
const maxShown = 64

// clip returns at most maxShown bytes of s, cut before a rune and marked
// "..." when cut. The result is a copy, so a kept diagnostic never keeps a
// file's text alive.
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

// quote returns s clipped and Go-quoted, for a value in a message.
func quote(s string) string { return strconv.Quote(clip(s)) }

// shown returns a kind or name as a diagnostic's resource shows it:
// clipped, Go-quoted when it holds a character that is not graphic, a
// quote or a backslash, and "" when empty, so the one-line text form of a
// diagnostic stays one line.
func shown(s string) string {
	if s == "" {
		return `""`
	}
	c := clip(s)
	for _, r := range c {
		if r == '"' || r == '\\' || r == utf8.RuneError || !strconv.IsGraphic(r) {
			return strconv.QuoteToGraphic(c)
		}
	}
	return c
}

// resourceID returns the identity a diagnostic names.
func resourceID(id tree.ID) *diag.ResourceID {
	return &diag.ResourceID{Kind: shown(string(id.Kind)), Name: shown(id.Name)}
}

// idText returns an identity for a message: Kind/name.
func idText(id tree.ID) string { return shown(string(id.Kind)) + "/" + shown(id.Name) }
