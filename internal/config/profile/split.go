// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"fmt"
	"slices"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// defaultSplitAt is the entry count from which a block collection is
// parsed entry by entry: every block collection is (01 req 8, 9, 10; 11
// req 17, 26). goccy parses a block mapping by recursing once per entry
// and copying the entries parsed so far at each level, so a mapping of n
// entries costs O(n²) time and allocation and n stack frames. It builds
// every node's path by concatenating its parent's, so long keys above
// many nodes cost memory quadratic in the input (a 1,000-byte key at 62
// levels above 4,096 entries took 263 MiB). It inserts the implicit null
// of an empty value by moving every later token of its input, so many
// empty values cost quadratic time. And where it reads block structure
// otherwise than YAML 1.2 (an empty sequence entry before a key, a tagged
// key after an empty value), a wide mapping, which had to be split, and a
// narrow one parsed differently. Parsing every block collection entry by
// entry hands goccy only the keys and the values that are no block
// collection (scalars, flow collections and block scalars), each with a
// path of its own, so none of these costs grows past one value and every
// block structure is the token pass's, whose checks and rangeNode's refuse
// the indentation YAML 1.2 refuses where goccy read a tree (doc.go).
// Options.splitAt raises the threshold in tests, or turns the split off to
// compare with goccy's own parse.
const defaultSplitAt = 1

// splitter parses one document by its block frames: it walks the block
// frames the token pass recorded and hands every entry's key and value to
// goccy separately, so each goccy call sees one key or one value and the
// whole parse is linear. A value with no block frame inside is one goccy
// call; a value that is itself a block collection recurses. The context
// checks goccy makes across an entry (a value or a tag left of or on its
// key's line, an empty key, a tag without content before another token,
// directives, implicit null positions) are made here, so the result is the
// one goccy's whole-document parse gives for every YAML Test Suite stream
// (TestSplitParseMatchesGoccy) and the edge cases of TestSplitEdgeCases,
// except where goccy misreads block structure, such as an empty sequence
// entry followed by a key at the sequence's column or a tagged key after
// an empty value, where the split parse follows the frames of the depth
// algorithm, and where goccy accepts what YAML 1.2 refuses, such as a value
// at its key's column on a later line, which the split parse refuses
// (rangeNode). FuzzLoadYAML compares the two parses where both accept.
type splitter struct {
	c      *converter
	tks    token.Tokens
	frames map[int]*blockFrame
	// starts are the first token indexes of the frames, sorted
	// (blockInside); nil until needed.
	starts []int
	// bodyHi ends the document body.
	bodyHi int
	// stopped is set by the first syntax error, which ends the document
	// as it ends goccy's parse; typing errors and duplicate keys are all
	// reported, as the converter reports them.
	stopped bool
}

// syntax reports a syntax error and stops the document.
func (s *splitter) syntax(at tree.Pos, msg string) *tree.Node {
	s.c.fail(codeParse, at, "syntax error: "+msg)
	s.stopped = true
	return &tree.Node{Kind: tree.KindNull}
}

