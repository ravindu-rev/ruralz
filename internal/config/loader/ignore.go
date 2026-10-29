// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// A .ruralzignore in any directory of a Bundle uses gitignore syntax
// (https://git-scm.com/docs/gitignore), matched case-sensitively against
// slash-separated paths relative to that directory, the same on every OS:
//
//   - blank lines and lines starting with # are skipped; trailing spaces
//     are dropped unless escaped with \; a trailing CR is dropped;
//   - ! negates a pattern, so a later match re-includes a path, but never
//     one inside an excluded directory, which is not read at all;
//   - a trailing / matches directories only;
//   - a pattern with a / at its start or middle is matched from the file's
//     directory, any other against the last path segment at every depth
//     below it;
//   - * matches any text but /, ? one character but /, [...] one character
//     of a set (! or ^ negates it, POSIX classes such as [:alpha:] are
//     ASCII) but never /;
//   - a leading **/ matches in every directory, a trailing /** everything
//     inside, /**/ zero or more directories; other runs of * are one *;
//   - \ makes the next character literal.
//
// A deeper file's rules override a shallower file's (walker.ignored). A
// pattern git cannot read (an unclosed [, an unknown class, a trailing \)
// matches nothing, as in git. Matching works on code points, where git
// reads bytes; the two differ only for ? and [...] against non-ASCII
// characters.

// ignoreRule is one pattern of .ruralzignore.
type ignoreRule struct {
	// re matches whole paths relative to the file's directory; nil
	// matches nothing.
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

// ignoreList holds the rules of one file in file order; the last matching
// rule wins.
type ignoreList []ignoreRule

// match reports whether a rule of l matches path, relative to the file's
// directory, and if so whether the last one that does excludes it. dir
// tells whether the path is a directory.
func (l ignoreList) match(path string, dir bool) (ignored, matched bool) {
	for i := len(l) - 1; i >= 0; i-- {
		r := l[i]
		if r.re == nil || (r.dirOnly && !dir) {
			continue
		}
		if r.re.MatchString(path) {
			return !r.negate, true
		}
	}
	return false, false
}

// ignored reports whether l alone excludes path.
func (l ignoreList) ignored(path string, dir bool) bool {
	ignored, _ := l.match(path, dir)
	return ignored
}

// parseIgnore reads the rules of the .ruralzignore at path, whose load has
// read before patterns already, and returns them with their count. A
// pattern past maxPatterns for the load, or longer than
// maxIgnorePatternBytes, is the RZ-CFG-001 returned, at its line; the load
// then stops.
func parseIgnore(data []byte, path string, before, maxPatterns int) (ignoreList, int, *diag.Diagnostic) {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	var rules ignoreList
	line := 0
	for len(data) > 0 {
		line++
		var raw []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			raw, data = data[:i], data[i+1:]
		} else {
			raw, data = data, nil
		}
		s := strings.TrimSuffix(string(raw), "\r")
		if s == "" || s[0] == '#' {
			continue
		}
		s = trimTrailingSpaces(s)
		negate := strings.HasPrefix(s, "!")
		if negate {
			s = s[1:]
		}
		dirOnly := strings.HasSuffix(s, "/")
		s = strings.TrimSuffix(s, "/")
		if s == "" {
			continue
		}
		if before+len(rules) >= maxPatterns || len(s) > maxIgnorePatternBytes {
			msg := "the Bundle's " + IgnoreFile + " files hold more than " + strconv.Itoa(maxPatterns) + " patterns"
			if len(s) > maxIgnorePatternBytes {
				msg = "pattern is longer than " + strconv.Itoa(maxIgnorePatternBytes) + " bytes"
			}
			return nil, 0, &diag.Diagnostic{
				Code: CodeLimit, Severity: diag.SeverityError,
				Location: diag.Location{File: path, Line: line, Column: 1}, Message: msg,
			}
		}
		anchored := strings.Contains(s, "/")
		s = strings.TrimPrefix(s, "/")
		rules = append(rules, ignoreRule{re: compileGlob(s, anchored), negate: negate, dirOnly: dirOnly})
	}
	return rules, len(rules), nil
}

// trimTrailingSpaces drops the spaces that end s unless a backslash
// escapes them, as git does.
func trimTrailingSpaces(s string) string {
	keep := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ':
		case '\\':
			i++
			keep = min(i+1, len(s))
		default:
			keep = i + 1
		}
	}
	return s[:keep]
}

