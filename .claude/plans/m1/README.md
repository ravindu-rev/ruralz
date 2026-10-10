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
| `m1-apply.js` | Applies triaged decisions per unit (owned paths, then review and fix) |
| `m1-gov.js` | Documentation and ADR units with dependencies (`after`) |
| `wave3-plan.md` | How wave 3 runs: nine small sections that each fit in one usage window, with the run order, the per-section procedure and a progress table |
| `runs/w3-s<N><a\|b>.json` | Ready arguments for each wave-3 section's two runs (package IDs, their notes from `forwards.json`, `deferMinor`) |
| `tools/usage.py`, `tools/wordcount.py` | Agent token usage per workflow run, and the manifest word count of a document |

## State on 2026-10-09

- `develop` holds waves 1 and 2, complete: WP-01 to WP-30, WP-32, WP-83 and WP-84, the two test-race fixes, the wave-2 boundary decisions (R-63 to R-75 in code, architecture and docs), the M1 guide updates in `AGENTS.md`, `CONTRIBUTING.md` and `.claude/rules/`, the documentation below, and the wave-2 completion of 2026-10-09.
- On 2026-10-09 every commit on `develop` after `main` was rewritten so the user is author and committer, each is signed with the user's SSH key, and none carries a `Co-Authored-By` trailer; commit hashes before that date no longer exist.
- Waves 3 to 10 are not started. Wave 3 has 25 work packages: WP-31, 33 to 40, 42 to 54, 65, 85 and 88. None depends on another in the same wave. Run wave 3 by `wave3-plan.md`: nine sections, each committed and pushed before the next; WP-31 runs in S9, after every wave-3 code package.

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
2. When a run finishes, read each package's `finalVerdict`. For every `pass`, the lead commits only the files inside that package's `dirs` (match `git status --porcelain --untracked-files=all` against `wps.json`), in dependency order, after `go build`, `go vet` and the tests of those packages pass:
   - title validated with `go run ./internal/tool/commitcheck title -title "<title>"` (allowed scopes in `AGENTS.md`);
   - `git commit -S -s -F <msg> --pathspec-from-file=<list>` (signed with the user's SSH signing key) so another package's staged files are never swept in;
   - message body of 2 to 6 lines, then `Refs: WP-xx (M1 architecture)`; no `Co-Authored-By` trailer;
   - verify with `git show --no-renames --name-only --format= HEAD`, then push `develop`.
3. At the wave boundary, run the whole-tree checks: `go build ./...`, `go vet ./...`, `bin/golangci-lint run ./...` and `fmt --diff`, `go run ./internal/tool/repocheck`, `go run ./internal/tool/depgate`, `go mod verify`, `go generate ./...` with no diff, `CGO_ENABLED=1 go test -race -shuffle=on -tags netgo,osusergo ./...`, `CGO_ENABLED=0 go test -shuffle=on ./...` and `make floor FLOOR_GOTOOLCHAIN=go1.26.8`. Fix a failing or flaky test at its root cause, never by skipping it.
4. Collect every package's `contractRequests`, triage them read-only (the same action classes as `triage.json`), ask the user only what is theirs to decide (ADRs, `docs/_meta/`, `nolicensecheck.allow`, runners, repository settings), apply the rest with `m1-apply.js`, record decisions in `specs/00-architecture.md` 2.16 and `arch/wps.json`, and add notes for later packages to `forwards.json`.
5. Signing in a Claude Code cloud container: its global git config (`/root/.gitconfig`) sets `commit.gpgsign`, `gpg.ssh.program=/tmp/code-sign` and its own key, so a plain `git commit -S` is signed with the environment's key, not the user's. Before committing, set repository-local `gpg.format ssh`, `gpg.ssh.program /usr/bin/ssh-keygen` (install `openssh-client` if it is missing) and `user.signingkey` to the user's key, then check that `git log --format='%G? %GK'` shows `G` and the user's key fingerprint.
6. Agents sometimes build a binary into the repository root (`go build ./internal/tool/x` without `-o`); delete such stray files before committing.

## Working rules from the user

- Commit directly to `develop`; no pull requests. The author comes from the repository git config (Ravindu Wijegunawardhana). Commits carry `Signed-off-by` (`-s`), are signed with the user's SSH signing key (`-S`), and carry no `Co-Authored-By` trailer.
- Multi-agent workflows are approved for all remaining M1 waves.
- After M1 come M1 integration, end-to-end tests and docs, then M2.
