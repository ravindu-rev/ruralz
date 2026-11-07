// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"context"

	"github.com/ravindu-rev/ruralz/internal/cli/audit"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
)

// auditHelp is the paragraph of ruralz bundle audit --help.
const auditHelp = `Validates the Bundle as ruralz bundle validate does, then checks the rendered
Bundle for security and best-practice findings: Routes without authentication,
cleartext hops, broad Plugin Capabilities and Plugin pool sizing. Prints one
finding per line, a diagnostic's rule being its code, or with --output json one
JSON array. With --environments and no --env it runs once per Environment of
the file, in file order. Exits 1 when a finding is an error, 0 otherwise.`

// AuditSpec returns the ruralz bundle audit command.
func AuditSpec(o Options) command.Spec {
	return command.Spec{
		Path:    []string{"bundle", "audit"},
		Summary: "Security and best-practice findings",
		Help:    auditHelp,
		Args: []command.Arg{{
			Name: "DIR", Usage: "Bundle directory or one-file Bundle",
			Optional: true, Default: ".", Complete: command.CompleteFile,
		}},
		New: func() command.Command { return &auditCommand{opts: o} },
	}
}

// auditCommand is one run of ruralz bundle audit
// (docs/reference/01-cli-and-api-surface.md "Global flags", "Output
// formats and exit codes"): the run's diagnostics and the audit rules'
// findings (internal/cli/audit) on its rendered Bundle, per Environment
// of --environments without --env.
type auditCommand struct {
	opts   Options
	bundle bundleFlags
	output string
}

// Flags binds the Bundle flags and --output text or json.
func (c *auditCommand) Flags(f *command.Flags) {
	c.bundle.bind(f)
	f.Output(&c.output, outputText, outputText, outputJSON)
}

// Run audits the Bundle and prints its findings as the data. It exits 1
// when a finding is an error, an invalid Bundle's included, and 0
// otherwise: warnings never change the code.
func (c *auditCommand) Run(ctx context.Context, sio command.IO, args []string) error {
	if err := c.bundle.check(); err != nil {
		return err
	}
	inv, err := start(sio, c.opts)
	if err != nil {
		return err
	}
	fs, err := c.findings(ctx, inv, args[0])
	if err != nil {
		return err
	}
	var out []byte
	if c.output == outputJSON {
		var buf bytes.Buffer
		if err := audit.WriteJSON(&buf, fs); err != nil {
			return command.NoResult(err)
		}
		out = buf.Bytes()
	} else {
		ds := make(diag.List, 0, len(fs))
		for _, f := range fs {
			ds = append(ds, f.Diagnostic())
		}
		out = diagnosticLines(ds, "", inv.colors(sio.Stdout))
	}
	if err := inv.writeStdout(out); err != nil {
		return err
	}
	if audit.HasErrors(fs) {
		return command.Negative()
	}
	return nil
}

// findings runs the Bundle that arg names and returns its findings: once
// per Environment of the --environments file without --env, the findings
// of stages A to C first, naming no Environment, and then each
// Environment's in file order, naming it; else the one run's.
func (c *auditCommand) findings(ctx context.Context, inv *invocation, arg string) ([]audit.Finding, error) {
	if c.bundle.hasEnvironments() {
		if _, err := inv.readEnvironments(ctx, c.bundle.environments); err != nil {
			return nil, err
		}
	}
	src := loader.Source{Path: arg}
	if c.bundle.hasEnvironments() && c.bundle.env == "" {
		res, err := inv.p.RunEach(ctx, pipeline.EachRequest{Source: src, Environments: c.bundle.envSource()})
		if err != nil {
			return nil, sourceError(arg, err)
		}
		out := audit.FromDiagnostics(res.Shared)
		// An error of stages A to C makes the Bundle invalid for every
		// Environment, though no run repeats it: no rule runs then.
		valid := !res.Shared.HasErrors()
		for i, run := range res.Runs {
			out = append(out, auditRun(inv, run, res.Environments[i], valid)...)
		}
		return out, nil
	}
	res, err := inv.p.Run(ctx, inv.request(src, &c.bundle))
	if err != nil {
		return nil, sourceError(arg, err)
	}
	return auditRun(inv, res, "", true), nil
}

// auditRun returns the findings of one run: its diagnostics, in the
// pipeline's order, and, when the run and the findings it shares (valid)
// have no error, the audit rules' findings on its rendered Bundle merged
// into them in the same order (audit.Sort), each naming env, the
// Environment of a multi-Environment run ("" for none).
func auditRun(inv *invocation, res *pipeline.Result, env string, valid bool) []audit.Finding {
	out := audit.FromDiagnostics(res.Diagnostics)
	if !valid || res.Validated == nil || res.HasErrors() {
		return out
	}
	found := audit.Check(res.Validated, res.Files, inv.p.HubIndex())
	if len(found) == 0 {
		return out
	}
	for _, f := range found {
		f.Environment = env
		out = append(out, f)
	}
	audit.Sort(out)
	return out
}
