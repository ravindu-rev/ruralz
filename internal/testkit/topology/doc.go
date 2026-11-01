// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package topology is the process harness of the end-to-end, chaos, Node
// integration, conformance and in-process benchmark suites
// (docs/engineering/03-testing-and-quality-strategy.md "Process harness"):
// it runs Nodes, ruralzd processes in file mode on one loopback host, and
// their dependencies, for one test.
//
// # Nodes
//
// Each Node serves its own private test render of a rendered Bundle, as
// ruralz test run --bundle does (docs/reference/01-cli-and-api-surface.md
// "Local ruralzd launched by the CLI"): every listener port and admin.port
// is rewritten to a free loopback port, kept across re-renders while an
// added listener gets a new one, which on Linux the topology holds until
// its teardown with sockets bound to it that never listen, so no other
// socket takes it before the Node binds it or while the Node restarts,
// and every secretRef, a SecretValue's or a Consumer API key's, becomes a
// file reference under the Node's own RURALZ_SECRET_ROOT that keeps its
// key, but those NodeOptions.AuthoredSecrets keeps as authored for a case
// that supplies their values itself, such as the env provider and secret
// root cases of the security suite.
// That is how several Nodes share one host. A Node reports its test digest,
// the Revision digest of the rewritten file it loads, beside the Revision
// digest of the unmodified render; Config.Digest computes both, since the
// harness runs no configuration pipeline of its own. Each Node owns a
// private directory of mode 0700 holding the test render RURALZ_CONFIG
// names, RURALZ_DATA_DIR, RURALZ_SECRET_ROOT, the admin operator and
// metrics token files, the admin TLS directory, the optional State Store
// MAC key file and its process logs. A Node process inherits nothing
// implicitly: its environment is those settings, RURALZ_BIND_LOOPBACK=true,
// RURALZ_FETCH_ALLOW=127.0.0.0/8,::1 for the loopback mocks, SSL_CERT_FILE
// naming a bundle of the harness's CAs, GOMAXPROCS, then Config.Env and
// NodeOptions.Env.
//
// # Runtime
//
// With more than one Node, each runs with GOMAXPROCS max(1, CPUs / Nodes).
// With Config.CPUs, as the target chaos profile's Nodes share RH-1's
// gateway cpuset, each Node process, a handover successor included,
// starts in a systemd-run scope of the user manager with AllowedCPUs set
// to that cpuset, which executes the Node in place, so the process the
// harness signals, samples and passes its clock socket to is the Node's;
// CPUs then counts the cpuset's.
// The harness waits up to ReadyTimeout (30 s) of real time for /readyz, as
// ruralz dev run --ready-timeout does. Stop sends SIGTERM, advances a
// controllable-clock Node through its Drain in DrainStep steps, and sends
// SIGKILL to a Node still running KillAfter (40 s) of real time later, as
// systemd's TimeoutStopSec does; Config overrides each. Restart, Kill,
// Signal, Reload (an atomic replacement of the test render) and Handover
// (a successor process on the same directory, ports and clock) serve the
// Hot Reload, Last-Known-Good, Drain and Zero-Downtime Upgrade scenarios.
//
// # Clock
//
// With Config.TestClock, the controllable-clock build of
// internal/testkit/binaries, the harness passes each Node process its own
// clock command socket as file descriptor 3 (internal/clock.CommandFD) and
// keeps one clock for all of them, handover successors included: Advance
// moves every Node's clock and returns once each answered that every timer
// due by then has fired, and Now reads it. A process's clock waits for the
// harness's start command and starts at the topology's time, the first
// process's at the real time it starts, so a process started while the
// clock leads real time, as after a walk, reads no earlier time and arms
// no timer from one. A Node stopped with Signal(SIGSTOP)
// misses advances and catches up when Signal(SIGCONT) resumes it. External
// processes (the State Store, the balancer, the mocks) run in real time;
// the harness waits for them, and for Nodes to act on what a timer
// started, with condition polls under real-time deadlines.
//
// # State Store, transport and mocks
//
// StartStateStore runs any internal/statestore/statestoretest/redisserver
// kind, a standalone redis-server process by default, a process kind also
// on another host (StateStoreOptions.Host), as the target chaos profile's
// on RH-1S. Each Node reaches it
// through its own fault proxy, or counting RESP proxy, one per endpoint of
// the URL (the server, a cluster's seeds or the sentinels), and the harness
// writes the URL through those proxies into the secret that
// NodeOptions.StateStoreURLs names; Node.StateStoreURL returns it, for a
// test that rotates a Node's URL secret to another State Store. With
// counting RESP proxies, each primary of a process cluster also announces
// a counting proxy of its own, shared by every Node (StateStore.Proxies),
// so the commands a cluster's slot map sends to the servers directly are
// counted too; the primaries' replicas replicate through those proxies,
// so their handshakes and acknowledgments (PING, AUTH, REPLCONF, PSYNC)
// count beside the Nodes' commands. The harness chooses TLS from
// internal/testkit/pki and credentials on every hop it owns: rediss:// with
// an ACL user, an https OTLP sink, and admin TLS through
// RURALZ_ADMIN_TLS_DIR with operator and metrics tokens. Transport turns
// each off; cleartext elsewhere appears only where the Bundle declares it.
// StartMockUpstream, StartIDP and StartOTLPSink run the in-process mocks,
// and StartBalancer runs the L4 balancer over the Nodes' listener of one
// name, probing /readyz, with PROXY protocol v2 on request.
//
// # Secret leaks
//
// Every secret value the harness supplies carries a canary
// (internal/testkit/canary): the secret files, the admin and metrics
// tokens, the MAC key and the State Store password. A value a test supplies
// must carry one from Canary or WithCanary. At teardown the harness scans
// every byte it captured for each canary, in every encoding canary.Scan
// knows: the Nodes' logs, every admin response but /debug/pprof profiles,
// the bytes a test hands to Capture (CLI output, client responses;
// HTTPClient captures its response bodies), the OTLP sinks' records, the
// mock Upstreams' request logs, the files under each RURALZ_DATA_DIR and
// every State Store's keyspace. A finding fails the test. The designated
// destinations, the State Store server and the mock IdP's token endpoint,
// are not scanned.
//
// # Failure evidence
//
// When a test fails, the harness copies each process's logs, a listing of
// each RURALZ_DATA_DIR, each Node's /debug/snapshots, the OTLP records and
// each balancer's transition log into a directory named after the test
// under RURALZ_TEST_ARTIFACT_DIR (Config.ArtifactDir overrides it).
//
// # Teardown
//
// Everything a Topology starts ends with its test. At cleanup it fetches
// each running Node's /debug/snapshots, stops every Node together through
// one Drain, scans for canaries, writes the evidence of a failed test, then
// closes the balancers, proxies, mocks and State Stores and removes its
// directory.
package topology
