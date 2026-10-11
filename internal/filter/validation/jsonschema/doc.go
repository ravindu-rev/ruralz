// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package jsonschema is the validation.json-schema Policy type: it checks
// JSON request bodies in onRequestBody against the inline JSON Schema of
// the Policy's config.schema and answers 400 RZ-RT-009 when a body is not
// acceptable (spec 07 groups J and K). It also exports the offline check
// of that schema (Check, a hub.PolicyCheck) that every binary runs at stage
// H, and CompileSchema with Schema.Raw for the M2 OpenAPI import and export.
//
// # Compile (07 reqs 74 to 77)
//
// CompileSchema compiles a schema document with
// github.com/santhosh-tekuri/jsonschema/v6 under these rules, each a
// RZ-CFG-005 finding at a JSON Pointer into the document when broken:
//
//   - draft 2020-12 is the default, and every $schema the document's schema
//     objects carry is exactly Draft;
//   - a URL loader refuses every URL, so $ref and $dynamicRef resolve only
//     to fragments of the document and to its embedded $id resources: no
//     file, network or bundled meta-schema is ever reached;
//   - format and the content keywords are annotations (no AssertFormat, no
//     AssertContent), and every regular expression is a Go regexp (RE2), so
//     ECMA-262-only syntax such as look-around is refused;
//   - the canonical (RFC 8785) form is at most MaxSchemaBytes;
//   - the nested applicator fan-out, the product of the allOf, anyOf and
//     oneOf branch counts and of the if, then and else members along any
//     schema path, with each $ref cycle counted once, is at most MaxFanOut;
//   - no reference cycle applies a schema to the same value again without
//     descending into it (proposed): the library would answer every value
//     reaching it with an internal error (07 req 13);
//   - validating one value of a body takes at most MaxEvaluations
//     subschema evaluations at any nesting depth up to 64 (proposed, see
//     "Evaluation bound");
//   - the document has at most MaxSubschemas schema positions (proposed);
//   - every number literal in the document stays within MaxNumberBytes and
//     MaxNumberExponent (see "Number bounds").
//
// Unknown keywords, x-ruralz-* included, are ignored as draft 2020-12
// requires (07 req 76), with one exception the library makes: it keeps
// the draft-07 dependencies keyword under draft 2020-12, so an array
// member there is enforced like dependentRequired and a schema member like
// dependentSchemas, and a failure names /dependencies/<name>. The
// library's compiler finds already-compiled subschemas through a linear
// scan of its work queue and compiles a schema resource with every
// subschema it reaches in one queue, so compile time grows with the square
// of the subschema count: 256 KiB of empty subschemas takes over 2 s. A
// document therefore holds at most MaxSubschemas schema positions
// (proposed), which bounds the library's worst case to about 0.2 s
// (BenchmarkCompileSchemaWorstCase). The evaluation analysis, which also
// carries the counts of the runtime model and bounds the run queue of
// every pattern (at most about 7 ms per pattern and 60 ms per document,
// BenchmarkPatternCost), adds at most about 0.3 s more, for 4,000 members
// against recursive mixins (BenchmarkEvaluationAnalysis), so a whole
// compile stays under about 0.45 s (BenchmarkCompileAnalysisWorstCase;
// about 2.2 s under -race), within 07 F6's 1 s, on a shared 4-vCPU 2.8 GHz
// machine.
//
// A compiled Schema is immutable and safe for concurrent use; the Factory
// shares one per distinct schema within a snapshot (filter.Shared, keyed by
// the SHA-256 of the document's normalized bytes) and carries it over to
// the next snapshot through BuildEnv.Previous (07 req 77).
//
// # Evaluation bound (proposed)
//
// The library validates without memoizing and takes no context (07
// section 9 item 21), so the fan-out product, which counts a $ref cycle
// once, does not bound the work: a recursive reference reached through two
// paths, such as {"oneOf":[{"items":{"$ref":"#"}},{"items":{"$ref":"#"}}]},
// doubles the work at every level of body nesting (seconds for a 40-byte
// body nested 20 deep). The compiled graph is therefore checked for an
// upper bound of the subschemas evaluated at one value of a body, at every
// nesting depth up to the decoder's 64: every allOf, anyOf and oneOf
// branch counts, if counts with the costlier of then and else (the
// library evaluates one of them), an object member or array item counts
// every subschema that can apply to it, by member name for properties and
// by index for prefixItems, and uniqueItems counts one evaluation at every
// value below the array, which it hashes. A count over MaxEvaluations is
// RZ-CFG-005 at the applicator or member keyword where it crosses.
// Recursive schemas whose recursion follows one path per level, such as
// {"oneOf":[{"type":"string"},{"type":"array","items":{"$ref":"#"}}]},
// allOf mixins recursing through different member names, or a tagged union
// written as an if/then/else chain whose branches recurse, stay well
// within it; if and then both recursing, as in
// {"if":{"items":{"$ref":"#"}},"then":{"items":{"$ref":"#"}}}, double the
// work per level and are refused.
//
// # Runtime (07 reqs 78 to 84)
//
// In onRequestBody the Filter accepts application/json and any */*+json
// media type whose only parameter is charset=utf-8, refuses an empty body,
// decodes the body strictly with internal/jsonval (one RFC 8259 value,
// valid UTF-8, depth at most 64, duplicate member names refused, numbers
// kept as their literal text), reserves the decoded values from
// limits.maxBufferedBytes along with the memory validation may hold (see
// "Working set"), checks the work validation may take (see "Work bound")
// and validates them. Every refusal is 400 RZ-RT-009 with a fixed detail
// or the absolute keyword location of the first failing leaf in pointer
// order, cut back to its longest ancestor within 256 bytes (proposed); the
// detail never quotes request content. A decoded body costing, with its
// working set, more than filter.DecodedLimitFactor times
// limits.maxRequestBodyBytes (10 MiB when the snapshot leaves the limit
// unset, 07 section 9 item 24), or whose validation may take more than
// WorkPerByte work units per byte of that limit, is filter.ErrTooLarge
// (413 RZ-RT-003) and a refused reservation filter.ErrBudget (503
// RZ-RT-004), both through the executor (07 req 15, architecture R-63).
// An internal validator error is cannot decide with the class default (07
// req 13). On success the body, headers and framing pass unchanged.
//
// # Working set (proposed)
//
// The library holds memory that grows with its evaluations rather than
// with the body: for a body that fails, an error for every failing keyword
// of every failing evaluation until Validate returns; a goroutine stack
// frame for every evaluation on the current path; and under
// unevaluatedProperties or unevaluatedItems, a copy of the value's member
// names or item indexes for every in-place evaluation, all live together
// along a reference chain (a 1,000-link chain over 20,000 members held
// 1 GB). Before validating, Handle walks the decoded body once, without
// allocating and alongside the number bounds, and reserves with the
// decoded values an upper bound of that memory, from the counts the
// compile analysis allows at each depth:
//
//   - at every value, its error units at 192 bytes plus 32 per level of
//     depth (measured at 123 to 141 bytes per failing evaluation plus 16.7
//     per level, and half again for group errors; a unit is a keyword that
//     can fail, or 6 names of a required, dependency or type list), and
//     the rational a failing numeric keyword keeps;
//   - along the costliest path from the root, 4 KiB of stack per
//     evaluation (measured 1.2 to 1.3 KiB used, up to three times that while
//     the stack doubles), and per evaluation that may copy unevaluated
//     names a map of 256 bytes plus 128 per member (measured at most 256
//     plus 122), or for indexes 192 plus 96 per item (measured at most 192
//     plus 83);
//   - once, the largest transient copy: the longest string, which the
//     library copies at every evaluation, the duplicates map of the longest
//     uniqueItems array (192 bytes per item, measured 102 to 168), or a
//     regexp matcher.
//
// Work is never reserved (see "Work bound"). TestWorkingSetCoversPeak
// samples the heap in use while the library validates dense failing bodies
// of every error shape and the unevaluated copies along reference chains:
// its peak stayed within 0.13 to 0.97 of the reservation. The decoded
// values and the working set together must fit in
// filter.DecodedLimitFactor times the raw limit, so a body that could fail
// everywhere must be small: an array of the order items of
// testdata/schemas/order.json costs about 59 times its raw size, so at the
// default limits such arrays above about 690 KiB are 413, and an array of
// short values under a 600-value enum above about 1.1 MiB, while a 7 MiB
// string under a pattern fits (TestHandleAcceptsLargeBodies). Under 07 req
// 84 the 1 KiB order of BenchmarkValidate1KiB takes about 70 µs on that
// machine, above the 25 µs p50 target; the library's validation is most of
// it.
//
// # Work bound (proposed)
//
// The library takes no context, so validation cannot be stopped once it
// starts (07 section 9 item 21), and its work grows with the values as
// well as with the evaluations: every evaluation at an object loops over
// all its members, every evaluation at a string copies it and a pattern
// scans it, const and enum compare whole values and parse numbers with
// math/big, the unevaluated copies grow with the members or items, and
// uniqueItems hashes every value below the array or, up to 20 items,
// compares them pairwise. The same walk therefore sums an upper bound of
// the work, in work units of about a nanosecond of the library on that
// machine, each term measured and rounded up by at least a quarter:
//
//   - per evaluation 384, per keyword that can fail 320 (type, const and
//     enum count once), per name of a list 32; the schema side of const and
//     enum comparisons, 16 per value, numbers at their parse cost and a
//     number nested in a const or enum value also at the costliest body
//     literal; per pattern, its setup;
//   - per member of an object, 48 per evaluation at it, and 384 per
//     evaluation that may copy unevaluated names (256 per item for
//     indexes);
//   - per byte of a string, 1 per evaluation at it, and per pattern 16 plus
//     16 per entry of the largest run queue the pattern's NFA can build
//     (regexcost.go: an exploration of its queues over its character
//     classes, or past its budget the whole program), so that the 259
//     instructions of the WHATWG e-mail pattern cost 160 per byte; member
//     names pay the same for patternProperties and are values of their own
//     under propertyNames;
//   - per number, per parse and comparison, 768 plus 28 per byte of the
//     literal and 12 per unit of its decimal exponent;
//   - per value below a uniqueItems array, a hashing pass (384) and its
//     comparisons, 19 of them under a pairwise check;
//   - per value, the cycle check (its evaluations squared, 4 each), and
//     with $dynamicRef or $recursiveRef, the walk of the whole path.
//
// A body whose work is over WorkPerByte (256) work units per byte of
// limits.maxRequestBodyBytes is filter.ErrTooLarge (413 RZ-RT-003), and
// nothing is reserved for it: at the 10 MiB default the limit is about 2.7
// x 10^9 units, so one request validates for at most about 2.7 s on that
// machine (TestWorkCoversTime measured 0.02 to 0.67 of the estimate across
// every term, and TestPatternCostCoversMatch checks patterns on
// adversarial inputs). The limit grows with the raw limit, like the
// decoded limit. For ordinary documents it never binds before the working
// set: their estimate, 50 to 300 units per byte of body, stays far below
// it at every size the working set admits, and a 7 MiB string under a
// pattern uses an eighth of it. It refuses the bodies whose work the
// schema amplifies, such as a 300 KB object or a 2 MiB string against
// 4,000 evaluations, or a 100 KB array against 200 comparisons with a
// 20,000-element const, each of which takes seconds (TestHandleWorkBound).
//
// # Number bounds (proposed)
//
// The library compares numbers through math/big, whose decimal parser
// takes time that grows with the literal's length and quadratically with
// its exponent: one literal 1e999999 costs about 45 ms, and a body could
// pack thousands of them. Past an exponent of about 10^6 math/big refuses
// the literal and the library dereferences the nil result and panics.
// When the compiled schema compares numbers anywhere (minimum, maximum,
// exclusiveMinimum, exclusiveMaximum, multipleOf, const, enum, uniqueItems
// or the integer type), a body number longer than MaxNumberBytes or with a
// decimal exponent beyond ±MaxNumberExponent is refused with 400
// RZ-RT-009 and the detail DetailNumber, wherever it is in the body, even
// in a member the schema never compares; schema documents obey the same
// bounds everywhere. Both bounds admit every IEEE 754 double and integers
// far beyond 64 bits, so 07 req 80 ("large integers validate exactly")
// still holds.
//
// The package never logs, starts no goroutine and reads no clock.
package jsonschema
