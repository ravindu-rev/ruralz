// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/token"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// maxFileTokens returns the most tokens all the documents of one file may
// hold: five quarters of MaxDocumentTokens, 500,000 by default (01 test
// plan FuzzProfileYAML; 11 req 17). The trees of a file's earlier documents
// stay live while a later one is tokenized and parsed, so three documents of
// 348,000 tokens each, 1,044,033 bytes, peaked at 184 to 276 MiB of heap,
// past the 256 MiB budget for an input of 1 MiB, and two documents at the
// limit at 216 to 234 MiB, where one peaks at 150 to 179 MiB. With a
// quarter of the limit left for the documents before the last, they keep at
// most 12 MiB live. A file of many small documents, such as a Bundle of many
// resources, holds as many tokens: 1 MiB of "a: 1\n---\n" holds 466,000.
func maxFileTokens(maxTokens int) int {
	return maxTokens + maxTokens/4
}

// frameKind is the kind of an open collection on the depth stack.
type frameKind uint8

const (
	frameFlow frameKind = iota
	frameBlockSeq
	frameBlockMap
)

// frame is one open collection: a flow collection with its cost record
// (flowcost.go), its open explicit key (explicit.go) and the shape of its
// current entry (flowentry.go), or a block sequence or mapping with the
// column of its entries and its record.
type frame struct {
	kind  frameKind
	col   int
	node  *blockFrame
	cost  flowCost
	key   flowKey
	shape flowShape
}

// blockFrame records a block collection the depth algorithm found: its
// token range and its entries. The split parse (split.go) uses the
// records.
type blockFrame struct {
	kind  frameKind // frameBlockSeq or frameBlockMap
	first int       // index of the first token: the first entry's start
	end   int       // index just past the last token
	// entries are the entries in order: a mapping entry starts at its key
	// (its first tag, '?' or scalar token) and has its ':' at colon, -1
	// until seen; a sequence entry starts at its '-' and has colon -1.
	entries []blockEntry
	// split is set when this frame or a frame inside it has at least
	// splitAt entries (always, unless a test raises the threshold), or
	// when goccy would misread this frame (an empty last sequence entry
	// before a key, emptyLastEntry).
	split bool
	// parent is the enclosing block frame; nil at the document level.
	parent *blockFrame
}

// blockEntry is one entry of a blockFrame.
type blockEntry struct {
	start, colon int
}

// tokenPass is the token pass of 01 req 8 and 9 over one file. It runs
// over each document's tokens before any parsing and keeps its findings:
// RZ-CFG-003 and RZ-CFG-004 are collected for the whole file, and the
// first RZ-CFG-001 stops the file.
type tokenPass struct {
	p         *fileParser
	maxDepth  int
	maxTokens int
	// fileTokens counts the tokens of the file's segments before the one
	// run checks; maxFileTokens bounds them with the segment's.
	fileTokens int
	// splitAt is the entry count from which a block collection is parsed
	// entry by entry; 0 never.
	splitAt int
	// splits maps the first token index of every block frame to split to
	// its record, for the segment last run.
	splits map[int]*blockFrame

	// errs counts every finding of the file; fatal is set by RZ-CFG-001.
	errs  int
	fatal bool

	// Per-document state, reset by a document marker.
	stack  []frame
	tokens int
	flow   int // open flow collections
	// keyStart is where the current block node starts (its first tag,
	// anchor, scalar token or flow collection start), the column of a
	// mapping entry's key. Tokens inside a flow collection never move it:
	// the collection is the node. Neither does a block scalar's content,
	// which belongs to the node its header started.
	keyStart     tree.Pos
	keyStartIdx  int
	keyStartSet  bool
	keyBlock     bool // the node at keyStart is a block scalar
	lastProperty bool // the previous token was a tag, an anchor or an anchor name
	lastTag      bool // the previous token other than a comment was a tag
	lastHeader   bool // the previous token other than a comment was a block scalar header
	lastTagLine  int  // the segment line of the last tag
	// q is the open explicit entry (explicit.go).
	q explicitEntry
	// next is the token after the last block scalar's content (literal.go).
	next blockNext

	// ended is set by a document end marker ("..."), after which only
	// comments may follow in the segment.
	ended bool

	// tks are the tokens of the segment run checks, cols corrects their
	// columns (columns.go), lineOff is the file line before the segment's
	// first, and src reads the segment's text (source.go).
	tks     token.Tokens
	cols    columnTable
	lineOff int
	src     srcCursor
	// fixes are the YAML 1.2 readings of the segment's scalars that goccy
	// reads otherwise (source.go), for the converter; buf is reused by
	// the quoted scalar reader.
	fixes fixes
	buf   []byte

	// The bounds of goccy's parse of the outermost open flow collection
	// (flowcost.go): its path bytes charged, its mapping entries that get
	// an inserted null and the sum of their token indexes, and its start.
	pathCost, nulls, nullSum int
	flowAt                   tree.Pos
}

