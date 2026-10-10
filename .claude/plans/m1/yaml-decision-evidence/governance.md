# YAML parser decision: what each option requires in this repository (role: governance)

Read on 2026-10-10 from `/home/user/ruralz` (branch `develop`, HEAD `985df34`). This role changed no repository file and ran no git command that changes state. `git status --short | wc -l` printed `0` mid-session. At the end it printed `3`: the untracked directories `internal/config/canonical/`, `internal/config/precedence/` and `internal/filter/validation/`. Other jobs running in this container (S2 work packages) created them, and this role did not touch them. The only commands that touched anything outside the scratch directory were read-only Go commands (`go list`, `go mod graph`, `go mod why`, `go doc`, and `go run ./internal/tool/commitcheck title`); `go mod why` filled the shared module cache with test-only modules, and nothing in the repository changed.

Options under review:

1. Keep `github.com/goccy/go-yaml` v1.19.2 and keep extending the guard layer in `internal/config/profile`.
2. Switch to `go.yaml.in/yaml/v4`, whose newest tag is `v4.0.0-rc.6`.
3. Write the parser by hand, with no third-party YAML module, or with goccy (or yaml v3) kept only as a test oracle.

## 1. The rules that decide what each option needs

| Rule | Where |
|---|---|
| Never invent libraries; a missing one becomes an `OQ-<docslug>-<n>` row | `AGENTS.md:37` |
| A change that contradicts a doc needs an ADR or an Open questions entry in that doc, in the same change | `AGENTS.md:38`, `CONTRIBUTING.md:69`, foundation pack section 14 (`docs/_meta/foundation-pack.md:485`) |
| Behavior or decision changes update `docs/` in the same change | `AGENTS.md:39` |
| depguard admits only the standard library, this module and the `admitted` list, in every file including tests | `AGENTS.md:64`, `.golangci.yml:22-48` |
| A new module needs, in the same change: a catalog row, an `admitted` entry, and a passing depgate. `go.mod` edits need two approvals | `.claude/rules/go-code.md:56`, `CONTRIBUTING.md:62` |
| Two approvals are needed for `docs/_meta/` (research included), `go.mod`, `api/`, `pkg/`, `.github/` and the security-sensitive packages | `scripts/ci-approvals.sh:28` (regex `^(api/\|pkg/\|docs/_meta/\|go\.mod$\|...)`), `AGENTS.md:89`, `CONTRIBUTING.md:67` |
| An accepted ADR changes only through a new ADR that supersedes it. The old ADR changes only its front-matter `status` | `.claude/rules/adr.md:23`, `docs/README.md:215` ("an ADR changes only through a new or superseding ADR") |
| The chosen option must match the foundation pack section 7 row. A changed row is a `docs/_meta/` change: two approvals and a section 14 entry | `.claude/rules/adr.md:24` |
| An ADR names a rejected library only when the tech stack doc lists it under "Alternatives not chosen". 400 to 1,500 words | `.claude/rules/adr.md:16-17` |
| A claim about an external library cites a URL already present in `docs/_meta/research/*`. A new fact needs a research addendum first | `.claude/rules/docs.md:30`, `docs/engineering/02-repository-layout-and-conventions.md:587` |
| A design may depend only on a library with a catalog row | `.claude/rules/docs.md:31` |
| `docs/_meta/` needs two approvals. The binding files also need an ADR or OQ entry; "a research addendum does not" | `.claude/rules/docs.md:51` |
| `/new-adr`: evidence must be in research first, then full bookkeeping (both indexes, manifest, foundation pack sections 2, 7 and 14, owning doc, catalog row and "Alternatives not chosen", every doc citing the old ADR). Do not choose the option: the user decides | `.claude/skills/new-adr/SKILL.md:10,16-17,25-31`. The skill is `disable-model-invocation: true` and runs only when the user types it (`CLAUDE.md`) |
| Research addenda and `docs/_meta` changes are the user's decision | `.claude/plans/m1/README.md:58` ("ask the user only what is theirs to decide (ADRs, `docs/_meta/`, ...)"). `.claude/plans/m1/user-decisions.md:4-7`: the CI tooling addendum was approved, and "Not approved: the Go net/http research addendum (request 170)" |
| Commits go straight to `develop` with no pull requests, so the `approvals` job never runs. The user's explicit approval stands in for the two approvals | `.claude/plans/m1/README.md:72` |
| Docs near their word caps must take word-neutral edits | `.claude/rules/docs.md` (Structure, last bullet). Counts in section 2 |

## 2. Where goccy is wired in today (measured)

