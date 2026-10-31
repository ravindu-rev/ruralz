// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Tests for the Nodes part of the process harness
// (docs/engineering/03-testing-and-quality-strategy.md "Process harness"):
// each Node's private test render, its secrets, its environment, its
// admin credentials and the readiness wait.

// Each Node serves its own test render: every listener port and admin.port
// rewritten to a free port, every secretRef a file under its own
// RURALZ_SECRET_ROOT keeping its key, and its test digest reported beside
// the Revision digest.
func TestNodesServeTheirOwnTestRender(t *testing.T) {
	topo := standinTopology(t)
	nodes := addNodes(t, topo, 2, nodeOptions(t, topo))
	revision, _ := standinDigest([]byte(testBundle))
	seen := map[int]string{}
	for _, n := range nodes {
		r := n.Render()
		if r.Revision != revision {
			t.Errorf("%s Revision = %s, want the unmodified render's %s", n.Name(), r.Revision, revision)
		}
		if want, _ := standinDigest(r.Bytes); r.TestDigest != want || r.TestDigest == r.Revision {
			t.Errorf("%s TestDigest = %s, want %s, unequal to the Revision", n.Name(), r.TestDigest, want)
		}
		onDisk, err := os.ReadFile(n.ConfigPath())
		if err != nil || !bytes.Equal(onDisk, r.Bytes) {
			t.Fatalf("%s RURALZ_CONFIG holds %q (%v), want the test render", n.Name(), onDisk, err)
		}
		if len(r.Listeners) != 2 || r.Listeners[0].Name != "http" || r.Listeners[0].AuthoredPort != 8080 ||
			r.Listeners[1].Name != "https" || r.Listeners[1].AuthoredPort != 8443 || r.AuthoredAdminPort != 9901 {
			t.Fatalf("%s render %+v", n.Name(), r)
		}
		for _, p := range []int{r.Listeners[0].Port, r.Listeners[1].Port, r.AdminPort} {
			if other, dup := seen[p]; dup {
				t.Errorf("port %d handed to %s and %s", p, other, n.Name())
			}
			seen[p] = n.Name()
		}
		if p, _ := n.Port("http"); p != r.Listeners[0].Port || n.AdminPort() != r.AdminPort {
			t.Errorf("%s Port/AdminPort disagree with its render", n.Name())
		}
		want := "http: 8080 -> " + strconv.Itoa(r.Listeners[0].Port) + "\nhttps: 8443 -> " + strconv.Itoa(r.Listeners[1].Port) +
			"\nadmin: 9901 -> " + strconv.Itoa(r.AdminPort) + "\n"
		if got := r.Mapping(); got != want {
			t.Errorf("%s Mapping = %q, want %q", n.Name(), got, want)
		}
		checkRewrittenRefs(t, n, r)

		doc, status, err := n.Readyz(t.Context())
		if err != nil || status != http.StatusOK || doc.Revision != DisplayRevision(r.TestDigest) {
			t.Errorf("%s /readyz = %d %+v (%v), want ready on the test digest", n.Name(), status, doc, err)
		}
		resp := get(t, topo.HTTPClient(), n.URL("http")+"/")
		if got := resp.Header.Get("X-Standin-Revision"); got != DisplayRevision(r.TestDigest) {
			t.Errorf("%s serves %s, want its test render", n.Name(), got)
		}
		// The https listener serves the certificate its secrets hold,
		// canary lines and all, trusted through the harness's CA.
		if resp := get(t, topo.HTTPClient(), n.URL("https")+"/"); resp.StatusCode != http.StatusOK || resp.TLS == nil {
			t.Errorf("%s https answered %d over TLS %v", n.Name(), resp.StatusCode, resp.TLS != nil)
		}
	}
}

