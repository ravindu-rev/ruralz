// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"runtime"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// largestPattern returns a pattern whose program measures exactly
// MaxPatternInstructions plus extra instructions: 16 repetitions of a class
// a thousand times (16,000 instructions, the 16 ranges of the classes 4
// more, and their 16 nodes) and a literal tail repeated to fill the rest
// (its node too), with programOverhead.
func largestPattern(tail rune, extra int) string {
	return strings.Repeat("[a-z]{1000}", 16) + fmt.Sprintf("%c{%d}", tail, MaxPatternInstructions-16004-17*nodeInstructions-programOverhead+extra)
}

// tailRune returns the i-th of eight distinct tail characters of
// largestPattern, from p on.
func tailRune(i int) rune { return rune("pqrstuvw"[i]) }

// TestProgramSizeBound checks the measure of a pattern's program: the
// instructions Go compiles a pattern into never exceed what the walk
// counts from its parsed syntax, for repetitions, alternations, captures,
// case folding, classes and empty matches; the count is the parser's own
// estimate, so it is exact for the plain repetitions that make programs
// large. The estimate alone, without the overhead and the nodes the
// measure adds, also bounds the program but for its fail, empty-group and
// match instructions, so an operator counted short shows on a large
// repetition of it, the star, plus, question mark and x{n,} included.
func TestProgramSizeBound(t *testing.T) {
	patterns := []string{
		`a`, `(?:)`, `^$`, `a*`, `(a*)*`, `(?:a?){0,5}`, `x{2,5}`, `x{3,}`, `x{0}`, `x{1}`, `(?i)abc`, `\pL{3}`,
		`[^\x00-\x{10FFFF}]`, `(a|b|c)+?`, `((a{2}){3}){4}`, `(?s).*`, `(?m)^a$`, `a|`, `(a|ab)(c|bcd)(d*)`,
		`(?U)a+b*`, `\b\B\A\z`, `[[:alpha:]]{10,20}`, `(?:(?:a|b){2,}c){2,4}`, largestPattern('x', 0),
		`(?:x?){1000}`, `(?:x*){500}`, `(?:(?:x?)*){500}`, `(?:x+){500}`, `(?:ab){400,}`, `(?:x{0,3}){200}`,
	}
	for _, tc := range costPatterns() {
		patterns = append(patterns, tc.pattern)
	}
	for _, expr := range patterns {
		re, err := syntax.Parse(expr, syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		bound := programSize(re) + programOverhead
		got := int64(len(program(t, noOnePass+expr).Inst))
		if got > bound {
			t.Errorf("pattern %.40q compiles to %d instructions, the walk counts %d", expr, got, bound)
		}
		if est := programInsts(re) + 3; got > est {
			t.Errorf("pattern %.40q compiles to %d instructions, the estimate counts %d with fail, empty group and match", expr, got, est)
		}
	}
	if re, _ := syntax.Parse(largestPattern('x', 0), syntax.Perl); programSize(re)+programOverhead != MaxPatternInstructions {
		t.Errorf("the largest pattern counts %d instructions, want %d", programSize(re)+programOverhead, MaxPatternInstructions)
	}
}

// TestPatternProgramLimits covers the bound on the program a pattern
// compiles to: unbounded, a repetition such as [a-z]{1000} repeated 300
// times (3,328 bytes) compiled for seconds and kept hundreds of megabytes,
// where a compile must return within 1 s. A pattern or patternProperties
// name whose measure may exceed MaxPatternInstructions, counting every copy
// of a repetition, the ranges of its classes and the parser's work on
// Unicode class escapes and case-insensitive ranges, is RZ-CFG-005 before
// anything compiles it; one at the bound compiles. The patterns of a
// document share MaxDocumentInstructions. The copies of a class share its
// ranges, which count once (counted in every copy, \p{L}{1,500} and
// ^[\p{L} .'-]{1,128}$ were refused, though without the one-pass matcher,
// which compilePattern never builds, they keep 46 KB or less).
func TestPatternProgramLimits(t *testing.T) {
	walked := func(schema string) *walker {
		t.Helper()
		doc := decodeInstance(t, schema)
		w := newWalker(doc)
		w.walk()
		return w
	}
	pattern := func(p string) string { return fmt.Sprintf(`{"pattern":%q}`, p) }
	cjk := cjkClass(20000)
	for _, tc := range []struct {
		name, schema, ptr, msg string
	}{
		{"one instruction over", pattern(largestPattern('x', 1)), "/pattern", "more than 16384 instructions"},
		{"repetitions of [a-z]{1000}", pattern(strings.Repeat("[a-z]{1000}", 300) + "x"), "/pattern", "more than 16384 instructions"},
		{"patternProperties name", `{"patternProperties":{"[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}":true}}`, "/patternProperties/[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}[a-z]{1000}", "patternProperties name compiles to a program of more than"},
		{"Unicode class escapes", pattern(strings.Repeat(`\pL`, 40)), "/pattern", "more than 16384 instructions"},
		{"Unicode class escapes past the bound before parsing", pattern(strings.Repeat(`\pL`, 16000)), "/pattern", "more than 16384 instructions"},
		{"escapes merged in one class", pattern("[" + strings.Repeat(`\pL`, 50) + "]"), "/pattern", "more than 16384 instructions"},
		{"case-insensitive ranges", pattern(`(?i)[\x{42}-\x{1e942}][\x{42}-\x{1e942}]`), "/pattern", "more than 16384 instructions"},
		{"a class's ranges", pattern(strings.Repeat(cjk[:3*2048+1]+"]", 41)), "/pattern", "more than 16384 instructions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := compileIssues(t, tc.schema)
			if len(issues) != 1 || issues[0].Pointer != tc.ptr || !strings.Contains(issues[0].Message, tc.msg) {
				t.Errorf("issues = %+v, want one at %q containing %q", issues, tc.ptr, tc.msg)
			}
		})
	}
	// At the bounds: the largest pattern compiles, and eight of them fill
	// the document's bound, which a ninth pattern of one character passes.
	if w := walked(pattern(largestPattern('x', 0))); len(w.issues) > 0 || w.insts != MaxPatternInstructions {
		t.Errorf("the largest pattern: issues %+v, %d instructions", w.issues, w.insts)
	}
	branches := make([]string, 8)
	for i := range branches {
		branches[i] = pattern(largestPattern(rune('p'+i), 0))
	}
	if w := walked(`{"allOf":[` + strings.Join(branches, ",") + `]}`); len(w.issues) > 0 || w.insts != MaxDocumentInstructions {
		t.Errorf("eight largest patterns: issues %+v, %d instructions, want %d", w.issues, w.insts, MaxDocumentInstructions)
	}
	issues := compileIssues(t, `{"allOf":[`+strings.Join(branches, ",")+`,{"properties":{"a":{"pattern":"a"}}}]}`)
	if len(issues) != 1 || issues[0].Pointer != "/allOf/8/properties/a/pattern" || !strings.Contains(issues[0].Message, "more than 131072 instructions together") {
		t.Errorf("a ninth pattern: issues %+v, want the document's bound at it", issues)
	}
	// What fits, measured exactly: Unicode class escapes count their
	// tables' ranges as parsing work and their classes' ranges in the
	// program, a class's ranges once whatever its copies (a repetition of
	// \pL, anchored or not, and i18n patterns), and case folding over one
	// wide range its characters.
	pl, cl := int64(classRanges(t, `\pL`)), int64(classRanges(t, cjk))
	ranges := func(n int64) int64 { return (n + rangesPerInstruction - 1) / rangesPerInstruction }
	const node = nodeInstructions
	escapes := func(n int64) int64 { return (n + escapeRangesPerInstruction - 1) / escapeRangesPerInstruction }
	for _, tc := range []struct {
		expr string
		want int64
	}{
		{strings.Repeat(`\pL`, 30), escapes(30*pl) + 30 + ranges(30*pl) + 30*node + programOverhead},
		{"[" + strings.Repeat(`\pL`, 45) + "]", escapes(45*pl) + 1 + ranges(pl) + node + programOverhead},
		{`^\pL{990}`, escapes(pl) + 991 + ranges(pl) + node + programOverhead},
		{`\pL{125}`, escapes(pl) + 125 + ranges(pl) + node + programOverhead},
		{`\p{L}{1,500}`, escapes(pl) + 999 + ranges(pl) + node + programOverhead},
		{`^\p{L}{1,500}$`, escapes(pl) + 1001 + ranges(pl) + node + programOverhead},
		{cjk + "{990}", 990 + ranges(cl) + node + programOverhead},
		{"^" + cjk + "{5}", 6 + ranges(cl) + node + programOverhead},
		{`(?i)[\x{42}-\x{1e942}]`, (0x1e942-0x42+foldRunesPerInstruction)/foldRunesPerInstruction + 1 + ranges(classRanges64(t, `(?i)[\x{42}-\x{1e942}]`)) + node + programOverhead},
		// The literals of an alternation keep their nodes.
		{"^(?:" + altLiterals(0x800, 330) + ")$", 2 + 660 + 329 + 330*node + programOverhead},
	} {
		if w := walked(pattern(tc.expr)); len(w.issues) > 0 || w.insts != tc.want {
			t.Errorf("pattern %.40q: issues %+v, %d instructions, want %d", tc.expr, w.issues, w.insts, tc.want)
		}
	}
	for _, expr := range []string{`^[\p{L} .'-]{1,128}$`, `^[\p{L}\p{M}\p{N}\p{P}\p{Zs}]{1,280}$`} {
		if w := walked(pattern(expr)); len(w.issues) > 0 {
			t.Errorf("pattern %q: issues %+v, want it to fit", expr, w.issues)
		}
	}
	// Case folding is charged only where a flag turns it on (reading (?-i)
	// as turning it on refused a case-sensitive pattern).
	for _, tc := range []struct {
		expr   string
		folded int
	}{
		{`(?i)[\x{42}-\x{1e942}]`, 0x1e942 - 0x42 + 1},
		{`(?i)[\x{0}-\x{10FFFF}]`, 1},
		{`(?i)[A-Z]`, 26},
		{`[\x{42}-\x{1e942}]`, 0},
		{`(?-i)[a-z]`, 0},
		{`(?i)(?-i)[a-z]`, 0},
		{`(?is-mU)[a-z]`, 26},
		{`(?s-i)[a-z]`, 0},
		{`(?i:[\101-\132])`, 26},
		{`(?i:a)[a-z]`, 0},
		{`((?i)a)[a-z]`, 0},
		{`(?i)((?-i)[a-z])[A-Z]`, 26},
		{`(?P<n>(?i)[a-z])[a-z]`, 26},
		{`\((?i)[a-z]\)[a-z]`, 52},
		{`[(](?i)[a-z][)][a-z]`, 26 + 1 + 26},
		{`(?i)[a\d[:alpha:]\p{Greek}]`, 1 + 2*asciiFold},
		// A Perl class escape outside a class folds too.
		{`(?i)\d\W`, 2 * asciiFold},
		{`\d\W`, 0},
		{`(?i)\Q[\x{42}-\x{1e942}]\E[\q-z]`, maxFold - minFold + 1},
		{`(?i)[\x41-\x5A]`, 26},
		{`(?i)[^\n-\x{1e942}]`, 0x1e942 - minFold + 1},
		// A flag group within a group sets the flags until that group closes,
		// and its own close is not a group's.
		{`(?i:a(?-i)b)[a-z]`, 0},
		{`(?i:a(?-i)b)(?i)[a-z]`, 26},
	} {
		if got := newWalker(nil).scanPattern(tc.expr).folded; got != tc.folded {
			t.Errorf("scanPattern(%q).folded = %d, want %d", tc.expr, got, tc.folded)
		}
	}
	// A Unicode class escape under case folding counts the folded table the
	// parser builds, which the cache keeps apart from the escape's own.
	w := newWalker(nil)
	plain, folded := classRanges(t, `\p{Lu}`), classRanges(t, `(?i)\p{Lu}`)
	if plain == folded {
		t.Fatalf("\\p{Lu} has %d ranges folded and not", plain)
	}
	for _, tc := range []struct {
		expr    string
		escapes int
	}{
		{`\p{Lu}`, plain},
		{`(?i)\p{Lu}`, folded},
		{`(?i:\p{Lu})\p{Lu}`, folded + plain},
	} {
		if got := w.scanPattern(tc.expr).escapes; got != tc.escapes {
			t.Errorf("scanPattern(%q).escapes = %d, want %d", tc.expr, got, tc.escapes)
		}
	}
	// Case-sensitive wide ranges, and three patterns of printable
	// characters each folding about 63,000, compile.
	mustCompile(t, `{"type":"string","pattern":"(?-i)[\\x{42}-\\x{1e942}][\\x{42}-\\x{1e942}]"}`)
	printable := `^(?i)[\\x{20}-\\x{7e}\\x{a0}-\\x{d7ff}\\x{e000}-\\x{fffd}]*$`
	mustCompile(t, `{"properties":{"name":{"pattern":"`+printable+`"},"title":{"pattern":"`+printable+`"},"note":{"pattern":"`+printable+`"}}}`)
}

