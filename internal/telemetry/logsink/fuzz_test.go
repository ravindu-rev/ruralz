// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

// FuzzProcessLog: arbitrary messages, keys, values and group names always
// give exactly one valid JSON line, with no raw newline inside it.
func FuzzProcessLog(f *testing.F) {
	f.Add("msg", "key", "value", "group", int64(1))
	f.Add("", "", "", "", int64(0))
	f.Add("a\nb", "time", "\xff\xfe", "", int64(-1))
	f.Add("\u2028", "code", "\"}{", "g", int64(7))
	f.Add("x", "authorization", "Bearer secret", "headers", int64(3))
	f.Fuzz(func(t *testing.T, msg, key, value, group string, n int64) {
		var out bytes.Buffer
		s, err := New(Options{Stdout: &out, NodeID: value})
		if err != nil {
			t.Fatal(err)
		}
		h := s.Handler(key).WithAttrs([]slog.Attr{slog.String(key, value)}).WithGroup(group)
		r := slog.NewRecord(time.Unix(n, n), slog.LevelInfo, msg, 0)
		r.AddAttrs(slog.String(key, value), slog.Group(group, slog.Int64(key, n)), slog.Group("", slog.String(value, msg)))
		if err := h.Handle(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		s.pump(nil)
		b := out.Bytes()
		if len(b) == 0 || b[len(b)-1] != '\n' || bytes.Count(b, []byte{'\n'}) != 1 {
			t.Fatalf("want exactly one line, got %q", b)
		}
		if !json.Valid(b) {
			t.Fatalf("invalid JSON: %q", b)
		}
	})
}
