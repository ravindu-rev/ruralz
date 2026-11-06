// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// The profile is checked against yaml.v3 (go.yaml.in/yaml/v3, a port of
// libyaml), which only tests import (ADR-0003): on every input both
// accept or both refuse, and where both accept they read the same trees,
// scalar texts, styles and positions, unless the input belongs to one of
// the deviation classes below, where libyaml reads YAML otherwise than
// YAML 1.2 and the profile follows YAML 1.2, or the profile restricts
// YAML on purpose.

// verdict is how the profile read an input, for the comparison.
type verdict uint8

const (
	// accepted: no finding.
	accepted verdict = iota
	// refused: a YAML syntax error.
	refused
	// valueFindings: only findings about values (RZ-CFG-002, RZ-CFG-005,
	// a value that does not match its tag); the YAML is valid and the
	// documents are left out, so only acceptance is compared.
	valueFindings
	// restricted: a profile finding (RZ-CFG-003, RZ-CFG-004, a key that is
	// not a scalar, a %YAML version), a limit or a byte pre-check, which
	// yaml.v3 does not share, so nothing is compared.
	restricted
)

// verdictOf classifies the profile's findings and returns the first syntax
// error among them.
func verdictOf(diags diag.List) (verdict, diag.Diagnostic) {
	v, first := accepted, diag.Diagnostic{}
	for _, d := range diags {
		switch {
		case d.Code == codeAnchor, d.Code == codeTag,
			d.Message == msgComplexKey, strings.Contains(d.Message, "directive: only YAML 1.2 is accepted"),
			strings.HasPrefix(d.Message, "nesting depth exceeds"), strings.HasSuffix(d.Message, " nodes"),
			strings.HasSuffix(d.Message, " nodes in its documents"), strings.HasPrefix(d.Message, "file is"),
			strings.HasPrefix(d.Message, "character U+"), strings.Contains(d.Message, "byte order mark"):
			return restricted, d
		case d.Code == codeDuplicate, d.Code == codeSchema,
			d.Message == msgNoValue, strings.Contains(d.Message, " does not match tag "):
			if v == accepted {
				v = valueFindings
			}
		case v != refused:
			// Every other RZ-CFG-001 is a YAML syntax error, a tag chain
			// included.
			v, first = refused, d
		}
	}
	return v, first
}

// v3Documents decodes data with yaml.v3 into one node per document, or
// returns its error; a panic in yaml.v3 is an error too.
func v3Documents(data []byte) (docs []*yaml.Node, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("yaml.v3 panicked: %v", r)
		}
	}()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			if errors.Is(err, io.EOF) {
				return docs, nil
			}
			return docs, err
		}
		docs = append(docs, &n)
	}
}

// diffYAML parses data with the profile and with yaml.v3 and describes how
// they differ, or returns "" when they agree or the profile restricts the
// input.
func diffYAML(data []byte) string {
	ours, diags, err := parse(context.Background(), data, 0, "f.yaml", FormatYAML, Options{}, true)
	if err != nil {
		return "profile error: " + err.Error()
	}
	v, syntax := verdictOf(diags)
	if v == restricted {
		return ""
	}
	theirs, terr := v3Documents(data)
	switch {
	case terr != nil && v != refused:
		return "yaml.v3 refuses (" + terr.Error() + "); the profile accepts"
	case terr == nil && v == refused:
		return "yaml.v3 accepts; the profile refuses: " + codes(diag.List{syntax}) + " " + syntax.Message
	case terr != nil, v != accepted:
		return ""
	}
	var roots []*yaml.Node
	for _, d := range theirs {
		if d.Kind == yaml.DocumentNode && len(d.Content) == 1 && !isNullNode(d.Content[0]) {
			roots = append(roots, d.Content[0])
		}
	}
	if len(roots) != len(ours) {
		return fmt.Sprintf("yaml.v3 reads %d documents, the profile %d", len(roots), len(ours))
	}
	for i, r := range roots {
		if m := compareNode(ours[i].Root, r, fmt.Sprintf("document %d", i)); m != "" {
			return m
		}
	}
	return ""
}

// isNullNode reports whether n is a null scalar, which the profile reads as
// an empty document.
func isNullNode(n *yaml.Node) bool {
	if n.Kind != yaml.ScalarNode {
		return false
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return n.Tag == tagNull
	}
	return n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) == 0 && classify(n.Value) == classNull
}

