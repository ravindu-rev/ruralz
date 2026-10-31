// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// maxLogLine bounds the part of a log line the writer holds while it waits
// for the line's end; a longer line passes unchanged in pieces.
const maxLogLine = 1 << 20

// Log record members (internal/telemetry/catalog and log/slog): the three
// a short line leads with, and the identifiers it leaves out.
const (
	logTime    = "time"
	logLevel   = "level"
	logMessage = "msg"
)

// droppedLogMembers are the Node, Revision, trace and span IDs, which a
// short line leaves out (internal/telemetry/catalog KeyNodeID, KeyRevision,
// KeyTraceID, KeySpanID).
func droppedLogMembers() []string { return []string{"node_id", "revision", "trace_id", "span_id"} }

// syncWriter serializes the writes of the child's two streams.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) write(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.w.Write(b)
}

// logWriter copies one of the child's streams to the CLI's standard error
// a whole line at a time, so the two streams never interleave inside a
// line; with compact, each JSON log line becomes its short form
// (CompactLine). Write never fails: a closed standard error only drops log
// lines.
type logWriter struct {
	out     *syncWriter
	compact bool

	mu  sync.Mutex
	buf []byte
}

// newLogWriter returns a writer to out; the writers of the child's two
// streams share out and its lock.
func newLogWriter(out *syncWriter, compact bool) *logWriter {
	return &logWriter{out: out, compact: compact}
}

// Write takes the next bytes of the stream.
func (l *logWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			l.buf = append(l.buf, p...)
			if len(l.buf) > maxLogLine {
				l.out.write(l.buf)
				l.buf = l.buf[:0]
			}
			break
		}
		l.buf = append(l.buf, p[:i+1]...)
		p = p[i+1:]
		l.emit()
	}
	return n, nil
}

// flush writes a last line that has no newline.
func (l *logWriter) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.buf = append(l.buf, '\n')
		l.emit()
	}
}

// emit writes the held line, which ends with a newline.
func (l *logWriter) emit() {
	line := l.buf
	if l.compact {
		if short, ok := CompactLine(bytes.TrimSuffix(line, []byte("\n"))); ok {
			short = append(short, '\n')
			line = short
		}
	}
	l.out.write(line)
	l.buf = l.buf[:0]
}

// CompactLine returns the short form of a JSON log line
// (docs/reference/01-cli-and-api-surface.md "Local ruralzd launched by the
// CLI", Logs): its time, level and message, then its other members as
// key=value in record order, without the Node, Revision, trace and span
// IDs. A string value prints bare unless it is empty or holds a space, a
// quote, an equals sign or a character that is not printable, which print
// it quoted; other values print as their compact JSON. ok is false for a
// line that is not one JSON object, which passes unchanged.
func CompactLine(line []byte) ([]byte, bool) {
	t := bytes.TrimSpace(line)
	if len(t) == 0 || t[0] != '{' {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(t))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	type member struct {
		key   string
		value json.RawMessage
	}
	var head [3]string
	var rest []member
	dropped := droppedLogMembers()
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		switch key {
		case logTime:
			head[0] = scalarText(v, false)
		case logLevel:
			head[1] = scalarText(v, false)
		case logMessage:
			head[2] = scalarText(v, false)
		default:
			if !slices.Contains(dropped, key) {
				rest = append(rest, member{key: key, value: v})
			}
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	var b strings.Builder
	for _, h := range head {
		if h == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(h)
	}
	for _, m := range rest {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(printableText(m.key))
		b.WriteByte('=')
		b.WriteString(scalarText(m.value, true))
	}
	return []byte(b.String()), true
}

// scalarText returns the text of a JSON value: a string's content, quoted
// when quote is set and the string needs it (or always when it cannot
// print bare), and any other value as its compact JSON.
func scalarText(v json.RawMessage, quote bool) string {
	var s string
	if len(v) > 0 && v[0] == '"' {
		if err := json.Unmarshal(v, &s); err == nil {
			if quote && needsQuote(s) {
				return strconv.Quote(s)
			}
			return printableText(s)
		}
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, v); err != nil {
		return printableText(string(v))
	}
	return buf.String()
}

// needsQuote reports a value that cannot print bare after "key=".
func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r == ' ' || r == '"' || r == '=' || !unicode.IsPrint(r) {
			return true
		}
	}
	return false
}

// printableText keeps s on one printable line: invalid UTF-8 and control
// characters become U+FFFD.
func printableText(s string) string {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return utf8.RuneError
		}
		return r
	}, s)
}
