// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/openapi"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/render"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
)

// importFormatOpenAPI is the one format of ruralz bundle import FORMAT.
const importFormatOpenAPI = "openapi"

// importHelp is the paragraph of ruralz bundle import --help.
const importHelp = `Reads an OpenAPI 3.0 or 3.1 document, YAML or JSON, with the files its
relative $ref references name, and writes a Bundle: a Gateway with one http
listener on port 8080, one Upstream per server the operations use and one
Route per operation. The Bundle goes to --output-dir DIR, which must not exist
or must be empty, or as a one-file Bundle to standard output. An operation
that cannot be mapped is left out with a warning on standard error. Exits 2
when the document cannot be read or is no OpenAPI 3.0 or 3.1 document.`

// ImportSpec returns the ruralz bundle import command.
func ImportSpec(o Options) command.Spec {
	return command.Spec{
		Path:    []string{"bundle", "import"},
		Summary: "Routes and Upstreams from OpenAPI 3.x",
		Help:    importHelp,
		Args: []command.Arg{
			{
				Name: "FORMAT", Usage: "Document format",
				Values: []string{importFormatOpenAPI}, Complete: command.CompleteValues,
			},
			{Name: "FILE", Usage: "OpenAPI document, YAML or JSON", Complete: command.CompleteFile},
		},
		New: func() command.Command { return &importCommand{opts: o} },
	}
}

// importCommand is one run of ruralz bundle import.
type importCommand struct {
	opts      Options
	f         *command.Flags
	outputDir string
}

// Flags binds --output-dir.
func (c *importCommand) Flags(f *command.Flags) {
	c.f = f
	f.String(&c.outputDir, "output-dir", "DIR", "",
		"Write the Bundle as a directory to DIR, which must not exist or must be empty", command.CompleteDir)
}

// Run imports the document that args[1] names.
func (c *importCommand) Run(ctx context.Context, sio command.IO, args []string) error {
	if c.f.IsSet("output-dir") && c.outputDir == "" {
		return command.Usagef("--output-dir needs a directory")
	}
	inv, err := start(sio, c.opts)
	if err != nil {
		return err
	}
	file := args[1]
	doc, err := openapi.Load(ctx, file, openapi.Options{})
	if err != nil {
		return importLoadError(sio, file, err)
	}
	b := openapi.Import(doc)
	for _, w := range b.Warnings {
		_, _ = sio.Stderr.Write([]byte("warning: " + w.String() + "\n"))
	}
	stream, err := b.Stream()
	if err != nil {
		return command.NoResult(err)
	}
	if err := importCheck(ctx, inv, stream); err != nil {
		return err
	}
	if c.outputDir == "" {
		return command.WriteData(sio, "", stream)
	}
	files, err := b.Files()
	if err != nil {
		return command.NoResult(err)
	}
	if err := render.WriteTree(c.outputDir, files, render.TreeOptions{}); err != nil {
		return command.NoResultf("--output-dir %s: %s", c.outputDir, strings.TrimPrefix(err.Error(), "render: "))
	}
	return nil
}

// importLoadError prints the problems of a document that did not load,
// one line each on standard error, and returns the exit 2 of a document
// that cannot be read.
func importLoadError(sio command.IO, file string, err error) error {
	var le *openapi.Error
	if !errors.As(err, &le) {
		var re *openapi.ReadError
		if errors.As(err, &re) {
			return command.NoResultf("%s", re.Error())
		}
		return err
	}
	for _, p := range le.Problems {
		_, _ = sio.Stderr.Write([]byte(p.String() + "\n"))
	}
	return command.NoResultf("%s is no readable OpenAPI 3.0 or 3.1 document", filepath.ToSlash(file))
}

// importedName is the file name the imported one-file Bundle's
// diagnostics name, should it not validate.
const importedName = "imported.yaml"

// importCheck runs the imported Bundle through every offline stage of the
// pipeline, as ruralz bundle validate would: an import writes only a
// Bundle without error. Its warnings print on standard error; an error,
// which a mapping rule failed to prevent, prints the diagnostics and
// exits 2 without writing anything.
func importCheck(ctx context.Context, inv *invocation, stream []byte) error {
	res, err := inv.p.Run(ctx, pipeline.Request{
		Source:    loader.Source{FS: oneFile{name: importedName, data: stream}, Path: importedName},
		Variables: subst.Map{},
	})
	if err != nil {
		return command.NoResult(err)
	}
	inv.stderrDiagnostics(res.Diagnostics, "")
	if res.HasErrors() || res.Validated == nil {
		return command.NoResultf("the imported Bundle does not validate (%s)", res.Diagnostics.FirstErrorCode())
	}
	return nil
}
