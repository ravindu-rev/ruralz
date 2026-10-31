// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode/utf8"
)

// The size of a pattern. Go's regexp compiles a pattern into a program the
// compiled Schema keeps, so the work and the memory of compiling grow with
// the program, not with the pattern: the 11 bytes of [a-z]{1000} compile to
// a thousand instructions, and unmeasured, seven patterns of 33 KB would
// compile for 25 s and keep 886 MB. The walk therefore measures every
// pattern before anything compiles it and refuses a pattern whose measure
// exceeds MaxPatternInstructions, or patterns whose measures exceed
// MaxDocumentInstructions together.
//
// A pattern's measure is the work of parsing it plus what its compiled form
// keeps, in instructions of 40 bytes: the instructions of its program as
// Go's own parser estimates them, every copy a repetition expands to
// included; programOverhead; the ranges of its character classes,
// rangesPerInstruction of them as one instruction; and nodeInstructions for
// every literal and class of the parsed expression, whose node the program
// may keep. The copies a repetition makes share the ranges and the node of
// its literal or class, which count once. The parsing work is counted before
// the parse, lexically, since the parser does it whatever the program: the
// ranges of the table it builds for each Unicode class escape (\p and \P;
// \pL holds about 680), which it merges and sorts within a class,
// escapeRangesPerInstruction of them as one instruction, and the characters
// of case-insensitive class ranges, which it folds one at a time (about 4 ms
// for [\x{42}-\x{1e942}] under (?i)), foldRunesPerInstruction of them as one
// instruction (proposed, measured on a shared 4-vCPU 2.8 GHz machine against
// Go's compile of an instruction). A pattern whose parsing work alone
// exceeds the bound is refused unparsed, and once the document's bound is
// spent no later pattern is parsed (parsed past the bound, hundreds of
// escape-heavy patterns would take 1.3 s and 1.3 GB for a schema refused
// anyway); a pattern refused after its parse still counts its parsing
// work. The library compiles every distinct pattern once per compile
// (regexpEngine), through compilePattern, which never builds Go's one-pass
// matcher: its tables are not part of the measure.

// rangesPerInstruction is the character-class ranges counted as one more
// instruction of the program: an instruction takes 40 bytes, a range 8.
const rangesPerInstruction = 5

// programOverhead is what every pattern counts besides the instructions
// its expression compiles to: the fail and match instructions, the no-op
// of noOnePass, and the compiled regexp's own structures and the spare
// capacity of a small program, together at most about 1 KB (measured 610
// to 990 bytes for patterns of two to four characters).
const programOverhead = 24

// nodeInstructions is what a literal or a class of the parsed expression
// counts: the program's instructions refer to its runes, which the parser
// keeps in the 112-byte node itself when they fit, as one or two do, so
// the node stays alive (131 anchored alternations of 330 two-rune
// literals keep 11 MB of programs and nodes without the one-pass matcher,
// 84 bytes per instruction).
const nodeInstructions = 3

// noOnePass is the empty group compilePattern compiles in front of every
// expression. Go's regexp builds a one-pass matcher for a program that
// starts with the begin-text assertion of an anchored pattern and has
// fewer than 1,000 instructions (regexp compileOnePass), and its tables
// hold a copy of every instruction, the ranges of every class again with
// a dispatch table, and at every alternation the merged first ranges of
// all its branches, so they grow with the square of an alternation (23
// anchored alternations of 330 classes, 258 KB within every bound, would
// keep 185 MB; 131 of 330 literals 142 MB; 131 anchored repetitions of a
// four-range class 21 MB). Behind the empty group the program starts
// with a no-op, so Go never builds one. The group matches the empty
// string, so the expression matches as written, and the matcher still
// knows the program is anchored (syntax.Prog.StartCond skips the no-op)
// and runs its backtracker or its NFA simulation, whose work regexcost.go
// bounds.
const noOnePass = "(?:)"

// compiledPattern is a regular expression compiled by compilePattern: it
// matches as Go's regexp compiles its expression, and names it.
type compiledPattern struct {
	re   *regexp.Regexp
	expr string
}

// MatchString reports whether s contains a match of the expression.
func (p *compiledPattern) MatchString(s string) bool { return p.re.MatchString(s) }

// String returns the expression as the schema writes it.
func (p *compiledPattern) String() string { return p.expr }

// compilePattern compiles the RE2 expression expr with Go's regexp behind
// noOnePass. The expression is parsed alone first, so an invalid one fails
// with the error Go's regexp gives it: the empty group would give a
// repetition operator at its start, after nothing or after flags such as
// (?i), the operand it lacks.
func compilePattern(expr string) (*compiledPattern, error) {
	if _, err := syntax.Parse(expr, syntax.Perl); err != nil {
		return nil, err
	}
	re, err := regexp.Compile(noOnePass + expr)
	if err != nil {
		return nil, err
	}
	return &compiledPattern{re: re, expr: expr}, nil
}