| Item | Evidence |
|---|---|
| `go.mod` direct requirement | `go.mod:9` `github.com/goccy/go-yaml v1.19.2` |
| depguard admission | `.golangci.yml:35` `- github.com/goccy/go-yaml` |
| depguard confinement | `.golangci.yml:77-81`, rule `goyaml`: files `["$all", "!**/internal/config/profile/**"]`, deny `github.com/goccy/go-yaml`, desc "the restricted YAML profile is the only YAML parser and emitter" |
| Module pin | `internal/tool/modpin/modpin.go:14` `_ "github.com/goccy/go-yaml"` (`//go:build tools`). While this line stays, `go mod tidy` keeps goccy in `go.mod` |
| Importers | `grep -rln '"github.com/goccy' --include=*.go .`: 17 files in `internal/config/profile/` plus `internal/tool/modpin/modpin.go` |
| Binaries | `go list -deps ./cmd/{ruralzd,ruralz-control,ruralz} \| grep -i yaml` prints nothing. No binary links any YAML module yet (they are still stubs), so depgate sees none today |
| Package size | `wc -l` of the production `.go` files in `internal/config/profile`: 6,232. Tests: 6,902 |
| Other YAML modules already in the graph | `go mod graph`: `cel.dev/cel-go@v0.32.0 go.yaml.in/yaml/v3@v3.0.4`, plus jwx, rueidis, otelslog, grpc-gateway and prometheus/common to v3.0.5. `go.mod:92` has `go.yaml.in/yaml/v3 v3.0.5 // indirect`. `CGO_ENABLED=0 go list -deps cel.dev/cel-go/cel` lists `go.yaml.in/yaml/v3`, so once `internal/cel` links into the binaries, every option ships yaml v3 as a second YAML implementation (soft criterion S6; spec 03 risk 17 at `.claude/plans/m1/specs/03-cel.md:668`) |
| ADR-0003 text naming goccy | `docs/adr/0003-configuration-format.md:35` (option 1 "using goccy/go-yaml"); `:49` ("depth checked over `lexer.Tokenize` output", a goccy API); `:52` Libraries row "`goccy/go-yaml` for tokens and AST"; `:76` Figure 1 "Restricted profile parse (goccy/go-yaml)"; `:93`, `:95`, `:96` Consequences; `:104` Confirmation "YAML Test Suite against `goccy/go-yaml`"; `:115` Pros |
| ADR-0003 checklist | `:108` "a pull request accepting a new YAML feature, file format or envelope key MUST amend this ADR" (amend means supersede under `.claude/rules/adr.md:23`) |
| ADR-0003 S1 clock | `:96`: goccy's last release (2026-01-08) "leaves the S1 window on 2027-01-08, triggering a health review", 90 days after today |
| Tech stack catalog | `docs/engineering/01-tech-stack-and-libraries.md:87` YAML 1.2 parser row (Alternatives column: "go-yaml v4 (`go.yaml.in/yaml/v4`); sigs.k8s.io/yaml (YAML 1.1 resolver)"). Figures 1 and 2 nodes `yaml["goccy/go-yaml"]` at `:145`, `:192`, edges at `:168`, `:213`, `:215`. "Alternatives not chosen" (`:380-395`) has no YAML row. The watch list (`:366-373`) has no goccy row |
| Foundation pack | `docs/_meta/foundation-pack.md:109` (section 5 heading "(ADR-0003)"), `:131` Config row (ADR-0003), `:152` YAML 1.2 parser row (goccy, "Selected at freeze"), `:508` section 14 freeze decision OQ-tech-stack-and-libraries-12 / OQ-configuration-model-16 (goccy), `:56` ADR range `ADR-0001`..`ADR-0019` |
| Configuration model | `docs/architecture/02-configuration-model.md:59` ("Depth is counted over `lexer.Tokenize` output before parsing, so over-deep input never reaches the parser") and `:61` ("The loader parses with `github.com/goccy/go-yaml`") |
| Layout doc | `docs/engineering/02-repository-layout-and-conventions.md:292`, Banned imports row for `github.com/goccy/go-yaml` outside `internal/config/profile` |
| Testing doc | `docs/engineering/03-testing-and-quality-strategy.md:189` cites goccy's README for self-reported suite results |
| Research | `docs/_meta/research/tooling-and-licenses.md:139-157` (section 6: goccy, go-yaml v4, v3, sigs.k8s.io/yaml) and `:405-423` (URL list) |
| Docs citing ADR-0003 | `grep -rln "ADR-0003\|0003-configuration-format" docs README.md`: 20 files. Eleven carry ADR-0003 in their front-matter `adrs`: docs/README, features catalog, roadmap, CLI reference, tech stack, layout, release, testing, configuration model, system overview, control plane. The manifest lists it at `docs/_meta/manifest.yaml:442,686,785,1094,1708,1847,2580` (per-doc lists) and `:2703` (ADR entry). ADR-0007, ADR-0016 and ADR-0019 cite it in accepted bodies, which stay unchanged |
| Next free IDs | ADR-0020 (highest `docs/adr/0019-*`). OQ-configuration-model-22 and OQ-tech-stack-and-libraries-26 (highest used anywhere in `docs` and `.claude/plans`: 21 and 25). R-77 in `specs/00-architecture.md` 2.16 (last is R-76 at `:6634`) |

Word counts (`python3 -I .claude/plans/m1/tools/wordcount.py <files>`) against the manifest `length_words` caps:

