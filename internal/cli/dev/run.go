// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dev

import (
	"context"
	"errors"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/launch"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
)

// runHelp is the paragraph of ruralz dev run --help.
const runHelp = `Renders DIR for --env, or from the process environment, into a private
directory and runs a local ruralzd on that test render, bound to loopback,
with a generated operator token. Once ruralzd is ready the command prints
its Revision, admin URL, listeners and a ruralz dev tap command line, then
polls DIR, the --environments and --secret-overrides files and the files
these name, and swaps in a new test render once a change has settled;
ruralzd Hot Reloads it. Ctrl-C stops ruralzd through a Drain, and a second
Ctrl-C ends the Drain at once.`

// workspacePrefix starts the name of dev run's private directory.
const workspacePrefix = "ruralz-dev-"

// RunSpec returns the ruralz dev run command. It runs where ruralzd is
// built: not on Windows (docs/reference/01-cli-and-api-surface.md
// "Platform support").
func RunSpec(o Options) command.Spec {
	return command.Spec{
		Path:    []string{"dev", "run"},
		Summary: "Local ruralzd with Hot Reload",
		Help:    runHelp,
		Args: []command.Arg{{
			Name: "DIR", Usage: "Bundle directory or one-file Bundle", Optional: true, Default: ".",
			Complete: command.CompleteFile,
		}},
		Platform: command.NotPlannedOn(map[string]string{"windows": "there is no ruralzd build; use WSL"}),
		New:      func() command.Command { return &runCommand{opts: o} },
	}
}

// runCommand is one run of ruralz dev run.
type runCommand struct {
	opts         Options
	flags        *command.Flags
	env          string
	environments string
	overrides    string
	ruralzd      string
	output       string
	ephemeral    bool
	memory       bool
	readyTimeout time.Duration
}

// Flags binds the Bundle flags and the launcher's flags.
func (c *runCommand) Flags(f *command.Flags) {
	c.flags = f
	f.String(&c.env, "env", "NAME", "",
		"Render with this Environment's overlay and variables, read from --environments", command.CompleteNone)
	f.String(&c.environments, "environments", "FILE", "", "Local Environment resources", command.CompleteFile)
	f.String(&c.overrides, "secret-overrides", "FILE", "",
		"Local files replacing secretRefs: provider:name keys, file path values", command.CompleteFile)
	f.Bool(&c.ephemeral, "ephemeral-ports", "Rewrite every listener port and admin.port to a free loopback port")
	f.Bool(&c.memory, "memory-state-store", "Replace the Gateway's stateStore with driver memory")
	f.Duration(&c.readyTimeout, "ready-timeout", launch.DefaultReadyTimeout,
		"How long to wait for /readyz after the launch and for each activation")
	f.String(&c.ruralzd, "ruralzd", "PATH", "", "The ruralzd to launch (default: beside ruralz, else on PATH)", command.CompleteFile)
	f.Output(&c.output, "text", "text", "json")
}

// Run renders, launches, watches and stops.
func (c *runCommand) Run(ctx context.Context, sio command.IO, args []string) error {
	src := args[0]
	if err := c.check(src); err != nil {
		return err
	}
	if err := checkSource(src); err != nil {
		return err
	}
	lo := c.opts.launchOptions()
	p, err := newPipeline(lo.Clock)
	if err != nil {
		return command.NoResultf("build the configuration pipeline: %w", err)
	}
	if c.environments != "" {
		if err := readEnvironments(ctx, p, c.environments); err != nil {
			return err
		}
	}
	if c.overrides != "" {
		if err := launch.CheckOverrides(ctx, c.overrides); err != nil {
			return err
		}
	}
	bin, err := launch.FindRuralzd(sio, c.ruralzd, lo)
	if err != nil {
		return err
	}
	ws, err := launch.NewWorkspace(workspacePrefix, c.opts.Rand)
	if err != nil {
		return err
	}
	notify, stopNotify := c.opts.notify()
	r := &runner{
		sio: sio, clock: lo.Clock, lo: lo, ws: ws, bin: bin, src: src,
		environments: c.environments, overrides: c.overrides,
		json:         c.output == "json",
		readyTimeout: c.readyTimeout,
		out:          command.NewLineWriter(sio.Stdout),
		errw:         command.NewLineWriter(sio.Stderr),
		sigs:         make(chan os.Signal, 4),
		colorErr:     command.Colors(sio, sio.Stderr, c.opts.isTerminalFunc()),
	}
	r.compact = !r.json && c.opts.isTerminalFunc()(sio.Stderr)
	r.renderer = launch.NewRenderer(p, ws, launch.Request{
		Source: src, Environments: c.environments, Environment: c.env,
		Variables:      subst.Environ(sio.Environ),
		EphemeralPorts: c.ephemeral, Overrides: c.overrides, MemoryStateStore: c.memory,
	})
	notify(r.sigs, os.Interrupt, syscall.SIGTERM)
	defer stopNotify(r.sigs)
	return r.run(ctx)
}

