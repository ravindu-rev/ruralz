// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// exportBundle is the Bundle of the export goldens: Route orders, a
// template with GET and POST and a body schema, and Routes assets (a
// prefix path) and feed (to a websocket Upstream), which export none;
// the listener port is ${PORT:-8080}.
func exportBundle() map[string]string {
	return map[string]string{
		"ruralz.yaml": `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: http
      protocol: http
      port: ${PORT:-8080}
`,
		"upstreams.yaml": upstreamDoc("") + `---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: feed
spec:
  protocol: websocket
  endpoints:
    - address: feed.internal:80
`,
		"policies.yaml": policyDoc("validate-order", "validation.json-schema", `  config:
    schema:
      type: object
      required: [sku]
      properties:
        sku: {type: string}
`),
		"routes/orders.yaml": `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: orders
spec:
  match:
    path: {template: "/v1/orders/{orderId}"}
    methods: [GET, POST]
  policies:
    - name: validate-order
  upstreams:
    - name: orders
`,
		"routes/assets.yaml": routeDoc("assets", "orders", ""),
		"routes/feed.yaml": `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: feed
spec:
  match:
    path: {exact: /feed}
  upstreams:
    - name: feed
`,
	}
}

// exportWarnings is what openapi and postman print on standard error for
// exportBundle: one warning per Route without an operation.
const exportWarnings = "warning: Route/assets: a prefix path has no OpenAPI path\n" +
	"warning: Route/feed: Upstream \"feed\" speaks websocket, not http\n"

// TestExportGolden pins each format's document of exportBundle on
// standard output, in each output mode it takes, and the warnings of the
// Routes without an operation on standard error.
func TestExportGolden(t *testing.T) {
	dir := writeBundle(t, exportBundle())
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"openapi.yaml", []string{"openapi", dir}, exportWarnings},
		{"openapi.json", []string{"openapi", dir, "--output", "json"}, exportWarnings},
		{"postman.json", []string{"postman", dir}, exportWarnings},
		{"dot.txt", []string{"dot", dir}, ""},
		{"dot-orders.txt", []string{"dot", dir, "--route", "orders"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := run(t, append([]string{"bundle", "export"}, tc.args...)...)
			if r.code != command.ExitOK || r.stderr != tc.stderr {
				t.Fatalf("export %q = exit %d, stderr %q; want 0 and %q", tc.args, r.code, r.stderr, tc.stderr)
			}
			golden(t, "export/"+tc.name, r.stdout)
		})
	}
}

// TestExportPostmanOutputJSON: postman takes --output json, its only
// form, and writes the same collection as without it.
func TestExportPostmanOutputJSON(t *testing.T) {
	dir := writeBundle(t, exportBundle())
	plain := run(t, "bundle", "export", "postman", dir)
	asJSON := run(t, "bundle", "export", "postman", dir, "--output", "json")
	if plain.code != command.ExitOK || asJSON.code != command.ExitOK || plain.stdout != asJSON.stdout {
		t.Errorf("postman = exit %d, with --output json exit %d; outputs differ:\n%s\n---\n%s", plain.code, asJSON.code, plain.stdout, asJSON.stdout)
	}
}

// TestExportOutputFile: --output-file receives the document, standard
// output stays empty, and the warnings still print on standard error.
func TestExportOutputFile(t *testing.T) {
	dir := writeBundle(t, exportBundle())
	for _, format := range []string{"openapi", "postman", "dot"} {
		t.Run(format, func(t *testing.T) {
			want := run(t, "bundle", "export", format, dir)
			out := filepath.Join(t.TempDir(), "document")
			r := run(t, "bundle", "export", format, dir, "--output-file", out)
			r.want(t, "export "+format+" --output-file", command.ExitOK, "", want.stderr)
			if got := readFile(t, out); got != want.stdout {
				t.Errorf("--output-file holds\n%s\nwant\n%s", got, want.stdout)
			}
		})
	}
}

