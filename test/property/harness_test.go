// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package property

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/cli"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
)

// bound is the real-time limit of a wait for a goroutine a test started;
// fake time never needs it.
const bound = 30 * time.Second

// epoch is the fake time every target starts at.
func epoch() time.Time { return time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC) }

// newRand returns the generator a fuzzed seed draws from, the only source
// of randomness of a target.
func newRand(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) //nolint:gosec // G404: reproducible test data from the fuzzed seed, not a secret.
}

// pick returns a random element of xs.
func pick[T any](r *rand.Rand, xs []T) T { return xs[r.IntN(len(xs))] }

// waitFor polls cond until it holds, failing t after bound of real time.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	if !pollFor(bound, cond) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// pollFor polls cond for up to window of real time and reports whether it
// held.
func pollFor(window time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(window)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		timer := time.NewTimer(time.Millisecond)
		<-timer.C
	}
	return true
}

// toolchain is the configuration pipeline every binary builds: the
// offline Policy checks of checks.Table() and the CEL compiler.
type toolchain struct {
	cel  *cel.Compiler
	pipe *pipeline.Pipeline
}

// sharedTools builds the toolchain once per test binary: compiling the
// embedded schema views takes a while.
var sharedTools = sync.OnceValues(func() (*toolchain, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	p, err := pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: c, Workers: 2})
	if err != nil {
		return nil, err
	}
	return &toolchain{cel: c, pipe: p}, nil
})

// tools returns the shared toolchain, failing tb when it does not build.
func tools(tb testing.TB) *toolchain {
	tb.Helper()
	tc, err := sharedTools()
	if err != nil {
		tb.Fatalf("build the configuration pipeline: %v", err)
	}
	return tc
}

// seedBundle is one Bundle of the golden corpus with the environments
// file its entry renders.
type seedBundle struct {
	// name is the entry and Bundle directory, such as
	// golden/shop-bundle/bundle.
	name  string
	files []corpus.File
	envs  []byte
}

// sharedSeeds reads the golden corpus once per test binary.
var sharedSeeds = sync.OnceValues(func() ([]seedBundle, error) {
	c, err := corpus.Default()
	if errors.Is(err, corpus.ErrNoRepo) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []seedBundle
	for _, e := range c.Entries {
		if e.Kind != corpus.Golden && e.Kind != corpus.JSONSubset {
			continue
		}
		for _, b := range e.Bundles {
			out = append(out, seedBundle{name: e.Name + "/" + b.Name, files: b.Files, envs: e.Environments})
		}
	}
	return out, nil
})