// A literal "${" in a rendered Bundle is written "$${", so substitution
// leaves it alone; the test render keeps that escape as it is instead of
// escaping it again, so the Node serves the value the Bundle holds.
func TestTestRenderKeepsSubstitutionEscapes(t *testing.T) {
	topo := standinTopology(t)
	o := nodeOptions(t, topo)
	o.Bundle = []byte(testBundle + `---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: literal
spec:
  type: headers
  config:
    request:
      set:
        - name: x-literal
          value: "$${HOME}"
`)
	n := addNodes(t, topo, 1, o)[0]
	if got := string(n.Render().Bytes); !strings.Contains(got, `value: "$${HOME}"`) || strings.Contains(got, "$$${") {
		t.Errorf("test render holds\n%s\nwant the escape $${HOME} kept as it is", got)
	}
}

// A Consumer API key's secretRef is a SecretRef field, the reference
// itself rather than a {secretRef: ...} value, and the test render
// rewrites it like every other reference. References
// NodeOptions.AuthoredSecrets lists stay as authored, by their exact
// reference or by provider and name for every key, and the harness writes
// no file for them.
func TestTestRenderRewritesSecretRefFieldsAndKeepsAuthored(t *testing.T) {
	topo := standinTopology(t)
	o := nodeOptions(t, topo)
	o.Bundle = []byte(testBundle + `---
apiVersion: ruralz/v1alpha1
kind: Consumer
metadata:
  name: app
spec:
  credentials:
    apiKeys:
      - name: rewritten
        secretRef: {provider: file, name: /etc/ruralz/keys/app}
      - name: authored-file
        secretRef: {provider: file, name: /etc/ruralz/keys/authored}
      - name: authored-keyed
        secretRef: {provider: file, name: /etc/ruralz/keys/all.json, key: one}
      - name: authored-env
        secretRef: {provider: env, name: RURALZ_SECRET_APP_KEY}
`)
	o.AuthoredSecrets = []string{"file:/etc/ruralz/keys/authored", "file:/etc/ruralz/keys/all.json", "env:RURALZ_SECRET_APP_KEY"}
	o.NoWait = true
	n := addNodes(t, topo, 1, o)[0]
	r := n.Render()
	docs, diags, err := profile.Parse(t.Context(), r.Bytes, 1, "render", profile.FormatYAML, profile.Options{})
	if err != nil || len(diags) > 0 {
		t.Fatalf("the test render does not parse: %v %s", err, diagText(diags))
	}
	var refs []*tree.Node
	for _, d := range docs {
		collectRefs(d.Root, &refs)
	}
	got := map[string]string{}
	for _, ref := range refs {
		got[text(ref, "provider")+":"+text(ref, "name")+"#"+text(ref, "key")] = ""
	}
	for _, kept := range []string{"file:/etc/ruralz/keys/authored#", "file:/etc/ruralz/keys/all.json#one", "env:RURALZ_SECRET_APP_KEY#"} {
		if _, ok := got[kept]; !ok {
			t.Errorf("authored reference %s not kept; the render holds %v", kept, got)
		}
	}
	path, ok := n.SecretPath("file:/etc/ruralz/keys/app")
	if !ok || !strings.HasPrefix(path, n.SecretRoot()+string(filepath.Separator)) {
		t.Fatalf("the API key's secretRef maps to %q (%v), want a file under the secret root", path, ok)
	}
	if _, ok := got["file:"+path+"#"]; !ok {
		t.Errorf("the render does not name %s for the API key; it holds %v", path, got)
	}
	if v, err := os.ReadFile(path); err != nil || !topo.canaries.carries(v) { //nolint:gosec // G304: the harness's own secret file
		t.Errorf("the API key's file holds %q (%v), want a canary", v, err)
	}
	for _, s := range r.Secrets {
		if strings.Contains(s.Ref, "authored") || strings.Contains(s.Ref, "all.json") || strings.HasPrefix(s.Ref, "env:RURALZ_SECRET_APP_KEY") {
			t.Errorf("the render lists the authored reference %s among the harness's secrets", s.Ref)
		}
	}
	for _, name := range []string{"/etc/ruralz/keys/authored", "/etc/ruralz/keys/all.json"} {
		p, err := secretPath(n.SecretRoot(), "file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the harness wrote %s for an authored reference (%v)", p, err)
		}
	}
}

