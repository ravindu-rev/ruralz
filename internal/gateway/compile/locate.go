// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// maxMessage bounds the bytes of a message built from another package's
// error text, as the pipeline's stages bound theirs
// (docs/architecture/02-configuration-model.md "Semantic validation").
const maxMessage = 512

// locator turns positions in a Revision's resources into source
// locations.
type locator struct {
	files *tree.FileTable
}

// pos returns the location of position p of resource r: through the run's
// file table, else with r's own file name when p lies in the file of r's
// root, otherwise, and for an unknown p, r's start.
func (l locator) pos(r *hub.Resource, p tree.Pos) diag.Location {
	if !p.Known() {
		return r.Source.Start
	}
	if l.files != nil {
		if loc := l.files.Location(p); loc.File != "" {
			return loc
		}
	}
	if r.Tree != nil && p.File == r.Tree.Pos.File && r.Source.File != "" {
		return diag.Location{File: r.Source.File, Line: int(p.Line), Column: int(p.Column)}
	}
	return r.Source.Start
}

// at returns the location of the field at path of r, or of its nearest
// authored ancestor when the field is missing or a default stage G
// materialized.
func (l locator) at(r *hub.Resource, path diag.Path) diag.Location {
	if r == nil {
		return diag.Location{}
	}
	n := r.Tree
	var best tree.Pos
	if n != nil && n.Style != tree.StyleDefaulted {
		best = n.Pos
	}
	for _, e := range path {
		next, ok := n.At(diag.Path{e})
		if !ok || next == nil || next.Style == tree.StyleDefaulted {
			break
		}
		n = next
		if n.Pos.Known() {
			best = n.Pos
		}
	}
	return l.pos(r, best)
}

// finding returns the error diagnostic of a refusal with code at path of
// r: the cause of err's outermost errcode.Error, without the code, cut to
// maxMessage bytes.
func (l locator) finding(code string, r *hub.Resource, path diag.Path, err error) diag.Diagnostic {
	return diag.Diagnostic{
		Code: code, Severity: diag.SeverityError, Location: l.at(r, path),
		Resource: r.ResourceID(), Path: path, Message: causeText(err),
	}
}

// causeText returns err's message for a diagnostic: the cause of its
// outermost errcode.Error, cut to maxMessage bytes on a code point
// boundary.
func causeText(err error) string {
	msg := err.Error()
	var ce *errcode.Error
	if errors.As(err, &ce) && ce.Err != nil {
		msg = ce.Err.Error()
	}
	msg = strings.ToValidUTF8(msg, "�")
	if len(msg) <= maxMessage {
		return msg
	}
	cut := maxMessage
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "..."
}

// specPath returns the path spec.<fields...>.
func specPath(fields ...string) diag.Path {
	p := make(diag.Path, 0, len(fields)+1)
	p = append(p, diag.Field("spec"))
	for _, f := range fields {
		p = append(p, diag.Field(f))
	}
	return p
}
