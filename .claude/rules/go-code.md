---
paths:
  - "**/*.go"
  - "go.mod"
  - ".golangci.yml"
---

# Go code in Ruralz

Authority: `docs/engineering/02-repository-layout-and-conventions.md` (Where Go code goes, Import boundaries, Code conventions, golangci-lint linter set). The hard rules in `AGENTS.md` apply first.

## File shape

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Package x does one thing, stated in a full sentence.
package x
```

- The build constraint line, when present, goes after the header and a blank line (`internal/buildinfo/floor_guard.go:1-6`). Build tags are only platform, Go version, `goexperiment`, and `integration` or `e2e` on `_test.go` files; never feature tags.
- Packages are lower-case, singular, no underscores, never `util`, `common` or `misc`. Files are `lower_snake.go` with tests beside them.
- Imports: standard library, then third-party modules, then `github.com/ravindu-rev/ruralz/...`, each group separated by a blank line (goimports with that local prefix, then gofumpt; `make fmt`). At M0 no third-party module is admitted, so files have at most two groups.
- Doc comments are full sentences starting with the identifier; exported identifiers are documented (revive). American spelling (misspell, locale US): `license`, never `licence`.
- Keep comments where they explain why. Do not write denied no-license-check terms in comments either (see `AGENTS.md`).

## Where code goes

- New code starts in `internal/`. Moving code to `pkg/` needs the release owner's approval, because `pkg/` becomes a SemVer promise.
- Each binary's `internal/<pkg>` exposes `Run(ctx context.Context, args []string, stdout, stderr io.Writer) int`; `cmd/<binary>/main.go` only wires it (`cmd/ruralz/main.go:17-22`). Parse flags inside `internal/` with a `flag.FlagSet` (`internal/cli/cli.go:58-67`).
- Exit codes are named constants that never change once released: `ExitOK = 0`, `ExitNegative = 1`, `ExitNoResult = 2` (`internal/cli/cli.go:19-28`). A binary that is still Planned writes its milestone to stderr and returns `ExitNotImplemented` (2) (`internal/gateway/gateway.go:17-27`); a Planned `ruralz` command does the same with `ExitNoResult`.
- Built-in Filters (M1) map from the Policy type: dots become slashes, hyphens drop (`auth.api-key` is `internal/filter/auth/apikey`); `headers` is `internal/filter/header`; `plugin` is `internal/pluginhost`.
- Wrapped libraries (depguard, `.golangci.yml:46-60`): wazero only in `internal/pluginhost`, rueidis only in `internal/statestore/redis`, `hashicorp/raft` only in `internal/controlstore/raft`.

## State, context, errors, output

- No `init()` and no mutable package-level variables. Tables and registries are functions returning fresh literals (`errcode.All()`, `repocheck.deniedTerms()`, `depgate` `policy.go`); compile regexps inside functions. Allowed exceptions: sentinel errors, `go:embed` variables, the linker-set `internal/buildinfo` variables, tests and generated code.
- `context.Context` is the first parameter of any function that blocks or calls the network or the State Store, and is never stored in a struct. Writing to an `io.Writer` or reading a local file does not need one. Name any unused parameter `_`, including `ctx` (revive). Use `exec.CommandContext` and context-aware HTTP (noctx, contextcheck).
- Every goroutine has an owner that cancels and waits for it; every channel, queue and pool is bounded.
- Wrap with `fmt.Errorf("...: %w", err)`; match with `errors.Is` and `errors.As` (errorlint). Messages are lower-case without a trailing period.
- Client-facing errors carry a registered `RZ-<AREA>-<NNN>` code. Take the area from the foundation pack section 8.6 table (CFG configuration, RT request handling before any Upstream, UP Upstream legs, AUTH, RL, PLG, AI, CP Ruralz Control, STS). When a Policy rejects a request, use STS if a State Store call failed under `failureMode: closed`, PLG if a Plugin trapped, otherwise the Policy type's area; a decision such as a denial never uses STS.
- Discard write errors explicitly: `_, _ = fmt.Fprintf(stderr, ...)`. Data goes to stdout, usage and errors to stderr.
- No logging exists yet. `log` is banned; `log/slog` arrives through `internal/telemetry` in M1 with metric names `ruralz_<component>_<name>_<unit>` (counters end `_total`) and spans `ruralz.filter.<name>`, `ruralz.upstream.<name>`, `ruralz.route.match`.
- JSON: the standard library (a third-party JSON module is a dependency like any other); camelCase tags (tagliatelle checks `pkg/config` and `internal/config`). No `unsafe`: no package is allow-listed for it.
- Switches over enums cover every value or have a `default` (exhaustive).

## gosec

gosec runs on production code and tests. `os.ReadFile` or `os.Open` on a non-constant path trips G304: put `//nolint:gosec // <why the path is trusted>` on that line, as `internal/errcode/errcode_test.go:87` does. Create directories with `0o750` or stricter (`internal/tool/schemagen/main.go:51`) and files with `0o600` or stricter (`internal/tool/repocheck/repocheck_test.go:70`); a file that must be world-readable keeps `0o644` with a reasoned suppression (`internal/tool/schemagen/main.go:59`). nolintlint rejects a directive that suppresses nothing, so add one only where gosec reports.