// TestExportUsage: the flag and argument combinations export refuses exit
// 2 with a usage error before reading the Bundle.
func TestExportUsage(t *testing.T) {
	dir := writeBundle(t, exportBundle())
	envs := envFile(t)
	usage := "Run 'ruralz bundle export --help' for usage.\n"
	tests := []struct {
		name string
		args []string
		msg  string
	}{
		{"--route with openapi", []string{"openapi", dir, "--route", "orders"}, "--route applies to export dot only"},
		{"--route with postman", []string{"postman", dir, "--route", "orders"}, "--route applies to export dot only"},
		{"an empty --route", []string{"dot", dir, "--route", ""}, "--route needs a Route name"},
		{"--output json with dot", []string{"dot", dir, "--output", "json"}, "export dot writes DOT text; --output does not apply"},
		{"--output yaml with dot", []string{"dot", dir, "--output", "yaml"}, "export dot writes DOT text; --output does not apply"},
		{"--output yaml with postman", []string{"postman", dir, "--output", "yaml"}, "export postman writes JSON"},
		{"--output text", []string{"openapi", dir, "--output", "text"}, `unsupported --output "text" (want yaml or json)`},
		{"an unknown format", []string{"swagger", dir}, `unsupported FORMAT "swagger" (want openapi, postman or dot)`},
		{"no format", nil, "missing argument FORMAT"},
		{"--environments without --env", []string{"openapi", dir, "--environments", envs}, "--environments needs --env: ruralz bundle export renders one Environment"},
		{"--env without --environments", []string{"openapi", dir, "--env", "prod"}, "--env needs --environments: fetching Environments from Ruralz Control is Planned (M2)"},
		{"an empty --environments", []string{"openapi", dir, "--environments", ""}, "--environments needs a file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := run(t, append([]string{"bundle", "export"}, tc.args...)...)
			r.want(t, tc.name, command.ExitNoResult, "", "ruralz bundle export: "+tc.msg+"\n"+usage)
		})
	}
}

// TestExportEnvironment: --env renders that Environment of
// --environments, whose variables reach the document: the listener port
// that sets the Postman baseUrl.
func TestExportEnvironment(t *testing.T) {
	dir := writeBundle(t, exportBundle())
	envs := writeFile(t, "environments.yaml", environmentDoc("prod", "\n  variables:\n    PORT: \"9090\""))
	r := run(t, "bundle", "export", "postman", dir, "--environments", envs, "--env", "prod")
	if r.code != command.ExitOK || !strings.Contains(r.stdout, `"value": "http://localhost:9090"`) {
		t.Errorf("export postman --env prod = exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
	plain := run(t, "bundle", "export", "postman", dir)
	if !strings.Contains(plain.stdout, `"value": "http://localhost:8080"`) {
		t.Errorf("export postman without --env lacks the default port:\n%s", plain.stdout)
	}
}

// TestExportInvalidBundle: an invalid Bundle prints its diagnostics on
// standard error, writes no document, --output-file included, and exits
// 1; warnings of a valid Bundle print there too.
func TestExportInvalidBundle(t *testing.T) {
	files := exportBundle()
	files["routes/broken.yaml"] = routeDoc("broken", "inventroy", "")
	dir := writeBundle(t, files)
	for _, format := range []string{"openapi", "postman", "dot"} {
		t.Run(format, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "document")
			r := run(t, "bundle", "export", format, dir, "--output-file", out)
			if r.code != command.ExitNegative || r.stdout != "" || !strings.Contains(r.stderr, " error RZ-CFG-009 Route/broken ") {
				t.Errorf("export %s of an invalid Bundle = exit %d, stdout %q, stderr %q", format, r.code, r.stdout, r.stderr)
			}
			if exportFileExists(out) {
				t.Errorf("export %s of an invalid Bundle wrote --output-file", format)
			}
		})
	}
	warn := exportBundle()
	warn["policies.yaml"] += "---\n" + policyDoc("secret-header", "headers", `  config:
    request:
      set:
        - name: authorization
          value: "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJlc2lnbmF0dXJl"
`)
	r := run(t, "bundle", "export", "dot", writeBundle(t, warn))
	if r.code != command.ExitOK || !strings.Contains(r.stderr, " warning RZ-CFG-013 Policy/secret-header ") {
		t.Errorf("export dot of a Bundle with a warning = exit %d, stderr %q", r.code, r.stderr)
	}
}

