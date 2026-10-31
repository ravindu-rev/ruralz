// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestScalarPos covers the file column of a character inside a scalar on
// one line: plain scalars map one to one; quoted scalars count every
// escape written before the character, a character an escape writes
// sitting at its backslash, a doubled quote in a single-quoted scalar, \x,
// \u, a surrogate pair and \U; the end of the value is the closing quote;
// lines end at LF, CR LF or a lone CR; columns count code points; a tag
// before the scalar is skipped. Block scalars, quoted scalars over several
// lines, values a default wrote, and text that does not hold the value
// fail.
func TestScalarPos(t *testing.T) {
	// bs is a backslash, so the escapes below stay as written.
	const bs = `\`
	tests := []struct {
		name  string
		text  string
		style tree.Style
		line  int32
		col   int32
		value string
		off   int
		want  int32 // 0: no position
	}{
		{"plain", "a: x == y\n", tree.StylePlain, 1, 4, "x == y", 5, 9},
		{"plain on a later CR LF line", "a: 1\r\nb: x == y\r\n", tree.StylePlain, 2, 4, "x == y", 5, 9},
		{"plain on a later lone CR line", "a: 1\rb: x == y\r", tree.StylePlain, 2, 4, "x == y", 5, 9},
		{"plain after multibyte characters", "é: x == y\n", tree.StylePlain, 1, 4, "x == y", 5, 9},
		{"plain with multibyte characters before the offset", "a: \"é\" == y\n", tree.StylePlain, 1, 4, `"é" == y`, 8, 11},
		{"plain the text does not hold", "a: x != y\n", tree.StylePlain, 1, 4, "x == y", 5, 0},
		{"plain at the end of the value", "a: x == y # c\n", tree.StylePlain, 1, 4, "x == y", 6, 10},
		{"double-quoted escapes", `a: "\"x\" == y"` + "\n", tree.StyleDoubleQuoted, 1, 4, `"x" == y`, 7, 14},
		{"double-quoted escape is the character", `a: "\"x\" == y"` + "\n", tree.StyleDoubleQuoted, 1, 4, `"x" == y`, 2, 8},
		{"double-quoted hex escapes", `a: "` + bs + "x41" + bs + "u00e9" + bs + `U0001F600 b"` + "\n", tree.StyleDoubleQuoted, 1, 4, "Aé😀 b", 9, 27},
		{"double-quoted surrogate pair", `a: "` + bs + "uD83D" + bs + `uDE00 b"` + "\n", tree.StyleDoubleQuoted, 1, 4, "😀 b", 5, 18},
		{"double-quoted lone surrogate", `a: "` + bs + `uD83D b"` + "\n", tree.StyleDoubleQuoted, 1, 4, "b", 0, 0},
		{"double-quoted named escapes", `a: "\t\n\\ b"` + "\n", tree.StyleDoubleQuoted, 1, 4, "\t\n\\ b", 4, 12},
		{"double-quoted end of the value", `a: "x\ty"` + "\n", tree.StyleDoubleQuoted, 1, 4, "x\ty", 3, 9},
		{"double-quoted over two lines", "a: \"x ==\n  y\"\n", tree.StyleDoubleQuoted, 1, 4, "x == y", 5, 0},
		{"double-quoted escaped line break", "a: \"x ==\\\n  y\"\n", tree.StyleDoubleQuoted, 1, 4, "x ==y", 4, 0},
		{"double-quoted unknown escape", `a: "\q"` + "\n", tree.StyleDoubleQuoted, 1, 4, "q", 0, 0},
		{"double-quoted the text does not hold", `a: "x == z"` + "\n", tree.StyleDoubleQuoted, 1, 4, "x == y", 5, 0},
		{"single-quoted doubled quote", "a: 'x == ''y'' && z'\n", tree.StyleSingleQuoted, 1, 4, "x == 'y' && z", 12, 19},
		{"single-quoted unterminated on the line", "a: 'x\n  y'\n", tree.StyleSingleQuoted, 1, 4, "x y", 2, 0},
		{"JSON escapes", `{"a": "x \"y\" ` + bs + `u00e9 z"}`, tree.StyleJSON, 1, 7, `x "y" é z`, 9, 23},
		{"tagged", "a: !!str x == y\n", tree.StylePlain, 1, 4, "x == y", 5, 15},
		{"tagged double-quoted", "a: !!str \"x\" \n", tree.StyleDoubleQuoted, 1, 4, "x", 1, 12},
		{"block literal", "a: |\n  x == y\n", tree.StyleLiteral, 1, 4, "x == y\n", 5, 0},
		{"defaulted", "a: x\n", tree.StyleDefaulted, 1, 4, "consumer.name", 0, 0},
		{"line past the text", "a: x\n", tree.StylePlain, 3, 4, "x", 0, 0},
		{"column past the line", "a: x\n", tree.StylePlain, 1, 9, "x", 0, 0},
		{"offset past the value", "a: x\n", tree.StylePlain, 1, 4, "x", 2, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := &tree.Node{Kind: tree.KindString, Style: tc.style, Pos: tree.Pos{File: 3, Line: tc.line, Column: tc.col}, Text: tc.value}
			pos, ok := scalarPos(n, tc.off, []byte(tc.text))
			switch {
			case tc.want == 0 && ok:
				t.Errorf("scalarPos(%q, %d) = %+v, want no position", tc.text, tc.off, pos)
			case tc.want != 0 && (!ok || pos != tree.Pos{File: 3, Line: tc.line, Column: tc.want}):
				t.Errorf("scalarPos(%q, %d) = %+v, %v; want column %d", tc.text, tc.off, pos, ok, tc.want)
			}
		})
	}
	if _, ok := scalarPos(nil, 0, nil); ok {
		t.Error("scalarPos(nil) reported a position")
	}
	if _, ok := scalarPos(&tree.Node{Kind: tree.KindString, Pos: tree.Pos{Line: 1, Column: 4}, Text: "x"}, -1, []byte("a: x\n")); ok {
		t.Error("scalarPos of a negative offset reported a position")
	}
	if _, ok := scalarPos(&tree.Node{Kind: tree.KindInt, Pos: tree.Pos{Line: 1, Column: 1}, Text: "1"}, 0, []byte("1")); ok {
		t.Error("scalarPos of a number reported a position")
	}
}

// TestScalarPosPlainLines covers a plain scalar over several lines, folded
// back to them: a character on a later line sits at its own line and
// column, white space that ends or starts a line is dropped, empty lines
// fold to line feeds, a character the folding wrote sits right after the
// line it ends, and text that does not fold to the value fails.
func TestScalarPosPlainLines(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		value     string
		off       int
		line, col int32 // line 0: no position
	}{
		{"second line", "a: x ==\n  y\n", "x == y", 5, 2, 3},
		{"first line", "a: x ==\n  y\n", "x == y", 2, 1, 6},
		{"third line after CR LF", "a: x\r\n   y\r\n   z\r\n", "x y z", 4, 3, 4},
		{"trailing and leading white space", "a: x  \n \t y\n", "x y", 2, 2, 4},
		{"empty lines fold to line feeds", "a: x\n\n  \n  y\n", "x\n\ny", 3, 4, 3},
		{"the folding's character", "a: x ==\n  y\n", "x == y", 4, 1, 8},
		{"end of the value", "a: x ==\n  y\n", "x == y", 6, 2, 4},
		{"text that does not fold to the value", "a: x ==\n  z\n", "x == y", 5, 0, 0},
		{"value past the text", "a: x ==\n", "x == y", 5, 0, 0},
		{"comment between the lines", "a: x\n  # c\n  y\n", "x y", 2, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := &tree.Node{Kind: tree.KindString, Style: tree.StylePlain, Pos: tree.Pos{File: 2, Line: 1, Column: 4}, Text: tc.value}
			pos, ok := scalarPos(n, tc.off, []byte(tc.text))
			switch {
			case tc.line == 0 && ok:
				t.Errorf("scalarPos(%q, %d) = %+v, want no position", tc.text, tc.off, pos)
			case tc.line != 0 && (!ok || pos != tree.Pos{File: 2, Line: tc.line, Column: tc.col}):
				t.Errorf("scalarPos(%q, %d) = %+v, %v; want %d:%d", tc.text, tc.off, pos, ok, tc.line, tc.col)
			}
		})
	}
}