| Doc | Words | Cap | Headroom |
|---|---|---|---|
| `docs/architecture/02-configuration-model.md` | 9,498 | 9,500 (`manifest.yaml`, configuration-model `length_words`) | 2 |
| `docs/engineering/01-tech-stack-and-libraries.md` | 4,931 | 5,000 | 69 |
| `docs/engineering/02-repository-layout-and-conventions.md` | 4,986 | 5,000 | 14 |
| `docs/engineering/03-testing-and-quality-strategy.md` | 4,939 | 5,000 | 61 |
| `docs/adr/0003-configuration-format.md` | 1,175 | 1,500 (`adr_common`) | 325 (body frozen) |
| Precedent ADR-0019, which superseded ADR-0011 by restating it in full | 1,496 | 1,500 | 4 |

depgate classification of each candidate's license files, using depgate's own phrase tests (`internal/tool/depgate/license.go:51-96`, which checks Apache before MIT). Python over the module cache:

```
go.yaml.in/yaml/v4@v4.0.0-rc.6/LICENSE  apache2: True  mit: False   -> "Apache-2.0"
go.yaml.in/yaml/v3@v3.0.5/LICENSE        apache2: True  mit: True    -> "Apache-2.0" (Apache checked first)
github.com/goccy/go-yaml@v1.19.2/LICENSE apache2: False mit: True    -> "MIT"
```

v4's MIT part (eight `internal/libyaml` files) is declared only in `NOTICE`. depgate does not read `NOTICE` (`licenseFileName` accepts LICENSE/LICENCE/COPYING/UNLICENSE only, `license.go:15-23`). `go.mod` of v4 rc.6: `module go.yaml.in/yaml/v4` / `go 1.18`, so it passes S2.

## 3. Option 1: keep goccy and continue the guard layer

ADR: no new ADR is required. ADR-0003 still holds:

- goccy is still the parser, used "for tokens and AST" (`:52`).
- Confirmation `:104` already allows "a checked-in list of profile-rejected or reviewed cases"; `expected-failures.txt` has 87 entries, 66 `profile:*` and 21 `reviewed:goccy-v1.19.2/...`.

There is one open contradiction. The profile refuses valid YAML 1.2 beyond the features the configuration model lists. Examples are `[a: ]`, `a: !!str\nb: 1` and a tab-only line between entries (`internal/config/profile/doc.go`, TestGoccyLimitations). The model says flow style is accepted (`02-configuration-model.md:48-57`). Under `AGENTS.md:38`, that needs an Open questions row in the configuration model: OQ-configuration-model-22 (refused valid YAML 1.2 shapes, options (a) list them in the loader reference, (b) fix upstream, (c) change parser). The doc has 2 words of headroom, so the row must be paid for by tightening other text.

Catalog row (`01-tech-stack-and-libraries.md:87`): no change to Chosen. Recommended additions:

- A watch list row for goccy, signal "last release 2026-01-08". This uses `https://github.com/goccy/go-yaml/releases/tag/v1.19.2`, already at `tooling-and-licenses.md:405`. Trigger: no release by 2027-01-08 (S1). Fallback: options 2 or 3.
- Optionally, extend the Risk column.

Both fit in the 69 words of headroom.

Research addendum: not required for that minimum. It is required if any doc cites the upstream state: open unreviewed PRs #889, #902, #948/#949 and #940/#941, issue #894 (octal `010`), issue #461 (no limits), and nothing merged since 2026-04-07. The upstream role lists these URLs as "NOT cited anywhere in docs/_meta/research". An addendum needs the user's approval and two approvals (`scripts/ci-approvals.sh:28`), but no ADR or OQ entry (`.claude/rules/docs.md:51`).

go.mod and depguard: none.

Foundation pack: none, unless the user wants the section 7 row's claim ("the only researched candidate that claims YAML 1.2-only scalar resolution") qualified, which needs research first.

Spec and architecture text (lead edits under `.claude/plans/m1`, no approvals):

- `specs/01-config-load.md`:
  - Req 6 (`:64`) and risk 1 (`:792`): MaxDocumentTokens 1,000,000 becomes 400,000 (hypothesis), and the per-file bound is added.
  - Req 8 (`:69-74`): "produced incrementally by scanner.Scanner" is not what the code does (CR in `w3-s1a-result.json[0].contractRequests[0]`). The refusal lists also go here (section 8 below).
  - Req 9 (`:75`): the refined depth rule and the converter's second depth bound.
  - Req 10 (`:76`): parse entry by entry, not with one `parser.Parse` per document.
  - Req 12 (`:78`): quoted, block and multi-line plain scalars are read from the source, and a final line break is assumed.
  - Section 3 API block (`:361-380`): it still shows `Limits` and no `error`, `EncodeWith`, `EncodeJSON` or `FormatOf`, so it must match the committed API (CR4 and CR5).
  - The loader `Limits` comment (`:570`), the test plan reasons `reviewed:<goccy issue>` (`:743`), and the section 7 OQ-configuration-model-18 row (`:771`).