// TestExportUnknownRoute: dot --route naming no Route of the Bundle exits
// 2 with the nearest Route name as a hint.
func TestExportUnknownRoute(t *testing.T) {
	dir := writeBundle(t, exportBundle())
	r := run(t, "bundle", "export", "dot", dir, "--route", "ordrs")
	r.want(t, "dot --route ordrs", command.ExitNoResult, "", "ruralz bundle export: unknown Route \"ordrs\" (did you mean \"orders\"?)\n")
}

// TestExportSource: a source that is missing exits 2 naming it, and a
// directory without ruralz.yaml is RZ-CFG-016, exit 1.
func TestExportSource(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	r := run(t, "bundle", "export", "openapi", missing)
	if r.code != command.ExitNoResult || r.stdout != "" || !strings.HasPrefix(r.stderr, "ruralz bundle export: cannot read "+missing) {
		t.Errorf("export of a missing source = exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	empty := writeBundle(t, map[string]string{"routes/x.yaml": routeDoc("x", "orders", "")})
	r = run(t, "bundle", "export", "openapi", empty)
	if r.code != command.ExitNegative || !strings.Contains(r.stderr, " error RZ-CFG-016: ") {
		t.Errorf("export of a directory without ruralz.yaml = exit %d, stderr %q", r.code, r.stderr)
	}
}

// TestExportArguments: FORMAT is required and takes openapi, postman or
// dot; DIR is optional, defaults to "." and completes file names, as a
// Bundle may be a directory or one file.
func TestExportArguments(t *testing.T) {
	spec := ExportSpec(Options{})
	if len(spec.Args) != 2 || spec.Args[0].Name != "FORMAT" || spec.Args[0].Optional ||
		!slices.Equal(spec.Args[0].Values, []string{"openapi", "postman", "dot"}) || spec.Args[0].Complete != command.CompleteValues {
		t.Errorf("FORMAT = %+v", spec.Args)
	}
	if d := spec.Args[1]; d.Name != "DIR" || !d.Optional || d.Default != "." || d.Complete != command.CompleteFile {
		t.Errorf("DIR = %+v", d)
	}
}

// TestExportExamples exports the example Bundles, read in place: every
// format exits 0, and openapi warns for the gRPC Route and the Route to
// the ai Upstream of shop-bundle.
func TestExportExamples(t *testing.T) {
	for _, tc := range []struct {
		dir, stderr string
	}{
		{"shop-bundle", "warning: Route/cart-grpc: match.grpc has no OpenAPI operation\n" +
			"warning: Route/support-chat: Upstream \"llm\" speaks ai, not http\n"},
		{"shop-rest-bundle", ""},
	} {
		dir := filepath.Join(examplesDir, tc.dir)
		for _, format := range []string{"openapi", "postman", "dot"} {
			r := run(t, "bundle", "export", format, dir)
			want := tc.stderr
			if format == "dot" {
				want = ""
			}
			if r.code != command.ExitOK || r.stderr != want || r.stdout == "" {
				t.Errorf("export %s %s = exit %d, stderr %q, %d bytes; want 0 and %q", format, tc.dir, r.code, r.stderr, len(r.stdout), want)
			}
		}
	}
}

// TestExportHelp: the help names the formats, the arguments and the
// flags on every platform.
func TestExportHelp(t *testing.T) {
	r := run(t, "bundle", "export", "--help")
	want := []string{
		"Usage: ruralz bundle export FORMAT [DIR] [FLAGS]", "OpenAPI 3.x, Postman collection or DOT graph of a Bundle.",
		"FORMAT  Document format (openapi, postman or dot)", "(default .)", "--route NAME", "--output yaml|json",
		"--output-file PATH", "--env NAME", "--environments FILE", "OpenAPI 3.1",
	}
	if missing := containsAll(r.stdout, want...); r.code != command.ExitOK || len(missing) > 0 {
		t.Errorf("export --help = exit %d, lacks %q:\n%s", r.code, missing, r.stdout)
	}
}

// exportFileExists reports whether a file exists at path.
func exportFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