// rangeNode converts the tokens [lo, hi) holding one node. after is the
// token before them (the ':' or '-' of the entry, or the "---" of the
// document), which places an empty value; sameLine forbids a block
// collection on after's line, as for an implicit key's value; col is the
// column of the entry's first token (its key or '-'), 0 for a document.
//
// A value that starts on a later line than after must be indented past
// col (01 req 8, 10; YAML 1.2 s-l+block-node: its node, properties
// included, is indented by at least n+1), except a block sequence that is a
// mapping value, which may sit at its key's column (seq-spaces). goccy
// accepted "k:\nv" as {k: v}, "-\nv" as [v] and "k:\n[a]" as {k: [a]},
// which YAML 1.2 refuses (minor finding of the eighth WP-33 review). Every
// position here is a source position (converter.tokenPos), never goccy's:
// goccy reports a plain scalar continued by a line starting with '-' at its
// end, column 1 when blank lines end the document, and one with trailing
// spaces too far right, so a check on goccy's columns refused "- run the
// job\n    -v\n\n" and accepted "k:\n value \n" left of its key.
func (s *splitter) rangeNode(lo, hi int, after *token.Token, sameLine bool, col int) *tree.Node {
	lo = s.skipComments(lo, hi)
	if lo >= hi {
		return s.empty(after)
	}
	tk := s.tks[lo]
	at := s.c.tokenPos(tk)
	later := after != nil && at.Line > s.c.tokenPos(after).Line
	switch {
	case tk.Type == token.TagType && int(at.Column) <= col:
		return s.syntax(at, "tag is not allowed in this context")
	case later && after.Type == token.MappingValueType && tk.Type == token.MappingStartType && int(at.Column) <= col:
		// goccy reads a flow mapping that starts a mapping value at its
		// key's column on a later line as a key of the enclosing mapping,
		// and refuses it ("k:\n{a: b}").
		return s.syntax(at, "unexpected map key")
	case later && int(at.Column) <= col && (tk.Type != token.SequenceEntryType || after.Type != token.MappingValueType):
		// Only a block sequence, which starts at its first '-', may be a
		// mapping value at its key's column.
		return s.syntax(at, "value is not allowed in this context")
	}
	start := lo
	var tag *token.Token
	if tk.Type == token.TagType {
		if f, ok := s.frames[s.skipComments(lo+1, hi)]; ok && s.covers(f, hi) {
			tag, lo = tk, f.first
		}
	}
	f, ok := s.frames[lo]
	if !ok || !s.covers(f, hi) {
		return s.goccy(start, hi, after, sameLine)
	}
	if first := s.c.tokenPos(s.tks[lo]); sameLine && after != nil && first.Line == s.c.tokenPos(after).Line {
		return s.syntax(first, notAllowedHere(f.kind))
	}
	n := s.frame(f)
	if tag != nil && !s.halted() {
		want := tagMap
		if f.kind == frameBlockSeq {
			want = tagSeq
		}
		if tag.Value != want {
			s.c.fail(codeParse, s.c.tokenPos(tag), "value does not match tag "+clip(tag.Value))
		}
		n.Pos = s.c.tokenPos(tag)
	}
	return n
}

// notAllowedHere returns goccy's message for a block collection where a
// value of one line must be.
func notAllowedHere(k frameKind) string {
	if k == frameBlockSeq {
		return "block sequence entries are not allowed in this context"
	}
	return "mapping value is not allowed in this context"
}

// empty returns the implicit null of an empty value after token after: in
// the column after it, as the converter places the nulls of goccy's parse
// (converter.placeNull), whatever follows the entry.
func (s *splitter) empty(after *token.Token) *tree.Node {
	n := &tree.Node{Kind: tree.KindNull}
	if after != nil {
		n.Pos = nextColumn(s.c.tokenPos(after))
	}
	return n
}

// covers reports whether frame f spans the tokens up to hi, apart from
// trailing comments.
func (s *splitter) covers(f *blockFrame, hi int) bool {
	return f.end >= 0 && f.end <= hi && s.skipComments(f.end, hi) == hi
}

// skipComments returns the index of the first non-comment token in
// [lo, hi), or hi.
func (s *splitter) skipComments(lo, hi int) int {
	for lo < hi && s.tks[lo].Type == token.CommentType {
		lo++
	}
	return lo
}

