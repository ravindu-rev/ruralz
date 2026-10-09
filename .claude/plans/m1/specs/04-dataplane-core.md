# M1 spec, area 4 of 11: Data plane core (listeners, Router, Filter Chain, snapshots, Hot Reload, admin)

Doc abbreviations used in references: DP = `docs/architecture/03-data-plane.md`; SO = `docs/architecture/01-system-overview.md`; FP = `docs/_meta/foundation-pack.md`; CM = `docs/architecture/02-configuration-model.md`; SEC = `docs/architecture/08-security-and-identity.md`; OBS = `docs/architecture/10-observability.md`; TMR = `docs/architecture/09-traffic-management-and-resilience.md`; MP = `docs/architecture/07-multi-protocol.md`; PBB = `docs/architecture/12-performance-budgets-and-benchmarking.md`; CP = `docs/architecture/04-control-plane-and-gitops.md`; ZDT = `docs/operations/02-zero-downtime-upgrades-and-hot-reload.md`; DT = `docs/operations/01-deployment-topologies.md`; CLI = `docs/reference/01-cli-and-api-surface.md`; ADR9 = `docs/adr/0009-http-stack-net-http-quic-go.md`; ADR15 = `docs/adr/0015-zero-downtime-upgrades-so-reuseport.md`; ADR10 = `docs/adr/0010-telemetry-opentelemetry-first.md`; TS = `docs/engineering/01-tech-stack-and-libraries.md`; RL = `docs/engineering/02-repository-layout-and-conventions.md`; TQ = `docs/engineering/03-testing-and-quality-strategy.md`; RVC = `docs/engineering/04-release-versioning-and-compatibility.md`; RM = `docs/roadmap/01-roadmap-and-milestones.md`. "(target)" and "(hypothesis)" are kept exactly as the docs tag values. "Proposed" marks a value this spec picks where the docs are silent (listed again in section 9).

Sibling specs consumed: area 1 `01-config-load.md` (`internal/config/{diag,tree,loader,validate,hub}`, `internal/secret`), area 2 `02-config-revision.md` (`internal/config/{revision,canonical,precedence}`).

## 1. Scope

Every M1 roadmap item of this area, from RM "M1 Core gateway" → "M1 scope" (rows named in brackets), with the owning design section:

| # | M1 item (RM row) | Design source |
|---|---|---|
| S1 | `ruralzd` Nodes; `node.id` ULID; `RURALZ_*` process settings [Ruralz Gateway; Observability "ULID `node.id`"] | DP "Process and concurrency model"; FP 2 (Env vars), 8.11; CP "Node process configuration"; TS "Concerns not yet selected" (ULID: own code, OQ-tech-stack-and-libraries-15) |
| S2 | Client listeners: HTTP/1.1 and h2c on 8080 TCP, HTTP/2 over TLS on 8443 TCP, HTTP/2 settings, deadlines [Ruralz Gateway] | DP "Listeners and protocols", "Bounded resources"; ADR9 "Decision outcome"; MP "HTTP/1.1, HTTP/2 and HTTP/3" |
| S3 | PROXY protocol v2 and `trustedProxies` (Client address) [Security; Ruralz Gateway] | CM "Gateway"; SEC "IP filtering and GeoIP" (Client address) |
| S4 | Router match and header handling; catch-all fallback, header and query string routing, wildcard routes, virtual hosts [Ruralz Gateway; Traffic and state] | DP "Router" (Compiled structure, Precedence, Worked example); TMR "Traffic shaping"; SEC "Request hardening" |
| S5 | REST and plain HTTP for any other content (Multi-protocol "Anything else") [Ruralz Gateway] | MP "One listener, dispatch after the match" |
| S6 | Filter Chain executor: fixed Phases, Filter class order, short-circuit, `failureMode`, `onLog` [Ruralz Gateway] | DP "Filter Chain execution", "Failure semantics"; FP 4, 8.10, 8.12; SEC "Authentication" (rule 1) |
| S7 | Body buffering gate and tee, request size limits [Configuration "body buffering"] | CM "Body buffering and limits"; DP "Streaming" |
| S8 | Load shedding: in-flight ceiling, connection ceiling, pre-routing writes [Ruralz Gateway "load shedding"] | DP "Bounded resources"; OQ-data-plane-6 (a) |
| S9 | Error format: RFC 9457 problem documents with `code` and `requestId`; RZ-RT registry [Ruralz Gateway "error format"] | DP "Error response format", "RZ-RT registry"; FP 8.6 |
| S10 | Compiled immutable snapshot, atomic swap, pins, retirement, grace, RZ-RT-014 | DP "Configuration snapshots and hot reload"; SO "Compile before swap" |
| S11 | Hot Reload in file mode (T2 watching a rendered directory), Last-Known-Good persistence and boot [Lifecycle] | DP "Sources, Last-Known-Good and Drain"; SO "Last-Known-Good"; ZDT "Configuration hot reload"; FP 8.2 |
| S12 | Drain on SIGTERM; `ruralz node drain` target (local SIGTERM) [Lifecycle; CLI] | ZDT "Drain timeline defaults"; CLI "Rollout, promotion and Node verbs"; OQ-data-plane-4 (b) |
| S13 | Zero-Downtime Upgrade: `SO_REUSEPORT`, CBPF steering, readiness socket, lock handoff (T5 VM with systemd, T1, T2) [Lifecycle] | ADR15; ZDT "Binary upgrades", "In-place handover"; SO "Hot Reload, Rollout and Zero-Downtime Upgrade" |
| S14 | Admin 9901 (TB-9): `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`, `/debug/snapshots`, `/debug/upstreams`, `/config/dump`, `/tap`, and admin auth [Ruralz Gateway] | DP "Admin endpoints"; CLI "Ruralz Gateway admin API", "API authentication"; SEC "Admin ports"; SO "Trust boundaries" TB-9; FP 8.4, 8.5 |
| S15 | Data dir layout, file lock, PID record [Lifecycle; CLI `ruralz node drain`] | FP 8.11; DP "Durable and shared state"; CLI OQ-cli-and-api-surface-10 |
| S16 | Area-owned telemetry: listener, HTTP, filter, config and snapshot metrics; `ruralz.route.match`, `ruralz.filter.<name>` spans; degraded reasons `lkg_boot`, `lkg_write_failed`, `header_limit_capped`, `snapshot_ending_overdue` [Observability] | OBS "Ruralz Gateway metrics", "Degraded states", "Span model", "Access logs" |
| S17 | Quality: protocol conformance (HTTP/1.1, HTTP/2), Router fuzzing, V-1, V-3, V-4, V-6, CE-1, CE-2, R1, O1, C1, C2 constants [Quality; Budgets] | TQ; PBB "Constants the suite verifies"; ZDT "Verification runbook" |

Out of scope here (other areas, consumed through interfaces in section 4): Upstream layer and composition engine (RZ-RT-010, RZ-RT-015, RZ-UP-*), built-in Filters (including RZ-RT-008, RZ-RT-009), CEL, State Store, loader/validation/canonical form, telemetry plumbing, CLI verbs.

## 2. Normative requirements

### A. Process, settings, identity, data dir

1. `ruralzd` MUST run exactly one configuration mode chosen at boot from `RURALZ_CONFIG`: a path means file mode (a rendered Bundle directory, applying no overlay); `oci://...` and `ruralz-control://HOST:8091` are Planned (M2) and in M1 MUST exit with code 2 naming the mode and "Planned (M2)". It never merges sources. [DP "Process and concurrency model"; CP "Node process configuration"; CM "Overlays"]
2. Process settings read once at start (a change needs a handover or Drain and restart, ZDT "Hot Reload versus restart"): `RURALZ_CONFIG` (required; unset exits 2), `RURALZ_DATA_DIR` (default `/var/lib/ruralz`, proposed), `RURALZ_LOG_LEVEL` (`debug`, `info`, `warn`, `error`; default `info`; other values exit 2), `RURALZ_STATE_STORE_URL` (fallback for `Gateway.spec.stateStore.url`; read by the State Store area), `RURALZ_ADMIN_TOKEN_FILE`, `RURALZ_ADMIN_METRICS_TOKEN_FILE`, `RURALZ_ADMIN_TLS_DIR` (OQ-security-and-identity-7 (a)), `RURALZ_SECRET_ROOT` (default `/etc/ruralz`), `RURALZ_FETCH_ALLOW` (OQ-security-and-identity-22 (a); consumed by area 1 and the Upstream area). `RURALZ_TRUST_POLICY_FILE`, `RURALZ_ENROLLMENT_TOKEN_FILE`, `RURALZ_REGISTRY_AUTH_FILE` are M2: a set value is logged at `warn` as unused in file mode without OCI. None of them ever changes a Revision. [FP 2 "Env vars"; CM "Environment substitution"; SEC "Secrets" rules 5-6, "Admin ports"]
3. `ruralzd` MUST refuse to start (exit 2) if `RURALZ_SECRET_ROOT` contains `${RURALZ_DATA_DIR}`, `RURALZ_ADMIN_TLS_DIR`, or either admin token file. [SEC "Secrets" rule 5]
4. `ruralzd` accepts no positional arguments or flags in M1 (any argument: exit 2, usage on stderr); `cmd/ruralzd` keeps only wiring. Exit codes: 0 after a completed Drain; 1 fatal runtime error or refused/timed-out handover; 2 usage or settings error. [RL "Where Go code goes"; proposed codes]
5. Data dir layout (this area owns it, DP "Durable and shared state"). All created owner-only (dirs 0700, files 0600) by the lock holder:

   | Path under `${RURALZ_DATA_DIR}` | Contents |
   |---|---|
   | `lock` | Empty file; `flock(LOCK_EX|LOCK_NB)` held for the process lifetime by the holder |
   | `holder.json` | PID record (OQ-cli-and-api-surface-10 (a)): `{"format":"ruralz.holder.v1","pid":<int>,"startTime":<uint64 clock ticks, /proc/<pid>/stat field 22>,"nodeId":"<ULID>","version":"<buildinfo version>"}`, written atomically after taking the lock |
   | `handover.sock` | Owner-only (0600) Unix stream socket served by the holder for readiness gating |
   | `identity/node-id` | `node.id`: 26-character ULID plus `\n`, generated at first boot, never rewritten |
   | `lkg/` | Last-Known-Good and candidate (req 57) |
   | `cache/oci/sha256/` | Reserved, Planned (M2); disposable |

   Nothing else is persisted; compiled Plugin code is memory-only (OQ-data-plane-11 (c)). [FP 8.11; DP "Durable and shared state"; CLI OQ-cli-and-api-surface-10]
6. `node.id` is a ULID generated by own code on `crypto/rand` (OQ-tech-stack-and-libraries-15 decided): 48-bit big-endian Unix milliseconds plus 80 random bits, encoded as 26 characters of Crockford base32 (alphabet `0123456789ABCDEFGHJKMNPQRSTVWXYZ`, uppercase output, parse case-insensitive, rejecting `I L O U`). It is `service.instance.id` and the access log `node_id`; never a metric label. [FP 2, 8.11; OBS "Naming and label rules", "Telemetry pipeline"]
7. Only the process holding the file lock writes Last-Known-Good and `holder.json`; it releases the lock at Drain start (SIGTERM or handover) and never writes under `lkg/` afterwards. A starting process that cannot take the lock (`EWOULDBLOCK`) enters handover (section J) on Linux; on other platforms it exits 1 ("data dir in use"). [SO "Hot Reload, Rollout and Zero-Downtime Upgrade" rule 3; FP 8.11; ADR15 "Identity"]
8. If `NOTIFY_SOCKET` is set, `ruralzd` speaks the systemd notify datagram protocol with the standard library: `READY=1` when first ready, `STOPPING=1` at Drain start, and, in a successor after an accepted handover, `MAINPID=<own pid>` then `READY=1` (OQ-zero-downtime-upgrades-and-hot-reload-7, proposed shape pending research). [ZDT "In-place handover"; DT "T5"]

### B. Listeners and HTTP servers

