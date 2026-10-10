// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/scanner"
	"github.com/goccy/go-yaml/token"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// goccyError is the error interface goccy's parser errors implement
// (yaml.SyntaxError among them). Matching the interface instead of
// *yaml.SyntaxError keeps the reflection-based root package of goccy out of
// every binary.
type goccyError interface {
	error
	GetToken() *token.Token
	GetMessage() string
}

// parseYAML runs the restricted YAML profile over one file: byte
// pre-checks, then for each document the token pass and, while the file
// has no token-pass finding, the parse and typing (01 B 7-14).
func (p *fileParser) parseYAML(ctx context.Context, src []byte) {
	if !p.checkSize(src) {
		return
	}
	text, ok := p.precheckYAML(src)
	if !ok {
		return
	}
	if text != "" && text[len(text)-1] != '\n' {
		// The text ends with a line break, as the YAML Test Suite reads a
		// stream that does not (L24T/01, JEF9/02): goccy's scanner drops a
		// tag that runs to the end of its input ("k: !e" gave no RZ-CFG-004)
		// and the trailing white space of a block scalar's last line there.
		// No position changes.
		text += "\n"
	}
	pass := &tokenPass{p: p, maxDepth: p.opts.MaxDepth, maxTokens: p.opts.MaxDocumentTokens, splitAt: p.opts.splitAt}
	if p.opts.passDepth > 0 {
		pass.maxDepth = p.opts.passDepth
	}
	// Parse findings and documents are held apart: a file with any
	// token-pass finding is not parsed (01 req 8), so they are dropped then.
	var parsed diag.List
	var docs []Document
	for _, seg := range splitDocuments(text) {
		if p.canceled(ctx) {
			return
		}
		if p.opts.Yield != nil {
			p.opts.Yield()
		}
		tks, ok := p.scan(seg, pass)
		if !ok {
			break
		}
		cols := columnsOf(tks, seg.text)
		pass.run(tks, seg.startLine-1, cols, seg.text)
		if pass.fatal {
			break
		}
		if pass.errs > 0 {
			continue
		}
		before := len(p.diags)
		p.parseSegment(tks, seg, pass.splits, cols, pass.fixes)
		parsed = append(parsed, p.diags[before:]...)
		p.diags = p.diags[:before]
		docs = append(docs, p.docs...)
		p.docs = nil
		if p.full {
			break
		}
	}
	if pass.errs > 0 {
		return
	}
	p.diags = append(p.diags, parsed...)
	p.docs = docs
}

// scan tokenizes one document with goccy's scanner, after bounding its
// token count from its bytes (estimateTokens): a document whose bound
// passes MaxDocumentTokens is refused before any token is materialized, so
// a flood of one-character tokens costs nothing (01 risk 1); the token
// pass checks the exact count. The bound over-counts indicators inside
// quoted and block scalars, so its finding says the count is an estimate.
// It reports false when the file must stop.
//
// A document holding a long run of blank lines is scanned with its
// clipping block scalars rewritten to keep chomping (chomp.go), which only
// spares goccy its own clipping: the token pass reads every block scalar's
// value from the source. The blank lines that end a document are not
// scanned (trimBlankTail), a tab before a quoted key's ':' is scanned as a
// space (untabSeparators), and a keep-chomping block scalar of blank lines
// takes strip chomping (stripEmptyKeep). A document is scanned again with
// the tabs of its double-quoted scalars written as spaces (untabQuotes) and
// with a tab that goccy runs into the tag before it written as a space
// (untabTags), when goccy's scan of it holds such tokens. Each rewrite
// changes one byte for one, so goccy reports every token at the position it
// has in the source, and no value changes: the token pass reads every
// quoted and block scalar, and every plain scalar holding a tab, from the
// source.
func (p *fileParser) scan(seg segment, pass *tokenPass) (token.Tokens, bool) {
	if n, over := estimateTokens(seg.text, p.opts.MaxDocumentTokens); over >= 0 && n > p.opts.MaxDocumentTokens {
		at := segmentPos(seg, over)
		pass.fail(p.pos(at.line, at.col), "document is too large to tokenize: estimated more than "+strconv.Itoa(p.opts.MaxDocumentTokens)+" tokens")
		return nil, false
	}
	text := seg.text
	if !p.opts.noRewrite && blankRuns(text, longBlankRun) {
		text, _ = keepChomping(text)
	}
	text = untabSeparators(trimBlankTail(text))
	text, _ = stripEmptyKeep(text)
	tks, ok := p.scanText(text, seg, pass)
	if ok && slices.ContainsFunc(tks, func(tk *token.Token) bool {
		return tk.Type == token.DoubleQuoteType && strings.IndexByte(tk.Origin, '\t') >= 0
	}) {
		if untabbed, n := untabQuotes(text); n > 0 {
			text = untabbed
			tks, ok = p.scanText(text, seg, pass)
		}
	}
	if ok {
		if untabbed, n := untabTags(tks, text); n > 0 {
			tks, ok = p.scanText(untabbed, seg, pass)
		}
	}
	return tks, ok
}

