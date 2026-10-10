// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// parseJSON is the strict RFC 8259 front end for .json files (01 req 15,
// architecture R-30): jsonval's scanner checks the grammar, UTF-8,
// surrogate pairing and depth, and this code builds the same tree a YAML
// twin produces. Duplicate names are found here, so every one is reported
// with both positions. The one top-level value must be an object.
func (p *fileParser) parseJSON(src []byte) {
	if !p.checkSize(src) {
		return
	}
	if bytes.HasPrefix(src, []byte{0xEF, 0xBB, 0xBF}) {
		p.add(p.errorAt(codeParse, p.pos(1, 1), "byte order mark (U+FEFF) is not allowed"))
		return
	}
	j := jsonParser{
		p:   p,
		s:   jsonval.NewScanner(src, jsonval.ScanOptions{MaxDepth: p.opts.MaxDepth, AllowDuplicateNames: true}),
		loc: jsonval.NewLocator(src),
	}
	t, err := j.s.Next()
	if err != nil {
		j.syntax(err)
		return
	}
	root, err := j.value(t)
	if err != nil {
		j.syntax(err)
		return
	}
	// After the one top-level value the scanner yields io.EOF, or an error
	// for a second value or any other trailing byte.
	if _, err := j.s.Next(); !errors.Is(err, io.EOF) {
		j.syntax(err)
		return
	}
	if j.failed {
		return
	}
	if root.Kind != tree.KindMap {
		p.add(p.errorAt(codeSchema, root.Pos, "a resource must be a mapping"))
		return
	}
	p.docs = append(p.docs, Document{Root: root, Start: root.Pos})
}

// jsonParser builds a tree from scanner tokens.
type jsonParser struct {
	p      *fileParser
	s      *jsonval.Scanner
	loc    *jsonval.Locator
	failed bool
}

// at returns the position of a byte offset.
func (j *jsonParser) at(off int) tree.Pos {
	pos := j.loc.Position(off)
	return j.p.pos(pos.Line, pos.Column)
}

// syntax reports a scanner failure as RZ-CFG-001 at its offset.
func (j *jsonParser) syntax(err error) {
	var e *jsonval.Error
	if !errors.As(err, &e) {
		j.p.add(j.p.errorAt(codeParse, j.at(0), "invalid JSON: "+err.Error()))
		return
	}
	msg := "invalid JSON: " + e.Msg
	if errors.Is(e, jsonval.ErrDepth) {
		msg = "nesting depth exceeds " + strconv.Itoa(j.p.opts.MaxDepth)
	}
	j.p.add(j.p.errorAt(codeParse, j.at(e.Offset), msg))
}

// value builds the value starting with token t.
func (j *jsonParser) value(t jsonval.Token) (*tree.Node, error) {
	j.p.built()
	n := &tree.Node{Style: tree.StyleJSON, Pos: j.at(t.Offset)}
	switch t.Kind {
	case jsonval.KindNull:
		n.Kind = tree.KindNull
	case jsonval.KindTrue, jsonval.KindFalse:
		n.Kind, n.Bool = tree.KindBool, t.Kind == jsonval.KindTrue
	case jsonval.KindNumber:
		j.number(n, string(j.s.Bytes(t)))
	case jsonval.KindString:
		n.Kind, n.Text = tree.KindString, j.s.Text(t)
	case jsonval.KindArrayStart:
		n.Kind, n.Items = tree.KindList, []*tree.Node{}
		for {
			it, err := j.s.Next()
			if err != nil {
				return nil, err
			}
			if it.Kind == jsonval.KindArrayEnd {
				return n, nil
			}
			v, err := j.value(it)
			if err != nil {
				return nil, err
			}
			n.Items = append(n.Items, v)
		}
	case jsonval.KindObjectStart:
		return n, j.object(n)
	default:
		// Unreachable: the scanner yields only value starts here.
		return nil, &jsonval.Error{Kind: jsonval.ErrSyntax, Msg: "invalid character at start of value", Offset: t.Offset, Other: -1}
	}
	return n, nil
}

// object fills n with the members of an object after its start token.
// Duplicate names are RZ-CFG-002 at the second name, related to the first;
// the first member is kept.
func (j *jsonParser) object(n *tree.Node) error {
	n.Kind, n.Members = tree.KindMap, []tree.Member{}
	var seen map[string]tree.Pos
	for {
		nt, err := j.s.Next()
		if err != nil {
			return err
		}
		if nt.Kind == jsonval.KindObjectEnd {
			return nil
		}
		name, keyPos := j.s.Text(nt), j.at(nt.Offset)
		vt, err := j.s.Next()
		if err != nil {
			return err
		}
		v, err := j.value(vt)
		if err != nil {
			return err
		}
		if seen == nil {
			seen = make(map[string]tree.Pos)
		}
		if first, dup := seen[name]; dup {
			d := j.p.errorAt(codeDuplicate, keyPos, "duplicate key "+strconv.Quote(clip(name)))
			d.Related = []diag.Related{{Location: j.p.location(first), Message: "first defined at"}}
			j.p.add(d)
			j.failed = true
			continue
		}
		seen[name] = keyPos
		n.Members = append(n.Members, tree.Member{Key: name, KeyPos: keyPos, Value: v})
	}
}

// number types a JSON number: an integer literal is KindInt in normalized
// decimal (-0 is 0) and must fit in int64, as YAML integers must (01 req
// 12); any other literal is KindFloat with its exact text, already RFC
// 8259 syntax.
func (j *jsonParser) number(n *tree.Node, lit string) {
	if strings.ContainsAny(lit, ".eE") {
		n.Kind, n.Text = tree.KindFloat, lit
		return
	}
	text, ok := normalizeInt(lit)
	if !ok {
		j.p.add(j.p.errorAt(codeSchema, n.Pos, "integer "+clip(lit)+" is outside the 64-bit range (not representable in the JSON data model)"))
		j.failed = true
	}
	n.Kind, n.Text = tree.KindInt, text
}