// TestPatternProgramMemory pins what the compiled Schema keeps for the
// largest programs MaxDocumentInstructions admits (package documentation
// "Compile": about 6 MB at most), measured exactly, after a collection,
// with the Schema alive. Among them are the anchored programs under 1,000
// instructions for which Go's regexp would build its one-pass matcher,
// whose tables the measure does not count: compilePattern never lets it
// (with it, 98 patterns ^\pL{990} kept 864 MB; 23 anchored alternations of
// 330 classes 185 MB; 131 of 330 literals 142 MB; 131 anchored repetitions
// of a four-range class 21 MB; eight ^\pL{1,115}$ 19 MB). With
// MaxSubschemas positions, whose compiled schemas the library keeps
// besides, a whole document keeps about 16 MB.
func TestPatternProgramMemory(t *testing.T) {
	kept := func(t *testing.T, schema string) int64 {
		t.Helper()
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		s := mustCompile(t, schema)
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(s)
		return growth(after.HeapAlloc, before.HeapAlloc)
	}
	for _, tc := range patternMemoryCases() {
		t.Run(tc.name, func(t *testing.T) {
			if n := kept(t, tc.schema); n > 8<<20 {
				t.Errorf("the compiled Schema keeps %d bytes, want at most 8 MiB", n)
			}
		})
	}
	for _, tc := range documentMemoryCases() {
		t.Run(tc.name, func(t *testing.T) {
			if n := kept(t, tc.schema); n > 16<<20 {
				t.Errorf("the compiled Schema keeps %d bytes, want at most 16 MiB", n)
			}
		})
	}
}