// reset starts a new document; idx is the index of the token that ends
// the previous one. A flow collection the document leaves open is bounded
// as one it closes there (closeFlow): goccy's parse of it inserts its
// nulls before it fails at the end of the tokens.
func (t *tokenPass) reset(idx int) {
	t.endExplicit()
	if t.flow > 0 {
		t.closeFlow(idx)
	}
	t.closeAll(idx)
	t.stack = t.stack[:0]
	t.tokens = 0
	t.flow = 0
	t.keyStartSet = false
	t.lastProperty = false
	t.lastTag = false
	t.lastHeader = false
}

// fail records an RZ-CFG-001 that stops the file.
func (t *tokenPass) fail(at tree.Pos, msg string) {
	t.p.add(t.p.errorAt(codeParse, at, msg))
	t.errs++
	t.fatal = true
}

// report records an RZ-CFG-003 or RZ-CFG-004; scanning continues until
// the file reaches MaxDiagnostics findings.
func (t *tokenPass) report(code string, at tree.Pos, msg string) {
	t.p.add(t.p.errorAt(code, at, msg))
	t.errs++
	if t.p.full {
		t.fatal = true
	}
}

// Messages of the document marker checks.
const (
	msgMarker      = `syntax error: "..." at the start of a line ends the document and must be followed by white space or a line break`
	msgAfterMarker = `syntax error: only a comment can follow the document end marker ("...")`
)

// run checks the tokens of one segment, whose text is text and whose
// first line is file line lineOff+1; cols is the segment's column table
// (columns.go). It stops at the first RZ-CFG-001.
func (t *tokenPass) run(tks token.Tokens, lineOff int, cols columnTable, text string) {
	t.splits, t.fixes = nil, nil
	t.tks, t.cols, t.lineOff, t.src = tks, cols, lineOff, newCursor(text)
	t.stack = t.stack[:0]
	t.flow = 0
	t.reset(0)
	t.ended = false
	t.next = blockNext{idx: -1}
	defer func() {
		t.reset(len(tks))
		t.tks, t.cols, t.src = nil, nil, srcCursor{}
	}()
	posOf := t.posOf
	defer func() { t.fileTokens += len(tks) }()
	maxFile := maxFileTokens(t.maxTokens)
	for i := 0; i < len(tks) && !t.fatal; i++ {
		tk := tks[i]
		t.tokens++
		if t.tokens > t.maxTokens {
			t.fail(posOf(tk), "document has more than "+strconv.Itoa(t.maxTokens)+" tokens")
			return
		}
		if t.fileTokens+i >= maxFile {
			t.fail(posOf(tk), "file has more than "+strconv.Itoa(maxFile)+" tokens in its documents")
			return
		}
		if t.ended && tk.Type != token.CommentType {
			// YAML 1.2 allows only a comment after "..." on its line
			// (l-document-suffix), and splitDocuments ends the segment
			// with that line. goccy reads a node there as the start of a
			// next document, which the parse of one document body cannot
			// hold (01 req 9: documents split at DocumentEndType).
			t.fail(posOf(tk), msgAfterMarker)
			return
		}
		afterTag, afterHeader := t.lastTag, t.lastHeader
		content := afterHeader && tk.Type == token.StringType
		if isPlainType(tk.Type) && !content {
			if t.plain(i); t.fatal {
				return
			}
		}
		at := posOf(tk)
		if tk.Type != token.CommentType {
			t.lastTag = tk.Type == token.TagType
			t.lastHeader = tk.Type == token.LiteralType || tk.Type == token.FoldedType
			if t.flow > 0 {
				if t.chargePath(at); t.fatal {
					return
				}
			}
		}
		if content {
			// The content of the block scalar whose header comes before
			// it. The header started the node, and the content must not
			// move keyStart: goccy puts empty content on the next line,
			// sometimes at a ':' ("- >\n:" scans as Folded@1:3, String@2:1,
			// MappingValue@2:1).
			t.lastProperty = false
			continue
		}
		switch tk.Type {
		case token.CommentType:
			continue
		case token.DocumentHeaderType, token.DocumentEndType:
			if !t.markerLine(tk) {
				// goccy's scanner ends a document at "..." in column 1
				// whatever follows it ("...x: 1", "...#"), where YAML 1.2
				// reads a plain scalar or refuses the line, and
				// splitDocuments does not cut. The check bounds the cost
				// of the parse, so it holds after any finding too.
				t.fail(at, msgMarker)
				return
			}
			t.reset(i)
			if tk.Type == token.DocumentHeaderType {
				t.tokens = 1
			} else {
				t.ended = true
			}
			continue
		case token.InvalidType:
			t.fail(at, "syntax error: "+tk.Error)
			return
		case token.DirectiveType:
			i = t.directive(tks, i, posOf)
			continue
		case token.AnchorType:
			t.report(codeAnchor, at, "anchors are not allowed"+nameSuffix(tks, i))
			t.property(at, i)
			if i+1 < len(tks) && tks[i+1].Position.Line == tk.Position.Line {
				i++ // the anchor name
				t.tokens++
			}
			continue
		case token.AliasType:
			t.report(codeAnchor, at, "aliases are not allowed"+nameSuffix(tks, i))
			t.scalar(at, i)
			if i+1 < len(tks) && tks[i+1].Position.Line == tk.Position.Line {
				i++ // the alias name
				t.tokens++
			}
			continue
		case token.MergeKeyType:
			t.report(codeAnchor, at, "merge keys (<<) are not allowed")
			t.scalar(at, i)
			continue
		case token.TagType:
			if afterTag && !t.keyAfterTag(i) {
				// YAML 1.2 gives a node at most one tag, and goccy's
				// parser recurses once per tag of a chain, outside the
				// depth limit (11 req 17, 26).
				t.fail(at, "a node has at most one tag")
				return
			}
			t.lastTagLine = tk.Position.Line
			if !isCoreTag(tk.Value) {
				t.report(codeTag, at, "tag "+clip(tk.Value)+" is not allowed; only the YAML 1.2 core tags are")
			}
			// The check only keeps goccy's parse from misreading the
			// file. A file with any token-pass finding is never parsed,
			// and that includes the RZ-CFG-004 just reported for a
			// non-core tag. After a finding, the check would add nothing
			// and would only stop the collection of the file's other
			// RZ-CFG-003 and RZ-CFG-004 findings (01 req 8).
			if t.errs == 0 && t.tagWithoutValue(i) {
				t.fail(at, "a tagged node has no value")
				return
			}
			if j := t.tagOnBlockLine(i); j >= 0 && t.errs == 0 {
				if j == i {
					t.fail(at, "value does not match tag "+clip(tk.Value))
				} else {
					t.fail(posOf(tks[j]), "a block collection cannot start on the line of its tag")
				}
				return
			}
			t.property(at, i)
			if t.flow > 0 && isGoccyScalarTag(tk.Value) && t.endsFlowEntry(i+1) {
				// goccy gives a scalar tag directly before ',' or ':' its
				// default value and inserts that token into its token
				// slice (newTagDefaultScalarValueNode), moving every later
				// token, as it does the implicit null of a mapping entry
				// without a value: the move is bounded the same way
				// (flowcost.go; 11 req 26).
				t.nulls++
				t.nullSum += i
			}
			if t.fatal {
				return
			}
			continue
		case token.DoubleQuoteType, token.SingleQuoteType:
			if t.quoted(i); t.fatal {
				return
			}
			t.structure(tk, at, i)
		case token.LiteralType, token.FoldedType:
			if t.blockScalar(i); t.fatal {
				return
			}
			t.structure(tk, at, i)
		default:
			t.structure(tk, at, i)
		}
	}
}

