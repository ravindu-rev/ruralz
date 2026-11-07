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

- Each tool is a `package main`, never linked into a binary; only `internal/tool` imports `internal/tool`. repocheck, commitcheck and depgate run as `go run ./internal/tool/<name>` from the root; schemagen and telemetrygen run through `//go:generate` lines (`pkg/config/v1alpha1/doc.go`, `internal/tool/telemetrygen/main.go`).
- Tools use only the standard library. Registries and denylists are functions returning literals.
- Checkers exit 1 for findings and 2 for usage or I/O errors, printing `<tool>: <error>` to stderr; schemagen exits 1 on any error.
- Commit scope is `build(tools)`.
- Self-tests read the real repository (repocheck `TestRepositoryIsClean`, depgate `TestRepositoryPasses`), so a new check must pass on the current tree.
- repocheck walks from the root and skips `.git`, `node_modules`, `testdata`, `bin`, `dist` and dot-directories; untracked files count. A new check needs a fixture in `testdata/bad` and its finding in the `TestBadRepo` want list, and a line in the check list of `internal/tool/repocheck/main.go`.
- `nolicensecheck.allow` lines are `<path or dir/> <term> # <reason>`.

## Makefile and scripts

- Every Makefile, script and workflow starts with `# Copyright <creation year> Revington` and `# SPDX-License-Identifier: Apache-2.0` (after the `#!/usr/bin/env bash` line in scripts). Scripts use `set -euo pipefail`; the Makefile uses `SHELL := bash` with `-eu -o pipefail`.
- Each PR pipeline job runs make targets: `ci` runs `lint`, `dco` and `supply-chain`, `unit` runs `test`, `integration` runs `integration`. The release runs `release`, and `release-dry-run` its steps without signing; `generate`, `build`, `fmt` and `tools` are for contributors. Keep new work inside those targets.
- A recipe reads a value that can come from a workflow, such as `DCO_RANGE`, from the environment (`"$$DCO_RANGE"`), never spliced into the shell command line.
- Build tools never enter `go.mod`. They are pinned, checksum-verified binaries installed into `bin/` by `scripts/install-tools.sh`, which pins only linux and darwin, so `make tools` fails on Windows: say so instead of working around it.

## Workflows (`.github/workflows/`)

- `pr.yml` is the PR pipeline: jobs `ci`, `unit` and `integration` in order, then `pr`, which passes only when the three passed and is the one required check. `release.yml` calls `pr.yml` through `workflow_call` on the commit it releases. Never rename these jobs, and put a new job into the `pr` job's `needs:` list. There are no path filters.
- Reference every action by its full commit SHA with a `# vX.Y.Z` comment; a new action needs a row in the CI tooling table of the tech stack document. Keep `persist-credentials: false` on checkout and read-only `permissions`.
- `setup-go` installs the `go.mod` toolchain line and exports `GOTOOLCHAIN=local`; do not set `GOTOOLCHAIN` at workflow level.
- The CLI ships for darwin and windows, which only `make build` and the release build compile: keep `internal/cli` and what it imports building there.
- Commit scope is `ci(workflows)`.