## Adding a dependency

A new module needs, in the same pull request: a row in `docs/engineering/01-tech-stack-and-libraries.md`, an allow entry in the depguard `admitted` rule of `.golangci.yml`, and passing depgate (`internal/tool/depgate/policy.go`). G2: Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause or ISC; MPL-2.0 only for `mplExceptions()` modules; EDL-1.0 only where `licenseElections()` elects it. G3: no `golang.org/x/crypto` package off the empty `cryptoDelegation()` list and no other import path containing `crypto` without a `cryptoExceptions()` row. `ruralzd` links no Raft module, and every module meets its `advisoryFloors()` minimum. `go.mod` edits need two approvals. Tools never go into `go.mod` as `tool` directives.

## Tests

- Tests live beside the code in the same package, use only the standard `testing` package (depguard blocks testify and go-cmp), and carry the license header.
- Style in this repo: table loops with `t.Errorf("Func(%q) = %v, want %v", ...)`, `t.Fatal` for setup failures, `t.Helper()` in helpers, `t.Context()`, `t.TempDir()`, `os.WriteFile(..., 0o600)`. Do not combine `t.Parallel()` with `t.Setenv`.
- Tests must pass under `-race -shuffle=on` with cgo and under `CGO_ENABLED=0`, and must not depend on order, wall-clock time, randomness or the network; inject time and randomness as fakes.
- Repository-wide invariant tests read the real working tree through relative paths: repocheck `TestRepositoryIsClean`, errcode `TestMatchesDocs`, schemagen `TestCommittedSchemaIsCurrent`, depgate `TestRepositoryPasses`, commitcheck `TestDocSlugs` and `TestRunUsage`. `TestRepositoryIsClean` scans untracked and gitignored files too (`tmp/` included), so a scratch file outside `testdata/`, `bin/`, `dist/` and dot-directories can break it.
- A fake `RZ` code in a test must be split so it is not one string literal (`internal/tool/repocheck/repocheck_test.go:27`); prefer a registered code.
- Every new package arrives with its tests. A bug found at a higher layer gets a regression test at the lowest layer that reproduces it. Coverage targets (80% per `internal/` package; 90% for loader, validation, precedence and canonical form) are not gated. Golden files go under `testdata/` with LF endings; fuzz targets and integration tests start in M1 (`docs/engineering/03-testing-and-quality-strategy.md`).

## Go version

`go.mod` says `go 1.26.0` and `toolchain go1.27.1`. The floor job builds and tests on Go 1.26 with `GOEXPERIMENT=jsonv2`, so do not use standard-library APIs added in Go 1.27. `internal/buildinfo/floor_guard.go` intentionally breaks a Go 1.26 build without `GOEXPERIMENT=jsonv2`; run `make floor FLOOR_GOTOOLCHAIN=go1.26.8` to check.