// markerLine reports whether the source line of document marker token tk
// is a marker line, as splitDocuments cuts at: the marker followed by white
// space or the end of the line.
func (t *tokenPass) markerLine(tk *token.Token) bool {
	off := t.src.offset(tk.Position.Line, 1)
	if off < 0 {
		return false
	}
	marker := "---"
	if tk.Type == token.DocumentEndType {
		marker = "..."
	}
	return markerLine(t.src.text[off:lineEnd(t.src.text, off)], marker)
}

// isGoccyScalarTag reports the tags goccy's parser gives a default value
// when no content follows them (parseTagValue).
func isGoccyScalarTag(tag string) bool {
	switch tag {
	case tagStr, tagInt, tagFloat, tagBool, tagNull, "!!binary", "!!timestamp":
		return true
	default:
		return false
	}
}

// endsFlowEntry reports whether the first token from idx on that is not a
// comment is a ',' or a ':'.
func (t *tokenPass) endsFlowEntry(idx int) bool {
	for idx < len(t.tks) && t.tks[idx].Type == token.CommentType {
		idx++
	}
	return idx < len(t.tks) && (t.tks[idx].Type == token.CollectEntryType || t.tks[idx].Type == token.MappingValueType)
}

// posOf returns the file position of a token: its line, and its column
// corrected for the tags and tabs before it (columns.go), or the position
// the token pass read it at from the source (source.go). goccy gives a
// block scalar's content column 0.
func (t *tokenPass) posOf(tk *token.Token) tree.Pos {
	if f, ok := t.fixes[tk]; ok && f.pos.Known() {
		return f.pos
	}
	return t.p.pos(tk.Position.Line+t.lineOff, max(t.col(tk), 1))
}