9. One `net/http` `http.Server` per `Gateway.spec.listeners[]` entry. `protocol: http`: `Server.Protocols` = HTTP/1.1 + `UnencryptedHTTP2` (h2c by prior knowledge only; `Upgrade: h2c` is ignored and the request served as HTTP/1.1). `protocol: https`: HTTP/1.1 + HTTP/2 via ALPN (`NextProtos` `h2`, `http/1.1`). The deprecated `golang.org/x/net/http2` `Server`, `ConfigureServer` and `h2c` are never used; HTTP/2 is configured only through `http.HTTP2Config`. `DisableGeneralOptionsHandler: true` so `OPTIONS *` reaches the Router. [ADR9 "Decision outcome"; DP "Listeners and protocols"; MP "One listener, dispatch after the match"]
10. Fixed per-server settings, set once at start of each server (not configurable in M1, OQ-data-plane-1 (a)): `ReadHeaderTimeout` 10 s; `IdleTimeout` 120 s; `ReadTimeout` and `WriteTimeout` unset; `MaxHeaderBytes` 256 KiB (a block above it gets `net/http`'s plain 431 before any handler); `HTTP2Config{MaxConcurrentStreams: 250, MaxReceiveBufferPerConnection: 256 KiB, MaxReceiveBufferPerStream: 64 KiB, MaxReadFrameSize: 16 KiB}` (all target). Per-connection memory before a handler is about 592 KiB (hypothesis). [DP "Bounded resources"; ADR9 "HTTP/2 settings"]
11. Node-wide client connection ceiling 20,000 (target) across all client listeners: a counting `net.Listener` acquires a slot before calling the inner `Accept` and releases it on `Conn.Close`; at the ceiling it stops calling `Accept` (no refusal, the kernel queue holds). The admin listener is not counted and has its own ceiling of 256 connections (proposed). [DP "Bounded resources"]
12. Body reads and response writes get deadlines through `http.ResponseController`: at admission `SetReadDeadline(t0+10 s)` and `SetWriteDeadline(t0+10 s)` (target); once the Route is fixed, `SetReadDeadline(t0+timeout)` and `SetWriteDeadline(t0+timeout+5 s)` where `timeout` is the Route `timeout` (materialized default 15 s per OQ-traffic-management-and-resilience-6 proposal; the +5 s write slack lets a 504 or 503 be written, proposed). Expiry resets the HTTP/2 stream or closes the HTTP/1.1 connection, freeing pin, unit and goroutine even of a blocked handler. [DP "Bounded resources"; TMR "Deadlines"]
13. Each `https` listener selects its certificate by SNI from `tls.certificates` through `tls.Config.GetConfigForClient`, which builds the handshake config from the current snapshot (min version, certificates in canonical name order, client-certificate mode) and the current secret table, so reloads and rotations reach new handshakes only. `tls.minVersion` default `"1.3"`; `"1.2"` allowed with suites fixed to ECDHE with AES-GCM or ChaCha20-Poly1305. No SNI or no SAN match: Go's first-supported-certificate rule over the name-ordered list. 0-RTT is off. When any Route bound to the listener attaches `auth.mtls`, the handshake requests (never verifies) a client certificate (`tls.RequestClientCert`); the `auth.mtls` Filter verifies. [DP "Listeners and protocols"; SEC "Transport security", "Mutual TLS"]
14. A Hot Reload never mutates a running server. Listener identity for change detection is (`name`, `port`, `protocol`, `proxyProtocol`) (`http3` from M3); a TLS or `hostnames` change needs no new socket. An added or changed listener gets a new socket (bound with `SO_REUSEPORT`, so reusing the same port is legal) and server before the swap; on a reused port the CBPF program steers the group to the new socket before the old one closes; a removed or replaced listener stops accepting after the swap (`Server.Shutdown`) while its connections finish. A bind failure rejects the Revision before the swap with the code of OQ-zero-downtime-upgrades-and-hot-reload-4 (section 7). `admin.port` changes move the admin server the same way. [DP "Listeners and protocols", "Why no in-flight request is dropped" rule 3; ZDT "Validation gate" step 10]
15. A Hot Reload that changes a listener's client-certificate mode sends GOAWAY on its HTTP/2 connections, closes idle connections and sets `Connection: close` on busy HTTP/1.1 ones; an `auth.mtls` Route reached over a connection that requested no certificate (coalesced HTTP/2 included) returns 421 (code owned by Security; section 9). [SEC "Mutual TLS"]
16. PROXY protocol v2 (`listeners[].proxyProtocol: true`, default false): every connection MUST start with a v2 header, parsed on the connection's first `Read` (never in the accept loop) within `ReadHeaderTimeout` 10 s: 12-byte signature `\x0D\x0A\x0D\x0A\x00\x0D\x0A\x51\x55\x49\x54\x0A`; version nibble 2; command `LOCAL` (0x0: use the TCP peer) or `PROXY` (0x1); family/transport `TCP4` (0x11, 12 address bytes) or `TCP6` (0x21, 36 bytes); `AF_UNSPEC` only with `LOCAL`; UDP and UNIX families, v1 text headers, a length above 4,096 bytes (proposed cap) or a truncated header close the connection, counted as `ruralz_listener_connections_total{result="refused"}`. TLVs are skipped. On `https` listeners the header precedes the TLS ClientHello. [CM "Gateway"; SEC "Client address"]
17. Client address `source.ip`/`source.port`: the connection peer is the PROXY v2 source on opted-in listeners (the TCP peer otherwise, and for `LOCAL`); if that peer is inside `Gateway.spec.trustedProxies` (CIDR set, default empty), `source.ip` is the rightmost address in `Forwarded` `for=` (if present) else `X-Forwarded-For` that is outside the list, parsing at most 16 addresses (target); if all parsed addresses are trusted, the leftmost parsed one; parsing stops at the first unparsable entry, keeping the last parsed address (proposed). IPv4-mapped IPv6 is unmapped. Computed once per request before routing and shared by Router CEL, `authz.ip`, access log `client_address` and `/tap`. [SEC "Client address"; OBS "Access logs"]
18. Forwarding headers toward Upstreams, built by this area for the Upstream layer: from an untrusted peer, `X-Forwarded-For` is overwritten with the peer address, `X-Forwarded-Proto` with the listener scheme, `X-Forwarded-Host` with the request host, and `Forwarded` removed ("overwritten, never appended"); from a trusted peer the existing `X-Forwarded-For` is kept with `, <peer>` appended and the others kept if present. [SEC "Client address"]

### C. Request admission, parsing and hardening

19. In-flight units: Node ceiling 20,000 (target), one unit per request (and per parallel composition step and stream pump, taken by other areas through the same API), taken by one atomic add in `ServeHTTP` before anything else; when full, 503 `RZ-RT-005` at once, never queued. During a handover the effective ceiling is 20,000 minus the other process's reported in-flight units (req 70). OQ-data-plane-6 (a): fixed ceiling, no adaptive limiter in M1. [DP "Bounded resources"; ZDT "In-place handover"]
20. Pre-routing responses (`RZ-RT-001`, `RZ-RT-002`, `RZ-RT-005`, `RZ-RT-006`, plus the proposed `RZ-RT-017`) get a 5 s write deadline (target); a counting limiter admits at most 2,000 such writes at once (target); beyond it the handler panics with `http.ErrAbortHandler` without writing. These responses run no Filter Chain. [DP "Bounded resources"]
21. Header limit: after admission the handler measures the header block — HTTP/1.1: `len(request line)+2 + Σ(len(name)+len(value)+4) + 2`; HTTP/2: `Σ(len(name)+len(value)+32)` over pseudo and regular fields (RFC 9113 §6.5.2) — and returns 431 `RZ-RT-002` when it exceeds the pinned snapshot's effective `limits.maxRequestHeaderBytes`. The effective value is min(configured, 256 KiB); a larger configured value is capped and raises `ruralz_node_degraded_info{reason="header_limit_capped"}` while that snapshot is active. More than 256 header fields (target) is also 431 `RZ-RT-002` (Go 1.27 builds MAY additionally set `Server.MaxHeaderValueCount` in a `//go:build go1.27` file, OQ-security-and-identity-20). [DP "Listeners and protocols"; SEC "Request hardening"; OBS "Degraded states"]
22. Framing: a request carrying both `Transfer-Encoding` and `Content-Length`, or `Transfer-Encoding` other than exactly `chunked`, is rejected with 400 `RZ-RT-017` (proposed code) before routing. Hop-by-hop fields are never forwarded: `Connection`, every field it names, `Keep-Alive`, `Proxy-Connection`, `TE` (except `trailers`), `Trailer`, `Transfer-Encoding`, `Upgrade`, `Proxy-Authenticate`, `Proxy-Authorization`. `Upgrade` requests are served as ordinary requests in M1 (an upgrade to an `http` Upstream is never tunneled; its code is OQ-multi-protocol-16, M3). `CONNECT` and HTTP/2 extended CONNECT match no Route (404 `RZ-RT-001`). [SEC "Request hardening"; MP "One listener, dispatch after the match"]
23. Path normalization, input `r.URL.EscapedPath()` (never the decoded `r.URL.Path`), output used identically by the Router, CEL `request.path`, `authz.*`, access log, `/tap` and the forwarded request (only a later `transform.request` rewrites it): (1) reject 400 `RZ-RT-017` if the path contains `%00`, a raw `\` or `%5C`/`%5c` (OQ-security-and-identity-21 (a)), any other control byte, or an invalid `%` escape; (2) decode percent-escapes of unreserved characters (`A-Z a-z 0-9 - . _ ~`) and uppercase the hex digits of the rest; an encoded slash `%2F` stays encoded and never splits a segment; (3) remove dot segments per RFC 3986 §5.2.4 (a path escaping the root clamps to `/`); (4) keep repeated slashes and a trailing slash (significant for `exact` and `template`). The request-target must be origin-form or absolute-form; asterisk-form matches no Route. Normalization is idempotent. [DP "Compiled structure"; SEC "Request hardening", T4]
24. Host: from `r.Host` (HTTP/1.1 `Host`, HTTP/2 `:authority`), lowercased, port stripped (IPv6 literals keep brackets stripped of the port), one trailing dot removed (proposed). If the listener sets `hostnames`, a host matching none of them (same exact and `*.` rules as req 27) gets 404 `RZ-RT-001` (proposed semantics). [DP "Compiled structure"; CM "Gateway"]
25. `request.headers` for CEL and header matching: lowercased names; repeated fields joined with `", "` (RFC 9110). The query string is never logged or put in a signal. [CM "Variables"; OBS O4]

### D. Router

26. One Router per listener is compiled into the snapshot from the Routes bound to it (`Route.spec.listeners`, default all). It uses pre-body data only: listener, host, path, method, headers, gRPC service and method (M3), CEL `match.when` over `request` (no body), `source`, `now`. [DP "Router"; FP 4]
27. Host tables: an exact-host hash map (lowercased, port stripped); a wildcard table of `*.` suffixes grouped by length, longest first; an any-host list (Routes without `hosts`). `*.` matches one or more leading labels and never the bare suffix: `*.shop.example` matches `eu.shop.example` and `a.b.shop.example`, not `shop.example` or `xshop.example`. A `match.hosts` entry has at most one leading `*.` label and no other `*` (validation, RZ-CFG-005, OQ-data-plane-2 (a)). [DP "Compiled structure"; CM "Route"]
28. Path index per host entry: a segment trie for `exact` and `template`; `prefix` matches on segment boundaries (`path == P`, or `path` starts with `P` and (`P` ends with `/` or `path[len(P)] == '/'`); `prefix: /` matches all); `regex` last, RE2 via `regexp`, matched against the whole normalized path (compiled as `^(?:re)$`, proposed). Paths are case-sensitive; a trailing slash is significant for `exact` and `template`. [DP "Compiled structure"]
29. Template grammar (proposed, enforced by `routematch.CheckTemplate` at validation as RZ-CFG-005): begins with `/`; segments split on `/`; each segment is either a literal (non-empty, no `{`, `}`) or exactly `{name}` with `name` matching `[A-Za-z_][A-Za-z0-9_]*`; names unique per template; no partial-segment captures, no catch-all. `{name}` matches exactly one non-empty segment. Captures reach CEL as `request.pathParams` with percent-escapes decoded. [DP "Precedence"; CM "Route"]
30. Candidate filters, applied in precedence order: `methods` (set of exact, case-sensitive tokens; `HEAD` does not imply `GET`, proposed); `headers` (name case-insensitive; `exact` equals the joined value of req 25; `regex` full-matches it; `present: true` requires presence, `present: false` absence, proposed); `grpc` (M3); `when` (compiled CEL, cost limit 1,000,000 units). [DP "Compiled structure"; CM "Limits"]
31. Precedence, a total order, highest first: (1) host tier: exact host, then wildcard with longer suffix, then no `hosts`; (2) path tier: `exact`, `template`, `prefix`, `regex`, no `path`; (3) within `template`: segment by segment from the left, a literal beats a `{param}`; (4) within `prefix`: longer first; (5) constraint count: more of the four fields `methods`, `headers`, `grpc`, `when` present (0 to 4) first; (6) `metadata.name` in byte order. `regex` paths order only by ranks 5 and 6. Identical match criteria are RZ-CFG-023 at validation, using `routematch.Key`. [DP "Precedence"]
32. Selection: the first candidate in that order whose remaining criteria match wins; a `methods`, `headers` or `when == false` mismatch falls through; none left gives 404 `RZ-RT-001`. A `match.when` runtime error (including cost-limit exhaustion) returns 500 `RZ-RT-006` without fallthrough. The span is `ruralz.route.match` with the matched Route name or `_unmatched`. The DP worked example (six Routes, six requests) MUST hold exactly. A catch-all fallback is a Route whose only criterion is `when: "true"` (rank 5 count 1, then name). [DP "Precedence", "Worked example: routing precedence"; TMR "Traffic shaping"; `docs/features/01-feature-catalog.md` "Routing" (Catch-all fallback Route)]
33. Snapshot pin plus Route match: 3 µs p50, 12 µs p99, 0 Ruralz-owned allocations (target) on RH-1; building Routers and chains for 10,000 Routes takes 2 s or less on one core, excluding Plugin compilation (target). [PBB "Per-stage latency budget"; DP "Activation"]

### E. Request lifecycle and dispatch

34. `ServeHTTP` order (normative): take in-flight unit (req 19) → admission deadlines (req 12) → pin snapshot (req 49) → trace context and `requestId` (telemetry area) → header limit (req 21) → framing and path normalization (reqs 22-23) → host and listener `hostnames` (req 24) → `source.ip` (req 17) → Route match (req 32) → Route deadlines; request context deadline `t0 + Route timeout` → declared `Content-Length` above `limits.maxRequestBodyBytes`: 413 `RZ-RT-003` at once without reading the body (no `100 Continue` sent) → dispatch → Filter Chain and Upstream layer → `onLog` → release pin, then unit. [DP "Anatomy of a request"; SO "Request lifecycle"]
35. Dispatch after the match: M1 serves every Route whose Upstreams are `protocol: http` (or a composition of `http` steps) with the HTTP reverse proxy path (Upstream layer's own loop on `http.Transport`); Routes to other protocols cannot exist in an M1 Revision (validation) — the dispatch table is keyed by Upstream protocol so M3 and M4 handlers slot in. [MP "One listener, dispatch after the match"; DP "Upstream layer"]
36. Node-generated responses after the Route is fixed (413 `RZ-RT-003`, 503 `RZ-RT-004`, 504 `RZ-RT-007`, 503/502 `RZ-RT-011`/`RZ-RT-012`, 503 `RZ-RT-014`, 503 `RZ-RT-016`, and Upstream and Filter errors) run `onResponse` like short-circuits (req 42) and `onLog`. `RZ-RT-007`: the Route `timeout` expired before any Upstream attempt; 504. [DP "Short-circuit", "RZ-RT registry"; TMR "Deadlines"]
37. Unsubscribed bodies stream: request and response copy through one pooled 32 KiB buffer per direction (target); a streamed request body is counted and cut at `limits.maxRequestBodyBytes` (decoded bytes): before commit the client gets 413 `RZ-RT-003`, after commit the leg is aborted and the stream reset. [DP "Streaming"; CM "Body buffering and limits"; SEC "Request hardening"]
38. Commit happens under the per-request mutex (req 53): the handler checks whether the request was ended; if so and not yet committed it writes 503 `RZ-RT-014` (or `RZ-RT-016` at the Drain deadline) instead. [DP "Why no in-flight request is dropped" rule 5]

### F. Filter Chain executor

39. Phases, fixed: `onRequestHeaders → onRequestBody → onRoute → onUpstreamRequest → onUpstreamResponseHeaders → onUpstreamResponseBody → onResponse → onLog`, plus `onChunk` (M3 sources; the executor accepts `onChunk` subscribers and never calls the State Store from it). Runs: `onRequestHeaders` once after the Route is fixed; `onRequestBody` once, if subscribed, within the gate caps; `onRoute` once; `onUpstreamRequest` and `onUpstreamResponseHeaders` per leg attempt (retries and steps included); `onUpstreamResponseBody` per leg if subscribed; `onResponse` once before headers reach the client; `onLog` always, after the response, read-only. [FP 4; DP "Phases"]
40. Chains come pre-resolved from area 2's `precedence.Chain` (client Phases and one set per Upstream leg), each Phase already in execution order: Filter class (cors, auth, authz, admission, validation, cache, upstream-auth, transform, custom), then scope (Gateway, Route, Upstream), then list position; response Phases (`onUpstreamResponseHeaders`, `onUpstreamResponseBody`, `onResponse`, response-direction `onChunk`) reversed; `onLog` in request order. Upstream-scoped Policies run only in their leg's Phases. A Phase with no subscriber costs one nil/length check. [FP 8.12; DP "Ordering"; SO "Design principles" (pay only for what is attached)]
41. A Policy's `when` is evaluated once, before its first Phase, with the base variables (plus `response` if that first Phase is a response Phase). `false`: the Policy is skipped for the whole request. Runtime error: `failureMode: closed` → the Policy runs; `open` → skipped. If the chain holds at least one auth-class Policy and `when` skipped every one, the request fails 401 `RZ-AUTH-001` (SEC Authentication rule 1, proposed to Data plane). [CM "Allowed places"; SEC "Authentication"]
42. Short-circuit: a Filter returning *respond* in `onRequestHeaders`, `onRequestBody`, `onRoute` or `onUpstreamRequest` skips to `onResponse` then `onLog`. `onResponse` runs the full response chain on generated responses (CORS and security headers reach a 401) except `transform.response`, `cache` and `ai.semantic-cache`. In `onUpstreamRequest` a short-circuit ends only that leg: for plain `upstreams` its response becomes the response; a composition step records it as its result. `onUpstreamResponseHeaders`, `onUpstreamResponseBody`, `onResponse` cannot short-circuit (the first may request a retry before commit through the Upstream layer); `onResponse` is the last status change. [DP "Short-circuit", "Phases"]
43. A Filter returns *continue*, *respond* or *cannot decide*. Cannot decide covers a failed or timed-out State Store call or remote dependency, a Plugin trap or limit (M2), a runtime error in a Policy `config` CEL field, and a recovered panic. Then `failureMode` applies (FP 8.10), status by Filter class and Phase, code from the Filter when it supplies one, else the class default:

    | Class | Request Phase, `closed` | Request Phase, `open` | Response Phase before commit |
    |---|---|---|---|
    | cors | 503 `RZ-RT-011` | skip (no CORS headers) | closed 502 `RZ-RT-012`; open skip |
    | auth | 401, Filter's `RZ-AUTH-<NNN>` (`RZ-PLG-<NNN>` on a trap) | not allowed (RZ-CFG-029) | n/a |
    | authz | 403, Filter's code, default `RZ-AUTH-015` | not allowed | n/a |
    | admission | 503 Filter's `RZ-STS-<NNN>`; `RZ-RL-<NNN>`/`RZ-AI-<NNN>` on a CEL error | admit unchecked | n/a |
    | validation | 503 `RZ-RT-011` (`RZ-AI-<NNN>` for `ai.guardrail`) | skip the check | `ai.guardrail` 502 `RZ-AI-<NNN>` or skip |
    | cache | 503 Filter's `RZ-STS-<NNN>`; `config.key` error always bypasses | bypass | store skipped |
    | upstream-auth | 401 `RZ-AUTH-020` (pack 8.10; OQ-data-plane-8) | not allowed | n/a |
    | transform | 503 `RZ-RT-011` | skip | closed 502 `RZ-RT-012`; open skip |
    | custom | 503 `RZ-PLG-<NNN>` (M2) | skip | closed 502 `RZ-PLG-<NNN>`; open skip |

    `open` skips the Policy for all its remaining Phases of this request. After commit, an `onChunk` failure ends the stream under `closed` and passes the chunk under `open` (M3). An `onLog` failure reaches telemetry only. A decision never uses `RZ-STS`. A `plugin` Policy takes its `filterClass`'s row with `RZ-PLG` on a trap (M2). [DP "Failure semantics", "Error handling"; FP 8.6, 8.10]
44. Every Filter call records `ruralz_filter_duration_seconds{policy,phase}` (histogram `fast`); every respond outcome `ruralz_filter_short_circuits_total{policy,phase,status_class}`; every cannot-decide `ruralz_filter_failures_total{policy,phase,mode}` (`mode` = `open` or `closed` as applied); sampled requests get one `ruralz.filter.<name>` span per Policy Phase call (`<name>` = Policy `metadata.name`) with type, Phase, outcome, applied `failureMode`; `onLog` creates no span. The access log `short_circuit` field names the Policy and Phase that responded and `failure_modes` up to 8 undecided Policies (target). [DP "Error handling"; OBS "Ruralz Gateway metrics", "Span model", "Access logs"]
45. The executor passes each Filter the per-request State Store deadline (the smaller of the Policy's `stateStoreTimeout` and the time left in the per-request deadline, which is the Route's largest `stateStoreTimeout` measured from the start of `onRequestHeaders`, proposed origin) and a per-Policy call identity so the State client can enforce one blocking round trip per Policy before commit. [FP 8.7; SO "State Store access"]

### G. Body buffering

46. Gate (nothing forwarded until the body is complete; response still uncommitted) when a subscribed `onRequestBody` Filter, a CEL reader of `request.body`, or a gated step needs the whole body; response-side gate when `onUpstreamResponseBody` or a `transform.response` in `onResponse` needs it. Tee (copy kept while streaming) for the store side of `cache` (and `ai.semantic-cache`, M3): out of budget or over its limit it stops copying and skips the store (`ruralz_cache_store_skipped_total`, cache area), never failing the request. [CM "Body buffering and limits"]
47. Node buffer budget `limits.maxBufferedBytes` (default 512 MiB, target), one per Node, carried across Hot Reloads (a new value applies to new reservations). Bytes are reserved as they arrive in 32 KiB increments (target), decoded values charged at the size of built Go values up to 4× the raw limit (target; beyond is oversized). A stream share of 25% (target; 128 MiB at the default) is reserved for streams: gates cannot take it; a subscribed stream reserves 32 KiB before commit from the stream share, then free general budget (M3 users). A failed gate reservation fails with 503 `RZ-RT-004`; a request body over `maxRequestBodyBytes` with 413 `RZ-RT-003`; a buffered plain `upstreams` response over `maxResponseBodyBytes` (default 10 MiB) with 502 `RZ-UP-010` (Upstream area code); step bodies per the composition area (`RZ-RT-015`). Gauge `ruralz_node_buffered_bytes`. Per request raw buffering is at most `maxRequestBodyBytes` plus twice `maxResponseBodyBytes`. [DP "Streaming", "Bounded resources"; CM "Body buffering and limits"; OBS metrics]
48. The rewritten body of a transform is reserved like a decoded value; the Node recomputes `Content-Length` and drops `Content-Encoding` on rewritten bodies (limits count decoded bytes). [DP "Transform Policies"]

### H. Snapshots, pins, retirement

49. A snapshot is the compiled, immutable form of one Revision: per-listener Routers, per-Route chains, CEL programs, Filter instances, Upstream settings and pool handles, literal Consumer key hashes, TLS settings, effective limits, `admin.port`, `trustedProxies`, the Revision digest and its canonical bytes. Resolved secrets, Endpoint sets and the `secretRef`-held key index live beside it, replaced copy-on-write; rotation never mutates a snapshot. New requests see only the active snapshot, published through one `atomic.Pointer`. [DP "Configuration snapshots and hot reload"]
50. One pin per request, held from before routing until `onLog` returns. Pins are cache-line-padded counter stripes (S = min(`GOMAXPROCS` at start, 8) stripes, the metrics stripe count, proposed reuse; stripe index assigned round robin per connection at accept, offset by the HTTP/2 stream ID): load pointer, increment own stripe, re-load pointer; if it changed, decrement and retry. The retirer swaps the pointer then waits for the stripes to sum to zero; zero pins proves no request runs on the snapshot. Pinning allocates nothing. [DP "Why no in-flight request is dropped" rule 1; OBS "Observability principles" (stripes)]
51. Only resources are closed: at zero pins the retirer closes pools and Filter/Plugin instances no newer snapshot shares; memory is left to the GC. [DP rule 2]
52. Bounded retirement: at most K = 2 retired snapshots (target) plus at most one closing or ending. When a new activation would make a third retired snapshot, the oldest becomes *closing*: its streams end at once (WebSocket 1001, gRPC `UNAVAILABLE` trailers, SSE end with retry hint; no stream kinds exist in M1, the hook is kept), and after a 30 s grace period (target) it becomes *ending*: everything still pinned ends with `RZ-RT-014`, counted by `ruralz_snapshot_retirement_ended_total`. Zero pins are expected within 5 s + the largest Plugin `limits.timeout` (0 in M1) + 1 s (target); a snapshot pinned longer stays ending, never freed early, and raises `ruralz_node_degraded_info{reason="snapshot_ending_overdue"}`. GOAWAY is never used for retirement (OQ-data-plane-12 (a)). Gauge `ruralz_config_retired_snapshots`. [DP rules 4-6; OBS "Degraded states"]
53. Ending protocol: at pin time each request registers its per-request record on its stripe's intrusive list (pooled, no allocation; `context.AfterFunc` is an acceptable alternative only if PB-8 still holds). At grace end the retirer walks the lists and runs each callback on its own goroutine (bounded by the in-flight ceiling): (1) cancel the request's root context, aborting Upstream and State Store I/O; (2) under the per-request mutex: if not committed, `SetReadDeadline(past)` and `SetWriteDeadline(now+5 s)` (target) and mark ended so the handler writes 503 `RZ-RT-014`; if committed, set both deadlines in the past (resets the HTTP/2 stream or closes the HTTP/1.1 connection); (3) guest calls are left to the Plugin `limits.timeout` (M2). The Drain deadline reuses this protocol with its own code (req 66). [DP rule 5]
54. Grace is never cut short (OQ-data-plane-13 (a)): the loader keeps only the latest pending Revision; it compiles and activates it only when (retired count < K) or no snapshot is closing or ending, so peak configuration memory is (K + 2) × snapshot size (hypothesis). A burst of activations delays the last by at most grace + ending bound + compile time (target). [DP rule 6; ZDT "Atomic swap"]

### I. Activation, Hot Reload, Last-Known-Good

55. Activation runs off the request path on one loader goroutine, in order: (1) verify (file mode: the watched directory is unsigned; Last-Known-Good and handover candidates: recompute `sha256` over the canonical bytes, mismatch RZ-CFG-027); (2) validate with area 1's loader (parse → … → CEL; rendered directory, no overlay, `${VAR}` from the process environment), canonicalize and digest with area 2; resolve every `secretRef` (RZ-CFG-026); build Routers, chains, CEL programs and Filters with W = max(1, `GOMAXPROCS`/2) workers that yield at least every 100 µs of work (target) (OQ-performance-budgets-and-benchmarking-6 (a)); (3) carry over unchanged pools, Filter instances and token buckets by identity and hash, warm only new ones (a pool that does not fit warms to 0); bind added or changed listeners (req 14); (4) publish with one atomic pointer store; (5) ACK (M2, hook only); (6) write the Revision and its canonical source to `lkg/` as candidate; file mode promotes it to Last-Known-Good at once; (7) retire the previous snapshot. A failure before (4) leaves the running snapshot untouched, logs every error diagnostic, and counts `ruralz_config_activations_total{result="rejected",code=<first error code>}`. A Revision whose digest equals the active one is a no-op. Success counts `{result="activated",code=""}` and `ruralz_config_activation_duration_seconds{stage,size_class}` for `verify`, `compile`, `plugin_compile` (0 in M1), `swap`, `total`; `size_class` `le1000`, `le10000`, `gt10000` Routes. [DP "Activation"; SO "Compile before swap"; ZDT "Validation gate", "Atomic swap"; OBS metrics]
56. Budgets: PB-7 Hot Reload of 5,000 Routes on an idle Node 500 ms or less: verify 25 ms, compile 400 ms, swap 25 ms (target); under S1 at half saturation 1 s or less, gateway-added p99 1.5 ms or less (hypothesis). Cold start to `/readyz` 200: 1,000 Routes 600 ms, 5,000 Routes 1.6 s; from Last-Known-Good 300 ms and 700 ms (target); process start and listener binding 150 ms or less (target). [PBB "Startup and reload time by configuration size", "Hot Reload stages"]
57. Last-Known-Good store under `${RURALZ_DATA_DIR}/lkg/` (proposed layout, version-marked per RVC "Versioned surfaces"): content files `sha256-<64 hex>.json` holding the exact `ruralz.canonical.v1` bytes; pointer files `candidate.json` and `lkg.json` holding `{"format":"ruralz.lkg.v1","digest":"sha256:<64 hex>","writtenAt":"<RFC 3339 UTC>","version":"<ruralzd version>"}`. Writes: temp file in `lkg/`, `fsync`, `rename`, `fsync` the directory; content first, pointer last. Files referenced by neither pointer are deleted after a successful pointer write. Resolved secrets are never written. A failed write is retried with backoff while the Node serves (proposed 1 s doubling to 60 s) and raises `lkg_write_failed` until it succeeds. Unknown `format` refuses the file (logged, treated as absent). `ruralz_config_revision_info{revision="rev-<12 hex>",role="active"|"lkg"}` (at most 2 series). [FP 8.2, 8.11; SO "Last-Known-Good"; DP "Failure semantics" (failed LKG write retried); OBS metrics]
58. Boot order (FP 8.2): (1) upgrade handover: the holder's active Revision from its verified candidate, no boot wait (section J); (2) file mode: a valid Bundle at `RURALZ_CONFIG`, else Last-Known-Good (verify digest, run the pipeline from reference resolution onward via area 1 `FromResources`, resolve secrets) with `ruralz_node_degraded_info{reason="lkg_boot"}` raised until a source Revision activates; (3) Control mode wait of up to 5 s (M2); (4) otherwise not ready, still watching the source. [FP 8.2; SO "Last-Known-Good"; DP "Sources, Last-Known-Good and Drain"]
59. File-mode watcher (no watcher library is catalogued, so standard library polling; proposed values): every 1 s it resolves `RURALZ_CONFIG` through symlinks and fingerprints the root (resolved path, device and inode) and every discovered file (relative path, size, mtime ns, inode). A changed root identity (atomic directory replacement, e.g. a symlink swap), or a one-file Bundle whose `ruralz.yaml` inode changed (atomic rename, as `ruralz dev run` writes), triggers an immediate load; any other change triggers a settle debounce: load once two consecutive fingerprints taken 2 s apart are equal, at the latest 30 s after the first change. SIGHUP forces a rescan (proposed; Go's default SIGHUP exit is suppressed). A failed load keeps the active Revision. [DP "Sources, Last-Known-Good and Drain"; SO "File mode lifecycle"; CLI "Local ruralzd launched by the CLI"]

### J. Readiness, Drain, Zero-Downtime Upgrade

60. `/readyz` returns 200 only when an active validated Revision is loaded, every `secretRef` it uses has resolved, all client listeners are bound and the Node is not draining; else 503 with JSON reasons. It never fails because the Control Stream is lost (M2). A secret that resolves after a cold start is retried with backoff (area 1). [FP 8.5; DP "Admin endpoints"; CM "secretRef"]
61. `/healthz` returns 200 `{"status":"ok"}` while the process responds. `/readyz` body: `{"status":"ready","revision":"rev-<12 hex>"}` or `{"status":"not_ready","reasons":[{"reason":"<enum>","code":"<RZ code, optional>","detail":"<non-secret text>"}]}` with `reason` in `no_revision`, `secrets_unresolved`, `listeners_unbound`, `draining` (proposed shape). [CLI "Admin API conventions"]
62. Drain triggers: SIGTERM or SIGINT (the signal `ruralz node drain` sends, OQ-data-plane-4 (b)), and an accepted handover. A second SIGTERM/SIGINT during a Drain jumps to the Drain deadline (proposed). [DP "Sources, Last-Known-Good and Drain"; CLI "Rollout, promotion and Node verbs"]
63. SIGTERM timeline (target, fixed defaults, OQ-zero-downtime-upgrades-and-hot-reload-1 (a)): 0 s readiness fails at once (`/readyz` 503 `draining`), `STOPPING=1`, lock and `holder.json` released, watcher and loader stop; 0-5 s accept window; 5 s stop accepting: `Server.Shutdown` on every client server (one HTTP/2 GOAWAY with last stream ID 2^31-1 and `NO_ERROR`, OQ-zero-downtime-upgrades-and-hot-reload-3 (a); `Connection: close` on HTTP/1.1; idle connections closed); 5-15 s going-away for long-lived connections, jittered (M3); 25 s Drain deadline, 20 s after stop-accepting: remaining work ends through the ending protocol with 503 `RZ-RT-016` before commit (proposed code, OQ-zero-downtime-upgrades-and-hot-reload-2 (b)); 25-30 s flush the post-commit queue and telemetry, at most 5 s; exit 0 by 30 s after SIGTERM. The admin server serves until exit. `Server.Shutdown` ignores hijacked connections, so the Drain tracks them (none in M1). Routes with a `timeout` above the 20 s deadline are cut at it. [ZDT "Drain timeline defaults"; ADR15 "Drain"]
64. Handover (Linux, in-place; T1, T2, T5): the successor starts with the same `RURALZ_CONFIG`, `RURALZ_DATA_DIR` and effective user ID, finds the live holder (lock busy), loads the holder's candidate through the full gate (verify digest, validate, resolve secrets, compile, warm), binds every client `port` and `admin.port` with `SO_REUSEPORT` without `listen`, then gates over `handover.sock`. [ADR15 "Bind, not listen", "Readiness gating"; ZDT "In-place handover" steps 1-4]
65. Pinning and steering: at startup and after every bind, the holder attaches to each TCP reuseport group (client ports and 9901) an `SO_ATTACH_REUSEPORT_CBPF` program `BPF_RET|BPF_K <index>` selecting its own socket (index 0 when alone). Once the successor is accepted it calls `listen` (its sockets join the groups, index 1, still receiving nothing), sends `listening`; the holder swaps each program to return 1, keeps accepting for a 3 s linger (target), closes idle admin connections and answers in-flight admin requests with `Connection: close`, then closes its listeners; the successor, once sole member and lock holder, re-attaches programs selecting index 0. Handover hosts SHOULD set `net.ipv4.tcp_migrate_req=1` (OQ-zero-downtime-upgrades-and-hot-reload-12 (a)). No listening descriptor ever crosses processes. [ADR15 "Pinning", "Steering", "Readiness move"; ZDT steps 5-6]
66. Handover gate protocol `ruralz.handover.v1` (proposed wire form): newline-delimited JSON over the Unix socket; the holder checks the peer's `SO_PEERCRED` UID equals its own. Successor → `{"type":"ready","format":"ruralz.handover.v1","version":…,"pid":…,"markers":{"handover":"ruralz.handover.v1","lkg":"ruralz.lkg.v1","holder":"ruralz.holder.v1","canonical":"ruralz.canonical.v1"},"activeDigest":"sha256:…"}`. Holder → `{"type":"refused","reason":"unknown_marker"|"older_candidate"|"draining"|"busy"}`, or `{"type":"reload","digest":…}` when it holds a newer candidate (successor reloads and sends `ready` again), or `{"type":"accepted","usage":{…}}` followed by a `usage` message each 1 s (target) carrying `connections`, `inflightUnits`, `bufferedBytes`, `pluginMemoryBytes`, `rssBytes`. Successor → `{"type":"listening"}`; holder → `{"type":"released"}` when it released the lock at Drain start. The holder accepts only its active digest. Unaccepted after 60 s (target) the successor exits 1 holding no listening socket; if the holder exits first, the successor takes the lock and listens normally. An N-1 successor past the binary rollback limit is refused (V-6). [ADR15; ZDT "In-place handover"; RVC "Versioned surfaces", "Binary rollback limit"]
67. Shared ceilings during overlap: the successor admits each Node-wide ceiling (connections, in-flight units, buffered bytes, Plugin memory) minus the holder's last reported usage, and, when a cgroup v2 `memory.max` exists, sets a soft memory limit of 90% of it minus the holder's RSS (target), restoring its own limit when the holder exits. Hosts MUST fit two processes until V-3 verifies sharing (OQ-zero-downtime-upgrades-and-hot-reload-6 (a)). No Node-local state crosses (token buckets restart). [ADR15 "Shared Node-wide ceilings"; ZDT "In-place handover"]
68. Handover Drain: starts at the linger's end with no accept window; the lock is released and `released` sent at Drain start; deadline 20 s later, 23 s after steering (target); `/readyz` 503 is unseen because probes moved in the linger. [ZDT "Drain timeline defaults"; ADR15 "Identity"]

### K. Admin server (9901)

69. Own `net/http` server on `Gateway.spec.admin.port` (default 9901; the default port before any Revision), binding all interfaces (OQ-system-overview-6 decided by SEC). Paths and methods exactly: `GET /healthz`, `GET /readyz` (both MAY be unauthenticated), `GET /metrics`, `GET /debug/pprof/` (and its sub-paths), `GET /debug/snapshots`, `GET /debug/upstreams`, `GET /config/dump`, `GET /tap` (streaming). Any other path 404, other methods 405, all as problem documents. Admin endpoints never change configuration or Node state. A new `/debug/*` path MUST be added to DP and CLI tables. Responses are JSON except `/metrics` and `/debug/pprof/`. [DP "Admin endpoints"; CLI "Ruralz Gateway admin API", "Admin API conventions"]
70. Admin authentication (OQ-security-and-identity-7 (a)): `RURALZ_ADMIN_TOKEN_FILE` (operator token, all paths), `RURALZ_ADMIN_METRICS_TOKEN_FILE` (`/metrics` only), `RURALZ_ADMIN_TLS_DIR` (serves TLS with `tls.crt`/`tls.key` and admits client certificates chaining to `ca.crt`, proposed file names; `VerifyClientCertIfGiven`). Token files are read at start, trailing whitespace trimmed, empty is an error (exit 2). Tokens arrive as `Authorization: Bearer <token>`, compared in constant time, accepted only over TLS or from a loopback peer. `/metrics` needs the metrics token, the operator token or an admitted client certificate on every interface, loopback included. `/debug/*`, `/config/dump` and `/tap` need the operator token or an admitted client certificate and are off (401) without an operator token configured. With no admin credential configured only `/healthz` and `/readyz` answer; every other path is 401. Failures are 401 problem documents (code: section 9 item 5). Admin without TLS reports `ruralz_security_cleartext_hops{hop="admin"}` (and `cleartext_hop`). [SEC "Admin ports"; CLI "API authentication", "Admin API"; SO TB-9]
71. `/metrics`: served by the telemetry area's handler (OTel Prometheus exporter, OQ-tech-stack-and-libraries-16 decided) over Ruralz aggregates; at most one collection in flight, reused for up to 1 s, at most 4 scrapes encoding at once (a fifth waits), pooled 64 KiB buffer, a scrape still writing after 10 s cancelled (target); no suffixes appended. [OBS "Observability principles"]
72. `/debug/pprof/`: the standard profiles via `net/http/pprof` handler functions mounted on the admin mux (never `http.DefaultServeMux`), including the goroutine leak profile where the toolchain provides it (Go 1.27). Mutex and block profiling are enabled only for the duration of a `?seconds=N` request (rates restored after; one such request at a time). [DP "Admin endpoints"; CLI; OBS "Debugging tools"]
73. `/debug/snapshots`: `{"snapshots":[{"state":"active"|"retired"|"closing"|"ending","revision":"rev-…","digest":"sha256:…","pins":<int>,"activatedAt":…,"retiredAt":…,"graceEndsAt":…}],"pending":"sha256:…"|null,"lastKnownGood":"sha256:…"|null}` (proposed shape). `/debug/upstreams`: the Upstream area's JSON (Endpoint sets, health, ejections, breaker states). [DP "Admin endpoints"]
74. `/config/dump`: area 2's document (`canonical.AppendDump`: `content` = exact active canonical bytes, `digest`, `lastKnownGood` when present, `revision`), `Content-Type: application/json`; `secretRef` shown, values never. `/tap` and `/config/dump` use is logged at `info` (path, peer, principal kind). [DP; CLI; area 2 R-73]
75. `/tap`: one JSON object per sampled exchange, newline-delimited (`Content-Type: application/x-ndjson`, proposed), flushed per event; optional `?sample=<0<r≤1>` (default 1, proposed). Event fields (proposed): `time`, `traceId`, `listener`, `protocol`, `route`, `method`, `host`, `path` (normalized, no query), `status`, `code`, `durationMs`, `requestBytes`, `responseBytes`, `upstream`, `endpoint`, `consumer`, `requestHeaders`, `responseHeaders`; header values of `authorization`, `proxy-authorization`, `cookie`, `set-cookie`, every header named by an `auth.api-key` `config.header` in the snapshot, and any name matching `(?i)(token|secret|key|password|session|auth)` are replaced with `[redacted]`; bodies, query strings and secrets never appear. At most 4 subscribers with 1 MiB of buffered events each (target); a slow subscriber drops events counted by `ruralz_tap_events_dropped_total`; a fifth subscriber gets 503 (code: section 9 item 6). Cost without a subscriber: one atomic load, under 10 ns, 0 allocations; with one: 2 µs or less and 1 allocation per event (target). [DP; OBS "Overhead per signal"; SEC "Secrets" rule 2]

### L. Errors and telemetry owned by this area

76. Error body: RFC 9457 `application/problem+json` with members in this order `title`, `status`, `code`, `requestId` (the request's 32-lowercase-hex OpenTelemetry trace ID, present even when unsampled); no `type` (about:blank), no `detail`, never echoing request content; `Cache-Control: no-store` and an exact `Content-Length` (proposed). Example: `{"title": "No matching Route", "status": 404, "code": "RZ-RT-001", "requestId": "4bf92f3577b34da6a3ce929d0e0e4736"}`. Titles are a fixed table per code (RZ-RT titles in `internal/problem`; other areas supply theirs). Every Node-generated response increments `ruralz_http_node_responses_total{code}`. gRPC and WebSocket mappings are M3. [DP "Error response format"; OBS O6]
77. RZ-RT registry as registered in `internal/errcode` (001 404, 002 431, 003 413, 004 503, 005 503, 006 500, 007 504, 008 403, 009 400, 010 404, 011 503, 012 502, 013 stream ended, 014 503 before commit / stream ended after, 015 502), plus the proposed M1 additions RZ-RT-016 (503 before commit, stream ended after: Drain deadline reached) and RZ-RT-017 (400: request target or framing rejected by request hardening), registered in DP "RZ-RT registry" and `internal/errcode` in the same change. [DP "RZ-RT registry"; FP 8.6]
78. Metrics this area records (OBS "Ruralz Gateway metrics"): `ruralz_http_requests_total{route,status_class}`, `ruralz_http_listener_requests_total{listener,protocol,status_class,origin}` (`protocol` `http1`|`http2`; `origin` `upstream`|`node`|`dependency`), `ruralz_http_node_responses_total{code}`, `ruralz_http_request_duration_seconds{route}`, `ruralz_http_gateway_duration_seconds{listener}` and `ruralz_http_gateway_duration_skipped_total{reason="clock_anomaly"}` (excluded-section word algorithm of OBS "Gateway-added time"), `ruralz_http_request_body_bytes{listener}`, `ruralz_http_response_body_bytes{listener}`, `ruralz_http_active_requests{listener}`, `ruralz_listener_open_connections{listener,protocol}`, `ruralz_listener_connections_total{listener,protocol,result}` (`accepted`, `tls_failure`, `refused`), `ruralz_listener_tls_handshake_duration_seconds{listener}` (from accept to a successful end of the listener's own `(*tls.Conn).HandshakeContext`, run with the handshake timeout off the Accept goroutine before the connection reaches `http.Server`; a handshake error counts `ruralz_listener_connections_total{result="tls_failure"}` instead; 06 req 77), the filter, config and snapshot families above, `ruralz_node_buffered_bytes`, `ruralz_tap_events_dropped_total`, `ruralz_security_cleartext_hops{hop="client"|"admin"}`, and `ruralz_node_degraded_info{reason}` for `lkg_boot`, `lkg_write_failed`, `header_limit_capped`, `snapshot_ending_overdue`, `cleartext_hop`. Route labels use `_unmatched` for pre-routing responses. Hot label sets use the per-connection stripe. [OBS]
79. Access log (record owned by the telemetry area) is populated from `onLog` for every request with a pinned snapshot, evaluating `Gateway.spec.telemetry.accessLog.when` (runtime error writes the entry); `RZ-RT-005` rejections are counted but not access-logged, to keep their 2-allocation budget (proposed). The server span is `<method> <route>`. Process logs go through `internal/telemetry` `slog` only; `http.Server.ErrorLog` is bridged there with a rate limit (proposed 10 records/s). [OBS "Access logs", "Span model", "Process logs"; PBB "Per-stage latency budget" (RZ-RT-005 row)]
80. Allocation and CPU budgets checked here: PB-8 30 or fewer Ruralz-owned allocations per S1 request around Router and executor (`testing.AllocsPerRun`, pre-parsed HTTP/1.1 request, discard `ResponseWriter`); `RZ-RT-005` rejection 12 µs p50, 40 µs p99, mean CPU 16 µs or less, 2 allocations; `onResponse` 2/8 µs, 2 allocations; response write of 1 KiB 8/30 µs, 2 allocations (all target). [PBB "Budget catalog", "Per-stage latency budget"; TQ "Benchmarks and regression gates"]

## 3. Proposed Go packages and API

Placement follows RL "Monorepo tree" (`internal/gateway/` holds listener, router, chain, upstream, admin, loader) and "Import boundaries": the CLI never imports `internal/gateway/...` and `internal/gateway/...` never imports `internal/control`, `internal/controlstore` or `internal/cli`. Pieces the CLI or Ruralz Control also need therefore live outside `internal/gateway`: `internal/routematch` (template and wildcard checks run in `ruralz bundle validate`), `internal/nodedir` (`ruralz node drain` reads `holder.json`), `internal/ulid` (Ruralz Control's Rollout IDs, M2), `internal/problem` (Ruralz Control REST errors, M2), `internal/filter` (the Filter SPI built-in Filters implement without importing the gateway). Every file has the license header; no `init()`, no mutable package-level variables (sentinel errors allowed), context first, `%w` wrapping, `slog` only via `internal/telemetry`.

| Package | Responsibility | Third-party imports |
|---|---|---|
| `internal/ulid` | ULID on `crypto/rand` | none |
| `internal/nodedir` | Data dir layout, flock, `holder.json`, `node.id` | `golang.org/x/sys/unix` (flock; `//go:build linux || darwin`) |
| `internal/problem` | RFC 9457 writer, RZ-RT titles | none |
| `internal/routematch` | Host, path, template, prefix primitives; `CheckTemplate`; match identity `Key` | none |
| `internal/filter` | Filter SPI: `Phase`, `Class`, `Exchange`, `Filter`, `Result`, factory registry | none |
| `internal/gateway` | `Run`: supervisor wiring the packages below | none |
| `internal/gateway/setting` | `RURALZ_*` parsing | none |
| `internal/gateway/admission` | In-flight units, pre-routing limiter, connection ceiling | none |
| `internal/gateway/body` | Buffer budget, gate reader, tee, limited reader | none |
| `internal/gateway/router` | Compiled per-listener Router | none (CEL via interface) |
| `internal/gateway/chain` | Filter Chain executor | none |
| `internal/gateway/snapshot` | Snapshot, `Holder`, pins, retirer, ending protocol | none |
| `internal/gateway/compile` | `hub.Bundle` + chains → `*snapshot.Snapshot`, carry-over | none |
| `internal/gateway/listener` | Listener set, servers, TLS, conn wrapper, `source.ip` | none |
| `internal/gateway/listener/proxyproto` | PROXY v2 parser | none |
| `internal/gateway/reuseport` | `SO_REUSEPORT` bind-without-listen, CBPF attach (`linux`); stub elsewhere | `golang.org/x/sys/unix` |
| `internal/gateway/handler` | `http.Handler`: lifecycle glue and dispatch | none |
| `internal/gateway/source` | File-mode polling watcher | none |
| `internal/gateway/reload` | Activation pipeline, pending slot, LKG writes and promotion | none |
| `internal/gateway/lkg` | Last-Known-Good store | none |
| `internal/gateway/readiness` | Readiness reasons | none |
| `internal/gateway/admin` | Admin server, auth, handlers | none (stdlib `net/http/pprof`) |
| `internal/gateway/tap` | `/tap` hub | none |
| `internal/gateway/drain` | Drain coordinator and timelines | none |
| `internal/gateway/handover` | Gate protocol, usage reports, steering orchestration | `golang.org/x/sys/unix` (`SO_PEERCRED`) |
| `internal/gateway/sdnotify` | systemd notify datagrams | none |

depguard additions: admit `golang.org/x/sys/unix` in the strict list; confine it to `**/internal/gateway/reuseport/**`, `**/internal/gateway/handover/**`, `**/internal/nodedir/**`; add `internal/gateway/listener`, `internal/gateway/admin`, `internal/nodedir` to the security-sensitive package list (proposed).

```go
package ulid // internal/ulid

type ULID [16]byte

func New(now time.Time, entropy io.Reader) (ULID, error) // entropy = crypto/rand.Reader in production
func Parse(s string) (ULID, error)                         // 26 chars, case-insensitive, rejects I L O U and overflow
func (u ULID) String() string                              // uppercase Crockford base32
func (u ULID) Time() time.Time
```

```go
package nodedir // internal/nodedir

const (
	LockFile       = "lock"
	HolderFile     = "holder.json"
	HandoverSocket = "handover.sock"
	NodeIDFile     = "identity/node-id"
	LKGDir         = "lkg"
	HolderFormat   = "ruralz.holder.v1"
)

var ErrLocked = errors.New("nodedir: data dir locked by a live process")

type Dir struct{ root string }

func Open(root string) (*Dir, error) // mkdir 0700 root, identity/, lkg/; rejects a root not owned by the euid
func (d *Dir) Path(rel string) string
func (d *Dir) TryLock() (*Lock, error) // ErrLocked when held
type Lock struct{ f *os.File }
func (l *Lock) Release() error

type Holder struct {
	Format    string `json:"format"`
	PID       int    `json:"pid"`
	StartTime uint64 `json:"startTime"`
	NodeID    string `json:"nodeId"`
	Version   string `json:"version"`
}

func (d *Dir) WriteHolder(h Holder) error // atomic; only while holding the Lock
func (d *Dir) ReadHolder() (Holder, error)
func (d *Dir) NodeID(gen func() (ulid.ULID, error)) (ulid.ULID, error) // read or create once
func WriteFileAtomic(dir, name string, data []byte, perm fs.FileMode) error // temp, fsync, rename, fsync dir
```

```go
package problem // internal/problem

const ContentType = "application/problem+json"

type Problem struct {
	Title     string
	Status    int
	Code      string // registered RZ code
	RequestID string // 32 lowercase hex
}

func Title(code string) string                // RZ-RT titles; falls back to errcode meaning
func Append(dst []byte, p Problem) []byte     // {"title","status","code","requestId"} in that order
func Write(w http.ResponseWriter, p Problem)  // headers + body; never reads the request
```

```go
package routematch // internal/routematch

var ErrRejected = errors.New("routematch: request target rejected") // → RZ-RT-017

func NormalizeHost(h string) string                // lowercase, strip port and one trailing dot
func NormalizePath(escaped string) (string, error) // req 23; idempotent

type HostPattern struct {
	Exact  string // set when not a wildcard
	Suffix string // ".shop.example" for "*.shop.example"
}

func ParseHostPattern(s string) (HostPattern, error) // RZ-CFG-005 rules
func (p HostPattern) Match(host string) bool

type Segment struct{ Literal, Param string } // exactly one non-empty
type Template struct{ Segments []Segment; TrailingSlash bool }

func ParseTemplate(s string) (Template, error)
func CheckTemplate(s string) error // implements area 1 validate.TemplateChecker
func PrefixMatch(prefix, path string) bool

type Key string                           // canonical identity of match criteria
func MatchKey(m *v1alpha1.RouteMatch) Key // equal keys ⇔ RZ-CFG-023
```

```go
package filter // internal/filter: the Filter SPI

type Phase uint8

const (
	OnRequestHeaders Phase = iota
	OnRequestBody
	OnRoute
	OnUpstreamRequest
	OnUpstreamResponseHeaders
	OnUpstreamResponseBody
	OnResponse
	OnLog
	OnChunk
	NumPhases
)

func (p Phase) String() string  // "onRequestHeaders" ...
func (p Phase) IsRequest() bool // onRequestHeaders..onUpstreamRequest
func (p Phase) IsResponse() bool

type PhaseSet uint16
type Class uint8 // Cors, Auth, Authz, Admission, Validation, Cache, UpstreamAuth, Transform, Custom (FP 8.12 order)
type Scope uint8 // Gateway, Route, Upstream

type Outcome uint8

const (
	Continue Outcome = iota
	Respond
	CannotDecide
)

type Response struct {
	Status int
	Header http.Header
	Body   []byte
	Code   string // RZ code when Node- or Filter-generated error; "" for a plain response
}

type Result struct {
	Outcome  Outcome
	Response *Response // Respond
	Code     string    // CannotDecide: RZ code to use under closed, "" = class default
	Err      error     // CannotDecide cause, logged and on the span; never sent to the client
}

// Exchange is the per-request state a Filter sees; pooled by the handler, valid only during Handle.
type Exchange struct{ /* unexported */ }

