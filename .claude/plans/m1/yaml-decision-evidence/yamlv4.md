# go.yaml.in/yaml/v4 as the YAML front end of the restricted profile: prototype and measurements

Role: yamlv4. Scratch root: `S=/tmp/claude-0/-home-user-ruralz/0aabfe6e-34a6-51d2-9b9b-23886b703760/scratchpad/yamldecision/yamlv4`.
No repository file was changed: `git -C /home/user/ruralz status --short` printed nothing after every step (last checked after the patched fuzz run). Toolchain: `go version go1.27.1 linux/amd64` (repo `go.mod` toolchain), plus `go1.26.0` for the floor check. Host: 4 CPUs, 16 GB RAM, shared.

## 0. Bottom line

- **Version.** The latest release is `v4.0.0-rc.6` (2026-06-17), a **release candidate**; the proxy lists only `rc.1` to `rc.6`, no stable `v4.0.0`.
- **Correctness.** A ~640-line front end over v4's `yaml.Node` gives the YAML 1.2 result for 216 of the 224 concrete inputs taken from the WP-33 findings and the 01 test plan. That covers every parser-behavior shape quoted in the confirmed findings of the w3-s1a, fix, harden (3 rounds) and harden2 runs. The 8 rows that still differ are listed in section 3.
  - Some confirmed findings concern only WP-33's own harness, tests or constants: harden R1#10 and R1#11, R2#7, R3#2 and R3#3, and harden2 B4 and major. They have no parser input of their own; their inputs that do exist are in the rows.
  - The harden2 major shape (20,000 Route documents in one file) is measured in section 4.1 and accepted.
- **YAML Test Suite.** The suite gives **279 pass / 123 fail** (WP-33: 315 / 87). The 66 profile restrictions match exactly, and v4 fixes 2 of WP-33's 21 reviewed goccy cases. The other 57 failures break down as follows:
  - 42 valid streams refused, of which 13 involve tabs;
  - 12 invalid streams accepted;
  - 3 accepted with a different value.
- **Cost.** Every WP-33 hostile row stays within 1.2 s and 206 MiB. The goccy-specific shapes are refused at the first offending token:
  - the ladders and comma-less entries in 2 to 45 ms;
  - the marker and dedent rows in up to 217 ms, when 1 MiB of valid entries comes first.
- **Blocker (cost).** v4 rc.4 to rc.6 have a **token-retention regression**. A flow collection that starts where an implicit key could start keeps every token in the scanner queue until it closes. Such positions are the document root, a `- ` entry, a value on its own line, or an item of a flow sequence. This costs about 500 bytes of heap per input byte:
  - 1 MiB `[1,1,...]` at the root: 498 to 502 MiB peak, 0.63 to 3.9 s across runs;
  - a 4 MiB JSON-style root mapping: 772 MiB.
  
  The cause is the dropped libyaml stale-simple-key rule. A two-condition patch restores linear cost (128 MiB, 0.45 s), with identical suite and row results.

## 1. Version, license, dependencies, Go version

Commands and outputs:

```
$ curl -sS https://proxy.golang.org/go.yaml.in/yaml/v4/@v/list
v4.0.0-rc.6
v4.0.0-rc.3
v4.0.0-rc.2
v4.0.0-rc.4
v4.0.0-rc.1
v4.0.0-rc.5
$ curl -sS https://proxy.golang.org/go.yaml.in/yaml/v4/@latest
{"Version":"v4.0.0-rc.6","Time":"2026-06-17T13:09:13Z","Origin":{"VCS":"git","URL":"https://github.com/yaml/go-yaml","Hash":"1c17b9cee72bf81ae73a8be50fbd7dd3a9985125","Ref":"refs/tags/v4.0.0-rc.6"}}
$ (cd $S/proto && go get go.yaml.in/yaml/v4@v4.0.0-rc.6 gopkg.in/yaml.v3@v3.0.1 && go mod verify)
all modules verified
go.sum: go.yaml.in/yaml/v4 v4.0.0-rc.6 h1:1h7H1ohdUh93/FyE4YaDa1Zh64K6VVbjF4K6WUxMtH4=
```

Release candidate dates from `https://proxy.golang.org/go.yaml.in/yaml/v4/@v/<v>.info`:

| Version | Date |
|---|---|
| rc.1 | 2025-07-30 |
| rc.2 | 2025-08-28 |
| rc.3 | 2025-11-04 |
| rc.4 | 2026-01-21 |
| rc.5 | 2026-06-08 |
| rc.6 | 2026-06-17 |

- **API churn between candidates.** rc.3 moved the code to `internal/libyaml`. rc.5 added `plugin/limit` and `yaml.LimitPlugin`. rc.4 replaced the simple-key logic (section 4.3).
- **License.** `$V4/LICENSE` is Apache-2.0. `$V4/NOTICE` states that 8 files ported from libyaml stay MIT (Kirill Simonov), namely `internal/libyaml/{api,emitter,parser,reader,scanner,writer,yaml,yamlprivate}.go`, and the rest is Apache-2.0 ("Copyright 2011-2019 Canonical Ltd, Copyright 2025 The go-yaml Project Contributors"). `$V4 = /root/go/pkg/mod/go.yaml.in/yaml/v4@v4.0.0-rc.6`. Both licenses are on gate G2 (`docs/engineering/01-tech-stack-and-libraries.md:37`).
- **go.mod.** The whole file is `module go.yaml.in/yaml/v4` and `go 1.18`, with no `require`. In the scratch module, `go mod graph` shows v4 with no edges.
- **Go floor (S2).** The following ran with go1.26.0 and passed (the test run also covers the fuzz seeds):

  ```
  GOTOOLCHAIN=go1.26.0 CGO_ENABLED=0 go vet ./...
  GOTOOLCHAIN=go1.26.0 CGO_ENABLED=0 go test -run 'TestFindingRows|TestYAMLTestSuite|FuzzParse' ./profile
  ```

  Output: `ok example.com/yamlv4proto/profile`, `go version go1.26.0 linux/amd64`, and "v4 builds with go1.26.0".
- **Repository graph today.** `go.yaml.in/yaml/v3 v3.0.5 // indirect` is already in the repo `go.mod` (`go.mod:92`), pulled in by cel-go, grpc-gateway, jwx, prometheus/common, rueidis, testify and otelslog (`go mod graph`). It is not linked: `go list -deps ./...` lists only `github.com/goccy/go-yaml/*`. v4 would be a new module.
- **Binary size.** Stripped `CGO_ENABLED=0` builds (`$S/size`):

  | Program | Bytes |
  |---|---|
  | empty program | 1,675,426 |
  | v4 `NewLoader` + `yaml.Node` | 2,498,722 (+823 KB) |
  | goccy `parser.ParseBytes` | 2,183,330 (+508 KB) |

  The `yaml` package links the reflection constructor, representer and emitter even when only `yaml.Node` is used.
- **Other properties.**
  - v4 has `init()` functions at `internal/libyaml/resolver.go:217`, `structmeta.go:74` and `node.go:42`. These are allowed in dependencies.
  - Production code is 13,338 lines in `internal/libyaml` (`wc -l`).
  - There is **no public token or event API**: `libyaml.ParserGetEvents` is internal. `go doc go.yaml.in/yaml/v4` lists Load, Unmarshal, Decoder, Loader, Node, Dump, Marshal, Encoder, Dumper, the options, the error types and `LimitPlugin`, and no token or event type.
  - There is **no `context.Context`** (criterion S3), so a front end can cancel only between documents.
  - The only limit hooks are `LimitPlugin.CheckDepth` and `CheckAlias` (criterion S4). There is no memory cap and no token cap.

## 2. The prototype (`$S/proto`)

Module `example.com/yamlv4proto`, `go 1.26.0`, requires `go.yaml.in/yaml/v4 v4.0.0-rc.6` (and `gopkg.in/yaml.v3 v3.0.1` for tests only).

| File | Lines | Non-blank, non-comment lines | Role |
|---|---|---|---|
| `profile/profile.go` | 168 | 114 | `Parse(src, Options) ([]Document, []Diag)`. Runs the size limit, the pre-checks, the directive pass, one `yaml.Loader` per file with `yaml.WithPlugin(depthLimit{MaxDepth})` and one `Load(*yaml.Node)` per document. Turns v4 internal panics into RZ-CFG-001. Maps `*yaml.LoadError` to RZ-CFG-001 at `Mark`, and "unknown anchor" to RZ-CFG-003. Refuses U+0085, U+2028 and U+2029. |
| `profile/walk.go` | 226 | 196 | One pass over the `yaml.Node` tree: anchors, aliases and plain `<<` keys give RZ-CFG-003; non-core tags, `!` and verbatim `!<...>` give RZ-CFG-004; depth over 64 and non-scalar keys give RZ-CFG-001; duplicates give RZ-CFG-002 with `Related`. Scalars are typed with Ruralz `scalar.go`. Key text is as written. Findings are capped at 100. v4 subtrees are released after conversion. |
| `profile/directive.go` | 99 | 73 | Prologue scan. `%YAML 1.2` once is rewritten to `#YAML 1.2` (same length). Another version or a duplicate gives RZ-CFG-001, `%TAG` gives RZ-CFG-004, and a directive without `---` gives RZ-CFG-001. |
| `profile/source.go` | 144 | 121 | Line index (LF, CR LF, CR), position to offset mapping in O(1) per lookup, and the 01 req 7 byte pre-checks. |
| `profile/scalar.go` | 318 | 258 | **Unchanged copy** of `internal/config/profile/scalar.go` (YAML 1.2 core typing, parser-independent). |
| `profile/tree.go` | 104 | 80 | Stand-ins for `tree.Node` and `diag`; not needed in a real implementation. |