// fix records the YAML 1.2 reading of a scalar token (source.go).
func (t *tokenPass) fix(tk *token.Token, f scalarFix) {
	if t.fixes == nil {
		t.fixes = fixes{}
	}
	t.fixes[tk] = f
}

// keyAfterTag reports whether the tag at token idx, which follows another
// tag, belongs to a different node (01 req 8, 12): in block context, a tag
// on a later line than the previous one that starts an implicit key, as in
// "spec: !!map\n  !!str name: x", where the first tag is the mapping's
// and the second its key's. In flow context a tag that follows a tag is
// always the same node's, and so is one on the same line. A chain stays
// bounded at two tags (11 req 17, 26): a key's tag is followed on its line
// by the key and its ':', never by another tag.
func (t *tokenPass) keyAfterTag(idx int) bool {
	tk := t.tks[idx]
	if t.flow > 0 || tk.Position.Line == t.lastTagLine {
		return false
	}
	return startsImplicitKey(t.tks, idx)
}

// tagWithoutValue reports a tag at token idx, in block context inside a
// block collection, that ends its line (comments aside) while the next
// token starts at or left of the innermost block collection's column, as
// in "a: !!map\nb: 1" (01 req 8, 9). In YAML 1.2 the tagged node is empty
// and the next token starts a sibling, but goccy's parser nests the
// sibling under the tagged node, one level per such line at one column:
// the tree's depth is then not the depth the algorithm of 01 req 9 counts
// from columns, goccy's recursion costs the square of it (11 req 17, 26),
// and the split parse (split.go) refuses the same lines, but with goccy's
// message about the next token. Two next tokens at a
// block mapping's column give the tagged node its value and are allowed:
// a "-" (a compact sequence, "a: !!seq\n- x") and the ':' of an explicit
// key that has none yet ("? !!str\n: v"). A document's tag, as in
// "--- !!map\na: 1", a tag before a document marker and a tag at the end
// of the input are outside this check.
func (t *tokenPass) tagWithoutValue(idx int) bool {
	if t.flow > 0 || len(t.stack) == 0 {
		return false
	}
	j := idx + 1
	for j < len(t.tks) && t.tks[j].Type == token.CommentType {
		j++
	}
	if j == len(t.tks) {
		return false
	}
	next := t.tks[j]
	if next.Type == token.DocumentHeaderType || next.Type == token.DocumentEndType {
		return false
	}
	line, col, ok := t.contentStart(idx)
	if !ok {
		line, col = next.Position.Line, t.col(next)
	}
	top := t.stack[len(t.stack)-1]
	if line == t.tks[idx].Position.Line || col > top.col {
		return false
	}
	if top.kind == frameBlockMap && col == top.col {
		switch next.Type {
		case token.SequenceEntryType:
			return false
		case token.MappingValueType:
			e := top.node.entries
			return len(e) == 0 || e[len(e)-1].colon >= 0
		default:
		}
	}
	return true
}

// contentStart returns the segment line and code-point column of the first
// character after the tag at token idx that is neither white space nor in a
// comment: where the next node starts. goccy reports a plain scalar that a
// line starting with '-' continues at its last line ("a: !!str 0\n -" puts
// "0 -" on line 2), so the next token's position does not tell (finding of
// the eighth WP-33 review). It reports false when the tag is not found in
// the source or nothing follows it.
func (t *tokenPass) contentStart(idx int) (line, col int, ok bool) {
	tk := t.tks[idx]
	text := t.src.text
	from := t.src.offset(tk.Position.Line, t.col(tk))
	if from < 0 || !strings.HasPrefix(text[from:], tk.Value) {
		return 0, 0, false
	}
	for off := from + len(tk.Value); off < len(text); {
		switch c := text[off]; {
		case isWhite(c):
			off++
		case c == '#':
			off = lineEnd(text, off)
		default:
			line, col = advance(text, from, off, tk.Position.Line, t.col(tk))
			return line, col, true
		}
	}
	return 0, 0, false
}

// contentAfter reports whether the tag at token idx has its content on its
// own line (contentStart).
func (t *tokenPass) contentAfter(idx int) bool {
	line, _, ok := t.contentStart(idx)
	return ok && line == t.tks[idx].Position.Line
}

// tagOnBlockLine checks a tag at token idx, in block context, followed on
// its line by a block collection (01 req 8, 12): a '-' or a '?', which
// YAML 1.2 never allows on the line of their node's properties, or an
// implicit key after !!map or !!seq, which YAML 1.2 gives the tag to, a
// scalar. goccy instead tags the collection in a sequence entry or a
// mapping value ("k: !!seq - a" reads as {k: [a]}, "k: !!map ? a" as
// {k: {a: null}}, "- !!map a: b" as [{a: b}]) but refuses the same tokens
// parsed on their own, as the split parse parses them. It returns the
// index of the '-' or '?', of the tag for a key, or -1.
func (t *tokenPass) tagOnBlockLine(idx int) int {
	j := idx + 1
	if t.flow > 0 || j == len(t.tks) || t.tks[j].Position.Line != t.tks[idx].Position.Line {
		return -1
	}
	switch tag := t.tks[idx].Value; {
	case t.tks[j].Type == token.SequenceEntryType, t.tks[j].Type == token.MappingKeyType:
		return j
	case (tag == tagMap || tag == tagSeq) && startsImplicitKey(t.tks, idx):
		return idx
	default:
		return -1
	}
}

