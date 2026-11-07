// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package openapi

import (
	"errors"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// reference is a parsed $ref value: a file and a JSON Pointer into it.
type reference struct {
	// file is the decoded relative path of the reference's file part,
	// slash-separated; "" names the file the reference is written in.
	file string
	// pointer holds the unescaped tokens of the fragment's JSON Pointer;
	// nil names the document's root.
	pointer []string
}

// Reasons a $ref value does not parse. Each names the rule it breaks.
var (
	errRefEmpty     = errors.New("an empty reference")
	errRefScheme    = errors.New("a URI with a scheme; only references within the document and to relative files resolve")
	errRefAbsolute  = errors.New("an absolute path; only references within the document and to relative files resolve")
	errRefBackslash = errors.New("a backslash; a URI reference separates its path segments with /")
	errRefQuery     = errors.New("a query, which no file reference holds")
	errRefEscape    = errors.New("an invalid percent escape")
	errRefNUL       = errors.New("a NUL byte")
	errRefPlainName = errors.New("a plain-name fragment; only JSON Pointer fragments resolve")
	errRefTilde     = errors.New("a ~ in its JSON Pointer that is neither ~0 nor ~1")
)

// parseRef parses the $ref value s: an optional relative file path, then
// an optional fragment that holds a JSON Pointer (RFC 6901), both percent
// decoded (RFC 3986). A reference with a scheme, such as an http URL, an
// absolute or network path, a query, a backslash or a plain-name fragment
// is refused, as written and once decoded, so an escape cannot smuggle
// one in: references resolve only within the document and to files
// relative to the file that holds them, and never reach the network.
func parseRef(s string) (reference, error) {
	if s == "" {
		return reference{}, errRefEmpty
	}
	part, frag, _ := strings.Cut(s, "#")
	var r reference
	if part != "" {
		if strings.Contains(part, "?") {
			return reference{}, errRefQuery
		}
		if err := checkFilePart(part); err != nil {
			return reference{}, err
		}
		p, err := url.PathUnescape(part)
		if err != nil {
			return reference{}, errRefEscape
		}
		if strings.ContainsRune(p, 0) {
			return reference{}, errRefNUL
		}
		if err := checkFilePart(p); err != nil {
			return reference{}, err
		}
		r.file = p
	}
	f, err := url.PathUnescape(frag)
	if err != nil {
		return reference{}, errRefEscape
	}
	if f == "" {
		return r, nil
	}
	if !strings.HasPrefix(f, "/") {
		return reference{}, errRefPlainName
	}
	for tok := range strings.SplitSeq(f[1:], "/") {
		u, ok := unescapeToken(tok)
		if !ok {
			return reference{}, errRefTilde
		}
		r.pointer = append(r.pointer, u)
	}
	return r, nil
}

// checkFilePart refuses a reference's file part that has a scheme, is an
// absolute or network path, or holds a backslash.
func checkFilePart(p string) error {
	switch {
	case hasScheme(p):
		return errRefScheme
	case strings.HasPrefix(p, "/"):
		return errRefAbsolute
	case strings.Contains(p, `\`):
		return errRefBackslash
	}
	return nil
}

// hasScheme reports whether s starts with a URI scheme and its colon
// (RFC 3986 section 3.1). A Windows drive letter, as in C:/x, reads as one
// and is refused with the other absolute forms.
func hasScheme(s string) bool {
	i := strings.IndexByte(s, ':')
	if i < 1 {
		return false
	}
	for j := range i {
		c := s[j]
		alpha := c|0x20 >= 'a' && c|0x20 <= 'z'
		if !alpha && (j == 0 || (c < '0' || c > '9') && c != '+' && c != '-' && c != '.') {
			return false
		}
	}
	return true
}

// unescapeToken undoes the JSON Pointer escapes of one token: ~1 is / and
// ~0 is ~, applied in one pass so ~01 is ~1. Any other ~ fails.
func unescapeToken(tok string) (string, bool) {
	if !strings.Contains(tok, "~") {
		return tok, true
	}
	var b strings.Builder
	for i := 0; i < len(tok); i++ {
		if tok[i] != '~' {
			b.WriteByte(tok[i])
			continue
		}
		if i+1 >= len(tok) {
			return "", false
		}
		switch tok[i+1] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", false
		}
		i++
	}
	return b.String(), true
}

// escapeToken writes a JSON Pointer token: ~ as ~0 and / as ~1.
func escapeToken(tok string) string {
	return strings.ReplaceAll(strings.ReplaceAll(tok, "~", "~0"), "/", "~1")
}

// pointerText returns the JSON Pointer of tokens.
func pointerText(tokens []string) string {
	var b strings.Builder
	for _, t := range tokens {
		b.WriteByte('/')
		b.WriteString(escapeToken(t))
	}
	return b.String()
}

// walk follows the JSON Pointer tokens from n: a member name on a
// mapping, a decimal index without leading zeros on a list. It returns
// the node, or nil and the index of the first token that names nothing.
func walk(n *tree.Node, tokens []string) (*tree.Node, int) {
	cur := n
	for i, t := range tokens {
		switch {
		case cur == nil:
			return nil, i
		case cur.Kind == tree.KindMap:
			next, ok := cur.Get(t)
			if !ok {
				return nil, i
			}
			cur = next
		case cur.Kind == tree.KindList:
			j, ok := listIndex(t, len(cur.Items))
			if !ok {
				return nil, i
			}
			cur = cur.Items[j]
		default:
			return nil, i
		}
	}
	return cur, -1
}

// listIndex parses a JSON Pointer array index below n: "0", or a decimal
// without a leading zero.
func listIndex(t string, n int) (int, bool) {
	if t == "" || len(t) > 1 && t[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(t); i++ {
		if t[i] < '0' || t[i] > '9' {
			return 0, false
		}
	}
	j, err := strconv.Atoi(t)
	if err != nil || j >= n {
		return 0, false
	}
	return j, true
}

// target is the node a reference resolved to, with its identity: its file
// and JSON Pointer, which tells a cycle.
type target struct {
	node *tree.Node
	key  string
}

// resolve returns the node the $ref value of the string node at names: a
// node of at's own file, or of the file its relative path names from the
// directory of at's file. A value that does not parse, a file that cannot
// be read and a pointer that names nothing are problems at at, and ok is
// false; err is only errStop or ctx's end.
func (l *load) resolve(at *tree.Node) (t target, ok bool, err error) {
	r, perr := parseRef(at.Text)
	if perr != nil {
		l.add(l.at(at), "$ref %s is %v", quote(at.Text), perr)
		return target{}, false, nil
	}
	base := l.fileAt(at)
	if base == nil {
		l.add(l.at(at), "$ref %s is in no file of the load", quote(at.Text))
		return target{}, false, nil
	}
	f := base
	if r.file != "" {
		p := filepath.Clean(filepath.Join(filepath.Dir(base.path), filepath.FromSlash(r.file)))
		if f, err = l.open(p, l.at(at), false); err != nil {
			return target{}, false, err
		}
	}
	if f.root == nil {
		// Its problems are reported already.
		return target{}, false, nil
	}
	n, bad := walk(f.root, r.pointer)
	if n == nil {
		l.add(l.at(at), "$ref %s names nothing: %s holds no %s", quote(at.Text),
			displayName(f.path), quote(pointerText(r.pointer[:bad+1])))
		return target{}, false, nil
	}
	return target{node: n, key: strconv.Itoa(int(f.id)) + "#" + pointerText(r.pointer)}, true, nil
}

// deref follows Reference Objects from n: while n is a mapping whose $ref
// member is set, n becomes the node that member names, so a chain of
// references ends at the object it leads to. Members beside $ref are
// ignored, as a Reference Object's are. A $ref that is not a string, a
// reference that does not resolve and a chain that returns to a node it
// passed are problems, and the result is nil; err is only errStop or
// ctx's end.
func (l *load) deref(n *tree.Node) (*tree.Node, error) {
	var seen map[string]bool
	for {
		r, ok := n.Get("$ref")
		if !ok {
			return n, nil
		}
		if r.Kind != tree.KindString {
			l.add(l.at(r), "$ref is a %s, not a string", kindName(r))
			return nil, nil
		}
		t, ok, err := l.resolve(r)
		if err != nil || !ok {
			return nil, err
		}
		if seen == nil {
			seen = map[string]bool{}
		}
		if seen[t.key] {
			l.add(l.at(r), "$ref %s closes a reference cycle that names no object", quote(r.Text))
			return nil, nil
		}
		seen[t.key] = true
		n = t.node
	}
}

// maxQuoted is the most code points of a document's value a problem
// quotes, as configuration diagnostics quote authored values.
const maxQuoted = 64

// quote returns s as a Go string literal of at most maxQuoted of its code
// points, "..." marking a longer value.
func quote(s string) string {
	if r := []rune(s); len(r) > maxQuoted {
		s = string(r[:maxQuoted]) + "..."
	}
	return strconv.Quote(s)
}

// kindName names the JSON type of n in problems.
func kindName(n *tree.Node) string {
	if n == nil {
		return "null"
	}
	switch n.Kind {
	case tree.KindBool:
		return "boolean"
	case tree.KindInt, tree.KindFloat:
		return "number"
	case tree.KindString:
		return "string"
	case tree.KindMap:
		return "mapping"
	case tree.KindList:
		return "list"
	default:
		return "null"
	}
}
