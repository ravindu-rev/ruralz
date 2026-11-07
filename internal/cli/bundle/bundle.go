// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package bundle implements the ruralz bundle commands of
// docs/reference/01-cli-and-api-surface.md ("Bundle verbs in detail",
// "Global flags", "Output formats and exit codes"): ruralz bundle validate,
// render, diff, build, export and audit, which run Bundles through the
// configuration pipeline facade (internal/config/pipeline) with the
// offline Policy checks every binary links (checks.Table()) and the CEL
// compiler, so the CLI reports what Ruralz Control ingest and Node
// activation report for the same overlay and variables, and ruralz bundle
// import, which writes a Bundle from an OpenAPI document and runs it
// through the same stages first. The M2 verb push is a table row marked
// Planned.
//
// # Registration
//
// The package holds no state. Specs returns the rows of the bundle noun,
// ValidateSpec, RenderSpec, DiffSpec, BuildSpec, ImportSpec, ExportSpec
// and AuditSpec one command each; the root's command table (cli.Registry)
// appends them and command.Execute dispatches them. Options carries the
// dependencies: the clock of the
// admin client's deadlines and of the pipeline's yield timer, the admin
// client's randomness, the CLI's build version and standard input; the
// zero value is the real machine. Each run builds one pipeline.
//
// # Sources and Bundle flags
//
// DIR defaults to "." and names a Bundle directory rooted at ruralz.yaml or
// one file that holds a one-file Bundle, such as a rendered Bundle; "-"
// reads standard input to its end as a one-file Bundle named "-" within the
// source limit. A directory without ruralz.yaml is RZ-CFG-016, while a
// source that is missing, unreadable or neither a directory nor a regular
// file exits 2. The loader's limits, 64 MiB of source and 20,000 resources
// among them, are RZ-CFG-001.
//
// --env NAME renders with that Environment's spec.overlay and
// spec.variables from the local --environments FILE; without --env and
// without --environments the process environment supplies ${VAR} and no
// overlay applies. --env without --environments exits 2, since fetching
// Environments from Ruralz Control is Planned (M2), and so does an empty
// value of either flag. A command that reads an --environments file that
// declares no Environment exits 2 with "no Environment in FILE". With
// --environments and no --env, ruralz bundle validate runs once per
// Environment of the file in file order (pipeline.RunEach): the findings
// of stages A to C print once and name no Environment, those of stages D
// to M once per Environment, naming it in the JSON member environment and
// as a last word [environment=<name>] of a text line. ruralz bundle render
// and build exit 2 there, except render --api-version, which checks every
// Environment.
//
// # ruralz bundle validate
//
// Prints the run's diagnostics as its data: one text line per diagnostic,
// "file:line:column severity code Kind/name path: message", or with
// --output json one JSON array of diagnostics, [] when clean. It exits 1
// when a diagnostic is an error and 0 otherwise, warnings included.
// --online adds the online stage, which fails each Plugin with RZ-CFG-028
// until fetching Plugin artifacts ships, Planned (M2), and passes a Bundle
// without Plugins.
//
// # ruralz bundle render
//
// Writes the rendered Bundle of one Environment as YAML, or with --output
// json the same resources as one JSON array; a secret field that holds
// anything but a reference fails the render with RZ-CFG-012 and nothing is
// written. --effective prints the effective Filter Chains of every Route in
// canonical order, a table led by a ROUTE column or JSON rows led by a
// member route, and with --route NAME the chain of one Route, the table of
// the configuration model's worked example; a NAME that is no Route of the
// Bundle exits 2 with the nearest Route name as a hint. The data goes to
// standard output or atomically to --output-file.
//
// --api-version VERSION --output-dir DIR rewrites every base and overlay
// file under VERSION into DIR at its own path and in its own format,
// unmerged and unsubstituted, a one-file Bundle under its own file name,
// then builds both trees offline for every Environment of the required
// --environments, or only for --env, and prints one line per Environment,
// "<env>: <source Revision> -> <converted Revision> equal", ending in
// differs or invalid, then the human diff of each Environment whose
// Revisions differ; a tree's diagnostics print on standard error after
// "source: " or "converted: ". An unserved VERSION is RZ-CFG-007 and exits
// 2; a source document whose apiVersion is not served, or any other finding
// of stages A to C, which leaves a file that cannot be rewritten whole,
// exits 1; neither writes anything. The command exits 1 unless every
// Environment's Revisions are equal.
//
// # ruralz bundle diff
//
// Compares FROM with TO: a Bundle directory, a file that holds a
// /config/dump document (a saved dump, JSON member dump) or else a rendered
// Bundle (member bundle), an admin URL whose /config/dump the admin client
// reads with the side's credentials (member admin), or "-" for at most one
// side; a Revision or OCI source exits 2 as Planned (M2). --from-env and
// --to-env stand in for --env per side; Environment flags are a usage
// error on a dump or admin side, and with --environments every Bundle side
// needs one. A dump's content must hash to its digest (RZ-CFG-027) and
// decode at the schema levels this CLI serves (RZ-CFG-024). Both sides run
// through the pipeline, a dump through canonical re-entry, and the diff
// engine (internal/config/diff) compares them; the command exits 0 without
// changes, 1 with changes and 2 for errors, a side that fails on its
// diagnostics included.
//
// # ruralz bundle build
//
// Builds the Revision and prints "rev-<12 hex> sha256:<64 hex>", or with
// --output json an object with digest, revision, environment (with --env)
// and diagnostics, a failed build leaving out digest and revision. The
// online stage runs unless --offline is set, so a Bundle that holds a
// Plugin exits 1 until the Plugin check ships. --output-file receives the
// ruralz.canonical.v1 content, which hashes to the printed digest.
//
// # ruralz bundle import
//
// ruralz bundle import openapi FILE reads an OpenAPI 3.0 or 3.1 document
// with the files its relative references name (internal/cli/openapi) and
// writes a Bundle: a Gateway edge with one http listener on 8080, one
// Upstream per server the operations use and one Route per operation. A
// document that cannot be read, or that is no readable OpenAPI 3.0 or 3.1
// document, exits 2, each problem printed on standard error as
// "<file>:<line>:<column>: <message>". What the mapping leaves out prints
// as "warning: <file>:<line>:<column>: <message>". The Bundle runs through
// every offline stage before anything is written; a Bundle that does not
// validate prints its diagnostics and exits 2. It then goes to --output-dir
// DIR, which must not exist or must be empty, as ruralz.yaml, upstreams/
// and routes/, all files or none (render.WriteTree), or else as a one-file
// Bundle to standard output.
//
// # ruralz bundle export
//
// ruralz bundle export openapi|postman|dot [DIR] renders one Environment
// offline and writes an OpenAPI 3.1 document of its HTTP Routes (YAML, or
// JSON with --output json), a Postman collection v2.1 of the same
// operations, or a Graphviz DOT graph of its Routes, Policies and
// Upstreams, of one Route with --route (internal/cli/export). A Route or
// method without an operation prints "warning: Route/<name>: <reason>"; an
// invalid Bundle prints its diagnostics on standard error and exits 1.
//
// # ruralz bundle audit
//
// ruralz bundle audit [DIR] validates as ruralz bundle validate does,
// once per Environment of --environments without --env, and runs the
// rules of internal/cli/audit on the rendered Bundle of each run without
// error. It prints every finding, the run's diagnostics included with
// their RZ code as rule, in the diagnostic text form with the rule in the
// code position, or with --output json as one array, and exits 1 when a
// finding is an error.
//
// # Diagnostics and color
//
// render, build and diff print their diagnostics, errors and warnings
// alike, as text lines on standard error in every mode, diff prefixing each
// line with "from: " or "to: " for its side. With --output json, a render
// or diff that fails on diagnostics also prints them as one array on
// standard output, each diff entry carrying side, and exits 1 for render
// and 2 for diff. Human output is colored only when its stream is a
// terminal that takes ANSI color (TERM is not dumb; on Windows, a console
// with virtual terminal processing) and NO_COLOR is unset or empty: in the
// human diff, + lines green, - lines red, ~ and > lines yellow; diagnostic
// severities error red and warning yellow. JSON, files and pipes are never
// colored.
package bundle

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
)

