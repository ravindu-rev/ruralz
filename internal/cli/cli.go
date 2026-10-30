// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package cli is the root of the ruralz command-line interface
// (docs/reference/01-cli-and-api-surface.md "CLI command tree"): Run wires
// one process to the command table that Registry builds, whose commands
// take the form ruralz <noun> <verb>, ruralz version and ruralz completion
// being the only exceptions. The table holds every noun and verb of the
// CLI command registry (docs/_meta/foundation-pack.md section 9) and the
// reference's command table: the commands this build runs, from the noun
// packages bundle, dev and node, version and completion, and every command
// a later milestone builds as a Planned row, which exits 2 naming its
// milestone and stays out of help and completion.
//
// The package holds no state: Registry builds the table per call, and Run
// builds the invocation's streams, environment snapshot and clock, and a
// signal context whose first SIGINT or SIGTERM cancels the command, which
// then exits 130 or 143.
package cli

import (
	"context"
	"io"
	"os"

	"github.com/ravindu-rev/ruralz/internal/cli/bundle"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/completion"
	"github.com/ravindu-rev/ruralz/internal/cli/dev"
	"github.com/ravindu-rev/ruralz/internal/cli/node"
	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Program is the name of the CLI binary, the first word of every command.
const Program = "ruralz"

// Exit codes of docs/reference/01-cli-and-api-surface.md "Exit codes".
// They never change once released.
const (
	// ExitOK is success with nothing to report.
	ExitOK = command.ExitOK
	// ExitNegative is a negative result, such as an invalid Bundle.
	ExitNegative = command.ExitNegative
	// ExitNoResult is no result: a usage error, unreadable input, an admin
	// error, an unsupported platform.
	ExitNoResult = command.ExitNoResult
	// ExitWaiting is waiting on a person.
	ExitWaiting = command.ExitWaiting
	// ExitInterrupted is an invocation SIGINT ended, 128 plus its number.
	ExitInterrupted = command.ExitInterrupted
	// ExitTerminated is an invocation SIGTERM ended, 128 plus its number.
	ExitTerminated = command.ExitTerminated
)

// Options are the dependencies of the CLI's commands. The zero value is
// the real machine.
type Options struct {
	// Clock runs every deadline, poll and backoff; nil is clock.Real().
	Clock clock.Clock
	// Rand supplies the admin client's traceparent IDs and ruralz dev
	// run's operator token; nil is crypto/rand.Reader.
	Rand io.Reader
	// Version is the CLI's build version; empty is
	// buildinfo.Get().Version.
	Version string
	// Stdin is what a Bundle source "-" reads; nil is os.Stdin.
	Stdin io.Reader
	// Dev holds the launcher's and the signals' dependencies of ruralz dev
	// run; its Clock, Rand and Version come from these Options.
	Dev dev.Options
}

// Registry returns the command table of ruralz: the rows of every noun,
// version and completion, with o's dependencies, each summary the purpose
// the reference's command table gives it.
func Registry(o Options) []command.Spec {
	var specs []command.Spec
	specs = append(specs, bundle.Specs(bundle.Options{Clock: o.Clock, Rand: o.Rand, Version: o.Version, Stdin: o.Stdin})...)
	d := o.Dev
	d.Clock, d.Rand, d.Version = o.Clock, o.Rand, o.Version
	specs = append(specs, dev.Specs(d)...)
	specs = append(specs, node.Specs(node.Options{Clock: o.Clock, Rand: o.Rand, Version: o.Version})...)
	specs = append(specs, plannedSpecs()...)
	specs = append(specs, versionSpec(o))
	comp := completion.Spec(Program, func() []command.Spec { return Registry(o) })
	comp.Summary = "Shell completion script"
	return append(specs, comp)
}

// Run executes the ruralz CLI with args (without the program name) and
// returns the process exit code. Data goes to stdout; progress, warnings,
// usage and errors go to stderr. The first SIGINT or SIGTERM cancels the
// command, which exits 130 after SIGINT and 143 after SIGTERM; a second
// SIGINT restores the default handling, so a third Ctrl-C kills the
// process.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	ctx, stop := command.SignalContext(ctx)
	defer stop()
	sio := command.NewIO(stdout, stderr, os.Environ(), clock.Real().Now, "")
	return command.Execute(ctx, sio, Program, Registry(Options{}), args)
}

// plannedSpecs returns the commands of the nouns without a package in this
// build, each a Planned row: ruralz test, rollout, plugin, ai and control
// (docs/reference/01-cli-and-api-surface.md "Command table").
func plannedSpecs() []command.Spec {
	return []command.Spec{
		{Path: []string{"test", "run"}, Summary: "Run request and expected-response cases", Planned: "M2"},
		{Path: []string{"rollout", "start"}, Summary: "Start a revert Rollout, print its ID", Planned: "M2"},
		{Path: []string{"rollout", "status"}, Summary: "State, batches, ACKs, NACKs, lagging Nodes", Planned: "M2"},
		{Path: []string{"rollout", "pause"}, Summary: "Enter paused", Planned: "M2"},
		{Path: []string{"rollout", "resume"}, Summary: "Continue its persisted plan", Planned: "M2"},
		{Path: []string{"rollout", "rollback"}, Summary: "Re-deliver the previous Revision", Planned: "M2"},
		{Path: []string{"rollout", "approve"}, Summary: "Approve a queued promotion or held revert", Planned: "M2"},
		{Path: []string{"rollout", "reject"}, Summary: "Discard a queued promotion or held revert", Planned: "M2"},
		{Path: []string{"plugin", "init"}, Summary: "Scaffold a Plugin with tests (plugin generator)", Planned: "M2"},
		{Path: []string{"plugin", "build"}, Summary: "Compile to WASM, derive Capabilities, generate shims", Planned: "M2"},
		{Path: []string{"plugin", "test"}, Summary: "Test on ruralzd's wazero host", Planned: "M2"},
		{Path: []string{"plugin", "push"}, Summary: "Publish to OCI and sign", Planned: "M2"},
		{Path: []string{"plugin", "inspect"}, Summary: "ABI, Phases, Capabilities, digest, signatures", Planned: "M2"},
		{Path: []string{"ai", "cost"}, Summary: "Cost attribution from provider-reported usage", Planned: "M3"},
		{Path: []string{"ai", "models"}, Summary: "List AIModel candidates", Planned: "M3"},
		{Path: []string{"control", "serve"}, Summary: "Launch a ruralz-control replica", Planned: "M2"},
		{Path: []string{"control", "join"}, Summary: "Get a new replica's peer certificate", Planned: "M2"},
		{Path: []string{"control", "backup"}, Summary: "Snapshot the Control Store and Node revocation list", Planned: "M2"},
		{Path: []string{"control", "restore"}, Summary: "Restore into an empty deployment", Planned: "M2"},
	}
}
