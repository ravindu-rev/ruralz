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
//     takes at most MaxEvaluations subschema evaluations (see "Evaluation
//     bound");
//   - the document has at most MaxSubschemas schema positions, a position
//     counting again for every meta-schema check the library repeats on it,
//     the bytes the repeated checks cover are at most MaxSchemaBytes, and
//     the reference targets the library checks apart cost at most
//     maxCheckCopies copies of its bookkeeping (see "Meta-schema checks");
//   - every pattern and patternProperties name measures at most
//     MaxPatternInstructions instructions, what its compiled form keeps
//     and the parser's work on it, and all of them together at most
//     MaxDocumentInstructions (architecture R-77; see "Patterns");
//   - every number literal the library parses, in a numeric or integer
//     keyword (minimum, maxLength and the like) or in a const or enum
//     value, stays within MaxNumberBytes and MaxNumberExponent (see "Number
//     bounds");
//   - every entry of a type list, of required, and of the lists of
//     dependentRequired and dependencies is a string, which the
//     meta-schema requires: its check compares these lists' entries for
//     uniqueItems before it checks their kind, parsing every number with
//     math/big (see "Number bounds").
//
// Unknown keywords, x-ruralz-* included, and annotations are ignored as
// draft 2020-12 requires (07 req 76), their numbers included, with one
// exception the library makes: it keeps the draft-07 dependencies keyword
// under draft 2020-12, so an array member there is enforced like
// dependentRequired and a schema member like dependentSchemas, and a
// failure names /dependencies/<name>. The draft-07 additionalItems is
// unknown too: the library collects its subschemas, their $id and anchors,
// but compiles one only as the target of a reference, so the rules above
// apply there only then (minor review finding), while every reference and
// $schema is checked wherever it is. The library's compiler finds
// already-compiled subschemas through a linear scan of its work queue and
// compiles a schema resource with every subschema it reaches in one queue,
// so compile time grows with the square of the subschema count: 256 KiB of
// empty subschemas takes over 2 s. A document therefore holds at most
// MaxSubschemas schema positions. At that bound a compile takes about 0.3 s
// (BenchmarkCompileSchemaWorstCase); the evaluation analysis, which also
// carries the counts of the runtime model and builds the states of the
// body paths, takes at most about 0.4 s more for 4,000 members against
// recursive mixins (BenchmarkEvaluationAnalysis). The compile-time work on
// patterns is bounded by its steps, never by a count of calls (review
// findings): the exploration of a pattern's run queues is charged its
// steps against patternBudget per pattern and regexpBudget per document
// (about 3 ms and 25 ms; BenchmarkPatternCost, TestQueueBoundBudget), and
// matching member names against patterns, a schema's properties against
// its patternProperties and the names a body path's state declares against
// the patterns of its other schemas, charges a name's length times each
// pattern's work per byte against one matchBudget per document (about 34
// ms; TestCompileMatchBudget), past which every pattern is assumed to
// match. Each bound alone keeps a whole compile under about 0.7 s
// (BenchmarkCompileAnalysisWorstCase, on a shared 4-vCPU 2.8 GHz machine
// at a load of 1 to 3: 0.54 to 0.7 s for the mixins, 0.39 to 0.57 s for a
// reference ladder over 3,900 members, 0.31 to 0.42 s for 125 reference
// targets outside the keywords next to 3,800 properties, 0.29 to 0.6 s for
// 2,000 properties against 2,000 patterns, 0.24 to 0.38 s for
// MaxSubschemas positions holding patterns, 0.1 to 0.15 s for the checks
// a ladder of member maps repeats, and at most about 0.15 s for documents
// whose patterns fill MaxDocumentInstructions: the largest programs,
// repetitions of classes, alternations of thousands of literals, anchored
// alternations and repetitions, Unicode class escapes merged in a class,
// case folding, and patterns past the bound). The bounds are separate, so
// their costs add up in one document (minor review findings): 3,800
// properties against recursive mixins, eight case-folded classes of
// merged Unicode class escapes and 120 reference targets outside the
// keywords compile in about 0.95 to 1.1 s at a load of up to 1 (the
// "combined" case), and with property names of slashes, which the
// library's locations escape to twice their bytes and its quadratic scan
// of its work queue compares, in 1.1 to 1.2 s ("combined, escaped
// names"). 07 F6's 1 s is therefore not met for these combined worst
// cases (proposed: an Open question with 07 req 90's figures; a joint
// compile budget, or a bound on the bytes of the positions' locations,
// would also refuse large ordinary schemas). Such a worst-case compile
// allocates up to about 330 MB in all (the evaluation analysis reuses the
// member maps of each level for the next), its heap peaks at about 35 MB,
// the collector's
// pacing included, and the compiled Schema keeps up to about 16 MB: the
// library's compiled schemas, about 2.4 KB a position (10 MB at
// MaxSubschemas), and the patterns' programs, at most about 6 MB at
// MaxDocumentInstructions (TestPatternProgramMemory); an ordinary schema
// takes milliseconds and keeps kilobytes.
//
// A compiled Schema is immutable and safe for concurrent use; the Factory
// shares one per distinct schema within a snapshot (filter.Shared, keyed by
// the SHA-256 of the document's normalized bytes) and carries it over to
// the next snapshot through BuildEnv.Previous (07 req 77), also to the
// other Policies of that snapshot with the same document that build after
// the carried-over one. A Policy new to the snapshot has no Previous, so
// when it builds first it compiles the document again; the carried-over
// Policy then takes that Schema, so a snapshot holds one Schema per
// document whatever the build order (minor review finding). A Build whose
// context has ended returns the context's error, without a code, at its
// start and between the stages of a compile, so a canceled or superseded
// snapshot compile skips the rest (minor review finding); the library's
// own compile cannot be interrupted.
//
// # Patterns
//
// Go's regexp compiles a pattern into a program whose size grows with its
// repetitions, not with its text: the 11 bytes of [a-z]{1000} compile to a
// thousand instructions, which the compiled Schema keeps (review finding:
// seven patterns of 33 KB compiled for 25 s and kept 886 MB). The rule
// walk therefore measures every pattern before anything compiles it
// (patternsize.go), in instructions of 40 bytes: the instructions Go's
// parser estimates from the parsed syntax, every copy of a repetition
// included; the ranges of its classes, rangesPerInstruction of them as one
// instruction, and nodeInstructions for every literal and class, whose
// node of the parsed syntax the program may keep, each once, since the
// copies a repetition makes share them; programOverhead for the regexp's
// own structures; plus the parser's own work, counted lexically before the
// parse: the ranges of the table it builds for each Unicode class escape
// (\p and \P; \pL holds about 680), which it merges and sorts within a
// class, escapeRangesPerInstruction of them as one instruction, and the
// characters of the case-insensitive class ranges, where a flag turns
// folding on, which it folds one at a time (about 4 ms for
// [\x{42}-\x{1e942}] under (?i)), foldRunesPerInstruction of them as one
// instruction (proposed, in place of a separate bound on folded
// characters). A pattern whose parsing work alone exceeds its bound is
// refused unparsed, no pattern is parsed once the document's bound is
// spent, and a pattern refused after its parse, or invalid, still counts
// its parsing work, so a refused schema costs no more than an accepted one
// (review finding).
//
// The library compiles every distinct pattern once (regexpCache), with
// Go's regexp behind an empty group, (?:), which matches the empty string,
// so every pattern matches as written. It keeps the program from starting
// with the begin-text assertion of an anchored pattern, for which, under
// 1,000 instructions, Go would build its one-pass matcher: its tables
// copy every instruction and the ranges of every class, and merge at every
// alternation the first ranges of all its branches, so they grow with the
// square of an alternation (review findings: 23 anchored alternations of
// 330 classes, a 258 KB schema within every bound, kept 185 MB; 98
// patterns ^\pL{990} kept 864 MB). Anchored patterns match with the
// backtracker or the NFA simulation, as the others do, whose work the
// runtime model bounds (regexcost.go, "Work bound"); the exploration of a
// pattern's run queues charges the bounds of its classes before it
// collects them. At run time a string or member name is charged a regexp
// match, its matcher and the backtracker's job stack, only for the
// patterns that may match it (see "Working set").
//
// # Meta-schema checks
//
// Before compiling a schema position the library checks its keyword tree
// against the meta-schema, unless an earlier check covered it: the
// document's keyword tree once, then every reference target outside it
// (under a member that is not a keyword) that no earlier check covered,
// each check taking about 15 to 50 µs a subschema and a copy of the
// library's bookkeeping, an entry per position checked so far
// (metacheck.go). In the order the library meets references, a target
// checked before its ancestor was checked again with the ancestor's tree
// (review findings: a ladder of 59 references from the deepest target up
// checked a 3,900-member tree 59 times, 5.3 s and 2.2 GB; 2,040 such
// targets copied the bookkeeping 2,040 times, 1.1 s). The compile
// therefore checks those targets first, in pointer order, an ancestor
// before its descendants, through a resource whose allOf refers to them in
// that order, so a target an earlier check covered is never checked
// again. What remains is bounded at the walk, which simulates that order:
// a target whose keyword tree still holds positions an earlier check
// covered (a reference to a member map such as #/x/properties, whose
// member names are read as keywords) checks them again, and they count
// again against MaxSubschemas, their JSON bytes against MaxSchemaBytes;
// and every target checked apart counts the document's positions,
// resources and anchors against maxCheckCopies (2^19, about 75 ms;
// proposed). Each is RZ-CFG-005 at the reference to the target that
// crosses it; moving such targets under $defs removes the cost.
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
// # Evaluation bound
//
// The library validates without memoizing and takes no context (07
// section 9 item 21), so the fan-out product, which counts a $ref cycle
// once, does not bound the work: a recursive reference reached through two
// paths, such as {"oneOf":[{"items":{"$ref":"#"}},{"items":{"$ref":"#"}}]},
// doubles the work at every level of body nesting (seconds for a 40-byte
// body nested 20 deep). The compiled graph is therefore analyzed for an
// upper bound of the subschemas evaluated at one value of a body, at every
// nesting depth up to the decoder's 64: every allOf, anyOf and oneOf
// branch counts, if counts with the costlier of then and else (the library
// evaluates one of them), a $dynamicRef counts each target the dynamic
// scope may resolve it to, an object member or array item counts every
// subschema that can apply to it, by member name for properties and by
// index for prefixItems, with contains and the uniqueItems hashing for
// every item, and uniqueItems counts one evaluation at every value below
// the array, which it hashes. A member name no properties keyword declares
// is assumed to meet every patternProperties schema of the schemas
// evaluated at its object, even patterns no name can match together (an
// over-approximation, minor review finding: {"patternProperties":
// {"^[a-z]+$":{"$ref":"#"},"^[A-Z]+$":{"$ref":"#"}}} is refused as if it
// doubled at every level). The first depth at which the count for the
// document's root may exceed MaxEvaluations is the schema's nesting limit:
// a body value nested that deep is filter.ErrTooLarge (413, see "Runtime")
// unless the state of its own path (see "Working set") bounds its
// evaluations within MaxEvaluations, as for the members of a free-form
// object no subschema evaluates, so the work per value stays within the
// bound. A nesting limit below MinNesting is RZ-CFG-005 at the applicator
// or member keyword where the count crosses: the doubling shapes cross 10
// to 12 levels down. Recursive schemas whose recursion follows one path
// per level, such as
// {"oneOf":[{"type":"string"},{"type":"array","items":{"$ref":"#"}}]},
// allOf mixins recursing through different member names, or a tagged union
// written as an if/then/else chain whose branches recurse, have no nesting
// limit; a rich-text document schema whose blocks hold blocks of four
// kinds grows along a Fibonacci curve and has a nesting limit of 28, and a
// component tree of rows and columns 20 (their documents' fit is under
// "Working set").
//
// # Runtime (07 reqs 78 to 84)
//
// In onRequestBody the Filter accepts application/json and any */*+json
// media type whose only parameter is charset=utf-8, refuses an empty body,
// decodes the body strictly with internal/jsonval (one RFC 8259 value,
// valid UTF-8, depth at most 64, duplicate member names refused, numbers
// kept as their literal text), and walks the decoded body once to check
// it against the schema's limits before the library runs, which takes no
// context. Each limit is a multiple of limits.maxRequestBodyBytes (10 MiB
// when the snapshot leaves the limit unset, 07 section 9 item 24), and a
// body past one is filter.ErrTooLarge (413 RZ-RT-003) before anything is
// reserved: decoded values costing more than filter.DecodedLimitFactor
// times it (architecture R-63); a decision pass that may hold more than
// WorkingSetFactor times it besides the decoded values (see "Working
// set"); work over WorkPerByte work units per byte of it (see "Work
// bound"); a reservation, the decoded values with what their maps, arrays
// and strings hold beyond the decoder's charge and the decision pass's
// working set, over ReservationFactor times it, so that with the raw body
// the request side holds at most 13 times the limit (130 MiB at the
// default; architecture R-77); and a body value nested as deep as the
// schema's nesting limit on a path whose state does not bound it. A
// reservation the budget refuses is filter.ErrBudget (503 RZ-RT-004); the
// executor answers both under either failureMode (07 req 15). A request
// whose context has ended is cannot decide before anything is reserved.
// Schema.ValidateWithin applies the same limits, but for the reservation,
// to a value an M2 caller decoded.
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
// set fits the working-set and reservation limits, the work of both passes
// and of its walk over the error tree the work limit, and the budget the
// rest of its reservation; otherwise, or when the request ends during the
// walk, the body is refused with DetailMismatch alone, a decision (07 reqs
// 13 and 18). An internal validator error, which the location pass
// reports, is cannot decide with the class default (07 req 13).
//
// # Working set
//
// The library holds memory that grows with its evaluations rather than
// with the body: for a body that fails, its errors until Validate returns;
// its call stack; and under unevaluatedProperties or unevaluatedItems, a
// copy of the value's member names or item indexes for every in-place
// evaluation, all live together along a reference chain (a 1,000-link
// chain over 20,000 members held 1 GB). Before validating, Handle walks
// the decoded body once, without allocating and alongside the number
// bounds, and charges every value the counts of the schemas the library
// may evaluate at it: those of its own path of member names and indexes
// (states.go: the states of an automaton built at compile time, within
// maxStates, maxTransitions and stateBudget, where a declared name never
// falls to the state of the names no schema declares; past them, and
// within them, at most the largest counts the analysis allows at the
// value's depth). Of a value's keywords, only those of its JSON kind can
// fail on it, a type that lists its kind never does, an integer type never
// fails on a number whose literal has neither a fraction nor an exponent,
// a lower and an upper bound (minimum and maximum, minItems and maxItems,
// minProperties and maxProperties) never fail together when the lower is
// at most the upper, and the scan decides minLength and maxLength from the
// string's length:
//
//   - the decision pass's error tree: at every value, 104 bytes per error
//     unit (the 80-byte error and its slot in the one list that holds it,
//     while the list doubles; a unit is a keyword that can fail, and a
//     type, const, enum or format failure ends its evaluation with one)
//     and per group error of an evaluation that may collect two; at an
//     object, the evaluations that loop over its members keep one member's
//     errors each (all of them under unevaluatedProperties), and a failure
//     before the loop excludes it; every evaluation with propertyNames
//     validates every member name on its own, keeping the name's errors in
//     either pass and wrapping them in one more error, only for a name
//     whose errors the scan cannot rule out;
//   - the location pass's error tree: at every value, 192 bytes per error
//     unit plus 32 per level of depth (measured at 123 to 141 bytes per
//     failing evaluation plus 16.7 per level, and half again for group
//     errors), the names its required, dependency and type lists copy, the
//     rational a failing numeric keyword keeps, the error that wraps a
//     failing member name's under each evaluation with propertyNames, and
//     at an array 16 bytes per item for each evaluation with minContains or
//     maxContains, whose error keeps the index of every item that matches
//     contains (review finding: 100 such evaluations over 16,000 items held
//     12.7 MB charged 58 KB);
//   - along the costliest path from the root, the call stack: the library
//     evaluates the in-place members of a schema one after the other, so
//     at each value it holds one chain of in-place evaluations, never more
//     than the value's evaluations (review finding: every evaluation was
//     charged a frame, so a 14 KB rich-text body reserved 29 MB of stack),
//     8 KiB per evaluation of the longest chain the value's state allows
//     (measured with debug.SetMaxStack at 1.4 to 1.6 KiB per in-place
//     evaluation and 2.2 to 2.5 KiB per evaluation of a nesting level's
//     chain, up to three times that while the stack doubles and copies;
//     the decision pass's wrapper adds three; TestStackWithinCharge); per
//     evaluation that may copy unevaluated names a map of 256 bytes plus
//     128 per member (measured at most 256 plus 122), or for indexes 192
//     plus 96 per item (measured at most 192 plus 83); per evaluation with
//     additionalProperties false 48 bytes per member it may list; and per
//     evaluation with contains 16 bytes per item for the indexes of the
//     items that match, which it lists while it loops;
//   - once, the largest transient copy: the longest string, which the
//     library copies at every evaluation, the duplicates map of the longest
//     uniqueItems array (192 bytes per item, measured 102 to 168), or a
//     regexp match of a string or a member name: the queues and threads of
//     the largest program that may match it (64 bytes per instruction),
//     the backtracker's visited set (32 KiB), and the backtracker's job
//     stack. Go's regexp runs its backtracker for a program of at most 500
//     instructions on an input shorter than 262,144 divided by the
//     program's instruction count, in bytes, and on n bytes its stack holds
//     at most (n+1) times held jobs plus one, held being the most
//     alternations of the program reachable through empty-width steps from
//     where the search at one input position starts (regexcost.go: 3 of
//     the WHATWG e-mail pattern's 126, all 241 of (?:(?:a??){240}a)*b), at
//     48 bytes a job (16 in a slice grown by appending, up to three times
//     that while it grows), for the program holding the most that the
//     backtracker may run on n bytes: never more than the visited set's
//     2^18 pairs, about 12.6 MB. Only the patterns a string's own path
//     applies to it, and its object's patternProperties to a member name,
//     count; where the scan does not know the path, every pattern of the
//     document, wherever a pattern or a length keyword may scan a value at
//     its depth (review findings: a pattern of 241 alternations held 4.9 MB
//     for a string of 537 bytes, charged 64 KB, TestHandleBacktrackCharge;
//     then every alternation of the document's costliest pattern, charged
//     to every string a pattern or a uniqueItems pass reached, refused a
//     149-byte sign-up body at a 64 KiB limit, TestHandlePatternFit). The
//     job stack binds before the body's size only at small limits, and
//     only for the inputs short enough for the backtracker: at a 64 KiB
//     maxRequestBodyBytes, whose working set is 512 KiB, a string of about
//     9,500 to 37,400 bytes under ^[^<>]*$, a program of 7 instructions
//     holding one job per position, is refused with 413, while a longer
//     one matches with the NFA simulation and passes; from 256 KiB up that
//     pattern never binds, and from about 1.6 MiB up no pattern does.
//
// TestWorkingSetCoversPeak measures the error trees exactly, the decision
// pass's from inside it, for dense failing bodies of every error shape,
// along reference chains and for ordinary order bodies: they stayed within
// 0.87 (decision) and 0.89 (location) of the charge; the sampled peak of
// the location pass, which holds the same copies as the decision pass,
// stayed within about 0.9 to 1.1 of its whole working set, the sampler's
// error included (the test allows a quarter more). The decision pass is
// charged its worst case over the library's member order, in which every
// alternative of a union may descend into the members of a value that fails
// it before failing on its discriminator, so a valid document of unions or
// recursive blocks reserves more than it holds.
//
// A valid body reserves its decoded values and the decision pass's working
// set. The decoder charges a value its headers and bytes (internal/jsonval)
// but not the hash table of an object, a group of 8 slots up to 8 members
// and a power of two slots past them, nor the spare capacity of an array's
// backing array (review finding: a one-member object holds 336 bytes, of
// which the decoder charges 80, so valid arrays of small objects held up to
// 3.8 times the reservation); the scan adds both, and the rounding of every
// string to its allocation size class (mapExtra, arrayExtra and
// stringExtra, measured within 1% on Go 1.26 and 1.27), so the
// reservation covers the decoded tree, measured exactly
// (TestHandleReservesDecodedMaps). R-63's decoded limit still applies to
// the decoder's charge, and the reservation limit to the whole (review
// finding: arrays of one-member objects reserved up to 17.6 times the raw
// limit).
//
// The measured fit at the 10 MiB default (architecture R-77: typical
// records fit up to about 70 to 84% of the limit, dense enum, union,
// numeric and uniqueItems arrays less, and operators raise
// maxRequestBodyBytes on Routes that validate them), each the largest
// valid body of its shape that passed, as measured, with the limit that
// stopped the next (TestHandleAcceptsLargeBodies and TestHandleMeasuredFit
// pin that each passes, within a few percent of its fraction, and reserves
// what the model charges; TestHandleFitEdges the counts; and
// TestHandleReservationLimit the reservation limit):
//
//   - arrays of tags under maxLength, of strings under a pattern, and long
//     strings: the full limit;
//   - arrays of order items and of line items: 81% and 70%, their decoded
//     values (about 4.9 and 5.7 times the body) reaching theirs;
//   - a map of 450,000 integers under propertyNames: over 60%;
//   - arrays of one-member objects, which decode to 47 times their bytes:
//     about 25%, at the reservation limit (20% under a bounded integer);
//   - an array of two-letter codes under a 250-value enum: 34%, at the work
//     limit; dense arrays of one-character enum values, which may each hold
//     an error: 31%, at the working-set limit;
//   - dense numeric arrays (minor review finding): decimals under a
//     minimum and a maximum, 53%, and prices under multipleOf 0.01, 69%,
//     at the working-set limit (under a divisor of a thousand digits such
//     as 1e-1000 about 10%, at the work limit); pairs of a
//     timestamp and a value under prefixItems, 63%, at the decoded limit;
//     longitude and latitude pairs, each bounded and counted by minItems
//     and maxItems, 38%, at the working-set limit; a time series of objects
//     of two numbers under required and additionalProperties false, 52%,
//     at the reservation limit;
//   - uniqueItems arrays (minor review findings): of integer ids, 33%, of
//     short string tags, 42%, of distinct prices, 37%, and of bounded
//     three-decimal readings, 21%, all at the working-set limit (the
//     duplicates map; a decimal is charged only the numbers whose bytes
//     can hash alike, hashColliders, where every split of its bytes held
//     such arrays to 10 to 12%, at the work limit); of objects, about
//     6,500 of two string members (2.1%) or 1,700 of one number member
//     (0.19%), at the work limit, every pair being charged a comparison;
//   - arrays of events under an 8-way and a 20-way oneOf of object variants:
//     32% and 13.6%, at the working-set limit, every failing variant being
//     charged its member's errors;
//   - arrays of orders composed with allOf of a base and their own
//     properties under unevaluatedProperties false: 56%, at the
//     working-set limit (the minor review finding measured 38% for the
//     composition nested in orders and their lines);
//   - arrays of records whose required member an if, then and else chooses
//     by kind: 62%, at the reservation limit;
//   - a JSON Patch (RFC 6902), a oneOf of three kinds of operation: 38%,
//     at the working-set limit;
//   - a GeoJSON FeatureCollection of polygons, a geometry being null or one
//     of six types: 14.7%;
//   - a rich-text document of plain paragraphs: 20%; of paragraphs,
//     headings, quotes and lists two levels deep: 4.7%; a component tree's
//     row of columns: 6.2%; a component tree nested 6 deep with 3 children
//     per node (27 KB) passes and one 7 deep (80 KB) does not, at the
//     working-set limit, the states of a recursion through four or three
//     alternatives growing with the nesting.
//
// For records and arrays of small objects, the heap at Handle's peak
// stayed within about 1.25 times the reservation, the excess being the
// collector's pacing; the decoder leaves the tables a wide object's map
// outgrew, and the backing arrays an array of short strings outgrew, to the
// collector before anything is reserved, so their peaks reached about 1.8
// and 1.6 times it. For unions and recursive documents the reservation is
// 3.5 to 11 times the peak at the sizes above, and up to about 70 times
// for small deeply nested ones. Under 07 req 84 the 1 KiB order of
// BenchmarkValidate1KiB takes about 90 µs on that machine, above the 25
// µs p50 target (an Open question, architecture R-77); the library's
// validation is two thirds of it.
//
// # Work bound
//
// The library takes no context, so validation cannot be stopped once it
// starts (07 section 9 item 21), and its work grows with the values as
// well as with the evaluations: every evaluation at an object loops over
// all its members and looks their names up, hashing them whole, every
// evaluation at a string copies it and a pattern scans it, const and enum
// compare whole values and parse numbers with math/big, the unevaluated
// copies grow with the members or items and their names, uniqueItems
// hashes every value below the array and compares the items that share a
// hash, and multipleOf divides with math/big. The same walk therefore sums
// an upper bound of the work, in work units of about a nanosecond of the
// library on that machine, each term measured and rounded up by at least a
// quarter:
//
//   - per evaluation 384, per keyword that can fail 320 (type, const and
//     enum count once), per name of a list 32, and 1 per 8 bytes of the
//     required and dependency names it looks up in an object; the schema
//     side of const and enum comparisons, 12 per value, numbers at their
//     parse cost and a number nested in a const or enum value also at the
//     costliest body literal; per pattern, its setup;
//   - per member of an object, 48 per evaluation at it, and 384 per
//     evaluation that may copy unevaluated names (256 per item for
//     indexes); and per 8 bytes of its name, 1 for every time the library
//     may hash or compare it: once per evaluation with member keywords
//     (the properties lookup), five times per evaluation that may copy
//     unevaluated names, per uniqueItems pass once plus twice the bits of
//     the member count (sorting the names), and once per uniqueItems
//     comparison; and per 8 bytes of the object's longest name, 1 per
//     const or enum object compared with it, which stops at the first name
//     it lacks (review finding: none of these grew with the name, so a 10
//     MiB name under 3,200 lookups or a 4,000-link unevaluated chain was
//     hashed for 1.35 to 3.2 s charged a few milliseconds);
//   - per byte of a string, 1 per evaluation at it, 2 more per evaluation
//     with minLength or maxLength, which counts its runes, 1 per
//     uniqueItems pass that hashes it, and per pattern
//     16 plus 16 per entry of the largest run queue the pattern's NFA can
//     build (regexcost.go: an exploration of its queues over its character
//     classes, or past its budget the whole program) and 2 per entry and
//     step of the search a queue entry makes in the ranges of the
//     program's largest class (up to 15 steps for 20,000 ranges; a
//     repetition of \pL takes about 21 ns per entry and byte, of a
//     one-range class 9), so that the 261 instructions of the WHATWG
//     e-mail pattern cost 224 per byte; member names pay the same for
//     patternProperties and are values of their own under propertyNames;
//   - per number, per parse and comparison, 768 plus 28 per byte of the
//     literal and 12 per unit of its decimal exponent; and per multipleOf
//     check, which divides the value by the divisor as math/big rationals
//     (a/b by c/d: a*d and c*b, divided by their greatest common divisor),
//     512, 64 per decimal digit of the smaller of the two products (each
//     step of the greatest common divisor), 16 per digit of a, b, c and
//     d, and 1 per 128 of the digit products a*b, c*d and (a+b)*(c+d),
//     the divisor's parts counted with its schema (measured on 1,500
//     random pairs within the number bounds, 0.25 to 26 µs, each at least
//     40% under the model; review finding: charged as one parse of the
//     body literal, 806,202 short decimals against a divisor of 127 bytes
//     took 6.0 to 6.5 s, over twice the bound below);
//   - per value below a uniqueItems array, a hashing pass (384) and its
//     comparisons: up to 20 items every pair, past that the items sharing
//     its item's hash, which the library's hash allows for any array or
//     object item with any other (it keeps neither a number's sign nor the
//     bounds of an array's or object's parts) and for a decimal with the
//     few numbers whose numerator and denominator bytes concatenate alike,
//     a split of its bytes whose second part is a power of two times a
//     power of five (18.4375 and 0.0001 share 01 27 10), but never for
//     distinct strings, booleans, nulls or integers but their negation;
//   - per evaluation, the cycle check, 4 per evaluation of its chain at the
//     value, and with $dynamicRef or $recursiveRef, 4 per evaluation of the
//     chains of the whole path;
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
// the raw limit, like the decoded limit. Records, tags and long strings
// cost 50 to 230 units per byte of body, so the bound does not bind before
// their decoded values. Unions cost more, every keyword of a failing
// branch being charged: events under an 8-way and a 20-way oneOf about 490
// and 1,190 units per byte, GeoJSON features 480, rich text 380 to 1,670,
// a component tree 1,240, where the decision pass's working set binds
// first; an array of two-letter codes under a 250-value enum about 740,
// every comparison being charged, refused above 34% of the limit. It
// refuses the bodies whose work the schema amplifies, such as a 300 KB
// object or a 2 MiB string against 4,000 evaluations, a 100 KB array
// against 200 comparisons with a 20,000-element const, or 2,048 arrays
// crafted to share one hash under uniqueItems, each of which takes seconds
// or grows with the square of the array (TestHandleWorkBound,
// TestUniqueItemsComparisons); so is a valid uniqueItems array of more
// than about 1,700 to 6,500 objects (architecture R-77; "Working set").
//
// # Number bounds
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
// documents obey the same bounds in every value the library parses: the
// numeric and integer keywords, which it compiles, and const and enum,
// whose values it compares. Its meta-schema check also compares the entries
// of four lists of names for uniqueItems, the type list, required, and
// every list of dependentRequired and of dependencies, every pair of up to
// 20 entries and past that every entry once for its hash, before it checks
// that each is a string (review finding: 20 literals of exponent 999,999
// in a 241-byte type list compiled for 11 s, and 21 that math/big refuses
// made it panic). Such an entry that is not a string is refused at the
// walk, at every schema position, with the meta-schema's own message, so
// the library never compares one: a number, and arrays and objects, whose
// comparison its hash does not bound. Both bounds admit every IEEE 754
// double and integers far beyond 64 bits, so 07 req 80 ("large integers
// validate exactly") still holds.
//
// The package never logs, starts no goroutine and reads no clock. It
// builds for 64-bit platforms only, the only ones Ruralz ships (ADR-0001):
// its counts are ints that saturate at 2^40 (countCap).
package jsonschema