- `specs/00-architecture.md`: the 1.2 row (`:114`) is unchanged. The 4.4 WP-33 scope (`:7172`) needs "incremental scanner token pass" and "MaxDocumentTokens 1,000,000" corrected. Add a new R-77 for the token and file bounds.
- `arch/wps.json` WP-33 scope: same correction.
- `specs/11-ops-quality.md` req 17 and 26 (`:59`, `:74`): the second depth bound, and the "4 x source size" bound, which WP-33 does not meet (yamlv4 evidence section 8 item 7).

Exported API: unchanged. The only possible addition is an `Options` field for the per-file bound, if that is the chosen fix for the harden2 major. Adding a named field breaks no caller.

## 4. Option 2: switch to `go.yaml.in/yaml/v4`

### ADR

A new ADR-0020 must supersede ADR-0003, because the accepted body names goccy at `:35,49,52,76,93,95,96,104,115`. A narrower ADR cannot work: an accepted ADR cannot be partly superseded or edited (`.claude/rules/adr.md:23`). So ADR-0020 restates the whole format decision: YAML 1.2, JSON Schema 2020-12, envelope, profile, overlays, CRD mirror. ADR-0019 is the precedent, at 1,496 words. ADR-0003 is 1,175 words, so a restatement plus a library rationale must fit under 1,500.

The user chooses status `proposed` or `accepted` (`.claude/skills/new-adr/SKILL.md:10`). ADR-0018 and ADR-0019, the precedents, were `accepted`. Bookkeeping, per `SKILL.md:25-31`:

- `docs/adr/README.md`: new row; ADR-0003 status; count sentence becomes "of the 20 ... 15 accepted, 2 proposed, 3 superseded" if ADR-0020 is accepted.
- `docs/README.md`: row; Summary count; adrs list; the "ADR-0001 to ADR-0019" range at `:215`.
- `docs/_meta/manifest.yaml`: a new `adrs:` entry; ADR-0003's status at `:2703`; docs-index "lists all 19 ADRs" at `:411` and `min_table_rows`; owning-doc adrs lists.
- Foundation pack: section 2 range (`:56`), section 5 heading (`:109`), section 7 Config (`:131`) and YAML parser (`:152`) rows, and a section 14 dated entry.
- Every doc stating the decision: cite ADR-0020, add it to the front-matter `adrs` and the manifest per-doc lists (7 manifest lists, 11 front matters), bump `last_updated`, word-neutral.

All `docs/_meta` parts need two approvals.

### Catalog and tech stack doc

- Rewrite row `:87`:
  - Chosen: `go.yaml.in/yaml/v4` at a pinned rc.
  - Alternatives: goccy/go-yaml; sigs.k8s.io/yaml.
  - License: `Apache-2.0 AND MIT` (NOTICE).
  - Risk: a release candidate with no v4.0.0. It refuses `%YAML 1.2`, and rc.4 to rc.6 have a token-retention regression (yamlv4 evidence section 0, about 500 B of heap per input byte for root flow).
  - ADR: ADR-0020.
- Figures 1 and 2: relabel the nodes at `:145` and `:192`.
- "Alternatives not chosen": a goccy row with a research URL, required because ADR-0020 names it as rejected (`.claude/rules/adr.md:16`). A sigs.k8s.io/yaml row too, if the restated options name it as ADR-0003 does at `:36`. That is a gap today: it is named, but not in that table.
- Soft-criterion exceptions paragraph (`:48`): v4 has no `context.Context` (S3) and no token API or memory cap (S4), per yamlv4 evidence section 1 and section 8 items 3 and 4.
- Watch list: v4, trigger "no v4.0.0, or the retention fix is not released".
- The update policy (`:336`) already requires a named reviewer for minor bumps of pre-1.0 modules. Treat rc bumps the same way: the API changed across rc.3, rc.4 and rc.5 (yamlv4 evidence `:55`).
- All of this must fit in 69 words of headroom, so existing text must be tightened.

### Research addendum (required)

The facts ADR-0020 and the row need that are not in research:

- The v4 tag list and `@latest` from the proxy (rc.6, 2026-06-17; main pseudo-version 2026-09-30).
- `docs/plugins.md`: the depth and alias limit plugin.
- `docs/options.md`: `WithUniqueKeys`, which applies to Go maps, not `yaml.Node`.
- Issue #146: `%YAML 1.2` is refused.
- go.mod `go 1.18`.
- No public token or event API, and no context.

Section 6 already has the v4 README, LICENSE, NOTICE and tags URLs (`tooling-and-licenses.md:146,154`). The addendum needs the user's approval and two approvals.

### go.mod, depguard and depgate

