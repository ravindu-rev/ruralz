// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package dev implements the ruralz dev commands of
// docs/reference/01-cli-and-api-surface.md: ruralz dev run, which renders a
// Bundle into a private test render and runs a local ruralzd on it with Hot
// Reload ("Local ruralzd launched by the CLI", through internal/cli/launch),
// and ruralz dev tap, which streams a Node's redacted /tap metadata
// ("Plugin, AI and development verbs").
//
// # Registration
//
// The package holds no state. Specs returns the rows of the dev noun, RunSpec
// and TapSpec one command each; the root's command table (cli.Registry)
// appends them and command.Execute dispatches them. Options carries the
// dependencies: the clock of every poll, settle, deadline and backoff, the
// randomness of the admin client and of the generated operator token, the
// CLI's build version, the launcher's options and the signals dev run
// forwards; the zero value is the real machine.
//
// # ruralz dev tap
//
// Subscribes to the /tap stream of the Node --admin names (default
// RURALZ_ADMIN_URL, else http://127.0.0.1:9901) through the admin client
// (internal/cli/adminclient), with ?sample= set to --sample RATE, a number in
// (0, 1] and 1 by default; another value is a usage error. --route NAME keeps
// the events whose route equals NAME, a client-side filter: the Node samples
// and costs the same whatever it keeps. The text form prints one line per
// event, "<time> <status> <method> <host><path> route=<route>
// upstream=<upstream> code=<code> <durationMs>ms", leaving out members that
// are empty, then one indented line per header value, "> <name>: <value>" for
// a request header and "< <name>: <value>" for a response header, in name
// order and redacted as /tap gives them; the status is colored on a terminal
// that takes color (2xx and 3xx green, 4xx yellow, 5xx red). --output json
// re-emits every line as received, {"dropped":N} lines included. Each
// {"dropped":N} line also warns on standard error.
//
// The first subscription must succeed, else the command exits 2: a refusal
// (401, or 503 RZ-RT-019 at the Node's subscriber limit) prints its problem
// document, and so does any other problem document, redirect or unexpected
// status. When the Node ends the stream, as at a Drain or shutdown, an admin
// port move or a missed write deadline, the command says so on standard
// error and subscribes again after a backoff that starts at 1 s and doubles
// up to 30 s (target), reset once a subscription is accepted; it reads the
// token file again each time, and an unreachable Node only lengthens the
// backoff. A later subscription the Node refuses, with a problem document, a
// redirect or an unexpected status, exits 2. Ctrl-C ends the command with
// exit 130, SIGTERM with 143.
//
// # ruralz dev run
//
// See RunSpec and internal/cli/launch: dev run renders DIR for --env, or
// from the process environment, into a private directory of mode 0700,
// launches ruralzd on that test render once a render is valid, prints a
// readiness block (or a ready NDJSON event with --output json), then polls
// the source every 500 ms (target) and swaps a new test render in by atomic
// rename once a change has settled for 250 ms (target), reporting each
// swap's activation or rejection. Ctrl-C sends ruralzd SIGTERM, which starts
// a Drain, a second Ctrl-C a second SIGTERM, and SIGKILL follows once the
// 30 s exit bound (target) passes; a SIGTERM the CLI receives is forwarded
// the same way. The command removes the private directory once ruralzd has
// exited and ends with 130, or 143 after SIGTERM; a ruralzd that exits on
// its own ends it with 2.
package dev

import (
	"io"
	"os"
	"os/signal"

	"github.com/ravindu-rev/ruralz/internal/buildinfo"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/launch"
	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Options are the dependencies of the dev commands. The zero value is the
// real machine.
type Options struct {
	// Clock runs dev tap's backoff, every poll, settle, deadline and exit
	// bound of dev run, and the admin client's deadlines; nil is
	// clock.Real().
	Clock clock.Clock
	// Rand supplies the admin client's traceparent IDs and dev run's
	// operator token; nil is crypto/rand.Reader.
	Rand io.Reader
	// Version is the CLI's build version, which the admin client sends in
	// User-Agent and compares with a Node's ruralz-version, and which dev
	// run requires of ruralzd's holder.json; empty is
	// buildinfo.Get().Version.
	Version string
	// Launch holds the launcher's own dependencies: where ruralz runs from
	// and the files the controllable-clock ruralzd test build inherits.
	// Its Clock, Rand and Version are taken from these Options.
	Launch launch.Options
	// Notify and StopNotify register and unregister the channel of the
	// SIGINT and SIGTERM signals dev run forwards to ruralzd; nil is
	// os/signal's Notify and Stop. The root's signal context cancels the
	// command on the first signal; Notify lets dev run count the second.
	Notify     func(c chan<- os.Signal, sig ...os.Signal)
	StopNotify func(c chan<- os.Signal)

	// terminal reports whether a stream is a terminal that takes ANSI
	// color; nil is command.IsTerminal. Tests set it.
	terminal func(w io.Writer) bool
}

// timer returns the configured clock, or the real one.
func (o Options) timer() clock.Clock {
	if o.Clock == nil {
		return clock.Real()
	}
	return o.Clock
}

// version returns the configured build version, or the binary's.
func (o Options) version() string {
	if o.Version == "" {
		return buildinfo.Get().Version
	}
	return o.Version
}

// notify returns the configured signal registration, or os/signal's.
func (o Options) notify() (func(c chan<- os.Signal, sig ...os.Signal), func(c chan<- os.Signal)) {
	n, s := o.Notify, o.StopNotify
	if n == nil {
		n = signal.Notify
	}
	if s == nil {
		s = signal.Stop
	}
	return n, s
}

// isTerminalFunc returns the configured terminal check, or the real one.
func (o Options) isTerminalFunc() func(io.Writer) bool {
	if o.terminal == nil {
		return command.IsTerminal
	}
	return o.terminal
}

// launchOptions returns the launcher's options with this package's clock,
// randomness and version.
func (o Options) launchOptions() launch.Options {
	l := o.Launch
	l.Clock, l.Rand, l.Version = o.timer(), o.Rand, o.version()
	return l
}

// Specs returns the rows of the dev noun for the root's command table: the
// noun description, ruralz dev run and ruralz dev tap.
func Specs(o Options) []command.Spec {
	return []command.Spec{
		{Path: []string{"dev"}, Summary: "Run and observe a local ruralzd"},
		RunSpec(o),
		TapSpec(o),
	}
}