func (x *Exchange) Request() RequestView             // method, scheme, host, normalized path, query, headers
func (x *Exchange) Source() SourceView               // ip netip.Addr, port, tlsVersion, clientCertSubject
func (x *Exchange) RouteName() string
func (x *Exchange) OutboundHeader() http.Header      // mutable request headers toward the leg
func (x *Exchange) Response() *ResponseView          // response Phases: status, mutable headers, buffered body
func (x *Exchange) Leg() LegView                     // leg Phases: upstream name, endpoint, attempt
func (x *Exchange) SetConsumer(c ConsumerRef) error  // second binding → RZ-AUTH-002 (Security area rule)
func (x *Exchange) Consumer() (ConsumerRef, bool)
func (x *Exchange) SetAuth(method string, claims any)
func (x *Exchange) RequestBody(ctx context.Context) ([]byte, error)  // gated body; RZ-RT-003/-004 errors
func (x *Exchange) Budget() Budget                                   // decoded-value reservations
func (x *Exchange) StateDeadline(policyTimeout time.Duration) time.Time // req 45
func (x *Exchange) CELActivation() any                               // lazily built by the CEL area
func (x *Exchange) Annotate(key string, v slog.Value)                // span attribute on the current Filter span

type Filter interface {
	Handle(ctx context.Context, p Phase, x *Exchange) Result
}

