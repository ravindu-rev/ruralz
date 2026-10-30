// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package gateway is Ruralz Gateway, the data plane ruralzd runs: Run
// composes one Node from its parts and serves until its Drain ends
// (docs/architecture/03-data-plane.md). It is the L7 root of
// docs/engineering/02-repository-layout-and-conventions.md "Package
// layers": it wires the packages of layers L0 to L6 and injects what an
// import would climb for, such as the admin server's /metrics,
// /config/dump, /debug/snapshots, /debug/upstreams and /tap handlers, the
// State Store drivers' openers, the offline Policy checks
// (checks.Table()), the built-in Filter tables and the CEL compiler.
//
// # Start
//
// Run takes the process clock first (clock.Process), before anything
// opens a descriptor: a controllable-clock test build started without its
// command socket exits 2. It then snapshots the environment for ${VAR}
// substitution and removes every OTEL_* variable, reads the RURALZ_*
// process settings (internal/gateway/setting) and refuses start, exit 2,
// on any invalid one, naming each on standard error; it opens the data
// directory (identity/ and lkg/ created 0700) and the Node's node.id, and
// starts the telemetry runtime, whose process logs go to standard output
// as JSON lines. An admin token file, admin TLS directory or secret root
// it cannot use refuses start too, exit 2.
//
// # Parts
//
// One process holds one instance of each Node-wide part: the secret
// resolver, the configuration pipeline with the CEL compiler and the
// offline checks, the built-in Filter tables and their Components, the
// State Store manager with the memory and redis openers and the Node's
// RURALZ_ZONE, the Upstream manager, the composition engine, the in-flight
// unit and connection ceilings, the buffer budget, the retirer, the request
// handler with the Response Cache replayer, the client listeners, the
// readiness tracker, the admin server and its sockets, the /tap hub,
// Last-Known-Good, the source watcher, the activation loader, sd_notify
// with its watchdog, and the Drain. Socket deadlines the Go runtime
// enforces (connection, response-controller and pre-routing write
// deadlines) are dated on the real clock in every build; every timer,
// TTL, backoff and timeline runs on the process clock
// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
// scope"). With RURALZ_BIND_LOOPBACK=true every client listener and the
// admin port bind 127.0.0.1 and, where the host has IPv6, ::1, one socket
// per address (listener.LoopbackHosts).
//
// # Boot
//
// The Drain runs before the Node boots, so a stop signal during the boot
// drains at once: SIGTERM and SIGINT go to it (drain.Notify), SIGHUP to
// the source (source.NotifySIGHUP), and SIGPIPE is ignored. The process
// then tries the data-directory lock. The lock holder writes holder.json,
// serves the handover socket, binds the admin port of the Bundle it boots
// when that Bundle's Gateway parses, else Last-Known-Good's, else 9901
// (probe-bound, listening and pinned at once, so /healthz and /readyz
// answer during the boot), boots by the file-mode order (the source, else
// Last-Known-Good, else not ready) and sends READY=1 after its first boot
// attempt, ready or not. A process that finds a live lock holder is a
// handover successor (Linux): it binds the admin port of the holder's
// candidate without listening, boots that candidate with its sockets
// bound and not listening, then runs the handover gate
// (internal/gateway/handover); once it takes the lock it watches the
// source and serves the handover socket in its turn. A refused or lost
// handover ends the process with exit 1. A boot that cannot bind its admin
// port exits 1.
//
// # Supervision
//
// Every background part runs on a goroutine the Node owns: the retirer,
// the Upstream manager, the secret resolver's poller, each Filter
// Component, the activation loop and the watchdog. One that returns, or
// panics, before the Node stops it, like an admin server whose Serve fails
// outside shutdown, is logged at error and starts a Drain on the
// stop-signal timeline that ends with exit 1, so the process manager
// restarts the Node (docs/architecture/03-data-plane.md "Process and
// concurrency model"). The watchdog sends WATCHDOG=1 while the admin
// server, the retirer and the activation loop each answered its probe
// within the interval: the admin server's probe is a GET /healthz over
// the admin socket.
//
// # Stop
//
// Run returns the Drain's exit code (internal/gateway/drain): 0 after a
// stop signal or an accepted handover, 1 after a failed part. The end of
// Run's context counts as a stop signal. After the Drain deadline its
// steps shut down, in order, the admin server, the Filter Components (once
// the last snapshot retired), the State Store manager with its post-commit
// queue, and the telemetry runtime with its final flush; Run then stops
// every remaining goroutine before it returns.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/gateway/setting"
	"github.com/ravindu-rev/ruralz/internal/telemetry"
)

