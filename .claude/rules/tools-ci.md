---
paths:
  - "internal/tool/**"
  - "Makefile"
  - "scripts/**"
  - ".github/**"
---

# CI tools, Makefile, scripts and workflows

Authority: `docs/engineering/02-repository-layout-and-conventions.md` sections "CI stages" and "Toolchain and modules"; `CONTRIBUTING.md` for the target table.

## `internal/tool/*`

- Each tool is a `package main`, never linked into a binary; only `internal/tool` may import `internal/tool` (depguard `tools` rule). repocheck, commitcheck and depgate run as `go run ./internal/tool/<name>` from the root; schemagen runs only through the `//go:generate` line in `pkg/config/v1alpha1/doc.go`.
- Tools use only the standard library, like the rest of the module. Registries and denylists are functions returning literals (`repocheck.deniedTerms()`, `commitcheck.componentScopes()`, `depgate` `policy.go`).
- Checkers (repocheck, commitcheck, depgate) exit 1 for findings and 2 for usage or I/O errors, printing `<tool>: <error>` to stderr (`internal/tool/repocheck/main.go:31-42`). schemagen, a generator, exits 1 on any error. Follow the 1 and 2 split in a new checker.
- Commit scope is `build(tools)`, never `feat(tools)` or `test(tools)`.
- Self-tests read the real repository: repocheck `TestRepositoryIsClean`, depgate `TestRepositoryPasses`, and commitcheck `TestDocSlugs` and `TestRunUsage` (which read `docs/` for slugs). A new check must pass on the current tree, and renaming a doc can break them. `TestGitCommits` builds its own temporary repository.
- repocheck walks the file system from the root and skips `.git`, `node_modules`, `testdata`, `bin`, `dist` and every dot-directory (`repocheck.go:49-54`); untracked files count. Its own tests split fake RZ codes (`"RZ-RT-" + "999"`) to avoid tripping the literal check.
- A new repocheck check needs a fixture: `TestBadRepo` (`repocheck_test.go:13-49`) runs every check over `testdata/bad` and requires exactly its `want` list, so add a fixture that trips the new check, add its finding to `want`, and make sure nothing else fires. Also update the check list in the `internal/tool/repocheck/main.go` package comment, the Ruralz-specific checks paragraph of doc 02, and step 1a of `.claude/skills/preflight/SKILL.md`.
- `nolicensecheck.allow` lines are `<path or dir/> <term> # <reason>`; the reason is required and the term must be a denied term or host (`repocheck.go:278-307`). Edits need two approvals.
- Not implemented yet: metric and span name checks (M1, with `internal/telemetry`) and the Ruralz Console placeholder check (M2).
- schemagen: see `.claude/rules/config-schema.md`.

## Makefile and scripts

- Every Makefile, script and workflow carries `# Copyright <creation year> Revington` and `# SPDX-License-Identifier: Apache-2.0` at the top, after the `#!/usr/bin/env bash` line in shell scripts (convention; not machine checked). Shell scripts use `set -euo pipefail`; the Makefile uses `SHELL := bash` with `-eu -o pipefail`.
- One CI stage calls one make target: `hygiene` (1), `lint` (2), `generate` (3), `build` (4), `floor` (4, Go 1.26), `test` (5), `supply-chain` (6). Keep new work inside those targets rather than inline in workflows.
- Build tools never enter `go.mod` as `tool` directives. They are pinned, checksum-verified binaries installed into `bin/` by `scripts/install-tools.sh` (golangci-lint v2.13.2, govulncheck v1.8.0).
- `install-tools.sh` pins only linux and darwin archives and expects `.tar.gz`, so `make tools` and `make lint` fail on Windows. Say so instead of working around it silently.
- `scripts/ci-changes.sh` sets the path filters: any change under `.github/workflows/` runs every stage; Go, `go.mod`, `.golangci.yml`, `Makefile` or `scripts/` changes run lint, build and test; `pkg/`, `api/`, `internal/tool/` and similar run the generate stage. A docs-only change runs only hygiene and supply-chain, so no Go test (including `TestMatchesDocs`) runs on it.
- `scripts/ci-approvals.sh` holds the two-approval path list (`api/`, `pkg/`, `docs/_meta/`, `go.mod`, security-sensitive packages, `nolicensecheck.allow`, `.github/`).

## Workflows (`.github/workflows/`)

- Required checks: `pr-fast` (stages 1 to 6 plus a final `pr-fast` job that passes when every job in its `needs:` list passed or was path-skipped), `pr-title` and `approvals`. Never rename these jobs: branch protection requires them by name.
- A new pr-fast job goes into the final job's `needs:` list. A new path filter is written in each of the three `printf` lines that append to `$out` in `scripts/ci-changes.sh` and declared under `jobs.changes.outputs`; a missing output makes its job skip, and a skipped job counts as passed.
- `nightly.yml` only reruns stage 6 (`make supply-chain`) each night. The stage 11 fuzz, chaos and scale jobs are Planned (M1).
- Reference every action by its full commit SHA with a `# vX.Y.Z` comment, as the existing steps do; a new action needs a row in the CI tooling table of `docs/engineering/01-tech-stack-and-libraries.md`. Keep `persist-credentials: false` on checkout and the read-only `permissions`.
- `setup-go` installs the `go.mod` toolchain line and then exports `GOTOOLCHAIN=local`; do not set `GOTOOLCHAIN` at workflow level (commit 38507ff).
- The Windows and macOS job runs `go test -shuffle=on ./internal/cli/... ./internal/buildinfo/... ./pkg/...`; keep those packages portable.
- Commit scope is `ci(workflows)`. Any `.github/` change needs two approvals.
