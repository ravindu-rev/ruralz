# Contributing to Ruralz

Ruralz is Apache-2.0 with no feature gating: every contribution lands in the one public build ([ADR-0002](docs/adr/0002-apache-2-license-no-feature-gating.md)). The design lives in [docs/](docs/README.md); start with the Contributor reading path there. [Repository layout and conventions](docs/engineering/02-repository-layout-and-conventions.md) is the authoritative source for everything below.

## Sign off every commit (DCO)

Ruralz uses the [Developer Certificate of Origin 1.1](https://developercertificate.org/), not a CLA. Every commit carries a `Signed-off-by:` trailer whose name and email match the commit author:

```sh
git commit -s                  # adds the trailer
git rebase --signoff main      # repairs a branch
```

`make dco` checks every non-merge commit of a range, by default the commits of your branch that `origin/main` lacks; the PR pipeline runs it on every pull request. Dependabot commits are exempt because Dependabot cannot sign off.

## Commit titles

Pull requests merge with a merge commit, so every commit reaches `main` with its own title and sign-off, and merge only when the branch is up to date with `main` and the required `pr` check passes. Every commit title follows Conventional Commits: `<type>(<scope>): <subject>`, imperative mood, a lower-case first word, no trailing period. No check enforces the titles, but the release reads them: a merge to `main` releases a new version when its commits hold a breaking change, a `feat`, a `fix` or a `perf`, and their titles become the release notes ([Release pipeline](docs/engineering/04-release-versioning-and-compatibility.md#release-pipeline)). Add `!` after the scope and a `BREAKING CHANGE:` footer for a breaking change; `Refs: OQ-<docslug>-<n>` or `Refs: ADR-NNNN` footers link decisions. Announce each deprecated surface with a `Deprecated: <surface>; removed in MAJOR.MINOR.0` footer, at least two minor releases after the release that ships it ([Conventional Commits](docs/engineering/02-repository-layout-and-conventions.md#conventional-commits)).

| Type | Scopes |
|---|---|
| `feat`, `fix`, `perf`, `refactor` | `gateway`, `control`, `cli`, `config`, `filter`, `plugin`, `ai`, `statestore`, `controlstream`, `console`, `sdk`, `api`, `deploy` |
| `test` | The scopes above, `conformance` |
| `docs` | A document slug: the file name under `docs/` without its numeric prefix and extension, such as `docs(data-plane)` |
| `build`, `ci` | `deps`, `tools`, `workflows` |
| `chore`, `revert` | Any |

## Build and check locally

Go comes from the `go.mod` `toolchain` line: with the default `GOTOOLCHAIN=auto`, any Go 1.21 or newer downloads it. The Makefile needs bash, git and curl.

```sh
make help                                       # list targets
make tools                                      # pinned golangci-lint, govulncheck, actionlint, shellcheck, buf, its protoc plugins and promtool into bin/
make lint dco supply-chain test integration     # what the PR pipeline runs
```

| Target | PR pipeline job | What it runs |
|---|---|---|
| `make lint` | `ci` | golangci-lint with `.golangci.yml`, gofumpt and goimports, actionlint on the workflows, and the buf rules checks: `buf format`, `buf lint`, `buf breaking` against `BREAKING_MAIN` (default `origin/main`) and the newest release tags, and the committed protobuf code against `buf generate` |
| `make dco` | `ci` | The DCO check of every non-merge commit of `DCO_RANGE` (default `origin/main..HEAD`) |
| `make supply-chain` | `ci` | `go mod verify`, govulncheck, and `depgate`: license gate G2, crypto denylist G3, the `ruralzd` Raft and `ruralz` jwx denylists, no test support in a binary, and advisory version floors |
| `make test` | `unit` | `go test -race -shuffle=on` with cgo, then the tests again with `CGO_ENABLED=0`, including the property tests, the fuzz seed corpora and `./test/conformance/config` |
| `make integration` | `integration` | The integration tests (tag `integration`): the conformance suites and the State Store, CLI and Node suites, with promtool's alert rule tests; needs a local `redis-server`, and Docker for the State Store container kinds |
| `make generate` | None | `go generate ./...` and `buf generate`, then fails if the generated JSON Schema, served-feature table, alert rules, Grafana dashboards (`deploy/grafana`) or protobuf code (`internal/gen/proto`) differ from the index |
| `make build` | None | Every binary for linux/amd64 and linux/arm64, the CLI also for darwin and windows, with `CGO_ENABLED=0`, `-trimpath` and `-s -w` into `dist/` |
| `make release-dry-run`, `make release` | None: the release | Release packaging without signing, then with keyless signing |

## Code conventions

- New Go code starts in `internal/`; `cmd/<binary>/main.go` holds wiring only. `pkg/` and `api/schema` import only the standard library and `pkg/`.
- Every hand-written Go, proto and TypeScript file starts with `Copyright <creation year> <copyright holder>` (Revington for Revington's work) and `SPDX-License-Identifier: Apache-2.0`; a proto file carries them as the trailing comment of its `syntax` statement, so its generated Go code starts with the `Code generated` line.
- The protos under `api/proto/` generate the Go code under `internal/gen/proto/`: change a proto, run `make generate`, and commit both.
- Client-facing errors carry an `RZ-<AREA>-<NNN>` code registered in `internal/errcode`.
- The JSON Schema under `api/schema/` is generated from the Go types in `pkg/config/v1alpha1`: change the types and their `+ruralz:` markers, run `make generate`, and commit both.
- No `init()`, no mutable package-level variables, no `import "C"`, and no `fmt.Print*` outside `internal/cli`.
- A new dependency needs a row in the [Tech stack catalog](docs/engineering/01-tech-stack-and-libraries.md) and an entry in the depguard allow list of `.golangci.yml` in the same pull request.

## Pull requests

- A pull request brings its tests, regenerated files and documentation with it.
- The one required check is `pr`, the last job of the PR pipeline, which passes once its `ci`, `unit` and `integration` jobs passed.
- A design change updates the owning document in `docs/` in the same pull request.

## Reporting security issues

Never report a vulnerability in a public issue; follow [SECURITY.md](SECURITY.md).