// untabQuotes writes as a space every tab inside a double-quoted scalar of
// text that follows other text on its line, and returns the text and the
// number of tabs written (01 req 8, 12, 14). For each such tab goccy's
// scanner steps one character too far, past the scalar's end, so it lost
// the line break after "a: \"x\ty\"" and read "b: 1" on the next line as
// on the scalar's line: it refused "a: \"x\ty\"\nb: 1", which YAML 1.2
// accepts, read a "..." line after the scalar as a plain scalar, and moved
// every later token a line up. The token pass reads every quoted scalar from
// the source (quoted.go), so the value keeps its tabs, and a tab and a space
// are one byte and one column each. A tab that indents a continuation line
// stays: goccy refuses it where YAML 1.2 does (YAML Test Suite DK95/01).
// The scalars are found in goccy's scan of the text with every tab written
// as a space, which the fault does not touch; a tab separates tokens as a
// space does, so that scan finds the same scalars.
func untabQuotes(text string) (string, int) {
	var s scanner.Scanner
	spaced := strings.ReplaceAll(text, "\t", " ")
	var tks token.Tokens
	func() {
		// A panic in goccy ends the search; the full scan reports it.
		defer func() { _ = recover() }()
		s.Init(spaced)
		for {
			sub, err := s.Scan()
			tks.Add(sub...)
			if err != nil || len(sub) == 0 {
				return
			}
		}
	}()
	cols := columnsOf(tks, spaced)
	cur := newCursor(text)
	b := []byte(text)
	n := 0
	for _, tk := range tks {
		if tk.Type != token.DoubleQuoteType || tk.Position == nil {
			continue
		}
		off := cur.offset(tk.Position.Line, cols.fix(tk.Position.Line, tk.Position.Column))
		if off < 0 || text[off] != '"' {
			continue
		}
		end := quoteEnd(text, off)
		if end < 0 {
			end = len(text)
		}
		leading := false
		for i := off + 1; i < end; i++ {
			switch c := text[i]; {
			case c == '\n':
				leading = true
			case c == '\t' && !leading:
				b[i] = ' '
				n++
			case c != ' ' && c != '\t':
				leading = false
			}
		}
	}
	if n == 0 {
		return text, 0
	}
	return string(b), n
}

// untabTags writes as a space the tab that ends each tag of a segment's
// tokens, scanned from text, and returns the text and the number of tabs
// written (01 req 12, 15). goccy's scanner ends a tag at a space or a line
// break only, so it reads "!!str\tx" as one tag and "[!e\tr]" as the tag
// "!e\tr]", where YAML 1.2 ends the tag at the tab, which separates it
// from its node as a space does. The space changes no position, and no
// value: values are read from the source, which keeps the tab.
func untabTags(tks token.Tokens, text string) (string, int) {
	var cols columnTable
	var b []byte
	cur := newCursor(text)
	n := 0
	for _, tk := range tks {
		tab := strings.IndexByte(tk.Value, '\t')
		if tk.Type != token.TagType || tab < 0 || tk.Position == nil {
			continue
		}
		if b == nil {
			cols, b = columnsOf(tks, text), []byte(text)
		}
		off := cur.offset(tk.Position.Line, cols.fix(tk.Position.Line, tk.Position.Column))
		if off < 0 || !strings.HasPrefix(text[off:], tk.Value) {
			continue
		}
		b[off+tab] = ' '
		n++
	}
	if n == 0 {
		return text, 0
	}
	return string(b), n
}

