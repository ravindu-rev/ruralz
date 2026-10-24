# M1 implementation plan and hand-off state

This directory holds the working plan the M1 Core gateway implementation follows. It is not a design document: `docs/` is the specification, and every requirement here cites it. From 2026-10-24 the work runs in small steps: `steps.md` is the plan of record, with the step procedure and the progress. From 2026-10-25 the steps run without the Workflow-tool run scripts listed below (decision 40), though they may use agents; the scripts stay as the record of waves 2 and 3.

## Contents

| Path | What it is |
|---|---|
| `steps.md` | The plan of record from 2026-10-24: every M1 step in order, to the milestone's exit, how a step runs (its own branch, tests and checks, signed commits, merge into `develop`, branch cleanup), the final review at the end, and the status |
| `wip/*.patch` | S2 work in progress parked by step 0.01: one patch per package against `76edfe0` (WP-36's split into overlay and substitution), which the package's first step applies and deletes, and `WP-51-attempt3-reference.patch`, the G04 attempt 3 edits made after `451a729`, for reading only; later also the `wip/<id>.patch` of a Blocked step that had started, which replaces the parked patch that step applied |
| `tools/check-steps.py` | The plan checks every step runs: step rows, counts and Done counts against the status table, the next step, branch names, package coverage, the split finding lists, the parked patches, the WP-51 classes, the S8 backlog steps and text hygiene |
| `specs/00-architecture.md` | The binding integration architecture: section 0 conventions, 1.2 package table with allowed imports, 1.3 lint rules, 2 shared contracts (the committed WP-01 Go code wins where it differs), 2.16 resolved conflicts R-1 to R-78, 3 data flow, 4.1 work package rules, 4.4 every work package's Owns, Depends on, Scope and Done when, and the exit criteria map |
| `specs/01-*.md` to `specs/11-*.md` | Area specs with numbered requirements; "04 req 34" means spec 04, requirement 34 |
| `arch/wps.json` | The 99 work packages: `id`, `title`, `wave`, `dirs` (the paths it owns), `dependsOn`, `specs`, `scope`, `size` |
| `user-decisions.md` | The user's decisions, from the wave-2 boundary on |
| `crs-all.json`, `triage.json` | The 171 wave-2 contract change requests and the lead's decision for each (`action`: contract, arch, forward, docfix, repometa, user, done, reject) |
| `forwards.json` | Notes for later work packages, keyed by WP id, which the step that builds the package reads |
| `m1-section.js` | Package script: implement each package, then the four-lens review, reproduction and fix loop per package; continues stopped packages from files (`initialFixFile`, `initialFixIds`, `reported`) |
| `m1-wp51-close.js` | WP-51 close loop: one engineer per group of the audit's gap classes in order, an independent check per attempt, then nine-lens review rounds until one reports zero findings. Continues a stopped run from a carry file (`carryFile`, `startGroup`, `startAttempt`, `carriedCount`) |
| `m1-resume.js` | Wave workflow used in waves 2 and 3: per work package implement, adversarial review, fix and re-review. Args: `ids`, `extra`; `reported`, `titles` and `reviews` resume packages whose engineer report or review is already saved |
| `m1-harden.js` | Hardening loop for one package that keeps failing review (written for WP-33 in S1): finish the last fix, then four review lenses, merge, independent reproduction of each blocker or major finding, fix, and repeat |
| `m1-yaml-decision.js` | Research for the YAML parser decision: history of the WP-33 findings, a go-yaml v4 prototype, hand-written parser feasibility, upstream state and repository changes, then a brief at `yaml-parser-decision.md`, two critiques and a revision |
| `yaml-parser-decision.md`, `yaml-decision-evidence/` | The YAML parser decision brief and its five researchers' evidence files |
| `m1-wp51-fix.js`, `m1-wp51-audit.js`, `wp51-library-audit.md` | The earlier WP-51 fix loop, the library audit script and the audit (321 rows; its 180 partial and gap rows form 21 gap classes) that steps 3.01 to 3.12 close |
| `m1-apply.js` | Applies triaged decisions per unit (owned paths, then review and fix) |
| `m1-gov.js` | Documentation and ADR units with dependencies (`after`) |
| `wave3-plan.md` | How wave 3 ran until 2026-10-24: nine sections, the per-section procedure, the progress table and the notes. Superseded by `steps.md` |
| `runs/` | Run arguments, results and continuation files: `w3-s2-wp51-carry2.json` (WP-51 at the 2026-10-17 pause) and `w3-s2-minors-continue3.json` (the open findings of WP-36 to WP-39) feed steps 3.01 to 3.20, and `s8-backlog.json` (the backlog deferred to S8, inventoried by step 0.01b) feeds its decisions at 3.00 and 3.68, the package steps that settle its code (decision 37) and the record steps 3.69 to 3.69k |
| `reports/` | Engineer reports a continued run reads, such as `WP-35.json` |
| `tools/usage.py`, `tools/wordcount.py` | Agent token usage per workflow run, and the manifest word count of a document |
| `tools/wf-status.js` | Live status of workflow runs for IDE sessions, where `/workflows` is unavailable: `node tools/wf-status.js --watch <run dir>...` |
| `tools/wf-watch.js` | Journal watcher for a Monitor: reports each agent start and result, and exits when an agent matching a label pattern starts, to stop a run at a clean point |
| `tools/backup-tick.sh` | Dated backups every 10 minutes of WP-51 and the S2 configuration packages, run as a Monitor |
| `tools/build-minors-continue3.js`, `tools/build-wp51-carry2.js` | How the continuation files of the 2026-10-17 pause were built from the run journals and transcripts; templates for the next stop |
| `runs/journals/`, `runs/scratch/` | Run journals of the S2 runs and a stopped attempt's scratch archive: records kept only on the machines that have them (this Linux checkout, excluded in `.git/info/exclude`, and the Windows checkout) and on `origin/wip/s2-handoff`, never committed to `develop`; delete them when S2 closes (step 3.20) |

## State on 2026-10-25

- `develop` holds waves 1 and 2, complete: WP-01 to WP-30, WP-32, WP-83 and WP-84, with the wave-2 boundary decisions and the wave-2 completion of 2026-10-09.
- From wave 3 it holds WP-33, WP-35 to WP-39 and WP-51, each with open findings, so sections S1 and S2 are still open (`wave3-plan.md` records them up to this date). WP-33 waits for its C2 rebuild (steps 3.21 to 3.30), and S2's open work continues as steps 3.01 to 3.12 (with 3.01b and 3.11b) and 3.16 to 3.20, after the backlog decisions of step 3.00, with S1's WP-35 and WP-36 as steps 3.13 to 3.15 (with 3.13b).
- On 2026-10-09 every commit on `develop` after `main` was rewritten so the user is author and committer, each is signed with the user's SSH key, and none carries a `Co-Authored-By` trailer; commit hashes before that date no longer exist.
- On 2026-10-24 the user had the plan split into small steps, each done on its own branch, merged into `develop` when done, with every other branch then cleared, and commits signed with `~/.ssh/id_ed25519` (`user-decisions.md` items 27 to 40). Step 0.01 adopted that plan and parked the S2 work of `wip/s2-handoff` in `wip/`; the branch stays on origin until step 3.20 (decision 33). Step 0.01b extended the plan to M1's exit and, on 2026-10-25, moved it to steps built with their tests, without workflow runs, and one review of all M1 work at the end (decision 40). Waves 4 to 10 are not started. `steps.md` "Status" names the next step.

## Wave-2 documentation and completion

Documentation the user approved at the wave-2 boundary (2026-10-03):

- `5951fe5` the CI tooling research addendum (section 11 of `docs/_meta/research/tooling-and-licenses.md`).
- `a824944` ADR-0018 superseding ADR-0010 and ADR-0019 superseding ADR-0011, with their indexes, manifest entries and foundation pack rows; OQ-observability-16 (a) and OQ-configuration-model-21 (a) closed; the foundation pack Upgrades row adopts OQ-zero-downtime-upgrades-and-hot-reload-12 (a).
- `fc9087e` WP-32: the engineering, performance, release, feature and vision documents, and the roadmap M1 note; OQ-testing-and-quality-strategy-11 (b) closed (chaos at scale on RH-1).
- `8cc7f32` the depguard messages and Go comments cite ADR-0018 and ADR-0019.

Wave-2 completion (2026-10-09): the 91 minor findings the wave-2 reviews left open and five lead items were triaged; 68 were fixed and 28 skipped, each skip because it was already fixed, a documented deviation, owned by a later work package (its note is in `forwards.json` or its scope), or not worth changing. The user also decided OQ-data-plane-18 (b): admin 404 is `RZ-RT-020` and admin 405 is `RZ-RT-021`; and approved the `docs/_meta` corrections listed in `user-decisions.md` item 6.

Small items left for a later pass (none blocks wave 3):

- OQ-observability-2 in `docs/architecture/10-observability.md` still reads blocking although the configuration model adopted `telemetry.otlp.tls`: WP-31 closes it (step 3.71).
- ADR-0018 "More information" links the layout document's `#import-boundaries` anchor; the OpenTelemetry confinement row sits under `#banned-imports`. The ADR is accepted, so the link stays until a superseding ADR.
- ADR-0019 superseded ADR-0011 without two of its More information items, and no other document holds them: (1) the proposed amendment that the repository layout document ban the root `opa/rego` import and confine OPA and cedar-go to one `internal/` wrapper package (a depguard rule in `.golangci.yml` with a row in `docs/engineering/02-repository-layout-and-conventions.md`); (2) the research action to extend `docs/_meta/research/go-libraries-runtime.md` section 9.2 on OPA prepared-query transactions and topdown deadline handling. Both are due before `authz.opa` and `authz.cedar` work (Planned (M2)); the accepted ADR bodies stay unchanged.
- `docs/architecture/08-security-and-identity.md` Cache partitioning row says sharing across principals needs a field (OQ-security-and-identity-25) and also that `config.key` replaces the principal; closing OQ-security-and-identity-25 in line with `docs/architecture/09-traffic-management-and-resilience.md` (spec 05 section 9 item 1) removes the tension. It blocks nothing in M1.

## How a step runs

`steps.md` "How a step runs" is the procedure, and this section does not restate it. The details it leaves to this README:

1. Contract requests are triaged with the action classes of `triage.json`. Decisions are recorded in `specs/00-architecture.md` 2.16 and `arch/wps.json`, and notes for later packages in `forwards.json`.
2. Signing in a Claude Code cloud container: the container has no `~/.ssh/id_ed25519`, and its global git config (`/root/.gitconfig`) sets `commit.gpgsign`, `gpg.ssh.program=/tmp/code-sign` and its own key, so a plain `git commit -S` there is signed with the environment's key. Ask the user how to sign before committing in a container.
3. Agents sometimes build a binary into the repository root (`go build ./internal/tool/x` without `-o`); delete such stray files before committing.
4. While a step's work is in progress, back up the package under work with a Monitor running `timeout 1790 bash .claude/plans/m1/tools/backup-tick.sh <scratchpad>`, and re-arm it when it ends. The script names backups with the date; add the package's paths to it when the step works outside the S2 packages.

## Resuming in a new session

1. Read this README, `steps.md` ("How a step runs", "Status", the step notes), all of `user-decisions.md` and the AGENTS.md "Quality standard".
2. `git status --porcelain` must be empty (`steps.md` item 1). Run `git fetch origin --prune`. Besides paused steps, only `main`, `develop`, until step 3.20 `wip/s2-handoff`, every open decision-38 `main-workflows/<id>` branch and Dependabot's `dependabot/*` branches should exist on origin (`steps.md` items 1 and 6). A `step/<id>` branch on origin is a paused step: check it out and read its step notes there. A local-only `step/<id>` branch is this machine's unfinished step, unless develop's `steps.md` marks it Done.
3. Check the tools:
   - golangci-lint v2.13.2 in `bin/` (`make tools` on Linux and macOS; Windows builds are installed by hand). Run it as `GOTOOLCHAIN=go1.27.1 bin/golangci-lint ...`: under go1.27.2 it reports "export data version 5" typechecking errors, still prints "0 issues." and exits 7.
   - `go mod download`: the WP-51 steps read the jsonschema/v6 v6.0.3 source under `$(go env GOMODCACHE)`.
   - `GOTOOLCHAIN=go1.26.8 go version` for the floor check.
   - For the integration suites and `make integration`: a local `redis-server` 7.0.15 (architecture 4.1 rule 6) and `promtool`, which `make tools` installs into `bin/` (step 0.02). This Linux machine has no `redis-server` and no Docker, so those suites run in CI on the step branch (decision 35); installing them here is the user's choice.
   - Signing as in "Working rules from the user"; after the first commit, `git log --format='%G? %GK' -1` shows `G` and this machine's key (on this Linux machine `SHA256:6fQ5We1xTxIoII4Zh8tTpYJUbvP8sYixkcXvpjJ6nl8`).
   - The GitHub CLI `gh`, logged in, for the CI runs on step branches (decision 35). This Linux machine lacks it; the user installs it before the first step that needs CI.
4. A checkout that still holds the S2 work uncommitted, as the Windows machine did on 2026-10-17, has it in `wip/` now: set it aside with `git stash push --include-untracked` before pulling `develop`. On this Linux checkout, `stash@{0}` holds the same package work as `wip/`, kept as a second copy until S2 closes.
5. Evidence paths in findings that point into an old session's scratchpad (`/tmp/claude-0/...`, or `.../Temp/claude/.../scratchpad` on Windows) may be gone. Steps reproduce from the input each finding states.

S2 at the 2026-10-17 pause, which steps 3.01 to 3.20 continue:

- **WP-51** (`m1-wp51-close.js`, runs `wf_8b2d7117-514` then `wf_721727d8-351`, decisions in `user-decisions.md` items 20 to 26):
  - Groups G01 to G03 are finished. G02 and G03 ended with 5 minors open, which step 3.01b fixes.
  - G04 attempts 1 and 2 are fixed and checked; the check of attempt 2 left 2 majors open.
  - G04 attempt 3 was stopped twice: mid-edit in the container, then after about 4 hours of the Windows run, while its engineer ran final stress and load checks, before its report and its independent check. Its edits are inside `wip/WP-51.patch`. Those made after the first stop are also alone in `wip/WP-51-attempt3-reference.patch`, since `451a729`, which holds the first stop's partial edits, is unreachable once the branch is deleted: 9 files, +2,005 and -547 lines against `451a729` (the heap meter in `meter_test.go` rewritten, plus `doc.go`, `fit_test.go`, `work_limit_test.go`, `working.go` and `working_test.go`). Its mutation battery killed all 40 mutants.
  - G05 to G11 and the review rounds have not run.
  - `runs/w3-s2-wp51-carry2.json` holds what step 3.01 needs: the history text, the carried and open items, every attempt's fix and check report, 11 contract requests, the changed paths, and `attempt3Progress` (the second stop, the changed files, the engineer's progress notes and the scratch archive), which 3.01 reads first.
  - `runs/scratch/wp51-g04a3-scratch.tar.gz` holds the stopped attempt's scratch directory without binaries: probes, pre-fix copies, the mutation batteries with their results, and the stress and load scripts. The agent transcripts of runs `wf_721727d8-351` and `wf_7860476f-71c` stay on the Windows machine, under `~/.claude/projects/c--Users-ravin-Projects-Ruralz/5104a226-9733-4230-8cf6-a5ff30e79ceb/subagents/workflows/`.