// escapeRangesPerInstruction is the ranges of Unicode class escape tables
// counted as one instruction of parsing work: merging and sorting them in
// a class takes about 0.11 µs a range, Go's compile about 0.2 to 0.4 µs an
// instruction.
const escapeRangesPerInstruction = 2

// foldRunesPerInstruction is the characters the parser folds one by one
// counted as one instruction of parsing work: about 31 ns a character.
const foldRunesPerInstruction = 8

// minFold and maxFold are the first and last characters Go's parser folds
// (regexp/syntax); a class range outside them, or covering them all, is
// not folded one by one.
const minFold, maxFold = 0x0041, 0x1e943

// patternScan is what the lexical pass over one pattern finds.
type patternScan struct {
	// escapes sums the ranges of its Unicode class escapes, each at the
	// size of the table the parser builds for it.
	escapes int
	// folded sums the characters the parser folds one by one.
	folded int
}

// parseWork returns the instructions the parsing work of ps counts as.
func (ps patternScan) parseWork() int64 {
	return int64((ps.escapes+escapeRangesPerInstruction-1)/escapeRangesPerInstruction) +
		int64((ps.folded+foldRunesPerInstruction-1)/foldRunesPerInstruction)
}

// pattern applies the RE2 rule and the program bounds to the regular
// expression expr, the pattern or patternProperties name (what) at ptr,
// without compiling it.
func (w *walker) pattern(expr, ptr, what string) {
	parse := w.scanPattern(expr).parseWork()
	if parse > MaxPatternInstructions {
		w.add(ptr, tooLarge(what))
		return
	}
	if w.instsOver {
		// The document's bound is reported: nothing more is parsed.
		return
	}
	if w.insts+parse > MaxDocumentInstructions {
		w.overInstructions(ptr)
		return
	}
	w.parsed++
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		w.insts += parse
		w.add(ptr, what+" is not a valid RE2 regular expression: "+err.Error())
		return
	}
	size := addCap(parse, programSize(re)+programOverhead)
	if size > MaxPatternInstructions {
		w.insts += parse
		w.add(ptr, tooLarge(what))
		return
	}
	w.insts += size
	if w.insts > MaxDocumentInstructions {
		w.overInstructions(ptr)
	}
}

// tooLarge is the finding of a pattern or patternProperties name (what)
// whose measure exceeds MaxPatternInstructions.
func tooLarge(what string) string {
	return fmt.Sprintf("%s compiles to a program of more than %d instructions (a repetition counts every copy, a character class its ranges, and the parser's work on Unicode class escapes and case-insensitive ranges counts too)", what, MaxPatternInstructions)
}

// overInstructions reports the document's bound at ptr, once.
func (w *walker) overInstructions(ptr string) {
	if !w.instsOver {
		w.instsOver = true
		w.add(ptr, fmt.Sprintf("the patterns of the schema compile to programs of more than %d instructions together", MaxDocumentInstructions))
	}
}

// programSize returns the measure of what the parsed expression re
// compiles to, besides programOverhead: programInsts, the ranges of its
// classes, rangesPerInstruction of them as one instruction, and
// nodeInstructions per literal and class.
func programSize(re *syntax.Regexp) int64 {
	ranges, nodes := programNodes(re)
	return addCap(addCap(programInsts(re), (ranges+rangesPerInstruction-1)/rangesPerInstruction), mulCap(nodes, nodeInstructions))
}

// programInsts returns an upper bound of the instructions Go compiles the
// parsed expression re into: the estimate Go's parser makes of them
// (regexp/syntax calcSize), which counts every copy a repetition expands
// to and the alternatives a star, a plus and a question mark add.
func programInsts(re *syntax.Regexp) int64 {
	var size int64
	switch re.Op {
	case syntax.OpLiteral:
		size = int64(len(re.Rune))
	case syntax.OpCapture, syntax.OpStar:
		size = 2 + programInsts(re.Sub[0])
	case syntax.OpPlus, syntax.OpQuest:
		size = 1 + programInsts(re.Sub[0])
	case syntax.OpConcat:
		for _, s := range re.Sub {
			size = addCap(size, programInsts(s))
		}
	case syntax.OpAlternate:
		for _, s := range re.Sub {
			size = addCap(size, programInsts(s))
		}
		size = addCap(size, int64(max(len(re.Sub)-1, 0)))
	case syntax.OpRepeat:
		sub := programInsts(re.Sub[0])
		switch {
		case re.Max == -1 && re.Min == 0:
			size = addCap(2, sub)
		case re.Max == -1:
			size = addCap(1, mulCap(int64(re.Min), sub))
		default:
			size = addCap(mulCap(int64(re.Max), sub), int64(re.Max-re.Min))
		}
	default:
		// A character class, an empty-width assertion or any character is
		// one instruction, and so is no match: calcSize's minimum.
	}
	return max(1, size)
}

