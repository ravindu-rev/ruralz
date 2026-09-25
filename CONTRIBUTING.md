# Contributing to Ruralz

Ruralz is Apache-2.0 with no feature gating: every contribution lands in the one public build ([ADR-0002](docs/adr/0002-apache-2-license-no-feature-gating.md)). The design lives in [docs/](docs/README.md); start with the Contributor reading path there. [Repository layout and conventions](docs/engineering/02-repository-layout-and-conventions.md) is the authoritative source for everything below.

## Sign off every commit (DCO)

Ruralz uses the [Developer Certificate of Origin 1.1](https://developercertificate.org/), not a CLA. Every commit carries a `Signed-off-by:` trailer whose name and email match the commit author:

```sh
git commit -s                  # adds the trailer
git rebase --signoff main      # repairs a branch
```

CI checks every non-merge commit of a pull request, also on merge queue runs, bots included.

## Pull request titles

Pull requests are squash-merged, so the title becomes the commit subject. It follows Conventional Commits: `<type>(<scope>): <subject>`, imperative mood, a lower-case first word, no trailing period. Add `!` after the scope and a `BREAKING CHANGE:` footer for a breaking change; `Refs: OQ-<docslug>-<n>` or `Refs: ADR-NNNN` footers link decisions.

| Type | Scopes |
|---|---|
| `feat`, `fix`, `perf`, `refactor` | `gateway`, `control`, `cli`, `config`, `filter`, `plugin`, `ai`, `statestore`, `controlstream`, `console`, `sdk`, `api`, `deploy` |
| `test` | The scopes above, `e2e`, `conformance` |
| `docs` | A document slug, such as `docs(data-plane)` |
| `build`, `ci` | `deps`, `tools`, `workflows` |
| `chore`, `revert` | Any |

## Build and check locally

Go comes from the `go.mod` `toolchain` line: with the default `GOTOOLCHAIN=auto`, any Go 1.21 or newer downloads it. The Makefile needs bash, git and curl.

```sh
make help                                            # list targets
make tools                                           # pinned golangci-lint and govulncheck into bin/
make hygiene lint generate build test supply-chain   # what CI stages 1 to 6 run
make floor FLOOR_GOTOOLCHAIN=go1.26.8                # the Go 1.26 floor job, with GOEXPERIMENT=jsonv2
```

| Target | CI stage | What it runs |
|---|---|---|
| `make hygiene` | 1 | `repocheck`, including the no-license-check scan; with `DCO_RANGE` or `PR_TITLE` set, the DCO and title checks |
| `make lint` | 2 | gofumpt, goimports and golangci-lint with `.golangci.yml` |
| `make generate` | 3 | `go generate ./...`, then fails if the generated JSON Schema differs |
| `make build` | 4 | Every binary for linux/amd64 and linux/arm64, the CLI also for darwin and windows, with `CGO_ENABLED=0` and `-trimpath` into `dist/` |
| `make test` | 5 | `go test -race -shuffle=on` with cgo, then the tests again with `CGO_ENABLED=0` |
| `make supply-chain` | 6 | `go mod verify`, govulncheck, and `depgate`: license gate G2, crypto denylist G3, the `ruralzd` Raft denylist and advisory version floors |

## Code conventions

- New Go code starts in `internal/`; `cmd/<binary>/main.go` holds wiring only. `pkg/` and `api/schema` import only the standard library and `pkg/`.
- Every hand-written Go, proto and TypeScript file starts with `Copyright <creation year> <copyright holder>` (Revington for Revington's work) and `SPDX-License-Identifier: Apache-2.0`.
- Client-facing errors carry an `RZ-<AREA>-<NNN>` code registered in `internal/errcode`.
- The JSON Schema under `api/schema/` is generated from the Go types in `pkg/config/v1alpha1`: change the types and their `+ruralz:` markers, run `make generate`, and commit both.
- No `init()`, no mutable package-level variables, no `import "C"`, and no `fmt.Print*` outside `internal/cli`.
- A new dependency needs a row in the [Tech stack catalog](docs/engineering/01-tech-stack-and-libraries.md) and an entry in the depguard allow list of `.golangci.yml` in the same pull request.

## Pull requests

- Keep a pull request under 400 changed lines, excluding generated files (target), and bring its tests, regenerated files and documentation with it.
- One CODEOWNERS approval is required; changes to `api/`, `pkg/`, `docs/_meta/`, `go.mod`, security-sensitive packages, the no-license-check allowlist or workflows need two.
- The required checks are `pr-fast` (stages 1 to 6), `pr-title` and `approvals`.
- A design change that contradicts a document needs an ADR or an entry in that document's Open questions.

## Reporting security issues

Never report a vulnerability in a public issue; follow [SECURITY.md](SECURITY.md).
