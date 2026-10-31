// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package jsonschema is the validation.json-schema Policy type: it checks
// JSON request bodies in onRequestBody against the inline JSON Schema of the
// Policy's config.schema and answers 400 RZ-RT-009 when a body is not
// acceptable. It also exports the offline check of that schema (Check, a
// hub.PolicyCheck) that every binary runs at stage H, and CompileSchema with
// Schema.Raw for the M2 OpenAPI import and export.
//
// # Compile
//
// CompileSchema compiles a schema document with
// github.com/santhosh-tekuri/jsonschema/v6 under these rules, each a
// RZ-CFG-005 finding at a JSON Pointer into the document when broken:
//
//   - draft 2020-12 is the default, and every $schema the document's schema
//     objects carry is exactly Draft: the walk checks it at every position
//     the library collects, whatever route reaches it, and the compile, a
//     backstop, at every position the library read one the walk did not
//     (see "Compiled set");
//   - a URL loader refuses every URL, so $ref, $dynamicRef and
//     $recursiveRef resolve only to fragments of the document and to its
//     embedded $id resources, and no file or network is ever reached: the
//     walk resolves every reference the library compiles as the library
//     will, against the same base URI and with the resources and anchors
//     the library has collected by then, and refuses, at the reference,
//     every one that leaves the document or does not resolve, before the
//     library compiles, so the loader is never asked and no bundled
//     meta-schema compiled (see "Compiled set");
//   - format and the content keywords are annotations: the compile sets
//     neither AssertFormat nor AssertContent, and with every $schema the
//     library reads exactly Draft, whose vocabularies lack format-assertion,
//     the library asserts neither in any schema it compiles; the compile
//     also refuses, at the keyword, any compiled schema in which it did (a
//     backstop, see "Compiled set");
//   - every regular expression is a Go regexp (RE2), so ECMA-262-only
//     syntax such as look-around is refused;
//   - the canonical (RFC 8785) form is at most MaxSchemaBytes;
//   - the nested applicator fan-out, the product of the allOf, anyOf and
//     oneOf branch counts and of the if, then and else members along any
//     schema path, with each $ref cycle counted once, is at most MaxFanOut
//     (see "Fan-out");
//   - no reference cycle applies a schema to the same value again without
//     descending into it (proposed): the library would answer every value
//     reaching it with an internal error;
//   - validating one value of a body nested less deep than MinNesting
//     takes at most MaxEvaluations subschema evaluations (see "Evaluation
//     bound");
//   - the document has at most MaxSubschemas schema positions, a position
//     counting again for every meta-schema check the library repeats on it,
//     the bytes the repeated checks cover are at most MaxSchemaBytes, and
//     the reference targets the library checks apart cost at most
//     maxCheckCopies copies of its bookkeeping (see "Meta-schema checks");
//     and the passes after the first of the walk's model of what the
//     library collects and compiles spend at most the walk-time compile
//     budget, compileBudget (see "Compiled set");
//   - every pattern and patternProperties name measures at most
//     MaxPatternInstructions instructions, what its compiled form keeps
//     and the parser's work on it, and all of them together at most
//     MaxDocumentInstructions (see "Patterns");
//   - every number literal the library parses, in a numeric or integer
//     keyword (minimum, maxLength and the like) or in a const or enum
//     value, stays within MaxNumberBytes and MaxNumberExponent (see "Number
//     bounds");
//   - every entry of a type list, of required, and of the lists of
//     dependentRequired and dependencies is a string, which the
//     meta-schema requires: its check compares these lists' entries for
//     uniqueItems before it checks their kind, parsing every number with
//     math/big (see "Number bounds");
//   - every schema the library compiled lies at a position the walk walked
//     live, so every rule above applied to it: the walk walks live exactly
//     the positions the library compiles, and the compile refuses any
//     other at its pointer, and a compile that panics with one finding at
//     the document, so no binary ends at stage H (backstops, see "Compiled
//     set").
//
// Unknown keywords, x-ruralz-* included, and annotations are ignored as
// draft 2020-12 requires, their numbers included, with two exceptions the
// library makes. It keeps the draft-07 dependencies keyword under draft
// 2020-12, so an array member there is enforced like dependentRequired and a
// schema member like dependentSchemas, and a failure names
// /dependencies/<name>. And it keeps the draft 2019-09 $recursiveRef,
// compiling and enforcing it as a static reference, as $ref
// (objcompiler.go:56-59, 269), a failure naming its target's keyword
// (/x/type under {"$recursiveRef":"#/x","x":{"type":"string"}}), so the walk
// follows and checks it as it does $ref (otherwise every rule here would be
// bypassed below a $recursiveRef target). A $recursiveAnchor of true
// would make the library resolve it dynamically, but the draft 2020-12
// meta-schema types $recursiveAnchor as an anchor name, so a boolean one is
// refused wherever the library compiles it, and the compile refuses any
// compiled schema that has one (a backstop): the static target is the
// only one. The draft-07 additionalItems is unknown too: the library
// collects its subschemas, their $id and anchors, but compiles one only
// when a reference it compiles names it, by JSON Pointer, anchor or $id,
// when it is the root of the resource of a schema it compiles, or as a
// $dynamicAnchor schema of a resource root it compiles, so the rules above
// apply there only then; as the meta-schema does not define it, the library
// never checks such a subschema against the meta-schema, so the compile
// does, before the library compiles it (without that check a multipleOf of 0
// would compile there, and every number reaching it would make the library
// panic). Every $schema is checked wherever the library may read it,
// and every reference wherever the library compiles it. The library's
// compiler finds already-compiled subschemas through a linear scan of
// its work queue and compiles a schema resource with every subschema it
// reaches in one queue, so compile time grows with the square of the
// subschema count: 256 KiB of empty subschemas takes over 2 s. A document
// therefore holds at most MaxSubschemas schema positions. At that bound
// a compile takes about 0.3 s (BenchmarkCompileSchemaWorstCase); the
// evaluation analysis, which also carries the counts of the runtime model
// and builds the states of the body paths, takes at most about 0.4 s
// more for 4,000 members against recursive mixins
// (BenchmarkEvaluationAnalysis). The compile-time work on patterns is
// bounded by its steps, never by a count of calls: the exploration of a
// pattern's run queues is charged its steps against patternBudget per
// pattern and regexpBudget per document (about 3 ms and 25 ms;
// BenchmarkPatternCost, TestQueueBoundBudget), and matching member names
// against patterns, a schema's properties against its patternProperties and
// the names a body path's state declares against the patterns of its other
// schemas, charges a name's length times each pattern's work per byte
// against one matchBudget per document (about 34 ms;
// TestCompileMatchBudget), past which every pattern is assumed to match.
// Each bound alone keeps a whole compile under about 0.7 s
// (BenchmarkCompileAnalysisWorstCase, on a shared 4-vCPU 2.8 GHz machine at
// a load of 1 to 3: 0.54 to 0.7 s for the mixins, 0.39 to 0.57 s for a
// reference ladder over 3,900 members, 0.31 to 0.42 s for 125 reference
// targets outside the keywords next to 3,800 properties, 0.29 to 0.6 s for
// 2,000 properties against 2,000 patterns, 0.24 to 0.38 s for MaxSubschemas
// positions holding patterns, 0.1 to 0.15 s for the checks a ladder of
// member maps repeats, 0.14 to 0.17 s for the states of 500 members that
// each apply a thousand patterns or kinds of length (merged one schema at a
// time after a sort or a scan of what a state holds, they would take 2.9 s
// and 1 s), and at most about 0.15 s for documents whose patterns fill
// MaxDocumentInstructions: the largest programs, repetitions of classes,
// alternations of thousands of literals, anchored alternations and
// repetitions, Unicode class escapes merged in a class, case folding, and
// patterns past the bound). The bounds are separate, so their costs add up
// in one document: 3,800 properties against recursive mixins, eight
// case-folded classes of merged Unicode class escapes and 120 reference
// targets outside the keywords compile in about 0.95 to 1.1 s at a load of
// up to 1 (the "combined" case), and with property names of slashes, which
// the library's locations escape to twice their bytes and its quadratic scan
// of its work queue compares, in 1.1 to 1.2 s ("combined, escaped names").
// The 1 s a compile is allowed is therefore not met for these combined worst
// cases (proposed: an Open question with these figures; a joint compile
// budget, or a bound on the bytes of the positions' locations, would also
// refuse large ordinary schemas). Such a worst-case compile allocates up to
// about 330 MB in all (the evaluation analysis reuses the member maps of
// each level for the next), its heap peaks at about 35 MB, the collector's
// pacing included, and the compiled Schema keeps up to about 16 MB: the
// library's compiled schemas, about 2.4 KB a position (10 MB at
// MaxSubschemas), and the patterns' programs, at most about 6 MB at
// MaxDocumentInstructions (TestPatternProgramMemory), the states sharing
// each distinct list of the programs that may match a string; an ordinary
// schema takes milliseconds and keeps kilobytes.
//
// A compiled Schema is immutable and safe for concurrent use, and validating
// with it never compiles a regular expression: the regexp engine the compile
// gives the library is frozen when the compile ends, its cache of compiled
// expressions dropped, so a later call fails and caches nothing, and no
// schema the library compiled asserts format, the one way the library keeps
// the engine in a compiled schema (format 'regex'; see "Compiled set"). The
// Factory shares one per distinct schema within a snapshot (filter.Shared,
// keyed by the SHA-256 of the document's normalized bytes) and carries it
// over to the next snapshot through BuildEnv.Previous, also to the other
// Policies of that snapshot with the same document that build after the
// carried-over one. A Policy new to the snapshot has no Previous, so when it
// builds first it compiles the document again; the carried-over Policy then
// takes that Schema, so a snapshot holds one Schema per document whatever
// the build order. A Build whose context has ended returns the context's
// error, without a code, at its start and between the stages of a compile,
// so a canceled or superseded snapshot compile skips the rest; the library's
// own compile cannot be interrupted.
//
// # Compiled set
//
// What the library compiles is not what the keywords show. It collects the
// document's keyword tree, every position the subschema keywords reach,
// below additionalItems too, with the base URI of each, the resources an $id
// starts there and the anchors of each resource; then, as the compile checks
// them apart in pointer order (see "Meta-schema checks"), every reference
// target outside what it collected, under the resource of the longest
// pointer prefix of the target collected by then, with the target's own
// keyword tree (roots.go:70-235, root.go:20-32). It compiles what the
// compile asks for and what that reaches: the subschemas of the keywords it
// compiles (not below additionalItems), the target of every reference it
// compiles, with every schema the root of its resource, and with every
// resource root, eagerly, the schema of each $dynamicAnchor the resource
// holds, which validation reaches through the dynamic scope alone
// (compiler.go:190-262, validator.go:781-813). A reference resolves against
// the resource of the longest pointer prefix of its own schema, to the
// resources and anchors collected when it compiles: a target checked apart
// compiles right after its own check, before the targets after it are
// collected, and everything else after all of them (objcompiler.go:361-384).
// The walk models exactly this (walk.go), so it walks live exactly the
// positions the library compiles, applies every rule at each, and refuses at
// the reference every reference that the library would load or fail to
// resolve, with the library's base and at the library's time. A walk that
// based a JSON Pointer target on the reference's own resource, recorded the
// $id and anchors of targets that only references the library never compiles
// reach, ignored when the library collects each target, or made a whole
// additionalItems subtree live for any identifier in it would let the
// library compile positions no rule covered, with references resolved
// elsewhere (a type list of 21 literals 1e9999999 would make the meta-schema
// check panic, the loader would be asked, and a multipleOf of 0 would
// compile and panic at every number), and would refuse, or accept, what the
// library does not.
//
// The walk computes the model to a fixpoint: each pass collects the keyword
// tree, then the targets found so far in pointer order, and closes what the
// library compiles from them; a target a pass finds anew it collects at once
// and the next pass in its place. An ordinary document takes one pass, or two
// with targets outside the keywords. A target an earlier pass found stays among
// the targets checked apart, so the compile compiles it, even when a later pass
// no longer reaches it, which only a target whose base an enclosing target
// found later changes can cause. A pass works on integer tables (model.go):
// every position is a node whose pointer is built once, the pointers a trie of
// their tokens, and what resolving a reference computes from the document alone
// is kept per base and reference, so a pass costs the same whatever the length
// of its pointers and URLs, which only the first pass to meet one builds or
// hashes (hashing every pointer prefix and building every target's pointer
// again in each pass would take 30 s and 31 GB for 228 passes under a
// 200,000-byte member name). A document can make every pass find one more such
// target, so what the passes after the first do, each step and each byte of a
// pointer, URL or string they meet anew, is a count product charged to the
// walk-time compile budget (compileBudget; proposed as this term's unit, value
// and pointer), RZ-CFG-005 at the reference whose new target made the walk take
// the pass that spent past it: 229 generations of such targets fit (generations
// in follow_test.go, about 0.88 of the budget), 400 do not, and neither do 3
// under a member name of 200,000 bytes, whose later passes meet eight pointers
// of that length anew. An ordinary document charges nothing, or, with targets
// outside the keywords, the second pass's steps, which repeat the first
// whatever the length of its pointers: about 61,000 units at MaxSubschemas
// positions holding three references each. Placing the targets a pass found
// among those found before charges nothing: each target is placed once in the
// whole walk, so placing them is bounded by the targets, not by the passes
// (charging both tokens of every comparison, ordinary documents near
// MaxSchemaBytes with hundreds of long-named targets outside the keywords,
// which the compile accepts, would spend up to 1.47 million units and be
// refused). The library alone refuses the generations too
// (TestFollowFixpointCost): compiling from the root alone, it bases each
// generation's target on the resource enclosing it before it collects the
// target that encloses it, and finds no pointer there. The order is the
// compile's, not the one the library alone would take: compiling from the root
// alone, it collects a target when it first meets a reference to it, in an
// order the map order of the members can change, so it may base a target before
// or after the target enclosing it, and fail to resolve an $id a later target
// holds; the compile's pointer order collects the enclosing target first, and
// every target before anything else compiles (TestAncestorTargetBase,
// TestReferenceVisibility).
//
// The library also reads a $schema at every position it collects and gives
// the resource there, and the resources inside it that have none, the
// dialect it names (roots.go:70-191). The walk applies its $schema rule at
// every position it collects, the library's own, and the compile backs it
// up: it lists every schema the library compiled from the document,
// reading the compiler's table of compiled schemas through reflect (read
// only; the library exports no list, the module is pinned,
// TestCompiledSchemas pins the table, and a compile whose table cannot be
// read is refused), applies the walk's $schema rule to the keyword tree of
// every listed position the walk did not collect, with the walk's finding
// at the $schema, inspects every listed schema, the $dynamicAnchor schemas
// included, and analyzes it as a start (compiled.go). A $recursiveRef
// target the walk did not follow, or a reference the library based on a
// resource the walk did not, would reach positions the walk never walked: a
// $dynamicAnchor schema there with format email would refuse the body
// {"a":"not an email"} with 400, a dialect of core alone would accept
// {"a":1} under properties {"a":false}, and a minimum in such an anchor
// schema would leave the number bounds off, so the body {"a":1e1000001}
// would make the library panic. After the inspection the compile refuses
// every listed schema at a position the walk did not walk live, at the
// top-most of them, before the analysis meets it, since the walk's other
// rules apply there only; and a panic during the compile is recovered into
// one finding at the document, the work done before it spent. The walk
// walks live every position the library compiles, so the list adds nothing
// and neither backstop fires.
//
// # Patterns
//
// Go's regexp compiles a pattern into a program whose size grows with its
// repetitions, not with its text: the 11 bytes of [a-z]{1000} compile to a
// thousand instructions, which the compiled Schema keeps (seven patterns
// of 33 KB compile for 25 s and keep 886 MB). The rule walk therefore
// measures every pattern before anything compiles it (patternsize.go), in
// instructions of 40 bytes: the instructions Go's parser estimates from
// the parsed syntax, every copy of a repetition included; the ranges
// of its classes, rangesPerInstruction of them as one instruction,
// and nodeInstructions for every literal and class, whose node of the
// parsed syntax the program may keep, each once, since the copies a
// repetition makes share them; programOverhead for the regexp's own
// structures; plus the parser's own work, counted lexically before the
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
// its parsing work, so a refused schema costs no more than an accepted
// one.
//
// The library compiles every distinct pattern once per compile
// (regexpEngine), with Go's regexp behind an empty group, (?:), which
// matches the empty string, so every pattern matches as written. It keeps
// the program from starting with the begin-text assertion of an anchored
// pattern, for which, under 1,000 instructions, Go would build its one-pass
// matcher: its tables copy every instruction and the ranges of every class,
// and merge at every alternation the first ranges of all its branches, so
// they grow with the square of an alternation (23 anchored alternations of
// 330 classes, a 258 KB schema within every bound, would keep 185 MB,
// and 98 patterns ^\pL{990} 864 MB). Anchored patterns match with the
// backtracker or the NFA simulation, as the others do, whose work the
// runtime model bounds (regexcost.go, "Work bound"); the exploration of a
// pattern's run queues charges the bounds of its classes before it collects
// them. At run time a string or member name is charged a regexp match, its
// matcher and the backtracker's job stack, only for the patterns that may
// match it (see "Working set").
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
// checked before its ancestor is checked again with the ancestor's tree (a
// ladder of 59 references from the deepest target up checks a 3,900-member
// tree 59 times, 5.3 s and 2.2 GB; 2,040 such targets copy the bookkeeping
// 2,040 times, 1.1 s). The compile therefore checks those targets first, in
// pointer order, an ancestor before its descendants, through a resource
// whose allOf refers to them in that order, so a target an earlier check
// covered is never checked again. Each such target compiles right after its
// own check, so its own references resolve only to what is collected up to
// it: one naming an $id or anchor that only a later target holds is refused
// at the reference, where the library would ask the loader or fail. What
// remains is bounded at the walk, which simulates that order: a target
// whose keyword tree still holds positions an earlier check covered (a
// reference to a member map such as #/x/properties, whose member names are
// read as keywords) checks them again, and they count again against
// MaxSubschemas, their JSON bytes against MaxSchemaBytes; and every target
// checked apart counts the document's positions, resources and anchors
// against maxCheckCopies (2^19, about 75 ms; proposed). Each is RZ-CFG-005
// at the reference to the target that crosses it; moving such targets under
// $defs removes the cost.
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
// The library validates without memoizing and takes no context, so the
// fan-out product, which counts a $ref cycle once, does not bound the work:
// a recursive reference reached through two paths, such as
// {"oneOf":[{"items":{"$ref":"#"}},{"items":{"$ref":"#"}}]}, doubles the
// work at every level of body nesting (seconds for a 40-byte body nested 20
// deep). The compiled graph, every schema the library compiled (see
// "Compiled set"), is therefore analyzed for an upper bound of the
// subschemas evaluated at one value of a body, at every nesting depth up to
// the decoder's 64: every allOf, anyOf and oneOf branch counts, if counts
// with the costlier of then and else (the library evaluates one of them), a
// $dynamicRef counts each target the dynamic scope may resolve it to, an
// object member or array item counts every subschema that can apply to it,
// by member name for properties and by index for prefixItems, with contains
// and the uniqueItems hashing for every item, and uniqueItems counts one
// evaluation at every value below the array, which it hashes. A member name
// no properties keyword declares is assumed to meet every patternProperties
// schema of the schemas evaluated at its object, even patterns no name can
// match together (an over-approximation: {"patternProperties":
// {"^[a-z]+$":{"$ref":"#"},"^[A-Z]+$":{"$ref":"#"}}} is refused as if it
// doubled at every level). The first depth at which the count for the
// document's root may exceed MaxEvaluations is the schema's nesting limit:
// a body value nested that deep is filter.ErrTooLarge (413, see "Runtime")
// unless the state of its own path (see "Working set") bounds its
// evaluations within MaxEvaluations, as for the members of a free-form
// object no subschema evaluates, so the work per value stays within the
// bound. A nesting limit below MinNesting is RZ-CFG-005 at the applicator
// or member keyword where the count crosses: the doubling shapes cross 10
// to 12 levels down. Recursive schemas whose recursion follows one path per
// level, such as
// {"oneOf":[{"type":"string"},{"type":"array","items":{"$ref":"#"}}]},
// allOf mixins recursing through different member names, or a tagged union
// written as an if/then/else chain whose branches recurse, have no nesting
// limit; a rich-text document schema whose blocks hold blocks of four kinds
// grows along a Fibonacci curve and has a nesting limit of 28, and a
// component tree of rows and columns 20 (their documents' fit is under
// "Working set").
//
// # Runtime
//
// In onRequestBody the Filter accepts application/json and any */*+json
// media type whose only parameter is charset=utf-8, refuses an empty body,
// decodes the body strictly with internal/jsonval (one RFC 8259 value,
// valid UTF-8, depth at most 64, duplicate member names refused, numbers
// kept as their literal text), and walks the decoded body once to check it
// against the schema's limits before the library runs, which takes no
// context. Each limit is a multiple of limits.maxRequestBodyBytes (10 MiB
// when the snapshot leaves the limit unset), and a body past one is
// filter.ErrTooLarge (413 RZ-RT-003) before anything is reserved: decoded
// values costing more than filter.DecodedLimitFactor times it; a decision
// pass that may hold more than WorkingSetFactor times it besides the
// decoded values (see "Working set"); work over WorkPerByte work units per
// byte of it (see "Work bound"); a reservation, the decoded values
// with what their maps, arrays and strings hold beyond the decoder's
// charge and the decision pass's working set, over ReservationFactor
// times it, so that with the raw body the request side holds at most 13
// times the limit (130 MiB at the default); and a body value nested
// as deep as the schema's nesting limit on a path whose state does
// not bound it. The working-set, reservation and work limits scale
// from MinScaledLimit (128 KiB) at least, 1 MiB, 1.5 MiB and 2^25 work
// units (proposed): what any body's validation holds and takes has parts
// that do not grow with the body, the decision pass's wrapper, a
// call-stack chain per value of the path, a matcher of the largest program
// that may match a string, the evaluations at the root, so scaled from a
// limit of a few KiB they would refuse valid bodies of a few bytes ({}
// under true at 1 and 2 KiB, a 616-byte order composed four levels
// deep at 16 KiB). Every maxRequestBodyBytes therefore works for small
// bodies, while the decoded limit still scales from the raw limit,
// filter.DecodedLimitFactor times it. A reservation the budget refuses is
// filter.ErrBudget (503 RZ-RT-004); the executor answers both under either
// failureMode. A request whose context has ended is cannot decide before
// anything is reserved. Schema.ValidateWithin applies the same limits, but
// for the reservation, to a value an M2 caller decoded.
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
// quotes request content. The location pass runs only when its working set
// fits the working-set and reservation limits, the work of both passes and
// of its walk over the error tree the work limit, and the budget the rest
// of its reservation; otherwise, or when the request ends during the walk,
// the body is refused with DetailMismatch alone, a decision. An internal
// validator error, which the location pass reports, is cannot decide with
// the class default.
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
//   - the decision pass's error tree: at every value, 104 bytes per error unit
//     (the 80-byte error and its slot in the one list that holds it, while the
//     list doubles; a unit is a keyword that can fail, and a type, const or
//     enum failure ends its evaluation with one, as an asserted format would,
//     which the compile refuses) and per group error of an evaluation that may
//     collect two; the references other than $ref ($recursiveRef and
//     $dynamicRef) and the applicators run only when none of the value's own
//     keywords failed, so their errors and those exclude each other
//     (TestDecisionPassOrder); at an object, the evaluations that loop over its
//     members keep one member's errors each (all of them under
//     unevaluatedProperties), and a failure before the loop excludes it; every
//     evaluation with propertyNames validates every member name on its own,
//     keeping the name's errors in either pass and wrapping them in one more
//     error, only for a name whose errors the scan cannot rule out;
//   - the location pass's error tree: at every value, 192 bytes per error unit
//     plus 32 per level of depth (measured at 123 to 141 bytes per failing
//     evaluation plus 16.7 per level, and half again for group errors), the
//     names its required, dependency and type lists copy, the rational a
//     failing numeric keyword keeps, the error that wraps a failing member
//     name's under each evaluation with propertyNames, and at an array 16 bytes
//     per item, at least two, for each evaluation with minContains or
//     maxContains, whose error keeps the last array of its list of the items
//     that match contains (100 such evaluations over 16,000 items hold
//     12.7 MB);
//   - along the costliest path from the root, the call stack: the library
//     evaluates the in-place members of a schema one after the other, so at
//     each value it holds one chain of in-place evaluations, never more than
//     the value's evaluations (with every evaluation charged a frame, a 14 KB
//     rich-text body would reserve 29 MB of stack), 8 KiB per evaluation of the
//     longest chain the value's state allows (measured with debug.SetMaxStack
//     at 1.4 to 1.6 KiB per in-place evaluation and 2.2 to 2.5 KiB per
//     evaluation of a nesting level's chain, up to three times that while the
//     stack doubles and copies; the decision pass's wrapper adds three;
//     TestStackWithinCharge), which leaves room for the records an evaluation
//     keeps on the heap while it runs, its validator, scope and unevaluated
//     record and the first array of its error list, about 230 bytes; per
//     evaluation of that chain that may copy unevaluated names, a map of 256
//     bytes plus 128 per member (measured at most 256 plus 122), or for indexes
//     192 plus 96 per item (measured at most 192 plus 83), the library dropping
//     a branch's copy when it merges it, before the next branch runs (charged
//     per evaluation, an allOf of 50 mixins under unevaluatedProperties would
//     be charged 84 times its peak and a 49 KB object refused,
//     TestHandleUnevaluatedBranches); per evaluation with additionalProperties
//     false 48 bytes per member it may list, and with contains 24 bytes per
//     item, at least two, for the indexes of the items that match, which it
//     lists while it loops: three times a name's or an index's bytes, the old
//     array live beside the new while append copies the list (a list just grown
//     past a capacity holds up to 23.9 bytes per item, TestGrowthCharges); per
//     object that a uniqueItems pass hashes, the sorted slice of its names, 16
//     bytes a name at the slice's allocation size; and once, 256 bytes for the
//     one list a pass grows at a time, whose new array the allocator's size
//     classes and headers round up past its items' charges;
//   - once, the largest transient copy: the copy of the longest string, which
//     the library makes at every evaluation, at its allocation size (a
//     100,000-byte string's copy takes 106,496 bytes, TestCopySizes), the
//     duplicates map of the longest uniqueItems array (192 bytes per item; it
//     holds about 58 and allocates up to 187 while it grows,
//     TestGrowthCharges), or a regexp match of a string or a member name: the
//     queues and threads of the largest program that may match it (64 bytes per
//     instruction), the backtracker's visited set (32 KiB), and the
//     backtracker's job stack. Go's regexp runs its backtracker for a program
//     of at most 500 instructions on an input shorter than 262,144 divided by
//     the program's instruction count, in bytes, and on n bytes its stack holds
//     at most (n+1) times held jobs plus one, held being the most alternations
//     of the program reachable through empty-width steps from where the search
//     at one input position starts (regexcost.go: 3 of the WHATWG e-mail
//     pattern's 126, all 241 of (?:(?:a??){240}a)*b), at 48 bytes a job (16 in
//     a slice grown by appending, up to three times that while it grows), for
//     the program holding the most that the backtracker may run on n bytes:
//     never more than the visited set's 2^18 pairs, about 12.6 MB. Only the
//     patterns a string's own path applies to it, and its object's
//     patternProperties to a member name, count; where the scan does not know
//     the path, every pattern of the document, wherever a pattern or a length
//     keyword may scan a value at its depth (a pattern of 241 alternations
//     holds 4.9 MB for a string of 537 bytes, TestHandleBacktrackCharge; every
//     alternation of the document's costliest pattern, charged to every string
//     a pattern or a uniqueItems pass reaches, would refuse a 149-byte sign-up
//     body at a 64 KiB limit, TestHandlePatternFit). The job stack binds before
//     the body's size only at small limits, and only for the inputs short
//     enough for the backtracker, which a longer one leaves for the NFA
//     simulation: up to MinScaledLimit, whose working set is 1 MiB, a string of
//     about 20,400 to 37,400 bytes under ^[^<>]*$, a program of 7 instructions
//     holding one job per position, is refused with 413, and one of about 5,100
//     to 20,100 bytes under ^(?:[a-z]+|[0-9]+|\s)*$, 13 instructions holding 4;
//     at 256 KiB the first never binds and the second from about 10,600 bytes
//     (4% of the limit); from 512 KiB up neither does, and from about 1.6 MiB
//     up no pattern does (proposed, an accepted consequence; matching with the
//     NFA simulation alone would remove it, at two to nine times the time of
//     the backtracker on such strings);
//   - in the location pass, at least what its walk over the error tree holds
//     once the pass has returned and dropped its path and transient copies:
//     four pointers of the document's longest location and keyword at their
//     allocation size (walkBytes; a body of one byte failing under a $defs
//     name of 100,000 bytes holds 106 KB in the walk, TestHandleWalkMemory).
//
// TestWorkingSetCoversPeak measures the error trees exactly, the decision
// pass's at its end, for dense failing bodies of every error shape, along
// reference chains, for ordinary order bodies and for bodies whose evaluations
// make copies and drop them: the dense trees stayed within 0.89 of the charge
// in either pass. It measures the live heap at the peak of each pass too,
// deterministically but for what the runtime itself allocates meanwhile (a
// thread it starts under load, 5.3 KB, which none of 54,000 measurements on the
// tests' one processor saw; the tests allow 16 KiB): it collects the heap fully
// where the library calls a test's probe, at the start of evaluations and at
// the end of those that reach their extension keywords, at the first of them in
// a pass, at others spread over the rest of it and at its end, and at its
// matches, whose matcher's own peak it computes from the match's allocations;
// what the library copies and drops between two such calls is replayed, the
// largest copy added to the largest live heap as the model adds its transient
// copy: a string's copy, the uniqueItems duplicates map, whose replica
// allocates exactly what the library's does (TestDuplicatesReplica), with the
// names it sorts, the names additionalProperties false lists, the errors a
// contains loop over false drops, the location walk's pointers and, bounded at
// 16 KiB within the number bounds, a number check's rationals
// (TestNumberCheckMemory). A list or a map also holds its old array or table
// besides the new one while it grows: TestGrowthCharges replays every growth
// against its charge, and TestCopySizes every copy's allocation size (a sample
// of the heap in use would count the garbage the library drops, under load up
// to 1.64 times the charge of the unevaluated indexes under branches whose live
// heap stays within 0.38 of it; marks at the first evaluations alone would miss
// the tree a failing body's decision pass grows to its end, the library running
// a schema's extension keywords after its applicators failed; and marks at the
// end of evaluations alone would miss every copy, the duplicates map of a valid
// uniqueItems array, about 58 bytes per item, among them, and the evaluations
// of true and those that fail before their end). The live peak stayed within
// 0.88 of the pass's whole working set in either pass, 0.96 for the sorted
// names of a wide object, which are most of its charge, and a pass that holds
// its error tree and one path's copies at once holds more than either term
// (TestDecisionTreeBesideCopies). The decision pass is charged its worst case
// over the library's member order, in which every alternative of a union may
// descend into the members of a value that fails it before failing on its
// discriminator, so a valid document of unions or recursive blocks reserves
// more than it holds.
//
// A valid body reserves its decoded values and the decision pass's working
// set. The decoder charges a value its headers and bytes (internal/jsonval)
// but not the hash table of an object, a group of 8 slots up to 8 members
// and a power of two slots past them, nor the spare capacity of an array's
// backing array (a one-member object holds 336 bytes, of which the decoder
// charges 80, so valid arrays of small objects would hold up to 3.8
// times the reservation); the scan adds both, and the rounding of every
// string to its allocation size class, or from 2 to 15 bytes its share of
// the allocator's 16-byte blocks, at most its own block or twice its bytes
// (mapExtra, arrayExtra and stringExtra; maps and arrays measured within
// 1% on Go 1.26 and 1.27, strings of 9 to 15 bytes, each alone in its
// block, at their charge; charged their bytes alone, the 9-byte tags of an
// order would hold Handle's live heap 3.6% over its reservation). So the
// reservation covers the decoded tree, measured after a collection, and the
// tiny allocator's blocks, counted by address
// (TestHandleReservesDecodedMaps, TestHandleMeasuredFit and
// TestTinyBlocks). The decoded limit, filter.DecodedLimitFactor times the
// raw limit, still applies to the decoder's charge, and the reservation
// limit to the whole (without it, arrays of one-member objects would
// reserve up to 17.6 times the raw limit).
//
// The measured fit at the 10 MiB default (typical records fit up to about
// 70 to 84% of the limit, dense enum, union, numeric and uniqueItems
// arrays less, and operators raise maxRequestBodyBytes on Routes that
// validate them), each the largest valid body of its shape that passed, as
// measured, with the limit that stopped the next
// (TestHandleAcceptsLargeBodies and TestHandleMeasuredFit pin that each
// passes, within a few percent of its fraction, and reserves what the
// model charges; TestHandleFitEdges the counts; and
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
//   - dense numeric arrays: decimals under a minimum and a maximum, 53%, and
//     prices under multipleOf 0.01, 69%, at the working-set limit (under a
//     divisor of a thousand digits such as 1e-1000 about 10%, at the work
//     limit); pairs of a timestamp and a value under prefixItems, 63%, at
//     the decoded limit; longitude and latitude pairs, each bounded and
//     counted by minItems and maxItems, 38%, at the working-set limit; a time
//     series of objects of two numbers under required and additionalProperties
//     false, 51.5%, at the reservation limit;
//   - uniqueItems arrays: of integer ids, 33%, of short string tags, 42%,
//     of distinct prices, 37%, and of bounded three-decimal readings,
//     21%, all at the working-set limit (the duplicates map; a decimal is
//     charged only the numbers whose bytes can hash alike, hashColliders,
//     where every split of its bytes held such arrays to 10 to 12%, at
//     the work limit); of objects, about 6,500 of two string members
//     (2.1%) or 1,700 of one number member (0.19%), at the work limit,
//     every pair being charged a comparison;
//   - arrays of events under an 8-way and a 20-way oneOf of object variants:
//     32% and 13.6%, at the working-set limit, every failing variant being
//     charged its member's errors;
//   - arrays of orders composed with allOf of a base and their own
//     properties under unevaluatedProperties false: 56%, at the
//     working-set limit (38% for the composition nested in orders and
//     their lines);
//   - arrays of records whose required member an if, then and else chooses
//     by kind: 61%, at the reservation limit;
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
// The reservation covers the live heap at Handle's peak for every shape above,
// measured as TestWorkingSetCoversPeak measures it and at the reservation
// (a sample of the heap in use would also count the tables a wide object's map
// outgrew and the arrays the decoder outgrew, garbage before anything is
// reserved, and marks at the end of evaluations alone would leave out the
// uniqueItems duplicates map and a long string's copy). For records and arrays
// of small objects the reservation is 1.3 to 1.6 times that peak, the decoded
// tree being most of both; for long strings 1.0, the string's copy as large as
// the string; for conditional records and timestamp pairs 2; for uniqueItems
// arrays 2.4 to 2.7, charged 192 bytes per item for duplicates maps that hold
// about 58; for dense numeric and enum arrays at the working-set limit 3.7 to
// 4.5, charged errors a valid body does not hold; for compositions, unions and
// recursive documents 3.2 to 18 at the sizes above, and up to about 114 for the
// component tree nested 6 deep.
//
// The 1 KiB order of BenchmarkValidate1KiB takes about 90 µs on that machine,
// above the 25 µs p50 target (an Open question); the library's validation is
// two thirds of it.
//
// # Work bound
//
// The library takes no context, so validation cannot be stopped once
// it starts, and its work grows with the values as well as with the
// evaluations: every evaluation at an object loops over all its members
// and looks their names up, hashing them whole, every evaluation at
// a string copies it and a pattern scans it, const and enum compare
// whole values and parse numbers with math/big, the unevaluated copies
// grow with the members or items and their names, uniqueItems hashes
// every value below the array and compares the items that share a hash,
// and multipleOf divides with math/big. The same walk therefore sums
// an upper bound of the work, in work units of about a nanosecond of the
// library on that machine, each term measured and rounded up by at least a
// quarter:
//
//   - per evaluation 384, per keyword that can fail 320 (type, const and
//     enum count once), per name of a list 32, and 1 per 8 bytes of the
//     required and dependency names it looks up in an object; the schema
//     side of const and enum comparisons, 12 per value, numbers at their
//     parse cost and a number nested in a const or enum value also at the
//     costliest body literal; per pattern, its setup; and the schema side
//     taken from the schema's own literals and compiled bounds, not from
//     the number bounds, so a literal past them, or a bound with more
//     digits than one within them gives, which only a position the walk
//     did not walk could hold, is charged its length and exponent, or its
//     digits, growing with their square past the bounds (a defense in
//     depth: with its exponent's charge capped at the bound, a const of
//     1e99999 at such a position would run at 29.7 ns a unit);
//   - per member of an object, 48 per evaluation at it, and 384 per
//     evaluation that may copy unevaluated names (256 per item for
//     indexes); and per 8 bytes of its name, 1 for every time the library
//     may hash or compare it: once per evaluation with member keywords
//     (the properties lookup), five times per evaluation that may copy
//     unevaluated names, per uniqueItems pass once plus twice the bits of
//     the member count (sorting the names), and once per uniqueItems
//     comparison; and per 8 bytes of the object's longest name, 1 per
//     const or enum object compared with it, which stops at the first name
//     it lacks (were none of these to grow with the name, a 10 MiB name
//     under 3,200 lookups or a 4,000-link unevaluated chain, hashed for
//     1.35 to 3.2 s, would be charged a few milliseconds);
//   - per byte of a string, 1 per evaluation at it, 2 more per evaluation
//     with minLength or maxLength, which counts its runes, 1 per
//     uniqueItems pass that hashes it, and per pattern 16 plus 16 per
//     entry of the largest run queue the pattern's NFA can build
//     (regexcost.go: an exploration of its queues over its character
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
//     40% under the model; charged as one parse of the body literal,
//     806,202 short decimals against a divisor of 127 bytes would pass
//     while taking 6.0 to 6.5 s, over twice the bound below);
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
//     value, and with $dynamicRef, 4 per evaluation of the chains of the
//     whole path (a $recursiveRef walks the whole scope only when its
//     target has a $recursiveAnchor of true, which the compile refuses, so
//     it costs what a $ref does);
//   - apart, per error unit of the location pass, its walk over the error
//     tree for the first failing leaf: 192 plus 2 per byte of the longest
//     location and keyword an error of the document may name (walkWork;
//     the walk reads every leaf's location, so a 160 KB body failing
//     under a 250,000-byte $defs name would otherwise hold Handle for
//     23 s).
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
// than about 1,700 to 6,500 objects ("Working set").
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
// that each is a string (20 literals of exponent 999,999 in a 241-byte
// type list would compile for 11 s, and 21 that math/big refuses would
// make it panic). Such an entry that is not a string is refused at the
// walk, with the meta-schema's own message, at every schema position the
// library compiles or checks (below additionalItems only where it compiles
// one), so it never compares one: a number, and arrays and objects, whose
// comparison its hash does not bound. Both bounds admit every IEEE 754
// double and integers far beyond 64 bits, so large integers still validate
// exactly.
//
// The package never logs, starts no goroutine and reads no clock. It
// builds for 64-bit platforms only, the only ones Ruralz ships (ADR-0001):
// its counts are ints that saturate at 2^40 (countCap).
package jsonschema
