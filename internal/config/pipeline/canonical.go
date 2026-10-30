// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"bytes"
	"context"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// Codes of canonical re-entry, each registered in internal/errcode.
const (
	// CodeLimit is RZ-CFG-001: canonical content past the source limit.
	CodeLimit = "RZ-CFG-001"
	// CodeMismatch is RZ-CFG-027: content whose recomputed canonical bytes
	// differ from it.
	CodeMismatch = "RZ-CFG-027"
)

// Canonical is ruralz.canonical.v1 content decoded for canonical re-entry,
// as canonical.Decoder.Decode, Verify or ReadDump return it.
type Canonical struct {
	// Resources are the decoded envelope trees, in content order.
	Resources []tree.Resource
	// Files is the table the content was decoded into
	// (canonical.Source.Files); nil leaves file names out of locations.
	Files *tree.FileTable
	// Content is the exact canonical bytes the resources were decoded
	// from.
	Content []byte
	// Text is the bytes the resources' positions count in: the whole
	// /config/dump document for ReadDump, whose positions count from its
	// start; nil means Content.
	Text []byte
}

// FromResources runs stages F to M on canonical content (Last-Known-Good,
// a handover candidate, a Control-mode Revision, a /config/dump read as a
// diff source), skipping stages A to E, and rejects content whose
// recomputed canonical bytes differ from it with RZ-CFG-027 at the first
// byte that differs, naming the resource that holds it
// (docs/architecture/02-configuration-model.md "Validation and diff
// semantics"). The stages run as in Run. Stage G writes every default and
// normalizes every value again, so content that leaves a default out, or
// holds a set out of order, differs from its recomputed bytes. A run
// without error has the content's own Revision, and online runs the
// online stage after it. The error is ctx's or a fault of the release; the
// run rewrites c.Resources in place.
func (p *Pipeline) FromResources(ctx context.Context, c Canonical, online bool) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rs := make([]*tree.Resource, len(c.Resources))
	for i := range c.Resources {
		rs[i] = &c.Resources[i]
	}
	text := c.Text
	if text == nil {
		text = c.Content
	}
	res := &Result{Files: c.Files}
	in := input{resources: rs, files: c.Files, online: online, canonical: c.Content, canonicalText: text, isCanonical: true}
	if err := p.finish(ctx, res, newKeeper(p.limits.MaxDiagnostics), in); err != nil {
		return nil, err
	}
	return res, nil
}

// CanonicalRequest names canonical content to re-enter.
type CanonicalRequest struct {
	// Content is the ruralz.canonical.v1 bytes.
	Content []byte
	// Digest is the digest the content must hash to (canonical.Decoder.
	// Verify); the zero Digest decodes without that check.
	Digest revision.Digest
	// Name is the file name diagnostics carry, such as the Last-Known-Good
	// content file; "" prints as "-".
	Name string
	// Online runs the online stage after the digest.
	Online bool
}

// FromCanonical decodes canonical content and re-enters it with
// FromResources. Content longer than Limits.MaxSourceBytes, the encoded
// size a Node accepts (docs/operations/02-zero-downtime-upgrades-and-hot-
// reload.md "Validation gate"), is RZ-CFG-001 at the first byte past it.
// A decode failure is a stopped Result with the decoder's findings:
// RZ-CFG-024 for content from a newer producer, RZ-CFG-027 for content
// that does not hash to its digest or is not in canonical form, RZ-CFG-001
// past a decoder limit. The error is ctx's or a fault of the release.
func (p *Pipeline) FromCanonical(ctx context.Context, req CanonicalRequest) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	files := &tree.FileTable{}
	limit := p.limits.MaxSourceBytes
	if int64(len(req.Content)) > limit {
		files.Add(tree.File{Path: req.Name, Role: tree.RoleCanonical})
		at := jsonval.PositionOf(req.Content, int(limit))
		res := &Result{Files: files, Stopped: true}
		keep := newKeeper(p.limits.MaxDiagnostics)
		keep.add(stageLoad, 0, diag.List{{
			Code: CodeLimit, Severity: diag.SeverityError, Location: diag.Location{File: req.Name, Line: at.Line, Column: at.Column},
			Message: "the canonical content passes the limit of " + strconv.FormatInt(limit, 10) + " bytes",
		}})
		res.Diagnostics = keep.result()
		return res, nil
	}
	src := canonical.Source{Name: req.Name, Files: files}
	var (
		rs  []tree.Resource
		err error
	)
	if req.Digest.IsZero() {
		rs, err = p.decoder.Decode(ctx, req.Content, src)
	} else {
		rs, err = p.decoder.Verify(ctx, req.Content, req.Digest, src)
	}
	if err != nil {
		if _, coded := errcode.CodeOf(err); !coded {
			return nil, err
		}
		keep := newKeeper(p.limits.MaxDiagnostics)
		keep.add(stageLoad, 0, canonical.DiagnosticsOf(err))
		return &Result{Files: files, Diagnostics: keep.result(), Stopped: true}, nil
	}
	return p.FromResources(ctx, Canonical{Resources: rs, Files: files, Content: req.Content}, req.Online)
}

// canonicalText returns the text lookup of canonical content: text, for
// the file of the resources' positions.
func canonicalText(rs []*tree.Resource, text []byte) func(tree.FileID) ([]byte, bool) {
	if len(rs) == 0 {
		return nil
	}
	file := rs[0].Start.File
	return func(id tree.FileID) ([]byte, bool) {
		if id != file || text == nil {
			return nil, false
		}
		return text, true
	}
}

// recomputed compares got, the canonical bytes stages F to L recomputed,
// with want, the content the resources were decoded from, and returns the
// RZ-CFG-027 of the first byte that differs, located in text, the bytes
// positions count in, with the resource that holds it.
func recomputed(got, want, text []byte, rs []*tree.Resource, files *tree.FileTable) (diag.Diagnostic, bool) {
	if bytes.Equal(got, want) {
		return diag.Diagnostic{}, false
	}
	off := 0
	for off < len(got) && off < len(want) && got[off] == want[off] {
		off++
	}
	d := diag.Diagnostic{
		Code: CodeMismatch, Severity: diag.SeverityError,
		Message: "content is not in " + canonical.Format + " form: the canonical bytes recomputed from it differ here (a default left out or a list out of order)",
	}
	// want lies in text at the offset of the content's first byte: the
	// resources' positions count from the start of text.
	base := bytes.Index(text, want)
	if base < 0 {
		base = 0
	}
	at := jsonval.PositionOf(text, base+off)
	var file tree.FileID
	if len(rs) > 0 {
		file = rs[0].Start.File
	}
	pos := tree.Pos{File: file, Line: int32(at.Line), Column: int32(at.Column)} //nolint:gosec // G115: positions within content of at most the source limit.
	if files != nil {
		d.Location = files.Location(pos)
	} else {
		d.Location = diag.Location{Line: at.Line, Column: at.Column}
	}
	// The resource that holds the byte is the last one starting before it.
	for _, r := range rs {
		if before(r.Start, pos) {
			d.Resource = r.ID.ResourceID()
		}
	}
	return d, true
}

// before reports whether a is at or before b.
func before(a, b tree.Pos) bool {
	return a.Line < b.Line || (a.Line == b.Line && a.Column <= b.Column)
}