// frame converts a split block collection entry by entry. It counts the
// collection's depth as the converter counts goccy's (converter.enter), so
// a too deep tree fails the same way wherever its levels come from; a
// depth failure stops the document like a syntax error.
func (s *splitter) frame(f *blockFrame) *tree.Node {
	s.c.p.built()
	if !s.c.enter(s.c.tokenPos(s.tks[f.first])) {
		s.stopped = true
		return &tree.Node{Kind: tree.KindNull}
	}
	defer s.c.leave()
	if f.kind == frameBlockSeq {
		l := &tree.Node{Kind: tree.KindList, Pos: s.c.tokenPos(s.tks[f.first]), Items: make([]*tree.Node, 0, len(f.entries))}
		for i, e := range f.entries {
			dash := s.tks[e.start]
			l.Items = append(l.Items, s.rangeNode(e.start+1, s.entryEnd(f, i), dash, false, int(s.c.tokenPos(dash).Column)))
			if s.halted() {
				break
			}
		}
		return l
	}
	m := &tree.Node{Kind: tree.KindMap, Members: make([]tree.Member, 0, len(f.entries))}
	seen := make(map[string]tree.Pos, len(f.entries))
	for i, e := range f.entries {
		end := s.entryEnd(f, i)
		keyEnd := end
		if e.colon >= 0 {
			keyEnd = e.colon
		}
		key, keyPos, keyTok, explicit, ok := s.key(e.start, keyEnd, e.colon)
		if s.halted() {
			break
		}
		var value *tree.Node
		switch {
		case e.colon >= 0:
			value = s.rangeNode(e.colon+1, end, s.tks[e.colon], !explicit, int(s.c.tokenPos(s.tks[e.start]).Column))
		default:
			value = s.empty(keyTok)
		}
		if s.halted() {
			break
		}
		if !ok {
			continue
		}
		if len(m.Members) == 0 {
			m.Pos = keyPos
		}
		s.c.addMember(m, seen, key, keyPos, value)
	}
	return m
}

// halted reports whether the document stopped: at a syntax error or at a
// collection deeper than MaxDepth, which goccy's part of the parse may
// have found (converter.enter).
func (s *splitter) halted() bool {
	return s.stopped || s.c.deep
}

// entryEnd returns the index just past entry i of f.
func (s *splitter) entryEnd(f *blockFrame, i int) int {
	if i+1 < len(f.entries) {
		return f.entries[i+1].start
	}
	return f.end
}

// key converts the tokens [lo, hi) of a mapping key: an optional '?' and
// one scalar node, as goccy accepts them. It returns the key's first token
// and whether it is explicit. An empty key is a syntax error, as goccy
// reports it, and a key of any other shape is not a scalar (01 req 10).
func (s *splitter) key(lo, hi, colon int) (key string, at tree.Pos, first *token.Token, explicit, ok bool) {
	lo = s.skipComments(lo, hi)
	explicit = lo < hi && s.tks[lo].Type == token.MappingKeyType
	marker := lo
	if explicit {
		lo = s.skipComments(lo+1, hi)
	}
	if lo >= hi {
		at := marker
		if !explicit && colon >= 0 {
			at = colon
		}
		s.syntax(s.c.tokenPos(s.tks[at]), "a mapping key is missing")
		return "", tree.Pos{}, nil, explicit, false
	}
	at = s.c.tokenPos(s.tks[lo])
	// The entry's first token may start the enclosing frame; any frame
	// after it would be a block collection in the key.
	if s.blockInside(marker+1, hi) >= 0 {
		s.syntax(at, "a mapping key must be a scalar")
		return "", at, nil, explicit, false
	}
	body, err := parseBody(s.tks[lo:hi])
	if err != nil {
		s.syntax(s.c.syntaxPos(err, s.tks[lo]), syntaxMessage(err))
		return "", at, nil, explicit, false
	}
	mk, scalar := body.(ast.MapKeyNode)
	switch body.(type) {
	case nil:
		return "", at, s.tks[lo], explicit, true
	case *ast.MappingNode, *ast.MappingValueNode, *ast.SequenceNode:
		scalar = false
	}
	if !scalar {
		s.syntax(at, "a mapping key must be a scalar")
		return "", at, nil, explicit, false
	}
	// The key reads as the converter reads the keys of goccy's whole
	// parse: as written and never typed, so an integer key out of the
	// 64-bit range is a key like any other (01 req 11).
	failed := s.c.failed
	s.c.failed = false
	key, _, ok = s.c.key(mk)
	bad := s.c.failed || !ok
	s.c.failed = failed || bad
	if bad {
		return "", at, nil, explicit, false
	}
	// goccy places the empty value of an explicit key after the key
	// node's first token.
	return key, at, s.tks[lo], explicit, true
}