// trimBlankTail returns text without the blank lines (spaces only, chomp.go)
// that end it, keeping the line break of its last other line. They hold no
// token, and goccy's scanner refuses them after a block scalar with an
// indentation indicator ("a: |2\n  x\n\n" is "invalid number of indent is
// specified in the multi-line header"), which a document without them
// passes. The token pass reads every block scalar, and the empty lines a
// keep-chomping one keeps, from the source (literal.go), and no other token
// moves (01 req 8, 12).
func trimBlankTail(text string) string {
	end := len(text)
	for end > 0 {
		start := strings.LastIndexByte(text[:end-1], '\n') + 1
		if end-1 < start || !blankLine(text[start:end]) {
			break
		}
		end = start
	}
	return text[:end]
}

// untabSeparators writes as a space every tab between a quoted scalar and
// the ':' after it, which goccy's scanner refuses ("tab character cannot use
// as a map key directly") where YAML 1.2 separates the key from its ':' with
// white space (01 req 8, 15: "{\"a\"\t: 1}" is a JSON file, and its YAML
// twin gives the same tree). The rewrite needs no token: a tab between a
// closing quote and a ':' is separation white space between a quoted key and
// its ':', or lies inside a quoted scalar, a block scalar or a comment,
// whose values the token pass reads from the source. A tab and a space are
// one byte and one column each, so no position changes. A line of white
// space holding a tab stays as it is: goccy refuses it outside a flow
// collection, which YAML 1.2 allows between nodes but not inside a plain
// scalar that indents too little ("a: b\n\t\n c"), and the scanner's
// tokens do not tell the two apart before the line.
func untabSeparators(text string) string {
	var b []byte
	for i := strings.IndexByte(text, ':'); i >= 0; {
		j := i - 1
		for j >= 0 && (text[j] == ' ' || text[j] == '\t') {
			j--
		}
		if j >= 0 && (text[j] == '"' || text[j] == '\'') {
			for k := j + 1; k < i; k++ {
				if text[k] != '\t' {
					continue
				}
				if b == nil {
					b = []byte(text)
				}
				b[k] = ' '
			}
		}
		next := strings.IndexByte(text[i+1:], ':')
		if next < 0 {
			break
		}
		i += next + 1
	}
	if b == nil {
		return text
	}
	return string(b)
}

// scanText tokenizes the text of one segment with goccy's scanner.
func (p *fileParser) scanText(text string, seg segment, pass *tokenPass) (tks token.Tokens, ok bool) {
	defer func() {
		// goccy is third-party code fed untrusted input; a panic in it
		// must not take down the process (11 req 26).
		if r := recover(); r != nil {
			pass.fail(p.pos(seg.startLine, 1), fmt.Sprintf("syntax error: YAML scanner failed: %v", r))
			tks, ok = nil, false
		}
	}()
	var s scanner.Scanner
	s.Init(text)
	for {
		sub, err := s.Scan()
		tks.Add(sub...)
		if errors.Is(err, io.EOF) {
			return tks, true
		}
		if err != nil {
			// The scanner appends its InvalidType token, which the token
			// pass reports; scanning past it would repeat the failure.
			if len(sub) == 0 || sub[len(sub)-1].Type != token.InvalidType {
				pass.fail(p.pos(seg.startLine, 1), "syntax error: "+err.Error())
				return nil, false
			}
			return tks, true
		}
		if len(sub) == 0 {
			return tks, true
		}
	}
}

// linecol is a 1-based line and code-point column.
type linecol struct{ line, col int }