The YAML front end proper is about 637 lines (504 code lines), plus the reused `scalar.go`. WP-33 has 6,232 production lines, 5,299 without `json.go`, `encode.go` and `scalar.go` (`wc -l internal/config/profile/*.go` without tests). The prototype does not implement:

- ctx/Yield;
- `MaxDocumentTokens`, since v4 exposes no tokens;
- the `diag` and `tree` integration;
- the JSON front end and `Encode`, which would stay as they are.

Test files: `yts_test.go`, `probe_test.go`, `hostile_test.go`, `pos_test.go`, `misc_test.go`, `tabs*_test.go`, `uniq_test.go`, `diff_test.go` and `fuzz_test.go`. Tools: `cmd/dump` (raw v4 nodes) and `cmd/v4only` (v4 alone, or v3, with peak heap). `$S/proto-patched` is the same module with `replace go.yaml.in/yaml/v4 => ../patched/v4` (section 4.3).

## 3. Concrete inputs from the WP-33 findings (rows, ladders, hostile small shapes)

Command: `cd $S/proto && CGO_ENABLED=0 go test -count=1 -run TestFindingRows -v ./profile` (output in `$S/notes/rows-final.txt`).

Each row is an input quoted in `.claude/plans/m1/runs/w3-s1a-*.json` (rounds 1 to 3 of harden, harden2, and the earlier blockers), in the WP-33 report deviations, or in the 01 test plan cases. `want` is the YAML 1.2 result the finding states, where it gives a yaml.v3, PyYAML or "YAML 1.2 gives" reference. Otherwise it is my reading of YAML 1.2.

Exception: the "harden2 B1" rows with a reserved directive want a refusal. That is the profile decision proposed as fix (a), and v4 refuses every reserved directive. YAML 1.2 itself ignores reserved directives.

Result: **TOTAL 224 rows, 8 differ.**

| Group | As YAML 1.2 | Differ |
|---|---|---|
| R1#3 plain continuation with '-' | 11 | 0 |
| R1#4 escaped space before a break | 1 | 0 |
| R1#5 tagged key after an empty value (width and order dependence) | 13 | 0 |
| R1#6 after an empty block scalar and a blank line | 9 | 0 |
| R1#7 tag at the end of the input | 6 | 0 |
| R1#8 tag on a key before a block scalar | 9 | 0 |
| R1#9 / R2#4 tabs inside plain scalars | 13 | 0 |
| R2#0 document markers (`...x`, `...#`, `... {}`) | 9 | 1 |
| R2#1 dedented entry | 2 | 0 |
| R2#2 tagged empty nodes in flow | 3 | 0 |
| R2#3 `?` with a tag in flow | 7 | 0 |
| R2#5 tabs before scalars, positions after tabs | 16 | 0 |
| R2#6 trailing white space positions | 7 | 0 |
| R3#0 `:` inside flow plain scalars (`{app:web}`, `[10:30]`) | 24 | 0 |
| R3#1 flow entries without `,` | 7 | 0 |
| R3#2 harden2 fuzz shapes | 2 | 2 |
| harden2 B1 reserved directive with flow indicators | 7 | 0 |
| harden2 B2 node after a flow collection | 2 | 0 |
| harden2 B3 tab after a quote character | 6 | 0 |
| w3-s1a blocker: tag before a sibling (`a: !!map\nb: 1`) | 10 | 0 |
| w3-s1a-fix blocker: explicit keys over lines | 13 | 0 |
| WP-33 deviations | 4 | 2 |
| 01 test plan scalars | 9 | 2 |
| 01 test plan RZ-CFG-001 | 11 | 1 |
| 01 test plan RZ-CFG-002/003/004 | 15 | 0 |

The 8 differences (verbatim output):

```
DIFF R2#0 "a: 1\n... # c\nb: 2\n"            want {"a":i:1} | {"b":i:2}
      got ERR RZ-CFG-001@3:1 syntax error: did not find expected <document start>   (v4 refuses a bare document after "..."; suite 7Z25, M7A3)
DIFF R3#2 "k: |\n x\n\n\n\n\n\n\n\n\n0\n 0\n"  want ERR RZ-CFG-001@11:
      got ERR RZ-CFG-001@13:1 ... could not find expected ':'   (refused; v4's ContextMark is L11.C1, the prototype reports Mark)
DIFF R3#2 "\"\t0\"#:"                          want ERR   got "\t0"   (comment without white space accepted; suite SU5Z class; only with AnyRoot, production gives RZ-CFG-005)
DIFF "- [-, -]\n"                              want ERR   got [["-","-"]]   (suite G5U8)
DIFF "[-]\n"                                   want ERR   got ["-"]         (suite YJV2)
DIFF "a: \"\\/\"\n"                            want {"a":"/"}  got ERR RZ-CFG-001@1:5 found unknown escape character   (YAML 1.2 "\/" escape; suite 3UYS)
DIFF "a: \"\\ud83d\\ude00\"\n"                  want {"a":"😀"} got ERR RZ-CFG-001@1:7 found invalid Unicode character escape code   (surrogate escapes refused; arguably correct YAML 1.2)
DIFF "a: |\n  x\n   "                          want {"a":"x\n \n"} got {"a":"x\n "}   (last line of spaces with no final break; suite L24T/01, JEF9/02)
```

Notable rows that pass, all as YAML 1.2:

- `labels: {app:web, tier: db}` gives `{"labels":{"app:web":null,"tier":"db"}}`, and `k: {x: [10:30, 11:00]}` gives `["10:30","11:00"]`.
- Tabs are kept: `x: 1\t2` gives `"1\t2"`, `a\tb: 1\nab: 2` gives two keys, and `k: a"\t:b` gives `"a\"\t:b"`.
- `a: !!map\nb: 1` gives RZ-CFG-001 ("value "" does not match tag !!map"), and `a: !!seq\n- x` gives `["x"]`.
- `x: |\n\n&a q: 1` gives RZ-CFG-003, and `x: >-\n\n!e x: 1` gives RZ-CFG-004.
- `spec:\n  x: !env` with no final newline gives RZ-CFG-004@6:6.
- `x:\n  !!str : |\n  k: !include other.yaml` gives RZ-CFG-004@3:6.
- `0:\n-\n1:` gives `{"0":[null],"1":null}`.
- The comma-less flow entries `x: [\n a: 1\n b: 2\n]` are refused, as are the explicit-key ladders.
- `a:\t&x 1` gives RZ-CFG-003@1:4, and `a:\t.inf` gives RZ-CFG-005@1:4.
- Positions are right after tabs (`kind:\tRoute` value at 1:7, `a: [x,\ty]` y at 1:8) and after trailing blanks (`x: abc   ` at 1:4).

## 4. Hostile inputs: time, peak heap, depth

The measure is `peakHeap` from `internal/config/profile/helpers_test.go:194`, copied: the largest growth of `/memory/classes/heap/objects:bytes` plus `/memory/classes/heap/stacks:bytes`, sampled every 1 ms. Generators are copied from the WP-33 tests. Parse uses default options (MaxDepth 64, MaxBytes 64 MiB).

### 4.1 Every row of WP-33 TestHostileInputsBounded, plus the round-specific shapes (inputs up to 1 MiB, 4 MB for one ladder)

The prototype command:

```
cd $S/proto && go test -c -o ../bin/profile.test ./profile
cd $S/notes && ../bin/profile.test -test.run '^TestHostileWP33$' -test.v
```

The WP-33 column comes from running the unmodified repository test:

```
cd /home/user/ruralz && CGO_ENABLED=0 go test -count=1 -run '^TestHostileInputsBounded$' -v ./internal/config/profile/
```

Its output is in `$S/notes/wp33-hostile.txt` and it printed `ok ... 11.536s`. WP-33 refuses dense documents with a byte-based token estimate (`DefaultMaxDocumentTokens = 400_000`, `profile.go:41`) and the prototype has no such cap, so some rows are accepted by one and refused by the other.

