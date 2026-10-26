// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"errors"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Codes this stage raises.
const (
	codeParse     = "RZ-CFG-001"
	codeDuplicate = "RZ-CFG-002"
	codeAnchor    = "RZ-CFG-003"
	codeTag       = "RZ-CFG-004"
	codeSchema    = "RZ-CFG-005"
)

// Default limits. A Node never enforces a limit below these CLI defaults.
const (
	// DefaultMaxBytes is the per-file size limit: the 64 MiB source
	// limit, which no single file can exceed.
	DefaultMaxBytes = 64 << 20
	// DefaultMaxDepth is the nesting limit (target).
	DefaultMaxDepth = 64
	// DefaultMaxDocumentTokens is the per-document node limit of the YAML
	// parser: it counts every scalar, key, collection and empty node of a
	// document's tree as the parser builds it, and a file's documents may
	// hold five quarters of it. The parser holds no tokens, so the memory
	// of a document is its tree's: about 110 to 160 bytes per node, which
	// keeps the densest input of 1 MiB, about 600,000 nodes, far below the
	// 256 MiB heap budget, and a file at its limit below 200 MiB.
	DefaultMaxDocumentTokens = 1_000_000
	// DefaultMaxDiagnostics is the per-run diagnostics cap (10,000,
	// proposed).
	DefaultMaxDiagnostics = 10_000
)

// yieldEvery is the number of tree nodes built between Yield calls.
const yieldEvery = 256

// Format is a source file format, chosen by file extension.
type Format uint8

// Formats.
const (
	// FormatYAML is the restricted YAML 1.2 profile (.yaml, .yml).
	FormatYAML Format = iota + 1
	// FormatJSON is strict RFC 8259 JSON (.json).
	FormatJSON
)

// String returns "yaml" or "json".
func (f Format) String() string {
	switch f {
	case FormatYAML:
		return "yaml"
	case FormatJSON:
		return "json"
	default:
		return "unknown"
	}
}

// FormatOf returns the format of a file name by its extension: .yaml and
// .yml are YAML and .json is JSON. Matching is case-sensitive on every OS,
// so .YAML is not a configuration file.
func FormatOf(name string) (Format, bool) {
	switch {
	case strings.HasSuffix(name, ".yaml"), strings.HasSuffix(name, ".yml"):
		return FormatYAML, true
	case strings.HasSuffix(name, ".json"):
		return FormatJSON, true
	default:
		return 0, false
	}
}

// Options configures Parse. Zero limits take their defaults.
type Options struct {
	// MaxBytes is the largest accepted file, in bytes (DefaultMaxBytes).
	MaxBytes int
	// MaxDepth is the deepest accepted nesting; the root mapping of a
	// document is depth 1 (DefaultMaxDepth).
	MaxDepth int
	// MaxDocumentTokens is the most tokens one YAML document may hold
	// (DefaultMaxDocumentTokens), a token being a node of its tree as the
	// parser builds it: a scalar, key, collection or empty node. A YAML
	// file may hold five quarters as many in all its documents.
	MaxDocumentTokens int
	// MaxDiagnostics bounds the findings kept for one file
	// (DefaultMaxDiagnostics): after MaxDiagnostics+1 the file stops, so a
	// file of a million anchors costs no more than the pipeline's cap,
	// which stops the run at the same count (diag.Collector).
	MaxDiagnostics int
	// Yield, when set, is called between documents and every 256 nodes
	// built, so the caller's worker can yield the processor. The profile
	// itself never reads the clock.
	Yield func()
}

func (o Options) withDefaults() Options {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultMaxDepth
	}
	if o.MaxDocumentTokens <= 0 {
		o.MaxDocumentTokens = DefaultMaxDocumentTokens
	}
	if o.MaxDiagnostics <= 0 {
		o.MaxDiagnostics = DefaultMaxDiagnostics
	}
	return o
}

// Document is one parsed document of a file.
type Document struct {
	// Root is the document's value: a KindMap node for every document
	// Parse returns.
	Root *tree.Node
	// Start is where the document starts: its "---" marker when present,
	// otherwise its first value.
	Start tree.Pos
}

// ErrFormat reports a Format other than FormatYAML and FormatJSON.
var ErrFormat = errors.New("profile: unknown format")

// Parse applies the restricted profile to the bytes of one file and returns
// its documents in stream order with the diagnostics found. file is the
// tree file ID recorded in every position and path the file name written
// into diagnostics.
//
// Every diagnostic is an error (RZ-CFG-001 to RZ-CFG-005) with a file,
// line and column. A document with an error is left out of the result, and
// a YAML file with an anchor, alias, merge key or tag outside the profile,
// or a finding that stops the file, yields no documents at all; empty,
// comment-only and null documents are skipped, and every returned
// document's root is a mapping. The returned error is non-nil
// only when ctx ends (ctx.Err()) or f is unknown (ErrFormat); there is no
// partial result then.
func Parse(ctx context.Context, src []byte, file tree.FileID, path string, f Format, o Options) ([]Document, diag.List, error) {
	return parse(ctx, src, file, path, f, o, false)
}

// parse is Parse; anyRoot keeps documents whose root is not a mapping, for
// the YAML Test Suite runner.
func parse(ctx context.Context, src []byte, file tree.FileID, path string, f Format, o Options, anyRoot bool) ([]Document, diag.List, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	p := &fileParser{file: file, path: path, opts: o.withDefaults(), anyRoot: anyRoot}
	switch f {
	case FormatYAML:
		p.parseYAML(ctx, src)
	case FormatJSON:
		p.parseJSON(src)
	default:
		return nil, nil, ErrFormat
	}
	if p.err != nil {
		return nil, nil, p.err
	}
	return p.docs, p.diags, nil
}

// fileParser holds the state of one Parse call.
type fileParser struct {
	file    tree.FileID
	path    string
	opts    Options
	anyRoot bool

	docs  []Document
	diags diag.List
	err   error
	nodes int
	// found counts the findings of the file; full is set once it passes
	// MaxDiagnostics, which stops the file.
	found int
	full  bool
}

// add records a finding, keeping at most MaxDiagnostics+1 per file.
func (p *fileParser) add(d diag.Diagnostic) {
	if p.full {
		return
	}
	p.found++
	p.diags = append(p.diags, d)
	if p.found > p.opts.MaxDiagnostics {
		p.full = true
	}
}

// pos returns a tree position in the parsed file.
func (p *fileParser) pos(line, column int) tree.Pos {
	return tree.Pos{File: p.file, Line: int32(line), Column: int32(column)} //nolint:gosec // G115: lines and columns of a file under MaxBytes fit in int32
}

// location converts a tree position to a diagnostic location.
func (p *fileParser) location(at tree.Pos) diag.Location {
	return diag.Location{File: p.path, Line: int(at.Line), Column: int(at.Column)}
}

// errorAt builds an error diagnostic at a position.
func (p *fileParser) errorAt(code string, at tree.Pos, msg string) diag.Diagnostic {
	return diag.Diagnostic{Code: code, Severity: diag.SeverityError, Location: p.location(at), Message: msg}
}

// built counts a built node and yields every yieldEvery nodes.
func (p *fileParser) built() {
	p.nodes++
	if p.opts.Yield != nil && p.nodes%yieldEvery == 0 {
		p.opts.Yield()
	}
}