// check refuses flag values the command cannot run with: an empty
// --environments, --env given empty or without --environments,
// --environments without --env, an empty --secret-overrides or --ruralzd,
// and "-" as DIR, since the command watches its source.
func (c *runCommand) check(src string) error {
	if c.flags.IsSet("environments") && c.environments == "" {
		return command.Usagef("--environments needs a file")
	}
	if c.flags.IsSet("env") {
		if c.env == "" {
			return command.Usagef("--env needs an Environment name")
		}
		if c.environments == "" {
			return command.Usagef("--env needs --environments: fetching Environments from Ruralz Control is Planned (M2)")
		}
	}
	if c.environments != "" && c.env == "" {
		return command.Usagef("--environments needs --env: ruralz dev run renders one Environment")
	}
	if c.flags.IsSet("secret-overrides") && c.overrides == "" {
		return command.Usagef("--secret-overrides needs a file")
	}
	if c.flags.IsSet("ruralzd") && c.ruralzd == "" {
		return command.Usagef("--ruralzd needs a path")
	}
	if src == "-" {
		return command.Usagef("DIR cannot be -: ruralz dev run watches its source, so it takes a directory or a file")
	}
	return nil
}

// checkSource refuses a source that is missing, unreadable or neither a
// directory nor a regular file, exit 2.
func checkSource(src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return command.NoResultf("cannot read %s: %w", src, err)
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return command.NoResultf("cannot read %s: neither a directory nor a regular file", src)
	}
	return nil
}

// newPipeline builds the configuration pipeline every binary builds: the
// offline Policy checks of checks.Table() and the CEL compiler, with
// GOMAXPROCS workers per stage, as the CLI runs it.
func newPipeline(clk clock.Clock) (*pipeline.Pipeline, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	return pipeline.New(pipeline.Options{
		Checks: checks.Table(), CEL: c, Workers: runtime.GOMAXPROCS(0), Now: clk.Now,
	})
}

// readEnvironments reads the --environments file, refusing one that
// cannot be read or declares no Environment, exit 2
// (docs/reference/01-cli-and-api-surface.md "Global flags").
func readEnvironments(ctx context.Context, p *pipeline.Pipeline, file string) error {
	envs, err := p.ReadEnvironments(ctx, loader.Source{Path: file})
	if err != nil {
		if errors.Is(err, loader.ErrSource) {
			return command.NoResultf("cannot read --environments %s: %w", file, err)
		}
		return err
	}
	if len(envs.FileOrder()) == 0 && !envs.Diagnostics().HasErrors() {
		return command.NoResultf("no Environment in %s", file)
	}
	return nil
}

// errExited is the end of a ruralzd that exited on its own.
var errExited = errors.New("ruralzd exited")

// exitedError is the exit 2 of a child that exited on its own, naming its
// exit status.
func exitedError(c *launch.Child, ready bool) error {
	if !ready {
		return command.NoResultf("%w before it was ready: %s", errExited, c.Status())
	}
	return command.NoResultf("%w: %s", errExited, c.Status())
}

// interruptionOf returns the error a run a signal ended returns: the first
// signal dev run received, else the cause of ctx, so command.Code exits
// 130 after SIGINT and 143 after SIGTERM.
func interruptionOf(ctx context.Context, first os.Signal) error {
	if first != nil {
		return command.Interrupted{Signal: first}
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return context.Canceled
}
