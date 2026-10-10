# Wave 3 plan: nine small sections that stay under the usage limit

Wave 3 builds 25 work packages: the configuration pipeline stages, the CEL module, the telemetry runtime, the request path, the Upstream runtimes, the auth Filters, the State Store manager, test servers, release tooling, examples and the WP-31 documents. Run as two big workflow runs, it would cost about $650 to $800 at API prices and hit the 5-hour usage limit two or three times, losing the agents that were running at the time.

This plan splits wave 3 into nine sections. Each section is small enough to finish inside one usage window, ends with its packages committed and pushed, and leaves nothing in flight. Read `README.md` in this directory first for the working rules and the wave procedure.

## Budget per section

| Measure | Value | Source |
|---|---|---|
| Agent usage that fit in one 5-hour window before the limit hit | about $140 to $160 at API prices | Both wave-2 limit hits (runs `wf_b1cb0ffb-f17` with `wf_a510ab83-bb4`, then `wf_e333a104-cc7` with `wf_2d694646-559`), measured from the agent transcripts |
| Target per section | $70 to $95, about 60% of a window | Leaves room for the lead's own work and checks |
| Cost per size unit (S = 0.5, M = 1, L = 2) | about $17 for implement, review and fixes | Wave 2: about $540 to $640 for 34 size units |
| Section size | 4 to 5 size units | $70 to $90 |

Costs are API-equivalent estimates at Claude Opus 5.5 prices ($4 input, $20 output, $0.20 cache read, $5 to $8 cache write per million tokens). On a subscription they measure how much of a usage window a section uses. About 60% of the cost is cache reads: each agent re-reads its growing context on every turn.

## Sections

| Section | Theme | Run a | Run b | Size units | Estimate | Why together |
|---|---|---|---|---|---|---|
| S1 | Configuration front end | WP-33 (L) | WP-35 (M), WP-36 (M) | 4 | ~$70 | YAML profile, schema view, overlays and substitution feed every later stage |
| S2 | Configuration back end | WP-37 (M), WP-38 (M) | WP-39 (M), WP-51 (M) | 4 | ~$70 | Defaults, precedence, canonical form, and `validation.json-schema`, which reuses the schema view from S1 |
| S3 | Expressions and telemetry | WP-34 (L) | WP-40 (L) | 4 | ~$70 | Two large, independent cores many later packages call |
| S4 | Request path | WP-43 (L), WP-42 (M) | WP-44 (M), WP-45 (M) | 5 | ~$85 | Router, exchange, listeners and the admin server (admin 404 and 405 use `RZ-RT-020` and `RZ-RT-021`) |
| S5 | Upstream and State Store | WP-46 (L) | WP-65 (M), WP-88 (M) | 4 | ~$70 | Upstream attempt loop, State Store manager, Response Cache revalidation |
| S6 | Auth Filters | WP-47 (L), WP-48 (M) | WP-49 (M), WP-50 (M) | 5 | ~$85 | `auth.jwt`, `auth.api-key`, `auth.basic`, `auth.mtls`, `authz.ip`, `cors`, `auth.upstream-oauth2` share the auth helpers and the challenge rules |
| S7 | Test kit, release, examples | WP-85 (L), WP-53 (S) | WP-52 (M), WP-54 (M) | 4.5 | ~$75 | State Store test servers, load generation, release packaging, examples |
| S8 | Wave-3 decisions | | | | ~$85 | Read-only triage of the contract change requests from S1 to S7, then the decisions applied by owned-path units (`m1-apply.js`) |
| S9 | Wave-3 documents and cleanup | WP-31 (M) | | 1 + cleanup | ~$75 | WP-31 closes the traffic, observability and scalability questions after every code package has landed; then one batched pass fixes the minor findings deferred from S1 to S7 |

Total: about $700, spread over nine sections. That is about the same as two big runs, but no section loses work to a limit hit. Run files are in `runs/w3-s<N><a|b>.json`; each holds the package IDs, each package's notes from `forwards.json` and `deferMinor: true`.

The order matters only between S1 and S2 (S2's `validation.json-schema` reuses S1's schema view) and for S8 and S9, which come last. No wave-3 package depends on another wave-3 package, so S3 to S7 can run in any order.

## Running one section

