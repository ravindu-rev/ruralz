// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command_test

import (
	"context"
	"slices"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/completion"
)

// The M1 command table of spec 10 req 12, built from fixture commands:
// the real commands live in later work packages, so the golden help and
// the dispatch tests pin the framework over this table.

// fixture is a Command whose flags are bound by a function and whose Run
// is scripted; the bound values are kept for assertions.
type fixture struct {
	bind func(f *command.Flags, v *values)
	run  func(ctx context.Context, sio command.IO, args []string, v *values) error
	v    values
}

// values holds the flag variables of one fixture invocation.
type values struct {
	f     *command.Flags
	strs  map[string]*string
	bools map[string]*bool
	durs  map[string]*time.Duration
}

func (v *values) str(name string) *string {
	p := new(string)
	v.strs[name] = p
	return p
}

func (v *values) boolean(name string) *bool {
	p := new(bool)
	v.bools[name] = p
	return p
}

func (v *values) dur(name string) *time.Duration {
	p := new(time.Duration)
	v.durs[name] = p
	return p
}

// snapshot returns every flag value as a string, for comparisons.
func (v *values) snapshot() map[string]string {
	out := map[string]string{}
	for k, p := range v.strs {
		out[k] = *p
	}
	for k, p := range v.bools {
		if *p {
			out[k] = "true"
		} else {
			out[k] = "false"
		}
	}
	for k, p := range v.durs {
		out[k] = p.String()
	}
	return out
}

func (x *fixture) Flags(f *command.Flags) {
	x.v = values{f: f, strs: map[string]*string{}, bools: map[string]*bool{}, durs: map[string]*time.Duration{}}
	if x.bind != nil {
		x.bind(f, &x.v)
	}
}

func (x *fixture) Run(ctx context.Context, sio command.IO, args []string) error {
	if x.run == nil {
		return nil
	}
	return x.run(ctx, sio, args, &x.v)
}

// echo prints the positionals and flag values as JSON.
func echo(_ context.Context, sio command.IO, args []string, v *values) error {
	return command.WriteJSON(sio.Stdout, map[string]any{"args": args, "flags": v.snapshot()})
}

type runFunc = func(ctx context.Context, sio command.IO, args []string, v *values) error

func newFixture(bind func(*command.Flags, *values), run runFunc) func() command.Command {
	return func() command.Command { return &fixture{bind: bind, run: run} }
}

// bundleFlags binds --env and --environments (spec 10 req 25).
func bundleFlags(f *command.Flags, v *values) {
	f.String(v.str("env"), "env", "NAME", "", "Environment to render with (needs --environments)", command.CompleteNone)
	f.String(v.str("environments"), "environments", "FILE", "", "Local Environment resources", command.CompleteFile)
}

// adminFlags binds the admin credential flags, and --admin when def is set.
func adminFlags(f *command.Flags, v *values, def string) {
	if def != "" {
		f.String(v.str("admin"), "admin", "URL", def, "Admin base URL of the Node", command.CompleteNone)
	}
	f.String(v.str("admin-token-file"), "admin-token-file", "PATH", "", "Operator admin token file", command.CompleteFile)
	f.String(v.str("ca-file"), "ca-file", "PATH", "", "Extra trust anchors (PEM)", command.CompleteFile)
	f.String(v.str("client-cert"), "client-cert", "PATH", "", "Client certificate for admin mTLS", command.CompleteFile)
	f.String(v.str("client-key"), "client-key", "PATH", "", "Key of --client-cert", command.CompleteFile)
}

func dirArg() command.Arg {
	return command.Arg{
		Name: "DIR", Usage: "Bundle directory or one-file Bundle", Optional: true, Default: ".",
		Complete: command.CompleteDir,
	}
}