| shape | bytes | v4 proto time | v4 proto peak | v4 proto first finding | WP-33 time | WP-33 peak |
|---|---|---|---|---|---|---|
| billion laughs | 917577 | 115ms | 24.0 MiB | RZ-CFG-003@1:4 anchors are not allowed (&a) | 203ms | 91 MiB |
| billion laughs past the token limit | 1048617 | 120ms | 27.8 MiB | RZ-CFG-003@1:4 anchors are not allowed (&a) | 23ms | 2 MiB |
| merge keys | 524277 | 140ms | 34.6 MiB | RZ-CFG-003@1:3 merge keys are not allowed (<<) | 123ms | 88 MiB |
| brackets | 1048576 | 5ms | 0.0 MiB | RZ-CFG-001@1:65 syntax error: while increasing flow level: nesting dep | 20ms | 3 MiB |
| flow sequence | 1048575 | 433ms | 139.5 MiB | accepted | 15ms | 2 MiB |
| block sequence | 799996 | 171ms | 59.4 MiB | RZ-CFG-005@1:1 a resource must be a mapping | 522ms | 168 MiB |
| block sequence past the token limit | 1048576 | 226ms | 70.9 MiB | RZ-CFG-005@1:1 a resource must be a mapping | 37ms | 2 MiB |
| one-character scalars at the token limit | 400001 | 151ms | 51.1 MiB | accepted | 467ms | 165 MiB |
| empty flow collections at the token limit | 400000 | 145ms | 30.0 MiB | accepted | 374ms | 167 MiB |
| single-pair mappings at the token limit | 499997 | 232ms | 71.1 MiB | accepted | 574ms | 189 MiB |
| empty entries at the token limit | 799995 | 205ms | 104.4 MiB | accepted | 287ms | 169 MiB |
| one-character scalars past the token limit | 999987 | 407ms | 124.5 MiB | accepted | 28ms | 1 MiB |
| long key over a flow sequence | 49157 | 16ms | 4.5 MiB | accepted | 26ms | 20 MiB |
| long keys nested over a flow sequence | 72276 | 5ms | 1.6 MiB | accepted | 15ms | 5 MiB |
| long keys nested over a long flow sequence | 96852 | 17ms | 4.5 MiB | accepted | 27ms | 20 MiB |
| long keys in flow mappings | 93015 | 15ms | 4.7 MiB | accepted | 27ms | 8 MiB |
| keys without values | 800006 | 415ms | 183.7 MiB | RZ-CFG-002@1:7 duplicate key "a" | 17ms | 2 MiB |
| keys and colons without values | 1200007 | 483ms | 180.9 MiB | RZ-CFG-002@1:8 duplicate key "k" | 9ms | 3 MiB |
| keys without values under the token limit | 300006 | 138ms | 72.2 MiB | RZ-CFG-002@1:7 duplicate key "a" | 186ms | 71 MiB |
| keys and colons without values under the token limit | 300007 | 125ms | 43.7 MiB | RZ-CFG-002@1:8 duplicate key "k" | 204ms | 83 MiB |
| wide mapping | 1048581 | 204ms | 50.8 MiB | accepted | 476ms | 169 MiB |
| documents | 1048581 | 407ms | 52.4 MiB | accepted | 873ms | 103 MiB |
| long scalar | 1048580 | 38ms | 3.1 MiB | accepted | 63ms | 42 MiB |
| deep block | 1053056 | 61ms | 10.1 MiB | RZ-CFG-001@65:132 syntax error: while increasing indent level: nesting | 20ms | 2 MiB |
| comments | 1048576 | 57ms | 8.7 MiB | accepted | 9ms | 2 MiB |
| dashes in a flow sequence | 1048583 | 6ms | 0.0 MiB | RZ-CFG-001@1:5 syntax error: while parsing a flow node: did not find e | 9ms | 2 MiB |
| dashes in a flow mapping | 1048586 | 5ms | 0.0 MiB | RZ-CFG-001@1:8 syntax error: while parsing a flow node: did not find e | 9ms | 2 MiB |
| tag chain | 1048577 | 6ms | 0.0 MiB | RZ-CFG-001@1:10 syntax error: while parsing a block mapping: did not f | 13ms | 2 MiB |
| tagged block scalar | 1048608 | 101ms | 13.8 MiB | accepted | 341ms | 72 MiB |
| tagged block scalar entry | 1048606 | 43ms | 12.5 MiB | RZ-CFG-005@1:1 a resource must be a mapping | 344ms | 55 MiB |
| block scalar before a comment | 1048606 | 56ms | 12.5 MiB | accepted | 382ms | 60 MiB |
| block scalar entry before a comment | 1048601 | 74ms | 12.5 MiB | RZ-CFG-005@1:1 a resource must be a mapping | 365ms | 55 MiB |
| tagged empty mappings | 1048576 | 45ms | 6.9 MiB | RZ-CFG-001@1:25 value "" does not match tag !!map | 68ms | 24 MiB |
| implicit keys over two lines | 1038101 | 8ms | 0.0 MiB | RZ-CFG-001@3:1 syntax error: while parsing a block mapping: did not fi | 36ms | 9 MiB |
| explicit keys over several lines | 100640 | 3ms | 0.0 MiB | RZ-CFG-001@2:3 syntax error: while parsing a block mapping: did not fi | 5ms | 0 MiB |
| explicit keys of varying width | 515468 | 3ms | 0.0 MiB | RZ-CFG-001@2:3 syntax error: while parsing a block mapping: did not fi | 15ms | 4 MiB |
| wide mapping before "...x" | 1048573 | 218ms | 55.4 MiB | accepted | 227ms | 88 MiB |
| wide mapping before "... x" | 1048574 | 161ms | 42.7 MiB | RZ-CFG-001@105426:6 syntax error: mapping values are not allowed in th | 217ms | 91 MiB |
| "...#" before a wide mapping | 1048586 | 10ms | 2.5 MiB | RZ-CFG-001@2:3 syntax error: mapping values are not allowed in this co | 190ms | 84 MiB |
| "...!!map" before a wide mapping | 1048590 | 12ms | 2.5 MiB | RZ-CFG-001@2:3 syntax error: mapping values are not allowed in this co | 180ms | 73 MiB |
| wide mapping before "... {}" | 1048572 | 217ms | 56.2 MiB | RZ-CFG-001@105426:5 syntax error: did not find expected <document star | 275ms | 85 MiB |
| long keys nested before "...x" | 96860 | 15ms | 4.7 MiB | accepted | 34ms | 8 MiB |
| "...#" before long keys nested | 96857 | 1ms | 0.0 MiB | RZ-CFG-001@2:1001 syntax error: mapping values are not allowed in this | 24ms | 8 MiB |
| "...#" before a long key flow | 49162 | 2ms | 0.0 MiB | RZ-CFG-001@2:16385 syntax error: mapping values are not allowed in thi | 12ms | 7 MiB |
| empty entries before "...x" | 50008 | 22ms | 6.4 MiB | RZ-CFG-002@3:1 duplicate key "k" | 21ms | 9 MiB |
| dedented entry after a wide mapping | 875009 | 170ms | 46.8 MiB | RZ-CFG-001@125002:2 syntax error: while parsing a block mapping: did n | 212ms | 90 MiB |
| dedented entry after empty values | 200009 | 39ms | 14.6 MiB | RZ-CFG-001@40002:2 syntax error: while parsing a block mapping: did no | 54ms | 27 MiB |
| dedented entry at the root | 280005 | 65ms | 20.5 MiB | RZ-CFG-002@2:3 duplicate key "k" | 67ms | 37 MiB |
| dedented entry after empty entries | 90009 | 16ms | 5.2 MiB | RZ-CFG-001@20002:2 syntax error: while parsing a block mapping: did no | 31ms | 11 MiB |
| dedented entry after long keys | 95921 | 12ms | 3.1 MiB | RZ-CFG-001@64:2 syntax error: while parsing a block mapping: did not f | 20ms | 7 MiB |
| tagged empty nodes in a sequence | 693007 | 117ms | 30.3 MiB | accepted | 142ms | 55 MiB |
| tagged empty values in a mapping | 948900 | 169ms | 35.2 MiB | accepted | 209ms | 70 MiB |
| tagged empty values of one key | 600010 | 96ms | 29.9 MiB | RZ-CFG-002@1:15 duplicate key "k" | 163ms | 65 MiB |
| tagged empty nodes before a closing bracket | 560012 | 112ms | 25.7 MiB | accepted | 122ms | 58 MiB |
| keys without values left open | 300005 | 146ms | 51.5 MiB | RZ-CFG-001@2:1 syntax error: while parsing a flow node: did not find e | 135ms | 65 MiB |
| tabs between flow entries | 599998 | 159ms | 45.7 MiB | accepted | 614ms | 150 MiB |
| tabs in a wide mapping | 440000 | 156ms | 40.3 MiB | RZ-CFG-002@2:1 duplicate key "k\tk" | 341ms | 97 MiB |
| tabs after tags | 400007 | 82ms | 12.8 MiB | accepted | 219ms | 85 MiB |
| flow ladder of long keys | 1048771 | 6ms | 0.0 MiB | RZ-CFG-001@3:838 syntax error: while parsing a flow sequence: did not  | 48ms | 10 MiB |
| flow ladders nested | 933353 | 7ms | 0.1 MiB | RZ-CFG-001@3:102 syntax error: while parsing a flow sequence: did not  | 51ms | 12 MiB |
| comma-less flow entries | 788897 | 9ms | 3.0 MiB | RZ-CFG-001@3:3 syntax error: while parsing a flow sequence: did not fi | 163ms | 53 MiB |
| comma-less flow keys without values | 628901 | 12ms | 3.0 MiB | RZ-CFG-001@3:3 syntax error: while parsing a flow sequence: did not fi | 126ms | 43 MiB |
| comma-less duplicate flow entries | 200007 | 3ms | 1.4 MiB | RZ-CFG-001@3:2 syntax error: while parsing a flow sequence: did not fi | 80ms | 34 MiB |
| flow pairs at the depth limit | 320 | 1ms | 0.1 MiB | RZ-CFG-001@1:129 nesting depth exceeds 64 | 12ms | 0 MiB |
| flow pairs of long keys at the depth limit | 63257 | 4ms | 0.3 MiB | RZ-CFG-001@1:31098 nesting depth exceeds 64 | 13ms | 6 MiB |
| documents at the token limit | 1044033 | 417ms | 85.4 MiB | accepted | 512ms | 193 MiB |
| documents at the file token limit | 699993 | 251ms | 87.1 MiB | accepted | 628ms | 146 MiB |
| R1#0 long key over flow (4,096) | 12293 | 6ms | 1.2 MiB | accepted | - | - |
| R1#1 flow keys without values n=40,000 | 80006 | 49ms | 19.0 MiB | RZ-CFG-002@1:7 duplicate key "a" | - | - |
| R1#1 flow keys without values n=80,000 | 160006 | 99ms | 37.9 MiB | RZ-CFG-002@1:7 duplicate key "a" | - | - |
| R1#1 flow keys without values n=160,000 | 320006 | 161ms | 75.9 MiB | RZ-CFG-002@1:7 duplicate key "a" | - | - |
| R1#2 dense flow 999,987 bytes | 999987 | 444ms | 135.0 MiB | accepted | - | - |
| R1#2 empty flow collections 999,967 bytes | 999967 | 273ms | 87.1 MiB | accepted | - | - |
| R2#0 100,000 entries + "...x: 1" | 988898 | 198ms | 52.1 MiB | accepted | - | - |
| R2#1 125,000 entries + dedent | 875009 | 175ms | 45.3 MiB | RZ-CFG-001@125002:2 syntax error: while parsing a block mapping: did n | - | - |
| R2#1 'k:\n-' pairs x 10,000 + dedent | 90009 | 16ms | 5.2 MiB | RZ-CFG-001@20002:2 syntax error: while parsing a block mapping: did no | - | - |
| R2#2 99,000 '!!str ,' | 693007 | 148ms | 27.0 MiB | accepted | - | - |
| R3#1 flow ladder L=30 R=100 K=100 | 451625 | 4ms | 0.0 MiB | RZ-CFG-001@3:102 syntax error: while parsing a flow sequence: did not  | - | - |
| R3#1 flow ladder L=40 R=100 K=100 | 602165 | 5ms | 0.0 MiB | RZ-CFG-001@3:102 syntax error: while parsing a flow sequence: did not  | - | - |
| harden2 B1 directive gadget n=8000 | 152007 | 2ms | 0.8 MiB | RZ-CFG-001@1:3 syntax error: while scanning a directive: found unknown | - | - |
| harden2 B1 directive gadget n=24000 | 456007 | 5ms | 3.0 MiB | RZ-CFG-001@1:3 syntax error: while scanning a directive: found unknown | - | - |
| harden2 B1 gadget without directive n=8000 | 143999 | 2ms | 0.8 MiB | RZ-CFG-001@2:3 syntax error: while parsing a flow sequence: did not fi | - | - |
| harden2 B2 value 8000 nulls + 190000 entries | 396011 | 9ms | 2.9 MiB | RZ-CFG-001@1:16008 syntax error: while parsing a block mapping: did no | - | - |
| harden2 B2 root 8000 nulls + 190000 entries | 396008 | 1.138s | 206.3 MiB | RZ-CFG-002@1:4 duplicate key "a" | - | - |
| harden2 B2 value 8000 nulls + 100000 entries | 216011 | 7ms | 2.9 MiB | RZ-CFG-001@1:16008 syntax error: while parsing a block mapping: did no | - | - |
| harden2 major 15,000 Route documents | 2103890 | 299ms | 37.4 MiB | accepted | - | - |
| harden2 major 20,000 Route documents | 2808890 | 419ms | 56.3 MiB | accepted | - | - |
| harden2 major 20,001 Route documents | 2809031 | 406ms | 51.8 MiB | accepted | - | - |
| w3-s1a taggedLadder 63x255 (712 KB) | 711884 | 37ms | 6.6 MiB | RZ-CFG-001@1:5 value "" does not match tag !!map | - | - |
| w3-s1a-fix explicit ladder k=31 x32 | 100640 | 2ms | 0.0 MiB | RZ-CFG-001@2:3 syntax error: while parsing a block mapping: did not fi | - | - |
| keyLadder 63 dashes x254 (4 MB) | 4082291 | 19ms | 0.0 MiB | RZ-CFG-001@3:1 syntax error: while parsing a block mapping: did not fi | - | - |

