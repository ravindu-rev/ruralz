// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cli/adminclient"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/diff"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// diffHelp is the paragraph of ruralz bundle diff --help.
const diffHelp = `FROM and TO are each a Bundle directory, a rendered Bundle file, a saved
ruralz node dump file, an admin URL such as https://node-a.example:9901 whose
/config/dump is read, or - for standard input (one side at most). Both sides run
through the configuration pipeline and compare as validated states. --from-env
and --to-env stand in for --env on one side. Exits 0 without changes, 1 with
changes and 2 for errors. Revision sources (rev-<12 hex>, sha256:<64 hex>,
oci://) are Planned (M2).`

// DiffSpec returns the ruralz bundle diff command.
func DiffSpec(o Options) command.Spec {
	return command.Spec{
		Path:    []string{"bundle", "diff"},
		Summary: "Compare FROM with TO",
		Help:    diffHelp,
		Args: []command.Arg{
			{Name: "FROM", Usage: "Bundle directory or file, saved dump, admin URL, or -", Complete: command.CompleteFile},
			{Name: "TO", Usage: "Same forms as FROM", Complete: command.CompleteFile},
		},
		New: func() command.Command { return &diffCommand{opts: o} },
	}
}

// diffCommand is one run of ruralz bundle diff.
type diffCommand struct {
	opts    Options
	f       *command.Flags
	bundle  bundleFlags
	fromEnv string
	toEnv   string
	admin   adminclient.Flags
	output  string
}

// Flags binds the Bundle flags, --from-env and --to-env, the admin flags
// with per-side credentials and --timeout, and --output text or json.
func (c *diffCommand) Flags(f *command.Flags) {
	c.f = f
	c.bundle.bind(f)
	f.String(&c.fromEnv, "from-env", "NAME", "", "Environment of FROM, in place of --env", command.CompleteNone)
	f.String(&c.toEnv, "to-env", "NAME", "", "Environment of TO, in place of --env", command.CompleteNone)
	c.admin.Bind(f, adminclient.FlagOptions{Timeout: true, Sides: []string{sideFrom, sideTo}})
	f.Output(&c.output, outputText, outputText, outputJSON)
}

// Sides of a diff: the JSON member side of their diagnostics and the
// prefix of their text lines.
const (
	sideFrom = "from"
	sideTo   = "to"
)

// sourceKind is what a FROM or TO names
// (docs/reference/01-cli-and-api-surface.md "Bundle verbs in detail").
type sourceKind uint8

const (
	// kindPath is a path: a directory, or a file that is a saved dump or a
	// rendered Bundle; "-" is standard input.
	kindPath sourceKind = iota
	// kindAdmin is an admin URL, starting with http:// or https://.
	kindAdmin
	// kindRevision is a bare rev-<12 hex> or sha256:<64 hex>.
	kindRevision
	// kindOCI is an OCI Revision, starting with oci://.
	kindOCI
)

// classify returns what arg names: an admin URL starts with http:// or
// https:// and an OCI Revision with oci://, the scheme in any case; a bare
// rev-<12 hex> or sha256:<64 hex> is a Revision; anything else is a path,
// so ./rev-162af81f5de4 names a directory.
func classify(arg string) sourceKind {
	lower := strings.ToLower(arg)
	switch {
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return kindAdmin
	case strings.HasPrefix(lower, "oci://"):
		return kindOCI
	}
	if _, err := revision.ParseDisplay(arg); err == nil {
		return kindRevision
	}
	if _, err := revision.Parse(arg); err == nil {
		return kindRevision
	}
	return kindPath
}