// m1Table returns the spec 10 req 12 table; run is every command's Run.
func m1Table(run runFunc) []command.Spec {
	specs := []command.Spec{
		{Path: []string{"bundle"}, Summary: "Validate, render, diff and build Bundles"},
		{Path: []string{"dev"}, Summary: "Run and observe a local ruralzd"},
		{Path: []string{"node"}, Summary: "Operate the local Node"},
		{
			Path: []string{"bundle", "validate"}, Summary: "Offline validation, source-mapped diagnostics",
			Args: []command.Arg{dirArg()},
			New: newFixture(func(f *command.Flags, v *values) {
				bundleFlags(f, v)
				f.Bool(v.boolean("online"), "online", "Also check Plugin artifacts")
				f.Output(v.str("output"), "text", "text", "json")
			}, run),
		},
		{
			Path: []string{"bundle", "render"}, Summary: "Render one Environment's one-file Bundle",
			Args: []command.Arg{dirArg()},
			New: newFixture(func(f *command.Flags, v *values) {
				bundleFlags(f, v)
				f.OutputFile(v.str("output-file"), "Write the data to PATH instead of stdout")
				f.Bool(v.boolean("effective"), "effective", "Print the resolved Filter Chains of --route")
				f.String(v.str("route"), "route", "NAME", "", "Route of --effective", command.CompleteNone)
				f.String(v.str("api-version"), "api-version", "VERSION", "", "Convert the sources to VERSION", command.CompleteNone)
				f.String(v.str("output-dir"), "output-dir", "DIR", "", "Directory of the converted sources", command.CompleteDir)
				f.Output(v.str("output"), "yaml", "yaml", "json")
				f.EnumMode("output", []string{"effective", "api-version"}, "text", "text", "json")
			}, run),
		},
		{
			Path: []string{"bundle", "diff"}, Summary: "Compare FROM with TO",
			Args: []command.Arg{
				{Name: "FROM", Usage: "Bundle directory, file, saved dump or admin URL", Complete: command.CompleteFile},
				{Name: "TO", Usage: "Same forms as FROM", Complete: command.CompleteFile},
			},
			New: newFixture(func(f *command.Flags, v *values) {
				bundleFlags(f, v)
				f.String(v.str("from-env"), "from-env", "NAME", "", "Environment of FROM", command.CompleteNone)
				f.String(v.str("to-env"), "to-env", "NAME", "", "Environment of TO", command.CompleteNone)
				adminFlags(f, v, "")
				f.Output(v.str("output"), "text", "text", "json")
			}, run),
		},
		{
			Path: []string{"bundle", "build"}, Summary: "Build a Revision, print its digest",
			Args: []command.Arg{dirArg()},
			New: newFixture(func(f *command.Flags, v *values) {
				bundleFlags(f, v)
				f.Bool(v.boolean("offline"), "offline", "Skip the online Plugin check")
				f.OutputFile(v.str("output-file"), "Write the canonical content to PATH")
				f.Output(v.str("output"), "text", "text", "json")
			}, run),
		},
		{
			Path: []string{"dev", "run"}, Summary: "Local ruralzd with Hot Reload",
			Args:     []command.Arg{dirArg()},
			Platform: command.NotPlannedOn(map[string]string{"windows": "there is no ruralzd build; use WSL"}),
			New: newFixture(func(f *command.Flags, v *values) {
				bundleFlags(f, v)
				f.String(v.str("secret-overrides"), "secret-overrides", "FILE", "", "Local files replacing secretRefs", command.CompleteFile)
				f.Bool(v.boolean("ephemeral-ports"), "ephemeral-ports", "Replace every port with a free one")
				f.Duration(v.dur("ready-timeout"), "ready-timeout", 30*time.Second, "How long to wait for /readyz")
				f.Output(v.str("output"), "text", "text", "json")
			}, run),
		},
		{
			Path: []string{"dev", "tap"}, Summary: "Stream redacted /tap metadata",
			New: newFixture(func(f *command.Flags, v *values) {
				f.String(v.str("route"), "route", "NAME", "", "Only print exchanges of this Route", command.CompleteNone)
				adminFlags(f, v, "http://127.0.0.1:9901")
				f.Output(v.str("output"), "text", "text", "json")
			}, run),
		},
		{
			Path: []string{"node", "drain"}, Summary: "Drain the local ruralzd",
			Platform: command.NotPlannedOn(map[string]string{
				"darwin":  "it needs /proc/locks",
				"windows": "there is no ruralzd and no SIGTERM",
			}),
			New: newFixture(func(f *command.Flags, v *values) {
				f.String(v.str("data-dir"), "data-dir", "PATH", "", "Data directory (default $RURALZ_DATA_DIR, else /var/lib/ruralz)", command.CompleteDir)
				f.Duration(v.dur("timeout"), "timeout", 5*time.Minute, "How long to await the exit")
			}, run),
		},
		{
			Path: []string{"node", "dump"}, Summary: "Save /config/dump, secrets omitted",
			New: newFixture(func(f *command.Flags, v *values) {
				adminFlags(f, v, "http://127.0.0.1:9901")
				f.OutputFile(v.str("output-file"), "Write the document to PATH instead of stdout")
			}, run),
		},
		{
			Path: []string{"version"}, Summary: "Version, commit, flavor, levels",
			New: newFixture(func(f *command.Flags, v *values) {
				f.Output(v.str("output"), "text", "text", "json")
			}, run),
		},
		{Path: []string{"bundle", "push"}, Summary: "Send source to Ruralz Control", Planned: "M2"},
		{Path: []string{"bundle", "audit"}, Summary: "Security and best-practice findings", Planned: "M2"},
		{Path: []string{"test", "run"}, Summary: "Run request and expected-response cases", Planned: "M2"},
		{Path: []string{"rollout", "status"}, Summary: "State, batches, ACKs, NACKs, lagging Nodes", Planned: "M2"},
		{Path: []string{"rollout", "start"}, Summary: "Start a revert Rollout", Planned: "M2"},
		{Path: []string{"ai", "cost"}, Summary: "Cost attribution", Planned: "M3"},
		{Path: []string{"node", "list"}, Summary: "Nodes with active and Last-Known-Good digests", Planned: "M2"},
	}
	registry := func() []command.Spec { return m1Table(run) }
	specs = append(specs, completion.Spec("ruralz", registry))
	return specs
}

// m1Paths are the ten runnable M1 commands (spec 10 req 3).
func m1Paths() [][]string {
	return [][]string{
		{"bundle", "build"},
		{"bundle", "diff"},
		{"bundle", "render"},
		{"bundle", "validate"},
		{"completion"},
		{"dev", "run"},
		{"dev", "tap"},
		{"node", "drain"},
		{"node", "dump"},
		{"version"},
	}
}

// runnable returns the paths of the runnable specs, sorted.
func runnable(specs []command.Spec) [][]string {
	var out [][]string
	for _, s := range specs {
		if s.New != nil {
			out = append(out, s.Path)
		}
	}
	slices.SortFunc(out, slices.Compare)
	return out
}