Reading of the table:

- **Every row is within 2 s and 256 MiB.** The slowest is "harden2 B2 root 8000 nulls + 190000 entries" at 1.138 s and 206 MiB, 546 bytes per byte. That is token retention (section 4.3) on a root flow mapping.
- **Nothing accepted is deeper than 64.** Every accepted tree is depth 64 or less: "long keys nested ..." reaches 64, as it should.
- **Shapes that cost WP-33 rounds are cheap.** v4's own grammar refuses the goccy-specific shapes at the first offending token:
  - the ladders (tagged, implicit-key, explicit-key and flow ladders, up to 4 MB), the comma-less entries and the directive gadget, in 2 to 45 ms with at most 7 MiB;
  - the `...` marker and dedent rows, which follow up to 1 MiB of valid entries, after parsing those entries, in 1 to 217 ms with at most 57 MiB.

  The markers v4 reads as YAML 1.2 does are accepted: `...x: 1` is a key.
- **Accepted documents cost less than in WP-33.** On accepted documents the prototype uses less heap than WP-33:
  - wide mapping 1 MiB: 50.8 vs 169 MiB;
  - documents 1 MiB: 52.4 vs 103 MiB;
  - one-character scalars at 400,001 bytes: 51 vs 165 MiB.
- **v4 accepts denser shapes.** It accepts the dense shapes WP-33 refuses by estimate, at 91 to 141 bytes per byte: 1 MiB `a: [1,1,...]` takes 135 to 140 MiB and 0.41 to 0.44 s.
- **Flow nulls are the densest accepted shape.** Flow mappings of keys without values cost about 250 bytes per byte. 800 KB takes 184 MiB, and the 1 MiB row in 4.2 takes 246 MiB, close to the 256 MiB budget. Those rows end in RZ-CFG-002 because the keys repeat.
- **One cost was the prototype's own.** An earlier version of the prototype mapped each tagged node's position to a byte offset by walking its line, which is quadratic on one long line. Its first run took 18 to 65 s on the tagged flow rows; that output file was overwritten by the rerun.

  v4 alone is linear on these shapes (`$S/bin/v4only`):

  ```
  tagged-empties-seq n=5000 3ms; n=10000 7ms; n=20000 21ms; n=40000 51ms
  tagged-values-seq n=40000 58ms
  ```

  With `source.offset` O(1) (cached per-line ASCII flag and code-point index), the rows above take 82 to 169 ms.

### 4.2 Larger inputs (1 to 64 MiB) and shapes chosen to look for super-linear paths

Command: `ROWS=... ../bin/profile.test -test.run '^TestHostileLarge$' -test.v`. Output is in `$S/notes/hostile-large.txt` and `$S/notes/hostile-64.txt`. The 64 MiB rows ran one at a time. The first 64 MiB attempt was a few bytes over 64 MiB and was refused by `MaxBytes`, so the generators were cut to just under 64 MiB.

The columns are: shape, bytes, time, peak heap growth, peak divided by input bytes, bytes allocated, result, first finding.

