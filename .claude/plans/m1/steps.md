# M1 steps

This is the plan of record for the whole of M1, to its exit, from 2026-10-24 (`user-decisions.md` items 27 to 40). It cuts the remaining work into small steps. Each step changes one package, one part of a package or one small set of documents, is built with its tests, and lands on `develop` when its tests and checks pass, before the next step starts. One review of all M1 work comes at the end, before the release: stage R for the code and C.03 to C.03i for the documents (decision 40). What each package must do is still defined by `arch/wps.json` (owned paths, scope and spec rows), `specs/00-architecture.md` 4.4 and the area specs; this file only cuts that work into steps and records progress. `wave3-plan.md` is the record of how wave 3 ran until 2026-10-24.

## How a step runs

1. **Start.** `git status --porcelain` must be empty: uncommitted work belongs to the step it was made for, so finish or pause that step first (item 7). Then run `git fetch origin --prune`, `git switch develop` and `git merge --ff-only origin/develop`.
   - Paused steps are the `origin/step/*` branches: resume one instead of starting a new step (item 7).
   - A local `step/*` branch with no `origin/step/*` is this machine's unfinished step: resume it. Only when develop's `steps.md` marks that step Done is the branch left over from a step finished on another machine; then check that `git log develop..step/<id>` holds only pause commits, and delete it with `git branch -D step/<id>`.
   - `git branch -a` must then list only `main`, `develop`, until step 3.20 `wip/s2-handoff` (decision 33), any `main-workflows/<id>` branch whose pull request into `main` is still open (decision 38), and Dependabot's `dependabot/*` branches. Create the branch with `git switch -c step/<id>` (for example `step/3.01`).
   - If the step names a parked patch in `wip/`, apply it on the new branch with `git apply .claude/plans/m1/wip/<name>.patch` (a paused step's branch already holds it); the step's plan commit (item 4) deletes the patch file.
2. **Work.** A step runs without the Workflow tool and its run scripts (decision 40); agents are fine. The lead may give parts of the step to subagents, several at once where the parts are independent, such as an implementation part, a library inventory or a final-review step's review. Agents edit only the paths of the step's commits (item 4), never `go.mod`, `go.sum` or `internal/tool/modpin` (architecture 4.1 rule 3) or `.claude/settings.json`, which only the lead edits, in the user's session, and never commit.
   - The lead first lists from the sources every finding, owner choice and contract request of the package (both sources; a later request replaces one it restates), each assigned to the step whose code it changes. A request that several packages filed is one class, applied once by the first step that reaches it; the later steps drop their copies.
   - The code that a backlog entry's `settledBy` (`runs/s8-backlog.json`) puts in this step is in its scope, with that change's document text; records of code that stays as it is wait for 3.69 to 3.69k (decisions 31 and 37), and what the package's later steps add waits for them.
   - If the scope separates a decision from the result that makes it observable (an error code from the check that produces it, a resource from its teardown), the lead moves the result into the step before the work starts.
   - Owner choices are asked before the work starts. Known contract requests are applied in the step, with their paths among the step's commit paths, and so is a request that comes up during the step.
3. **Done when.**
   - The step's scope is built and every known finding and request it carries is fixed; no review runs per step (decision 40). A package's last step ends with its package check: the whole package against its 4.4 entry, its "Done when" included. The package counts as done after it, and its review comes in the final review (stage R).
   - Every requirement of the scope has its test, and every finding was fixed with its whole class, with a test per member that fails without the fix (AGENTS.md "Quality standard").
   - The scoped checks pass for the touched packages (`go build`, `go vet`, lint and format, tests with and without the race detector, `go test -tags integration` where the package has integration-tagged tests, the Go 1.26 floor), then the whole-tree checks below. Every step also runs `python3 -I .claude/plans/m1/tools/check-steps.py` after updating this file.
   - A step that changes only `.claude/plans/` and the agent instruction files (`AGENTS.md`, `CLAUDE.md`), which no build or CI stage reads, runs the plan checks instead: `python3 -I .claude/plans/m1/tools/check-steps.py`; after staging, `git diff --cached --check -- . ':!*.patch'` (patch files hold diff context lines that are a single space); `go run ./internal/tool/repocheck`; and the title and DCO checks of item 4. The other stages have nothing to check there.
   - Behavior and decision changes update `docs/` in the same step, or add an Open questions row to the owning document when they need a decision (AGENTS.md "Sources of truth"; decision 31). Contract requests are applied in the step (item 2): plan-spec ones go into the plan commit, and changes to another package's owned paths into lead commits (item 4; architecture 4.1 rule 3). A request that needs a user decision is asked during the step, before it is applied. The backlog deferred to S8 before 2026-10-24 (`runs/s8-backlog.json`) has its decisions at 3.00 and 3.68; the code they need in packages S1 or S2 closes lands in those packages' steps (decision 37), and the records, with code in packages that close later (such as schemaidx for E18 and the executor for E63), wait for steps 3.69 to 3.69k.
   - A check this machine cannot run runs in CI on the step branch before item 5 (decision 35): the Windows and macOS jobs for `internal/cli`, `internal/buildinfo` and `pkg/`, the container State Store leg, and suites that need `redis-server` or Docker. Push the branch, run `gh workflow run pr-fast.yml --ref step/<id> -f all=true` and the same for `pr-full.yml`, and wait until both pass (`gh run watch`). The user installs and logs in `gh` before the first such step. GitHub dispatches only workflows that exist on `main`, so step 0.03 brings `develop`'s workflow files there, and every step that adds or changes a workflow with a dispatch or call trigger opens the same kind of pull request in that step, whether or not it dispatches it (decision 38). A package whose "Done when" needs a nightly job, such as the Chaos job at 10 Nodes (WP-80, WP-99), dispatches `nightly.yml` on the step branch the same way; while RH-1 is absent, as before 9.04, its Latency job then fails at the RH-1 check, and the step closes the issue that failure opens with a pointer to C.10 to C.12.
   - The step notes record the step's commits, the usage of its agents (`tools/usage.py`), its CI runs, and every check that could not run anywhere, with the reason (CLAUDE.md).
4. **Commit** on the step branch, signed with the machine's `~/.ssh/id_ed25519` through the repository git config (README, "Working rules from the user"):
   - If the branch holds pause commits (item 7), first run `git reset --soft $(git merge-base develop HEAD)`; their changes stay staged.
   - The commits a step may make, each holding only its own paths. Stage each with `git add --pathspec-from-file=<list>` and commit with `git commit -S -s -F <msg> --pathspec-from-file=<list>`; `git apply` creates untracked files, which a pathspec commit refuses unless they are added first.
     - One per package of the step, with only that package's owned paths.
     - One `docs(<slug>)` commit per design document the step changes (decision 31), or one `docs:` commit without a scope when a change spans several documents, such as an ADR with its bookkeeping (`docs: add ADR-NNNN ...`, `.claude/rules/adr.md`).
     - One `build(deps)` commit, made by the lead only (architecture 4.1 rule 3), for `go.mod` and `go.sum`, with the `.golangci.yml` and `internal/tool/modpin` entries that belong to the same dependency change.
     - One lead commit per other package the step changes, with that package's scope: a contract request that touches another package's owned paths (architecture 4.1 rule 3), or a lead edit the architecture names (7.03: `internal/tool/sizegate`, 4.1 rule 1 and convention 7; 9.01: the `internal/tool/modpin` deletion, R-61).
     - One `chore:` commit for agent instruction and settings files (AGENTS.md, CLAUDE.md, `.claude/rules/`, `.claude/settings.json`), in which a package's last step also brings AGENTS.md's "What exists today" and "Not built yet" lists up to date, and one `docs:` commit without a scope for repository documents outside `docs/` (CONTRIBUTING.md, README.md).
     - Then the step's plan commit (`chore:`): this file's status row and step notes, any record the step keeps in `runs/`, the plan-spec edits (`specs/`, `arch/`, `forwards.json`, the WP-51 audit record), and the deletion of the `wip/` patch files the step applied.
   - Each title is checked with `go run ./internal/tool/commitcheck title -title "<title>"`. A body has 2 to 6 lines that name the step, then `Refs: WP-xx (M1 architecture)` where a package is concerned. No `Co-Authored-By` trailer. Then `go run ./internal/tool/commitcheck dco -range develop..HEAD`; `git log --format='%G? %GK' develop..HEAD` must show `G` and this machine's key on every commit, and `git status --porcelain` must be empty.
   - Steps that touch two-approval paths (`go.mod`, `.github/`, `api/`, `pkg/`, `docs/_meta/`, `test/bench/allocgate.json`, `nolicensecheck.allow`, the security-sensitive packages) land like any other step (decision 32).
5. **Merge.** Run `git fetch origin` first: if `origin/develop` moved during the step (a pull request you merged, such as Dependabot's), rebase the step branch onto it (`git rebase origin/develop`, which signs the rebased commits again) and rerun the checks of item 3 and the commit checks of item 4. Then `git switch develop`, `git merge --ff-only origin/develop`, `git merge --ff-only step/<id>`, `git push origin develop`. No workflow runs on a push to `develop`, since no push trigger names it, so the checks of item 3, with its CI runs on the step branch, are the gate.
6. **Clear.** If the step branch was pushed, delete it on origin first (`git push origin --delete step/<id>`): a branch whose upstream still holds pause commits refuses `git branch -d`. Then `git branch -d step/<id>`, and delete any other branch except `main`, `develop`, until step 3.20 `wip/s2-handoff` (decision 33), every open decision-38 `main-workflows/<id>` branch, and Dependabot's `dependabot/*` branches, locally and on origin. `git ls-remote --heads origin` and `git branch -a` must then show only those.
7. **Pause.** A step stopped before it is done commits its work in progress on its branch, signed, as `chore: pause step <id>`, with the resume point in the step notes, and pushes the branch so another machine can resume it. Until the step is done, its branch is the only one besides `main`, `develop`, until step 3.20 `wip/s2-handoff`, every open decision-38 `main-workflows/<id>` branch, and Dependabot's `dependabot/*` branches. Item 4 drops the pause commits before the real commits, and item 6 deletes the remote branch.

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
- A step should take about two hours of work or less.
- When a step's findings, known ones or a final-review step's, number more than about 15, or their fixes fall into unrelated classes, split the rest into follow-up steps before fixing, each named with the next free letter (`3.37c` after `3.37b`, `R.05b` after `R.05`). Each class stays inside one step.
- A step that waits on something only the user can provide, such as RH-1, gets the status `Blocked: <reason>`, and the steps that do not depend on it go on. A step that has not started gets the mark through a branch `step/<id>-blocked`, cut from `develop`, that changes only this file (no new row) and is fast-forwarded and deleted like a step. A step that had started does it on its own branch `step/<id>`: it drops its pause commits (`git reset --soft $(git merge-base develop HEAD)`), moves the work in progress, with the content of any parked patch it applied, out of the commit and the working tree into `wip/<id>.patch`, deletes those parked patch files, writes "Applies `wip/<id>.patch`" in place of their "Applies" text, with the Blocked status, into its row, and lands that one commit the same way; applying that patch on a new branch is its first action when it resumes. From 10.01 on, every step, Blocked ones included, lands through the route that 10.01's owner choice sets.
- The steps of one package run in order, and a package that another step of the same package imports (specs/00 section 1.2) is built first. The packages inside a wave do not depend on each other's code, but their tests may import one another. A step that changes a package runs the tests of every package whose tests import it (`go list -test -deps` finds them; for example, the schemaview tests import profile), and those test files are among the step's commit paths, so a fix to them lands in the step. A wave starts when the previous one is done, Blocked steps excepted.
- "Owner" marks a step only the user can do or decide. "Owner choice" marks a finding or request whose fix needs the user's choice; ask it during the step.

## Status

| Stage | Steps | Done |
|---|---|---|
| 0 Plan and tooling | 4 | 2 |
| 3 Wave 3: close S2 and S1, the other wave-3 packages, decisions and documents | 93 | 0 |
| 4 Wave 4 | 42 | 0 |
| 5 Wave 5 | 11 | 0 |
| 6 Wave 6 | 11 | 0 |
| 7 Wave 7 | 9 | 0 |
| 8 Wave 8 | 27 | 0 |
| 9 Wave 9 | 5 | 0 |
| R Final review of all M1 code | 106 | 0 |
| C M1 close-out before the release | 29 | 0 |
| 10 Wave 10 | 2 | 0 |
| X M1 exit | 3 | 0 |
| **All** | **342** | **2** |

Next step: 0.02.

## Done before this plan

| Part | Work packages | State |
|---|---|---|
| Wave 1 | WP-01 | Done; its fresh review is R.01 to R.04 |
| Wave 2 | WP-02 to WP-30, WP-32, WP-83, WP-84 | Done; their fresh reviews are R.05 to R.42 |
| Wave 3, S1 | WP-33, WP-35, WP-36 | Committed with open findings; steps 3.13 to 3.15 (with 3.13b) and 3.21 to 3.30 close them |
| Wave 3, S2 | WP-37, WP-38, WP-39, WP-51 | Committed with open findings; steps 3.01 to 3.12 and 3.16 to 3.20 close them |

## 0. Plan and tooling

| Step | Package | Scope | Status |
|---|---|---|---|
| 0.01 | Plan | Adopt this plan: the steps, user decisions 27 to 35, the AGENTS.md sentence for decision 30, the README, wave-3 plan and architecture updates, signing with `~/.ssh/id_ed25519`. Park the S2 work of `wip/s2-handoff` in `wip/`; the branch stays until step 3.20 | Done 2026-10-24 |
| 0.01b | Plan | Cover the whole of M1 in small steps: the work done before this plan, the M1 close-out stage C and the exit stage X, 31 large steps split into smaller ones (`b` rows), and step 0.03 for workflows on `main`, and the S8 backlog inventoried into `runs/s8-backlog.json`: its decisions at 3.00 and 3.68, its code settled in the package steps (3.11b among them) and its records in 3.69 to 3.69k; decisions 36 to 40 | Done 2026-10-25 |
| 0.02 | Tools | `make tools`, so `bin/golangci-lint` v2.13.2 and actionlint exist for the whole-tree checks of step 3.01 (the run scripts' step mode is dropped by decision 40) | Not started |
| 0.03 | Workflows on main | Owner step (decision 38): a pull request into `main`, from a branch `main-workflows/<id>` cut from `main` that item 6 does not clear while the pull request is open, that adds `pr-full.yml` and `main.yml` and brings the dispatch inputs of `develop`'s `pr-fast.yml` and `nightly.yml` to `main`'s copies. `main`'s copies of the added files keep only their `workflow_dispatch` and `workflow_call` triggers, without push, pull_request or schedule triggers, so nothing runs them against `main`'s code (the pull request's own checks are `main`'s `pr-fast.yml`, `pr-title.yml` and `approvals.yml`, which wants two approvals for `.github/`). First check whether `main`'s branch protection requires that check; if it does, you merge with an administrator bypass or relax the rule for decision-38 pull requests, recorded in the step notes, and the same for later workflow pull requests. Check once whether a dispatch reads the inputs of the dispatched branch's file. Every later step that adds or changes a workflow with a dispatch or call trigger brings it to `main` the same way in that step, whether or not it dispatches it | Owner |


## 3. Wave 3

### Backlog decisions S1 and S2 need first

User decision 37: a package settles every pending backlog request that touches its code, with the document text for that code, by its last step's package check (decision 40). These backlog entries (`runs/s8-backlog.json`, see the wave-3 decisions below) have options that change the code of a package S1 or S2 closes, so you decide them before step 3.01, and the steps that own that code settle it (3.02, 3.03, 3.05, 3.11, 3.11b, 3.13b, 3.15, 3.16, 3.17).

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.00 | Owner decisions | The 16 backlog entries whose choices may change the code of a package S1 or S2 closes, asked together and recorded in `user-decisions.md`: E21 (the `$${` rule, which step 3.21 then records), E26 (JSON null in free content), E28 (a host-name marker), E30 (the `json.Valid` span), E41 (the cap of 20 issues), E46 (in-place reference cycles), E48 (the 256-byte cut of the RZ-RT-009 detail), E49 (the compile cost of combined worst cases), E58 (the unit of the pattern measure), E60 (the nesting-limit over-approximation for patternProperties), E62 (a compile memo in the stage H runner, or a change to `hub.PolicyCheck`), E64 (meta-schema check rules), E65 (Filter reuse across Policies), E66 (64-bit builds), E68 (413 for valid strings under small limits, or NFA-only matching), E70 (working-set floors) | Owner |

### Close S2: WP-51 validation.json-schema

Owned path: `internal/filter/validation/jsonschema/`. The gap classes are in `wp51-library-audit.md` section 4, and the decisions in `user-decisions.md` items 20 to 26. Steps 3.01 to 3.11 (3.01b and 3.11b included) each close their classes with a test per member; step 3.12 checks the whole package. WP-51 counts as done only after 3.12, and its review comes in the final review (decision 40). Step 3.01 applies `wip/WP-51.patch`; `wip/WP-51-attempt3-reference.patch` is for reading only and is deleted in the same plan commit.

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.01 | WP-51 | Applies `wip/WP-51.patch`. Finish G04 attempt 3 (GC-08: deterministic memory measurements), with its two open majors fixed: the heap meter misses short-lived copies such as the `uniqueItems` map, and the race run times out in `TestHandleMeasuredFit` | Not started |
| 3.01b | WP-51 | The five minors carried from G02 and G03 (`runs/w3-s2-wp51-carry2.json`) and the carry file's contract requests 1 to 4, 6, 10 and 11 applied to spec 07, R-77 and the audit record | Not started |
| 3.02 | WP-51 | G05a: design the one walk-time compile budget of 1 MiB (decision 20) for both its byte and count terms, and apply its one contract request to the plan specs (audit 4.2), with the parts of backlog entry E49 that 3.00's decision puts into the budget's design, together with the carry file's requests 5, 7, 8 and 9, which define the budget's fixpoint term; owner choice: that term's cap (proposed 2^18). Implement the byte terms, GC-03 (23 members) | Not started |
| 3.03 | WP-51 | G05b: implement the count terms, GC-09 (10 members), in the budget 3.02 designed; and the code, with its document text, that 3.00's decisions on backlog entries E49 and E64 require | Not started |
| 3.04 | WP-51 | G06: non-canonical pointer indexes accepted as aliases (GC-04, decision 21); the eight integer keywords above 2^63-1 clamped (GC-05, decision 22) | Not started |
| 3.05 | WP-51 | G07: regex parser work the lexical pass does not count (GC-06); anchor-name bytes and the decision wrapper's frames charged (GC-07, decision 24); and the code, with its document text, that 3.00's decision on backlog entry E58 requires | Not started |
| 3.06 | WP-51 | G08: regexp matcher costs from the compiled program with `add()` depth (GC-11); the 128 MiB stack ceiling (GC-15, decision 23) | Not started |
| 3.07 | WP-51 | G09: kept memory restated (GC-12), the number model at the library's parse sites (GC-13), values outside the decoder's data model (GC-14) | Not started |
| 3.08 | WP-51 | G10a: pin every walk rule and resolution mirror per keyword, form and route (GC-16) | Not started |
| 3.09 | WP-51 | G10b: prove every work-model term with a test that a mutation fails (GC-17) | Not started |
| 3.10 | WP-51 | G11a: bound the raw `config.schema` decode before the size check (GC-10); deterministic compile diagnostics, with `config.schema` decoded under the configuration's own depth limit (GC-19, decision 25) | Not started |
| 3.11 | WP-51 | G11b: keyword locations per error kind (GC-18); library deviations documented and pinned (GC-20); round-9 minors 2 and 4; and the code, with its document text, that 3.00's decisions on backlog entries E41, E48, E68 and E70 require | Not started |
| 3.11b | WP-51 | The code, with its document text, that 3.00's decisions on backlog entries E46, E60, E62, E65 and E66 require, if any; backlog entry E54's `filter.Factory` contract (a Build whose context ended returns `ctx.Err()` uncoded, a `filter.Shared` failure that is not a compile finding passes unchanged, and the snapshot compiler rejects the Revision only on RZ-CFG errors), and for E65's WP-70 option the `filter.Shared` contract, both in `internal/filter/filter.go`, and for E62's `hub.PolicyCheck` option that contract in `internal/config/hub` (all WP-01, lead commits), each with its specs/00 section 2 mirror, before any Factory package closes | Not started |
| 3.12 | WP-51 | The whole package against its 4.4 entry, with every gap class of `wp51-library-audit.md` closed and the contract requests of 3.01 to 3.11b applied; package check. Its review is R.60, in the final review | Not started |

### Close S1 and S2: the open findings of WP-35 to WP-39

The open findings are in `runs/w3-s2-minors-continue3.json`; the numbers below are their positions in each package's list. Each package's contract requests are in that file's `_meta.contractRequests` and in `reports/<WP>.json`. The owner choices and the requests of each step come from those sources through the lead's list (item 2), not from this table. The first step of each package applies its patch from `wip/`; for WP-37 and WP-39 that patch holds the package's whole work in progress, so the first step lands all of it.

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.13 | WP-35 schema view | Applies `wip/WP-35.patch`, with fix0, which closed its 11 findings (`reports/WP-35.json`); its 12 contract requests (the same in both sources); requests 4, 5, 8 and 9 of `reports/WP-35.json` (JSON indexes 3, 4, 7 and 8) change WP-01 (`internal/config/tree`, `internal/config/diag`) and WP-03 (`internal/config/schemaidx`) as lead commits | Not started |
| 3.13b | WP-35 | The stage F code that 3.00's decisions on backlog entries E26, E28 and E30 require, with its document text (for E28 the configuration model's `x-ruralz-*` keyword table, keeping the document's net word change at zero or below, the layout document's marker vocabulary and the marker table in `.claude/rules/config-schema.md`; the configuration model for E26 and E30), and with `pkg/config/v1alpha1` and its regenerated schema, schemagen and schemaidx as lead commits where E28 takes the marker or E30 moves or replaces the `json.Valid` check; backlog entry E20's `Node.Secret()` accessor in schemaidx (a WP-03 lead commit), with schemaview's callers switched to it; package check | Not started |
| 3.14 | WP-36 overlay | Applies `wip/WP-36-overlay.patch` (the overlay work only; it builds and tests without the substitution patch). Findings 3, 4, 8, 9 and 11 (two majors, not reproduced yet), and the requests that change the overlay code | Not started |
| 3.15 | WP-36 substitution | Applies `wip/WP-36-subst.patch`. Findings 1, 2, 5, 6, 7 and 10 (two majors in the credential redaction class, not reproduced yet), the requests that change the substitution code, backlog entry E23 (the schemaidx export of a Walk position's undispatched schema, a WP-03 lead commit, that substitution then uses), 3.00's decision on E21: the substitution side, and under its option (b) also `profile.Encode`, which then leaves `${` verbatim in forbidden positions (a WP-33 lead commit; `encode.go` does not depend on goccy, so the C2 rebuild keeps it), with the rule text of spec 01 reqs 28 and 31 and the configuration model's substitution paragraph and render sentence, and substitution's callers switched to E20's accessor; package check | Not started |
| 3.16 | WP-37 conversion, contract and work accounting | Applies `wip/WP-37.patch`. Findings 1, 2, 4, 6, 7, 8, 9, 10 and 14: the converter contract and the checks after conversion, budgets and yield accounting, the flaky `TotalAlloc` test; and stage G's `JSONSchemaDocument` decode accounting, with its document text, as 3.00's decision on backlog entry E30 requires | Not started |
| 3.17 | WP-37 defaults and normalization | Findings 3, 5, 11, 12, 13 and 15 to 18: the number literal form, silent drops, untested branches, comments; the stage G code, with its document text, that 3.00's decisions on backlog entries E26 and E28 require (under E26's canonical-null option also the null form of `internal/config/canonical`, a WP-39 lead commit, so stage G never keeps a null the canonical form refuses), and stage G's callers switched to E20's accessor; package check | Not started |
| 3.18 | WP-38 precedence | Applies `wip/WP-38.patch`. Reproduce the 9 findings with tests and fix them (the adversarial lens that did not report runs in the final review); package check | Not started |
| 3.19 | WP-39 encoder and yield | Applies `wip/WP-39.patch`, on top of 3.17's canonical null form if there is one (`git apply -3` where its context moved). Findings 1, 2, 4, 7, 11, 16, 18 and 19: the yield class over every input-length loop, the level-0 and `since` rules, encoder bounds | Not started |
| 3.20 | WP-39 decoder, dump and streams | Findings 3, 5, 6, 8, 9, 10, 12 to 15 and 17; package check. S2 then closes: delete `wip/s2-handoff` on origin (decision 33), and on this machine `runs/journals/`, `runs/scratch/`, their two lines in `.git/info/exclude` and the S2 stash; ask the user to clear the Windows checkout's copies | Not started |

### Close S1: WP-33 restricted YAML profile

Owned paths: `internal/config/profile/` and `test/fixtures/yaml-test-suite/`; the document and dependency steps commit outside them as item 4 allows. The steps follow `yaml-parser-decision.md` section 5 and user decisions 8 to 11, with one change of order that decision 26 requires: "WP-33's rebuild waits for ADR-0020". So ADR-0020 comes first (3.23), as `proposed` if you choose so, which keeps the fallback below open; it takes effect at the switch (3.28).

Stop rule and fallback, decision 8 verbatim: "stop if the second review round confirms more than two blocker or major findings in parser-core code, or the parser's agent cost passes $250 before a clean round; then keep WP-33 as committed meanwhile and move to go-yaml v4 pinned at rc.3." With no review rounds before the end, tests decide it (decision 40): the fallback applies if the parser steps 3.24 to 3.26 cannot get their tests passing, including 3.27's yaml v3 differential with only the listed deviation classes, before their usage passes $250, tallied with `tools/usage.py` after each step. A step that meets the stop rule does not land: its branch is deleted, and a re-plan step replaces the WP-33 steps not yet landed. It removes the unexported parser of the parser steps that landed, reverts the C2 text of 3.21, and revises the proposed ADR-0020 and its rows for v4.

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.21 | WP-33 plan text | Spec and plan text for C2 (brief section 5.1 step 2): spec 01, the specs/00 rows and a new R-row, the spec 02, 03, 10 and 11 lines, the WP-33 scope in `arch/wps.json`, the `forwards.json` notes; the 7 contract requests in `reports/WP-33.json`, with the R-row of the `$${` rule as 3.00's decision on backlog entry E21 set it (under its option (b), spec 01 reqs 28 and 31 change with their code in 3.15) | Not started |
| 3.22 | WP-33 research | The research addendum in `docs/_meta/research/tooling-and-licenses.md` (content approved in decision 11; brief section 5.5 item 2), which ADR-0020 cites | Not started |
| 3.23 | WP-33 ADR | ADR-0020 superseding ADR-0003: you type `/new-adr` or ask for it in words, and choose its status first (`proposed` keeps decision 8's fallback open). The new-adr skill's bookkeeping lands in the same change. With status `proposed`, the skill items that make it take effect wait for 3.28, recorded as a deviation in the step notes: ADR-0003 marked superseded, the foundation pack row pointed at ADR-0020, and goccy moved to "Alternatives not chosen"; the yaml v3 test-only catalog row waits for its dependency at 3.27. ADR-0020 may name v4 only once the tech stack document lists it under "Alternatives not chosen" (`.claude/rules/adr.md`), so v4's row, with its research URL from 3.22, lands here | Owner |
| 3.24 | WP-33 parser part 1 | Unexported, beside the goccy path: scanner, flow and block tokens, directives (reserved ones ignored, decision 10), limits counted during the parse, and decision 9's strict refusals of a closing `]` or `}` aligned with its key (with its own message) and of `"a"#c`; per-construct tests; the stop rule applies | Not started |
| 3.25 | WP-33 parser part 2 | Unexported: scalars on the kept quoted and literal helpers, with decision 9's refusal of QB6E-shaped quoted lines; per-construct tests; the stop rule applies | Not started |
| 3.26 | WP-33 parser part 3 | Unexported: document structure to `tree.Node` with positions, duplicate keys, raw nodes dropped after each document, memory measured again, generator property tests, and the regression rows of brief step 3 (the inputs of WP-33's five open S1 findings, the 32 style rows, the merge-key rows, the 1,024-character key and the cost-ladder rows in `hostile_test`); the stop rule applies | Not started |
| 3.27 | WP-33 differential test | The yaml v3 differential target over the new parser and its list of known deviation classes, with everything AGENTS.md requires in the same change: `go.yaml.in/yaml/v3` as a direct requirement, its `.golangci.yml` admitted entry and the rule confining it to `internal/config/profile` tests, and its test-only catalog row in `docs/engineering/01-tech-stack-and-libraries.md`; the stop rule applies to its differential | Not started |
| 3.28 | WP-33 switch | The pre-switch differential with 3.27's deviation filters (brief section 5.1 step 4; nothing from it is committed), then `Parse` on the new parser. Delete the goccy files and the tests that only exercise them, fix the expectations that encoded goccy readings, and cut the YAML Test Suite expected failures from 87 to 67 (`go test -tags integration`). The tests of every package that imports the profile stay green (brief section 5.1 step 4; `go list -test -deps`), WP-35's, WP-38's and WP-39's among them, with their test files in the step's commit paths where an expectation encoded a goccy reading. With your OK, ADR-0020 becomes `accepted` and the skill items 3.23 deferred land (ADR-0003 superseded, the foundation pack row, goccy under "Alternatives not chosen"), with the tech stack figures, the layout, configuration model (word-neutral) and testing document lines, and decision 9's loader-reference note before WP-75 records goldens | Not started |
| 3.29 | WP-33 drop goccy | Drop goccy/go-yaml from `go.mod`, `internal/tool/modpin` and `.golangci.yml`, then `go mod tidy` | Not started |
| 3.30 | WP-33 package check | The whole package against its 4.4 entry; package check. S1 closes, and WP-33's review, with its hostile-input pass, comes in the final review | Not started |

### The other wave-3 packages

These were sections S3 to S7. The spec rows for each package are in `arch/wps.json`.

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.31 | WP-34 CEL module | One environment per place and site, the library subset, `Check` with the RZ-CFG-014 and RZ-CFG-015 messages, static cost at nominal sizes | Not started |
| 3.32 | WP-34 | Runtime: the cost limit, `ContextEval` under min(deadline, 50 ms), error kinds and per-place rules, the `ProgramSet` cache, `ReferencesRequestBody`, `Default` for retryOn and failureWhen | Not started |
| 3.33 | WP-34 | `DecodeJSON` with the budget it is given, `PrepareRoute` and `PrepareConsumer` (also in `celtest`), the place-to-annotation test, `FuzzCompile` and the cost-estimator target; package check | Not started |
| 3.34 | WP-40 telemetry runtime | The `OTEL_*` scrub, the resource (service name, version, instance id, semconv schema URL), the providers and their `Shutdown`, the logger factory | Not started |
| 3.34b | WP-40 | The OTLP/gRPC trace, metric and log pipelines with bounded queues and `otlp.tls` through tlsconf (OQ-observability-2 (a)), with the telemetry cleartext hop reported when that TLS is absent (09 req 19, through `emit.NodeStatus`), the otelslog bridge, and the pipelines' flush within the providers' `Shutdown` (5 s) | Not started |
| 3.35 | WP-40 | `/metrics` over a private registry (exporter, producer, OpenMetrics exemplars), runtime metrics, and the metric name identity gate (09 tests 32 and 33) | Not started |
| 3.36 | WP-40 | The degraded set and the other cleartext hops (`emit.NodeStatus`), `Apply` per Revision, 09 tests 36 to 38; package check | Not started |
| 3.37 | WP-42 router | Compile and the zero-allocation `Match`: host tiers, the path trie, candidate filters (methods, headers, when) over `routematch.Criteria` built with `routematch.CriteriaOf` (R-68), the precedence total order, and `match.when` errors (RZ-RT-006, a 500 without fallthrough) | Not started |
| 3.37b | WP-42 | The `ruralz.route.match` span, the compile of 10,000 Routes within budget, `FuzzRouterMatch` against a reference matcher; package check | Not started |
| 3.38 | WP-43 per-request exchange | Pooled state for `filter.Exchange`, `filter.Message` and `snapshot.RequestState`: Enter and SetLeg, when decisions, PolicyState slots, Stripe and RouteMetrics, leg views with a Vars copy, reset for reuse | Not started |
| 3.39 | WP-43 | Lazy `expr.Vars`, header views joined per RFC 9110, body access through gates and budget, `SetBody` (Content-Length, Content-Encoding and digest removal), `Decoded` with the R-63 budget | Not started |
| 3.39b | WP-43 | Identity once, rate-limit field accumulation, tee, `ReplaceResponse`, `Final`, the access-record feed, the embedded gateway timer; package check | Not started |
| 3.40 | WP-44 reuseport | `internal/gateway/reuseport/`, which the listener package imports: bind without listen, CBPF self-steering on Linux, stubs elsewhere | Not started |
| 3.41 | WP-44 listeners | `internal/gateway/listener/`: the listener set, servers (protocols, timeouts, header limits), the connection ceiling, PROXY v2, TLS from the retire holder, `Binder` with RZ-CFG-039; package check | Not started |
| 3.42 | WP-45 readiness and tap | `internal/gateway/readiness/` (the reason set) and `internal/gateway/tap/` (the NDJSON hub with per-subscriber sampling, dropped lines and the RZ-RT-019 subscriber limit) | Not started |
| 3.43 | WP-45 admin server | `internal/gateway/admin/`: its own mux on `admin.port`, the injected handlers, `/debug/snapshots`, `/debug/pprof`, adminauth, problem errors, 404 RZ-RT-020 and 405 RZ-RT-021; package check | Not started |
| 3.44 | WP-46 upstream runtimes | The manager and runtimes carried over by identity (discovery, health, pickers with their panic mode, breaker, bulkhead, retry budget), transports (pools, TLS, the guarded dialer), the Run scheduler, pool metrics, and the `upstream_panic` and `balancer_budget` reasons | Not started |
| 3.45 | WP-46 attempt loop | `Do` with deadlines and per-try timeouts; error classes and RZ-UP codes; failureWhen, which runs before retryOn in each attempt (05 req 32); retries: retryOn reading `error.kind`, Filter Retry, the replayable request body, backoff | Not started |
| 3.45b | WP-46 | hashKey evaluated once per leg with its random-Endpoint fallback and metric (05 req 14) | Not started |
| 3.46 | WP-46 outgoing request | Hop-by-hop removal, forwarding headers, Host or sni (R-28), `Tracer.Inject`, `GetBody` only for buffered bodies; package check | Not started |
| 3.47 | WP-65 post-commit queue | `internal/statestore/postcommit/`, which the manager imports: the four-class queue with byte caps, eight writers, batches, drop counters, the flush at Drain | Not started |
| 3.48 | WP-65 State Store manager | `internal/statestore/manager/`: the effective configuration, drivers by Role, injected openers, the MAC key, reuse, store handles with retain and release, `SetNodeCount`, shutdown order; package check | Not started |
| 3.49 | WP-88 revalidation | `internal/filter/cache/revalidate/`: the pool component, the partition lease (R-71), replay with validators, 304 refresh, variant delete | Not started |
| 3.50 | WP-88 coalescing | `internal/filter/cache/coalesce/`: miss coalescing for up to 4,096 keys, leader and followers, deadlines, the fallback fetch; package check | Not started |
| 3.51 | WP-47 JWKS manager | The `jwks` subpackage: the Node-wide component (fetch through egress, cache-control clamp, refresh, prefetch, limiter, `jwks_stale`, age metric) | Not started |
| 3.52 | WP-47 auth.jwt | The compact parser, the alg allow list (RS256, PS256, ES256, EdDSA), jws verification with jwk, jws and jwa only, each refusal a typed error | Not started |
| 3.52b | WP-47 | The Factory and Filter: claims decoded with jsonval, the fixed 60 s skew and 24 h lifetime (R-32), audiences, issuer config, Consumer binding through identity, and every refusal answered with its RZ-AUTH code; package check | Not started |
| 3.53 | WP-48 auth.api-key | `internal/filter/auth/apikey/` | Not started |
| 3.54 | WP-48 auth.basic | `internal/filter/auth/basic/`: PBKDF2 verify, the Node-wide throttle with its success cache, buckets and hash queue, 429 RZ-AUTH-007; package check | Not started |
| 3.55 | WP-49 auth.mtls | `internal/filter/auth/mtls/` | Not started |
| 3.56 | WP-49 authz.ip | `internal/filter/authz/ip/` | Not started |
| 3.57 | WP-49 cors | `internal/filter/cors/`; package check | Not started |
| 3.58 | WP-50 token registry | The Node-wide token registry component: client-credentials token sources shared by (tokenUrl, client, scopes), fetch through egress with https only and no redirect (R-49), each refusal a typed error, refresh ahead of expiry with jitter, single flight, the age and refresh-failure metrics | Not started |
| 3.58b | WP-50 auth.upstream-oauth2 | The Filter: Authorization injection in onUpstreamRequest, and failure 401 RZ-AUTH-020 (OQ-data-plane-8 (a)) for every registry error, as spec 06's test of a 302 shows; package check | Not started |
| 3.59 | WP-85 redisserver | The process flavor: standalone, replica with scripted failover, TLS, ACL user, three-primary cluster. Needs a local `redis-server` 7.0.15 (architecture 4.1 rule 6), which this machine lacks; installing it is your choice, else the integration tests skip and the step notes record it | Not started |
| 3.60 | WP-85 container flavor | testcontainers-go with digest-pinned images, and the selection by `RURALZ_TEST_STATESTORE` | Not started |
| 3.61 | WP-85 respproxy | RESP command counting and the fault proxy; package check | Not started |
| 3.62 | WP-53 examples | `examples/quickstart` (the SM-7 path, `quickstart.sh` with `RURALZ_FETCH_ALLOW`) and `examples/file-mode` (the T2 and T5 Bundle with overlays and the systemd install README), using only v1alpha1 fields, with a README per example | Not started |
| 3.62b | WP-53 | `examples/shop-bundle` (the configuration model example verbatim) and `examples/control` (`environments.yaml`, `clusters.yaml`), with the same rules; package check | Not started |
| 3.63 | WP-52 l4lb | `internal/testkit/l4lb/` | Not started |
| 3.64 | WP-52 loadgen | `internal/testkit/loadgen/`; package check | Not started |
| 3.65 | WP-54 releasekit | `internal/tool/releasekit/` | Not started |
| 3.66 | WP-54 depgate | The notices subcommand, the denylist per binary, the transitive license classification | Not started |
| 3.67 | WP-54 deploy files | `deploy/container/` (Dockerfile) and `deploy/systemd/` (unit, env file, sysctl drop-in, handover helper); package check | Not started |

### Wave-3 decisions

This was section S8. Under decision 31 each step applies its own requests; these steps hold only the backlog the old plan deferred to S8 before 2026-10-24. Step 0.01b listed it in `runs/s8-backlog.json`: the S1 and S2 runs' 100 requests merge into 71 entries. 5 were already applied and 22 are applied by the package steps 3.01 to 3.30, which leaves the 44 below (E-numbers are the file's entry ids). Word budgets are tight: every step in this plan that edits one of these documents keeps its net word change at zero or below (`.claude/rules/docs.md`). On 2026-10-24 the configuration model had 9,498 of 9,500 words, the performance document 6,408 of 6,500, the layout document 4,989 of 5,000 and the data plane document 6,495 of 6,500. Wave-2 requests went through `triage.json` and are not part of it. The WP-51 records come after WP-51's steps 3.01 to 3.12, so each step applies what still holds then, with the figures those steps changed.

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.68 | Owner decisions | The 2 remaining backlog entries that need your decision, recorded in `user-decisions.md`: E43 (the security-sensitive package list, which decides whether WP-51's final review gets a security reviewer) and E63 (the executor's vocabulary for a canceled request) | Owner |
| 3.69 | Backlog: WP-51 compile records | E46, E58, E60 and E64 (as 3.00 decided), E67 and E69: the plan-spec records in spec 07 and the specs/00 R-77 records, and the configuration model rows for what earlier steps did not change (a step that changed WP-51's code for one of them landed its document text) | Not started |
| 3.69b | Backlog: WP-51 request-time records | E40, E42, E48 (as 3.00 decided, where 3.11 did not change it), E52, E53, E57, E61 and E71: spec 07 (section 3, reqs 80 to 82, section 9 item 21), the specs/00 R-77 records and 1.2 row, and the data plane document's record of the 400 RZ-RT-009 number bound (E42) | Not started |
| 3.69c | Backlog: WP-51 limits in the documents | WP-51's compile guards (E41, as 3.00 decided, where 3.11 did not change them), nesting limit and fan-out, body buffering and measured fit (E45, E51, E59), then the working-set floors on those bounds and the 413 for valid strings under small limits (E70 and E68, as 3.00 decided, where 3.11 did not change them): the configuration model with decision 15's row, spec 07, R-77 and the performance document | Not started |
| 3.69d | Backlog: performance rows | The Open questions rows for validation latency and compile cost (E44, and E49 as 3.00 decided; decision 15), with the figures measured after 3.12: the performance document, the configuration model, spec 07 req 90, R-77, and the WP-81 note in `forwards.json` | Not started |
| 3.69e | Backlog: configuration rules | JSON null in free content (E26) and the `json.Valid` span (E30), as 3.00 decided, and the RZ-CFG-038 message (E35): specs 01 and 02, the R-80 row that step 3.13 adds or a new specs/00 R-row, and the configuration model for what 3.13b, 3.16, 3.17 and 3.20 did not already document | Not started |
| 3.69f | Backlog: notes for later packages | E12, E14, E22, E25, E27, E38, E39 (the WP-76 note), E50, and E62 and E65 (as 3.00 decided): `forwards.json`, `arch/wps.json`, the specs/00 4.4 scopes, spec 07 req 77 and spec 11 | Not started |
| 3.69g | Backlog: schema index | E18, and E21 as 3.00 decided, in `internal/config/schemaidx` (compile.go, the NoSubstitution comment in lookup.go; WP-03), with the spec 02 test 19 text for E21; E20 was settled in 3.13b, 3.15 and 3.17, and E23 in 3.15 | Not started |
| 3.69h | Backlog: cancellation | E63 as 3.68 decided: the `Result.Err` error.type list in `internal/filter/filter.go` (WP-01) with its specs/00 section 2 mirror, the executor's error vocabulary for a request whose context ended (WP-20), spec 07 reqs 13 and 86, and the data plane document's failure semantics; the WP-70 note of E54, whose contract was settled in 3.11b, in `forwards.json` | Not started |
| 3.69i | Backlog: host-name marker | E28 as 3.00 decided: the configuration model sentence if 3.00 chose it, where 3.13b and 3.17 did not land it; the marker, if chosen, landed with its document text in 3.13b and 3.17 | Not started |
| 3.69j | Backlog: security-sensitive packages | E43 as 3.68 decided: `scripts/ci-approvals.sh`, the layout document's list and its gosec suppression row, AGENTS.md, CONTRIBUTING.md and the matching ask rules in `.claude/settings.json` | Not started |
| 3.69k | Backlog: 64-bit builds | E66 as 3.00 decided: a note in the specs/00 4.4 WP-71 scope (ADR-0001 and the Makefile already list only 64-bit platforms, and changing the accepted ADR would need a superseding ADR); the int64 count change, if chosen, landed in 3.11b | Not started |

### Wave-3 documents

This was section S9 (WP-31).

| Step | Package | Scope | Status |
|---|---|---|---|
| 3.70 | WP-31 traffic | `docs/architecture/09-traffic-management-and-resilience.md`: its Open questions and R-rows from the WP-31 scope | Not started |
| 3.71 | WP-31 observability | `docs/architecture/10-observability.md` | Not started |
| 3.72 | WP-31 scalability | `docs/architecture/11-scalability-and-distributed-state.md`; package check | Not started |

## 4. Wave 4

| Step | Package | Scope | Status |
|---|---|---|---|
| 4.01 | WP-41 Redis driver | The rueidis client with its required options, the Ruralz dialer, URL validation and the topology fit (RZ-CFG-026), metrics and degraded reasons. Its integration tests, here and in 4.02 to 4.04, need the local `redis-server` of step 3.59 | Not started |
| 4.02 | WP-41 | `ruralz_v1.lua` (R-20, R-71), SCRIPT LOAD per primary, EVALSHA only, NOSCRIPT to RZ-STS-002 with a single-flight reload | Not started |
| 4.03 | WP-41 | The 8,192 in-flight ceiling, per-shard breaker and pacing, the cluster slot map and same-slot verification | Not started |
| 4.04 | WP-41 | The INFO memory poller and store-byte admission (R-72), the entry MAC; package check | Not started |
| 4.05 | WP-55 loader | Sources (directory, one-file Bundle, stdin), discovery and `.ruralzignore`, limits | Not started |
| 4.06 | WP-55 | Envelope and identity, Environments (RZ-CFG-022), overlay application, substitution, bounded worker parallelism; package check | Not started |
| 4.07 | WP-56 semantic validation | Reference resolution (RZ-CFG-009) and the cross-resource rules for listeners, hosts and identical match (RZ-CFG-023 over `routematch.Criteria`, R-68) | Not started |
| 4.07b | WP-56 | Composition limits (RZ-CFG-031, RZ-CFG-032), https certificate presence, `jwksUrl` and `tokenUrl` (RZ-CFG-037), identity static checks | Not started |
| 4.08 | WP-56 | Secret use collection with destinations and positions, the binding check (RZ-CFG-041), CEL sites checked with `expr.Compiler`, the injected `hub.Checks` | Not started |
| 4.09 | WP-56 | RZ-CFG-034 over each Route's chain at stage K, and the serve check RZ-CFG-040 (R-75); package check | Not started |
| 4.10 | WP-57 diff | The structural diff of two canonical Revisions keyed by identity and keyed lists, with stable ordering | Not started |
| 4.10b | WP-57 | Impact classes from `+ruralz:impact` (the security class per OQ-security-and-identity-22 (a), secret destination changes), the text and JSON models the CLI consumes, the diff-law properties; package check | Not started |
| 4.11 | WP-58 render | The rendered Bundle (`profile.Encode`, `$${` preserved, with the render idempotence property over `$${`) and `--api-version` conversion (identity in M1) | Not started |
| 4.11b | WP-58 | The `--effective --route NAME` table from precedence Rows and the build artifact (canonical plus digest); package check | Not started |
| 4.12 | WP-59 authz.cel | `internal/filter/authz/cel/` | Not started |
| 4.13 | WP-59 headers | `internal/filter/header/`; package check | Not started |
| 4.14 | WP-60 transforms | The shared engine, the dotpath grammar and tree operations, `FuzzDotPath` | Not started |
| 4.15 | WP-60 | The request Factory: body rewrite with budget reservation, header recomputation, query and path edits, per-attempt isolation | Not started |
| 4.16 | WP-60 | The response Factory and the failure semantics (RZ-RT-011, RZ-RT-012); package check | Not started |
| 4.17 | WP-61 ratelimit | The key program with its errors (RZ-RL-005), the Node-wide key table with local buckets and their denials (RZ-RL-001), the first-seen budget and local-only entries, `config.localOnly` | Not started |
| 4.18 | WP-61 | GCRA calls through `statestore.Call` and their denials (RZ-RL-002), the tokens taken kept in PolicyState for Undo (R-39), fail-open windows | Not started |
| 4.18b | WP-61 | RateLimit fields through `AddRateLimitField` (R-18), metrics, the admission property with a deterministic clock; package check | Not started |
| 4.19 | WP-62 quota | The key program, Consumer quotas from `expr.Consumer.QuotaByName` (403 RZ-RL-004 when missing), the reservation with key digest and window start in PolicyState (R-39), the denial cache and their denials (RZ-RL-003), failureMode open by default for the reservation call (OQ-configuration-model-8 (a)), Finish releasing the state | Not started |
| 4.19b | WP-62 | Refunds in onLog through the post-commit Enqueuer when a later Policy rejected or the final code is RZ-UP-005, RZ-UP-006 or RZ-UP-008, RateLimit fields; package check | Not started |
| 4.20 | WP-63 Response Cache | Directives, the key program, partitions by principal, the pipelined lookup, the hot layer | Not started |
| 4.21 | WP-63 | Coalescing and stale-while-revalidate (WP-88), stale-if-error, only-if-cached (RZ-RT-018) | Not started |
| 4.22 | WP-63 | Tee and store in Finish, unsafe-method invalidation, hit reservation release, store-byte admission, metrics; package check | Not started |
| 4.23 | WP-64 composition | Aggregate mode: one in-flight unit per parallel step (503 RZ-RT-005), each step's `when` and `pathExpression` evaluated before it (05 reqs 43 and 46), step legs through the attempt loop with one LegRun each (05 req 42; BeginLeg; concurrent legs share nothing mutable, R-42), buffer reservations (R-63), the merge under group with jsonval (05 req 47), partial responses (ruralz-partial), and the failures of 05 req 49: RZ-RT-015 (a CEL runtime error or a step body over `maxBodyBytes`), 503 RZ-RT-004 when the buffer budget cannot cover a step's reservation, a leg's own RZ-UP code for an Upstream cause, an `onUpstreamRequest` short-circuit recorded as the step's result, and RZ-UP-011 (a merged step's non-2xx) | Not started |
| 4.24 | WP-64 | Sequential mode: steps in order with the steps variables (05 req 55), a step whose `when` is false skipped (05 req 44), `pathExpression` and `when` evaluated before their step (05 req 46), the bodies that later steps read gated and capped at `maxBodyBytes` (05 req 48), the gated client body replayed to every step that sends one (05 req 54), the last executed step's response streamed unless that step transforms it, then merged (05 reqs 44 and 47), the failures of 05 req 49 (RZ-RT-015, 503 RZ-RT-004, the leg's RZ-UP code, the short-circuit, optional steps with ruralz-partial), RZ-UP-011 for a non-final step's non-2xx, and the last step's non-2xx passed through (05 req 50) | Not started |
| 4.24b | WP-64 | Conditional mode: each step's when and the chosen step's `pathExpression` (05 req 46), RZ-RT-010 (404) when no step's when is true, the chosen step's response streamed unless transformed, then merged (05 reqs 45 and 47), the failures of 05 req 49 (RZ-RT-015, 503 RZ-RT-004, the leg's RZ-UP code, the short-circuit, an optional step with ruralz-partial), and the chosen step's non-2xx passed through (05 req 50); package check | Not started |
| 4.25 | WP-66 request handler | Admission through dispatch: unit, deadlines, pin, trace, header limits, framing and normalization, the Connection-option strip, source.ip, match, the dispatch table, the Route deadline, early 413, with the pin and the unit released on every early-exit path | Not started |
| 4.26 | WP-66 | The response path: executor client Phases, the Outbound fill and Forward (R-41), onResponse, problem documents for generated responses, commit under `PinnedRequest.Mu` (RZ-RT-014, RZ-RT-016), streaming copy with pooled buffers, onLog and Finish (which release PolicyState and a cache hit's reservation), then the pin and the unit (04 req 34) | Not started |
| 4.26b | WP-66 | RateLimit fields (sfv), metrics on the stripe, accessLog.when and the access record (R-48), tap publish, the `handlertest` fixtures | Not started |
| 4.27 | WP-66 replay | `internal/gateway/replay/`; package check | Not started |
| 4.28 | WP-67 sd_notify | `internal/gateway/sdnotify/`, which the drain package imports | Not started |
| 4.29 | WP-67 drain | `internal/gateway/drain/`; package check | Not started |
| 4.30 | WP-68 CLI admin client | Admin URL validation with the loopback cleartext guard, TLS from `RURALZ_ADMIN_TLS_DIR` or flags, the bearer token file, no redirects, problem-document errors | Not started |
| 4.30b | WP-68 | The `/config/dump` decode through canonical with the 256 MiB cap, the `/tap` NDJSON stream with dropped lines, `/readyz`; package check | Not started |
| 4.31 | WP-86 plain-upstreams forwarder | The weighted leg pick (RZ-UP-008 when every weight is 0), BeginLeg per leg, the attempt loop call, response streaming with bulkhead slot release, UpstreamResponse fields for logs | Not started |
| 4.31b | WP-86 | `snapshot.LegRunner` per snapshot for replay (R-43) and the `/debug/upstreams` JSON handler injected into the admin server (R-57); package check | Not started |
| 4.32 | WP-87 test kit topology | The T1, T2 and T5-host topologies of Nodes, State Store behind faultproxy, mocks, IdP and OTLP sink, with per-Node port overlays rendered through `${E2E_*}` variables, and cleanup on `t.Cleanup` (Drain, then kill after 35 s, logs kept on failure) | Not started |
| 4.32b | WP-87 | The chaos topology, symlink swap, restart, handover, scrape and canary scan; package check | Not started |

## 5. Wave 5

| Step | Package | Scope | Status |
|---|---|---|---|
| 5.01 | WP-69 pipeline facade | `Run(ctx, Options)` for stages A to M with gating and full-stage collection, its `Options`, the `hub.Validated` output with Revision, SecretUses (with destinations) and CEL sites, and the serve check after stage M when `Serving` is set (R-75) | Not started |
| 5.01b | WP-69 | `FromResources` re-entering at stage F from canonical content with the byte-identity and digest check (R-47), identical diagnostics for every binary; package check | Not started |
| 5.02 | WP-70 snapshot compiler | Consumers, the CEL Builder seeded with the previous ProgramSet, `PrepareConsumer` and `PrepareRoute` | Not started |
| 5.02b | WP-70 | One Filter per (Policy, scope, Phase set) through `filter.Registry` with carry-over by identity and canonical config, `BuildEnv.Replayer`, compiled Policies with When and SpanName, BodyNeeds | Not started |
| 5.03 | WP-70 | The Route protocol, LegChains and Legs, Routers, forwarders, listener TLS, Gateway limits, the metric Plan, State Store handles, W workers that yield; package check | Not started |
| 5.04 | WP-71 built-in Filter tables | `builtin.New(Deps)` returning a `*filter.Registry` with every served PolicyType's Factory and the Node-wide Components (R-45); `builtin/checks.Table()` for headers, transform.request, transform.response and validation.json-schema, importing only those packages (R-34); tests that the served set equals the registry's served flags and that the checks import no jwt or statestore drivers (`go list -deps`); package check | Not started |
| 5.05 | WP-72 node drain | `node drain`, Linux only: read `holder.json`, verify the PID namespace, start time and FLOCK WRITE line, signal through a pidfd, await the PID; darwin and windows exit 2 | Not started |
| 5.05b | WP-72 node dump | `node dump` (`/config/dump` to a file or stdout); package check | Not started |
| 5.06 | WP-89 handover | The protocol over `handover.sock` with SO_PEERCRED: messages, refusals, usage reports, the format marker | Not started |
| 5.07 | WP-89 | The CBPF steering swap and the 3 s linger, shared ceilings from the usage reports, the cgroup soft memory limit | Not started |
| 5.07b | WP-89 | The successor's MAINPID then READY, and the holder's Drain through gateway/drain; package check | Not started |

## 6. Wave 6

| Step | Package | Scope | Status |
|---|---|---|---|
| 6.01 | WP-73 Last-Known-Good | `internal/gateway/lkg/`: content and pointers, the fsync protocol, format markers, cleanup, the retry with `lkg_write_failed` | Not started |
| 6.02 | WP-73 file source | `internal/gateway/source/`: the file-mode polling watcher (R-36) and SIGHUP | Not started |
| 6.03 | WP-73 activation | `internal/gateway/reload/`, which uses both: the loader goroutine (pending slot, K gate, verify, the pipeline with Serving, secret resolution, compile, bind, the Binding set then publish, resolver Activate) | Not started |
| 6.03b | WP-73 | The boot order with `lkg_boot` through FromResources, readiness reasons, activation metrics and revision info; package check | Not started |
| 6.04 | WP-74 CLI bundle commands | `bundle validate`, with Environment selection, the sources and limits flags, outputs and exit codes | Not started |
| 6.05 | WP-74 | `bundle render` | Not started |
| 6.06 | WP-74 | `bundle diff` | Not started |
| 6.07 | WP-74 | `bundle build`; package check | Not started |
| 6.08 | WP-75 configuration conformance | The golden corpus: canonical bytes, digests, render and effective outputs, diagnostics in text and JSON | Not started |
| 6.09 | WP-75 | The JSON subset, loader, hostile-input and negative corpora, with exact codes and paths | Not started |
| 6.10 | WP-75 | The examples through the pipeline, and identical digests on every platform; package check | Not started |

## 7. Wave 7

| Step | Package | Scope | Status |
|---|---|---|---|
| 7.01 | WP-76 ruralzd wiring | Settings, the telemetry runtime, the data dir and lock, the admin server with its injected handlers, the State Store manager, the secret resolver, the upstream manager, and their teardown in the Drain order of 04 req 63 (reverse order only to unwind a failed start) | Not started |
| 7.02 | WP-76 | The built-in Filters and components on owned goroutines (R-45), the CEL compiler, the reload loop, the listener set, each added to the shutdown order (components cancelled after the last snapshot retired) | Not started |
| 7.02b | WP-76 | Signals (SIGHUP rescan, SIGTERM and SIGINT drain), exit codes 0, 1 and 2, the handover successor path, `cmd/ruralzd` as wiring only | Not started |
| 7.03 | WP-76 | Lead step: remove sizegate's M0 stub (in WP-27's `internal/tool/sizegate`, its own commit) so the 89 MiB idle RSS gate is enforced; package check | Not started |
| 7.04 | WP-77 CLI launcher | `internal/cli/launch/` | Not started |
| 7.05 | WP-77 | `dev run` and `dev tap` | Not started |
| 7.06 | WP-77 | The root `Run`, registry, help, planned-command table, version and completion wiring, `cmd/ruralz`; package check | Not started |
| 7.07 | WP-78 property suites | YAML and JSON digest equality and reorder invariance through `pipeline.Run`, the conversion round trip through convert and canonical | Not started |
| 7.07b | WP-78 | Render idempotence through the pipeline, diff laws over pipeline outputs, the LKG file-mode property through reload with a fake source, seeds and nightly fuzz registration; package check | Not started |

## 8. Wave 8

| Step | Package | Scope | Status |
|---|---|---|---|
| 8.01 | WP-79 end-to-end suite | Scenarios 1, 2, 6 and 7, `bundle validate` over `examples/`, secret leak canaries | Not started |
| 8.02 | WP-79 | One end-to-end test per M1 command, cross-binary diagnostics equality | Not started |
| 8.03 | WP-79 | The T1 real-binary dev run and the T2 topologies, Hot Reload and LKG (11 req 38) | Not started |
| 8.03b | WP-79 | Handover and Drain with zero failed requests (11 G 44, 46 to 48, reduced counts), the air-gapped process variant (11 req 40), the quickstart in 10 minutes or less (target) (SM-7); package check | Not started |
| 8.04 | WP-80 chaos | CE-1 to CE-6 | Not started |
| 8.05 | WP-80 | CE-12, CE-15, CE-16 and the scale selection | Not started |
| 8.06 | WP-80 | `chaos-scale.yml`; package check | Not started |
| 8.07 | WP-81 benchmark harness | The LoadTool adapter, the measurement rules, records and `record.schema.json` | Not started |
| 8.08 | WP-81 | The gates: macro p99 and absolute gates, the budget table, the latency rotation, the soak and memory runs | Not started |
| 8.09 | WP-81 | `allocgate.json`, benchmark registration, the PB-7 and PB-8 hooks, the budget export; package check | Not started |
| 8.10 | WP-82 protocol conformance | The HTTP/1.1 wire suite: framing rejections (RZ-RT-017), header limits, hop-by-hop removal, Host and X-Forwarded-Host (R-28), encoded NUL, backslash and control bytes (OQ-security-and-identity-21 (a)) | Not started |
| 8.10b | WP-82 | The HTTP/2 wire suite with the x/net/http2 Framer and hpack (h2c prior knowledge, GOAWAY on drain), PROXY v2 LOCAL (R-65), the TLS cases; package check | Not started |
| 8.11 | WP-90 benchmark scenarios | The scenario Bundles, with recorded digests | Not started |
| 8.12 | WP-90 | The R1 ladder generator | Not started |
| 8.13 | WP-90 | The functional reduced-scale variants and the constants runs; package check | Not started |
| 8.14 | WP-91 Node integration: CEL | `test/integration/cel/` | Not started |
| 8.15 | WP-91 Node integration: telemetry | `test/integration/telemetry/`; package check | Not started |
| 8.16 | WP-92 Node integration: security | JWKS rotation, stale keys and prefetch; the mTLS request-mode switch on Hot Reload with GOAWAY and 421; upstream TLS and mTLS, wrong CA (RZ-UP-002) | Not started |
| 8.16b | WP-92 | auth.upstream-oauth2: a single fetch under 200 concurrent requests, and forced expiry; State Store TLS with requirepass, `state_store_unauthenticated`, and an uncredentialed non-loopback URL refused with RZ-CFG-026; the admin 9901 matrix over real sockets; the basic-auth throttle under load; secret-to-destination binding (RZ-CFG-041) and the entry MAC with `RURALZ_STATE_STORE_MAC_KEY_FILE` (R-49); package check | Not started |
| 8.17 | WP-93 Node integration: State Store | NOSCRIPT after SCRIPT FLUSH, the cluster same-slot and different-slot cases, a full post-commit queue | Not started |
| 8.17b | WP-93 | The ADR8 round-trip test through respproxy between a Node and `redis-server`; package check | Not started |
| 8.18 | WP-94 T5 systemd | `systemd-analyze verify` of `deploy/systemd` with ExecStart on a built binary (integration tag, stage 8) | Not started |
| 8.18b | WP-94 | The live test (build tag t5) and `t5-systemd.yml`: install, start, 20 reload handovers under load, a refused handover, stop, kill and the LKG restart; package check | Not started |
| 8.19 | WP-95 Compose end-to-end | The Docker Compose file-mode suite (build tag compose): the image from dist binaries or a given image digest, redis, a mock upstream and an OpenTelemetry Collector, over the roadmap scenarios and 09 test 48; and `image-checks.yml`, which runs it (nightly and workflow_call with its image-digest input, which release.yml passes, R-51), with a workflow_dispatch trigger so this step and 8.19b can run it in CI and C.02 on `develop` (an addition to WP-95's scope: owner choice; if you decline it, 8.19 and 8.19b record their Compose runs as skipped) | Not started |
| 8.19b | WP-95 | The air-gapped image variant on an internal network, run through `image-checks.yml`; package check. If you declined 8.19's trigger, the step notes record WP-95's "Done when" run on GitHub as a deviation, met in stage 12 on `v0.1.0-rc.1` (10.02) and confirmed at X.02 (C.02) | Not started |
| 8.20 | WP-99 game days | GD-1 to GD-5 automated drills, run by the nightly Chaos job | Not started |
| 8.20b | WP-99 | The failure rows FC-16, FC-19, FC-20 and FC-21 and the TQ row "Upstream Endpoints reset or slowed", run by the nightly Chaos job; package check | Not started |

## 9. Wave 9

| Step | Package | Scope | Status |
|---|---|---|---|
| 9.01 | WP-96 release workflows | Lead step first: delete `internal/tool/modpin` (R-61), its own commit. Then `releasegate`, with a fake GitHub API in its tests | Not started |
| 9.02 | WP-96 | `release-build.yml` | Not started |
| 9.03 | WP-96 | Owner choice first: the dry run that WP-96's "Done when" asks for (specs/00 4.4: a workflow_dispatch run on a test tag with signing and publishing disabled, green on GitHub) calls RH-1 jobs that cannot pass before 9.04 (the nightly Latency job and `chaos-scale.yml` fail at their RH-1 check, and the rh-1 runner takes no unprotected tag), so either `release.yml` gets a dry-run input that skips them (an addition to WP-96's scope, with its line in the release document), or the step notes record a deviation: the dry run green apart from those jobs, which 10.02 lists under "Skipped gates" until RH-1 runs them. Then `release.yml`, `release-verify.yml` and the image-check wiring, with that workflow_dispatch trigger and its line in the release document; `release.yml` goes to `main` with only that trigger before the dry run (decision 38), and the test tag stays outside `v*`, so pushing it starts no release; package check | Not started |
| 9.04 | WP-97 RH-1 | You provision RH-1 (rented bare metal, OQ-performance-budgets-and-benchmarking-2 (b)). Then the host preparation scripts under `deploy/benchhost/` and the runner | Owner |
| 9.05 | WP-97 | Owner choice first: the rh-1 runner runs only jobs of protected branches and tags, so you choose the kind of ref RH-1 runs on: `develop`, protected before this step by a rule that still allows step pushes, or tags matching a ruleset-protected pattern; RH-1 jobs on the release tags rely on 10.01's protected `v*` tag rule. C.08b and C.10 to C.12 then run on such a ref at the commit under test. Then the budget runs, the soak, CE-12 at full rate, the records and the budget export; package check | Not started |

## R. Final review of all M1 code

The one review of all M1 code (user decision 40), after the last implementation step of wave 9 and before stage C: each package gets a fresh reviewer against its 4.4 entry, its area spec rows and the documents, the wave-1 and wave-2 packages included (decision 19). A package with code under the security-sensitive paths of AGENTS.md also gets a security reviewer. The findings of a review step are fixed in follow-up steps named after it (`R.05b`), each class in one step with a test per member that fails without the fix; no second round runs (decision 40). The larger packages are reviewed in parts, and the documents, WP-31's among them, get the same review in C.03 to C.03i. WP-98, which 10.01 builds after this stage, is reviewed in 10.01.

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
| R.43 | WP-33 | Restricted YAML and JSON profile, with a hostile-input pass over the parser | Not started |
| R.44 | WP-34 | CEL module | Not started |
| R.45 | WP-35 | Schema validation view | Not started |
| R.46 | WP-36 | Overlays and substitution | Not started |
| R.47 | WP-37 | Defaults, normalization and conversion | Not started |
| R.48 | WP-38 | Precedence and effective chains | Not started |
| R.49 | WP-39 | Canonical form | Not started |
| R.50 | WP-40 | Telemetry runtime | Not started |
| R.51 | WP-42 | Router | Not started |
| R.52 | WP-43 | Per-request exchange | Not started |
| R.53 | WP-44 | Listeners, servers and SO_REUSEPORT | Not started |
| R.54 | WP-45 | Admin server, tap and readiness | Not started |
| R.55 | WP-46 | Upstream runtimes and attempt loop | Not started |
| R.56 | WP-47 | auth.jwt and JWKS, with a security reviewer | Not started |
| R.57 | WP-48 | auth.api-key and auth.basic, with a security reviewer | Not started |
| R.58 | WP-49 | auth.mtls, authz.ip and cors, with a security reviewer | Not started |
| R.59 | WP-50 | auth.upstream-oauth2, with a security reviewer | Not started |
| R.60 | WP-51 | validation.json-schema, with a security reviewer if 3.68 adds the package to the security-sensitive list (E43) | Not started |
| R.61 | WP-52 | Test kit load generation and balancing | Not started |
| R.62 | WP-53 | Examples | Not started |
| R.63 | WP-54 | Release tooling and packaging | Not started |
| R.64 | WP-65 | State Store manager and post-commit queue | Not started |
| R.65 | WP-85 | State Store test servers and container flavors | Not started |
| R.66 | WP-88 | Response Cache revalidation and coalescing | Not started |
| R.67 | WP-41 | Redis State Store driver | Not started |
| R.68 | WP-55 | Loader | Not started |
| R.69 | WP-56 | Semantic validation | Not started |
| R.70 | WP-57 | Diff | Not started |
| R.71 | WP-58 | Render | Not started |
| R.72 | WP-59 | authz.cel and headers, with a security reviewer | Not started |
| R.73 | WP-60 | Transforms | Not started |
| R.74 | WP-61 | ratelimit | Not started |
| R.75 | WP-62 | quota | Not started |
| R.76 | WP-63 | Response Cache Filter | Not started |
| R.77 | WP-64 | Composition | Not started |
| R.78 | WP-66 | Request handler and replay | Not started |
| R.79 | WP-67 | Drain and sd_notify | Not started |
| R.80 | WP-68 | CLI admin client | Not started |
| R.81 | WP-86 | Plain-upstreams forwarder and upstream debug | Not started |
| R.82 | WP-87 | Test kit topology | Not started |
| R.83 | WP-69 | Configuration pipeline facade | Not started |
| R.84 | WP-70 | Snapshot compiler | Not started |
| R.85 | WP-71 | Built-in Filter tables | Not started |
| R.86 | WP-72 | CLI node commands | Not started |
| R.87 | WP-89 | Zero-Downtime handover | Not started |
| R.88 | WP-73 | Activation, Hot Reload and Last-Known-Good | Not started |
| R.89 | WP-74 | CLI bundle commands | Not started |
| R.90 | WP-75 | Configuration conformance and golden corpus | Not started |
| R.91 | WP-76 | ruralzd wiring | Not started |
| R.92 | WP-77 | CLI launcher, dev commands and root | Not started |
| R.93 | WP-78 | Cross-package property suites | Not started |
| R.94 | WP-79 | End-to-end suite | Not started |
| R.95 | WP-80 | Chaos experiments and chaos at scale | Not started |
| R.96 | WP-81 | Benchmark harness, gates and records | Not started |
| R.97 | WP-82 | Protocol conformance | Not started |
| R.98 | WP-90 | Benchmark scenarios, R1 generator and functional variants | Not started |
| R.99 | WP-91 | Node integration: CEL and telemetry | Not started |
| R.100 | WP-92 | Node integration: security | Not started |
| R.101 | WP-93 | Node integration: State Store and round trip | Not started |
| R.102 | WP-94 | T5 systemd verification | Not started |
| R.103 | WP-95 | Compose end-to-end and air-gapped image start | Not started |
| R.104 | WP-99 | Game-day drills and failure rows | Not started |
| R.105 | WP-96 | Stage 12 release workflows and gates | Not started |
| R.106 | WP-97 | RH-1 provisioning, budget runs and soak; Blocked while 9.04 and 9.05 are | Not started |

## C. M1 close-out before the release

The M1 integration, end-to-end tests and documents the user named, and the M1 exit criteria of `docs/roadmap/01-roadmap-and-milestones.md` ("M1 exit criteria" 1 to 7) that the release does not prove by itself, each checked against the code and the records on `develop` once the final review of stage R and its fixes have landed. A gap becomes a follow-up step named after the C step with its next free letter (`C.05b`), with the owning package in its Package column. RH-1 (9.04) runs 9.05, C.08b and C.10 to C.12, on a ref of the kind 9.05 chooses (its runner takes only protected branches and tags). Until RH-1 exists, 9.04's provisioning and those steps are Blocked, the release goes ahead with them listed under "Skipped gates" (WP-98), and X.02 lists exit criteria 2 and 3 and criterion 5's RH-1 part as open, so X.03 stays Blocked: M1 then ships `0.1.0` but does not exit. When RH-1 arrives after 10.02, a failed RH-1 run's follow-up step fixes `develop`, and X.03 then confirms on a patch release that carries the fix.

| Step | Package | Scope | Status |
|---|---|---|---|
| C.01 | Integration inputs | Optional `job` and Latency `slot` inputs for `nightly.yml`, defaulting to every job and the normal rotation so the schedule and release.yml's workflow_call still run everything, with the Makefile variable and the WP-81 harness override that carry the slot (owner choice: an exception to architecture 4.1 rule 3 for those lead commits, recorded as an R-row; the workflow goes to `main` as decision 38 says) | Not started |
| C.01b | Integration runs | `make integration`, and the nightly fuzz and Chaos jobs (`CHAOS_SCALE=nightly`, 10 Nodes) in CI (decision 35), dispatched on this step's branch with `job` set to fuzz and then to chaos, so the Latency job, which needs RH-1 (C.10 to C.12), does not run; WP-81's harness dry run locally. The follow-up step that fixes a failed run closes the nightly-failure issue it opened, and one that fixes a fuzz crasher commits its seed (11 req 27) and closes the fuzz-crasher issue. If you declined C.01's inputs, this step dispatches the whole nightly workflow, on this step's branch while RH-1 is absent, then closing the issue that the Latency job's failed RH-1 check opens with a pointer to C.10 to C.12, and on a ref of the kind 9.05 chose, at the commit under test, once 9.04 has provisioned RH-1, whose runner takes no job of another ref; a follow-up step per failure class | Not started |
| C.02 | End-to-end | The end-to-end suite (WP-79, in `main.yml`: `gh workflow run main.yml --ref develop`), `t5-systemd.yml` (WP-94) and `image-checks.yml` (WP-95) on `develop`'s tip, the last through the workflow_dispatch trigger of 8.19 or, if you declined that trigger, in stage 12 on `v0.1.0-rc.1` (10.02), confirmed at X.02; a follow-up step per failure class | Not started |
| C.03 | Documents: configuration and data plane | The documents' part of the final review (decision 40): a fresh review, against the built code, of the configuration model, data plane and system overview documents, for drift the steps' own document updates missed | Not started |
| C.03b | Documents: security and traffic | The same review of the security and identity and the traffic management documents | Not started |
| C.03c | Documents: observability and scalability | The same review of the observability and the scalability documents | Not started |
| C.03d | Documents: performance and libraries | The same review of the performance budgets document and the tech stack document | Not started |
| C.03e | Documents: engineering | The same review of the repository layout, testing and release documents | Not started |
| C.03f | Documents: operations | The same review of the operations documents | Not started |
| C.03g | Documents: reference and indexes | The same review of the CLI reference, the foundation pack, the glossary, the Feature Catalog, `README.md`, `docs/README.md` and `docs/adr/README.md`, and a check of AGENTS.md, CLAUDE.md and `.claude/rules/`, whose "What exists today" and "Not built yet" lists each package's last step keeps current | Not started |
| C.03h | Documents: other M1 text | The same review of the roadmap, the vision document, the multi-protocol document, the M1 rows of the control plane and AI gateway documents, and any other document with M1 text; a follow-up step per document with drift | Not started |
| C.03i | Documents: ADRs | The same review of every ADR with Planned (M1) text; drift in an accepted ADR becomes an Owner step, since only a superseding ADR you start can change it | Not started |
| C.04 | Exit 5: golden corpus | The golden corpus byte-exact, and configuration conformance at 100% of cases for shipped features (target), with identical digests on every platform: WP-75, and the WP-33 and WP-35 suites (YAML Test Suite, JSON Schema Test Suite) that specs/00 section 7 maps to this criterion | Not started |
| C.05 | Exit 5: protocol conformance | Protocol conformance at 100% of cases for shipped features (target) (WP-82) | Not started |
| C.06 | Exit 5: fuzzing | No open fuzz-crasher issue, and a seed per fixed crasher | Not started |
| C.07 | Exit 5: commands | An end-to-end test for every M1 command in foundation pack section 9 | Not started |
| C.08 | Exit 5: chaos and game days | CE-1 to CE-6, CE-12, CE-15 and CE-16 at 10 Nodes in the nightly Chaos job, and GD-1 to GD-5, dispatched on `develop` with C.01's `job` input set to chaos (or, if you declined the inputs, the whole workflow: on `develop` while RH-1 is absent, then closing the issue that the Latency job's failed RH-1 check opens with a pointer to C.10 to C.12, and on a ref of the kind 9.05 chose, at the commit under test, once 9.04 has provisioned RH-1, with any Latency failure handed to C.10 to C.12), and recorded | Not started |
| C.08b | Exit 5: chaos at target scale | The RH-1 runs of `chaos-scale.yml` on a ref of the kind 9.05 chose, at the commit under test: CE-5 with at least 100 Nodes, CE-12 at 1,000,000 calls per second and CE-15 at 100,000 new keys per second (target), passed and recorded | Not started |
| C.09 | Exit 4: quickstart | The clean-runner quickstart of `main.yml` (WP-27), which specs/00 section 7 maps to this criterion, run on `develop` with `gh workflow run main.yml --ref develop`: a first proxied request in 10 minutes or less (target) (SM-7) | Not started |
| C.10 | Exit 3: throughput | S1 at 50,000 rps or more on 4 vCPU (target) (PB-4), from the S1 ladder in slot 4 of the Latency rotation (11 req 75), dispatched with C.01's `slot` input (or, if you declined it, the whole workflow on a night the rotation picks slot 4) on RH-1 on a ref of the kind 9.05 chose | Not started |
| C.11 | Exit 3: Hot Reload | A Hot Reload of 5,000 Routes in 500 ms or less (target) (PB-7), from the R1 runs in slot 4 of the Latency rotation, dispatched with C.01's `slot` input (or, if you declined it, the whole workflow on a night the rotation picks slot 4) on RH-1 on a ref of the kind 9.05 chose | Not started |
| C.12 | Exit 2: latency | Gateway-added latency on RH-1 in S2 at half saturation, from the nightly S2 A/B and the S2 ladder of slot 4, dispatched with C.01's `slot` input (or, if you declined it, the whole workflow on a night the rotation picks slot 4) on a ref of the kind 9.05 chose: 1 ms or less at p99 and 150 µs or less at p50 (target) (SM-4, SM-5) | Not started |
| C.13 | Exit 6: catalog coverage | 100% of Feature Catalog rows (target) have a milestone or a reasoned `Not planned` (SM-2) | Not started |
| C.14 | Exit 6: M1 rows, transformation | 100% of the Planned (M1) rows (target) of the Feature Catalog sections "Request and response transformation" and "Services connectivity" are implemented (SM-3); a gap becomes a follow-up step of its package | Not started |
| C.14b | Exit 6: M1 rows, traffic | The same for "Traffic management" and "API governance and monetization" | Not started |
| C.14c | Exit 6: M1 rows, tools and routing | The same for "CI/CD, GitOps and development tools" and "Routing" | Not started |
| C.14d | Exit 6: M1 rows, security | The same for "Security" and "Authentication and authorization" | Not started |
| C.14e | Exit 6: M1 rows, observability | The same for "Observability" | Not started |
| C.15 | Exit 7: open questions | Every Open question that blocks an M1 feature is closed. On 2026-10-24 the open ones are OQ-traffic-management-and-resilience-2, -5 (its M1 part), -6, -11, -16, -19, -20 and -21, OQ-observability-2, and OQ-scalability-and-distributed-state-2, -3 (its M1 part), -10 and -11, all in WP-31's scope (3.70 to 3.72) | Not started |

## 10. Wave 10

| Step | Package | Scope | Status |
|---|---|---|---|
| 10.01 | WP-98 | `scripts/repo-settings.sh` and the release runbook section, with the protected `v*` tag rule that the rh-1 runner's protected-ref policy (WP-97) requires for release.yml's RH-1 jobs on the candidate and for X.03, whether or not RH-1 exists yet. Owner choice before it lands: the branch protection it applies to `develop` (required checks and the merge queue) stops direct pushes, so every step after 10.01 needs another way to land, such as pull requests through the merge queue, and so does the decision-38 pull request into `main`, whose copy of `pr-full.yml` has no pull_request or merge_group trigger. Its review, the last part of the final review, comes in this step, since WP-98 is built after stage R (decision 40) | Not started |
| 10.02 | WP-98 release | Owner choice first: whether `develop` merges into `main` before the tags, since the release document has releases cut from `main`; such a merge resolves the add/add and content conflicts in the workflow files 0.03 changed on `main` to `develop`'s versions, which restores their triggers. Then, with 10.01's protected `v*` tag rule in place, you tag `v0.1.0-rc.1` at least 2 weeks before `v0.1.0` (11 req 90), `release.yml` is fixed until green, then you tag `v0.1.0` and SM-12 is confirmed. The RH-1 gates that did not run (the latency gate, the soak, chaos at target scale) are listed under "Skipped gates" with their approver; package check | Owner |

## X. M1 exit

| Step | Package | Scope | Status |
|---|---|---|---|
| X.01 | Exit 1: release | Release `0.1.0` ships `ruralzd` and `ruralz` for every production platform, with 100% of artifacts signed (target) (SM-12), checked with release-verify, with `v0.1.0-rc.1` tagged at least 2 weeks before it (specs/00 section 7) | Not started |
| X.02 | M1 evidence | The evidence of exit criteria 1 to 7 (end-to-end scenario 1, which specs/00 section 7 maps to criterion 4, C.04 to C.15 and X.01), with C.02's image checks as the roadmap's Quality evidence, in one table, each item confirmed again on the `v0.1.0` tag where a later step (C.14's follow-ups, 10.02's fixes) could have changed it, with every open criterion listed | Not started |
| X.03 | M1 exit | Once 9.04, 9.05, C.08b and C.10 to C.12 are done, X.02's table is confirmed again, with the RH-1 runs on the `v0.1.0` tag, which 10.01's tag rule protects; then you declare M1 exited. If a follow-up step changed code after 10.02, those runs use a `v0.1.x` patch release instead, which you cut through `release.yml` first, as an Owner follow-up step, by the release document's rules for a patch release. Blocked while those steps are; `0.1.0` ships at 10.02 either way | Owner |

## After M1

M2 starts after X.03. It gets its own steps then; nothing of M2 is in this plan.

## Step notes

Newest first. Each step adds a short note when it lands: its commits, its run IDs, its agent usage and anything the next steps need.

- 0.01b (2026-10-24):
  - The plan covers the whole of M1 (user decision 36): the work done before it, the close-out stage C and the exit stage X, 31 large steps split into smaller ones, and the S8 backlog. The backlog inventory ran as one background agent (resumed once after an API timeout); its 71 entries are in `runs/s8-backlog.json`, and review moved E21, E22, E25, E39, E42, E53, E57 and E68 to C.
  - Review rounds, each by a fresh reviewer agent on this step's change only (decision 34 keeps the rest out of scope), all fixed in this step: round 1, 6 major and 14 minor; round 2, 1 and 11 (E25, E53 and E57 moved to C); round 3, 3 and 9; round 4, 2 and 11 (E42 moved to C, E65 needs a decision); round 5, 2 and 9 (E60 needs a decision; decision 37 brought step 3.00 and the package steps that settle backlog code); round 6, 4 and 7 (decision 38 and step 0.03; E21 and E43 asked at 3.00); round 7, 3 and 10 (`settledBy` added to the backlog file); round 8, 2 and 8 (E68 moved to C and asked at 3.00; 3.13 split); round 9, 2 and 8 (E41 and E48 asked at 3.00; X.03 added); round 10, 2 and 11 (decision-38 branches in items 1, 6 and 7; the Latency slot input); round 11, 1 and 10 (E30 also settled in 3.16); round 12, 3 and 9 (the user confirmed in decision 37 that records of unchanged code may wait; `develop` moved to `5db08d8` with pull request #2, onto which this branch was fast-forwarded); round 13, 1 and 7 (E62 asked at 3.00; the cleartext hop moved to 3.34b); round 14, 1 and 5 (the `v*` tag rule moved to 10.01; C.01, WP-64, WP-89 and the document reviews split); round 15, no blocker or major and 8 minor (E39 moved to C; Dependabot's branches); round 16, 1 and 5 (`release.yml` goes to `main` dispatch-only at 9.03); round 17, no blocker or major and 5 minor (a Blocked step's patch replaces the parked one; E21's `settledBy` names code steps only); round 18, no blocker or major and 6 minor (WP-64's modes share 05 reqs 46 and 49; nightly runs for WP-80 and WP-99); round 19, 1 and 3 (9.03's dry run became an owner choice; E21's and E26's halves in two packages land together). The user asked for rounds until one reports zero, then stopped the loop after round 19 (decision 39); round 19's fixes were not reviewed again.
  - Decision 40, on 2026-10-25 after round 19: no workflow runs (agents are fine, as the user then clarified) and no review per step. Steps land when their tests and checks pass, and one review of all M1 work comes at the end: stage R, moved after wave 9 and widened by R.43 to R.106 to every package of waves 3 to 9, and C.03 to C.03i for the documents. 0.02 became `make tools`, tests decide decision 8's stop rule, every package's last step ends with a package check, and E43 moved to 3.68. This change was not reviewed.
  - No workflow ran: one inventory agent (about 342,000 subagent tokens, resumed once after an API timeout) and one fresh reviewer agent per round (about 307,000 to 361,000 subagent tokens each).
  - Checks for this plan-only step: the plan checks, `git diff --cached --check`, repocheck and the title check before the commit; the DCO and signature checks on the commit before the merge.

- 0.01 (2026-10-24):
  - The plan adopted, with user decisions 27 to 35 and the AGENTS.md sentence for decision 30. Review rounds so far, each by a fresh reviewer agent: round 1 reported 4 major and 18 minor findings, round 2 reported 3 major and 11 minor, round 3 reported 7 major and 5 minor, round 4 reported 4 major and 10 minor, and round 5 reported 3 major and 9 minor, all fixed in this step. By decision 34 the step lands after round 5's fixes, without a sixth round. The work ran in this session, with no workflow.
  - The S2 work from `wip/s2-handoff` (tip `266e9a8`) is parked in `wip/`. `WP-35.patch`, `WP-37.patch`, `WP-38.patch`, `WP-39.patch` and `WP-51.patch` are each `git diff --binary --no-renames 76edfe0 266e9a8` over that package's owned paths. WP-36's is split into `WP-36-overlay.patch` and `WP-36-subst.patch`, each of which builds and passes its tests alone. Together they rebuild the handoff tree's package paths exactly, and every intermediate state of the steps that apply them passes `go vet` and the `CGO_ENABLED=0` tests (checked by round 2's reviewer in scratch copies).
  - `WP-51-attempt3-reference.patch` is `451a729..266e9a8` over WP-51's path: the G04 attempt 3 edits made on the Windows machine after the first stop. The attempt's earlier partial edits are inside `451a729`, and so inside `WP-51.patch`. `451a729` becomes unreachable when the branch is deleted.
  - The branch's plan files are part of this step. Its run journals and scratch archive stay off `develop`: on this machine under `runs/journals/` and `runs/scratch/` (excluded in `.git/info/exclude`), and on the Windows checkout. A temporary safety bundle of the branch was kept in the session scratchpad until step 0.01b removed it, since the branch itself stays on origin (decision 33).
  - Checks for this plan-only step: the plan checks, `git diff --cached --check`, repocheck and the title check before the commit; the DCO and signature checks on the commit before the merge. The Go whole-tree checks have nothing to check (no Go change).
  - `wip/s2-handoff` stays on origin until step 3.20 (decision 33); this step deletes only its own branch.