// checkRewrittenRefs checks that the test render names only file
// references under the Node's secret root, keyed ones keeping their key,
// and that each file holds a registered canary.
func checkRewrittenRefs(t *testing.T, n *Node, r *Render) {
	t.Helper()
	docs, diags, err := profile.Parse(t.Context(), r.Bytes, 1, "render", profile.FormatYAML, profile.Options{})
	if err != nil || len(diags) > 0 {
		t.Fatalf("the test render does not parse: %v %s", err, diagText(diags))
	}
	var refs []*tree.Node
	for _, d := range docs {
		collectRefs(d.Root, &refs)
	}
	if len(refs) != 6 {
		t.Fatalf("%s test render holds %d references, want 6", n.Name(), len(refs))
	}
	for _, ref := range refs {
		name := text(ref, "name")
		if text(ref, "provider") != "file" || !strings.HasPrefix(name, n.SecretRoot()+string(filepath.Separator)) {
			t.Errorf("%s reference %s:%s is not a file under its secret root", n.Name(), text(ref, "provider"), name)
		}
	}
	byRef := map[string]SecretRef{}
	for _, s := range r.Secrets {
		byRef[s.Ref] = s
	}
	for _, ref := range []string{refCert, refKey, refStoreURL, refCredsCA, refCredsKey} {
		s, ok := byRef[ref]
		if !ok {
			t.Errorf("%s render lists no %s", n.Name(), ref)
			continue
		}
		if path, _ := n.SecretPath(ref); path != s.Path {
			t.Errorf("SecretPath(%s) = %s, want %s", ref, path, s.Path)
		}
		v, ok := n.SecretValue(ref)
		if !ok || !n.t.canaries.carries(v) {
			t.Errorf("%s secret %s = %q, want a value carrying a canary", n.Name(), ref, v)
		}
	}
	if got, want := byRef[refStoreURL].Path, filepath.Join(n.SecretRoot(), "env", "RURALZ_STATE_STORE_URL"); got != want {
		t.Errorf("env reference maps to %s, want %s", got, want)
	}
	// The keyed references share one file, a JSON object of their keys.
	data, err := os.ReadFile(byRef[refCredsCA].Path)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]string
	if err := json.Unmarshal(data, &obj); err != nil || len(obj) != 3 {
		t.Fatalf("%s creds.json = %s (%v), want a JSON object of three keys", n.Name(), data, err)
	}
	if v, _ := n.SecretValue(refCredsCA); obj["ca"] != string(v) {
		t.Errorf("creds.json ca = %q, want %q", obj["ca"], v)
	}
	if fi, err := os.Stat(byRef[refKey].Path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode %v (%v), want 0600", fi.Mode(), err)
	}
}

