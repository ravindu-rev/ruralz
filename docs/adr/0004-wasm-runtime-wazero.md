---
id: ADR-0004
title: "WASM runtime: wazero"
status: accepted
date: 2026-09-25
deciders: [ruralz-core]
related:
  - docs/architecture/05-wasm-plugin-system.md
  - docs/engineering/01-tech-stack-and-libraries.md
  - docs/_meta/foundation-pack.md
  - docs/architecture/01-system-overview.md
  - docs/architecture/08-security-and-identity.md
---

# ADR-0004: WASM runtime: wazero

## Context and problem statement

Differentiator (1), the WASM plugin system, runs operator-installed Plugins inside every Node of Ruralz Gateway (`ruralzd`) under Plugin ABI v1 ([ADR-0005](0005-plugin-abi-v1.md)); the `ruralz` CLI runs the same host for `ruralz plugin test`. Principle P6 requires a sandbox with deny-by-default Capabilities and declared limits (`limits.memoryBytes`, `limits.timeout`) in which a faulty Plugin fails only its own Policy ([Vision](../vision/01-vision-and-positioning.md#principles)).

Which WebAssembly runtime executes that guest code in a `CGO_ENABLED=0` static binary ([ADR-0001](0001-implementation-language-go.md)), under an Apache-2.0-compatible license, with enforceable memory and time limits? Nothing is implemented yet: the runtime and Plugins are Planned (M2), and the proxy-wasm compatibility adapter that shares this runtime is Planned (M4).

## Decision drivers

- **Gate G1**: pure Go, cross-built for linux/amd64 and linux/arm64 ([Selection criteria](../engineering/01-tech-stack-and-libraries.md#selection-criteria)); goal WG-3 of the owning document.
- **Gate G2**: a license compatible with Apache-2.0 distribution ([ADR-0002](0002-apache-2-license-no-feature-gating.md)).
- **Criterion S4**: memory caps and deadlines the host controls, since P6 limits are enforced per call.
- **Isolation (TB-2)**: a trap fails the call, never the Node ([System overview](../architecture/01-system-overview.md#trust-boundaries)).
- **One host everywhere**: `ruralzd` and `ruralz plugin test` MUST behave identically.
- **Measured cost (P10)**: a pooled Phase call costs 50 µs or less at p99 (target), SM-6.
- **Maintenance**: recent releases and a small transitive graph (S1, S6).

## Considered options

1. **wazero**, repository `wazero/wazero`, Go module path `github.com/tetratelabs/wazero`, v1.12.0 released 2026-05-29 ([source](https://github.com/wazero/wazero/releases/tag/v1.12.0)) ([source](https://proxy.golang.org/github.com/tetratelabs/wazero/@latest)).
2. **wasmtime-go** v49, calling the Wasmtime C API through CGO ([source](https://github.com/bytecodealliance/wasmtime-go/blob/main/README.md)).
3. **Extism go-sdk** as the host, itself a wazero wrapper ([source](https://github.com/extism/go-sdk)).
4. **mosn proxy-wasm-go-host** as the host, which depends on wazero v1.2.1 and `wasmer-go` ([source](https://github.com/mosn/proxy-wasm-go-host/blob/main/go.mod)).

## Decision outcome

Chosen option: "wazero, imported as `github.com/tetratelabs/wazero`", because it is the only researched runtime that is pure Go and "doesn't rely on CGO" ([source](https://github.com/wazero/wazero/blob/main/README.md)), is Apache-2.0 ([source](https://github.com/wazero/wazero/blob/v1.12.0/LICENSE)), and exposes the page limits, context-deadline interruption and shared compiled code that the owning document's [Runtime](../architecture/05-wasm-plugin-system.md#runtime) rules need. This matches the foundation pack section 7 WASM runtime row: repository `github.com/wazero/wazero`, Go module path `github.com/tetratelabs/wazero`. Code and `go.mod` MUST use the module path; `import "github.com/wazero/wazero"` fails because the `.mod` file declares the other path ([source](https://proxy.golang.org/github.com/wazero/wazero/@v/v1.12.0.mod)). Rules, all Planned (M2):

| Concern | Rule |
|---|---|
| Version and wrapping | v1.12.x floor, pinned in `go.mod`; imported only by `internal/pluginhost`, which both `ruralzd` and the CLI link ([Banned imports](../engineering/02-repository-layout-and-conventions.md#banned-imports)) |
| Engine | Compiler mode, which runs only on amd64 and arm64; the interpreter, often ten times slower ([source](https://github.com/wazero/wazero/blob/main/README.md)), is not a production engine, so `ruralzd` production platforms are linux/amd64 and linux/arm64 ([Static builds](../engineering/01-tech-stack-and-libraries.md#static-builds)) |
| Runtimes and cache | One runtime per distinct `limits.memoryBytes`, since the page limit is a runtime setting, all sharing one in-memory `NewCompilationCache()`; `NewCompilationCacheWithDir` is never used, because persisted native code is covered by no signature (OQ-data-plane-11, option (c)) ([source](https://github.com/wazero/wazero/blob/main/cache.go)) |
| Compilation | Once per digest and memory limit, off the request path, deduplicated by Ruralz because the cache is not locked during compilation ([source](https://github.com/wazero/wazero/blob/main/cache.go)); one compile at a time with `experimental.WithCompilationWorkers` at most max(1, `GOMAXPROCS`/4) ([source](https://github.com/wazero/wazero/blob/main/experimental/compilationworkers.go)) |
| Instances | `InstantiateModule` on the shared compiled code, one instance per concurrent Phase call, because `api.Function.Call` is not goroutine-safe ([source](https://github.com/wazero/wazero/blob/main/api/wasm.go)); a pool manager instantiates in the background, never on the request path |
| Memory | `WithMemoryLimitPages` from `limits.memoryBytes`, default 16 MiB (target), replacing wazero's 65536-page (4 GiB) default ([source](https://github.com/wazero/wazero/blob/main/config.go)); Gateway `limits.maxPluginMemoryBytes` caps the Node |
| Time | `WithCloseOnContextDone(true)` with a per-call deadline from `limits.timeout`, default 5 ms (target), plus a host-side budget of 1,000 Host Function calls per call (target), because wazero has no fuel metering ([source](https://github.com/wazero/wazero/issues/422)) |
| Faults | A trapped or timed-out instance is closed and discarded; its refill is rate-limited per the owning document's [Default limits](../architecture/05-wasm-plugin-system.md#default-limits) |

*Figure 1: how `internal/pluginhost` uses wazero on one Node, from a verified artifact to a Phase call.*

```mermaid
flowchart LR
  art["Verified Plugin artifact, pinned by digest"]
  q["Compile queue: one at a time, deduplicated"]
  rt["wazero runtime per limits.memoryBytes"]
  cc["Shared in-memory compilation cache"]
  cm["Compiled code per digest"]
  pm["Pool manager, off the request path"]
  pool["Instance pool, one instance per call"]
  fc["Filter Chain executor"]
  pc["Phase call under a context deadline"]
  hf["Host Functions behind Capability checks"]
  flt["Trap or deadline: instance discarded, failureMode applies"]
  art --> q --> rt
  rt --- cc
  rt --> cm --> pm --> pool
  fc --> pc
  pool --> pc
  pc --> hf
  pc -.-> flt
```

### Consequences

- Good, because the gateway stays one static `CGO_ENABLED=0` binary and wazero's only `go.mod` dependency is `golang.org/x/sys` ([source](https://github.com/wazero/wazero/blob/main/go.mod)), so G1, G2 and S6 hold.
- Good, because the CLI links the same `internal/pluginhost`, so `ruralz plugin test` enforces production limits exactly as a Node does.
- Good, because compiled code is shared across instances and runtimes in memory ([source](https://github.com/wazero/wazero/blob/main/runtime.go)), so a Hot Reload that keeps a digest reuses its compiled code and pools.
- Bad, because deadline interruption costs 10 to 20x on loop-heavy guests, and no fix had merged by 2026-09-12 ([source](https://github.com/wazero/wazero/issues/2466)); SM-6 is measured with it on, and whether a wall clock suffices stays OQ-wasm-plugin-system-10.
- Bad, because a compile cannot be cancelled and compiled size has no API, so the owning document bounds abandoned compiles to 1 per Node (target) and accounts compiled code as 4 × Wasm bytes (hypothesis), pending OQ-wasm-plugin-system-16.
- Bad, because the only dated comparison, from 2023, has wazero trailing Wasmtime significantly ([source](https://00f.net/2023/01/04/webassembly-benchmark-2023/)), so Ruralz must publish its own numbers (P10).
- Bad, because a compiler defect is a sandbox escape (threat PT-1), mitigated by pinning, `govulncheck`, fuzzing and SM-13 ([Threat model](../architecture/05-wasm-plugin-system.md#threat-model)).
- Bad, because FIPS mode does not cover Wasm ([source](https://go.dev/doc/security/fips140)); FIPS builds, Planned (M5), give Plugins approved cryptography only through the `crypto.use` Capability ([FIPS build](../engineering/01-tech-stack-and-libraries.md#fips-build)).
- Bad, because the project is small, with 14 commits in 90 days and 45 open issues and pull requests ([source](https://github.com/wazero/wazero)), so fixes such as #2466 may land slowly.

### Confirmation

- **Import rule**, Planned (M0): depguard rejects `github.com/tetratelabs/wazero` outside `internal/pluginhost` ([Banned imports](../engineering/02-repository-layout-and-conventions.md#banned-imports)).
- **G1 cross-build and license gate**, Planned (M0): all three binaries build with `CGO_ENABLED=0` for linux/amd64 and linux/arm64, and the gate reads the linked package set ([Admission](../engineering/01-tech-stack-and-libraries.md#admission)).
- **Plugin ABI conformance suite** in `pr-full`, Planned (M2): trap, deadline and memory cases return the same `RZ-PLG` codes on the `ruralzd` and `ruralz plugin test` hosts, with wazero in compiler mode ([Conformance suites](../engineering/03-testing-and-quality-strategy.md#conformance-suites)).
- **SM-6 microbenchmark gate**, Planned (M2): a pooled Phase call with deadline interruption on stays at 50 µs or less at p99 (target) ([Benchmarks and regression gates](../engineering/03-testing-and-quality-strategy.md#benchmarks-and-regression-gates)).
- **Fuzzing**, Planned (M2): one target per Host Function group; out-of-bounds guest pointers trap the guest, never the host ([Fuzzing](../engineering/03-testing-and-quality-strategy.md#fuzzing)).
- **`govulncheck`** on every pull request, Planned (M0); SM-13 keeps unpatched sandbox escapes older than 30 days at zero (target).
- **Review checklist item**: a pull request that enables `NewCompilationCacheWithDir`, disables `WithCloseOnContextDone`, omits `WithMemoryLimitPages` or adds a second WebAssembly runtime MUST amend this ADR.

## Pros and cons of the options

### wazero

- Good, because it provides the documented host pattern: compile once, then instantiate per worker ([source](https://github.com/wazero/wazero/blob/main/runtime.go)).
- Good, because v1.12.0 added exception handling, typed references and extended constant expressions ([source](https://github.com/wazero/wazero/releases/tag/v1.12.0)), so current toolchains' output loads.
- Bad, because it has no fuel or instruction metering; maintainers point to context cancellation and page limits instead ([source](https://github.com/wazero/wazero/issues/422)).
- Bad, because the repository name and Go module path differ ([source](https://github.com/wazero/wazero/blob/main/go.mod)), a naming trap the foundation pack corrected at freeze.

### wasmtime-go

- Good, because it offers fuel (`SetConsumeFuel`), epoch interruption, a stack cap and `Store.Limiter` ([source](https://github.com/bytecodealliance/wasmtime-go/blob/main/config.go)) ([source](https://github.com/bytecodealliance/wasmtime-go/blob/main/store.go)).
- Bad, because it links the prebuilt Rust `libwasmtime` through CGO ([source](https://github.com/bytecodealliance/wasmtime-go/blob/main/ffi.go)), which breaks G1 and ADR-0001; the tech stack lists it under [Alternatives not chosen](../engineering/01-tech-stack-and-libraries.md#alternatives-not-chosen) until an ADR accepts CGO.

### Extism go-sdk

- Good, because it wraps wazero's pooling as `CompiledPlugin.Instance()` and carries manifest limits such as `memory.max_pages` and `timeout_ms` ([source](https://github.com/extism/go-sdk/blob/main/extism.go)).
- Bad, because its last commit was 2025-05-14 and it pins wazero v1.9.0 ([source](https://github.com/extism/go-sdk/blob/main/go.mod)), which fails S1; Ruralz reuses Extism's guest conventions only ([ADR-0005](0005-plugin-abi-v1.md)).

### mosn proxy-wasm-go-host

- Good, because it would run proxy-wasm filters without new host code.
- Bad, because it was last pushed in 2024 ([source](https://github.com/mosn/proxy-wasm-go-host)), pins wazero v1.2.1 beside `wasmer-go`, and its original repository is gone ([source](https://github.com/tetratelabs/proxy-wasm-go-host)); the Planned (M4) adapter is Ruralz code on this runtime instead.

## More information

- Owning document: [WASM plugin system](../architecture/05-wasm-plugin-system.md#runtime), with [Default limits](../architecture/05-wasm-plugin-system.md#default-limits) and [Performance model](../architecture/05-wasm-plugin-system.md#performance-model); the catalog row is in [Tech stack and libraries](../engineering/01-tech-stack-and-libraries.md#library-catalog).
- Research: [Go runtime libraries](../_meta/research/go-libraries-runtime.md) section 2 and [Tooling and licenses](../_meta/research/tooling-and-licenses.md) section 4. The rejection of the repository path is inferred from the proxy's `.mod` file and was not reproduced with a Go toolchain.
- Related decisions: [ADR-0001](0001-implementation-language-go.md) (static builds), [ADR-0005](0005-plugin-abi-v1.md) (Plugin ABI v1 and the proxy-wasm adapter) and [ADR-0017](0017-artifact-signing.md) (Plugin signatures verified before compilation).
- Revisit when wazero ships metering or a cheaper interruption fix for issue #2466, or if fuel metering becomes mandatory, which would need an ADR accepting CGO for wasmtime-go.
- Proposed owning-document amendment: [Data plane](../architecture/03-data-plane.md#durable-and-shared-state) still lists an on-disk `${RURALZ_DATA_DIR}/cache/wazero/` directory, which contradicts the in-memory-only rule; it SHOULD drop that row and close OQ-data-plane-11 with option (c).