// Options are the dependencies of the bundle commands. The zero value is
// the real machine.
type Options struct {
	// Clock runs the admin client's deadlines of a diff side that is a
	// Node, and the pipeline's yield timer; nil is clock.Real().
	Clock clock.Clock
	// Rand supplies the admin client's traceparent IDs; nil is
	// crypto/rand.Reader.
	Rand io.Reader
	// Version is the CLI's build version, which the admin client sends in
	// User-Agent and compares with a Node's ruralz-version; empty is
	// buildinfo.Get().Version.
	Version string
	// Stdin is what a source "-" reads; nil is os.Stdin.
	Stdin io.Reader

	// terminal reports whether a stream is a terminal that takes ANSI
	// color; nil is isTerminal. Tests set it.
	terminal func(w io.Writer) bool
	// pipelineFunc builds the configuration pipeline of one run; nil is
	// newPipeline. Tests set it.
	pipelineFunc func(o Options) (*pipeline.Pipeline, error)
}

// timer returns the configured clock, or the real one.
func (o Options) timer() clock.Clock {
	if o.Clock == nil {
		return clock.Real()
	}
	return o.Clock
}

// stdin returns the configured standard input, or the process's.
func (o Options) stdin() io.Reader {
	if o.Stdin == nil {
		return os.Stdin
	}
	return o.Stdin
}

