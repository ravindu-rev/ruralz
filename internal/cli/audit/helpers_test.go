// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
)

// sharedPipeline is the pipeline every binary builds, with the offline
// Policy checks and the CEL compiler, shared by the tests.
var sharedPipeline = sync.OnceValues(func() (*pipeline.Pipeline, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	now := func() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }
	return pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: c, Workers: 2, Now: now})
})

// audited runs the one-file Bundle of docs through the pipeline and
// returns Check's findings, failing the test unless the run is valid.
func audited(t *testing.T, docs ...string) []Finding {
	t.Helper()
	p, err := sharedPipeline()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ruralz.yaml"), []byte(strings.Join(docs, "---\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := p.Run(t.Context(), pipeline.Request{Source: loader.Source{Path: dir}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.HasErrors() || res.Validated == nil {
		var b strings.Builder
		for _, d := range res.Diagnostics {
			b.Write(d.AppendText(nil))
			b.WriteByte('\n')
		}
		t.Fatalf("the test Bundle is invalid:\n%s", b.String())
	}
	return Check(res.Validated, res.Files, p.HubIndex())
}

// lines returns the text line of each finding of fs.
func lines(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, string(f.AppendText(nil)))
	}
	return out
}

// ofRule returns the text lines of the findings of fs whose rule is rule.
func ofRule(fs []Finding, rule string) []string {
	var out []string
	for _, f := range fs {
		if f.Rule == rule {
			out = append(out, string(f.AppendText(nil)))
		}
	}
	return out
}

// gateway returns a Gateway document named edge with one https listener
// and the YAML of spec members after listeners, indented by two spaces.
func gateway(tail string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: https
      protocol: https
      port: 8443
      tls:
        certificates:
          - name: api
            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.crt}}
            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.key}}
` + tail
}

// orders returns the TLS Upstream orders, which every test Route names.
func orders() string {
	return `apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: orders
spec:
  protocol: http
  endpoints:
    - address: orders.internal:443
  tls: {}
`
}

// route returns a Route document to Upstream orders with the YAML of spec
// members after upstreams, indented by two spaces.
func route(name, tail string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: ` + name + `
spec:
  match:
    path: {prefix: /` + name + `}
  upstreams:
    - name: orders
` + tail
}

// policy returns a Policy document of type typ with the YAML of spec
// members after type, indented by two spaces.
func policy(name, typ, tail string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: ` + name + `
spec:
  type: ` + typ + `
` + tail
}

// apiKey is an auth.api-key Policy named key and the Consumer its keys
// match.
func apiKey() []string {
	return []string{
		policy("key", "auth.api-key", "  config: {}\n"),
		`apiVersion: ruralz/v1alpha1
kind: Consumer
metadata:
  name: acme
spec:
  credentials:
    apiKeys:
      - name: primary
        hash: "sha256:d1bec0f9342e5607570b2636ed0256023b13be7d7d4bb45e1bed3cfee80dc1b6"
`,
	}
}

// plugin returns a Plugin document named name granting caps, with the
// YAML of spec members after capabilities, indented by two spaces.
func plugin(name, caps, tail string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Plugin
metadata:
  name: ` + name + `
spec:
  image: ghcr.io/acme/` + name + `:1.0.0@sha256:fc37c2971bb6e82aa12a871aed718818afcf45ab42bd093f0cf52f4ad8045435
  abi: ruralz.plugin.v1
  phases: [onRequestHeaders]
  capabilities: [` + caps + `]
` + tail
}

// pluginPolicy returns a plugin Policy document named name running the
// Plugin p with the YAML of spec members after plugin, indented by two
// spaces.
func pluginPolicy(name, p, tail string) string {
	return policy(name, "plugin", "  plugin: "+p+"\n"+tail)
}
