// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
)

// sharedPipeline is the pipeline of every render test: every binary's
// checks and CEL compiler on a fake clock. Compiling the schema views
// takes a while, so the tests share it.
var sharedPipeline = sync.OnceValues(func() (*pipeline.Pipeline, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	return pipeline.New(pipeline.Options{
		Checks: checks.Table(), CEL: c, Workers: runtime.GOMAXPROCS(0),
		Now: clocktest.New(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)).Now,
	})
})

func testPipeline(t *testing.T) *pipeline.Pipeline {
	t.Helper()
	p, err := sharedPipeline()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// testBundle is a one-file Bundle with two listeners, a file secret with a
// key and one without, an env secret, a redis State Store and a literal
// "$${" value that every render keeps escaped.
const testBundle = `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: public
      protocol: http
      port: 8080
    - name: secure
      protocol: https
      port: 8443
      tls:
        certificates:
          - name: main
            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.crt}}
            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls/keys.json, key: pem}}
  stateStore:
    driver: redis
    url: {secretRef: {provider: env, name: RURALZ_STATE_STORE_URL}}
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: hello
spec:
  protocol: http
  endpoints:
    - address: "127.0.0.1:9000"
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: literal
spec:
  type: headers
  config:
    response:
      set: [{name: x-literal, value: "$${LITERAL}"}]
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: hello
spec:
  match:
    path: {prefix: /}
  policies:
    - name: literal
  upstreams:
    - name: hello
`

// newTestWorkspace returns a workspace in the test's temporary directory.
func newTestWorkspace(t *testing.T) *Workspace {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	ws, err := NewWorkspace("ruralz-test-", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Remove() })
	return ws
}