| shape | bytes | time | peak | peak/bytes | allocated | result | first finding |
|---|---|---|---|---|---|---|---|
| wide mapping 1 MiB | 1048581 | 232ms | 51.2 MiB | 51.2x | 69.6 MiB | 1 docs, depth 1 | accepted |
| wide mapping 4 MiB | 4194309 | 738ms | 202.4 MiB | 50.6x | 257.3 MiB | 1 docs, depth 1 | accepted |
| wide mapping 8 MiB | 8388609 | 2.29s | 400.4 MiB | 50.1x | 508.9 MiB | 1 docs, depth 1 | accepted |
| wide mapping 16 MiB | 16777217 | 4.463s | 688.3 MiB | 43.0x | 990.6 MiB | 1 docs, depth 1 | accepted |
| Route documents 8 MiB | 8388683 | 1.206s | 157.7 MiB | 19.7x | 404.7 MiB | 59573 docs, depth 4 | accepted |
| flow sequence 1 MiB | 1048575 | 385ms | 141.8 MiB | 141.8x | 169.7 MiB | 1 docs, depth 2 | accepted |
| flow sequence 8 MiB | 8388607 | 5.469s | 971.9 MiB | 121.5x | 1349.3 MiB | 1 docs, depth 2 | accepted |
| flow sequence at the root 1 MiB | 1048572 | 629ms | 502.5 MiB | 502.5x | 992.1 MiB | 0 docs, depth 0 | RZ-CFG-005@1:1 a resource must be a mapping |
| flow sequence at the root 2 MiB | 2097148 | 3.792s | 972.3 MiB | 486.1x | 1949.2 MiB | 0 docs, depth 0 | RZ-CFG-005@1:1 a resource must be a mapping |
| JSON-style root mapping 1 MiB | 1048608 | 278ms | 139.1 MiB | 139.1x | 323.0 MiB | 1 docs, depth 3 | accepted |
| JSON-style root mapping 4 MiB | 4194350 | 2.657s | 771.9 MiB | 193.0x | 1507.3 MiB | 1 docs, depth 3 | accepted |
| same mapping in block style 4 MiB | 4194309 | 740ms | 202.8 MiB | 50.7x | 257.3 MiB | 1 docs, depth 1 | accepted |
| block sequence 8 MiB | 8388611 | 1.898s | 549.7 MiB | 68.7x | 822.2 MiB | 1 docs, depth 2 | accepted |
| block nulls 1 MiB | 1068538 | 204ms | 71.8 MiB | 70.5x | 89.9 MiB | 1 docs, depth 1 | accepted |
| block nulls 8 MiB | 9374650 | 1.825s | 558.3 MiB | 62.4x | 716.1 MiB | 1 docs, depth 1 | accepted |
| flow nulls 1 MiB | 1048582 | 427ms | 246.3 MiB | 246.3x | 286.9 MiB | 0 docs, depth 0 | RZ-CFG-002@1:7 duplicate key "a" |
| flow nulls 8 MiB | 8388614 | 8.742s | 1926.4 MiB | 240.8x | 2275.6 MiB | 0 docs, depth 0 | RZ-CFG-002@1:7 duplicate key "a" |
| flow keys with ':' and no value 8 MiB | 8388613 | 2.758s | 1192.1 MiB | 149.0x | 1506.4 MiB | 0 docs, depth 0 | RZ-CFG-002@1:8 duplicate key "k" |
| long plain key 1 MiB | 1048580 | 26ms | 3.1 MiB | 3.1x | 6.0 MiB | 1 docs, depth 1 | accepted |
| long plain key 8 MiB | 8388612 | 185ms | 23.8 MiB | 3.0x | 47.8 MiB | 1 docs, depth 1 | accepted |
| long quoted key 8 MiB | 8388614 | 183ms | 23.8 MiB | 3.0x | 47.8 MiB | 1 docs, depth 1 | accepted |
| long plain scalar 8 MiB | 8388612 | 215ms | 23.8 MiB | 3.0x | 47.8 MiB | 1 docs, depth 1 | accepted |
| multi-line plain scalar 8 MiB | 8388613 | 231ms | 59.5 MiB | 7.4x | 121.9 MiB | 1 docs, depth 1 | accepted |
| long double-quoted scalar 8 MiB | 8388612 | 163ms | 21.0 MiB | 2.6x | 37.1 MiB | 1 docs, depth 1 | accepted |
| long block scalar 8 MiB | 8388676 | 154ms | 23.3 MiB | 2.9x | 52.5 MiB | 1 docs, depth 1 | accepted |
| block scalar + 4 Mi blank lines (8 MiB) | 8388614 | 252ms | 115.7 MiB | 14.5x | 235.9 MiB | 1 docs, depth 1 | accepted |
| block scalar + 4 Mi blank lines then comment | 8388618 | 273ms | 115.7 MiB | 14.5x | 235.9 MiB | 1 docs, depth 1 | accepted |
| keep block scalar + 8 Mi blank lines | 8388685 | 1.132s | 222.0 MiB | 27.7x | 431.0 MiB | 1 docs, depth 1 | accepted |
| comments 8 MiB | 8388608 | 242ms | 83.1 MiB | 10.4x | 145.5 MiB | 0 docs, depth 0 | accepted |
| brackets 8 MiB | 8388608 | 43ms | 0.0 MiB | 0.0x | 0.0 MiB | 0 docs, depth 0 | RZ-CFG-001@1:65 syntax error: while increasing flow level: nesting depth exceeds... |
| braces 8 MiB | 8388608 | 37ms | 0.0 MiB | 0.0x | 0.0 MiB | 0 docs, depth 0 | RZ-CFG-001@1:65 syntax error: while increasing flow level: nesting depth exceeds... |
| compact sequences 1 MiB of '- ' | 1048578 | 5ms | 0.0 MiB | 0.0x | 0.0 MiB | 0 docs, depth 0 | RZ-CFG-001@1:129 syntax error: while increasing indent level: nesting depth excee... |
| deep flow at the limit, wide (64 levels x 100k) | 200131 | 134ms | 82.9 MiB | 434.1x | 167.8 MiB | 1 docs, depth 64 | accepted |
| flow pairs '[k: ' x 63 | 320 | 2ms | 0.1 MiB | 299.3x | 0.0 MiB | 0 docs, depth 0 | RZ-CFG-001@1:129 nesting depth exceeds 64 |
| flow pairs '[k: ' x 1000 | 5005 | 2ms | 0.1 MiB | 12.6x | 0.0 MiB | 0 docs, depth 0 | RZ-CFG-001@1:260 syntax error: while increasing flow level: nesting depth exceeds... |
| anchors 8 MiB | 8388604 | 1.138s | 269.2 MiB | 33.7x | 396.1 MiB | 0 docs, depth 0 | RZ-CFG-003@1:3 anchors are not allowed (&a) |
| aliases to one anchor 8 MiB | 8388622 | 1.605s | 543.3 MiB | 67.9x | 665.3 MiB | 0 docs, depth 0 | RZ-CFG-003@1:4 anchors are not allowed (&a) |
| tags 8 MiB | 8388612 | 1.564s | 371.4 MiB | 46.4x | 489.7 MiB | 0 docs, depth 0 | RZ-CFG-004@1:5 tag !x is not allowed |
| long tag 8 MiB | 8388615 | 248ms | 32.0 MiB | 4.0x | 71.8 MiB | 0 docs, depth 0 | RZ-CFG-004@1:4 tag !xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx... |
| long anchor 8 MiB | 8388615 | 284ms | 24.1 MiB | 3.0x | 55.8 MiB | 0 docs, depth 0 | RZ-CFG-003@1:4 anchors are not allowed (&xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx... |
| flow mapping, long single line at the root 2 MiB | 2097161 | 1.625s | 399.5 MiB | 199.8x | 779.1 MiB | 1 docs, depth 1 | accepted |
| flow sequence as implicit key 1 MiB | 1048583 | 1.694s | 497.6 MiB | 497.6x | 940.2 MiB | 0 docs, depth 0 | RZ-CFG-001@1:1 syntax error: a mapping key must be a scalar |
| 64 MiB + 1 byte | 67108865 | 2ms | 0.0 MiB | 0.0x | 0.0 MiB | 0 docs, depth 0 | RZ-CFG-001@1:1 file is larger than 67108864 bytes |
| wide mapping 64 MiB | 67108810 | 28.684s | 2990.6 MiB | 46.7x | 3834.9 MiB | 1 docs, depth 1 | accepted |
| Route documents 64 MiB | 67108708 | 12.248s | 1183.8 MiB | 18.5x | 3208.7 MiB | 473379 docs, depth 4 | accepted |
| block sequence 64 MiB | 67108863 | 44.672s | 4973.1 MiB | 77.7x | 6645.5 MiB | 1 docs, depth 2 | accepted |
| long plain scalar 64 MiB | 67108864 | 2.335s | 199.4 MiB | 3.1x | 439.4 MiB | 1 docs, depth 1 | accepted |
| comments between entries 8 MiB | 8388616 | 3.143s | 649.5 MiB | 81.2x | 1119.2 MiB | 1 docs, depth 1 | accepted |

Reading of the table:

- **Memory is linear in input but large per byte.**

  | Shape | Bytes of heap per input byte |
  |---|---|
  | Route documents | 18.5 to 21 |
  | wide mapping | 43 to 51 |
  | block nulls | 62 to 70 |
  | block sequence | 69 to 78 |
  | flow sequence (value position) | 120 to 140 |
  | flow nulls | 241 to 246 |

  Most of it is v4's `yaml.Node`, which is built whole for a document before any check runs. `cmd/v4only` (v4 alone, no Ruralz tree) gives:

  | Shape | Bytes | Peak | Ratio | Time |
  |---|---|---|---|---|
  | `block-seq` | 1,048,579 | 49.3 MiB | 47.2x | |
  | `wide-map` | 5,656,058 | 209.3 MiB | 38.8x | |
  | `flow-seq` | 1,048,583 | 97.0 MiB | 97.0x | |
  | `flow-nulls` | 1,048,582 | 194.4 MiB | 194.4x | 525 ms |

- **The 4 × source bound fails, as it does for WP-33.** The 11 req 17 proposal ("MUST stay below 4 × the source size") is not met by any structured shape. WP-33 measured about 24x for Route documents (harden2 major finding).
- **Time at 64 MiB.** A wide mapping takes 28.7 s and 2.99 GB. Route documents (473,379 documents, far above MaxResources) take 12.2 s and 1.18 GB. A block sequence takes 44.7 s and 4.97 GB. Time grows slightly faster than input because of GC: the wide mapping takes 0.23, 0.74, 2.29, 4.46 and 28.7 s at 1, 4, 8, 16 and 64 MiB.
- **Long scalars, keys, tags, anchors and comments are linear and cheap.**

  | Shape | Time | Peak |
  |---|---|---|
  | 8 MiB plain key | 185 ms | 24 MiB |
  | 8 MiB block scalar | 154 ms | 23 MiB |
  | block scalar + 4 Mi blank lines | 252 ms | 116 MiB |
  | keep block scalar + 8 Mi blank lines | 1.13 s | 222 MiB |
  | 64 MiB plain scalar | 2.3 s | 199 MiB |

  goccy's O(m·L) clipping has no equivalent in v4.
- **Depth.** 8 MiB of `[` or `{` is refused in 37 to 43 ms with 0 MiB, and `- - - ...` (1 MiB) in 5 ms, by v4's scanner calling the depth hook.
- **Retention rows.** The root-flow and implicit-key rows show the retention of section 4.3:

  | Row | Peak | Ratio |
  |---|---|---|
  | flow sequence at the root, 1 MiB | 502 MiB | 502x |
  | flow sequence at the root, 2 MiB | 972 MiB | 486x |
  | JSON-style root mapping, 4 MiB | 772 MiB | 193x |
  | flow sequence as an implicit key, 1 MiB | 498 MiB | |
  | deep flow at the limit, 200 KB | 83 MiB | 434x |

### 4.3 The token-retention regression (rc.4 to rc.6), with a patch experiment

**v4 alone** (`$S/bin/v4only <shape> <n> [v4|v3]`, source `$S/proto/cmd/v4only/main.go`; `peak` is heap growth):

```
v4 flow-seq             n=524288 bytes=1048583 time=383ms  peak=97.0MiB  ratio=97.0x   (value position "a: [1,...]")
v4 flow-seq-root        n=524288 bytes=1048580 time=3.891s peak=497.6MiB ratio=497.6x  ("[1,...]" at the root; reruns 2.165s, 2.589s)
v4 nested-flow          n=524288 bytes=1048585 time=2.569s peak=497.5MiB ratio=497.5x  ("a: [[1,...]]")
v4 flow-seq-entry       n=524288 bytes=1048582 time=2.385s peak=497.7MiB              ("- [1,...]")
v4 flow-seq-root-multiline n=262144 bytes=786437 time=1.198s peak=257.2MiB            ("[\n1,\n...]": multi-line too)
v4 flow-map-root-multiline n=262144 bytes=4083207 time=4.145s peak=624.7MiB           (JSON object over 262,145 lines)
v4 json-root            n=30000  bytes=1248970 time=1.279s peak=255.0MiB ratio=214.1x
v4 flow-seq-after-doc   n=262144 bytes=524296  time=193ms  peak=45.9MiB               ("--- [1,...]": no retention)
v3 flow-seq-root        n=262144 bytes=524292  time=197ms  peak=45.5MiB               (gopkg.in/yaml.v3 v3.0.1)
v3 json-root            n=30000  bytes=1248970 time=178ms  peak=42.5MiB
v3 nested-flow          n=524288 bytes=1048585 time=423ms  peak=96.0MiB
```

**Profile.** `v4only flow-seq-root 262144 v4 root.prof` followed by `go tool pprof -top` shows:

- `runtime.memmove` 37.7%;
- `(*Parser).insertToken` 45.2% cumulative, all on line `scanner.go:3292 parser.tokens = append(parser.tokens, *token)` reached through `runtime.growslice`;
- `fetchFlowEntry` 80% of its callers;
- `procyield`, `tgkill` and GC scanning taking the rest.

The token queue grows to hold every token of the collection.

**Cause.** `fetchMoreTokens` at `$V4/internal/libyaml/scanner.go:794-830` keeps fetching while the outermost potential simple key, `simple_key_stack[0]`, is at the queue head. Nothing marks a key stale when the line changes or after 1024 characters. Release by release:

- yaml.v3 had that rule: `simple_key.mark.line < parser.mark.line || simple_key.mark.index+1024 < parser.mark.index` at `gopkg.in/yaml.v3@v3.0.1/scannerc.go:896`, checked in `yaml_parser_fetch_more_tokens` at 657-680.
- v4 rc.1 to rc.3 still have it (`grep -rln 'mark.line < parser.mark.line'` finds `scannerc.go` in rc.1 and rc.2 and `internal/libyaml/scanner.go` in rc.3).
- rc.4 replaced it with `simple_key_stack` and the check is gone: the same grep finds nothing in rc.4. It is also absent in rc.5 and rc.6.
- rc.3 on the same input: `rc.3 flow-seq-root n=524288 bytes=1048580 time=365ms peak=91.5MiB` (`$S/rc3`).

The positions affected are those where an implicit key may start: the document root, after `- `, a value on its own line, and items of a flow sequence (after `[` or `,`). So JSON written as `.yaml`, lists of lists, and `- [..]` all hit it.

**Patch experiment.** Source: `$S/patched/v4`, a copy of rc.6 with `cmd/`, `example/` and `docs/` removed, built only through a `replace` in `$S/proto-patched`. It adds two conditions:

1. In `fetchMoreTokens`, it stops fetching when the head key is on an earlier line or more than 1024 characters back:

   ```go
   } else if key.mark.Line < parser.mark.Line || key.mark.Index+1024 < parser.mark.Index { break }
   ```

2. In `fetchValue`, it requires `simple_key.mark.Index+1024 >= parser.mark.Index`.

Results with the patch:

- Unchanged results: `TestYAMLTestSuite` still gives 402 cases, 279 pass, 123 fail, with the same IDs. `TestFindingRows` still gives 224 rows with the same 8 differing.
- `v4only` (patched):

  ```
  flow-seq-root 524288: 408ms 95.8MiB
  flow-seq-root-multiline 524288: 337ms 91.6MiB
  nested-flow 524288: 309ms 91.6MiB
  flow-seq-entry 524288: 360ms 92.4MiB
  json-root 30000: 182ms 41.9MiB
  flow-map-root-multiline 262144: 447ms 95.4MiB
  ```

- `TestHostileLarge` (patched):

  | Row | Time | Peak |
  |---|---|---|
  | flow sequence at the root 1 MiB | 451 ms | 127.9 MiB |
  | flow sequence at the root 2 MiB | 861 ms | 238.0 MiB |
  | JSON-style root mapping 4 MiB | 652 ms | 184.0 MiB |
  | flow sequence as implicit key 1 MiB | 399 ms | 91.6 MiB (refused: implicit key over 1024 characters) |

- `FuzzParse` on the patched build: 180 s, 337,998 executions, PASS (`$S/notes/fuzz-patched.txt`).

### 4.4 Recursion and built-in limits in v4 (source and measurements)

- **The scanner and the event parser are iterative.** The parser keeps an explicit `states []ParserState` stack (`parser.go:246`, push and pop at 482, 531, 575-576).
- **The composer recurses once per nesting level.** `Compose` calls `parseChild`, which calls `Compose` (`composer.go:243`, `251` and `360-361`). The resolver also recurses: `Resolver.Resolve` calls `r.Resolve(child)` at `resolver.go:66, 74, 79`.
- **There is a built-in depth limit of 10,000 per kind.**
  - `DefaultDepthCheck` (`options.go:61-69`, `const maxDepth = 10000`) is the default (`options.go:431`).
  - The scanner calls it with the open flow levels on each `[`/`{` (`scanner.go:3154`) and with the open block indentation levels on each indent push (`scanner.go:3201`).
  - `yaml.WithPlugin(LimitPlugin)` replaces it. The prototype passes `depthLimit{64}`. Both counts are at most the node depth, so 64 never refuses a document within 64, and the walker enforces the exact limit.

  Measurements:

  ```
  v4 deep-flow n=10000   ok (62 ms, 12 MiB)
  v4 deep-flow n=10001   "exceeded max depth of 10000" at L1.C10001
  v4 deep-block n=10001  "while increasing indent level ... exceeded max depth of 10000"
  v4-nolimit (limit.DepthNone) deep-flow n=100000   ok 427 ms 110.4 MiB
  v4-nolimit deep-flow n=1000000  ok 7.265 s 1240 MiB   (composer recursion on the goroutine stack; unbounded)
  v4-nolimit deep-pairs n=100000  ok 444 ms 145 MiB
  ```

  With the hook disabled, the composer's goroutine stack grows with depth. Go's default maximum stack is 1 GB, and exceeding it is a fatal error that `recover` cannot catch. This is inferred, not measured past 1,000,000 levels. The hook must never be disabled.
- **Alias limits apply only when building Go values.** `DefaultAliasCheck` (`options.go:71-`) is a ratio heuristic called only from `Constructor.Construct` (`constructor.go:86-89`). A `yaml.Node` target returns at `constructor.go:91-93` and never expands aliases. The composer stores an alias as a pointer to the anchored node (`composer.go` `alias()`). The 1 MiB billion-laughs input costs 27.8 MiB and is refused at the first anchor.
- **Quadratic path outside the profile.** v4's own unique-key check (`constructor.go:593-600`, a nested loop) applies only when decoding into Go maps or structs. `TestUniqueKeysCost` shows:

  | Keys | Into `map[string]any` | Into `yaml.Node` |
  |---|---|---|
  | 10,000 | 370 ms | 18 ms |
  | 20,000 | 1.514 s | 29 ms |
  | 40,000 | 7.378 s | 74 ms |

  The profile must load into `yaml.Node`, as the prototype does.
- **Panics.** Internal "please report" panics are re-raised by `handleErr` (`errors.go:251-258`); examples are at `composer.go:104` and `106` and `resolver.go:180`. The prototype recovers them in `load()`. Fuzzing (section 7) found none.

## 5. YAML Test Suite (test/fixtures/yaml-test-suite, data-2022-01-17, 402 cases)

The runner is `$S/proto/profile/yts_test.go`. Its comparison code (`eventDocuments`, `compareEvent`, `jsonDocuments`, `compareJSON`) is copied from `internal/config/profile/yamltestsuite_integration_test.go`, so the rules are the same:

- an `error` case must be refused;
- any other case must give its `test.event` documents (kind, text, typed value, style) and its `in.json`.

It calls `Parse(in, Options{AnyRoot: true})`. Command: `cd $S/proto && CGO_ENABLED=0 go test -count=1 -run TestYAMLTestSuite -v ./profile`. Output: `YAML Test Suite: 402 cases, 279 pass, 123 fail` (`$S/notes/yts-final.txt`). WP-33 reports 315 pass and 87 expected failures (task statement; `expected-failures.txt` lists 87 IDs).

| Class | Count | IDs |
|---|---|---|
| WP-33 profile:anchor (still refused) | 29 | same 29 as `expected-failures.txt`. RZ-CFG-003 is the first finding except 2SXE, 6M2F, 8XYN and W5VH, where v4's syntax error comes first. v4 limits anchor names to alphanumerics (8XYN `&😁`, W5VH `&:@*!$"<foo>:`) and refuses empty keys (6M2F), so `reasonShown` would need to accept RZ-CFG-001 for these. |
| WP-33 profile:tag | 21 | same 21. `!` is reported as "tag ! is not allowed" and `!<...>` as a verbatim tag. |
| WP-33 profile:directive | 4 | BEC7, MUS6/02, MUS6/03, MUS6/04 |
| WP-33 profile:complex-key | 12 | same 12 (M2N8/00 is refused by v4's parser first) |
| reviewed goccy, now passing | 2 | DFF7, FH7J |
| reviewed goccy, still failing | 19 | 2JQS, 4MUZ/02, 9C9N, 9JBA, 9MMW, CFD4, CVW2, DK95/04, FRK4, M7A3, NHX8, NKF9, QB6E, S3PD, SM9W/01, SU5Z, UKK6/00, VJP3/01, Y79Y/003. 9MMW is now parsed correctly and refused as a complex key (`{JSON: like}:adjacent`), so it becomes profile:complex-key. |
| new failures, not in WP-33's list | 38 | see below |

The 57 failures beyond the profile restrictions, by behavior:

- **Valid streams refused (42).**
  - Tabs as separation or indentation (12): 6BCT, 6CA3, A2M4, DK95/00, DK95/03, DK95/07, Q5MG, Y79Y/010, 96NN/00, 96NN/01, R4YG, Y79Y/001, plus DK95/04 (listed before).
  - Reserved directives (4): 2LFX, 6LVF, MUS6/05, MUS6/06. These would become profile:directive if the profile refuses reserved directives, which is harden2 fix (a).
  - Multi-line or adjacent flow keys (7): 4MUZ/00, 4MUZ/01, 5MUD, 9SA2, K3WX, NJ66, UT92, plus 4MUZ/02 and VJP3/01 (listed before).
  - `?` inside flow plain scalars: JR7V.
  - Empty flow nodes: 58MP, 5T43, WZ62, plus CFD4 and FRK4 (listed before).
  - Empty block keys: 2JQS, NHX8, NKF9, S3PD, SM9W/01, UKK6/00 (listed before).
  - Bare document after `...`: 7Z25, plus M7A3 (listed before).
  - The YAML 1.2 `\/` escape: 3UYS.
  - The complex key 9MMW.
- **Invalid streams accepted (12).**
  - 9C9N, 9JBA, CVW2, QB6E, SU5Z and Y79Y/003, which goccy also accepts.
  - New: DK95/01, G5U8, HRE5 (`"\'"` escape accepted), S98Z, X4QW and YJV2.

  Each gives the obvious tree, for example `key: "value"# c` gives `value`. None is a structure change.
- **Accepted with a different value (3).**
  - JEF9/02 (`- |+\n   ` gives "", want "\n") and L24T/01 (`foo: |\n  x\n   ` gives "x\n ", want "x\n \n"): a last line of spaces with no final line break.
  - W4TN: a document-root block scalar indented 0 does not end at `...` or `---`. `--- >\nfoo\n---\na: 1` gives one string document, `"foo --- a: 1\n"`, via `cmd/dump`. In production the scalar root is RZ-CFG-005, so the file is refused either way.

v4's own list (`$V4/yts/known-failing-tests`) has 225 subtests over 149 case IDs (`awk -F/ '{print $2}' | sort -u | wc -l`), against the same data release.

Some anchor-free reviewed cases pass with v4 and fail with goccy, and the reverse. The net change against WP-33 is −36 passes: +2 fixed, −38 newly failing.

## 6. Positions, duplicate keys, tags and styles

Command: `go test -run 'TestErrorPositions|TestPositions|TestMisc|TestTabs' -v ./profile`.

### Positions

- **Line and column of every node, in code points.** `yaml.Node.Line` and `Node.Column` are 1-based (`yaml.go:105-109`, "1-indexed"). A tab counts as one column.
  - `k: {a: 1,\tb: [x,\ty, {z: 2}]}` puts `b` key at 1:11, `b` value at 1:14, `[0]` at 1:15, `[1]` at 1:18, `[2]` at 1:21, `z` key at 1:22 and `z` value at 1:25. All are correct.
  - `ключ: значение\nk2: [日本, 語]` gives value 1:7, 日 at 2:6 and 語 at 2:10, counted in code points.
  - CR LF and lone CR are handled: `a: 1\rb: [x,\r  y]\r` puts `b` at 2:4 and `y` at 3:3.
- **Where a node starts.**
  - Block keys start at the key. A value written on the next line is at that line (`a: # c\n  b` gives 2:3).
  - An empty value is right after the `:` (`empty:\nnext: 1` gives 1:7).
  - A tagged or anchored node starts at its first property (`a: !!str x` gives 1:4, the `!`). That is where the Ruralz tag and anchor findings go.
- **Errors.** `*yaml.LoadError` carries `Mark` (the problem) and `ContextMark` plus `ContextMsg` (the construct start), each with line and column (`errors.go:36-49`). Examples (go-yaml text, then the prototype):

  ```
  "a: [1, 2\nb: 3\n"  -> (while parsing a flow sequence) at L1.C4-L2.C2: did not find expected ',' or ']'  -> RZ-CFG-001@2:2
  "a: \"unterminated\n" -> (while scanning a quoted scalar) at L1.C4-L2.C1: found unexpected end of stream
  "a: b: c\n" -> at L1.C5: mapping values are not allowed in this context
  "a:\n  b: 1\n c: 2\n" -> (while parsing a block mapping) at L1.C1-L3.C2: did not find expected key
  "k: |\n x\n" + 8 blank + "0\n 0\n" -> (while scanning a simple key) at L11.C1-L13.C1   (ContextMark is the key start)
  "a:\n\tb: 1\n" -> at L2.C1: found character that cannot start any token
  ```

  yaml.v3 reports only a line (`yaml: line 2: ...`, see the `TestTabsV3` output).

### Duplicate keys

v4 does **not** report duplicate keys for a `yaml.Node` target. `a: 1\na: 2\n` loads both pairs (`cmd/dump`). `WithUniqueKeys`, default true (`options.go:428`), runs only in `Constructor.mapping` for Go maps and structs (`constructor.go:593`). The Ruralz walker reports every duplicate with both positions:

- `1: a\n"1": b` gives RZ-CFG-002@2:1;
- `k: {a: 1, a: 2}` gives RZ-CFG-002@1:11.

### Tags and styles

- **Explicit core tag.** The composer shortens the tag (`!!str`) and sets `TaggedStyle` (`composer.go:111-118`, `if tag != "" && tag != "!"`). The resolver never touches a node with a tag.
- **Implicit tags are v4's own resolution, not the YAML 1.2 core schema.** The resolver fills `Node.Tag` (`resolver.go:44-85`). `cmd/dump` of a plain-scalar flow sequence shows:

  | Scalar | v4 tag |
  |---|---|
  | `0b1`, `1_000` | `!!int` |
  | `2026-09-23` | `!!timestamp` |
  | `-0` | `!!float` |
  | `yes`, `on` | `!!str` |
  | `<<` | `!!merge` |

  Ruralz must therefore type by itself. The prototype calls `resolvePlain` when `TaggedStyle` is clear and the style is plain, and `resolveTagged` when `TaggedStyle` is set. The 01 test plan scalar row passes: `yes`, `on`, `0b1`, `1_000` and `2026-09-23` are strings, `0777` is 777, `0o17` is 15, `0x1F` is 31, `1e3` is float `1e3` and `-0` is 0.
- **Non-specific tag `!`.** It is kept as `Tag == "!"` without `TaggedStyle`: `- ! 12` gives `scalar@3:3 tag="!" style=0`. The walker refuses it.
- **Verbatim tag.** `!<tag:yaml.org,2002:str>` becomes `!!str` with `TaggedStyle`, the same as `!!str`. The walker tells them apart by reading the source at the node position. `a: !<tag:yaml.org,2002:str> x` gives RZ-CFG-004@1:4.
- **Styles.** `Style` flags are `DoubleQuotedStyle` (2), `SingleQuotedStyle` (4), `LiteralStyle` (8), `FoldedStyle` (16), `FlowStyle` (32) and `TaggedStyle` (1). Plain is none of these (`cmd/dump`: `style=2` for `"12"`, `style=8` for `|`).
- **%YAML versions.** v4 refuses `%YAML 1.2` ("found incompatible YAML document", `parser.go:638-641`, `if token.major != 1 || token.minor != 1`) and accepts `%YAML 1.1` (`cmd/dump`). `directive.go` works around this.
- **Reserved directives.** v4 refuses them ("found unknown directive name", `scanner.go:2021`).
- **Line separators.** v4 treats U+0085, U+2028 and U+2029 as line breaks, a YAML 1.1 rule: `a: x\u2028y\nb: 1` gives an error at 3:1. The prototype refuses those characters (RZ-CFG-001). Their escapes `\N`, `\L` and `\P` stay accepted.
- **Implicit key length.** Implicit keys longer than 1024 characters are accepted: an 8 MiB plain key loads. YAML 1.2 forbids them, and the patched build refuses them.

## 7. Fuzzing and differential

- **FuzzParse** (`profile/fuzz_test.go`). Seeds are the 19 WP-33 `FuzzLoadYAML` seeds plus every suite `in.yaml`. Each input must:
  - not panic, an "internal parser error" included;
  - produce only RZ-CFG-001 to 005;
  - give no accepted tree deeper than 64;
  - return no documents together with findings;
  - take less than 2 s for both root modes.

  Command:

  ```
  go test -c -fuzz=FuzzParse -o ../bin/profilef.test ./profile
  cd profile && ../../bin/profilef.test -test.run '^$' -test.fuzz '^FuzzParse$' -test.fuzztime 300s -test.parallel 2 -test.fuzzcachedir $S/notes/fuzzcache
  ```

  Result: `fuzz: elapsed: 5m1s, execs: 1923108 ... new interesting: 317 (total: 738)` and `PASS`.
- **Differential against yaml.v3 v3.0.1.** `TestDifferentialV3` compares raw node shapes, structure plus text plus plain or quoted, over 738 inputs (the suite plus the fuzz corpus). Output: `same 391, differ 3, only v4 accepts 12, only v3 accepts 1, both refuse 331`. Every difference inspected is a v4 YAML 1.2 fix:
  - `{ ?foo: bar }` gives key `?foo`;
  - `[?x]` gives `?x`;
  - 4ABK, DBG4, DK3J, FP8R, HWV9, QT73;
  - U99R `- !!str, xxx` is refused, as it should be.

## 8. What the front end still has to handle, and open risks

1. **Retention (rc.4 to rc.6).** This is the blocker for the 01 test plan budget (256 MiB, 2 s at 1 MiB) and for the 64 MiB limit: about 500x means about 32 GB at 64 MiB of root-level flow. It needs one of:
   - an upstream fix: restore the stale-key rule, about two conditions (section 4.3);
   - a vendored patch;
   - pinning rc.3, which has the rule but not the depth hook, and has older APIs;
   - a byte-level pre-scan bounding flow collections, which is the kind of workaround WP-33 grew from.
2. **Release-candidate status.** No stable v4. The API changed across rc.3, rc.4 and rc.5 (package move, simple-key rework, plugin API).
3. **No token or event API.** Memory per document is bounded only by input bytes: about 20 to 250 bytes of heap per input byte for accepted shapes, and 3 to 5 GB at 64 MiB. A `MaxDocumentTokens` limit (01 req 6, 1,000,000 tokens) cannot be enforced exactly. It would need a byte estimate or a count of nodes after composition.
4. **No context cancellation.** One 64 MiB document runs 12 to 45 s.
5. **Expected-failures list.** It would hold about 52 `reviewed:v4` entries plus 4 reserved-directive entries if those become profile:directive, against 21 for goccy. The new user-visible refusals:
   - tabs after `-` or `:` separators and in blank lines (`a: 1\n\t\nb: 2` and `a:\n  - x\n  -\ty` are refused, a block scalar whose first line starts with a tab is refused, and an embedded script whose later lines start with tabs is accepted);
   - `"\/"`;
   - a bare document after `...`;
   - multi-line flow keys;
   - empty keys.
6. **Small wrong-value cases.** A last line of spaces in a block scalar with no final newline (JEF9/02, L24T/01), and a document-root block scalar indented 0 swallowing `---` (production refuses those with RZ-CFG-005).
7. **The 11 req 17 bound "below 4 × the source size"** is not met; WP-33 does not meet it either.

## 9. Reproduce

```
cd $S/proto
CGO_ENABLED=0 go test -count=1 -run 'TestYAMLTestSuite|TestFindingRows|TestErrorPositions|TestPositions|TestMisc|TestTabs|TestTabsV3|TestUniqueKeysCost|TestDifferentialV3' -v ./profile
go test -c -o ../bin/profile.test ./profile && cd ../notes && ../bin/profile.test -test.run '^TestHostile(WP33|Large)$' -test.v   # ROWS=<substring> filters
go build -o ../bin/v4only ./cmd/v4only && ../bin/v4only flow-seq-root 524288 [v4|v3|v4-nolimit]
go build -o ../bin/dump ./cmd/dump && ../bin/dump 'a: !!str 0777\n'
cd $S/proto-patched && go test -count=1 -run 'TestYAMLTestSuite|TestFindingRows' -v ./profile
```

Raw outputs are in `$S/notes/`: `yts-final.txt`, `rows-final.txt`, `hostile-wp33.txt`, `hostile-large.txt`, `hostile-64.txt`, `wp33-hostile.txt`, `side-by-side.md`, `fuzz1.txt`, `fuzz-patched.txt`, `diffv3.txt` and `findings.txt` (the extracted review findings).
