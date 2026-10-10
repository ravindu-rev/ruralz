// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package profile is stage B of the configuration pipeline (architecture
// 3.1): it turns the bytes of one Bundle file into value trees under the
// restricted YAML 1.2 profile or strict JSON, and writes trees back as
// restricted-profile YAML or JSON. It is the only package that imports
// goccy/go-yaml (architecture 1.3).
//
// # YAML (01 B 7-14)
//
// Parse first applies the byte pre-checks: valid UTF-8, no U+FEFF, only
// YAML printable characters; CR LF and CR become LF, and a text that does
// not end with a line break gets one, as the YAML Test Suite reads such a
// stream; neither changes a position. The text is then cut into documents
// at their markers, and each document goes through the token pass and the
// parse in turn, so peak token memory is one document's.
//
// The token pass reads goccy's scanner tokens. It reports anchors,
// aliases and merge keys (RZ-CFG-003) and tags other than the seven YAML
// 1.2 core tags or %TAG (RZ-CFG-004) for the whole file, and stops the
// file at the first RZ-CFG-001: a %YAML version other than 1.2, an invalid
// token, nesting deeper than MaxDepth, more than MaxDocumentTokens tokens
// in a document or five quarters of it in the file's documents (the trees
// of earlier documents stay live while a later one is parsed), a "..."
// goccy reads as a document end on a line that is no marker or a token
// after a document end (segment.go), or a shape goccy reads otherwise than
// YAML 1.2. These are a block sequence entry or a lone plain "-" inside a
// flow collection, a node with two tags, a tag that ends its line before a
// token at or left of its block collection's column (goccy nests "b: 1" in
// "a: !!map\nb: 1" under a), a block collection on its tag's line, an
// entry indented less than the block collection before it at no enclosing
// collection's column, a mapping entry at a block sequence's column that
// no mapping there holds ("-\nk: v"), a block mapping key after a tab on
// its line, a ':' with no key on its line after a node that started on an
// earlier line or is a block scalar (goccy reads a multi-line implicit
// key), an explicit entry ('?') whose key is not one node on the '?' line,
// is empty, or is followed by a node that is neither its ':' nor a new
// entry, or in a flow collection holds more than one node or a tag goccy
// does not group with its content (explicit.go), a plain scalar goccy
// scanned out of a quoted scalar, a property or a flow collection, or a
// lone "?" (plain.go), a quoted scalar with an escape YAML 1.2 does not
// define (quoted.go), a block scalar inside a flow collection, one whose
// header holds more than one chomping and one indentation indicator, or
// one whose lines goccy reads to another end (literal.go), a flow entry
// holding more than an optional '?', a node, a ':' and a node, which goccy
// reads by columns and without ',', and a ':' goccy reads otherwise inside
// a flow collection: followed by a character a plain scalar holds, which
// goccy splits as a value indicator while a flow mapping is open
// ("{app:web}" read as {app: web}), or before a flow indicator outside one
// ("[a:]" read as ["a:"]) (flowentry.go), and a flow collection whose
// parse by goccy would pass a memory or time bound (flowcost.go). The
// checks that only keep goccy from misreading a file are skipped once the
// file has a finding: it is never parsed then, and the scan goes on
// collecting RZ-CFG-003 and RZ-CFG-004. The token count is first bounded
// from the bytes before tokenizing; the bound over-counts indicators
// inside quoted and block scalars, so a document whose bound passes the
// limit is refused as too large to tokenize even when its exact count
// would not.
//
// goccy's scanner reads some valid text otherwise than YAML 1.2, so a
// document is scanned with one character rewritten for one where that
// changes no value and no position: the blank lines that end it are left
// out (an indentation indicator before them was refused), a tab between a
// quoted key and its ':' is read as a space, a keep-chomping block scalar
// of blank lines takes strip chomping (goccy refused the next token), and,
// when goccy's scan holds them, the tabs inside a double-quoted scalar
// (goccy stepped past the scalar's end, losing the line break after it)
// and the tab that ends a tag ("!!str\tx") are read as spaces (yaml.go,
// chomp.go). The token pass also reads from the source the scalars whose
// value or position goccy's scanner gets wrong, and those readings win:
// every quoted scalar (goccy drops the spaces escapes give before a line
// break), every block scalar (goccy drops trailing spaces and miscounts
// empty lines), every plain scalar over several lines (after a line
// starting with '-' goccy keeps a comment in the value and reports a later
// line, and it miscounts empty lines of spaces) and every plain scalar
// holding a tab (goccy leaves the tab out of the value).
//
// A file with any token-pass finding is not parsed. Each document is parsed
// entry by entry along the block collections the token pass found
// (split.go): goccy's parser reads every key and every value that is not a
// block collection on its own, so no goccy cost grows past one value, and
// the block structure is the token pass's, which refuses what YAML 1.2's
// indentation rules refuse where goccy reads a tree: a value on a later
// line at or left of its entry's column, except a block sequence that is a
// mapping value, and the shapes above. A document whose tokens do not hold
// one body is refused without goccy's parse of it (yaml.go), and the split
// parse never hands goccy a range holding a block collection (split.go).
// Ruralz code then types every plain scalar by the YAML 1.2 core schema
// from its text, never from goccy's token type, reports duplicate keys
// with both positions (RZ-CFG-002), skips empty and null documents and
// refuses a root that is not a mapping (RZ-CFG-005). The conversion also
// counts the depth of the tree it builds and fails a document with
// RZ-CFG-001 at the first collection deeper than MaxDepth, so no shape the
// token pass mispredicts returns a deeper tree (flow pairs such as "[k:
// [k: v]]" add a level per flow sequence that the token pass does not
// count). That bound covers the returned tree only: goccy has parsed the
// value, with every node and path of it, before the conversion starts, so
// the cost of goccy's parse rests on the token pass predicting goccy's
// nesting from columns in block context and exactly inside a flow
// collection, which goccy parses whole. Keys and values carry 1-based
// lines and code-point columns. goccy's scanner puts every token after a
// tag or after most tabs one column short, and a plain scalar before
// trailing spaces too far right, so every token is located in the source
// and every reported position corrected (columns.go); the implicit null of
// an empty value sits in the column after its ':', '-' or explicit key,
// whatever follows the entry.
//
// goccy v1.19.2 costs are kept bounded without changing a result: the
// entry-by-entry parse keeps wide block mappings, empty values and long
// keys linear (split.go), a document with a long run of blank lines has its
// clipping block scalars scanned with keep chomping (chomp.go), and the
// paths and inserted nulls of a flow collection are bounded (flowcost.go).
// The entry-by-entry parse and the keep-chomping rewrite are checked
// against goccy's own parse over the YAML Test Suite.
//
// goccy v1.19.2 refuses some valid YAML 1.2, and the profile keeps it
// refused (TestGoccyLimitations, the reviewed entries of the YAML Test
// Suite's expected-failures.txt): a flow pair whose value is on a later
// line at or left of its key ("[a:\n b]", "[? a\n : b]"), an empty value
// or explicit key in a flow sequence ("[a: ]", "[k: ,]", "[? a]"), a ','
// that starts a line after a flow pair, a ':' after a JSON-like key in a
// flow sequence outside a flow mapping ("[\"a\":b]"), a plain scalar in a
// flow collection continued by a line starting with '-' or another plain
// line ("{a: b\n c}"), a tag that ends its line before a block scalar
// header, a value over several lines after a tagged implicit key ("!!str
// k: x\n  y") or a quoted explicit key, an explicit key whose tag ends the
// '?' line, a tagged empty node before a sibling ("a: !!str\nb: 1"), and a
// line of white space holding a tab between two lines of content
// ("a: 1\n\t\nb: 2") or after the last.
//
// # JSON (01 C 15)
//
// .json files go through jsonval's strict RFC 8259 scanner and build the
// same trees: duplicate names are RZ-CFG-002 with both positions, any
// other failure RZ-CFG-001 at the scanner's offset, and the one top-level
// value must be an object.
//
// # Encoding (01 req 31; 02 req 44-47; architecture R-29)
//
// Encode is the one restricted-profile emitter: block style, two-space
// indentation, "---" separators, the quoting rule of 02 req 45 and "${"
// written "$${". EncodeJSON writes the JSON form. Neither depends on a
// library release.
//
// # Purity
//
// The package reads no file, environment, network or clock (01 req 55);
// Options.Yield lets the caller's worker yield. Functions are safe for
// concurrent use; Parse starts no goroutine.
package profile
