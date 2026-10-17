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
// and only when present when it has no default (02 req 23). Its value is
// written once and taken back when its bytes are the default's, which is
// encoded once per goroutine of a call, so since positions nested in one
// another cost one encoding of each value: the work is linear in the
// trees. The schema level of the served apiVersion is the maximum
// x-ruralz-since (schemaidx.Index.Level); at level 0, which is every M1
// schema, no field carries the keyword and the encoder does not consult
// the schema at all. Options.Level names the level to render at; this
// release renders only its own (02 req 28).
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
//     the string ruralz.canonical.v1 (of any JSON type), a top-level or
//     resource member the format does not define, a kind this release does
//     not know, a field unknown to the hub schema or whose x-ruralz-since
//     exceeds the served level (02 req 33);
//   - anything else that is not exactly ruralz.canonical.v1 bytes with
//     RZ-CFG-027 "content is not in ruralz.canonical.v1 form": invalid
//     JSON, a wrong shape, null, whitespace, member or resource order,
//     duplicates, a number or string not in its RFC 8785 form, a since
//     field written at its default. The check re-encodes every decoded
//     resource and compares the bytes (02 req 31(b));
//   - content past a limit of the decoder with RZ-CFG-001, the code of the
//     loader's size and depth limits (01 req 6, 10 req 32): nesting deeper
//     than DecoderOptions.MaxDepth (128 by default), more resources than
//     MaxResources (20,000, the loader's) or more JSON values than
//     MaxValues (2^26, one per byte of the loader's 64 MiB source limit),
//     defaults no Revision of this release exceeds (proposed; see Memory
//     below).
//
// A newer producer wins over a form problem wherever each appears, so an
// older Node reports RZ-CFG-024 for content it cannot judge: every newer
// finding of the document, its resources and their fields is reported,
// and only when there is none are the form findings. The fields of a
// resource whose kind the hub schema knows are walked even when its
// envelope has a form problem (no string metadata.name, or a kind such as
// Environment that the format never holds); a resource without a string
// kind, or of a kind this release does not know, is judged by its
// envelope alone. Three outcomes end the judging where they occur and are
// reported alone, one finding, whatever came before them (unless more
// newer findings than the cap did, below): invalid JSON (RZ-CFG-027),
// since no tree can be built past it, a limit (RZ-CFG-001), since the
// content past it is not read, and a format other than the string
// ruralz.canonical.v1 (RZ-CFG-024), since the content after it follows
// rules this release does not know. Values are not judged: a value outside
// an enum or a Policy type this release does not register passes Decode
// and is stage F's (RZ-CFG-005) when FromResources re-runs the pipeline.
// Each code's findings are in content order.
//
// A finding about a resource whose kind and metadata.name are strings
// names that resource (a kind this release does not know included) and
// has a path from the resource root (status, kind,
// spec.upstreams[name=a].x); one about a resource without them, or about
// the document, names none and has a path from the document root
// (resources[3], resources[3].spec.x, schemaLevel). Text from the content
// in a finding is bounded: a quoted value in a message, a resource name
// or kind, and each member name, list key and set element of a path keep
// at most 64 bytes, cut before a rune and marked "..."; a set element
// whose canonical JSON is longer is named by its index, and a resource
// name or kind that holds a character that is not graphic, a quote, a
// backslash or invalid UTF-8 is shown Go-quoted, an empty one as "". The
// diagnostics of a decode therefore print in space linear in their
// number, whatever the content, and every text they hold is a copy, so an
// error kept for a status or a log keeps none of the content alive.
// Member names, keys and set elements of a path are quoted by diag, which
// leaves C1 controls and format characters such as U+202E unescaped
// (WP-39 contract request for internal/config/diag).
//
// A decode keeps at most DecoderOptions.MaxDiagnostics findings (01 req
// 50, 10 req 32; 10,000 by default). Each code has its own budget, so a
// newer producer still wins. Once the parser has completed elements and
// document members holding more newer findings than the cap, it stops,
// and the decode reports the first MaxDiagnostics in content order and
// one RZ-CFG-001 "too many diagnostics; stopped after N"; that outcome
// comes before invalid JSON, a limit and another format later in the
// content, which the parser does not reach. Form findings past the cap
// are dropped without stopping the parse, which goes on looking for newer
// findings, and are reported the same way. The kept findings, and whether
// the cap was passed, depend only on the content, never on the worker
// count or on scheduling. Every count option means at most 2^30 and
// Workers at most 256, so math.MaxInt is a valid "no cap" and no option
// overflows the decoder's arithmetic.
//
// # Memory
//
// A decode copies the content into one string, which the trees share, and
// builds a tree.Node of 112 bytes and a member or item slot per JSON
// value: about 120 to 160 bytes per value, so the trees of realistic
// content take about 11 to 14 times its bytes and those of content made of
// short values (a list of zeros) up to some 90 times. The trees and the
// copy they share of the PBB ladder Revisions measure about 13 times the
// content (12.8 times for 5,000 Routes: 19.5 MB beside 1.5 MB), against
// the target of four times the canonical size for the peak validation
// memory of a Node loading canonical content (configuration model,
// Validation and diff semantics; 02 req 78), which the trees alone miss
// before stages F to M run (WP-39 contract request for 02 req 78 and
// architecture R-47, whose []tree.Resource result fixes this
// representation). MaxValues and MaxResources bound them: at the
// defaults, which no Revision of this release reaches, the trees would
// take some 8 to 10 GiB, so a 256 MiB /config/dump (DefaultDumpLimit)
// whose one resource holds short values reaches the value limit after
// about 128 MiB of its content, holding some 8 GiB of trees by then; a
// caller with less memory passes lower limits.
//
// A decode known to fail (a finding, or a resource whose re-encoding
// differs) keeps no tree it has checked: the trees a check kept before a
// finding was known, on any goroutine, are dropped when the parser
// completes its next chunk after it is (those checked after a differing
// re-encoding but before it was known are kept until the decode returns,
// as a decode that succeeds keeps its own), and the parser's scratch
// keeps neither links to an element nor, past 4,096 slots, its width.
// What a failing decode holds
// is the content, the parser's copy of it, the findings kept, the
// scanner's records of the member names of the widest object read
// (about 40 bytes per member, kept by jsonval until the parse ends;
// WP-39 contract request for internal/jsonval), and the elements in
// flight: the one the parser is building and the chunks of at most 32
// elements completed but not yet checked, at most 2(Workers-1)+1 of them
// (see Concurrency). An element is built in full before it is checked,
// so one wide element costs its tree while in flight, some 60 times its
// bytes for a list of zeros, bounded only by MaxValues (about 120 to 200
// bytes per value: at the default limit some 8 to 13 GiB); the limits
// and these figures are recorded for the owner's decision (WP-39
// contract request for 02 sections 2.6 and 9 and 10 section 4). With
// elements that are narrow, or once a wide one is dropped, a failing
// decode of hostile content holds about twice the content, and with the
// default resource limit a hostile /config/dump stops after 20,000
// elements. Of a document member other than resources, and of a document
// that is not an object, only the kind and position are kept: an object
// or array there is scanned, not built.
//
// Decode applies no integer range rule: an integer literal above 2^53
// there is the RFC 8785 form of a double (R-64). It does not materialize
// defaults either; omitted since fields regain theirs when FromResources
// re-runs stage G (02 req 33).
//
// [Decoder.Verify] first checks that the content hashes to the expected
// digest (RZ-CFG-027 "content hashes to sha256:X, want sha256:Y", 02 req
// 31(a)) and then decodes it. Failures are errcode errors carrying
// RZ-CFG-024, RZ-CFG-027 or, for a limit, RZ-CFG-001, whose cause is a
// [*Findings] with one diag.Diagnostic per problem, located in the
// content, up to the cap and its one RZ-CFG-001; [DiagnosticsOf] extracts
// them. A caller that merges
// them into a diag.Collector of the same cap keeps one RZ-CFG-001: the
// Collector refuses the decode's own once it is full.
//
// # /config/dump
//
// [AppendDump] writes the document the admin server serves (the handler
// is injected into the admin server, R-57): the RFC 8785 serialization of
// {"content":<canonical document>,"digest":"sha256:...","lastKnownGood":
// "sha256:...","revision":"rev-..."} with the content bytes copied exactly
// and lastKnownGood omitted when there is none. It reports false, and
// writes nothing, for a Revision without a digest or whose content is not
// one JSON object between the canonical header and trailer, nested less
// deeply than jsonval.MaxDepthLimit, such as the zero Revision of a Node
// that has none.
// [Decoder.ReadDump] reads one with a size cap, verifies the content
// against its digest and keeps the received bytes, so ruralz node dump
// saves them unchanged (02 req 74).
//
// # Concurrency
//
// Encoders and Decoders are immutable after construction and safe for
// concurrent use. An Encode with Workers above 1 owns at most that many
// goroutines, never more than it has resources, feeds them chunks of
// resources, stops handing out chunks once its context ends or a chunk
// fails or panics, and waits for every goroutine before it returns. A
// Decode with Workers above 1 runs on at most that many goroutines: the
// calling goroutine parses and, each time the parser completes a chunk of
// 32 resources, hands it to the goroutines the decode owns, starting one
// more with each chunk it hands over until there are Workers-1, through a
// channel that holds at most Workers-1 chunks, or checks it itself when
// the channel is full. So at most 2(Workers-1)+1 completed chunks are in
// flight at once: Workers-1 in the channel, one in each goroutine and one
// the parser checks. The checks (the envelope rules, the newer walk and
// the re-encoding) thus run while the parser goes on, and the decode
// closes the channel and waits for every goroutine before it returns, on
// success, failure, cancellation or a panic. A panic while resources are
// checked or encoded, on a worker or on the calling goroutine, is raised
// again on the calling goroutine once every goroutine of the call has
// returned, wrapping the panic value and its stack, so the worker count
// never changes how a failure surfaces; a panic elsewhere (the parser,
// the sort of an encode, a hash) is raised as it is, on the calling
// goroutine, after the goroutines the call started were stopped and
// waited for.
//
// With Options.Now or DecoderOptions.Now set, every goroutine of a call
// reads that clock between two units of work and once every 256 units
// inside one, and calls runtime.Gosched once YieldAfter (100 µs by
// default) has passed since its last yield (01 req 53; 02 section 7,
// OQ-performance-budgets-and-benchmarking-6 (a)), so a Node's activation
// and its Last-Known-Good and handover verification share the processor
// with live traffic. The clock is read at each boundary between two
// resources written, parsed, checked, sequenced or built as envelopes,
// two chunks joined, and two pieces of 64 KiB of content hashed or
// copied; the units counted between boundaries are resources collected,
// kinds checked, comparisons of the sort, values built, walked or
// written, members judged, tokens scanned, document and dump members
// read, chunks gathered, and findings offered, compared or listed. A
// single token, the read of a /config/dump body and a call into
// schemaidx for one value are not divided, and AppendDump, which runs on
// an admin request's goroutine, takes no clock. Now is read only from the work whose panics a call
// recovers, so a panic in it surfaces as one in that work does.
// Output never depends on scheduling: results are kept per chunk in
// content order, and the findings each goroutine offers are kept in
// content order by a shared keeper that holds the first ones of each
// code, under its cap. The package does not log, reads the clock only
// through Options.Now and DecoderOptions.Now, and performs no I/O beyond
// the reader given to ReadDump.
package canonical