// compareNode compares the profile's node with yaml.v3's at path.
func compareNode(ours *tree.Node, theirs *yaml.Node, path string) string {
	switch theirs.Kind {
	case yaml.MappingNode:
		if ours.Kind != tree.KindMap || len(ours.Members) != len(theirs.Content)/2 {
			return fmt.Sprintf("%s: yaml.v3 reads a mapping of %d entries, the profile %s", path, len(theirs.Content)/2, show(ours, false))
		}
		if m := comparePos(ours.Pos, theirs, path); m != "" {
			return m
		}
		for i, m := range ours.Members {
			k, val := theirs.Content[2*i], theirs.Content[2*i+1]
			if k.Kind != yaml.ScalarNode || k.Value != m.Key {
				return fmt.Sprintf("%s: yaml.v3 reads key %d as %q, the profile %q", path, i, k.Value, m.Key)
			}
			if k.Value != "" {
				if msg := comparePos(m.KeyPos, k, path+" key "+quoteKey(m.Key)); msg != "" {
					return msg
				}
			}
			if msg := compareNode(m.Value, val, path+"."+m.Key); msg != "" {
				return msg
			}
		}
		return ""
	case yaml.SequenceNode:
		if ours.Kind != tree.KindList || len(ours.Items) != len(theirs.Content) {
			return fmt.Sprintf("%s: yaml.v3 reads a sequence of %d items, the profile %s", path, len(theirs.Content), show(ours, false))
		}
		if m := comparePos(ours.Pos, theirs, path); m != "" {
			return m
		}
		for i, it := range ours.Items {
			if m := compareNode(it, theirs.Content[i], fmt.Sprintf("%s[%d]", path, i)); m != "" {
				return m
			}
		}
		return ""
	case yaml.ScalarNode:
		return compareScalar(ours, theirs, path)
	default:
		return fmt.Sprintf("%s: yaml.v3 reads node kind %d", path, theirs.Kind)
	}
}

// compareScalar compares scalars: yaml.v3's text typed as the profile types
// it (the core schema, or the explicit core tag), its style, and its
// position unless it is empty, where the two place it differently.
func compareScalar(ours *tree.Node, theirs *yaml.Node, path string) string {
	style := tree.StylePlain
	switch {
	case theirs.Style&yaml.DoubleQuotedStyle != 0:
		style = tree.StyleDoubleQuoted
	case theirs.Style&yaml.SingleQuotedStyle != 0:
		style = tree.StyleSingleQuoted
	case theirs.Style&yaml.LiteralStyle != 0:
		style = tree.StyleLiteral
	case theirs.Style&yaml.FoldedStyle != 0:
		style = tree.StyleFolded
	}
	var want typed
	switch {
	case theirs.Style&yaml.TaggedStyle != 0:
		want = resolveTagged(theirs.Tag, theirs.Value)
	case style != tree.StylePlain:
		want = typed{kind: tree.KindString, text: theirs.Value}
	default:
		want = resolvePlain(theirs.Value)
	}
	got := typed{kind: ours.Kind, text: ours.Text, b: ours.Bool}
	if want.code != "" || got != want || (ours.Kind != tree.KindNull && ours.Style != style) {
		return fmt.Sprintf("%s: yaml.v3 reads %q (tag %s, style %d), the profile %s (style %d)", path, theirs.Value, theirs.Tag, style, show(ours, false), ours.Style)
	}
	if theirs.Value == "" && style == tree.StylePlain {
		return ""
	}
	return comparePos(ours.Pos, theirs, path)
}

// comparePos compares a position with yaml.v3's line and column.
func comparePos(ours tree.Pos, theirs *yaml.Node, path string) string {
	if int(ours.Line) != theirs.Line || int(ours.Column) != theirs.Column {
		return fmt.Sprintf("%s: yaml.v3 places it at %d:%d, the profile at %d:%d", path, theirs.Line, theirs.Column, ours.Line, ours.Column)
	}
	return ""
}

// TestYAMLv3Differential compares the profile with yaml.v3 over the YAML
// Test Suite, the fuzz seeds and generated streams of YAML indicators,
// scalars, tags, flow collections and comments (ADR-0003): every
// difference belongs to a deviation class.
func TestYAMLv3Differential(t *testing.T) {
	inputs := suiteInputs(t)
	for _, s := range hostileSeeds() {
		if len(s) <= 4<<10 {
			inputs = append(inputs, s)
		}
	}
	n := 20000
	if testing.Short() || raceEnabled {
		n = 2000
	}
	inputs = append(inputs, generatedStreams(n)...)
	classes := map[string]int{}
	for _, in := range inputs {
		m := diffYAML([]byte(in))
		if m == "" {
			continue
		}
		c := deviationOf(in, m)
		if c == "" {
			t.Errorf("%q: %s", in, m)
			continue
		}
		classes[c]++
	}
	t.Logf("%d inputs; deviations by class: %v", len(inputs), classes)
}

