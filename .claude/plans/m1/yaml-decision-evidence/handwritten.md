# Hand-written parser for the restricted YAML profile: evidence

Role: handwritten. Question: can Ruralz replace `github.com/goccy/go-yaml`
plus WP-33's workaround layer in `internal/config/profile` with a parser
written for exactly the restricted profile, and at what effort and risk?

All paths below are relative to the scratch directory
`/tmp/claude-0/-home-user-ruralz/0aabfe6e-34a6-51d2-9b9b-23886b703760/scratchpad/yamldecision/handwritten/`
unless they start with `/home/user/ruralz`. The repository was not modified
(`git status --short | wc -l` printed `0` after every run). WP-33 was
exercised read-only through `go test -overlay`, which adds scratch files to
the package build without writing into the tree.

## 1. Summary of results

| Measure | Result | Where |
|---|---|---|
| Spike size | `hw/parser.go` 1,156 lines (1,008 non-blank non-comment), plus 183-line `convert.go` (typing and duplicate keys, the WP-33 converter's job) | section 4 |
| Code reused from WP-33 verbatim | 433 lines of readers (`readQuoted`, escapes, `readBlockScalar`, `headerValid`, `detectIndent`, `lineEnd`, `markerLine`, `yamlPrintable`) with a one-line change, and all of `scalar.go` (318 lines) | section 3 |
| YAML Test Suite (402 cases, data-2022-01-17), exact `test.event` comparison | 241 of 242 valid in-profile cases equal, the other (9MMW) is a complex key the profile refuses; 94 of 94 error cases rejected; 66 of 66 out-of-profile cases show their reason, and 46 of them also produce the exact event stream | section 5 |
| Expected-failures list | from 87 entries (66 `profile:*`, 21 `reviewed:goccy-*`) to 67: the 20 other reviewed goccy cases pass | section 5 |
| Differential against WP-33 on 2,455 inputs (suite, fuzz corpus, all string literals of WP-33's tests) | 0 tree differences where both accept; every acceptance difference on the suite goes the suite's way for the spike | section 6 |
| Generated streams (random trees rendered in random styles, checked against the tree they came from) | 0 mismatches in 100,000 streams (12.7 MB) | section 7 |
| Coverage-guided differential fuzz against WP-33 | 3,377,636 executions in 6 min, no panic or hang; all 1,527 tree differences shrink to WP-33's open harden2 blocker 3 (tab rewrite); WP-33 accepts several streams the suite rules invalid and misses `<<` merge keys after a tab or `?` | section 8 |
| Cost | linear; 1 MiB hostile shapes 0.08 to 120 ms; 200,000 nested `[` stopped at level 65 in 80 µs; synthetic 1,000-Route benchmark 21 ms/op, 11.8 MB/op, 141k allocs/op against WP-33's 150 ms/op, 53.5 MB/op, 1.43M allocs/op | section 9 |
| Defects found in the spike during the assessment | 6, each by a different oracle, all fixed in the spike | section 10 |

## 2. What the profile needs

- Spec 01 section B (`/home/user/ruralz/.claude/plans/m1/specs/01-config-load.md:66-80`): byte pre-checks (req 7), the token pass (req 8), depth (req 9), parse (req 10), duplicate keys (11), Ruralz-owned core-schema typing (12), documents (13), positions in code points (14). Requirements 8 to 10 are written in goccy's terms (`scanner.Scanner`, `lexer.Tokenize`, `parser.Parse(tokens, 0, parser.AllowDuplicateMapKey())`).
- CM "Restricted YAML profile" (`/home/user/ruralz/docs/architecture/02-configuration-model.md:46-61`): comments, flow style and multi-document files accepted; YAML 1.2 core scalars; no duplicate keys, anchors, aliases, merge keys, custom tags; UTF-8 only; 64 MiB, 20,000 resources, 64 levels.
- What Ruralz configuration actually uses: block mappings and sequences, flow collections (`hosts: [a.example.com]`), plain, quoted and literal scalars, comments, `---` separators. Explicit keys (`?`), folded scalars, `%YAML 1.2` and core tags are allowed by the spec but not needed by the example Bundle.

## 3. Reuse of the existing WP-33 readers

Classification of `/home/user/ruralz/internal/config/profile/*.go` (production, `wc -l` and non-blank non-comment lines):

| File | Lines (code) | Reuse in a hand-written parser |
|---|---|---|
| `quoted.go` | 252 (203) | `readQuoted`, `foldBreaks`, `appendEscape`, `appendCodePoint`, `hexValue` (quoted.go:19-212) reused verbatim; `tokenPass.quoted` (221-252) is goccy glue, dropped |
| `literal.go` | 257 (185) | `headerValid`, `readBlockScalar`, `detectIndent` (literal.go:21-172) reused; `tokenPass.blockScalar` (187-257) dropped. One change needed: see S98Z below |
| `scalar.go` | 318 (259) | reused unchanged (typing, tags, normalization) |
| `precheck.go` | 106 (87) | reused (`yamlPrintable`, CR LF handling) |
| `source.go` | 117 (77) | `lineEnd`, `leadingSpaces`, `markerAt` reused; `srcCursor`/`advance` exist to map goccy positions back to the source, dropped |
| `json.go`, `encode.go`, `profile.go` | 181, 434, 266 | unchanged (JSON front end, encoder, API) |
| `tokens.go`, `yaml.go`, `split.go`, `columns.go`, `chomp.go`, `explicit.go`, `flowentry.go`, `flowcost.go`, `segment.go`, `plain.go` | 4,150 lines (2,894 code) | goccy-specific (token pass, column corrections, split parse, cost bounds, misread refusals): replaced by the parser |

Totals: 6,232 production lines (4,354 code) and 6,902 test lines (5,460 code) (`ls *.go | grep -v _test.go | xargs cat | grep -cvE '^\s*(//.*)?$'` printed `4354`; the same over `*_test.go` printed `5460`).

Reuse caveat (evidence): `detectIndent` exempts comment lines from the "leading empty line holds more spaces than the first content line" check (`literal.go:162`, `line[sp] != '#'`). Used alone it accepts suite error case S98Z (`empty block scalar: >\n \n  \n   \n # comment`); WP-33 passes S98Z only because goccy refuses it. Removing the exemption makes the spike reject S98Z and changes no other suite result (`diff out/reuse.go.orig spike/hw/reuse.go` shows the one line; suite before: `error map[FAIL:1 pass:93]`, after: `error map[pass:94]`). The readers were written to correct goccy's values, not to be complete; each reused function needs its own review against YAML 1.2.2.

## 4. The spike

`spike/hw/parser.go`: recursive descent over the text, no token stream.
- Block structure by indentation: `node(n, ctx, compact, outer)` parses `s-l+block-node`/`s-l+block-indented` with parent indentation `n`; `blockMap(m)`, `blockSeq(m)` hold entries at column `m`; the sequence-at-key-column rule (`seq-spaces`) for mapping values; compact `- - a`, `- k: v`; tabs never indent, and a tab after an indicator forbids a compact collection.
- Implicit keys are detected without backtracking: the first node on a line is read once, and becomes a key when `:` and white space follow on the same line, or is continued as a multi-line plain scalar otherwise.
- Flow collections: `flowCollection(ind)` with flow pairs, explicit `?` entries, JSON-like adjacent values, empty keys and values, trailing commas; every flow line must be indented at least `ind` (YAML 1.2.2 `s-flow-line-prefix`).
- Scalars: plain (`plainLine`, `plainLines` with YAML 1.2.2 folding and continuation rules), quoted (WP-33 `readQuoted` plus indentation and marker checks), block (WP-33 `readBlockScalar`).
- Profile: anchors, aliases and plain `<<` keys are RZ-CFG-003, tags other than the seven core tags and `%TAG` RZ-CFG-004, both collected while the parse goes on; `%YAML` other than 1.2, non-scalar keys and every syntax error RZ-CFG-001, which stops the file.
- Limits: depth counted per open collection (`enter`), so recursion depth is bounded by `MaxDepth`; a node budget (`MaxNodes`) replaces the token budget.
- Positions: 1-based line and code-point column of every node, key and error (`colAt` with a per-line cache).

Supporting code: `hw/convert.go` (typing with the copied `scalar.go`, duplicate keys, null documents skipped, in the dump format of WP-33's tree), `hw/events.go` (test.event format), `gen/gen.go` (tree generator and renderer), `cmd/{suite,diff,prop,minv3,hostile,probe,depth,firsterr,inputs,rename}`, `overlay/` (WP-33 dump, fuzz and shrink tests used only through `-overlay`).

## 5. YAML Test Suite, exact event streams

Command: `cd spike && go build -o ../out/suite ./cmd/suite && ../out/suite -v` (output `out/suite-final.txt`):

```
9MMW      valid-in-profile      FAIL  reviewed:goccy-v1.19.2/single-pair-implicit-entries rejected: RZ-CFG-001@3:5 syntax error: a mapping key must be a scalar
cases 402
valid-in-profile      map[FAIL:1 pass:241]
error                 map[pass:94]
valid-out-of-profile  map[pass:20 pass+events:46]
goccy-reviewed cases (listed reviewed:goccy-v1.19.2/*): map[FAIL:1 pass:20]
```

- The runner compares the full event stream (`+DOC ---`, `-DOC ...`, `+MAP {}`, scalar styles, tags), which is stricter than WP-33's runner (`yamltestsuite_integration_test.go:271-317` compares trees and scalar styles, not markers or flow style).
- 9MMW holds `[ {JSON: like}:adjacent ]`, a flow mapping as a key: a `profile:complex-key` case, listed today as a goccy limitation.
- Classes: valid-in-profile are cases without an `error` file and without a `profile:*` entry in `/home/user/ruralz/test/fixtures/yaml-test-suite/expected-failures.txt` (that file: 29 anchor, 21 tag, 12 complex-key, 4 directive, 21 reviewed; `grep -v '^#' ... | awk '{print $2}' | sort | uniq -c`).
- "pass+events": the anchor and tag cases keep parsing after the collected RZ-CFG-003/004, and 46 of them produce the exact stream, so the structure parse is checked beyond the profile too.

## 6. Differential against WP-33 on fixed inputs

Inputs (`out/inputs-all.jsonl`, `cmd/inputs all`): 402 suite inputs, 19 seeds of `testdata/fuzz/FuzzLoadYAML`, 2,034 string literals from WP-33's `*_test.go` files that contain a YAML indicator. WP-33 results came from `overlay/zz_hwdump_test.go` (`CGO_ENABLED=0 HWDUMP_IN=... HWDUMP_OUT=out/wp33-all.jsonl go test -overlay overlay/overlay.json -run '^TestHWDump$' ./internal/config/profile/`). Comparison: `out/diff -in out/inputs-all.jsonl -wp33 out/wp33-all.jsonl` (`out/diff-final.txt`):

```
suite:wp33:agree-accept                              213
suite:wp33:both-accept-positions-differ              14
suite:wp33:agree-reject-same-code                    152
suite:wp33:agree-reject-other-code                   4
suite:wp33:hw-accepts-wp33-rejects                   13
suite:wp33:wp33-accepts-hw-rejects                   6
lit:wp33:agree-accept                                973
lit:wp33:both-accept-positions-differ                71
lit:wp33:hw-accepts-wp33-rejects                     258
lit:wp33:wp33-accepts-hw-rejects                     5
```

- No `both-accept-tree-differs` row exists for WP-33: on the 1,277 inputs both accept (227 suite, 1,044 literals, 6 corpus), the typed trees are equal.
- Suite, spike accepts and WP-33 rejects (13): 4MUZ/02, CFD4, DFF7, DK95/04, FH7J, FRK4, M7A3, NHX8, NKF9, S3PD, SM9W/01, UKK6/00, VJP3/01, all valid per the suite and listed as goccy limitations.
- Suite, WP-33 accepts and spike rejects (6): 9C9N, 9JBA, CVW2, QB6E, SU5Z, Y79Y/003, all error cases (WP-33 pins them as accepted-invalid in `TestReviewedFindings`).
- Literals WP-33 accepts and the spike rejects (5): `flowcost_test.go:80-81`, `flowentry_test.go:116,122`, `quoted_test.go:19`. Three of them are asserted as accepted by WP-33's tests (`quoted_test.go:19` wants `{k:s:"  "}` for `k: "\ \n"`; `flowentry_test.go:116` and `:122` are under the comment "Entries YAML 1.2 accepts"), but they have the shapes of suite error cases QB6E, 9C9N and VJP3/00 (a line inside a quoted scalar or flow collection under a block mapping indented less than one space). WP-33's tests therefore cannot be reused verbatim.
- Positions: the 14 suite differences are conventions (empty value placed at the next token instead of after the key; block mapping placed at `?` instead of the key; mapping under a tagged key). First-error positions on the 88 suite error cases both reject: 58 identical, 20 same line, 10 other line (`out/spike-firsterr.jsonl` against `out/wp33-all.jsonl`).

## 7. Generated streams (property test, spec 01 test plan P2)

`gen/gen.go` builds random trees (strings from a word list with indicators, tabs, `---`, `...`, Unicode) and renders them in random styles: plain (one or several lines, tabs after continuation indentation), single and double quoted (folded over lines), literal block scalars with indentation indicators and all chomping modes, block mappings and sequences at random indentation, compact collections, sequences at the key's column, flow collections over several lines with trailing commas, flow pairs, adjacent JSON values, comments and blank lines, `%YAML 1.2`, `---`, `...`. The parse is compared with the rendered tree, so no other parser is the oracle.

```
$ out/prop -n 100000 -seed 1000000
streams 100000, bytes 12737919, spike mismatches 0, yaml.v3 mismatches 37831, spike parse time 504.862901ms
```

yaml.v3 v3.0.1 is not a usable YAML 1.2 oracle: it refuses `%YAML 1.2` (`yaml: found incompatible YAML document`, `go run ./cmd/v3probe`), refuses tabs at a line start, reads `[?y]` as `[{y: null}]` and `[a:]` as `["a:"]`, and accepts `x: [\na: 1,\nb: 2\n]` and `k: "a\n\tb"` (suite shapes 9C9N and DK95/01, both errors). Its disagreements were still used: `cmd/minv3` shrinks every stream the spike accepts and yaml.v3 does not read the same way. The first shrink run found a spike defect (section 10, item 6); after the fix, 264 shrunk groups without tabs (`out/minv3-plain2.txt`) and 214 with tabs (`out/minv3-tabs.txt`) all reduce to yaml.v3's YAML 1.1 behaviour (`?` and `:` starting a plain scalar in a flow collection, a tab before a top-level node, empty keys).

Gaps of the generator: no folded scalars, no explicit `?` keys, no tags, no comments inside flow collections; the suite and section 8 cover those.

## 8. Coverage-guided differential fuzz against WP-33

The spike's files were copied into package `profile` with every top-level identifier prefixed `hw` (`cmd/rename`), and `overlay/fuzz/zz_hw_fuzz_test.go` runs both parsers on each input. The test binary was built with `go test -c -fuzz=FuzzHWvsWP33 -overlay overlay/fuzz-overlay.json` and run from `out/fuzzwd` (so the repository's `testdata` is never written), seeded with the 2,455 inputs:

```
fuzz: elapsed: 6m0s, execs: 3377636 (8587/sec), new interesting: 777 (total: 3232)
PASS
```

No panic and no hang (a panic other than the parser's internal stop would fail the target). Disagreements were logged (`out/hwfuzz.log.gz`, 325,854 lines) and classified:

```
Counter({'hw-accepts-wp33-rejects': 321534, 'wp33-accepts-hw-rejects': 2793, 'tree': 1527})
```

- Trees differ (1,527): `TestHWShrink` reduced all of them to 199 inputs, every one containing a quote, a tab and `:` (`out/shrink-tree.txt`), for example `a"\t:b`: WP-33 returns `a" :b`, the spike `a"\t:b`. This is the open harden2 blocker 3 (`untabSeparators`, `/home/user/ruralz/.claude/plans/m1/runs/w3-s1a-harden2-result.json`), found again in 28 s by the first, uninstrumented run. The spike reads it as YAML 1.2 (and yaml.v3) does.
- WP-33 accepts, spike rejects (2,793), shrunk per message (`out/shrink-hwrej.txt`): `a: [\n]`, `- [\n]`, `x: {\n}` (line indented less than its context: VJP3/00 shape), comments with no white space before them (`""#`, `[0,#\n]`: SU5Z, CVW2 shapes), `{,}` (leading comma), `{a:[]}` (YAML 1.2.2 production 147 needs white space after `:` unless the key is JSON-like), `\n\t-` and `\t? k` (a tab cannot indent a block collection: Y79Y/004 to 009 shapes), `% d\n---` (a directive needs a name, production `ns-directive-name`), `[1\n b:\n]` (implicit key over two lines in a flow sequence), and `<<:\t""`, `? <<` (merge keys). Probing WP-33's public `Parse` directly (`out/probe-wp33-out.jsonl`): `<<: x` is RZ-CFG-003, but `<<:\tx` is accepted as `{'<<': 'x'}` and `? <<\n: x` as `{'<<': 'x'}`, so the RZ-CFG-003 merge-key check of spec 01 req 8 can be bypassed; this is not among the remaining findings of the harden2 record.
- Spike accepts, WP-33 rejects (321,534): 279,032 are WP-33's deliberate refusal of `{app:web}`-style colons; the rest, shrunk per message (`out/shrink-wprej.txt`), are valid YAML 1.2 that goccy refuses: `|\n....` (M7A3 shape), `a:\n\t` (DK95/04 shape), `[? ]`, `:`, `[k: ]`, `!!str k: x\n y`, `x: |\n\n'':`, ` |2\n#`, and `<<<:` / `1<<:` that WP-33 reports as merge keys although the key is not `<<` (false positives).

## 9. Hostile inputs and cost

`cmd/hostile <case> <bytes>` (outputs `out/hostile-1mib.txt`, `out/hostile-large.txt`, `out/hostile-scaling.txt`):

```
deep-flow         bytes=200000    time=80µs     ... RZ-CFG-001@1:64 syntax error: nesting is deeper than 64 levels
wide-flow-seq     bytes=1048579   time=119.414ms  alloc=69.7 MiB live=62.2 MiB docs=1
wide-flow-map     bytes=1048585   time=42.512ms   alloc=26.3 MiB live=22.1 MiB
flow-nulls        bytes=396003    time=1.354ms    ... RZ-CFG-001@1:16007 syntax error: unexpected '[' after the node
routes            bytes=2808890   time=82.946ms   alloc=45.8 MiB live=46.4 MiB docs=20000 errs=0
long-indent       bytes=1052633   time=729µs
blank-lines       bytes=1048586   time=24.33ms
anchors           bytes=1048579   time=229.081ms  errs=174762
routes            bytes=67108992  time=783.341ms  ... RZ-CFG-001@1997289:5 syntax error: more than 4194304 nodes
block-scalar      bytes=67108865  time=306.06ms   live=222.1 MiB
```

- The goccy cost classes of the review rounds do not exist by construction: no token materialization, one pass, no re-parse of ranges. The harden2 null-shift shape (`k: {a,...a} [1,...]`, 396 KB, 5.65 s in WP-33 per the record) costs 1.35 ms; the 20,000-document single file that WP-33 now refuses with "file has more than 500000 tokens" (harden2 major) parses in 83 ms.
- Scaling 1, 4, 16 MiB: blank-lines 24, 68, 254 ms; quoted-multiline 10, 36, 128 ms; long-indent 1.9, 2.8, 11 ms (linear).
- Depth (`go run ./cmd/depth`): 64 nested flow sequences and 64 nested block mappings accepted, 65 refused (`1:65` for flow; block reports `65:66`, one column right of the 65th key, a position detail to fix).
- Memory is the tree's (about 120 to 250 bytes per node in the spike's `Node`); the per-document node budget and MaxDiagnostics cap (10,000, not in the spike) bound it.
- Benchmark on WP-33's own synthetic input (1,000 Route documents, parse plus typing):

```
spike: BenchmarkParseYAML-4  171  21149986 ns/op  22.11 MB/s  11784058 B/op   141023 allocs/op
WP-33: BenchmarkParseYAML-4   22 151229998 ns/op   3.09 MB/s  53463284 B/op  1430196 allocs/op  1430 allocs/resource
```

## 10. Defects found in the spike during the assessment

| # | Defect | Found by | Status |
|---|---|---|---|
| 1 | Comment after a node rejected when the separating white space had been consumed (`key:    # c`) | YAML Test Suite (2LFX, 5NYZ, 735Y, J9HZ, P94K, W42U, ...) | fixed |
| 2 | Tab-only line after a block scalar accepted (Y79Y/000; after a block scalar only `l-chomped-empty` lines may follow) | suite | fixed |
| 3 | Reused `detectIndent` comment exemption accepts S98Z | suite | fixed in the copied reader |
| 4 | First line's columns off by one (column cache initialized to 0) | WP-33 position differential | fixed |
| 5 | `%` with an empty directive name accepted | WP-33 fuzz corpus seed `c03d1c4f150cc904` | fixed |
| 6 | Value adjacent to a plain flow key accepted (`{a:[]}`, YAML 1.2.2 production 147) | yaml.v3 differential plus shrinking | fixed |

Each oracle found something the others missed: the suite alone would have left items 4 to 6.

## 11. Effort estimate

Production (internal/config/profile):

| Part | Lines | Basis |
|---|---|---|
| Parser (new) | 1,400 to 1,800 | spike 1,156; add diag.Diagnostic with Related, MaxDiagnostics, Yield every 256 nodes, ctx checks, tree.Node building with KeyPos, position conventions, user-facing messages (the spike has 39 distinct messages, `grep -o 'p.fail("[^"]*' | sort -u | wc -l`) |
| Readers and typing (reused) | about 600 | quoted.go:19-212, literal.go:21-172, scalar.go, precheck.go, source helpers |
| Unchanged | about 880 | json.go, encode.go, profile.go (Options without goccy test hooks) |
| doc.go | about 80 | the 151-line doc today is mostly the goccy limitation list |
| Total | 3,000 to 3,400 | against 6,232 today; goccy-specific files (4,150 lines) removed |

Tests:

| Part | Lines | Basis |
|---|---|---|
| Kept with edits | about 2,800 | scalar, json, encode, precheck, quoted, literal, hostile, bench, helpers, the suite runner (switch to exact events, drop 20 reviewed entries, relist 9MMW as `profile:complex-key`) |
| Dropped or rewritten | about 2,700 | columns_test 268, chomp_test 372, split_test 213, segment_test 212, flowcost_test 102, flowentry_test 194, limits_test (TestGoccyLimitations) 72, fuzz oracles `widthIndependent` and `splitMatchesWhole` (fuzz_test.go 688), parts of yaml_test and edge_test; at least the three literal tests of section 6 assert acceptance of invalid YAML 1.2 |
| New | 1,500 to 2,000 | production tables per construct (block, compact, flow, plain folding, quoted, block scalars, tabs, markers, directives, positions and messages goldens), generator property test (gen.go is 483 lines), differential fuzz target |

Documents (same pull request rules, AGENTS.md "Sources of truth"): a new ADR superseding the library part of ADR-0003 (`docs/adr/0003-configuration-format.md:35,52,76,93-96,104,115`), the catalog row (`docs/engineering/01-tech-stack-and-libraries.md:87,145,192`), confinement row (`docs/engineering/02-repository-layout-and-conventions.md:292`), CM (`docs/architecture/02-configuration-model.md:59-61`, which also mandates "counted over lexer.Tokenize output"), foundation pack section 7 (`docs/_meta/foundation-pack.md:152,508`, two approvals), spec 01 req 8 to 10 and risks 1 to 3, 00-architecture 1.2 row (`.claude/plans/m1/specs/00-architecture.md:114`), `go.mod:9` and `.golangci.yml:35,80`.

## 12. Risk areas, with the evidence for each

| Area | Evidence of risk | Evidence of control |
|---|---|---|
| Block scalar indentation | defects 2 and 3; auto-detection, indicators with leading empty lines, trailing comments are where reused code relied on goccy | suite block-scalar cases pass with exact events; generator covers indicators and chomping; folded scalars only by the suite |
| Plain scalar folding | continuation lines starting with `-`, `#` after white space, tab-only lines with fewer spaces than the indentation (comment lines), document markers | suite; generator with multi-line plain and tabs; fuzz differential found no class beyond the known WP-33 one |
| Flow edge cases | defect 6; JSON adjacency, one-line implicit keys in sequences, empty keys and values, `?` and `:` starting plain scalars, flow line indentation | suite (4ABK, 5T43, C2DT, CT4Q, DFF7, FRK4, VJP3, 9MMW); minimization against yaml.v3 |
| Tabs | 20 suite cases (DK95/00-08, Y79Y/000-010); tabs never indent, may separate, a tab line ends a plain scalar when indented less than the scalar | all pass; fuzz classes `\n\t-`, `\t? k` |
| Error positions | conventions differ from WP-33 for empty values and explicit keys; 30 of 88 first-error positions differ from WP-33; the depth error is one column late | positions are computed from the cursor, so any convention is a local change; goldens needed |
| Strictness | YAML 1.2 rejects `key: [\n]` with `]` at column 1 under a block mapping, `"a"#c`, `{a:[]}`; libyaml-family parsers and goccy accept them (yaml.v3 probe: `a: [\n]` accepted) | a deliberate profile decision; a hand-written parser can choose, with a clear message |
| Recursion | Go recursion, bounded by MaxDepth (64) times a constant | a configurable depth far above 64 would need an explicit stack |

## 13. Is the safety net enough?

- The YAML Test Suite alone is not: WP-33 passed it with 21 reviewed entries and still had 5 confirmed findings in the fourth round; the spike passed it fully after defects 1 to 3 but still had defects 4 to 6.
- WP-33's tests alone are not: they are partly goccy-specific (section 11) and assert acceptance of invalid YAML 1.2 in at least three places (section 6).
- What made the spike converge within one session: the suite compared by exact events, the generator property test, and differential fuzzing with shrinking against two other implementations (yaml.v3 and WP-33 itself), triaged against YAML 1.2.2 productions and suite cases. Keeping WP-33's current implementation as a test-only differential oracle during the switch is not possible under depguard once goccy is removed, so the differential should run before goccy is dropped, and the generator and suite remain as the permanent net.

## 14. Maintenance cost against a library

- Library today: 4,150 production lines exist only to predict or correct goccy v1.19.2 (column shifts, scanner state across `---`, token-count cost bounds, tab rewrites); four review rounds confirmed 12, 8, 4 and 5 findings (`w3-s1a-harden-result.json` rounds 1 to 3 and `w3-s1a-harden2-result.json`), and four of the five still open are in this layer, the fifth in its fuzz harness. Every goccy release can change the token stream these corrections assume (TestGoccyLimitations exists to detect that).
- Hand-written: the target is fixed (YAML 1.2.2, 2021) and narrowed by the profile; no third-party dependency (removes goccy from go.mod, depguard and the supply-chain gate); cost and memory are properties of Ruralz code. The cost moves to owning a parser: YAML's subtle corners (section 12), message and position quality, and keeping the generator, suite runner and fuzz targets maintained.

## 15. Reproduction

```
S=/tmp/claude-0/-home-user-ruralz/0aabfe6e-34a6-51d2-9b9b-23886b703760/scratchpad/yamldecision/handwritten
cd $S/spike && go vet ./... && go build -o ../out/suite ./cmd/suite && ../out/suite -v
go run ./cmd/inputs all > ../out/inputs-all.jsonl
cd /home/user/ruralz && CGO_ENABLED=0 HWDUMP_IN=$S/out/inputs-all.jsonl HWDUMP_OUT=$S/out/wp33-all.jsonl go test -count=1 -overlay $S/overlay/overlay.json -run '^TestHWDump$' ./internal/config/profile/
cd $S/spike && go run ./cmd/diff -in ../out/inputs-all.jsonl -wp33 ../out/wp33-all.jsonl
go run ./cmd/prop -n 100000 -seed 1000000
go run ./cmd/prop -n 4000 -tabs=false -directives=false -v3log ../out/v3.tsv && go run ./cmd/minv3 ../out/v3.tsv 300
go test -run '^$' -bench BenchmarkParseYAML ./hw
cd /home/user/ruralz && CGO_ENABLED=0 go test -c -fuzz=FuzzHWvsWP33 -o $S/out/profile_hw.test -overlay $S/overlay/fuzz-overlay.json ./internal/config/profile/
cd $S/out/fuzzwd && HWFUZZ_SEEDS=$S/out/inputs-all.jsonl HWFUZZ_LOG=$S/out/hwfuzz.log $S/out/profile_hw.test -test.run '^$' -test.fuzz '^FuzzHWvsWP33$' -test.fuzztime 360s -test.parallel 2 -test.fuzzcachedir $S/out/fuzzcache
```

Go 1.27.1 (`go version`), 4 CPUs shared with other jobs, so absolute times are indicative; the ratios come from runs on the same machine minutes apart.
