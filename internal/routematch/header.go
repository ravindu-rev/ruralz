// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"net/textproto"
	"regexp"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// CompileRegex compiles a path.regex or headers[].regex criterion as RE2
// matched against the whole input: re becomes ^(?:re)$ (04 req 28, 30). re
// is compiled on its own first, so an unbalanced expression such as "a)|(.*"
// cannot close the group and escape the anchors. A valid re that ends inside
// a \Q quote (such as `\Qa.b`) gets a closing \E before ")$", which would
// otherwise be quoted too. An invalid expression is an error carrying
// [CodeInvalid] (RZ-CFG-005).
func CompileRegex(re string) (*regexp.Regexp, error) {
	if _, err := regexp.Compile(re); err != nil {
		return nil, errcode.Errorf(CodeInvalid, "regular expression %q: %w", re, err)
	}
	closeQuote := ""
	if endsInQuote(re) {
		closeQuote = `\E`
	}
	rx, err := regexp.Compile(`^(?:` + re + closeQuote + `)$`)
	if err != nil {
		return nil, errcode.Errorf(CodeInvalid, "regular expression %q: %w", re, err)
	}
	return rx, nil
}

// endsInQuote reports whether re, a valid RE2 expression, ends inside a \Q
// quote that no \E closes. A valid expression has no other open state at
// its end: groups and classes are closed, and every "\" escapes the next
// byte. As in regexp/syntax, a quote ends at the first `\E` after `\Q`.
func endsInQuote(re string) bool {
	for i := 0; i+1 < len(re); i++ {
		if re[i] != '\\' {
			continue
		}
		if re[i+1] != 'Q' {
			i++
			continue
		}
		end := strings.Index(re[i+2:], `\E`)
		if end < 0 {
			return true
		}
		i += 2 + end + 1
	}
	return false
}

// MethodMatch reports whether method passes a methods criterion (04 req
// 30): an empty set passes every method; otherwise the method must equal
// one entry exactly, case-sensitively, and HEAD does not imply GET.
func MethodMatch(methods []string, method string) bool {
	return len(methods) == 0 || slices.Contains(methods, method)
}

// JoinHeader returns the value CEL request.headers and header criteria see
// for one field (04 req 25): repeated fields joined with ", " (RFC 9110
// 5.3). It returns values[0] itself, allocating nothing, for a single field.
func JoinHeader(values []string) string {
	switch len(values) {
	case 0:
		return ""
	case 1:
		return values[0]
	default:
		return strings.Join(values, ", ")
	}
}

// HeaderMatcher is one compiled headers[] criterion (04 req 30).
type HeaderMatcher struct {
	// Name is the lower-cased header name.
	Name string
	// Key is the canonical http.Header key of Name.
	Key string

	exact   string
	regex   *regexp.Regexp
	present *bool
}

// CompileHeader compiles c. Exactly one of Exact, Regex and Present must be
// set; otherwise, or for an invalid name or expression, the error carries
// [CodeInvalid] (RZ-CFG-005).
func CompileHeader(c HeaderCriteria) (HeaderMatcher, error) {
	if c.Name == "" || strings.ContainsFunc(c.Name, func(r rune) bool { return !isTokenChar(r) }) {
		return HeaderMatcher{}, errcode.Errorf(CodeInvalid, "header name %q is not an RFC 9110 token", c.Name)
	}
	forms := 0
	for _, set := range []bool{c.Exact != "", c.Regex != "", c.Present != nil} {
		if set {
			forms++
		}
	}
	if forms != 1 {
		return HeaderMatcher{}, errcode.Errorf(CodeInvalid,
			"header %q criterion sets %d of exact, regex and present; want exactly one", c.Name, forms)
	}
	m := HeaderMatcher{
		Name:    lowerASCII(c.Name),
		Key:     textproto.CanonicalMIMEHeaderKey(c.Name),
		exact:   c.Exact,
		present: c.Present,
	}
	if c.Regex != "" {
		rx, err := CompileRegex(c.Regex)
		if err != nil {
			return HeaderMatcher{}, err
		}
		m.regex = rx
	}
	return m, nil
}

// isTokenChar reports whether r may appear in an RFC 9110 token.
func isTokenChar(r rune) bool {
	switch {
	case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		return true
	default:
		return strings.ContainsRune("!#$%&'*+-.^_`|~", r)
	}
}

// Match reports whether a request whose field lines for m.Name are values
// (nil or empty when the field is absent) passes m: exact equals the joined
// value, regex fully matches it, present true requires the field and present
// false its absence. Only a regex over a repeated field allocates.
func (m *HeaderMatcher) Match(values []string) bool {
	switch {
	case m.present != nil:
		return (len(values) > 0) == *m.present
	case len(values) == 0:
		return false
	case m.regex != nil:
		return m.regex.MatchString(JoinHeader(values))
	default:
		return joinedEquals(values, m.exact)
	}
}

// joinedEquals reports whether JoinHeader(values) == want without joining.
func joinedEquals(values []string, want string) bool {
	for i, v := range values {
		if i > 0 {
			var ok bool
			if want, ok = strings.CutPrefix(want, ", "); !ok {
				return false
			}
		}
		var ok bool
		if want, ok = strings.CutPrefix(want, v); !ok {
			return false
		}
	}
	return want == ""
}