- go.mod: add `go.yaml.in/yaml/v4 v4.0.0-rc.6`, or a pseudo-version, or a fork, and remove `github.com/goccy/go-yaml`. In `modpin.go`, drop line 14. Two approvals.
- If the retention patch is needed, there are two routes:
  - A fork module plus a `replace` directive, or a new module path. Each needs its own catalog row and research. Apache-2.0 section 4(b) requires notices on modified files.
  - Vendoring into the repository is ruled out by the repository's own lint. v4 rc.6 has three `init()` functions (`internal/libyaml/{resolver,structmeta,node}.go`), banned by gochecknoinits. It has package-level `var` in 10 files and non-Revington headers (goheader). It is 16,106 non-test lines.
- depguard: in `admitted`, replace `github.com/goccy/go-yaml` with `go.yaml.in/yaml/v4`. The existing `go/` entry already covers the "go." prefix quirk (`.golangci.yml:27-29`). The `goyaml` rule must deny `go.yaml.in/yaml/v4` outside `internal/config/profile`. Update the layout doc row at `:292`.
- depgate: no `policy.go` change, since it classifies v4 as Apache-2.0 (section 2). Release obligation: carry v4's `NOTICE` in `THIRD_PARTY_LICENSES` (Apache-2.0 section 4(d)). WP-54 (`depgate notices`, whose scope already names "go.yaml.in/yaml/v3, option/v3") must add v4.
- `advisoryFloors()`: issue #316 (memory corruption, fixed in rc.5) has no GHSA or CVE (upstream evidence section 3.5). By the "advisory" rule it gets no floor; pinning at or above rc.5 is a review decision.

### Spec and architecture text

- `specs/01`: req 8 to 10 (`:69-76`) are written against goccy's scanner, `lexer.Tokenize`, `parser.Parse` and `AllowDuplicateMapKey`, so all three need rewriting. Also req 12 ("decoded by goccy", `:78`), req 14 (goccy rune-based positions, `:80`), section 3 table (`:212`), the depguard text (`:223`), section 5 Libraries row (`:691`), risks 1 to 3 and 15 (`:792-806`), and the test plan (`:739`, `:743`).
- `specs/00`: the 1.2 row's third-party column (`:114`), the 1.3 depguard block (`:212`, `:228-230`), the 4.4 WP-33 scope (`:7172`), the section 5 module table (`:7709`), and a new R-77.
- `specs/02`: `:605`, and req 45 (`:144`), which cites "ADR3 § Decision outcome (Libraries row)".
- `specs/10`: `:611`.
- `specs/11`: req 18 (`:60`, "against `goccy/go-yaml`"), `:275` and `:582`.
- `specs/03` risk 17: `:668`.
- wps.json: WP-33 and WP-54 scopes.
- Docs:
  - `02-configuration-model.md:59,61`: word-neutral, with 2 words of headroom.
  - Testing `:189`: the source becomes v4's README, already in research.
  - Layout `:292`.

## 5. Option 3: hand-written parser

### 3a. No third-party YAML module

ADR: ADR-0020 must supersede ADR-0003, with the same full restatement and bookkeeping as option 2. The Libraries row becomes Ruralz code. The considered options name goccy and v4 as rejected, so the tech stack doc needs "Alternatives not chosen" rows for both, with research URLs. Those URLs already exist: goccy README and token.go; v4 README (`tooling-and-licenses.md:145-146`).

Catalog row: precedent is the ULID and CLI framework rows (`01-tech-stack-and-libraries.md:85-86`):

- Chosen: "own code in `internal/config/profile`".
- License: "Apache-2.0 (Ruralz code)".
- CGO: "Ruralz code".
- ADR: ADR-0020.
- Alternatives: goccy, v4, sigs.k8s.io/yaml.
- Figures 1 and 2: remove the `yaml` node and its three edges.

Optionally, record it as a closed OQ-tech-stack-and-libraries-26, as with -15 and -18.

Research addendum: not strictly required if ADR-0020 argues from Ruralz evidence alone. That evidence is the review history (12, 8, 4 and 5 confirmed findings), the spike measurements, and research URLs already present. Required if it cites goccy's upstream state (unreviewed PRs, no release since 2026-01-08) or v4's later state.

go.mod: remove `github.com/goccy/go-yaml` and modpin line 14, then `go mod tidy`. `go mod graph` shows that only Ruralz requires goccy (`github.com/ravindu-rev/ruralz github.com/goccy/go-yaml@v1.19.2`), so it leaves the graph. Two approvals.

depguard:

- Delete the `admitted` entry at `:35`.
- Either delete the `goyaml` rule or widen it to deny `github.com/goccy/go-yaml`, `go.yaml.in/yaml`, `gopkg.in/yaml` and `sigs.k8s.io/yaml` everywhere. The strict `admitted` list already blocks them; the rule only records intent.
- The layout doc row at `:292` changes to match.

depgate: nothing.

Foundation pack: change row `:152` to Ruralz code, with the same section 2, 5, 7 and 14 edits as option 2.

Spec and architecture text: the same places as option 2:

