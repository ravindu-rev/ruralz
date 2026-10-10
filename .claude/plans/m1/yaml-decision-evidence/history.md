# WP-33 YAML parser decision: review history and code map (role: history)

Scope: classify every confirmed finding of every WP-33 review by root cause, map the production code of
`internal/config/profile` to parser-independent profile rules versus goccy workarounds, and list the goccy
behaviors the profile compensates for, each with an example input measured on raw goccy v1.19.2.

Scratch files (all under `/tmp/claude-0/-home-user-ruralz/0aabfe6e-34a6-51d2-9b9b-23886b703760/scratchpad/yamldecision/history/`):

- `dump-confirmed.txt`, `dump-minors.txt`: the confirmed, refuted and minor findings extracted from the run files.
- `declmap/` (own scratch Go module): lists every top-level declaration of the package with its line range (`declmap/decls.tsv`).
- `codemap.tsv`: the line-range classification of every production line (K, A, W); totals below are computed from it.
- `goccyprobe/` (own scratch Go module, imports goccy v1.19.2 and go.yaml.in/yaml/v3 v3.0.5 from the module cache): `cases.out`, `extra.out`, `scan.out`, `cost.out`.

No repository file was changed. Repository state: `git status --short` empty; HEAD `985df34`; the package equals commit `086e74d`
(`git diff --stat 086e74d -- internal/config/profile` prints nothing).

## 1. The review record

Sources: `.claude/plans/m1/reports/WP-33.json`, `.claude/plans/m1/runs/w3-s1a*.json`, scripts `m1-resume.js` and `m1-harden.js`.

| Loop | Run file | What the record keeps | Serious (blocker+major) confirmed | Minor |
|---|---|---|---|---|
| L0 first review | `w3-s1a-result.json` `firstReview.counts` = `[2, 2, 4]` | counts only (`m1-resume.js:197`: `counts: ['blocker','major','minor'].map(...)`); the findings text is not saved | 4 | 4 |
| L0 re-review 1 | not saved (`maxRounds` 2; only the final review is kept, `m1-resume.js:200-201`) | nothing | unknown | unknown |
| L0 re-review 2 | `w3-s1a-result.json` `remaining` | 1 blocker (tagged empty node) | 1 | 0 |
| L1 targeted fix | `w3-s1a-fix-result.json` (`firstReview.counts` `[1,0,0]` is the saved input review, the same blocker) | `remaining` 1 blocker ('?' grouping), `remainingMinor` 1 | 1 | 1 |
| L2 lead | `w3-s1a-fix2.json` `reviews.WP-33.findings[0]` | lead blocker (depth guarantee rests on predicting goccy) | 1 | 0 |
| Harden R1 | `w3-s1a-harden-result.json` `rounds[0]` | 12 confirmed, 2 refuted | 12 | (passed to the fix, not saved) |
| Harden R2 | `rounds[1]` | 8 confirmed, 0 refuted | 8 | (not saved) |
| Harden R3 | `rounds[2]`, `remainingConfirmed` (same 4, checked by `where`), `remainingMinor` 12 | 4 confirmed, 1 refuted | 4 | 12 |
| Harden2 R1 | `w3-s1a-harden2-result.json` `rounds[0]`, `remainingConfirmed` (same 5), `remainingMinor` 15 | 5 confirmed, 0 refuted | 5 | 15 |

Command used for the counts:

```
python3 -I -c "import json; d=json.load(open('runs/w3-s1a-harden-result.json')); [print(r['round'],len(r['confirmed']),len(r['refuted'])) for r in d['rounds']]"
round 1 confirmed 12 refuted 2 / round 2 confirmed 8 refuted 0 / round 3 confirmed 4 refuted 1
harden2: round 1 confirmed 5 refuted 0; remainingMinor 12 (harden) and 15 (harden2)
```

`remainingMinor` in the harden files is the last round's merged minors plus the findings the verifier confirmed but demoted to minor
(`m1-harden.js:158-165`: `lastMinors = [...lastMinors, ...demoted]`). Harden R3: 8 merged + 4 demoted (H-m9..H-m12, original
severity major). Harden2: 9 merged + 6 demoted (H2-m10..H2-m15).

The L0 first-review findings are not recoverable from the run files. `reports/WP-33.json` deviations 12 to 19 name "Finding 1, 2, 3, 5
and 6" of a review whose fix they describe; those are reconstructed below and marked `*` (they may belong to the first review or to the
unsaved re-review 1).

## 2. Classification

Classes: (a) goccy scanner departs from YAML 1.2 (token text, type, position, line handling); (b) goccy parser structure or nesting departs
from YAML 1.2; (c) goccy cost, quadratic or worse, or a heap per token that breaks the budget; (d) Ruralz profile logic wrong on its own;
(e) a defect introduced by an earlier workaround; (f) test or fuzz harness, or documentation of a bound.

