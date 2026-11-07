// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package openapi reads OpenAPI 3.0 and 3.1 documents and maps them to
// Bundles for ruralz bundle import openapi
// (docs/reference/01-cli-and-api-surface.md "OpenAPI import"). It is
// Ruralz's own OpenAPI model: YAML goes through the restricted YAML 1.2
// profile of internal/config/profile, the only YAML parser of Ruralz, and
// JSON through the same package's strict RFC 8259 scanner, so no OpenAPI
// or YAML library is linked.
//
// # Load
//
// Load reads the root document, YAML or JSON by its extension (another
// name reads as YAML, which also reads JSON text), and every file its
// references name. The profile's rules hold: anchors, aliases, merge keys
// and custom tags are refused, and so is a file of more than one
// document. A load is bounded as one Bundle load is (Limits): 64 MiB of
// source text, 20,000 files, 64 nesting levels, 1,000,000 nodes in one
// document and 4,000,000 across the files; the first limit passed stops
// it.
//
// The model holds the openapi version, which must be 3.0.x or 3.1.x,
// info, the servers of the document, of each path item and of each
// operation, the paths and the 3.1 webhooks with their Path Item Objects,
// each operation's operationId, tags, parameters, request body, servers
// and, as written, its responses, security and callbacks, and the
// components as written. Members the model does not read, extensions
// among them, are ignored; a member it reads that has the wrong JSON type
// is a problem.
//
// # References
//
// Wherever the model reads an object, a Reference Object resolves to it,
// through any chain of references: path items, parameters and request
// bodies. A $ref is a relative file path, a fragment holding a JSON
// Pointer (RFC 6901), or both, percent decoded (RFC 3986); the file is
// found from the directory of the file that holds the reference, read once
// and parsed once however often it is named. A reference with a scheme,
// such as an http URL, an absolute or network path, a query, a backslash
// or a plain-name fragment is a problem, so a load never reaches the
// network, and so are a pointer that names nothing, a file that cannot be
// read and a chain that returns to a reference it passed. Schemas stay as
// written, their own references unresolved.
//
// Every problem of a load comes back at once, in file and position order,
// as an *Error; a root document that cannot be read is a *ReadError.
//
// # Import
//
// Import maps a document to a Bundle (see Import): one Gateway, one
// Upstream per server the operations use and one Route per operation, in
// canonical order. Files lays the Bundle out as a directory, Stream as a
// one-file Bundle.
//
// # Effects
//
// The package reads files only through Options.FS, never the network or
// the clock, and starts no goroutine. Load and Import are safe to call
// concurrently; a Document is read-only.
package openapi