// segmentPos returns the file position of byte offset off of a segment.
func segmentPos(seg segment, off int) linecol {
	line, col := seg.startLine, 1
	for i := 0; i < off; {
		if seg.text[i] == '\n' {
			line, col = line+1, 1
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(seg.text[i:])
		col++
		i += size
	}
	return linecol{line, col}
}

// parseSegment parses one document's tokens and converts every document
// they hold into a tree (01 req 10). A document holding a block collection
// takes the split parse (split.go), and goccy's parser never sees its
// tokens whole: when they hold more than one document body, which the
// token pass and the document cut rule out, the segment is refused
// (refuseShape). cols corrects the columns goccy gives the tokens
// (columns.go), and fixed holds the token pass's readings of the scalars
// goccy reads otherwise (source.go).
func (p *fileParser) parseSegment(tks token.Tokens, seg segment, splits map[int]*blockFrame, cols columnTable, fixed fixes) {
	lineOff := seg.startLine - 1
	if len(splits) > 0 {
		lo, hi, header, bad := bodyRange(tks)
		if bad >= 0 {
			c := converter{p: p, lineOff: lineOff, cols: cols, fixes: fixed}
			c.refuseShape(tks, bad)
			return
		}
		if lo >= 0 {
			c := converter{p: p, lineOff: lineOff, cols: cols, fixes: fixed}
			if header != nil {
				// goccy checks the directives before "---" on its own.
				if _, err := parseBody(tks[:slices.Index(tks, header)+1]); err != nil {
					p.add(p.errorAt(codeParse, c.syntaxPos(err, header), "syntax error: "+syntaxMessage(err)))
					return
				}
			}
			s := splitter{c: &c, tks: tks, frames: splits, bodyHi: hi}
			// A block collection cannot start on the "---" line.
			root := s.rangeNode(lo, hi, header, true, 0)
			if c.failed {
				return
			}
			start := root.Pos
			if header != nil {
				start = c.tokenPos(header)
			}
			p.keep(Document{Root: root, Start: start})
			return
		}
	}
	var file *ast.File
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("YAML parser failed: %v", r)
			}
		}()
		file, err = parser.Parse(tks, 0, parser.AllowDuplicateMapKey())
		return err
	}()
	if err != nil {
		c := converter{p: p, lineOff: lineOff, cols: cols, fixes: fixed}
		p.add(p.errorAt(codeParse, c.syntaxPos(err, nil), "syntax error: "+syntaxMessage(err)))
		return
	}
	for _, d := range file.Docs {
		if d == nil || d.Body == nil {
			continue
		}
		if _, ok := d.Body.(*ast.DirectiveNode); ok {
			// goccy returns the directives before "---" as a document of
			// their own; the token pass has checked them.
			continue
		}
		c := converter{p: p, lineOff: lineOff, cols: cols, fixes: fixed}
		root := c.node(d.Body)
		if c.failed {
			continue
		}
		start := root.Pos
		if d.Start != nil {
			start = c.tokenPos(d.Start)
		}
		p.keep(Document{Root: root, Start: start})
	}
}

// bodyRange returns the token range [lo, hi) of the one document body in
// tks, after its directives and "---", and the "---" token; lo is -1 when
// tks hold no body. bad is the index of the first token that breaks the
// shape of one document, or -1: a directive after "---" or after the body
// ("--- %0" and "a: 1\n%x" scan so), a second "---" or "...", a token
// after "...", or the first token of a body after directives without
// "---".
func bodyRange(tks token.Tokens) (lo, hi int, header *token.Token, bad int) {
	lo, hi = -1, len(tks)
	directiveLine := -1
	for i, tk := range tks {
		switch {
		case tk.Type == token.CommentType:
		case tk.Type == token.DirectiveType:
			if lo >= 0 || header != nil {
				return 0, 0, nil, i
			}
			directiveLine = tk.Position.Line
		case lo < 0 && tk.Position.Line == directiveLine:
			// A directive's name and parameters.
		case tk.Type == token.DocumentHeaderType:
			if lo >= 0 || header != nil {
				return 0, 0, nil, i
			}
			header = tk
		case hi != len(tks):
			return 0, 0, nil, i
		case tk.Type == token.DocumentEndType:
			hi = i
		case lo < 0:
			if directiveLine >= 0 && header == nil {
				return 0, 0, nil, i
			}
			lo = i
		}
	}
	return lo, hi, header, -1
}