Rule for the primary class: the first place in the pipeline where behavior departs from YAML 1.2 (scanner before parser), except that (c)
is used when the input's structure is predicted correctly and only goccy's algorithm is costly, (e) when the defect would not exist without
Ruralz workaround code, (f) when the defect is in a test, harness or a doc claim. "Without goccy" says whether the finding would exist with a
conforming, linear parser: gone, indirect (lives in code or limits that exist only because of goccy), or stays.

### 2.1 Serious (blocker or major) confirmed findings

| ID | Round | Verified severity | Defect (input) | Primary | Also | Without goccy | Evidence |
|---|---|---|---|---|---|---|---|
| L0-F1* | L0 | n/a | block '-' entries inside a flow collection nest one sequence per dash outside the depth count; `[-]` accepted | b | a | gone | WP-33.json DEV 12; `tokens.go:707-713` |
| L0-F2* | L0 | n/a | tag chains: goccy recurses once per tag, outside the depth limit | b | c | gone | DEV 19; `tokens.go:296-303` |
| L0-F3* | L0 | n/a | a token after a tag is reported one column early | a | | gone | DEV 16 |
| L0-F5* | L0 | n/a | byte-based token estimate can undercount | d | | indirect (the estimate exists because goccy's Scan is not incremental, CR 0) | DEV 13, CR 0 |
| L0-F6* | L0 | n/a | `a:\n-\nb: 1` nested as {a:[{b:1}]}; result depends on mapping width | b | e | gone | DEV 14 |
| L0-R | L0 re-review 2 | blocker | `a: !!map\nb: 1` nests b under a; 712 KB input reached depth 16,066, 6.0 s, 1,679 MiB | b | c | gone | w3-s1a-result `remaining[0]` |
| L1-R | L1 | blocker | ':' completing an open '?' is grouped with the node before it; ladders reached depth 1,056 and 2,139; the exception came from the previous review | b | e | gone | w3-s1a-fix-result `remaining[0]` |
| L2-LEAD | L2 | blocker | the MaxDepth guarantee rests only on the token pass predicting goccy's nesting | b | | gone | w3-s1a-fix2.json finding 1 |
| H1-C1 | H R1 | blocker | path string per AST node: 49 KB input 298 MiB, 72 KB valid input at depth 64 263 MiB | c | | gone | dump-confirmed H-R1-C1; goccy `parser/context.go:85-95` |
| H1-C2 | H R1 | blocker | implicit null inserted by shifting the token slice: `a: {a,...}` 160 KB 4.2 s, 320 KB 34.7 s | c | | gone | H-R1-C2; `parser/context.go:151-174` |
| H1-C3 | H R1 | major | about 1,000,000 tokens in 1 MiB take 361 to 485 MiB (goccy tokens + AST + tree) | c | d | gone or reduced | H-R1-C3 |
| H1-C4 | H R1 | major | plain scalar continued by a '-' line keeps `# comment`, reported at its last line | a | | gone | H-R1-C4 |
| H1-C5 | H R1 | major | `k: "\ ` + break + `"` loses an escaped space | a | | gone | H-R1-C5; goccy `scanner.go:330-346` |
| H1-C6 | H R1 | major | tagged key after an empty value: narrow parse refuses, split parse accepts | b | e | gone | H-R1-C6 |
| H1-C7 | H R1 | major | after an empty block scalar and a blank line the next line is one plain String (`x: \|\n\n&a q: 1` key "&a q") | a | | gone | H-R1-C7 |
| H1-C8 | H R1 | major | a tag at end of input without newline is dropped (custom tag not reported) | a | | gone | H-R1-C8 |
| H1-C9 | H R1 | major | tag column skew moves the indentation base: siblings swallowed into a block scalar; valid multi-line values refused | a | | gone | H-R1-C9 |
| H1-C10 | H R1 | blocker | tabs deleted inside plain scalars (`col1\tcol2` -> `col1col2`, false duplicate keys) | a | | gone | H-R1-C10 |
| H1-C11 | H R1 | major | linearity test of chomp.go flaky (ratio limit 24 inside the noise) | f | | indirect (tests a goccy workaround) | H-R1-C11 |
| H1-C12 | H R1 | major | widthIndependent compares accept/refuse only, at one split width | f | | indirect (tests the split parse) | H-R1-C12 |
| H2-C1 | H R2 | blocker | `...` at column 1 is a document end whatever follows; content after `...` tokenized; whole-document goccy parse 22 s, 1,039 MiB | a | c, d | gone | H-R2-C1; goccy `scanner.go:1046-1062` |
| H2-C2 | H R2 | blocker | dedented line between two columns makes rangeNode hand a whole range to goccy: 875 KB 36 s, 60 GB allocated | e | c | gone | H-R2-C2 |
| H2-C3 | H R2 | blocker | second insertion site: tag default value (`[!!str ,...]`) 693 KB 9.8 s | c | | gone | H-R2-C3 |
| H2-C4 | H R2 | major | flow '?' paired with its tag alone when the tag ends the line | b | | gone | H-R2-C4 |
| H2-C5 | H R2 | major | tab in a single-line plain scalar dropped (`x: 1\t2` is int 12); round-1 fix covered multi-line only | a | | gone | H-R2-C5; goccy `scanner.go:1470-1476` |
| H2-C6 | H R2 | major | goccy columns skip tabs; the round-1 quoted reader then refused valid YAML (`{"a":\t"b"}`) | e | a | gone | H-R2-C6 ("the round-1 quoted-scalar reader introduced this regression") |
| H2-C7 | H R2 | major | trailing spaces shift a plain scalar's column right | a | | gone | H-R2-C7 |
| H2-C8 | H R2 | major | DefaultMaxDocumentTokens lowered to 400,000 against the 1,000,000 of the specs, no contract request | d | c | indirect (lowered for goccy's heap per token) | H-R2-C8 |
| H3-C1 | H R3 | major | inside a flow mapping goccy splits `app:web` at ':' | a | | gone | H-R3-C1; goccy `scanner.go:978-995` |
| H3-C2 | H R3 | blocker | flow entries without ',' read with block-mapping column logic: ladders 3,747 MiB, 80,000 entries 17.9 s | b | c | gone | H-R3-C2 |
| H3-C3 | H R3 | major | FuzzLoadYAML fails: harness reads goccy's raw column; keep-chomping rewrite moves an error position; production accepted `\t"": 1` | f | e, a | indirect | H-R3-C3 |
| H3-C4 | H R3 | blocker | converter depth guard bounds the returned tree only, docs claimed more, no cost test | f | c | indirect | H-R3-C4 |
| H4-C1 | H2 R1 | blocker | '{' on a reserved directive line keeps goccy in flow mode after '---'; '}' closes '[' through `!!map`; 304 KB 1,645 MiB | a | b, c, d | gone | H2-R1-C1 |
| H4-C2 | H2 R1 | major | null insertion moves every later token of the parse unit, past the collection: 396 KB 5.65 s | c | e | gone | H2-R1-C2 |
| H4-C3 | H2 R1 | major | untabSeparators rewrites tabs inside plain scalars (`k: a"\t:b` -> `a" :b`) | e | | gone (no rewrite needed) | H2-R1-C3 |
| H4-C4 | H2 R1 | major | FuzzLoadYAML startsWithKey scans raw text, the profile scans the untabbed text | f | e | indirect | H2-R1-C4 |
| H4-C5 | H2 R1 | major | per-file bound of 5/4 MaxDocumentTokens refuses a 2.8 MB file of 20,000 resources | d | e | indirect (added for goccy's heap per token) | H2-R1-C5 |

Refuted candidates (classified, not counted):

| ID | Defect | Class | Why refuted (verifier) |
|---|---|---|---|
| H1-X1 | '?', ':' and flow indicators scanned as plain text | a | documented goccy limitation, narrow and split agree |
| H1-X2 | value at its key's column accepted (`a:\nb`) | b | no binding requirement; later fixed as H-m10 |
| H3-X1 | `a: \|1-` at end of document refused | a | refusal required by 01 req 8 |

### 2.2 Minor findings (as recorded)

| ID | Defect | Primary | Also |
|---|---|---|---|
| L1-m | '?' paired with the next token only (`? "q"k` -> {q: k}) | b | |
| H-m1 | multi-document file near the token limit peaks 212-280 MiB | c | d |
| H-m2 | null position depends on what follows (`k1:` 1:5 alone, 1:4 before a sibling) | b | |
| H-m3 | DefaultMaxDocumentTokens 400,000 vs spec | d | c |
| H-m4 | more goccy refusals of valid YAML undocumented (`k: [? a]`, `k: [a\n  -b]`...) | b | a |
| H-m5 | suite runner accepts any finding for reviewed entries | f | |
| H-m6 | converter depth-guard mutants survive | f | |
| H-m7 | `\|--` header accepted by the source reader | e | a |
| H-m9 (demoted major) | split parse uses goccy's raw positions; trailing blank lines flip accept/refuse | a | e |
| H-m10 (demoted major) | value at key's or dash's column accepted (`k:\nv`, `-\nk: v`) | b | |
| H-m11 (demoted major) | multi-line values after a tagged key or quoted '?' key refused | a | |
| H-m12 (demoted major) | tab before a JSON-like key's ':' and tab-only lines refused | a | |
| H-m8 | no fuzz oracle for the split parse | f | |
| H2-m1 | cost comments and hostile rows miss directive and trailing-node shapes | f | |
| H2-m2 | `x0:\n  k0: !!null\ny: 1` refused by tagWithoutValue copying goccy | e | b |
| H2-m3 | `k: \|2\n # c` refused (goccy indent error) | a | |
| H2-m4 | null placed inside the key text (`? abc` null at 1:4) | e | |
| H2-m5 | tag not ended at ']' (`[!!str]` -> RZ-CFG-004 "!!str]") | a | |
| H2-m6 | wrong message for `{? a\n  b : c}` from flowKeyNode | e | a |
| H2-m7 | flowentry.go mutants survive | f | |
| H2-m8 | TestGoccyLimitations runs the token pass, cannot see a goccy change | f | |
| H2-m9 | duplicated flow-entry state; charAfter fails open | e | |
| H2-m10 (demoted major) | splitMatchesWhole oracle fails where goccy's whole parse misreads | f | b |
| H2-m11 (demoted major) | empty block scalar + blank line breaks `- run: echo` | a | |
| H2-m12 (demoted major) | strings.TrimSpace trims Unicode white space (NBSP, U+3000) | d | a (U+0085) |
| H2-m13 (demoted major) | untabTags one pass: consecutive tagged keys with tabs give RZ-CFG-004 | a | e |
| H2-m14 (demoted major) | tab before a plain key in a flow mapping refused | a | |
| H2-m15 (demoted major) | continuation lines after `!!str :` accepted at or left of the mapping column | a | e |

### 2.3 Counts per class and per round (primary class)

Serious findings:

| Round | a | b | c | d | e | f | Total | of which a+b+c (goccy behavior) | d+e+f (Ruralz code or tests) |
|---|---|---|---|---|---|---|---|---|---|
| L0 first review (8; 5 reconstructed*) | 1* | 3* | 0 | 1* | 0 | 0 | 5 of 4+4 | 4* | 1* |
| L0 re-review 2 | 0 | 1 | 0 | 0 | 0 | 0 | 1 | 1 | 0 |
| L1 | 0 | 1 | 0 | 0 | 0 | 0 | 1 | 1 | 0 |
| L2 lead | 0 | 1 | 0 | 0 | 0 | 0 | 1 | 1 | 0 |
| Harden R1 | 6 | 1 | 3 | 0 | 0 | 2 | 12 | 10 (83%) | 2 (17%) |
| Harden R2 | 3 | 1 | 1 | 1 | 2 | 0 | 8 | 5 (63%) | 3 (38%) |
| Harden R3 | 1 | 1 | 0 | 0 | 0 | 2 | 4 | 2 (50%) | 2 (50%) |
| Harden2 R1 | 1 | 0 | 1 | 1 | 1 | 1 | 5 | 2 (40%) | 3 (60%) |
| Recorded total (32, without L0*) | 11 | 6 | 5 | 2 | 3 | 5 | 32 | 22 | 10 |
| The four loops of the task (12, 8, 4, 5 = 29) | 11 | 3 | 5 | 2 | 3 | 5 | 29 | 19 | 10 |

Counting secondary classes too, findings that involve a defect of an earlier workaround (e): R1 1 of 12 (H1-C6), R2 2 of 8, R3 1 of 4,
harden2 4 of 5 (C2, C3, C4, C5).

Minor findings (28 recorded):

| Round | a | b | c | d | e | f | Total |
|---|---|---|---|---|---|---|---|
| L1 | 0 | 1 | 0 | 0 | 0 | 0 | 1 |
| Harden R3 | 3 | 3 | 1 | 1 | 1 | 3 | 12 |
| Harden2 R1 | 6 | 0 | 0 | 1 | 4 | 4 | 15 |
| Total | 9 | 4 | 1 | 2 | 5 | 7 | 28 |

All 60 recorded and classifiable findings (32 serious + 28 minor): a 20, b 10, c 6, d 4, e 8, f 12. Only one, H2-m12 (strings.TrimSpace
on Unicode white space), is a defect that would stay with any parser; every other one is goccy behavior (36 of 60) or lives in Ruralz code,
limits or tests that exist only because of goccy (23 of 60).

New goccy behaviors first reported per round by confirmed serious findings (rows of section 4): R1 10 (G1, G5, G6, G8, G9, G26, G42,
G47, G48, G52), R2 7 (G3, G4, G10, G15, G16, G36, G49), R3 4 (G18, G19, G27, G37), harden2 3 (G17, G41, G50). The minors of R3 and
harden2 add G11, G13, G14, G32, G44 and more variants. The supply is falling but did not reach zero in any round.

## 3. Production code map

Totals from `codemap.tsv` (each line in exactly one range; the script checks coverage and prints `coverage ok=True`):

- K: parser-independent profile logic with no goccy type in it; survives a parser change unchanged.
- A: logic any parser needs (token and depth limits, anchor, alias, merge-key and tag refusals, directives, conversion to the tree,
  duplicate keys, scalar keys, tag resolution, positions, error mapping) written against goccy's token and AST types; the logic survives,
  the code is re-targeted.
- W: exists only to work around goccy (behavior, positions or cost).

| File | Lines | K | A | W | What it is for |
|---|---|---|---|---|---|
| chomp.go | 338 | 0 | 0 | 338 | keep-chomping rewrite (goccy clips trailing breaks one at a time) and strip rewrite of empty keep scalars (G23, G24) |
| columns.go | 324 | 0 | 0 | 324 | column table: goccy columns after tags, tabs, trailing spaces (G2-G4) |
| doc.go | 151 | 50 | 0 | 101 | package documentation; 101 lines describe goccy shapes, rewrites, readers, split parse, cost bounds and goccy limitations |
| encode.go | 434 | 434 | 0 | 0 | Encode, EncodeWith, EncodeJSON (02 req 44-47, R-29) |
| explicit.go | 336 | 0 | 0 | 336 | '?' entries in block and flow context (G34-G36) |
| flowcost.go | 156 | 0 | 0 | 156 | path-byte and inserted-null bounds on goccy's flow parse (G47-G49) |
| flowentry.go | 154 | 0 | 0 | 154 | flow entry shape and ':' rules (G18, G19, G37) |
| json.go | 181 | 181 | 0 | 0 | JSON front end on jsonval (01 req 15) |
| literal.go | 257 | 0 | 0 | 257 | block scalars re-read from the source, header checks (G22, G25) |
| plain.go | 298 | 0 | 0 | 298 | plain scalars re-read (tabs, multi-line folding), misread refusals (G1, G6-G8, G21) |
| precheck.go | 106 | 106 | 0 | 0 | UTF-8, U+FEFF, printable set, size (01 req 7) |
| profile.go | 266 | 248 | 0 | 18 | API, codes, default limits, Options, diagnostics; W = test knobs splitAt, noRewrite, passDepth |
| quoted.go | 252 | 0 | 0 | 252 | quoted scalars re-read from the source (G5) |
| scalar.go | 318 | 318 | 0 | 0 | YAML 1.2 core schema typing, int64 range, float text (R-64), core tags (01 req 12) |
| segment.go | 171 | 0 | 0 | 171 | document cut and byte-based token estimate, because one goccy Scan call returns every token (G28) |
| source.go | 117 | 0 | 0 | 117 | source cursor and scalarFix table used by the readers |
| split.go | 406 | 0 | 0 | 406 | entry-by-entry split parse (G31, G33, G42, G47, G51) |
| tokens.go | 1020 | 24 | 172 | 824 | token pass; A = token limit, anchors/aliases/merge keys, tag refusal, directives, depth stack, per-document reset; W = everything that predicts or refuses goccy's readings |
| yaml.go | 947 | 15 | 520 | 412 | driver and converter; A = parse loop, converter (depth, keys, typing, tags); W = rewrites (untab*, trimBlankTail), estimate scan, split dispatch, bodyRange/refuseShape, null placement, source readings |
| Total | 6232 | 1376 | 692 | 4164 | |

Shares: K 1,376 (22.1%), A 692 (11.1%), W 4,164 (66.8%). Lines whose logic survives a parser change: K + A = 2,068 (33.2%).
Assumption: the replacement parses one document at a time and exposes tokens or events incrementally. If it does not (like goccy, whose
first Scan call returns all 11 tokens of a three-document stream, `scan.out`), segment.go (171) partly survives: up to 2,239 lines (35.9%).
The A share is an upper bound for the re-targeted code: the converter holds goccy-specific branches (MappingValueNode, synthesized tokens).

Command:

```
python3 -I - codemap.tsv internal/config/profile   (script in the session; prints the table above and "coverage ok=True")
TOTAL 6232 1376 692 4164
```

Test code (not classified line by line): 6,902 lines in 24 `_test.go` files. Files that test W-only code: chomp_test 372, columns_test 268,
edge_test 526, flowcost_test 102, flowentry_test 194, limits_test 72, literal_test 156, plain_test 331, quoted_test 143, segment_test 212,
source_test 39, split_test 213 (2,628 lines). Files that test K-only code: encode_test 424, json_test 128, precheck_test 130,
scalar_test 234 (916 lines). Mixed: yaml_test 1,128, fuzz_test 688, yamltestsuite_integration_test 638, hostile_test 532, helpers_test
243, bench_test 109, race/norace 20. `wc -l *_test.go`.

YAML Test Suite list (`test/fixtures/yaml-test-suite/expected-failures.txt`, 87 entries): 29 profile:anchor, 21 profile:tag,
12 profile:complex-key, 4 profile:directive, 21 reviewed:goccy-v1.19.2 (`grep -v '^#' expected-failures.txt | awk ...`).

## 4. goccy v1.19.2 behaviors the profile compensates for

Measured on raw goccy with `goccyprobe` (own module; `go.sum` hashes equal the repository's: goccy `h1:PmFC1S6h8ljIz6gMRBopkjP1TVT7xuwrButHID66PoM=`,
yaml/v3 `h1:N6y/pJk8buWs9NY5ERU2HSMfm+IuD/OtfdAnq6kESPw=`). Build and run:

```
cd goccyprobe && GOTOOLCHAIN=local GOPROXY=off GOFLAGS=-mod=mod GOSUMDB=off CGO_ENABLED=0 go build -o probe .
./probe > cases.out; ./probe extra > extra.out; ./probe scan > scan.out; ./probe cost > cost.out   (cost: real 0m10.2s)
```

"goccy" is `goccy.NewDecoder(...).Decode` per document plus `lexer.Tokenize`; "yaml.v3" is go.yaml.in/yaml/v3 v3.0.5 (libyaml port),
used as the reference reading; where libyaml itself departs from the YAML 1.2 spec this is said. Status: comp = compensated;
limit = goccy's refusal of valid YAML kept as a documented limitation; open = not compensated at the last review.

Scanner:

| # | goccy behavior | Example input | goccy (measured) | Reference | Compensation (file:line) | Status, finding |
|---|---|---|---|---|---|---|
| G1 | tab inside a plain scalar left out of the value | `x: 1\t2` | `Integer("12")`, {x: 12} | "1\t2" | plain.go:71-81, tabbedPlain 272-287 | comp; H1-C10, H2-C5 |
| G2 | column after a tag one short | `a: !!str x` | x@1:9 | real 1:10 | columns.go 15-81; tokens.go posOf 392-402 | comp; L0-F3*, H1-C9 |
| G3 | tab not counted in columns outside quotes | `kind:\tRoute` | Route@1:6 | yaml.v3 1:7 | columns.go | comp; H2-C6 |
| G4 | plain scalar placed by its end (trailing spaces) | `x: true  ` | true@1:6 | yaml.v3 1:4 | columns.go find/plainEnd 141-324 | comp; H2-C7 |
| G5 | escaped white space before a break dropped in double quotes | `k: "\ ` + LF + `"` | " " | "  " | quoted.go readQuoted 19-94, quoted 214-252 | comp; H1-C5 |
| G6 | plain scalar continued by a '-' line keeps `# c`, placed at last line | `k: x\n  -1 # c` | "x -1 # c"@2:9 | "x -1"@1:4 | plain.go foldPlain 164-270 | comp; H1-C4, H-m9 |
| G7 | lines of spaces in a multi-line plain scalar folded to one break | `k: a\n  \n  \n  c` | "a\nc" | "a\n\nc" | plain.go foldPlain (doc 26-40) | comp |
| G8 | after an empty block scalar and a blank line, next line's node scanned as one plain String | `x: \|\n\n&a q: 1` | key "&a q", no anchor | {q: 1} with anchor | plain.go:83-131 misread refusals | comp (refuse); H1-C7; `- run` variant open H2-m11 |
| G9 | tag at end of input without final break dropped | `a: !!binary` | no Tag token, {a: null} | "" (tagged) | yaml.go:46-53 appends LF | comp; H1-C8 |
| G10 | tag not ended at a tab | `a: !!str\tx` | Tag("!!str\tx"), {a: null} | "x" | yaml.go untabTags 214-245, rescan 141-145 | comp; H2-C6; variants open H2-m13 |
| G11 | tag not ended at a flow indicator | `k: [!!str]` | Tag("!!str]"), error | error (libyaml); YAML 1.2 [""] | none (RZ-CFG-004 misreport) | open; H2-m5 |
| G12 | double-quoted scalar holding a tab: scanner steps past its end | `a: "x\ty"\nb: 1` | b@1:10, "map key-value is pre-defined" | {a: "x\ty", b: 1} | yaml.go untabQuotes 149-212 | comp; harness H4-C4 (`"\t0"#:` read as key) |
| G13 | tab between a quoted key and ':' refused | `{"a"\t: 1}` | Invalid token, error | {a: 1} | yaml.go untabSeparators 267-308 | comp; H-m12; caused H4-C3 |
| G14 | tab before a plain key in a flow mapping refused | `k: {a: b,\tc: d}` | "tab character cannot use as a map key directly" | {k: {a: b, c: d}} | none | limit/open; H2-m14 |
| G15 | `...` at column 1 ends the document whatever follows | `a: 1\nb: 2\n...x: 1` | 2 docs | 1 doc, key "...x" | tokens.go:253-262; segment.go markerLine 17-28 | comp; H2-C1 |
| G16 | tokens after a `...` marker start a document | `a: 1\n... {}` | 2 docs {a:1};;{} | error on 2nd | tokens.go:215-223; yaml.go bodyRange/refuseShape 439-513 | comp; H2-C1 |
| G17 | flow counters not reset at `---`: '{' on a directive line | `%x {\n---\nk:v` | {k: "v"} | error (YAML 1.2: scalar root) | none | open; H4-C1 |
| G18 | ':' + non-space splits a plain scalar while a flow mapping is open | `labels: {app:web, tier: db}` | {app: web} | {"app:web": null} | flowentry.go flowShapeColon 80-104 | comp (refuse); H3-C1 |
| G19 | ':' before a flow indicator kept in the scalar outside a flow mapping | `k: [a:]` | ["a:"] | libyaml ["a:"] too; YAML 1.2 [{a: null}] | flowentry.go flowColonEnd 134-154 | comp (refuse); H3-C1 |
| G20 | lone '-' before a flow indicator is a plain scalar | `[-]` | ["-"] | libyaml ["-"] too; suite YJV2 is an error | tokens.go:806-813 | comp (refuse); L0-F1* |
| G21 | lone '?' scanned as the plain scalar "?" | `{\n?\n}` | {"?": null} | {null: null} | plain.go:85-93 | comp (refuse); H1-X1, H-m5 (DFF7) |
| G22 | block scalar's trailing spaces dropped at end of input | `k: \|\n  a  ` (no LF) | "a" | "a  " | yaml.go:46-53; literal.go readBlockScalar 47-143 | comp |
| G23 | clipping block scalar trims breaks one at a time | `a: \|\n  x\n` + N LF + `b: 1` | tokenize N=4000 92 ms, 8000 590 ms, 16000 1.387 s | | chomp.go keepChomping 43-134, cutBlankRuns 182-238 | comp; CR 3 |
| G24 | empty keep scalar before content refused | `a: \|+\n\nb: 1` | "could not find multi-line content" | {a: "\n", b: 1} | chomp.go stripEmptyKeep 240-326 | comp |
| G25 | indentation indicator + blank lines at end refused | `a: \|2\n  x\n\n` | Invalid "invalid number of indent" | {a: "x\n"} | yaml.go trimBlankTail 247-265 | comp; H3-X1; comment variant open H2-m3 |
| G26 | continuation indent measured from the tag-shifted key column | `!!str k: x\n  y`; `x:\n  !!str : \|\n  k: v\n  z: 1`; `x:\n  !!str : b\n  c` | error; {"": "k: v\nz: 1\n"}; {"": "b c"} | {k: "x y"}; {"": "", k: v, z: 1}; error | tokens.go 411-551, literal.go checks (refuse) | partial: limit H-m11, open H2-m15; H1-C9 |
| G27 | tab before a quoted block key accepted | `\t"": 1` | {"": 1} | error | tokens.go tabIndented 826-847 | comp; H3-C3 |
| G28 | one Scan call returns every remaining token (not incremental) | `a: 1\n---\nb: 2\n---\nc: 3` | first Scan: 11 of 11 tokens | | segment.go 37-171; yaml.go scan 98-148 | comp; CR 0 |

Parser:

| # | goccy behavior | Example input | goccy (measured) | Reference | Compensation | Status, finding |
|---|---|---|---|---|---|---|
| G30 | same-column sibling nested under a tagged empty value | `a: !!map\nb: 1` | {a: {b: 1}} | {a: "", b: 1} | tokens.go tagWithoutValue 427-476 | comp (refuse); L0-R |
| G31 | key nested into an empty last sequence entry | `a:\n-\nb: 1` | {a: [{b: 1}]} | {a: [null], b: 1} | tokens.go block 909-918, 938-945; split.go | comp; L0-F6* |
| G32 | mapping at a sequence's column nested into the last entry | `-\nk: v` | [{k: v}] | error | tokens.go:928-937 (msgSeqMap) | comp (refuse); H-m10 |
| G33 | value on a later line at its key's column accepted | `k:\nv` | {k: "v"} | error | split.go rangeNode 103-114 | comp (refuse); H1-X2, H-m10 |
| G34 | ':' with no key on its line grouped with the node before it | `? "a"\n  - "b"\n:` | {a: [{b: null}]} | error | tokens.go:757-790; explicit.go | comp (refuse); L1-R |
| G35 | '?' paired with the next token only | `? "q"k` | {q: "k"} | error | explicit.go | comp (refuse); L1-m |
| G36 | flow '?' paired with its tag alone when the tag ends the line | `k: {? !!str\n  x}` | {k: {"": "x"}} | {k: {x: null}} | explicit.go flowKeyTag 315-336 | comp (refuse); H2-C4 |
| G37 | flow entries without ',' read by block-mapping column logic | `x: [\n a: 1\n b: 2\n]` | {x: [{a: 1, b: 2}]} | error | flowentry.go 23-78 | comp (refuse); H3-C2 |
| G38 | block sequence entries inside a flow collection nested | `a: [- - x]` | {a: [[["x"]]]} | error | tokens.go:707-713 | comp (refuse); L0-F1* |
| G39 | two tags on one node accepted (one recursion per tag) | `a: !!str !!str x` | {a: "x"} | error | tokens.go:296-303 | comp (refuse); L0-F2* |
| G40 | tag followed on its line by '-' tags a block collection | `k: !!seq - a` | {k: ["a"]} | error | tokens.go tagOnBlockLine 513-535 | comp (refuse) |
| G41 | `!!map` in a flow sequence: parseMap takes '}' as its end | `%x {{{\n---\n[!!map\n a: b\n }\n,...]]]` | [{a: b}, [{a: b}, [{a: b}]]] | error | none | open; H4-C1 |
| G42 | tagged key after an empty value refused | `a:\n  b:\n!!str c: 1` | "tag is not allowed in this context" | {a: {b: null}, c: 1} | split parse accepts; tokens.go:909-918 | comp; H1-C6 |
| G43 | tagged empty value before a sibling refused | `a: !!str\nb: 1` | "unexpected scalar value" | {a: "", b: 1} | kept refused; tagWithoutValue, split.go | limit; DEV 20, H2-m2 |
| G44 | implicit null's position depends on what follows | `k1:` / `k1:\nw0: v` | null@1:5 / null@1:4 | | yaml.go placeNull 710-734; split.go empty 152-161 | comp; H-m2, H2-m4 |
| G45 | content token made up for a tag without content | `a: !!str` | per the yaml.go:937-940 comment (not probed) | | yaml.go synthesized 937-947 | comp (code only) |
| G46 | directives before `---` returned as a document of their own | `%YAML 1.2\n---\na: 1` | per the yaml.go:421-425 comment (not probed) | | yaml.go:421-425 | comp (code only) |

Cost (`cost.out`; live = heap after the parse with tokens and AST kept alive):

| # | goccy behavior | Example input | Measured | Compensation | Finding |
|---|---|---|---|---|---|
| G47 | full path string stored on every AST node (`parser/context.go:85-95`) | n-char key over an n-item flow sequence | n=2048 6 MiB alloc; 4096 23 MiB; 8192 83 MiB (x3.6-3.8 per doubling) | split.go; flowcost.go maxFlowPath | H1-C1 |
| G48 | implicit null inserted by shifting the token slice (`parser/context.go:151-174`) | `a: {` + `a,` x n + `a}` | n=20000 101 ms; 40000 304 ms (H1-C2: 160000 34.7 s) | flowcost.go maxFlowNullShift | H1-C2 |
| G49 | tag default value inserted the same way | `a: [` + `!!str ,` x n + `x]` | n=20000 90 ms; 40000 347 ms | tokens.go:327-336 | H2-C3 |
| G50 | insertion moves every later token of the parse unit | `k: {a,...} [1,...]` | H4-C2: 8000 nulls + 190000 items 5.65 s | none | H4-C2 open |
| G51 | block mapping parsed by one recursion per entry, copying entries | n lines `kI: v` | n=2500 29 MiB/28 ms; 5000 112 MiB/100 ms; 10000 427 MiB/424 ms | split.go (defaultSplitAt = 1) | CR 3 |
| G52 | heap per token for tokens + AST | `a: [` + `1,` x n + `1]` | n=100000 66 MiB live (349 B/token); 200000 96 MiB (254 B/token) | DefaultMaxDocumentTokens 400,000 (profile.go:32-41), maxFileTokens (tokens.go:15-27) | H1-C3, H2-C8, H4-C5 |

Not reproduced: doc.go's general claim that goccy "miscounts empty lines" of block scalars; nine simple block-scalar shapes (B1-B9 in
`extra.out`) agree with yaml.v3. The divergences found are at end of input (G22), after empty scalars (G8, G24, G25) and after tags (G26).
Defensive only: panic recovery around goccy (yaml.go:312-319, split.go parseBody 380-406); no panic was observed in this research.

## 5. Notes and limits of this evidence

- The L0 first-review findings exist only as counts; five are reconstructed from WP-33.json deviations and may include re-review 1 findings.
- Classification is judgment applied to each finding's own text and evidence; the primary-class rule is stated in section 2.
- The A/W boundary inside tokens.go and yaml.go is drawn at function or block level; `codemap.tsv` lists every range so it can be re-checked.
- yaml.v3 (libyaml) is a reference, not the YAML 1.2 spec; G19 and G20 show libyaml shares two goccy readings that the profile refuses.
