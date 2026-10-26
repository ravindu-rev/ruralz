// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// DefaultDumpLimit is the /config/dump body cap ReadDump applies when its
// limit is zero or negative: 256 MiB (proposed).
const DefaultDumpLimit = 256 << 20

// Dump is a /config/dump document read by ReadDump.
type Dump struct {
	// Body is the document exactly as read; ruralz node dump saves these
	// bytes unchanged, so the file is a valid diff source.
	Body []byte
	// Content is the exact canonical bytes, a subslice of Body.
	Content []byte
	// Digest is the active Revision's digest; Content hashes to it.
	Digest revision.Digest
	// LastKnownGood is the Last-Known-Good digest; zero when the Node has
	// none.
	LastKnownGood revision.Digest
}

// AppendDump appends the /config/dump document: the RFC 8785
// serialization of {"content":<canonical document>,"digest":"sha256:...",
// "lastKnownGood":"sha256:...","revision":"rev-..."}, whose members are
// already in RFC 8785 order. The content bytes are copied exactly, never
// re-encoded, and lastKnownGood is omitted when lkg is zero.
//
// active is the published snapshot's Revision: one Encoder.Encode or
// EncodeResources returned, directly or through pipeline.Run or
// pipeline.FromResources. A Node without a Revision has no dump: when
// active has no digest, or its content is not one JSON object that starts
// with the canonical header ({"format":"ruralz.canonical.v1",
// "resources":[) and ends with its trailer (]}), the zero Revision
// included, AppendDump returns dst unchanged and false; otherwise the
// document it appends is one JSON object (see dumpable). It does not
// re-hash the content or check its canonical form, which ReadDump
// verifies on the reading side. The admin server receives this writer as
// an injected handler (Deps.Dump, func() ([]byte, bool)) and serves the
// document as application/json.
func AppendDump(dst []byte, active revision.Revision, lkg revision.Digest) ([]byte, bool) {
	if !dumpable(active) {
		return dst, false
	}
	dst = append(dst, `{"content":`...)
	dst = append(dst, active.Content...)
	dst = append(dst, `,"digest":"`...)
	dst = append(dst, active.Digest.String()...)
	if !lkg.IsZero() {
		dst = append(dst, `","lastKnownGood":"`...)
		dst = append(dst, lkg.String()...)
	}
	dst = append(dst, `","revision":"`...)
	dst = append(dst, active.Digest.Short()...)
	return append(dst, `"}`...), true
}

// dumpable reports whether r can be the content of a dump: a digest, and
// content that is exactly one JSON value, an object, by jsonval's strict
// scanner (no duplicate member names, nesting up to one level less than
// jsonval.MaxDepthLimit, so the dump document, one level deeper, stays
// within the deepest nesting any reader of a dump accepts), starting with
// the canonical header and ending with the trailer, so the dump is one
// JSON object whose content member holds a document of the canonical
// shape. The scan is linear in the content, and costs about fifteen times
// the copy (BenchmarkAppendDump: some 3 ms per MB of content); that is
// the price of never serving a malformed body. Neither the digest nor the
// canonical form is re-checked: a Revision from the encoder is consistent
// by construction, and ReadDump verifies both.
func dumpable(r revision.Revision) bool {
	c := r.Content
	if r.Digest.IsZero() || len(c) < len(header)+len(trailer) ||
		string(c[:len(header)]) != header || string(c[len(c)-len(trailer):]) != trailer {
		return false
	}
	s := jsonval.NewScanner(c, jsonval.ScanOptions{MaxDepth: jsonval.MaxDepthLimit - 1})
	t, err := s.Next()
	if err != nil || t.Kind != jsonval.KindObjectStart {
		return false
	}
	if _, err := skip(s, t, nil); err != nil {
		return false
	}
	_, err = s.Next()
	return errors.Is(err, io.EOF)
}

