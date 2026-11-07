// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// spdxNode is a parsed SPDX license expression (SPDX 2.3 Annex D): a
// license term, or the AND or OR of two or more operands.
type spdxNode struct {
	// op is "AND" or "OR" for a compound expression and "" for a term.
	op string
	// args are the operands of AND and OR.
	args []*spdxNode
	// license is a term's license identifier or LicenseRef, as written.
	license string
	// plus marks "<license>+", that version or any later one.
	plus bool
	// exception is a term's WITH exception, or "".
	exception string
}

// parseSPDX parses an SPDX license expression. Operators are upper case,
// while identifiers match ignoring case (SPDX 2.3 Annex D.2); WITH binds
// tighter than AND, and AND tighter than OR. With legacySlash, "/" also
// separates alternatives, the OR that Cargo's manifest format still
// defines though it deprecates it.
func parseSPDX(s string, legacySlash bool) (*spdxNode, error) {
	toks, err := spdxTokens(s, legacySlash)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, errors.New("the expression is empty")
	}
	p := spdxParser{toks: toks}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	if tok := p.peek(); tok != "" {
		return nil, p.unexpected(tok)
	}
	return n, nil
}

// spdxTokens splits an expression into parentheses and words; with
// legacySlash a "/" becomes an OR.
func spdxTokens(s string, legacySlash bool) ([]string, error) {
	var out []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(' || c == ')':
			out = append(out, string(c))
			i++
		case c == '/' && legacySlash:
			out = append(out, "OR")
			i++
		case isSPDXWordByte(c):
			j := i
			for j < len(s) && isSPDXWordByte(s[j]) {
				j++
			}
			out = append(out, s[i:j])
			i = j
		default:
			r, _ := utf8.DecodeRuneInString(s[i:])
			return nil, fmt.Errorf("character %q is not part of an SPDX expression", r)
		}
	}
	return out, nil
}

// isSPDXWordByte reports whether c can be part of a license identifier,
// LicenseRef or exception: idstring characters, ":" of DocumentRef forms
// and the "+" suffix.
func isSPDXWordByte(c byte) bool {
	return isIDStringByte(c) || c == ':' || c == '+'
}

// isIDStringByte reports whether c is an SPDX idstring character: a
// letter, a digit, "-" or ".".
func isIDStringByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.'
}

// isIDString reports whether s is a non-empty SPDX idstring.
func isIDString(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !isIDStringByte(s[i]) {
			return false
		}
	}
	return true
}

// isRef reports whether s is "<kind>-<idstring>", optionally after
// "DocumentRef-<idstring>:", the form of LicenseRef and AdditionRef
// identifiers.
func isRef(s, kind string) bool {
	if doc, ref, ok := strings.Cut(s, ":"); ok {
		d, found := strings.CutPrefix(doc, "DocumentRef-")
		if !found || !isIDString(d) {
			return false
		}
		s = ref
	}
	id, found := strings.CutPrefix(s, kind+"-")
	return found && isIDString(id)
}

// spdxParser parses the tokens of one expression by recursive descent.
type spdxParser struct {
	toks []string
	pos  int
}

func (p *spdxParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *spdxParser) next() string {
	tok := p.peek()
	if tok != "" {
		p.pos++
	}
	return tok
}

// unexpected describes a token left over after a complete expression.
func (p *spdxParser) unexpected(tok string) error {
	switch {
	case tok == ")":
		return errors.New("a closing parenthesis has no opening one")
	case tok == "WITH":
		return errors.New("WITH follows a single license, which takes one exception")
	case slices.Contains([]string{"AND", "OR", "WITH"}, strings.ToUpper(tok)):
		return fmt.Errorf("%q is not an operator: SPDX operators are AND, OR and WITH in upper case", tok)
	}
	return fmt.Errorf("%q follows a license without an operator between them", tok)
}

// or parses operands separated by OR.
func (p *spdxParser) or() (*spdxNode, error) {
	return p.list("OR", p.and)
}

// and parses operands separated by AND.
func (p *spdxParser) and() (*spdxNode, error) {
	return p.list("AND", p.term)
}

// list parses operands that op separates, flattening nested operands of
// the same operator.
func (p *spdxParser) list(op string, operand func() (*spdxNode, error)) (*spdxNode, error) {
	var args []*spdxNode
	for {
		n, err := operand()
		if err != nil {
			return nil, err
		}
		if n.op == op {
			args = append(args, n.args...)
		} else {
			args = append(args, n)
		}
		if p.peek() != op {
			break
		}
		p.next()
	}
	if len(args) == 1 {
		return args[0], nil
	}
	return &spdxNode{op: op, args: args}, nil
}

