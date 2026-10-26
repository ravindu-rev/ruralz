// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import "strings"

// segment is one piece of a scanned string: literal text (escapes
// resolved) or one expression.
type segment struct {
	// lit is the literal text when name is "".
	lit string
	// name is the variable of an expression.
	name string
	// def is the default of ${NAME:-default}.
	def string
	// hasDef reports the :- form.
	hasDef bool
}

// Reasons a ${ is malformed.
const (
	reasonUnterminated = "unterminated ${"
	reasonName         = "a variable name matches [A-Za-z_][A-Za-z0-9_]*"
	reasonOperator     = "only ${NAME} and ${NAME:-default} are supported"
	reasonNested       = "a default must not contain ${"
)

// searchWindow is the most bytes one search reads before it reports them
// to the scanner's meter, so a long scalar yields while it is scanned,
// one textStep at a time.
const searchWindow = textStep

// scanEach reads s by the grammar and passes its pieces to each, in
// order: $${ is a literal ${, ${NAME} and ${NAME:-default} are
// expressions (NAME matching [A-Za-z_][A-Za-z0-9_]*, default any run of
// characters without } and without ${), and a $ not followed by { is
// literal. A literal piece is a non-empty substring of s; an escape ends
// one, so the literal text between two expressions may come in several
// pieces. Any other ${ is malformed: scanEach stops there and returns the
// reason, having passed the pieces before it; read leniently, a malformed
// ${ is literal text and the scan goes on after it, which is how the
// authored text an RZ-CFG-005 error leaves in place reads. each returning
// false stops the scan. meter, when not nil, receives the bytes the scan
// reads.
//
// scanEach allocates nothing and reads every byte of s a bounded number
// of times in both modes, so it costs time linear in len(s) and constant
// memory, on hostile input too. A default ends at the next } and must not
// hold ${, so the scanner keeps one forward-only search for the next },
// one for the next ${ and one for the next $: a } after the next ${ never
// closes the current default (the default is nested, or unterminated when
// no } remains), and a search never reads again what an earlier one
// passed. The positions searched from only grow: the next ${ after an
// expression's :- lies past it, whether the expression was valid or, read
// leniently, malformed.
func scanEach(s string, lenient bool, meter func(bytes int), each func(segment) bool) (bad string) {
	sc := scanner{
		s: s, meter: meter,
		dollar: search{sub: "$", from: -1}, close: search{sub: "}", from: -1}, open: search{sub: "${", from: -1},
	}
	// The literal piece being read is s[start:i].
	start := 0
	emit := func(end int) bool {
		if end > start {
			return each(segment{lit: s[start:end]})
		}
		return true
	}
	for i := 0; i < len(s); {
		j := sc.next(&sc.dollar, i)
		if j > i {
			i = j
			continue
		}
		switch {
		case strings.HasPrefix(s[i:], "$${"):
			// The escape reads as the ${ at s[i+1:i+3].
			if !emit(i) {
				return ""
			}
			start = i + 1
			i += 3
		case strings.HasPrefix(s[i:], "${"):
			e, n, reason := sc.expression(i + 2)
			if reason != "" && lenient {
				i += 2
				continue
			}
			if reason != "" {
				return reason
			}
			if !emit(i) || !each(e) {
				return ""
			}
			i += 2 + n
			start = i
		default:
			i++
		}
	}
	emit(len(s))
	return ""
}

// scanner holds the forward-only searches of one scanEach call.
type scanner struct {
	s     string
	meter func(bytes int)
	// dollar, close and open find the next $, } and ${.
	dollar, close, open search
}

// search finds the next occurrence of sub. at is the first occurrence at
// or after from, len(s) when none remains; from is -1 before the first
// search.
type search struct {
	sub      string
	from, at int
}

// read reports n bytes read to the meter.
func (sc *scanner) read(n int) {
	if sc.meter != nil && n > 0 {
		sc.meter(n)
	}
}

// next returns the index of the first c.sub at or after p, or len(s).
// Positions only grow from one call to the next, so the answer of the
// last search stands while p has not passed it, and a new search starts
// past every byte an earlier one read: all searches of one string read
// it once (scanEach). A search reports its bytes to the meter at least
// every searchWindow bytes.
func (sc *scanner) next(c *search, p int) int {
	s := sc.s
	if c.from >= 0 && p >= c.from && p <= c.at {
		return c.at
	}
	c.from = p
	for q := p; q < len(s); q += searchWindow {
		end := min(len(s), q+searchWindow+len(c.sub)-1)
		if i := strings.Index(s[q:end], c.sub); i >= 0 {
			sc.read(i + len(c.sub))
			c.at = q + i
			return c.at
		}
		sc.read(end - q)
	}
	c.at = len(s)
	return c.at
}

