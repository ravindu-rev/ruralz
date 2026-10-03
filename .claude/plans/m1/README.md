# M1 implementation plan and hand-off state

This directory holds the working plan the M1 Core gateway implementation follows. It is not a design document: `docs/` is the specification, and every requirement here cites it. The scripts assume the repository at `/home/user/ruralz`; if the checkout lives elsewhere, change `REPO` and `M1` at the top of each script.

## Contents

| Path | What it is |
|---|---|
| `specs/00-architecture.md` | The binding integration architecture: section 0 conventions, 1.2 package table with allowed imports, 1.3 lint rules, 2 shared contracts (the committed WP-01 Go code wins where it differs), 2.16 resolved conflicts R-1 to R-75, 3 data flow, 4.1 work package rules, 4.4 every work package's Owns, Depends on, Scope and Done when, and the exit criteria map |
| `specs/01-*.md` to `specs/11-*.md` | Area specs with numbered requirements; "04 req 34" means spec 04, requirement 34 |
| `arch/wps.json` | The 99 work packages: `id`, `title`, `wave`, `dirs` (the paths it owns), `dependsOn`, `specs`, `scope`, `size` |
| `user-decisions.md` | The user's decisions at the wave-2 boundary |
| `crs-all.json`, `triage.json` | The 171 wave-2 contract change requests and the lead's decision for each (`action`: contract, arch, forward, docfix, repometa, user, done, reject) |
| `forwards.json` | Notes for later work packages, keyed by WP id; pass them to the wave script as `extra` |
| `m1-resume.js` | Wave workflow: per work package implement, adversarial review, fix and re-review (up to two rounds). Args: `ids`, `extra`; `reported`, `titles` and `reviews` resume packages whose engineer report or review is already saved. The agent commit stage is off (`args.commit` unset); the lead commits |
| `m1-apply.js` | Applies triaged decisions per unit (owned paths, then review and fix) |
| `m1-gov.js` | Documentation and ADR units with dependencies (`after`) |
| `runs/w3-args-A.json`, `runs/w3-args-B.json` | Ready arguments for wave 3 (24 work packages with their forward notes); WP-31 is not in them |

## State on 2026-10-03

- `develop` holds waves 1 and 2: WP-01 to WP-30, WP-83 and WP-84, the two test-race fixes, the wave-2 boundary decisions (R-63 to R-75 in code, architecture and docs), and the M1 guide updates in `AGENTS.md`, `CONTRIBUTING.md` and `.claude/rules/`.
- Documentation the user approved at the wave-2 boundary: the CI tooling research addendum, ADR-0018 superseding ADR-0010, ADR-0019 superseding ADR-0011, the foundation pack Upgrades row, and the rest of WP-32 (see "Documentation status" below).
- Waves 3 to 10 are not started. Wave 3 has 25 work packages: WP-31, 33 to 40, 42 to 54, 65, 85 and 88. None depends on another in the same wave. WP-31 owns `docs/architecture/09`, `10` and `11`, which the ADR work also edits, so run it after that work is committed.

## Documentation status

Filled in at hand-off; see the latest commit touching this file.

## How a wave runs

1. Launch `m1-resume.js` in two concurrent runs of about 12 work packages each (the container has 4 CPUs, and a workflow runs at most CPUs minus 2 agents at once). Put the large (`size: L`) packages first. Build `extra` from `forwards.json`.
2. When a run finishes, read each package's `finalVerdict`. For every `pass`, the lead commits only the files inside that package's `dirs` (match `git status --porcelain --untracked-files=all` against `wps.json`), in dependency order, after `go build`, `go vet` and the tests of those packages pass:
   - title validated with `go run ./internal/tool/commitcheck title -title "<title>"` (allowed scopes in `AGENTS.md`);
   - `git -c commit.gpgsign=false commit -s -F <msg> --pathspec-from-file=<list>` so another package's staged files are never swept in;
   - message body of 2 to 6 lines, then `Refs: WP-xx (M1 architecture)` and the `Co-Authored-By` trailer `CLAUDE.md` asks for;
   - verify with `git show --no-renames --name-only --format= HEAD`, then push `develop`.
3. At the wave boundary, run the whole-tree checks: `go build ./...`, `go vet ./...`, `bin/golangci-lint run ./...` and `fmt --diff`, `go run ./internal/tool/repocheck`, `go run ./internal/tool/depgate`, `go mod verify`, `go generate ./...` with no diff, `CGO_ENABLED=1 go test -race -shuffle=on -tags netgo,osusergo ./...`, `CGO_ENABLED=0 go test -shuffle=on ./...` and `make floor FLOOR_GOTOOLCHAIN=go1.26.8`. Fix a failing or flaky test at its root cause, never by skipping it.
4. Collect every package's `contractRequests`, triage them read-only (the same action classes as `triage.json`), ask the user only what is theirs to decide (ADRs, `docs/_meta/`, `nolicensecheck.allow`, runners, repository settings), apply the rest with `m1-apply.js`, record decisions in `specs/00-architecture.md` 2.16 and `arch/wps.json`, and add notes for later packages to `forwards.json`.
5. Agents sometimes build a binary into the repository root (`go build ./internal/tool/x` without `-o`); delete such stray files before committing.

## Working rules from the user

- Commit directly to `develop`; no pull requests. The author comes from the repository git config (Ravindu Wijegunawardhana). Commits carry `Signed-off-by` (`-s`) and the `Co-Authored-By` trailer from `CLAUDE.md`.
- Multi-agent workflows are approved for all remaining M1 waves.
- After M1 come M1 integration, end-to-end tests and docs, then M2.