// refuseShape refuses the tokens of a document with block collections
// that do not hold one document body, at token bad (bodyRange), without
// parsing the body. A directive out of place is refused with goccy's own
// message, from goccy's parse of the directive's line, or of the
// directives before the body, alone: goccy refuses them while it groups
// tokens, whatever comes after. Anything else is RZ-CFG-001 at the token.
func (c *converter) refuseShape(tks token.Tokens, bad int) {
	ended := slices.ContainsFunc(tks[:bad], func(tk *token.Token) bool {
		return tk.Type == token.DocumentHeaderType || tk.Type == token.DocumentEndType
	})
	var probe token.Tokens
	msg := `syntax error: directives must be followed by "---"`
	switch {
	case tks[bad].Type == token.DirectiveType:
		end := bad + 1
		for end < len(tks) && tks[end].Position.Line == tks[bad].Position.Line {
			end++
		}
		probe, msg = tks[bad:end], `syntax error: a directive must come before the document's "---"`
	case tks[bad].Type == token.DocumentHeaderType:
		msg = `syntax error: a document marker ("---") inside a document`
	case ended:
		msg = msgAfterMarker
	default:
		// Directives, then a body without "---".
		probe = tks[:bad]
	}
	if probe != nil {
		if _, err := parseBody(probe); err != nil {
			c.fail(codeParse, c.syntaxPos(err, tks[bad]), "syntax error: "+syntaxMessage(err))
			return
		}
	}
	c.fail(codeParse, c.tokenPos(tks[bad]), msg)
}

// syntaxMessage returns the message of a goccy error.
func syntaxMessage(err error) string {
	var ge goccyError
	if errors.As(err, &ge) {
		return ge.GetMessage()
	}
	return err.Error()
}

// converter turns one goccy document into a tree, typing scalars by the
// core schema (01 req 12) and reporting duplicate keys (01 req 11).
type converter struct {
	p       *fileParser
	lineOff int
	failed  bool
	// cols corrects goccy's columns (columns.go).
	cols columnTable
	// fixes are the token pass's readings of the scalars goccy reads
	// otherwise (source.go): their values and positions win.
	fixes fixes
	// depth counts the collections open around the node being converted;
	// deep is set by the first collection deeper than MaxDepth (enter).
	depth int
	deep  bool
}

// enter opens a collection starting at at, as the second bound of 01 req 9
// behind the token pass (11 req 17, 26). The depth of the tree is counted
// as it is built, in goccy's parse of a value and in the split parse alike:
// the first collection deeper than MaxDepth fails the document with
// RZ-CFG-001, as the token pass words it, and nothing more of the document
// is converted, so no tree deeper than MaxDepth is ever returned and the
// conversion never recurses past it. This bounds the returned tree only:
// goccy has parsed the value, built every node and path of it and recursed
// once per level before the conversion starts. The cost of that parse
// rests on the token pass, which predicts goccy's nesting from columns in
// block context and must predict it exactly inside a flow collection,
// which goccy parses whole (flowcost.go, flowentry.go): every shape it
// mispredicted (tagged empty nodes, multi-line implicit keys, explicit
// keys, flow entries without ',') let goccy build a tree deeper than
// MaxDepth, at a cost of up to 3,747 MiB, before this bound refused it.
// enter reports whether the collection may be converted; leave closes it.
func (c *converter) enter(at tree.Pos) bool {
	if c.deep {
		return false
	}
	if c.depth >= c.p.opts.MaxDepth {
		c.deep = true
		c.fail(codeParse, at, "nesting depth exceeds "+strconv.Itoa(c.p.opts.MaxDepth))
		return false
	}
	c.depth++
	return true
}

// leave closes the collection enter opened.
func (c *converter) leave() { c.depth-- }

// collectionPos returns where a collection node starts: its '[', '{' or
// first '-', or the first token of its first entry ('?', a tag or the
// key), which is where the split parse's frame starts too.
func (c *converter) collectionPos(n ast.Node) tree.Pos {
	if v, ok := n.(*ast.MappingNode); ok && !v.IsFlowStyle && len(v.Values) > 0 && v.Values[0].Key != nil {
		return c.tokenPos(v.Values[0].Key.GetToken())
	}
	return c.tokenPos(n.GetToken())
}

// tokenPos returns the file position of a token, with its column
// corrected for the tags before it on its line, or the position the token
// pass read it at.
func (c *converter) tokenPos(tk *token.Token) tree.Pos {
	if tk == nil || tk.Position == nil {
		return tree.Pos{}
	}
	if f, ok := c.fixes[tk]; ok && f.pos.Known() {
		return f.pos
	}
	return c.p.pos(tk.Position.Line+c.lineOff, max(c.cols.fix(tk.Position.Line, tk.Position.Column), 1))
}