// Closer is implemented by Filters holding pools or goroutines; called at zero pins when no newer snapshot shares them.
type Closer interface{ Close() error }

type BuildEnv struct {
	Policy    *v1alpha1.Policy
	Scope     Scope
	Secrets   SecretLookup
	Previous  Filter // same Policy identity and config hash in the previous snapshot, for carry-over; nil if none
}

type Factory interface {
	Build(ctx context.Context, env BuildEnv) (Filter, PhaseSet, error) // error carries an RZ-CFG code
}

type Registry struct{ /* map[v1alpha1.PolicyType]Factory, constructed, immutable */ }

func NewRegistry(f map[v1alpha1.PolicyType]Factory) *Registry
func (r *Registry) Factory(t v1alpha1.PolicyType) (Factory, bool)
```

```go
package admission // internal/gateway/admission

type Units struct{ /* n, limit atomic.Int64 */ }

func NewUnits(limit int64) *Units      // 20_000
func (u *Units) TryAcquire(k int64) bool // atomic add; false (and undo) above the limit
func (u *Units) Release(k int64)
func (u *Units) SetExternal(n int64)     // handover: other process's usage
func (u *Units) InUse() int64

type PreRouting struct{ /* n atomic.Int64; max 2_000 */ }
func (p *PreRouting) TryAcquire() bool
func (p *PreRouting) Release()