// compileGlob translates a gitignore pattern, without its !, trailing /
// and leading /, into a regular expression over whole root-relative paths.
// anchored patterns match from the root; others match the last segment at
// any depth. It returns nil for a pattern that matches nothing.
func compileGlob(p string, anchored bool) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`^`)
	if !anchored {
		b.WriteString(`(?:.*/)?`)
	}
	for i := 0; i < len(p); {
		switch c := p[i]; c {
		case '*':
			j := i
			for j < len(p) && p[j] == '*' {
				j++
			}
			segment := j-i >= 2 && (i == 0 || p[i-1] == '/') && (j == len(p) || p[j] == '/')
			switch {
			case !segment:
				b.WriteString(`[^/]*`)
			case j == len(p):
				// "**" alone, or a trailing "/**": everything below.
				b.WriteString(`.*`)
			default:
				// A leading "**/" or a middle "/**/": zero or more
				// directories.
				b.WriteString(`(?:.*/)?`)
				j++
			}
			i = j
		case '?':
			b.WriteString(`[^/]`)
			i++
		case '[':
			class, n, ok := bracket(p[i:])
			if !ok {
				return nil
			}
			b.WriteString(class)
			i += n
		case '\\':
			if i+1 == len(p) {
				return nil
			}
			_, size := utf8.DecodeRuneInString(p[i+1:])
			b.WriteString(regexp.QuoteMeta(p[i+1 : i+1+size]))
			i += 1 + size
		default:
			_, size := utf8.DecodeRuneInString(p[i:])
			b.WriteString(regexp.QuoteMeta(p[i : i+size]))
			i += size
		}
	}
	b.WriteString(`$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// runeRange is an inclusive range of code points.
type runeRange struct{ lo, hi rune }

// posixClasses returns the ASCII ranges of the POSIX character classes git
// accepts in a bracket expression.
func posixClasses() map[string][]runeRange {
	return map[string][]runeRange{
		"alnum":  {{'0', '9'}, {'A', 'Z'}, {'a', 'z'}},
		"alpha":  {{'A', 'Z'}, {'a', 'z'}},
		"blank":  {{'\t', '\t'}, {' ', ' '}},
		"cntrl":  {{0, 0x1f}, {0x7f, 0x7f}},
		"digit":  {{'0', '9'}},
		"graph":  {{0x21, 0x7e}},
		"lower":  {{'a', 'z'}},
		"print":  {{0x20, 0x7e}},
		"punct":  {{0x21, 0x2f}, {0x3a, 0x40}, {0x5b, 0x60}, {0x7b, 0x7e}},
		"space":  {{'\t', '\r'}, {' ', ' '}},
		"upper":  {{'A', 'Z'}},
		"xdigit": {{'0', '9'}, {'A', 'F'}, {'a', 'f'}},
	}
}

// bracket translates the bracket expression that starts p into a
// character class that never matches '/'. n is the bytes it spans; ok is
// false for an unclosed expression, an unknown class name or a set that
// can match nothing but '/'.
func bracket(p string) (class string, n int, ok bool) {
	i := 1
	negate := i < len(p) && (p[i] == '!' || p[i] == '^')
	if negate {
		i++
	}
	var set []runeRange
	classes := posixClasses()
	for first := true; ; first = false {
		if i >= len(p) {
			return "", 0, false
		}
		if p[i] == ']' && !first {
			i++
			break
		}
		if strings.HasPrefix(p[i:], "[:") {
			end := strings.Index(p[i+2:], ":]")
			if end < 0 {
				return "", 0, false
			}
			rs, known := classes[p[i+2:i+2+end]]
			if !known {
				return "", 0, false
			}
			set = append(set, rs...)
			i += 2 + end + 2
			continue
		}
		lo, size := classRune(p, i)
		if size == 0 {
			return "", 0, false
		}
		i += size
		hi := lo
		if i+1 < len(p) && p[i] == '-' && p[i+1] != ']' {
			r, size := classRune(p, i+1)
			if size == 0 {
				return "", 0, false
			}
			hi = r
			i += 1 + size
		}
		if lo <= hi {
			set = append(set, runeRange{lo, hi})
		}
	}
	var b strings.Builder
	b.WriteByte('[')
	if negate {
		b.WriteString(`^/`)
	}
	wrote := false
	for _, r := range set {
		for _, part := range withoutSlash(r) {
			b.WriteString(`\x{` + strconv.FormatInt(int64(part.lo), 16) + `}`)
			if part.hi != part.lo {
				b.WriteString(`-\x{` + strconv.FormatInt(int64(part.hi), 16) + `}`)
			}
			wrote = true
		}
	}
	b.WriteByte(']')
	switch {
	case negate && !wrote:
		return `[^/]`, i, true
	case !wrote:
		return "", 0, false
	default:
		return b.String(), i, true
	}
}

// classRune reads one character of a bracket expression at p[i], a \
// escape included, and its size; size 0 means the expression ended.
func classRune(p string, i int) (rune, int) {
	if p[i] == '\\' {
		if i+1 == len(p) {
			return 0, 0
		}
		r, size := utf8.DecodeRuneInString(p[i+1:])
		return r, 1 + size
	}
	return utf8.DecodeRuneInString(p[i:])
}

// withoutSlash returns r minus '/'.
func withoutSlash(r runeRange) []runeRange {
	if r.lo > '/' || r.hi < '/' {
		return []runeRange{r}
	}
	var out []runeRange
	if r.lo < '/' {
		out = append(out, runeRange{r.lo, '/' - 1})
	}
	if r.hi > '/' {
		out = append(out, runeRange{'/' + 1, r.hi})
	}
	return out
}
