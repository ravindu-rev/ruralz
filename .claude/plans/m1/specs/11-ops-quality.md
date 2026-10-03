# M1 spec, area 11 of 11: Operations, quality gates and release 0.1.0

Design reader output for milestone M1 "Core gateway". Sources (abbreviations used in references):
RM = docs/roadmap/01-roadmap-and-milestones.md; TQ = docs/engineering/03-testing-and-quality-strategy.md;
PBB = docs/architecture/12-performance-budgets-and-benchmarking.md; SDS = docs/architecture/11-scalability-and-distributed-state.md;
HA = docs/operations/04-high-availability-and-disaster-recovery.md; DT = docs/operations/01-deployment-topologies.md;
ZDU = docs/operations/02-zero-downtime-upgrades-and-hot-reload.md; RVC = docs/engineering/04-release-versioning-and-compatibility.md;
RL = docs/engineering/02-repository-layout-and-conventions.md; TS = docs/engineering/01-tech-stack-and-libraries.md;
ADR2/3/8/15 = docs/adr/000N-*.md; FP = docs/_meta/foundation-pack.md; DP = docs/architecture/03-data-plane.md;
OBS = docs/architecture/10-observability.md; SEC = docs/architecture/08-security-and-identity.md; CM = docs/architecture/02-configuration-model.md;
CLI = docs/reference/01-cli-and-api-surface.md; VIS = docs/vision/01-vision-and-positioning.md; RES-T = docs/_meta/research/tooling-and-licenses.md;
RES-S = docs/_meta/research/scalability-patterns.md. Sibling specs: A1 config-load, A2 config-revision, A3 cel, A4 dataplane-core,
A5 upstream-traffic, A6 security, A8 statestore, A9 observability, A10 cli.

Classification tag on every item: **[NOW]** implementable and runnable in this environment (Linux amd64, 4 CPU, 15 GiB, root,
`unshare` works, no Docker daemon, no systemd as PID 1 but `systemd-analyze` 255 present, local `redis-server` 7.0.15,
fd hard limit 20,000); **[CI]** implementable now, runnable only in GitHub Actions (Docker daemon, systemd PID 1, arm64 runners,
OIDC keyless signing, GHCR, larger runners); **[ENV]** blocked by environment (RH-1 bare metal, staging clusters for human game days,
a previous release as N-1); **[ROW]** needs a Tech stack catalog or CI tooling row first (doc pull request, TS "Admission").

## 1. Scope

| # | M1 item (this area) | Source |
|---|---|---|
| S1 | CI stages 8 integration and conformance, 9 regression gates, 10 end-to-end, 11 nightly, 12 release; pipelines `pr-full`, `main`, `nightly`, `release` | RM "M1 scope" Quality row; RL "CI stages"; TQ "Test pyramid" |
| S2 | Golden corpus (byte-exact), JSON subset fixtures, loader conformance suite, hostile-input suite, upstream test suites (YAML Test Suite, JSON-Schema-Test-Suite), Go test meta-validating both schema files and `examples/` (suite harness, CI placement; content by A1/A2) | RM Configuration row; ADR3 "Confirmation"; CM "Canonical form and Revision"; RL "Schema generation from Go types" (generator rules); TQ "Golden tests", "Conformance suites" |
| S3 | Protocol conformance suite in `pr-full` (HTTP/1.1, HTTP/2) | RM Quality row; TQ "Conformance suites"; DP "Listeners and protocols", "Bounded resources"; SEC "Request hardening" |
| S4 | Property tests, fuzzing (M1 target families), round-trip test, integration tests (harness and State Store matrix) | RM Quality row; TQ "Required properties", "Fuzzing", "Integration tests"; ADR8 "Confirmation" |
| S5 | Docker Compose end-to-end test in file mode: quickstart, `--effective --route`, `ruralz bundle diff` against Node `/config/dump`, Zero-Downtime Upgrade; every M1 pack section 9 command; secret leak tests; air-gapped start | RM Quality, Security rows; TQ "End-to-end tests" scenarios 1, 2 (REST), 6, 7; ADR2 "Confirmation"; RM "M1 exit criteria" 4, 5 |
| S6 | Chaos experiments CE-1 to CE-6, CE-12, CE-15, CE-16; TQ chaos rows (Node kill/SIGTERM, State Store slow then stopped, Upstream Endpoints reset or slowed); game days GD-1 to GD-5; HA M1 failure rows FC-1, FC-3 (Node and State Store parts), FC-10 to FC-13, FC-16, FC-19, FC-20 (file mode), FC-21 (`file`/`env` part) | RM Quality row, exit 5; SDS "Chaos experiments"; TQ "Chaos testing"; HA "Failure catalog", "Game days", "RPO and RTO" |
| S7 | Zero-Downtime Upgrade and Drain verification V-1, V-3, V-4, V-6; ZG-1 to ZG-4 | ZDU "Guarantees", "Verification runbook"; ADR15 "Confirmation" |
| S8 | Budgets PB-1 to PB-4, PB-6 to PB-11, PB-14 to PB-16; scenarios S1, S2, S3, S5, S5x, O1, O2, C1, C2, R1, G1; microbenchmarks; soak; alloc/op, size and idle RSS gates; macro p99 and macro latency; absolute budgets and criteria; component and CEL benchmarks; binary and CI size check; nightly and release runs publish records | RM Budgets row, exit 2, 3; PBB all M1 sections; TQ "Benchmarks and regression gates" |
| S9 | Topologies T1 development single binary, T2 file mode watching a rendered directory, T5 VM with systemd in file mode (shipped unit and helper) | RM Lifecycle row; DT "T1 and T2", "T5 and T6", "Network and ports", "File-mode bootstrap"; ZDU "In-place handover" |
| S10 | Release `0.1.0`: binaries, container images, checksums, SBOM, signatures, provenance, notes, release audit per tag; reproducible rebuild; `cmd/ruralzd`, `cmd/ruralz`, `test/`, `examples/` | RM Release row, exit 1; RVC "One product version", "Release cadence", "Release artifacts"; ADR2 "Confirmation"; SEC "Release supply chain"; TQ "Release gates" |

## 2. Normative requirements

### A. CI pipelines and stages 8 to 12

1. Stage 8 "Integration and conformance" (`pr-full`) MUST run `make integration` = `CGO_ENABLED=1 go test -race -shuffle=on -tags integration,netgo,osusergo -timeout 25m ./...`, which includes the three conformance suites (M1: configuration and protocol; Plugin ABI M2), YAML Test Suite and JSON-Schema-Test-Suite runs, State Store integration, and `ruralz bundle validate` over every Bundle under `examples/` (process level, exit 0 required). Trigger: Go, proto, schema, `examples/` or `test/` changes, and every merge queue run; blocks merge. [RL "CI stages" row 8; TQ "Test pyramid"] [NOW locally with `redis-server`; container matrix CI]
2. Stage 8 MUST run on linux/amd64 and linux/arm64 (OQ-repository-layout-and-conventions-6, section 7: native arm64 runners). [RL OQ table] [CI]
3. Stage 9 "Regression gates" (`pr-full`) MUST run `make gates`: the alloc/op gate (requirements 64 to 67) and the size and idle RSS gate (requirement 68). Trigger: Go changes; blocks merge. [RL row 9; PBB "Regression policy and gates"] [NOW]
4. `pr-full` total wall clock 30 minutes (target); its aggregator job `pr-full` passes only when each stage passed or was skipped by its path filter, and is a required check like `pr-fast`. The path filter regex for `pr-full`: `\.go$|^go\.(mod|sum)$|^\.golangci\.yml$|^Makefile$|^scripts/|^(api|examples|test|deploy)/|^internal/tool/`; any change under `.github/workflows/` selects every stage. [TQ "Test pyramid" stage table; RL "CI stages" preamble] [CI]
5. Stage 10 "End-to-end" (`main`, every merge) MUST run `make e2e` = `go test -tags e2e -timeout 40m ./test/e2e/...` plus the secret leak assertions in every run; it does not block merge but a failure blocks the next release. Budget 45 minutes (target). Docker Compose is replaced by the process harness (section 3, risk 1); the Compose variant is kept only for the image air-gapped start in stage 12. [RL row 10; TQ "End-to-end tests"] [NOW]
6. Stage 11 "Nightly" (`nightly`, daily) MUST run four parallel jobs: **Fuzz** (every `Fuzz*` target 15 minutes, eight targets per shard, shards on general runners; the grown corpus kept as a CI artifact), **Chaos** (every M1 chaos scenario and CE experiment against at least 10 Nodes, target), **Latency** (RH-1 only, alone on the machine, longest job 3 hours, target), and the existing supply chain rescan. No Scale job in M1 (SM-9 and SM-10 are M2/M3). A failure opens a GitHub issue labeled `nightly-failure` (fuzz crashers: `fuzz-crasher`) and blocks the next release. [TQ "Test pyramid" nightly table; RL row 11; PBB "Suite layers"] [Fuzz, Chaos: CI (runnable NOW locally at reduced scale); Latency: ENV]
7. Stage 12 "Release" (`release`, on a release candidate or release tag) MUST re-run every other stage and nightly job on the tagged commit, then the release gates (requirement 105), the ADR2 release audit and air-gapped start, then build, sign and publish (section L). Shipped binaries come only from stage 12, with stage 4's `CGO_ENABLED=0`, `-trimpath` and `-ldflags -X` flags; the tested commit MUST be the signed commit. [RL row 12; TQ "Release gates"; RVC "Release artifacts"] [CI]
8. Stages 1 to 11 each call one `make` target runnable locally; stage 12 also needs signing credentials. New targets: `integration`, `gates`, `e2e`, `nightly` (dispatching `NIGHTLY_JOB=fuzz|chaos|latency`), `fuzz`, `chaos`, `bench-latency`, `release`, `release-dry-run`, `quickstart`. [RL "CI stages"] [NOW]
9. Stage 5 (existing) MUST also run the untagged configuration conformance tests (golden, JSON subset, loader, hostile-input, negative corpus) on linux, the floor job and the CLI matrix (darwin/arm64, darwin/amd64, windows/amd64), with identical digests and identical JSON golden bytes in the release and floor jobs; the `test-cli` job's package list MUST add `./test/conformance/config/...`. [TQ "Unit and property tests", "Golden tests"; RL row 5] [CI; linux part NOW]
10. Every added workflow pins actions by commit SHA, sets `permissions: contents: read` at workflow level and widens per job only (`id-token: write`, `attestations: write`, `packages: write`, `contents: write`, `issues: write` exactly where used), sets `persist-credentials: false`, and uses no workflow-level `GOTOOLCHAIN` (existing convention). Actions beyond `actions/checkout` v7.0.1 and `actions/setup-go` v7.0.0 need CI tooling rows (section 5). [TS "CI tooling"] [ROW]
11. Existing `pr-fast.yml`, and the new `pr-full.yml`, `main.yml`, `nightly.yml` MUST accept `workflow_call` with input `all: true` that disables path filters, so `release.yml` re-runs them on the tag. [TQ "Release gates"] [CI]

### B. Configuration conformance suite (harness and CI placement; fixtures by A1/A2)

12. Golden corpus location and format follow A2 (`test/conformance/config/golden/<entry>/`: `bundle/`, `environments.yaml`, `expected.json` = `{"level":0,"digests":{"<env>|-":"sha256:…"}}`, `canonical/<env>.json`, `effective/<env>/<route>.txt|.json`, `diff/<name>.json|.txt`, `CHANGES`). Every release MUST reproduce every entry byte for byte at every schema level inside the skew window (only level 0 in `0.1.0`), except entries whose `CHANGES` records a change shipped as a new `ruralz/v1alpha1` schema level. Regeneration only with `go test ./test/conformance/config -run Golden -update`. [TQ "Golden tests"; RVC "One product version"; CM "Canonical form and Revision"] [NOW]
13. The corpus MUST include `shop-bundle` (CM "Complete annotated example Bundle", taken verbatim from `examples/shop-bundle/`) for `prod`, `staging` and no Environment, and its JSON twin, which MUST yield one digest. The digests printed in docs (`rev-162af81f5de4`, `sha256:68f78253…`) are illustrative; the first conforming build records the real digest after review (risk 5). [ADR3 "Confirmation" golden corpus] [NOW]
14. JSON subset fixtures: tab indentation, `\/` escapes and surrogate-pair `\u` escapes (for example `"😀"`) in `.json` files MUST yield exactly the digest of their YAML twins; an unpaired surrogate is RZ-CFG-001. Location `test/conformance/config/json-subset/<case>/{yaml,json}/`. [ADR3 "Confirmation"; A1 req 15] [NOW]
15. Loader conformance suite: one fixture per rejection RZ-CFG-001 (parse error, non-UTF-8, byte order mark, size, depth, resource count), RZ-CFG-002 (duplicate key), RZ-CFG-003 (anchor, alias, merge key), RZ-CFG-004 (custom tag); an alias-expansion fixture (billion-laughs shape) rejected with RZ-CFG-003 before expansion with bounded memory; plain scalars `yes`, `on`, `0777`, `0b1`, `1_000`, `2026-09-23`: `0777` MUST decode as 777 raw or substituted into an integer field and stay `"0777"` substituted into a string field; the others are strings. Each case asserts code, file, line and column. [ADR3 "Confirmation"; CM "Restricted YAML profile"] [NOW]
16. Negative corpus: one fixture per offline RZ-CFG code (001 to 023, 025, 029 to 032, 034 to 037) with file, line and column, metadata naming `stage`, `binaries` and source mapping (A1 layout `test/conformance/config/<code>-<slug>/fixture.yaml`). `ruralz bundle validate --output json` and `ruralzd` (rendered Bundle in file mode, its stages only) MUST emit identical code, file, line and column; `ruralz-control` joins in M2. The five checks that can fail after a green offline run carry no position; integration and e2e cases assert code and resource identity where raised: RZ-CFG-024 at file-mode load, RZ-CFG-026 at load, RZ-CFG-027 at Last-Known-Good boot, RZ-CFG-028 at `ruralz bundle build` (M1: no OCI, so every Plugin is RZ-CFG-028 unless `--offline`), RZ-CFG-033 (Revision signatures M2). [TQ "Conformance suites"] [NOW; cross-binary part is stage 8]
17. Hostile-input suite: generated at test time (never committed): a source of 64 MiB + 1 byte, 20,001 resources, nesting depth 65, each MUST fail with RZ-CFG-001 without a crash; peak loader heap (`runtime/metrics` `/memory/classes/heap/objects:bytes` max sampled every 10 ms) is recorded for OQ-configuration-model-18 and MUST stay below 4 × the source size (proposed bound, reported). [ADR3 "Confirmation" hostile input; CM "Restricted YAML profile"] [NOW]
18. Upstream suites (stage 8): the YAML Test Suite at a pinned data release vendored under `test/fixtures/yaml-test-suite/` against `goccy/go-yaml` through A1's profile, failing only on a checked-in `expected-failures.txt`; a listed case that passes also fails. The JSON-Schema-Test-Suite at a pinned commit, `tests/draft2020-12/` without `optional/` (except formats Ruralz uses), against `jsonschema/v6`: zero failures. Vendored suites carry their MIT `LICENSE` and a `SOURCE` file (URL, commit, SHA-256 of the tarball); `scripts/update-test-suites.sh` refreshes them. [ADR3 "Confirmation" upstream suites; TQ "Conformance suites"] [NOW]
19. Meta-validation Go test (A1 `internal/config/schemaview/meta_test.go`): both schema files compile against draft 2020-12 with `santhosh-tekuri/jsonschema/v6` and every Bundle under `examples/` validates for each Environment of `examples/control/environments.yaml` and without `--env`. It runs in stage 5 (path filter already selects `examples/`). [RL "Schema generation from Go types" generator rules] [NOW]
20. `.gitattributes` keeps `* text=auto eol=lf`; fixtures that must contain CRLF or a BOM live under a `raw/` directory marked `test/fixtures/**/raw/** -text`. [TQ "Golden tests"] [NOW]

