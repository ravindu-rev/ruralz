---
paths:
  - "internal/tool/**"
  - "Makefile"
  - "scripts/**"
  - ".github/**"
---

# CI tools, Makefile, scripts and workflows

Authority: `docs/engineering/02-repository-layout-and-conventions.md` sections "CI stages" and "Toolchain and modules".

## `internal/tool/*`

- Each tool is a `package main`, never linked into a binary; only `internal/tool` imports `internal/tool`. repocheck, commitcheck and depgate run as `go run ./internal/tool/<name>` from the root; schemagen runs through the `//go:generate` line in `pkg/config/v1alpha1/doc.go`.
- Tools use only the standard library. Registries and denylists are functions returning literals.
- Checkers exit 1 for findings and 2 for usage or I/O errors, printing `<tool>: <error>` to stderr; schemagen exits 1 on any error.
- Commit scope is `build(tools)`.
- Self-tests read the real repository (repocheck `TestRepositoryIsClean`, depgate `TestRepositoryPasses`, commitcheck `TestDocSlugs` and `TestRunUsage`, which read `docs/` file names for slugs), so a new check must pass on the current tree and renaming a doc can break them.
- repocheck walks from the root and skips `.git`, `node_modules`, `testdata`, `bin`, `dist` and dot-directories; untracked files count. A new check needs a fixture in `testdata/bad` and its finding in the `TestBadRepo` want list, and a line in the check list of `internal/tool/repocheck/main.go`.
- `nolicensecheck.allow` lines are `<path or dir/> <term> # <reason>`.

## Makefile and scripts

- Every Makefile, script and workflow starts with `# Copyright <creation year> Revington` and `# SPDX-License-Identifier: Apache-2.0` (after the `#!/usr/bin/env bash` line in scripts). Scripts use `set -euo pipefail`; the Makefile uses `SHELL := bash` with `-eu -o pipefail`.
- One CI stage calls one make target: `hygiene` (1), `lint` (2), `generate` (3), `build` and `floor` (4), `test` (5), `supply-chain` (6), `integration` (8), `gates` (9), `e2e` and `quickstart` (10), `nightly` with `NIGHTLY_JOB=fuzz|chaos|latency` (11), `release-dry-run`, `release` and `release-gates` (12). Keep new work inside those targets.
- Build tools never enter `go.mod`. They are pinned, checksum-verified binaries installed into `bin/` by `scripts/install-tools.sh`, which pins only linux and darwin, so `make tools` fails on Windows: say so instead of working around it.
- `scripts/ci-changes.sh` sets the path filters; a docs-only change runs only hygiene and supply-chain, so no Go test runs on it in CI.
- `scripts/ci-approvals.sh perf-override` accepts the stage 9 gate failures when a maintainer applies the `perf-override` label.

## Workflows (`.github/workflows/`)

- Required checks: `pr-fast` (stages 1 to 6, plus a final `pr-fast` job that passes when every job in its `needs:` list passed or was path-skipped) and `pr-title`. Never rename these jobs.
- A new pr-fast job goes into the final job's `needs:` list. A new path filter is written in each `printf` line that appends to `$out` in `scripts/ci-changes.sh` and declared under `jobs.changes.outputs`; a missing output makes its job skip.
- Reference every action by its full commit SHA with a `# vX.Y.Z` comment; a new action needs a row in the CI tooling table of the tech stack document. Keep `persist-credentials: false` on checkout and read-only `permissions`.
- `setup-go` installs the `go.mod` toolchain line and exports `GOTOOLCHAIN=local`; do not set `GOTOOLCHAIN` at workflow level.
- The Windows and macOS job runs `go test -shuffle=on ./internal/cli/... ./internal/buildinfo/... ./pkg/...`; keep those packages portable.
- Commit scope is `ci(workflows)`.