// generatedStreams returns n streams built from YAML atoms by a fixed
// generator, so every run sees the same inputs.
func generatedStreams(n int) []string {
	atoms := []string{
		"? ", "- ", ": ", "a", "\"b\"", "'c'", "!!str ", "!!map ", "!!seq ", "!!int 1", "|", ">",
		"[x]", "{k: v}", "[", "]", ",", "k: ", "", " ", "# c", "\n", "\t",
		"-b #c", "- z #c", "!!str c: 1", "k:", "\"q\": 1", "|-", ">+", "...", "---", "#",
		"a:b", ":", "?", "{", "}", "1.5", "~", "''", "\"\\t\"",
	}
	g := splitMix(0x5eed)
	out := make([]string, 0, n)
	var b strings.Builder
	for range n {
		b.Reset()
		if g.below(2) == 0 {
			b.WriteString("k: ")
		}
		for range 1 + g.below(6) {
			for range 1 + g.below(4) {
				b.WriteString(atoms[g.below(len(atoms))])
			}
			b.WriteByte('\n')
			b.WriteString(strings.Repeat(" ", g.below(7)))
		}
		out = append(out, b.String())
	}
	return out
}

// splitMix is the SplitMix64 generator: a deterministic source of test
// inputs, not of anything secret.
type splitMix uint64

// below returns a number in [0, n).
func (g *splitMix) below(n int) int {
	*g += 0x9e3779b97f4a7c15
	z := uint64(*g)
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb
	z ^= z >> 31
	return int(z % uint64(n)) //nolint:gosec // G115: n is a small positive int, so the remainder fits
}

// FuzzYAMLv3Differential is the differential target of ADR-0003: the
// profile and yaml.v3 agree on every input of up to 4 KiB outside the
// deviation classes.
func FuzzYAMLv3Differential(f *testing.F) {
	for _, s := range suiteInputs(f) {
		f.Add([]byte(s))
	}
	for _, s := range hostileSeeds() {
		if len(s) <= 4<<10 {
			f.Add([]byte(s))
		}
	}
	for _, c := range deviationClasses() {
		for _, w := range c.witnesses {
			f.Add([]byte(w))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4<<10 {
			return
		}
		if m := diffYAML(data); m != "" && deviationOf(string(data), m) == "" {
			t.Fatalf("%q: %s", data, m)
		}
	})
}

// deviation is a class of inputs on which yaml.v3 and the profile differ
// by design: libyaml reads them otherwise than YAML 1.2, which the profile
// follows.
type deviation struct {
	name string
	// why says how each side reads the class, and which YAML Test Suite
	// cases show YAML 1.2's reading.
	why string
	// matches reports whether src, on which the two differ as diff says
	// (diffYAML), holds the class's shape.
	matches func(src, diff string) bool
	// witnesses are inputs of the class on which the two differ.
	witnesses []string
}

// Prefixes of diffYAML's descriptions.
const (
	v3Refuses = "yaml.v3 refuses"
	v3Accepts = "yaml.v3 accepts"
)

