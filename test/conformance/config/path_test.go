// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/bundle"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
)

// The binaries a negative fixture's fixture.yaml names in binaries
// (docs/engineering/03-testing-and-quality-strategy.md "Conformance
// suites"), and the in-process paths alone, which a fixture reached
// through the stand-ins names instead.
const (
	binaryCLI     = "ruralz"
	binaryControl = "ruralz-control"
	binaryNode    = "ruralzd"
	// inProcessSuffix names a binary's in-process path alone, such as
	// ruralz-in-process: the stand-ins link into the suite, never into a
	// built binary.
	inProcessSuffix = "-in-process"
)

// knownBinaries returns every value binaries may hold.
func knownBinaries() []string {
	return []string{
		binaryCLI, binaryControl, binaryNode,
		binaryCLI + inProcessSuffix, binaryControl + inProcessSuffix, binaryNode + inProcessSuffix,
	}
}

// run is one run of a Bundle: what a binary is asked to do with it.
type run struct {
	// bundle is the Bundle: a directory rooted at ruralz.yaml or a
	// one-file Bundle, an OS path.
	bundle string
	// environments is the environments file, "" for none. With one, env
	// names the Environment to render, or "" renders every Environment in
	// file order, as ruralz bundle validate --environments does without
	// --env. Without one, the Bundle renders without --env over an empty
	// variable set: no overlay, and ${VAR} only with a default.
	environments, env string
	// standIns runs on the stand-in pipelines (standInCodes).
	standIns bool
}

// outcome is what a runner reports for a run: its diagnostics in the text
// and JSON forms of docs/architecture/02-configuration-model.md
// "Diagnostics and source map", the findings of the steps that follow the
// digest, which only a Node runs, and the Revision digest when the run
// reached one. No diagnostic leaves the text empty.
type outcome struct {
	text, json []byte
	// after are the findings of the Node's steps after the digest, one text
	// line each; they never compare with the CLI's.
	after []string
	// digest is the full sha256 digest, "" without a Revision.
	digest string
	// list holds the diagnostics of a run through the configuration
	// pipeline facade; nil for a command, whose outcome is only its output.
	list diag.List
}

// runner runs Bundles as one binary does, on its path. The in-process
// runners are cliPath and nodePath; the integration-tagged cases add the
// built binaries (binaryRunners).
type runner interface {
	// binary is the binary the runner stands for: ruralz, ruralz-control
	// or ruralzd.
	binary() string
	// inProcess reports a runner of the test process, the only kind the
	// stand-ins reach.
	inProcess() bool
	// validate runs r and reports the outcome; the error is a run that
	// produced no result, such as a source that cannot be read.
	validate(ctx context.Context, r run) (outcome, error)
}

// runs reports whether p runs a fixture naming binaries: it names p's
// binary, or, for an in-process runner, the binary's in-process path.
func runs(p runner, binaries []string) bool {
	return slices.Contains(binaries, p.binary()) || (p.inProcess() && slices.Contains(binaries, p.binary()+inProcessSuffix))
}

// runners returns every runner of this run of the suite: the in-process
// ones, then the built binaries of the integration-tagged cases.
func (s *suite) runners(t testing.TB) []runner {
	t.Helper()
	return append([]runner{cliPath{s}, nodePath{s}}, binaryRunners(t, s)...)
}

// loaderSource returns the loader source of an OS path.
func loaderSource(p string) loader.Source { return loader.Source{Path: p} }

// environmentsSource returns the loader source of an environments file,
// opened by its base name in its directory, so its findings name the file
// as a CLI run from that directory does (--environments
// environments.yaml), on every machine.
func environmentsSource(p string) loader.Source {
	return loader.Source{FS: os.DirFS(filepath.Dir(p)), Path: filepath.Base(p)}
}

// outcomeOf writes ds in both forms.
func outcomeOf(ds diag.List, digest string) (outcome, error) {
	var text, js bytes.Buffer
	if err := diag.WriteText(&text, ds); err != nil {
		return outcome{}, err
	}
	if err := diag.WriteJSON(&js, ds); err != nil {
		return outcome{}, err
	}
	return outcome{text: text.Bytes(), json: js.Bytes(), digest: digest, list: ds}, nil
}

// texts returns the text form of ds.
func texts(ds diag.List) string {
	var b strings.Builder
	_ = diag.WriteText(&b, ds)
	return b.String()
}

// facadeOutcome runs r through the configuration pipeline facade p as the
// CLI's commands do: with an environments file and no Environment every
// Environment once, the findings every Environment shares first and naming
// none, then each Environment's in file order, naming it; else one render.
func facadeOutcome(ctx context.Context, p *pipeline.Pipeline, r run) (outcome, error) {
	if r.environments != "" && r.env == "" {
		res, err := p.RunEach(ctx, pipeline.EachRequest{Source: loaderSource(r.bundle), Environments: environmentsSource(r.environments)})
		if err != nil {
			return outcome{}, err
		}
		ds := slices.Clone(res.Shared)
		for _, run := range res.Runs {
			ds = append(ds, run.Diagnostics...)
		}
		return outcomeOf(ds, "")
	}
	res, err := p.Run(ctx, requestOf(r))
	if err != nil {
		return outcome{}, err
	}
	digest := ""
	if res.Validated != nil {
		digest = res.Validated.Revision.Digest.String()
	}
	return outcomeOf(res.Diagnostics, digest)
}

// requestOf returns the pipeline request of a run of one render.
func requestOf(r run) pipeline.Request {
	req := pipeline.Request{Source: loaderSource(r.bundle)}
	if r.environments != "" {
		req.Environments, req.Environment = environmentsSource(r.environments), r.env
	} else {
		req.Variables = subst.Map{}
	}
	return req
}

