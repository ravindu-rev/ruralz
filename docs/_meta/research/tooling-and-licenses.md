# Tooling and Dependency License Facts

| Field | Value |
|---|---|
| Topic | Documentation, build and supply-chain tooling (MADR, markdownlint-cli2, lychee, mermaid, js-yaml, buf, golangci-lint, GoReleaser, cosign and sigstore-go, Syft, oras-go); exact licenses of the Control Store module graph (hashicorp/raft, raft-boltdb/v2, bbolt, go-msgpack/v2, go-hclog and the rest of raft's imports); the wazero repository versus module path; the `golang.org/x/net/http2` deprecation; YAML 1.2 parser and JSON Schema draft 2020-12 validator candidates; other unselected library candidates |
| Snapshot date | 2026-09-23 |
| Requested by | Tech-stack escalations L1-2, L2-1, L3-1 (OQ-tech-stack-and-libraries-5) and L1-3, L2-2, L3-2 (OQ-tech-stack-and-libraries-10); OQ-tech-stack-and-libraries-12 and -13; OQ-configuration-model-16; research amendments in `docs/_meta/reviews/foundation-b.amendments.json` |
| Method | Primary sources only, read on 2026-09-23: GitHub REST API through `gh api` (repository license detection, `LICENSE` file text at the pinned tag, release and tag metadata, `go.mod` files, Go source import blocks), the npm registry JSON (`registry.npmjs.org`), the Go module proxy (`proxy.golang.org`, including one module zip), project READMEs and release notes, the Apache License 2.0 text, go.dev release notes and pkg.go.dev. Every license below was checked against the `LICENSE` text, not only the GitHub badge; where GitHub reports `NOASSERTION` the text was read and the result is stated. Release dates are the GitHub release `published_at` (UTC) or npm publish time unless marked "tag date". No Go toolchain was available, so `go list -deps` and `go mod graph` were not run; the import analysis in section 3 comes from reading source files (see Gaps). |

## 1. Documentation tooling (OQ-tech-stack-and-libraries-5)

None of these tools is linked into `ruralzd`, `ruralz-control` or `ruralz`; they run in CI or on contributor machines.

| Tool | Repository | License (SPDX) | LICENSE file | Latest version (date) | Source |
|---|---|---|---|---|---|
| MADR (Markdown Architectural Decision Records) | https://github.com/adr/madr; site https://adr.github.io/madr/ | `MIT OR CC0-1.0` | (https://github.com/adr/madr/blob/develop/LICENSE) (https://github.com/adr/madr/blob/develop/LICENSE.MIT) (https://github.com/adr/madr/blob/develop/LICENSE.CC0-1.0) | 4.0.0 (2024-09-17) | (https://github.com/adr/madr/releases/tag/4.0.0) (https://github.com/adr/madr/blob/develop/package.json) |
| markdownlint-cli2 | https://github.com/DavidAnson/markdownlint-cli2 | `MIT` | (https://github.com/DavidAnson/markdownlint-cli2/blob/main/LICENSE) | 0.23.3 (2026-09-20) | (https://registry.npmjs.org/markdownlint-cli2) (https://github.com/DavidAnson/markdownlint-cli2/tags) |
| markdownlint (rule library used by markdownlint-cli2) | https://github.com/DavidAnson/markdownlint | `MIT` | (https://github.com/DavidAnson/markdownlint/blob/main/LICENSE) | 0.41.1 (2026-07-13) | (https://registry.npmjs.org/markdownlint) |
| lychee (link checker) | https://github.com/lycheeverse/lychee | `Apache-2.0 OR MIT` | (https://github.com/lycheeverse/lychee/blob/master/LICENSE-APACHE) (https://github.com/lycheeverse/lychee/blob/master/LICENSE-MIT) | lychee-v0.24.2 (2026-05-01); a rolling `nightly` prerelease was republished 2026-09-21 | (https://github.com/lycheeverse/lychee/releases/tag/lychee-v0.24.2) (https://github.com/lycheeverse/lychee/blob/master/lychee-bin/Cargo.toml) |
| mermaid | https://github.com/mermaid-js/mermaid | `MIT` | (https://github.com/mermaid-js/mermaid/blob/develop/LICENSE) | 12.0.0 (2026-09-10); newest 11.x is 11.17.2 (2026-08-25) | (https://registry.npmjs.org/mermaid) (https://github.com/mermaid-js/mermaid/releases/tag/mermaid%4012.0.0) |
| @mermaid-js/parser | https://github.com/mermaid-js/mermaid (`packages/parser`) | `MIT` | (https://github.com/mermaid-js/mermaid/blob/develop/LICENSE) | 2.0.0 (2026-09-10) | (https://registry.npmjs.org/@mermaid-js/parser) |
| @mermaid-js/tiny | https://github.com/mermaid-js/mermaid | `MIT` | (https://github.com/mermaid-js/mermaid/blob/develop/LICENSE) | 12.0.0 (2026-09-10) | (https://registry.npmjs.org/@mermaid-js/tiny) |
| @mermaid-js/layout-elk | https://github.com/mermaid-js/mermaid | `MIT` | (https://github.com/mermaid-js/mermaid/blob/develop/LICENSE) | 1.0.0 (2026-09-10) | (https://registry.npmjs.org/@mermaid-js/layout-elk) |
| @mermaid-js/mermaid-zenuml | https://github.com/mermaid-js/mermaid (`packages/mermaid-zenuml`) | `MIT` | (https://github.com/mermaid-js/mermaid/blob/develop/LICENSE) | 1.0.1 (2026-09-18) | (https://registry.npmjs.org/@mermaid-js/mermaid-zenuml) |
| @mermaid-js/mermaid-cli (`mmdc`) | https://github.com/mermaid-js/mermaid-cli | `MIT` | (https://github.com/mermaid-js/mermaid-cli/blob/master/LICENSE) | 11.17.0 (2026-09-02) | (https://registry.npmjs.org/@mermaid-js/mermaid-cli) |
| js-yaml | https://github.com/nodeca/js-yaml | `MIT` | (https://github.com/nodeca/js-yaml/blob/master/LICENSE) | 5.4.2 (2026-09-13); 4.3.2 (2026-08-26) is the newest 4.x installed here | (https://registry.npmjs.org/js-yaml) (https://github.com/nodeca/js-yaml/blob/master/CHANGELOG.md) |

- The MADR site states the work is "dual-licensed under MIT and CC0" and that users "can choose between one of them"; it presents 4.0.0 (2024-09-17) as current, with "bare" and "minimal" template variants (https://adr.github.io/madr/).
- MADR's root `LICENSE` file contains only the SPDX expression `MIT OR CC0-1.0`; the full texts are in `LICENSE.MIT` and `LICENSE.CC0-1.0`, and `package.json` declares `"license": "MIT OR CC0-1.0"` (https://github.com/adr/madr/blob/develop/LICENSE) (https://github.com/adr/madr/blob/develop/package.json).
- lychee's `README.md` says it is "licensed under either of" Apache-2.0 or MIT, and `lychee-bin/Cargo.toml` declares `license = "Apache-2.0 OR MIT"` (https://github.com/lycheeverse/lychee/blob/master/README.md) (https://github.com/lycheeverse/lychee/blob/master/lychee-bin/Cargo.toml).
- mermaid 12.0.0 "is a breaking release (ES2024, Safari 17.4+, Node 22.12+)", and existing flowcharts "will re-lay out and recolour" unless `layout: dagre`, `theme: default` and `look: classic` are set (https://github.com/mermaid-js/mermaid/releases/tag/mermaid%4012.0.0).
- The npm `dist-tags` for mermaid on 2026-09-23 are `latest` 12.0.0 and `backport` 10.9.8 (https://registry.npmjs.org/mermaid).
- js-yaml 5.4.0 (2026-08-25) lists `[breaking]` changes to the low-level AST node style representation and the `sortKeys` option (https://github.com/nodeca/js-yaml/blob/master/CHANGELOG.md).
- Repository state (not a web source): `scripts/package.json` declares `js-yaml` `^4.1.0`, `mermaid` `^11.12.0` as an optional dependency, and `engines.node` `>=20`; `scripts/node_modules` holds mermaid 11.17.2 and js-yaml 4.3.2. `scripts/verify-docs.mjs` runs `npx --yes markdownlint-cli2` with no version pin, so it resolves to the npm `latest` tag (0.23.3 on 2026-09-23).
- Analysis: the `^11.12.0` and `^4.1.0` ranges keep the docs gate on mermaid 11.x and js-yaml 4.x. Moving to mermaid 12 would need Node 22.12 or newer, above the declared `>=20` floor.

## 2. Build, release and supply-chain tooling

| Tool | Repository (Go module) | License (SPDX) | LICENSE file | Latest version (date) | Source |
|---|---|---|---|---|---|
| buf | https://github.com/bufbuild/buf | `Apache-2.0` | (https://github.com/bufbuild/buf/blob/main/LICENSE) | v1.73.0 (2026-09-11) | (https://github.com/bufbuild/buf/releases/tag/v1.73.0) |
| golangci-lint | https://github.com/golangci/golangci-lint | `GPL-3.0` | (https://github.com/golangci/golangci-lint/blob/main/LICENSE) | v2.13.2 (2026-08-27) | (https://github.com/golangci/golangci-lint/releases/tag/v2.13.2) |
| GoReleaser (OSS) | https://github.com/goreleaser/goreleaser | `MIT` | (https://github.com/goreleaser/goreleaser/blob/main/LICENSE.md) | v2.18.2 (2026-09-17) | (https://github.com/goreleaser/goreleaser/releases/tag/v2.18.2) |
| cosign | https://github.com/sigstore/cosign (`github.com/sigstore/cosign/v3`) | `Apache-2.0` | (https://github.com/sigstore/cosign/blob/main/LICENSE) | v3.1.3 (2026-08-06); v2 line: v2.6.5 (2026-08-06) | (https://github.com/sigstore/cosign/releases/tag/v3.1.3) (https://github.com/sigstore/cosign/releases/tag/v2.6.5) (https://github.com/sigstore/cosign/blob/v3.1.3/go.mod) |
| sigstore-go | https://github.com/sigstore/sigstore-go | `Apache-2.0` | (https://github.com/sigstore/sigstore-go/blob/main/LICENSE) | v1.3.0 (2026-07-30) | (https://github.com/sigstore/sigstore-go/releases/tag/v1.3.0) |
| Syft | https://github.com/anchore/syft | `Apache-2.0` | (https://github.com/anchore/syft/blob/main/LICENSE) | v1.52.0 (2026-09-17) | (https://github.com/anchore/syft/releases/tag/v1.52.0) |
| oras-go | https://github.com/oras-project/oras-go (`oras.land/oras-go/v2`) | `Apache-2.0` | (https://github.com/oras-project/oras-go/blob/main/LICENSE) | v2.6.2 (2026-07-10) | (https://github.com/oras-project/oras-go/releases/tag/v2.6.2) (https://github.com/oras-project/oras-go/blob/v2.6.2/go.mod) |

- cosign's `LICENSE` at v3.1.3 is the Apache License 2.0 text; the GitHub API reports `NOASSERTION` for that tag because of extra content, so the SPDX value above comes from the text (https://github.com/sigstore/cosign/blob/v3.1.3/LICENSE).
- golangci-lint is GPL-3.0, both at v2.13.2 and on `main` (https://github.com/golangci/golangci-lint/blob/v2.13.2/LICENSE).
- Go 1.24 added `tool` directives, so `go.mod` can "track executable dependencies" and `go get -tool` adds the `require` directives too (https://go.dev/doc/go1.24).
- Analysis: running golangci-lint in CI does not distribute it, so tech-stack gate G2 (a license gate "over the whole module graph") is not triggered as long as the binary is installed separately. If golangci-lint were added as a `tool` directive in the Ruralz `go.mod`, its GPL-3.0 module and its dependencies would join the module graph and fail that gate. Install it as a pinned binary or GitHub Action instead.
- The oras-go floor in the tech-stack catalog (v2.6.2 or newer) matches the newest release (https://github.com/oras-project/oras-go/releases/tag/v2.6.2). The sigstore-go catalog floor v1.3.x matches v1.3.0 (https://github.com/sigstore/sigstore-go/releases/tag/v1.3.0).

## 3. Control Store module licenses (OQ-tech-stack-and-libraries-10)

### 3.1 Direct modules and their LICENSE files

| Module | Version | License (SPDX) | LICENSE file at that version | Latest release (date) | Source |
|---|---|---|---|---|---|
| `github.com/hashicorp/raft` | v1.8.0 | `MPL-2.0` | (https://github.com/hashicorp/raft/blob/v1.8.0/LICENSE) | v1.8.0 (2026-09-16) | (https://github.com/hashicorp/raft/releases/tag/v1.8.0) |
| `github.com/hashicorp/raft-boltdb/v2` | v2.4.2 | `MPL-2.0` | (https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/LICENSE) | v2.4.2 (2026-09-16) | (https://github.com/hashicorp/raft-boltdb/releases/tag/v2.4.2) (https://proxy.golang.org/github.com/hashicorp/raft-boltdb/v2/@v/v2.4.2.zip) |
| `go.etcd.io/bbolt` | v1.4.1 (required by raft-boltdb/v2 v2.4.2) | `MIT` | (https://github.com/etcd-io/bbolt/blob/v1.4.1/LICENSE) | v1.5.0 (2026-06-21); v1.4.1 was published 2025-06-10 | (https://github.com/etcd-io/bbolt/releases/tag/v1.5.0) (https://github.com/etcd-io/bbolt/releases/tag/v1.4.1) (https://github.com/etcd-io/bbolt/blob/v1.5.0/LICENSE) |
| `github.com/hashicorp/go-msgpack/v2` | v2.1.5 (required by raft v1.8.0 and raft-boltdb/v2 v2.4.2) | `MIT` | (https://github.com/hashicorp/go-msgpack/blob/v2.1.5/LICENSE) | v2.1.5 (2025-09-24) | (https://github.com/hashicorp/go-msgpack/releases/tag/v2.1.5) |
| `github.com/hashicorp/go-hclog` | v1.6.3 (required by raft v1.8.0) | `MIT` | (https://github.com/hashicorp/go-hclog/blob/v1.6.3/LICENSE) | v1.6.3 (2024-04-01) | (https://github.com/hashicorp/go-hclog/releases/tag/v1.6.3) |

- The `v2/` directory of raft-boltdb has no `LICENSE` of its own (it holds `README.md`, `go.mod`, `bolt_store.go`, `util.go`, `metrics.go` and tests); the repository-root `LICENSE` is MPL-2.0 (https://github.com/hashicorp/raft-boltdb/tree/v2.4.2/v2) (https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/LICENSE).
- The module zip served by the Go proxy for `github.com/hashicorp/raft-boltdb/v2@v2.4.2` contains `LICENSE` with the text "Mozilla Public License, version 2.0", so license scanners reading the module zip see MPL-2.0 (https://proxy.golang.org/github.com/hashicorp/raft-boltdb/v2/@v/v2.4.2.zip).
- go-msgpack/v2's MIT license text carries the copyright line "Copyright (c) 2012-2015 Ugorji Nwoke" (https://github.com/hashicorp/go-msgpack/blob/v2.1.5/LICENSE).
- raft v1.8.0 `go.mod` requires `go-hclog v1.6.3`, `go-metrics v0.7.0` and `go-msgpack/v2 v2.1.5`, and lists `go-immutable-radix v1.3.1`, `golang-lru v1.0.2`, `fatih/color`, `mattn/go-colorable`, `mattn/go-isatty` and `golang.org/x/sys` as indirect (https://github.com/hashicorp/raft/blob/v1.8.0/go.mod).
- raft-boltdb/v2 v2.4.2 `go.mod` requires `github.com/boltdb/bolt v1.3.1`, `go-metrics v0.7.0`, `go-msgpack/v2 v2.1.5`, `raft v1.8.0`, the v1 module `github.com/hashicorp/raft-boltdb v0.0.0-20230125174641-2a8082862702` and `go.etcd.io/bbolt v1.4.1` (https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/v2/go.mod).

### 3.2 Modules imported by the non-test code of raft and raft-boltdb/v2

| Module | Version in the requiring `go.mod` | Imported by (non-test file) | License (SPDX) | LICENSE file | Source |
|---|---|---|---|---|---|
| `github.com/hashicorp/go-metrics` | v0.7.0 (released 2026-09-16) | raft `api.go`; raft-boltdb/v2 `bolt_store.go`, `metrics.go` | `MIT` | (https://github.com/hashicorp/go-metrics/blob/v0.7.0/LICENSE) | (https://github.com/hashicorp/raft/blob/v1.8.0/api.go) (https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/v2/bolt_store.go) |
| `github.com/hashicorp/go-immutable-radix` | v1.3.1 (released 2021-06-28) | go-metrics `metrics.go` (`iradix`) | `MPL-2.0` | (https://github.com/hashicorp/go-immutable-radix/blob/v1.3.1/LICENSE) | (https://github.com/hashicorp/go-metrics/blob/v0.7.0/metrics.go) (https://github.com/hashicorp/go-metrics/blob/v0.7.0/go.mod) |
| `github.com/hashicorp/golang-lru` | v1.0.2 (released 2023-08-08) | go-immutable-radix `iradix.go` (`golang-lru/simplelru`) | `MPL-2.0` | (https://github.com/hashicorp/golang-lru/blob/v1.0.2/LICENSE) | (https://github.com/hashicorp/go-immutable-radix/blob/v1.3.1/iradix.go) |
| `github.com/boltdb/bolt` | v1.3.1 (tagged 2017-07-17; repository archived) | raft-boltdb/v2 `bolt_store.go` (`MigrateToV2`) | `MIT` | (https://github.com/boltdb/bolt/blob/v1.3.1/LICENSE) | (https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/v2/bolt_store.go) (https://github.com/boltdb/bolt) |
| `github.com/fatih/color` | v1.13.0 (raft) / v1.19.0 (raft-boltdb/v2) | go-hclog | `MIT` | (https://github.com/fatih/color/blob/v1.19.0/LICENSE.md) | (https://github.com/hashicorp/go-hclog/blob/v1.6.3/go.mod) |
| `github.com/mattn/go-colorable` | v0.1.12 / v0.1.15 | go-hclog | `MIT` | (https://github.com/mattn/go-colorable/blob/v0.1.15/LICENSE) | (https://github.com/hashicorp/go-hclog/blob/v1.6.3/go.mod) |
| `github.com/mattn/go-isatty` | v0.0.14 / v0.0.24 | go-hclog | `MIT` | (https://github.com/mattn/go-isatty/blob/v0.0.24/LICENSE) | (https://github.com/hashicorp/go-hclog/blob/v1.6.3/go.mod) |
| `golang.org/x/sys` | v0.47.0 / v0.48.0 | go-isatty, bbolt | `BSD-3-Clause` (GitHub reports `NOASSERTION`; the text is the Go Authors BSD 3-clause license) | (https://github.com/golang/sys/blob/v0.48.0/LICENSE) | (https://github.com/etcd-io/bbolt/blob/v1.4.1/go.mod) |

- raft-boltdb/v2 `bolt_store.go` imports `v1 "github.com/boltdb/bolt"`, `github.com/hashicorp/go-metrics`, `github.com/hashicorp/raft` and `go.etcd.io/bbolt`; `util.go` imports `github.com/hashicorp/go-msgpack/v2/codec` (https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/v2/bolt_store.go) (https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/v2/util.go).
- `MigrateToV2(source, destination string)` in `bolt_store.go` opens the source with `v1.Open`, which is why the archived `boltdb/bolt` module is linked (https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/v2/bolt_store.go).
- The GitHub repository `boltdb/bolt` is archived; its last push was 2018-03-02 (https://github.com/boltdb/bolt).
- go-metrics v0.7.0 `metrics.go` imports `iradix "github.com/hashicorp/go-immutable-radix"`, and go-immutable-radix v1.3.1 `iradix.go` imports `github.com/hashicorp/golang-lru/simplelru` (https://github.com/hashicorp/go-metrics/blob/v0.7.0/metrics.go) (https://github.com/hashicorp/go-immutable-radix/blob/v1.3.1/iradix.go).

### 3.3 Modules reached only through tests or sub-packages

| Module | Why it appears | License (SPDX) | Source |
|---|---|---|---|
| `github.com/hashicorp/raft-boltdb` (v1, pseudo-version `2a8082862702`) | Imported only by raft-boltdb/v2 `bolt_store_test.go` | `MPL-2.0` | (https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/v2/bolt_store_test.go) (https://github.com/hashicorp/raft-boltdb/blob/2a8082862702/LICENSE) |
| `github.com/armon/go-metrics` v0.4.1 | Indirect in raft-boltdb/v2 `go.mod`; imported by the v1 `bolt_store.go` | `MIT` (repository now redirects to hashicorp/go-metrics) | (https://github.com/hashicorp/raft-boltdb/blob/2a8082862702/bolt_store.go) (https://github.com/hashicorp/go-metrics/blob/v0.4.1/LICENSE) |
| `github.com/hashicorp/go-msgpack` v0.5.5 (v1) | Indirect in raft-boltdb/v2 `go.mod` | `BSD-3-Clause` | (https://github.com/hashicorp/go-msgpack/blob/v0.5.5/LICENSE) |
| `github.com/hashicorp/go-cleanhttp`, `github.com/hashicorp/go-retryablehttp` | In go-metrics v0.7.0 `go.mod` (indirect, for the Circonus sink sub-package); not imported by the root `go-metrics` package | `MPL-2.0` (license at `HEAD`, not at the pinned version) | (https://github.com/hashicorp/go-metrics/blob/v0.7.0/go.mod) (https://github.com/hashicorp/go-cleanhttp/blob/master/LICENSE) (https://github.com/hashicorp/go-retryablehttp/blob/main/LICENSE) |
| `github.com/DataDog/datadog-go`, `github.com/circonus-labs/circonus-gometrics`, `github.com/circonus-labs/circonusllhist`, `github.com/prometheus/client_golang`, `client_model`, `common`, `procfs`, `github.com/Microsoft/go-winio`, `github.com/beorn7/perks`, `github.com/cespare/xxhash/v2`, `github.com/munnerz/goautoneg`, `github.com/pkg/errors`, `github.com/tv42/httpunix` | In go-metrics v0.7.0 `go.mod` for its sink sub-packages | `MIT`, `BSD-3-Clause`, `Apache-2.0` (circonusllhist: Apache-2.0 header text, GitHub `NOASSERTION`), `Apache-2.0` x4, `MIT`, `MIT`, `MIT`, `BSD-3-Clause` (goautoneg: 3-clause text, GitHub `NOASSERTION`), `BSD-2-Clause`, `MIT` respectively (licenses at `HEAD`) | (https://github.com/hashicorp/go-metrics/blob/v0.7.0/go.mod) (https://github.com/openhistogram/circonusllhist/blob/master/LICENSE) (https://github.com/munnerz/goautoneg/blob/master/LICENSE) |
| Benchmark codecs in go-msgpack/v2 (`Sereal`, `go-xdr`, `json-iterator`, `easyjson`, `ffjson`, `msgp`, `mgo.v2`, `vmihailenco/msgpack.v2`) | In go-msgpack/v2 v2.1.5 `go.mod`; the module has a `codec/bench` directory | Not checked | (https://github.com/hashicorp/go-msgpack/blob/v2.1.5/go.mod) (https://github.com/hashicorp/go-msgpack/tree/v2.1.5/codec) |

### 3.4 What this means for the MPL-2.0 exception table (analysis)

- **Confirmed MPL-2.0 and linked:** `hashicorp/raft`, `hashicorp/raft-boltdb/v2`, `hashicorp/go-immutable-radix` and `hashicorp/golang-lru`. The last two are not named in the tech-stack document today; they come in through go-metrics, which raft cannot drop.
- **Permissive, so their provisional exception rows can go:** `hashicorp/go-msgpack/v2` (MIT), `go.etcd.io/bbolt` (MIT), `hashicorp/go-hclog` (MIT) and `hashicorp/go-metrics` (MIT).
- **Needs an S1 or health note:** the archived `boltdb/bolt` v1.3.1 (MIT) is linked through `MigrateToV2`.
- **Depends on how the license gate reads the module graph:** the MPL-2.0 `go-cleanhttp` and `go-retryablehttp` sit only in go-metrics' `go.mod`. They appear in `go mod graph`, but they would appear in `go list -deps` only if a Circonus sink package were imported. A gate that scans `go mod graph` would flag them; a gate that scans `go list -deps -test=false` would not.
- **Obligations:** the MPL-2.0 obligations already written for raft ("use unmodified; keep notices; publish patches to MPL-2.0 files under MPL-2.0") apply unchanged to the three other MPL-2.0 modules.

## 4. wazero: repository versus Go module path

| Fact | Value | Source |
|---|---|---|
| GitHub repository | `github.com/wazero/wazero`; the API resolves `tetratelabs/wazero` to `wazero/wazero` | (https://github.com/wazero/wazero) |
| Module path in `go.mod` (on `main` and at v1.12.0) | `module github.com/tetratelabs/wazero` | (https://github.com/wazero/wazero/blob/main/go.mod) (https://github.com/wazero/wazero/blob/v1.12.0/go.mod) |
| Go proxy, canonical path | `github.com/tetratelabs/wazero@latest` is v1.12.0 (2026-05-28), origin URL `https://github.com/tetratelabs/wazero`, tag `refs/tags/v1.12.0`, hash `2ab480b5...` | (https://proxy.golang.org/github.com/tetratelabs/wazero/@latest) |
| Go proxy, repository path | `github.com/wazero/wazero@latest` returns the same hash, but its `.mod` file still declares `module github.com/tetratelabs/wazero` | (https://proxy.golang.org/github.com/wazero/wazero/@latest) (https://proxy.golang.org/github.com/wazero/wazero/@v/v1.12.0.mod) |
| Latest release and license | v1.12.0, GitHub release published 2026-05-29; Apache-2.0 | (https://github.com/wazero/wazero/releases/tag/v1.12.0) (https://github.com/wazero/wazero/blob/v1.12.0/LICENSE) |

- Analysis: `import "github.com/wazero/wazero"` does not work. The go command rejects a module whose `go.mod` path differs from the path it was required as. Code and `go.mod` must use `github.com/tetratelabs/wazero`, and prose can name the repository `wazero/wazero`. This matches the tech-stack catalog wording and closes OQ-tech-stack-and-libraries-1 as a naming note.

## 5. `golang.org/x/net/http2` Server and Transport deprecation

| Fact | Value | Source |
|---|---|---|
| Proposal | golang/go#78064 "x/net/http2: deprecate Transport and Server", opened 2026-03-11 | (https://github.com/golang/go/issues/78064) |
| Proposal status | "likely accept" 2026-06-25; "accepted" 2026-07-08 | (https://github.com/golang/go/issues/78064) |
| Change | CL 819740 "http2: deprecate Transport and Server" (linked on the issue 2026-08-23); issue closed 2026-08-27 | (https://go.dev/cl/819740) (https://github.com/golang/go/issues/78064) |
| First x/net release with the deprecation | v0.59.0, tag commit 2026-09-08. v0.58.0 (2026-08-12) has no `Deprecated:` marker on `Server` or `Transport` | (https://github.com/golang/net/tree/v0.59.0/http2) (https://github.com/golang/net/tree/v0.58.0/http2) |
| `Server` doc comment at v0.59.0 | "Deprecated: Use [http.Server] instead." (`http2/server_common.go`) | (https://github.com/golang/net/blob/v0.59.0/http2/server_common.go) |
| `ConfigureServer` at v0.59.0 | "Deprecated: Set [http.Server.Protocols] instead." | (https://github.com/golang/net/blob/v0.59.0/http2/server_common.go) |
| `Transport` doc comment at v0.59.0 | "Deprecated: Use [http.Transport] instead." (`http2/transport_common.go`) | (https://github.com/golang/net/blob/v0.59.0/http2/transport_common.go) |
| `ClientConn` at v0.59.0 | "Deprecated: Use [http.ClientConn] instead." | (https://github.com/golang/net/blob/v0.59.0/http2/transport.go) |
| `Framer` at v0.59.0 | Not deprecated ("A Framer reads and writes Frames.") | (https://github.com/golang/net/blob/v0.59.0/http2/frame.go) |

- Newest x/net tag on 2026-09-23 is v0.59.0 (https://github.com/golang/net/tags).
- Analysis: this backs the tech-stack wording "`x/net/http2` only for low-level APIs such as `Framer`" and closes OQ-tech-stack-and-libraries-2 with option (a) or (b) as written.

## 6. YAML 1.2 parser candidates (OQ-tech-stack-and-libraries-12, OQ-configuration-model-16)

The configuration model requires YAML 1.2 core scalars (`yes` and `on` are strings), rejects duplicate keys, anchors, aliases, merge keys and custom tags, and states that "a YAML 1.1 decoder, which turns `on` into a boolean, does not qualify" (docs/architecture/02-configuration-model.md, Restricted YAML profile).

| Candidate | Module path | License (SPDX) | LICENSE file | Latest version (date) | YAML 1.1 versus 1.2 behavior | Source |
|---|---|---|---|---|---|---|
| goccy/go-yaml | `github.com/goccy/go-yaml` | `MIT` | (https://github.com/goccy/go-yaml/blob/master/LICENSE) | v1.19.2 (2026-01-08) | The source says the library "is supposed to be compliant only with YAML 1.2". The YAML 1.1 words `y`, `yes`, `on`, `off` and similar are used "solely for encoding the bool value with quotes" and are not treated as keywords at parse time | (https://github.com/goccy/go-yaml/releases/tag/v1.19.2) (https://github.com/goccy/go-yaml/blob/master/token/token.go) |
| go-yaml v4 (YAML org) | `go.yaml.in/yaml/v4` | `Apache-2.0 AND MIT` (Apache-2.0 overall; files ported from libyaml stay MIT per `NOTICE`) | (https://github.com/yaml/go-yaml/blob/main/LICENSE) (https://github.com/yaml/go-yaml/blob/main/NOTICE) | v4.0.0-rc.6 (tag date 2026-06-17); no final v4 release | README: "supports most of YAML 1.2, but preserves some behavior from 1.1" | (https://github.com/yaml/go-yaml/blob/main/README.md) (https://github.com/yaml/go-yaml/tags) |
| go-yaml v3 (YAML org) | `go.yaml.in/yaml/v3` | `Apache-2.0 AND MIT` (as above) | (https://github.com/yaml/go-yaml/blob/v3.0.5/LICENSE) | v3.0.5 (tag date 2026-07-26) | README for v3: YAML 1.1 bools are honored "as long as they are being decoded into a typed bool value. Otherwise they behave as a string"; `0777` octals are read per YAML 1.1 | (https://github.com/yaml/go-yaml/blob/main/README.md) (https://github.com/yaml/go-yaml/blob/v3.0.5/go.mod) |
| sigs.k8s.io/yaml | `sigs.k8s.io/yaml` | `MIT AND BSD-3-Clause` (MIT for Sam Ghods' code plus a Go Authors BSD section; GitHub reports `NOASSERTION`) | (https://github.com/kubernetes-sigs/yaml/blob/master/LICENSE) | v1.6.0 (2025-07-24) | Converts YAML to JSON "using go-yaml". At v1.6.0 `yaml.go` imports `go.yaml.in/yaml/v2`, whose resolver maps `y`, `yes`, `on` and similar to booleans (YAML 1.1) | (https://github.com/kubernetes-sigs/yaml/blob/master/README.md) (https://github.com/kubernetes-sigs/yaml/blob/v1.6.0/yaml.go) (https://github.com/yaml/go-yaml/blob/v2.4.2/resolve.go) |

- goccy/go-yaml README: of the YAML Test Suite's "402 cases in total", `gopkg.in/yaml.v3` "passes `295`", and goccy/go-yaml "successfully passes nearly 60 additional test cases (2024/12/15)" (https://github.com/goccy/go-yaml/blob/master/README.md).
- goccy/go-yaml has "No dependencies" and exposes `lexer.Tokenize` and `parser.Parse` for AST access (https://github.com/goccy/go-yaml/blob/master/README.md).
- goccy/go-yaml: duplicate mapping keys are a syntax error by default, and the option `AllowDuplicateMapKey()` "ignore[s] syntax error when mapping keys that are duplicates"; `DisallowUnknownField()` and `Strict()` reject unknown struct fields (https://github.com/goccy/go-yaml/blob/v1.19.2/option.go).
- goccy/go-yaml activity: the last three default-branch commits are dated 2026-02-26, 2026-03-10 and 2026-04-07; the last release is v1.19.2 (2026-01-08) (https://github.com/goccy/go-yaml/commits/master) (https://github.com/goccy/go-yaml/releases).
- go-yaml README: "Versions `v1`, `v2`, and `v3` will remain as **frozen legacy**" with "security-fixes only"; new work happens in `v4` at `go.yaml.in/yaml/v4` (https://github.com/yaml/go-yaml/blob/main/README.md).
- go-yaml v3 records duplicate keys as a decode error, "mapping key %#v already defined at line %d" (https://github.com/yaml/go-yaml/blob/v3.0.5/decode.go).
- go-yaml v2 README: "supports most of YAML 1.1 and 1.2" (https://github.com/yaml/go-yaml/blob/v2.4.2/README.md).
- Analysis: sigs.k8s.io/yaml v1.6.0 decodes through a YAML 1.1 resolver, so it fails the configuration model's rule. goccy/go-yaml is the only candidate that claims YAML 1.2-only scalar resolution and rejects duplicate keys by default. go-yaml v3 and v4 still keep some 1.1 behavior: typed-bool coercion of `yes` and `on`, and `0777` octals. Either parser still needs a Ruralz pass over the AST to reject anchors, aliases, merge keys and custom tags, because none of the READMEs describes an option that rejects them.

## 7. JSON Schema draft 2020-12 validator candidates (OQ-tech-stack-and-libraries-13)

| Candidate | Module path | License (SPDX) | LICENSE file | Latest version (date) | Drafts and features | Source |
|---|---|---|---|---|---|---|
| santhosh-tekuri/jsonschema | `github.com/santhosh-tekuri/jsonschema/v6` | `Apache-2.0` | (https://github.com/santhosh-tekuri/jsonschema/blob/boon/LICENSE) | v6.0.3 (2026-08-06) | Passes the JSON-Schema-Test-Suite, "excluding optional", for draft-04, -06, -07, 2019-09 and 2020-12. Supports vocabulary-based validation and custom `$schema` URLs, a custom regex engine, and format and content assertions behind flags | (https://github.com/santhosh-tekuri/jsonschema/blob/boon/README.md) (https://github.com/santhosh-tekuri/jsonschema/releases/tag/v6.0.3) |
| kaptinlin/jsonschema | `github.com/kaptinlin/jsonschema` | `MIT` | (https://github.com/kaptinlin/jsonschema/blob/main/LICENSE) | v0.9.9 (2026-08-27) | Draft 2020-12, 2019-09, -07, -06, -04; defaults to 2020-12 when `$schema` is absent; `format` is annotation-only in the standard 2020-12 dialect | (https://github.com/kaptinlin/jsonschema/blob/main/README.md) (https://github.com/kaptinlin/jsonschema/releases/tag/v0.9.9) |
| google/jsonschema-go | `github.com/google/jsonschema-go` | `MIT` | (https://github.com/google/jsonschema-go/blob/main/LICENSE) | v0.4.3 (GitHub release 2026-04-27; pkg.go.dev shows 2026-04-17) | "supports both draft-07 and the 2020-12 draft specifications"; also infers schemas from Go structs | (https://pkg.go.dev/github.com/google/jsonschema-go/jsonschema) (https://github.com/google/jsonschema-go/releases/tag/v0.4.3) |
| xeipuuv/gojsonschema | `github.com/xeipuuv/gojsonschema` | GitHub detects no license (`none`) | Not verified | Last push 2024-06-28 | Not evaluated further: no 2020-12 claim was checked, and it fails S1 | (https://github.com/xeipuuv/gojsonschema) |

- santhosh-tekuri/jsonschema v6.0.3 `go.mod` declares `go 1.21` and requires only `golang.org/x/text` (plus `dlclark/regexp2`, "used for testing") (https://github.com/santhosh-tekuri/jsonschema/blob/v6.0.3/go.mod).
- santhosh-tekuri/jsonschema v6 exposes a `Vocabulary` type ("defines a set of keywords, their syntax and their semantics") for custom keywords, and a `ValidatorContext.ValueLocation()` that returns the instance location as a JSON-path token array (https://github.com/santhosh-tekuri/jsonschema/blob/v6.0.3/vocab.go).
- kaptinlin/jsonschema's `go.mod` on `main` declares `go 1.27.0` and pulls in `goccy/go-yaml`, `go-json-experiment/json` and `kaptinlin/messageformat-go` (https://github.com/kaptinlin/jsonschema/blob/main/go.mod).
- google/jsonschema-go `go.mod` declares `go 1.23.0`; its README says it "has no dependencies outside of the standard library" (https://github.com/google/jsonschema-go/blob/main/go.mod) (https://github.com/google/jsonschema-go/blob/main/README.md).
- Analysis: kaptinlin/jsonschema's `go 1.27.0` directive fails tech-stack criterion S2 (builds on the Go 1.26 floor). santhosh-tekuri/jsonschema/v6 meets G2 and S2, claims full 2020-12 test-suite compliance and has a documented custom-keyword API, which the configuration model's seven annotation keywords need.

## 8. Other unselected concerns: candidate licenses

| Concern (OQ) | Candidate | License (SPDX) | LICENSE file | Latest version (date) | Source |
|---|---|---|---|---|---|
| Protobuf runtime (OQ-14) | `google.golang.org/protobuf` (repository protocolbuffers/protobuf-go) | `BSD-3-Clause` | (https://github.com/protocolbuffers/protobuf-go/blob/master/LICENSE) | v1.36.12 (proxy time 2026-08-10) | (https://proxy.golang.org/google.golang.org/protobuf/@latest) |
| ULID (OQ-15) | `github.com/oklog/ulid/v2` | `Apache-2.0` | (https://github.com/oklog/ulid/blob/main/LICENSE) | v2.1.2 (2026-07-23) | (https://github.com/oklog/ulid/releases/tag/v2.1.2) |
| `/metrics` exposition (OQ-16) | `github.com/prometheus/client_golang` | `Apache-2.0` | (https://github.com/prometheus/client_golang/blob/main/LICENSE) | v1.24.1 (2026-07-24) | (https://github.com/prometheus/client_golang/releases/tag/v1.24.1) |
| `/metrics` exposition (OQ-16) | `go.opentelemetry.io/otel/exporters/prometheus` | `Apache-2.0` | (https://github.com/open-telemetry/opentelemetry-go/blob/main/LICENSE) | v0.68.0 (proxy time 2026-08-25) | (https://proxy.golang.org/go.opentelemetry.io/otel/exporters/prometheus/@latest) |
| SigV4 (OQ-17) | `github.com/aws/aws-sdk-go-v2` (`aws/signer/v4`) | `Apache-2.0` | (https://github.com/aws/aws-sdk-go-v2/blob/main/LICENSE.txt) | root module v1.47.0 (proxy time 2026-09-09) | (https://proxy.golang.org/github.com/aws/aws-sdk-go-v2/@latest) |
| CLI framework (OQ-18) | `github.com/spf13/cobra` | `Apache-2.0` | (https://github.com/spf13/cobra/blob/main/LICENSE.txt) | v1.10.2 (2025-12-04) | (https://github.com/spf13/cobra/releases/tag/v1.10.2) |
| SAML (OQ-19) | `github.com/crewjam/saml` | `BSD-2-Clause` | (https://github.com/crewjam/saml/blob/main/LICENSE) | v0.5.1 (tag date 2025-04-14); no GitHub releases | (https://proxy.golang.org/github.com/crewjam/saml/@latest) (https://github.com/crewjam/saml/tags) |
| `postgres` driver (OQ-20) | `github.com/jackc/pgx/v5` | `MIT` | (https://github.com/jackc/pgx/blob/master/LICENSE) | v5.11.0 (2026-09-07) | (https://github.com/jackc/pgx/releases/tag/v5.11.0) |

- Analysis: crewjam/saml's last tag (2025-04-14) is more than 12 months before the snapshot, so it fails S1. The dependencies of these candidates were not license-checked.

## Sources

https://adr.github.io/madr/
https://github.com/adr/madr
https://github.com/adr/madr/releases/tag/4.0.0
https://github.com/adr/madr/blob/develop/LICENSE
https://github.com/adr/madr/blob/develop/LICENSE.MIT
https://github.com/adr/madr/blob/develop/LICENSE.CC0-1.0
https://github.com/adr/madr/blob/develop/package.json
https://github.com/DavidAnson/markdownlint-cli2
https://github.com/DavidAnson/markdownlint-cli2/blob/main/LICENSE
https://github.com/DavidAnson/markdownlint-cli2/tags
https://registry.npmjs.org/markdownlint-cli2
https://github.com/DavidAnson/markdownlint/blob/main/LICENSE
https://registry.npmjs.org/markdownlint
https://github.com/lycheeverse/lychee
https://github.com/lycheeverse/lychee/releases/tag/lychee-v0.24.2
https://github.com/lycheeverse/lychee/blob/master/LICENSE-APACHE
https://github.com/lycheeverse/lychee/blob/master/LICENSE-MIT
https://github.com/lycheeverse/lychee/blob/master/README.md
https://github.com/lycheeverse/lychee/blob/master/lychee-bin/Cargo.toml
https://github.com/mermaid-js/mermaid
https://github.com/mermaid-js/mermaid/blob/develop/LICENSE
https://github.com/mermaid-js/mermaid/releases/tag/mermaid%4012.0.0
https://registry.npmjs.org/mermaid
https://registry.npmjs.org/@mermaid-js/parser
https://registry.npmjs.org/@mermaid-js/tiny
https://registry.npmjs.org/@mermaid-js/layout-elk
https://registry.npmjs.org/@mermaid-js/mermaid-zenuml
https://registry.npmjs.org/@mermaid-js/mermaid-cli
https://github.com/mermaid-js/mermaid-cli/blob/master/LICENSE
https://github.com/nodeca/js-yaml
https://github.com/nodeca/js-yaml/blob/master/LICENSE
https://github.com/nodeca/js-yaml/blob/master/CHANGELOG.md
https://registry.npmjs.org/js-yaml
https://github.com/bufbuild/buf/releases/tag/v1.73.0
https://github.com/bufbuild/buf/blob/main/LICENSE
https://github.com/golangci/golangci-lint/releases/tag/v2.13.2
https://github.com/golangci/golangci-lint/blob/main/LICENSE
https://github.com/golangci/golangci-lint/blob/v2.13.2/LICENSE
https://github.com/goreleaser/goreleaser/releases/tag/v2.18.2
https://github.com/goreleaser/goreleaser/blob/main/LICENSE.md
https://github.com/sigstore/cosign/releases/tag/v3.1.3
https://github.com/sigstore/cosign/releases/tag/v2.6.5
https://github.com/sigstore/cosign/blob/v3.1.3/go.mod
https://github.com/sigstore/cosign/blob/v3.1.3/LICENSE
https://github.com/sigstore/cosign/blob/main/LICENSE
https://github.com/sigstore/sigstore-go/releases/tag/v1.3.0
https://github.com/sigstore/sigstore-go/blob/main/LICENSE
https://github.com/anchore/syft/releases/tag/v1.52.0
https://github.com/anchore/syft/blob/main/LICENSE
https://github.com/oras-project/oras-go/releases/tag/v2.6.2
https://github.com/oras-project/oras-go/blob/v2.6.2/go.mod
https://github.com/oras-project/oras-go/blob/main/LICENSE
https://go.dev/doc/go1.24
https://github.com/hashicorp/raft/releases/tag/v1.8.0
https://github.com/hashicorp/raft/blob/v1.8.0/LICENSE
https://github.com/hashicorp/raft/blob/v1.8.0/go.mod
https://github.com/hashicorp/raft/blob/v1.8.0/api.go
https://github.com/hashicorp/raft-boltdb/releases/tag/v2.4.2
https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/LICENSE
https://github.com/hashicorp/raft-boltdb/tree/v2.4.2/v2
https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/v2/go.mod
https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/v2/bolt_store.go
https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/v2/util.go
https://github.com/hashicorp/raft-boltdb/blob/v2.4.2/v2/bolt_store_test.go
https://github.com/hashicorp/raft-boltdb/blob/2a8082862702/LICENSE
https://github.com/hashicorp/raft-boltdb/blob/2a8082862702/bolt_store.go
https://proxy.golang.org/github.com/hashicorp/raft-boltdb/v2/@v/v2.4.2.zip
https://github.com/etcd-io/bbolt/releases/tag/v1.5.0
https://github.com/etcd-io/bbolt/releases/tag/v1.4.1
https://github.com/etcd-io/bbolt/blob/v1.4.1/LICENSE
https://github.com/etcd-io/bbolt/blob/v1.5.0/LICENSE
https://github.com/etcd-io/bbolt/blob/v1.4.1/go.mod
https://github.com/hashicorp/go-msgpack/releases/tag/v2.1.5
https://github.com/hashicorp/go-msgpack/blob/v2.1.5/LICENSE
https://github.com/hashicorp/go-msgpack/blob/v2.1.5/go.mod
https://github.com/hashicorp/go-msgpack/tree/v2.1.5/codec
https://github.com/hashicorp/go-msgpack/blob/v0.5.5/LICENSE
https://github.com/hashicorp/go-hclog/releases/tag/v1.6.3
https://github.com/hashicorp/go-hclog/blob/v1.6.3/LICENSE
https://github.com/hashicorp/go-hclog/blob/v1.6.3/go.mod
https://github.com/hashicorp/go-metrics/blob/v0.7.0/LICENSE
https://github.com/hashicorp/go-metrics/blob/v0.7.0/go.mod
https://github.com/hashicorp/go-metrics/blob/v0.7.0/metrics.go
https://github.com/hashicorp/go-metrics/blob/v0.4.1/LICENSE
https://github.com/hashicorp/go-immutable-radix/blob/v1.3.1/LICENSE
https://github.com/hashicorp/go-immutable-radix/blob/v1.3.1/iradix.go
https://github.com/hashicorp/golang-lru/blob/v1.0.2/LICENSE
https://github.com/boltdb/bolt
https://github.com/boltdb/bolt/blob/v1.3.1/LICENSE
https://github.com/fatih/color/blob/v1.19.0/LICENSE.md
https://github.com/mattn/go-colorable/blob/v0.1.15/LICENSE
https://github.com/mattn/go-isatty/blob/v0.0.24/LICENSE
https://github.com/golang/sys/blob/v0.48.0/LICENSE
https://github.com/hashicorp/go-cleanhttp/blob/master/LICENSE
https://github.com/hashicorp/go-retryablehttp/blob/main/LICENSE
https://github.com/openhistogram/circonusllhist/blob/master/LICENSE
https://github.com/munnerz/goautoneg/blob/master/LICENSE
https://github.com/wazero/wazero
https://github.com/wazero/wazero/blob/main/go.mod
https://github.com/wazero/wazero/blob/v1.12.0/go.mod
https://github.com/wazero/wazero/blob/v1.12.0/LICENSE
https://github.com/wazero/wazero/releases/tag/v1.12.0
https://proxy.golang.org/github.com/tetratelabs/wazero/@latest
https://proxy.golang.org/github.com/wazero/wazero/@latest
https://proxy.golang.org/github.com/wazero/wazero/@v/v1.12.0.mod
https://github.com/golang/go/issues/78064
https://go.dev/cl/819740
https://github.com/golang/net/tree/v0.59.0/http2
https://github.com/golang/net/tree/v0.58.0/http2
https://github.com/golang/net/blob/v0.59.0/http2/server_common.go
https://github.com/golang/net/blob/v0.59.0/http2/transport_common.go
https://github.com/golang/net/blob/v0.59.0/http2/transport.go
https://github.com/golang/net/blob/v0.59.0/http2/frame.go
https://github.com/golang/net/tags
https://github.com/goccy/go-yaml/releases/tag/v1.19.2
https://github.com/goccy/go-yaml/releases
https://github.com/goccy/go-yaml/commits/master
https://github.com/goccy/go-yaml/blob/master/LICENSE
https://github.com/goccy/go-yaml/blob/master/README.md
https://github.com/goccy/go-yaml/blob/master/token/token.go
https://github.com/goccy/go-yaml/blob/v1.19.2/option.go
https://github.com/yaml/go-yaml/blob/main/LICENSE
https://github.com/yaml/go-yaml/blob/main/NOTICE
https://github.com/yaml/go-yaml/blob/main/README.md
https://github.com/yaml/go-yaml/tags
https://github.com/yaml/go-yaml/blob/v3.0.5/LICENSE
https://github.com/yaml/go-yaml/blob/v3.0.5/go.mod
https://github.com/yaml/go-yaml/blob/v3.0.5/decode.go
https://github.com/yaml/go-yaml/blob/v2.4.2/README.md
https://github.com/yaml/go-yaml/blob/v2.4.2/resolve.go
https://github.com/kubernetes-sigs/yaml/blob/master/LICENSE
https://github.com/kubernetes-sigs/yaml/blob/master/README.md
https://github.com/kubernetes-sigs/yaml/blob/v1.6.0/yaml.go
https://github.com/santhosh-tekuri/jsonschema/blob/boon/LICENSE
https://github.com/santhosh-tekuri/jsonschema/blob/boon/README.md
https://github.com/santhosh-tekuri/jsonschema/releases/tag/v6.0.3
https://github.com/santhosh-tekuri/jsonschema/blob/v6.0.3/go.mod
https://github.com/santhosh-tekuri/jsonschema/blob/v6.0.3/vocab.go
https://github.com/kaptinlin/jsonschema/blob/main/LICENSE
https://github.com/kaptinlin/jsonschema/blob/main/README.md
https://github.com/kaptinlin/jsonschema/blob/main/go.mod
https://github.com/kaptinlin/jsonschema/releases/tag/v0.9.9
https://pkg.go.dev/github.com/google/jsonschema-go/jsonschema
https://github.com/google/jsonschema-go/releases/tag/v0.4.3
https://github.com/google/jsonschema-go/blob/main/LICENSE
https://github.com/google/jsonschema-go/blob/main/go.mod
https://github.com/google/jsonschema-go/blob/main/README.md
https://github.com/xeipuuv/gojsonschema
https://proxy.golang.org/google.golang.org/protobuf/@latest
https://github.com/protocolbuffers/protobuf-go/blob/master/LICENSE
https://github.com/oklog/ulid/releases/tag/v2.1.2
https://github.com/oklog/ulid/blob/main/LICENSE
https://github.com/prometheus/client_golang/releases/tag/v1.24.1
https://github.com/prometheus/client_golang/blob/main/LICENSE
https://proxy.golang.org/go.opentelemetry.io/otel/exporters/prometheus/@latest
https://github.com/open-telemetry/opentelemetry-go/blob/main/LICENSE
https://proxy.golang.org/github.com/aws/aws-sdk-go-v2/@latest
https://github.com/aws/aws-sdk-go-v2/blob/main/LICENSE.txt
https://github.com/spf13/cobra/releases/tag/v1.10.2
https://github.com/spf13/cobra/blob/main/LICENSE.txt
https://proxy.golang.org/github.com/crewjam/saml/@latest
https://github.com/crewjam/saml/tags
https://github.com/crewjam/saml/blob/main/LICENSE
https://github.com/jackc/pgx/releases/tag/v5.11.0
https://github.com/jackc/pgx/blob/master/LICENSE

## Gaps

- **No module-graph run.** No Go toolchain was available, so `go list -deps` and `go mod graph` were not run against a module that imports raft v1.8.0 and raft-boltdb/v2 v2.4.2. The linked set in section 3.2 comes from reading import blocks in non-test source files. A package imported from a file not read here (for example another raft source file) could add a module. The Planned (M0) license gate should confirm the list.
- **Versions after MVS.** Section 3 gives the versions each dependency's own `go.mod` requires. The versions Ruralz actually builds with depend on minimal version selection across the whole Ruralz module graph, so they may be higher. No license was found to change between the pinned and newest versions checked, but only the tags listed were read.
- **Module-graph-only dependencies.** The licenses in section 3.3 for the go-metrics sink dependencies (including the MPL-2.0 `go-cleanhttp` and `go-retryablehttp`) are read at `HEAD`, not at the versions go-metrics v0.7.0 requires. The benchmark codecs in go-msgpack/v2's `go.mod` (`Sereal`, `mgo.v2` and others) were not license-checked. Whether any of them shows up in the Ruralz module graph after module-graph pruning (go-msgpack/v2 declares `go 1.24.0`, go-metrics `go 1.25.0`) was not tested.
- **MPL-2.0 Exhibit B.** Whether any of the four MPL-2.0 modules carries the "Incompatible With Secondary Licenses" notice was not checked. It does not affect Apache-2.0 distribution, but it would matter to a GPL-licensed downstream.
- **wazero rejection message.** The claim that the go command rejects `github.com/wazero/wazero` as an import path is inferred from the proxy's `.mod` content and the go command's module-path check. It was not reproduced with a Go toolchain.
- **Release dates.** GitHub release `published_at`, tag commit dates, npm publish times and proxy `Time` values sometimes differ by hours or days: wazero v1.12.0 has proxy time 2026-05-28 and GitHub release 2026-05-29; google/jsonschema-go v0.4.3 shows 2026-04-17 on pkg.go.dev and 2026-04-27 on GitHub. The table cells name which one is used.
- **golang/go CL 819740.** The CL page itself was not fetched. Its title and link come from the bot comment on golang/go#78064, and the deprecation text was checked directly in x/net v0.59.0 source.
- **YAML parser conformance.** The YAML Test Suite figures for goccy/go-yaml are self-reported in its README as of 2024-12-15, and no independent run was made. No candidate documents an option that rejects anchors, aliases or merge keys, so the restricted profile's rejection logic is assumed to be Ruralz code over the AST. That rejection logic is not verified against any library.
- **JSON Schema compliance.** santhosh-tekuri/jsonschema's compliance is shown as Bowtie badges in its README, and the Bowtie report was not fetched. kaptinlin/jsonschema's `go 1.27.0` directive was read on `main`, not at the v0.9.9 tag. google/jsonschema-go's support for custom keywords and source positions was not checked.
- **xeipuuv/gojsonschema license.** GitHub detects no license file, and the repository was not examined further.
- **MADR 4.0 release notes.** Only the release tag, the site and the license files were read. What changed between 3.0.0 and 4.0.0 was not recorded.