// deviationClasses is the checked-in list of deviation classes
// (ADR-0003).
func deviationClasses() []deviation {
	re := regexp.MustCompile
	tabIndent := re(`(?m)^[ ]*\t|[-?:][ ]*\t`)
	colonLine := re(`(?m)(^|[\[{,]|-[ \t])[ \t]*:([ \t,\]}]|$)`)
	flowIndicator := re(`(^|[\s\[{,])[?:][^\s,\[\]{}]|:[?:]|[^\s,\[\]{}]\s*\?|:[,\[\]{}]|(^|[\s\[{,:])[-?][,\[\]{}]`)
	commentAfterToken := re(`["'\[\]{},:]#|[|>][+\-1-9]{0,2}#`)
	topBlockScalar := re(`(?m)^[ \t]*(---[ \t]+)?(![^\s]*[ \t]+)?([|>][^\n]*\n([ \t]*\n)*[^\s]|[|>][+-]?[1-9])`)
	directive := re(`(?m)^%`)
	endMarker := re(`(?m)^\.\.\.([ \t]|$)`)
	escape := re(`\\[/']|\\u[dD][89abAB]`)
	tagFlowChar := re(`![!\w%.~-]*[,\[\]{}]`)
	return []deviation{
		{
			name: "no-final-break",
			why: "The YAML Test Suite reads a stream that does not end with a line break as if it did " +
				"(JEF9/02, L24T/01), and so does the profile; libyaml ends a block scalar's last line there " +
				"without its line break",
			matches: func(src, _ string) bool {
				return src != "" && !strings.HasSuffix(src, "\n") && !strings.HasSuffix(src, "\r") && diffYAML([]byte(src+"\n")) == ""
			},
			witnesses: []string{"- |+\n   ", "a: |\n  x\n   "},
		},
		{
			name: "unicode-breaks",
			why: "libyaml reads U+0085, U+2028 and U+2029 as line breaks, as YAML 1.1 defined them, while " +
				"YAML 1.2 reads them as ordinary characters (YAML 1.2.2 section 5.4)",
			matches:   func(src, _ string) bool { return strings.ContainsAny(src, "\u0085  ") },
			witnesses: []string{"a: 0\u0085\n", "a: b c\n"},
		},
		{
			name: "quoted-indent",
			why: "libyaml does not check the indentation of a quoted scalar's later lines, which YAML 1.2 " +
				"indents past the block collection holding the scalar, with spaces (YAML Test Suite DK95/01, QB6E)",
			matches: func(_, diff string) bool {
				return strings.HasPrefix(diff, v3Accepts) && strings.HasSuffix(diff, msgQuotedIndent)
			},
			witnesses: []string{"quoted: \"a\nb\"\n", "foo: \"bar\n\tbaz\"\n"},
		},
		{
			name: "flow-indent",
			why: "libyaml does not check the indentation of a flow collection's lines, which YAML 1.2 " +
				"indents past the block collection holding it, its closing bracket included " +
				"(YAML Test Suite 9C9N, Y79Y/003)",
			matches: func(_, diff string) bool {
				return strings.HasPrefix(diff, v3Accepts) && (strings.HasSuffix(diff, msgFlowIndent) || strings.HasSuffix(diff, msgFlowClose))
			},
			witnesses: []string{"hosts: [\n  a\n]\n", "flow: [a,\nb]\n"},
		},
		{
			name: "block-scalar-comment-line",
			why: "After a block scalar's leading empty lines libyaml reads a line that starts with '#' as a " +
				"comment, while YAML 1.2 reads a line indented past the collection as content, so the empty " +
				"lines before it may not hold more spaces (YAML Test Suite S98Z)",
			matches: func(_, diff string) bool {
				return strings.HasPrefix(diff, v3Accepts) && strings.HasSuffix(diff, msgBlockLeadingSpaces)
			},
			witnesses: []string{"empty block scalar: >\n \n  \n   \n # comment\n"},
		},
		{
			name: "comment-space",
			why: "libyaml starts a comment at a '#' right after a token, where YAML 1.2 needs white space " +
				"before it: after a quoted scalar, a flow collection or a ',', and in a block scalar header " +
				"(YAML Test Suite 9JBA, CVW2, SU5Z, X4QW)",
			matches: func(src, diff string) bool {
				return strings.HasPrefix(diff, v3Accepts) && commentAfterToken.MatchString(src)
			},
			witnesses: []string{"a: \"x\"#c\n", "[a, b]#c\n", "[#\n]\n", "k: |#c\n  x\n"},
		},
		{
			name: "escapes",
			why: "libyaml refuses the escape \\/ and accepts \\', which YAML 1.2 defines the other way round, " +
				"and refuses a surrogate pair written as two \\u escapes, which the profile joins as JSON does " +
				"(YAML Test Suite 3UYS, HRE5)",
			matches:   func(src, _ string) bool { return escape.MatchString(src) },
			witnesses: []string{"a: \"\\/\"\n", "a: \"\\'\"\n", "a: \"\\uD83D\\uDE00\"\n"},
		},
		{
			name: "directives",
			why: "libyaml accepts only %YAML 1.1 and %TAG, refusing %YAML 1.2 and the reserved directives, " +
				"which YAML 1.2 ignores, as the profile does; the profile accepts %YAML 1.2 only " +
				"(YAML Test Suite 27NA, 2LFX, 6LVF, 6ZKB, 9DXL, MUS6/05, MUS6/06, RTP8, UT92)",
			matches:   func(src, _ string) bool { return directive.MatchString(src) },
			witnesses: []string{"%YAML 1.2\n---\na: 1\n", "%FOO bar\n---\na: 1\n"},
		},
		{
			name: "document-markers",
			why: "libyaml refuses a \"...\" with no document before it and requires \"---\" before every " +
				"document but the first, while YAML 1.2 allows both (YAML Test Suite 7Z25, HWV9, QT73)",
			matches: func(src, diff string) bool {
				return strings.HasPrefix(diff, v3Refuses) && endMarker.MatchString(src)
			},
			witnesses: []string{"...\n", "a\n...\nb: 1\n"},
		},
		{
			name: "tag-characters",
			why: "libyaml reads a flow indicator after a tag's handle as part of the tag (\"!!str,\"), which " +
				"YAML 1.2 ends there and then refuses for want of white space, or, in a flow collection, " +
				"reads as an empty tagged node before the ',' (YAML Test Suite U99R, WZ62)",
			matches:   func(src, _ string) bool { return tagFlowChar.MatchString(src) },
			witnesses: []string{"- !!str, x\n", "{\n  foo : !!str,\n  !!str : bar,\n}\n"},
		},
		{
			name: "top-level-block-scalar",
			why: "libyaml reads the content of a block scalar at the document level only from the second " +
				"column, and an indentation indicator there as that column, while YAML 1.2 indents the " +
				"content past the document's -1: a line at column 1 is content, and \"|1\" starts the " +
				"content at column 1 (YAML Test Suite DK3J, FP8R, M7A3, W4TN)",
			matches:   func(src, _ string) bool { return topBlockScalar.MatchString(src) },
			witnesses: []string{"--- |\nline\n", "--- >\nline1\n# no comment\nline3\n", "|\n%x\n", "!!str\n  |\nk: v\n", ">1\n 0\n"},
		},
		{
			name: "block-scalar-header-column",
			why: "libyaml takes a block scalar header on the line after its key, '-' or '?' at any column, " +
				"where YAML 1.2 indents a node on a later line past its entry's collection",
			matches: func(src, diff string) bool {
				return strings.HasPrefix(diff, v3Accepts) && headerAtParentColumn(src)
			},
			witnesses: []string{"k:\n|\n x\n", "?\n>\n a\n", "k: !!map\n>+\n a\n", "- -\n  |\n"},
		},
		{
			name: "flow-pair-empty-explicit-key",
			why: "In a flow sequence libyaml drops the token after the '?' of an explicit key with nothing in " +
				"it, as if that token were the key: the ':' before the pair's value, the ',' after the pair or " +
				"the closing ']'. It so refuses [? : x], [? , x] and [ ? ], which YAML 1.2 reads as a pair with " +
				"an empty key (YAML 1.2.2 section 7.4.2, rules 143, 146 and 150), reads [? , : x] as one pair " +
				"where YAML 1.2 reads two, and accepts [? : : x], [? , , x] and [? ]], which YAML 1.2 refuses: " +
				"after the pair comes a ':' that starts no node, an empty entry or a ']' that closes nothing " +
				"(rules 126, 137 and 138). In a flow mapping libyaml keeps the token ({? : x}). With \"\" written " +
				"as each such key, which libyaml reads as YAML 1.2 does, the two agree or differ as another " +
				"class says",
			matches: func(src, _ string) bool {
				filled, ok := fillEmptyFlowKeys(src)
				if !ok {
					return false
				}
				d := diffYAML([]byte(filled))
				return d == "" || deviationOf(filled, d) != ""
			},
			witnesses: []string{"[? : x]\n", "[? , x]\n", "[ ? ]\n", "[? , : x]\n", "[? : : x]\n", "[? , , x]\n", "[? ]]\n", "- [? #c\n  : : ]\n"},
		},
		{
			name: "colon-line",
			why: "libyaml refuses a ':' value indicator with no key before it on its line: an entry with an " +
				"empty key at the start of a line, after '- ', '[', '{' or ',', and a flow mapping key " +
				"whose ':' is on a later line, which YAML 1.2 allows; the profile reads the empty key as \"\" " +
				"(YAML Test Suite 2JQS, 4MUZ, CFD4, FRK4, NHX8, NKF9, S3PD, SM9W/01, UKK6/00, VJP3/01)",
			matches: func(src, diff string) bool {
				return strings.HasPrefix(diff, v3Refuses) && colonLine.MatchString(src)
			},
			witnesses: []string{": a\n", "- :\n", "a: [: b]\n", "a: {\"x\"\n  : y}\n"},
		},
		{
			name: "flow-key-lines",
			why: "libyaml keeps a flow mapping's key on one line, while YAML 1.2 lets a flow mapping's key " +
				"span lines (YAML Test Suite 9SA2, NJ66)",
			matches: func(src, diff string) bool {
				return strings.Contains(diff, "did not find expected ',' or '}'") && strings.Contains(src, "{")
			},
			witnesses: []string{"- { \"multi\n  line\": value}\n", "- { multi\n  line: value}\n"},
		},
		{
			name: "tab-indentation",
			why: "libyaml refuses a tab where it looks for indentation: at the start of a line in block " +
				"context, after a block indicator ('-', '?', ':') and in a block scalar's indentation, where " +
				"YAML 1.2 allows a tab as separation (YAML Test Suite 6BCT, 6CA3, 96NN, A2M4, DK95/00, " +
				"DK95/03, DK95/04, DK95/07, Q5MG, R4YG, Y79Y/001, Y79Y/010)",
			matches: func(src, diff string) bool {
				return strings.HasPrefix(diff, v3Refuses) && tabIndent.MatchString(src)
			},
			witnesses: []string{"foo:\n\tbar\n", "foo: 1\n\t\nbar: 2\n", "k:\n-\t-1\n", "foo: |-\n \tbar\n"},
		},
		{
			name: "flow-indicators",
			why: "In a flow collection libyaml reads '?' and ':' as indicators wherever they start a token and " +
				"ends a plain scalar at '?', while YAML 1.2 starts and continues a plain scalar with them before " +
				"a character a plain scalar can hold ([?x], {x: :x}, {\"a\"::b}, {key: value?}); it keeps a ':' " +
				"before a flow indicator in the scalar ([a:, b], {0000 :}), where YAML 1.2 reads a value indicator; and " +
				"it reads '-' before a flow indicator ([-]) as the scalar \"-\", which YAML 1.2 refuses " +
				"(YAML Test Suite 4ABK, 58MP, 5MUD, 5T43, 652Z, DBG4, G5U8, HM87, JR7V, K3WX, YJV2)",
			matches: func(src, _ string) bool {
				return strings.ContainsAny(src, "[{") && flowIndicator.MatchString(src)
			},
			witnesses: []string{"[?x]\n", "{x: :x}\n", "- {\"a\"::b}\n", "- {key: value?}\n", "[a:, b]\n", "{0000 :}\n", "[-]\n"},
		},
	}
}