// isTerminalFunc returns the configured terminal check, or the real one.
func (o Options) isTerminalFunc() func(io.Writer) bool {
	if o.terminal == nil {
		return isTerminal
	}
	return o.terminal
}

// Specs returns the rows of the bundle noun for the root's command table:
// the noun description, ruralz bundle validate, render, diff, build,
// import, export and audit, and push as a Planned row, which exits 2
// naming its milestone and stays out of help and completion.
func Specs(o Options) []command.Spec {
	return []command.Spec{
		{Path: []string{"bundle"}, Summary: "Validate, render, diff, build, import, export and audit Bundles"},
		ValidateSpec(o),
		RenderSpec(o),
		DiffSpec(o),
		BuildSpec(o),
		{Path: []string{"bundle", "push"}, Summary: "Send source to Ruralz Control, or a signed Revision to OCI", Planned: "M2"},
		ImportSpec(o),
		ExportSpec(o),
		AuditSpec(o),
	}
}

// stdinName is the source that reads standard input.
const stdinName = "-"

// dirArg is the optional DIR of validate, render and build.
func dirArg() command.Arg {
	return command.Arg{
		Name: "DIR", Usage: "Bundle directory, one-file Bundle, or - for standard input",
		Optional: true, Default: ".", Complete: command.CompleteFile,
	}
}

// newPipeline builds the configuration pipeline every binary builds: the
// offline Policy checks of checks.Table() and the CEL compiler, with
// GOMAXPROCS workers per stage, as the CLI runs it.
func newPipeline(o Options) (*pipeline.Pipeline, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	return pipeline.New(pipeline.Options{
		Checks: checks.Table(), CEL: c, Workers: runtime.GOMAXPROCS(0), Now: o.timer().Now,
	})
}

// invocation is one run of a bundle command: its streams, options and
// pipeline.
type invocation struct {
	sio  command.IO
	opts Options
	p    *pipeline.Pipeline
}

// start builds the pipeline of one run; a failure exits 2.
func start(sio command.IO, o Options) (*invocation, error) {
	build := o.pipelineFunc
	if build == nil {
		build = newPipeline
	}
	p, err := build(o)
	if err != nil {
		return nil, command.NoResultf("build the configuration pipeline: %w", err)
	}
	return &invocation{sio: sio, opts: o, p: p}, nil
}

// lookupEnv reads the invocation's environment snapshot.
func (inv *invocation) lookupEnv(key string) (string, bool) {
	if inv.sio.LookupEnv == nil {
		return "", false
	}
	return inv.sio.LookupEnv(key)
}

// variables returns the ${VAR} values of a render without --env: the
// process environment the invocation read.
func (inv *invocation) variables() subst.Source { return subst.Environ(inv.sio.Environ) }

// request returns the pipeline request of the Bundle src rendered as the
// Bundle flags b say: for --env, that Environment of --environments, whose
// spec.variables are the only ${VAR} values; without it, no overlay and the
// process environment's values.
func (inv *invocation) request(src loader.Source, b *bundleFlags) pipeline.Request {
	req := pipeline.Request{Source: src}
	if b.env != "" {
		req.Environments, req.Environment = b.envSource(), b.env
	} else {
		req.Variables = inv.variables()
	}
	return req
}

// source returns the Bundle source that arg names: "-" reads standard
// input to its end, within the source limit, as a one-file Bundle named
// "-"; anything else is an OS path the loader reads.
func (inv *invocation) source(arg string) (loader.Source, error) {
	if arg != stdinName {
		return loader.Source{Path: arg}, nil
	}
	data, err := readLimited(inv.opts.stdin(), inv.p.Limits().MaxSourceBytes)
	if err != nil {
		return loader.Source{}, command.NoResultf("read standard input: %w", err)
	}
	return stdinSource(data), nil
}

// readEnvironments reads the --environments file and refuses one that
// declares no Environment (docs/reference/01-cli-and-api-surface.md
// "Global flags"): no Environment passed its checks and the file holds no
// error, which would explain the loss.
func (inv *invocation) readEnvironments(ctx context.Context, file string) (*loader.Environments, error) {
	envs, err := inv.p.ReadEnvironments(ctx, loader.Source{Path: file})
	if err != nil {
		return nil, sourceError("--environments "+file, err)
	}
	if len(envs.FileOrder()) == 0 && !envs.Diagnostics().HasErrors() {
		return nil, command.NoResultf("no Environment in %s", file)
	}
	return envs, nil
}

// sourceError maps an error of a pipeline call that read what names: a
// source the loader cannot read exits 2 naming it; ctx's errors and faults
// of the release pass, exiting by command.Code.
func sourceError(what string, err error) error {
	if errors.Is(err, loader.ErrSource) {
		msg := strings.TrimPrefix(err.Error(), loader.ErrSource.Error()+": ")
		return command.NoResultf("cannot read %s: %s", what, msg)
	}
	return err
}
