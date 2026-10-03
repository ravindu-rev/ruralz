// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package completion_test

import (
	"context"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/completion"
)

// The M1 command table of spec 10 req 12 with fixture commands (the real
// commands live in later work packages), for the golden scripts.

// binder binds a fixture command's flags.
type binder func(f *command.Flags)

// fixture is a Command with bound flags and nothing to run.
type fixture struct{ bind binder }

func (x fixture) Flags(f *command.Flags) {
	if x.bind != nil {
		x.bind(f)
	}
}

func (fixture) Run(context.Context, command.IO, []string) error { return nil }

func cmd(bind binder) func() command.Command {
	return func() command.Command { return fixture{bind: bind} }
}

func bundleFlags(f *command.Flags) {
	var env, envs string
	f.String(&env, "env", "NAME", "", "Environment to render with (needs --environments)", command.CompleteNone)
	f.String(&envs, "environments", "FILE", "", "Local Environment resources", command.CompleteFile)
}

func adminFlags(f *command.Flags, def string) {
	var admin, token, ca, cert, key string
	if def != "" {
		f.String(&admin, "admin", "URL", def, "Admin base URL of the Node", command.CompleteNone)
	}
	f.String(&token, "admin-token-file", "PATH", "", "Operator admin token file", command.CompleteFile)
	f.String(&ca, "ca-file", "PATH", "", "Extra trust anchors (PEM)", command.CompleteFile)
	f.String(&cert, "client-cert", "PATH", "", "Client certificate for admin mTLS", command.CompleteFile)
	f.String(&key, "client-key", "PATH", "", "Key of --client-cert", command.CompleteFile)
}

func output(f *command.Flags) {
	var o string
	f.Output(&o, "text", "text", "json")
}

func dirArg() command.Arg {
	return command.Arg{
		Name: "DIR", Usage: "Bundle directory or one-file Bundle", Optional: true, Default: ".",
		Complete: command.CompleteDir,
	}
}

// m1Table returns the spec 10 req 12 table, with Planned rows and one
// deprecated verb and flag that completion leaves out.
func m1Table() []command.Spec {
	var s string
	var b bool
	var d time.Duration
	specs := []command.Spec{
		{Path: []string{"bundle"}, Summary: "Validate, render, diff and build Bundles"},
		{Path: []string{"dev"}, Summary: "Run and observe a local ruralzd"},
		{Path: []string{"node"}, Summary: "Operate the local Node"},
		{Path: []string{"rollout"}, Summary: "Rollouts (M2)"},
		{
			Path: []string{"bundle", "validate"}, Summary: "Offline validation, source-mapped diagnostics",
			Args: []command.Arg{dirArg()},
			New: cmd(func(f *command.Flags) {
				bundleFlags(f)
				f.Bool(&b, "online", "Also check Plugin artifacts")
				output(f)
			}),
		},
		{
			Path: []string{"bundle", "render"}, Summary: "Render one Environment's one-file Bundle",
			Args: []command.Arg{dirArg()},
			New: cmd(func(f *command.Flags) {
				bundleFlags(f)
				f.OutputFile(&s, "Write the data to PATH instead of stdout")
				f.Bool(&b, "effective", "Print the resolved Filter Chains of --route")
				f.String(&s, "route", "NAME", "", "Route of --effective", command.CompleteNone)
				f.String(&s, "api-version", "VERSION", "", "Convert the sources to VERSION", command.CompleteNone)
				f.String(&s, "output-dir", "DIR", "", "Directory of the converted sources", command.CompleteDir)
				f.Output(&s, "yaml", "yaml", "json")
				f.EnumMode("output", []string{"effective", "api-version"}, "text", "text", "json")
			}),
		},
		{
			Path: []string{"bundle", "diff"}, Summary: "Compare FROM with TO",
			Args: []command.Arg{
				{Name: "FROM", Usage: "Bundle directory, file, saved dump or admin URL", Complete: command.CompleteFile},
				{Name: "TO", Usage: "Same forms as FROM", Complete: command.CompleteFile},
			},
			New: cmd(func(f *command.Flags) {
				bundleFlags(f)
				f.String(&s, "from-env", "NAME", "", "Environment of FROM", command.CompleteNone)
				f.String(&s, "to-env", "NAME", "", "Environment of TO", command.CompleteNone)
				adminFlags(f, "")
				output(f)
			}),
		},
		{
			Path: []string{"bundle", "build"}, Summary: "Build a Revision, print its digest",
			Args: []command.Arg{dirArg()},
			New: cmd(func(f *command.Flags) {
				bundleFlags(f)
				f.Bool(&b, "offline", "Skip the online Plugin check")
				f.OutputFile(&s, "Write the canonical content to PATH")
				output(f)
			}),
		},
		{
			Path: []string{"dev", "run"}, Summary: "Local ruralzd with Hot Reload",
			Args:     []command.Arg{dirArg()},
			Platform: command.NotPlannedOn(map[string]string{"windows": "there is no ruralzd build; use WSL"}),
			New: cmd(func(f *command.Flags) {
				bundleFlags(f)
				f.String(&s, "secret-overrides", "FILE", "", "Local files replacing secretRefs", command.CompleteFile)
				f.Bool(&b, "ephemeral-ports", "Replace every port with a free one")
				f.Duration(&d, "ready-timeout", 30*time.Second, "How long to wait for /readyz")
				output(f)
			}),
		},
		{
			Path: []string{"dev", "tap"}, Summary: "Stream redacted /tap metadata",
			New: cmd(func(f *command.Flags) {
				f.String(&s, "route", "NAME", "", "Only print exchanges of this Route", command.CompleteNone)
				adminFlags(f, "http://127.0.0.1:9901")
				output(f)
			}),
		},
		{
			Path: []string{"node", "drain"}, Summary: "Drain the local ruralzd",
			New: cmd(func(f *command.Flags) {
				f.String(&s, "data-dir", "PATH", "", "Data directory (default $RURALZ_DATA_DIR, else /var/lib/ruralz)", command.CompleteDir)
				f.Duration(&d, "timeout", 5*time.Minute, "How long to await the exit")
				f.String(&s, "lock-dir", "PATH", "", "Old name of --data-dir", command.CompleteDir)
				f.Deprecate("lock-dir", "use --data-dir")
			}),
		},
		{
			Path: []string{"node", "dump"}, Summary: "Save /config/dump, secrets omitted",
			New: cmd(func(f *command.Flags) {
				adminFlags(f, "http://127.0.0.1:9901")
				f.OutputFile(&s, "Write the document to PATH instead of stdout")
			}),
		},
		{
			Path: []string{"node", "save"}, Summary: "Old name of node dump", Deprecated: "use ruralz node dump",
			New: cmd(nil),
		},
		{Path: []string{"version"}, Summary: "Version, commit, flavor, levels", New: cmd(output)},
		{Path: []string{"bundle", "push"}, Summary: "Send source to Ruralz Control", Planned: "M2"},
		{Path: []string{"rollout", "status"}, Summary: "State of a Rollout", Planned: "M2"},
		{Path: []string{"node", "list"}, Summary: "Nodes with their digests", Planned: "M2"},
	}
	return append(specs, completion.Spec("ruralz", m1Table))
}