// startsImplicitKey reports whether the node property at token idx starts
// an implicit key: an anchor and its name may follow it, then a ':' on its
// line, after a scalar or at once (an empty key).
func startsImplicitKey(tks token.Tokens, idx int) bool {
	line := tks[idx].Position.Line
	on := func(j int) bool { return j < len(tks) && tks[j].Position.Line == line }
	j := idx + 1
	if on(j) && tks[j].Type == token.AnchorType {
		j += 2 // the anchor and its name
	}
	if on(j) && isKeyScalar(tks[j]) {
		j++
	}
	return on(j) && tks[j].Type == token.MappingValueType
}

// isPlainType reports the types goccy's scanner gives a plain scalar,
// which it types by the YAML 1.1 rules of its own.
func isPlainType(tp token.Type) bool {
	switch tp {
	case token.StringType, token.NullType, token.BoolType,
		token.IntegerType, token.BinaryIntegerType, token.OctetIntegerType, token.HexIntegerType,
		token.FloatType, token.InfinityType, token.NanType:
		return true
	default:
		return false
	}
}

// isKeyScalar reports a scalar token that can be an implicit key.
func isKeyScalar(tk *token.Token) bool {
	switch tk.Type {
	case token.StringType, token.SingleQuoteType, token.DoubleQuoteType,
		token.NullType, token.ImplicitNullType, token.BoolType,
		token.IntegerType, token.BinaryIntegerType, token.OctetIntegerType, token.HexIntegerType,
		token.FloatType, token.InfinityType, token.NanType:
		return true
	default:
		return false
	}
}

// nameSuffix returns " (&name)" or " (*name)" for the anchor or alias
// whose name follows token i on its line.
func nameSuffix(tks token.Tokens, i int) string {
	if i+1 < len(tks) && tks[i+1].Position.Line == tks[i].Position.Line && tks[i+1].Value != "" {
		return " (" + tks[i].Value + clip(tks[i+1].Value) + ")"
	}
	return ""
}

// directive checks the directive starting at token i and returns the index
// of its last token: %TAG is RZ-CFG-004 and a %YAML version other than 1.2
// is RZ-CFG-001; other directive names are reserved and ignored, as YAML
// 1.2 specifies.
func (t *tokenPass) directive(tks token.Tokens, i int, posOf func(*token.Token) tree.Pos) int {
	start := tks[i]
	var params []*token.Token
	for i+1 < len(tks) && tks[i+1].Position.Line == start.Position.Line && tks[i+1].Type != token.CommentType {
		i++
		params = append(params, tks[i])
	}
	t.tokens += len(params)
	if len(params) == 0 {
		return i
	}
	switch params[0].Value {
	case "TAG":
		t.report(codeTag, posOf(start), "%TAG directives are not allowed")
	case "YAML":
		version := ""
		if len(params) > 1 {
			version = params[1].Value
		}
		if version != "1.2" {
			t.fail(posOf(start), "%YAML "+clip(version)+" directive: only YAML 1.2 is accepted")
		}
	}
	return i
}

// col returns the real column of a token (columns.go).
func (t *tokenPass) col(tk *token.Token) int {
	return t.cols.fix(tk.Position.Line, tk.Position.Column)
}

// property notes a tag or anchor at token idx: it starts the node that
// follows unless another property already did on the same line.
func (t *tokenPass) property(at tree.Pos, idx int) {
	t.start(at, idx)
	if t.flow > 0 {
		t.flowKeyNode(at, idx)
		t.flowNode(-1)
	}
	t.lastProperty = true
}

// scalar notes a value token at idx: it starts a node unless properties
// on its line already did. A block scalar header makes that node a block
// scalar.
func (t *tokenPass) scalar(at tree.Pos, idx int) {
	t.start(at, idx)
	if t.flow == 0 {
		tp := t.tks[idx].Type
		t.keyBlock = tp == token.LiteralType || tp == token.FoldedType
	} else {
		t.flowKeyNode(at, idx)
		t.flowNode(len(t.tks[idx].Value))
	}
	t.lastProperty = false
}