// text returns the value of a scalar token: the token pass's reading when
// it has one, otherwise goccy's.
func (c *converter) text(tk *token.Token) string {
	if f, ok := c.fixes[tk]; ok && f.set {
		return f.text
	}
	return tk.Value
}

// syntaxPos returns the position of a goccy error's token, or of
// fallback, or the document start.
func (c *converter) syntaxPos(err error, fallback *token.Token) tree.Pos {
	var ge goccyError
	if errors.As(err, &ge) {
		if tk := ge.GetToken(); tk != nil && tk.Position != nil {
			return c.tokenPos(tk)
		}
	}
	if fallback != nil && fallback.Position != nil {
		return c.tokenPos(fallback)
	}
	return c.p.pos(c.lineOff+1, 1)
}

// fail records an error diagnostic for this document.
func (c *converter) fail(code string, at tree.Pos, msg string) {
	c.p.add(c.p.errorAt(code, at, msg))
	c.failed = true
}

// node converts a value node. Once a collection is too deep (enter), the
// loops over collections convert nothing more of the document.
func (c *converter) node(n ast.Node) *tree.Node {
	c.p.built()
	switch v := n.(type) {
	case nil:
		return &tree.Node{Kind: tree.KindNull}
	case *ast.MappingNode:
		if !c.enter(c.collectionPos(v)) {
			return &tree.Node{Kind: tree.KindNull}
		}
		defer c.leave()
		m := &tree.Node{Kind: tree.KindMap, Members: make([]tree.Member, 0, len(v.Values))}
		if v.IsFlowStyle {
			m.Pos = c.tokenPos(v.Start)
		}
		c.members(m, v.Values)
		return m
	case *ast.MappingValueNode:
		// goccy v1.19.2 returns a mapping as a MappingNode and keeps its
		// MappingValueNodes inside it; one on its own is no node of a tree.
		at := c.tokenPos(v.GetToken())
		c.fail(codeParse, at, "syntax error: unexpected mapping value")
		return &tree.Node{Kind: tree.KindNull, Pos: at}
	case *ast.SequenceNode:
		if !c.enter(c.collectionPos(v)) {
			return &tree.Node{Kind: tree.KindNull}
		}
		defer c.leave()
		l := &tree.Node{Kind: tree.KindList, Pos: c.tokenPos(v.Start), Items: make([]*tree.Node, 0, len(v.Values))}
		for i, it := range v.Values {
			if c.deep {
				break
			}
			item := c.node(it)
			if i < len(v.Entries) && v.Entries[i] != nil && v.Entries[i].Start != nil && v.Entries[i].Start.Type == token.SequenceEntryType {
				c.placeNull(item, it, v.Entries[i].Start)
			}
			l.Items = append(l.Items, item)
		}
		return l
	case *ast.TagNode:
		return c.tagged(v)
	case *ast.LiteralNode:
		style := tree.StyleLiteral
		if v.Start != nil && v.Start.Type == token.FoldedType {
			style = tree.StyleFolded
		}
		return &tree.Node{Kind: tree.KindString, Style: style, Pos: c.tokenPos(v.Start), Text: c.literalText(v)}
	case *ast.AnchorNode, *ast.AliasNode, *ast.MergeKeyNode:
		// The token pass rejects these before parsing; kept for safety.
		at := c.tokenPos(n.GetToken())
		c.fail(codeAnchor, at, "anchors, aliases and merge keys are not allowed")
		return &tree.Node{Kind: tree.KindNull, Pos: at}
	default:
		tk := n.GetToken()
		if tk == nil {
			c.fail(codeParse, tree.Pos{}, "syntax error: unexpected "+n.Type().String()+" node")
			return &tree.Node{Kind: tree.KindNull}
		}
		return c.scalar(tk, "", tree.Pos{})
	}
}

// members appends the entries of a mapping, up to a value too deep
// (enter). A key is a scalar or refused, which never opens a collection.
func (c *converter) members(m *tree.Node, values []*ast.MappingValueNode) {
	seen := make(map[string]tree.Pos, len(values))
	for _, mv := range values {
		key, keyPos, ok := c.key(mv.Key)
		if !ok {
			continue
		}
		if len(m.Members) == 0 && !m.Pos.Known() {
			m.Pos = keyPos
		}
		value := c.node(mv.Value)
		if c.deep {
			return
		}
		c.placeNull(value, mv.Value, mv.Start)
		c.addMember(m, seen, key, keyPos, value)
	}
}

