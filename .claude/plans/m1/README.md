# M1 implementation plan and hand-off state

This directory holds the working plan the M1 Core gateway implementation follows. It is not a design document: `docs/` is the specification, and every requirement here cites it. The scripts assume the repository at `/home/user/ruralz`; if the checkout lives elsewhere, change `REPO` and `M1` at the top of each script.

## Contents

| Path | What it is |
|---|---|
| `specs/00-architecture.md` | The binding integration architecture: section 0 conventions, 1.2 package table with allowed imports, 1.3 lint rules, 2 shared contracts (the committed WP-01 Go code wins where it differs), 2.16 resolved conflicts R-1 to R-76, 3 data flow, 4.1 work package rules, 4.4 every work package's Owns, Depends on, Scope and Done when, and the exit criteria map |
| `specs/01-*.md` to `specs/11-*.md` | Area specs with numbered requirements; "04 req 34" means spec 04, requirement 34 |
| `arch/wps.json` | The 99 work packages: `id`, `title`, `wave`, `dirs` (the paths it owns), `dependsOn`, `specs`, `scope`, `size` |
| `user-decisions.md` | The user's decisions at the wave-2 boundary |
| `crs-all.json`, `triage.json` | The 171 wave-2 contract change requests and the lead's decision for each (`action`: contract, arch, forward, docfix, repometa, user, done, reject) |
| `forwards.json` | Notes for later work packages, keyed by WP id; pass them to the wave script as `extra` |
| `m1-resume.js` | Wave workflow: per work package implement, adversarial review, fix and re-review (up to two rounds). Args: `ids`, `extra`; `reported`, `titles` and `reviews` resume packages whose engineer report or review is already saved. The agent commit stage is off (`args.commit` unset); the lead commits |
| `m1-harden.js` | Hardening loop for one package that keeps failing review (written for WP-33 in S1): finish the last fix, then four review lenses (depth and cost, narrow versus split differential, spec and valid input, tests and code), merge, independent reproduction of each blocker or major finding, fix, and repeat until a round confirms none (at most three rounds) |
| `m1-section.js` | Section script: implement each package, then the four-lens review, reproduction and fix loop of `m1-harden.js` per package (at most three review rounds); heavier and slower than `m1-resume.js` |
| `m1-yaml-decision.js` | Research for the YAML parser decision: history of the WP-33 findings, a go-yaml v4 prototype, hand-written parser feasibility, upstream state and repository changes, then a brief at `yaml-parser-decision.md`, two critiques and a revision |
| `yaml-parser-decision.md`, `yaml-decision-evidence/` | The YAML parser decision brief and its five researchers' evidence files |
| `m1-wp51-fix.js` | Targeted fix loop for WP-51: a fix of verified findings, then four-lens review, reproduction and fix rounds (at most three); `findingsFile` and `startRound` resume it from saved findings |
| `m1-apply.js` | Applies triaged decisions per unit (owned paths, then review and fix) |
| `m1-gov.js` | Documentation and ADR units with dependencies (`after`) |
| `wave3-plan.md` | How wave 3 runs: nine small sections that each fit in one usage window, with the run order, the per-section procedure and a progress table |
| `runs/w3-s<N><a\|b>.json` | Ready arguments for each wave-3 section's two runs (package IDs, their notes from `forwards.json`, `deferMinor`, now false everywhere) |
| `tools/usage.py`, `tools/wordcount.py` | Agent token usage per workflow run, and the manifest word count of a document |
| `m1-wp51-close.js` | WP-51 close loop: one engineer per group of the audit's gap classes in order, an independent check per attempt, then nine-lens review rounds until one reports zero findings. Continues a stopped run from a carry file (`carryFile`, `startGroup`, `startAttempt`, `carriedCount`) |
| `tools/wf-status.js` | Live status of workflow runs for IDE sessions, where `/workflows` is unavailable: `node tools/wf-status.js --watch <run dir>...` |
| `tools/wf-watch.js` | Journal watcher for a Monitor: reports each agent start and result, and exits when an agent matching a label pattern starts, to stop a run at a clean point |
| `tools/backup-tick.sh` | Dated backups of the S2 packages every 10 minutes, run as a Monitor |
| `tools/build-minors-continue3.js`, `tools/build-wp51-carry2.js` | How the continuation files of the 2026-10-17 pause were built from the run journals and transcripts; templates for the next stop |
| `runs/journals/`, `runs/scratch/` | Run journals and a stopped attempt's scratch archive: records only, never committed to `develop`, deleted once S2 is committed |

