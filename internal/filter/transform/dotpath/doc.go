// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package dotpath parses the dot paths of the transform Policies and edits
// JSON bodies by them (docs/architecture/03-data-plane.md "Transform
// Policies").
//
// # Paths
//
// Parse reads a path with jsonval.ParseDotPath, the parser a composition
// step's target uses too, so both read one syntax: "." separates segments,
// and \., \* and \\ write a literal dot, star or backslash. An empty
// segment, an unescaped "*" inside a longer segment, or another backslash
// escape, is a syntax error, which the Policies report as RZ-CFG-005, and
// so is text that is not UTF-8. A segment that is exactly "*" matches
// every element of an array, in order; a numeric segment, ASCII digits
// that fit an int, indexes an array and names a member of an object, so
// "007" selects element 7 of an array and member "007" of an object.
//
// # Editing
//
// An Editor edits documents in the transform form that Normalize writes
// (jsonval.Compactor): compact, members in their order, strings minimally
// escaped, numbers verbatim. Normalize also checks that a body is strict
// JSON as CEL decodes it: one value of valid UTF-8, no duplicate member
// names, at most 64 levels deep. Each operation reads one document and
// appends the edited one, so every object keeps its member order and every
// number its text, and a new member goes to the end of its object:
//
//   - Remove drops the members and elements a path matches, later array
//     elements moving up.
//   - Extract drops them as Remove does and captures them, and Collect
//     captures them without changing the document: a move takes its values
//     out with Extract, an append with Collect.
//   - Set writes a value at every match, creating missing objects on the
//     way.
//   - Append adds values to the end of an array, creating it when missing.
//   - Replace rewrites the strings a path matches.
//
// Over a missing path, and over a missing or empty array for "*", an
// operation acts on nothing. A path through a scalar, "*" on an object,
// and, for a write, a member name on an array or an index past an array's
// end are *PathError values, as are a Replace match that is not a string
// and an Append target that is not an array. An operation whose output
// passes Editor.Limit stops with ErrOutputCap.
//
// # Cost
//
// The Editor scans with a reused jsonval.Scanner and writes into the
// caller's buffers, copying every untouched value as a byte range, so
// after warm-up Normalize and every operation allocate nothing beyond the
// growth of those buffers; a Replacer may allocate, as a regexp
// replacement does
// (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Per-stage latency budget").
package dotpath