// cliEpoch is the fixed time of the CLI's clock in process.
func cliEpoch() time.Time { return time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC) }

// cliPath is the CLI's path in process (docs/engineering/03-testing-and-
// quality-strategy.md "Conformance suites"): ruralz bundle validate of
// internal/cli/bundle, in text and with --output json, and for a single
// render without an error ruralz bundle build --offline --output json for
// the digest, dispatched by internal/cli/command as the ruralz root
// dispatches them, over an empty process environment, so a render without
// --env substitutes only defaults. A run with an environments file runs in
// the directory that holds the Bundle and names both relative to it, as
// the documents' examples run the CLI (--environments
// control/environments.yaml ./shop-bundle); the corpus's environments
// files sit beside their Bundles, so their findings name
// environments.yaml. A run on the stand-ins goes through the facade with
// the CLI's options (facadeOutcome), the one path that links them.
type cliPath struct{ s *suite }

func (cliPath) binary() string  { return binaryCLI }
func (cliPath) inProcess() bool { return true }

func (c cliPath) validate(ctx context.Context, r run) (outcome, error) {
	if r.standIns {
		return facadeOutcome(ctx, c.s.standIn, r)
	}
	if r.environments == "" {
		return c.commands(ctx, r.bundle, nil, true)
	}
	dir := filepath.Dir(r.bundle)
	envs, err := filepath.Rel(dir, r.environments)
	if err != nil {
		return outcome{}, err
	}
	flags := []string{"--environments", envs}
	if r.env != "" {
		flags = append(flags, "--env", r.env)
	}
	var o outcome
	err = inDir(dir, func() error {
		var err error
		o, err = c.commands(ctx, "."+string(filepath.Separator)+filepath.Base(r.bundle), flags, r.env != "")
		return err
	})
	return o, err
}

// commands runs ruralz bundle validate on the Bundle at arg with flags, in
// text and in JSON, and, when digest is set and validate finds no error,
// ruralz bundle build --offline for the Revision digest.
func (c cliPath) commands(ctx context.Context, arg string, flags []string, digest bool) (outcome, error) {
	validate := append([]string{"bundle", "validate"}, flags...)
	text, code, err := c.execute(ctx, append(slices.Clone(validate), arg))
	if err != nil {
		return outcome{}, err
	}
	js, codeJSON, err := c.execute(ctx, append(append(slices.Clone(validate), "--output", "json"), arg))
	if err != nil {
		return outcome{}, err
	}
	if code != codeJSON {
		return outcome{}, fmt.Errorf("ruralz bundle validate exits %d, and %d with --output json", code, codeJSON)
	}
	o := outcome{text: text, json: js}
	if !digest || code != command.ExitOK {
		return o, nil
	}
	out, _, err := c.execute(ctx, append(append([]string{"bundle", "build", "--offline", "--output", "json"}, flags...), arg))
	if err != nil {
		return outcome{}, err
	}
	var built struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(out, &built); err != nil || built.Digest == "" {
		return outcome{}, fmt.Errorf("ruralz bundle build prints %q: %v", out, err)
	}
	o.digest = built.Digest
	return o, nil
}

// execute runs the ruralz command args in process and returns its standard
// output and exit code, 0 or 1, the codes of a result; any other code is
// an error that carries its standard error.
func (cliPath) execute(ctx context.Context, args []string) ([]byte, int, error) {
	clk := clocktest.New(cliEpoch())
	var stdout, stderr bytes.Buffer
	sio := command.NewIO(&stdout, &stderr, nil, clk.Now, "")
	code := command.Execute(ctx, sio, binaryCLI, bundle.Specs(bundle.Options{Clock: clk}), args)
	if code != command.ExitOK && code != command.ExitNegative {
		return nil, code, fmt.Errorf("ruralz %s: exit %d: %s", strings.Join(args, " "), code, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), code, nil
}

// inDir runs f in the working directory dir and then restores the one
// before. The suite's tests never run in parallel, so no other test sees
// the change.
func inDir(dir string, f func() error) (err error) {
	old, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.Chdir(old)) }()
	return f()
}

// errNodeEnvironments refuses a run with an environments file on the
// Node's path: a file-mode Node reads none and selects no overlay.
var errNodeEnvironments = errors.New("a file-mode Node reads no environments file")

// nodePath is ruralzd's path in process: what a file-mode Node runs on a
// Bundle source (docs/architecture/02-configuration-model.md "Rendered
// Bundle"): stages A to M through the facade with the Node's worker
// default, no overlay and ${VAR} from its process environment, here empty,
// then the snapshot compiler's serve check (compile.Serve) on a Revision.
type nodePath struct{ s *suite }

func (nodePath) binary() string  { return binaryNode }
func (nodePath) inProcess() bool { return true }

func (n nodePath) validate(ctx context.Context, r run) (outcome, error) {
	if r.environments != "" {
		return outcome{}, errNodeEnvironments
	}
	p := n.s.node
	if r.standIns {
		p = n.s.standInNode
	}
	res, err := p.Run(ctx, requestOf(r))
	if err != nil {
		return outcome{}, err
	}
	digest := ""
	var after diag.List
	if res.Validated != nil {
		digest = res.Validated.Revision.Digest.String()
		if after, err = compile.Serve(p, res.Validated, res.Files); err != nil {
			return outcome{}, err
		}
	}
	out, err := outcomeOf(res.Diagnostics, digest)
	for _, d := range after {
		out.after = append(out.after, string(d.AppendText(nil)))
	}
	return out, err
}