// start notes that a block node starts at token idx. It does nothing when
// properties earlier on the same line already started the node, and it
// does nothing inside a flow collection, whose start is the node. A node
// that starts is checked against the open explicit entry (explicit.go).
func (t *tokenPass) start(at tree.Pos, idx int) {
	if t.flow > 0 {
		return
	}
	if t.keyStartSet && t.lastProperty && t.keyStart.Line == at.Line {
		return
	}
	t.explicitNode(at, idx)
	if t.fatal {
		return
	}
	t.keyStart, t.keyStartIdx, t.keyStartSet = at, idx, true
	t.keyBlock = false
}

// structure applies the depth algorithm of 01 req 9 to token idx. Depth is
// the number of open collections: a flow collection counts once; in block
// context a sequence entry at column c, or a mapping entry whose key starts
// at column c, first closes the block collections deeper than c and then
// opens (kind, c) unless it is already the innermost one. A mapping entry
// also closes a sequence at its own column whose parent is a mapping at
// that column (a sequence written at its key's indentation ends at the next
// key), which the literal rule would otherwise count again at every key.
func (t *tokenPass) structure(tk *token.Token, at tree.Pos, idx int) {
	switch tk.Type {
	case token.SequenceStartType, token.MappingStartType:
		t.start(at, idx)
		if t.flow > 0 {
			t.flowKeyNode(at, idx)
		}
		if t.fatal {
			return
		}
		cost := t.openFlow(tk.Type == token.SequenceStartType, at)
		t.flow++
		t.push(frame{kind: frameFlow, cost: cost}, at)
		t.lastProperty = false
	case token.SequenceEndType, token.MappingEndType:
		if t.flow > 0 {
			t.flowEntryEnd(idx, false)
			t.flow--
			for len(t.stack) > 0 {
				top := t.stack[len(t.stack)-1]
				t.stack = t.stack[:len(t.stack)-1]
				if top.kind == frameFlow {
					break
				}
				t.close(top, idx)
			}
			if t.flow == 0 {
				t.closeFlow(idx)
			}
		}
		t.lastProperty = false
	case token.SequenceEntryType:
		if t.flow > 0 {
			// YAML 1.2 has no "- " indicator in flow context; goccy
			// nests one sequence per dash, outside the depth limit and
			// in quadratic memory (01 req 9, 11 req 26).
			t.fail(at, "block sequence entries are not allowed inside a flow collection")
			return
		}
		t.explicitBlock(at, at.Column, true)
		if t.fatal {
			return
		}
		f := t.block(frameBlockSeq, int(at.Column), at, idx)
		t.entry(f, blockEntry{start: idx, colon: -1})
		t.keyStartSet, t.lastProperty = false, false
	case token.MappingKeyType:
		if t.flow > 0 {
			t.flowKey(at, idx)
			t.lastProperty = false
			return
		}
		t.lastProperty = false
		t.explicitKey(at)
		if t.fatal {
			return
		}
		f := t.block(frameBlockMap, int(at.Column), at, idx)
		t.entry(f, blockEntry{start: idx, colon: -1})
		t.keyStartSet = false
	case token.MappingValueType:
		if t.flow > 0 {
			if t.flowShapeColon(at, idx); t.fatal {
				return
			}
			t.flowColon()
			t.lastProperty = false
			return
		}
		if t.keyStartSet && t.keyStart.Line == at.Line && !t.keyBlock {
			// An implicit key: the entry starts at the key.
			if t.tabIndented(t.keyStartIdx) {
				t.misread(t.keyStart, msgTabKey)
				return
			}
			t.explicitBlock(at, t.keyStart.Column, false)
			if t.fatal {
				return
			}
			f := t.block(frameBlockMap, int(t.keyStart.Column), at, t.keyStartIdx)
			t.entry(f, blockEntry{start: t.keyStartIdx, colon: idx})
		} else {
			// No key on the ':' line. The ':' may complete an explicit
			// key: its '?' entry has no ':' yet, and explicitValue checks
			// that the key is one node (explicit.go). If not, and a node comes
			// before the ':', that node started on an earlier line or is
			// a block scalar (keyStart stays on the header). YAML 1.2
			// keeps an implicit key on one line, but goccy makes that
			// node a key and attaches the entry to the innermost open
			// sequence, whatever column the ':' is at. A ladder of such
			// lines ("- - \"\"\n:") then nests deeper on every line,
			// past MaxDepth, while this algorithm closes back to the
			// ':' column and counts a few levels. The split parse also
			// refuses these lines, with goccy's message. These lines are
			// RZ-CFG-001 here (01 req 8,
			// 9; 11 req 17, 26). At the column of an enclosing block
			// mapping, YAML 1.2 reads such a ':' as an entry with an empty
			// key after the node instead ("a: 1\n: b"), which the profile
			// refuses as goccy does, with its own message. Once the file
			// has a finding, it is never parsed, so the check is skipped
			// and the scan goes on collecting RZ-CFG-003 and RZ-CFG-004. A
			// ':' with no node before it is an empty key, which goccy
			// refuses itself.
			completes := t.explicitValue(at)
			if t.fatal {
				return
			}
			if !completes && t.keyStartSet && t.errs == 0 {
				msg := msgMultiLineKey
				if t.mappingAt(int(at.Column)) {
					msg = msgEmptyKey
				}
				t.fail(at, msg)
				return
			}
			f := t.block(frameBlockMap, int(at.Column), at, idx)
			if completes && f != nil && len(f.entries) > 0 && f.entries[len(f.entries)-1].colon < 0 {
				f.entries[len(f.entries)-1].colon = idx
			} else {
				t.entry(f, blockEntry{start: idx, colon: idx})
			}
		}
		t.keyStartSet, t.lastProperty = false, false
	case token.CollectEntryType:
		if t.flow > 0 {
			t.flowEntryEnd(idx, true)
			t.stack[len(t.stack)-1].shape = flowShape{}
		}
		t.lastProperty = false
	default:
		if t.flow > 0 && tk.Type == token.StringType && tk.Value == "-" && !t.colonFollows(idx) {
			// YAML 1.2 starts a plain scalar with '-' only when a
			// character allowed in a flow plain scalar follows it, so
			// "[-]" and "[-, -]" are invalid; goccy reads such a '-' as a
			// string. Before ':', as in "[-: x]", the '-' is a valid key.
			t.fail(at, `"-" is not a plain scalar inside a flow collection`)
			return
		}
		t.scalar(at, idx)
	}
}

