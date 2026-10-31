// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/diff"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/render"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// convert is ruralz bundle render --api-version VERSION --output-dir DIR
// (docs/reference/01-cli-and-api-surface.md "Bundle verbs in detail"): it
// rewrites every base and overlay file of the source Bundle under VERSION
// into DIR at its own path and in its own format, unmerged and
// unsubstituted, then builds both trees offline for every Environment of
// --environments, or only for --env, and compares their Revisions.
//
// An unserved VERSION is RZ-CFG-007 and exits 2; a source document whose
// apiVersion is not served is RZ-CFG-007 and exits 1, and so does any
// other finding of stages A to C, which leaves a file it cannot rewrite
// whole; neither writes anything. DIR must not exist or must be empty, and
// must lie outside the source Bundle; the files are written into a hidden
// directory beside DIR and renamed into place (render.WriteTree), so DIR
// holds all of them or none. A one-file Bundle is written to DIR under its
// own file name. The command exits 1 unless every Environment's Revisions
// are equal, which is exactly when every diff is empty.
func (c *renderCommand) convert(ctx context.Context, inv *invocation, arg string) error {
	conv := inv.p.Converters()
	if d, bad := conv.CheckServed(c.apiVersion); bad {
		// The hint names the served apiVersions.
		d.Message = "--api-version " + quoted(c.apiVersion) + " is not served by this release"
		d.Path = nil
		inv.stderrDiagnostics(diag.List{d}, "")
		return command.Exit(command.ExitNoResult)
	}
	envs, err := inv.readEnvironments(ctx, c.bundle.environments)
	if err != nil {
		return err
	}
	order := envs.FileOrder()
	if len(order) == 0 {
		// Every Environment of the file failed its checks; the file's
		// findings say why.
		return c.failed(inv, envs.Diagnostics())
	}
	if c.bundle.env != "" {
		order = []string{c.bundle.env}
	}
	src := loader.Source{Path: arg}
	files, oneFile, ds, err := c.sourceFiles(ctx, inv, src, arg)
	if err != nil {
		return err
	}
	if ds.HasErrors() {
		return c.failed(inv, ds)
	}
	// Warnings of stages A to C print with the builds' findings below.
	out, ds, err := render.Convert(ctx, files, c.apiVersion, conv)
	if err != nil {
		return renderError(err)
	}
	if ds.HasErrors() {
		return c.failed(inv, ds)
	}
	inv.stderrDiagnostics(ds, "")
	var root string
	if !oneFile {
		root = arg
	}
	if err := render.WriteTree(c.outputDir, out, render.TreeOptions{SourceRoot: root}); err != nil {
		return command.NoResultf("--output-dir %s: %s", c.outputDir, strings.TrimPrefix(err.Error(), "render: "))
	}
	converted := loader.Source{Path: c.outputDir}
	if oneFile {
		if len(out) != 1 {
			return command.NoResultf("convert %s: a one-file Bundle converted to %d files", arg, len(out))
		}
		converted = loader.Source{Path: filepath.Join(c.outputDir, filepath.FromSlash(out[0].Path))}
	}
	return c.checkConversion(ctx, inv, order, src, converted, arg)
}

// renderError maps an error of the render package to exit 2 without the
// package's prefix; ctx's errors pass, exiting by command.Code.
func renderError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return command.NoResultf("%s", strings.TrimPrefix(err.Error(), "render: "))
}

// sourceFiles returns every base and overlay file of the source Bundle as
// stage B parses it, with the findings of stages A to C; oneFile reports a
// one-file Bundle, whose file is named by its base name.
func (c *renderCommand) sourceFiles(ctx context.Context, inv *invocation, src loader.Source, arg string) (files []render.SourceFile, oneFile bool, ds diag.List, err error) {
	b, err := inv.p.Parse(ctx, src)
	if err != nil {
		return nil, false, nil, sourceError(arg, err)
	}
	ds = append(ds, b.Diagnostics()...)
	if b.Stopped() {
		return nil, false, ds, nil
	}
	fi, err := os.Stat(arg)
	if err != nil {
		return nil, false, nil, command.NoResultf("cannot read %s: %w", arg, err)
	}
	oneFile = !fi.IsDir()
	for _, f := range b.Files() {
		rs, fds, err := b.ParseFile(ctx, f.Path)
		if err != nil {
			return nil, false, nil, command.NoResult(err)
		}
		ds = append(ds, fds...)
		sf := render.SourceFile{Path: f.Path}
		if oneFile {
			sf.Path = path.Base(f.Path)
		}
		for _, r := range rs {
			sf.Documents = append(sf.Documents, r.Root)
		}
		files = append(files, sf)
	}
	return files, oneFile, dedupe(ds), nil
}

// dedupe returns ds without the findings ParseFile repeats from the
// Bundle's own stages B and C, sorted.
func dedupe(ds diag.List) diag.List {
	seen := make(map[string]bool, len(ds))
	out := make(diag.List, 0, len(ds))
	for _, d := range ds {
		k := string(d.AppendText(nil))
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, d)
	}
	out.Sort()
	return out
}

// conversionBuild builds one tree for each Environment it is called with,
// offline, as ruralz bundle build --env does, and keeps each run, whose
// state a diff of differing Revisions compares.
type conversionBuild struct {
	p    *pipeline.Pipeline
	src  loader.Source
	envs loader.Source
	runs map[string]*pipeline.Result
}

// build is the render.BuildFunc of the tree.
func (b *conversionBuild) build(ctx context.Context, env string) (revision.Digest, diag.List, error) {
	res, err := b.p.Run(ctx, pipeline.Request{Source: b.src, Environments: b.envs, Environment: env})
	if err != nil {
		return revision.Digest{}, nil, err
	}
	b.runs[env] = res
	if res.Validated == nil {
		return revision.Digest{}, res.Diagnostics, nil
	}
	return res.Validated.Revision.Digest, res.Diagnostics, nil
}

