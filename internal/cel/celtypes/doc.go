// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package celtypes adapts the internal/expr views to cel.dev/cel-go (03-cel
// requirements 17 to 25): the ruralz.* object types and their field
// accessors (a types.Provider and types.Adapter, no protobuf code generation
// and no reflection), the CEL values of the request views, the activation
// over an expr.Vars, and the expr.Value implementation for JSON documents and
// dyn results.
//
// # Types
//
// [NewProvider] declares ruralz.Request, ruralz.Source, ruralz.Route,
// ruralz.Consumer, ruralz.Auth, ruralz.Response, ruralz.Error,
// ruralz.Upstream, ruralz.Step and ruralz.AI with the fields of 03
// requirement 17, and delegates every other type to a cel-go registry.
// [Variables] lists the CEL variables of an expr.Var set with their declared
// types, for the environment of a place.
//
// # Values
//
// Views are read lazily: [Activation] resolves a variable to a one-pointer
// wrapper, so an expression pays only for the fields it selects. Header maps
// look names up case-insensitively without canonicalizing the key on the
// heap, join repeated field lines with ", " and never show Host in
// request.headers; request.query is parsed on first selection. Every map
// iterates its keys in ascending byte order (03 req 6.3), so comprehensions,
// join results and error selection are deterministic.
//
// [Value] is the expr.Value of the CEL implementation. [FromNative] wraps a
// decoded JSON tree (map[string]any, []any, string, json.Number, bool, nil)
// without copying it: objects iterate in ascending key order, integer
// literals within int64 read as CEL int and every other number as double
// (03 req 25). [FromVal] wraps a dyn result. AppendBody writes the transform
// body form of 03 requirement 41 and Native the equivalent tree.
//
// # Errors
//
// Runtime failures raised here wrap [ErrNull], [ErrBody], [ErrNoSuchKey],
// [ErrNotJSON] or [ErrUnsupported], so internal/cel maps them to an
// expr.ErrorKind with errors.Is through cel-go's error values. Their text
// never contains request data.
//
// # Concurrency
//
// A Provider is immutable after NewProvider and safe for concurrent use.
// Views read the expr structs they wrap and never write them, so concurrent
// evaluations over one expr.Vars are safe while nobody writes it. A Value is
// immutable; an error Value, such as a body that did not decode, hands every
// read a new cel-go error value, because cel-go writes the node ID into the
// error values an evaluation produces. The package starts no goroutine.
package celtypes