// placeNull places the implicit null goccy's parser makes for an empty
// value, n converted from v, in the column after the token it follows,
// after: the entry's ':' or '-', or the first token of a key with no ':'
// (01 req 14). goccy puts it one column further at the end of its tokens
// than inside them, so the null's position depended on what came after the
// entry ("k1:" alone and before "w0: v"); the split parse places it the same
// way (splitter.empty).
func (c *converter) placeNull(n *tree.Node, v ast.Node, after *token.Token) {
	if _, ok := v.(*ast.NullNode); !ok || n.Kind != tree.KindNull || after == nil {
		return
	}
	if tk := v.GetToken(); tk != nil && tk.Type == token.ImplicitNullType {
		n.Pos = nextColumn(c.tokenPos(after))
	}
}

// nextColumn returns the position one column after at, or the zero position
// for an unknown one.
func nextColumn(at tree.Pos) tree.Pos {
	if at.Known() {
		at.Column++
	}
	return at
}

// addMember appends a member to m, reporting a duplicate key at the
// second key with the first as related location (01 req 11); the first
// entry of a key is kept.
func (c *converter) addMember(m *tree.Node, seen map[string]tree.Pos, key string, keyPos tree.Pos, value *tree.Node) {
	if first, dup := seen[key]; dup {
		d := c.p.errorAt(codeDuplicate, keyPos, "duplicate key "+strconv.Quote(clip(key)))
		d.Related = []diag.Related{{Location: c.p.location(first), Message: "first defined at"}}
		c.p.add(d)
		c.failed = true
		return
	}
	seen[key] = keyPos
	m.Members = append(m.Members, tree.Member{Key: key, KeyPos: keyPos, Value: value})
}

// key returns a mapping key's string: the plain text as written or the
// decoded quoted or block content, never typed, so 1: and "1": are the
// same key (01 req 11). A non-scalar key is RZ-CFG-001 (01 req 10).
func (c *converter) key(k ast.MapKeyNode) (string, tree.Pos, bool) {
	var n ast.Node = k
	if mk, ok := n.(*ast.MappingKeyNode); ok {
		n = mk.Value
	}
	at := tree.Pos{}
	if n != nil {
		at = c.tokenPos(n.GetToken())
	}
	switch v := n.(type) {
	case nil:
		return "", c.tokenPos(k.GetToken()), true
	case *ast.TagNode:
		failed := c.failed
		c.failed = false
		t := c.tagged(v)
		bad := c.failed
		c.failed = failed || bad
		if bad {
			return "", at, false
		}
		if t.Kind == tree.KindMap || t.Kind == tree.KindList {
			c.fail(codeParse, at, "a mapping key must be a scalar")
			return "", at, false
		}
		return c.keyText(v, t), at, true
	case *ast.LiteralNode:
		return c.literalText(v), at, true
	case *ast.MappingNode, *ast.MappingValueNode, *ast.SequenceNode, *ast.AnchorNode, *ast.AliasNode, *ast.MergeKeyNode:
		c.fail(codeParse, at, "a mapping key must be a scalar")
		return "", at, false
	default:
		tk := n.GetToken()
		if tk == nil {
			c.fail(codeParse, at, "a mapping key must be a scalar")
			return "", at, false
		}
		if tk.Type == token.ImplicitNullType {
			return "", at, true
		}
		return c.text(tk), at, true
	}
}

// literalText returns the value of a block scalar: the token pass's
// reading of its source (literal.go), which every block scalar header in a
// parsed document has, or goccy's value.
func (c *converter) literalText(v *ast.LiteralNode) string {
	if f, ok := c.fixes[v.Start]; ok && f.set {
		return f.text
	}
	if v.Value == nil {
		return ""
	}
	return v.Value.Value
}

// keyText returns the text of a tagged key: its content as written.
func (c *converter) keyText(v *ast.TagNode, t *tree.Node) string {
	switch content := v.Value.(type) {
	case nil:
		return t.Text
	case *ast.LiteralNode:
		return c.literalText(content)
	default:
		if tk := content.GetToken(); tk != nil && !synthesized(tk, v.Start) {
			return c.text(tk)
		}
		return ""
	}
}