// term parses a parenthesized expression or a license with an optional
// "+" and WITH exception.
func (p *spdxParser) term() (*spdxNode, error) {
	tok := p.next()
	switch tok {
	case "":
		return nil, errors.New("the expression ends where a license is expected")
	case "(":
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.next() != ")" {
			return nil, errors.New("an opening parenthesis is not closed")
		}
		if p.peek() == "WITH" {
			return nil, errors.New("WITH follows a single license, not a parenthesized expression")
		}
		return n, nil
	case ")", "AND", "OR", "WITH":
		return nil, fmt.Errorf("%q stands where a license is expected", tok)
	}
	n := &spdxNode{license: tok}
	if base, ok := strings.CutSuffix(tok, "+"); ok && isIDString(base) {
		n.license, n.plus = base, true
	} else if !isIDString(tok) && !isRef(tok, "LicenseRef") {
		return nil, fmt.Errorf("%q is not a license identifier or LicenseRef", tok)
	}
	if p.peek() != "WITH" {
		return n, nil
	}
	p.next()
	exc := p.next()
	switch {
	case exc == "":
		return nil, errors.New("the expression ends after WITH")
	case !isIDString(exc) && !isRef(exc, "AdditionRef"):
		return nil, fmt.Errorf("WITH is followed by %q, not a license exception", exc)
	}
	n.exception = exc
	return n, nil
}

// written renders a term as the expression wrote it.
func (n *spdxNode) written() string {
	s := n.license
	if n.plus {
		s += "+"
	}
	if n.exception != "" {
		s += " WITH " + n.exception
	}
	return s
}

// key is a term's admission key: its license without "+", since the
// version a "+" term names is one of its choices, and its WITH exception,
// lower-cased because SPDX identifiers match ignoring case.
func (n *spdxNode) key() string {
	s := n.license
	if n.exception != "" {
		s += " WITH " + n.exception
	}
	return strings.ToLower(s)
}

// String renders the expression canonically: one space around operators
// and parentheses only around an OR inside an AND.
func (n *spdxNode) String() string {
	if n.op == "" {
		return n.written()
	}
	parts := make([]string, len(n.args))
	for i, a := range n.args {
		parts[i] = a.String()
		if n.op == "AND" && a.op == "OR" {
			parts[i] = "(" + parts[i] + ")"
		}
	}
	return strings.Join(parts, " "+n.op+" ")
}

// resolves reports whether the expression resolves to terms admit accepts:
// a term when admit accepts its key, AND when every operand resolves, OR
// when one does.
func (n *spdxNode) resolves(admit func(key string) bool) bool {
	switch n.op {
	case "AND":
		for _, a := range n.args {
			if !a.resolves(admit) {
				return false
			}
		}
		return true
	case "OR":
		return slices.ContainsFunc(n.args, func(a *spdxNode) bool { return a.resolves(admit) })
	default:
		return admit(n.key())
	}
}

// blocking returns the terms, as written and once each, that keep an
// expression from resolving: a refused term, those of the AND operands
// that do not resolve, and those of every operand of an OR none of whose
// operands resolves.
func (n *spdxNode) blocking(admit func(key string) bool) []string {
	var out []string
	add := func(terms []string) {
		for _, t := range terms {
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	switch n.op {
	case "AND", "OR":
		if n.op == "OR" && n.resolves(admit) {
			return nil
		}
		for _, a := range n.args {
			if !a.resolves(admit) {
				add(a.blocking(admit))
			}
		}
	default:
		if !admit(n.key()) {
			out = []string{n.written()}
		}
	}
	return out
}

// keys returns the admission keys of every term of the expression, once
// each, in order.
func (n *spdxNode) keys() []string {
	if n.op == "" {
		return []string{n.key()}
	}
	var out []string
	for _, a := range n.args {
		for _, k := range a.keys() {
			if !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	return out
}

// spdxTermKey parses a policy entry that names one license term, such as
// an addendum license or an election, and returns its admission key.
func spdxTermKey(term string) (string, error) {
	n, err := parseSPDX(term, false)
	if err != nil {
		return "", err
	}
	if n.op != "" {
		return "", fmt.Errorf("%q is an expression, not one license term", term)
	}
	return n.key(), nil
}