- **WP-35 to WP-39** (`m1-section.js`, runs `wf_c0fb6de8-9cc`, `wf_2dcc9b57-933`, then `wf_7860476f-71c`): the lead stopped the last run at 17:23 UTC while no fix agent ran, so no package holds partial edits from it.
  - `runs/w3-s2-minors-continue3.json` holds the open findings: WP-36 11 (4 majors not reproduced yet), WP-37 18, WP-38 9 (1 major not reproduced yet; its adversarial lens did not report), WP-39 19 (5 majors verified). Its `_meta.contractRequests` holds the 25 contract requests this run's fixers filed (WP-35 12, WP-36 7, WP-38 6).
  - WP-35 has no open findings: fix0 closed its 11, with that fix report in `reports/WP-35.json`; its review comes in the final review (decision 40).
  - WP-37's fix0 died on an API timeout ("No response from API") before it changed a file; the run then skipped WP-37's reviews without a journal entry.

## Workflow runs

Decision 40 (2026-10-25) stops the Workflow-tool runs. Agents are fine: a step may give parts to subagents, several at once where the parts are independent, such as a final-review step's review. The run scripts, journals and continuation files stay as the record of waves 2 and 3. If a workflow is ever launched again, CLAUDE.md's rules apply: launch it only in the turn of a typed user message that names it, since an `AskUserQuestion` answer does not reach its agents, and never put a secret in that message.

