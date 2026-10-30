// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package listener runs the client listeners of a Node
// (docs/architecture/03-data-plane.md "Listeners and protocols" and
// "Bounded resources"): a Set binds one SO_REUSEPORT socket per Gateway
// listener (internal/gateway/reuseport) and feeds it to [Shards] net/http
// Server shards that call one http.Handler for every request.
//
// # Servers
//
// Each client listener serves through K = [Shards] Server shards that
// share the handler ("Sources, Last-Known-Good and Drain"): the socket's
// dispatching listener, one accept loop, hands connection n to shard n
// modulo K through the shard's channel-fed listener, so the shards hold
// equal shares of the connections. Server.Shutdown sends all of a
// Server's GOAWAYs at once and ignores hijacked connections, so the Drain
// shuts the shards down one at a time and clients reconnect in K waves,
// without a per-connection GOAWAY API or the x/net/http2 Server. A shard
// that stopped taking connections passes its turn to the next shard
// still serving.
//
// Every Server is configured once, at start, with the Bounded resources
// values: ReadHeaderTimeout (which net/http also applies to the TLS
// handshake), IdleTimeout, MaxHeaderBytes (a larger header block gets
// net/http's plain 431 before any handler) and, through HTTP2Config only
// (docs/adr/0009-http-stack-net-http-quic-go.md), the HTTP/2 stream,
// receive-buffer and frame limits. ReadTimeout and WriteTimeout stay
// unset: the handler sets body and write deadlines. An https listener
// serves HTTP/1.1 and HTTP/2 negotiated by ALPN h2; an http listener
// serves HTTP/1.1 and h2c by prior knowledge only (no Upgrade: h2c).
// Server.ErrorLog goes to the telemetry error-log bridge. HTTP/3 needs
// quic-go, which is not an admitted module yet, so no listener serves
// UDP.
//
// # Connections
//
// One goroutine per socket, the dispatching listener, accepts connections
// and hands each to a shard of the listener's current Servers; an accept
// error other than a closed socket is retried after 5 ms, doubling up to
// 1 s. A connection passes, in
// order: the Node-wide client connection ceiling (admission.ConnLimiter:
// a slot per accepted connection; at the ceiling the listener holds the
// connection it accepted until a slot frees), the PROXY protocol v2 reader
// when the listener sets proxyProtocol (internal/clientaddr), the
// listener's [Conn] record and, on https, TLS. The handler finds the record with [ConnFrom]: the
// listener name, the connection's stripe, the client-address peer, the
// per-connection cache of auth.mtls and whether the handshake requested a
// client certificate.
//
// TLS comes from internal/tlsconf: each handshake reads the listener's
// current state, which picks the certificate by SNI and sets the minimum
// version and the client-certificate request mode of auth.mtls, so secret
// rotations and Hot Reloads reach new handshakes only. net/http runs each
// handshake; a goroutine per TLS connection waits on the same handshake to
// measure it, so ruralz_listener_tls_handshake_duration_seconds runs from
// accept to the handshake's end.
//
// ruralz_listener_open_connections counts a connection under http1 until
// it is known to speak HTTP/2 (ALPN h2, or the h2c preface).
// ruralz_listener_connections_total counts each connection once: accepted
// with its protocol once known (a cleartext connection that closes before
// its first bytes decide counts as http1), tls_failure for a failed
// handshake, refused for a missing, malformed or late PROXY v2 header.
// Failures count under http1, the default protocol, and a connection closed
// locally first counts no failure.
//
// # Hot Reload
//
// A Hot Reload never changes a running Server. [Set.Prepare], before the
// snapshot swap, binds a new socket for every added listener and every
// listener whose identity (snapshot.ListenerKey: name, port, protocol,
// proxyProtocol) changed, without listening on Linux, so a port error
// rejects the Revision with RZ-CFG-039 while nothing queues on the new
// socket; it also builds the TLS state of every https listener whose TLS
// settings changed. [Plan.Commit], right after the swap, listens and
// serves on the new sockets, steers each port's reuseport group to its
// new socket, publishes the new TLS states and stops the sockets no
// listener uses any more. A socket replaced on the same port keeps
// accepting for the Linger after its group was steered away, so
// handshakes begun before the steering finish, then closes; a removed
// listener stops accepting at once. A changed client-certificate request
// mode starts a new group of Server shards on the same socket and shuts
// the previous group down: GOAWAY on HTTP/2, idle connections closed,
// Connection: close on busy HTTP/1.1 ones. A stopped socket's shards shut
// down the same way, so their connections finish. Commit also reports the
// http listeners as cleartext client hops. [Plan.Abort] releases what
// Prepare bound when the activation fails before the swap.
//
// # Drain and Zero-Downtime Upgrade
//
// In the process that holds the data-directory lock (Options.Steer, or
// [Set.Pin] once a handover successor takes the lock), each port's
// reuseport group is pinned to the socket serving it, after every bind
// (ADR-0015 "Pinning"). [Set.SteerToSuccessor] steers every group to the
// socket a handover successor added (ADR-0015 "Steering").
//
// The Drain (internal/gateway/drain) drives the rest: [Set.Bound] tells
// whether any socket accepts; [Set.StopAccepting] closes every socket,
// and so every dispatching listener, while the shards keep serving (also
// a handover's linger end); [Set.ShutdownShard] starts the one
// Server.Shutdown of shard i of every listener, without waiting, at its
// jittered time; at the Drain deadline [Set.Shutdown] shuts down every
// shard left and waits for the connections within its context, then
// closes the rest; [Set.Close] ends everything at once and waits for
// every goroutine the Set started.
package listener
