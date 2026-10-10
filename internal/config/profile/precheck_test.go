// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"strings"
	"testing"
)

// TestPrecheck covers the byte pre-checks of 01 req 7: each failure is
// RZ-CFG-001 at the offending character, in code-point columns.
func TestPrecheck(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		line, col int
		msg       string
	}{
		{name: "invalid UTF-8", src: "a: 1\nb: \xff\n", line: 2, col: 4, msg: "not valid UTF-8"},
		{name: "truncated sequence", src: "é: \xc3", line: 1, col: 4, msg: "not valid UTF-8"},
		{name: "surrogate encoding", src: "a: \xed\xa0\x80\n", line: 1, col: 4, msg: "not valid UTF-8"},
		{name: "BOM at start", src: "\uFEFFa: 1\n", line: 1, col: 1, msg: "byte order mark"},
		{name: "BOM later", src: "a: x\uFEFF\n", line: 1, col: 5, msg: "byte order mark"},
		{name: "BOM in a quoted scalar", src: "a: \"x\uFEFF\"\n", line: 1, col: 6, msg: "byte order mark"},
		{name: "BEL", src: "a: b\x07\n", line: 1, col: 5, msg: "U+0007"},
		{name: "NUL", src: "a: \x00", line: 1, col: 4, msg: "U+0000"},
		{name: "DEL", src: "a: \x7f\n", line: 1, col: 4, msg: "U+007F"},
		{name: "C1 control", src: "a: \u0080\n", line: 1, col: 4, msg: "U+0080"},
		{name: "U+FFFE", src: "é: x\n\n  \uFFFE", line: 3, col: 3, msg: "U+FFFE"},
		{name: "after CRLF lines", src: "a: 1\r\nb: 2\r\nc: \x01", line: 3, col: 4, msg: "U+0001"},
		{name: "after a lone CR", src: "a: 1\rb: \x01", line: 2, col: 4, msg: "U+0001"},
	}
	for _, tt := range tests {
		docs, diags := parseYAML(t, tt.src)
		d := wantOne(t, diags, codeParse, tt.line, tt.col)
		if !strings.Contains(d.Message, tt.msg) {
			t.Errorf("%s: message %q, want it to mention %q", tt.name, d.Message, tt.msg)
		}
		if len(docs) != 0 {
			t.Errorf("%s: %d documents, want none", tt.name, len(docs))
		}
	}
}

// TestPrecheckAccepts covers the printable ranges that stay accepted (01
// req 7).
func TestPrecheckAccepts(t *testing.T) {
	for _, s := range []string{"\t", "\u0085", "\u00A0", "\uD7FF", "\uE000", "\uFFFD", "\U00010000", "\U0010FFFF", "😀"} {
		docs, diags := parseYAML(t, "a: \"x"+s+"\"\n")
		if len(diags) != 0 || len(docs) != 1 {
			t.Errorf("%U: diagnostics %q, %d documents", []rune(s)[0], codes(diags), len(docs))
			continue
		}
		if got, _ := docs[0].Root.Get("a"); got.Text != "x"+s {
			t.Errorf("%U: value %q", []rune(s)[0], got.Text)
		}
	}
}

// TestLineBreaksParseIdentically checks that CRLF and CR files parse
// exactly like LF files, positions included (01 req 7).
func TestLineBreaksParseIdentically(t *testing.T) {
	lf := "# comment\n" +
		"apiVersion: ruralz/v1alpha1\n" +
		"kind: Route\n" +
		"spec:\n" +
		"  lit: |\n" +
		"    line one\n" +
		"\n" +
		"    line three\n" +
		"  folded: >\n" +
		"    a\n" +
		"    b\n" +
		"  quoted: \"multi\n" +
		"    line\"\n" +
		"  single: 'x\n" +
		"\n" +
		"    y'\n" +
		"  plain: one\n" +
		"    two\n" +
		"  list:\n" +
		"  - 1\n" +
		"  - [2, 3]\n" +
		"---\n" +
		"b: 2\n"
	want, diags := parseYAML(t, lf)
	if len(diags) != 0 || len(want) != 2 {
		t.Fatalf("LF: diagnostics %q, %d documents", codes(diags), len(want))
	}
	if v, _ := want[0].Root.Get("spec"); v == nil {
		t.Fatal("no spec")
	} else if lit, _ := v.Get("lit"); lit.Text != "line one\n\nline three\n" {
		t.Fatalf("literal = %q", lit.Text)
	}
	for name, src := range map[string]string{
		"CRLF": strings.ReplaceAll(lf, "\n", "\r\n"),
		"CR":   strings.ReplaceAll(lf, "\n", "\r"),
	} {
		got, diags := parseYAML(t, src)
		if len(diags) != 0 || len(got) != len(want) {
			t.Fatalf("%s: diagnostics %q, %d documents", name, codes(diags), len(got))
		}
		for i := range want {
			if a, b := show(want[i].Root, true), show(got[i].Root, true); a != b {
				t.Errorf("%s document %d:\n got %s\nwant %s", name, i, b, a)
			}
			if want[i].Start != got[i].Start {
				t.Errorf("%s document %d: start %v, want %v", name, i, got[i].Start, want[i].Start)
			}
		}
	}
}

// TestSizeLimit reports a file over MaxBytes at the first byte past it,
// for both formats (01 req 6).
func TestSizeLimit(t *testing.T) {
	src := []byte("a: 1\nb: 22\n")
	for _, f := range []Format{FormatYAML, FormatJSON} {
		docs, diags, err := Parse(context.Background(), src, 0, "f", f, Options{MaxBytes: 7})
		if err != nil {
			t.Fatal(err)
		}
		d := wantOne(t, diags, codeParse, 2, 3)
		if !strings.Contains(d.Message, "11 bytes, over the 7-byte limit") || len(docs) != 0 {
			t.Errorf("%v: %q, %d documents", f, d.Message, len(docs))
		}
	}
}
