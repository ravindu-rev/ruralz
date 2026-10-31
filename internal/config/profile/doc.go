// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package profile is stage B of the configuration pipeline: it turns the
// bytes of one Bundle file into value trees under the restricted YAML 1.2
// profile or strict JSON, and writes trees back as restricted-profile YAML
// or JSON. It is the only YAML parser and emitter of Ruralz, and it is
// Ruralz code: no YAML library is linked (ADR-0003).
//
// # YAML
//
// Parse first applies the byte pre-checks: valid UTF-8, no U+FEFF, only
// YAML printable characters; CR LF and CR become LF, and a text that does
// not end with a line break gets one, as the YAML Test Suite reads such a
// stream; neither changes a position.
//
// The parser then reads the text once, by the productions of YAML 1.2.2
// chapters 6 to 9, and builds the value trees directly: there is no token
// or event stage, so time is linear in the input and the memory held is
// the trees'. A syntax error is RZ-CFG-001 at the character that breaks
// the grammar, with a message naming the rule; it leaves its document out,
// and the parse goes on at the next document marker. Where libyaml-based
// parsers read YAML otherwise, the parser follows YAML 1.2: it refuses a
// block mapping key or a compact collection after a tab, a line of a quoted
// scalar or a flow collection, its closing bracket included, indented at
// or left of the block collection holding it, a comment right after a
// token, a value after ':' without white space in a flow collection unless
// the key is quoted ("{a:[]}"), a '?' or '-' before a flow indicator ("[?]",
// "[-]"), a tab where the empty lines after a block scalar or the first
// comment after them need spaces when the document goes on, and an implicit
// key over more than one line or 1,024 characters; and it reads an entry
// with an empty key, whose key is "", a reserved directive, which it
// ignores, a bare document after "...", and a line indented past its
// collection after a block scalar's empty lines, which is content (YAML
// Test Suite S98Z). The deviation list in differential_test.go names every
// such class.
//
// The profile restricts YAML further. Anchors, aliases and merge keys are
// RZ-CFG-003 and tags other than the seven core tags, or %TAG, RZ-CFG-004,
// reported for the whole file; a file with any of them yields no documents
// and none of the findings about their values. A mapping key must be a
// scalar (RZ-CFG-001). A %YAML version other than 1.2, nesting deeper than
// MaxDepth, more than MaxDocumentTokens nodes in a document or five
// quarters of it in the file's documents (the trees of earlier documents
// stay live while a later one is parsed), and more than MaxDiagnostics
// findings stop the file. With a Budget, the nodes of every file of one
// load share it in place of the five quarters, JSON values included. The
// parser recurses only as the nodes nest, so MaxDepth bounds its stack
// too.
//
// Plain scalars are typed by the YAML 1.2 core schema from their text
// (scalar.go), and a core tag forces the type: a value that does not match
// it is RZ-CFG-001 at the tag. Duplicate keys are RZ-CFG-002 with both
// positions, keys comparing as written; empty and null documents are
// skipped, and a root that is not a mapping is RZ-CFG-005. Keys and values
// carry 1-based lines and code-point columns: a node is at its first
// character, a tagged node at its tag, a block mapping at its first entry,
// a flow collection at its bracket, and the implicit null of an empty
// value in the column after its ':' or '-', or after an explicit key's
// position.
//
// The parser is checked against the YAML Test Suite, where the profile's
// refusals are the listed expected failures, and against yaml.v3 as a
// test-only differential oracle (differential_test.go,
// FuzzYAMLv3Differential).
//
// # JSON
//
// .json files go through jsonval's strict RFC 8259 scanner and build the
// same trees: duplicate names are RZ-CFG-002 with both positions, any
// other failure RZ-CFG-001 at the scanner's offset, and the one top-level
// value must be an object.
//
// # Encoding
//
// Encode is the one restricted-profile emitter: block style, two-space
// indentation, "---" separators, strings plain only where the core schema
// reads them back as the same strings, and "${" written "$${". EncodeJSON
// writes the JSON form. Neither depends on a library release.
//
// # Purity
//
// The package reads no file, environment, network or clock; Options.Yield
// lets the caller's worker yield. Functions are safe for concurrent use;
// Parse starts no goroutine.
package profile