// Process exit codes of ruralzd (internal/gateway/setting).
const (
	// ExitOK follows a completed Drain after a stop signal or an
	// accepted handover.
	ExitOK = setting.ExitOK
	// ExitRuntime is a fatal runtime error: a part that stopped after
	// boot, an admin port that cannot be bound, a refused or lost
	// handover.
	ExitRuntime = setting.ExitRuntime
	// ExitUsage refuses start: an argument, an invalid process setting,
	// an unusable data directory, admin credential or secret root, or a
	// controllable-clock test build started without its clock commands.
	ExitUsage = setting.ExitUsage
)

// Run starts a Node from the process's settings and serves until its
// Drain ends, returning the process exit code. args are the command-line
// arguments, of which ruralzd takes none; process logs go to stdout and
// errors before the telemetry runtime starts to stderr. The end of ctx
// starts a Drain as a stop signal does.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return run(ctx, args, stdout, stderr, osProcess())
}

// process is what a Node takes from the operating system process; tests
// replace each part, so several Nodes run in one test binary.
type process struct {
	// clock returns the process clock (clock.Process).
	clock func() (clock.Clock, error)
	// wall is the real clock socket deadlines are dated on.
	wall clock.Clock
	// lookupEnv reads the process environment once the OTEL_* variables
	// are gone: the settings, secrets and the service manager's variables.
	lookupEnv func(string) (string, bool)
	// variables returns the ${VAR} values: the environment as it was
	// before the OTEL_* variables were removed.
	variables func() subst.Source
	// pid is the process ID holder.json, the handover and MAINPID= name.
	pid int
	// signals registers the stop signals and SIGHUP; tests stop a Node
	// through its context instead.
	signals bool
	// libraryErrors routes the OpenTelemetry SDK's errors to the process
	// log, a library global set once per process.
	libraryErrors bool
	// fileLimit returns the soft RLIMIT_NOFILE; nil reads the process's.
	fileLimit func() (uint64, bool)
	// rand draws the Drain jitter and the retry backoffs; nil means the
	// math/rand/v2 runtime generator.
	rand rand.Source
	// start is when the process started: a handover successor's gate
	// counts from it. Zero means the process clock's reading at start.
	start time.Time
}

// osProcess returns the process of the ruralzd binary.
func osProcess() process {
	return process{
		clock: clock.Process,
		wall:  clock.Real(),
		// Snapshot the environment, then remove every OTEL_* variable
		// before any OpenTelemetry constructor runs (ADR-0018).
		variables:     func() subst.Source { return telemetry.ScrubOTELEnvironment() },
		lookupEnv:     os.LookupEnv,
		pid:           os.Getpid(),
		signals:       true,
		libraryErrors: true,
	}
}

// run is Run over p.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, p process) int {
	clk, err := p.clock()
	if err != nil {
		if errors.Is(err, clock.ErrNoCommandDescriptor) {
			_, _ = fmt.Fprintf(stderr, "ruralzd: %v: this controllable-clock test build runs only under the test harness\n", err)
			return ExitUsage
		}
		_, _ = fmt.Fprintf(stderr, "ruralzd: process clock: %v\n", err)
		return ExitRuntime
	}
	vars := p.variables()
	s, err := setting.Load(args, p.lookupEnv)
	if err != nil {
		reportSettings(stderr, err)
		return setting.ExitCode(err)
	}
	if p.start.IsZero() {
		p.start = clk.Now()
	}
	n, code := start(ctx, p, clk, s, vars, stdout, stderr)
	if n == nil {
		return code
	}
	return n.serve(ctx)
}

// reportSettings writes each settings error of setting.Load to stderr, one
// line each, and the usage text after a command-line argument.
func reportSettings(stderr io.Writer, err error) {
	errs := []error{err}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs = j.Unwrap()
	}
	usage := false
	for _, e := range errs {
		_, _ = fmt.Fprintf(stderr, "ruralzd: %v\n", e)
		usage = usage || errors.Is(e, setting.ErrUsage)
	}
	if usage {
		_, _ = io.WriteString(stderr, setting.Usage)
	}
}
