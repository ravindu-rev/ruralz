// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package respproxy

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// recorder is a sink that writes what it receives as lines.
type recorder struct{ events []string }

func (r *recorder) command(args [][]byte, argc int64, cut bool) {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = string(a)
	}
	s := fmt.Sprintf("cmd %s argc=%d", strings.Join(parts, "|"), argc)
	if cut {
		s += " cut"
	}
	r.events = append(r.events, s)
}

func (r *recorder) replyStart() { r.events = append(r.events, "start") }

func (r *recorder) replyEnd(kind byte, msg []byte) {
	s := "end " + string(kind)
	if len(msg) > 0 {
		s += " " + string(msg)
	}
	r.events = append(r.events, s)
}

func (r *recorder) push() { r.events = append(r.events, "push") }

type parseCase struct {
	name    string
	in      string
	want    []string
	wantErr bool
}

// parse feeds in to a fresh parser in the given chunks and returns the
// events and whether the stream stopped parsing.
func parse(replies bool, chunks ...string) ([]string, bool) {
	r := &recorder{}
	p := newParser(replies, r)
	for _, c := range chunks {
		p.feed([]byte(c))
	}
	return r.events, p.err != nil
}

func commandCases() []parseCase {
	return []parseCase{
		{name: "array of bulk strings", in: "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n", want: []string{"cmd SET|k|v argc=3"}},
		{name: "pipelined commands", in: "*1\r\n$4\r\nPING\r\n*2\r\n$3\r\nGET\r\n$1\r\nk\r\n", want: []string{"cmd PING argc=1", "cmd GET|k argc=2"}},
		{name: "binary argument holding CRLF", in: "*2\r\n$4\r\nECHO\r\n$4\r\na\r\nb\r\n", want: []string{"cmd ECHO|a\r\nb argc=2"}},
		{name: "empty argument", in: "*2\r\n$4\r\nECHO\r\n$0\r\n\r\n", want: []string{"cmd ECHO| argc=2"}},
		{name: "inline command", in: "PING\r\n", want: []string{"cmd PING argc=1"}},
		{name: "inline command ended by LF", in: "set  k\tv\n", want: []string{"cmd set|k|v argc=3"}},
		{name: "blank inline lines and empty arrays are skipped", in: "\r\n\n*0\r\n*-1\r\n PING \r\n", want: []string{"cmd PING argc=1"}},
		{name: "inline then array", in: "PING\r\n*1\r\n$4\r\nPING\r\n", want: []string{"cmd PING argc=1", "cmd PING argc=1"}},
		{name: "argument of another type", in: "*1\r\n+PING\r\n", wantErr: true},
		{name: "null bulk argument", in: "*1\r\n$-1\r\n", wantErr: true},
		{name: "bad command length", in: "*x\r\n", wantErr: true},
		{name: "negative command length", in: "*-2\r\n", wantErr: true},
		{name: "bad argument length", in: "*1\r\n$+4\r\nPING\r\n", wantErr: true},
		{name: "argument not followed by CRLF", in: "*1\r\n$4\r\nPINGxx", wantErr: true},
		{name: "nothing after a desync", in: "*1\r\n:1\r\n*1\r\n$4\r\nPING\r\n", wantErr: true},
		{name: "events before a desync stay", in: "*1\r\n$4\r\nPING\r\n*1\r\n:1\r\n", want: []string{"cmd PING argc=1"}, wantErr: true},
	}
}

