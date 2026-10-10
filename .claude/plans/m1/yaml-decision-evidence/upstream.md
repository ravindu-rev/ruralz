# Upstream state of Go YAML parsers (role: upstream), read 2026-10-10

Scratch root: `/tmp/claude-0/-home-user-ruralz/0aabfe6e-34a6-51d2-9b9b-23886b703760/scratchpad/yamldecision/upstream/`
(downloads in `dl/`, probe module in `probe/`). No repository file was changed. No git command that changes state was run.

## 0. Method and access

- `gh api` and `curl` to `github.com` / `api.github.com` / `codeload.github.com` return HTTP 403 "GitHub access to this repository is not enabled for this session" for every repo except `ravindu-rev/ruralz`; the GitHub MCP tools refuse the same way (`Access denied: repository "goccy/go-yaml" is not configured for this session`). I did not call `add_repo` (the tool says to ask the user first, and a subagent cannot).
- Work-arounds used: WebFetch of `github.com` HTML pages (works; it summarizes, so issue/PR wording below is the fetch tool's quote or paraphrase), `raw.githubusercontent.com` (200), `proxy.golang.org` (200), `pkg.go.dev` (200), `api.osv.dev` (POST works), and the local Go module cache (`$(go env GOMODCACHE)`, read only) which already held `github.com/goccy/go-yaml@v1.19.2`, `go.yaml.in/yaml/v4@v4.0.0-rc.1..rc.6`, `go.yaml.in/yaml/v3@v3.0.5`.
- Toolchain for probes: `go version go1.27.1 linux/amd64`. Probe module `probe/main.go` (my code only) built with `GOFLAGS=-mod=mod`; downloaded modules were only compiled through the Go toolchain.

## 1. URLs already cited in docs/_meta/research (Ruralz documents may cite only these)

Checked with `grep -rxF <url>` / `grep -rF "(<url>)"` over `docs/_meta/research/*.md`. All are in `tooling-and-licenses.md` section 6 and its URL list (lines 139-157, 405-423):

- https://github.com/goccy/go-yaml/releases/tag/v1.19.2
- https://github.com/goccy/go-yaml/releases
- https://github.com/goccy/go-yaml/commits/master
- https://github.com/goccy/go-yaml/blob/master/LICENSE
- https://github.com/goccy/go-yaml/blob/master/README.md
- https://github.com/goccy/go-yaml/blob/master/token/token.go
- https://github.com/goccy/go-yaml/blob/v1.19.2/option.go
- https://github.com/yaml/go-yaml/blob/main/LICENSE
- https://github.com/yaml/go-yaml/blob/main/NOTICE
- https://github.com/yaml/go-yaml/blob/main/README.md
- https://github.com/yaml/go-yaml/tags
- https://github.com/yaml/go-yaml/blob/v3.0.5/LICENSE, .../v3.0.5/go.mod, .../v3.0.5/decode.go
- https://github.com/yaml/go-yaml/blob/v2.4.2/README.md, .../v2.4.2/resolve.go
- https://github.com/kubernetes-sigs/yaml/blob/master/LICENSE, .../blob/master/README.md, .../blob/v1.6.0/yaml.go

NOT cited anywhere in docs/_meta/research (a research addendum is needed before any Ruralz doc cites them): every goccy issue/PR URL below, https://github.com/goccy/go-yaml/security, https://github.com/goccy/go-yaml/blob/master/yaml_test_suite_test.go, goccy `scanner/scanner.go`, every yaml/go-yaml issue URL, https://github.com/yaml/go-yaml/commits/main, https://github.com/yaml/go-yaml/security, https://github.com/yaml/go-yaml/tree/main/yts, the v4 `docs/plugins.md`, https://github.com/go-yaml/yaml, all pkg.go.dev, proxy.golang.org and api.osv.dev URLs, https://yaml.org/libraries/, https://matrix.yaml.info/, https://github.com/go-openapi/go-yaml, https://github.com/yaml/yamlstar, https://github.com/yaml/yamlstar-go, https://github.com/braydonk/yaml, https://github.com/zclconf/go-cty-yaml, https://pkg.go.dev/github.com/mrpicc0lo/yaml-go.

Two statements in section 6 of the research doc are now contradicted (section 2.6 below): goccy's number resolution is partly YAML 1.1, and v4 rejects a `%YAML 1.2` directive.

## 2. github.com/goccy/go-yaml

### 2.1 Releases: none after v1.19.2

- `curl https://proxy.golang.org/github.com/goccy/go-yaml/@v/list` (sorted tail): `... v1.18.0 v1.19.0 v1.19.1 v1.19.2`. `@latest`: `{"Version":"v1.19.2","Time":"2026-01-08T01:12:13Z", ... "Hash":"92bc79cb5f685e999ad131473168fc45215d12d9"}`.
- https://github.com/goccy/go-yaml/releases (WebFetch): newest is "1.19.2 (08 Jan, marked Latest)" with one entry, "Fix anchor reference regression in nested structures (#839)". 1.19.1: #829, #834. 1.19.0: #763, #795, #754, #756, #758, #759, #790.
- No release-note entry in 1.19.0..1.19.2 fixes a scanner/parser behavior from the WP-33 findings.

### 2.2 Default branch after v1.19.2

- `curl https://proxy.golang.org/github.com/goccy/go-yaml/@v/master.info`: `v1.19.3-0.20260407131736-edee2f91616c`, `Time 2026-04-07T13:17:36Z`.
- https://github.com/goccy/go-yaml/commits/master (WebFetch), newest first: Apr 7, 2026 #862 (decoder TagNode); Mar 10, 2026 #838 (token quoting for Ruby); Feb 26, 2026 #852 (encoder crash); Feb 16, 2026 #850 "Fix empty sequence item consuming sibling mapping key"; Feb 16, 2026 #848 (sibling anchor aliases); Jan 8, 2026 #839 (v1.19.2).
- Source diff, master (edee2f9, from raw.githubusercontent.com) versus the v1.19.2 module cache, files `scanner/scanner.go scanner/context.go parser/parser.go parser/context.go parser/node.go parser/token.go token/token.go lexer/lexer.go ast/ast.go decode.go`: changed lines 0,0,2,0,0,0,2,0,0,18. The only parse-path change is `parser/parser.go:1178`:
  `< if tk.Column() < seqCol {` / `> if tk.Column() < seqCol || (tk.Column() == seqCol && tk.Line() != seqLine) {` (PR #850). `token/token.go:693` adds `':'` to the quoting set (encoder, #838). The scanner is byte-identical, so no scanner behavior in the WP-33 findings is fixed even on master.
- Merged PRs list (https://github.com/goccy/go-yaml/pulls?q=is%3Apr%20is%3Amerged%20sort%3Acreated-desc): newest merge is #862 on Apr 7, 2026. Nothing merged in the six months since.

### 2.3 Maintenance activity

- Repo page (WebFetch https://github.com/goccy/go-yaml): 2.2k stars, 266 forks, 154 open issues, 88 open pull requests, 820 commits, MIT, not archived. README: "I'm looking for sponsors this library. This library is being developed as a personal project in my spare time." (`dl/goccy/master_README.md:416`), and "The main maintainer is @goccy, but we are also building a system to develop as a team with trusted developers" (line 38).
- Open PRs that fix WP-33 behaviors (section 2.5) received no review from goccy or shuheiktgw: WebFetch of #949, #941, #889, #902, #900 each reports "No reviews" and only the author plus codecov-commenter as participants.
- Issue #888 was closed by its own author on Sep 8, 2026 with "Based on repo activity I don't think this will get explored." (https://github.com/goccy/go-yaml/issues/888); "Neither goccy nor shuheiktgw took part."
- Recent issues filed by others carry no maintainer reply: #948 (Oct 7), #940 (Sep 23), #928 (Sep 2), #932 (Sep 14), #894 (Jul 17), #890 (Jul 11).
- goccy's own PR #865 "fix(ast): preserve indentation when replacing block-style nodes" (Apr 11, 2026) is still open (open-PR list page 3).
- pkg.go.dev: "Imported by: 2,983", "Published: Jan 8, 2026" (`curl https://pkg.go.dev/github.com/goccy/go-yaml`, regex on HTML).
- `go.mod` on master: `module github.com/goccy/go-yaml` / `go 1.21.0`, no requires (`dl/goccy/master_go.mod`). LICENSE identical at v1.19.2 and master (`diff` silent), "MIT License / Copyright (c) 2019 Masaaki Goshima".

### 2.4 Security

- https://github.com/goccy/go-yaml/security (WebFetch): "No security policy detected." "There aren't any published security advisories."
- OSV: `curl -X POST -d '{"package":{"name":"github.com/goccy/go-yaml","ecosystem":"Go"}}' https://api.osv.dev/v1/query` -> `count 0`.
- Issue #461 "Parsing malicious or large YAML documents can consume excessive amounts of CPU or memory" (https://github.com/goccy/go-yaml/issues/461): label changed from bug to question, closed Feb 16, 2025. goccy (Nov 29, 2024): "The decoder has already done the job correctly ..." and "guards are already in place to prevent stack overflow"; he stated no CPU, memory, depth or size limit; the only follow-up was an encoder option (PR #606).
- Issue #890 SIGSEGV on `key:\n}` (https://github.com/goccy/go-yaml/issues/890): a commenter (Aug 7, 2026) says it panics only up to v1.12.0; on v1.15.x..v1.19.2 and master the input is accepted and read as `key: }`. PR #900 (open, unreviewed) would make that `}` a syntax error "could not find '{' character corresponding to '}'" (https://github.com/goccy/go-yaml/pull/900).
- Open issue #797 "panic: strings: negative Repeat count" (Sep 30, 2025) is listed in the column/position search; not read further.

### 2.5 WP-33 findings versus upstream issues and PRs

All states read 2026-10-10 by WebFetch; "unreviewed" means the PR page shows "No reviews" and no maintainer comment.

| WP-33 behavior (round) | Upstream | State |
|---|---|---|
| Tab inside a double-quoted scalar: scanner steps past the closing quote (untabQuotes; harden2 RC3 fuzz failure) | Issue #948 "Literal tab characters in double-quoted strings cause parsing to skip closing double-quote" (Oct 7, 2026; v1.19.2; from hashicorp/terraform-provider-aws#50292); PR #949 | open, unreviewed |
| Tab inside a plain scalar dropped or changed (untabSeparators; harden2 RC2) | Issue #940 "A tab inside a plain scalar is removed when decoding" (Sep 23, 2026; reporter says a property-based tool found "about 29 other potential bugs"); PR #941 | open, unreviewed |
| Wide block mappings parse in quadratic time (WP-33 report) | PR #889 "parse block mappings iteratively to avoid O(n^2) parse time": 200,000 entries (4.2 MB) 20,271 ms before, 339 ms after; "about 161 GB allocated before" | open, unreviewed (opened Jul 9, 2026) |
| Nesting cost quadratic (path string per level); converter guard runs only after goccy's parse (harden depth major, harden2 RC0) | PR #902 "limit nesting depth to avoid quadratic parse time": `[`x400,000 (800 KB) 31 s; proposes a fixed `maxParseDepth = 10000`, not configurable | open, unreviewed (Jul 29, 2026). 10000 is far above Ruralz's MaxDepth 64 |
| A `}` outside a flow collection is ignored / read as plain text (harden2 RC0 part b) | Issue #890; PR #900 | open, unreviewed |
| Empty last sequence entry before a key at the same column is misnested (harden) | Issue #766; PR #850 | merged to master Feb 16, 2026; NOT released |
| Empty block scalar followed by a comment/blank line breaks the next scan; keep chomping (harden2 major 2) | Issues #824, #826; PRs #942, #880 | open |
| Explicit key `?` mispairing (fix round) | Issues #892, #950; PRs #952, #899, #947 | open |
| `:` inside flow collections read differently from YAML 1.2 (harden2 blocker 1, `{app:web}`) | PR #943 "Preserve colons in unquoted flow-map keys" (open); PR #866 "treat colon as plain scalar content in flow mapping keys" (closed Apr 24, 2026, unmerged); suite name `colon-at-the-beginning-of-adjacent-flow-scalar` is in goccy's own skip list | open / closed |
| Multi-line plain scalar in a flow mapping split | Issue #951 (Oct 8, 2026); PR #953 | open |
| Flow counters not reset at `---`, `{` on a directive line (harden2 RC0 part 1) | No issue or PR found: searches `directive` (11 results, none about this) and `"flow" is:issue` (23 results) | none |
| Null insertion moves every later token of the parse unit (harden2 RC1) | No issue or PR found. Issue #928 / PR #929 is a different quadratic path (printer `PrintTokens` on flow style: 16,000 entries 2,718 ms flow vs 48 ms block, measured on master edee2f9) | none |
| Comment-only document drops later documents; consecutive `---` | Issue #870; PRs #887, #934, #877 | open |
| BOM at stream start treated as content | Issue #906; PRs #907, #933 | open |
| `Token.Position.Offset` wrong after comments | Issue #856; PRs #857, #867 | open |

Cause lines confirmed in the v1.19.2 module cache: `scanner/scanner.go:51-52` declare `startedFlowSequenceNum` / `startedFlowMapNum`; `clearState()` at `scanner/scanner.go:1509-1514` resets only `prevLineIndentNum`, `lastDelimColumn`, `indentLevel`, `indentNum`.

### 2.6 YAML 1.2 claims versus code

- `token/token.go:301-304` (v1.19.2): "go-yaml should not treat these as reserved keywords at parsing time. as go-yaml is supposed to be compliant only with YAML 1.2." (already cited by the research doc).
- But `token/token.go:593` strips `_` from numbers and `:612` `case strings.HasPrefix(normalized, "0") && len(normalized) > 1 && dotCount == 0: base = 8` resolves `010` as octal 8, which is YAML 1.1 (YAML 1.2 core needs `0o`). Upstream issue #894 "0-prefixed integers interpreted as base 8" (open, Jul 17, 2026; PR #925 open; earlier PR #897 closed unmerged). Measured: probe prints `goccy#894 leading-zero int  input "a: 010\n"  goccy: {"a":8}` (section 6). WP-33 resolves core scalars in Ruralz code, so this does not reach the profile, but the research doc's "only candidate that claims YAML 1.2-only scalar resolution" needs this caveat.
- goccy's own YAML Test Suite harness (https://github.com/goccy/go-yaml/blob/master/yaml_test_suite_test.go, identical at v1.19.2: `cmp` silent) compares decoded values with `in.json` only and, for error cases, only checks that `Unmarshal` fails; it skips 47 names (28 marked "no json." and 19 known failures, counted with `awk '/var failureTestNames/,/^}/' | grep -c '^\s*"'`). The README figure ("nearly 60 additional test cases ( 2024/12/15 )", README line 35) is unchanged on master.
- https://matrix.yaml.info/ (data commit 6e6c296a, 2022-01-17) lists only one Go entry, `go-yaml-json`, row `224 6 49 29 79 15 303 70` (my reading: 303 pass, 70 fail of 402); goccy is not in the matrix. Version of that go-yaml row not shown.

## 3. go.yaml.in/yaml/v4 (yaml/go-yaml)

### 3.1 Who maintains it; takeover

- README (`main` and `v4.0.0-rc.6` identical, `diff` silent; lines 18-37): "This project started as a fork of the extremely popular go-yaml project, and is being maintained by the official YAML organization." "The YAML team took over ongoing maintenance and development of the project after discussion with go-yaml's author, @niemeyer, following his decision to label the project repository as "unmaintained" in April 2025." "We have put together a team of dedicated maintainers including representatives of go-yaml's most important downstream projects."
- Lines 42-49: "Versions v1, v2, and v3 will remain as frozen legacy. They will receive security-fixes only ... All ongoing work, including new features and routine bug-fixes, will happen in v4."
- Committers seen: every commit on `main` Sep 10-30, 2026 is committed by ingydotnet; authors ingydotnet and ccoVeille (https://github.com/yaml/go-yaml/commits/main); v3.0.5 commits by ccoVeille, thaJeztah, carloslima (https://github.com/yaml/go-yaml/compare/v3.0.4...v3.0.5). No CODEOWNERS, MAINTAINERS, GOVERNANCE or SECURITY file (raw.githubusercontent.com 404 for each). CONTRIBUTING.md:192-200 (rc.6): "We are a Work in Progress ... There are lots of opinions and ideas about how to do things, even within the core team."
- yaml.org lists it as "Supported by The YAML Company", "Actively maintained", "Pure implementation", license "Apache-2.0" (https://yaml.org/libraries/).

### 3.2 Release status and dates

- Proxy `.info` times: rc.1 2025-07-30, rc.2 2025-08-28, rc.3 2025-11-04, rc.4 2026-01-21, rc.5 2026-06-08, rc.6 2026-06-17 (`@latest` = v4.0.0-rc.6). No final v4.0.0.
- `main.info`: `v4.0.0-rc.6.0.20260930150120-823c494d8493`, 2026-09-30.
- https://github.com/yaml/go-yaml/releases: "There aren't any releases here" (tags only; open issue #114 "Create GitHub releases for the tagged releases we support").
- Issue #237 "v4.0.0 Release Planning" (open; Jan 4, 2026): "The go-yaml team is planning to release v4.0.0 in January 2026." Last comment (Jan 25, 2026, andig) asks that "the breaking rc4 changes are not rushed to release". Issue #147 "Stable version of v4.0" (open): ingydotnet Jan 4, 2026 "Hopefully it will happen this week or next." Nine months later there is still no v4.0.0.
- API churn continues on `main` after rc.6 (commits Sep 20-25, 2026): "Add parser plugin API and selectors", "Remove ComposeAndResolve", "Hide legacy loader and dumper setters", "Remove visibility of Options.From legacy", "Simplify the Decoder and Encoder". Issue #316 thread (ccoVeille, Mar 14, 2026): "This is an error we did by releasing the first -rc1 assuming the first v4.0.0 would be easy to be released."
- Activity: repo page 534 stars, 77 forks, 74 open issues, 46 open PRs, 826 commits; pkg.go.dev "Imported by: 376", "Published: Jun 17, 2026".
- `go.mod` (main and rc.6): `module go.yaml.in/yaml/v4` / `go 1.18`, no requires.

### 3.3 YAML 1.2 support claims and gaps

- README lines 54-66: "supports most of YAML 1.2, but preserves some behavior from 1.1 for backwards compatibility": YAML 1.1 bools "as long as they are being decoded into a typed bool value. Otherwise they behave as a string"; "Supports octals encoded and decoded as 0777 per YAML 1.1".
- Code (rc.6): `internal/libyaml/constructor.go:184-193` and `:308-322` map `y yes on ...` to true when the target is a Go bool; `internal/libyaml/resolver.go:161` `strconv.ParseInt(plain, 0, 64)` (so `010` -> 8; probe confirms `v4: {"a":8}`).
- `%YAML 1.2` is rejected: `internal/libyaml/parser.go:638-640` (rc.6) and `:696-698` (main 823c494): `if token.major != 1 || token.minor != 1 { return formatParserError("found incompatible YAML document", ...)`. Probe on rc.6 and on main: `v4: null err=go-yaml load error in parser at L1.C1: found incompatible YAML document` for `%YAML 1.2\n---\nfoo: 42\n`. Issue #146 "go-yaml doesn't support YAML 1.2 documents" (open, Oct 15, 2025), ingydotnet: "I'd rather not change anything about it until we have 1.2 fully supported." Spec 01 (`.claude/plans/m1/specs/01-config-load.md:72`) refuses only "a `%YAML` directive other than `1.2`", so v4 would need a Ruralz pre-pass for this.
- Open #144/#145: "go-yaml accepts invalid YAML integer notation that are valid in Go notation" / canonical notation (Oct 15, 2025).
- Escaped slash `"a\/b"` (YAML 1.2 escape, suite 3UYS) is in v4's known-failing list (section 3.4).

### 3.4 YAML Test Suite (self-reported by v4)

- `yts/test_suite_test.go` (rc.6 module cache) runs `./testdata/data-2022-01-17` (same data release as Ruralz's `test/fixtures/yaml-test-suite/SOURCE`, ref data-2022-01-17, commit 6e6c296a) with four subtests per case: LoadTest, EventComparisonTest (libyaml events vs `test.event`), MarshalTest, JSONComparisonTest (`reflect.DeepEqual` against `encoding/json`), skipping names in `yts/known-failing-tests`.
- Counts (`grep -v '^\s*$' | awk -F/ '{print $3}' | sort | uniq -c`): rc.1 235 lines over 152 cases; rc.6 225 lines over 149 cases (75 Event, 57 JSON, 83 Load, 10 Marshal); main (raw, 2026-09-30) 222 lines over 146 cases (75 Event, 57 JSON, 80 Load, 10 Marshal).
- Against Ruralz's 402 pinned cases (94 with `error`): of the 146 main-list cases, 26 are error cases (Load accepted invalid input in 18, event parser in 14) and 120 are valid cases (61 event mismatch, 62 load failure, 54 JSON mismatch). Cases passing all of Load/Event/JSON: 263 of 402.
- Overlap with Ruralz `expected-failures.txt` (87 entries; 30 profile:anchor, 22 profile:tag, 13 profile:complex-key, 5 profile:directive, the rest reviewed:goccy-v1.19.2): 87 of the 146 v4 cases (excluding Marshal-only) are NOT in Ruralz's list (73 valid, 14 error). Of those, 35 valid cases fail only JSON, and 33 of the 48 JSON-only failures have a bare number in `in.json` or are multi-document cases where the harness loads only the first document, so many JSON failures are harness artifacts. 31 valid cases fail Event+Load (examples 27NA `%YAML 1.2`, 2LFX reserved directive, 3UYS escaped slash, 4MUZ-00/01 flow colon on next line, 58MP `{x: :x}`). This is v4's own list, not a run of mine.

### 3.5 Limits and security

- Built-in limits (rc.6 `internal/libyaml/options.go:64-66`): `const maxDepth = 10000` "exceeded max depth of %d"; `:72-92` alias-ratio heuristic "document contains excessive aliasing" (400,000 / 4,000,000 construct thresholds). Same constants on main (`options.go:79-104`).
- `docs/plugins.md` (rc.6): "The limit plugin controls the maximum nesting depth and alias expansion allowed during parsing." Options `DepthValue(n)` "Max nesting depth (both flow and block)", `DepthNone()`, `DepthFunc(fn)`, `AliasValue(n)`, `AliasNone()`, `AliasFunc(fn)`; "Both bare NewLoader(data) and version presets ... include default limits equivalent to limit.New()". Source: `plugin/limit/plugin.go:134`. Whether `DepthValue(64)` stops the parse before cost is paid was not measured here (yamlv4 role).
- `docs/options.md:414-439`: `WithUniqueKeys` "When enabled, loading fails if duplicate keys are found" (default on).
- https://github.com/yaml/go-yaml/security: "No security policy detected." "There aren't any published security advisories." OSV query for `go.yaml.in/yaml/v4` and `go.yaml.in/yaml/v3`: `count 0`.
- Issue #316 "Memory corruption in decoder due to unsafe cast" (opened Mar 14, closed Mar 15, 2026; PR #318): type confusion through an unsafe `*Node` cast that checked only the type name. Module cache: rc.4 has 1 `reflect.NewAt(` site and 0 `isYAMLNodePkg`; rc.5 and rc.6 have the allowlist `internal/libyaml/structmeta.go:116-122` (`"gopkg.in/yaml.v3", "go.yaml.in/yaml/v3"`) used at `constructor.go:959`. No GHSA/CVE was issued.
- Open #358 "Potential panic in emitter with malformed UTF-8" (May 25, 2026); #167 "Get rid of panic and return an error where it is possible" closed Sep 19, 2026.
- Error positions: issue #288 "Cannot get source location of parse errors after yaml.ParserError was removed" (open since Feb 8, 2026, rc.4; no maintainer reply); #343 "BC break: odd line/column notation in errors" (ingydotnet, Jun 12, 2026: an "error" plugin API is planned). rc.6 exports `LoadError = libyaml.LoadError` / `LoadErrors` (`yaml.go:491-500`).

### 3.6 License

- LICENSE and NOTICE identical at rc.6 and main (`diff` silent). NOTICE: files `internal/libyaml/{api,emitter,parser,reader,scanner,writer,yaml,yamlprivate}.go` "are still covered by their original MIT license" (Copyright 2006-2010 Kirill Simonov); "All the remaining project files are covered by the Apache license" (Copyright 2011-2019 Canonical Ltd, 2025 The go-yaml Project Contributors). Matches the research doc's `Apache-2.0 AND MIT`. The GitHub sidebar shows only "Apache-2.0".

## 4. gopkg.in/yaml.v3 and go.yaml.in/yaml/v3

- https://github.com/go-yaml/yaml: "This repository was archived by the owner on Apr 1, 2025. It is now read-only." README first heading "THIS PROJECT IS UNMAINTAINED" (`dl/goyamlold/v3_README.md`, Gustavo Niemeyer).
- Proxy: `gopkg.in/yaml.v3` versions `v3.0.0 v3.0.1`, `@latest` v3.0.1 2022-05-27. pkg.go.dev "Imported by: 44,731".
- OSV for `gopkg.in/yaml.v3`: GHSA-hp87-p4gw-j4gq / CVE-2022-28948 / GO-2022-0603 "Denial of Service"/"Panic", fixed in 3.0.1. (`gopkg.in/yaml.v2` has 3 more DoS advisories, all fixed by 2.2.8.)
- Successor `go.yaml.in/yaml/v3`: v3.0.2 2025-06-02, v3.0.3 2025-06-02, v3.0.4 2025-06-29, v3.0.5 2026-07-26 (proxy). v3.0.4..v3.0.5 holds 10 commits (tag retraction, gofmt revert, CodeQL workflow backport, test-library moves, removal of `gopkg.in/check.v1`); none is described as a security fix (https://github.com/yaml/go-yaml/compare/v3.0.4...v3.0.5). pkg.go.dev "Imported by: 555".
- YAML 1.1 behavior for v3 is the same as in the v4 README section quoted above (typed bools, `0777`).

## 5. Other pure-Go candidates

| Candidate | State (evidence) | License | Fit |
|---|---|---|---|
| sigs.k8s.io/yaml | `@latest` v1.6.0 2025-07-24 (proxy), unchanged since the research doc; wraps `go.yaml.in/yaml/v2` (YAML 1.1 resolver, already in research doc) | MIT AND BSD-3-Clause (research doc) | no (YAML 1.1) |
| github.com/go-openapi/go-yaml | Hard fork of goccy. README (pinned commit c7b5c60): "Early days. The module path has changed, the API will change, and there is no release yet. If you want a stable version of this library today, use upstream". Claims: parser "scores 393 of them ... and agrees with all 393", "the YAML 1.2 core schema" by default, duplicate keys rejected by default, "the parser still reads a whole document". Proxy has only `v0.0.0-20261001105429-c7b5c60d8490`; repo 1 star, 0 forks, 1,626 commits; go.mod `go 1.25.0`, requires `github.com/go-openapi/testify/v2 v2.6.1`; pkg.go.dev "Imported by: 0". No depth or size limit documented (grep for depth/limit/fuzz/dos in README: none). | Apache-2.0, with goccy MIT terms in NOTICE | not adoptable now (no release); worth watching |
| YAMLStar (github.com/yaml/yamlstar, yamlstar-go) | "a pure YAML 1.2 loader implemented in Clojure"; module `github.com/yaml/yamlstar` v0.1.23 (2026-10-04) requires `github.com/ebitengine/purego`, `github.com/glojurelang/glojure`, `go4.org/unsafe/assume-no-moving-gc` and others (`proxy .../@v/v0.1.23.mod`); yamlstar-go v0.1.21/23 fail in the proxy ("case-insensitive file name collision: README.md and ReadMe.md"); claims "100% compliant" with no test-suite numbers | MIT | no (pre-1.0, Clojure runtime, many unvetted deps) |
| github.com/braydonk/yaml | Fork of yaml.v3 "maintained by @braydonk particularly for the interests of yamlfmt"; v0.9.0 2025-02-10 | as yaml.v3 | no (yaml.v3 behavior, formatter-focused) |
| github.com/zclconf/go-cty-yaml | v1.2.0 2025-12-18; requires `github.com/zclconf/go-cty`; libyaml-derived (LICENSE.libyaml); README not readable through raw (404) | Apache-2.0 (GitHub sidebar) | no (tied to cty) |
| github.com/mrpicc0lo/yaml-go | v1.0.3 2026-01-15; "Imported by: 0"; README claims "100% YAML 1.2 specification-compliant" with no test-suite results; badges point at `github.com/yourusername/yaml-go` | MIT | no |
| github.com/kylelemons/go-gypsy | v1.0.0 2020-08-30; "Simplified YAML parser" (yaml.org) | Apache-2.0 | no |
| github.com/go-faster/yaml | v0.4.6 2023-04-24 (proxy), yaml.v3 fork | not checked | no (inactive) |

yaml.org/libraries lists for Go only yaml/go-yaml, go-gypsy, goccy/go-yaml ("Supports YAML 1.2"), YAMLStar and YAMLScript (https://yaml.org/libraries/). A web search found no other maintained pure-Go parser with independent YAML Test Suite results.

YAML Test Suite itself: newest data tag is still `data-2022-01-17` (https://github.com/yaml/yaml-test-suite/tags).

## 6. Probe (my scratch module, released versions)

Command: `cd probe; GOFLAGS=-mod=mod go get github.com/goccy/go-yaml@v1.19.2 go.yaml.in/yaml/v4@v4.0.0-rc.6; go run .` (output in `probe/out-release.txt`). Both libraries unmarshal into `any`, printed as JSON.

```
goccy#948 tab in double-quoted   "a:\n  b: \"x\ty\"\n  c: y\n"
  goccy: null err=[2:14] value is not allowed in this context. map key-value is pre-defined
  v4:    {"a":{"b":"x\ty","c":"y"}}
goccy#940 tab in plain scalar    "k: a\tb\n"
  goccy: {"k":"ab"}            v4: {"k":"a\tb"}
goccy#894 leading-zero int       "a: 010\n"
  goccy: {"a":8}               v4: {"a":8}
goccy#766 empty seq item         "a:\n- \nb: 1\n"
  goccy: {"a":[{"b":1}]}       v4: {"a":[null],"b":1}
yaml/go-yaml#146 %YAML 1.2       "%YAML 1.2\n---\nfoo: 42\n"
  goccy: {"foo":42}            v4: null err=go-yaml load error in parser at L1.C1: found incompatible YAML document
YAML 1.1 bool word               "a: on\nb: yes\n"
  goccy: {"a":"on","b":"yes"}  v4: {"a":"on","b":"yes"}
```

Same probe with `github.com/goccy/go-yaml@v1.19.3-0.20260407131736-edee2f91616c` and `go.yaml.in/yaml/v4@v4.0.0-rc.6.0.20260930150120-823c494d8493` (go.mod restored afterwards): identical except goccy#766 now gives `{"a":[null],"b":1}` (PR #850), and v4 main still refuses `%YAML 1.2`.

## 7. Gaps

- GitHub REST/MCP access was blocked; issue and PR text comes from WebFetch summaries, which may paraphrase. Counts (154 open issues, 88 open PRs) are page values on 2026-10-10.
- Upstream searches used the first result page of each GitHub search; an older or differently worded issue about flow counters at `---` or null-insertion cost could exist.
- v4 YAML Test Suite numbers are v4's own `known-failing-tests` list, not a run made here; the JSON subtest has harness artifacts. The yamlv4 role measures v4 directly.
- go-openapi/go-yaml and YAMLStar claims are self-reported and unverified.
- The v4 limit plugin's effect on parse cost (does `DepthValue(64)` stop before the cost is paid) was not measured here.