- specs/01 req 8 to 10, 12 and 14, section 3 table "Third-party import: none", depguard text, section 5 row removed, risks, test plan.
- specs/00 `:114` "none", `:212`, `:228-230`, `:7172`, `:7709` row removed.
- specs/02 `:605`; specs/10 `:611`; specs/11 `:60`, `:275`, `:582`.
- CM `:59,61`.
- Testing `:189`: its claim that "the selected libraries self-report" no longer covers YAML.

Commit types. The code change is behavior-changing (refusal sets differ), so `fix(config)` or `feat(config)`, not `refactor`. Titles checked with `go run ./internal/tool/commitcheck title`: `fix(config): parse the restricted YAML profile with Ruralz code` ok; `build(deps): drop goccy/go-yaml` ok; `docs: add ADR-0020 superseding ADR-0003 for the YAML parser` ok.

### 3b. goccy kept only as a test oracle

A differential fuzz target in `internal/config/profile` tests. depguard applies to tests (`AGENTS.md:64`), so the `admitted` entry and the `goyaml` confinement stay. `go.mod` keeps goccy as a direct requirement, imported only from `_test.go`. depgate ignores it (`go list -deps -test=false`, `gate.go:39`).

The catalog row becomes test-only, like the "Test tooling (not shipped)" row (`:89`, CGO "Not linked"). The S1 date (2027-01-08) still applies to a dependency kept in `go.mod`. ADR-0020 is still required.

The handwritten role notes that WP-33's 6,000-line goccy layer cannot serve as the oracle once goccy is out of production. So run the differential before goccy is dropped, or keep a smaller oracle.

### 3c. go.yaml.in/yaml/v3 as the test oracle

v3 is already in `go.mod:92` as indirect, through cel-go. Using it directly in tests needs:

- moving it to a direct requirement (two approvals);
- an `admitted` entry and a confinement rule;
- a test-only catalog row, backed by research section 6, which already covers v3 at `:147,155-156`.

## 6. Side by side

| Requirement | 1 keep goccy | 2 go-yaml v4 | 3a hand-written | 3b hand-written + goccy test oracle |
|---|---|---|---|---|
| ADR | None (ADR-0003 holds) | ADR-0020 superseding ADR-0003, full restatement | ADR-0020 superseding ADR-0003 | ADR-0020 superseding ADR-0003 |
| Foundation pack (`docs/_meta`, 2 approvals, section 14) | None | Sections 2, 5, 7 (Config, YAML rows), 14 | Same as 2 | Same as 2 |
| Catalog row `:87` | Unchanged; add watch list row | Rewrite; Alternatives not chosen rows for goccy (and sigs.k8s.io/yaml); S3/S4 exceptions; watch list | Rewrite as Ruralz code; Alternatives not chosen rows for goccy and v4 | As 3a plus test-only goccy row |
| Research addendum (user + 2 approvals) | Only if upstream state is cited | Required | Only if upstream state is cited | Only if upstream state is cited |
| go.mod (2 approvals) | None | Add v4 (or fork), drop goccy | Drop goccy | Keep goccy (test-only) |
| depguard | None | Swap admitted entry and confinement | Delete or widen to a deny-all | Unchanged |
| depgate / release | None | NOTICE in THIRD_PARTY_LICENSES (WP-54) | None | None |
| CM doc (2-word headroom) | OQ-configuration-model-22 for refused valid YAML | `:59,61` plus an OQ row for v4's refusals | `:59,61` | `:59,61` |
| specs/01 req 8 to 10 | Wording and refusal lists | Rewrite | Rewrite | Rewrite |
| specs/00 `:114`, `:7709`, 1.3 block | Unchanged | Change | Change | `:7709` "tests only" |
| Exported API | Unchanged (maybe an added Options field) | Signatures unchanged; MaxDocumentTokens, Yield and ctx semantics weaken | Unchanged | Unchanged |

## 7. Who uses the profile API, and can it stay unchanged

Exported API today (`go doc -all ./internal/config/profile`):

- `Parse(ctx, src, file, path, f Format, o Options) ([]Document, diag.List, error)`.
- `Options{MaxBytes, MaxDepth, MaxDocumentTokens, MaxDiagnostics, Yield}`. Its unexported, goccy-only test hooks are `splitAt`, `noRewrite` and `passDepth` (`profile.go:108-119`).
- `Document{Root, Start}`.
- `Encode`, `EncodeWith`, `EncodeJSON`, and `EncodeOptions{Verbatim}`.
- `Format`, `FormatYAML`, `FormatJSON`, `FormatOf` and `Format.String`.
- `DefaultMaxBytes`, `DefaultMaxDepth`, `DefaultMaxDocumentTokens` (400,000) and `DefaultMaxDiagnostics`.
- `ErrEncode` and `ErrFormat`.

Current Go callers outside the package: none. `grep -rn "profile\." --include=*.go internal pkg cmd | grep -v internal/config/profile/` prints nothing.

Planned consumers (`arch/wps.json`, `specs/00-architecture.md`, specs):

