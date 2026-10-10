// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml/token"
)

// TestDocumentMarkers covers the document markers goccy's scanner reads
// where the document cut does not (01 req 8, 9, 10; 11 req 17, 26; blocker
// of the seventh WP-33 review). goccy ends a document at "..." in column 1
// whatever follows it, and reads a node after a real "..." marker as the
// start of the next document, where YAML 1.2 allows only a comment. Such a
// segment held tokens after its document end, and goccy's whole parse took
// it, block collections included: wrong trees ("a:\n-\nb: 1\n...x: 1"
// nested b under the empty entry, 30 such pairs to depth 60) and quadratic
// time. Each is RZ-CFG-001 now, at the default threshold, with the split
// threshold raised and with goccy's own parse, after any other finding
// too.
func TestDocumentMarkers(t *testing.T) {
	var pairs strings.Builder
	for i := range 30 {
		pairs.WriteString("k" + strconv.Itoa(i) + ":\n-\n")
	}
	tests := []struct{ src, want, msg string }{
		{"a: 1\nb: 2\n...x: 1\n", "RZ-CFG-001@3:1", msgMarker},
		{"a:\n-\nb: 1\n...x: 1\n", "RZ-CFG-001@4:1", msgMarker},
		{pairs.String() + "...x: 1\n", "RZ-CFG-001@61:1", msgMarker},
		{"a: 1\n...#\n", "RZ-CFG-001@2:1", msgMarker},
		{"a: 1\n...#c\nb: 1\n", "RZ-CFG-001@2:1", msgMarker},
		{"...!!map\nb: 1\n", "RZ-CFG-001@1:1", msgMarker},
		{"...#\na:\n-\nb: 1\n", "RZ-CFG-001@1:1", msgMarker},
		{"a: [1,\n...x]\n", "RZ-CFG-001@2:1", msgMarker},
		{"a: 1\n... {}\n", "RZ-CFG-001@2:5", msgAfterMarker},
		{"a: 1\n... ? x\n", "RZ-CFG-001@2:5", msgAfterMarker},
		{"a: 1\n... x: 1\n", "RZ-CFG-001@2:5", msgAfterMarker},
		{"a: 1\n... invalid\n", "RZ-CFG-001@2:5", msgAfterMarker},
		{"a: 1\n... !!str\n", "RZ-CFG-001@2:5", msgAfterMarker},
		{"a: 1\n...\t[]\n", "RZ-CFG-001@2:5", msgAfterMarker},
		// After an RZ-CFG-003 or RZ-CFG-004 the checks still stop the
		// file: they bound the cost of the parse.
		{"a: &x 1\n...x: 1\n", "RZ-CFG-003@1:4 RZ-CFG-001@2:1", ""},
		{"a: !e 1\n... b\n", "RZ-CFG-004@1:4 RZ-CFG-001@2:5", ""},
		// Valid: a marker followed by white space, a comment or nothing,
		// and a line of four dots, which goccy does not read as a marker.
		{"a: 1\n... # c\n", "{a:i:1}", ""},
		{"a: 1\n...\nb: 2\n", "{a:i:1} {b:i:2}", ""},
		{"a: 1\n...\t\n---\nb: 2\n", "{a:i:1} {b:i:2}", ""},
		{"a: \"x\n  ...y\"\n", `{a:s:"x ...y"}`, ""},
		{"a: |\n  ...x\n", `{a:s:"...x\n"}`, ""},
	}
	for _, tt := range tests {
		for _, splitAt := range []int{-1, 0, 3} {
			docs, diags, err := parse(t.Context(), []byte(tt.src), 0, "f.yaml", FormatYAML, Options{splitAt: splitAt}, true)
			if err != nil {
				t.Fatal(err)
			}
			got := codes(diags)
			if got == "" {
				var trees []string
				for _, d := range docs {
					trees = append(trees, show(d.Root, false))
				}
				got = strings.Join(trees, " ")
			}
			if got != tt.want {
				t.Errorf("%q, split at %d: %s, want %s", tt.src, splitAt, got, tt.want)
			}
			if tt.msg != "" && (len(diags) != 1 || diags[0].Message != tt.msg) {
				t.Errorf("%q, split at %d: messages %+v, want %q", tt.src, splitAt, diags, tt.msg)
			}
		}
	}
}

// TestMarkerTokensOnMarkerLines checks that goccy's DocumentHeader and
// DocumentEnd tokens of every accepted stream lie on its segments' own
// marker lines (01 req 9: documents are split at DocumentHeaderType and
// DocumentEndType; seventh WP-33 review): a "---" token on a "---" line
// before the segment's body, a "..." token on its last line, a "..." line,
// and nothing but comments after it. Streams that break this are refused
// (TestDocumentMarkers), so every parsed segment holds one document.
func TestMarkerTokensOnMarkerLines(t *testing.T) {
	inputs := append(suiteInputs(t), "a: 1\n... # c\n---\nb: 2\n...\n", "%YAML 1.2\n--- # c\na: 1\n...\n", "a: 1\n...x: 1\n")
	checked := 0
	for _, in := range inputs {
		docs, diags := parseWith(t, in, 0)
		if diags != "" {
			continue
		}
		if err := markersAligned(in); err != nil {
			t.Errorf("%q (%d documents): %v", in, len(docs), err)
		}
		checked++
	}
	if checked < 200 {
		t.Fatalf("checked %d streams", checked)
	}
	if markersAligned("a: 1\n...x: 1\n") == nil || markersAligned("a: 1\n... b\n") == nil {
		t.Error("misplaced markers pass the check")
	}
}