// expression parses the text at s[at:], just after a ${, and returns the
// expression and the number of bytes it used, closing } included.
func (sc *scanner) expression(at int) (segment, int, string) {
	s := sc.s
	n := sc.name(at)
	if n == 0 {
		if at == len(s) {
			return segment{}, 0, reasonUnterminated
		}
		return segment{}, 0, reasonName
	}
	e := segment{name: s[at : at+n]}
	r := at + n
	switch {
	case r == len(s):
		return segment{}, 0, reasonUnterminated
	case s[r] == '}':
		return e, n + 1, ""
	case strings.HasPrefix(s[r:], ":-"):
		d := r + 2
		end := sc.next(&sc.close, d)
		if end == len(s) {
			return segment{}, 0, reasonUnterminated
		}
		if sc.next(&sc.open, d) < end {
			return segment{}, 0, reasonNested
		}
		e.def, e.hasDef = s[d:end], true
		return e, end - at + 1, ""
	case s[r:] == ":":
		return segment{}, 0, reasonUnterminated
	default:
		return segment{}, 0, reasonOperator
	}
}

// name returns the length of the variable name at s[at:] (nameLen),
// reporting the bytes it reads at least every searchWindow bytes.
func (sc *scanner) name(at int) int {
	s := sc.s
	n := 0
	for {
		start := at + n
		end := min(len(s), start+searchWindow)
		k := nameLen(s[start:end])
		if n > 0 {
			k = nameTail(s[start:end])
		}
		sc.read(k)
		n += k
		if start+k < end || end == len(s) {
			return n
		}
	}
}

// nameLen returns the length of the variable name at the start of s, 0
// when s does not start with one. A name holds no $, so the names read
// after the ${ of one string never overlap.
func nameLen(s string) int {
	if s == "" || !nameStart(s[0]) {
		return 0
	}
	return 1 + nameTail(s[1:])
}

// nameTail returns the length of the run of [A-Za-z0-9_] at the start of
// s.
func nameTail(s string) int {
	// bounded: one window of at most searchWindow bytes, reported by name.
	for i := 0; i < len(s); i++ {
		if c := s[i]; !nameStart(c) && (c < '0' || c > '9') {
			return i
		}
	}
	return len(s)
}

// nameStart reports a byte that may start a variable name: [A-Za-z_].
func nameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// run is one piece of a scalar's authored text as the RZ-CFG-013 rules
// read it (eachRun).
type run struct {
	// text is the literal text and defaults of the run, escapes resolved.
	text string
	// lead marks the run at the start of the scalar.
	lead bool
}

// eachRun passes f the text written in the Bundle of scalar text s, the
// reading of both RZ-CFG-013 credential rules (walker.detectRuns,
// walker.literal): the text s renders when every variable is unset, so a
// ${NAME:-default} reads as its default whether the scalar takes it or
// not, cut into runs at each ${NAME} without default, whose text never
// comes from the Bundle. A variable's value is never read: it is not text
// of the Bundle. s is read leniently, so a malformed ${ is literal text
// (on valid text the two modes agree). f returning false stops the
// reading. meter, when not nil, receives the bytes read and copied; a run
// of one piece is a substring of s, so only runs with escapes or defaults
// are copied.
func eachRun(s string, meter func(bytes int), f func(run) bool) {
	read := meter
	if read == nil {
		read = func(int) {}
	}
	var b strings.Builder
	var first string
	pieces, lead, stopped := 0, true, false
	add := func(text string) {
		if text == "" {
			return
		}
		switch pieces {
		case 0:
			first = text
		case 1:
			appendText(&b, first, read)
			appendText(&b, text, read)
		default:
			appendText(&b, text, read)
		}
		pieces++
	}
	flush := func() bool {
		text := first
		if pieces > 1 {
			text = b.String()
			b.Reset()
		}
		ok := pieces == 0 || f(run{text: text, lead: lead})
		pieces, first = 0, ""
		return ok
	}
	scanEach(s, true, meter, func(sg segment) bool {
		switch {
		case sg.name == "":
			add(sg.lit)
		case sg.hasDef:
			add(sg.def)
		default:
			if !flush() {
				stopped = true
				return false
			}
			lead = false
		}
		return true
	})
	if !stopped {
		flush()
	}
}

// secretLike reports a variable name matching
// (?i)(secret|password|token|apikey). A name of the substitution grammar
// is ASCII, so ASCII case folding is the regular expression's, and the
// test allocates nothing. It reads the name in windows of textStep bytes,
// each overlapping the next by a word's length less one, and reports them
// to read.
func secretLike(name string, read func(int)) bool {
	for _, w := range [...]string{"secret", "password", "token", "apikey"} {
		for q := 0; q < len(name); q += textStep {
			end := min(len(name), q+textStep+len(w)-1)
			read(end - q)
			if containsLower(name[q:end], w) {
				return true
			}
		}
	}
	return false
}

// containsLower reports whether s holds w, a lower-case ASCII word, in
// any ASCII case.
func containsLower(s, w string) bool {
next:
	// bounded: one window of the name, reported by secretLike.
	for i := 0; i+len(w) <= len(s); i++ {
		// bounded: the bytes of one word.
		for j := 0; j < len(w); j++ {
			c := s[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != w[j] {
				continue next
			}
		}
		return true
	}
	return false
}
