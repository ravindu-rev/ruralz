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
//     schema path, with each $ref cycle counted once, is at most MaxFanOut
//     (see "Fan-out");
//   - no reference cycle applies a schema to the same value again without
//     descending into it (proposed): the library would answer every value
//     reaching it with an internal error (07 req 13);
//   - validating one value of a body nested less deep than MinNesting
//     takes at most MaxEvaluations subschema evaluations (proposed, see
//     "Evaluation bound");
//   - the document has at most MaxSubschemas schema positions (proposed);
//   - every number literal the library parses, in a numeric or integer
//     keyword (minimum, maxLength and the like) or in a const or enum
//     value, stays within MaxNumberBytes and MaxNumberExponent (see "Number
//     bounds").
//
// Unknown keywords, x-ruralz-* included, and annotations are ignored as
// draft 2020-12 requires (07 req 76), their numbers included, with one
// exception the library makes: it keeps the draft-07 dependencies keyword
// under draft 2020-12, so an array member there is enforced like
// dependentRequired and a schema member like dependentSchemas, and a
// failure names /dependencies/<name>. The library's compiler finds
// already-compiled subschemas through a linear scan of its work queue and
// compiles a schema resource with every subschema it reaches in one queue,
// so compile time grows with the square of the subschema count: 256 KiB of
// empty subschemas takes over 2 s. A document therefore holds at most
// MaxSubschemas schema positions (proposed). At that bound a compile takes
// about 0.3 s (BenchmarkCompileSchemaWorstCase); the evaluation analysis,
// which also carries the counts of the runtime model and builds the states
// of the body paths, takes at most about 0.4 s more for 4,000 members
// against recursive mixins (BenchmarkEvaluationAnalysis). The compile-time
// work on patterns is bounded by its steps, never by a count of calls
// (review findings): the exploration of a pattern's run queues charges
// every step before it takes it, against patternBudget per pattern and
// regexpBudget per document (about 3 ms and 25 ms; BenchmarkPatternCost,
// TestQueueBoundBudget), and matching member names against patterns, a
// schema's properties against its patternProperties and the names a body
// path's state declares against the patterns of its other schemas, charges
// a name's length times each pattern's work per byte against one
// matchBudget per document (about 34 ms; TestCompileMatchBudget), past
// which every pattern is assumed to match. A whole compile stays under
// about 0.6 s (BenchmarkCompileAnalysisWorstCase: 0.56 s for the mixins,
// 0.18 s for nine alternations of 3,900 literals, 0.06 s for 300 names of
// 800 bytes against 200 patterns), within 07 F6's 1 s, on a shared 4-vCPU
// 2.8 GHz machine. Such a worst-case compile allocates up to about 300 MB
// in all but holds at most about 18 MB at once, and the compiled Schema
// keeps up to about 9 MB; an ordinary schema takes milliseconds and keeps
// kilobytes.
//
// A compiled Schema is immutable and safe for concurrent use; the Factory
// shares one per distinct schema within a snapshot (filter.Shared, keyed by
// the SHA-256 of the document's normalized bytes) and carries it over to
// the next snapshot through BuildEnv.Previous (07 req 77), also to the
// other Policies of that snapshot with the same document.
//
// # Fan-out
//
// The fan-out of a schema is the largest product of the branch factors
// along a schema path from it that repeats no schema: a path that reaches
// a schema already on it ends there, which is how a $ref cycle counts once.
// It is computed exactly, over the strongly connected components of the
// compiled graph (schemas that reach each other through references) from
// the leaves up, so it does not depend on the order of members or of
// visits; past fanOutSteps steps within one component, an upper bound of
// the component decides, which can only refuse more.
//
// # Evaluation bound (proposed)
//
// The library validates without memoizing and takes no context (07
// section 9 item 21), so the fan-out product, which counts a $ref cycle
// once, does not bound the work: a recursive reference reached through two
// paths, such as {"oneOf":[{"items":{"$ref":"#"}},{"items":{"$ref":"#"}}]},
// doubles the work at every level of body nesting (seconds for a 40-byte
// body nested 20 deep). The compiled graph is therefore analyzed for an
// upper bound of the subschemas evaluated at one value of a body, at every
// nesting depth up to the decoder's 64: every allOf, anyOf and oneOf
// branch counts, if counts with the costlier of then and else (the
// library evaluates one of them), an object member or array item counts
// every subschema that can apply to it, by member name for properties and
// by index for prefixItems, and uniqueItems counts one evaluation at every
// value below the array, which it hashes. The first depth at which the
// count for the document's root may exceed MaxEvaluations is the schema's
// nesting limit: a body value nested that deep is filter.ErrTooLarge (413,
// see "Runtime") unless the state of its own path (see "Working set")
// bounds its evaluations within MaxEvaluations, as for the members of a
// free-form object no subschema evaluates, so the work per value stays
// within the bound. A nesting
// limit below MinNesting is RZ-CFG-005 at the applicator or member keyword
// where the count crosses: the doubling shapes cross 11 or 12 levels down.
// Recursive schemas whose recursion follows one path per level, such as
// {"oneOf":[{"type":"string"},{"type":"array","items":{"$ref":"#"}}]},
// allOf mixins recursing through different member names, or a tagged union
// written as an if/then/else chain whose branches recurse, have no nesting
// limit; a rich-text document schema whose blocks hold blocks of four
// kinds grows along a Fibonacci curve and has a nesting limit of 28, and a
// component tree of rows and columns 20.
//
// # Runtime (07 reqs 78 to 84)
//
// In onRequestBody the Filter accepts application/json and any */*+json
// media type whose only parameter is charset=utf-8, refuses an empty body,
// decodes the body strictly with internal/jsonval (one RFC 8259 value,
// valid UTF-8, depth at most 64, duplicate member names refused, numbers
// kept as their literal text), and walks the decoded body once to check
// it against the schema's limits before the library runs, which takes no
// context: a decoded body costing more than filter.DecodedLimitFactor
// times limits.maxRequestBodyBytes (10 MiB when the snapshot leaves the
// limit unset, 07 section 9 item 24), a body whose validation may hold
// more than WorkingSetFactor times that limit besides its decoded values
// (see "Working set") or take more than WorkPerByte work units per byte of
// it (see "Work bound"), or a body value nested as deep as the schema's
// nesting limit on a path whose state does not bound it, is
// filter.ErrTooLarge (413 RZ-RT-003), and a refused reservation
// filter.ErrBudget (503 RZ-RT-004), both through the executor (07 req 15,
// architecture R-63). A request whose context has ended is cannot decide
// before anything is reserved.
//
// Validation then runs in up to two passes. The decision pass validates
// for a boolean result, through a second resource whose not applies the
// document: the library then keeps no location of its errors and stops
// early where it can, and its working set is reserved from
// limits.maxBufferedBytes with the decoded values. A valid body ends
// there: the body, headers and framing pass unchanged. For a body that
// fails, the location pass validates again to name the failing keyword:
// every refusal is 400 RZ-RT-009 with a fixed detail or the absolute
// keyword location of the first failing leaf in pointer order, cut back to
// its longest ancestor within 256 bytes (proposed); the detail never
// quotes request content. The location pass runs only when its working
// set fits the same limit, the work of both passes and of its walk over the
// error tree the work limit, and the budget the rest of its reservation;
// otherwise, or when the request ends during the walk, the body is refused
// with DetailMismatch alone, a decision (07 reqs 13 and 18). An internal
// validator error, which the location pass reports, is cannot decide with
// the class default (07 req 13).
//
// # Working set (proposed)
//
// The library holds memory that grows with its evaluations rather than
// with the body: for a body that fails, its errors until Validate returns;
// a goroutine stack frame for every evaluation on the current path; and
// under unevaluatedProperties or unevaluatedItems, a copy of the value's
// member names or item indexes for every in-place evaluation, all live
// together along a reference chain (a 1,000-link chain over 20,000 members
// held 1 GB). Before validating, Handle walks the decoded body once,
// without allocating and alongside the number bounds, and charges every
// value the counts of the schemas the library may evaluate at it: those of
// its own path of member names and indexes (states.go: the states of an
// automaton built at compile time, within maxStates, maxTransitions and
// stateBudget, where a declared name never falls to the state of the names
// no schema declares; past them, and within them, at most the largest
// counts the analysis allows at the value's depth). Of a value's keywords,
// only those of its
// JSON kind can fail on it, a type that lists its kind never does, and the
// scan decides minLength and maxLength from the string's length:
//
//   - the decision pass's error tree: at every value, 104 bytes per error
//     unit (the 80-byte error and its slot in the one list that holds it,
//     while the list doubles; a unit is a keyword that can fail, and a
//     type, const, enum or format failure ends its evaluation with one)
//     and per group error of an evaluation that may collect two; at an
//     object, the evaluations that loop over its members keep one
//     member's errors each (all of them under unevaluatedProperties), and
//     a failure before the loop excludes it; member names under
//     propertyNames keep their errors as the location pass does;
//   - the location pass's error tree: at every value, 192 bytes per error
//     unit plus 32 per level of depth (measured at 123 to 141 bytes per
//     failing evaluation plus 16.7 per level, and half again for group
//     errors), the names its required, dependency and type lists copy, and
//     the rational a failing numeric keyword keeps;
//   - along the costliest path from the root, 4 KiB of stack per
//     evaluation (measured 1.2 to 1.3 KiB used, up to three times that while
//     the stack doubles; the decision pass's wrapper adds three), per
//     evaluation that may copy unevaluated names a map of 256 bytes plus 128
//     per member (measured at most 256 plus 122), or for indexes 192 plus 96
//     per item (measured at most 192 plus 83), and per evaluation with
//     additionalProperties false 48 bytes per member it may list;
//   - once, the largest transient copy: the longest string, which the
//     library copies at every evaluation, the duplicates map of the longest
//     uniqueItems array (192 bytes per item, measured 102 to 168), or a
//     regexp matcher.
//
// TestWorkingSetCoversPeak measures the error trees exactly, the decision
// pass's from inside it, for dense failing bodies of every error shape,
// along reference chains and for ordinary order bodies: they stayed within
// 0.22 to 0.87 (decision) and 0.10 to 0.89 (location) of the charge; the
// sampled peak of the location pass, which holds the same stack and copies
// as the decision pass, stayed within its whole working set. The decoded
// values are limited on their own, at filter.DecodedLimitFactor times the
// raw limit as R-63 states, and the decision pass's working set at
// WorkingSetFactor times it. At the 10 MiB default, valid arrays of order
// items and of line items pass up to 81% and 70% of the limit, where their
// decoded values (about 4.9 and 5.7 times the body) reach theirs; arrays of
// tags under maxLength and long strings pass at the full limit, arrays of
// strings under a pattern up to 84%, dense arrays of one-character enum
// values, which may each hold an error, up to 31%, and events under a
// 20-way oneOf of objects, whose failing branches may each hold errors, up
// to 15%.
//
// A valid body reserves its decoded values and the decision pass's working
// set. The decoder charges a value its headers and bytes (internal/jsonval)
// but not the hash table of an object, a group of 8 slots up to 8 members
// and a power of two slots past them, nor the spare capacity of an array's
// backing array (review finding: a one-member object holds 336 bytes, of
// which the decoder charges 80, so valid arrays of small objects held up to
// 3.8 times the reservation); the scan adds both, and the rounding of every
// string to its allocation size class (mapExtra, arrayExtra and
// stringExtra, measured within 1% on Go 1.26 and 1.27), so the reservation
// covers the decoded tree, measured exactly
// (TestHandleReservesDecodedMaps). R-63's decoded limit still applies to
// the decoder's charge. The heap at Handle's peak stayed within 0.6 to 1.2
// times the reservation, the excess being the collector's pacing; a wide
// object's map also leaves the tables it outgrew to the collector while the
// decoder builds it, before anything is reserved. Under 07 req 84 the 1 KiB
// order of BenchmarkValidate1KiB takes about 90 µs on that machine, above
// the 25 µs p50 target; the library's validation is two thirds of it.
//
// # Work bound (proposed)
//
// The library takes no context, so validation cannot be stopped once it
// starts (07 section 9 item 21), and its work grows with the values as
// well as with the evaluations: every evaluation at an object loops over
// all its members, every evaluation at a string copies it and a pattern
// scans it, const and enum compare whole values and parse numbers with
// math/big, the unevaluated copies grow with the members or items, and
// uniqueItems hashes every value below the array and compares the items
// that share a hash. The same walk therefore sums an upper bound of the
// work, in work units of about a nanosecond of the library on that
// machine, each term measured and rounded up by at least a quarter:
//
//   - per evaluation 384, per keyword that can fail 320 (type, const and
//     enum count once), per name of a list 32; the schema side of const and
//     enum comparisons, 12 per value, numbers at their parse cost and a
//     number nested in a const or enum value also at the costliest body
//     literal; per pattern, its setup;
//   - per member of an object, 48 per evaluation at it, and 384 per
//     evaluation that may copy unevaluated names (256 per item for
//     indexes);
//   - per byte of a string, 1 per evaluation at it, 2 more per evaluation
//     with minLength or maxLength, which counts its runes, and per pattern
//     16 plus 16 per entry of the largest run queue the pattern's NFA can
//     build
//     (regexcost.go: an exploration of its queues over its character
//     classes, or past its budget the whole program), so that the 259
//     instructions of the WHATWG e-mail pattern cost 160 per byte; member
//     names pay the same for patternProperties and are values of their own
//     under propertyNames;
//   - per number, per parse and comparison, 768 plus 28 per byte of the
//     literal and 12 per unit of its decimal exponent;
//   - per value below a uniqueItems array, a hashing pass (384) and its
//     comparisons: up to 20 items every pair, past that the items sharing
//     its item's hash, which the library's hash allows for any array or
//     object item with any other (it keeps neither a number's sign nor the
//     bounds of an array's or object's parts) and for a decimal with the
//     few numbers whose numerator and denominator bytes concatenate alike,
//     but never for distinct strings, booleans, nulls or integers but
//     their negation;
//   - per value, the cycle check (its evaluations squared, 4 each), and
//     with $dynamicRef or $recursiveRef, the walk of the whole path;
//   - apart, per error unit of the location pass, its walk over the error
//     tree for the first failing leaf: 192 plus 2 per byte of the longest
//     location and keyword an error of the document may name (walkWork;
//     review finding: the walk reads every leaf's location, so a 160 KB
//     body failing under a 250,000-byte $defs name held Handle for 23 s).
//
// A body whose work is over WorkPerByte (256) work units per byte of
// limits.maxRequestBodyBytes is filter.ErrTooLarge (413 RZ-RT-003), and
// nothing is reserved for it: at the 10 MiB default the limit is about 2.7
// x 10^9 units, so one request validates for at most about 2.7 s on that
// machine (BenchmarkWorkModel and BenchmarkPatternModel report the
// nanoseconds per unit of every term, at most 0.8 on that machine at rest;
// BenchmarkLocationWalk the walk's). The location pass runs only when both
// passes and the walk fit the limit together, and the walk stops, refusing
// with DetailMismatch alone, when the request ends. The limit grows with
// the raw limit, like the decoded limit. Ordinary documents cost 90 to 230
// units per byte of body, so the bound does not bind before their decoded
// values. A long enum or a union costs more, every comparison of an enum
// and every keyword of a union's branches at its failing cost being charged
// (minor review finding): an array of two-letter codes under a 250-value
// enum about 740 units per byte, refused above 34% of the limit (27% before
// comparisons were calibrated at 12), events under a 20-way oneOf of
// objects about 1,200, where the decision pass's working set binds first,
// at 15% (see "Working set"). It refuses the bodies whose work the schema
// amplifies, such as a 300 KB object or a 2 MiB string against 4,000
// evaluations, a 100 KB array against 200 comparisons with a 20,000-element
// const, or 2,048 arrays crafted to share one hash under uniqueItems, each
// of which takes seconds or grows with the square of the array
// (TestHandleWorkBound, TestUniqueItemsComparisons); so is a valid
// uniqueItems array of more than a few thousand objects.
//
// # Number bounds (proposed)
//
// The library compares numbers through math/big, whose decimal parser
// takes time that grows with the literal's length and quadratically with
// its exponent: one literal 1e999999 costs about 45 ms, and a body could
// pack thousands of them. Past an exponent of about 10^6 math/big refuses
// the literal and the library dereferences the nil result and panics.
// A body number the library may parse, under a numeric keyword (minimum,
// maximum, exclusiveMinimum, exclusiveMaximum, multipleOf), the integer
// type, a const or enum number, a const or enum value holding arrays or
// objects above it, or a uniqueItems array above it, that is longer than
// MaxNumberBytes or has a decimal exponent beyond ±MaxNumberExponent, is
// refused with 400 RZ-RT-009 and the detail DetailNumber, wherever it is
// in the body; where the scan does not know a value's path (past the
// states' bounds or the nesting limit), every number is checked when the
// schema compares numbers anywhere (Schema.ComparesNumbers). Schema
// documents obey the same bounds in every value the library parses. Both
// bounds admit every IEEE 754 double and integers far beyond 64 bits, so
// 07 req 80 ("large integers validate exactly") still holds.
//
// The package never logs, starts no goroutine and reads no clock.
package jsonschema