// headerAtParentColumn reports whether a line of src starts with a block
// scalar indicator at or left of the column of the innermost block
// collection of the line holding content before it.
func headerAtParentColumn(src string) bool {
	prev := -1
	for line := range strings.Lines(src) {
		line = strings.TrimRight(line, "\r\n")
		indent := len(line) - len(strings.TrimLeft(line, " "))
		rest := line[indent:]
		switch {
		case strings.TrimSpace(rest) == "" || rest[0] == '#':
			continue
		case (rest[0] == '|' || rest[0] == '>') && prev >= indent:
			return true
		}
		prev = innerColumn(line, indent)
	}
	return false
}

// innerColumn returns the column of the innermost block collection that a
// line starting with content at col holds: past each compact '-' or '?'
// indicator to the node after it, or that indicator when it ends the line.
func innerColumn(line string, col int) int {
	for col < len(line) && (line[col] == '-' || line[col] == '?') && (col+1 == len(line) || line[col+1] == ' ') {
		next := col + 1
		for next < len(line) && line[next] == ' ' {
			next++
		}
		if next == len(line) {
			return col
		}
		col = next
	}
	return col
}

// fillEmptyFlowKeys returns src with "" written as the key of each
// explicit key with nothing in it that starts a flow sequence entry
// (emptyFlowKeyEnds), and whether src holds one.
func fillEmptyFlowKeys(src string) (string, bool) {
	ends := emptyFlowKeyEnds(src)
	if len(ends) == 0 {
		return src, false
	}
	var b strings.Builder
	last := 0
	for _, e := range ends {
		b.WriteString(src[last:e])
		b.WriteString(`""`)
		last = e
	}
	b.WriteString(src[last:])
	return b.String(), true
}

