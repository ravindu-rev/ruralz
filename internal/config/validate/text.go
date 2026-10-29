// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"errors"
	"regexp/syntax"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// maxQuoted is how many code points of an authored value a message
// quotes, as stages G and I do, so a hostile value never makes a
// diagnostic large.
const maxQuoted = 64

// maxMessage bounds the bytes of a message built from another package's
// error text, which may quote authored values beyond maxQuoted.
const maxMessage = 512

// clip returns s cut to maxQuoted code points plus "..." when longer, with
// invalid UTF-8 replaced, so a quoted value stays short and printable.
func clip(s string) string {
	n := 0
	for i := range s {
		if n == maxQuoted {
			return strings.ToValidUTF8(s[:i], "�") + "..."
		}
		n++
	}
	return strings.ToValidUTF8(s, "�")
}

// quote returns the clipped value in double quotes.
func quote(s string) string { return strconv.Quote(clip(s)) }

// causeText returns the message of err for a diagnostic: the cause of its
// outermost errcode.Error (without the "RZ-CFG-NNN: " prefix), with every
// full quotation of an authored value in values replaced by its clipped
// form, cut to maxMessage bytes.
func causeText(err error, values ...string) string {
	msg := err.Error()
	var ce *errcode.Error
	if errors.As(err, &ce) && ce.Err != nil {
		msg = ce.Err.Error()
	}
	for _, v := range values {
		if utf8.RuneCountInString(v) > maxQuoted {
			msg = strings.ReplaceAll(msg, strconv.Quote(v), quote(v))
		}
	}
	return limitMessage(msg)
}

// limitMessage cuts msg to at most maxMessage bytes on a code point
// boundary, marking the cut with "...".
func limitMessage(msg string) string {
	if len(msg) <= maxMessage {
		return msg
	}
	cut := maxMessage
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "..."
}

// isRegexError reports whether err holds a regexp/syntax error.
func isRegexError(err error) bool {
	var se *syntax.Error
	return errors.As(err, &se)
}

// regexText describes an invalid RE2 expression without quoting it whole:
// the regexp/syntax error code and a clipped fragment.
func regexText(err error) string {
	var se *syntax.Error
	if errors.As(err, &se) {
		return "invalid RE2 expression: " + string(se.Code) + ": " + quote(se.Expr)
	}
	return limitMessage("invalid RE2 expression: " + err.Error())
}
