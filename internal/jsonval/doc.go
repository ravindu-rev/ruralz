// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package jsonval is Ruralz's strict JSON library: an RFC 8259 scanner, a
// mutable value tree, a deterministic encoder, the RFC 8785 number form and
// the JSON media-type test.
//
// Production code cannot use encoding/json/jsontext while go.mod declares
// go 1.26.0, so every strict JSON reader in Ruralz goes through this
// package: the configuration JSON front end, canonical decode, transforms,
// validation.json-schema instance decode, JWT claims and the composition
// merge. jsontext appears only in the go1.27 test oracle of this package.
//
// # Scanner
//
// [Scanner] tokenizes one JSON text held in memory. It accepts exactly one
// top-level value surrounded by optional whitespace, requires valid UTF-8,
// rejects unpaired surrogate escapes, limits nesting depth and, unless told
// otherwise, rejects duplicate object member names. Tokens carry byte offsets;
// [Locator] turns offsets into 1-based lines and code-point columns. After
// warm-up, scanning allocates nothing per token.
//
// # Tree
//
// [Decode] and [Decoder] build a tree of *[Object] (members in input order)
// or map[string]any, []any, string, json.Number (the literal text of the
// input number), bool and nil. Every value is charged against a cost budget
// (see [CostValue]) before it is built, so a caller bounds the memory a body
// can take (CM "Body buffering and limits").
//
// # Encoding
//
// [Append] writes a tree without insignificant whitespace, with object
// members in ascending byte order of their names, minimal string escaping (no
// HTML escaping) and numbers verbatim. [AppendCanonical] writes the RFC 8785
// form: members in UTF-16 code unit order and every number as an ECMAScript
// double ([AppendFloat], [AppendCanonicalNumber]).
//
// The transform form keeps member order instead: an [Encoder] with
// [OrderInsertion] writes the members of an *[Object] in their order (input
// order, then insertion order) and those of a map[string]any, which has
// none, in ascending byte order, and a [Compactor] rewrites a JSON text
// compactly with its members in input order, its strings minimally escaped
// ([AppendString], [AppendStringBytes]) and its numbers verbatim, without
// building a tree (docs/architecture/03-data-plane.md "Transform Policies":
// body operations keep each object's member order and each number's text,
// a new member going to the end of its object).
//
// # Concurrency
//
// Package functions are safe for concurrent use. A [Scanner], [Decoder],
// [Encoder], [Compactor] or [Locator] is owned by one goroutine at a time. A tree may be
// read by any number of goroutines while none changes it, since reading an
// *[Object] never writes (a decoded body is shared by every reader);
// changing a tree needs exclusive access.
package jsonval
