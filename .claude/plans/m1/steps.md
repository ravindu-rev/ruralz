# M1 steps

This is the plan of record for the rest of M1 from 2026-10-24 (`user-decisions.md` items 27 to 35). It cuts the remaining work into small steps. Each step changes one package, one part of a package or one small set of documents, is reviewed until a fresh review reports zero findings, and lands on `develop` before the next step starts. What each package must do is still defined by `arch/wps.json` (owned paths, scope and spec rows), `specs/00-architecture.md` 4.4 and the area specs; this file only cuts that work into steps and records progress. `wave3-plan.md` is the record of how wave 3 ran until 2026-10-24.

## How a step runs

1. **Start.** `git status --porcelain` must be empty: uncommitted work belongs to the step it was made for, so finish or pause that step first (item 7). Then run `git fetch origin --prune`, `git switch develop` and `git merge --ff-only origin/develop`.
   - Paused steps are the `origin/step/*` branches: resume one instead of starting a new step (item 7).
   - A local `step/*` branch with no `origin/step/*` is this machine's unfinished step: resume it. Only when develop's `steps.md` marks that step Done is the branch left over from a step finished on another machine; then check that `git log develop..step/<id>` holds only pause commits, and delete it with `git branch -D step/<id>`.
   - `git branch -a` must then list only `main`, `develop` and, until step 3.20, `wip/s2-handoff` (decision 33). Create the branch with `git switch -c step/<id>` (for example `step/3.01`).
   - If the step names a parked patch in `wip/`, apply it with `git apply .claude/plans/m1/wip/<name>.patch`; the step's plan commit (item 4) deletes the patch file.
2. **Work.** Launch the step's workflow in the turn of a typed user message that names the step (README, "Starting a workflow run"), or do the work directly when it needs no agents. Package steps use `m1-section.js` or `m1-wp51-close.js` and document steps `m1-gov.js`; the R steps use `m1-section.js` in its review-only mode for code and `m1-gov.js` for documents (step 0.02). Agents edit only the paths of the step's commits (item 4), never `go.mod`, `go.sum` or `internal/tool/modpin`, which only the lead edits (architecture 4.1 rule 3), and never commit.
   - The lead builds the step brief from the sources first: every finding, owner choice and contract request of the package (both sources; a later request replaces one it restates), each assigned to the step whose code it changes. A request that several packages filed is one class, applied once by the first step that reaches it; the later steps drop their copies.
   - The brief lists what the later steps of the same package add, and the findings and requests assigned to them, as known and excluded (decision 30), so reviewers do not report them; a review still covers everything the step lands.
   - Owner choices are asked before the run. Known contract requests are applied by the step's run, with their paths among the step's commit paths, so its review covers them; a request filed during the run is applied before a final review round, never after the last one.
3. **Done when.**
   - A fresh review of the step reports zero findings of any severity. A step lands when its own scope is at zero, even while the package's other known findings wait for its later steps; the package counts as done only after its last step, whose review covers the whole package against its 4.4 entry (decision 30).
   - Every finding was fixed with its whole class, with a test per member that fails without the fix (AGENTS.md "Quality standard").
   - The scoped checks pass for the touched packages (`go build`, `go vet`, lint and format, tests with and without the race detector, `go test -tags integration` where the package has integration-tagged tests, the Go 1.26 floor), then the whole-tree checks below. Every step also runs `python3 -I .claude/plans/m1/tools/check-steps.py` after updating this file.
   - A step that changes only `.claude/plans/` and the agent instruction files (`AGENTS.md`, `CLAUDE.md`), which no build or CI stage reads, runs the plan checks instead: `python3 -I .claude/plans/m1/tools/check-steps.py`; after staging, `git diff --cached --check -- . ':!*.patch'` (patch files hold diff context lines that are a single space); `go run ./internal/tool/repocheck`; and the title and DCO checks of item 4. The other stages have nothing to check there.
   - Behavior and decision changes update `docs/` in the same step, or add an Open questions row to the owning document when they need a decision (AGENTS.md "Sources of truth"; decision 31). Contract requests are applied in the step (item 2): plan-spec ones go into the plan commit, and changes to another package's owned paths into lead commits (item 4; architecture 4.1 rule 3). A request that needs a user decision is asked during the step, before it is applied. Only the backlog deferred to S8 before 2026-10-24 waits for steps 3.68 and 3.69.
   - A check this machine cannot run runs in CI on the step branch before item 5 (decision 35): the Windows and macOS jobs for `internal/cli`, `internal/buildinfo` and `pkg/`, the container State Store leg, and suites that need `redis-server` or Docker. Push the branch, run `gh workflow run pr-fast.yml --ref step/<id> -f all=true` and the same for `pr-full.yml`, and wait until both pass (`gh run watch`). The user installs and logs in `gh` before the first such step.
   - The step notes record the step's commits, its run IDs, the agent usage of its runs (`tools/usage.py`), its CI runs, and every check that could not run anywhere, with the reason (CLAUDE.md).
4. **Commit** on the step branch, signed with the machine's `~/.ssh/id_ed25519` through the repository git config (README, "Working rules from the user"):
   - If the branch holds pause commits (item 7), first run `git reset --soft $(git merge-base develop HEAD)`; their changes stay staged.
   - The commits a step may make, each holding only its own paths. Stage each with `git add --pathspec-from-file=<list>` and commit with `git commit -S -s -F <msg> --pathspec-from-file=<list>`; `git apply` creates untracked files, which a pathspec commit refuses unless they are added first.
     - One per package of the step, with only that package's owned paths.
     - One `docs(<slug>)` commit per design document the step changes (decision 31), or one `docs:` commit without a scope when a change spans several documents, such as an ADR with its bookkeeping (`docs: add ADR-NNNN ...`, `.claude/rules/adr.md`).
     - One `build(deps)` commit, made by the lead only (architecture 4.1 rule 3), for `go.mod` and `go.sum`, with the `.golangci.yml` and `internal/tool/modpin` entries that belong to the same dependency change.
     - One lead commit per other package the step changes, with that package's scope: a contract request that touches another package's owned paths (architecture 4.1 rule 3), or a lead edit the architecture names (7.03: `internal/tool/sizegate`, 4.1 rule 1 and convention 7; 9.01: the `internal/tool/modpin` deletion, R-61).
     - Then the step's plan commit (`chore:`): this file's status row and step notes, the run result in `runs/`, the plan-spec edits (`specs/`, `arch/`, `forwards.json`, the WP-51 audit record), and the deletion of the `wip/` patch files the step applied.
   - Each title is checked with `go run ./internal/tool/commitcheck title -title "<title>"`. A body has 2 to 6 lines that name the step, then `Refs: WP-xx (M1 architecture)` where a package is concerned. No `Co-Authored-By` trailer. Then `go run ./internal/tool/commitcheck dco -range develop..HEAD`; `git log --format='%G? %GK' develop..HEAD` must show `G` and this machine's key on every commit, and `git status --porcelain` must be empty.
   - Steps that touch two-approval paths (`go.mod`, `.github/`, `api/`, `pkg/`, `docs/_meta/`, `test/bench/allocgate.json`, `nolicensecheck.allow`, the security-sensitive packages) land like any other step (decision 32).