// side is one of the two states a diff compares.
type side struct {
	// name is sideFrom or sideTo; label is FROM or TO.
	name, label string
	// arg is FROM or TO as given.
	arg  string
	kind sourceKind
	// env is the side's Environment and envFlag the flag that named it,
	// --from-env, --to-env or --env; both empty without one.
	env, envFlag string
	// origin is what the side was read from, once known: a Bundle (a
	// directory or a rendered Bundle file), a saved dump or a Node.
	origin diff.Origin
	// src is the Bundle of a Bundle side.
	src loader.Source
	// client reads the /config/dump of an admin side.
	client *adminclient.Client
	// dump, rs and files are a decoded /config/dump document.
	dump  canonical.Dump
	rs    []tree.Resource
	files *tree.FileTable
	// res is the pipeline's run of the side, and diags its findings or the
	// findings of a dump that failed its check.
	res   *pipeline.Result
	diags diag.List
}

// failed reports a side without a validated state.
func (s *side) failed() bool {
	return s.res == nil || s.res.Validated == nil || s.diags.HasErrors()
}

// diffSide returns the side as the diff engine compares it.
func (s *side) diffSide() diff.Side {
	return diff.Side{State: s.res.Validated, Origin: s.origin, Location: s.arg, Environment: s.env}
}

// Run compares FROM with TO.
func (c *diffCommand) Run(ctx context.Context, sio command.IO, args []string) error {
	if err := c.checkFlags(); err != nil {
		return err
	}
	sides := []*side{c.newSide(sideFrom, "FROM", args[0]), c.newSide(sideTo, "TO", args[1])}
	if args[0] == stdinName && args[1] == stdinName {
		return command.Usagef("FROM and TO both read standard input; at most one side may be -")
	}
	for _, s := range sides {
		switch s.kind {
		case kindRevision:
			return command.NoResultf("%s %s: a Revision source is Planned (M2) and not in this build", s.label, s.arg)
		case kindOCI:
			return command.NoResultf("%s %s: an OCI Revision source is Planned (M2) and not in this build", s.label, s.arg)
		case kindAdmin:
			if s.envFlag != "" {
				return command.Usagef("%s names the Environment of a Bundle side, and %s is an admin URL", s.envFlag, s.label)
			}
		case kindPath:
		}
	}
	inv, err := start(sio, c.opts)
	if err != nil {
		return err
	}
	defer func() {
		for _, s := range sides {
			if s.client != nil {
				s.client.Close()
			}
		}
	}()
	for _, s := range sides {
		if s.kind != kindAdmin {
			continue
		}
		// New checks the URL and the credentials before anything is read.
		cfg := c.admin.NodeConfig(sio, s.name, s.label, s.arg)
		if s.client, err = adminclient.New(cfg, adminclient.Options{
			Stderr: sio.Stderr, Clock: c.opts.Clock, Rand: c.opts.Rand, Version: c.opts.Version,
		}); err != nil {
			return err
		}
	}
	for _, s := range sides {
		if s.kind == kindPath {
			if err := inv.readPathSide(ctx, s); err != nil {
				return err
			}
		}
	}
	if err := c.checkEnvironments(ctx, inv, sides); err != nil {
		return err
	}
	for _, s := range sides {
		if err := inv.runSide(ctx, s, c.bundle.envSource()); err != nil {
			return err
		}
	}
	return c.compare(ctx, inv, sides[0], sides[1])
}

// checkFlags refuses an empty Environment flag, an Environment flag
// without --environments, and --env beside --from-env or --to-env.
func (c *diffCommand) checkFlags() error {
	if err := c.bundle.check(); err != nil {
		return err
	}
	if c.f.IsSet("env") && (c.f.IsSet("from-env") || c.f.IsSet("to-env")) {
		return command.Usagef("--env names the Environment of both sides; give it or --from-env and --to-env, not both")
	}
	if err := c.bundle.checkEnvFlag("from-env", c.fromEnv); err != nil {
		return err
	}
	return c.bundle.checkEnvFlag("to-env", c.toEnv)
}

