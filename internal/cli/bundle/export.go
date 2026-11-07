// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"context"
	"errors"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/export"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Formats of ruralz bundle export FORMAT.
const (
	exportOpenAPI = "openapi"
	exportPostman = "postman"
	exportDOT     = "dot"
)

// exportHelp is the paragraph of ruralz bundle export --help.
const exportHelp = `Exports one Environment's rendered Bundle: openapi writes an OpenAPI 3.1
document of its HTTP Routes as YAML, or with --output json as JSON; postman a
Postman collection v2.1 of the same operations; dot a Graphviz DOT graph of its
Routes, Policies and Upstreams, or with --route of one Route. A Route or method
without an OpenAPI operation is left out with a warning on standard error.
Exits 1 when the Bundle is invalid.`

// ExportSpec returns the ruralz bundle export command.
func ExportSpec(o Options) command.Spec {
	return command.Spec{
		Path:    []string{"bundle", "export"},
		Summary: "OpenAPI 3.x, Postman collection or DOT graph of a Bundle",
		Help:    exportHelp,
		Args: []command.Arg{
			{
				Name: "FORMAT", Usage: "Document format",
				Values: []string{exportOpenAPI, exportPostman, exportDOT}, Complete: command.CompleteValues,
			},
			{
				Name: "DIR", Usage: "Bundle directory or one-file Bundle",
				Optional: true, Default: ".", Complete: command.CompleteFile,
			},
		},
		New: func() command.Command { return &exportCommand{opts: o} },
	}
}

// exportCommand is one run of ruralz bundle export.
type exportCommand struct {
	opts       Options
	f          *command.Flags
	bundle     bundleFlags
	route      string
	outputFile string
	output     string
}

// Flags binds the Bundle flags, --route, --output-file and --output yaml
// or json.
func (c *exportCommand) Flags(f *command.Flags) {
	c.f = f
	c.bundle.bind(f)
	f.String(&c.route, "route", "NAME", "", "The Route of export dot", command.CompleteNone)
	f.OutputFile(&c.outputFile, "Write the document to PATH instead of standard output")
	f.Enum(&c.output, "output", outputYAML, []string{outputYAML, outputJSON},
		"Format of export openapi; postman always writes JSON and dot DOT text")
}

// Run exports the Bundle that args[1] names in the format args[0] names.
func (c *exportCommand) Run(ctx context.Context, sio command.IO, args []string) error {
	format := args[0]
	if err := c.exportCheck(format); err != nil {
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
	res, err := inv.p.Run(ctx, inv.request(loader.Source{Path: args[1]}, &c.bundle))
	if err != nil {
		return sourceError(args[1], err)
	}
	// Errors and warnings alike print on standard error, as for render and
	// build; an invalid Bundle has nothing to export.
	inv.stderrDiagnostics(res.Diagnostics, "")
	if res.HasErrors() || res.Validated == nil {
		return command.Negative()
	}
	var data []byte
	if format == exportDOT {
		if data, err = export.DOT(res.Validated, c.route); err != nil {
			if errors.Is(err, export.ErrUnknownRoute) {
				return command.NoResultf("%s", err)
			}
			return command.NoResult(err)
		}
	} else {
		ops, skips := export.Operations(res.Validated)
		for _, s := range skips {
			_, _ = sio.Stderr.Write([]byte("warning: Route/" + s.Route + ": " + s.Reason + "\n"))
		}
		if data, err = c.exportDocument(format, res.Validated, ops); err != nil {
			return err
		}
	}
	return command.WriteData(sio, c.outputFile, data)
}

// exportCheck refuses the flag combinations ruralz bundle export does not
// take: --route needs FORMAT dot and a name; --output applies to openapi,
// and to postman only as json; one Environment, as for every Bundle
// reader that renders one.
func (c *exportCommand) exportCheck(format string) error {
	if err := c.bundle.check(); err != nil {
		return err
	}
	switch {
	case c.f.IsSet("route") && format != exportDOT:
		return command.Usagef("--route applies to export dot only")
	case c.f.IsSet("route") && c.route == "":
		return command.Usagef("--route needs a Route name")
	case c.f.IsSet("output") && format == exportDOT:
		return command.Usagef("export dot writes DOT text; --output does not apply")
	case c.f.IsSet("output") && format == exportPostman && c.output != outputJSON:
		return command.Usagef("export postman writes JSON")
	}
	return c.bundle.oneEnvironment("export")
}

// exportDocument returns the OpenAPI document of ops, as YAML or with
// --output json as JSON, or their Postman collection.
func (c *exportCommand) exportDocument(format string, v *hub.Validated, ops []export.Operation) ([]byte, error) {
	if format == exportPostman {
		data, err := export.Postman(v, ops)
		if err != nil {
			return nil, command.NoResult(err)
		}
		return data, nil
	}
	doc := export.OpenAPI(v, ops)
	var buf bytes.Buffer
	opts := profile.EncodeOptions{Verbatim: true}
	var err error
	if c.output == outputJSON {
		err = profile.EncodeJSON(&buf, doc, opts)
	} else {
		err = profile.EncodeWith(&buf, []*tree.Node{doc}, opts)
	}
	if err != nil {
		return nil, command.NoResultf("encode the OpenAPI document: %w", err)
	}
	return buf.Bytes(), nil
}