type ConnLimiter struct{ /* sem chan struct{} sized 20_000, external usage */ }
func NewConnLimiter(limit int) *ConnLimiter
func (c *ConnLimiter) Wrap(ln net.Listener) net.Listener // Accept blocks at the ceiling; slot freed on Close
```

```go
package body // internal/gateway/body

const Increment = 32 << 10

var (
	ErrTooLarge = errors.New("body: over limit")           // → RZ-RT-003 / RZ-UP-010 / step rule
	ErrBudget   = errors.New("body: buffer budget spent")  // → RZ-RT-004
)

type Budget struct{ /* total, streamShare, used, streamUsed atomic.Int64 */ }

func NewBudget(total int64) *Budget        // streamShare = total / 4
func (b *Budget) SetTotal(total int64)     // on activation; outstanding reservations kept
func (b *Budget) ReserveGate(n int64) bool // general budget only, in Increment steps
func (b *Budget) ReserveStream(n int64) bool
func (b *Budget) Release(n int64)
func (b *Budget) Used() int64

type Buffer struct{ /* pooled chunks, reserved bytes */ }
func (b *Buffer) Bytes() []byte
func (b *Buffer) Release()

func ReadGate(ctx context.Context, r io.Reader, limit int64, bud *Budget) (*Buffer, error)
func LimitReader(r io.Reader, limit int64) io.Reader // streaming cap, ErrTooLarge
type Tee struct{ /* copy while streaming; Stop() on limit or budget */ }
```

```go
package router // internal/gateway/router

type RouteSpec struct { // compile input per bound Route
	Name     string
	Index    int32 // into snapshot.Snapshot.Routes
	Match    *v1alpha1.RouteMatch
	When     BoolProgram // nil when absent
}

type BoolProgram interface {
	EvalBool(ctx context.Context, act any) (bool, error) // cost limit 1,000,000; error → RZ-RT-006
}

type Request struct {
	Host    string // normalized
	Path    string // normalized
	Method  string
	Header  http.Header
	Act     func() any // lazy CEL activation for when
}

type Result struct {
	Index  int32 // -1: RZ-RT-001
	Params []Param // template captures, backed by caller-provided storage
	Err    error   // when runtime error → RZ-RT-006
}

type Param struct{ Name, Value string }

type Router struct{ /* host tables, per-entry path index, ordered candidate lists */ }

func Build(ctx context.Context, routes []RouteSpec, yield func()) (*Router, error)
func (r *Router) Match(ctx context.Context, q *Request, params []Param) Result // 0 allocations
```

```go
package snapshot // internal/gateway/snapshot

type State uint8 // Active, Retired, Closing, Ending, Freed

type Snapshot struct {
	Digest    revision.Digest
	Canonical []byte
	Limits    Limits // effective: header cap applied, body limits, budget total
	AdminPort int
	Trusted   []netip.Prefix
	Listeners []ListenerSpec
	Routers   map[string]*router.Router // by listener name
	Routes    []*Route                   // Route: name, labels, timeout, chain, legs, dispatch kind
	Resources []filter.Closer            // closed at zero pins unless shared
	// unexported: pin stripes, state, timestamps, request lists
}

type Holder struct{ /* atomic.Pointer[Snapshot] */ }

func (h *Holder) Pin(stripe int, rec *Request) *Snapshot // req 50; registers rec for the ending protocol
func (s *Snapshot) Unpin(stripe int, rec *Request)
func (h *Holder) Active() *Snapshot

// Request is the pooled per-request record the ending protocol acts on.
type Request struct {
	Mu        sync.Mutex
	Committed bool
	Ended     EndReason // none, grace (RZ-RT-014), drain (RZ-RT-016)
	Cancel    context.CancelFunc
	RC        *http.ResponseController
	// intrusive list links
}

type Retirer struct{ /* K, grace, ending bound, clock, metrics */ }

func NewRetirer(h *Holder, cfg RetireConfig) *Retirer // K=2, Grace=30s, EndingBound=6s
func (r *Retirer) CanActivate() bool                    // req 54 precondition
func (r *Retirer) Swap(next *Snapshot) (prev *Snapshot) // atomic store + state transitions
func (r *Retirer) Run(ctx context.Context) error        // pin polling (50 ms, proposed), grace timers, closing
func (r *Retirer) EndAll(reason EndReason)              // Drain deadline
func (r *Retirer) Snapshots() []Info                    // /debug/snapshots
```

```go
package chain // internal/gateway/chain

type Policy struct {
	Name        string
	Type        v1alpha1.PolicyType
	Class       filter.Class
	Scope       filter.Scope
	FailureMode v1alpha1.FailureMode
	When        router.BoolProgram // nil when absent
	Phases      filter.PhaseSet
	Filter      filter.Filter
	Security    bool // closed only
}

type Chain struct {
	Client   [filter.NumPhases][]*Policy // execution order from precedence.Chain
	Legs     map[string]*[filter.NumPhases][]*Policy
	AuthN    int  // number of auth-class Policies (SEC rule 1)
	NeedBody Flags // request gate, response gate, tee
}

type Executor struct{ /* metrics, tracer, logger */ }

type Outcome struct {
	Response *filter.Response // non-nil: generated response to send (short-circuit or failureMode)
	EndLeg   bool             // onUpstreamRequest short-circuit
}

