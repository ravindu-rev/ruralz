// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Messages of the YAML parser that tests and the YAML Test Suite runner
// match.
const (
	msgMultiLineKey   = "syntax error: an implicit key must be on one line"
	msgComplexKey     = "a mapping key must be a scalar"
	msgTagChain       = "a node has at most one tag"
	msgNoValue        = "a tagged node has no value"
	msgAfterMarker    = `syntax error: only a comment can follow the document end marker ("...")`
	msgDirectivesEnd  = `syntax error: directives must be followed by "---"`
	msgDirectiveName  = "syntax error: a directive needs a name after '%'"
	msgYAMLTwice      = "syntax error: a document has at most one %YAML directive"
	msgYAMLParams     = "syntax error: a %YAML directive takes one version"
	msgAfterRoot      = "syntax error: a document holds one node; start the next document with \"---\""
	msgDedent         = "syntax error: the entry is indented less than the block collection before it but matches no enclosing one"
	msgIndentMoreMap  = "syntax error: the line is indented more than the entries of the block mapping it follows"
	msgIndentMoreSeq  = "syntax error: the line is indented more than the entries of the block sequence it follows"
	msgSeqInMap       = "syntax error: a block sequence entry at the column of a mapping's keys is allowed only as the value of a key with nothing after its ':'"
	msgNoColon        = "syntax error: a block mapping entry needs a ':' after its key"
	msgLongKey        = "syntax error: an implicit key is limited to 1024 characters; write a longer key after '? '"
	msgTabKey         = "syntax error: a block mapping key cannot be indented with a tab; indent it with spaces"
	msgTabEntry       = "syntax error: a block sequence entry cannot be indented with a tab; indent it with spaces"
	msgTabCompact     = "syntax error: a tab cannot separate a block indicator from the block collection after it; use spaces"
	msgTabBlock       = "syntax error: a tab cannot indent a block collection or block scalar; indent it with spaces"
	msgSameLineValue  = "syntax error: a block collection cannot start on its key's line; put it on the next line, indented"
	msgSameLineDoc    = `syntax error: a block collection cannot start on the "---" line; put it on the next line`
	msgPropsBlock     = "syntax error: a block collection cannot start on the line of its tag or anchor; put it on the next line"
	msgPropsSep       = "syntax error: a tag or anchor must be followed by white space"
	msgAnchorChain    = "syntax error: a node has at most one anchor"
	msgAnchorName     = "syntax error: an anchor needs a name after '&'"
	msgAliasName      = "syntax error: an alias needs a name after '*'"
	msgAliasProps     = "syntax error: an alias cannot have a tag or anchor"
	msgVerbatimTag    = "syntax error: a verbatim tag must end with '>' on its line"
	msgAfterNode      = "syntax error: a node must be followed by white space, a ':' or the end of its line"
	msgAfterFlowNode  = "syntax error: a node in a flow collection must be followed by white space, ',', ':' or the closing bracket"
	msgUnexpected     = "syntax error: unexpected text after the node"
	msgCommentSpace   = "syntax error: a comment must be separated from the node before it by white space"
	msgFlowDash       = "syntax error: a block sequence entry ('- ') is not allowed inside a flow collection"
	msgDashAlone      = "syntax error: a '-' before a flow indicator starts no node; quote it (\"-\") for the string"
	msgFlowEmpty      = "syntax error: an entry is missing before this ','"
	msgFlowSeqNext    = "syntax error: a flow sequence entry must be followed by ',' or ']'"
	msgFlowMapNext    = "syntax error: a flow mapping entry must be followed by ',' or '}'"
	msgFlowSeqOpen    = "syntax error: the flow sequence has no closing ']'"
	msgFlowMapOpen    = "syntax error: the flow mapping has no closing '}'"
	msgFlowMarker     = "syntax error: a flow collection must be closed before the document marker"
	msgFlowIndent     = "syntax error: a line inside a flow collection must be indented more than the block collection that holds it"
	msgFlowClose      = "syntax error: a flow collection's closing bracket must be indented more than the block collection that holds it; YAML 1.2 does not allow it at that column"
	msgColonSpace     = "syntax error: in a flow collection a ':' must be followed by white space before its value, unless its key is quoted"
	msgQuestionSpace  = "syntax error: an explicit key's '?' must be followed by white space"
	msgExplicitPlace  = "syntax error: in a flow collection an explicit key ('? ') can only start an entry"
	msgQuotedIndent   = "syntax error: a line inside a quoted scalar must be indented more than the block collection that holds it"
	msgQuotedMarker   = "syntax error: a document marker is not allowed inside a quoted scalar"
	msgBlockScalarTab = "syntax error: a tab cannot indent the empty lines after a block scalar or the first comment after them; use spaces"
	msgReserved       = " is a reserved indicator and cannot start a plain scalar"
	msgPercent        = `syntax error: '%' cannot start a plain scalar; a directive goes before the document's "---"`
	msgBadStart       = "syntax error: a node cannot start with "
)