// documentMemoryCases are documents of MaxSubschemas positions, whose
// compiled schemas the library keeps (about 2.4 KB each), with patterns:
// the largest ones, or a small one in every position.
func documentMemoryCases() []analysisCase {
	var b strings.Builder
	b.WriteString(`{"properties":{`)
	for i := range MaxSubschemas - 9 {
		fmt.Fprintf(&b, `"%x":{"$ref":"#"},`, i)
	}
	for i := range 8 {
		fmt.Fprintf(&b, `"q%d":{"pattern":%q}`, i, largestPattern(tailRune(i), 0))
		if i < 7 {
			b.WriteByte(',')
		}
	}
	b.WriteString(`}}`)
	return []analysisCase{
		{name: "positions and the largest patterns", schema: b.String()},
		{name: "small patterns", schema: fillPatterns(func(i int) string { return fmt.Sprintf(`^%c$`, 0x3400+i) })},
	}
}

// patternMemoryCases are documents whose patterns fill
// MaxDocumentInstructions, or MaxSchemaBytes first, in the shapes that keep
// the most: the largest programs, anchored or not, repetitions of classes
// of many ranges, and the anchored programs under 1,000 instructions for
// which Go would build its one-pass matcher, among them documents that kept
// hundreds of megabytes with that matcher.
func patternMemoryCases() []analysisCase {
	return []analysisCase{
		{name: "largest anchored patterns", schema: patternsOf(8, func(i int) string { return "^" + largestPattern(tailRune(i), -2) + "$" })},
		{name: "anchored class repetitions", schema: fillPatterns(func(i int) string { return fmt.Sprintf(`^\pL{990}%c`, 0x3400+i) })},
		{name: "large class repetitions", schema: fillPatterns(func(i int) string { return fmt.Sprintf(`^%s{990}%c`, cjkClass(1000), 0x3400+i) })},
		{name: "anchored alternations of classes", schema: fillPatterns(func(i int) string { return anchoredAlternation(330, fmt.Sprintf("%c", 0x3400+i)) })},
		{name: "anchored alternations of literals", schema: fillPatterns(func(i int) string { return "^(?:" + altLiterals(0x800+i*331, 330) + ")$" })},
		{name: "anchored repetitions of a four-range class", schema: fillPatterns(func(i int) string { return fmt.Sprintf(`^[a-bd-eg-hj-k]{990}%c$`, 0x3400+i) })},
		{name: "anchored optional repetitions", schema: fillPatterns(func(i int) string { return fmt.Sprintf(`^\pL{1,115}%c$`, 0x3400+i) })},
	}
}

