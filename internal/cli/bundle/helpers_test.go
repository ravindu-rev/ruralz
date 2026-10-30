// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
)

// testVersion is the CLI build version of the tests.
const testVersion = "0.4.0"

func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// examplesDir is the repository's examples/ directory, read in place.
const examplesDir = "../../../examples"

// sharedPipeline is the pipeline of every test run but those that test
// its construction: every binary's checks and CEL compiler on a fake clock.
// Compiling the embedded schema views takes a while, so the tests share it.
var sharedPipeline = sync.OnceValues(func() (*pipeline.Pipeline, error) {
	return newPipeline(Options{Clock: clocktest.New(epoch())})
})

// newTestPipeline builds a pipeline with o, every binary's checks and a
// CEL compiler on a fake clock filling in what o leaves unset.
func newTestPipeline(o pipeline.Options) (*pipeline.Pipeline, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	o.Checks, o.CEL, o.Now = checks.Table(), c, clocktest.New(epoch()).Now
	return pipeline.New(o)
}

// testOptions are the bundle commands' options on a fake clock with the
// shared pipeline.
func testOptions() Options {
	return Options{
		Clock: clocktest.New(epoch()), Version: testVersion,
		pipelineFunc: func(Options) (*pipeline.Pipeline, error) { return sharedPipeline() },
	}
}

// result is what one command.Execute returned.
type result struct {
	code           int
	stdout, stderr string
}

// execute runs ruralz args... over Specs(o) with environ.
func execute(t *testing.T, o Options, environ []string, args ...string) result {
	t.Helper()
	return executeCtx(t.Context(), o, environ, args...)
}

// executeCtx is execute under ctx.
func executeCtx(ctx context.Context, o Options, environ []string, args ...string) result {
	var out, errOut bytes.Buffer
	sio := command.NewIO(&out, &errOut, environ, epoch, "linux")
	code := command.Execute(ctx, sio, "ruralz", Specs(o), args)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// run executes with testOptions and no environment.
func run(t *testing.T, args ...string) result {
	t.Helper()
	return execute(t, testOptions(), nil, args...)
}

// withStdin returns o reading standard input from s.
func withStdin(o Options, s string) Options {
	o.Stdin = strings.NewReader(s)
	return o
}

// want fails unless r has the exit code, standard output and standard
// error given.
func (r result) want(t *testing.T, what string, code int, stdout, stderr string) {
	t.Helper()
	if r.code != code || r.stdout != stdout || r.stderr != stderr {
		t.Errorf("%s = exit %d\n--- stdout ---\n%s\n--- stderr ---\n%s\nwant exit %d\n--- stdout ---\n%s\n--- stderr ---\n%s",
			what, r.code, r.stdout, r.stderr, code, stdout, stderr)
	}
}

// wantCode fails unless r has the exit code.
func (r result) wantCode(t *testing.T, what string, code int) {
	t.Helper()
	if r.code != code {
		t.Errorf("%s = exit %d, want %d\n--- stdout ---\n%s\n--- stderr ---\n%s", what, r.code, code, r.stdout, r.stderr)
	}
}

// writeFiles writes files, slash path to content, under dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeBundle writes a Bundle directory of files and returns its path.
func writeBundle(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bundle")
	writeFiles(t, dir, files)
	return dir
}

// writeFile writes one file named name in a new directory and returns its
// path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// readFile returns the content of path.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: a file the test wrote.
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// gatewayDoc is ruralz.yaml of the test Bundles: one http listener.
const gatewayDoc = `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
`

// upstreamDoc returns an Upstream document with tail appended under spec.
func upstreamDoc(name, tail string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: ` + name + `
spec:
  protocol: http
  endpoints:
    - address: ` + name + `.internal:80
` + tail
}

// routeDoc returns a Route document to Upstream up with tail appended
// under spec.
func routeDoc(name, up, tail string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: ` + name + `
spec:
  match:
    path: {prefix: /` + name + `}
  upstreams:
    - name: ` + up + `
` + tail
}

// policyDoc returns a Policy document of type typ with the YAML of spec
// members after type, indented by two spaces.
func policyDoc(name, typ, spec string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: ` + name + `
spec:
  type: ` + typ + `
` + spec
}

// pluginDoc is a Plugin pinned by digest.
const pluginDoc = `apiVersion: ruralz/v1alpha1
kind: Plugin
metadata:
  name: geo
spec:
  image: ghcr.io/acme/geo:1.0.0@sha256:fc37c2971bb6e82aa12a871aed718818afcf45ab42bd093f0cf52f4ad8045435
  abi: ruralz.plugin.v1
  phases: [onRequestHeaders]
  capabilities: [request.headers.read]
  limits: {memoryBytes: 16Mi, timeout: 5ms}
`

// environmentDoc returns an Environment document with the YAML of spec
// after "spec:".
func environmentDoc(name, spec string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Environment
metadata:
  name: ` + name + `
spec:` + spec + "\n"
}

// docs joins YAML documents into one stream.
func docs(ds ...string) string { return strings.Join(ds, "---\n") }

// simpleBundle is a valid Bundle: a Gateway, an Upstream orders and a
// Route orders whose timeout is ${TIMEOUT:-5s}.
func simpleBundle() map[string]string {
	return map[string]string{
		"ruralz.yaml":        gatewayDoc,
		"upstreams.yaml":     upstreamDoc("orders", ""),
		"routes/orders.yaml": routeDoc("orders", "orders", "  timeout: ${TIMEOUT:-5s}\n"),
	}
}

// envFile writes an environments file of staging (TIMEOUT 3s) then prod
// (overlay prod, TIMEOUT 4s) and returns its path.
func envFile(t *testing.T) string {
	t.Helper()
	return writeFile(t, "environments.yaml", docs(
		environmentDoc("staging", "\n  variables:\n    TIMEOUT: 3s"),
		environmentDoc("prod", "\n  variables:\n    TIMEOUT: 4s"),
	))
}

// build runs the shared pipeline on the Bundle dir for env (none: the
// process environment, empty) and returns its Revision, failing the test
// unless the run is clean.
func build(t *testing.T, dir, envs, env string) revision.Revision {
	t.Helper()
	p, err := sharedPipeline()
	if err != nil {
		t.Fatal(err)
	}
	req := pipeline.Request{Source: loader.Source{Path: dir}}
	if env != "" {
		req.Environments, req.Environment = loader.Source{Path: envs}, env
	}
	res, err := p.Run(t.Context(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Validated == nil || len(res.Diagnostics) > 0 {
		t.Fatalf("Run of %s: %d diagnostics, Validated %v", dir, len(res.Diagnostics), res.Validated != nil)
	}
	return res.Validated.Revision
}

// dumpOf returns the /config/dump document of rev with the
// Last-Known-Good digest lkg.
func dumpOf(t *testing.T, rev revision.Revision, lkg revision.Digest) []byte {
	t.Helper()
	b, ok := canonical.AppendDump(nil, rev, lkg)
	if !ok {
		t.Fatal("AppendDump refused the Revision")
	}
	return b
}

// lines splits s into its lines, without the newline of the last.
func lines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// endless is a reader of endless spaces, as a standard input that never
// ends.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// failingReader fails every read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// containsAll reports the members of want s lacks.
func containsAll(s string, want ...string) []string {
	var missing []string
	for _, w := range want {
		if !strings.Contains(s, w) {
			missing = append(missing, w)
		}
	}
	return slices.Clip(missing)
}
