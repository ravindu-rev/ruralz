// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"context"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
)

// validateHelp is the paragraph of ruralz bundle validate --help.
const validateHelp = `Runs every offline stage of the configuration pipeline and prints each
diagnostic as one line, or with --output json as one JSON array. With
--environments and no --env it runs once per Environment of the file, in file
order. Exits 1 when a diagnostic is an error, 0 otherwise.`

// ValidateSpec returns the ruralz bundle validate command.
func ValidateSpec(o Options) command.Spec {
	return command.Spec{
		Path:    []string{"bundle", "validate"},
		Summary: "Offline validation, source-mapped diagnostics",
		Help:    validateHelp,
		Args:    []command.Arg{dirArg()},
		New:     func() command.Command { return &validateCommand{opts: o} },
	}
}

// validateCommand is one run of ruralz bundle validate.
type validateCommand struct {
	opts   Options
	bundle bundleFlags
	online bool
	output string
}

// Flags binds the Bundle flags, --online and --output text or json.
func (c *validateCommand) Flags(f *command.Flags) {
	c.bundle.bind(f)
	f.Bool(&c.online, "online", "Also check each Plugin artifact (fetching artifacts is Planned (M2))")
	f.Output(&c.output, outputText, outputText, outputJSON)
}

// Run validates the Bundle and prints its diagnostics as the data.
func (c *validateCommand) Run(ctx context.Context, sio command.IO, args []string) error {
	if err := c.bundle.check(); err != nil {
		return err
	}
	inv, err := start(sio, c.opts)
	if err != nil {
		return err
	}
	ds, err := c.diagnostics(ctx, inv, args[0])
	if err != nil {
		return err
	}
	var out []byte
	if c.output == outputJSON {
		if out, err = diagnosticsJSON(ds); err != nil {
			return command.NoResult(err)
		}
	} else {
		out = diagnosticLines(ds, "", inv.colors(sio.Stdout))
	}
	if err := inv.writeStdout(out); err != nil {
		return err
	}
	if ds.HasErrors() {
		return command.Negative()
	}
	return nil
}

// diagnostics runs the Bundle that arg names: once per Environment of the
// --environments file without --env, the shared findings first and then
// each Environment's in file order, else once.
func (c *validateCommand) diagnostics(ctx context.Context, inv *invocation, arg string) (diag.List, error) {
	if c.bundle.hasEnvironments() {
		if _, err := inv.readEnvironments(ctx, c.bundle.environments); err != nil {
			return nil, err
		}
	}
	src, err := inv.source(arg)
	if err != nil {
		return nil, err
	}
	if c.bundle.hasEnvironments() && c.bundle.env == "" {
		res, err := inv.p.RunEach(ctx, pipeline.EachRequest{Source: src, Environments: c.bundle.envSource(), Online: c.online})
		if err != nil {
			return nil, sourceError(arg, err)
		}
		out := append(diag.List(nil), res.Shared...)
		for _, run := range res.Runs {
			out = append(out, run.Diagnostics...)
		}
		return out, nil
	}
	req := inv.request(src, &c.bundle)
	req.Online = c.online
	res, err := inv.p.Run(ctx, req)
	if err != nil {
		return nil, sourceError(arg, err)
	}
	return res.Diagnostics, nil
}