5. **Merge.** `git switch develop`, `git merge --ff-only step/<id>`, `git push origin develop`. No CI runs on pushes to `develop` (`pr-fast` and `pr-full` run on pull requests and `main`), so the checks of item 3 are the gate.
6. **Clear.** If the step branch was pushed, delete it on origin first (`git push origin --delete step/<id>`): a branch whose upstream still holds pause commits refuses `git branch -d`. Then `git branch -d step/<id>`, and delete any other branch except `main`, `develop` and, until step 3.20, `wip/s2-handoff` (decision 33), locally and on origin. `git ls-remote --heads origin` and `git branch -a` must then show only those.
7. **Pause.** A step stopped before it is done commits its work in progress on its branch, signed, as `chore: pause step <id>`, with the resume point in the step notes, and pushes the branch so another machine can resume it. Until the step is done, its branch is the only one besides `main`, `develop` and, until step 3.20, `wip/s2-handoff`. Item 4 drops the pause commits before the real commits, and item 6 deletes the remote branch.

Whole-tree checks, the local form of CI stages 1 to 6 (`/preflight`):
- `go run ./internal/tool/repocheck` and the commit checks of item 4;
- `GOTOOLCHAIN=go1.27.1 make lint` (golangci-lint `config verify`, `fmt --diff` and `run`, then actionlint);
- `go generate ./...` with no diff;
- `make build` (every binary and platform, the Windows and macOS CLI included);
- `make test` (race tests with cgo, then `CGO_ENABLED=0`) and `make floor FLOOR_GOTOOLCHAIN=go1.26.8`;
- `make supply-chain` (`go mod verify`, govulncheck, depgate);
- at the end of each wave, `make integration` as well (stage 8: every integration-tagged suite with the race detector), as specs/00 section 0 item 7 asks. It needs `promtool` and a local `redis-server` 7.0.15 (architecture 4.1 rule 6); on a machine without them it runs through `pr-full` on the branch of the wave's last step (item 3).

Fix a failing or flaky test at its root cause, never by skipping it.

Size and order:
- A step should finish in one run of about two hours or less.
- When a review reports more than about 15 findings, or the fixes fall into unrelated classes, split the rest into follow-up steps (`<id>b`, `<id>c`) before fixing. Each class stays inside one step.
- The steps of one package run in order, and a package that another step of the same package imports (specs/00 section 1.2) is built first. The packages inside a wave do not depend on each other's code, but their tests may import one another. A step that changes a package runs the tests of every package whose tests import it (`go list -test -deps` finds them; for example, the schemaview tests import profile), and those test files are among the step's commit paths, so a fix to them is reviewed in the step. A wave starts when the previous one is done.
- "Owner" marks a step only the user can do or decide. "Owner choice" marks a finding or request whose fix needs the user's choice; ask it during the step.

## Status

| Stage | Steps | Done |
|---|---|---|
| 0 Plan and tooling | 2 | 1 |
| 3 Wave 3: close S2 and S1, the other wave-3 packages, decisions and documents | 73 | 0 |
| R Fresh review of waves 1 and 2 | 42 | 0 |
| 4 Wave 4 | 32 | 0 |
| 5 Wave 5 | 7 | 0 |
| 6 Wave 6 | 10 | 0 |
| 7 Wave 7 | 7 | 0 |
| 8 Wave 8 | 20 | 0 |
| 9 Wave 9 | 5 | 0 |
| 10 Wave 10 | 2 | 0 |
| X M1 exit | 7 | 0 |
| **All** | **207** | **1** |

Next step: 0.02.

## 0. Plan and tooling

| Step | Package | Scope | Status |
|---|---|---|---|
| 0.01 | Plan | Adopt this plan: the steps, user decisions 27 to 35, the AGENTS.md sentence for decision 30, the README, wave-3 plan and architecture updates, signing with `~/.ssh/id_ed25519`. Park the S2 work of `wip/s2-handoff` in `wip/`; the branch stays until step 3.20 | Done 2026-10-24 |
| 0.02 | Run scripts | Step mode for `m1-wp51-close.js`, `m1-section.js` and `m1-gov.js`, and the tools for step 3.01 (scope below) | Not started |

Step 0.02 scope:
- Make each run script the steps use (`m1-wp51-close.js`, `m1-section.js`, `m1-gov.js`) carry out every rule of "How a step runs" that reaches its agents (items 1 to 3 and 7) and every step row it serves. The step brief lists, per script, each rule it breaks today, with the line, and the step's review checks every rule against every script. The gaps below are where the brief starts, not its limit:
  - no `step` argument: the paths of the step's commits, its scope, and the later steps' findings, classes and requests, which reviews and checks treat as known and excluded (decision 30);
  - rules that confine agents to one package, or have the lead apply contract requests after the run (`m1-wp51-close.js` rules 1 and 7, `m1-section.js` rules 1, 3 and 10), where steps apply them in the run and agents write every path of the step's commits;
  - review loops that pass with findings open or stop at a fixed round limit (`m1-gov.js`);
  - stale starting points, dates and authorities (`m1-wp51-close.js` names `e1e9657`; `m1-gov.js` cites the wave-2 decisions);
  - prompts that name `develop` instead of the step branch, defaults for the Windows checkout, and `m1-gov.js`'s hard-coded `/home/user/ruralz` with no `repo` argument.
