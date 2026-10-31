// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"context"
	"encoding/json"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// buildHelp is the paragraph of ruralz bundle build --help.
const buildHelp = `Builds the Revision of one Environment and prints rev-<12 hex> and its full
digest. The online Plugin check runs unless --offline is set; fetching Plugin
artifacts is Planned (M2), so a Bundle that holds a Plugin needs --offline.
--output-file receives the ruralz.canonical.v1 content, which hashes to the
digest.`

// BuildSpec returns the ruralz bundle build command.
func BuildSpec(o Options) command.Spec {
	return command.Spec{
		Path:    []string{"bundle", "build"},
		Summary: "Build a Revision, print its digest",
		Help:    buildHelp,
		Args:    []command.Arg{dirArg()},
		New:     func() command.Command { return &buildCommand{opts: o} },
	}
}

// buildCommand is one run of ruralz bundle build.
type buildCommand struct {
	opts       Options
	bundle     bundleFlags
	offline    bool
	outputFile string
	output     string
}

// Flags binds the Bundle flags, --offline, --output-file and --output text
// or json.
func (c *buildCommand) Flags(f *command.Flags) {
	c.bundle.bind(f)
	f.Bool(&c.offline, "offline", "Skip the online Plugin check")
	f.OutputFile(&c.outputFile, "Write the canonical content (ruralz.canonical.v1) to PATH")
	f.Output(&c.output, outputText, outputText, outputJSON)
}

// buildResult is the JSON output of ruralz bundle build: a failed build
// leaves out digest and revision.
type buildResult struct {
	Digest      string          `json:"digest,omitempty"`
	Revision    string          `json:"revision,omitempty"`
	Environment string          `json:"environment,omitempty"`
	Diagnostics json.RawMessage `json:"diagnostics"`
}

// Run builds the Revision.
func (c *buildCommand) Run(ctx context.Context, sio command.IO, args []string) error {
	if err := c.bundle.check(); err != nil {
		return err
	}
	if err := c.bundle.oneEnvironment("build"); err != nil {
		return err
	}
	inv, err := start(sio, c.opts)
	if err != nil {
		return err
	}
	if c.bundle.hasEnvironments() {
		if _, err := inv.readEnvironments(ctx, c.bundle.environments); err != nil {
			return err
		}
	}
	src, err := inv.source(args[0])
	if err != nil {
		return err
	}
	req := inv.request(src, &c.bundle)
	req.Online = !c.offline
	res, err := inv.p.Run(ctx, req)
	if err != nil {
		return sourceError(args[0], err)
	}
	inv.stderrDiagnostics(res.Diagnostics, "")
	// A build is the Revision of a run without error: the online stage,
	// which follows the digest, fails it as any other stage does.
	ok := res.Validated != nil && !res.HasErrors()
	if ok && c.outputFile != "" {
		// The exact canonical bytes, which hash to the digest.
		if err := command.WriteData(sio, c.outputFile, res.Validated.Revision.Content); err != nil {
			return err
		}
	}
	if c.output == outputJSON {
		out := buildResult{Environment: c.bundle.env}
		if out.Diagnostics, err = diagnosticsJSON(res.Diagnostics); err != nil {
			return command.NoResult(err)
		}
		if ok {
			d := res.Validated.Revision.Digest
			out.Digest, out.Revision = d.String(), d.Short()
		}
		if err := inv.writeJSON(out); err != nil {
			return err
		}
	} else if ok {
		d := res.Validated.Revision.Digest
		if err := inv.writeStdout([]byte(d.Short() + " " + d.String() + "\n")); err != nil {
			return err
		}
	}
	if !ok {
		return command.Negative()
	}
	return nil
}
