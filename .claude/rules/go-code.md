---
paths:
  - "**/*.go"
  - "go.mod"
  - ".golangci.yml"
---

# Go code in Ruralz

Authority: `docs/engineering/02-repository-layout-and-conventions.md`. The hard rules in `AGENTS.md` apply first.

## File shape

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Package x does one thing, stated in a full sentence.
package x
```

- A build constraint goes after the header and a blank line. Build tags are only platform, Go version, `goexperiment`, and `integration` or `e2e` on `_test.go` files; never feature tags.
- Packages are lower-case, singular, no underscores, never `util`, `common` or `misc`. Files are `lower_snake.go` with tests beside them.
- Imports: standard library, third-party, then `github.com/ravindu-rev/ruralz/...`, groups separated by a blank line (`make fmt`: goimports, then gofumpt).
- Doc comments are full sentences starting with the identifier; exported identifiers are documented. American spelling (`license`, never `licence`). Comments explain why.

## Where code goes

- New code starts in `internal/`. Moving code to `pkg/` makes it a SemVer promise: ask the owner first.
- Each binary's `internal/<pkg>` exposes `Run(ctx context.Context, args []string, stdout, stderr io.Writer) int`; `cmd/<binary>/main.go` only wires it. Parse flags inside `internal/` with a `flag.FlagSet`.
- Exit codes are named constants that never change once released (`ExitOK = 0`, `ExitNegative = 1`, `ExitNoResult = 2` in `internal/cli`). A binary or command that is still Planned writes its milestone to stderr and returns 2.
- Built-in Filters map from the Policy type: dots become slashes, hyphens drop (`auth.api-key` is `internal/filter/auth/apikey`); `headers` is `internal/filter/header`; `plugin` is `internal/pluginhost`.
- Wrapped libraries stay in their package (depguard confinement rules in `.golangci.yml`): wazero in `internal/pluginhost`, rueidis in `internal/statestore/redis`, cel-go in `internal/cel`, jwx in `internal/filter/auth/jwt`, OpenTelemetry in `internal/telemetry` and `internal/testkit/otlpsink`, `hashicorp/raft` in `internal/controlstore/raft`, `go.yaml.in/yaml/v3` in `internal/config/profile` tests (its differential oracle).

## State, context, errors, output

- No `init()` and no mutable package-level variables. Tables and registries are functions returning fresh literals (`errcode.All()`); compile regexps inside functions.
- `context.Context` is the first parameter of any function that blocks or calls the network or the State Store, and is never stored in a struct. Name unused parameters `_`. Use `exec.CommandContext` and context-aware HTTP.
- Every goroutine has an owner that cancels and waits for it; every channel, queue and pool is bounded.
- Wrap with `fmt.Errorf("...: %w", err)`; match with `errors.Is` and `errors.As`. Messages are lower-case without a trailing period.
- Client-facing errors carry a registered `RZ-<AREA>-<NNN>` code, area from foundation pack section 8.6. A Policy rejection uses STS when a State Store call failed under `failureMode: closed`, PLG when a Plugin trapped, otherwise the Policy type's area; a denial never uses STS.
- Discard write errors explicitly: `_, _ = fmt.Fprintf(stderr, ...)`. Data goes to stdout, usage and errors to stderr.
- Process logs use `log/slog` through `internal/telemetry/logsink` with keys from `internal/telemetry/catalog`. Metric names are `ruralz_<component>_<name>_<unit>` (counters end `_total`); spans `ruralz.filter.<name>`, `ruralz.upstream.<name>`, `ruralz.route.match`.
- JSON uses the standard library, camelCase tags. No `unsafe`.
- Switches over enums cover every value or have a `default`.

## gosec

gosec runs on production code and tests. `os.ReadFile` or `os.Open` on a non-constant path trips G304: put `//nolint:gosec // <why the path is trusted>` on that line. Directories `0o750` or stricter, files `0o600` or stricter; a file that must be world-readable keeps `0o644` with a reasoned suppression. nolintlint rejects a directive that suppresses nothing.

## Adding a dependency

A new module needs, in the same change, a row in `docs/engineering/01-tech-stack-and-libraries.md`, an `admitted` entry in `.golangci.yml`, and passing depgate (`internal/tool/depgate/policy.go`): license Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause or ISC (MPL-2.0 only for `mplExceptions()`), no crypto package outside the depgate lists, no Raft module in `ruralzd`, and every module at its `advisoryFloors()` minimum. Tools never enter `go.mod` as `tool` directives.

## Tests

- Tests live beside the code in the same package, use only the standard `testing` package and carry the license header.
- Style: table tests with named cases, `t.Errorf("Func(%q) = %v, want %v", ...)`, `t.Fatal` for setup failures, `t.Helper()`, `t.Context()`, `t.TempDir()`, `os.WriteFile(..., 0o600)`. Do not combine `t.Parallel()` with `t.Setenv`.
- Inject time and randomness as fakes.
- Repository-wide tests read the real tree: repocheck `TestRepositoryIsClean` (scans untracked files too, so keep scratch files out of the tree), errcode `TestMatchesDocs`, schemagen `TestCommittedSchemaIsCurrent`, depgate `TestRepositoryPasses`, commitcheck `TestDocSlugs` and `TestRunUsage`, registry tests that parse foundation pack section 10.
- A fake `RZ` code in a test is split so it is not one string literal (`"RZ-RT-" + "999"`); prefer a registered code.
- Golden files go under `testdata/` with LF endings.

## Go version

`go.mod` says `go 1.26.0` and a `toolchain` line. The floor job builds and tests on Go 1.26 with `GOEXPERIMENT=jsonv2`, so never use standard library APIs added in Go 1.27; `internal/buildinfo/floor_guard.go` breaks a Go 1.26 build without `GOEXPERIMENT=jsonv2` on purpose.