// newSide returns the side name of argument arg, with its Environment:
// its own flag's, else --env's.
func (c *diffCommand) newSide(name, label, arg string) *side {
	s := &side{name: name, label: label, arg: arg, kind: classify(arg)}
	own, value := "from-env", c.fromEnv
	if name == sideTo {
		own, value = "to-env", c.toEnv
	}
	switch {
	case c.f.IsSet(own):
		s.env, s.envFlag = value, "--"+own
	case c.f.IsSet("env"):
		s.env, s.envFlag = c.bundle.env, "--env"
	}
	return s
}

// checkEnvironments applies the Environment rules once every side is
// classified: an Environment flag on a dump side is a usage error, and
// with --environments every Bundle side needs an Environment, whose file
// must declare one.
func (c *diffCommand) checkEnvironments(ctx context.Context, inv *invocation, sides []*side) error {
	read := false
	for _, s := range sides {
		switch s.origin {
		case diff.OriginDump:
			if s.envFlag != "" {
				return command.Usagef("%s names the Environment of a Bundle side, and %s %s is a /config/dump document", s.envFlag, s.label, s.arg)
			}
		case diff.OriginBundle:
			if c.bundle.hasEnvironments() && s.env == "" {
				return command.Usagef("--environments needs an Environment for %s: give --env or --%s-env", s.label, s.name)
			}
			read = read || s.env != ""
		case diff.OriginNone, diff.OriginAdmin:
		}
	}
	if read {
		if _, err := inv.readEnvironments(ctx, c.bundle.environments); err != nil {
			return err
		}
	}
	return nil
}

// readPathSide reads a path side: a directory is a Bundle side; a file,
// or standard input for "-", is a dump side when it holds a /config/dump
// document and a Bundle side, a rendered Bundle, otherwise. A source that
// is missing, unreadable or neither a directory nor a regular file exits
// 2.
func (inv *invocation) readPathSide(ctx context.Context, s *side) error {
	limit := int64(adminclient.MaxConfigDumpBytes)
	if s.arg == stdinName {
		data, err := readLimited(inv.opts.stdin(), max(limit, inv.p.Limits().MaxSourceBytes))
		if err != nil {
			return command.NoResultf("%s: read standard input: %w", s.label, err)
		}
		if startsObject(data) {
			isDump, err := inv.decodeDump(ctx, s, data, stdinName, diff.OriginDump)
			if err != nil || isDump {
				return err
			}
		}
		s.origin, s.src = diff.OriginBundle, stdinSource(data)
		return nil
	}
	fi, err := os.Stat(s.arg)
	if err != nil {
		return command.NoResultf("cannot read %s: %w", s.label, err)
	}
	switch {
	case fi.IsDir():
		s.origin, s.src = diff.OriginBundle, loader.Source{Path: s.arg}
		return nil
	case !fi.Mode().IsRegular():
		return command.NoResultf("cannot read %s %s: neither a directory nor a regular file", s.label, s.arg)
	}
	data, err := readObjectFile(s.arg, limit)
	switch {
	case errors.Is(err, errNotObject):
	case err != nil:
		return command.NoResultf("cannot read %s: %w", s.label, err)
	default:
		// Named as the loader names a one-file Bundle: the path as given,
		// slash-separated.
		isDump, err := inv.decodeDump(ctx, s, data, filepath.ToSlash(s.arg), diff.OriginDump)
		if err != nil || isDump {
			return err
		}
	}
	s.origin, s.src = diff.OriginBundle, loader.Source{Path: s.arg}
	return nil
}

// decodeDump reads body as a /config/dump document named name in
// diagnostics, checking that its content hashes to its digest and decodes
// at the schema levels this CLI serves. It reports false for a body that
// is no dump document. A dump whose check fails is still a dump: its
// findings become the side's, which fail it.
func (inv *invocation) decodeDump(ctx context.Context, s *side, body []byte, name string, origin diff.Origin) (bool, error) {
	files := &tree.FileTable{}
	dump, rs, err := inv.p.Decoder().ReadDump(ctx, bytes.NewReader(body), adminclient.MaxConfigDumpBytes, canonical.Source{Name: name, Files: files})
	switch {
	case err == nil:
		s.origin, s.dump, s.rs, s.files = origin, dump, rs, files
		return true, nil
	case ctx.Err() != nil:
		return false, ctx.Err()
	case errors.Is(err, canonical.ErrNotDump), errors.Is(err, canonical.ErrDumpTooLarge):
		return false, nil
	}
	if _, coded := errcode.CodeOf(err); coded {
		s.origin, s.files, s.diags = origin, files, canonical.DiagnosticsOf(err)
		return true, nil
	}
	return false, command.NoResultf("%s %s: %w", s.label, s.arg, err)
}