| WP (wave) | Uses | Evidence |
|---|---|---|
| WP-55 Loader (4) | `Parse`, `FormatOf`, `Options` limits incl. `MaxDocumentTokens` and `Yield`, `Document`, `ErrFormat`; `LoadEnvironments` (the `--environments` file) | wps.json `dependsOn` WP-33; `specs/00:145`; `specs/01:566-571` loader `Limits.MaxDocumentTokens`; CR4 in `w3-s1a-result.json[0].contractRequests[4]`; `specs/10:99` |
| WP-58 Render (4) | `Encode`, `EncodeWith(...Verbatim)` for convert, `EncodeJSON` for `--output json` | wps.json scope "profile.Encode"; `specs/00:148`, `:7398`; CR5 |
| WP-69 Pipeline facade (5) | Through the loader: limits and worker yield | wps.json scope (limits, worker count) |
| WP-74 CLI bundle commands (6) | Through the pipeline and render; limits flags (none in M1) | wps.json |
| WP-75 Configuration conformance (6) | Golden diagnostics text and JSON for RZ-CFG-001 to 004: profile messages and positions; hostile corpus | wps.json scope |
| WP-77 CLI launcher and dev commands | `ruralz dev run --secret-overrides FILE`, "a YAML mapping (area 1 restricted profile)" | `specs/10-cli.md:169` (req 78); WP-77 scope "10 2.9 70-85" |
| WP-78 Property suites (7) | YAML and JSON digest equality through `pipeline.Run` | wps.json |
| WP-73 Activation and LKG (6) | `FromResources` skips the parse; file-mode reload goes through the loader | wps.json |
| WP-37, WP-38, WP-39, WP-51 (S2) | Told they may use "only its exported API (Parse, Options, Encode, EncodeJSON, FormatOf), which any parser change keeps" | `forwards.json:97,114,208,221` |
| WP-54 Release tooling | Notices and license classification of transitive modules | wps.json scope |
| WP-27 (done) | `scripts/update-test-suites.sh` and fuzzplan. Fuzz targets are discovered by `go test -list '^Fuzz'`, so renames need no registration | `internal/tool/fuzzplan/main.go:13-15` |

Verdict per option:

- **Option 1: unchanged.** The open choice is whether the per-file bound becomes its own `Options` field. That is additive and breaks no named-field caller.
- **Option 2: the signatures can stay, but three contracts weaken.**
  1. `MaxDocumentTokens` cannot be counted, because v4 has no public token or event API (yamlv4 evidence `:79`, `:650`). It must be redefined, for example as a node count or a byte bound, which changes the meaning WP-55's `Limits` passes through.
  2. `Yield` "every 256 nodes built" can only run during conversion, after v4 has built the `yaml.Node` tree.
  3. `ctx` can only be checked between documents (`:80`, `:651`: one 64 MiB document runs 12 to 45 s).

  Messages and positions differ: `*yaml.LoadError` Mark and ContextMark. WP-75 has not recorded goldens yet, so nothing committed breaks.
- **Option 3: unchanged.** The handwritten role keeps `profile.go`, `json.go`, `encode.go` and `scalar.go`; only the goccy test hooks go. The spike bounds documents by nodes ("more than 4194304 nodes"). Keeping the field name `MaxDocumentTokens` means defining "token" as Ruralz's scanner token. A rename (`MaxDocumentNodes`) would change the API that the forward notes promised to keep.

`Encode`, `EncodeWith` and `EncodeJSON` are Ruralz code under every option. R-29 (`specs/00:6587`) says "golden output never depends on a library release", and `specs/02:605` says the same.

## 8. Open WP-33 contract requests any option must settle

Source indexes are into the run files under `.claude/plans/m1/runs/`. None is triaged yet: `triage.json` names WP-33 only for older wave-2 forwards.