// emptyFlowKeyEnds returns the offsets in src, a stream with LF line
// breaks, of the token after each explicit key with nothing in it that
// starts a flow sequence entry: a '?' followed by white space, then only
// white space, line breaks and comments before a ':' value indicator, a ','
// or the closing ']' (YAML 1.2.2 rules 138, 139 and 150). It reads src as
// YAML 1.2 does as far as finding them needs: a quoted scalar, and in block
// context a flow collection, starts only where a node can; comments, quoted
// scalars, properties and the lines of a block scalar, those indented more
// than its header's line, hold no flow collection; and a line indented more
// than the line a block plain scalar starts on continues the scalar.
func emptyFlowKeyEnds(src string) []int {
	n := len(src)
	blank := func(i int) bool { return i < 0 || i >= n || src[i] == ' ' || src[i] == '\t' || src[i] == '\n' }
	flowAt := func(i int) bool { return i < n && strings.IndexByte(",[]{}", src[i]) >= 0 }
	lineEnd := func(i int) int {
		if k := strings.IndexByte(src[i:], '\n'); k >= 0 {
			return i + k
		}
		return n
	}
	// sep returns the offset past the white space, line breaks and comments
	// at i.
	sep := func(i int) int {
		for i < n {
			switch {
			case blank(i):
				i++
			case src[i] == '#' && blank(i-1):
				i = lineEnd(i)
			default:
				return i
			}
		}
		return n
	}
	// quoted returns the offset past the quoted scalar at i, or n.
	quoted := func(i int) int {
		q := src[i]
		for j := i + 1; j < n; j++ {
			switch {
			case q == '"' && src[j] == '\\':
				j++
			case src[j] == q && q == '\'' && j+1 < n && src[j+1] == '\'':
				j++
			case src[j] == q:
				return j + 1
			}
		}
		return n
	}
	// property returns the offset past the tag, anchor or alias at i.
	property := func(i int, inFlow bool) int {
		i++
		for i < n && !blank(i) && (!inFlow || !flowAt(i)) {
			i++
		}
		return i
	}
	var ends []int
	// flow reads the flow collection whose bracket is at i and returns the
	// offset past its closing bracket, or n. A node can start after an
	// opening bracket, a ',', an indicator or a property, and an entry of a
	// flow sequence after its '[' or a ','; after a quoted scalar or a flow
	// collection a ':' is an indicator whatever follows it (rule 149).
	flow := func(i int) int {
		open := []byte{src[i]}
		node, entry, json := true, src[i] == '[', false
		for i++; i < n && len(open) > 0; {
			c := src[i]
			switch {
			case blank(i):
				i++
			case c == '#' && blank(i-1):
				i = lineEnd(i)
			case c == '[' || c == '{':
				open = append(open, c)
				node, entry, json = true, c == '[', false
				i++
			case c == ']' || c == '}':
				open = open[:len(open)-1]
				node, entry, json = false, false, true
				i++
			case c == ',':
				node, entry, json = true, open[len(open)-1] == '[', false
				i++
			case c == '?' && node && blank(i+1):
				j := sep(i + 1)
				empty := j < n && (src[j] == ',' || src[j] == ']' || (src[j] == ':' && (blank(j+1) || flowAt(j+1))))
				if entry && empty {
					ends = append(ends, j)
				}
				node, entry, json = true, false, false
				i++
			case c == ':' && (json || blank(i+1) || flowAt(i+1)):
				node, entry, json = true, false, false
				i++
			case (c == '"' || c == '\'') && node:
				i = quoted(i)
				node, entry, json = false, false, true
			case (c == '!' || c == '&' || c == '*') && node:
				i = property(i, true)
				node, entry, json = c != '*', false, false
			default:
				// A plain scalar's character, or one that starts no node.
				node, entry, json = false, false, false
				i++
			}
		}
		return i
	}
	// plain is the indentation of the line where the block plain scalar
	// before the cursor starts, or -1.
	plain := -1
	for i := 0; i < n; {
		e := lineEnd(i)
		indent := 0
		for i+indent < e && src[i+indent] == ' ' {
			indent++
		}
		j := i + indent
		for j < e && (src[j] == ' ' || src[j] == '\t') {
			j++
		}
		switch {
		case j == e:
			// An empty line neither ends nor starts a node.
			i = e + 1
			continue
		case src[j] == '#':
			plain, i = -1, e+1
			continue
		}
		node := plain < 0 || indent <= plain
		if node {
			plain = -1
			switch {
			case indent == 0 && (strings.HasPrefix(src[i:], "---") || strings.HasPrefix(src[i:], "...")) && blank(i+3):
				j = i + 3
			case indent == 0 && src[i] == '%':
				// A directive.
				i = e + 1
				continue
			}
		}
		for j < n && src[j] != '\n' {
			c := src[j]
			switch {
			case c == ' ' || c == '\t':
				j++
			case c == '#' && blank(j-1):
				j = lineEnd(j)
			case c == ':' && blank(j+1):
				node, plain = true, -1
				j++
			case !node:
				j++
			case (c == '-' || c == '?') && blank(j+1):
				j++
			case c == '[' || c == '{':
				j, node = flow(j), false
			case c == '"' || c == '\'':
				j, node = quoted(j), false
			case c == '|' || c == '>':
				// A block scalar: the lines after its header that are empty
				// or indented more than the header's line.
				for j = lineEnd(j); j < n; {
					k := j + 1
					s := k
					for s < n && src[s] == ' ' {
						s++
					}
					w := s
					for w < n && (src[w] == ' ' || src[w] == '\t') {
						w++
					}
					if w < n && src[w] != '\n' && s-k <= indent {
						break
					}
					j = lineEnd(k)
				}
			case c == '!' || c == '&' || c == '*':
				j, node = property(j, false), c != '*'
			default:
				node, plain = false, indent
				j++
			}
		}
		i = j + 1
	}
	return ends
}