// fillPatterns returns a schema whose properties hold n schemas with the
// pattern expr(i) each, for the most patterns n that fit
// MaxDocumentInstructions, all measuring as expr(0) does, MaxSchemaBytes
// and MaxSubschemas.
func fillPatterns(expr func(i int) string) string {
	w := newWalker(nil)
	w.pattern(expr(0), "", "pattern")
	n := min(int(MaxDocumentInstructions/max(w.insts, 1)), MaxSubschemas-1)
	build := func(n int) string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf(`"%d":{"pattern":%q}`, i, expr(i))
		}
		return `{"properties":{` + strings.Join(out, ",") + `}}`
	}
	schema := build(n)
	for n > 1 && len(schema) > MaxSchemaBytes-64 {
		n = n * (MaxSchemaBytes - 64) / len(schema)
		schema = build(n)
	}
	return schema
}

// patternsOf returns a schema whose allOf holds n schemas with the pattern
// expr(i) each.
func patternsOf(n int, expr func(i int) string) string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(`{"pattern":%q}`, expr(i))
	}
	return `{"allOf":[` + strings.Join(out, ",") + `]}`
}

// anchoredAlternation returns ^(?:...)$ around k branches, each a class of
// ten distinct CJK runes followed by tail: Go's one-pass matcher would
// merge the first ranges of all the branches at every alternation.
func anchoredAlternation(k int, tail string) string {
	var b strings.Builder
	b.WriteString("^(?:")
	for br := range k {
		if br > 0 {
			b.WriteByte('|')
		}
		b.WriteByte('[')
		for j := range 10 {
			fmt.Fprintf(&b, "%c", 0x4e00+2*(10*br+j))
		}
		b.WriteByte(']')
		b.WriteString(tail)
	}
	b.WriteString(")$")
	return b.String()
}