// runSide runs the side through the pipeline: a Bundle side for its
// Environment, else with the process environment's ${VAR} values; a dump
// side, or a Node's /config/dump read first, re-enters stages F to M as
// canonical content (pipeline.FromResources).
func (inv *invocation) runSide(ctx context.Context, s *side, envs loader.Source) error {
	if s.kind == kindAdmin {
		body, err := s.client.ConfigDump(ctx)
		if err != nil {
			return err
		}
		where := strings.TrimSuffix(s.client.URL(), "/") + adminclient.PathConfigDump
		isDump, err := inv.decodeDump(ctx, s, body, where, diff.OriginAdmin)
		if err != nil {
			return err
		}
		if !isDump {
			return command.NoResultf("%s %s: GET %s answered with no /config/dump document", s.label, s.arg, where)
		}
	}
	if s.diags.HasErrors() {
		return nil
	}
	var res *pipeline.Result
	var err error
	if s.origin == diff.OriginBundle {
		req := pipeline.Request{Source: s.src}
		if s.env != "" {
			req.Environments, req.Environment = envs, s.env
		} else {
			req.Variables = inv.variables()
		}
		if res, err = inv.p.Run(ctx, req); err != nil {
			return sourceError(s.label+" "+s.arg, err)
		}
	} else {
		c := pipeline.Canonical{Resources: s.rs, Files: s.files, Content: s.dump.Content, Text: s.dump.Body}
		if res, err = inv.p.FromResources(ctx, c, false); err != nil {
			return err
		}
	}
	s.res, s.diags = res, res.Diagnostics
	return nil
}

// compare prints both sides' diagnostics on standard error, each line
// after its side's "from: " or "to: ", then the diff of two validated
// states: the human form, colored on a terminal, or ruralz.diff.v1. A side
// that fails on its diagnostics exits 2, with --output json printing them
// as one array on standard output, each entry naming its side.
func (c *diffCommand) compare(ctx context.Context, inv *invocation, from, to *side) error {
	inv.stderrDiagnostics(from.diags, sideFrom+": ")
	inv.stderrDiagnostics(to.diags, sideTo+": ")
	if from.failed() || to.failed() {
		if c.output == outputJSON {
			items, err := sideDiagnosticsJSON(sideDiagnostics{sideFrom, from.diags}, sideDiagnostics{sideTo, to.diags})
			if err != nil {
				return command.NoResult(err)
			}
			if err := inv.writeJSON(items); err != nil {
				return err
			}
		}
		return command.Exit(command.ExitNoResult)
	}
	engine, err := diff.New(inv.p.HubIndex(), diff.Options{Workers: runtime.GOMAXPROCS(0), Registry: inv.p.Registry()})
	if err != nil {
		return command.NoResult(err)
	}
	d, err := engine.Compare(ctx, from.diffSide(), to.diffSide())
	if err != nil {
		if _, coded := errcode.CodeOf(err); coded {
			return command.NoResult(err)
		}
		return fmt.Errorf("compare: %w", err)
	}
	var out []byte
	if c.output == outputJSON {
		out = d.AppendJSON(nil)
	} else {
		out = d.AppendText(nil)
		if inv.colors(inv.sio.Stdout) {
			out = colorDiff(out, d)
		}
	}
	if err := inv.writeStdout(out); err != nil {
		return err
	}
	if d.Changed() {
		return command.Negative()
	}
	return nil
}