- `m1-wp51-close.js` also gets the group cut of steps 3.01 to 3.11 (3.01's work, G05, G10 and G11 each in two), a mode that stops after a named group with its carried items fixed and its groups checked, a mode that runs only the review rounds (3.12), and a history file, `runs/wp51-history.json`, that each group step appends to and the next one reads. The continued G04 engineer reads `wip/WP-51-attempt3-reference.patch` for the attempt's edits made after `451a729`.
- `m1-section.js` also gets a review-only mode (the code R steps, which have no `reports/<WP>.json`), a findings subset by list position (steps 3.14 to 3.20), contract requests from both sources (a later request replacing one it restates), and optional extra lenses: a reader lens on named files (steps 3.24 to 3.26 and 3.30) and a hostile-input lens (every WP-33 code step, 3.24 to 3.30, by the lead default of 2026-10-10).
- `m1-gov.js`, which the document steps and document R steps use, also gets lenses for documents (the style guide, the manifest outline and word band, canonical names, links) and a review-only mode for the document R steps, since every unit starts with a writer today.
- `tools/check-workflow.sh <script>`: a syntax check for the run scripts. They start with `export const meta` and end in a top-level `return`, so a plain `node --check` fails; the tool drops the `export`, wraps the body in an async function, and runs `node --check` on that.
- `make tools`, so `bin/golangci-lint` v2.13.2 and actionlint exist for the whole-tree checks of step 3.01.

## 3. Wave 3

### Close S2: WP-51 validation.json-schema

Owned path: `internal/filter/validation/jsonschema/`. The gap classes are in `wp51-library-audit.md` section 4, and the decisions in `user-decisions.md` items 20 to 26. Steps 3.01 to 3.11 (3.01b included) each close their classes with a test per member, checked independently; step 3.12 runs the full review rounds. WP-51 counts as done only after 3.12. Step 3.01 applies `wip/WP-51.patch`; `wip/WP-51-attempt3-reference.patch` is for reading only and is deleted in the same plan commit.

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.01 | WP-51 | Applies `wip/WP-51.patch`. Finish G04 attempt 3 (GC-08: deterministic memory measurements) and check G04 independently to zero. Its two open majors: the heap meter misses short-lived copies such as the `uniqueItems` map, and the race run times out in `TestHandleMeasuredFit` | Not started |
| 3.01b | WP-51 | The five minors carried from G02 and G03 (`runs/w3-s2-wp51-carry2.json`), an independent recheck of G01 to G03 to zero, and the carry file's contract requests 1 to 4, 6, 10 and 11 applied to spec 07, R-77 and the audit record | Not started |
| 3.02 | WP-51 | G05a: design the one walk-time compile budget of 1 MiB (decision 20) for both its byte and count terms, and apply its one contract request to the plan specs (audit 4.2), together with the carry file's requests 5, 7, 8 and 9, which define the budget's fixpoint term; owner choice: that term's cap (proposed 2^18). Implement the byte terms, GC-03 (23 members) | Not started |
| 3.03 | WP-51 | G05b: implement the count terms, GC-09 (10 members), in the budget 3.02 designed | Not started |
| 3.04 | WP-51 | G06: non-canonical pointer indexes accepted as aliases (GC-04, decision 21); the eight integer keywords above 2^63-1 clamped (GC-05, decision 22) | Not started |
| 3.05 | WP-51 | G07: regex parser work the lexical pass does not count (GC-06); anchor-name bytes and the decision wrapper's frames charged (GC-07, decision 24) | Not started |
| 3.06 | WP-51 | G08: regexp matcher costs from the compiled program with `add()` depth (GC-11); the 128 MiB stack ceiling (GC-15, decision 23) | Not started |
| 3.07 | WP-51 | G09: kept memory restated (GC-12), the number model at the library's parse sites (GC-13), values outside the decoder's data model (GC-14) | Not started |
| 3.08 | WP-51 | G10a: pin every walk rule and resolution mirror per keyword, form and route (GC-16) | Not started |
| 3.09 | WP-51 | G10b: prove every work-model term with a test that a mutation fails (GC-17) | Not started |
| 3.10 | WP-51 | G11a: bound the raw `config.schema` decode before the size check (GC-10); deterministic compile diagnostics, with `config.schema` decoded under the configuration's own depth limit (GC-19, decision 25) | Not started |
| 3.11 | WP-51 | G11b: keyword locations per error kind (GC-18); library deviations documented and pinned (GC-20); round-9 minors 2 and 4 | Not started |
| 3.12 | WP-51 | Closing review: full review rounds until one reports zero findings, applying the contract requests the rounds file | Not started |

### Close S2: the open findings of WP-35 to WP-39

The open findings are in `runs/w3-s2-minors-continue3.json`; the numbers below are their positions in each package's list. Each package's contract requests are in that file's `_meta.contractRequests` and in `reports/<WP>.json`. The owner choices and the requests of each step come from those sources through the step brief (item 2), not from this table. The first step of each package applies its patch from `wip/`; for WP-37 and WP-39 that patch holds the package's whole work in progress, so the first step lands all of it.

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.13 | WP-35 schema view | Applies `wip/WP-35.patch`. A fresh review of fix0, which closed its 11 findings (`reports/WP-35.json`), until zero; its contract requests; package review | Not started |
| 3.14 | WP-36 overlay | Applies `wip/WP-36-overlay.patch` (the overlay work only; it builds and tests without the substitution patch). Findings 3, 4, 8, 9 and 11 (two majors, not reproduced yet), and the requests that change the overlay code | Not started |
| 3.15 | WP-36 substitution | Applies `wip/WP-36-subst.patch`. Findings 1, 2, 5, 6, 7 and 10 (two majors in the credential redaction class, not reproduced yet), and the requests that change the substitution code; package review | Not started |
| 3.16 | WP-37 conversion, contract and work accounting | Applies `wip/WP-37.patch`. Findings 1, 2, 4, 6, 7, 8, 9, 10 and 14: the converter contract and the checks after conversion, budgets and yield accounting, the flaky `TotalAlloc` test | Not started |
| 3.17 | WP-37 defaults and normalization | Findings 3, 5, 11, 12, 13 and 15 to 18: the number literal form, silent drops, untested branches, comments; package review | Not started |
| 3.18 | WP-38 precedence | Applies `wip/WP-38.patch`. Run the adversarial lens again, reproduce the 9 findings, fix them; package review | Not started |
| 3.19 | WP-39 encoder and yield | Applies `wip/WP-39.patch`. Findings 1, 2, 4, 7, 11, 16, 18 and 19: the yield class over every input-length loop, the level-0 and `since` rules, encoder bounds | Not started |
| 3.20 | WP-39 decoder, dump and streams | Findings 3, 5, 6, 8, 9, 10, 12 to 15 and 17; package review. S2 then closes: delete `wip/s2-handoff` on origin (decision 33), `runs/journals/`, `runs/scratch/` and the S2 stash on this machine, and ask the user to clear the Windows checkout's copies | Not started |

### Close S1: WP-33 restricted YAML profile

Owned paths: `internal/config/profile/` and `test/fixtures/yaml-test-suite/`; the document and dependency steps commit outside them as item 4 allows. The steps follow `yaml-parser-decision.md` section 5 and user decisions 8 to 11, with one change of order that decision 26 requires: "WP-33's rebuild waits for ADR-0020". So ADR-0020 comes first (3.23), as `proposed` if you choose so, which keeps the fallback below open; it takes effect at the switch (3.28).

Stop rule and fallback, decision 8 verbatim: "stop if the second review round confirms more than two blocker or major findings in parser-core code, or the parser's agent cost passes $250 before a clean round; then keep WP-33 as committed meanwhile and move to go-yaml v4 pinned at rc.3." The parser-core steps are 3.24 to 3.26. Each step's second review round decides the stop rule. Below the threshold, the step keeps running review rounds until one reports zero findings, as long as the parser steps' total agent usage stays under $250 ("before a clean round"); usage is tallied with `tools/usage.py` after each run (lead default, `user-decisions.md`). A step that meets the stop rule does not land: its branch is deleted, and a re-plan step replaces the WP-33 steps not yet landed. It removes the unexported parser of the parser steps that landed, reverts the C2 text of 3.21, and revises the proposed ADR-0020 and its rows for v4.

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.21 | WP-33 plan text | Spec and plan text for C2 (brief section 5.1 step 2): spec 01, the specs/00 rows and a new R-row, the spec 02, 03, 10 and 11 lines, the WP-33 scope in `arch/wps.json`, the `forwards.json` notes; the 7 contract requests in `reports/WP-33.json` | Not started |
| 3.22 | WP-33 research | The research addendum in `docs/_meta/research/tooling-and-licenses.md` (content approved in decision 11; brief section 5.5 item 2), which ADR-0020 cites | Not started |
| 3.23 | WP-33 ADR | ADR-0020 superseding ADR-0003: you type `/new-adr` or ask for it in words, and choose its status first (`proposed` keeps decision 8's fallback open). The new-adr skill's bookkeeping lands in the same change. With status `proposed`, the skill items that make it take effect wait for 3.28, recorded as a deviation in the step notes: ADR-0003 marked superseded, the foundation pack row pointed at ADR-0020, and goccy moved to "Alternatives not chosen"; the yaml v3 test-only catalog row waits for its dependency at 3.27. ADR-0020 may name v4 only once the tech stack document lists it under "Alternatives not chosen" (`.claude/rules/adr.md`), so v4's row, with its research URL from 3.22, lands here | Owner |
| 3.24 | WP-33 parser part 1 | Unexported, beside the goccy path: scanner, flow and block tokens, directives (reserved ones ignored, decision 10), limits counted during the parse, and decision 9's strict refusals of a closing `]` or `}` aligned with its key (with its own message) and of `"a"#c`; per-construct tests. Review with the four lenses, a reader lens and a hostile-input lens; the stop rule applies | Not started |
| 3.25 | WP-33 parser part 2 | Unexported: scalars on the kept quoted and literal helpers, with decision 9's refusal of QB6E-shaped quoted lines; per-construct tests. Same lenses, the reader lens on the quoted and literal helpers; the stop rule applies | Not started |
| 3.26 | WP-33 parser part 3 | Unexported: document structure to `tree.Node` with positions, duplicate keys, raw nodes dropped after each document, memory measured again, generator property tests, and the regression rows of brief step 3 (the inputs of WP-33's five open S1 findings, the 32 style rows, the merge-key rows, the 1,024-character key and the cost-ladder rows in `hostile_test`). Same lenses; the stop rule applies | Not started |
| 3.27 | WP-33 differential test | The yaml v3 differential target over the new parser and its list of known deviation classes, with everything AGENTS.md requires in the same change: `go.yaml.in/yaml/v3` as a direct requirement, its `.golangci.yml` admitted entry and the rule confining it to `internal/config/profile` tests, and its test-only catalog row in `docs/engineering/01-tech-stack-and-libraries.md`. Review with the four lenses and the hostile-input lens | Not started |
| 3.28 | WP-33 switch | The pre-switch differential with 3.27's deviation filters (brief section 5.1 step 4; nothing from it is committed), then `Parse` on the new parser. Delete the goccy files and the tests that only exercise them, fix the expectations that encoded goccy readings, and cut the YAML Test Suite expected failures from 87 to 67 (`go test -tags integration`). The tests of every package that imports the profile stay green (brief section 5.1 step 4; `go list -test -deps`), WP-35's, WP-38's and WP-39's among them, with their test files in the step's commit paths where an expectation encoded a goccy reading. Review with the four lenses and the hostile-input lens. With your OK, ADR-0020 becomes `accepted` and the skill items 3.23 deferred land (ADR-0003 superseded, the foundation pack row, goccy under "Alternatives not chosen"), with the tech stack figures, the layout, configuration model (word-neutral) and testing document lines, and decision 9's loader-reference note before WP-75 records goldens | Not started |
| 3.29 | WP-33 drop goccy | Drop goccy/go-yaml from `go.mod`, `internal/tool/modpin` and `.golangci.yml`, then `go mod tidy`. Review with the four lenses and the hostile-input lens | Not started |
| 3.30 | WP-33 closing review | The whole package: the four lenses, the reader lens and the hostile-input lens, until zero. S1 closes | Not started |

### The other wave-3 packages

These were sections S3 to S7. The spec rows for each package are in `arch/wps.json`.

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.31 | WP-34 CEL module | One environment per place and site, the library subset, `Check` with the RZ-CFG-014 and RZ-CFG-015 messages, static cost at nominal sizes | Not started |
| 3.32 | WP-34 | Runtime: the cost limit, `ContextEval` under min(deadline, 50 ms), error kinds and per-place rules, the `ProgramSet` cache, `ReferencesRequestBody`, `Default` for retryOn and failureWhen | Not started |
| 3.33 | WP-34 | `DecodeJSON` with the budget it is given, `PrepareRoute` and `PrepareConsumer` (also in `celtest`), the place-to-annotation test, `FuzzCompile` and the cost-estimator target; package review | Not started |
| 3.34 | WP-40 telemetry runtime | The `OTEL_*` scrub, the resource, providers, OTLP/gRPC trace, metric and log pipelines with bounded queues and `otlp.tls`, the otelslog bridge, the logger factory | Not started |
| 3.35 | WP-40 | `/metrics` over a private registry (exporter, producer, OpenMetrics exemplars), runtime metrics, and the metric name identity gate (09 tests 32 and 33) | Not started |
| 3.36 | WP-40 | The degraded set and cleartext hops (`emit.NodeStatus`), `Apply` per Revision, `Shutdown` with a 5 s flush, 09 tests 36 to 38; package review | Not started |
| 3.37 | WP-42 router | The whole package: host tiers, path trie, candidate filters, the precedence order, `match.when` errors, its span, the 10,000-Route compile budget, a zero-allocation `Match`, `FuzzRouterMatch` | Not started |
| 3.38 | WP-43 per-request exchange | Pooled state for `filter.Exchange`, `filter.Message` and `snapshot.RequestState`: Enter and SetLeg, when decisions, PolicyState slots, Stripe and RouteMetrics, leg views with a Vars copy, reset for reuse | Not started |
| 3.39 | WP-43 | Lazy `expr.Vars`, header views, body access through gates and budget, `SetBody`, `Decoded` with the R-63 budget, identity once, rate-limit fields, tee, `ReplaceResponse`, `Final`, the access-record feed, the gateway timer; package review | Not started |
| 3.40 | WP-44 reuseport | `internal/gateway/reuseport/`, which the listener package imports: bind without listen, CBPF self-steering on Linux, stubs elsewhere | Not started |
| 3.41 | WP-44 listeners | `internal/gateway/listener/`: the listener set, servers (protocols, timeouts, header limits), the connection ceiling, PROXY v2, TLS from the retire holder, `Binder` with RZ-CFG-039; package review | Not started |
| 3.42 | WP-45 readiness and tap | `internal/gateway/readiness/` (the reason set) and `internal/gateway/tap/` (the NDJSON hub with per-subscriber sampling, dropped lines and the RZ-RT-019 subscriber limit) | Not started |
| 3.43 | WP-45 admin server | `internal/gateway/admin/`: its own mux on `admin.port`, the injected handlers, `/debug/snapshots`, `/debug/pprof`, adminauth, problem errors, 404 RZ-RT-020 and 405 RZ-RT-021; package review | Not started |
| 3.44 | WP-46 upstream runtimes | The manager and runtimes carried over by identity (discovery, health, pickers, breaker, bulkhead, retry budget), transports (pools, TLS, the guarded dialer), the Run scheduler, pool metrics | Not started |
| 3.45 | WP-46 attempt loop | `Do` with deadlines and per-try timeouts, retries (retryOn, Filter Retry, replayable body, backoff), hashKey once per leg with its fallback and metric, failureWhen, error classes and RZ-UP codes, the `upstream_panic` and `balancer_budget` reasons | Not started |
| 3.46 | WP-46 outgoing request | Hop-by-hop removal, forwarding headers, Host or sni (R-28), `Tracer.Inject`, `GetBody` only for buffered bodies; package review | Not started |
| 3.47 | WP-65 post-commit queue | `internal/statestore/postcommit/`, which the manager imports: the four-class queue with byte caps, eight writers, batches, drop counters, the flush at Drain | Not started |
| 3.48 | WP-65 State Store manager | `internal/statestore/manager/`: the effective configuration, drivers by Role, injected openers, the MAC key, reuse, store handles with retain and release, `SetNodeCount`, shutdown order; package review | Not started |
| 3.49 | WP-88 revalidation | `internal/filter/cache/revalidate/`: the pool component, the partition lease (R-71), replay with validators, 304 refresh, variant delete | Not started |
| 3.50 | WP-88 coalescing | `internal/filter/cache/coalesce/`: miss coalescing for up to 4,096 keys, leader and followers, deadlines, the fallback fetch; package review | Not started |
| 3.51 | WP-47 JWKS manager | The `jwks` subpackage: the Node-wide component (fetch through egress, cache-control clamp, refresh, prefetch, limiter, `jwks_stale`, age metric) | Not started |
| 3.52 | WP-47 auth.jwt | The Factory and Filter: compact parser, alg allow list, jws verification, claims with jsonval, skew and lifetime, audiences, issuer, Consumer binding; package review | Not started |
| 3.53 | WP-48 auth.api-key | `internal/filter/auth/apikey/` | Not started |
| 3.54 | WP-48 auth.basic | `internal/filter/auth/basic/`: PBKDF2 verify, the Node-wide throttle with its success cache, buckets and hash queue, 429 RZ-AUTH-007; package review | Not started |
| 3.55 | WP-49 auth.mtls | `internal/filter/auth/mtls/` | Not started |
| 3.56 | WP-49 authz.ip | `internal/filter/authz/ip/` | Not started |
| 3.57 | WP-49 cors | `internal/filter/cors/`; package review | Not started |
| 3.58 | WP-50 auth.upstream-oauth2 | The whole package | Not started |
| 3.59 | WP-85 redisserver | The process flavor: standalone, replica with scripted failover, TLS, ACL user, three-primary cluster. Needs a local `redis-server` 7.0.15 (architecture 4.1 rule 6), which this machine lacks; installing it is your choice, else the integration tests skip and the step notes record it | Not started |
| 3.60 | WP-85 container flavor | testcontainers-go with digest-pinned images, and the selection by `RURALZ_TEST_STATESTORE` | Not started |
| 3.61 | WP-85 respproxy | RESP command counting and the fault proxy; package review | Not started |
| 3.62 | WP-53 examples | The four examples and their READMEs | Not started |
| 3.63 | WP-52 l4lb | `internal/testkit/l4lb/` | Not started |
| 3.64 | WP-52 loadgen | `internal/testkit/loadgen/`; package review | Not started |
| 3.65 | WP-54 releasekit | `internal/tool/releasekit/` | Not started |
| 3.66 | WP-54 depgate | The notices subcommand, the denylist per binary, the transitive license classification | Not started |
| 3.67 | WP-54 deploy files | `deploy/container/` (Dockerfile) and `deploy/systemd/` (unit, env file, sysctl drop-in, handover helper); package review | Not started |

### Wave-3 decisions

This was section S8. Under decision 31 each step applies its own requests; these steps hold only the backlog the old plan deferred to S8 before 2026-10-24 (the requests of the S1 and S2 runs, and decision 15's configuration model and performance rows). Wave-2 requests went through `triage.json` and are not part of it.

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.68 | Backlog | List the backlog by target file and mark the requests that need your decision. Leave out any request that a package's current report or continuation file restates: that package's step applies it | Not started |
| 3.69 | Owner decisions | Your decisions on the requests 3.68 marked. Then add one step per target file after this one (3.69a, 3.69b and so on) to apply the backlog | Owner |

### Wave-3 documents

This was section S9 (WP-31).

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.70 | WP-31 traffic | `docs/architecture/09-traffic-management-and-resilience.md`: its Open questions and R-rows from the WP-31 scope | Not started |
| 3.71 | WP-31 observability | `docs/architecture/10-observability.md` | Not started |
| 3.72 | WP-31 scalability | `docs/architecture/11-scalability-and-distributed-state.md`; package review | Not started |

## R. Fresh review of waves 1 and 2

User decision 19: every wave-1 and wave-2 package gets a fresh review under the quality standard, with its findings fixed to zero. These steps run after wave 3 and before wave 4. The larger packages are reviewed in parts.

| Step | Package | Scope | Status |
|---|---|---|---|
| R.01 | WP-01 configuration contracts | `internal/config/diag/`, `tree/`, `revision/`, `hub/` | Not started |
| R.02 | WP-01 request-path contracts | The `internal/filter` root and `filtertest/`, `internal/gateway/snapshot/`, `internal/phase/`, `internal/expr/` | Not started |
| R.03 | WP-01 runtime contracts | `internal/clock/`, `problem/`, `adminapi/`, the `internal/secret` and `internal/statestore` roots, `internal/telemetry/catalog/` and `emit/` | Not started |
| R.04 | WP-01 code registry and lint rules | `internal/errcode/`, `.golangci.yml`, and WP-01's registry and catalog rows in the design documents; package review | Not started |
| R.05 | WP-02 | Strict JSON value library | Not started |
| R.06 | WP-03 | Schema index | Not started |
| R.07 | WP-04 | Policy type registry | Not started |
| R.08 | WP-05 | Route match primitives and request normalization | Not started |
| R.09 | WP-06 | Identity, Consumer index and auth helpers | Not started |
| R.10 | WP-07 | Secret resolver | Not started |
| R.11 | WP-08 | CEL type adapters | Not started |
| R.12 | WP-09 | Telemetry aggregates | Not started |
| R.13 | WP-10 | Tracing | Not started |
| R.14 | WP-11 | Process logs and access logs | Not started |
| R.15 | WP-12 | Telemetry gates, alert rules and dashboards | Not started |
| R.16 | WP-13 | State Store base: breaker, keys, memory driver, test fake | Not started |
| R.17 | WP-14 | Process identity, data dir and settings | Not started |
| R.18 | WP-15 | Client address | Not started |
| R.19 | WP-16 | Egress guard and TLS configuration | Not started |
| R.20 | WP-17 | Admin authentication, redaction and digest checks | Not started |
| R.21 | WP-18 | HTTP field rules and structured fields | Not started |
| R.22 | WP-19 | Admission and body buffering | Not started |
| R.23 | WP-20 | Filter Chain executor | Not started |
| R.24 | WP-21 | Snapshot holder and retirement | Not started |
| R.25 | WP-22 | Endpoint balancing | Not started |
| R.26 | WP-23 | Discovery and health | Not started |
| R.27 | WP-24 | Resilience primitives | Not started |
| R.28 | WP-25 | CLI framework and completion | Not started |
| R.29 | WP-26 test kit base, part 1 | `internal/testkit/binaries/`, `proc/`, `pki/`, `faultproxy/` | Not started |
| R.30 | WP-26 test kit base, part 2 | `internal/testkit/canary/`, `promtext/`, `benchrecord/`, `corpus/`; package review | Not started |
| R.31 | WP-27 build | `Makefile`, `.gitattributes` and the four `scripts/` | Not started |
| R.32 | WP-27 CI workflows | `pr-fast.yml`, `pr-full.yml`, `main.yml` and `nightly.yml` | Not started |
| R.33 | WP-27 tools | `internal/tool/benchgate/`, `sizegate/`, `fuzzplan/`; package review | Not started |
| R.34 | WP-28 | Schema and configuration types | Not started |
| R.35 | WP-29 configuration model | `docs/architecture/02-configuration-model.md` | Not started |
| R.36 | WP-29 | The foundation pack, the system overview and the glossary; package review | Not started |
| R.37 | WP-30 | The data plane and security and identity documents | Not started |
| R.38 | WP-30 | The deployment topologies, zero-downtime upgrades and CLI reference documents; package review | Not started |
| R.39 | WP-32 engineering documents | Repository layout, testing, release, and the tech stack CI rows | Not started |
| R.40 | WP-32 | Performance budgets, vision, feature catalog and the ADRs; package review | Not started |
| R.41 | WP-83 | Roadmap amendments for M1 | Not started |
| R.42 | WP-84 | Test kit mocks and OTLP sink | Not started |

## 4. Wave 4

| Step | Package | Scope | Status |
|---|---|---|---|
| 4.01 | WP-41 Redis driver | The rueidis client with its required options, the Ruralz dialer, URL validation and the topology fit (RZ-CFG-026), metrics and degraded reasons. Its integration tests, here and in 4.02 to 4.04, need the local `redis-server` of step 3.59 | Not started |
| 4.02 | WP-41 | `ruralz_v1.lua` (R-20, R-71), SCRIPT LOAD per primary, EVALSHA only, NOSCRIPT to RZ-STS-002 with a single-flight reload | Not started |
| 4.03 | WP-41 | The 8,192 in-flight ceiling, per-shard breaker and pacing, the cluster slot map and same-slot verification | Not started |
| 4.04 | WP-41 | The INFO memory poller and store-byte admission (R-72), the entry MAC; package review | Not started |
| 4.05 | WP-55 loader | Sources (directory, one-file Bundle, stdin), discovery and `.ruralzignore`, limits | Not started |
| 4.06 | WP-55 | Envelope and identity, Environments (RZ-CFG-022), overlay application, substitution, bounded worker parallelism; package review | Not started |
| 4.07 | WP-56 semantic validation | Reference resolution (RZ-CFG-009) and the cross-resource rules: listeners, hosts, identical match (RZ-CFG-023), composition limits, https certificates, `jwksUrl` and `tokenUrl`, identity static checks | Not started |
| 4.08 | WP-56 | Secret use collection with destinations and positions, the binding check (RZ-CFG-041), CEL sites checked with `expr.Compiler`, the injected `hub.Checks` | Not started |
| 4.09 | WP-56 | RZ-CFG-034 over each Route's chain at stage K, and the serve check RZ-CFG-040 (R-75); package review | Not started |
| 4.10 | WP-57 diff | The whole package | Not started |
| 4.11 | WP-58 render | The whole package | Not started |
| 4.12 | WP-59 authz.cel | `internal/filter/authz/cel/` | Not started |
| 4.13 | WP-59 headers | `internal/filter/header/`; package review | Not started |
| 4.14 | WP-60 transforms | The shared engine, the dotpath grammar and tree operations, `FuzzDotPath` | Not started |
| 4.15 | WP-60 | The request Factory: body rewrite with budget reservation, header recomputation, query and path edits, per-attempt isolation | Not started |
| 4.16 | WP-60 | The response Factory and the failure semantics (RZ-RT-011, RZ-RT-012); package review | Not started |
| 4.17 | WP-61 ratelimit | The key program, the Node-wide key table with local buckets, the first-seen budget and local-only entries, `config.localOnly` | Not started |
| 4.18 | WP-61 | GCRA through `statestore.Call`, Undo through PolicyState, fail-open windows, RZ-RL-001, RZ-RL-002 and RZ-RL-005, RateLimit fields, metrics, the admission property; package review | Not started |
| 4.19 | WP-62 quota | The whole package | Not started |
| 4.20 | WP-63 Response Cache | Directives, the key program, partitions by principal, the pipelined lookup, the hot layer | Not started |
| 4.21 | WP-63 | Coalescing and stale-while-revalidate (WP-88), stale-if-error, only-if-cached (RZ-RT-018) | Not started |
| 4.22 | WP-63 | Tee and store in Finish, unsafe-method invalidation, hit reservation release, store-byte admission, metrics; package review | Not started |
| 4.23 | WP-64 composition | Aggregate mode: step legs through the attempt loop, buffer reservations, the merge under group, partial responses | Not started |
| 4.24 | WP-64 | Sequential and conditional modes, step when and variables, client body replay, RZ-UP-011, RZ-RT-010 and RZ-RT-015; package review | Not started |
| 4.25 | WP-66 request handler | Admission through dispatch: unit, deadlines, pin, trace, header limits, framing and normalization, the Connection-option strip, source.ip, match, the dispatch table, the Route deadline, early 413 | Not started |
| 4.26 | WP-66 | Client Phases through Finish: Outbound and Forward, onResponse, problem documents, RateLimit fields, commit, streaming copy, onLog, metrics, accessLog.when, tap publish, `handlertest` | Not started |
| 4.27 | WP-66 replay | `internal/gateway/replay/`; package review | Not started |
| 4.28 | WP-67 sd_notify | `internal/gateway/sdnotify/`, which the drain package imports | Not started |
| 4.29 | WP-67 drain | `internal/gateway/drain/`; package review | Not started |
| 4.30 | WP-68 CLI admin client | The whole package | Not started |
| 4.31 | WP-86 plain-upstreams forwarder | The whole package | Not started |
| 4.32 | WP-87 test kit topology | The whole package | Not started |

## 5. Wave 5

| Step | Package | Scope | Status |
|---|---|---|---|
| 5.01 | WP-69 pipeline facade | The whole package | Not started |
| 5.02 | WP-70 snapshot compiler | Consumers, the seeded CEL Builder, the Prepare calls, one Filter per (Policy, scope, Phase set) with carry-over, compiled Policies, BodyNeeds | Not started |
| 5.03 | WP-70 | The Route protocol, LegChains and Legs, Routers, forwarders, listener TLS, Gateway limits, the metric Plan, State Store handles, W workers that yield; package review | Not started |
| 5.04 | WP-71 built-in Filter tables | The whole package | Not started |
| 5.05 | WP-72 CLI node commands | `node drain` and `node dump` | Not started |
| 5.06 | WP-89 handover | The protocol over `handover.sock` with SO_PEERCRED: messages, refusals, usage reports, the format marker | Not started |
| 5.07 | WP-89 | The steering swap, linger, shared ceilings, the cgroup memory limit, MAINPID and READY, the holder's Drain; package review | Not started |

## 6. Wave 6

| Step | Package | Scope | Status |
|---|---|---|---|
| 6.01 | WP-73 Last-Known-Good | `internal/gateway/lkg/`: content and pointers, the fsync protocol, format markers, cleanup, the retry with `lkg_write_failed` | Not started |
| 6.02 | WP-73 file source | `internal/gateway/source/`: the file-mode polling watcher (R-36) and SIGHUP | Not started |
| 6.03 | WP-73 activation | `internal/gateway/reload/`, which uses both: the loader goroutine (pending slot, K gate, verify, the pipeline with Serving, secret resolution, compile, bind, the Binding set then publish, resolver Activate), the boot order with `lkg_boot`, readiness reasons, activation metrics and revision info; package review | Not started |
| 6.04 | WP-74 CLI bundle commands | `bundle validate`, with Environment selection, the sources and limits flags, outputs and exit codes | Not started |
| 6.05 | WP-74 | `bundle render` | Not started |
| 6.06 | WP-74 | `bundle diff` | Not started |
| 6.07 | WP-74 | `bundle build`; package review | Not started |
| 6.08 | WP-75 configuration conformance | The golden corpus: canonical bytes, digests, render and effective outputs, diagnostics in text and JSON | Not started |
| 6.09 | WP-75 | The JSON subset, loader, hostile-input and negative corpora, with exact codes and paths | Not started |
| 6.10 | WP-75 | The examples through the pipeline, and identical digests on every platform; package review | Not started |

## 7. Wave 7

| Step | Package | Scope | Status |
|---|---|---|---|
| 7.01 | WP-76 ruralzd wiring | Settings, the telemetry runtime, the data dir and lock, the admin server with its injected handlers, the State Store manager, the secret resolver, the upstream manager | Not started |
| 7.02 | WP-76 | The built-in Filters and components on owned goroutines, the CEL compiler, the reload loop, the listener set, signals, shutdown order, exit codes, the handover successor path, `cmd/ruralzd` | Not started |
| 7.03 | WP-76 | Lead step: remove sizegate's M0 stub (in WP-27's `internal/tool/sizegate`, its own commit) so the 89 MiB idle RSS gate is enforced; package review | Not started |
| 7.04 | WP-77 CLI launcher | `internal/cli/launch/` | Not started |
| 7.05 | WP-77 | `dev run` and `dev tap` | Not started |
| 7.06 | WP-77 | The root `Run`, registry, help, planned-command table, version and completion wiring, `cmd/ruralz`; package review | Not started |
| 7.07 | WP-78 property suites | The whole package | Not started |

## 8. Wave 8

| Step | Package | Scope | Status |
|---|---|---|---|
| 8.01 | WP-79 end-to-end suite | Scenarios 1, 2, 6 and 7, `bundle validate` over `examples/`, secret leak canaries | Not started |
| 8.02 | WP-79 | One end-to-end test per M1 command, cross-binary diagnostics equality | Not started |
| 8.03 | WP-79 | The T1 and T2 topologies, Hot Reload and LKG, handover and Drain with zero failed requests, the air-gapped variant, the quickstart in 10 minutes or less; package review | Not started |
| 8.04 | WP-80 chaos | CE-1 to CE-6 | Not started |
| 8.05 | WP-80 | CE-12, CE-15, CE-16 and the scale selection | Not started |
| 8.06 | WP-80 | `chaos-scale.yml`; package review | Not started |
| 8.07 | WP-81 benchmark harness | The LoadTool adapter, the measurement rules, records and `record.schema.json` | Not started |
| 8.08 | WP-81 | The gates: macro p99 and absolute gates, the budget table, the latency rotation, the soak and memory runs | Not started |
| 8.09 | WP-81 | `allocgate.json`, benchmark registration, the PB-7 and PB-8 hooks, the budget export; package review | Not started |
| 8.10 | WP-82 protocol conformance | The whole package | Not started |
| 8.11 | WP-90 benchmark scenarios | The scenario Bundles, with recorded digests | Not started |
| 8.12 | WP-90 | The R1 ladder generator | Not started |
| 8.13 | WP-90 | The functional reduced-scale variants and the constants runs; package review | Not started |
| 8.14 | WP-91 Node integration: CEL | `test/integration/cel/` | Not started |
| 8.15 | WP-91 Node integration: telemetry | `test/integration/telemetry/`; package review | Not started |
| 8.16 | WP-92 Node integration: security | The whole package | Not started |
| 8.17 | WP-93 Node integration: State Store | The whole package | Not started |
| 8.18 | WP-94 T5 systemd | The whole package | Not started |
| 8.19 | WP-95 Compose and air-gapped image | The whole package | Not started |
| 8.20 | WP-99 game days | The whole package | Not started |

## 9. Wave 9

| Step | Package | Scope | Status |
|---|---|---|---|
| 9.01 | WP-96 release workflows | Lead step first: delete `internal/tool/modpin` (R-61), its own commit. Then `releasegate`, with a fake GitHub API in its tests | Not started |
| 9.02 | WP-96 | `release-build.yml` | Not started |
| 9.03 | WP-96 | `release.yml`, `release-verify.yml` and the image-check wiring; package review | Not started |
| 9.04 | WP-97 RH-1 | You provision RH-1 (rented bare metal, OQ-performance-budgets-and-benchmarking-2 (b)). Then the host preparation scripts under `deploy/benchhost/` and the runner | Owner |
| 9.05 | WP-97 | The budget runs, the soak, CE-12 at full rate, the records and the budget export | Not started |

## 10. Wave 10

| Step | Package | Scope | Status |
|---|---|---|---|
| 10.01 | WP-98 | `scripts/repo-settings.sh` and the release runbook section. Owner choice before it lands: the branch protection it applies to `develop` (required checks and the merge queue) stops direct pushes, so steps 10.02 and X.01 to X.07 need another way to land, such as pull requests through the merge queue | Not started |
| 10.02 | WP-98 release | You tag `v0.1.0-rc.1`, `release.yml` is fixed until green, then you tag `v0.1.0` and SM-12 is confirmed | Owner |

## X. M1 exit

One step per M1 exit criterion in `docs/roadmap/01-roadmap-and-milestones.md` ("M1 exit criteria" 1 to 7): check it against the records, fix any gap, and record the result. These steps are split further when they are reached.

| Step | Package | Scope | Status |
|---|---|---|---|
| X.01 | Exit 1 | Release `0.1.0` with every artifact signed (SM-12) | Not started |
| X.02 | Exit 2 | Gateway-added latency on RH-1 in S2 (SM-4, SM-5) | Not started |
| X.03 | Exit 3 | S1 throughput (PB-4) and the Hot Reload of 5,000 Routes (PB-7) | Not started |
| X.04 | Exit 4 | The scripted quickstart (SM-7) | Not started |
| X.05 | Exit 5 | Golden corpus, conformance, fuzz crashers, end-to-end tests per M1 command, chaos experiments and game days | Not started |
| X.06 | Exit 6 | Feature Catalog milestones and the Planned (M1) rows (SM-2, SM-3) | Not started |
| X.07 | Exit 7 | Every Open question that blocks an M1 feature is closed | Not started |

## After M1

M1 integration, end-to-end tests and documentation, then M2, as the README's working rules say. They are split into steps here when M1 exits.

## Step notes

Newest first. Each step adds a short note when it lands: its commits, its run IDs, its agent usage and anything the next steps need.

- 0.01 (2026-10-24):
  - The plan adopted, with user decisions 27 to 35 and the AGENTS.md sentence for decision 30. Review rounds so far, each by a fresh reviewer agent: round 1 reported 4 major and 18 minor findings, round 2 reported 3 major and 11 minor, round 3 reported 7 major and 5 minor, round 4 reported 4 major and 10 minor, and round 5 reported 3 major and 9 minor, all fixed in this step. By decision 34 the step lands after round 5's fixes, without a sixth round. The work ran in this session, with no workflow.
  - The S2 work from `wip/s2-handoff` (tip `266e9a8`) is parked in `wip/`. `WP-35.patch`, `WP-37.patch`, `WP-38.patch`, `WP-39.patch` and `WP-51.patch` are each `git diff --binary --no-renames 76edfe0 266e9a8` over that package's owned paths. WP-36's is split into `WP-36-overlay.patch` and `WP-36-subst.patch`, each of which builds and passes its tests alone. Together they rebuild the handoff tree's package paths exactly, and every intermediate state of the steps that apply them passes `go vet` and the `CGO_ENABLED=0` tests (checked by round 2's reviewer in scratch copies).
  - `WP-51-attempt3-reference.patch` is `451a729..266e9a8` over WP-51's path: the G04 attempt 3 edits made on the Windows machine after the first stop. The attempt's earlier partial edits are inside `451a729`, and so inside `WP-51.patch`. `451a729` becomes unreachable when the branch is deleted.
  - The branch's plan files are part of this step. Its run journals and scratch archive stay off `develop`: on this machine under `runs/journals/` and `runs/scratch/` (excluded in `.git/info/exclude`), and on the Windows checkout. A temporary safety bundle of the branch is at `/tmp/claude-1000/-home-ravindu-Desktop-Projects-ruralz/7b12d423-1929-4551-9c3e-50cb84a825d0/scratchpad/wip-s2-handoff.bundle`, on tmpfs, so it is lost at the next reboot.
  - Checks for this plan-only step: the plan checks, `git diff --cached --check`, repocheck and the title check before the commit; the DCO and signature checks on the commit before the merge. The Go whole-tree checks have nothing to check (no Go change).
  - `wip/s2-handoff` stays on origin until step 3.20 (decision 33); this step deletes only its own branch.