## State on 2026-10-10

- `develop` holds waves 1 and 2, complete: WP-01 to WP-30, WP-32, WP-83 and WP-84, the two test-race fixes, the wave-2 boundary decisions (R-63 to R-75 in code, architecture and docs), the M1 guide updates in `AGENTS.md`, `CONTRIBUTING.md` and `.claude/rules/`, the documentation below, and the wave-2 completion of 2026-10-09.
- On 2026-10-09 every commit on `develop` after `main` was rewritten so the user is author and committer, each is signed with the user's SSH key, and none carries a `Co-Authored-By` trailer; commit hashes before that date no longer exist.
- Wave 3 is in progress by `wave3-plan.md` (nine sections, each committed and pushed before the next; WP-31 runs in S9). Section S1 is done (2026-10-10): WP-33, WP-35 and WP-36 are committed. WP-33 carries five open findings (`runs/w3-s1a-harden2-result.json`) until the user decides the YAML parser question. The research (`m1-yaml-decision.js`, run `wf_782bab9a-2c7`) finished on 2026-10-10: its brief `yaml-parser-decision.md` recommends C2, a hand-written parser checked in tests against yaml v3; the evidence is in `yaml-decision-evidence/`. The user chose C2, strict YAML 1.2 and ignored reserved directives, and approved the research addendum (`user-decisions.md` items 8 to 11). Still open before the WP-33 rebuild: the spec and plan text (brief section 5.1 step 2), the research addendum, ADR-0020 (the user starts it) and the `go.mod` approval. S2 is recorded in `wave3-plan.md`: WP-37, WP-38 and WP-39 passed; WP-51 is committed after eight review rounds (`5f426d5`); a ninth round's package is saved as `runs/wp51-postfix8.patch` with four blockers open (`runs/w3-s2-wp51-r9.json`); the user decides how S2 closes. Work stops after S1 until the user resumes it; S2's run files are ready (`runs/w3-s2a.json`, `runs/w3-s2b.json`) for `m1-resume.js` or `m1-section.js`. Waves 4 to 10 are not started.

## Wave-2 documentation and completion

Documentation the user approved at the wave-2 boundary (2026-10-03):

- `5951fe5` the CI tooling research addendum (section 11 of `docs/_meta/research/tooling-and-licenses.md`).
- `a824944` ADR-0018 superseding ADR-0010 and ADR-0019 superseding ADR-0011, with their indexes, manifest entries and foundation pack rows; OQ-observability-16 (a) and OQ-configuration-model-21 (a) closed; the foundation pack Upgrades row adopts OQ-zero-downtime-upgrades-and-hot-reload-12 (a).
- `fc9087e` WP-32: the engineering, performance, release, feature and vision documents, and the roadmap M1 note; OQ-testing-and-quality-strategy-11 (b) closed (chaos at scale on RH-1).
- `8cc7f32` the depguard messages and Go comments cite ADR-0018 and ADR-0019.

Wave-2 completion (2026-10-09): the 91 minor findings the wave-2 reviews left open and five lead items were triaged; 68 were fixed and 28 skipped, each skip because it was already fixed, a documented deviation, owned by a later work package (its note is in `forwards.json` or its scope), or not worth changing. The user also decided OQ-data-plane-18 (b): admin 404 is `RZ-RT-020` and admin 405 is `RZ-RT-021`; and approved the `docs/_meta` corrections listed in `user-decisions.md` item 6.

Small items left for a later pass (none blocks wave 3):