// programNodes returns the ranges of the character classes of the parsed
// expression re and its literals and classes, each once: the program's
// copies of a literal or a class share its runes.
func programNodes(re *syntax.Regexp) (ranges, nodes int64) {
	switch re.Op {
	case syntax.OpCharClass:
		ranges, nodes = int64(len(re.Rune)/2), 1
	case syntax.OpLiteral:
		nodes = 1
	default:
		// No other node holds runes the program refers to.
	}
	for _, s := range re.Sub {
		r, n := programNodes(s)
		ranges, nodes = addCap(ranges, r), addCap(nodes, n)
	}
	return ranges, nodes
}

// scanPattern is the lexical pass over expr before it is parsed: the
// ranges of its Unicode class escapes and the characters the parser folds
// one by one in the class ranges where case folding is on. It mirrors the
// parser's reading of escapes, character classes, groups and their flags
// (a flag group sets the flags until its enclosing group closes, and
// (?flags:...) within itself only); where it cannot decode a range, it
// charges the whole fold. It takes time linear in expr.
func (w *walker) scanPattern(expr string) patternScan {
	l := lexer{w: w, expr: expr, named: strings.LastIndex(expr, ":]")}
	// groups holds the fold state at each open group, to restore at its
	// close.
	var groups []bool
	for i := 0; i < len(expr); {
		switch expr[i] {
		case '\\':
			i = l.escape(i)
		case '[':
			i = l.class(i)
		case '(':
			groups = append(groups, l.fold)
			var flagsOnly bool
			i, flagsOnly = l.group(i)
			if flagsOnly {
				groups = groups[:len(groups)-1]
			}
		case ')':
			if n := len(groups); n > 0 {
				l.fold = groups[n-1]
				groups = groups[:n-1]
			}
			i++
		default:
			i++
		}
	}
	return l.ps
}

// lexer is the state of scanPattern: fold is whether case folding is on at
// the current position, and named the index of the last ":]" of expr,
// which a POSIX class such as [:alpha:] ends with.
type lexer struct {
	w     *walker
	expr  string
	fold  bool
	named int
	ps    patternScan
}

// group reads the opening of the group at i, as the parser does, and
// returns the index past what it read, and whether it was a flag group
// such as (?i) that sets the flags of its enclosing group rather than
// opening one. Within (?flags:...) the flags apply to the group only; an
// i before any - turns folding on, after it off.
func (l *lexer) group(i int) (int, bool) {
	s := l.expr[i:]
	if len(s) < 2 || s[1] != '?' {
		return i + 1, false
	}
	fold, neg := l.fold, false
	j := 2
	for ; j < len(s) && strings.IndexByte("imsU-", s[j]) >= 0; j++ {
		switch s[j] {
		case '-':
			neg = true
		case 'i':
			fold = !neg
		}
	}
	switch {
	case j < len(s) && s[j] == ')':
		l.fold = fold
		return i + j + 1, true
	case j < len(s) && s[j] == ':':
		l.fold = fold
		return i + j + 1, false
	}
	// A named capture, or a syntax error the parser reports.
	return i + 2, false
}

// asciiFold bounds the characters folding a Perl or POSIX class takes: its
// ranges lie in ASCII.
const asciiFold = 128

// escape scans the escape at i outside a character class and returns the
// index past it: a quoted run \Q...\E, a Unicode class escape, a Perl
// class escape, or one escaped character.
func (l *lexer) escape(i int) int {
	s := l.expr[i:]
	if len(s) < 2 {
		return len(l.expr)
	}
	switch s[1] {
	case 'Q':
		if end := strings.Index(s[2:], `\E`); end >= 0 {
			return i + 2 + end + 2
		}
		return len(l.expr)
	case 'p', 'P':
		return i + l.unicodeEscape(s)
	case 'd', 'D', 's', 'S', 'w', 'W':
		if l.fold {
			// The parser folds the ASCII ranges of a Perl class one by one.
			l.ps.folded += asciiFold
		}
		return i + 2
	}
	_, size := utf8.DecodeRuneInString(s[1:])
	return i + 1 + size
}

