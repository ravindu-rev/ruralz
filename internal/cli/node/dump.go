// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cli/adminclient"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// dumpHelp is the paragraph of ruralz node dump --help.
const dumpHelp = `Reads the Node's /config/dump, checks that its content hashes to its digest
and decodes at the schema levels this CLI serves, then writes it exactly as
received, so the file is a ruralz bundle diff source. A document that fails
the check is not written.`

// DumpSpec returns the ruralz node dump command.
func DumpSpec(o Options) command.Spec {
	return command.Spec{
		Path:    []string{"node", "dump"},
		Summary: "Save /config/dump, secrets omitted",
		Help:    dumpHelp,
		New:     func() command.Command { return &dumpCommand{opts: o} },
	}
}

// dumpCommand is one run of ruralz node dump.
type dumpCommand struct {
	opts       Options
	admin      adminclient.Flags
	outputFile string
}

// Flags binds the admin flags with --admin and --timeout, and
// --output-file; the command takes no --output, since its output is the
// document.
func (c *dumpCommand) Flags(f *command.Flags) {
	c.admin.Bind(f, adminclient.FlagOptions{URL: true, Timeout: true})
	f.OutputFile(&c.outputFile, "Write the document to PATH instead of standard output")
}

// Run reads, checks and writes the Node's /config/dump.
func (c *dumpCommand) Run(ctx context.Context, sio command.IO, _ []string) error {
	client, err := adminclient.New(c.admin.Config(sio), adminclient.Options{
		Stderr: sio.Stderr, Clock: c.opts.Clock, Rand: c.opts.Rand, Version: c.opts.Version,
	})
	if err != nil {
		return err
	}
	defer client.Close()
	body, err := client.ConfigDump(ctx)
	if err != nil {
		return err
	}
	where := strings.TrimSuffix(client.URL(), "/") + adminclient.PathConfigDump
	dump, err := checkDump(ctx, body, where)
	if err != nil {
		return err
	}
	if err := command.WriteData(sio, c.outputFile, dump.Body); err != nil {
		return err
	}
	if c.outputFile != "" {
		_, _ = fmt.Fprintln(sio.Stderr, savedLine(dump))
	}
	return nil
}

// checkDump checks a /config/dump body before anything is written
// (docs/reference/01-cli-and-api-surface.md, "Rollout, promotion and Node
// verbs"): it must be a dump document whose revision names its digest,
// whose content hashes to that digest (RZ-CFG-027) and decodes at the
// schema levels this CLI serves (RZ-CFG-024), in ruralz.canonical.v1 form
// (RZ-CFG-027) and within the decoder's limits (RZ-CFG-001). where names
// the document, the URL it came from. Every failure exits 2; one that
// carries an RZ code prints the code and the first finding, the number of
// others after it.
func checkDump(ctx context.Context, body []byte, where string) (canonical.Dump, error) {
	op := http.MethodGet + " " + where
	set, err := schemaidx.Embedded()
	if err != nil {
		return canonical.Dump{}, command.NoResultf("%s: load the schema this CLI serves: %w", op, err)
	}
	idx, ok := set.Index(v1alpha1.APIVersion)
	if !ok {
		return canonical.Dump{}, command.NoResultf("%s: this CLI serves no %s schema", op, v1alpha1.APIVersion)
	}
	dec := canonical.NewDecoder(idx, canonical.DecoderOptions{Workers: runtime.GOMAXPROCS(0)})
	dump, _, err := dec.ReadDump(ctx, bytes.NewReader(body), adminclient.MaxConfigDumpBytes, canonical.Source{Name: where})
	switch {
	case err == nil:
		return dump, nil
	case ctx.Err() != nil:
		return canonical.Dump{}, fmt.Errorf("%s: %w", op, context.Cause(ctx))
	case errors.Is(err, canonical.ErrNotDump), errors.Is(err, canonical.ErrDumpTooLarge):
		return canonical.Dump{}, command.NoResultf("%s: %s", op, strings.TrimPrefix(err.Error(), "canonical: "))
	default:
		return canonical.Dump{}, command.NoResultf("%s: the document fails its check: %w", op, err)
	}
}

// savedLine is the line on standard error after --output-file received a
// dump: its Revision, full digest and Last-Known-Good digest.
func savedLine(d canonical.Dump) string {
	lkg := "no Last-Known-Good"
	if !d.LastKnownGood.IsZero() {
		lkg = "Last-Known-Good " + d.LastKnownGood.String()
	}
	return "saved " + d.Digest.Short() + " " + d.Digest.String() + ", " + lkg
}