func (e *Executor) Run(ctx context.Context, p filter.Phase, pols []*Policy, x *filter.Exchange, st *RunState) Outcome
func (e *Executor) RunLog(ctx context.Context, pols []*Policy, x *filter.Exchange, st *RunState)
type RunState struct{ /* per-request skip bitset, when results, auth-skipped count */ }
```

```go
package handler // internal/gateway/handler

type Forwarder interface { // Upstream layer area
	Forward(ctx context.Context, x *filter.Exchange, route *snapshot.Route, h LegHooks) (*UpstreamResponse, error)
}

type LegHooks interface { // called by the Forwarder per leg attempt
	OnUpstreamRequest(ctx context.Context, leg string, x *filter.Exchange) *filter.Response
	OnUpstreamResponseHeaders(ctx context.Context, leg string, x *filter.Exchange)
	OnUpstreamResponseBody(ctx context.Context, leg string, x *filter.Exchange) *filter.Response
}

type Config struct {
	Listener  string
	Scheme    string // http | https
	Holder    *snapshot.Holder
	Units     *admission.Units
	Pre       *admission.PreRouting
	Budget    *body.Budget
	Exec      *chain.Executor
	Forwarder Forwarder
	Tap       *tap.Hub
	Tel       Telemetry // telemetry area facade
}

func New(cfg Config) http.Handler
```

```go
package listener // internal/gateway/listener

type Set struct{ /* by name: socket, server, spec */ }

func (s *Set) Apply(ctx context.Context, want []snapshot.ListenerSpec, mk func(snapshot.ListenerSpec) http.Handler) (commit func(), rollback func(), err error)
// Apply binds added/changed listeners before the swap; commit closes removed ones after it.
func (s *Set) Shutdown(ctx context.Context) error // Drain: Server.Shutdown on each
func (s *Set) Bound() bool

func ServerFor(spec snapshot.ListenerSpec, h http.Handler, tlsFor TLSSource) *http.Server // req 9-10
type TLSSource interface{ ConfigForClient(listener string, chi *tls.ClientHelloInfo) (*tls.Config, error) }
func SourceAddr(peer netip.AddrPort, trusted []netip.Prefix, h http.Header) netip.AddrPort // req 17
```

```go
package proxyproto // internal/gateway/listener/proxyproto

const MaxHeaderLen = 4096 + 16

type Header struct {
	Command byte           // 0 LOCAL, 1 PROXY
	Src     netip.AddrPort // zero for LOCAL
	Dst     netip.AddrPort
}

func Read(r io.Reader) (Header, error) // exactly one header; errors close the connection
```

```go
package reuseport // internal/gateway/reuseport (linux; other GOOS return ErrUnsupported)

type Socket struct{ /* fd, addr */ }

func Bind(network string, addr netip.AddrPort) (*Socket, error) // socket, SO_REUSEPORT, bind; no listen
func (s *Socket) Listen() (net.Listener, error)                  // listen(backlog = net.core.somaxconn) → net.FileListener
func (s *Socket) SelectIndex(i uint32) error                     // SO_ATTACH_REUSEPORT_CBPF: {BPF_RET|BPF_K, i}
```

```go
package reload // internal/gateway/reload

type Candidate struct {
	Origin  Origin          // File, LKG, Handover (M2: OCI, Control)
	Load    func(ctx context.Context) (*loader.Result, error) // file mode
	Content []byte          // canonical bytes (LKG, handover)
	Want    revision.Digest // verify target when Content is set
}

type Loader struct{ /* one pending slot, retirer, compile deps, lkg store, metrics */ }

func (l *Loader) Submit(c Candidate)           // replaces any pending candidate
func (l *Loader) Run(ctx context.Context) error // single goroutine; req 54-55
func (l *Loader) Pending() (revision.Digest, bool)
```

```go
package lkg // internal/gateway/lkg

const Format = "ruralz.lkg.v1"

type Store struct{ /* *nodedir.Dir */ }

func (s *Store) WriteCandidate(ctx context.Context, d revision.Digest, content []byte) error
func (s *Store) Promote(d revision.Digest) error
func (s *Store) Candidate() (revision.Digest, []byte, error) // verified: RZ-CFG-027 on mismatch
func (s *Store) LastKnownGood() (revision.Digest, []byte, error)
```

```go
package source // internal/gateway/source

type Event uint8 // Replaced (immediate), Settled, Forced (SIGHUP)

type Dir struct {
	Path      string
	Poll      time.Duration // 1s
	Settle    time.Duration // 2s
	MaxSettle time.Duration // 30s
}

func (d *Dir) Run(ctx context.Context, out chan<- Event) error // out has capacity 1; sends coalesce
```

```go
package readiness // internal/gateway/readiness

type Reason string

const (
	NoRevision        Reason = "no_revision"
	SecretsUnresolved Reason = "secrets_unresolved"
	ListenersUnbound  Reason = "listeners_unbound"
	Draining          Reason = "draining"
)

type State struct{ /* atomic bitset + details under mutex */ }

func (s *State) Set(r Reason, on bool, code, detail string)
func (s *State) Ready() bool
func (s *State) AppendJSON(dst []byte, active revision.Digest) []byte
```

```go
package admin // internal/gateway/admin

type Credentials struct {
	OperatorToken []byte      // from RURALZ_ADMIN_TOKEN_FILE
	MetricsToken  []byte      // from RURALZ_ADMIN_METRICS_TOKEN_FILE
	TLS           *tls.Config // from RURALZ_ADMIN_TLS_DIR; nil = cleartext
}

type Deps struct {
	Readiness *readiness.State
	Snapshots interface{ Snapshots() []snapshot.Info }
	Upstreams UpstreamDebugger // Upstream area: WriteJSON(w io.Writer) error
	Dump      func() ([]byte, bool) // canonical.AppendDump over the active snapshot
	Tap       *tap.Hub
	Metrics   http.Handler // telemetry area
	Logger    *slog.Logger
}

func NewServer(c Credentials, d Deps) *Server
func (s *Server) Serve(ctx context.Context, ln net.Listener) error
func (s *Server) CloseForHandover() // closes idle conns, sets Connection: close on in-flight responses
```

```go
package tap // internal/gateway/tap

var ErrFull = errors.New("tap: subscriber limit reached")

type Hub struct{ /* atomic.Pointer to subscriber slice; max 4 */ }

func (h *Hub) Active() bool          // one atomic load
func (h *Hub) Publish(ev *Event)     // non-blocking; drops per subscriber with a counter
func (h *Hub) Subscribe(ctx context.Context, sample float64) (*Sub, error)
func (s *Sub) Next(ctx context.Context) ([]byte, error) // one encoded NDJSON line
```

```go
package drain // internal/gateway/drain

type Timeline struct{ AcceptWindow, Deadline, Flush, ExitBound time.Duration }

func OnSignal() Timeline   // {5s, 20s, 5s, 30s}
func OnHandover() Timeline // {0, 20s, 5s, 25s}

type Coordinator struct{ /* readiness, listener set, retirer, lock, flushers, clock */ }

func (c *Coordinator) Run(ctx context.Context, tl Timeline) error // blocks until exit is allowed
```

```go
package handover // internal/gateway/handover

const Format = "ruralz.handover.v1"

type Usage struct {
	Connections, InflightUnits, BufferedBytes, PluginMemoryBytes, RSSBytes int64
}