- OQ-observability-2 in `docs/architecture/10-observability.md` still reads blocking although the configuration model adopted `telemetry.otlp.tls`: WP-31 closes it.
- ADR-0018 "More information" links the layout document's `#import-boundaries` anchor; the OpenTelemetry confinement row sits under `#banned-imports`. The ADR is accepted, so the link stays until a superseding ADR.
- ADR-0019 superseded ADR-0011 without two of its More information items, and no other document holds them: (1) the proposed amendment that the repository layout document ban the root `opa/rego` import and confine OPA and cedar-go to one `internal/` wrapper package (a depguard rule in `.golangci.yml` with a row in `docs/engineering/02-repository-layout-and-conventions.md`); (2) the research action to extend `docs/_meta/research/go-libraries-runtime.md` section 9.2 on OPA prepared-query transactions and topdown deadline handling. Both are due before `authz.opa` and `authz.cedar` work (Planned (M2)); the accepted ADR bodies stay unchanged.
- `docs/architecture/08-security-and-identity.md` Cache partitioning row says sharing across principals needs a field (OQ-security-and-identity-25) and also that `config.key` replaces the principal; closing OQ-security-and-identity-25 in line with `docs/architecture/09-traffic-management-and-resilience.md` (spec 05 section 9 item 1) removes the tension. It blocks nothing in M1.

## How a wave runs

1. Launch `m1-resume.js` in two concurrent runs of about 12 work packages each (the container has 4 CPUs, and a workflow runs at most CPUs minus 2 agents at once). Put the large (`size: L`) packages first. Build `extra` from `forwards.json`.
2. When a run finishes, read each package's `finalVerdict`. A package passes only when its last review reports no findings of any severity (AGENTS.md "Quality standard"; `user-decisions.md` item 16). For every `pass`, the lead commits only the files inside that package's `dirs` (match `git status --porcelain --untracked-files=all` against `wps.json`), in dependency order, after `go build`, `go vet` and the tests of those packages pass:
   - title validated with `go run ./internal/tool/commitcheck title -title "<title>"` (allowed scopes in `AGENTS.md`);
   - `git commit -S -s -F <msg> --pathspec-from-file=<list>` (signed with the user's SSH signing key) so another package's staged files are never swept in;
   - message body of 2 to 6 lines, then `Refs: WP-xx (M1 architecture)`; no `Co-Authored-By` trailer;
   - verify with `git show --no-renames --name-only --format= HEAD`, then push `develop`.
3. At the wave boundary, run the whole-tree checks: `go build ./...`, `go vet ./...`, `bin/golangci-lint run ./...` and `fmt --diff`, `go run ./internal/tool/repocheck`, `go run ./internal/tool/depgate`, `go mod verify`, `go generate ./...` with no diff, `CGO_ENABLED=1 go test -race -shuffle=on -tags netgo,osusergo ./...`, `CGO_ENABLED=0 go test -shuffle=on ./...` and `make floor FLOOR_GOTOOLCHAIN=go1.26.8`. Fix a failing or flaky test at its root cause, never by skipping it.
4. Collect every package's `contractRequests`, triage them read-only (the same action classes as `triage.json`), ask the user only what is theirs to decide (ADRs, `docs/_meta/`, `nolicensecheck.allow`, runners, repository settings), apply the rest with `m1-apply.js`, record decisions in `specs/00-architecture.md` 2.16 and `arch/wps.json`, and add notes for later packages to `forwards.json`.
5. Signing in a Claude Code cloud container: its global git config (`/root/.gitconfig`) sets `commit.gpgsign`, `gpg.ssh.program=/tmp/code-sign` and its own key, so a plain `git commit -S` is signed with the environment's key, not the user's. Before committing, set repository-local `gpg.format ssh`, `gpg.ssh.program /usr/bin/ssh-keygen` (install `openssh-client` if it is missing) and `user.signingkey` to the user's key, then check that `git log --format='%G? %GK'` shows `G` and the user's key fingerprint.
6. Agents sometimes build a binary into the repository root (`go build ./internal/tool/x` without `-o`); delete such stray files before committing.

## Resuming in a new session

State on 2026-10-17 at 18:41 UTC: S2 is paused at the user's request, and none of its work after `76edfe0` is on `develop`. The work in progress travels as signed WIP commits on `wip/s2-handoff`, the one approved exception to "no other branches": `451a729` from the container session, then `c9eb746` and a commit adding the helper tools and the scratch archive, both from the user's Windows machine. The tip's tree is `76edfe0` plus all S2 work. On the Windows checkout (`C:/Users/ravin/Projects/Ruralz`) the same work is also in the working tree, uncommitted. Resume by run ID works only in the session that ran a run, so each run continues from files.

- **WP-51 fix** (`m1-wp51-close.js`, runs `wf_8b2d7117-514` then `wf_721727d8-351`, decisions in `user-decisions.md` items 20 to 26):
  - Groups G01 to G03 are finished. G02 and G03 ended with 5 minors open, which go to the carried fix before the review rounds.
  - G04 attempts 1 and 2 are fixed and checked; the check of attempt 2 left 2 majors open.
  - G04 attempt 3 was stopped twice: mid-edit in the container, then after about 4 hours of the Windows run, while its engineer ran final stress and load checks, before its report and its independent check. Its edits are in the working tree: 9 files, +2,005 and -547 lines against `451a729` (the heap meter in `meter_test.go` rewritten, plus `doc.go`, `fit_test.go`, `work_limit_test.go`, `working.go` and `working_test.go`). Its mutation battery killed all 40 mutants.
  - G05 to G11 and the review rounds have not run.
  - `runs/w3-s2-wp51-carry2.json` holds everything the run needs: the history text, the carried and open items, every attempt's fix and check report, 11 contract requests, the changed paths, and `attempt3Progress` (the second stop, the changed files, the engineer's progress notes and the scratch archive), which the script tells the continued engineer to read first.
  - `runs/scratch/wp51-g04a3-scratch.tar.gz` holds the stopped attempt's scratch directory without binaries: probes, pre-fix copies, the mutation batteries with their results, and the stress and load scripts. The agent transcripts of runs `wf_721727d8-351` and `wf_7860476f-71c` stay on the Windows machine, under `~/.claude/projects/c--Users-ravin-Projects-Ruralz/5104a226-9733-4230-8cf6-a5ff30e79ceb/subagents/workflows/`.