1. **Start of a usage window.** Start a section only near the start of a 5-hour window, or when Claude Code's usage display shows at least two thirds of the window left. Run one section at a time.
2. **Preflight (no agent tokens).** `git fetch origin && git status` (clean, up to date with `origin/develop`). Set up signing as `README.md` step 5 describes, and delete stray binaries in the repository root.
3. **Launch both runs together.** Two `Workflow` calls on `m1-resume.js`, one with `runs/w3-s<N>a.json` and one with `runs/w3-s<N>b.json`, so four agents work at once. A section takes about 2 to 3 hours.
4. **Commit each passing package** when its run finishes, in dependency order, exactly as `README.md` "How a wave runs" step 2 describes (owned paths only, `--pathspec-from-file`, `git commit -S -s`, no `Co-Authored-By`). A package whose `finalVerdict` is not `pass` stays uncommitted, is listed in the progress table, and gets a targeted fix run in S9.
5. **Whole-tree checks (local CPU, no agent tokens).** `go build ./...`, `go vet ./...`, `bin/golangci-lint run ./...`, `go run ./internal/tool/repocheck`, `go run ./internal/tool/depgate`, `go generate ./...` with no diff, the race and `CGO_ENABLED=0` test runs, and `make floor FLOOR_GOTOOLCHAIN=go1.26.8`. Fix failures at their root cause; a flaky test gets a root-cause fix like the wave-2 ones.
6. **Notes for later sections (no agent tokens).** Read each package's `contractRequests`. A request that a package in a later wave-3 section must follow becomes a note in `forwards.json` and in that section's run files. Everything else waits for S8.
7. **Record and push.** Fill in the section's row in the progress table below, save each run's result to `runs/w3-s<N><a|b>-result.json` (the deferred minor findings are in it, with their text), commit as `chore: record wave-3 section S<N> in the M1 plan`, and push.
8. **Measure.** Total the section's agent usage with `python3 tools/usage.py <workflows dir> <run id>...` (it reads each run's `journal.jsonl` and `agent-*.jsonl` transcripts; price output at $20, cache writes at $5 to $8 and cache reads at $0.20 per million) and record it. If a section costs more than about $95, make the next sections smaller by moving one M package to S9 or a new section.

## If a limit hits anyway

A run that hits the limit stops its agents; finished agents' results stay in its `journal.jsonl`. After the window resets, resume as in wave 2. Extract each finished `impl:WP-xx` result to a report file and pass the IDs as `reported` with their `titles`; pass any finished review as `reviews`. Then launch a fresh run of `m1-resume.js` with the same section run file plus those fields. Only the agents that were in flight run again.

## Token-saving rules, and what they cost

| Rule | Saving | Quality cost |
|---|---|---|
| Two runs per section, never more than four agents | None in tokens; finishes inside the window | None |
| `deferMinor`: no fix round for minor-only findings; one batched cleanup in S9 | About 1 in 6 packages skips a full fix and re-review cycle (5 of 32 in wave 2) | Minor findings land a few sections later |
| Up to two fix rounds, and a second only while a blocker or major finding remains | With `deferMinor`, round two runs only for real defects; wave 2 needed it for 4 of 32 packages, all medium size, so no cap is set (`maxRounds` can set one per package) | None |
| Lead commits; no agent commit stage | One agent per package | None |
| Whole-tree checks run by the lead | No agent tokens | None |
| Lean lead work: no large tool outputs, results read from files | Keeps the lead's own usage small | None |

Optional, off by default (each trades some quality for tokens; try one in a section and compare):

- Reviewer effort `medium` for M and S packages (the `effort` option of `agent()`), keeping L packages at the default.
- A short brief per package (its 4.4 entry, its 1.2 rows, the R-rows and spec requirements it cites), passed through `extra`, so the engineer reads less of the 7,700-line architecture.

## Progress

| Section | Packages committed | Not passed (moved to S9) | Agent usage | Commits | Date |
|---|---|---|---|---|---|
| S1 | WP-35 `79154e9`, WP-36 `196619b`, WP-33 `086e74d` | None; WP-33 landed with five open findings (four blockers, one major) pending the YAML parser decision | About $315 to $377: 1,067M cache read and 20.4M other tokens over six runs (runs a and b, two targeted fixes, the hardening loop and one targeted round) | 3 package commits, 7 plan commits | 2026-10-09 to 2026-10-10 |
| S2 | | | | | |
| S3 | | | | | |
| S4 | | | | | |
| S5 | | | | | |
| S6 | | | | | |
| S7 | | | | | |
| S8 | | | | | |
| S9 | | | | | |

### S1 notes

- WP-35 and WP-36 passed in one run each, at about $22 to $27 per size unit, above the $17 estimate.
- WP-33 never came back clean. Its reviews kept finding new places where goccy/go-yaml v1.19.2 departs from YAML 1.2 or costs more than the token pass predicts: confirmed blocker or major findings went 12, 8, 4 and 5 over the hardening loop (`m1-harden.js`) and one targeted round. The package grew to about 6,200 production lines, most of them workarounds. The user chose to commit it with the five open findings (`runs/w3-s1a-harden2-result.json`) and to weigh a parser change; the decision brief is `yaml-parser-decision.md`.
- A container restart stopped one fix run mid-way; its edits survived on disk and the next run finished them. Back up uncommitted package trees in the scratchpad during long runs.