// Every secret the harness supplies carries a canary: a value a test
// supplies must carry one; and the render refuses Bundles it cannot
// rewrite.
func TestAddNodesRefuses(t *testing.T) {
	gateway := "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: gw\nspec:\n  listeners:\n    - {name: http, protocol: http, port: 8080}\n"
	upstream := "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: up\nspec:\n  protocol: http\n  endpoints: [{address: \"127.0.0.1:9000\"}]\n"
	tests := []struct {
		name string
		mod  func(*NodeOptions)
		want string
	}{
		{"empty Bundle", func(o *NodeOptions) { o.Bundle = nil }, "Bundle is empty"},
		{"no Gateway", func(o *NodeOptions) { o.Bundle = []byte(upstream) }, "holds 0 Gateways"},
		{"two Gateways", func(o *NodeOptions) { o.Bundle = []byte(gateway + "---\n" + gateway) }, "holds 2 Gateways"},
		{"unparsable", func(o *NodeOptions) { o.Bundle = []byte("kind: [") }, "does not parse"},
		{"unserved apiVersion", func(o *NodeOptions) {
			o.Bundle = []byte(strings.Replace(gateway, "ruralz/v1alpha1", "ruralz/v9", 1))
		}, "unserved apiVersion"},
		{"secret without canary", func(o *NodeOptions) { o.Secrets[refCert] = []byte("-----BEGIN CERTIFICATE-----") }, "carries no canary"},
		{"MAC key without canary", func(o *NodeOptions) { o.MACKey = bytes.Repeat([]byte("k"), 32) }, "MACKey carries no canary"},
		{"secret variable without canary", func(o *NodeOptions) { o.Env = []string{"RURALZ_SECRET_TOKEN=plain"} }, "RURALZ_SECRET_TOKEN carries no canary"},
		{"unknown State Store", func(o *NodeOptions) { o.StateStoreURLs = map[string]string{refStoreURL: "nowhere"} }, `names no State Store "nowhere"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			topo := standinTopology(t)
			o := nodeOptions(t, topo)
			tt.mod(&o)
			_, err := topo.AddNodes(t.Context(), 1, o)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("AddNodes = %v, want an error naming %q", err, tt.want)
			}
		})
	}
	if _, err := standinTopology(t).AddNodes(t.Context(), 0, NodeOptions{}); err == nil {
		t.Error("AddNodes(0) succeeded")
	}
}

// A reference listed in MissingSecrets gets no value, so the Node cannot
// resolve it and stays unready naming it; the readiness wait gives up
// after Config.ReadyTimeout with the last answer.
func TestMissingSecretsKeepTheNodeUnready(t *testing.T) {
	topo := standinTopology(t, func(c *Config) { c.ReadyTimeout = 2 * time.Second })
	o := nodeOptions(t, topo)
	o.MissingSecrets = []string{"file:/etc/ruralz/tls/tls.key", refCredsCA}
	o.NoWait = true
	n := addNodes(t, topo, 1, o)[0]
	keyPath, _ := n.SecretPath(refKey)
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Errorf("the missing secret's file exists: %v", err)
	}
	if _, ok := n.SecretValue(refCredsCA); ok {
		t.Error("the missing keyed secret has a value")
	}
	err := n.WaitReady(t.Context())
	if err == nil || !strings.Contains(err.Error(), "not ready within 2s") || !strings.Contains(err.Error(), adminapi.ReasonSecretsUnresolved) {
		t.Fatalf("WaitReady = %v, want a timeout naming the unresolved secrets", err)
	}
	doc, status, err := n.Readyz(t.Context())
	if err != nil || status != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, %v", status, err)
	}
	var unresolved []string
	for _, r := range doc.Reasons {
		if r.Reason == adminapi.ReasonSecretsUnresolved && r.Code == "RZ-CFG-026" {
			unresolved = append(unresolved, r.Detail)
		}
	}
	if len(unresolved) != 2 {
		t.Errorf("unresolved references %v, want the key file and the ca key", unresolved)
	}
}

// A readiness wait that gives up reports the last answer /readyz gave, not
// the poll its own deadline cut off: here every /readyz after the first
// hangs until the wait ends.
func TestWaitReadyReportsTheLastAnswer(t *testing.T) {
	topo := standinTopology(t, func(c *Config) { c.ReadyTimeout = 2 * time.Second })
	o := nodeOptions(t, topo)
	o.MissingSecrets = []string{refCredsCA}
	o.Env = []string{standinHangReadyzEnv + "=1"}
	o.NoWait = true
	n := addNodes(t, topo, 1, o)[0]
	err := n.WaitReady(t.Context())
	if err == nil || !strings.Contains(err.Error(), "last /readyz: 503") || !strings.Contains(err.Error(), adminapi.ReasonSecretsUnresolved) {
		t.Fatalf("WaitReady = %v, want the last answer, a 503 naming the unresolved secrets", err)
	}
}

// A Node process that exits before it is ready fails at once, with its
// output.
func TestNodeExitingBeforeReadyFails(t *testing.T) {
	topo := standinTopology(t)
	o := nodeOptions(t, topo)
	o.Env = []string{envBindLoopback + "=false"} // the stand-in refuses to start
	start := time.Now()
	_, err := topo.AddNodes(t.Context(), 1, o)
	if err == nil || !strings.Contains(err.Error(), "RURALZ_BIND_LOOPBACK=true are required") {
		t.Fatalf("AddNodes = %v, want the process's refusal", err)
	}
	if time.Since(start) > DefaultReadyTimeout {
		t.Errorf("the failure took %v, the whole readiness wait", time.Since(start))
	}
}

// A Node process inherits nothing implicitly: the harness's settings,
// GOMAXPROCS max(1, CPUs / Nodes) with more than one Node, then Config.Env
// and NodeOptions.Env.
func TestNodeEnvironment(t *testing.T) {
	t.Setenv("HOME", "/inherited")
	topo := standinTopology(t)
	nodes := addNodes(t, topo, 2, nodeOptions(t, topo))
	m := waitLine(t, nodes[0], `standin: env GOMAXPROCS=(\S*) procs=(\d+) RURALZ_BIND_LOOPBACK=(\S*) RURALZ_FETCH_ALLOW=(\S*) SSL_CERT_FILE=(\S*) HOME=(\S*)\n`)
	if want := strconv.Itoa(max(1, runtime.NumCPU()/2)); m[1] != want || m[2] != want {
		t.Errorf("GOMAXPROCS = %q (runtime %s), want %s for two Nodes", m[1], m[2], want)
	}
	if m[3] != "true" || m[4] != "127.0.0.0/8,::1" || m[5] != topo.bundle || m[6] != "" {
		t.Errorf("env = %q, want loopback binding and fetches, the CA bundle and no inherited HOME", m[3:])
	}
	o := nodeOptions(t, topo)
	o.GOMAXPROCS = 1
	third := addNodes(t, topo, 1, o)[0]
	if m := waitLine(t, third, `standin: env GOMAXPROCS=(\S*) `); m[1] != "1" {
		t.Errorf("GOMAXPROCS override = %q, want 1", m[1])
	}

	single := standinTopology(t)
	one := addNodes(t, single, 1, nodeOptions(t, single))[0]
	if m := waitLine(t, one, `standin: env GOMAXPROCS=(\S*) `); m[1] != "" {
		t.Errorf("a lone Node got GOMAXPROCS=%q, want the Go default", m[1])
	}
}

// The admin port takes the harness's operator and metrics tokens over TLS
// from its CA, as the real admin authentication checks them.
func TestAdminCredentials(t *testing.T) {
	topo := standinTopology(t)
	n := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	if !strings.HasPrefix(n.AdminURL(), "https://127.0.0.1:") {
		t.Errorf("AdminURL = %s, want https on loopback", n.AdminURL())
	}
	for _, tt := range []struct {
		path   string
		cred   Credential
		status int
	}{
		{"/metrics", Operator, http.StatusOK},
		{"/metrics", Metrics, http.StatusOK},
		{"/metrics", Anonymous, http.StatusUnauthorized},
		{"/debug/snapshots", Operator, http.StatusOK},
		{"/debug/snapshots", Metrics, http.StatusUnauthorized},
		{"/healthz", Anonymous, http.StatusOK},
	} {
		resp, err := n.AdminAs(t.Context(), tt.path, tt.cred)
		if err != nil || resp.Status != tt.status {
			t.Errorf("AdminAs(%s, %d) = %d, %v; want %d", tt.path, tt.cred, resp.Status, err, tt.status)
		}
	}
	if resp, err := n.Admin(t.Context(), "/metrics"); err != nil || !bytes.Contains(resp.Body, []byte("ruralz_config_activations_total")) {
		t.Errorf("/metrics = %q, %v", resp.Body, err)
	}

	cleartext := standinTopology(t, func(c *Config) { c.Transport.CleartextAdmin = true })
	cn := addNodes(t, cleartext, 1, nodeOptions(t, cleartext))[0]
	if !strings.HasPrefix(cn.AdminURL(), "http://127.0.0.1:") {
		t.Errorf("cleartext AdminURL = %s", cn.AdminURL())
	}
	if resp, err := cn.Admin(t.Context(), "/debug/snapshots"); err != nil || resp.Status != http.StatusOK {
		t.Errorf("cleartext admin = %d, %v", resp.Status, err)
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	good := standinCfg(t)
	for _, tt := range []struct {
		name string
		mod  func(*Config)
	}{
		{"no binary", func(c *Config) { c.Binary = "" }},
		{"missing binary", func(c *Config) { c.Binary = filepath.Join(t.TempDir(), "none") }},
		{"no digest", func(c *Config) { c.Digest = nil }},
		{"negative ready timeout", func(c *Config) { c.ReadyTimeout = -1 }},
		{"negative kill delay", func(c *Config) { c.KillAfter = -1 }},
	} {
		c := good
		tt.mod(&c)
		if topo, err := newTopology(t, c); err == nil {
			t.Errorf("%s: newTopology succeeded", tt.name)
			topo.teardown()
		}
	}
}

func TestSecretPath(t *testing.T) {
	root := filepath.FromSlash("/root/secrets")
	for _, tt := range []struct {
		provider, name, want string
	}{
		{"file", "/etc/ruralz/tls/tls.key", "/root/secrets/file/etc/ruralz/tls/tls.key"},
		{"env", "RURALZ_STATE_STORE_URL", "/root/secrets/env/RURALZ_STATE_STORE_URL"},
		{"kubernetes", "redis-auth", "/root/secrets/kubernetes/redis-auth"},
		{"vault", "secret/data/app", "/root/secrets/vault/secret/data/app"},
	} {
		got, err := secretPath(root, tt.provider, tt.name)
		if err != nil || got != filepath.FromSlash(tt.want) {
			t.Errorf("secretPath(%s, %s) = %s, %v; want %s", tt.provider, tt.name, got, err, tt.want)
		}
	}
	for _, bad := range [][2]string{{"file", "/../../etc/passwd"}, {"file", ""}, {"", "x"}, {"../file", "x"}, {"vault", "a/../../b"}} {
		if got, err := secretPath(root, bad[0], bad[1]); err == nil {
			t.Errorf("secretPath(%q, %q) = %s, want a refusal", bad[0], bad[1], got)
		}
	}
}

func TestDisplayRevision(t *testing.T) {
	d := "sha256:162af81f5de482d540e09cf25205ffe39b705fd2dcf5eca88eb42e92b098a85e"
	if got := DisplayRevision(d); got != "rev-162af81f5de4" {
		t.Errorf("DisplayRevision = %q", got)
	}
	for _, bad := range []string{"", "rev-162af81f5de4", "sha256:abc"} {
		if got := DisplayRevision(bad); got != "" {
			t.Errorf("DisplayRevision(%q) = %q, want empty", bad, got)
		}
	}
}

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"TestX/sub case": "TestX_sub_case", "admin-node-1": "admin-node-1", "..": "_..", "": "_", "a\x00b": "a_b",
	} {
		if got := fileName(in); got != want {
			t.Errorf("fileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A topology torn down refuses new Nodes and dependencies.
func TestTornDownTopologyRefusesWork(t *testing.T) {
	topo := standinTopology(t)
	topo.teardown()
	if _, err := topo.AddNodes(context.Background(), 1, nodeOptions(t, topo)); err == nil {
		t.Error("AddNodes after teardown succeeded")
	}
}
