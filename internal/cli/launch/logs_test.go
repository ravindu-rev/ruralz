// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// TestCompactLine checks the short form of a JSON log line: time, level
// and message, then the other members as key=value in record order,
// without the Node, Revision, trace and span IDs; a line that is not one
// JSON object passes unchanged.
func TestCompactLine(t *testing.T) {
	for _, c := range []struct {
		name string
		line string
		want string
		ok   bool
	}{
		{
			"record",
			`{"time":"2026-09-26T12:00:00.000001Z","level":"WARN","msg":"revision rejected","component":"reload","node_id":"01ARYZ6S41TSV4RRFFQ69G5FAV","revision":"rev-162af81f5de4","trace_id":"t","span_id":"s","code":"RZ-CFG-026","diagnostics":2}`,
			`2026-09-26T12:00:00.000001Z WARN revision rejected component=reload code=RZ-CFG-026 diagnostics=2`,
			true,
		},
		{
			"quoted values",
			`{"time":"t","level":"INFO","msg":"m","path":"/a b","empty":"","eq":"a=b","quote":"say \"hi\"","ctl":"a\u0007b"}`,
			`t INFO m path="/a b" empty="" eq="a=b" quote="say \"hi\"" ctl="a\ab"`,
			true,
		},
		{
			"nested values and literals",
			`{"time":"t","level":"DEBUG","msg":"m","group":{"a": 1, "b":[true,null]},"n":1.5e3,"ok":false,"none":null}`,
			`t DEBUG m group={"a":1,"b":[true,null]} n=1.5e3 ok=false none=null`,
			true,
		},
		{"members in any order", `{"msg":"m","x":1,"level":"ERROR","time":"t"}`, `t ERROR m x=1`, true},
		{"no framing", `{"a":"b"}`, `a=b`, true},
		{"message with control characters", `{"msg":"a\nb"}`, "a�b", true},
		{"not JSON", `plain text line`, "", false},
		{"array", `[1,2]`, "", false},
		{"trailing text", `{"msg":"m"} extra`, "", false},
		{"two objects", `{"msg":"m"}{"msg":"n"}`, "", false},
		{"broken", `{"msg":`, "", false},
		{"non-string key", `{1:2}`, "", false},
	} {
		got, ok := CompactLine([]byte(c.line))
		if ok != c.ok || string(got) != c.want {
			t.Errorf("%s: CompactLine = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestLogWriter checks the copy of the child's streams: whole lines
// however the writes split them, a last line without a newline flushed,
// compacted only with compact, and the two streams never interleaving
// inside a line.
func TestLogWriter(t *testing.T) {
	var out bytes.Buffer
	sw := &syncWriter{w: &out}
	compact := newLogWriter(sw, true)
	raw := newLogWriter(sw, false)
	for _, part := range []string{`{"time":"t","level":"IN`, `FO","msg":"up"}` + "\nnot json\n", `{"msg":"last"}`} {
		_, _ = compact.Write([]byte(part))
	}
	_, _ = raw.Write([]byte(`{"msg":"raw"}` + "\n"))
	compact.flush()
	raw.flush()
	want := "t INFO up\nnot json\n" + `{"msg":"raw"}` + "\nlast\n"
	if got := out.String(); got != want {
		t.Errorf("logs =\n%q\nwant\n%q", got, want)
	}

	out.Reset()
	var wg sync.WaitGroup
	line := func(c byte) string { return strings.Repeat(string(c), 100) + "\n" }
	for i, w := range []*logWriter{newLogWriter(sw, false), newLogWriter(sw, false)} {
		c := byte('a' + i)
		wg.Go(func() {
			for range 50 {
				l := line(c)
				_, _ = w.Write([]byte(l[:30]))
				_, _ = w.Write([]byte(l[30:]))
			}
		})
	}
	wg.Wait()
	for _, l := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if len(l) != 100 || strings.Trim(l, string(l[0])) != "" {
			t.Fatalf("interleaved line %q", l)
		}
	}
}

// TestLogWriterLongLine: a line longer than the bound passes unchanged in
// pieces instead of growing without limit.
func TestLogWriterLongLine(t *testing.T) {
	var out bytes.Buffer
	w := newLogWriter(&syncWriter{w: &out}, true)
	long := strings.Repeat("x", maxLogLine+10)
	_, _ = w.Write([]byte(long))
	if out.Len() != maxLogLine+10 {
		t.Fatalf("held %d bytes of a line over the bound", maxLogLine+10-out.Len())
	}
	_, _ = w.Write([]byte("\n"))
	w.flush()
	if out.String() != long+"\n" {
		t.Errorf("the long line changed")
	}
}