// seeds returns the golden corpus's Bundles, failing tb on a read error.
func seeds(tb testing.TB) []seedBundle {
	tb.Helper()
	s, err := sharedSeeds()
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

// addBundleSeeds adds every Bundle of the golden corpus as an input
// (archive, environments, seed) of a Bundle target, the archive in the
// corpus's txtar form (corpus.Archive), once per seed value.
func addBundleSeeds(f *testing.F, values ...uint64) {
	f.Helper()
	for i, s := range seeds(f) {
		a := corpus.Archive{Comment: []byte(s.name + "\n"), Files: s.files}
		for _, v := range values {
			f.Add(a.Format(), slices.Clone(s.envs), v+uint64(i)*7919)
		}
	}
}

// Paths of a Bundle in its in-memory file system.
const (
	// bundleDir holds the Bundle.
	bundleDir = "b"
	// envsFile is the environments file beside it.
	envsFile = "environments.yaml"
	// maxFiles bounds the files of a fuzzed archive.
	maxFiles = 256
)

// memBundle is a Bundle and its environments file in one in-memory file
// system, which the loader reads through loader.Source.FS.
type memBundle struct {
	fs      fstest.MapFS
	hasEnvs bool
}

// newMemBundle returns the Bundle of files, relative to the Bundle root,
// with the environments file envs (none when empty). A name that is not a
// clean relative slash path is left out, as no Bundle can hold it.
func newMemBundle(files []corpus.File, envs []byte) memBundle {
	m := memBundle{fs: fstest.MapFS{}}
	for i, f := range files {
		if i >= maxFiles {
			break
		}
		if !fs.ValidPath(f.Name) || f.Name == "." || strings.Contains(f.Name, `\`) {
			continue
		}
		m.fs[bundleDir+"/"+f.Name] = &fstest.MapFile{Data: f.Data, Mode: 0o600}
	}
	if len(envs) > 0 {
		m.fs[envsFile] = &fstest.MapFile{Data: envs, Mode: 0o600}
		m.hasEnvs = true
	}
	return m
}

// withFiles returns a Bundle of the given files, relative to the Bundle
// root, with m's environments file.
func (m memBundle) withFiles(files map[string][]byte) memBundle {
	out := memBundle{fs: fstest.MapFS{}, hasEnvs: m.hasEnvs}
	for name, data := range files {
		out.fs[bundleDir+"/"+name] = &fstest.MapFile{Data: data, Mode: 0o600}
	}
	if m.hasEnvs {
		out.fs[envsFile] = m.fs[envsFile]
	}
	return out
}

// files returns the Bundle's files by path relative to the Bundle root.
func (m memBundle) files() map[string][]byte {
	out := map[string][]byte{}
	for name, f := range m.fs {
		if rel, ok := strings.CutPrefix(name, bundleDir+"/"); ok {
			out[rel] = f.Data
		}
	}
	return out
}

// source is the Bundle as a loader source.
func (m memBundle) source() loader.Source { return loader.Source{FS: m.fs, Path: bundleDir} }

// envsSource is the environments file as a loader source.
func (m memBundle) envsSource() loader.Source { return loader.Source{FS: m.fs, Path: envsFile} }

// noEnv names the render without --env, over the variables a target
// passes (none unless it says so).
const noEnv = ""

// renders returns the renders of m: without --env first, then each
// Environment of its environments file in file order. A file that cannot
// be read or holds an error adds none.
func (tc *toolchain) renders(t testing.TB, m memBundle) []string {
	t.Helper()
	out := []string{noEnv}
	if !m.hasEnvs {
		return out
	}
	envs, err := tc.pipe.ReadEnvironments(t.Context(), m.envsSource())
	if err != nil || envs.Diagnostics().HasErrors() {
		return out
	}
	return append(out, envs.FileOrder()...)
}

// run runs the pipeline on m for one render: env names an Environment of
// m's environments file, or noEnv for the render without one, whose ${VAR}
// values are vars.
func (tc *toolchain) run(t testing.TB, m memBundle, env string, vars subst.Source, render bool) *pipeline.Result {
	t.Helper()
	req := pipeline.Request{Source: m.source(), Render: render}
	if env != noEnv {
		req.Environments, req.Environment = m.envsSource(), env
	} else {
		if vars == nil {
			vars = subst.Map{}
		}
		req.Variables = vars
	}
	res, err := tc.pipe.Run(t.Context(), req)
	if err != nil {
		t.Fatalf("pipeline.Run(%s): %v", env, err)
	}
	return res
}

// digests returns the Revision of each render of m, the zero digest for
// one that has an error; the render without an Environment has no
// ${VAR} values.
func (tc *toolchain) digests(t testing.TB, m memBundle, renders []string) []string {
	t.Helper()
	out := make([]string, len(renders))
	for i, env := range renders {
		if res := tc.run(t, m, env, nil, false); res.Validated != nil && !res.HasErrors() {
			out[i] = res.Validated.Revision.Digest.String()
		}
	}
	return out
}

// sourceFile is one base or overlay file of a Bundle as stage B parses
// it: neither merged nor substituted.
type sourceFile struct {
	// path is relative to the Bundle root.
	path string
	// overlay names the overlay that holds the file; "" for a base file.
	overlay string
	docs    []*tree.Node
}

// parseSource returns the base and overlay files of m as the loader reads
// them; false when stages A to C report an error.
func (tc *toolchain) parseSource(t testing.TB, m memBundle) ([]sourceFile, bool) {
	t.Helper()
	b, err := tc.pipe.Parse(t.Context(), m.source())
	if err != nil {
		if errors.Is(err, loader.ErrSource) {
			return nil, false
		}
		t.Fatalf("Parse: %v", err)
	}
	if b.Stopped() || b.Diagnostics().HasErrors() {
		return nil, false
	}
	var out []sourceFile
	for _, f := range b.Files() {
		rs, ds, err := b.ParseFile(t.Context(), f.Path)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", f.Path, err)
		}
		if ds.HasErrors() {
			return nil, false
		}
		sf := sourceFile{path: f.Path, overlay: f.Overlay}
		for _, r := range rs {
			sf.docs = append(sf.docs, r.Root)
		}
		out = append(out, sf)
	}
	return out, true
}

// yamlOf writes docs as a restricted-profile YAML stream, strings as they
// are: source text whose ${VAR} and $${ stay as authored.
func yamlOf(t testing.TB, docs []*tree.Node) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := profile.EncodeWith(&buf, docs, profile.EncodeOptions{Verbatim: true}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// jsonOf writes doc as one JSON object, strings as they are.
func jsonOf(t testing.TB, doc *tree.Node) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := profile.EncodeJSON(&buf, doc, profile.EncodeOptions{Verbatim: true}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// yamlSafe returns JSON text as a YAML flow document: the restricted
// profile refuses some characters raw, even in a quoted scalar, which JSON
// writes raw (DEL, the C1 controls, U+FEFF, U+FFFE, U+FFFF and the line
// separators U+2028 and U+2029), so they become \uXXXX escapes, which
// both read alike. Such characters occur only inside JSON strings.
func yamlSafe(js []byte) []byte {
	var b bytes.Buffer
	for _, r := range string(js) {
		switch {
		case r == 0x7f, r >= 0x80 && r <= 0x9f, r == 0xfeff, r == 0xfffe, r == 0xffff, r == 0x2028, r == 0x2029:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.Bytes()
}

// canonicalText returns n as RFC 8785 JSON, the form in which two trees
// equal in value compare equal whatever their member order and number
// spelling.
func canonicalText(t testing.TB, n *tree.Node) string {
	t.Helper()
	b, err := canonical.AppendNode(nil, n)
	if err != nil {
		t.Fatalf("canonical.AppendNode: %v", err)
	}
	return string(b)
}

// texts returns the text lines of ds.
func texts(ds diag.List) string {
	var b strings.Builder
	for _, d := range ds {
		b.Write(d.AppendText(nil))
		b.WriteByte('\n')
	}
	return b.String()
}

// codesOf returns the codes of the errors among ds.
func codesOf(ds diag.List) []string {
	var out []string
	for _, d := range ds {
		if d.Severity == diag.SeverityError {
			out = append(out, d.Code)
		}
	}
	return out
}

// ruralz runs the ruralz command args in process, dispatched through the
// root's command table (cli.Registry) as cmd/ruralz dispatches it, over
// the process environment environ and the fake clock, and returns its
// standard output, standard error and exit code.
func ruralz(ctx context.Context, environ []string, args ...string) (stdout, stderr string, code int) {
	clk := clocktest.New(epoch())
	var out, errOut bytes.Buffer
	sio := command.NewIO(&out, &errOut, environ, clk.Now, "")
	code = command.Execute(ctx, sio, cli.Program, cli.Registry(cli.Options{Clock: clk}), args)
	return out.String(), errOut.String(), code
}

// writeTree writes files, by slash path relative to dir, under dir.
func writeTree(t testing.TB, dir string, files map[string][]byte) {
	t.Helper()
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