| # | Request | Source | Depends on option? |
|---|---|---|---|
| 1 | DefaultMaxDocumentTokens 400,000 (code) versus 1,000,000 (specs 01 req 6, risk 1, `specs/00:7172`, wps.json, `specs/01:365,570`). Measured 360 to 450 B per token for dense shapes, about 400 to 485 MiB at 1M, past the 256 MiB budget (01 test plan `:739`, 11 req 17). Choose: lower the hypothesis to 400,000, or keep 1M and raise the heap budget | `w3-s1a-harden-result.json` rounds[0].fix.contractRequests[0]; `w3-s1a-harden2-result.json` finish.contractRequests[0] | Number is goccy-specific. v4 cannot count tokens; hand-written counts its own tokens or nodes |
| 2 | Per-file bound of five quarters of MaxDocumentTokens (500,000). It refuses a 2.8 MB file of 20,000 Routes, below MaxResources and 64 MiB (harden2 major 4), which breaks 01 req 1(b) one-file Bundles and 01 req 6. Options: scale with input (`max(5/4 x MaxDocumentTokens, len(src)/2)`), a separate `Options` field, or record it under OQ-configuration-model-18 | harden2 finish.contractRequests[0]; remainingConfirmed[4] | Yes. The hand-written spike parses that file in 83 ms and 46 MiB (handwritten evidence section 9) |
| 3 | Spec 01 req 8 refusal list, batch 1: block sequence entries or a lone `-` in flow; two tags on one node; a tag ending its line before a token at or left of its block column | `w3-s1a-fix-result.json[0]` contractRequests[0] | goccy-specific |
| 4 | Batch 2: explicit `?` entry shapes (multi-node key, empty key, misplaced node, ungrouped tag) | `w3-s1a-harden-result.json` finish.contractRequests[0] | goccy-specific |
| 5 | Batch 3: token-pass refusals, plus valid YAML still refused: `a: \|+\nb: 1`; `?\n  a\n: b`; `k: [!!str ]`; and others | harden rounds[0].fix.contractRequests[4] | goccy-specific |
| 6 | Batch 4: `...` on a non-marker line; tokens after `...`; dedented entries matching no column; explicit flow key shapes; lone `?`; null-shift counting tags | harden rounds[1].fix.contractRequests[0] | goccy-specific |
| 7 | Batch 5: seven new RZ-CFG-001 refusals (flow entry shape, `:` in flow, key after tab, mapping at sequence column, later-line value at or left of column, block header extras) | harden2 finish.contractRequests[1] | Mostly goccy-specific. Some, such as block header extras and key after tab, are YAML 1.2 errors any parser refuses |
| 8 | Valid YAML 1.2 that goccy refuses and the profile keeps refused (10 shapes). The CM must list them or carry an OQ row | harden2 finish.contractRequests[2] | goccy-specific. v4 has its own list (about 42 valid suite streams refused, 13 with tabs; yamlv4 evidence section 0, section 8 item 5). Hand-written: none beyond profile choices |
| 9 | Token-pass wording: the scanner is not incremental; documents are cut at marker lines; the token count is bounded from bytes first, and that bound over-counts quoted and block scalars (record under OQ-configuration-model-18) | `w3-s1a-result.json[0]` contractRequests[0], [6] | goccy-specific |
| 10 | Refined depth rule (01 req 9), and the second depth bound in the converter (01 req 9, 11 req 17 and 26) | result contractRequests[1]; harden finish.contractRequests[1] | goccy-specific |
| 11 | Flow cost bounds `maxFlowPath` 32 MiB and `maxFlowNullShift` 2^26 (OQ-configuration-model-18) | harden rounds[0].fix.contractRequests[1] | goccy-specific |
| 12 | Entry-by-entry parse (01 req 10); scalars read from source and a final line break assumed (01 req 12) | harden rounds[0].fix.contractRequests[2], [3] | goccy-specific. The final line break convention also applies to v4 and hand-written |
| 13 | `$${` rule: 01 req 31 (every string) versus 02 req 46 (only where substitution runs, `specs/02:145`). Encode follows 01 req 31 | result contractRequests[2] | No. Encode is parser-independent; needs an R-77 or R-78 entry |
| 14 | WP-55 and WP-58 API notes: the error is only `ctx.Err()` or `ErrFormat`; diagnostics cap MaxDiagnostics+1; Encode, EncodeWith Verbatim, EncodeJSON KindList. Spec 01 section 3 (`:361-380`) is stale | result contractRequests[4], [5] | No |
| 15 | Upstream goccy issues, or an upgrade: quadratic `parseMap` and `bufferedSrc`, tab scanning, and the others listed | result contractRequests[3]; harden rounds[0].fix.contractRequests[5]; harden2 finish.contractRequests[3] | Only option 1 (and 3b) |
| 16 | The five open confirmed findings (four blockers, one major) | `w3-s1a-harden2-result.json` remainingConfirmed[0..4] | Fixes under option 1; superseded by a rewrite under 2 or 3 |

## 9. Gaps and caveats

- The two-approval rule is a pull request check. Work here commits straight to `develop`, so "two approvals" means the user's explicit approval, recorded as in `user-decisions.md`.
- ADR-0003 names sigs.k8s.io/yaml as a considered option (`:36`), but it has no "Alternatives not chosen" row. A superseding ADR that keeps that option needs the row, or must drop it.
- The configuration model's front matter still lists ADR-0011 (`02-configuration-model.md:9`), which ADR-0019 superseded. Any edit there should also do this cleanup, word-neutral.
- `AGENTS.md`'s "What exists today" table does not yet list `internal/config/profile`, `overlay`, `subst` or `schemaview`, which were committed in S1. Fix with a `chore:` commit under any option.
- The research doc's section 6 analysis (`tooling-and-licenses.md:157`) is now partly out of date (upstream evidence section 2.6). goccy resolves `010` as octal, as ADR-0003 `:95` already says, and v4 refuses `%YAML 1.2`. Correcting it is a research addendum.
- Not verified here:
  - golangci-lint behavior with the changed depguard rules (`make lint` downloads tools and prompts).
  - `go mod tidy` results for each option. The `go mod graph` output only shows that Ruralz alone requires goccy.