func replyCases() []parseCase {
	return []parseCase{
		{name: "simple string", in: "+OK\r\n", want: []string{"start", "end +"}},
		{name: "error", in: "-NOSCRIPT No matching script\r\n", want: []string{"start", "end - NOSCRIPT No matching script"}},
		{name: "integer", in: ":42\r\n", want: []string{"start", "end :"}},
		{name: "double", in: ",3.14\r\n", want: []string{"start", "end ,"}},
		{name: "boolean", in: "#t\r\n", want: []string{"start", "end #"}},
		{name: "big number", in: "(3492890328409238509324850943850943825024385\r\n", want: []string{"start", "end ("}},
		{name: "RESP3 null", in: "_\r\n", want: []string{"start", "end _"}},
		{name: "bulk string", in: "$5\r\nhello\r\n", want: []string{"start", "end $"}},
		{name: "bulk string holding CRLF", in: "$4\r\n\r\n\r\n\r\n", want: []string{"start", "end $"}},
		{name: "empty bulk string", in: "$0\r\n\r\n", want: []string{"start", "end $"}},
		{name: "RESP2 null bulk string", in: "$-1\r\n", want: []string{"start", "end $"}},
		{name: "blob error", in: "!21\r\nSYNTAX invalid syntax\r\n", want: []string{"start", "end ! SYNTAX invalid syntax"}},
		{name: "verbatim string", in: "=15\r\ntxt:Some string\r\n", want: []string{"start", "end ="}},
		{name: "array", in: "*2\r\n$1\r\na\r\n:1\r\n", want: []string{"start", "end *"}},
		{name: "empty array", in: "*0\r\n", want: []string{"start", "end *"}},
		{name: "RESP2 null array", in: "*-1\r\n", want: []string{"start", "end *"}},
		{name: "nested aggregates", in: "*2\r\n*1\r\n+a\r\n%1\r\n+k\r\n*0\r\n", want: []string{"start", "end *"}},
		{name: "map", in: "%2\r\n+a\r\n:1\r\n+b\r\n:2\r\n", want: []string{"start", "end %"}},
		{name: "set", in: "~2\r\n+a\r\n+b\r\n", want: []string{"start", "end ~"}},
		{name: "push answers nothing", in: ">2\r\n+message\r\n+x\r\n", want: []string{"push"}},
		{name: "attribute ahead of a reply", in: "|1\r\n+ttl\r\n:3600\r\n$5\r\nhello\r\n", want: []string{"start", "end $"}},
		{name: "empty attribute", in: "|0\r\n+OK\r\n", want: []string{"start", "end +"}},
		{name: "attribute inside an array", in: "*2\r\n|1\r\n+a\r\n*1\r\n+b\r\n:1\r\n:2\r\n", want: []string{"start", "end *"}},
		{name: "attribute ahead of a push", in: "|1\r\n+a\r\n+b\r\n>1\r\n+p\r\n", want: []string{"push"}},
		{name: "nested error is no error reply", in: "*1\r\n-ERR inner\r\n", want: []string{"start", "end *"}},
		{name: "pipelined replies around a push", in: "+OK\r\n>1\r\n+p\r\n:1\r\n", want: []string{"start", "end +", "push", "start", "end :"}},
		{name: "unknown type byte", in: "?x\r\n", wantErr: true},
		{name: "streamed string", in: "$?\r\n;4\r\nHell\r\n;0\r\n", wantErr: true},
		{name: "bad blob length", in: "$abc\r\n", wantErr: true},
		{name: "bad array length", in: "*-2\r\n", wantErr: true},
		{name: "null map", in: "%-1\r\n", wantErr: true},
		{name: "null set", in: "~-1\r\n", wantErr: true},
		{name: "null blob error", in: "!-1\r\n", wantErr: true},
		{name: "blob not followed by CRLF", in: "$1\r\nab", want: []string{"start"}, wantErr: true},
	}
}

