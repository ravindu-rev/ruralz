// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/precedence"
	"github.com/ravindu-rev/ruralz/internal/config/render"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// renderHelp is the paragraph of ruralz bundle render --help.
const renderHelp = `Writes the rendered Bundle of one Environment, a one-file Bundle in canonical
order with the overlay merged and ${VAR} substituted, as YAML or with --output
json as one JSON array. --effective prints the effective Filter Chains of every
Route, or with --route of one; --api-version VERSION --output-dir DIR converts
the source files to VERSION and checks that every Environment of
--environments keeps its Revision.`

// RenderSpec returns the ruralz bundle render command.
func RenderSpec(o Options) command.Spec {
	return command.Spec{
		Path:    []string{"bundle", "render"},
		Summary: "Render one Environment's one-file Bundle",
		Help:    renderHelp,
		Args:    []command.Arg{dirArg()},
		New:     func() command.Command { return &renderCommand{opts: o} },
	}
}

// renderCommand is one run of ruralz bundle render.
type renderCommand struct {
	opts       Options
	f          *command.Flags
	bundle     bundleFlags
	effective  bool
	route      string
	apiVersion string
	outputDir  string
	outputFile string
	output     string
}

// Flags binds the Bundle flags, --effective, --route, --api-version,
// --output-dir, --output-file and --output: yaml or json, and text or json
// with --effective or --api-version.
func (c *renderCommand) Flags(f *command.Flags) {
	c.f = f
	c.bundle.bind(f)
	f.Bool(&c.effective, "effective", "Print the effective Filter Chains of every Route, or of --route")
	f.String(&c.route, "route", "NAME", "", "The Route of --effective", command.CompleteNone)
	f.String(&c.apiVersion, "api-version", "VERSION", "",
		"Convert the source files to VERSION, such as ruralz/v1alpha1 (needs --output-dir and --environments)", command.CompleteNone)
	f.String(&c.outputDir, "output-dir", "DIR", "", "Directory of the converted files (needs --api-version)", command.CompleteDir)
	f.OutputFile(&c.outputFile, "Write the data to PATH instead of standard output")
	f.Output(&c.output, outputYAML, outputYAML, outputJSON)
	f.EnumMode("output", []string{"effective", "api-version"}, outputText, outputText, outputJSON)
}