// Messages of a ':' with no key on its line after a node, and of a key
// after a tab.
const (
	msgMultiLineKey = "an implicit key must be on one line"
	msgEmptyKey     = "syntax error: a mapping key is missing"
	msgTabKey       = "syntax error: a block mapping key cannot be indented with a tab; indent it with spaces"
)

// tabIndented reports whether a tab comes before the block mapping key
// starting at token idx in the white space that separates it from the start
// of its line or from the indicator before it (01 req 8, 15). YAML 1.2
// indents block collections with spaces only (s-indent), so "\t\"a\": 1"
// and "a:\n  \t\"b\": 1" are invalid, as yaml.v3 and the YAML Test Suite
// (DK95/06) read them. goccy refuses a plain key there, but reads a quoted
// key, or any key at the start of the input, as if the tab were a space, so
// such a file read otherwise alone than after other entries.
func (t *tokenPass) tabIndented(idx int) bool {
	tk := t.tks[idx]
	off := t.src.offset(tk.Position.Line, t.col(tk))
	for i := off - 1; i >= 0; i-- {
		switch t.src.text[i] {
		case ' ':
		case '\t':
			return true
		default:
			return false
		}
	}
	return false
}

// mappingAt reports whether an entry at column col continues an open block
// mapping: the innermost open collection at or left of col is a mapping at
// col, or a sequence at col inside one.
func (t *tokenPass) mappingAt(col int) bool {
	for i := len(t.stack) - 1; i >= 0; i-- {
		f := t.stack[i]
		switch {
		case f.kind == frameFlow || f.col > col:
			continue
		case f.col < col:
			return false
		case f.kind == frameBlockMap:
			return true
		default:
			return i > 0 && t.stack[i-1].kind == frameBlockMap && t.stack[i-1].col == col
		}
	}
	return false
}

// colonFollows reports whether a ':' follows token idx at once: on its
// line, in the next column.
func (t *tokenPass) colonFollows(idx int) bool {
	if idx+1 >= len(t.tks) {
		return false
	}
	tk, next := t.tks[idx], t.tks[idx+1]
	return next.Type == token.MappingValueType && next.Position.Line == tk.Position.Line && t.col(next) == t.col(tk)+1
}

// Messages of the block indentation checks: an entry whose column matches
// no enclosing block collection after it ends deeper ones, and a mapping
// entry at a block sequence's column outside every mapping there.
const (
	msgIndent = "syntax error: the entry is indented less than the block collection before it but matches no enclosing one"
	msgSeqMap = "syntax error: a mapping entry at the column of a block sequence's entries must belong to the mapping that holds the sequence"
)