// blockInside returns the index of the first token in [lo, hi) that
// starts a block frame, or -1.
func (s *splitter) blockInside(lo, hi int) int {
	if s.starts == nil {
		s.starts = make([]int, 0, len(s.frames))
		for first := range s.frames {
			s.starts = append(s.starts, first)
		}
		slices.Sort(s.starts)
	}
	i, _ := slices.BinarySearch(s.starts, lo)
	if i < len(s.starts) && s.starts[i] < hi {
		return s.starts[i]
	}
	return -1
}

// goccy parses the tokens [lo, hi) as one value node with goccy's parser.
// sameLine forbids a block collection on after's line.
//
// goccy's parser never sees a block collection the token pass recorded: it
// parses a block mapping in time and memory quadratic in its entries (11
// req 17, 26). The token pass gives every value that holds a block
// collection a frame that covers it (rangeNode), so a range holding one is
// refused here, as goccy's parse would refuse it, at the first token past
// the frame or at the frame's start, without parsing it.
func (s *splitter) goccy(lo, hi int, after *token.Token, sameLine bool) *tree.Node {
	if j := s.blockInside(lo, hi); j >= 0 {
		if f := s.frames[j]; f.end >= j && f.end < hi {
			if k := s.skipComments(f.end, hi); k < hi {
				j = k
			}
		}
		return s.syntax(s.c.tokenPos(s.tks[j]), "value is not allowed in this context")
	}
	first := s.tks[s.skipComments(lo, hi)]
	onLine := sameLine && after != nil && s.c.tokenPos(first).Line == s.c.tokenPos(after).Line
	if onLine && first.Type == token.SequenceEntryType {
		return s.syntax(s.c.tokenPos(first), notAllowedHere(frameBlockSeq))
	}
	if last := s.lastToken(lo, hi); last != nil && last.Type == token.TagType {
		// goccy reads the token after a tag with no content as the tag's
		// content, in context, and fails on it.
		if next := s.skipComments(hi, s.bodyHi); next < s.bodyHi {
			return s.syntax(s.c.tokenPos(s.tks[next]), "unexpected scalar value")
		}
	}
	body, err := parseBody(s.tks[lo:hi])
	if err != nil {
		return s.syntax(s.c.syntaxPos(err, first), syntaxMessage(err))
	}
	if onLine && isBlockMapping(body) {
		return s.syntax(s.c.tokenPos(first), notAllowedHere(frameBlockMap))
	}
	return s.c.node(body)
}

// lastToken returns the last non-comment token in [lo, hi), or nil.
func (s *splitter) lastToken(lo, hi int) *token.Token {
	for i := hi - 1; i >= lo; i-- {
		if s.tks[i].Type != token.CommentType {
			return s.tks[i]
		}
	}
	return nil
}

// isBlockMapping reports a block mapping node. goccy v1.19.2 returns a
// mapping as a MappingNode, never as a bare MappingValueNode.
func isBlockMapping(n ast.Node) bool {
	v, ok := n.(*ast.MappingNode)
	return ok && !v.IsFlowStyle
}

// parseBody parses tokens holding one node and returns it; a panic in
// goccy becomes an error.
func parseBody(tks token.Tokens) (body ast.Node, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("YAML parser failed: %v", r)
		}
	}()
	f, err := parser.Parse(tks, 0, parser.AllowDuplicateMapKey())
	if err != nil {
		return nil, err
	}
	var bodies []ast.Node
	for _, d := range f.Docs {
		if d != nil && d.Body != nil {
			bodies = append(bodies, d.Body)
		}
	}
	switch len(bodies) {
	case 0:
		return nil, nil
	case 1:
		return bodies[0], nil
	default:
		return nil, fmt.Errorf("value is not allowed in this context")
	}
}