// stop is the panic value that unwinds the parser.
type stop uint8

const (
	// stopDocument follows a syntax error: the document is left out and
	// the parse resumes at the next document marker.
	stopDocument stop = iota + 1
	// stopFile follows a limit, a %YAML version, MaxDiagnostics or the end
	// of ctx: nothing more of the file is parsed.
	stopFile
)

// maxFileNodes returns the most nodes all the documents of one file may
// hold: five quarters of MaxDocumentTokens. The trees of a file's earlier
// documents stay live while a later one is parsed, so the bound keeps a
// file's trees within a quarter more than its largest document.
func maxFileNodes(perDocument int) int {
	return perDocument + perDocument/4
}

// yamlParser parses one LF-normalized YAML stream that ends with a line
// break, building trees directly: there is no token or event stage, so
// time is linear in the input and the memory held is the trees'.
type yamlParser struct {
	p    *fileParser
	ctx  context.Context
	text string

	// off is the cursor's byte offset; line and col its 1-based line and
	// code-point column.
	off, line, col int

	// directivesOK allows directives: at the stream start and after a
	// "..." line.
	directivesOK bool
	// markerLine is the line of the "---" that started the current
	// document, or 0; errLine is the line of the last syntax error.
	markerLine, errLine int

	depth     int
	docNodes  int
	fileNodes int
	maxFile   int
	// closedIndent is the indentation of the block collection that closed
	// last, or -2, which tells a dedent between two collections' columns
	// from a line indented past a collection's entries.
	closedIndent int

	buf []byte // storage reused by readQuoted

	// findings are the file's diagnostics in order; sem marks those about
	// the values of a parsed document (RZ-CFG-002, RZ-CFG-005 and a value
	// that does not match its tag), which a file with a profile finding or
	// a fatal one does not report (parseYAML).
	findings diag.List
	sem      []bool
	// docFailed marks the current document: a value finding leaves it out.
	docFailed bool
	// profile is set by an RZ-CFG-003 or RZ-CFG-004 finding, fatal by a
	// finding that stops the file.
	profile, fatal bool
	docs           []Document
}

// parseYAML runs the restricted YAML profile over one file: the byte
// pre-checks, then the parse of every document. A syntax error leaves its
// document out, and the parse goes on at the next document marker. An
// anchor, alias, merge key or tag outside the profile (RZ-CFG-003,
// RZ-CFG-004) is reported wherever it is in the file, and the file then
// yields no documents and none of the findings about their values; so does
// a finding that stops the file: a %YAML version other than 1.2, nesting
// deeper than MaxDepth, more nodes than MaxDocumentTokens, or
// MaxDiagnostics passed.
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
		// stream that does not (L24T/01, JEF9/02). No position changes.
		text += "\n"
	}
	y := &yamlParser{p: p, ctx: ctx, text: text, line: 1, col: 1, maxFile: maxFileNodes(p.opts.MaxDocumentTokens), closedIndent: -2}
	y.run()
	keepSem := !y.profile && !y.fatal
	kept := y.findings[:0]
	for i, d := range y.findings {
		if keepSem || !y.sem[i] {
			kept = append(kept, d)
		}
	}
	if p.diags == nil {
		p.diags = kept
	} else {
		p.diags = append(p.diags, kept...)
	}
	if keepSem && !p.full && p.err == nil {
		p.docs = y.docs
	}
}

// run parses the documents of the stream.
func (y *yamlParser) run() {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if r != any(stopFile) {
			panic(r)
		}
	}()
	y.directivesOK = true
	for y.off < len(y.text) {
		if err := y.ctx.Err(); err != nil {
			y.p.err = err
			return
		}
		if y.p.opts.Yield != nil {
			y.p.opts.Yield()
		}
		y.documentOrResume()
	}
}

// documentOrResume parses one document, or after a syntax error in it
// moves to the next document marker.
func (y *yamlParser) documentOrResume() {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if r != any(stopDocument) {
			panic(r)
		}
		y.resume()
	}()
	y.document()
}

