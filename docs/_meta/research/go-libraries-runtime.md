# Go Runtime and Library Facts for the Gateway

| Field | Value |
|---|---|
| Topic | Go toolchain (1.26/1.27) and third-party Go libraries named or considered in the tech-stack decisions: WASM runtimes, HTTP/2, HTTP/3, RPC, telemetry, Raft, Redis clients, JOSE/OIDC, and policy engines |
| Snapshot date | 2026-09-23 |
| Method | Primary sources only: GitHub release pages, `go.mod` files, READMEs and source doc comments read through the GitHub REST API (`gh api`) on 2026-09-23; go.dev release notes and security docs; pkg.go.dev; project issue trackers. "Health" counts are GitHub API values on 2026-09-23. "Commits (90d)" means commits on the default branch since 2026-06-25. `open_issues_count` from the GitHub API includes open PRs. No benchmarks were run. |

## 1. Go toolchain: 1.26 and 1.27

### 1.1 Release dates

| Release | Date | Source |
|---|---|---|
| go1.26.0 | 2026-02-10 | (https://go.dev/doc/devel/release) |
| go1.26.8 (latest 1.26 patch) | 2026-09-01 | (https://go.dev/doc/devel/release) |
| go1.27.0 | 2026-08-19 | (https://go.dev/doc/devel/release) |
| go1.27.1 (latest) | 2026-09-01 | (https://go.dev/doc/devel/release) |

### 1.2 Go 1.26 items relevant to servers

- The Green Tea garbage collector is on by default. The notes claim a 10–40% cut in GC overhead in real programs, plus about 10% more on amd64 CPUs with vector instructions (Intel Ice Lake, AMD Zen 4 and newer). You can opt out with `GOEXPERIMENT=nogreenteagc`, which the notes expect to be removed in Go 1.27 (https://go.dev/doc/go1.26).
- The baseline overhead of a cgo call drops by about 30%. This does not affect a `CGO_ENABLED=0` build (https://go.dev/doc/go1.26).
- On 64-bit platforms the heap base address is randomized. Opt out with `GOEXPERIMENT=norandomizedheapbase64` (https://go.dev/doc/go1.26).
- An experimental `goroutineleak` profile (`GOEXPERIMENT=goroutineleakprofile`) is available at `/debug/pprof/goroutineleak` (https://go.dev/doc/go1.26).
- New `runtime/metrics` scheduler metrics: `/sched/goroutines`, `/sched/threads:threads` and `/sched/goroutines-created:goroutines` (https://go.dev/doc/go1.26).
- `crypto/tls` enables the post-quantum hybrid key exchanges `SecP256r1MLKEM768` and `SecP384r1MLKEM1024` by default. Disable them with `GODEBUG=tlssecpmlkem=0` (https://go.dev/doc/go1.26).
- The notes list `GODEBUG` settings deprecated for removal in Go 1.27: `tlsrsakex`, `tls10server`, `tls3des`, `tlsunsafeekm` and `x509keypairleaf` (https://go.dev/doc/go1.26).
- `net/http` adds `HTTP2Config.StrictMaxConcurrentRequests` and `Transport.NewClientConn()` (https://go.dev/doc/go1.26).
- `ReverseProxy.Director` is deprecated in favor of `ReverseProxy.Rewrite` (https://go.dev/doc/go1.26).
- New `log/slog.NewMultiHandler()` (https://go.dev/doc/go1.26).
- `GOFIPS140` can select the frozen Go Cryptographic Module v1.26.0. New `crypto/fips140` helpers: `WithoutEnforcement()`, `Enforced()` and `Version()` (https://go.dev/doc/go1.26).

### 1.3 Go 1.27 items relevant to servers

- **Size-specialized malloc.** Some allocations under 80 bytes are up to 30% cheaper. The expected gain is about 1% in allocation-heavy programs, and binaries grow by about 60 KB. Opt out with `GOEXPERIMENT=nosizespecializedmalloc`, expected to be removed in 1.28 (https://go.dev/doc/go1.27).
- **`goroutineleak` profile is GA.** It is served at `/debug/pprof/goroutineleak`, and the `goroutineleakprofile` experiment is deleted (https://go.dev/doc/go1.27).
- **Tracebacks show goroutine labels.** Tracebacks now include pprof goroutine labels for modules whose `go` directive is 1.27 or later. Opt out with `GODEBUG=tracebacklabels=0` (https://go.dev/doc/go1.27).
- **HTTP/2 client priority (RFC 9218).** The HTTP/2 server honors client priority signals. Opt out with `Server.DisableClientPriority` (https://go.dev/doc/go1.27).
- **HTTP/1 body draining.** On close, HTTP/1 `Response.Body` drains unread content up to a conservative limit so connections can be reused (https://go.dev/doc/go1.27).
- **Header value limit.** New `Server.MaxHeaderValueCount`, with `DefaultMaxHeaderValueCount` as the default (https://go.dev/doc/go1.27).
- **Post-quantum TLS.** New `crypto/mldsa` package (FIPS 204). TLS 1.3 supports the ML-DSA signatures `MLDSA44`, `MLDSA65` and `MLDSA87`, and the MLKEM1024 key exchange (https://tip.golang.org/doc/go1.27).
- **`encoding/json/v2` and `encoding/json/jsontext` ship in std.** `encoding/json` v1 is now backed by the v2 implementation. Opt out with `GOEXPERIMENT=nojsonv2` (https://go.dev/doc/go1.27).
- **HTTP/2 moved into std.** The HTTP/2 implementation moved from the bundled `net/http/h2_bundle.go` (present on `release-branch.go1.26`) into `net/http/internal/http2` (present on `release-branch.go1.27`) (https://github.com/golang/go/tree/release-branch.go1.27/src/net/http/internal) (https://github.com/golang/go/tree/release-branch.go1.26/src/net/http).
- **Not in the 1.27 notes.** There is no HTTP/3, PGO or `GOFIPS140` change in the Go 1.27 notes (https://go.dev/doc/go1.27).

### 1.4 FIPS 140-3 (`GOFIPS140`)

| Module version | Frozen from | CMVP status | Certificates | Source |
|---|---|---|---|---|
| v1.0.0 | Go 1.24 | Validated | CMVP #5247, CAVP A6650 | (https://go.dev/doc/security/fips140) |
| v1.26.0 | Go 1.26 | "Pending Review" on the CMVP Modules In Process List | CAVP A8028. Entropy source ESV #E318 (CAVP A7715) | (https://go.dev/doc/security/fips140) |

- `GOFIPS140` values are `off` (default), `latest`, `v1.0.0`, `v1.26.0`, `inprocess` and `certified`. Any value other than `off` turns FIPS mode on by default (https://go.dev/doc/security/fips140).
- The runtime `GODEBUG` values are `fips140=off|on|only`. The docs say `only` is "not intended to be used in production" (https://go.dev/doc/security/fips140).
- FIPS mode is not supported on OpenBSD, Wasm, AIX or 32-bit Windows (https://go.dev/doc/security/fips140).
- `GOFIPS140` and the `fips140` GODEBUG first appeared in Go 1.24 (February 2025) (https://go.dev/doc/go1.24).

### 1.5 PGO

- Since Go 1.21, `go build` defaults to `-pgo=auto` and picks up a `default.pgo` CPU profile from the main package directory (https://go.dev/doc/pgo).
- The docs report gains of about 2–14% on a representative set of programs as of Go 1.22 (https://go.dev/doc/pgo).
- The first PGO build takes longer to compile, and binaries may grow slightly (https://go.dev/doc/pgo).

## 2. WASM runtimes

### 2.1 Comparison

| Library | Latest version (date) | License | CGO | Commits (90d) / open issues+PRs | Source |
|---|---|---|---|---|---|
| wazero | v1.12.0 (2026-05-29) | Apache-2.0 | No | 14 / 45 | (https://github.com/wazero/wazero/releases/tag/v1.12.0) (https://github.com/wazero/wazero) |
| wasmtime-go | v49.0.0 tag (2026-09-21), tracking Wasmtime v49.0.0 (2026-09-21) | Apache-2.0 | Yes (links the prebuilt Rust `libwasmtime`) | 4 / 41 | (https://github.com/bytecodealliance/wasmtime-go) (https://github.com/bytecodealliance/wasmtime/releases/tag/v49.0.0) |
| Extism go-sdk | v1.7.1 (2025-03-19) | BSD-3-Clause | No (wazero) | 0 (last commit 2025-05-14) / 5 | (https://github.com/extism/go-sdk/releases/tag/v1.7.1) (https://github.com/extism/go-sdk) |
| proxy-wasm-go-host (mosn) | v0.2.0 (last push 2024-07-22) | Apache-2.0 | No for the wazero backend. It also depends on `wasmer-go` | 0 / 4 | (https://github.com/mosn/proxy-wasm-go-host) (https://github.com/mosn/proxy-wasm-go-host/blob/main/go.mod) |

### 2.2 wazero details

- The repository moved to the `wazero` GitHub org, but the Go module path is still `github.com/tetratelabs/wazero`. `go.mod` on main declares `go 1.25.0` and `require golang.org/x/sys v0.44.0` (https://github.com/wazero/wazero/blob/main/go.mod).
- v1.11.0 (2025-12-19) raised the minimum to Go 1.24 and added `golang.org/x/sys`, wazero's first `go.mod` dependency (https://github.com/wazero/wazero/releases/tag/v1.11.0).
- v1.12.0 (2026-05-29) added extended constant expressions, exception handling, typed references and non-blocking I/O on custom files (https://github.com/wazero/wazero/releases/tag/v1.12.0).
- It is pure Go and "doesn't rely on CGO" (https://github.com/wazero/wazero/blob/main/README.md).
- The Compiler (AOT) mode works only on amd64 and arm64. The interpreter works everywhere and is "often by order of magnitude (10x)" slower than the Compiler (https://github.com/wazero/wazero/blob/main/README.md).
- **Compile cache.** By default compiled code lives in memory only until `Runtime.Close` and is not shared between runtimes. `NewCompilationCache()` shares compiled code in memory across runtimes. `NewCompilationCacheWithDir(dir)` also persists it to disk, keyed on the wazero version (https://github.com/wazero/wazero/blob/main/cache.go) (https://github.com/wazero/wazero/blob/main/config.go).
- **Cache concurrency.** The cache is checked before compiling but not locked during compilation, so concurrent compiles of the same module all do the work. The docs recommend compiling once and sharing the `CompiledModule` (https://github.com/wazero/wazero/blob/main/cache.go).
- **Memory limits.** `RuntimeConfig.WithMemoryLimitPages(n)` caps each memory in 64 KiB pages. The default is 65536 pages, which allows 4 GiB per instance. `WithMemoryCapacityFromMax(true)` allocates the maximum eagerly (https://github.com/wazero/wazero/blob/main/config.go).
- **Custom memory allocator.** `experimental.MemoryAllocator` lets the host supply the `LinearMemory` backing store (https://github.com/wazero/wazero/blob/main/experimental/memory.go).
- **Timeouts.** `WithCloseOnContextDone(true)` stops guest execution when the call's context is cancelled or reaches its deadline, and closes the module. It is off by default because it adds periodic checks (https://github.com/wazero/wazero/blob/main/config.go).
- **Cost of `WithCloseOnContextDone`.** Open issue #2466 (2026-01-11) reports a 10–20x slowdown on loop-heavy guests with it enabled. A prototype that checks every 100 loop iterations brought the overhead to about 60%. As of 2026-09-12 no fix has merged (https://github.com/wazero/wazero/issues/2466).
- **No fuel or instruction metering.** Maintainers point to context cancellation, memory page limits and `FunctionListenerFactory` instead (https://github.com/wazero/wazero/issues/422).
- **Instance pooling.** `api.Function.Call` "is not goroutine-safe", so a host must give each goroutine its own instance or function handle (https://github.com/wazero/wazero/blob/main/api/wasm.go). The documented pattern is to compile once, then call `InstantiateModule` on the shared `CompiledModule`, once per worker (https://github.com/wazero/wazero/blob/main/runtime.go). Extism wraps this pattern as `CompiledPlugin.Instance()` (https://github.com/extism/go-sdk).
- **Parallel compilation.** The experimental `experimental.WithCompilationWorkers(ctx, n)` compiles in parallel (https://github.com/wazero/wazero/blob/main/experimental/compilationworkers.go).

### 2.3 wasmtime-go details

- Installs as `github.com/bytecodealliance/wasmtime-go/v49`. It calls the Wasmtime C API through CGO (https://github.com/bytecodealliance/wasmtime-go/blob/main/README.md).
- Prebuilt libraries ship for linux-x86_64, linux-aarch64, macos-x86_64, macos-aarch64 and windows-x86_64. Non-Windows builds link with `-lwasmtime -lm -ldl -pthread` (https://github.com/bytecodealliance/wasmtime-go/blob/main/ffi.go).
- It has the metering controls that wazero lacks. Fuel: `Config.SetConsumeFuel` with `Store.SetFuel` and `Store.GetFuel`. Epoch interruption: `Config.SetEpochInterruption` with `Store.SetEpochDeadline`. Stack cap: `Config.SetMaxWasmStack`. Resource limits: `Store.Limiter` (https://github.com/bytecodealliance/wasmtime-go/blob/main/config.go) (https://github.com/bytecodealliance/wasmtime-go/blob/main/store.go).

### 2.4 Benchmarks: wazero vs Wasmtime

- The only primary, dated comparison found is from 2023-01-04. It ran on a Scaleway Zen 2 CPU, pinned to one core, with 200 runs per test. Wazero trailed Wasmtime significantly. The author still recommended wazero for embedding in Go "unless performance is absolutely critical" (https://00f.net/2023/01/04/webassembly-benchmark-2023/).
- The wazero maintainers publish no current head-to-head numbers. One host-call microbenchmark (Apple M4 Pro) measured `call_go_host_with_stack` at 47.62 ns/op as the baseline in a 2026-05-08 comment (68.54 ns/op with a proposed change) (https://github.com/wazero/wazero/issues/2466).

### 2.5 Extism

- The go-sdk is built on wazero and pins `github.com/tetratelabs/wazero v1.9.0`. Its `go.mod` declares `go 1.22.0` (https://github.com/extism/go-sdk/blob/main/go.mod).
- Manifest limits: `memory.max_pages`, `memory.max_http_response_bytes`, `memory.max_var_bytes`, `allowed_hosts` and `timeout_ms` (https://github.com/extism/go-sdk/blob/main/extism.go).
- The core Extism repo (`extism/extism`) is still active: v1.30.0 shipped 2026-06-04 and it was last pushed 2026-09-02 (https://github.com/extism/extism/releases).
- The officially supported PDK languages are Rust, JavaScript, Go, Haskell, AssemblyScript, C, Zig and .NET (https://extism.org/docs/concepts/pdk).

### 2.6 proxy-wasm-go-host

- `github.com/tetratelabs/proxy-wasm-go-host` returns HTTP 404 on the GitHub API (https://github.com/tetratelabs/proxy-wasm-go-host).
- The surviving implementation is `mosn.io/proxy-wasm-go-host` (v0.2.0). Its `go.mod` declares `go 1.18` and requires `tetratelabs/wazero v1.2.1` and `wasmerio/wasmer-go v1.0.4` (https://github.com/mosn/proxy-wasm-go-host/blob/main/go.mod).

## 3. HTTP stack

### 3.1 net/http and golang.org/x/net/http2

| Item | Status as of 2026-09-23 | Source |
|---|---|---|
| h2c (cleartext HTTP/2) | Built into `net/http` since Go 1.24 through `Server.Protocols` / `Transport.Protocols` with `UnencryptedHTTP2`. Uses prior knowledge (RFC 9113 §3.3). `Upgrade: h2c` is not supported | (https://go.dev/doc/go1.24) |
| `x/net/http2/h2c` | Deprecated: "Unencrypted HTTP/2 is now supported directly by the net/http package" | (https://pkg.go.dev/golang.org/x/net/http2/h2c) |
| `x/net/http2` `Transport`, `Server`, `ConfigureServer`, `ConfigureTransport(s)`, `ClientConn` | Deprecated by commit "http2: deprecate Transport and Server" (2026-08-27), first released in x/net v0.59.0 (2026-09-08). The rationale: "every supported feature of these types is available through public APIs in the net/http package" | (https://github.com/golang/net/commit/f3ca0345eec6) (https://github.com/golang/go/issues/78064) |
| HTTP/2 settings | `Server.HTTP2` / `Transport.HTTP2` (`HTTP2Config`) since Go 1.24. `StrictMaxConcurrentRequests` added in Go 1.26 | (https://go.dev/doc/go1.24) (https://go.dev/doc/go1.26) |
| Latest x/net | v0.59.0 (2026-09-08), BSD-3-Clause, 51 commits (90d) | (https://pkg.go.dev/golang.org/x/net/http2/h2c) (https://github.com/golang/net) |

- Streaming note: the deprecation covers only `Transport`, `Server`, `ConfigureServer`, `ConfigureTransport(s)`, `ClientConn`/`ClientConnState`, `RoundTripOpt` and `ServeConnOpts`. Other `x/net/http2` APIs, such as `Framer`, are not on the list. The deprecated types are superseded by `net/http` (https://github.com/golang/go/issues/78064).

### 3.2 quic-go and HTTP/3

| Field | Value | Source |
|---|---|---|
| Latest | v0.63.0 (2026-09-22). Still pre-1.0 | (https://github.com/quic-go/quic-go/releases/tag/v0.63.0) |
| License / CGO | MIT / pure Go | (https://github.com/quic-go/quic-go) |
| Health | 110 commits (90d), 212 open issues+PRs, 11,779 stars | (https://github.com/quic-go/quic-go) |
| Go requirement | Go 1.26 or newer since v0.62.0 (2026-08-30) | (https://github.com/quic-go/quic-go/releases/tag/v0.62.0) |
| Cadence | v0.60.0 on 2026-06-06, v0.61.0 on 2026-07-24, v0.62.0 on 2026-08-30, v0.63.0 on 2026-09-22 | (https://github.com/quic-go/quic-go/releases) |

- v0.63.0 breaking changes in `http3`: server requests leave `URL.Scheme` and `URL.Host` empty, as `net/http` does, and stream APIs return `*http3.Error` (https://github.com/quic-go/quic-go/releases/tag/v0.63.0).
- v0.62.0 added RFC 9218 stream priorities (`SetPriority`), `PRIORITY_UPDATE` handling on HTTP/3 servers, `TryWriteAll` and many request-validation fixes (https://github.com/quic-go/quic-go/releases/tag/v0.62.0).
- **FIPS 140-3.** Since v0.60.0, quic-go says it is "ready for use in FIPS 140-3 environments when built with Go 1.26 or newer and used with the Go Cryptographic Module" (https://github.com/quic-go/quic-go/releases/tag/v0.60.0).
  - quic-go does not seek its own validation (https://github.com/quic-go/quic-go/blob/master/FIPS140.md).
  - It turns off FIPS enforcement for Initial packet protection and the Retry integrity tag, and guards the ChaCha20 path (https://github.com/quic-go/quic-go/blob/master/FIPS140.md).
  - It builds AES-GCM packet AEADs by calling the unexported `crypto/tls.aeadAESGCMTLS13` via `go:linkname`, pending golang/go#79219 (https://github.com/quic-go/quic-go/blob/master/FIPS140.md).
  - The tracking issue #5077 closed on 2026-05-23 (https://github.com/quic-go/quic-go/issues/5077).

### 3.3 Standard-library HTTP/3

- Go 1.26 and Go 1.27 ship no HTTP/3 (https://go.dev/doc/go1.26) (https://go.dev/doc/go1.27).
- Proposal golang/go#77440 ("net/http: pluggable HTTP/3", which would add `Protocols.SetHTTP3` and let an external implementation be registered) went on hold on 2026-06-10 (https://github.com/golang/go/issues/77440).
- Proposal golang/go#70914 ("x/net/http3: add experimental HTTP/3 implementation") is also on hold (https://github.com/golang/go/issues/70914).
- On master (the Go 1.28 development branch), CL 835305 "net/http/internal/http3: move HTTP/3 from x/net to std" (mailed 2026-09-18, merged 2026-09-21) added an unexported `net/http/internal/http3` directory. A follow-up CL deletes `x/net/internal/http3` (2026-09-21) (https://github.com/golang/go/issues/70914) (https://github.com/golang/go/tree/master/src/net/http/internal).

## 4. RPC: connect-go vs grpc-go

| Field | connectrpc.com/connect | google.golang.org/grpc | Source |
|---|---|---|---|
| Latest | v1.21.0 (2026-09-08). v2.0.0-alpha.1 (2026-09-11) at `connectrpc.com/connect/v2` | v1.84.0 (2026-09-17) | (https://github.com/connectrpc/connect-go/releases) (https://github.com/grpc/grpc-go/releases/tag/v1.84.0) |
| License / CGO | Apache-2.0 / none | Apache-2.0 / none | (https://github.com/connectrpc/connect-go) (https://github.com/grpc/grpc-go) |
| Health | 29 commits (90d), 27 open | 112 commits (90d), 136 open | (https://github.com/connectrpc/connect-go) (https://github.com/grpc/grpc-go) |
| Protocols served | gRPC, gRPC-Web and Connect, on `net/http` handlers, over HTTP/1.1 or HTTP/2 | gRPC. Native HTTP/2 transport, or `Server.ServeHTTP` on `net/http` | (https://github.com/connectrpc/connect-go/blob/main/README.md) (https://github.com/grpc/grpc-go/blob/master/server.go) |
| Go floor | Two most recent Go majors | `go 1.25.0` in go.mod | (https://github.com/connectrpc/connect-go/blob/main/README.md) (https://github.com/grpc/grpc-go/blob/master/go.mod) |

- connect-go v1.21.0 adds `WithRequestGate`, which runs checks such as authentication after headers arrive and before decompression, unmarshalling or interceptors (https://github.com/connectrpc/connect-go/releases/tag/v1.21.0).
- The v1 module stays "stable and supported indefinitely" on the `v1` branch. v2 changes only the Go API: the wire protocol is unchanged (https://github.com/connectrpc/connect-go/blob/main/README.md) (https://github.com/connectrpc/connect-go/releases/tag/v2.0.0-alpha.1).
- grpc-go's `ServeHTTP` requires HTTP/2, which in practice means TLS with the standard server. The doc comment warns it "does not support some gRPC features available through grpc-go's HTTP/2 server" (https://github.com/grpc/grpc-go/blob/master/server.go).
- grpc-go v1.84.0 replaces the `grpc.lb.pick_first.*` OTel metrics with `grpc.subchannel.*` (gRFC A94). It also fixes several xDS RBAC fail-open bugs (https://github.com/grpc/grpc-go/releases/tag/v1.84.0).

## 5. OpenTelemetry Go

| Signal | Status | Source |
|---|---|---|
| Traces | Stable | (https://github.com/open-telemetry/opentelemetry-go/blob/main/README.md) |
| Metrics | Stable | (https://github.com/open-telemetry/opentelemetry-go/blob/main/README.md) |
| Logs | Release Candidate | (https://github.com/open-telemetry/opentelemetry-go/blob/main/README.md) |

- The latest stable release is v1.46.0 (2026-08-25). It is the last release that supports Go 1.25 and adds the `http/json` protocol to `otlptracehttp` (https://github.com/open-telemetry/opentelemetry-go/releases/tag/v1.46.0).
- v1.47.0-rc.1 (2026-08-28) says v1.47.0 "is expected to include the `v1` release of the OpenTelemetry Go Logs API and SDK" for `go.opentelemetry.io/otel/log` and `go.opentelemetry.io/otel/sdk/log` (https://github.com/open-telemetry/opentelemetry-go/releases/tag/v1.47.0-rc.1).
- The same RC deprecates `otel/log/global` in favor of `otel.Logger` and `otel.SetLoggerProvider`, and drops Go 1.25 (https://github.com/open-telemetry/opentelemetry-go/releases/tag/v1.47.0-rc.1).
- The slog bridge `go.opentelemetry.io/contrib/bridges/otelslog` is at tag v0.20.1, and its module declares `go 1.26.0`. contrib v1.46.0 shipped 2026-08-26 (https://github.com/open-telemetry/opentelemetry-go-contrib/blob/main/bridges/otelslog/go.mod) (https://github.com/open-telemetry/opentelemetry-go-contrib/releases/tag/v1.46.0).
- Health: 353 commits (90d), 151 open issues+PRs, Apache-2.0 (https://github.com/open-telemetry/opentelemetry-go).

## 6. Raft libraries

| Field | hashicorp/raft | go.etcd.io/raft/v3 | lni/dragonboat | Source |
|---|---|---|---|---|
| Latest | v1.8.0 (2026-09-16) | v3.7.0 (2026-06-21/23) | v3.3.8 (2023-09-25). v4 unreleased on master | (https://github.com/hashicorp/raft/releases/tag/v1.8.0) (https://github.com/etcd-io/raft/releases/tag/v3.7.0) (https://github.com/lni/dragonboat/releases) |
| License | MPL-2.0 | Apache-2.0 | Apache-2.0 | (https://github.com/hashicorp/raft) (https://github.com/etcd-io/raft) (https://github.com/lni/dragonboat) |
| Commits (90d) / open | 8 / 55 | 23 / 84 | 0 (last commit 2025-07-23) / 65 | GitHub API (https://github.com/hashicorp/raft) (https://github.com/etcd-io/raft) (https://github.com/lni/dragonboat) |
| Transport | Built in: `NetworkTransport` / TCP (`tcp_transport.go`), in-memory transport | None. "Users must implement their own transportation layer" | Built in, pluggable ("Custom Raft log storage and transport support") | (https://github.com/hashicorp/raft/tree/main) (https://github.com/etcd-io/raft/blob/main/README.md) (https://github.com/lni/dragonboat/blob/master/README.md) |
| Log storage | External `LogStore`/`StableStore`: `raft-boltdb` (pure Go) or `raft-mdb` (cgo) | None. The user implements storage (`MemoryStorage` for tests) | Pebble by default, RocksDB optional | (https://github.com/hashicorp/raft/blob/main/README.md) (https://github.com/etcd-io/raft/blob/main/README.md) (https://github.com/lni/dragonboat/blob/master/README.md) |
| Snapshots | `FileSnapshotStore`, in-memory and discard snapshot stores | Snapshot types in the state machine. The user persists them | Built-in snapshotting and log compaction | (https://github.com/hashicorp/raft/tree/main) (https://github.com/etcd-io/raft/blob/main/README.md) (https://github.com/lni/dragonboat/blob/master/README.md) |

- hashicorp/raft v1.8.0 removes the `armon/go-metrics` compatibility shim, so no build tags are needed. It also persists the commit index in the LogStore to speed up recovery (https://github.com/hashicorp/raft/releases/tag/v1.8.0).
- raft-boltdb v2.4.2 (2026-09-16) is the matching release. Tags v2.4.0 and v2.4.1 "should not be used" (https://github.com/hashicorp/raft-boltdb/releases/tag/v2.4.2).
- The root `github.com/hashicorp/raft-boltdb` module requires `github.com/boltdb/bolt v1.3.1` (https://github.com/hashicorp/raft-boltdb/blob/master/go.mod).
- `github.com/hashicorp/raft-boltdb/v2` requires `go.etcd.io/bbolt v1.4.1` and `go-msgpack/v2`, and declares `go 1.26.0` (https://github.com/hashicorp/raft-boltdb/blob/master/v2/go.mod).
- etcd raft v3.7.0 moved from gogo/protobuf to standard protobuf. It improved the ReadIndex flow and allows bootstrapping from a snapshot that carries only ConfState (https://github.com/etcd-io/raft/blob/main/CHANGELOG/CHANGELOG-3.7.md).
- Dragonboat claims 9 million writes/s (16-byte payloads, 3 nodes, "mid-range hardware", RocksDB, in-memory state machine). On a single Raft group it claims 1.25 million writes/s at 1.3 ms average and 2.6 ms P99, using about 3 cores at 2.8 GHz per server (https://github.com/lni/dragonboat/blob/master/README.md).
- Dragonboat's README says master "is our unstable branch for development" toward v4.0 (https://github.com/lni/dragonboat/blob/master/README.md).

## 7. Redis clients: rueidis vs go-redis v9

| Field | github.com/redis/rueidis | github.com/redis/go-redis/v9 | Source |
|---|---|---|---|
| Latest | v1.0.78 (2026-09-15) | v9.22.0 (2026-08-03). v9.23.0-beta.1 (2026-09-11) | (https://github.com/redis/rueidis/releases/tag/v1.0.78) (https://github.com/redis/go-redis/releases) |
| License | Apache-2.0 | BSD-2-Clause | (https://github.com/redis/rueidis) (https://github.com/redis/go-redis) |
| Commits (90d) / open | 35 / 12 | 88 / 76 | (https://github.com/redis/rueidis) (https://github.com/redis/go-redis) |
| Auto-pipelining | Default for non-blocking commands. Opt out with `DisableAutoPipelining`, or opt back in per command with `ToPipe()` | `AutoPipeliner` on `Client` and `ClusterClient`, **experimental**, added in v9.22.0 | (https://github.com/redis/rueidis/blob/main/README.md) (https://github.com/redis/go-redis/releases/tag/v9.22.0) |
| Client-side caching | Server-assisted (opt-in tracking) through `DoCache` / `DoMultiCache`. `DefaultCacheBytes` 128 MiB per connection | **Experimental**. Standalone client only, RESP3 only, DB 0 only. Disabled when a credentials provider is set | (https://github.com/redis/rueidis/blob/main/rueidis.go) (https://github.com/redis/go-redis/releases/tag/v9.22.0) |
| RESP3 | RESP3-first. `DisableCache` is required when the server lacks RESP3/tracking. RESP2 cannot mix SUBSCRIBE with other commands | RESP2 or RESP3 through `Protocol: 2/3` | (https://github.com/redis/rueidis/blob/main/rueidis.go) (https://github.com/redis/go-redis/blob/master/README.md) |
| Server versions | Valkey-specific features (Valkey 8.1 availability zone) | Tested against Redis CE 8.0, 8.2, 8.4, 8.8 and 8.10 | (https://github.com/redis/rueidis/blob/main/README.md) (https://github.com/redis/go-redis/blob/master/README.md) |

- rueidis reports about 14x the throughput of go-redis in a local benchmark on a MacBook Pro 16" M1 Pro 2021, with parallelism 64, 16-byte keys and 64-byte values (https://github.com/redis/rueidis/blob/main/README.md).
- go-redis reports about 1M+ SET/s with its blocking `AutoPipeline()` versus about 100k unpipelined, over local loopback, which it calls "indicative, not a guarantee" (https://github.com/redis/go-redis/releases/tag/v9.22.0).

## 8. JOSE / JWT / OIDC

| Library | Latest (date) | License | Commits (90d) / open | Notes | Source |
|---|---|---|---|---|---|
| lestrrat-go/jwx/v4 | v4.5.0 (2026-09-08). v4.0.0 on 2026-04-19 | MIT | 76 / 5 | Full JWA/JWE/JWK/JWS/JWT with ML-KEM, ML-DSA and HPKE | (https://github.com/lestrrat-go/jwx/releases/tag/v4.5.0) (https://github.com/lestrrat-go/jwx/blob/develop/v4/README.md) |
| golang-jwt/jwt/v5 | v5.3.1 (2026-01-28) | MIT | 3 / 49 | JWT only. `go 1.21` in go.mod | (https://github.com/golang-jwt/jwt/releases/tag/v5.3.1) (https://github.com/golang-jwt/jwt/blob/main/go.mod) |
| go-jose/go-jose/v4 | v4.1.5 (2026-09-03) | Apache-2.0 | 11 / 56 | v4 is stable. v3 gets critical fixes only. A v5 with breaking changes is "forthcoming" | (https://github.com/go-jose/go-jose/releases/tag/v4.1.5) (https://github.com/go-jose/go-jose/blob/main/README.md) |
| coreos/go-oidc/v3 | v3.21.0 (2026-09-01) | Apache-2.0 | 6 / 23 | Requires `go-jose/go-jose/v4 v4.1.4` and `x/oauth2 v0.36.0` | (https://github.com/coreos/go-oidc/releases/tag/v3.21.0) (https://github.com/coreos/go-oidc/blob/v3/go.mod) |

- jwx v4 requires Go 1.26 or later, and on Go 1.26 it also requires `GOEXPERIMENT=jsonv2`. It uses `encoding/json/v2`, which is standard from Go 1.27 (https://github.com/lestrrat-go/jwx/blob/develop/v4/README.md).
- v4.5.0 and v3.3.0 fix GHSA-4cf7-xm37-g63h, where unescaped custom claim or header names allowed member injection. The release says "v2, v1, and v0 are unmaintained" (https://github.com/lestrrat-go/jwx/releases/tag/v4.5.0).

## 9. Policy engines

### 9.1 cel-go (moved to `cel.dev/cel-go`)

- `github.com/google/cel-go` is now a "READ-ONLY COMPATIBILITY REPOSITORY" of type aliases. The canonical code is `github.com/cel-expr/cel-go` under the import path `cel.dev/cel-go`, and the old repository "will eventually be removed" (https://github.com/google/cel-go/blob/main/README.md).
- The latest release is v0.32.0 (2026-08-19), whose breaking change is "Switch module and import paths to cel.dev/cel-go" (#1413) (https://github.com/cel-expr/cel-go/releases/tag/v0.32.0).
- v0.32.0 also adds JWT data types, an HMAC library, aggregate size computations and aggregate semantics in the policy compiler (https://github.com/cel-expr/cel-go/releases/tag/v0.32.0).
- License Apache-2.0. Health: 143 commits (90d), 68 open. `go.mod` declares `go 1.23.0` (https://github.com/cel-expr/cel-go).
- **Policy package.** `cel.dev/cel-go/policy` implements a YAML policy format "inspired by Kubernetes Admission Policy". Policies have `name`, `description`, `imports` and `rule`, and the default semantic is `FIRST_MATCH` (https://github.com/google/cel-go/blob/main/policy/README.md).
- **Cost limits.** `cel.CostLimit(uint64)` (a ProgramOption that implies `OptTrackCost`), `cel.CostTracking(estimator)` and `cel.CostEstimatorOptions` for static estimates. `cel.InterruptCheckFrequency(n)` together with `Program.ContextEval(ctx, vars)` allows cancellation (https://github.com/cel-expr/cel-go/blob/master/cel/options.go) (https://github.com/cel-expr/cel-go/blob/master/cel/program.go).

### 9.2 OPA v1 (Go embedding)

- The latest release is v1.20.2 (2026-09-03). v1.20.0 (2026-08-27) added the Rego keywords `and` and `or`, and made partial evaluation faster (https://github.com/open-policy-agent/opa/releases/tag/v1.20.0).
- License Apache-2.0. Health: 383 commits (90d), 305 open. `go.mod` declares `go 1.26.0` (https://github.com/open-policy-agent/opa) (https://github.com/open-policy-agent/opa/blob/main/go.mod).
- **Embedding API.** Embed through `github.com/open-policy-agent/opa/v1/rego`. `(*Rego).PrepareForEval(ctx)` returns a `PreparedEvalQuery` for repeated `Eval(ctx, ...)` calls, and `PrepareForPartial` does the same for partial evaluation (https://github.com/open-policy-agent/opa/blob/main/v1/rego/rego.go).
- The root `github.com/open-policy-agent/opa/rego` package is marked Deprecated, kept for v0.x transition "for the lifetime of OPA v1.x" (https://github.com/open-policy-agent/opa/blob/main/rego/rego.go).

### 9.3 cedar-go

- The latest release is v1.8.0 (2026-06-01). It includes conformance-driven behavior changes: it rejects IPv6 zone identifiers and changes `IsLoopback`/`IsMulticast` results for IPv4-mapped addresses (https://github.com/cedar-policy/cedar-go/releases/tag/v1.8.0).
- License Apache-2.0. Health: 0 commits since 2026-06-25 (last commit 2026-06-01), 19 open. `go.mod` declares `go 1.23.0` (https://github.com/cedar-policy/cedar-go).
- It includes the core authorizer, all core and extended types (including RFC 80 datetime and duration) and schema parsing (https://github.com/cedar-policy/cedar-go/blob/main/README.md).
- It does not yet include the schema validator (experimental in `x/exp/schema`), the formatter, partial evaluation (experimental `x/exp/batch` only) or policy templates (https://github.com/cedar-policy/cedar-go/blob/main/README.md).

## 10. Summary recommendation table

These recommendations are the analyst's reading of the facts above. Each row's basis is cited in the section it references.

| Concern | Recommended | Alternative / avoid | Basis |
|---|---|---|---|
| WASM runtime | wazero v1.12.0, imported as `github.com/tetratelabs/wazero`. Use a shared `CompilationCache`, `WithMemoryLimitPages`, `WithCloseOnContextDone`, and one instance per worker | wasmtime-go if fuel or epoch metering becomes mandatory, but it needs CGO and conflicts with `CGO_ENABLED=0` | §2.2, §2.3 |
| CPU limiting in plugins | Deadline-based context cancellation now. Budget for the #2466 overhead, or add host-side call budgets | Do not assume wazero fuel exists | §2.2 |
| Plugin SDK conventions | Follow Extism's memory and host-function conventions, but not a runtime dependency on the go-sdk (stale, pins wazero v1.9.0) | — | §2.5 |
| proxy-wasm adapter | Write it in-house on wazero | mosn/proxy-wasm-go-host (stale since 2024, wazero v1.2.1) | §2.6 |
| HTTP/1.1 and HTTP/2 | `net/http` with `Server.Protocols`, `HTTP2Config` and `UnencryptedHTTP2` for h2c | `x/net/http2` Server/Transport and `h2c` (deprecated) | §3.1 |
| HTTP/3 | quic-go/http3 v0.63.x, pinned. Expect breaking changes on minor releases | Go stdlib HTTP/3 (internal only, unreleased) | §3.2, §3.3 |
| gRPC ingress | connect-go v1.21.x (v1 module) | connect v2 (alpha) | §4 |
| gRPC upstream client | grpc-go v1.84.x | — | §4 |
| Telemetry | OTel Go v1.46.x traces and metrics. Adopt logs at v1.47.0 once released stable. otelslog bridge until then | — | §5 |
| Control Store | hashicorp/raft v1.8.0 + `raft-boltdb/v2` v2.4.2 (bbolt). Note the MPL-2.0 license | dragonboat (no release since 2023). etcd raft (build your own transport and storage) | §6 |
| State Store client | rueidis v1.0.78 | go-redis v9.22 (its CSC and auto-pipelining are experimental) | §7 |
| JOSE | jwx v4.5.0 (needs Go 1.27, or Go 1.26 + `GOEXPERIMENT=jsonv2`) | golang-jwt (JWT only) | §8 |
| OIDC discovery | go-oidc v3.21.0, which brings in go-jose v4 alongside jwx | — | §8 |
| Expressions | cel-go v0.32.0 via `cel.dev/cel-go`, with `CostLimit` and `ContextEval` | `github.com/google/cel-go` import path | §9.1 |
| Rego | OPA v1.20.x `v1/rego` with `PrepareForEval` | Root `opa/rego` (deprecated) | §9.2 |
| Cedar | cedar-go v1.8.0 (core authorizer only) | — | §9.3 |
| Toolchain | Go 1.27.1 as the build floor if jwx v4 is used. `GOFIPS140=v1.0.0` (validated) for the FIPS variant, `v1.26.0` once certified | — | §1, §8 |

## Sources

https://go.dev/doc/devel/release
https://go.dev/doc/go1.24
https://go.dev/doc/go1.26
https://go.dev/doc/go1.27
https://tip.golang.org/doc/go1.27
https://go.dev/doc/security/fips140
https://go.dev/doc/pgo
https://github.com/golang/go/tree/release-branch.go1.27/src/net/http/internal
https://github.com/golang/go/tree/release-branch.go1.26/src/net/http
https://github.com/golang/go/tree/master/src/net/http/internal
https://github.com/golang/go/issues/78064
https://github.com/golang/go/issues/77440
https://github.com/golang/go/issues/70914
https://github.com/golang/net
https://github.com/golang/net/commit/f3ca0345eec6
https://pkg.go.dev/golang.org/x/net/http2/h2c
https://github.com/wazero/wazero
https://github.com/wazero/wazero/releases/tag/v1.12.0
https://github.com/wazero/wazero/releases/tag/v1.11.0
https://github.com/wazero/wazero/blob/main/go.mod
https://github.com/wazero/wazero/blob/main/README.md
https://github.com/wazero/wazero/blob/main/config.go
https://github.com/wazero/wazero/blob/main/cache.go
https://github.com/wazero/wazero/blob/main/runtime.go
https://github.com/wazero/wazero/blob/main/api/wasm.go
https://github.com/wazero/wazero/blob/main/experimental/memory.go
https://github.com/wazero/wazero/blob/main/experimental/compilationworkers.go
https://github.com/wazero/wazero/issues/2466
https://github.com/wazero/wazero/issues/422
https://00f.net/2023/01/04/webassembly-benchmark-2023/
https://github.com/bytecodealliance/wasmtime-go
https://github.com/bytecodealliance/wasmtime-go/blob/main/README.md
https://github.com/bytecodealliance/wasmtime-go/blob/main/ffi.go
https://github.com/bytecodealliance/wasmtime-go/blob/main/config.go
https://github.com/bytecodealliance/wasmtime-go/blob/main/store.go
https://github.com/bytecodealliance/wasmtime/releases/tag/v49.0.0
https://github.com/extism/go-sdk
https://github.com/extism/go-sdk/releases/tag/v1.7.1
https://github.com/extism/go-sdk/blob/main/go.mod
https://github.com/extism/go-sdk/blob/main/extism.go
https://github.com/extism/extism/releases
https://extism.org/docs/concepts/pdk
https://github.com/tetratelabs/proxy-wasm-go-host
https://github.com/mosn/proxy-wasm-go-host
https://github.com/mosn/proxy-wasm-go-host/blob/main/go.mod
https://github.com/quic-go/quic-go
https://github.com/quic-go/quic-go/releases
https://github.com/quic-go/quic-go/releases/tag/v0.63.0
https://github.com/quic-go/quic-go/releases/tag/v0.62.0
https://github.com/quic-go/quic-go/releases/tag/v0.60.0
https://github.com/quic-go/quic-go/blob/master/FIPS140.md
https://github.com/quic-go/quic-go/issues/5077
https://github.com/connectrpc/connect-go
https://github.com/connectrpc/connect-go/releases
https://github.com/connectrpc/connect-go/releases/tag/v1.21.0
https://github.com/connectrpc/connect-go/releases/tag/v2.0.0-alpha.1
https://github.com/connectrpc/connect-go/blob/main/README.md
https://github.com/grpc/grpc-go
https://github.com/grpc/grpc-go/releases/tag/v1.84.0
https://github.com/grpc/grpc-go/blob/master/server.go
https://github.com/grpc/grpc-go/blob/master/go.mod
https://github.com/open-telemetry/opentelemetry-go
https://github.com/open-telemetry/opentelemetry-go/blob/main/README.md
https://github.com/open-telemetry/opentelemetry-go/releases/tag/v1.46.0
https://github.com/open-telemetry/opentelemetry-go/releases/tag/v1.47.0-rc.1
https://github.com/open-telemetry/opentelemetry-go-contrib/releases/tag/v1.46.0
https://github.com/open-telemetry/opentelemetry-go-contrib/blob/main/bridges/otelslog/go.mod
https://github.com/hashicorp/raft
https://github.com/hashicorp/raft/tree/main
https://github.com/hashicorp/raft/blob/main/README.md
https://github.com/hashicorp/raft/releases/tag/v1.8.0
https://github.com/hashicorp/raft-boltdb/releases/tag/v2.4.2
https://github.com/hashicorp/raft-boltdb/blob/master/go.mod
https://github.com/hashicorp/raft-boltdb/blob/master/v2/go.mod
https://github.com/etcd-io/raft
https://github.com/etcd-io/raft/releases/tag/v3.7.0
https://github.com/etcd-io/raft/blob/main/README.md
https://github.com/etcd-io/raft/blob/main/CHANGELOG/CHANGELOG-3.7.md
https://github.com/lni/dragonboat
https://github.com/lni/dragonboat/releases
https://github.com/lni/dragonboat/blob/master/README.md
https://github.com/redis/rueidis
https://github.com/redis/rueidis/releases/tag/v1.0.78
https://github.com/redis/rueidis/blob/main/README.md
https://github.com/redis/rueidis/blob/main/rueidis.go
https://github.com/redis/go-redis
https://github.com/redis/go-redis/releases
https://github.com/redis/go-redis/releases/tag/v9.22.0
https://github.com/redis/go-redis/blob/master/README.md
https://github.com/lestrrat-go/jwx/releases/tag/v4.5.0
https://github.com/lestrrat-go/jwx/blob/develop/v4/README.md
https://github.com/golang-jwt/jwt/releases/tag/v5.3.1
https://github.com/golang-jwt/jwt/blob/main/go.mod
https://github.com/go-jose/go-jose/releases/tag/v4.1.5
https://github.com/go-jose/go-jose/blob/main/README.md
https://github.com/coreos/go-oidc/releases/tag/v3.21.0
https://github.com/coreos/go-oidc/blob/v3/go.mod
https://github.com/google/cel-go/blob/main/README.md
https://github.com/google/cel-go/blob/main/policy/README.md
https://github.com/cel-expr/cel-go
https://github.com/cel-expr/cel-go/releases/tag/v0.32.0
https://github.com/cel-expr/cel-go/blob/master/cel/options.go
https://github.com/cel-expr/cel-go/blob/master/cel/program.go
https://github.com/open-policy-agent/opa
https://github.com/open-policy-agent/opa/releases/tag/v1.20.0
https://github.com/open-policy-agent/opa/blob/main/go.mod
https://github.com/open-policy-agent/opa/blob/main/v1/rego/rego.go
https://github.com/open-policy-agent/opa/blob/main/rego/rego.go
https://github.com/cedar-policy/cedar-go
https://github.com/cedar-policy/cedar-go/releases/tag/v1.8.0
https://github.com/cedar-policy/cedar-go/blob/main/README.md

## Gaps

- **Foundation-pack module path.** §7 (ADR-0004) names `github.com/wazero/wazero`, but the module path in wazero's `go.mod` is `github.com/tetratelabs/wazero` (https://github.com/wazero/wazero/blob/main/go.mod). This needs a `foundation_amendments` entry.
- **Foundation-pack HTTP/2 dependency.** §7 (ADR-0009) lists `golang.org/x/net/http2`. Its Server and Transport are deprecated as of x/net v0.59.0, and h2c has been in `net/http` since Go 1.24. The ADR wording should become "`net/http` (HTTP/1.1, HTTP/2, h2c)".
- **cel-go import path.** It changed to `cel.dev/cel-go` in v0.32.0. Docs and ADR-0011 should use the new path. The GitHub API returned 0 stars for `google/cel-go`, which is consistent with a replaced compatibility repository but could not be confirmed as intentional.
- **jwx v4 and Go 1.26.** Go 1.26 builds of jwx v4 need `GOEXPERIMENT=jsonv2`. This interacts with ADR-0001 ("Go 1.26 or newer"). Whether a `GOEXPERIMENT` build still counts as a standard build under the project's reproducibility or FIPS policy was not checked.
- **Two JOSE stacks.** go-oidc v3 depends on go-jose v4, so jwx v4 + go-oidc means two JOSE implementations in the binary. go-oidc's own `go.mod` confirms the dependency; the binary-size impact was not measured.
- **Green Tea opt-out in Go 1.27.** Go 1.26 notes said `nogreenteagc` was expected to be removed in Go 1.27. The Go 1.27 notes (as fetched) do not mention its removal, so its status is unverified.
- **PGO GA version.** The fetched summary of go.dev/doc/pgo said PGO became generally available in Go 1.20. Go 1.20 shipped PGO as a preview, and the GA version was not verified from the primary text. Only the `-pgo=auto` default since Go 1.21 is asserted above.
- **Go stdlib HTTP/3.** No release timeline is published. The code on master is `internal` and proposals #77440 and #70914 are on hold, so availability in Go 1.28 would be speculation.
- **Go FIPS module v1.26.0.** It is not yet validated (Modules In Process). No expected certificate date was found.
- **quic-go FIPS internals.** quic-go's FIPS path depends on `go:linkname` into `crypto/tls`, which future Go releases could break (tracked in golang/go#79219). The status of #79219 was not checked.
- **Benchmarks.** No 2025–2026 primary benchmark comparing wazero and Wasmtime was found. The only numbers are from 2023 (Zen 2) and a single host-call microbenchmark. Third-party comparison sites were found but not used as primary sources. The rueidis and go-redis throughput figures are vendor-reported, from laptop or loopback setups.
- **wasmtime-go README.** The README install line says `v49.0.1`, but only a `v49.0.0` tag exists. The README also says prebuilt binaries cover only x86_64, while `ffi.go` and the `build/` directory include linux-aarch64 and macos-aarch64.
- **Missing publish dates.** `bridges/otelslog` v0.20.1 was confirmed as a tag, but its publish date was not retrieved. The etcd raft v3.7.0 date differs between the CHANGELOG (2026-06-21) and the GitHub release (2026-06-23).
- **Upstream proxy-wasm-go-host.** The tetratelabs repository returns 404. Whether it was deleted, made private or renamed could not be determined.
- **cedar-go activity.** There have been no commits since 2026-06-01, although the repo's `pushed_at` is 2026-07-15 (a non-default-branch push). Whether it is still actively maintained is uncertain.
- **grpc-go gRPC-Web.** No primary source was checked for gRPC-Web support in grpc-go, so none is claimed.
