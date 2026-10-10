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

// Codes this stage raises (01 sections B and C; architecture 3.1 stage B).
const (
	codeParse     = "RZ-CFG-001"
	codeDuplicate = "RZ-CFG-002"
	codeAnchor    = "RZ-CFG-003"
	codeTag       = "RZ-CFG-004"
	codeSchema    = "RZ-CFG-005"
)

// Default limits (01 req 6, OQ-configuration-model-18). A Node never
// enforces a limit below these CLI defaults.
const (
	// DefaultMaxBytes is the per-file size limit: the 64 MiB source
	// limit, which no single file can exceed.
	DefaultMaxBytes = 64 << 20
	// DefaultMaxDepth is the nesting limit (target).
	DefaultMaxDepth = 64
	// DefaultMaxDocumentTokens is the per-document token limit of the YAML
	// token pass (hypothesis, 01 req 6 and risk 1). A document's tokens,
	// goccy's syntax tree and the value tree peak at 360 to 450 bytes of
	// heap per token for the densest shapes (one-character scalars, empty
	// flow collections, single-pair flow mappings), so 1,000,000 tokens
	// took 400 MiB, past the 256 MiB heap budget for an input of 1 MiB (01
	// test plan FuzzProfileYAML; 11 req 17), and
	// 400,000 stay under 180 MiB, which leaves room for the paths of a
	// flow collection (maxFlowPath).
	DefaultMaxDocumentTokens = 400_000
	// DefaultMaxDiagnostics is the per-run diagnostics cap of 01 req 50
	// (10,000, proposed).
	DefaultMaxDiagnostics = 10_000
)

// yieldEvery is the number of tree nodes built between Yield calls (01 req
// 53: "every 256 nodes inside a unit").
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
// so .YAML is not a configuration file (01 req 3).
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
	// (DefaultMaxDocumentTokens); a YAML file may hold five quarters as
	// many in all its documents.
	MaxDocumentTokens int
	// MaxDiagnostics bounds the findings kept for one file
	// (DefaultMaxDiagnostics): after MaxDiagnostics+1 the file stops, so a
	// file of a million anchors costs no more than the pipeline's cap,
	// which stops the run at the same count (diag.Collector).
	MaxDiagnostics int
	// Yield, when set, is called between documents and every 256 nodes
	// built, so the caller's worker can yield the processor (01 req 53).
	// The profile itself never reads the clock.
	Yield func()

	// splitAt overrides defaultSplitAt in tests: a higher threshold lets
	// goccy parse the block collections of fewer entries whole, and a
	// negative one the whole document, as goccy alone reads it.
	splitAt int
	// noRewrite turns the keep-chomping rewrite (chomp.go) off, so tests
	// can compare its result with goccy's own scan.
	noRewrite bool
	// passDepth, when positive, replaces MaxDepth in the token pass only,
	// so tests can reach the converter's own depth bound (yaml.go) with
	// trees the token pass would stop.
	passDepth int
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
	switch {
	case o.splitAt == 0:
		o.splitAt = defaultSplitAt
	case o.splitAt < 0:
		o.splitAt = 0
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

// Parse applies the restricted profile to the bytes of one file (01 B
// 7-14 for YAML, C 15 for JSON) and returns its documents in stream order
// with the diagnostics found. file is the tree file ID recorded in every
// position and path the file name written into diagnostics.
//
// Every diagnostic is an error (RZ-CFG-001 to RZ-CFG-005) with a file,
// line and column. A document with an error is left out of the result, and
// a YAML file whose token pass reported anything yields no documents at
// all; empty, comment-only and null documents are skipped, and every
// returned document's root is a mapping. The returned error is non-nil
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

// canceled records ctx's error and reports whether the call must stop.
func (p *fileParser) canceled(ctx context.Context) bool {
	if p.err == nil {
		p.err = ctx.Err()
	}
	return p.err != nil
}

// built counts a built node and yields every yieldEvery nodes.
func (p *fileParser) built() {
	p.nodes++
	if p.opts.Yield != nil && p.nodes%yieldEvery == 0 {
		p.opts.Yield()
	}
}

// keep adds a parsed document unless its root disqualifies it: a null
// root is skipped (01 req 13) and any other non-mapping root is
// RZ-CFG-005.
func (p *fileParser) keep(d Document) {
	switch {
	case d.Root == nil || d.Root.Kind == tree.KindNull:
		return
	case d.Root.Kind != tree.KindMap && !p.anyRoot:
		p.add(p.errorAt(codeSchema, d.Root.Pos, "a resource must be a mapping"))
		return
	}
	p.docs = append(p.docs, d)
}