// resume moves the cursor to the next line at or after its own that is a
// document marker, or to the end of the text. It leaves out the "---"
// that started the failed document and a marker line the syntax error is
// on; a marker that only ended a flow collection left open starts the
// next document.
func (y *yamlParser) resume() {
	t := y.text
	start := strings.LastIndexByte(t[:y.off], '\n') + 1
	y.off, y.col = start, 1
	line := y.line
	for start < len(t) {
		end := lineEnd(t, start)
		if markerAt(t[start:end]) && line != y.markerLine && line != y.errLine {
			break
		}
		start = end + 1
		line++
	}
	y.skipTo(min(start, len(t)))
	y.directivesOK = false
}

// document parses one document: its directives, its "---", its root node
// and its "..." (YAML 1.2.2 chapter 9).
func (y *yamlParser) document() {
	y.depth, y.docNodes, y.docFailed, y.markerLine, y.closedIndent = 0, 0, false, 0, -2
	var directiveAt tree.Pos
	directives, yamlSeen := false, false
	li := y.peekContentLine()
	for !li.eof && y.directivesOK && li.indent == 0 && !li.tab && y.text[li.start] == '%' {
		at := y.directive(&yamlSeen)
		if !directives {
			directives, directiveAt = true, at
		}
		li = y.peekContentLine()
	}
	if li.eof {
		if directives {
			y.syntax(directiveAt, msgDirectivesEnd)
		}
		return
	}
	var root *tree.Node
	var start tree.Pos
	switch {
	case li.marker && y.text[li.start] == '-':
		start = y.here()
		y.markerLine = y.line
		y.directivesOK = false
		y.skipTo(li.start + 3)
		root = y.blockValue(-1, afterDoc, y.here())
	case li.marker:
		if directives {
			y.syntax(directiveAt, msgDirectivesEnd)
		}
		y.documentEnd(li)
		return
	default:
		if directives {
			y.syntax(directiveAt, msgDirectivesEnd)
		}
		y.directivesOK = false
		root = y.blockNodeAt(li, -1, afterDoc, props{})
		start = root.Pos
	}
	li = y.peekContentLine()
	switch {
	case li.eof, li.marker && y.text[li.start] == '-':
	case li.marker:
		y.documentEnd(li)
	default:
		y.skipTo(li.first)
		at := y.here()
		switch {
		case y.text[li.first] == ':' && y.blankAt(li.first+1):
			y.syntax(at, msgMultiLineKey)
		case y.closedIndent > li.indent:
			y.syntax(at, msgDedent)
		default:
			y.syntax(at, msgAfterRoot)
		}
	}
	y.complete(root, start)
}

// documentEnd reads a "..." line: only white space or a comment may follow
// the marker (l-document-suffix), and directives may follow the line.
func (y *yamlParser) documentEnd(li lineInfo) {
	y.skipTo(li.start + 3)
	y.skipWhite()
	if c := y.cur(); c != '\n' && c != 0 && c != '#' {
		y.syntax(y.here(), msgAfterMarker)
	}
	y.skipTo(min(lineEnd(y.text, y.off)+1, len(y.text)))
	y.directivesOK = true
}

// complete keeps a parsed document: its value findings are reported, a
// document with one is left out, a null root is skipped and any other root
// that is not a mapping is RZ-CFG-005.
func (y *yamlParser) complete(root *tree.Node, start tree.Pos) {
	switch {
	case y.docFailed, root == nil, root.Kind == tree.KindNull:
	case root.Kind != tree.KindMap && !y.p.anyRoot:
		y.add(y.p.errorAt(codeSchema, root.Pos, "a resource must be a mapping"), true)
	default:
		y.docs = append(y.docs, Document{Root: root, Start: start})
	}
}

