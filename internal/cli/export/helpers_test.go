// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package export

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
)

// update rewrites the golden files: go test ./internal/cli/export -update.
var update = flag.Bool("update", false, "rewrite testdata golden files")

// examplesDir is the repository's examples/ directory, read in place.
const examplesDir = "../../../examples"

// sharedPipeline is the pipeline of every test: every binary's checks and
// CEL compiler on a fake clock. Compiling the embedded schema views takes
// a while, so the tests share it.
var sharedPipeline = sync.OnceValues(func() (*pipeline.Pipeline, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	clk := clocktest.New(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
	return pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: c, Workers: 2, Now: clk.Now})
})

// validatedDir runs the shared pipeline on the Bundle at path, rendered
// without an Environment over no variables, and returns its validated
// Bundle, failing the test on an error diagnostic.
func validatedDir(t *testing.T, path string) *hub.Validated {
	t.Helper()
	p, err := sharedPipeline()
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Run(t.Context(), pipeline.Request{Source: loader.Source{Path: path}})
	if err != nil {
		t.Fatalf("Run(%s): %v", path, err)
	}
	if res.HasErrors() || res.Validated == nil {
		var b strings.Builder
		for _, d := range res.Diagnostics {
			b.Write(d.AppendText(nil))
			b.WriteByte('\n')
		}
		t.Fatalf("Run(%s) failed:\n%s", path, b.String())
	}
	return res.Validated
}

// validated writes the Bundle files, slash path to content, and returns
// its validated Bundle.
func validated(t *testing.T, files map[string]string) *hub.Validated {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bundle")
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return validatedDir(t, dir)
}

// bundleOf returns the files of a Bundle whose ruralz.yaml holds gateway
// and whose resources.yaml holds docs.
func bundleOf(gateway string, docs ...string) map[string]string {
	return map[string]string{"ruralz.yaml": gateway, "resources.yaml": strings.Join(docs, "---\n")}
}

// gatewayYAML returns a Gateway edge with the listeners web (http, 8080)
// and secure (https, 8443), and tail appended under spec.
func gatewayYAML(tail string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: web
      protocol: http
      port: 8080
    - name: secure
      protocol: https
      port: 8443
      tls:
        certificates:
          - name: main
            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls.crt}}
            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls.key}}
` + tail
}

// upstreamYAML returns an Upstream of protocol proto.
func upstreamYAML(name, proto string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: ` + name + `
spec:
  protocol: ` + proto + `
  endpoints:
    - address: ` + name + `.internal:80
`
}

// routeYAML returns a Route with the YAML of match members and the spec
// members of tail, both indented under their parent.
func routeYAML(name, match, tail string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: ` + name + `
spec:
  match:
` + match + tail
}

// toUpstream is the tail of a Route that sends to the Upstream up.
func toUpstream(up string) string { return "  upstreams:\n    - name: " + up + "\n" }

// policyYAML returns a Policy of type typ with the YAML of spec members
// after type.
func policyYAML(name, typ, spec string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: ` + name + `
spec:
  type: ` + typ + `
` + spec
}

// golden compares got with testdata/<name>, a slash path, rewriting it
// with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", filepath.FromSlash(name))
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under testdata.
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", name, err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run with -update to accept)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}