- **WP-35 to WP-39 minors** (`m1-section.js`, runs `wf_c0fb6de8-9cc`, `wf_2dcc9b57-933`, then `wf_7860476f-71c`): the lead stopped the last run at 17:23 UTC while no fix agent ran, so no package holds partial edits from it.
  - `runs/w3-s2-minors-continue3.json` holds the open findings: WP-36 11 (4 majors not reproduced yet), WP-37 18, WP-38 9 (1 major not reproduced yet; its adversarial lens did not report), WP-39 19 (5 majors verified). Its `_meta.contractRequests` holds the 25 contract requests this run's fixers filed (WP-35 12, WP-36 7, WP-38 6).
  - WP-35 has no open findings: fix0 closed its 11, and its next step is a fresh review, with that fix report in `reports/WP-35.json`.
  - WP-37's fix0 died on an API timeout ("No response from API") before it changed a file; the run then skipped WP-37's reviews without a journal entry.
- The journals of the five runs are in `runs/journals/`, and the scratch archive is in `runs/scratch/`. They are a record only: never commit them to `develop`, and delete them once S2 is committed.
- Evidence paths in findings that point into an old session's scratchpad (`/tmp/claude-0/...`, or `.../Temp/claude/.../scratchpad` on Windows) may be gone. Agents reproduce from the input each finding states.

Steps:

1. Read this README, `wave3-plan.md` (S2 notes), `user-decisions.md` and the AGENTS.md "Quality standard".
2. Restore the work on `develop` as uncommitted changes, unless the checkout already holds it. The WIP commits only transport it.
   ```bash
   git fetch origin develop wip/s2-handoff
   git checkout -B develop origin/develop
   git diff --binary 76edfe0 origin/wip/s2-handoff | git apply
   ```
   `git status` must then list the same paths as `git diff --name-only 76edfe0 origin/wip/s2-handoff`. If `develop` has moved past `76edfe0`, the diff applies on top of it; resolve any conflict before a run starts.