// writeTestFile writes content to dir/name, creating parents, and returns
// the path.
func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// digestOf builds the Bundle at path as ruralzd loads a test render: one
// source, no Environment and no variable.
func digestOf(t *testing.T, path string) revision.Digest {
	t.Helper()
	res, err := testPipeline(t).Run(t.Context(), pipeline.Request{Source: loader.Source{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Validated == nil {
		t.Fatalf("%s does not build:\n%v", path, res.Diagnostics)
	}
	return res.Validated.Revision.Digest
}

// mustRender renders with r, failing the test on any error.
func mustRender(t *testing.T, r *Renderer) *Render {
	t.Helper()
	rend, err := r.Render(t.Context())
	if err != nil {
		t.Fatalf("Render = %v", err)
	}
	return rend
}

// swapped swaps rend in and returns the test render's bytes.
func swapped(t *testing.T, r *Renderer, ws *Workspace, rend *Render) string {
	t.Helper()
	if err := r.Swap(rend); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ws.Config())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestRenderUnchanged: without a rewrite the test render is the rendered
// Bundle, its test digest the Revision digest, no port mapping prints, and
// the listeners and admin port are the authored ones.
func TestRenderUnchanged(t *testing.T) {
	ws := newTestWorkspace(t)
	src := writeTestFile(t, t.TempDir(), "bundle.yaml", testBundle)
	r := NewRenderer(testPipeline(t), ws, Request{Source: src})
	rend := mustRender(t, r)
	if rend.TestDigest != rend.Revision || rend.Revision != digestOf(t, src) {
		t.Errorf("test digest %s, Revision %s, want both the source's %s", rend.TestDigest, rend.Revision, digestOf(t, src))
	}
	if rend.Rewritten || rend.Ports() != nil || len(rend.Mapping()) != 0 {
		t.Errorf("a render without a rewrite has ports %v", rend.Ports())
	}
	wantListeners := []Listener{
		{Port: Port{Name: "public", Port: 8080, TestPort: 8080}, Protocol: "http"},
		{Port: Port{Name: "secure", Port: 8443, TestPort: 8443}, Protocol: "https"},
	}
	if !slices.Equal(rend.Listeners, wantListeners) || rend.Admin != (Port{Name: "admin", Port: 9901, TestPort: 9901}) {
		t.Errorf("listeners %v, admin %v", rend.Listeners, rend.Admin)
	}
	if rend.Listeners[0].URL() != "http://127.0.0.1:8080" || rend.Listeners[1].URL() != "https://127.0.0.1:8443" || rend.AdminURL() != "http://127.0.0.1:9901" {
		t.Errorf("URLs %s, %s, %s", rend.Listeners[0].URL(), rend.Listeners[1].URL(), rend.AdminURL())
	}
	text := swapped(t, r, ws, rend)
	if digestOf(t, ws.Config()) != rend.TestDigest {
		t.Error("the swapped test render does not build to the test digest")
	}
	if !strings.Contains(text, `value: "$${LITERAL}"`) {
		t.Errorf("the literal ${ is not escaped in the test render:\n%s", text)
	}
	if _, err := os.Stat(ws.staged()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Swap left the staged file: %v", err)
	}
}

// TestRenderEphemeralPorts: every listener port and admin.port becomes a
// free loopback port; the mapping prints; a re-render keeps each port's
// replacement and an added listener gets a new one; the test digest is the
// swapped file's and differs from the Revision.
func TestRenderEphemeralPorts(t *testing.T) {
	ws := newTestWorkspace(t)
	dir := t.TempDir()
	src := writeTestFile(t, dir, "bundle.yaml", testBundle)
	r := NewRenderer(testPipeline(t), ws, Request{Source: src, EphemeralPorts: true})
	first := mustRender(t, r)
	if !first.Rewritten || first.TestDigest == first.Revision || first.Revision != digestOf(t, src) {
		t.Fatalf("rewritten %v, test digest %s, Revision %s", first.Rewritten, first.TestDigest, first.Revision)
	}
	ports := first.Ports()
	if len(ports) != 3 || ports[0].Name != "public" || ports[1].Name != "secure" || ports[2].Name != "admin" {
		t.Fatalf("ports %v", ports)
	}
	seen := map[int]bool{}
	for i, p := range ports {
		authored := []int{8080, 8443, 9901}[i]
		if p.Port != authored || p.TestPort == authored || p.TestPort <= 0 || seen[p.TestPort] {
			t.Errorf("port %v of %v", p, ports)
		}
		seen[p.TestPort] = true
	}
	wantMapping := []string{
		"public: 8080 -> " + itoa(ports[0].TestPort),
		"secure: 8443 -> " + itoa(ports[1].TestPort),
		"admin: 9901 -> " + itoa(ports[2].TestPort),
	}
	if got := first.Mapping(); !slices.Equal(got, wantMapping) {
		t.Errorf("Mapping = %q, want %q", got, wantMapping)
	}
	text := swapped(t, r, ws, first)
	if digestOf(t, ws.Config()) != first.TestDigest {
		t.Error("the swapped test render does not build to the test digest")
	}
	if !strings.Contains(text, "port: "+itoa(ports[2].TestPort)) || !strings.Contains(text, `value: "$${LITERAL}"`) {
		t.Errorf("test render:\n%s", text)
	}

	again := mustRender(t, r)
	if !slices.Equal(again.Ports(), ports) || !again.SameMapping(first) || again.TestDigest != first.TestDigest {
		t.Errorf("a re-render moved the ports: %v, then %v", ports, again.Ports())
	}
	added := strings.Replace(testBundle, "    - name: secure\n", "    - name: extra\n      protocol: http\n      port: 8081\n    - name: secure\n", 1)
	writeTestFile(t, dir, "bundle.yaml", added)
	third := mustRender(t, r)
	got := third.Ports()
	if len(got) != 4 || got[0] != ports[0] || got[1].Name != "extra" || got[2] != ports[1] || got[3] != ports[2] || seen[got[1].TestPort] {
		t.Errorf("after adding a listener: %v, before %v", got, ports)
	}
	if third.SameMapping(first) {
		t.Error("SameMapping reports an added listener as the same mapping")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// TestRenderMemoryStateStore: the Gateway's stateStore becomes driver
// memory, its redis URL reference gone.
func TestRenderMemoryStateStore(t *testing.T) {
	ws := newTestWorkspace(t)
	src := writeTestFile(t, t.TempDir(), "bundle.yaml", testBundle)
	r := NewRenderer(testPipeline(t), ws, Request{Source: src, MemoryStateStore: true})
	rend := mustRender(t, r)
	if rend.TestDigest == rend.Revision || rend.Rewritten {
		t.Errorf("test digest %s, Revision %s, ports rewritten %v", rend.TestDigest, rend.Revision, rend.Rewritten)
	}
	text := swapped(t, r, ws, rend)
	if !strings.Contains(text, "stateStore:\n    driver: memory\n") || strings.Contains(text, "RURALZ_STATE_STORE_URL") {
		t.Errorf("test render:\n%s", text)
	}
	if digestOf(t, ws.Config()) != rend.TestDigest {
		t.Error("the swapped test render does not build to the test digest")
	}
}

// TestRenderOverrides: every secretRef an override names becomes a file
// reference to its copy under the private secret root, its key kept; the
// copies are made at the swap with mode 0600; an unused entry warns; and a
// file reference without an entry fails the render with RZ-CFG-026 at the
// reference.
func TestRenderOverrides(t *testing.T) {
	ws := newTestWorkspace(t)
	dir := t.TempDir()
	src := writeTestFile(t, dir, "bundle.yaml", testBundle)
	keys := writeTestFile(t, t.TempDir(), "keys.json", `{"pem":"key-canary"}`)
	writeTestFile(t, dir, "local/tls.crt", "cert-canary")
	writeTestFile(t, dir, "local/redis-url", "redis://127.0.0.1:6379")
	writeTestFile(t, dir, "local/unused", "x")
	overrides := writeTestFile(t, dir, "overrides.yaml", "file:/etc/ruralz/tls/tls.crt: local/tls.crt\n"+
		"file:/etc/ruralz/tls/keys.json: "+keys+"\n"+
		"env:RURALZ_STATE_STORE_URL: local/redis-url\n"+
		"env:RURALZ_SECRET_UNUSED: local/unused\n")
	r := NewRenderer(testPipeline(t), ws, Request{Source: src, Overrides: overrides})
	rend := mustRender(t, r)
	wantWarn := []string{"warning: --secret-overrides " + overrides + ": env:RURALZ_SECRET_UNUSED overrides no secretRef"}
	if !slices.Equal(rend.Warnings, wantWarn) {
		t.Errorf("warnings %q, want %q", rend.Warnings, wantWarn)
	}
	if rend.TestDigest == rend.Revision {
		t.Error("rewritten references kept the Revision digest")
	}
	cert := filepath.Join(ws.SecretRoot(), "file", "etc", "ruralz", "tls", "tls.crt")
	key := filepath.Join(ws.SecretRoot(), "file", "etc", "ruralz", "tls", "keys.json")
	url := filepath.Join(ws.SecretRoot(), "env", "RURALZ_STATE_STORE_URL")
	if _, err := os.Stat(cert); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a copy exists before the swap: %v", err)
	}
	text := swapped(t, r, ws, rend)
	for _, ref := range []string{
		`certificate:\n\s+secretRef:\n\s+provider: file\n\s+name: ` + regexp.QuoteMeta(quoted(cert)) + `\n`,
		`privateKey:\n\s+secretRef:\n\s+provider: file\n\s+name: ` + regexp.QuoteMeta(quoted(key)) + `\n\s+key: pem\n`,
		`url:\n\s+secretRef:\n\s+provider: file\n\s+name: ` + regexp.QuoteMeta(quoted(url)) + `\n`,
	} {
		if !regexp.MustCompile(ref).MatchString(text) {
			t.Errorf("test render lacks %s:\n%s", ref, text)
		}
	}
	for p, want := range map[string]string{cert: "cert-canary", key: `{"pem":"key-canary"}`, url: "redis://127.0.0.1:6379"} {
		got, err := os.ReadFile(p) //nolint:gosec // G304: a copy in the test's private directory.
		if err != nil || string(got) != want {
			t.Errorf("copy %s = %q, %v; want %q", p, got, err, want)
		}
		if fi, err := os.Stat(p); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("copy %s has mode %v, want 0600", p, fi.Mode().Perm())
		}
	}
	if digestOf(t, ws.Config()) != rend.TestDigest {
		t.Error("the swapped test render does not build to the test digest")
	}

	writeTestFile(t, dir, "local/tls.crt", "rotated-canary")
	if err := r.RefreshSecrets(mustRender(t, r)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(cert); string(got) != "rotated-canary" { //nolint:gosec // G304: a copy in the test's private directory.
		t.Errorf("RefreshSecrets left %q", got)
	}

	writeTestFile(t, dir, "overrides.yaml", "env:RURALZ_STATE_STORE_URL: local/redis-url\n")
	_, err := r.Render(t.Context())
	var f *Failure
	if !errors.As(err, &f) || len(f.Diagnostics) != 2 {
		t.Fatalf("Render without file entries = %v", err)
	}
	for i, ref := range []string{"file:/etc/ruralz/tls/tls.crt", "file:/etc/ruralz/tls/keys.json"} {
		d := f.Diagnostics[i]
		want := "secretRef " + ref + " has no entry in --secret-overrides " + overrides + "; with overrides every file reference needs one"
		if d.Code != codeSecret || d.Severity != diag.SeverityError || d.Message != want || d.File == "" || d.Line == 0 ||
			d.Resource == nil || d.Resource.Kind != "Gateway" || d.Resource.Name != "edge" {
			t.Errorf("diagnostic %d = %+v, want %q at the reference", i, d, want)
		}
	}
	if f.Error() != "the render failed with 2 errors" {
		t.Errorf("Failure.Error() = %q", f.Error())
	}
}

// consumerKeyBundle is a Bundle whose Consumer holds an API key by a file
// secretRef: the secret field is the SecretRef itself, not a SecretValue
// holding one.
const consumerKeyBundle = `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: public
      protocol: http
      port: 8080
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: hello
spec:
  protocol: http
  endpoints:
    - address: "127.0.0.1:9000"
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: apikey
spec:
  type: auth.api-key
  config: {header: x-api-key}
---
apiVersion: ruralz/v1alpha1
kind: Consumer
metadata:
  name: partner
spec:
  credentials:
    apiKeys:
      - name: primary
        secretRef: {provider: file, name: /etc/ruralz/keys/partner}
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: hello
spec:
  match:
    path: {prefix: /}
  policies:
    - name: apikey
  upstreams:
    - name: hello
`

// TestRenderOverridesConsumerKey: a Consumer API key's secretRef, a secret
// field that is a SecretRef itself, takes its override like any other
// reference, and without an entry fails the render with RZ-CFG-026 at the
// Consumer.
func TestRenderOverridesConsumerKey(t *testing.T) {
	ws := newTestWorkspace(t)
	dir := t.TempDir()
	src := writeTestFile(t, dir, "bundle.yaml", consumerKeyBundle)
	writeTestFile(t, dir, "local/partner-key", "key-canary")
	overrides := writeTestFile(t, dir, "overrides.yaml", "file:/etc/ruralz/keys/partner: local/partner-key\n")
	r := NewRenderer(testPipeline(t), ws, Request{Source: src, Overrides: overrides})
	rend := mustRender(t, r)
	if rend.TestDigest == rend.Revision {
		t.Error("the rewritten API key reference kept the Revision digest")
	}
	key := filepath.Join(ws.SecretRoot(), "file", "etc", "ruralz", "keys", "partner")
	text := swapped(t, r, ws, rend)
	ref := `secretRef:\n\s+provider: file\n\s+name: ` + regexp.QuoteMeta(quoted(key)) + `\n`
	if !regexp.MustCompile(ref).MatchString(text) || strings.Contains(text, "name: /etc/ruralz/keys/partner\n") {
		t.Errorf("the test render does not name the copy %s:\n%s", key, text)
	}
	if got, err := os.ReadFile(key); err != nil || string(got) != "key-canary" { //nolint:gosec // G304: a copy in the test's private directory.
		t.Errorf("copy %s = %q, %v", key, got, err)
	}
	if digestOf(t, ws.Config()) != rend.TestDigest {
		t.Error("the swapped test render does not build to the test digest")
	}

	writeTestFile(t, dir, "overrides.yaml", "env:RURALZ_SECRET_OTHER: local/partner-key\n")
	_, err := r.Render(t.Context())
	var f *Failure
	if !errors.As(err, &f) || len(f.Diagnostics) != 1 {
		t.Fatalf("Render without the key's entry = %v", err)
	}
	d := f.Diagnostics[0]
	want := "secretRef file:/etc/ruralz/keys/partner has no entry in --secret-overrides " + overrides + "; with overrides every file reference needs one"
	if d.Code != codeSecret || d.Message != want || d.Resource == nil || d.Resource.Kind != "Consumer" || d.Resource.Name != "partner" {
		t.Errorf("diagnostic = %+v, want %q at Consumer/partner", d, want)
	}
}

// quoted returns the YAML form of a path in the test render: bare when the
// encoder writes it plain, else double-quoted.
func quoted(p string) string {
	if strings.ContainsAny(p, `\: `) {
		return `"` + strings.ReplaceAll(p, `\`, `\\`) + `"`
	}
	return p
}

// TestRenderOverridesJSON: the overrides file may be JSON.
func TestRenderOverridesJSON(t *testing.T) {
	ws := newTestWorkspace(t)
	dir := t.TempDir()
	src := writeTestFile(t, dir, "bundle.yaml", strings.Replace(testBundle, "  stateStore:\n    driver: redis\n    url: {secretRef: {provider: env, name: RURALZ_STATE_STORE_URL}}\n", "", 1))
	writeTestFile(t, dir, "a", "a")
	writeTestFile(t, dir, "b", `{"pem":"b"}`)
	overrides := writeTestFile(t, dir, "overrides.json", `{"file:/etc/ruralz/tls/tls.crt": "a", "file:/etc/ruralz/tls/keys.json": "b"}`)
	rend := mustRender(t, NewRenderer(testPipeline(t), ws, Request{Source: src, Overrides: overrides}))
	if len(rend.Warnings) != 0 || len(rend.secrets) != 2 {
		t.Errorf("warnings %q, copies %v", rend.Warnings, rend.secrets)
	}
}

// TestRenderOverridesUnusable: an overrides file that cannot be read or
// used exits 2 naming the problem, at CheckOverrides and at a render.
func TestRenderOverridesUnusable(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "present", "x")
	src := writeTestFile(t, dir, "bundle.yaml", testBundle)
	for _, c := range []struct {
		name, content, want string
	}{
		{"not a pair", "nocolon: present\n", `key "nocolon" is not a secretRef provider:name pair`},
		{"empty provider", ":name: present\n", `key ":name" is not a secretRef provider:name pair`},
		{"empty name", "file:: present\n", `key "file:" is not a secretRef provider:name pair`},
		{"not a path", "file:/a: 1\n", `the value of "file:/a" is not a file path`},
		{"empty path", "file:/a: \"\"\n", `the value of "file:/a" is not a file path`},
		{"missing file", "file:/a: absent\n", `the file of "file:/a": `},
		{"directory", "file:/a: .\n", `the file of "file:/a": not a regular file`},
		{"escaping name", "file:/../../x: present\n", `cannot place secretRef file:/../../x under the private secret root`},
		{"parse error", "a: [\n", "RZ-CFG-001"},
		{"two documents", "a: b\n---\nc: d\n", "want one object of provider:name keys and file paths, found 2 documents"},
	} {
		overrides := writeTestFile(t, dir, "overrides.yaml", c.content)
		for _, err := range []error{
			CheckOverrides(t.Context(), overrides),
			func() error {
				_, err := NewRenderer(testPipeline(t), newTestWorkspace(t), Request{Source: src, Overrides: overrides}).Render(t.Context())
				return err
			}(),
		} {
			if command.Code(t.Context(), err) != command.ExitNoResult || !strings.Contains(err.Error(), c.want) ||
				!strings.HasPrefix(err.Error(), "--secret-overrides "+overrides+": ") {
				t.Errorf("%s: %v, want exit 2 with %q", c.name, err, c.want)
			}
		}
	}
	missing := filepath.Join(dir, "absent.yaml")
	if err := CheckOverrides(t.Context(), missing); command.Code(t.Context(), err) != command.ExitNoResult || !strings.HasPrefix(err.Error(), "--secret-overrides "+missing+": ") {
		t.Errorf("a missing overrides file: %v", err)
	}
	if got := OverrideFiles(t.Context(), missing); !slices.Equal(got, []string{missing}) {
		t.Errorf("OverrideFiles of a missing file = %q", got)
	}
}

// TestOverrideFiles: the overrides file and every file it names, resolved
// against the file's directory.
func TestOverrideFiles(t *testing.T) {
	dir := t.TempDir()
	a := writeTestFile(t, dir, "secrets/a", "a")
	abs := writeTestFile(t, t.TempDir(), "b", "b")
	overrides := writeTestFile(t, dir, "overrides.yaml", "file:/a: secrets/a\nenv:B: "+abs+"\n")
	if got, want := OverrideFiles(t.Context(), overrides), []string{overrides, a, abs}; !slices.Equal(got, want) {
		t.Errorf("OverrideFiles = %q, want %q", got, want)
	}
}

// TestRenderFailure: an invalid Bundle fails the render with its
// diagnostics and stages nothing.
func TestRenderFailure(t *testing.T) {
	ws := newTestWorkspace(t)
	src := writeTestFile(t, t.TempDir(), "bundle.yaml", strings.Replace(testBundle, "    - name: hello\n", "    - name: missing\n", 1))
	_, err := NewRenderer(testPipeline(t), ws, Request{Source: src}).Render(t.Context())
	var f *Failure
	if !errors.As(err, &f) || !f.Diagnostics.HasErrors() || f.Error() != "the render failed with 1 error" {
		t.Fatalf("Render = %v", err)
	}
	if _, err := os.Stat(ws.staged()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed render staged a file: %v", err)
	}
}

// TestRenderSourceUnreadable: a source that cannot be read exits 2.
func TestRenderSourceUnreadable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	_, err := NewRenderer(testPipeline(t), newTestWorkspace(t), Request{Source: missing}).Render(t.Context())
	if command.Code(t.Context(), err) != command.ExitNoResult || !strings.HasPrefix(err.Error(), "cannot read "+missing+": ") {
		t.Fatalf("Render = %v", err)
	}
}

// TestRenderEnvironment: --env renders the Environment's overlay with its
// variables; without it the process environment supplies ${VAR}.
func TestRenderEnvironment(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "bundle")
	writeTestFile(t, bundleDir, "ruralz.yaml", strings.Replace(testBundle, `"127.0.0.1:9000"`, `"${UPSTREAM}"`, 1))
	writeTestFile(t, bundleDir, "overlays/prod/upstream.yaml", "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: hello\nspec:\n  protocol: http\n  endpoints:\n    - address: \"127.0.0.3:9000\"\n")
	envs := writeTestFile(t, dir, "environments.yaml", "apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: prod\nspec:\n  overlay: prod\n  variables:\n    UPSTREAM: \"127.0.0.2:9000\"\n")
	ws := newTestWorkspace(t)
	prod := mustRender(t, NewRenderer(testPipeline(t), ws, Request{Source: bundleDir, Environments: envs, Environment: "prod"}))
	text := swapped(t, NewRenderer(testPipeline(t), ws, Request{Source: bundleDir}), ws, prod)
	if !strings.Contains(text, `"127.0.0.3:9000"`) {
		t.Errorf("the prod render lacks its overlay:\n%s", text)
	}
	local := mustRender(t, NewRenderer(testPipeline(t), ws, Request{Source: bundleDir, Variables: subst.Environ([]string{"UPSTREAM=127.0.0.4:9000"})}))
	text = swapped(t, NewRenderer(testPipeline(t), ws, Request{Source: bundleDir}), ws, local)
	if !strings.Contains(text, `"127.0.0.4:9000"`) || bytes.Contains([]byte(text), []byte("${UPSTREAM}")) {
		t.Errorf("the render without --env does not substitute the process environment:\n%s", text)
	}
	if prod.Revision == local.Revision {
		t.Error("two renders with different values share a Revision")
	}
}