### C. Protocol conformance suite (HTTP/1.1, HTTP/2)

21. Location `test/conformance/protocol/`, tag `integration`, stage 8. It drives the built `ruralzd` binary through its listeners (Bundle with an `http` listener for HTTP/1.1 and h2c prior knowledge, an `https` listener with ALPN `h2` and `http/1.1`) against the in-process mock upstream; raw HTTP/1.1 over `net.Conn`, HTTP/2 through `golang.org/x/net/http2.Framer` (non-deprecated low-level API; `http2.Server`/`Transport` stay banned by forbidigo). 100% of cases MUST pass for shipped features. [TQ "Conformance suites"; TS HTTP row] [NOW]
22. HTTP/1.1 cases (each asserts status, `Content-Type`, and for Node-generated errors an RFC 9457 body `{"title","status","code","requestId"}` with `requestId` = 32 lowercase hex): unmatched Route 404 `RZ-RT-001`; header block above `limits.maxRequestHeaderBytes` and below 256 KiB 431 `RZ-RT-002` problem document; above 256 KiB plain 431 before any handler; more than 256 header fields 431 `RZ-RT-002`; body above `maxRequestBodyBytes` 413 `RZ-RT-003`; differing duplicate `Content-Length` 400; invalid `Content-Length` 400; `Transfer-Encoding` other than `chunked` 501; `Transfer-Encoding: chunked` plus `Content-Length`: never forwarded with both framings (Go removes `Content-Length`, risk 7); whitespace before the colon 400; obs-fold either 400 or replaced by SP before forwarding (Go unfolds; RFC 9112 section 5.2 allows both), never forwarded with CRLF; missing `Host` 400; hop-by-hop fields (`Connection`, `Keep-Alive`, `Proxy-Connection`, `TE`, `Transfer-Encoding`, `Upgrade`, and every field named in `Connection`) removed in both directions; `Upgrade: h2c` ignored (never 101); `Expect: 100-continue`; pipelined requests answered in order; `HEAD` without body; `Connection: close` honored; `ReadHeaderTimeout` 10 s closes a slow-header connection (one case, 11 s); encoded NUL and backslash in the path rejected per OQ-security-and-identity-21 (a) with the code A4 registers; path normalization identical for Router and upstream request. [DP "Bounded resources", "Listeners and protocols", "Error response format"; SEC "Request hardening"] [NOW]
23. HTTP/2 cases: server SETTINGS advertise `MAX_CONCURRENT_STREAMS` 250, `INITIAL_WINDOW_SIZE` 65,536 (per-stream receive buffer) and `MAX_FRAME_SIZE` 16,384 (target); stream 251 concurrently open is refused (`RST_STREAM REFUSED_STREAM`); header block above 256 KiB gets plain 431; `RZ-RT-002` below it; h2c prior knowledge on the `http` listener; ALPN `h2` on `https`, fallback to HTTP/1.1 without ALPN; malformed requests (missing `:path`, uppercase field names, connection-specific fields, `TE` other than `trailers`) are stream errors, never proxied; rapid-reset (HEADERS then RST_STREAM, 10,000 streams) and CONTINUATION floods leave the Node serving other clients with `/healthz` 200; flow control: a stream holding 64 KiB unread and a connection holding 256 KiB block further DATA until WINDOW_UPDATE; a Drain sends at least one GOAWAY with `NO_ERROR` and in-flight streams complete (M1 sends one GOAWAY through `Server.Shutdown`, OQ-zero-downtime-upgrades-and-hot-reload-3 (a)); `:authority` becomes the upstream `Host`; h2 client to HTTP/1.1 Upstream translation. [DP "Bounded resources"; PBB "Constants the suite verifies" (stream 251, plain 431); ZDU "Drain timeline defaults"] [NOW]
24. TLS cases on `https`: TLS 1.3 and 1.2 per listener `tls.minVersion`; SNI certificate selection; `ruralz_listener_tls_handshake_duration_seconds` observed; handshake failure counted `ruralz_listener_connections_total{result="tls_failure"}`. [OBS "Ruralz Gateway metrics"; RM Observability row] [NOW]

### D. Property tests and fuzzing (placement and gates; targets by owning areas)

25. Every required property of TQ "Required properties" shipped in M1 is a `testing.F` target (OQ-testing-and-quality-strategy-1 (a)): canonical form reorder and YAML/JSON digest equality (A2), render idempotence with `$${` (A2), conversion round trip (A2), substitution never injects structure (A1), precedence (A2), diff laws (A2), Rate Limit admission with a deterministic clock (A5), Last-Known-Good file mode (A4). Seeds run in stage 5; each runs in the nightly Fuzz job. Rollout, Control-mode Last-Known-Good and Token Budget properties are M2/M3. [TQ "Required properties"] [NOW]
26. M1 fuzz target families: restricted YAML loader (rejections carry RZ-CFG-001 to RZ-CFG-004; memory bounded under alias bombs), env substitution and overlay merge (output validates or fails with a registered code), CEL compile and cost estimator (finite estimate, or RZ-CFG-014/RZ-CFG-015; runtime cost-limit error at 1,000,000 units, target), canonicalization (parse, canonicalize, parse: identical bytes), Router match and header handling (matches equal a reference matcher). No target may panic, hang or crash the Node. Property plus family targets count toward "25 or more by M2" (target). [TQ "Fuzzing"] [NOW]
27. Seed corpora come from the golden and negative corpora (targets load them through a shared helper, section 3). Each crasher becomes a committed seed under the target package's `testdata/fuzz/<FuzzName>/` before its fix merges. [TQ "Fuzzing"] [NOW]
28. Nightly fuzz plan: `internal/tool/fuzzplan` lists targets with `go test -list '^Fuzz' ./...`, assigns them to shards of 8 sorted by package and name (deterministic), and each shard runs `go test -run '^$' -fuzz '^<Name>$' -fuzztime 15m <pkg>` sequentially (2 hours per shard, inside the 3-hour limit). Corpus directories (`$GOCACHE/fuzz`) persist between nights as an artifact. A new crasher opens a `fuzz-crasher` issue with the failing input attached. [TQ "Test pyramid" nightly table, "Fuzzing"] [CI; NOW locally]

### E. Integration harness and State Store matrix

29. State Store integration backends: `process` (local binary: `RURALZ_TEST_REDIS_SERVER`, else `redis-server` on `PATH`; standalone, replica, TLS, ACL, three-primary cluster) and `container` (testcontainers-go v0.44.x, images pinned by digest: Redis 8, Valkey 9.0.1 or newer, Dragonfly, three-primary Redis and Valkey clusters on Linux runners). Selection by `RURALZ_TEST_STATESTORE=process|container` (default `process` when a binary is found, else `container` when Docker answers, else skip with a reason). Launchers are A8's `internal/statestore/statestoretest/redisserver`; this area wires the container flavors and CI. [TQ "Integration tests"; A8 6.4] [process: NOW (Redis 7.0.15 only, below doc floors, risk 10); container: CI]
30. Required State Store scenarios (content A8/A5, harness here): `NOSCRIPT` after `SCRIPT FLUSH` applies `failureMode` with no second round trip and reloads off path; a full post-commit queue drops with a counter and never delays the response; clustered servers: same-slot consumptive calls as one script, others sequential stopping at the first deny, never `CROSSSLOT`. Vector-search scenarios are M3. [TQ "Integration tests"] [NOW for Redis 7 standalone and cluster; others CI]
31. Round-trip test: a counting RESP proxy between Node and State Store asserts zero commands for local denials and over-limit cache hits, at most one blocking call per admitted request per Policy, one shared script when consumptive keys share a hash slot. [ADR8 "Confirmation"] [NOW]

### F. End-to-end tests (process harness)

32. The harness (section 3, `internal/testkit/...`) replaces Docker Compose: file mode with two Nodes, a `redis` State Store process, in-process mock Upstreams, a mock JWKS/token IdP over TLS, an in-process OTLP collector, an L4 balancer with `/readyz` checks, all on loopback; Nodes get distinct ports by rendering one Bundle per Node through an overlay with `${E2E_*}` port variables (same-host constraint, risk 2). Binaries are built once per test binary with the release flags (`CGO_ENABLED=0 -trimpath -ldflags -X`), or taken from `RURALZ_TEST_BIN_DIR`. [TQ "End-to-end tests"] [NOW]
33. Scenario 1 quickstart (SM-7): `examples/quickstart/quickstart.sh` goes from download to a first proxied request: fetch the archive and `SHA256SUMS` (or `RURALZ_QUICKSTART_ARCHIVE` for a local file), verify the checksum, extract, start `ruralzd` on `examples/quickstart/` and `curl` a proxied Route until 200; it prints `first proxied request after <seconds> s`. MUST be 600 s or less (target) on a clean runner; the e2e test asserts the script's exit 0 and the proxied body. [TQ scenario 1; VIS SM-7; RM exit 4] [NOW with local archive; clean machine CI]
34. Scenario 2 (REST): `shop-bundle` REST Routes, rendered with the e2e overlay (section M), serve with the Filter Chains `ruralz bundle render --effective --route <r> --output json` prints: for every REST Route the ordered `ruralz.filter.<policy>` spans received by the OTLP collector for one sampled request equal the printed rows' Policies in Phase order, and behavioral probes pass (missing JWT 401 `RZ-AUTH-001`, over-limit 429 `RZ-RL-001`/`RZ-RL-002`, CORS preflight headers, security headers set). gRPC and AI Routes are M3. [TQ scenario 2] [NOW]
35. Scenario 6: `ruralz bundle diff <admin URL> <dir>` and `ruralz bundle diff <node dump file> <dir>` exit 0 on the served Bundle and exit 1 with a `ruralz.diff.v1` document naming the injected difference (a Route `timeout` change) when the directory differs; a `--output json` golden is kept for the injected change. [TQ scenario 6; FP 9] [NOW]
36. Scenario 7 Zero-Downtime Upgrade under load: requirements 44 to 47. [TQ scenario 7] [NOW reduced; full V-3 ENV]
37. Every M1 pack section 9 command has an end-to-end test (tests authored by A10 under `test/e2e/cli_*_test.go` with this harness): `bundle validate`, `bundle render` (`--env`, `--environments`, `--effective --route`, `--api-version`), `bundle diff` (directory, file, admin URL), `bundle build` (`--offline` for Bundles with Plugins), `dev run`, `dev tap`, `node drain`, `node dump`; plus `version` and `completion`. Exit codes 0, 1, 2 and 130 are asserted where the CLI reference defines them. [RM exit 5; FP 9; A10 6.6] [NOW]
38. Hot Reload and Last-Known-Good: a directory replacement activates on both Nodes (`ruralz_config_activations_total{result="activated"}` +1, `/config/dump` digest changes); a long request started before the swap completes with 200 on its snapshot (ZG-1); an invalid Bundle changes nothing and counts `{result="rejected",code=<RZ-CFG>}` (ZG-2); a Node restarted after its source is removed boots Last-Known-Good with `ruralz_node_degraded_info{reason="lkg_boot"}` = 1 and `/readyz` 200; a corrupted Last-Known-Good candidate fails its digest (RZ-CFG-027) and the Node stays not ready (503, reason `no_revision`); an unresolvable `secretRef` keeps 503 `secrets_unresolved` with RZ-CFG-026. [ZDU "Guarantees"; FP 8.2, 8.5; HA "Detached Nodes and Last-Known-Good"] [NOW]
39. Secret leak tests (every stage 10 and stage 12 run): a canary `rzcanary-<32 hex from crypto/rand>` is injected through `secretRef` `file` (under a temporary `RURALZ_SECRET_ROOT`) and `env` (`RURALZ_SECRET_CANARY`) into an `auth.upstream-oauth2` `clientSecret`, a Consumer API key reference, and the `stateStore.url` password (redis `requirepass`); traffic includes successes and failures, and clients present the canary API key, which the mock Upstream MUST never receive (credential stripping, SEC "Request hardening"). The run fails if the canary, in raw, standard and URL-safe base64, hex, URL-encoded or JSON-escaped form, appears in: Node stdout/stderr and access logs, `/config/dump`, `/tap` output, `/metrics`, `/readyz` and `/debug/snapshots`, OTLP traces, logs and metrics received by the collector, any `ruralz` stdout/stderr (render, diff, dump, validate, dev run), or error response bodies. Designated destinations (the mock token endpoint, redis `AUTH`) are excluded; `/debug/pprof` heap profiles are excluded. [TQ "End-to-end tests", "Security scanning"; A6 6.6] [NOW]
40. Air-gapped start, process variant: the e2e topology runs inside a fresh network namespace containing only loopback (child re-exec with `SysProcAttr.Cloneflags = CLONE_NEWNET`, `lo` brought up with `SIOCSIFFLAGS`), no license file anywhere, and asserts every M1 feature path serves: each M1 Policy type on one Route, file mode, Hot Reload, Last-Known-Good, `/metrics`, `/tap`, redis State Store. Image variant (stage 12): the release image by digest on a Docker `--internal` network with only the fixtures. FIPS images join in M5. [ADR2 "Confirmation" air-gapped start; RL row 12] [process: NOW (root); image: CI]
41. T1: `ruralz dev run --ephemeral-ports examples/quickstart` reaches `ready`, proxies, Hot Reloads on edit, keeps the active Revision on a broken edit, and exits 130 on SIGINT with its temporary data directory removed; `memory` State Store. [DT "T1 and T2"; CLI "Local ruralzd launched by the CLI"] [NOW]
42. T2: two Nodes read one rendered directory each (same content, port overlay), behind the L4 balancer; CI-style atomic directory replacement (symlink swap of `RURALZ_CONFIG`, A4 req 59) activates on both within 1 s polling plus compile; `/readyz` on 9901 is the balancer probe; no mixed modes. [DT "T1 and T2", "Configuration delivery per topology"] [NOW]
43. File-mode bootstrap: at first boot a Node creates `node.id` (ULID) under `${RURALZ_DATA_DIR}/identity/`, loads `RURALZ_CONFIG`, resolves every `secretRef` and boots a valid Bundle, else Last-Known-Good, else stays not ready. [DT "File-mode bootstrap"] [NOW]