// conversionResult is the JSON result of one Environment of the
// conversion check.
type conversionResult struct {
	Environment string          `json:"environment"`
	Source      treeResult      `json:"source"`
	Converted   treeResult      `json:"converted"`
	Equal       bool            `json:"equal"`
	Diff        json.RawMessage `json:"diff,omitempty"`
}

// treeResult is one tree's Revision, or the diagnostics of a tree that
// failed to build in place of it.
type treeResult struct {
	Revision    string          `json:"revision,omitempty"`
	Digest      string          `json:"digest,omitempty"`
	Diagnostics json.RawMessage `json:"diagnostics,omitempty"`
}

// checkConversion builds the source and the converted tree for each
// Environment of order and prints one line per Environment, "<env>: <source
// Revision> -> <converted Revision> equal", ending in differs when the
// Revisions differ or invalid when a tree fails to build, with "-" for the
// Revision it lacks and its diagnostics on standard error, prefixed
// "source: " or "converted: "; then the human diff of each Environment
// whose Revisions differ. With --output json it prints one array of
// conversionResult. It exits 1 unless every Revision is equal.
func (c *renderCommand) checkConversion(ctx context.Context, inv *invocation, order []string, src, converted loader.Source, arg string) error {
	envs := c.bundle.envSource()
	from := &conversionBuild{p: inv.p, src: src, envs: envs, runs: map[string]*pipeline.Result{}}
	to := &conversionBuild{p: inv.p, src: converted, envs: envs, runs: map[string]*pipeline.Result{}}
	cs, err := render.VerifyConversion(ctx, order, from.build, to.build)
	if err != nil {
		return renderError(err)
	}
	engine, err := diff.New(inv.p.HubIndex(), diff.Options{Workers: runtime.GOMAXPROCS(0), Registry: inv.p.Registry()})
	if err != nil {
		return command.NoResult(err)
	}
	color := c.outputFile == "" && inv.colors(inv.sio.Stdout)
	var lines, diffs []byte
	results := make([]conversionResult, 0, len(cs))
	for _, cv := range cs {
		env := cv.Environment
		inv.stderrDiagnostics(named(cv.Source.Diagnostics, env), "source: ")
		inv.stderrDiagnostics(named(cv.Converted.Diagnostics, env), "converted: ")
		r := conversionResult{Environment: env, Equal: cv.Equal()}
		if r.Source, err = outcomeJSON(cv.Source, env); err != nil {
			return command.NoResult(err)
		}
		if r.Converted, err = outcomeJSON(cv.Converted, env); err != nil {
			return command.NoResult(err)
		}
		verdict := "equal"
		switch {
		case !built(cv.Source) || !built(cv.Converted):
			verdict = "invalid"
		case !cv.Equal():
			verdict = "differs"
			d, err := engine.Compare(ctx,
				diff.Side{State: from.runs[env].Validated, Origin: diff.OriginBundle, Location: arg, Environment: env},
				diff.Side{State: to.runs[env].Validated, Origin: diff.OriginBundle, Location: converted.Path, Environment: env})
			if err != nil {
				return command.NoResult(err)
			}
			r.Diff = d.AppendJSON(nil)
			text := d.AppendText(nil)
			if color {
				text = colorDiff(text, d)
			}
			diffs = append(append(diffs, '\n'), text...)
		}
		lines = append(lines, env+": "+shortOf(cv.Source)+" -> "+shortOf(cv.Converted)+" "+verdict+"\n"...)
		results = append(results, r)
	}
	// The text report is the lines, then the diffs.
	report := lines
	report = append(report, diffs...)
	if c.output == outputJSON {
		var buf bytes.Buffer
		if err := command.WriteJSON(&buf, results); err != nil {
			return command.NoResult(err)
		}
		report = buf.Bytes()
	}
	if err := command.WriteData(inv.sio, c.outputFile, report); err != nil {
		return err
	}
	if !render.AllEqual(cs) {
		return command.Negative()
	}
	return nil
}

// built reports whether a tree built: a digest and no error.
func built(o render.Outcome) bool { return !o.Digest.IsZero() && !o.Diagnostics.HasErrors() }

// shortOf is the display form of a tree's Revision, "-" for a tree that
// failed to build.
func shortOf(o render.Outcome) string {
	if !built(o) {
		return "-"
	}
	return o.Digest.Short()
}

// outcomeJSON is the JSON of one tree: its Revision with the full digest,
// or the diagnostics of a tree that failed to build in place of it.
func outcomeJSON(o render.Outcome, env string) (treeResult, error) {
	if built(o) {
		return treeResult{Revision: o.Digest.Short(), Digest: o.Digest.String()}, nil
	}
	arr, err := diagnosticsJSON(named(o.Diagnostics, env))
	if err != nil {
		return treeResult{}, err
	}
	return treeResult{Diagnostics: arr}, nil
}

// named returns a copy of ds whose diagnostics name the Environment env,
// as the findings of one Environment of a multi-Environment run do.
func named(ds diag.List, env string) diag.List {
	out := append(diag.List(nil), ds...)
	for i := range out {
		out[i].Environment = env
	}
	return out
}

// maxQuoted is the most code points of an authored value a message quotes
// (docs/architecture/02-configuration-model.md "Semantic validation").
const maxQuoted = 64

// quoted returns s as a Go string literal of at most maxQuoted of its code
// points, "..." marking a longer value.
func quoted(s string) string {
	if r := []rune(s); len(r) > maxQuoted {
		s = string(r[:maxQuoted]) + "..."
	}
	return strconv.Quote(s)
}
