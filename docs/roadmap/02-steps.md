# Steps

The work left in the current milestone, M1 Core gateway, in build order: a row depends only on rows above it. Take the first row and delete it when its work and tests are done and the gates pass. The linked document defines what each row builds; the [Roadmap and milestones](01-roadmap-and-milestones.md) holds the milestone's scope and exit criteria.

| Step | Paths | Design |
|---|---|---|
| Router | `internal/gateway/router/` | [Data plane](../architecture/03-data-plane.md) |
| Per-request exchange | `internal/gateway/exchange/` | [Data plane](../architecture/03-data-plane.md) |
| Listeners, servers and SO_REUSEPORT | `internal/gateway/listener/`, `internal/gateway/reuseport/` | [Data plane](../architecture/03-data-plane.md) |
| Admin server, tap and readiness | `internal/gateway/admin/`, `internal/gateway/tap/`, `internal/gateway/readiness/` | [Data plane](../architecture/03-data-plane.md), [CLI and API surface](../reference/01-cli-and-api-surface.md) |
| Upstream runtimes and attempt loop | `internal/gateway/upstream/` (package root) | [Traffic management](../architecture/09-traffic-management-and-resilience.md) |
| `auth.jwt` and JWKS | `internal/filter/auth/jwt/` | [Security and identity](../architecture/08-security-and-identity.md) |
| `auth.api-key` and `auth.basic` | `internal/filter/auth/apikey/`, `internal/filter/auth/basic/` | [Security and identity](../architecture/08-security-and-identity.md) |
| `auth.mtls`, `authz.ip` and `cors` | `internal/filter/auth/mtls/`, `internal/filter/authz/ip/`, `internal/filter/cors/` | [Security and identity](../architecture/08-security-and-identity.md) |
| `auth.upstream-oauth2` | `internal/filter/auth/upstreamoauth2/` | [Security and identity](../architecture/08-security-and-identity.md) |
| State Store manager and post-commit queue | `internal/statestore/manager/`, `internal/statestore/postcommit/` | [Scalability](../architecture/11-scalability-and-distributed-state.md) |
| State Store test servers and container flavors | `internal/statestore/statestoretest/redisserver/`, `internal/statestore/statestoretest/respproxy/` | [Testing](../engineering/03-testing-and-quality-strategy.md) |
| Response Cache revalidation and coalescing | `internal/filter/cache/revalidate/`, `internal/filter/cache/coalesce/` | [Traffic management](../architecture/09-traffic-management-and-resilience.md) |
| Test kit: load generation and L4 balancing | `internal/testkit/loadgen/`, `internal/testkit/l4lb/` | [Testing](../engineering/03-testing-and-quality-strategy.md) |
| Examples | `examples/` | [Configuration model](../architecture/02-configuration-model.md) |
| Release tooling and packaging | `internal/tool/releasekit/`, `internal/tool/depgate/`, `deploy/container/`, `deploy/systemd/` | [Release](../engineering/04-release-versioning-and-compatibility.md) |
| Redis State Store driver | `internal/statestore/redis/` | [Scalability](../architecture/11-scalability-and-distributed-state.md) |
| Loader | `internal/config/loader/` | [Configuration model](../architecture/02-configuration-model.md) |
| Semantic validation | `internal/config/validate/` | [Configuration model](../architecture/02-configuration-model.md) |
| Diff | `internal/config/diff/` | [Configuration model](../architecture/02-configuration-model.md), [CLI and API surface](../reference/01-cli-and-api-surface.md) |
| Render | `internal/config/render/` | [Configuration model](../architecture/02-configuration-model.md), [CLI and API surface](../reference/01-cli-and-api-surface.md) |
| `authz.cel` and `headers` | `internal/filter/authz/cel/`, `internal/filter/header/` | [Security and identity](../architecture/08-security-and-identity.md), [Data plane](../architecture/03-data-plane.md) |
| `transform.request` and `transform.response` | `internal/filter/transform/` | [Data plane](../architecture/03-data-plane.md) |
| `ratelimit` | `internal/filter/ratelimit/` | [Traffic management](../architecture/09-traffic-management-and-resilience.md), [Scalability](../architecture/11-scalability-and-distributed-state.md) |
| `quota` | `internal/filter/quota/` | [Traffic management](../architecture/09-traffic-management-and-resilience.md), [Scalability](../architecture/11-scalability-and-distributed-state.md) |
| Response Cache Filter (`cache`) | `internal/filter/cache/` (package root) | [Traffic management](../architecture/09-traffic-management-and-resilience.md) |
| Composition | `internal/gateway/composition/` | [Traffic management](../architecture/09-traffic-management-and-resilience.md) |
| Request handler and replay | `internal/gateway/handler/`, `internal/gateway/replay/` | [Data plane](../architecture/03-data-plane.md) |
| Drain and sd_notify | `internal/gateway/drain/`, `internal/gateway/sdnotify/` | [Zero-downtime upgrades](../operations/02-zero-downtime-upgrades-and-hot-reload.md) |
| CLI admin client | `internal/cli/adminclient/` | [CLI and API surface](../reference/01-cli-and-api-surface.md) |
| Plain-upstreams forwarder and upstream debug | `internal/gateway/upstream/forward/` | [Traffic management](../architecture/09-traffic-management-and-resilience.md) |
| Test kit topology | `internal/testkit/topology/` | [Testing](../engineering/03-testing-and-quality-strategy.md) |
| Configuration pipeline facade | `internal/config/pipeline/` | [Configuration model](../architecture/02-configuration-model.md) |
| Snapshot compiler | `internal/gateway/compile/` | [Data plane](../architecture/03-data-plane.md) |
| Built-in Filter tables | `internal/filter/builtin/` | [Data plane](../architecture/03-data-plane.md) |
| `ruralz node` commands | `internal/cli/node/` | [CLI and API surface](../reference/01-cli-and-api-surface.md) |
| Zero-Downtime Upgrade handover | `internal/gateway/handover/` | [Zero-downtime upgrades](../operations/02-zero-downtime-upgrades-and-hot-reload.md) |
| Activation, Hot Reload and Last-Known-Good | `internal/gateway/reload/`, `internal/gateway/lkg/`, `internal/gateway/source/` | [Zero-downtime upgrades](../operations/02-zero-downtime-upgrades-and-hot-reload.md) |
| `ruralz bundle` commands | `internal/cli/bundle/` | [CLI and API surface](../reference/01-cli-and-api-surface.md) |
| Configuration conformance and golden corpus | `test/conformance/config/` | [Testing](../engineering/03-testing-and-quality-strategy.md) |
| `ruralzd` wiring | `internal/gateway/` (package root), `cmd/ruralzd/` | [Data plane](../architecture/03-data-plane.md) |
| CLI launcher, `ruralz dev` commands and root | `internal/cli/launch/`, `internal/cli/dev/`, `internal/cli/` (package root), `cmd/ruralz/` | [CLI and API surface](../reference/01-cli-and-api-surface.md) |
| Cross-package property suites | `test/property/` | [Testing](../engineering/03-testing-and-quality-strategy.md) |
| End-to-end suite | `test/e2e/` | [Testing](../engineering/03-testing-and-quality-strategy.md) |
| Chaos experiments and chaos at scale | `test/chaos/`, `.github/workflows/chaos-scale.yml` | [Testing](../engineering/03-testing-and-quality-strategy.md) |
| Benchmark harness, gates and records | `test/bench/harness/`, `test/bench/allocgate.json`, `test/bench/record.schema.json` | [Performance budgets](../architecture/12-performance-budgets-and-benchmarking.md) |
| Protocol conformance | `test/conformance/protocol/` | [Testing](../engineering/03-testing-and-quality-strategy.md) |
| Benchmark scenarios, R1 generator and functional variants | `test/bench/scenarios/`, `test/bench/gen/`, `test/bench/functional/` | [Performance budgets](../architecture/12-performance-budgets-and-benchmarking.md) |
| Node integration: CEL and telemetry | `test/integration/cel/`, `test/integration/telemetry/` | [Testing](../engineering/03-testing-and-quality-strategy.md) |
| Node integration: security | `test/integration/security/` | [Testing](../engineering/03-testing-and-quality-strategy.md), [Security and identity](../architecture/08-security-and-identity.md) |
| Node integration: State Store and round trip | `test/integration/statestore/` | [Testing](../engineering/03-testing-and-quality-strategy.md), [Scalability](../architecture/11-scalability-and-distributed-state.md) |
| T5 systemd verification | `test/t5/`, `.github/workflows/t5-systemd.yml` | [Deployment topologies](../operations/01-deployment-topologies.md) |
| Compose end-to-end and air-gapped image start | `test/compose/`, `.github/workflows/image-checks.yml` | [Testing](../engineering/03-testing-and-quality-strategy.md) |
| Game-day drills and failure rows | `test/chaos/gameday/` | [High availability](../operations/04-high-availability-and-disaster-recovery.md) |
| Release workflows and gates | `.github/workflows/release.yml`, `release-build.yml`, `release-verify.yml`, `internal/tool/releasegate/` | [Release](../engineering/04-release-versioning-and-compatibility.md) |
| RH-1 provisioning, budget runs and soak | `deploy/benchhost/` | [Performance budgets](../architecture/12-performance-budgets-and-benchmarking.md) |
| Release `0.1.0`: candidate, cut and repository settings | `scripts/repo-settings.sh`, the release runbook | [Release](../engineering/04-release-versioning-and-compatibility.md) |