// class scans the character class at i, as the parser reads it, and
// returns the index past it.
func (l *lexer) class(i int) int {
	i++
	if i < len(l.expr) && l.expr[i] == '^' {
		i++
	}
	for first := true; i < len(l.expr) && (l.expr[i] != ']' || first); first = false {
		t := l.expr[i:]
		switch {
		case len(t) > 2 && t[0] == '[' && t[1] == ':' && i+2 <= l.named:
			// A POSIX class such as [:alpha:]: the parser reads up to the
			// next ":]".
			i += 2 + strings.Index(t[2:], ":]") + 2
			if l.fold {
				l.ps.folded += asciiFold
			}
			continue
		case len(t) >= 2 && t[0] == '\\' && (t[1] == 'p' || t[1] == 'P'):
			i += l.unicodeEscape(t)
			continue
		case len(t) >= 2 && t[0] == '\\' && strings.IndexByte("dDsSwW", t[1]) >= 0:
			i += 2
			if l.fold {
				l.ps.folded += asciiFold
			}
			continue
		}
		lo, n := classChar(t)
		i += n
		hi := lo
		if t = l.expr[i:]; len(t) >= 2 && t[0] == '-' && t[1] != ']' {
			hi, n = classChar(t[1:])
			i += 1 + n
		}
		if l.fold {
			l.ps.folded += foldWork(lo, hi)
		}
	}
	return min(i+1, len(l.expr))
}

// unicodeEscape scans the Unicode class escape (\pX, \p{Name}, \PX or
// \P{Name}) at the start of s, adds the ranges of the table the parser
// builds for it, and returns its length.
func (l *lexer) unicodeEscape(s string) int {
	n := len(s)
	if len(s) > 2 && s[2] == '{' {
		if end := strings.IndexByte(s, '}'); end >= 0 {
			n = end + 1
		}
	} else if len(s) > 2 {
		_, size := utf8.DecodeRuneInString(s[2:])
		n = 2 + size
	}
	esc := s[:n]
	if l.fold {
		esc = "(?i)" + esc
	}
	ranges, ok := l.w.tables[esc]
	if !ok {
		// The parser's own reading of the escape alone, once per escape and
		// document: an invalid one makes the pattern invalid.
		if re, err := syntax.Parse(esc, syntax.Perl); err == nil {
			ranges = len(re.Rune) / 2
		}
		l.w.tables[esc] = ranges
	}
	l.ps.escapes += ranges
	return n
}

// classChar decodes the character-class character at the start of t, as
// the parser does, and returns it with its length; -1 stands for a
// character it cannot decode.
func classChar(t string) (rune, int) {
	if t == "" {
		return -1, 0
	}
	if t[0] != '\\' {
		r, size := utf8.DecodeRuneInString(t)
		return r, size
	}
	if len(t) < 2 {
		return -1, 1
	}
	c := t[1]
	switch {
	case c >= utf8.RuneSelf:
		_, size := utf8.DecodeRuneInString(t[1:])
		return -1, 1 + size
	case c >= '0' && c <= '7':
		r, n := rune(c-'0'), 2
		for n < 4 && n < len(t) && t[n] >= '0' && t[n] <= '7' {
			r = r*8 + rune(t[n]-'0')
			n++
		}
		return r, n
	case c == 'x':
		return hexEscape(t)
	case !isAlnum(c):
		return rune(c), 2
	}
	switch c {
	case 'a':
		return '\a', 2
	case 'f':
		return '\f', 2
	case 'n':
		return '\n', 2
	case 'r':
		return '\r', 2
	case 't':
		return '\t', 2
	case 'v':
		return '\v', 2
	}
	return -1, 2
}

// hexEscape decodes the \xHH or \x{H...} escape at the start of t.
func hexEscape(t string) (rune, int) {
	if len(t) > 2 && t[2] == '{' {
		end := strings.IndexByte(t, '}')
		if end < 0 {
			return -1, len(t)
		}
		var r rune
		for _, c := range []byte(t[3:end]) {
			v := unhex(c)
			if v < 0 || r > 0x10ffff {
				return -1, end + 1
			}
			r = r*16 + v
		}
		return r, end + 1
	}
	if len(t) < 4 || unhex(t[2]) < 0 || unhex(t[3]) < 0 {
		return -1, min(len(t), 4)
	}
	return unhex(t[2])*16 + unhex(t[3]), 4
}

// unhex returns the value of a hexadecimal digit, or -1.
func unhex(c byte) rune {
	switch {
	case c >= '0' && c <= '9':
		return rune(c - '0')
	case c >= 'a' && c <= 'f':
		return rune(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return rune(c-'A') + 10
	}
	return -1
}

// isAlnum reports whether c is an ASCII letter or digit.
func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// foldWork returns the characters the parser folds one by one for the
// class range lo-hi under case folding: those within minFold and maxFold,
// unless the range covers them all; -1 for a character the scan cannot
// decode charges them all.
func foldWork(lo, hi rune) int {
	switch {
	case lo < 0 || hi < 0:
		return maxFold - minFold + 1
	case hi < lo, lo <= minFold && hi >= maxFold, hi < minFold, lo > maxFold:
		return 1
	}
	return int(min(hi, maxFold)-max(lo, minFold)) + 1
}