3. Check the tools:
   - golangci-lint v2.13.2 in `bin/` (`make tools` on Linux and macOS; Windows builds are installed by hand). Run it as `GOTOOLCHAIN=go1.27.1 bin/golangci-lint ...`: under go1.27.2 it reports "export data version 5" typechecking errors, still prints "0 issues." and exits 7.
   - `go mod download`: the agents read the jsonschema/v6 v6.0.3 source under the module cache, so `LIB` in `m1-wp51-close.js` must match `$(go env GOMODCACHE)`.
   - `GOTOOLCHAIN=go1.26.8 go version` for the floor check.
   - Both scripts default to the Windows checkout and describe Git Bash on Windows. On another machine, add the machine args to step 5's args and use that checkout's paths there: `"repo"` (the checkout), `"windows": false`, and for WP-51 `"lib"` (the jsonschema/v6 v6.0.3 directory under `go env GOMODCACHE`). In the container that was `"repo": "/home/user/ruralz", "lib": "/root/go/pkg/mod/github.com/santhosh-tekuri/jsonschema/v6@v6.0.3", "windows": false`.
   - Signing (Working rules): repository-local `gpg.format ssh`, `user.signingkey` set to the user's key file and `gpg.ssh.allowedSignersFile` listing both public keys. The Windows checkout already has them, with the key in `C:/Users/ravin/OneDrive/Documents/Keys`.
4. While runs are going, back up the S2 packages with a Monitor running `timeout 1790 bash .claude/plans/m1/tools/backup-tick.sh <scratchpad>`, and re-arm it when it ends. On Windows, pass the scratchpad as a `/c/...` path (GNU tar reads `C:` as a remote host), and keep the `timeout`: an expired Monitor there leaves its loop running.
5. Launch both runs in the turn of a typed user message that names them, such as "resume both S2 runs". Never put the signing key in that message.
   - WP-51: `Workflow({scriptPath: "C:/Users/ravin/Projects/Ruralz/.claude/plans/m1/m1-wp51-close.js", args: {"maxRounds": 6, "maxAttempts": 3, "startGroup": "G04", "startAttempt": 3, "carryFile": "C:/Users/ravin/Projects/Ruralz/.claude/plans/m1/runs/w3-s2-wp51-carry2.json", "carriedCount": 5, "scratchpad": "<this session's scratchpad>"}})`
   - Minors: `Workflow({scriptPath: "C:/Users/ravin/Projects/Ruralz/.claude/plans/m1/m1-section.js", args: {"ids": ["WP-35", "WP-36", "WP-37", "WP-38", "WP-39"], "reported": ["WP-35"], "initialFixFile": "C:/Users/ravin/Projects/Ruralz/.claude/plans/m1/runs/w3-s2-minors-continue3.json", "initialFixIds": ["WP-36", "WP-37", "WP-38", "WP-39"], "titles": {"WP-35": "fix(config): close the deferred findings of the schema view", "WP-36": "fix(config): close the deferred findings of overlay and substitution", "WP-37": "fix(config): close the deferred findings of stage G defaults", "WP-38": "fix(config): close the deferred findings of stage I precedence", "WP-39": "fix(config): close the deferred findings of the canonical form"}, "maxRounds": {"WP-35": 4, "WP-36": 4, "WP-37": 4, "WP-38": 4, "WP-39": 4}}})`
   - If a run stops again, continue it the same way: rebuild the carry or continuation file from the new journal (see "Starting a workflow run"). An agent that dies on an API error returns no result, so the journal has its `started` entry but no `result` entry, and `m1-section.js` then stops that package without a log line: check every package's agents in the journal.