### G. Zero-Downtime Upgrade and Drain verification

44. Handover e2e (T2/T5 host): Node A (binary built with `-X version=0.1.0-e2e.a`) serves under open-loop load of HTTP/1.1 keep-alive clients honoring `Connection: close` and h2c clients honoring GOAWAY, plus a keep-alive health checker polling A's 9901 `/readyz` every 100 ms; Node B (`0.1.0-e2e.b`, same `RURALZ_CONFIG`, `RURALZ_DATA_DIR`, UID) starts; pass: B reports ready and holds the lock (`holder.json` pid = B), A exits 0 within 23 s of steering plus 5 s flush (target), zero failed requests, zero refused connections, every probe 200. Runs inside a private network namespace twice, with `net.ipv4.tcp_migrate_req` = 1 and = 0 (per-namespace sysctl; host untouched); with 0, resets are recorded, expected 0 on loopback. [ZDU "In-place handover", ZG-3; ADR15 "Confirmation" V-3] [NOW (root, reduced: 50 handovers at 1,000 rps)]
45. V-3 full: 1,000 handovers under S1 at half saturation (target) with the 9901 keep-alive checker, `tcp_migrate_req` 1 and 0, plus refused handovers: none refused or failed at 1; no OOM kill or State Store connection refusal. Refusal cases: older candidate (refused at once), unknown format marker (successor built with `go build -overlay` replacing A4's marker constant file, refused `unknown_marker`), holder draining (`draining`), 60 s acceptance timeout (successor that never becomes ready); the holder keeps serving and the successor exits 1 holding no listening socket in every case. The 60 s timeout case runs 10 times, not 1,000 (risk 8). [ZDU "Verification runbook" V-3] [ENV (RH-1) full; refusals NOW]
46. V-4 / CE-2: SIGTERM (or `ruralz node drain`) to a Node holding HTTP/1.1 and HTTP/2 connections behind the balancer: `/readyz` 503 at 0 s, `STOPPING=1` if `NOTIFY_SOCKET` set, accept window 5 s, stop accepting at 5 s with GOAWAY `NO_ERROR` and `Connection: close`, Drain deadline 25 s (work still running then ends with the A4 drain code, proposed `RZ-RT-016`, OQ-zero-downtime-upgrades-and-hot-reload-2 (b)), post-commit and telemetry flush at most 5 s, exit 0 by 30 s; zero failed new requests; reconnects spread over the Drain. WebSocket and SSE parts are M3. [ZDU "Drain timeline defaults", V-4; SDS CE-2] [NOW]
47. V-6: a handover to N-1 past the binary rollback limit is refused and N keeps serving. For `0.1.0` there is no N-1 release: the successor is HEAD built with `go build -overlay` lowering the served schema level of one field used by the active Revision (RZ-CFG-024 path); from `0.2.0` the release job uses the previous tag's binary. [ZDU V-6; RVC "Binary rollback limit"] [NOW (overlay variant); ENV (real N-1)]
48. V-1: the R1 reload ladder (requirement 81) plus each validation gate step failing (verify, compile, secret resolution) changes nothing; three quick activations over 20,000 sessions are M3 (sessions). [ZDU V-1] [NOW functional; ENV budgets]

### H. Chaos experiments, failure rows and game days

49. Harness: `test/chaos/` (tag `e2e`, run by `make chaos` in the nightly Chaos job), at least 10 Nodes (target) behind the L4 balancer, a `redis` State Store behind a TCP fault proxy (per Node when partitions are needed), Upstream Endpoints behind fault proxies, and open-loop load (never closed loop). Faults come from process signals (SIGKILL, SIGTERM, SIGSTOP/SIGCONT for pause), fault proxy modes (delay, black-hole, reset, refuse), and redis commands (`REPLICAOF NO ONE`, `SCRIPT FLUSH`, `CONFIG SET maxmemory`) (OQ-scalability-and-distributed-state-10 (a)). Every degraded state asserted MUST be visible as a metric, else the scenario fails (P10). [SDS "Chaos experiments"; TQ "Chaos testing"] [NOW at 10 Nodes and scaled rates; CI at target rates]
50. CE-1: SIGKILL one Node at 50% load (target): errors only on requests in flight on that Node (the load generator tags each request with its backend via the balancer's connection record); no Quota under-charge: for a `quota` Policy (`unit: requests`) the State Store counter at the end is ≥ the count of 2xx responses admitted by that Policy. [SDS CE-1; HA FC-1, GD-1] [NOW]
51. CE-2: requirement 46 at 10 Nodes, one Node SIGTERMed. [SDS CE-2] [NOW]
52. CE-3: 200 ms added to every State Store call (target), `stateStoreTimeout` 50 ms: p99 rises at most one timeout, then returns within 10% of baseline once breakers open (breaker: 50% of at least 20 calls in 5 s, target); `ruralz_node_degraded_info{reason="state_store_breaker_open"}` = 1 while open. [SDS CE-3; SG-2, SG-4] [NOW]
53. CE-4 / GD-3: State Store black-holed for 60 s (target): per key admissions within the fail-open bounds: file mode without a declared ceiling at most N_serving × `requests` per window; with a declared ceiling c at most N_serving × c (target); a `ratelimit` Policy with `failureMode: closed` returns 503 `RZ-STS-001` (timeout) then `RZ-STS-003` (breaker open) (M1 substitute for `closed` Token Budgets, which are M3); `ruralz_ratelimit_decisions_total{result="fail_open"}` rises; `ruralz_state_calls_total{result="timeout"}` then `{result="skipped"}`. [SDS CE-4; HA GD-3, "Local rate-limit fallback"] [NOW]
54. CE-5: State Store primary killed under at least 100 Nodes (target): every Node reconnected within 10 s and `failureMode` for at most 15 s (target); extra admissions within the lag bound L × rate (hypothesis). Failover is scripted: the replica gets `REPLICAOF NO ONE` and the fault proxy (the stable `stateStore.url` address) switches upstream to it. [SDS CE-5; ZDU V-7; HA FC-12] [NOW at 10 Nodes; CI at 100 Nodes (larger runner, about 9 GiB)]
55. CE-6: State Store filled to `maxmemory` (for example 64 MiB, `noeviction`): stores skip above 70% (`ruralz_cache_store_skipped_total{reason="memory"}` rises), no limit key evicted (`INFO stats` `evicted_keys` stays 0 and every `rz:rl:` key sampled before remains); a store with an eviction policy raises `state_store_eviction_policy`. [SDS CE-6; HA FC-13] [NOW]
56. CE-12: hot key: GCRA calls for the key at most 2 × N_serving × ceiling per window (counted by the RESP proxy), none when `config.localOnly` (only if OQ-scalability-and-distributed-state-11 (a) lands `localOnly` in M1); shard script CPU under 70% (target, from `INFO cpu` deltas); no co-tenant breaker opens. Target offered load 1,000,000 calls per second is not reachable locally; the bound is checked at scaled rates. [SDS CE-12] [NOW scaled; CI/ENV full]
57. CE-15: key-rotation flood, 100,000 new keys per second (target): per key at most N_serving × ceiling per window; first-seen GCRA calls per Node within min(500, B / N_published) per second, else 200 per second without a published count (file mode) (target); no key with a GCRA answer evicted from the 1,048,576-entry table (`ruralz_ratelimit_bucket_evictions_total` only for keys without one). [SDS CE-15; PBB "Constants" rate-limit key table] [NOW scaled (5,000 keys/s); CI full]
58. CE-16: cache stores saturate the post-commit queue (State Store delayed 500 ms through the proxy) during invalidations (unsafe methods bumping generations): zero `cache_invalidate` drops (`ruralz_state_writes_dropped_total{kind="cache_invalidate"}` stays 0) while `cache_set` drops are counted; "Plugin writes drain" is M2. Queue capacity 64,000 items and 85 MiB (target). [SDS CE-16, "Post-commit write queue"] [NOW]
59. TQ row "Upstream Endpoints reset or slowed": fault proxies reset one of three Endpoints and delay another by 2 × `perTryTimeout`: outlier ejection (`ruralz_upstream_ejections_total`), budgeted retries (`ruralz_upstream_retry_budget_exhausted_total` bounded), breaker open (`ruralz_upstream_breaker_state_info{state="open"}`) with 503 `RZ-UP-005`, and the expected `RZ-UP-001`, `RZ-UP-003`, `RZ-UP-004`, `RZ-UP-007` codes by case. [TQ "Chaos testing"; A5 req 37, 40] [NOW]
60. FC rows: FC-16 JWKS unreachable: cached keys serve until expiry, then 401 `RZ-AUTH-006`, `jwks_stale` raised (expiry shortened via the IdP's `Cache-Control: max-age` at the 5-minute clamp minimum); FC-19 DNS failure: discovery keeps its last Endpoint set, `discovery_stale` after 3 refreshes (A5 stdlib DNS responder); FC-20 file mode: an invalid Revision reaches no Node (rejected), a valid-but-wrong one reaches all (documented, not a failure); FC-21 `file` secret missing at cold start: not ready; running Nodes keep values. [HA "Failure catalog"] [NOW]
61. GD-1 to GD-5 automated drills (nightly Chaos job): GD-1 = CE-1 plus the killed Node restarted and `/readyz` 200 within 30 s (target); GD-2 zone loss: Nodes split into two "zones"; one zone's Nodes (via their balancer-side fault proxies) and the State Store primary black-holed: survivors under 80% CPU (target, `/proc/<pid>/stat` sampled), State Store `failureMode` 15 s or less (target) after scripted failover; GD-3 = CE-4; GD-4 partition: half the Nodes cut from the State Store: cut-off Nodes within the fail-open ceiling rows, connected ones within the healthy GCRA row (`requests` per window sustained, 2 × `requests` in one window) (target); GD-5 rebuild: kill the redis process and delete its data, start a replacement at the same address per HA "State Store rebuild": Nodes redial with full jitter (base 100 ms, cap 5 s), `SCRIPT LOAD`, breakers close after 3 successful probes, traffic RTO met (30 minutes or less, target; observed seconds), no `state_store_eviction_policy` afterward, `ruralz_state_writes_dropped_total` back to baseline. Human game days on staging are quarterly and need a staging environment (risk 12). [HA "Game days", "State Store rebuild"] [NOW automated; ENV human]

### I. Benchmarks, budgets and gates

62. Microbenchmarks (stage 9 and `pr-full`): `go test -bench -benchmem` benchmarks owned by their areas cover Router match, Filter Chain executor, CEL, canonicalization and validation (A4, A3, A2, A1), plus A5's `BenchmarkLegPooled`, `BenchmarkRateLimitLocal`, `BenchmarkRateLimitMemoryGCRA`, `BenchmarkBreakerAccounting`. The gate list is data (`test/bench/allocgate.json`), so adding a benchmark never edits the tool. [PBB "Suite layers"; TQ "Benchmarks and regression gates"] [NOW]
63. PB-8: `testing.AllocsPerRun` around Router and Filter Chain executor for an S1 pass-through request (pre-parsed HTTP/1.1 request, discard `ResponseWriter`, A4 hook) MUST be 30 or fewer Ruralz-owned allocations (target), `net/http` internals excluded; per-stage allocation budgets (Upstream layer 6, `onResponse` 2, response write 2, `onLog` 10, others 0; S1 rows sum to 20) are asserted where A4/A5 expose a stage hook. [PBB "Budget catalog" PB-8, "Per-stage latency budget"] [NOW]
64. alloc/op gate algorithm (`internal/tool/benchgate`): create a worktree of the merge base (`git merge-base HEAD origin/<base>`), build both test binaries (`go test -c`) with the release toolchain, then run base and head alternately A, B, A, B … 10 runs each (target) with `GOGC=off`, `GOMAXPROCS=4` and `-test.cpu 4`, `-test.benchtime <N>x` per benchmark (default 2000x), `-test.benchmem`, `-test.run '^$'`; parse `allocs/op` and `B/op`; fail when the head median allocs/op exceeds the base median by more than 3% (target), or exceeds a configured absolute cap (PB-8's 30). One extra allocation at 30 is 3.3% and fails. Output: a table per benchmark and exit 1 on failure, 2 on tool error. [PBB "Regression policy and gates", "Statistics and runners"; TQ "Benchmarks and regression gates"] [NOW]
65. A new benchmark with no base counterpart passes but is reported; a removed benchmark fails unless listed in the pull request's `allocgate.json` change. [proposed] [NOW]
66. Overrides need recorded maintainer approval (a `perf-override` label applied by a maintainer, checked by the job); loosening a value edits PBB (BP-7). [PBB "Regression policy and gates"] [CI]
67. OQ-testing-and-quality-strategy-2 (c): alloc/op runs on shared runners, latency only on RH-1. [PBB "Statistics and runners"] [NOW]
68. Size and idle RSS gate (`internal/tool/sizegate`, stage 9): build `ruralzd` for linux/amd64 and linux/arm64 with the release flags plus `-s -w`; fail when a stripped binary exceeds 160 MiB (167,772,160 bytes) (target). Start linux/amd64 `ruralzd` with telemetry defaults, no Revision (`RURALZ_CONFIG` pointing at an empty directory), no connections, for 120 s (idle settle, target); sample `VmRSS` from `/proc/<pid>/status` every second over the last 10 s; fail when the maximum exceeds 89 MiB (93,323,264 bytes) (target). Report both values in the job summary. [PBB "Memory budget", "Regression policy and gates"; TS library catalog paragraph (OQ-performance-budgets-and-benchmarking-5 (a))] [NOW]
69. Scenario Bundles live in `test/bench/scenarios/<ID>/` and use only CM fields; S2 is PBB's Bundle verbatim (fake hostnames rewritten by an overlay to the lab hosts); S1 drops the Gateway Policies; S3, O1 and C2 add an `https` listener on 8443; S5 and O2 overlays set `stateStore {driver: redis, url: secretRef env RURALZ_STATE_STORE_URL}`; O2 sets `stateStoreTimeout: 1s`; S5x sets `10ms`. Every scenario Bundle is validated in stage 5 and renders to a fixed Revision whose digest is recorded. [PBB "Scenarios"] [NOW]
70. Scenario definitions (all open loop, HTTP/1.1 on 8080 unless noted; latency budgets are gateway-added): S1 GET, 1 KiB response from the mock, 256 keep-alive connections (target), no Filter Chain; S2 as S1 with `auth.jwt` (RS256 2,048-bit, generator rotating 10,000 tokens from two keys, no verified-token cache) and `ratelimit` (key `request.headers["x-bench-key"]` cycling 100,000 values uniformly, `limits: [{requests: 20000, window: 1s}]`) at the Gateway, `memory` driver, `traceSampling` 0.01, every request access-logged, OTLP to RH-1U; S3 as S2 with 64 connections of up to 64 streams over HTTP/2 TLS 1.3 on 8443; S5 `ratelimit` on `redis` (Valkey on RH-1S); S5x as S5 with 500 µs ± 100 µs normal delay each way on RH-1S and `stateStoreTimeout: 10ms`; O1 S3's chain on 120 connections of 250 streams, O1a mock delay 1 s at 30,000 rps, O1b no delay at 42,000 rps; O2 S5 on 12,000 of C1's connections at 24,000 rps with every State Store reply delayed 500 ms, `stateStoreTimeout: 1s`; C1 20,000 idle connections, cleartext and TLS runs; C2 S3 at half saturation then 10,000 new TLS connections within 2 s; R1 reload ladder; G1 open-loop GCRA `EVALSHA` through the State client until p99 > 1 ms or script CPU 70%, plus a variant with 10 connections per emulated Node for 1,000 Nodes. [PBB "Scenarios"] [scenario code NOW; gating runs ENV]
71. Budgets that gate on RH-1 (target unless marked): PB-1 gateway-added p99 1 ms or less, S1 at 10,000 rps per core (40,000 rps on the 4-core cpuset); PB-2 p99 1 ms or less and PB-3 p50 150 µs or less, S2 at half saturation (16,000 rps); PB-4 S1 saturation 50,000 rps or more on 4 vCPU; PB-6 base RSS 100 MB + 2 KB × R; PB-7 Hot Reload of 5,000 Routes 500 ms or less; PB-8 30 allocations; PB-9 same-zone GCRA p99 1 ms or less (hypothesis), S5; PB-10 telemetry CPU 5% or less of Node CPU at defaults, S2 on/off; PB-11 O1b goodput 90% or more of saturation, excess only as `RZ-RT-005`; PB-14 cross-zone GCRA p99 2 ms or less (hypothesis), S5x; PB-15 S2 p99 1 ms or less at 60% (19,200 rps); PB-16 S2 p99 2 ms or less at 80% (25,600 rps). Throughput: S1 50,000, S2 32,000, S3 28,000, S5 40,000 rps or more. Resources at S1 40,000 rps: CPU per 10,000 rps S1 0.8, S2 1.25, S3 1.42 vCPU; GC 10% or less of Node CPU at `GOGC=100`; loaded RSS with 256 connections 256 MiB or less; 99.9% pooled upstream requests after warm-up; TLS 1.3 ECDSA P-256 handshake CPU 1 ms p99. Per-stage p99 sum for S2 at most 400 µs (285 µs planned). [PBB "Budget catalog", "Throughput targets", "Resources per core", "Per-stage latency budget"] [ENV]
72. Scenario pass criteria: O1 admitted plus `RZ-RT-005` within 0.5% of offered, zero `RZ-UP-006`, active requests plateau at 20,000, O1a rejects at p99 1 ms or less, O1b meets PB-11; O2 State Store calls in flight plateau at 8,192, excess skipped and counted in `ruralz_state_calls_total{result="skipped"}`, bounded goroutines, skipped requests add 1 ms or less at p99; C1 within connection memory rows (idle cleartext 24 KiB, idle TLS 96 KiB); C2 existing p99 5 ms or less, handshake p99 1 s or less. [PBB "Scenarios"] [ENV; functional variants NOW]
73. Measurement rules: saturation = highest open-loop step whose achieved rate stays within 0.5% of offered with end-to-end p99 10 ms or less and errors 0.01% or less; rate ladder in 5% steps from 50% to 110% of expected saturation, 60 s each (13 steps); warm-up 60 s discarded, extended in 30 s steps to 180 s until the p99 of three consecutive 10 s windows varies by 5% or less; five measured runs of 180 s per scenario and commit, interleaved A, B, A, B with the baseline; offered rates fixed (S1 40,000, S2 16,000 rps); percentiles p50, p90, p99, p99.9, max from HdrHistogram-compatible histograms at 3 significant digits; a run is valid only if achieved rate is within 0.5% of offered, generator and mock CPU stay below 70%, mock p99 beyond its delay is 200 µs or less, and `ruralz_telemetry_logs_dropped_total` does not increase; baseline p99 coefficient of variation 2% or less, else inconclusive (re-run once next night, then an issue). Both external (from intended send time) and internal (`ruralz_http_gateway_duration_seconds`, access-log `gateway_duration`) views gate. [PBB "Throughput targets", "Open-loop load and coordinated omission", "Warm-up, duration and percentiles", "Measurement points", "Reproducibility"] [ENV]
74. Macro p99 gate: median-of-runs p99, external or internal, above the baseline by more than 5% (target) with four of five pairs slower fails; baseline = the same scenario's previous run re-run interleaved; for `release`, the previous release on the same hosts. Absolute gate: any budget, throughput target, scenario criterion or constants-table row missed fails. [PBB "Regression policy and gates"] [ENV]
75. Latency job schedule (M1 rotation, every item every four nights, target): nightly S1 and S2 A/B (80 min) + component benchmarks (15) + one slot (75) + reserve (10) = 180 min; slot 1 S3 A/B, O1a, O1b, C2, S3 ladder; slot 2 S5 A/B, O2, G1, S5 ladder; slot 3 S5x A/B, C1 cleartext and TLS, PB-15 and PB-16 runs; slot 4 S1 and S2 ladders, R1 idle and loaded, constants runs. [PBB "Suite layers"] [ENV]
76. Load tools (OQ-performance-budgets-and-benchmarking-1 (a)): oha v1.16.0 primary (`-q` fixed rate with `--latency-correction`), vegeta cross-checks S1 (`-rate`), fortio for S7 (M3). They run as pinned, checksum-verified binaries (never in `go.mod`), after CI tooling rows land. The in-repo Go open-loop generator (`internal/testkit/loadgen`) drives chaos, e2e, smoke and G1-style State client load, never a gating latency number. [PBB "Open-loop load and coordinated omission"] [ROW, ENV]
77. Result record: every macro and component run writes one JSON record (schema `test/bench/record.schema.json`, section 3) with Identity (scenario, commit, baseline, Revision digest, toolchain), Environment (hardware profile and microarchitecture, kernel, cpuset, `GOMAXPROCS`, `GOGC`, `GOMEMLIMIT`, sysctls including the local port range, NIC settings), Load (tool, version, command line, offered and achieved rate, connections, key cardinality and distribution, injected delay distribution), Latency (external and internal percentiles, raw histogram logs), Resources (CPU per request, GC CPU share, RSS at load and idle settle, live heap, alloc/op), Validity and budgets (rate error, generator and mock CPU, drops, noise, verdict; per budget value, result, SLO, verdict). Nightly and release runs publish records, raw histograms and CPU profiles (never heap profiles) from the repository (P10), and release notes list every budget with its result. [PBB "Result record", "Publication"] [NOW (format); ENV (data)]
78. Budget runs use `GOGC=100` without `GOMEMLIMIT`; an informational run holds C1 over TLS plus S2 in a 2.5 GiB container with `GOMEMLIMIT` at 90%. [PBB "Memory budget"] [ENV]
79. Soak (release only, RH-1): S2 at half saturation for 2 hours: RSS drift 2% or less after 10 minutes, flat goroutines (`ruralz_runtime_goroutines`). [PBB "Suite layers"] [ENV]
80. Component benchmarks (RH-1 nightly Latency job; code NOW): loader compile ladder, JWT per algorithm (RS256 about 40 µs CPU, hypothesis), token bucket shards, `RZ-RT-005` rejection (mean CPU 16 µs or less, target), metric aggregates at `-cpu 4,32`, CEL typical match and key expressions under 2 µs p99 (target). [PBB "Suite layers", "Per-stage latency budget" rules] [NOW code; ENV gating]
81. R1 ladder generator (`test/bench/gen`): Bundles of R Routes, R/10 Upstreams, R/20 Policies and one Gateway (R = 100, 1,000, 5,000, 10,000, 17,000 → 116, 1,151, 5,751, 11,501, 19,551 resources): exact and template paths, `auth.jwt` and `ratelimit` at the Gateway, `authz.cel` on one Route in twenty, no Plugins; two alternating Revisions differ in every Route's `timeout` and in one Upstream in ten's Endpoint (R/100 new pools). Budgets per size (target): cold start to `/readyz` 200 300 ms / 600 ms / 1.6 s / 3 s / 5 s; from Last-Known-Good 200 / 300 / 700 ms / 1.3 s / 2.2 s; Hot Reload idle 50 / 125 / 500 ms / 1 s / 1.7 s; snapshot live heap 50 KB / 500 KB / 2.5 MB / 5 MB / 8.5 MB; peak configuration memory 200 KB / 2 MB / 10 MB / 20 MB / 34 MB. Stage budgets at 5,000 Routes from `ruralz_config_activation_duration_seconds{stage}`: `verify` 25 ms, `compile` 400 ms, `plugin_compile` 0, `swap` 25 ms, `total` 500 ms; under S1 at half saturation reload 1 s or less (target), gateway-added p99 1.5 ms or less (hypothesis). Loader workers W = max(1, `GOMAXPROCS`/2) yielding every 100 µs (OQ-performance-budgets-and-benchmarking-6 (a)). [PBB "Startup and reload time by configuration size", "Hot Reload stages"] [NOW smoke (non-gating); ENV gating]
82. Constants the suite verifies (M1 rows; Plugin rows M2): 20,000 client connections and in-flight units (C1, C2, O1); 250 streams, 256 KiB header block, 1 MiB chunk (frame-limit run: stream 251 refused, plain 431, `RZ-RT-013`); 2,000 pre-routing writes with a 5 s deadline (2,500 clients never reading `RZ-RT-001`: extra handlers abort); per-connection memory about 592 KiB (slow-header and HTTP/2 window flood, 20,000 connections); K = 2 retired snapshots and 30 s grace (ten activations per second under S2: zero failed unary requests, peak within (K + 2) × snapshot); 512 MiB buffered bytes (body flood: `RZ-RT-004`, no OOM); rate-limit key table 1,048,576 entries about 64 MiB, local-only segment 131,072, first-seen budget (CE-15, CE-12); balancer 256 MiB and hot-entry 64 MiB (`ring-hash` ladder, cache key flood); Quota denial cache 65,536 entries (100,000 denied Quota keys); post-commit queue 64,000 items and 85 MiB (CE-16); 8,192 State Store calls in flight and 2 + 8 connections per shard (O2, G1); span queue 8,192 and access-log queue 8,192 records and 4 MiB (S2 at both trace caps). [PBB "Constants the suite verifies"] [functional at reduced scale NOW; full ENV]

### J. Topology T5: systemd unit and helper

83. Ship `deploy/systemd/ruralzd.service`, `deploy/systemd/ruralzd-handover` (POSIX sh helper), `deploy/systemd/ruralzd.env` (example `EnvironmentFile`) and `deploy/systemd/99-ruralzd.conf` (sysctl drop-in `net.ipv4.tcp_migrate_req = 1`), also inside every linux `ruralzd` archive under `deploy/systemd/`. This answers OQ-zero-downtime-upgrades-and-hot-reload-7 with (a); the directives below are the research-spike proposal, verified by requirement 86 before `0.1.0`. [ZDU "In-place handover"; DT "T5"] [NOW (files, offline verify); CI (live)]
84. Unit content (proposal):

    ```ini
    [Unit]
    Description=Ruralz Gateway (ruralzd)
    Documentation=https://github.com/ravindu-rev/ruralz/blob/main/docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
    Wants=network-online.target
    After=network-online.target

    [Service]
    Type=notify
    NotifyAccess=all
    User=ruralz
    Group=ruralz
    EnvironmentFile=-/etc/default/ruralzd
    Environment=RURALZ_DATA_DIR=/var/lib/ruralz RURALZ_CONFIG=/etc/ruralz-bundle
    Environment=RURALZ_ADMIN_TOKEN_FILE=%d/admin-token
    LoadCredential=admin-token:/etc/ruralz-admin/admin-token
    StateDirectory=ruralz
    StateDirectoryMode=0700
    ExecStart=/usr/local/bin/ruralzd
    ExecReload=/usr/local/lib/ruralz/ruralzd-handover
    KillMode=control-group
    KillSignal=SIGTERM
    TimeoutStartSec=90
    TimeoutStopSec=40
    Restart=on-failure
    RestartSec=2
    LimitNOFILE=65536
    NoNewPrivileges=yes
    CapabilityBoundingSet=
    ProtectSystem=strict
    ProtectHome=yes
    PrivateTmp=yes
    PrivateDevices=yes
    ProtectKernelTunables=yes
    ProtectKernelModules=yes
    ProtectControlGroups=yes
    RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
    RestrictNamespaces=yes
    RestrictRealtime=yes
    LockPersonality=yes
    SystemCallArchitectures=native

    [Install]
    WantedBy=multi-user.target
    ```

    Rules behind it: `TimeoutStopSec` 40 s MUST exceed the 30 s exit bound; `systemctl stop` is a Drain; `RURALZ_SECRET_ROOT` keeps its default `/etc/ruralz`, so the Bundle (`/etc/ruralz-bundle`), data directory and the admin token (credentials directory) lie outside it (SEC "Secrets" rule 5); `MemoryDenyWriteExecute` is deliberately absent (wazero compiler mode, M2); `KillMode=control-group` makes `systemctl stop` during a handover Drain both processes. [DT "T5"; ZDU "Drain timeline defaults"; SEC "Secrets"] [NOW]
85. Helper behavior (`ExecReload`, so `systemctl reload ruralzd` performs a Zero-Downtime Upgrade after the operator replaced `/usr/local/bin/ruralzd`): start `${RURALZ_BINARY:-/usr/local/bin/ruralzd}` in the background inside the unit's cgroup with the unit's environment and stdio (journal); poll every 0.5 s until `${RURALZ_DATA_DIR}/holder.json` names the new PID (accepted: exit 0), the new process exits (refused: exit 1 with its status), or 75 s elapse (60 s handover timeout plus margin: send SIGTERM to the new process, exit 1). `ruralzd` sends `READY=1` after its first boot attempt, `STOPPING=1` at Drain start, and, as successor after taking the lock, `MAINPID=<own pid>` then `READY=1` (A4 req 8; risk 9 on readiness timing). [ZDU "In-place handover" steps 1, 4, 7] [NOW (file); CI (live)]
86. Verification: `systemd-analyze verify` on the unit (with `ExecStart` rewritten to a built binary path) runs in stage 8 [NOW]; a live job on a GitHub-hosted Ubuntu runner (systemd PID 1, `sudo`) installs the unit, starts it (reaches `active (running)` with a valid Bundle and with none), runs 20 `systemctl reload` handovers under load with the 9901 keep-alive checker (zero failed requests, `MainPID` equals the successor after each, old PID exits by 28 s), a refused handover (`systemctl reload` exits non-zero, service stays active on the old PID), `systemctl stop` exits within 40 s with exit status 0, and `systemctl kill -s SIGKILL` followed by automatic restart boots Last-Known-Good. [ZDU V-3, V-4; ADR15 "Consequences" (systemd unit needed)] [CI]
87. Balancer rules documented with the unit (T2/T5): `/readyz` checks on 9901, TLS passthrough, no stickiness, idle timeout above 120 s, slow start 30 to 60 s, detection within 5 s (probe interval × unhealthy threshold). [DT "Network and ports"; ZDU "Drain timeline defaults"] [NOW (docs)]

### K. Container image

88. Image `ghcr.io/ravindu-rev/ruralzd` for linux/amd64 and linux/arm64, built by stage 12 from the exact released binaries (never rebuilt inside Docker), `deploy/container/Dockerfile`:

    ```dockerfile
    ARG CA_SOURCE=golang:1.27.1-bookworm@sha256:<pinned>
    FROM --platform=$BUILDPLATFORM ${CA_SOURCE} AS rootfs
    RUN mkdir -p /out/etc/ssl/certs /out/var/lib/ruralz /out/etc/ruralz /out/etc/ruralz-bundle \
     && cp /etc/ssl/certs/ca-certificates.crt /out/etc/ssl/certs/ca-certificates.crt \
     && chown 65532:65532 /out/var/lib/ruralz && chmod 0700 /out/var/lib/ruralz
    FROM scratch
    ARG TARGETOS TARGETARCH VERSION COMMIT
    COPY --from=rootfs /out/ /
    COPY dist/${TARGETOS}_${TARGETARCH}/ruralzd /usr/local/bin/ruralzd
    COPY dist/notices/ruralzd_${TARGETOS}_${TARGETARCH}/ /usr/share/doc/ruralzd/
    ENV RURALZ_DATA_DIR=/var/lib/ruralz RURALZ_CONFIG=/etc/ruralz-bundle
    USER 65532:65532
    EXPOSE 8080/tcp 8443/tcp 9901/tcp
    LABEL org.opencontainers.image.source="https://github.com/ravindu-rev/ruralz" \
          org.opencontainers.image.licenses="Apache-2.0" \
          org.opencontainers.image.version="${VERSION}" org.opencontainers.image.revision="${COMMIT}"
    ENTRYPOINT ["/usr/local/bin/ruralzd"]
    ```

    The image ships a CA bundle (OQ-tech-stack-and-libraries-25 (a)); archives read the host store; no roots are embedded. `/usr/share/doc/ruralzd/` holds `LICENSE`, `NOTICE` and `THIRD_PARTY_LICENSES` including the Mozilla CA list's MPL-2.0 data notice. Built with the runner's `docker buildx` CLI, `SOURCE_DATE_EPOCH` = commit time and `rewrite-timestamp=true` for reproducible layers; UDP 8443 is exposed from M3. [RVC "Release artifacts"; ADR2 release audit; TS OQ-tech-stack-and-libraries-25] [CI]
89. Tags: `X.Y.Z` and `X.Y` for a release (never re-pushed: the job fails if `X.Y.Z` exists), `X.Y.Z-rc.N` only for a release candidate, never `latest`; deployments SHOULD pin by digest. [RVC "Release artifacts"] [CI]

### L. Release 0.1.0

90. Versioning: tag `v0.1.0` on `github.com/ravindu-rev/ruralz`; binaries embed `version=0.1.0` (tag without `v`), full `commit`, `flavor=default`; a signed release candidate `v0.1.0-rc.N` at least 2 weeks earlier (target). One tag builds every artifact. [RVC "One product version", "Release cadence"] [CI]
91. Binary archives (M1): `ruralzd_<version>_<os>_<arch>.tar.gz` for linux/amd64, linux/arm64 (production) and darwin/arm64, darwin/amd64 (development only); `ruralz_<version>_<os>_<arch>.tar.gz` for linux/amd64, linux/arm64, darwin/arm64, darwin/amd64 and `ruralz_<version>_windows_amd64.zip`. No `ruralz-control` archive or image until M2. Each archive holds one top directory `<name>_<version>_<os>_<arch>/` with the binary, `LICENSE`, `NOTICE`, `THIRD_PARTY_LICENSES` (generated from that artifact's own linked packages, stating where MPL-2.0 source is available; none for `ruralzd` and `ruralz` in M1) and `README.md`; linux `ruralzd` archives add `deploy/systemd/*`. [RVC "Release artifacts"; ADR2 "Confirmation" release audit] [NOW (dry run)]
92. Archives are deterministic: entries sorted, modification time = commit time (`git log -1 --format=%ct`), uid/gid 0 with empty names, modes 0755 (binaries, helper) and 0644 (others), gzip header without name and mtime; zip entries with the same fixed time. A rebuild job on a fresh runner MUST reproduce identical binaries and archives (SHA-256 compare) (target). [RVC "Release artifacts"; RL "Toolchain and modules" (no build date)] [NOW]
93. `SHA256SUMS`: one line per archive and per published schema file, `<64 lowercase hex><two spaces><file name>`, sorted by file name, LF, final newline (sha256sum-compatible). [RVC "Release artifacts"] [NOW]
94. JSON Schema assets: `ruralz-v1alpha1-authoring.schema.json` and `ruralz-v1alpha1-rendered.schema.json` copied byte for byte from `api/schema/ruralz/v1alpha1/`; `$id` stays per OQ-repository-layout-and-conventions-4 (recommended (b), non-blocking). OpenAPI is M2. [RVC "Release artifacts"] [NOW]
95. SBOM: CycloneDX 1.7 JSON per archive (`<archive>.cdx.json`) and per image, each also attached as an attestation (`predicate-type` CycloneDX); SPDX is not shipped in `0.1.0` (OQ-release-versioning-and-compatibility-3 (a)). [RVC "Release artifacts"; SEC "Release supply chain"] [ROW, CI]
96. Signatures: keyless Sigstore bound to the release workflow's OIDC identity, logged in Rekor v2, as Sigstore bundles: `SHA256SUMS.sigstore.json` (covering every archive and schema file) and image signatures as OCI referrers; the chart is M2. Verification instructions in the release notes MUST require cosign 3.1.3 or newer on 3.x, or 2.6.5 or newer on 2.x, with `--certificate-identity-regexp '^https://github.com/ravindu-rev/ruralz/.github/workflows/release-build.yml@refs/tags/v'` and `--certificate-oidc-issuer https://token.actions.githubusercontent.com`. [RVC "Release artifacts" bullets] [ROW, CI]
97. Provenance: SLSA Build Level 3 attestation per archive and per image through GitHub artifact attestations produced inside a reusable workflow (`release-build.yml`, `workflow_call`) that both builds and attests. [RVC "Release artifacts" bullets; RES licensing-landscape (GitHub attestations L3 with reusable workflows)] [ROW, CI]
98. "100% of artifacts signed" (SM-12, exit 1) is checked by a final `verify` job: `cosign verify-blob --bundle SHA256SUMS.sigstore.json SHA256SUMS`, `sha256sum -c SHA256SUMS`, `cosign verify` of every image digest, `gh attestation verify` of every archive and image for provenance and SBOM; any failure blocks publication. [VIS SM-12; RM exit 1] [CI]
99. Release notes (`internal/tool/releasekit notes`): Features, Fixes, Deprecations, Upgrade notes, Schema, ABI and Control Stream additions (from Conventional Commits since the previous tag; for `0.1.0` since the root), every Performance Budget with its result from the release run, verification instructions, and a "Skipped gates" section listing any skipped gate with its approver. [RVC "Release artifacts"; PBB "Publication"; TQ "Release gates"] [NOW]
100. Release audit per tag (ADR2): the repocheck no-license-check scan finds 0 gated features (SM-1); every archive and image carries `LICENSE`, `NOTICE` (upstream attributions, the EDL-1.0 election) and its own `THIRD_PARTY_LICENSES`; the depgate G2/G3 checks pass per shipped binary and platform. [ADR2 "Confirmation"; VIS SM-1] [NOW]
101. Publication: a GitHub Release `v0.1.0` (draft until every gate and verification passes, then published) with archives, `SHA256SUMS`, `SHA256SUMS.sigstore.json`, `*.cdx.json`, schema files and notes; images pushed by digest before tagging. [RVC "Release artifacts"] [CI]
102. Security fix policy and support windows start at `0.1.0`: `SECURITY.md` channel acknowledged within 2 business days (target); lines per RVC "Release cadence and support windows". No code beyond release notes templates. [RVC "Security fix policy"] [NOW (docs)]
103. `make release` (stage 12) = `releasekit` build, package, notices, checksums, SBOM, notes, verify manifest; `make release-dry-run` does everything without signing, attesting or publishing and is runnable locally. [RL row 12] [NOW (dry run)]
104. Release job permissions: build and attest jobs `id-token: write`, `attestations: write`, `contents: read`; image job adds `packages: write`; publish job `contents: write` only. Runs in a GitHub Environment `release` with required reviewers. [RVC "Release artifacts" (SLSA L3 isolation)] [CI]
105. Release gates on the candidate commit: all CI stages green (with floor and release jobs); golden corpus byte-exact except recorded changes; conformance 100% of cases for shipped features; zero open `fuzz-crasher` issues and every fixed crasher has a regression seed; every M1 chaos scenario and CE experiment passed on the candidate commit; benchmarks within 5% p99 and 3% alloc/op of the previous release (no previous release for `0.1.0`: absolute budgets only); every security scanning check green. Skipping a gate needs a release-notes entry and approval by a maintainer from outside the author's team (Environment reviewers). [TQ "Release gates"] [CI; benchmarks ENV]

### M. `examples/`

106. `examples/shop-bundle/` is CM's complete annotated example verbatim (every file of CM "Complete annotated example Bundle"); `examples/control/environments.yaml` and `examples/control/clusters.yaml` are its `control/` files, a sibling of the Bundle root as in CM "Bundle layout and merge" (inside the root they would be RZ-CFG-017). [CM; A2 golden] [NOW]
107. `examples/quickstart/ruralz.yaml`: one Gateway (`http` 8080, admin 9901), one Route `GET /healthz` exact to an Upstream `self` with static Endpoint `127.0.0.1:9901` (the Node's own unauthenticated `/healthz`), `memory` State Store; `quickstart.sh` sets `RURALZ_FETCH_ALLOW=127.0.0.1/32` (OQ-security-and-identity-22 (a)); README shows how to point the Upstream at a real service. This needs no upstream process on a clean machine (risk 4). [VIS SM-7] [NOW]
108. `examples/file-mode/`: a T2/T5 Bundle: `http` and `https` listeners (`file` secretRefs under `/etc/ruralz/tls`), `redis` State Store via `secretRef env RURALZ_STATE_STORE_URL`, `auth.jwt`, `ratelimit`, `quota`, `cors`, `headers` (security headers, `overridable: false`), `cache`, two Upstreams with retries, breaker and health checks, overlays `staging` and `prod` using the Environments of `examples/control/environments.yaml`; its README carries the systemd install steps. [DT "T1 and T2", "T5"] [NOW]
109. Every example validates in stage 5 (A1 meta test) and stage 8 (`ruralz bundle validate` per Environment), and `examples/file-mode` plus the quickstart run in e2e. [RL rows 5, 8] [NOW]

## 3. Proposed Go packages and API

Placement: shared test support under `internal/testkit/` (importable by `_test.go` files anywhere and by `test/...`), suites under `test/`, CI programs under `internal/tool/` (standard library only, never linked into a binary). Proposed depguard rule `testkit`: `files: ["$all", "!$test", "!**/internal/testkit/**", "!**/test/**"]`, deny `github.com/ravindu-rev/ruralz/internal/testkit`; stage 6 depgate adds `internal/testkit` and `internal/tool` to the per-binary denylist over `go list -deps ./cmd/...`. Conventions hold: license header, context first, bounded goroutines with an owner, `%w`, no `init()`, no mutable package globals (binaries and ports are passed down from `TestMain`, never cached globally).

| Package | Role | Imports (third party) |
|---|---|---|
| `internal/testkit/binaries` | Build `ruralzd`/`ruralz` once per test binary with release flags into a temp dir, or use `RURALZ_TEST_BIN_DIR`; build variants with `go build -overlay` (fault-variant successors) | none |
| `internal/testkit/proc` | Start, signal, wait, kill processes; log capture (file plus 8 MiB ring); `/proc/<pid>` CPU, RSS, threads sampling; child re-exec into a new network namespace | `golang.org/x/sys/unix` (ioctl, sysctl write) |
| `internal/testkit/pki` | Test CA, server and client certificates (ECDSA P-256, RSA 2,048), PEM files, `SSL_CERT_FILE` bundle | none |
| `internal/testkit/mockup` | Mock Upstream: HTTP/1.1, h2c, TLS; programmable body size, delay distribution, status, reset, slow body, echo; request log | none |
| `internal/testkit/mockidp` | JWKS (RS256, ES256, rotating keys, `Cache-Control`), OAuth2 client-credentials token endpoint, JWT minting via stdlib crypto | none |
| `internal/testkit/otlpsink` | In-process OTLP/gRPC collector (TLS or h2c), records traces, logs, metrics; span-order queries; raw bytes for canary scans (reuses A9's test collector types) | `go.opentelemetry.io/proto/otlp`, `google.golang.org/grpc` (brought by A9) |
| `internal/testkit/faultproxy` | Protocol-agnostic TCP fault proxy (pass, delay, black-hole, reset, refuse, upstream switch); A8's `statestoretest.FaultProxy` and `CountingProxy` wrap it | none |
| `internal/testkit/l4lb` | L4 TCP balancer with `/readyz` probes, redispatch on dial failure, per-backend stats, connection-to-backend attribution | none |
| `internal/testkit/loadgen` | Open-loop HTTP/1.1, h2c and h2 load with coordinated-omission-free latency and a log-linear histogram | none |
| `internal/testkit/promtext` | Scrape and parse Prometheus text exposition; counters, gauges, histogram quantiles | none |
| `internal/testkit/canary` | Canary generation and multi-encoding leak scanning | none |
| `internal/testkit/topology` | Compose the above into T1, T2 and chaos topologies with cleanup | none |
| `internal/testkit/benchrecord` | Result record type, JSON writer, schema check | none |
| `test/conformance/config` | Golden, JSON subset, loader, hostile, negative corpus (untagged); upstream suites and cross-binary diagnostics (`integration`) | `goccy/go-yaml`, `jsonschema/v6` (via A1) |
| `test/conformance/protocol` | HTTP/1.1 and HTTP/2 wire suite (`integration`) | `golang.org/x/net/http2` (Framer only) |
| `test/e2e` | Scenarios 1, 2, 6, 7, command tests (A10), secret leak, air-gapped, T1/T2, handover, Drain (`e2e`) | none |
| `test/chaos` | CE-1 to CE-6, CE-12, CE-15, CE-16, TQ chaos rows, FC rows, GD-1 to GD-5 (`e2e`) | none |
| `test/bench` | Scenario Bundles, R1 generator (`gen`), macro harness (`e2e`), `allocgate.json`, `record.schema.json` | none |
| `internal/tool/benchgate` | Stage 9 alloc/op A/B gate | none |
| `internal/tool/sizegate` | Stage 9 stripped size and idle RSS gate | none |
| `internal/tool/fuzzplan` | Nightly fuzz shard plan and runner | none |
| `internal/tool/releasekit` | Stage 12 packaging, notices (via depgate), checksums, notes, manifest verify, reproducibility compare | none |
| `internal/tool/depgate` (extended) | New subcommand `notices -binary B -goos O -goarch A` printing `THIRD_PARTY_LICENSES` | none |

Key APIs (illustrative signatures; exported for the areas' integration and e2e tests):

```go
// Package binaries builds the Ruralz binaries under test.
package binaries

type Set struct{ Ruralzd, Ruralz string } // absolute paths

type BuildOptions struct {
	Version string            // -X buildinfo.version, default "0.0.0-test"
	Overlay map[string]string // go build -overlay: repo-relative file -> replacement path
	GOOS, GOARCH string       // default host
}

// Build compiles cmd/ruralzd and cmd/ruralz with CGO_ENABLED=0, -trimpath and
// -ldflags -X into dir; RURALZ_TEST_BIN_DIR short-circuits when set and no Overlay.
func Build(ctx context.Context, repoRoot, dir string, o BuildOptions) (Set, error)
```

```go
package proc

type Spec struct {
	Name   string   // log prefix, e.g. "node-3"
	Path   string   // absolute binary path
	Args   []string
	Env    []string // complete environment; nothing is inherited implicitly
	Dir    string
	LogDir string   // stdout/stderr files <Name>.out, <Name>.err
}

type Usage struct{ UserCPU, SysCPU time.Duration; RSSBytes int64; Threads int }

type Process struct{ /* cmd, done chan, exit status, ring buffers */ }

func Start(ctx context.Context, s Spec) (*Process, error)
func (p *Process) PID() int
func (p *Process) Signal(sig os.Signal) error
func (p *Process) Kill() error
func (p *Process) Done() <-chan struct{}
func (p *Process) Wait(ctx context.Context) (exitCode int, err error)
func (p *Process) Output() (stdout, stderr []byte) // bounded copies
func (p *Process) Sample() (Usage, error)          // /proc/<pid>/stat and status

// InNetNS re-executes the test binary (os.Args[0], -test.run=^name$) in a new
// network namespace with loopback up and the given per-namespace sysctls, and
// returns its exit status; the child sees RURALZ_TEST_IN_NETNS=1.
func InNetNS(ctx context.Context, testName string, sysctls map[string]string) (int, error)
```

```go
package faultproxy

type Mode interface{ mode() }
type Pass struct{}
type Delay struct{ Mean, StdDev time.Duration } // normal per direction and chunk, clamped at 0
type Blackhole struct{}                         // keep sockets, read and discard, never forward
type Reset struct{}                             // RST existing and new connections (SO_LINGER 0)
type Refuse struct{}                            // close the listener; reopen on the next non-Refuse mode

type Config struct {
	Listen   string // "127.0.0.1:0"
	Upstream string
	MaxConns int    // default 16,384; beyond it new connections are reset
	Tap      func(dir Direction, b []byte) // optional synchronous inspector (RESP counting)
}

type Stats struct{ Accepted, Active, Reset int64; BytesUp, BytesDown int64 }

func Start(ctx context.Context, c Config) (*Proxy, error)
func (p *Proxy) Addr() string
func (p *Proxy) SetMode(m Mode)          // atomic; applies to live and new connections
func (p *Proxy) SetUpstream(addr string) // failover: new connections dial addr
func (p *Proxy) Stats() Stats
func (p *Proxy) Close() error
```

```go
package l4lb

type Backend struct{ Name, Addr, ReadyURL string }

type Config struct {
	Listen         string
	Backends       []Backend
	ProbeInterval  time.Duration // default 1 s
	UnhealthyAfter int           // default 2 failed probes (detection 2 s, inside the 5 s accept window)
	HealthyAfter   int           // default 2
	IdleTimeout    time.Duration // default 150 s, above the Node's 120 s
	Redispatch     bool          // default true: one retry of a failed dial on the next healthy backend
}

type BackendStats struct{ Healthy bool; Open, Accepted, DialFailures int64 }

func Start(ctx context.Context, c Config) (*Balancer, error)
func (b *Balancer) Addr() string
func (b *Balancer) Stats() map[string]BackendStats
func (b *Balancer) BackendOf(localClientAddr string) (name string, ok bool) // CE-1 attribution
```

```go
package loadgen

type Protocol int // HTTP1, H2C, H2TLS

type KeySource interface{ Next(i int64) string } // Uniform(n), Zipf(n, s), Rotating(perSecond), Single(k)

type Target struct {
	URL       string
	Method    string
	Header    http.Header
	Body      []byte
	Protocol  Protocol
	KeyHeader string    // e.g. "x-bench-key"
	Keys      KeySource
	Tokens    []string  // Authorization bearer rotation (S2)
}

type Config struct {
	Rate        float64       // fixed arrival rate, requests per second
	Duration    time.Duration
	Warmup      time.Duration // recorded separately, excluded from Result.Latency
	Connections int           // keep-alive connection cap (S1: 256)
	MaxInFlight int           // worker cap; a send that finds no worker is Missed, never queued
	Timeout     time.Duration
	Poisson     bool          // default uniform spacing
	TLS         *tls.Config
}

type Outcome string // "ok", "http_4xx", "http_5xx", "reset", "refused", "timeout", "other"

type Result struct {
	Offered, Sent, Missed int64
	Outcomes              map[Outcome]int64
	Codes                 map[string]int64 // problem+json "code" (RZ-...) counts
	Latency               *Histogram       // measured from intended send time
	Windows               []Window         // 1 s windows for warm-up stability and error timelines
	AchievedRate          float64
}

func Run(ctx context.Context, t Target, c Config) (Result, error)
func (r Result) Valid(maxRateError float64) error // 0.005 per PBB validity rule

type Histogram struct{ /* log-linear, 1 µs to 60 s, 3 significant digits */ }

func NewHistogram() *Histogram
func (h *Histogram) Record(d time.Duration)
func (h *Histogram) Quantile(q float64) time.Duration
func (h *Histogram) Merge(o *Histogram)
func (h *Histogram) WriteTo(w io.Writer) (int64, error) // raw log for result records
```

```go
package topology

type StateStore int // None, Memory, Redis, RedisReplicated

type Options struct {
	Nodes        int               // default 2 (e2e), 10 (chaos)
	BundleDir    string            // source Bundle rendered per Node with its port overlay
	Env          string            // --env for the render, optional
	Environments string            // --environments file, optional
	StateStore   StateStore
	FaultProxies bool              // a proxy per Node in front of the State Store and per Endpoint
	Balancer     bool
	OTLP         bool
	Canary       bool              // inject the canary secret (requirement 39)
	NodeEnv      map[string]string // extra RURALZ_* settings
	GOMAXPROCS   int               // per Node; default 1 when Nodes > 4
}

type Node struct {
	Name, DataDir, ConfigDir                    string
	HTTPAddr, HTTPSAddr, AdminAddr, AdminToken  string
	Proc                                        *proc.Process
}

type Topology struct {
	Nodes    []*Node
	Balancer *l4lb.Balancer
	Store    Server // A8 redisserver handle behind its fault proxy
	OTLP     *otlpsink.Sink
	Canary   canary.Canary
	Mocks    map[string]*mockup.Server
	IdP      *mockidp.Server
}

// Start builds nothing: bins come from TestMain. Cleanup (Drain, then kill after
// 35 s, logs kept on failure) is registered with t.Cleanup.
func Start(ctx context.Context, t testing.TB, bins binaries.Set, o Options) *Topology
func (tp *Topology) ReplaceBundle(ctx context.Context, n *Node, files map[string][]byte) error // symlink swap
func (tp *Topology) WaitReady(ctx context.Context) error
func (tp *Topology) Scrape(ctx context.Context, n *Node) (promtext.Set, error)
func (tp *Topology) Restart(ctx context.Context, n *Node) error
func (tp *Topology) Handover(ctx context.Context, n *Node, successor string) (*Node, error)
func (tp *Topology) ScanForCanary(ctx context.Context) []canary.Finding // requirement 39 sources
```

```go
package canary

type Canary struct{ Value string } // "rzcanary-" + 32 hex from crypto/rand

type Finding struct{ Source, Encoding string; Offset int }

func New() (Canary, error)
func (c Canary) Scan(source string, data []byte) []Finding // raw, base64 std/url, hex, URL, JSON-escaped
```

```go
package benchrecord

type Percentiles struct{ P50, P90, P99, P999, Max time.Duration }

type BudgetResult struct {
	ID, Value, Result, SLO string // "PB-2", "<= 1ms (target)", "0.84ms", "SLO-GW-2"
	Pass bool
}

type Record struct {
	Format      string `json:"format"` // "ruralz.bench.v1"
	Identity    struct{ Scenario, Commit, Baseline, RevisionDigest, Toolchain string } `json:"identity"`
	Environment struct {
		HardwareProfile, Microarchitecture, Kernel, Cpuset, NIC string
		GOMAXPROCS                                              int
		GOGC, GOMEMLIMIT                                        string
		Sysctls                                                 map[string]string
	} `json:"environment"`
	Load struct {
		Tool, Version, CommandLine                     string
		OfferedRate, AchievedRate                      float64
		Connections, KeyCardinality                    int
		KeyDistribution, InjectedDelay                 string
	} `json:"load"`
	Latency   struct{ External, Internal Percentiles; HistogramFiles []string } `json:"latency"`
	Resources struct {
		CPUPerRequest                            time.Duration
		GCCPUShare                               float64
		RSSLoadBytes, RSSIdleBytes, LiveHeapBytes int64
		AllocsPerOp                              float64
	} `json:"resources"`
	Validity struct {
		RateError, GeneratorCPU, MockCPU, NoiseCV float64
		Drops                                     int64
		Verdict                                   string // "valid", "invalid", "inconclusive"
	} `json:"validity"`
	Budgets []BudgetResult `json:"budgets"`
}
```

```go
// internal/tool/benchgate: go run ./internal/tool/benchgate -base origin/main -config test/bench/allocgate.json
type Config struct {
	Runs       int     `json:"runs"`       // 10
	Threshold  float64 `json:"threshold"`  // 0.03
	GOMAXPROCS int     `json:"gomaxprocs"` // 4
	Benchmarks []struct {
		Package   string `json:"package"`   // "./internal/gateway/router"
		Pattern   string `json:"pattern"`   // "^BenchmarkMatch$"
		Benchtime string `json:"benchtime"` // "2000x"
		MaxAllocs *int64 `json:"maxAllocs,omitempty"` // PB-8 cap where set
	} `json:"benchmarks"`
}
```

`internal/tool/sizegate` flags: `-binary PATH -max-size 167772160 -idle-rss -max-rss 93323264 -settle 120s`. `internal/tool/releasekit` subcommands: `package` (archives from `dist/`), `checksums`, `notes -from <tag|root> -to <tag> -budgets <records dir> -skipped <file>`, `verify -manifest release-manifest.json` (every expected artifact exists and is covered by `SHA256SUMS`), `compare -a DIR -b DIR` (reproducibility). `internal/tool/fuzzplan` subcommands: `list`, `shard -shards N -index I`, `run -fuzztime 15m`.

Concurrency model: every harness goroutine has an owner that cancels and waits for it (proxy connections: 2 goroutines each, capped by `MaxConns`; balancer: one prober per backend plus 2 per connection; loadgen: one scheduler, a fixed worker pool of `MaxInFlight`, a scheduler-to-worker channel of capacity 0 so a busy pool is counted as `Missed`; topology: one reaper per process, one scraper per Node when enabled). All stop on the test context and `t.Cleanup`; nothing is unbounded. Chaos tests run serially inside the Chaos job (they share the 4-CPU host); within one test, fault injection and assertions run on the test goroutine against metric snapshots taken at fixed times.

Exported for other areas: `internal/testkit/*` (A4, A5, A6, A8, A9, A10 integration and e2e tests), the negative and golden fixture runners' conventions (A1, A2), `test/bench/allocgate.json` (benchmark owners register gate entries), the record schema, the release artifacts and the unit and Dockerfile.

## 4. Dependencies on other areas

| Needs | From | Used for |
|---|---|---|
| `ruralzd` process contract: `RURALZ_CONFIG` (path), `RURALZ_DATA_DIR`, `RURALZ_LOG_LEVEL`, `RURALZ_ADMIN_TOKEN_FILE`, `RURALZ_ADMIN_METRICS_TOKEN_FILE`, `RURALZ_ADMIN_TLS_DIR`, `RURALZ_SECRET_ROOT`, `RURALZ_FETCH_ALLOW`; exit codes 0/1/2; `/readyz` JSON reasons; `/config/dump`; `/debug/snapshots`; `/tap` NDJSON; data dir files `lock`, `holder.json` (`ruralz.holder.v1`), `handover.sock`; handover protocol `ruralz.handover.v1` and refusal reasons; sd_notify `READY=1`, `STOPPING=1`, `MAINPID=`; symlink-swap and inode-change watcher; Drain timeline and drain code; format-marker and schema-level constants in one file each (for `-overlay` variants) | A4 | Harness, requirements 38, 44 to 47, 83 to 86 |
| AllocsPerRun hook: pre-parsed HTTP/1.1 request, discard `ResponseWriter`, Router plus chain entry point; stage allocation hooks | A4, A5 | PB-8 (requirement 63) |
| Loader, render, build, diff, canonical APIs; diagnostics JSON (`code`, `file`, `line`, `column`, identity); golden layout; negative fixtures; meta test; YAML and JSON-Schema suite runners | A1, A2 | Section B |
| CLI commands, flags, exit codes, `--ephemeral-ports` mapping line, `--output json`, `adminclient`; per-command e2e tests | A10 | Requirements 35, 37, 41 |
| Metric names and labels (`ruralz_config_activations_total`, `ruralz_config_activation_duration_seconds`, `ruralz_node_degraded_info` reasons `lkg_boot`, `state_store_breaker_open`, `state_store_eviction_policy`, `jwks_stale`, `discovery_stale`, `cleartext_hop`; `ruralz_state_*`, `ruralz_ratelimit_*`, `ruralz_quota_*`, `ruralz_cache_*`, `ruralz_upstream_*`, `ruralz_http_*`, `ruralz_listener_*`, `ruralz_runtime_*`, `ruralz_telemetry_*`), pre-created series at 0, span names `ruralz.filter.<name>`, OTLP/gRPC transport, in-process collector types | A9 | Chaos and e2e assertions, scenario 2 |
| `redisserver` launcher, `statestoretest.FaultProxy`/`CountingProxy` (to be built on `internal/testkit/faultproxy`), key prefixes `rz:rl:`, `rz:qt:`, `rz:rc:`, breaker constants, post-commit classes | A8 | Section E, CE-3 to CE-6, CE-16, GD-2 to GD-5 |
| `ratelimit` fields (`key`, `limits`, `failureMode`, `stateStoreTimeout`, per-Node ceiling field, `config.localOnly` if adopted), quota and cache behavior, Upstream retries, breaker, ejection, DNS responder test helper | A5 | CE-4, CE-12, CE-15, CE-16, requirement 59, FC-19 |
| Admin auth, canary injection points (`clientSecret`, API key refs), JWKS cache rules, redaction guarantees, egress guard allowing loopback via `RURALZ_FETCH_ALLOW` | A6 | Requirements 39, 60, 107 |
| CEL cost estimator fuzz target, CEL benchmark | A3 | Requirements 26, 80 |
| `internal/errcode` registry (every asserted code) | M0 | All assertions |

Provides: stages 8 to 12 and their workflows and `make` targets, the test kit, the conformance suite harnesses, chaos and benchmark suites, gates and records, the systemd unit and helper, the Dockerfile and image, release artifacts and verification, and `examples/`.

## 5. Libraries

| Module or tool | Version | Scope | Why | Row status |
|---|---|---|---|---|
| Standard library | Go 1.26 floor, go1.27.1 toolchain | everything; `archive/tar`, `archive/zip`, `compress/gzip`, `crypto/sha256`, `debug/buildinfo`, `net`, `net/http`, `os/exec`, `syscall` (Cloneflags) | Tools and harness need nothing else | n/a |
| `golang.org/x/sys` | v0.48.0 | `internal/testkit/proc` (ioctl `SIOCSIFFLAGS`, sysctl files) | Namespaces and loopback for air-gapped and `tcp_migrate_req` runs | Catalog row (Zero-Downtime Upgrade socket steering) |
| `golang.org/x/net` | v0.59.0 or newer | `test/conformance/protocol` only (`http2.Framer`, `hpack`) | Frame-level HTTP/2 cases | Catalog HTTP row ("x/net/http2 only for non-deprecated low-level APIs such as Framer") |
| `github.com/testcontainers/testcontainers-go` (+ `modules/redis`, `modules/valkey`) | v0.44.x (latest patch at implementation) | stage 8 container flavors | Redis 8, Valkey 9.0.1+, Dragonfly, clusters in CI | Catalog "Test tooling (not shipped)" |
| `github.com/goccy/go-yaml`, `github.com/santhosh-tekuri/jsonschema/v6` | v1.19.2, v6.0.3 (added by A1) | upstream suites, meta test | ADR3 confirmation | Catalog rows |
| `go.opentelemetry.io/proto/otlp`, `google.golang.org/grpc` | as pinned by A9 | `otlpsink` | Decode received spans | Brought by A9's telemetry rows |
| cosign | v3.1.3 | release signing and verification | Keyless Sigstore bundles, Rekor v2 | Research RES-T section 2; CI tooling row needed |
| Syft | v1.52.0 | CycloneDX 1.7 SBOMs per archive and image | OQ-release-versioning-and-compatibility-3 (a) | Research RES-T section 2; row needed; 1.7 support to verify (risk 11) |
| `actions/attest-build-provenance`, `actions/attest-sbom`, `actions/upload-artifact`, `actions/download-artifact` | pinned SHAs | release and nightly | SLSA L3 provenance, SBOM attestation, artifacts between jobs | Not researched; rows needed |
| oha, vegeta | v1.16.0; version to capture | Latency job | OQ-performance-budgets-and-benchmarking-1 (a) | Research RES-S 5.2; rows needed |
| Container images | `golang:1.27.1-bookworm`, `redis:8`, `valkey/valkey:9.0.x`, `docker.dragonflydb.io/dragonflydb/dragonfly` by digest | CA source; CI State Store flavors | Pinned by digest | Base image choice needs a research note (risk 11) |

No new module is linked into `ruralzd` or `ruralz` by this area. GoReleaser (researched) is not used: `make`/`releasekit` keep one build path and reproducible flags.

## 6. Test plan

Unit and table tests (stage 5, hermetic, `-race`, shuffled):
1. `benchgate`: parser on captured `go test -bench` output (multiple lines per benchmark, `-count`, missing `-benchmem` → tool error 2); medians with even and odd counts; 30 → 31 allocations fails (3.3%), 100 → 103 passes (exactly 3%), 100 → 104 fails; absolute cap exceeded fails even without regression; new benchmark reported and passing; removed benchmark fails; worktree cleanup on every exit path.
2. `sizegate`: size limit boundary (equal passes, +1 byte fails); RSS sampler on a fake `/proc` tree; settle timing with an injected clock.
3. `releasekit`: archive determinism (build twice → identical bytes; entry order, mtimes, modes, uid/gid); `.zip` for windows only; top directory naming; `SHA256SUMS` format golden; `compare` detects a one-byte difference; `verify` fails on a missing or uncovered artifact; notes golden from a fixture commit list (feat/fix/deprecation/`!` breaking), budgets table and "Skipped gates".
4. `depgate notices`: golden `THIRD_PARTY_LICENSES` for a fixture module graph including an MPL-2.0 exception (states where source is available) and the EDL-1.0 election.
5. `fuzzplan`: deterministic sharding (same input → same shards; eight per shard); `go test -list` parsing.
6. `loadgen`: histogram accuracy within 0.1% relative at 3 significant digits over 1 µs to 60 s; latency from intended send time (a stalled server yields back-filled high latencies, wrk2-style check); `Missed` counted when workers are exhausted; `Valid` at 0.49% and 0.51% rate error; problem+json `code` extraction.
7. `faultproxy`: each mode on live and new connections (delay distribution mean and standard deviation within 5% over 10,000 samples; black-hole stalls without closing; reset yields `ECONNRESET` at the peer; refuse yields `ECONNREFUSED`; upstream switch); `MaxConns` overflow resets; goroutines return to baseline after `Close`.
8. `l4lb`: probe state machine (2 failures → unhealthy, 2 successes → healthy), redispatch on refused dial, idle timeout, `BackendOf` attribution.
9. `promtext`: parser golden on A9's `/metrics` golden, histogram quantile interpolation, label matching.
10. `canary`: every encoding found at the right offset; no false positives on random data (10,000 cases).
11. `proc`: start, signal, wait, output bounds, `/proc` sampling; `InNetNS` child sees only `lo` (skip without root).
12. `binaries`: overlay build produces a binary whose `ruralz version` differs as injected.

Configuration conformance (stage 5 untagged, stage 8 `integration` parts): golden entries (A2 list: `shop-bundle` × {prod, staging, none}, JSON twin, `overridable-{absent,true,false}`, `minimal`, one per Policy type, `lists`, `numbers-and-units`); JSON subset pairs (tabs, `\/`, `😀`, unpaired surrogate → RZ-CFG-001); loader cases RZ-CFG-001 (each of six causes), -002, -003 (anchor, alias, merge key, billion laughs with heap under 16 MiB), -004 (`!include`, `!env`); scalars `yes`, `on`, `0777` (int raw 777, int substituted 777, string substituted `"0777"`), `0b1`, `1_000`, `2026-09-23`; hostile generated inputs (requirement 17); negative corpus for every offline code with `ruralz` versus `ruralzd` equality (stage 8); YAML Test Suite with `expected-failures.txt` ratchet; JSON-Schema-Test-Suite zero failures; floor job and darwin/windows digests identical.

Protocol conformance (stage 8): every case of requirements 22 to 24, including every Node-generated code: `RZ-RT-001` (404), `RZ-RT-002` (431 problem), plain 431 (256 KiB), `RZ-RT-003` (413), `RZ-RT-005` (503, in-flight ceiling lowered through a 20,000-unit flood in the C1 constants run, else skipped with reason), `RZ-RT-013` (1 MiB chunk cap where streaming Routes exist; M1: unit test by A4), stream 251 refused, GOAWAY on Drain.

Integration (stage 8): State Store scenarios of requirement 30 per flavor available; round-trip test (requirement 31); `systemd-analyze verify` of the unit; `ruralz bundle validate` over `examples/` per Environment; Dockerfile lint by `docker buildx build --check` (CI only).

End-to-end (stage 10, `e2e`): quickstart (script exit 0, body, elapsed ≤ 600 s); scenario 2 span order per REST Route plus probes (`RZ-AUTH-001`, `RZ-RL-001` or `RZ-RL-002`, CORS, security headers); scenario 6 (exit 0 and 1, `ruralz.diff.v1` golden, admin URL and dump file sources, RZ-CFG-027 on a tampered dump file → exit 2); Hot Reload (ZG-1 long request, ZG-2 invalid Bundle with `{result="rejected",code="RZ-CFG-005"}`), Last-Known-Good (`lkg_boot`, RZ-CFG-027 corrupted candidate → not ready, RZ-CFG-026 unresolvable secret → 503 `secrets_unresolved`); handover (requirement 44, `tcp_migrate_req` 1 and 0, reduced counts); refusals (older candidate, `unknown_marker` via overlay, `draining`, 60 s timeout ×1 locally); V-6 overlay variant (RZ-CFG-024, refused, N keeps serving); Drain (requirement 46, via SIGTERM and via `ruralz node drain`, exit 0 by 30 s, drain code for a 40 s upstream request); T1 dev run; T2 two Nodes with symlink swap; secret leak on every test's topology; air-gapped (root; skipped with reason otherwise); A10's per-command tests.

Chaos (nightly Chaos job, `e2e`): requirements 50 to 61, each asserting codes and metrics: `RZ-STS-001`, `RZ-STS-002` (NOSCRIPT under `closed`), `RZ-STS-003`, `RZ-STS-004` (in-flight ceiling full in O2 miniature), `RZ-UP-001`, `RZ-UP-003`, `RZ-UP-004`, `RZ-UP-005`, `RZ-UP-007`, `RZ-UP-008` (all Endpoints ejected or refused), `RZ-AUTH-006`, `RZ-RL-001`, `RZ-RL-002`; fail-open bound arithmetic checked per key from the generator's per-key admit counts; GD-1 to GD-5 drills.

Benchmarks and gates: stage 9 on every Go change; R1 ladder smoke nightly (non-gating locally, gating on RH-1); scenario harness dry run with the Go generator against local processes (validates records and schema; numbers informational); size and idle RSS gate on linux/amd64 and arm64 (arm64 CI).

Release (stage 12 and `make release-dry-run`): dry run builds all archives, `SHA256SUMS`, notices and notes locally; CI rebuild compare; image build and air-gapped image run; cosign and attestation verification (requirement 98).

## 7. Open questions blocking M1 in this area

| ID | Question | Adopt | What the code does |
|---|---|---|---|
| OQ-release-versioning-and-compatibility-3 (listed in RM exit 7) | Which tools sign artifacts and generate SBOMs; does SPDX ship? | (a) cosign v3.1.3 plus an SBOM generator after research: Syft v1.52.0 (both in RES-T section 2); CycloneDX 1.7 only, no SPDX in `0.1.0`; provenance through GitHub artifact attestations (reusable workflow) | CI tooling rows for cosign, Syft and the attestation actions land in TS with this area's first release pull request; `scripts/install-tools.sh` pins release binaries by SHA-256; `release-build.yml` signs `SHA256SUMS`, images and attests; verify job per requirement 98 |
| OQ-repository-layout-and-conventions-6 (listed in RM exit 7) | Where do linux/arm64 tests run? | No option marked; recommended (a) native arm64 runners (GitHub-hosted `ubuntu-24.04-arm`), falling back to (c) native nightly if unavailable | `pr-full` stage 8 matrix `[ubuntu-latest, ubuntu-24.04-arm]`; size gate for arm64 runs there |
| OQ-zero-downtime-upgrades-and-hot-reload-7 | Which systemd directives keep the new process as main process across a handover? | (a) research, then a shipped unit and helper | Unit and helper of requirements 83 to 85; A4 implements sd_notify; the live CI test (requirement 86) is the research evidence and MUST pass before `0.1.0`; until then `systemctl stop` and start is a Drain and restart |
| OQ-testing-and-quality-strategy-2 | Statistics tool and runner hardware for benchmark gates | (c) alloc/op on shared runners, latency on RH-1 (chosen by PBB) | `benchgate` in stage 9 on `ubuntu-latest`; latency job targets a self-hosted `rh-1` runner label; medians plus four-of-five pair rule; no statistics tool until OQ-performance-budgets-and-benchmarking-1 adds one |
| OQ-performance-budgets-and-benchmarking-1 | Load tools and A/B statistics tool | (a) proposed: oha, vegeta, fortio, then a researched statistics tool | Latency harness drives oha (primary) and vegeta (S1 cross-check) through a `LoadTool` adapter; rows added before the M1 macro gate |
| OQ-performance-budgets-and-benchmarking-2 | How is RH-1 provisioned and funded? | No option marked; owner decision. Recommended (b) rented bare metal meeting the RH-1 spec (fastest to provision), keeping (a) as the long-term home | Harness is host-agnostic (record captures the fingerprint); the Latency job is defined but skipped with an explicit "RH-1 not provisioned" failure annotation; M1 exit criteria 2 and 3 stay open until it runs |
| OQ-performance-budgets-and-benchmarking-6 | Loader workers W = max(1, `GOMAXPROCS`/2) yielding every 100 µs? | (a) proposed | R1 ladder asserts stage budgets and the loaded-reload bound; A4 implements |
| OQ-scalability-and-distributed-state-10 | Chaos fault-injection tooling | (a) TCP fault proxy and signals in the harness (with OQ-testing-and-quality-strategy-3 (a), a Ruralz Go proxy) | `internal/testkit/faultproxy`, `proc` signals, redis commands |
| OQ-tech-stack-and-libraries-25 (blocking container images) | Where do static binaries get CA roots? | No option marked; recommended (a): the image ships a CA bundle, archives read the host store | Dockerfile copies `/etc/ssl/certs/ca-certificates.crt` from a pinned image; notices cover the MPL-2.0 CA data; no embedded roots |
| OQ-scalability-and-distributed-state-2 | `redis` topologies in M1 | (a) standalone with replicas, and sharded (as A8) | Harness launches standalone, replica (scripted failover) and three-primary cluster; no Sentinel |
| OQ-scalability-and-distributed-state-11 | Pack 8.7, 8.8, 8.11 amendments (`localOnly`, first-seen budget, ceilings) | (a) recommended | CE-12 `localOnly` assertion and CE-15 first-seen assertion are enabled only if the amendment lands; otherwise they assert the "as written" bounds |
| OQ-cli-and-api-surface-10 | Where the lock holder records PID and start time | (a) a file under `${RURALZ_DATA_DIR}` (`holder.json`, A4) | systemd helper and drain tests read `holder.json` |
| OQ-security-and-identity-7 (Blocking names M1), -22 (listed in RM exit 7) | Admin credential settings; secret root and fetch allow | (a) both | Harness passes `RURALZ_ADMIN_TOKEN_FILE`, temporary `RURALZ_SECRET_ROOT` and `RURALZ_FETCH_ALLOW=127.0.0.0/8,::1`; quickstart sets `127.0.0.1/32` |
| OQ-security-and-identity-21 (listed in RM exit 7) | Reject encoded NUL and backslashes? | (a) yes | Protocol suite cases (requirement 22) |

Non-blocking, decided for this area: OQ-testing-and-quality-strategy-1 (a) `testing.F`; -3 (a) Go fault proxy; -4 (a) fuzzing in CI only; -5 (a) image scanner after research (none in `0.1.0`, govulncheck on binaries runs); OQ-performance-budgets-and-benchmarking-3 (a) placements; -4 (a) RH-2 gates from M2; OQ-zero-downtime-upgrades-and-hot-reload-1 (a) fixed timings, -2 (b) new drain code, -3 (a) one GOAWAY, -12 (a) `tcp_migrate_req` SHOULD with 3 s linger; OQ-repository-layout-and-conventions-4 (b); OQ-configuration-model-18 (a) 64 levels, memory measured.

## 8. Deferred (M2 and later): do not build now

| Item | Milestone | Extension point left |
|---|---|---|
| Plugin ABI conformance suite, `ruralz plugin test` on PDK scaffolds, Plugin constants rows, S4, PB-5, SM-6 benchmarks | M2 | `allocgate.json` entries; stage 8 target list is data |
| Control-mode e2e (scenarios 3, 4), kind, Helm chart, CRDs, `ruralz-control` archive and image, chart signing, Control Stream fuzz targets, `buf breaking` | M2 | `topology.Options` gains a mode; Dockerfile parameterized by binary name; `release-build.yml` binary list is a matrix |
| Chaos rows for Ruralz Control, Raft, signatures; CE-7 to CE-10, GD-6 to GD-16; F1/F2 scale job; SM-9 | M2 | Nightly Scale job slot defined but empty |
| OCI Revision pull and signature verification tests, `revision_signature_off` | M2 | `topology` source kind interface (directory now) |
| Compatibility gate (previous-release handover, skew RZ-CFG-024 across real releases) | from `0.2.0` | V-6 test takes the N-1 binary path as input |
| WebSocket/SSE Drain and Hot Reload streams (scenario 5), Autobahn, HTTP/3 (S3 variant, UDP 8443, `EXPOSE 8443/udp`), gRPC, GraphQL, AI dialect fixtures, AI provider mock, S6/S6n/S7, SM-10, SM-11, CE-13, CE-14 | M3 | Protocol suite organized per protocol directory |
| Release-trend bench suite, S8, `default.pgo`, CE-11, CE-17, GD-14 | M4 | Record format versioned `ruralz.bench.v1` |
| FIPS job and `-fips` artifacts, FIPS air-gapped images | M5 | `FLAVOR` variable already in the Makefile; image name suffix parameter |
| Image vulnerability scanner (OQ-testing-and-quality-strategy-5), OpenSSF Scorecard 8.0 (SM-12, M2) | M2 | Release workflow job slot |

## 9. Risks and ambiguities

1. **Docker Compose versus no daemon.** TQ, RM and RL name Docker Compose (and testcontainers) for e2e and integration; this environment has no daemon. Resolution: the process harness is the M1 e2e implementation everywhere (same topology: two Nodes, State Store, mocks, collector); Compose remains only for the image air-gapped start in stage 12; record the deviation in TQ with the conformance pass (no silent change of the documented tool).
2. **One host, one port set.** A rendered Bundle fixes listener ports, so two Nodes on one host cannot share a Bundle (a second `SO_REUSEPORT` binder with the same UID would join the first Node's reuseport group). Resolution: per-Node port overlay (digests differ per Node in multi-Node tests), or a network namespace per Node when equal digests matter; handover tests use one Node.
3. **`shop-bundle` is not M1-servable.** It uses `http3: true` (M3), Kubernetes discovery and `kubernetes` secrets (M2), a Plugin (M2), gRPC and AI Routes (M3), yet TQ scenario 2 says its REST Routes are M1. Resolution: the e2e overlay `test/e2e/testdata/shop-m1/overlays/e2e/` (copied next to the verbatim Bundle at test time; overlays never change the base digest) deletes Plugin, AI, gRPC resources and their references, sets `http3: false`, replaces discovery with static Endpoints to mocks, rewrites `kubernetes` secretRefs to `file` and sets ports from `${E2E_*}`. Golden digests use the verbatim Bundle through the CLI only. A4 must define what a M1 Node does with `http3: true` (reject with a code, not ignore).
4. **Quickstart upstream.** Static responses are M2, so a first proxied request needs an upstream on a clean machine. Resolution: proxy to the Node's own `/healthz` on loopback with `RURALZ_FETCH_ALLOW`; if A6 excludes the admin port from allowed destinations, the quickstart script must start a documented local upstream instead, which costs SM-7 time.
5. **Illustrative digests in docs.** `rev-162af81f5de4` and `sha256:68f78253…` are fake; the golden corpus records real digests once, after review, and the golden freeze MUST follow the M1 default-changing decisions (OQ-security-and-identity-1, OQ-traffic-management-and-resilience-5/-6) or each later change needs a schema level and `CHANGES` entry (A2 R-25).
6. **Budgets need RH-1.** PB-1 to PB-4, PB-6, PB-7, PB-9 to PB-11, PB-14 to PB-16, soak and the macro gate cannot be measured here; OQ-performance-budgets-and-benchmarking-1 and -2 are open. `0.1.0` could ship with the benchmark gate skipped under TQ's skip rule, but M1 cannot exit (criteria 2 and 3). Local numbers are never published as results (P10).
7. **"Conflicting framing rejected" (SEC) versus `net/http`.** Go accepts `Transfer-Encoding: chunked` with a valid `Content-Length` by deleting `Content-Length` before the handler (RFC 9112 section 6.3), so Ruralz cannot reject it. Resolution: the suite asserts no ambiguous forwarding and Go's rejections (duplicate differing `Content-Length`, invalid values, unknown codings); SEC should say "normalized per RFC 9112, never forwarded with both".
8. **V-3 refused/timed-out count.** "1,000 refused or timed-out handovers" at a 60 s timeout would take about 17 hours; refusals are instant. Resolution: 1,000 instant refusals plus 10 timeouts; ask ZDU to state it.
9. **sd_notify readiness timing.** With `Type=notify`, a Node that never becomes ready (no valid Bundle, no Last-Known-Good) would hit `TimeoutStartSec` and restart-loop, although FP 8.2 says it stays not ready and keeps watching. A4 req 8 says `READY=1` "when first ready". Recommendation: send `READY=1` after the first boot attempt completes (ready or not) with `STATUS=` carrying the readiness reason; systemd `MAINPID=` acceptance from a non-main process in the unit's cgroup under `NotifyAccess=all`, and `ExecReload` leftovers staying in the cgroup, are unverified: requirement 86 is the gate.
10. **State Store versions.** Local `redis-server` 7.0.15 is below the documented Redis 8 and Valkey 9.0.1 floors and cannot cover Dragonfly; those flavors run only in CI containers. State Store scripts using Redis 8-only commands would fail locally (none planned in M1).
11. **Unrowed tools and images.** No signing, SBOM, load or attestation tool has a TS catalog or CI tooling row; cosign, Syft (and GoReleaser) have research rows, oha/vegeta/fortio have research, GitHub attestation actions and the CA source image do not. Syft's CycloneDX 1.7 output is unverified (fallback: 1.6, ECMA-424 first edition, recorded as a deviation). Each row lands before first use.
12. **Game days versus drills.** HA game days run "on staging Clusters" quarterly; no staging exists. Resolution: M1 exit accepts the automated GD-1 to GD-5 drills of the nightly Chaos job as evidence; the first human game day waits for a staging environment.
13. **Scale in chaos.** CE-5 needs at least 100 Nodes (about 9 GiB of `ruralzd` processes) and CE-12/CE-15 need 1,000,000 and 100,000 calls per second: locally only scaled variants; full runs need larger CI runners or RH-1-class hosts. The fd hard limit 20,000 here also blocks C1 at 20,000 connections.
14. **Duplicate test-kit ownership.** A8 proposes `statestoretest.FaultProxy` and `redisserver`; this area needs a protocol-agnostic proxy for Endpoints and partitions too. Resolution: one TCP proxy in `internal/testkit/faultproxy`, which A8's types wrap (RESP counting as a tap); `redisserver` stays A8's.
15. **Stripped binaries.** PBB gates a "stripped" `ruralzd`, while RVC and the Makefile do not strip release builds. Recommendation: release builds add `-s -w` too, so the gated binary is the shipped one (reproducibility is unaffected; pprof and stack traces keep working through pclntab); needs an RVC sentence.
16. **Idle RSS measurement.** PBB reads RSS from cgroup accounting; shared CI runners use `VmRSS`. The two differ (page cache); the gate uses `VmRSS` with the 89 MiB limit and RH-1 reports cgroup values.
17. **Examples layout.** `control/` must sit beside, not inside, each Bundle root (RZ-CFG-017); A1 assumes a `control/environments.yaml` per Bundle. Resolution: `examples/control/` shared by all examples (the CM example's control files verbatim), mapped explicitly in the meta test.
18. **Air-gapped start needs root.** The process variant needs `CLONE_NEWNET` (root or an unprivileged user namespace); GitHub runners provide `sudo`. Without it the test skips with a reason, and the stage 12 image variant remains authoritative.
