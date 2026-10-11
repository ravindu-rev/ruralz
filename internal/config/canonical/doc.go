// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package canonical is stage L of the configuration pipeline: the
// ruralz.canonical.v1 serialization that a Revision digest covers (02
// reqs 21 to 28), its strict decoder and verification (02 reqs 29 to 34,
// architecture R-47 and R-64), and the /config/dump document (02 reqs 73
// and 74, architecture R-57).
//
// # Encoding
//
// [Encoder.Encode] writes the materialized trees of a hub.Bundle (stage G
// output, hub.Resource.Tree) as one RFC 8785 (JCS) JSON object:
//
//	{"format":"ruralz.canonical.v1","resources":[{"kind":"Gateway","metadata":{...},"spec":{...}},...]}
//
// Resources are sorted by hub.KindOrder, then by name bytewise. Each
// resource object holds kind, metadata (name, labels and annotations,
// minus the ruralz.io/conversion-data annotation) and spec, and never
// apiVersion, file names, comments or Environment names, so converting a
// Bundle keeps its Revision. Object members are sorted by UTF-16 code
// units, strings are escaped minimally with no HTML escaping (CEL text
// such as response.status >= 400 && x < 2 stays byte-exact), numbers take
// the ECMAScript form of their double through
// jsonval.AppendCanonicalNumber, and null is never written. The document
// has no whitespace and no trailing newline, and its SHA-256 is the
// Revision digest (internal/config/revision).
//
// A field whose schema carries x-ruralz-since above 0 is written only
// when its value differs from its default, compared by canonical bytes,
// and only when present when it has no default (02 req 23). The schema
// level of the served apiVersion is the maximum x-ruralz-since
// (schemaidx.Index.Level); at level 0, which is every M1 schema, no field
// carries the keyword and the encoder does not consult the schema at all.
// Options.Level names the level to render at; this release renders only
// its own (02 req 28).
//
// The encoder does not normalize: sorting set and map lists, scalar
// normalization and defaults belong to stage G (internal/config/defaults),
// which hands over trees already in their canonical shape.
//
// # Decoding and verification
//
// [Decoder.Decode] reads canonical content (Last-Known-Good, a handover
// candidate, a /config/dump document, a build output used as a diff
// source) with jsonval's strict scanner into []tree.Resource whose
// positions refer to a tree.RoleCanonical file, ready for
// pipeline.FromResources to run stages F to M on them (R-47). Each
// resource's tree is the usual envelope, with an apiVersion member naming
// the served hub apiVersion added in front. Decode rejects:
//
//   - content from a newer producer with RZ-CFG-024: a format other than
//     ruralz.canonical.v1, a top-level or resource member the format does
//     not define, a kind this release does not know, a field unknown to
//     the hub schema or whose x-ruralz-since exceeds the served level (02
//     req 33);
//   - anything else that is not exactly ruralz.canonical.v1 bytes with
//     RZ-CFG-027 "content is not in ruralz.canonical.v1 form": invalid
//     JSON, a wrong shape, null, whitespace, member or resource order,
//     duplicates, a number or string not in its RFC 8785 form, a since
//     field written at its default. The check re-encodes every decoded
//     resource and compares the bytes (02 req 31(b)).
//
// Decode applies no integer range rule: an integer literal above 2^53
// there is the RFC 8785 form of a double (R-64). It does not materialize
// defaults either; omitted since fields regain theirs when FromResources
// re-runs stage G (02 req 33).
//
// [Decoder.Verify] first checks that the content hashes to the expected
// digest (RZ-CFG-027 "content hashes to sha256:X, want sha256:Y", 02 req
// 31(a)) and then decodes it. Failures are errcode errors carrying
// RZ-CFG-024 or RZ-CFG-027 whose cause is a [*Findings] with one
// diag.Diagnostic per problem, located in the content; [DiagnosticsOf]
// extracts them.
//
// # /config/dump
//
// [AppendDump] writes the document the admin server serves (the handler
// is injected into the admin server, R-57): the RFC 8785 serialization of
// {"content":<canonical document>,"digest":"sha256:...","lastKnownGood":
// "sha256:...","revision":"rev-..."} with the content bytes copied exactly
// and lastKnownGood omitted when there is none. [Decoder.ReadDump] reads
// one with a size cap, verifies the content against its digest and keeps
// the received bytes, so ruralz node dump saves them unchanged (02 req 74).
//
// # Concurrency
//
// Encoders and Decoders are immutable after construction and safe for
// concurrent use. A call with Workers above 1 owns that many goroutines,
// feeds them chunks of resources, stops handing out chunks once its
// context ends or a chunk fails, and waits for every goroutine before it
// returns; output never depends on scheduling. The package does not log,
// read the clock or perform I/O beyond the reader given to ReadDump.
package canonical
