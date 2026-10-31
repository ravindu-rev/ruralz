// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dev

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
)

// ANSI color sequences of the text form's status.
const (
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiReset  = "\x1b[0m"
)

// tapLine is either shape of a /tap line: an event, or the {"dropped":N}
// notice the Node sends before its next event after a slow subscriber
// lost events. Unknown members are ignored.
type tapLine struct {
	adminapi.TapEvent
	Dropped *int64 `json:"dropped"`
}

// decodeTapLine decodes one /tap line: an event, or a drop notice whose
// count it returns, -1 for an event; ok is false for a line that is not a
// JSON object of either shape.
func decodeTapLine(line []byte) (ev adminapi.TapEvent, dropped int64, ok bool) {
	if t := bytes.TrimLeft(line, " \t\r"); len(t) == 0 || t[0] != '{' {
		return adminapi.TapEvent{}, -1, false
	}
	var l tapLine
	if err := json.Unmarshal(line, &l); err != nil {
		return adminapi.TapEvent{}, -1, false
	}
	if l.Dropped != nil {
		return adminapi.TapEvent{}, max(*l.Dropped, 0), true
	}
	return l.TapEvent, -1, true
}

// formatEvent returns the text form of ev
// (docs/reference/01-cli-and-api-surface.md "Plugin, AI and development
// verbs"): "<time> <status> <method> <host><path> route=<route>
// upstream=<upstream> code=<code> <durationMs>ms", leaving out members that
// are empty, then one indented line per header value, "> <name>: <value>"
// for a request header and "< <name>: <value>" for a response header, in
// name order. With color the status takes the color of its class. Text the
// Node chose keeps to printable characters, so a line cannot break the form
// or drive a terminal. The result ends without a newline.
func formatEvent(ev adminapi.TapEvent, color bool) []byte {
	var b strings.Builder
	words := make([]string, 0, 8)
	add := func(s string) {
		if s != "" {
			words = append(words, s)
		}
	}
	add(printable(ev.Time))
	if ev.Status != 0 {
		add(statusText(ev.Status, color))
	}
	add(printable(ev.Method))
	add(printable(ev.Host + ev.Path))
	add(member("route", ev.Route))
	add(member("upstream", ev.Upstream))
	add(member("code", ev.Code))
	add(strconv.FormatFloat(ev.DurationMs, 'f', -1, 64) + "ms")
	b.WriteString(strings.Join(words, " "))
	writeHeaders(&b, '>', ev.RequestHeaders)
	writeHeaders(&b, '<', ev.ResponseHeaders)
	return []byte(b.String())
}

// member is "<name>=<value>", or "" for an empty value.
func member(name, value string) string {
	if value == "" {
		return ""
	}
	return name + "=" + printable(value)
}

// statusText is the status, wrapped in the color of its class with color:
// 2xx and 3xx green, 4xx yellow, 5xx red.
func statusText(status int, color bool) string {
	s := strconv.Itoa(status)
	if !color {
		return s
	}
	switch {
	case status >= 200 && status < 400:
		return ansiGreen + s + ansiReset
	case status >= 400 && status < 500:
		return ansiYellow + s + ansiReset
	case status >= 500 && status < 600:
		return ansiRed + s + ansiReset
	default:
		return s
	}
}

// writeHeaders appends one indented line per value of h, names sorted:
// "  <mark> <name>: <value>".
func writeHeaders(b *strings.Builder, mark byte, h map[string][]string) {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		for _, v := range h[name] {
			b.WriteString("\n  ")
			b.WriteByte(mark)
			b.WriteByte(' ')
			b.WriteString(printable(name))
			b.WriteString(": ")
			b.WriteString(printable(v))
		}
	}
}

// printable keeps s on one printable line: invalid UTF-8 and control
// characters become U+FFFD.
func printable(s string) string {
	ok := utf8.ValidString(s)
	for _, r := range s {
		if !ok || unicode.IsControl(r) {
			ok = false
			break
		}
	}
	if ok {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return utf8.RuneError
		}
		return r
	}, strings.ToValidUTF8(s, string(utf8.RuneError)))
}