// Run renders the Bundle, prints its effective Filter Chains or converts
// its sources.
func (c *renderCommand) Run(ctx context.Context, sio command.IO, args []string) error {
	if err := c.checkFlags(args[0]); err != nil {
		return err
	}
	inv, err := start(sio, c.opts)
	if err != nil {
		return err
	}
	if c.f.IsSet("api-version") {
		return c.convert(ctx, inv, args[0])
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
	req.Render = !c.effective
	res, err := inv.p.Run(ctx, req)
	if err != nil {
		return sourceError(args[0], err)
	}
	if res.HasErrors() || res.Validated == nil {
		return c.failed(inv, res.Diagnostics)
	}
	var data []byte
	if c.effective {
		inv.stderrDiagnostics(res.Diagnostics, "")
		if data, err = c.effectiveTable(inv, res); err != nil {
			return err
		}
	} else {
		var buf bytes.Buffer
		format := render.FormatYAML
		if c.output == outputJSON {
			format = render.FormatJSON
		}
		refused, err := inv.p.WriteRendered(ctx, &buf, res, format)
		if err != nil {
			return command.NoResult(err)
		}
		if len(refused) > 0 {
			return c.failed(inv, append(append(diag.List(nil), res.Diagnostics...), refused...))
		}
		inv.stderrDiagnostics(res.Diagnostics, "")
		data = buf.Bytes()
	}
	return command.WriteData(sio, c.outputFile, data)
}

// checkFlags refuses the flag combinations ruralz bundle render does not
// take (docs/reference/01-cli-and-api-surface.md "Bundle verbs in
// detail"): --api-version excludes --effective and --route, needs
// --output-dir and --environments, and reads no standard input; --route
// needs --effective; --output-dir needs --api-version; text output needs
// --effective or --api-version; --environments needs --env except with
// --api-version.
func (c *renderCommand) checkFlags(arg string) error {
	if err := c.bundle.check(); err != nil {
		return err
	}
	convert := c.f.IsSet("api-version")
	switch {
	case convert && (c.effective || c.f.IsSet("route")):
		return command.Usagef("--api-version excludes --effective and --route")
	case c.f.IsSet("route") && !c.effective:
		return command.Usagef("--route needs --effective")
	case c.f.IsSet("output-dir") && !convert:
		return command.Usagef("--output-dir needs --api-version")
	case convert && !c.f.IsSet("output-dir"):
		return command.Usagef("--api-version needs --output-dir")
	case convert && !c.bundle.hasEnvironments():
		return command.Usagef("--api-version needs --environments: it checks the conversion for every Environment of the file")
	case convert && arg == stdinName:
		return command.Usagef("--api-version converts the files of a Bundle and reads no standard input")
	case !convert && !c.effective && c.output == outputText:
		// --effective=false set the text mode of --output.
		if c.f.IsSet("output") {
			return command.Usagef("--output text needs --effective or --api-version")
		}
		c.output = outputYAML
	}
	if convert {
		return nil
	}
	return c.bundle.oneEnvironment("render")
}

// failed prints the diagnostics of a render that fails on them: as text
// lines on standard error, and with --output json also as one array on
// standard output. It exits 1, as an invalid Bundle does.
func (c *renderCommand) failed(inv *invocation, ds diag.List) error {
	inv.stderrDiagnostics(ds, "")
	if c.output == outputJSON {
		arr, err := diagnosticsJSON(ds)
		if err != nil {
			return command.NoResult(err)
		}
		if err := inv.writeStdout(arr); err != nil {
			return err
		}
	}
	return command.Negative()
}

// effectiveTable returns the --effective output of res: the chain of
// --route, or of every Route in canonical order by name.
func (c *renderCommand) effectiveTable(inv *invocation, res *pipeline.Result) ([]byte, error) {
	format := render.FormatText
	if c.output == outputJSON {
		format = render.FormatJSON
	}
	var buf bytes.Buffer
	if c.f.IsSet("route") {
		rows, err := inv.p.Effective(res, c.route)
		if errors.Is(err, render.ErrUnknownRoute) {
			return nil, command.NoResultf("%s", strings.TrimPrefix(err.Error(), "render: "))
		}
		if err != nil {
			return nil, command.NoResult(err)
		}
		if err := render.WriteEffective(&buf, rows, format); err != nil {
			return nil, command.NoResult(err)
		}
		return buf.Bytes(), nil
	}
	var rows []routeRow
	for _, r := range res.Validated.Bundle.Resources() {
		if r.Kind != v1alpha1.KindRoute {
			continue
		}
		chain, err := inv.p.Effective(res, r.Name)
		if err != nil {
			return nil, command.NoResult(err)
		}
		for _, row := range chain {
			rows = append(rows, routeRow{Route: r.Name, Row: row})
		}
	}
	table, err := routesTable(rows, format)
	if err != nil {
		return nil, command.NoResult(err)
	}
	return table, nil
}

// routeRow is one row of the effective Filter Chains of every Route: the
// Route's name, then the row of its chain.
type routeRow struct {
	// Route is the Route's metadata.name.
	Route string `json:"route"`
	precedence.Row
}

// Layout of the --effective text table, as render.WriteEffective lays out
// the table of one Route: text/tabwriter padding each column to its widest
// cell plus two spaces, the last column not padded.
const (
	tableMinWidth = 0
	tableTabWidth = 8
	tablePadding  = 2
)

// routesTable returns rows in format f: render.FormatText is the table of
// render.WriteEffective led by a ROUTE column, the header alone for no
// rows; render.FormatJSON the rows as one JSON array whose objects lead
// with the member route, [] for no rows.
func routesTable(rows []routeRow, f render.Format) ([]byte, error) {
	var buf bytes.Buffer
	switch f {
	case render.FormatText:
		tw := tabwriter.NewWriter(&buf, tableMinWidth, tableTabWidth, tablePadding, ' ', 0)
		_, _ = io.WriteString(tw, "ROUTE\tPHASE\tLEG\tPOLICY\tFROM\tREASON\n")
		for _, r := range rows {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Route, r.Phase, r.Leg, r.Policy, r.From, r.Reason)
		}
		if err := tw.Flush(); err != nil {
			return nil, err
		}
	case render.FormatJSON:
		if err := command.WriteJSON(&buf, rows); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: %s for an effective table", render.ErrFormat, f)
	}
	return buf.Bytes(), nil
}