// block opens or continues a block collection of kind at column col for
// an entry starting at token start, and returns its record.
//
// An entry that ends block collections deeper than its column must
// continue an enclosing one at its column (YAML 1.2 returns a less
// indented line to the indentation of an enclosing collection), or be a
// mapping entry after a sequence at its key's column; a mapping entry at a
// sequence's column must be that case (msgSeqMap). Otherwise, as in
// "a:\n  k: v\n c: 2", it would open a sibling of the collection it ended
// in the same entry, which goccy's parse of the entry refuses only after
// parsing every collection before it whole, in time quadratic in the
// entries (11 req 17, 26): it is RZ-CFG-001 here, unless the file has a
// finding already and is never parsed.
func (t *tokenPass) block(kind frameKind, col int, at tree.Pos, start int) *blockFrame {
	popped := false
	for len(t.stack) > 0 {
		top := t.stack[len(t.stack)-1]
		if top.kind == frameFlow || top.col <= col {
			break
		}
		popped = true
		t.stack = t.stack[:len(t.stack)-1]
		if top.kind == frameBlockSeq && t.tks[start].Type == token.TagType && t.emptyLastEntry(top.node, start) {
			// goccy reads a tag after an empty last entry as that entry's
			// value and refuses it left of the '-' ("a:\n -\n!!str k: 1"
			// is "tag is not allowed in this sequence context"), while the
			// split parse reads YAML 1.2's {a: [null], k: 1}; the split
			// parse is taken for the sequence too, whatever the split
			// threshold, so the empty entry's null has the position the
			// split parse gives it.
			top.node.split = true
		}
		t.close(top, start)
	}
	n := len(t.stack)
	seqInMap := kind == frameBlockMap && n >= 2 && t.stack[n-1].kind == frameBlockSeq && t.stack[n-1].col == col &&
		t.stack[n-2].kind == frameBlockMap && t.stack[n-2].col == col
	if popped && t.errs == 0 && !seqInMap && (n == 0 || t.stack[n-1].col != col || t.stack[n-1].kind != kind) {
		t.fail(t.posOf(t.tks[start]), msgIndent)
		return nil
	}
	if kind == frameBlockMap && !seqInMap && n > 0 && t.stack[n-1].kind == frameBlockSeq && t.stack[n-1].col == col && t.errs == 0 {
		// A mapping entry at the column of a block sequence's entries
		// continues a mapping that holds the sequence as a value, or
		// nothing: YAML 1.2 indents the mapping of a sequence entry past
		// its '-'. goccy nested it in the last entry ("-\nk: v" as [{k: v}],
		// "rules:\n    -\n    match: x" as {rules: [{match: x}]}; minor
		// finding of the eighth WP-33 review).
		t.fail(t.posOf(t.tks[start]), msgSeqMap)
		return nil
	}
	if seqInMap {
		if t.emptyLastEntry(t.stack[n-1].node, start) {
			// goccy nests the key in an empty last entry ("a:\n-\nb: 1"
			// reads as {a: [{b: 1}]}); the split parse keeps YAML 1.2's
			// structure whatever the split threshold. Marking the
			// sequence marks the mapping too (close).
			t.stack[n-1].node.split = true
		}
		t.close(t.stack[n-1], start)
		t.stack = t.stack[:n-1]
		return t.stack[n-2].node
	}
	if n > 0 && t.stack[n-1].kind == kind && t.stack[n-1].col == col {
		return t.stack[n-1].node
	}
	f := frame{kind: kind, col: col, node: &blockFrame{kind: kind, first: start, end: -1}}
	for i := n - 1; i >= 0; i-- {
		if t.stack[i].node != nil {
			f.node.parent = t.stack[i].node
			break
		}
	}
	t.push(f, at)
	return f.node
}

// emptyLastEntry reports whether the last entry of the block sequence f
// holds nothing but comments up to token end.
func (t *tokenPass) emptyLastEntry(f *blockFrame, end int) bool {
	if f == nil || len(f.entries) == 0 {
		return false
	}
	for i := f.entries[len(f.entries)-1].start + 1; i < end && i < len(t.tks); i++ {
		if t.tks[i].Type != token.CommentType {
			return false
		}
	}
	return true
}

// entry records an entry of a block frame.
func (t *tokenPass) entry(f *blockFrame, e blockEntry) {
	if f != nil {
		f.entries = append(f.entries, e)
	}
}

// close ends the block collection of a frame at token end. A frame with
// at least splitAt entries, or one marked split already, is recorded for
// the split parse, and every enclosing frame is marked, so it is recorded
// when it closes, after this one.
func (t *tokenPass) close(f frame, end int) {
	b := f.node
	if b == nil || b.end >= 0 {
		return
	}
	b.end = end
	if t.splitAt <= 0 || (len(b.entries) < t.splitAt && !b.split) {
		return
	}
	for p := b; p != nil; p = p.parent {
		p.split = true
	}
	if t.splits == nil {
		t.splits = make(map[int]*blockFrame)
	}
	t.splits[b.first] = b
}

// closeAll ends every open block collection at token end.
func (t *tokenPass) closeAll(end int) {
	for i := len(t.stack) - 1; i >= 0; i-- {
		t.close(t.stack[i], end)
	}
}

// push opens a collection and checks the depth limit.
func (t *tokenPass) push(f frame, at tree.Pos) {
	t.stack = append(t.stack, f)
	if len(t.stack) > t.maxDepth {
		t.fail(at, "nesting depth exceeds "+strconv.Itoa(t.maxDepth))
	}
}