// deviationOf returns the name of the first class src belongs to, given
// how the two differ on it, or "". The classes read src with every line
// break as LF, as both parsers do.
func deviationOf(src, diff string) string {
	src = strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	for _, c := range deviationClasses() {
		if c.matches(src, diff) {
			return c.name
		}
	}
	return ""
}

// TestDeviationWitnesses checks the deviation list: every class has
// witnesses, on each of which the two differ and the class matches, so a
// class that does not describe yaml.v3 fails here.
func TestDeviationWitnesses(t *testing.T) {
	for _, c := range deviationClasses() {
		if c.name == "" || c.why == "" || len(c.witnesses) == 0 {
			t.Errorf("class %q: name, reason and witnesses are required", c.name)
		}
		for _, w := range c.witnesses {
			diff := diffYAML([]byte(w))
			if diff == "" {
				t.Errorf("class %s: yaml.v3 and the profile agree on %q", c.name, w)
				continue
			}
			if !c.matches(w, diff) {
				t.Errorf("class %s does not match its witness %q (%s)", c.name, w, diff)
			}
		}
	}
}

// TestFillEmptyFlowKeys covers the shape of the flow-pair-empty-explicit-key
// class: "" goes before the ':', ',' or ']' after each explicit key with
// nothing in it that starts a flow sequence entry, whatever separation comes
// between, at any nesting and after any entry or block node; a key holding a
// node, a tag or a quoted scalar, a '?' that indicates no explicit key, a
// flow mapping's entry, block context, and text in a quoted scalar, a block
// scalar, a plain scalar or a comment stay as they are. A filled stream
// holds no such key.
func TestFillEmptyFlowKeys(t *testing.T) {
	tests := []struct{ src, want string }{
		// Keys libyaml reads otherwise.
		{"[? : x]\n", "[? \"\": x]\n"},
		{"[? :]\n", "[? \"\":]\n"},
		{"[? :, x]\n", "[? \"\":, x]\n"},
		{"[? , x]\n", "[? \"\", x]\n"},
		{"[ ? ]\n", "[ ? \"\"]\n"},
		{"[? : : ]", "[? \"\": : ]"},
		{"[?\t: x]\n", "[?\t\"\": x]\n"},
		{"k: [?\n  : x]\n", "k: [?\n  \"\": x]\n"},
		{"- [? # c\n  , x]\n", "- [? # c\n  \"\", x]\n"},
		{"[a, ? ]\n", "[a, ? \"\"]\n"},
		{"[[? ], ? , {k: [? : v]}]\n", "[[? \"\"], ? \"\", {k: [? \"\": v]}]\n"},
		{"[{a: b}, ? : x]\n", "[{a: b}, ? \"\": x]\n"},
		{"k: \"v\"\nl: [\"a\", ? : 'b''c', ? ]\n", "k: \"v\"\nl: [\"a\", ? \"\": 'b''c', ? \"\"]\n"},
		{"k: |\n  x\nl: [? ]\n", "k: |\n  x\nl: [? \"\"]\n"},
		{"k: a\nl: [? ]\n", "k: a\nl: [? \"\"]\n"},
		{"--- [? ]\n", "--- [? \"\"]\n"},
		{"!!seq [? ]\n", "!!seq [? \"\"]\n"},
		{"\"k\": [? ]\n", "\"k\": [? \"\"]\n"},
		// Left as they are.
		{"[? a : x]\n", ""},
		{"[? \"\" : x]\n", ""},
		{"[? !!str : x]\n", ""},
		{"[? :x]\n", ""},
		{"[? [a] : x]\n", ""},
		{"[?: x]\n", ""},
		{"[?]\n", ""},
		{"[a ? : x]\n", ""},
		{"[!!str ? : x]\n", ""},
		{"{? : x, ? , ? }\n", ""},
		{"[{? : x}]\n", ""},
		{"? : x\n", ""},
		{"- ? \n  : x\n", ""},
		{"k: \"[? : x]\"\n", ""},
		{"k: '[a'', ? ]'\n", ""},
		{"k: [\"a\\\", ? ]\"]\n", ""},
		{"k: |\n  [? : x]\n", ""},
		{"k: a [? : x]\n", ""},
		{"k: a\n  [? : x]\n", ""},
		{"# [? : x]\n", ""},
		{"k: [a #, ? ]\n  ]\n", ""},
	}
	for _, tt := range tests {
		want, wantOK := tt.want, tt.want != ""
		if !wantOK {
			want = tt.src
		}
		got, ok := fillEmptyFlowKeys(tt.src)
		if got != want || ok != wantOK {
			t.Errorf("fillEmptyFlowKeys(%q) = %q, %v, want %q, %v", tt.src, got, ok, want, wantOK)
			continue
		}
		if again, ok := fillEmptyFlowKeys(got); ok {
			t.Errorf("fillEmptyFlowKeys(%q) = %q, which still holds an empty key (%q)", tt.src, got, again)
		}
	}
}