type Message struct {
	Type         string            `json:"type"`
	Format       string            `json:"format,omitempty"`
	Version      string            `json:"version,omitempty"`
	PID          int               `json:"pid,omitempty"`
	Markers      map[string]string `json:"markers,omitempty"`
	ActiveDigest string            `json:"activeDigest,omitempty"`
	Digest       string            `json:"digest,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	Usage        *Usage            `json:"usage,omitempty"`
}

type HolderSide struct{ /* socket server, active digest source, steering callbacks */ }
func (h *HolderSide) Serve(ctx context.Context) error

type SuccessorSide struct{ /* dial, timeout 60s */ }
func (s *SuccessorSide) Gate(ctx context.Context, active revision.Digest) (accepted <-chan Usage, err error)
```

```go
package gateway // internal/gateway (existing stub replaced)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int
```

Concurrency model:

- Request path: one goroutine per request or HTTP/2 stream (owned by `net/http`); no global lock across I/O; snapshot pointer, pins, units, pre-routing counter, budget and counters are atomics; per-request state is pooled.
- Fixed background goroutines owned by the `gateway.Run` supervisor (each cancelled and awaited): one `Serve` per client listener and one for admin; source watcher; reload loader (single); compile workers W = max(1, `GOMAXPROCS`/2) created and joined inside one activation; retirer; secret poller (area 1); LKG retry writer; handover socket server (holder) or gate client (successor) plus the 1 s usage reporter; signal handler; `/tap` streaming handlers (at most 4); drain coordinator.
- Ending callbacks run one goroutine per pinned request only at grace end or the Drain deadline, bounded by the in-flight ceiling.
- Channels: source → loader capacity 1 (coalescing); loader pending slot size 1; tap per-subscriber 1 MiB byte-bounded ring.

Exported for other areas: the `internal/filter` SPI; `problem`; `routematch.CheckTemplate` and `MatchKey` (area 1 validation); `nodedir` (CLI `ruralz node drain`); `ulid`; `admission.Units` (composition steps, stream pumps); `body.Budget`, `ReadGate`, `Tee` (Filters, composition, cache); `snapshot.Route` accessors and `handler.Forwarder`/`LegHooks` contracts (Upstream layer); readiness JSON (CLI `ruralz dev run`); `/tap` NDJSON schema (CLI `ruralz dev tap`).

## 4. Dependencies on other areas

| Direction | Area | Contract |
|---|---|---|
| Needs | Area 1 (loader) | `loader.Load(ctx, Source{Dir}, Options{Workers: W, Yield: 100µs, Variables: os.Environ snapshot, no Environment})`, `loader.FromResources` for LKG and handover candidates, `diag.List.FirstErrorCode()`, `hub.Bundle` typed resources, `secret.Resolver` (`Resolve`, `Activate`, `Current`, `Subscribe`, `Run`) for readiness and TLS certificates; `validate.Checkers.Template` is provided by this area (`routematch.CheckTemplate`) |
| Needs | Area 2 (canonical, precedence) | `canonical.Encoder.Encode` (digest and bytes), `canonical.Decoder.Verify(content, want)` (RZ-CFG-027), `canonical.AppendDump` for `/config/dump`, `revision.Digest` (`String`, `Short`), `precedence.Chain` per Route with `Client [NumPhases][]Entry` and `Legs` in execution order (the two areas' chain types MUST be unified before coding, section 9 item 20) |
| Needs | CEL area | Compiled bool programs for `match.when` and `Policy.spec.when` with a runtime cost limit of 1,000,000 units and `ContextEval`; an activation built lazily from the request view without per-request map allocation; `request.query` and `request.pathParams` shapes |
| Needs | Filters area (security, traffic, transform, cache, validation) | `filter.Factory` per Policy type registered in one `filter.Registry`; Filters return class-appropriate codes (e.g. `RZ-AUTH-00x`, `RZ-STS-00x`, `RZ-RL-005`); the `auth.mtls` listener client-certificate flag and the 421 code; credential header names for `/tap` redaction |
| Needs | Upstream layer and composition area | `handler.Forwarder` implementing the attempt loop, retries, breakers and `RZ-UP-*`/`RZ-RT-010`/`RZ-RT-015`, calling `LegHooks`; pool carry-over keyed by protocol, TLS settings and Endpoint across snapshots; `UpstreamDebugger` JSON for `/debug/upstreams`; forwarding-header set from req 18 |
| Needs | State Store area | State client handle carried in the snapshot; one-round-trip-per-Policy enforcement keyed by the executor's call identity; post-commit queue `Flush(ctx)` for the Drain flush step; `RURALZ_STATE_STORE_URL` handling |
| Needs | Telemetry area | `internal/telemetry` logger, metric handles on Ruralz aggregates (stripe-aware), tracer with per-connection ID generator and sampling caps, access-log record API, `/metrics` `http.Handler`, gateway-duration excluded-section word, `ruralz_node_degraded_info` setter, cleartext-hop gauge |
| Needs | `internal/errcode` | Registration of RZ-RT-016 and RZ-RT-017 (this area's registry, DP owns RT) and of the listener-bind RZ-CFG code (configuration-model owns); proposed `errcode.Error{Code; Err}` wrapper from area 2 |
| Needs | CLI area | `ruralz node drain` reads `nodedir.Holder` and signals SIGTERM; `ruralz dev run` sets `RURALZ_DATA_DIR`, `RURALZ_ADMIN_TOKEN_FILE`, rewrites ports and awaits `/readyz`; `ruralz dev tap` consumes `/tap`; `ruralz node dump` saves `/config/dump` |
| Needs | Release area | Shipped systemd unit and handover helper (OQ-zero-downtime-upgrades-and-hot-reload-7); `-ldflags -X` build metadata (exists) |
| Provides | All request-path areas | Filter SPI, executor semantics (ordering, short-circuit, `failureMode`), problem writer, body budget, in-flight units, pinned snapshot, normalized request view and `source.ip` |
| Provides | Area 1 | `routematch.CheckTemplate`, `routematch.ParseHostPattern` (wildcard rule), `routematch.MatchKey` (RZ-CFG-023 identity) |

## 5. Libraries

| Module | Version | Why | Catalog row |
|---|---|---|---|
| Standard library (Go 1.26 floor, go1.27.1 toolchain) | — | `net/http` (`Server.Protocols`, `HTTP2Config`, `ResponseController`), `crypto/tls`, `net/netip`, `regexp`, `sync/atomic`, `context`, `os/signal`, `runtime/pprof`, `net/http/pprof` (handler functions only), `crypto/rand`, `crypto/sha256`, `encoding/json`, `bufio`, `log/slog` via `internal/telemetry`. Go 1.27-only APIs (`Server.MaxHeaderValueCount`, goroutine leak profile) only in `//go:build go1.27` files | TS "HTTP/1.1 and HTTP/2" (standard library) |
| `golang.org/x/sys` | v0.48.0 (2026-08-31; `go 1.26.0`) | `unix.Socket/Bind/Listen`, `SO_REUSEPORT`, `SO_ATTACH_REUSEPORT_CBPF` with `unix.SockFprog`/`unix.SockFilter`, `Flock`, `SO_PEERCRED` (`GetsockoptUcred`) | TS "Zero-Downtime Upgrade socket steering" v0.48.x (the row names steering; flock and peer credentials use the same module, noted in section 9) |
| `cel.dev/cel-go` | v0.32.0 | Only through the CEL area's compiled programs (`match.when`, `Policy.spec.when`); never imported here | TS "Expressions" v0.32.x |
| `go.opentelemetry.io/otel` (+ `sdk`, `trace`) | v1.46.0 | Spans `ruralz.route.match`, `ruralz.filter.<name>` through `internal/telemetry` | TS "Telemetry" v1.46.x |
| `go.opentelemetry.io/otel/exporters/prometheus` | v0.68.0 (requires otel v1.46.0; pulls `github.com/prometheus/client_golang` v1.24.1) | `/metrics` exposition, decided by OQ-tech-stack-and-libraries-16; wired by the telemetry area, mounted here | Decided selection; client_golang row gap in section 9 |
| `go.opentelemetry.io/contrib/bridges/otelslog` | v0.20.1 | Logs via `internal/telemetry` only | TS "Telemetry" v0.20.x |

No watcher library (no catalog row): the file-mode watcher polls with the standard library. No ULID library: own code (decided). No third-party PROXY protocol parser. fasthttp and `x/net/http2` servers stay banned (ADR9).

## 6. Test plan

Coverage target 90% of statements for `routematch`, `router`, `chain`, `snapshot`, `body`, `admission`, `proxyproto`, `problem`, `ulid`, `lkg`, `handover` (TQ "Test pyramid"). Unit tests are hermetic (fake clock, injected entropy, fake Filters, fake Forwarder), shuffled, run with `-race` and again with `CGO_ENABLED=0`; Linux-only tests carry `//go:build linux`. No Docker daemon: integration and end-to-end tests use in-process mocks, `httptest`, and the built `ruralzd` binary; the local `redis-server` 7.0.15 is not needed by this area (State Store tests belong to that area).

Unit and table tests:

1. `setting`: every variable valid/invalid; unset `RURALZ_CONFIG` → 2; `oci://`, `ruralz-control://` → 2 with "Planned (M2)"; bad `RURALZ_LOG_LEVEL` → 2; secret root containing the data dir, TLS dir or a token file → 2; M2 variables warn; positional argument → 2.
2. `ulid`: 26-char alphabet, time round trip, lexical order equals time order for distinct ms, `Parse` rejects `I L O U`, lowercase accepted, overflow first char > `7` rejected, deterministic output with fixed entropy.
3. `nodedir`: modes 0700/0600; second `TryLock` in the same process (separate open file description) → `ErrLocked`; release lets it succeed; `holder.json` golden; atomic write survives a simulated crash (temp file left behind is ignored); `node-id` created once and reused.
4. `problem`: golden bytes for RZ-RT-001..007, 011, 012, 014, 016, 017; header set (`application/problem+json`, `no-store`, exact length); a request path with `"` `<` `\u0000` never appears in the body.
5. `routematch`: host table (`API.Shop.Example:8443` → `api.shop.example`, `[::1]:8080` → `[::1]`, trailing dot); wildcard (`*.shop.example` vs `eu.shop.example` ✓, `a.b.shop.example` ✓, `shop.example` ✗, `xshop.example` ✗; `api.*.example` and `**.x` invalid); path normalization (`/a/./b/../c` → `/a/c`, `/%41` → `/A`, `/%2f` → `/%2F` kept as one segment, `/a//b/` kept, `/..` → `/`, `/%00`, `/a\b`, `/%5C`, `/%zz` → `ErrRejected`); template grammar valid/invalid (`/v1/{id}`, `/v1/{id}/x`, `/{a}/{a}` ✗, `/v1/{id}.json` ✗, `/v1/{}` ✗, missing leading `/` ✗); prefix boundaries (`/v1/orders` vs `/v1/orders`, `/v1/orders/`, `/v1/orders/42` ✓, `/v1/ordersX` ✗; `/` matches all; `/v1/` vs `/v1` ✗); `MatchKey` equal for reordered `hosts`/`methods`/`headers`, different for any other change.
6. `router`: the DP worked example as a golden table (6 Routes, the 6 listed requests plus `DELETE api.shop.example/v1/orders` → `orders-any`, `GET other.example/x` → `fallback`); one pair per precedence rank 1-6; regex ordered only by ranks 5-6; methods, headers and `when == false` fallthrough; `when` runtime error → `RZ-RT-006` even though a lower Route would match; no candidate → `RZ-RT-001`; `Route.spec.listeners` binding (Route absent on the other listener); listener `hostnames` mismatch → `RZ-RT-001`; template params decoded; catch-all `when: "true"` ranks last; `CONNECT` and `OPTIONS *` unmatched; 0 allocations per match (`testing.AllocsPerRun`); build of 10,000 Routes within 2 s on one core (benchmark gate).
7. `chain` with fake Filters: order by class, scope, position; response Phases reversed; `onLog` in request order; short-circuit from each of the four request Phases runs `onResponse` minus `transform.response`, `cache`, `ai.semantic-cache`, then `onLog`; `onUpstreamRequest` short-circuit ends only the leg; the full failure table of req 43, one case per class × {request closed, request open, response closed, response open} with exact status and code (RZ-RT-011, RZ-RT-012, RZ-AUTH-015 default, RZ-AUTH-020, RZ-STS-001 passthrough, RZ-RL-005 passthrough, RZ-PLG passthrough for `custom`); `open` skips later Phases of the same Policy; `when` false/true/error × closed/open; all auth-class Policies skipped by `when` → 401 `RZ-AUTH-001`, one running → no 401; `onLog` failure counted, response unchanged; a panicking Filter becomes cannot-decide; metrics and span names asserted through a telemetry fake; empty Phase performs no call.
8. `body`: reservation in 32 KiB steps; gates never take the 25% stream share; stream reservations use the share then general budget; exhausted → `ErrBudget` (→ 503 `RZ-RT-004`); declared `Content-Length` over the limit → 413 `RZ-RT-003` without reading (reader never called; no `100 Continue`); chunked body over the limit → 413 before commit; streamed body over the limit after commit → leg aborted, stream reset; `SetTotal` lowering keeps outstanding reservations; property test (random concurrent reserve/release) keeps `used ≤ total` and returns to 0.
9. `admission`: exactly 20,000 concurrent units under `-race` with 40,000 goroutines, the rest rejected with 503 `RZ-RT-005` and none queued; `SetExternal` lowers capacity; pre-routing limiter admits 2,000, the 2,001st aborts with `http.ErrAbortHandler` and writes nothing; connection limiter blocks `Accept` at the ceiling and resumes on `Close`.
10. `proxyproto`: v4 and v6 `PROXY`, `LOCAL` with `AF_UNSPEC`, TLVs skipped, bad signature, v1 text header, version 1 nibble, UDP family, UNIX family, truncated at every byte offset, length above the cap → error; parsing reads exactly the header length (next byte is the ClientHello or request line).
11. `listener.SourceAddr`: untrusted peer ignores XFF; trusted peer picks the rightmost untrusted XFF address; `Forwarded` preferred over XFF; all trusted → leftmost; 17th address not parsed; unparsable entry stops; IPv4-mapped unmapped; PROXY source used as peer; forwarding headers overwritten (untrusted) or appended (trusted).
12. `snapshot`: pin/unpin property test with concurrent swaps under `-race` where each freed snapshot sets a poisoned flag that any pinned reader asserts is unset; K = 2 transitions with a fake clock: third retirement makes the oldest closing, grace 30 s → ending; pending waits while a closing snapshot exists and `retired == K`; ending before commit → 503 `RZ-RT-014`; after commit → deadlines in the past (connection closed or stream reset observed by an `httptest` client); `ruralz_snapshot_retirement_ended_total` increments; pins held past 6 s after ending → `snapshot_ending_overdue`; `ruralz_config_retired_snapshots` gauge; resources closed only at zero pins and only when unshared.
13. `reload`: latest-pending coalescing (submit A, B, C while blocked → only C activates); rejection leaves the active pointer and counts `{result="rejected",code}` with the first diagnostic's code; same digest no-op; file mode writes candidate then promotes; LKG write failure retried with backoff and `lkg_write_failed` raised then cleared; stage histogram labels and `size_class` boundaries (1,000; 10,000).
14. `lkg`: golden pointer file; content digest mismatch → `RZ-CFG-027`; unknown `format` → treated as absent; unreferenced files removed; crash between content and pointer writes leaves the previous pointer valid.
15. `source`: symlink swap → immediate `Replaced`; in-place edits → one `Settled` after 2 s of stability; continuous edits → load at 30 s; one-file rename → immediate; missing directory → error logged, no event; SIGHUP → `Forced`.
16. `admin`: auth matrix over {no credentials, metrics token only, operator token only, both, TLS client certificate} × {each path} × {loopback, non-loopback cleartext, TLS} with exact 200/401 outcomes; constant-time compare used (inspection via a counting comparer in tests); `/readyz` reasons golden per reason; `/debug/snapshots` golden; `/config/dump` bytes equal `canonical.AppendDump`; unknown path 404, `POST` 405 as problem documents; `/debug/pprof/mutex?seconds=1` restores rates; `CloseForHandover` sets `Connection: close`.
17. `tap`: no subscriber → `Active()` false and 0 allocations in `Publish` path; redaction of each listed header and of an `auth.api-key` configured header name; no query string; 5th subscriber → 503; slow subscriber drops with `ruralz_tap_events_dropped_total`.
18. `drain` with a fake clock: `/readyz` 503 at 0 s; `Accept` still served at 4.9 s; `Shutdown` at 5 s; in-flight request still running at 25 s ends with 503 `RZ-RT-016` (before commit) or a reset (after); lock released at 0 s and no `lkg/` write afterwards; exit by 30 s; second SIGTERM jumps to the deadline.
19. `handover` messages: golden encodings of every message; unknown marker → `refused unknown_marker`; older candidate → `refused older_candidate`; newer candidate → `reload`; UID mismatch refused; 60 s timeout; holder exit during the gate → successor takes the lock.

Fuzz targets (TQ "Fuzzing"; seeds in `pr-fast`, long runs nightly): `FuzzRouterMatchesReference` (random Route sets and requests; the compiled Router equals a brute-force reference matcher that evaluates every Route and sorts by the six ranks — the TQ "Router match and header handling" target); `FuzzNormalizePath` (idempotent, never contains a `.` or `..` segment, never decodes `%2F`, rejects NUL and backslash forms); `FuzzNormalizeHost`; `FuzzProxyProtoRead` (no panic, bounded read); `FuzzSourceAddr` (XFF/Forwarded parsing); `FuzzHandoverMessage`; `FuzzLKGPointer`; `FuzzProblemAppend` (valid JSON always).

Golden tests: problem documents, `/readyz`, `/debug/snapshots`, `/tap` event, `holder.json`, LKG pointer, handover messages, router worked-example table; files under `testdata/`, `eol=lf`.

Integration tests (`//go:build integration`, stage 8):

20. Protocol conformance (HTTP/1.1, HTTP/2; TQ "Conformance suites"): real servers on ephemeral ports with an `httptest` mock Upstream through a fake Forwarder: HTTP/1.1 keep-alive; h2c prior knowledge on `http`; `Upgrade: h2c` answered as HTTP/1.1; ALPN `h2` and `http/1.1` on `https`; TLS 1.3 default and 1.2 with allowed suites only; SNI selection among two certificates; stream 251 on one HTTP/2 connection refused (`MaxConcurrentStreams`); header block over 256 KiB → plain 431 (no problem body); over the snapshot limit → 431 problem `RZ-RT-002`; 257 header fields → 431 `RZ-RT-002`; slow header write → connection closed at 10 s; idle close at 120 s (fake clock where possible, else shortened test-only constant injected through the package API, never a global); both `Transfer-Encoding` and `Content-Length` → 400 `RZ-RT-017`; `%00` path → 400 `RZ-RT-017`; hop-by-hop headers absent at the mock; forwarding headers per req 18; PROXY v2 listener with and without header (TLS and cleartext); connection ceiling (set to 64 in the test) blocks the 65th.
21. Hot Reload under load (R1-lite): a Go load generator at a fixed rate across 100 reloads, zero failed requests; ten activations per second with one long request pinned → `RZ-RT-014` exactly at grace end; peak retained snapshots ≤ K + 2.
22. File mode lifecycle with the built binary: boot from a valid directory; corrupt the directory → rejected, still serving; restart with a corrupt directory → Last-Known-Good boot, `lkg_boot` raised, `/readyz` 200; fix the directory → activation clears `lkg_boot`; unresolvable `file` secret → `/readyz` 503 `secrets_unresolved` with `RZ-CFG-026`, then resolves after the file appears; empty data dir and invalid directory → not ready.
23. Drain (CE-2 for HTTP/1.1 and HTTP/2, V-4 M1 part): SIGTERM while 200 keep-alive and HTTP/2 connections run; zero failed new requests during the accept window; GOAWAY observed; exit ≤ 30 s; `ruralz node drain`-style PID lookup via `nodedir.ReadHolder`.
24. Handover (Linux; V-3-lite, V-6): holder and successor binaries on the same data dir under load: zero failed requests and no refused connections; a keep-alive health checker on 9901 sees 200 throughout; CBPF steering verified by connection counts per process; run with `net.ipv4.tcp_migrate_req` 1 and 0 when the sysctl is writable (skipped otherwise); refused handover (fake successor with an unknown marker) leaves the holder serving; successor timeout at 60 s (shortened through injected config); holder SIGKILLed during the gate → successor takes the lock and serves.
25. Admin TLS with `RURALZ_ADMIN_TLS_DIR` and a client certificate; tokens refused over non-loopback cleartext.
26. Secret canary (TQ "End-to-end tests"): a canary secret through `secretRef` never appears in logs, `/config/dump`, `/tap`, or problem bodies.

Benchmarks and gates (stage 9): Router match (0 allocs), pin + match (p99 ≤ 12 µs reported), `AllocsPerRun` S1 path ≤ 30 (PB-8), `RZ-RT-005` rejection ≤ 2 allocs, compile of 5,000 Routes ≤ 400 ms with W workers, `/tap` publish without subscriber 0 allocs; C1 (20,000 idle connections within the connection memory rows) and O1 (in-flight plateau at 20,000, rejections never queue) as nightly jobs on runners that allow the file-descriptor count.

Every error code path this area emits has at least one test: RZ-RT-001 (items 6, 20), RZ-RT-002 (20), RZ-RT-003 (8, 20), RZ-RT-004 (8), RZ-RT-005 (9), RZ-RT-006 (6), RZ-RT-007 (fake Forwarder never called before a short Route timeout expires, in `handler` tests), RZ-RT-011 and RZ-RT-012 (7), RZ-RT-014 (12, 21), RZ-RT-016 (18, 23), RZ-RT-017 (5, 20), RZ-AUTH-001 via `when` skip (7), RZ-CFG-026 at boot (22), RZ-CFG-027 at LKG boot (14), the listener-bind RZ-CFG code (reload test binding an occupied port), admin 401 (16), `/tap` 503 (17).

## 7. Open questions blocking M1 in this area

From the Open questions tables whose Blocking column names M1, and the RM exit criterion 7 list, those touching this area:

| ID | Question | Adopt | What the code does |
|---|---|---|---|
| OQ-data-plane-2 | How is the wildcard host `*.` registered for `Route.spec.match.hosts`? | (a) one leading `*.` label (already answered in CM "Route") | `routematch.ParseHostPattern` enforces it (RZ-CFG-005 via area 1); Router wildcard table per req 27 |
| OQ-data-plane-9 | Which area covers Node-generated failures outside "before any Upstream" (RZ-RT-011 to -015)? | (a) amend pack 8.6 `RT` to "request and response handling on a Node outside Upstream legs" (current) | No code change: existing `internal/errcode` RT rows stand; the `AreaRT` doc comment and FP 8.6 wording are updated by the pack amendment; new RZ-RT-016/017 fall under the amended meaning |
| OQ-cli-and-api-surface-10 | Where does the lock holder record its PID and start time? | (a) a file under `${RURALZ_DATA_DIR}` (proposed) | `nodedir.WriteHolder` writes `holder.json` (req 5); `ruralz node drain` reads it via `nodedir.ReadHolder` |
| OQ-zero-downtime-upgrades-and-hot-reload-7 | Which systemd directives keep the new process as main process across a handover? | (a) research, then a shipped unit and helper | `internal/gateway/sdnotify` (req 8): `READY=1`, `STOPPING=1`, `MAINPID=` from the successor; unit and helper shipped by the release area after research; until closed, T5 handover is documented as `systemctl stop`/`start` (Drain and restart) |
| OQ-security-and-identity-7 | Which settings hold admin credentials? | (a) `RURALZ_ADMIN_TOKEN_FILE`, `RURALZ_ADMIN_METRICS_TOKEN_FILE`, `RURALZ_ADMIN_TLS_DIR`, amending pack section 2 (proposed) | `setting` parses them; `admin.Credentials` enforce req 70 |
| OQ-security-and-identity-21 | Reject encoded NUL and backslashes? | (a) yes (proposed; owner data-plane) | `routematch.NormalizePath` rejects `%00`, `\`, `%5C` with 400 `RZ-RT-017` (proposed code) |
| OQ-security-and-identity-6 (RM exit 7) | How are trusted proxies declared? | (c) both, answered in CM "Gateway" (`trustedProxies`, `listeners[].proxyProtocol`) | `proxyproto` parser and `listener.SourceAddr` per reqs 16-18 |
| OQ-security-and-identity-18 (RM exit 7, listed with `authz.ip`) | Registration of Security-authored `config` schemas | (a) register as authored (answered in CM "Registered from feature documents") | No executor change; this area supplies the `source.ip` that `authz.ip` reads; the Filter is the security area's |
| OQ-security-and-identity-22 | How are secrets and Node connections restricted? | (a) `RURALZ_SECRET_ROOT`, `RURALZ_SECRET_` prefix, `RURALZ_FETCH_ALLOW` … (proposed) | `setting` reads `RURALZ_SECRET_ROOT` and `RURALZ_FETCH_ALLOW`; start refusal of req 3; resolution rules live in area 1 |
| OQ-performance-budgets-and-benchmarking-6 | Does the config loader adopt W = max(1, `GOMAXPROCS`/2) workers yielding every 100 µs? | (a) yes, fixed (proposed; owner data-plane) | `reload` passes `Workers: W, Yield: 100µs` to area 1/2 and uses W for Router and chain compile |
| OQ-observability-16 | Which OpenTelemetry Go SDK interface exports Ruralz aggregates and ends series? | (a) external producer (the Prometheus exporter and OTLP reader take a `metric.Producer`) | Owned by telemetry; this area only mounts the `/metrics` handler and records into aggregates |
| OQ-traffic-management-and-resilience-6 (canonical form) | Deadline defaults and fields | (c) per-protocol Route timeouts (recommended) with the proposed defaults (Route `timeout` 15 s) | Router/handler read the materialized Route `timeout`; deadlines per req 12; no default hard-coded here |
| OQ-traffic-management-and-resilience-2 (header contract) | How do admitted responses get RateLimit fields? | (a) `onResponse` on both types | No executor change (Filters add headers in `onResponse`); option (c) would have put it here |
| OQ-repository-layout-and-conventions-6 (RM exit 7) | Where do linux/arm64 tests run? | (a) native arm64 runners | The conformance and handover integration suites run on both architectures; `reuseport` CBPF code has no arch-specific parts |
| OQ-zero-downtime-upgrades-and-hot-reload-6 (ADR15 lists it as blocking; ZDT "No (answered)") | Overlap memory | (a) hosts sized for two processes until V-3 | Req 67 implemented; documentation states the sizing |

Non-blocking decisions this spec relies on (all "current" options): OQ-data-plane-1 (a) fixed bounded-resource defaults; OQ-data-plane-4 (b) local SIGTERM; OQ-data-plane-6 (a) fixed in-flight ceiling; OQ-data-plane-8 (a) 401 for upstream-auth; OQ-data-plane-10 (a) no stream TTFB/idle limit; OQ-data-plane-12 (a) per-stream closes; OQ-data-plane-13 (a) grace never cut short; OQ-zero-downtime-upgrades-and-hot-reload-1 (a), -2 (b), -3 (a), -4 (a), -12 (a); OQ-system-overview-6 decided by SEC (all interfaces); OQ-observability-6 (a) log level by restart; OQ-observability-9 (a) no trace ID header; OQ-security-and-identity-17 (b) fixed client timeouts.

## 8. Deferred (M2+; do not build) and extension points

| Deferred item | Milestone | Extension point left in M1 |
|---|---|---|
| Control mode: Control Stream client, boot wait 5 s, ACK/NACK after swap, level-triggered LKG promotion at the promoted digest, `detached` state, enrollment identity files, `draining` Heartbeat, `RZ-CP-002` retry | M2 | `reload.Candidate.Origin`, an `ack func(revision.Digest, error)` hook after step (4), a `PromotionPolicy` interface (file: on activation), `readiness` never gains a Control Stream reason, `nodedir` reserves `identity/` |
| OCI pull mode (`RURALZ_CONFIG=oci://`), Sigstore verification (RZ-CFG-033), trust policy, `cache/oci/sha256/` | M2 | `source` interface beside `source.Dir`; `reload` verify stage takes a `Verifier`; `setting.Mode` enum |
| Plugin host (wazero pools, `plugin_compile` stage, `RZ-PLG` codes, Plugin memory in handover usage) | M2 | `filter.Factory` for `plugin`; `chain` already routes `filterClass`; `Usage.PluginMemoryBytes` always 0 |
| HTTP/3 (quic-go, UDP 8443, `Alt-Svc`, QUIC slots of 16 connections, OQ-data-plane-14/15/16), FIPS refusal of `http3: true` | M3; FIPS M5 | Listener identity includes `http3`; `reuseport` group list accepts UDP; `ConnLimiter.Acquire(n)` takes a weight |
| gRPC, gRPC-Web, Connect, WebSocket, SSE flushing, GraphQL match (bounded pre-match body read), stream share consumers, `onChunk`, streams closed on retirement and Drain (1001, `UNAVAILABLE`, SSE retry), OQ-zero-downtime-upgrades-and-hot-reload-10 jitter | M3 | Dispatch table keyed by Upstream protocol and match type; `filter.OnChunk` Phase; `body.Budget.ReserveStream`; retirer `Closing` hook calling a stream registry that is empty in M1; `protocol` label enum |
| Topic ingress, embedded MQTT | M4 | Router takes listener kinds beyond HTTP |
| Remote drain (OQ-cli-and-api-surface-4), Kubernetes preStop and T4 Drain and restart | M2 | Drain triggered only by signals and handover |
| Persisted Node count under `${RURALZ_DATA_DIR}` (OQ-scalability-and-distributed-state-11, OQ-traffic-management-and-resilience-19 (c)) | M2 | `nodedir` layout table is versioned; add a file only after the pack 8.11 amendment |
| Adaptive concurrency (OQ-data-plane-6 (c)), configurable ceilings (OQ-data-plane-1 (b)/(c)), live log level (OQ-observability-6 (b)) | Not planned now | Ceilings are constructor parameters, not constants read at call sites |

## 9. Risks and ambiguities

1. **Grace versus "activation never waits".** SO "Compile before swap" says activation never waits and uses HTTP/2 GOAWAY; DP rule 6 (OQ-data-plane-12 (a), -13 (a)) waits and uses per-stream closes. Resolution: follow DP (req 54); SO is amended by the owners.
2. **Missing defaults for `limits.maxRequestBodyBytes` and `maxRequestHeaderBytes`.** The schema has none; CM's example uses 10Mi and 64Ki and CM's 30 MiB per-request figure implies 10 MiB. Recommend `+ruralz:default=10Mi` and `+ruralz:default=64Ki` markers before the M1 golden corpus is frozen (defaults are inside the digest). Until then the Node treats absent as 10 MiB and 64 KiB.
3. **Header cap versus schema maximum.** OQ-data-plane-1 (a) mentions a 256 KiB schema maximum, but DP says a larger Revision value is capped as degraded (`header_limit_capped`). A schema maximum makes the degraded state unreachable. Recommend: no schema maximum in M1; cap at run time (req 21).
4. **Header-count code.** SEC caps 256 fields but names no code; reuse 431 `RZ-RT-002` (closest meaning). Needs a note in the DP registry.
5. **Admin 401 code.** CLI requires problem documents with `code`, but no code covers admin authentication. Recommend reusing `RZ-AUTH-001` (credential missing) and `RZ-AUTH-002` (credential invalid) after Security confirms; else register two codes.
6. **New codes.** RZ-RT-016 (Drain deadline, OQ-zero-downtime-upgrades-and-hot-reload-2 (b)), RZ-RT-017 (hardening rejection: encoded NUL/backslash, invalid escape, conflicting framing) and a `/tap` capacity refusal (recommend reusing 503 with a new RZ-RT-018 or answering 429 without body; needs a decision) must be added to DP "RZ-RT registry" and `internal/errcode` together, or repocheck fails on unregistered literals. The listener-bind rejection code (OQ-zero-downtime-upgrades-and-hot-reload-4 (a), a transient RZ-CFG code) belongs to the configuration model; area 2 already proposes RZ-CFG-038, so this one is the next free number.
7. **Listener `hostnames` semantics** are undefined beyond "limits the listener to these host names". Recommended: a request host matching none → 404 `RZ-RT-001`; TLS certificate choice unaffected.
8. **PROXY v2 plus `trustedProxies` composition and XFF fallbacks** (all-trusted, unparsable entry, `Forwarded` versus `X-Forwarded-For` precedence) are unspecified; req 17 picks conservative rules that collapse to a proxy address rather than a client-supplied one. SEC should confirm.
9. **Router details the docs leave open**: regex anchoring (full match chosen), header `exact` against the joined value, `present: false` meaning absence, "constraint count" as count of the four fields (not entries), template grammar without partial or catch-all segments, `HEAD` not implying `GET`, repeated slashes kept, host trailing dot stripped. Each is a behavior visible in the golden corpus; decide before M1 freeze.
10. **Deadline wording.** "10 s deadlines from admission, then the Route timeout" is read as: 10 s until the Route is fixed, then the Route `timeout`, plus a proposed 5 s write slack so 504 `RZ-RT-007`/503 can be written. A long Route `timeout` means a slow client can hold a unit for that long (bounded by the in-flight ceiling).
11. **Admin port before a Revision.** `admin.port` lives in the Bundle, but `/healthz` and `/readyz` must answer before any Revision. Recommended: bind 9901 until the first Revision, then move per req 14. A Bundle choosing another port briefly shows two admin ports during the move.
12. **`RURALZ_DATA_DIR` default** is undefined (only the CLI defaults to the variable). Proposed `/var/lib/ruralz`; alternatively make it required.
13. **File watcher.** No watcher library has a catalog row and no settle interval is specified; polling (1 s, 2 s settle, 30 s cap) is proposed. inotify through `x/sys/unix` would widen that module's catalog purpose.
14. **`x/sys/unix` scope.** The catalog row names steering only; flock and `SO_PEERCRED` use the same already-linked module. Recommend widening the row's text in the PR that first imports it.
15. **`/metrics` serving needs `prometheus/client_golang`.** The decided OTel Prometheus exporter is a `prometheus.Collector`; serving it needs `promhttp` or `expfmt` from `github.com/prometheus/client_golang`/`common` (Apache-2.0), which have no catalog row and would fail the strict depguard list. The telemetry area must add the row (or encode OpenMetrics itself) before `/metrics` lands.
16. **`/tap` sampling and event schema** are undefined; req 75 proposes them. `ruralz dev tap --route` filters client-side, so the server-side `sample` parameter is optional.
17. **`/readyz` and `/debug/snapshots` JSON shapes** are undefined; proposed shapes become a versioned surface once the CLI parses them.
18. **`RZ-RT-005` and access logs.** OBS logs every request, but PBB budgets a rejection at 2 allocations while an access-log record costs 8. Proposed: rejections are counted, not access-logged.
19. **Allocation budget and ending callbacks.** `context.AfterFunc` allocates per registration; the pooled intrusive list avoids it. The PB-8 gate decides.
20. **Area 1 and area 2 disagree on the effective-chain type** (`hub.EffectiveChain` versus `precedence.Chain`); this spec consumes `precedence.Chain`. Unify before implementation.
21. **Scope-order corner.** FP 8.12 orders by scope (Gateway, Route, Upstream) within a Phase, but area 2 forbids Gateway/Route Policies in upstream-leg Phases, so leg chains hold only Upstream Policies. If M2 Plugins at Route scope ever run in `onUpstreamRequest`, the executor must merge client and leg entries by (class, scope, position).
22. **CBPF index bookkeeping.** Socket indices in a reuseport group shift when a member closes; a stale index falls back to hashing. The successor re-attaches its own program after becoming sole member (req 65). Without `tcp_migrate_req`, handshakes needing a second SYN-ACK retransmission can be reset (ADR15, hypothesis).
23. **Handover with an older candidate.** A failed candidate write on the holder makes a successor boot an older candidate, which the holder refuses (ZDT step 4); operators see a refused handover until the write succeeds (`lkg_write_failed`).
24. **Upgrade and CONNECT handling in M1** are unspecified (OQ-multi-protocol-16 is M3). Stripping `Upgrade` and never tunneling is safe; a clearer rejection code can come with M3.
25. **`net/http/pprof` import** runs its package `init` that registers on `http.DefaultServeMux`; harmless because Ruralz never serves the default mux, but the repository's "no init()" rule applies to Ruralz code only. State this in the package doc.
26. **Floor versus release toolchain.** HTTP/2 lives in different code on Go 1.26 and 1.27 (ADR9); the conformance suite MUST run in both floor and release jobs, and Go 1.27-only APIs stay in build-tagged files.
27. **Docker Compose end-to-end (RM M1 Quality) is not runnable here** (no Docker daemon); the binary-level integration tests in section 6 cover file mode, Drain and handover, and the Compose suite is added where a daemon exists.
28. **Upstream-auth status** stays 401 `RZ-AUTH-020` (pack 8.10) although SEC proposes 503 (OQ-security-and-identity-9, OQ-data-plane-8, both marked blocking without a milestone); a later amendment changes one row of the req 43 table.
29. **`ruralz_listener_connections_total{result="refused"}`** has no stated trigger; this spec uses it for PROXY v2 failures (and connections closed at the admin ceiling). OBS should confirm.