// altLiterals returns an alternation of n two-rune literals, each a
// distinct rune from first on followed by x.
func altLiterals(first, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("%cx", first+i)
	}
	return strings.Join(parts, "|")
}

// TestCompilePatternMatches checks the library's regexp engine
// (compilePattern) against Go's regexp: the empty group in front of every
// expression changes no match and no error, also for expressions that
// start with a repetition operator, after nothing or after flags, which it
// would complete (FuzzCompilePattern found (?)*), and names the expression
// as written; and the program it compiles never starts with the
// begin-text assertion, so Go builds no one-pass matcher (regexp
// compileOnePass), which would make an anchored alternation
// (anchoredAlternation) keep 8 MB.
func TestCompilePatternMatches(t *testing.T) {
	inputs := []string{"", "a", "ab", "abc", "aaa", "x", "xa", "K", "\u212a", "a\nb", "0123", "丂a", "αβ", "a@b.c"}
	for _, expr := range []string{
		``, `a`, `^a`, `^a$`, `\Aa\z`, `^a|^b`, `|^a`, `^a|`, `(?:^a|^b)`, `(^a)`, `(?i)^k$`, `(?m)^a$`, `^`, `$`, `^$`,
		`^[a-z]+$`, `^(?:a|bb|ccc)*$`, `^\pL{1,3}$`, `(?U)^a+`, `^\d+(?:\.\d+)?$`, emailPattern, `[a-z]{3}`, `{2,}`, `{a`, `a{2}`,
		`^(?:` + altLiterals(0x4e00, 30) + `)$`, `\Q*\E`, `^x?y?z?$`,
	} {
		want, wantErr := regexp.Compile(expr)
		got, err := compilePattern(expr)
		if (err != nil) != (wantErr != nil) || err != nil && err.Error() != wantErr.Error() {
			t.Errorf("compilePattern(%q) error = %v, want %v", expr, err, wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if got.String() != expr {
			t.Errorf("compilePattern(%q).String() = %q", expr, got.String())
		}
		for _, in := range inputs {
			if got.MatchString(in) != want.MatchString(in) {
				t.Errorf("compilePattern(%q).MatchString(%q) = %v, want %v", expr, in, got.MatchString(in), want.MatchString(in))
			}
		}
		if prog := program(t, noOnePass+expr); prog.Inst[prog.Start].Op == syntax.InstEmptyWidth {
			t.Errorf("compilePattern(%q) starts with an empty-width assertion: Go may build its one-pass matcher", expr)
		}
	}
	for _, expr := range []string{`*a`, `+`, `?a`, `{2}a`, `{1,2}`, `(?)*`, `(?i)+a`, `\Q\E*`, `(`, `a)`, `(?<`, `\8`, `a**`, `[z-a]`} {
		_, wantErr := regexp.Compile(expr)
		if _, err := compilePattern(expr); err == nil || wantErr == nil || err.Error() != wantErr.Error() {
			t.Errorf("compilePattern(%q) error = %v, want %v", expr, err, wantErr)
		}
	}
	// The anchored alternation keeps its program, not the 8 MB of one-pass
	// tables plain Go keeps for it.
	expr := anchoredAlternation(330, "a")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	p, err := compilePattern(expr)
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(p)
	if kept := growth(after.HeapAlloc, before.HeapAlloc); err != nil || kept > 512<<10 {
		t.Errorf("compilePattern(anchored alternation) = %v, keeps %d bytes, want at most 512 KiB", err, kept)
	}
}

// classRanges returns the ranges of the character class expr parses to.
func classRanges(t testing.TB, expr string) int {
	t.Helper()
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil || re.Op != syntax.OpCharClass {
		t.Fatalf("Parse(%.20q) = %v, %v, want a class", expr, re, err)
	}
	return len(re.Rune) / 2
}

// classRanges64 is classRanges as an int64.
func classRanges64(t testing.TB, expr string) int64 { return int64(classRanges(t, expr)) }

// TestPatternParsesBounded covers the patterns past a document's
// MaxDocumentInstructions: parsed, 617 patterns of 100 \pL each, in a
// schema refused anyway, took 1.3 s and 1.3 GB in every entry point, where
// a compile must return within 1 s. A pattern whose parsing work alone
// takes the document past its bound is not parsed, and no pattern after
// the bound is; a pattern refused after its parse still counts its
// parsing work. The test counts the parses, so it is deterministic.
func TestPatternParsesBounded(t *testing.T) {
	invalid := int(MaxDocumentInstructions / ((35*int64(classRanges(t, `\pL`)) + escapeRangesPerInstruction - 1) / escapeRangesPerInstruction))
	for _, tc := range []struct {
		name      string
		schema    string
		maxParsed int
		issues    int
	}{
		// About 13,800 each: nine fit, the tenth's escapes cross the bound.
		{"escapes", patternsOf(600, func(i int) string { return strings.Repeat(`\pL`, 30) + fmt.Sprint(i) }), 9, 1},
		{"escapes merged in a class", patternsOf(600, func(i int) string { return "[" + strings.Repeat(`\pL`, 40) + "]" + fmt.Sprint(i) }), 10, 1},
		// Refused one by one after the parse: their parsing work counts,
		// so the document's bound stops the parses after eight.
		{"refused after the parse", patternsOf(600, func(i int) string { return "[" + strings.Repeat(`\pL`, 45) + "]{1000}x{1000}" + fmt.Sprint(i) }), 8, 9},
		// Invalid after the parse, each counts its parsing work (about
		// 11,900), so the document's bound stops the parses after ten or
		// eleven, each one issue, and the bound's.
		{"invalid after the parse", patternsOf(600, func(i int) string { return strings.Repeat(`\pL`, 35) + fmt.Sprint(i) + "(" }), invalid, invalid + 1},
		// Refused one by one before the parse: none is parsed.
		{"refused before the parse", patternsOf(600, func(i int) string { return strings.Repeat(`\pL`, 60) + fmt.Sprint(i) }), 0, MaxIssues},
		{"folding", patternsOf(600, func(i int) string { return `(?i)[\x{42}-\x{1e942}]` + fmt.Sprint(i) }), 8, 1},
		// The tenth pattern's escapes cross the bound unparsed, and the light
		// patterns after it, which would fit what is left, are not parsed
		// either.
		{"light patterns after the bound", patternsOf(600, func(i int) string {
			if i < 10 {
				return strings.Repeat(`\pL`, 30) + fmt.Sprint(i)
			}
			return "a" + fmt.Sprint(i)
		}), 9, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWalker(decodeInstance(t, tc.schema))
			w.walk()
			if w.parsed > tc.maxParsed || len(w.issues) != tc.issues {
				t.Errorf("parsed %d patterns with %d issues (%+v), want at most %d with %d", w.parsed, len(w.issues), w.issues[:min(len(w.issues), 2)], tc.maxParsed, tc.issues)
			}
		})
	}
}

// TestScanPatternPrefixes runs the lexical pass over every prefix of
// patterns that use each construct it reads, which cuts escapes, classes
// and quoted runs anywhere (the compile never panics): it never
// fails, and where the parser builds a class of more than a hundred ranges
// from a Unicode class escape, the pass counts the escape's ranges.
func TestScanPatternPrefixes(t *testing.T) {
	for _, expr := range []string{
		`(?i)[^\x{42}-\x{1e942}\101-\132\pL\p{Greek}[:alpha:]\d-]\Q[a-z]\E\x41\x{263a}\PN\p{^Han}(?-i:[a-\z])`,
		`[]a]` + `[^]-]` + `[a-]` + `[\]` + `\`, `(?is)[\x{10FFFF}-\x{0}][\777-\0][\a-\v]`,
	} {
		for i := range len(expr) + 1 {
			prefix := expr[:i]
			ps := newWalker(nil).scanPattern(prefix)
			if re, err := syntax.Parse(prefix, syntax.Perl); err == nil && !strings.Contains(prefix, "(?i") {
				ranges := 0
				var walk func(*syntax.Regexp)
				walk = func(r *syntax.Regexp) {
					if r.Op == syntax.OpCharClass {
						ranges += len(r.Rune) / 2
					}
					for _, s := range r.Sub {
						walk(s)
					}
				}
				walk(re)
				if strings.ContainsAny(prefix, "pP") && ps.escapes == 0 && ranges > 100 {
					t.Errorf("prefix %q: no escape ranges counted, the parser built %d", prefix, ranges)
				}
			}
		}
	}
}

// TestHandleMatcherCharge covers the matcher's charge: charged to every
// body, the matcher of the document's largest pattern made a {} body under
// a schema with a large pattern reserve 64 MB, and eight such requests
// spent the Node's budget; the matcher is charged only when a pattern may
// reach a value or its member name.
func TestHandleMatcherCharge(t *testing.T) {
	schema := fmt.Sprintf(`{"properties":{"code":{"type":"string","pattern":%q}}}`, largestPattern('x', 0))
	s := mustCompile(t, schema)
	for _, body := range []string{`{}`, `{"other":"x"}`, `[]`, `1`} {
		if _, scan := scanBody(t, s, body); scan.transient != 0 {
			t.Errorf("body %s, which no pattern reaches: transient %d, want 0", body, scan.transient)
		}
	}
	if _, scan := scanBody(t, s, `{"code":"x"}`); scan.transient != matcherBytes+matcherInstBytes*int64(s.work.flags.patterns.insts) {
		t.Errorf("a string the pattern reaches: transient %d, want the matcher", scan.transient)
	}
	f := buildFilter(t, schema, 0)
	x, _ := exchange("application/json", []byte(`{}`))
	px := &peakExchange{Exchange: x}
	if r := f.Handle(t.Context(), phase.OnRequestBody, px); r.Outcome != filter.Continue || px.peak > 64<<10 {
		t.Errorf("Handle({}) = %+v, reserved %d, want Continue with a few kilobytes", r, px.peak)
	}
}