func checkCases(t *testing.T, replies bool, cases []parseCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, failed := parse(replies, tc.in)
			if failed != tc.wantErr {
				t.Errorf("parse(%q) desync = %v, want %v", tc.in, failed, tc.wantErr)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("parse(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestParserCommands checks that client bytes parse into commands, arrays
// of bulk strings or inline lines, and that anything Redis would refuse
// as a protocol error stops the parse.
func TestParserCommands(t *testing.T) { checkCases(t, false, commandCases()) }

// TestParserReplies checks every RESP2 and RESP3 reply type, nesting,
// attributes (which annotate a reply and are not one) and pushes (which
// answer no command), and that only a top-level error carries a message.
func TestParserReplies(t *testing.T) { checkCases(t, true, replyCases()) }

// TestParserChunkBoundaries checks that the events never depend on where
// the proxy's reads split the stream: every case, alone and all cases
// back to back, split at every byte, at every single point, and into
// seeded random chunks.
func TestParserChunkBoundaries(t *testing.T) {
	for _, mode := range []struct {
		replies bool
		cases   []parseCase
	}{{false, commandCases()}, {true, replyCases()}} {
		var all strings.Builder
		streams := []string{}
		for _, tc := range mode.cases {
			streams = append(streams, tc.in)
			if !tc.wantErr {
				all.WriteString(tc.in)
			}
		}
		streams = append(streams, all.String())
		rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: reproducible chunk sizes
		for _, s := range streams {
			want, wantFailed := parse(mode.replies, s)
			check := func(how string, chunks []string) {
				t.Helper()
				got, failed := parse(mode.replies, chunks...)
				if failed != wantFailed || !slices.Equal(got, want) {
					t.Fatalf("%q fed %s: events %q (desync %v), want %q (desync %v)", s, how, got, failed, want, wantFailed)
				}
			}
			bytewise := make([]string, len(s))
			for i := range s {
				bytewise[i] = s[i : i+1]
			}
			check("byte by byte", bytewise)
			for i := range len(s) + 1 {
				check(fmt.Sprintf("split at %d", i), []string{s[:i], s[i:]})
			}
			for range 20 {
				var chunks []string
				for rest := s; len(rest) > 0; {
					n := min(1+rng.IntN(7), len(rest))
					chunks, rest = append(chunks, rest[:n]), rest[n:]
				}
				check(fmt.Sprintf("in chunks %q", chunks), chunks)
			}
		}
	}
}

// TestParserBounds checks the bounds that keep a parser's memory small:
// lines, nesting, kept arguments and kept error messages.
func TestParserBounds(t *testing.T) {
	long := strings.Repeat("a", maxLine)
	if _, failed := parse(true, "+"+long[:maxLine-1]+"\r\n"); failed {
		t.Error("a line of maxLine bytes with its CR: desync, want a reply")
	}
	if _, failed := parse(true, "+"+long+"a"); !failed {
		t.Error("a line longer than maxLine: want a desync")
	}
	if _, failed := parse(false, long+"a"); !failed {
		t.Error("an inline command longer than maxLine: want a desync")
	}

	nest := func(depth int) string { return strings.Repeat("*1\r\n", depth) + "+a\r\n" }
	if got, failed := parse(true, nest(maxDepth)); failed || !slices.Equal(got, []string{"start", "end *"}) {
		t.Errorf("nesting %d deep = %q (desync %v), want one reply", maxDepth, got, failed)
	}
	if _, failed := parse(true, nest(maxDepth+1)); !failed {
		t.Errorf("nesting %d deep: want a desync", maxDepth+1)
	}

	args := make([]string, maxArgs+6)
	for i := range args {
		args[i] = fmt.Sprint(i)
	}
	got, _ := parse(false, cmd(args...))
	if want := fmt.Sprintf("cmd %s argc=%d cut", strings.Join(args[:maxArgs], "|"), len(args)); !slices.Equal(got, []string{want}) {
		t.Errorf("a command of %d arguments = %q, want %q", len(args), got, want)
	}
	big := strings.Repeat("k", maxArgBytes+10)
	for _, in := range []string{cmd("GET", big), "GET " + big + "\r\n"} {
		got, _ := parse(false, in)
		if want := "cmd GET|" + big[:maxArgBytes] + " argc=2 cut"; !slices.Equal(got, []string{want}) {
			t.Errorf("a %d-byte argument = %q, want it cut to %d bytes", len(big), got, maxArgBytes)
		}
	}
	msg := "ERR " + strings.Repeat("m", maxErrBytes)
	for _, in := range []string{"-" + msg + "\r\n", fmt.Sprintf("!%d\r\n%s\r\n", len(msg), msg)} {
		got, _ := parse(true, in)
		if want := "end " + in[:1] + " " + msg[:maxErrBytes]; len(got) != 2 || got[1] != want {
			t.Errorf("a %d-byte error = %q, want it cut to %d bytes", len(msg), got, maxErrBytes)
		}
	}
}

// TestParseLen checks that a length is an optional minus sign and
// decimal digits fitting int64, nothing else.
func TestParseLen(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true},
		{"-1", -1, true},
		{"123", 123, true},
		{"00012", 12, true},
		{"9223372036854775807", 1<<63 - 1, true},
		{"-9223372036854775808", -1 << 63, true},
		{"9223372036854775808", 0, false},
		{"", 0, false},
		{"-", 0, false},
		{"+1", 0, false},
		{" 1", 0, false},
		{"1 ", 0, false},
		{"1a", 0, false},
		{"?", 0, false},
	} {
		n, ok := parseLen([]byte(tc.in))
		if ok != tc.ok || (ok && n != tc.want) {
			t.Errorf("parseLen(%q) = %d, %v, want %d, %v", tc.in, n, ok, tc.want, tc.ok)
		}
	}
}