6. When both runs report zero findings:
   - Apply the contract requests: each run's result, the carry file's `contractRequests` and the continuation file's `_meta.contractRequests` ("How a wave runs" step 4; `docs/_meta/` changes need the user's approval).
   - Run the whole-tree checks ("How a wave runs" step 3).
   - Commit on `develop`, one signed commit per package: WP-51 as `fix(filter): ...` with `Refs: WP-51 (M1 architecture)`, and WP-35 to WP-39 with the five titles above. Then commit the plan and spec files as `chore: ...`, without `runs/journals/` and `runs/scratch/`.
   - Push `develop`, then delete `wip/s2-handoff` on origin (`git push origin --delete wip/s2-handoff` works from the Windows checkout; the container's git proxy refused it), so only `main` and `develop` remain.
7. Record S2 in `wave3-plan.md`. Then comes WP-33's C2 rebuild, after the user asks for ADR-0020, and S3.

## Starting a workflow run

The workflow harness gives every agent, verbatim, the user message that triggered the turn in which the run launched, as "the only user voice in this task". It marks the script's prompt text as carrying no user authority, so quoted consent in a prompt does not count, and Claude Code blocks such an edit as instruction poisoning. On 2026-10-10 the S2 runs and the YAML research launched in a turn that answered an `AskUserQuestion` choice. Every agent saw only the user's earlier message ("Do not start any workflow, section or package until I say which") and refused. The runs wrote no files, cost about $8 to $12, and the review loop still gave WP-38 a `pass` verdict on no code.

- Launch or resume a run only in the turn of a typed user message that names it, such as "Run S2 with m1-resume.js and the YAML parser research". An `AskUserQuestion` answer is not relayed. A later message, such as a status question, becomes the relayed request for any run launched or resumed in its turn.
- Never paste the signing key into a message that starts a run: every agent receives it verbatim. Send it in a message of its own.
- `m1-resume.js` and `m1-section.js` stop a package whose engineer or fix round returns nothing or `blocked` with no files, and report `finalVerdict: blocked` with `stoppedAt`; never commit or record such a package. `m1-yaml-decision.js` stops before the brief unless every researcher reports `status: done`.

- Resume replays only the longest unchanged prefix of agent calls in call order. A sequential script (`m1-wp51-close.js`) resumes cleanly; a script that runs packages in parallel (`m1-section.js`) does not, because the call order differs on replay and every agent after the first difference runs live (on 2026-10-17 a resume of `wf_9c24f909-7ca` began re-running round 1). Continue such a run instead: extract each package's open findings and last fix report from its `journal.jsonl` into a file (as `runs/w3-s2-minors-continue.json` and `reports/<id>.json`), then launch `m1-section.js` with `initialFixFile` and `initialFixIds` (and `reported` for a package whose next step is a review).

## Working rules from the user

- Commit directly to `develop`; no pull requests and no other branches. The one exception is `wip/s2-handoff`, approved on 2026-10-17 to carry the S2 work to another subscription and used again the same day to pause S2 on the user's Windows machine. When S2 is committed, it is deleted on origin so that only `main` and `develop` remain (the user's instruction of 2026-10-17). The author comes from the repository git config (Ravindu Wijegunawardhana). Commits carry `Signed-off-by` (`-s`), are signed with the user's SSH signing key (`-S`), and carry no `Co-Authored-By` trailer.
- Signing key: from 2026-10-16 the user signs with an RSA 4096 key, fingerprint `SHA256:LIT3iCVGg5U16s1+r/ew5sxXydYhQ7y2g6VONvpKVpw`; commits before that carry the ED25519 key `SHA256:TsJhk+hdD1BB/pTF0GWj1jPlRSUm0MRu+MF94u0Ixpg`. Put both public keys in the allowed-signers file and check that `git log --format='%G? %GK'` shows `G` with the current fingerprint. On 2026-10-17 the user chose the ED25519 key in `C:\Users\ravin\OneDrive\Documents\Keys` for commits from the Windows machine.
- In a container, ask the user for the signing key in a message of its own when a commit batch is ready, and delete it from the container once the batch is pushed. On the Windows machine the key stays in the user's folder and only the repository's git config points to it. Validate every commit title with `go run ./internal/tool/commitcheck title -title "<title>"`.
- Changes under `docs/_meta/` need the user's explicit approval.
- Nothing closes with an open finding of any severity. Fix whole classes, with a proven test per case (AGENTS.md "Quality standard"; `user-decisions.md` item 16).
- Order in S2: the WP-51 fix to zero findings, then the deferred minors of WP-35 to WP-39, then WP-33's C2 rebuild after the user asks for ADR-0020. Stop and report after each.
- While runs are going, back up `internal/filter/validation/` (and the config packages under fix) to the scratchpad every 10 minutes, with dated file names (`tools/backup-tick.sh`).
- Multi-agent workflows are approved for all remaining M1 waves.
- After M1 come M1 integration, end-to-end tests and docs, then M2.