// directive reads the directive line at the cursor and returns its
// position: %TAG is RZ-CFG-004, a %YAML version other than 1.2 is
// RZ-CFG-001 and stops the file, and other directive names are reserved and
// ignored, as YAML 1.2 specifies.
func (y *yamlParser) directive(yamlSeen *bool) tree.Pos {
	at := y.here()
	t := y.text
	end := lineEnd(t, y.off)
	i := y.off + 1
	for i < end && !isWhite(t[i]) {
		i++
	}
	name := t[y.off+1 : i]
	if name == "" {
		y.syntax(at, msgDirectiveName)
	}
	var params []string
	for {
		for i < end && isWhite(t[i]) {
			i++
		}
		if i >= end || t[i] == '#' {
			break
		}
		j := i
		for j < end && !isWhite(t[j]) {
			j++
		}
		params = append(params, t[i:j])
		i = j
	}
	switch name {
	case "YAML":
		if *yamlSeen {
			y.syntax(at, msgYAMLTwice)
		}
		*yamlSeen = true
		version := ""
		if len(params) > 0 {
			version = params[0]
		}
		if len(params) > 1 {
			y.syntax(at, msgYAMLParams)
		}
		if version != "1.2" {
			y.fatalAt(at, "%YAML "+clip(version)+" directive: only YAML 1.2 is accepted")
		}
	case "TAG":
		y.profileFinding(codeTag, at, "%TAG directives are not allowed")
	}
	y.skipTo(min(end+1, len(t)))
	return at
}

// lineInfo is the next line that holds content.
type lineInfo struct {
	// start and first are the offsets of the line and of its first
	// character other than white space; indent is the number of spaces
	// that start it, and tab says a tab comes before first.
	start, first, indent int
	tab                  bool
	// eof says no such line remains; marker says it is "---" or "...".
	eof, marker bool
}

// peekContentLine moves the cursor, at the start of a line, past lines of
// white space and comment lines to the start of the next line holding
// content, and describes it.
func (y *yamlParser) peekContentLine() lineInfo {
	t := y.text
	for s := y.off; s < len(t); {
		end := lineEnd(t, s)
		sp := s
		for sp < end && t[sp] == ' ' {
			sp++
		}
		f := sp
		for f < end && isWhite(t[f]) {
			f++
		}
		if f == end || t[f] == '#' {
			s = end + 1
			continue
		}
		y.skipTo(s)
		return lineInfo{start: s, first: f, indent: sp - s, tab: f > sp, marker: sp == s && markerAt(t[s:end])}
	}
	y.skipTo(len(t))
	return lineInfo{start: len(t), first: len(t), eof: true}
}

// endLine checks that only white space or a comment remains on the
// cursor's line and moves the cursor to the start of the next line.
func (y *yamlParser) endLine() {
	t := y.text
	white := y.off == 0 || isBlank(t[y.off-1])
	for y.off < len(t) {
		switch c := t[y.off]; {
		case c == ' ' || c == '\t':
			y.off++
			y.col++
			white = true
		case c == '#' && white:
			y.skipTo(lineEnd(t, y.off))
		case c == '\n':
			y.skipTo(y.off + 1)
			return
		case c == '#':
			y.syntax(y.here(), msgCommentSpace)
		case c == ':' && y.blankAt(y.off+1):
			y.syntax(y.here(), msgMultiLineKey)
		default:
			y.syntax(y.here(), msgUnexpected)
		}
	}
}

// here returns the cursor's position.
func (y *yamlParser) here() tree.Pos {
	return y.p.pos(y.line, y.col)
}

// posOf returns the position of byte offset off at or after the cursor.
func (y *yamlParser) posOf(off int) tree.Pos {
	line, col := y.line, y.col
	for i := y.off; i < off; i++ {
		switch c := y.text[i]; {
		case c == '\n':
			line, col = line+1, 1
		case c < 0x80 || c >= 0xC0:
			col++
		}
	}
	return y.p.pos(line, col)
}

// skipTo moves the cursor forward to byte offset to, counting lines and
// code-point columns.
func (y *yamlParser) skipTo(to int) {
	t := y.text
	for i := y.off; i < to; i++ {
		switch c := t[i]; {
		case c == '\n':
			y.line++
			y.col = 1
		case c < 0x80 || c >= 0xC0:
			y.col++
		}
	}
	y.off = to
}

// adv moves the cursor past one ASCII character on its line.
func (y *yamlParser) adv() {
	y.off++
	y.col++
}

// skipWhite moves the cursor past spaces and tabs and reports whether it
// passed a tab.
func (y *yamlParser) skipWhite() (tab bool) {
	for y.off < len(y.text) {
		switch y.text[y.off] {
		case ' ':
		case '\t':
			tab = true
		default:
			return tab
		}
		y.off++
		y.col++
	}
	return tab
}

// cur returns the byte at the cursor, 0 at the end of the text.
func (y *yamlParser) cur() byte {
	if y.off < len(y.text) {
		return y.text[y.off]
	}
	return 0
}