// scalar types a scalar token (01 req 12): quoted scalars are strings and
// plain scalars follow the core schema; with tag set, the tag forces the
// type of the content, and a finding is reported at the tag (tagAt).
func (c *converter) scalar(tk *token.Token, tag string, tagAt tree.Pos) *tree.Node {
	at := c.tokenPos(tk)
	report := at
	if tagAt.Known() {
		report = tagAt
	}
	n := &tree.Node{Pos: at}
	text := c.text(tk)
	quoted := false
	switch tk.Type {
	case token.DoubleQuoteType:
		n.Style, quoted = tree.StyleDoubleQuoted, true
	case token.SingleQuoteType:
		n.Style, quoted = tree.StyleSingleQuoted, true
	case token.ImplicitNullType:
		text = ""
	default:
		// A plain scalar, typed from its text below.
	}
	var t typed
	switch {
	case tag != "":
		t = resolveTagged(tag, text)
	case quoted:
		t = typed{kind: tree.KindString, text: text}
	default:
		t = resolvePlain(text)
	}
	if t.code != "" {
		c.fail(t.code, report, t.msg)
		n.Kind = tree.KindNull
		return n
	}
	n.Kind, n.Text, n.Bool = t.kind, t.text, t.b
	return n
}

// tagged resolves a node carrying a core tag; the node starts at its tag.
func (c *converter) tagged(v *ast.TagNode) *tree.Node {
	n := c.resolveTag(v)
	if v.Start != nil && v.Start.Position != nil {
		n.Pos = c.tokenPos(v.Start)
	}
	return n
}

// resolveTag converts the content of a tagged node. !!map and !!seq
// require a mapping and a sequence; the scalar tags require a scalar whose
// content matches them, and a mismatch is RZ-CFG-001 (01 req 12).
func (c *converter) resolveTag(v *ast.TagNode) *tree.Node {
	tag := ""
	at := tree.Pos{}
	if v.Start != nil {
		tag, at = v.Start.Value, c.tokenPos(v.Start)
	}
	mismatch := func() *tree.Node {
		c.fail(codeParse, at, "value does not match tag "+clip(tag))
		return &tree.Node{Kind: tree.KindNull, Pos: at}
	}
	switch inner := v.Value.(type) {
	case *ast.MappingNode, *ast.MappingValueNode:
		if tag != tagMap {
			return mismatch()
		}
		return c.node(inner)
	case *ast.SequenceNode:
		if tag != tagSeq {
			return mismatch()
		}
		return c.node(inner)
	case *ast.TagNode:
		return mismatch()
	case *ast.LiteralNode:
		n := c.node(inner)
		if tag == tagMap || tag == tagSeq {
			return mismatch()
		}
		t := resolveTagged(tag, n.Text)
		if t.code != "" {
			c.fail(t.code, at, t.msg)
			return n
		}
		n.Kind, n.Text, n.Bool = t.kind, t.text, t.b
		return n
	case nil:
		if tag == tagMap || tag == tagSeq {
			return mismatch()
		}
		empty := &token.Token{Type: token.ImplicitNullType}
		if v.Start != nil {
			empty.Position = v.Start.Position
		}
		return c.scalar(empty, tag, at)
	default:
		if tag == tagMap || tag == tagSeq {
			return mismatch()
		}
		tk := inner.GetToken()
		if tk == nil {
			return mismatch()
		}
		if synthesized(tk, v.Start) {
			tk = &token.Token{Type: token.ImplicitNullType, Position: tk.Position}
		}
		c.p.built()
		return c.scalar(tk, tag, at)
	}
}

// synthesized reports a content token goccy made up for a tag without
// content: it sits inside the tag's own text.
func synthesized(tk, tag *token.Token) bool {
	if tk.Type == token.ImplicitNullType {
		return true
	}
	if tag == nil || tag.Position == nil || tk.Position == nil || tk.Position.Line != tag.Position.Line {
		return false
	}
	return tk.Position.Column > tag.Position.Column && tk.Position.Column < tag.Position.Column+utf8.RuneCountInString(tag.Value)
}
