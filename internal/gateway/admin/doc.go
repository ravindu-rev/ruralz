// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package admin is the Ruralz Gateway admin server on
// Gateway.spec.admin.port, default 9901 (docs/architecture/03-data-plane.md
// "Admin endpoints"; docs/reference/01-cli-and-api-surface.md "Ruralz
// Gateway admin API" and "Admin API conventions").
//
// # Endpoints
//
// Every endpoint answers GET only: /healthz ({"status":"ok"} while the
// process responds), /readyz (internal/gateway/readiness), /metrics,
// /debug/pprof/ (Go runtime profiles), /debug/snapshots, /debug/upstreams,
// /config/dump and /tap (internal/gateway/tap). Paths compare exactly and
// are never rewritten. Any other path is a 404 problem document with
// RZ-RT-020, and another method on a known path a 405 with RZ-RT-021 and
// Allow: GET. No endpoint changes configuration or Node state.
//
// Every admin problem document, adminauth's 401s and the tap Hub's
// included, carries as requestId the trace ID of the request's valid
// traceparent, else a new trace ID drawn as for a data-plane request
// without one (RequestID).
//
// The wiring injects what this package would have to import upward
// (docs/engineering/02-repository-layout-and-conventions.md "Package
// layers"): the /metrics handler (internal/telemetry), the /config/dump
// and /debug/upstreams handlers, and the /debug/snapshots view. An
// endpoint whose handler is not injected is not served (404).
//
// # Authentication
//
// internal/adminauth authenticates every request before routing
// (docs/architecture/08-security-and-identity.md "Admin ports"): /healthz
// and /readyz are open; /metrics takes the metrics token, the operator
// token or an admitted client certificate; every other path, unknown paths
// included, takes the operator token or a client certificate and is off
// without an operator token. Refusals are 401 problem documents with
// RZ-AUTH-001 or RZ-AUTH-002, and every /tap and /config/dump use is
// logged, so New refuses an Authenticator without its audit logger.
//
// # Profiles
//
// /debug/pprof/ serves the net/http/pprof handlers: the index, cmdline,
// profile, symbol, trace and every runtime/pprof profile the running
// toolchain registers, the goroutine leak profile included where it
// exists. Mutex and block profiling are off except while a ?seconds=
// request for that profile runs, and then record every event (rate 1).
//
// # Server
//
// The server is its own net/http server with the Node's http.Server
// bounds of "Bounded resources" (ReadHeaderTimeout 10 s, IdleTimeout
// 120 s, a 256 KiB header block, the HTTP/2 stream and buffer bounds, no
// ReadTimeout or WriteTimeout, so /tap and profiles can stream) and at
// most 256 connections across its listeners (admission.AdminConnections).
// It serves HTTP/1.1, and HTTP/2 over TLS when RURALZ_ADMIN_TLS_DIR is
// set; without TLS the admin hop is a reported cleartext hop
// (ruralz_security_cleartext_hops{hop="admin"}). The wiring binds the
// socket (Last-Known-Good's admin port before the first activation, else
// DefaultPort) and passes the listener to Serve; a Hot Reload that moves
// the port serves the new listener and closes the old one. Shutdown ends
// every /tap stream and profile window at once, then drains the rest
// within its context. Imported for its handlers, net/http/pprof also
// registers them on http.DefaultServeMux, which no ruralzd server may
// serve: every http.Server sets its Handler.
package admin