// blankAt reports whether byte offset i is white space, a line break or
// the end of the text.
func (y *yamlParser) blankAt(i int) bool {
	return i >= len(y.text) || isBlank(y.text[i])
}

// blankOrFlowAt is blankAt, and in a flow collection also a flow
// indicator.
func (y *yamlParser) blankOrFlowAt(i int, inFlow bool) bool {
	return y.blankAt(i) || (inFlow && isFlowIndicator(y.text[i]))
}

// atLineEnd reports whether only white space or a comment remains on the
// cursor's line, the cursor being past any white space.
func (y *yamlParser) atLineEnd() bool {
	c := y.cur()
	return c == '\n' || c == 0 || (c == '#' && y.off > 0 && isBlank(y.text[y.off-1]))
}

func isWhite(c byte) bool { return c == ' ' || c == '\t' }

func isBlank(c byte) bool { return c == ' ' || c == '\t' || c == '\n' }

func isFlowIndicator(c byte) bool {
	return c == ',' || c == '[' || c == ']' || c == '{' || c == '}'
}

// count counts a finding against MaxDiagnostics: after MaxDiagnostics+1
// the file stops, so a file of a million anchors costs no more than the
// pipeline's cap, which stops the run at the same count (diag.Collector).
func (y *yamlParser) count() {
	y.p.found++
	if y.p.found > y.p.opts.MaxDiagnostics {
		y.p.full = true
		panic(stopFile)
	}
}

// add adds a finding of the file; sem marks a value finding.
func (y *yamlParser) add(d diag.Diagnostic, sem bool) {
	y.findings = append(y.findings, d)
	y.sem = append(y.sem, sem)
	y.count()
}

// record adds a finding of the file.
func (y *yamlParser) record(code string, at tree.Pos, msg string) {
	y.add(y.p.errorAt(code, at, msg), false)
}

// profileFinding records an RZ-CFG-003 or RZ-CFG-004 finding; the parse
// goes on, so every one in the file is reported.
func (y *yamlParser) profileFinding(code string, at tree.Pos, msg string) {
	y.profile = true
	y.record(code, at, msg)
}

// syntax records RZ-CFG-001 for a syntax error and leaves the document,
// after the value findings found in it so far.
func (y *yamlParser) syntax(at tree.Pos, msg string) {
	y.errLine = int(at.Line)
	y.record(codeParse, at, msg)
	panic(stopDocument)
}

// fatalAt records RZ-CFG-001 and stops the file.
func (y *yamlParser) fatalAt(at tree.Pos, msg string) {
	y.fatal = true
	y.record(codeParse, at, msg)
	panic(stopFile)
}

// semantic records a finding about a value of the current document, which
// leaves the document out.
func (y *yamlParser) semantic(d diag.Diagnostic) {
	y.docFailed = true
	y.add(d, true)
}

// semanticAt records a value finding at a position.
func (y *yamlParser) semanticAt(code string, at tree.Pos, msg string) {
	y.semantic(y.p.errorAt(code, at, msg))
}

// enter opens a collection starting at at; the first collection deeper than
// MaxDepth stops the file. The root mapping of a document is depth 1.
// Recursion follows the nesting, so it is bounded by MaxDepth too.
func (y *yamlParser) enter(at tree.Pos) {
	if y.depth >= y.p.opts.MaxDepth {
		y.fatalAt(at, "nesting depth exceeds "+strconv.Itoa(y.p.opts.MaxDepth))
	}
	y.depth++
}

// leave closes the collection enter opened.
func (y *yamlParser) leave() { y.depth-- }

// built counts a node of the tree: every scalar, key, collection and empty
// node. MaxDocumentTokens bounds the nodes of a document and five quarters
// of it those of the file (maxFileNodes), and passing either stops the
// file. It yields every 256 nodes and checks ctx every 4,096.
func (y *yamlParser) built(at tree.Pos) {
	y.p.built()
	y.docNodes++
	y.fileNodes++
	switch {
	case y.docNodes > y.p.opts.MaxDocumentTokens:
		y.fatalAt(at, "document has more than "+strconv.Itoa(y.p.opts.MaxDocumentTokens)+" nodes")
	case y.fileNodes > y.maxFile:
		y.fatalAt(at, "file has more than "+strconv.Itoa(y.maxFile)+" nodes in its documents")
	case y.p.nodes%4096 == 0:
		if err := y.ctx.Err(); err != nil {
			y.p.err = err
			panic(stopFile)
		}
	}
}