// ReadDump reads one /config/dump document of at most limit bytes
// (DefaultDumpLimit when limit is zero or negative), checks that its
// content hashes to its digest and that revision names that digest
// (CodeMismatch), and decodes the content as Decode does (CodeNewer,
// CodeMismatch, CodeLimit); positions count from the start of the
// document. Members other than content, digest, lastKnownGood and
// revision are ignored, so a newer Node's additions do not fail it. A
// body over limit is ErrDumpTooLarge and one that is not a dump
// document ErrNotDump, nesting beyond jsonval.MaxDepthLimit included; a
// read failure is returned wrapped, and a Decoder without a schema
// index fails with ErrNoIndex before it reads. ctx is checked before
// decoding; it does not interrupt a blocked read.
func (d *Decoder) ReadDump(ctx context.Context, r io.Reader, limit int64, src Source) (Dump, []tree.Resource, error) {
	if d.idx == nil {
		return Dump{}, nil, ErrNoIndex
	}
	if limit <= 0 {
		limit = DefaultDumpLimit
	}
	y := d.newYielder()
	defer d.done(y)
	// One byte past the limit tells a body at the limit from a larger one.
	body, err := io.ReadAll(io.LimitReader(r, min(limit, math.MaxInt64-1)+1))
	if err != nil {
		return Dump{}, nil, fmt.Errorf("canonical: read /config/dump: %w", err)
	}
	if int64(len(body)) > limit {
		return Dump{}, nil, fmt.Errorf("%w: more than %d bytes", ErrDumpTooLarge, limit)
	}
	// The content's nesting is the decode's to judge (CodeLimit, located),
	// so the scan of the document allows jsonval's own limit.
	f, err := scanDump(body, jsonval.MaxDepthLimit, y)
	if err != nil {
		return Dump{}, nil, err
	}
	dump := Dump{Body: body, Content: body[f.lo:f.hi]}
	if dump.Digest, err = revision.Parse(f.digest); err != nil {
		return Dump{}, nil, fmt.Errorf("%w: digest: %w", ErrNotDump, err)
	}
	if f.hasLKG {
		if dump.LastKnownGood, err = revision.Parse(f.lkg); err != nil {
			return Dump{}, nil, fmt.Errorf("%w: lastKnownGood: %w", ErrNotDump, err)
		}
	}
	if err := hashMatches(dump.Content, dump.Digest, src.Name, y); err != nil {
		return Dump{}, nil, err
	}
	if f.revision != dump.Digest.Short() {
		// A member of the dump document, not of a resource: the path is
		// from the document root.
		p := newLocator(body).Position(f.revOff)
		return Dump{}, nil, fail(CodeMismatch, nil, finding(CodeMismatch, diag.Location{File: src.Name, Line: p.Line, Column: p.Column}, nil, diag.Path{diag.Field("revision")},
			"revision "+strconv.Quote(clip(f.revision))+" does not name digest "+dump.Digest.String()))
	}
	rs, err := d.decode(ctx, newInput(body, f.lo, f.hi, src), y)
	if err != nil {
		return Dump{}, nil, err
	}
	return dump, rs, nil
}

// dumpFields are the members ReadDump reads: the content's byte range,
// the digest strings and the offset of the revision value.
type dumpFields struct {
	lo, hi                int
	revOff                int
	digest, revision, lkg string
	hasContent, hasDigest bool
	hasRevision, hasLKG   bool
}

// scanDump reads the top-level object of a dump document with jsonval's
// strict scanner, recording the content object's byte range and the
// digest, revision and lastKnownGood strings, and skipping other members,
// charging y for each member and token.
func scanDump(body []byte, maxDepth int, y *yielder) (dumpFields, error) {
	var f dumpFields
	s := jsonval.NewScanner(body, jsonval.ScanOptions{MaxDepth: maxDepth})
	t, err := s.Next()
	if err != nil {
		return f, fmt.Errorf("%w: %w", ErrNotDump, err)
	}
	if t.Kind != jsonval.KindObjectStart {
		return f, fmt.Errorf("%w: not a JSON object", ErrNotDump)
	}
	for {
		nt, err := s.Next()
		if err != nil {
			return f, fmt.Errorf("%w: %w", ErrNotDump, err)
		}
		if nt.Kind == jsonval.KindObjectEnd {
			break
		}
		y.charge(siteDump, 1)
		name := s.Text(nt)
		vt, err := s.Next()
		if err != nil {
			return f, fmt.Errorf("%w: %w", ErrNotDump, err)
		}
		switch name {
		case "content":
			if vt.Kind != jsonval.KindObjectStart {
				return f, fmt.Errorf("%w: content is not a JSON object", ErrNotDump)
			}
			end, err := skip(s, vt, y)
			if err != nil {
				return f, fmt.Errorf("%w: %w", ErrNotDump, err)
			}
			f.lo, f.hi, f.hasContent = vt.Offset, end, true
		case "digest", "revision", "lastKnownGood":
			if vt.Kind != jsonval.KindString {
				return f, fmt.Errorf("%w: %s is not a string", ErrNotDump, name)
			}
			switch v := s.Text(vt); name {
			case "digest":
				f.digest, f.hasDigest = v, true
			case "revision":
				f.revision, f.revOff, f.hasRevision = v, vt.Offset, true
			default:
				f.lkg, f.hasLKG = v, true
			}
		default:
			if _, err := skip(s, vt, y); err != nil {
				return f, fmt.Errorf("%w: %w", ErrNotDump, err)
			}
		}
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		return f, fmt.Errorf("%w: %w", ErrNotDump, err)
	}
	switch {
	case !f.hasContent:
		return f, fmt.Errorf("%w: no content member", ErrNotDump)
	case !f.hasDigest:
		return f, fmt.Errorf("%w: no digest member", ErrNotDump)
	case !f.hasRevision:
		return f, fmt.Errorf("%w: no revision member", ErrNotDump)
	}
	return f, nil
}

// skip consumes the value that starts with t and returns the offset just
// past it, charging y for each token of a container.
func skip(s *jsonval.Scanner, t jsonval.Token, y *yielder) (int, error) {
	if t.Kind != jsonval.KindObjectStart && t.Kind != jsonval.KindArrayStart {
		return t.End, nil
	}
	for depth := 1; ; {
		t, err := s.Next()
		if err != nil {
			return 0, err
		}
		y.charge(siteToken, 1)
		switch t.Kind {
		case jsonval.KindObjectStart, jsonval.KindArrayStart:
			depth++
		case jsonval.KindObjectEnd, jsonval.KindArrayEnd:
			if depth--; depth == 0 {
				return t.End, nil
			}
		default:
		}
	}
}