// markersAligned reports a DocumentHeader or DocumentEnd token of text
// that is not on its segment's own marker line.
func markersAligned(text string) error {
	pre := &fileParser{opts: Options{}.withDefaults()}
	text, ok := pre.precheckYAML([]byte(text))
	if !ok {
		return nil
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	for _, seg := range splitDocuments(text) {
		tks, _ := scanString(seg.text)
		lines := strings.Split(strings.TrimSuffix(seg.text, "\n"), "\n")
		body, ended := false, false
		directive := -1
		for _, tk := range tks {
			line := ""
			if l := tk.Position.Line; l >= 1 && l <= len(lines) {
				line = lines[l-1]
			}
			switch {
			case tk.Type == token.CommentType, tk.Position.Line == directive:
			case ended:
				return fmt.Errorf("%s after the document end, line %d of the segment at %d", tk.Type, tk.Position.Line, seg.startLine)
			case tk.Type == token.DocumentHeaderType:
				if body || !markerLine(line, "---") {
					return fmt.Errorf("\"---\" token on line %q of the segment at %d", line, seg.startLine)
				}
			case tk.Type == token.DocumentEndType:
				if tk.Position.Line != len(lines) || !markerLine(line, "...") {
					return fmt.Errorf("\"...\" token on line %q of the segment at %d", line, seg.startLine)
				}
				ended = true
			case tk.Type == token.DirectiveType:
				directive = tk.Position.Line
			default:
				body = true
			}
		}
	}
	return nil
}

// TestRefuseShape covers the parse of a document with block collections
// whose tokens do not hold one document body (01 req 9, 10; 11 req 17, 26;
// seventh WP-33 review): it never hands goccy's parser the whole document,
// which parses a block mapping in quadratic time, but refuses it at the
// offending token, with goccy's message for a directive out of place.
// The token pass refuses tokens after a document end first; for those, it
// runs here over the body alone, so the shape reaches the parse.
func TestRefuseShape(t *testing.T) {
	var wide strings.Builder
	for i := range 40000 {
		wide.WriteString("k" + strconv.Itoa(i) + ": v\n")
	}
	tests := []struct{ src, want, msg string }{
		{wide.String() + "...x: 1\n", "RZ-CFG-001@40001:4", msgAfterMarker},
		{"a: 1\n... x: 1\n", "RZ-CFG-001@2:5", msgAfterMarker},
		{"a: 1\n...\n...\n", "RZ-CFG-001@3:1", msgAfterMarker},
		{"a: 1\n--- \nb: 1\n", "RZ-CFG-001@2:1", `syntax error: a document marker ("---") inside a document`},
		{"a: 1\n%x\nb: 2\n", "RZ-CFG-001@2:1", "syntax error: unexpected directive value. document not started"},
		{"--- %0\n-\n", "RZ-CFG-001@1:5", "syntax error: unexpected directive value. document not started"},
		{"%FOO bar\na: 1\n", "RZ-CFG-001@1:1", "syntax error: unexpected directive value. document not started"},
		{"# c\n%FOO bar\na: 1\n", "RZ-CFG-001@2:1", "syntax error: unexpected directive value. document not started"},
	}
	for _, tt := range tests {
		tks, err := scanString(tt.src)
		if err != nil {
			t.Fatal(err)
		}
		p := &fileParser{path: "f.yaml", opts: Options{}.withDefaults()}
		pass := &tokenPass{p: p, maxDepth: 64, maxTokens: 1 << 20, splitAt: 1}
		cols := columnsOf(tks, tt.src)
		if pass.run(tks, 0, cols, tt.src); len(p.diags) != 0 {
			// The body: up to its document end.
			end := slices.IndexFunc(tks, func(tk *token.Token) bool { return tk.Type == token.DocumentEndType })
			p.diags, pass.errs, pass.fatal = nil, 0, false
			pass.run(tks[:end], 0, cols, tt.src)
		}
		if len(pass.splits) == 0 || len(p.diags) != 0 {
			t.Fatalf("%q: no block frames, or %q", tt.src[:min(len(tt.src), 40)], codes(p.diags))
		}
		before := allocBytes()
		p.parseSegment(tks, segment{text: tt.src, startLine: 1}, pass.splits, cols, pass.fixes)
		if got := codes(p.diags); got != tt.want || p.diags[0].Message != tt.msg || len(p.docs) != 0 {
			t.Errorf("%q: %s %+v, want %s %q", tt.src[:min(len(tt.src), 40)], got, p.diags, tt.want, tt.msg)
		}
		// goccy's whole parse of the wide mapping allocated 6 GiB.
		if alloc := allocBytes() - before; alloc > 64<<20 {
			t.Errorf("%q: allocated %d MiB", tt.src[:min(len(tt.src), 40)], alloc>>20)
		}
	}
	// A stream with no body has no block frame, and bodyRange says so.
	if lo, _, _, bad := bodyRange(token.Tokens{}); lo != -1 || bad != -1 {
		t.Errorf("empty tokens: lo %d, bad %d", lo, bad)
	}
	if _, _, err := Parse(context.Background(), []byte("a: 1\n...x: 1\n"), 0, "f", FormatYAML, Options{}); err != nil {
		t.Fatal(err)
	}
}
