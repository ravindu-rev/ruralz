---
description: Run the local equivalent of Ruralz CI stages 1 to 6 (repocheck, DCO and title checks, schema drift, build, vet, tests, supply chain) plus quick text checks on changed files, and report pass, fail or skipped for each.
when_to_use: Before finishing a change, before a commit or pull request, or when the user asks whether CI will pass.
argument-hint: "[pull request title]"
allowed-tools: Bash(go version) Bash(go env *) Bash(go run ./internal/tool/repocheck *) Bash(go run ./internal/tool/commitcheck *) Bash(go run ./internal/tool/depgate *) Bash(go generate *) Bash(go build *) Bash(go vet *) Bash(go test *) Bash(go mod verify) Bash(gofmt -l *) Bash(make lint) Bash(golangci-lint *) Bash(git status *) Bash(git diff *) Bash(git log *) Bash(git ls-files *) Bash(git rev-parse *) Bash(command -v *) PowerShell(go version) PowerShell(go env *) PowerShell(go run ./internal/tool/repocheck *) PowerShell(go run ./internal/tool/commitcheck *) PowerShell(go run ./internal/tool/depgate *) PowerShell(go generate *) PowerShell(go build *) PowerShell(go vet *) PowerShell(go test *) PowerShell(go mod verify) PowerShell(git status *) PowerShell(git diff *) PowerShell(git log *) PowerShell(git ls-files *) PowerShell(git rev-parse *)
---

# Preflight

Run every step from the repository root, in order, and keep going after a failure. Prefer the Bash tool (Git Bash); the commands also work in PowerShell with `$env:CGO_ENABLED = "0"` instead of the inline assignment. Do not install tools, and do not fix anything unless the user asks: this skill only reports.

## 0. Environment

- `go version`. If Go is missing, mark steps 1a to 6 **skipped (Go not installed)**, do the manual checks in 1d, and still run step 7.
- The version should be go1.27.1 or newer (from the `go.mod` toolchain line). Go 1.26 without `GOEXPERIMENT=jsonv2` fails on purpose at `internal/buildinfo/floor_guard.go`.
- Base for commit checks: `origin/main` if `git rev-parse --verify origin/main` succeeds, else `main`.

## 1. Hygiene (CI stage 1)

a. `go run ./internal/tool/repocheck`: cmd wiring, `import "C"`, RZ literals, public imports, proto and TypeScript headers, no-license-check scan. It scans untracked files too.
b. `go run ./internal/tool/commitcheck dco -range <base>..HEAD`. Skip with a note when HEAD has no commits beyond the base.
c. Title: with an argument, `go run ./internal/tool/commitcheck title -title "$ARGUMENTS"`. Without one, check each `git log --no-merges --format=%s <base>..HEAD` subject the same way and report them as advisory (CI checks only the pull request title).
d. Only when Go is missing, check by hand and report as **manual**. Title or subjects: they match `^([a-z]+)(?:\(([a-z0-9][a-z0-9-]*)\))?(!)?: (.+)$`, use a type and scope from the `AGENTS.md` table (a `docs` scope is a document slug), do not start with an upper-case letter or a space, and have no trailing period. DCO: `git log --no-merges --format='%h %an <%ae>%n%(trailers:key=Signed-off-by,valueonly)' <base>..HEAD` shows, for each commit, a sign-off whose name equals the author exactly and whose email matches ignoring case. RZ codes: every `RZ-<AREA>-<NNN>` string in changed `.go` files outside `internal/errcode/` appears in `All()` of `internal/errcode/errcode.go`.

## 2. Lint (CI stage 2)

- If `bin/golangci-lint` exists, run `make lint`.
- If only a golangci-lint 2.13.x is on PATH, run `golangci-lint config verify`, `golangci-lint fmt --diff ./...` and `golangci-lint run ./...` directly; `make lint` would try to download a copy into `bin/`.
- Otherwise mark lint **skipped** (`make tools` has no Windows pin) and, as a partial substitute, run `go vet ./...` and `gofmt -l .` (gofumpt is stricter).

## 3. Generated code (CI stage 3)

`go generate ./...`, then `git diff --quiet -- api/schema/ruralz` and `git ls-files --others --exclude-standard -- api/schema/ruralz`, as `make generate` does. A diff or an untracked file means the schema differs from the index: either the committed schema was stale, or regenerated files are not staged yet. Report which, and leave the files for the user to review.

## 4. Build (CI stage 4)

`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...`, plus `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/preflight/ ./cmd/ruralz` for the Windows CLI. Building one main package without `-o` would drop an executable in the repository root; `bin/` is gitignored and skipped by repocheck. The full cross-build is `make build`; run it only when the user asks.

## 5. Tests (CI stage 5)

`CGO_ENABLED=0 go test -shuffle=on ./...`. When `command -v gcc` (or `cc`) succeeds, also run `CGO_ENABLED=1 go test -race -shuffle=on -tags netgo,osusergo ./...` (the first line of `make test`). Report failing tests by name with the first lines of output.

## 6. Supply chain (CI stage 6)

`go mod verify`, then `go run ./internal/tool/depgate`. govulncheck is **skipped** unless `bin/govulncheck` exists.

## 7. Text checks on changed files

Changed files are `git diff --name-only <base>...HEAD` plus `git status --porcelain --untracked-files=all`. On them:

- Only when Go is missing (repocheck covers it otherwise), files under `cmd/ internal/ pkg/ sdk/ console/`: the no-license-check terms and host from `deniedTerms()` and `deniedHosts()` in `internal/tool/repocheck/repocheck.go`, matched as `AGENTS.md` describes. Skip `testdata/`, `bin/`, `dist/`, dot-directories, and any path and term listed in `internal/tool/repocheck/nolicensecheck.allow`.
- New `.go` files start with the two-line license header, or with `// Code generated`.
- Markdown under `docs/`: no em dashes, no emojis, no `TODO`, `TBD`, `XXX`, `lorem` or `???`; `Planned (Mx)` or `Not planned` tags, never `Supported`. An added or removed `RZ-` code in a doc needs `go test -run TestMatchesDocs ./internal/errcode/`, because CI skips Go tests on docs-only changes.
- `pkg/config/v1alpha1` changed but `api/schema/ruralz/` did not: warn that the schema is probably stale.

## Report

A table `Step | Result | Detail` with **pass**, **fail**, **manual** or **skipped (reason)** for every step. Then list each failure with the file and line and the rule it breaks (cite `AGENTS.md` or the area guide). End with the checks only CI can run here (lint, race tests, govulncheck, Go 1.26 floor, approvals).