## Working rules from the user

The user's decisions are in `user-decisions.md`, and `steps.md` holds the procedure that carries them out. What this README adds:

- Until 10.01's owner choice sets another route, a done step lands on `develop` by fast-forward, with no pull request (decisions 27 and 28, and the lead defaults), and the only pull requests the steps open are decision 38's into `main`, from step 0.03 and each later step that adds or changes a workflow with a dispatch or call trigger. Steps never merge or close a pull request they did not open, such as Dependabot's: those are yours.
- The author comes from the repository git config: Ravindu Wijegunawardhana, `ravindudenuwan.dev@gmail.com`. No `Co-Authored-By` trailer.
- Signing (decision 29): each machine's default ED25519 key, `~/.ssh/id_ed25519` (`%USERPROFILE%\.ssh\id_ed25519` on Windows), set in that checkout's repository-local config: `gpg.format ssh`, `user.signingkey` (here `/home/ravindu/.ssh/id_ed25519`), `commit.gpgsign true`, and `gpg.ssh.allowedSignersFile` pointing at `.git/allowed_signers`. On this Linux machine the key is `SHA256:6fQ5We1xTxIoII4Zh8tTpYJUbvP8sYixkcXvpjJ6nl8`. The Windows checkout's config still points at the OneDrive key it used before; point it at that machine's default key before committing there.
- `.git/allowed_signers` belongs to one checkout, so every checkout's file lists the key of every machine that commits (this checkout's file does not list the Windows machine's default key yet: add it, from that machine's `id_ed25519.pub`, before a Windows commit lands), plus the two keys that signed `develop` since the 2026-10-09 rewrite: the RSA key `SHA256:LIT3iCVGg5U16s1+r/ew5sxXydYhQ7y2g6VONvpKVpw` (from 2026-10-16) and the ED25519 key `SHA256:TsJhk+hdD1BB/pTF0GWj1jPlRSUm0MRu+MF94u0Ixpg` (before 2026-10-16, and the Windows commits of 2026-10-17). The commits of `main` were signed with another key (`SHA256:32dP45eSMmVSt/G/CGvcxl/P+MO3Nwj9xeTh/GSA2wc`), which the file does not list, or are unsigned, so they show `U` or `N`; its merge commit `1f5039e` carries GitHub's GPG signature and shows `E`.
- Steps run in `steps.md` order; stop and report after each step.
- No Workflow-tool runs (decision 40); agents are fine. Steps land when their tests and checks pass, and one review of all M1 work comes at the end.
- The plan runs to M1's exit: M1 integration, end-to-end tests and documents are its stage C, before the release (decision 36). M2 follows.
