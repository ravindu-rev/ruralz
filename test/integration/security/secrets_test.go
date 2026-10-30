// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Secrets rules 5 and 6 (docs/architecture/08-security-and-identity.md
// "Secrets"): a file reference resolves only under RURALZ_SECRET_ROOT,
// before and after resolving symlinks, and only when neither group nor
// others may write it; an env reference only for RURALZ_STATE_STORE_URL
// and RURALZ_SECRET_*; a violation is RZ-CFG-026, so the Node rejects the
// Revision and keeps its active one. A Node refuses to start when the
// secret root holds its data directory, admin TLS directory, an admin
// token file or the MAC key file, or when RURALZ_FETCH_ALLOW holds an
// unparsable entry. Every Node connection for the Bundle refuses loopback
// addresses unless RURALZ_FETCH_ALLOW holds them, at connect time and on
// each redirect hop; it never follows https to http, nor a tokenUrl
// redirect.

// keyedRoute is the Route /keyed behind auth.api-key, the Consumer app
// whose one API key is held by keyRef (a secretRef flow mapping), and the
// Upstream backend at addr.
func keyedRoute(addr, keyRef string) []byte {
	return bundle(
		gatewayDoc(webListener),
		upstreamDoc("backend", addr, ""),
		policyDoc("apikey", "auth.api-key", "    header: x-api-key\n"),
		"apiVersion: ruralz/v1alpha1\nkind: Consumer\nmetadata:\n  name: app\nspec:\n  credentials:\n    apiKeys:\n      - name: main\n        secretRef: "+keyRef+"\n",
		routeDoc("keyed", "/keyed", "backend", "apikey"),
	)
}

// envRef is an env secretRef as a flow mapping.
func envRef(name string) string { return fmt.Sprintf("{provider: env, name: %q}", name) }

// rawFileRef is a file secretRef as a flow mapping, without the
// secretRef key keyedRoute adds.
func rawFileRef(name string) string { return fmt.Sprintf("{provider: file, name: %q}", name) }

// wantRejected swaps the Node's render for rendered and waits until the
// Node logged its rejection with code, then checks that the Node keeps
// serving its Revision and counts the rejection in
// ruralz_config_activations_total, read after the 1 s the Node reuses a
// metrics collection.
func wantRejected(t *testing.T, top *topology.Topology, n *topology.Node, rendered []byte, code string) {
	t.Helper()
	// The Revision /readyz names: the harness's last render may be one
	// the Node rejected before.
	doc, status, err := n.Readyz(t.Context())
	if err != nil || status != http.StatusOK {
		t.Fatalf("/readyz = %d %+v, %v; want ready", status, doc, err)
	}
	counted := freshCounter(t, top, n, "ruralz_config_activations_total", "result", "rejected", "code", code)
	logged := len(logRecords(n, "revision rejected", "code", code))
	r := reload(t, n, rendered)
	waitFor(t, "the rejection with "+code, func() (bool, string) {
		got := len(logRecords(n, "revision rejected", "code", code))
		return got == logged+1, fmt.Sprintf("%d rejections with %s logged", got, code)
	})
	// A rejection after the Revision digest names it.
	if recs := logRecords(n, "revision rejected", "code", code); recs[len(recs)-1]["digest"] != nil && recs[len(recs)-1]["digest"] != r.TestDigest {
		t.Fatalf("the rejection logged %v, want the swapped render's digest %s", recs[len(recs)-1], r.TestDigest)
	}
	if after, status, err := n.Readyz(t.Context()); err != nil || status != http.StatusOK || after.Revision != doc.Revision {
		t.Fatalf("/readyz after the rejection = %d %+v, %v; want ready on %s", status, after, err, doc.Revision)
	}
	if got := freshCounter(t, top, n, "ruralz_config_activations_total", "result", "rejected", "code", code); got != counted+1 {
		t.Fatalf("%v activations rejected with %s counted, want %v", got, code, counted+1)
	}
}

// wantActivated swaps the Node's render for rendered and waits until the
// Node serves it.
func wantActivated(t *testing.T, n *topology.Node, rendered []byte) {
	t.Helper()
	r := reload(t, n, rendered)
	if err := n.WaitRevision(t.Context(), r.TestDigest); err != nil {
		t.Fatal(err)
	}
}

func TestSecretFileAndEnvRules(t *testing.T) {
	top := newTopology(t, topology.Config{})
	mock := startMock(t, top, mockup.Config{})
	// The Node's secret root, the harness's layout: checked below.
	root := filepath.Join(top.Dir(), "node-1", "secrets")
	outside := filepath.Join(top.Dir(), "outside", "app-key")
	link := filepath.Join(root, "link", "app-key")
	groupWritable := filepath.Join(root, "group-writable", "app-key")
	otherWritable := filepath.Join(root, "other-writable", "app-key")
	groupReadable := filepath.Join(root, "group-readable", "app-key")
	fileKey, outsideKey, envKey, otherEnvKey := top.Canary(), top.Canary(), top.Canary(), top.Canary()
	keys := map[string]string{groupWritable: top.Canary(), otherWritable: top.Canary(), groupReadable: top.Canary()}
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{
		Bundle:  keyedRoute(mock.Addr(), rawFileRef("/etc/ruralz/keys/app")),
		Secrets: map[string][]byte{"file:/etc/ruralz/keys/app": []byte(fileKey)},
		AuthoredSecrets: []string{
			"file:" + outside, "file:" + link, "file:" + groupWritable, "file:" + otherWritable, "file:" + groupReadable,
			"env:APP_KEY", "env:RURALZ_SECRET_APP_KEY",
		},
		Env: []string{"RURALZ_SECRET_APP_KEY=" + envKey, "APP_KEY=" + otherEnvKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	n := nodes[0]
	if n.SecretRoot() != root {
		t.Fatalf("secret root %s, want %s", n.SecretRoot(), root)
	}
	c := top.HTTPClient()
	url := n.URL("web") + "/keyed"
	wantKey := func(what, key string) {
		t.Helper()
		wantOK(t, what, get(t, c, url, "x-api-key", key))
	}
	wantKey("key of the harness's file secret", fileKey)

	writeKey := func(path, key string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil { //nolint:gosec // G302: the modes under test, group or other writable included
			t.Fatal(err)
		}
	}
	writeKey(outside, outsideKey, 0o600)
	t.Run("file outside the secret root", func(t *testing.T) {
		wantRejected(t, top, n, keyedRoute(mock.Addr(), rawFileRef(outside)), "RZ-CFG-026")
		wantCode(t, "key outside the root", get(t, c, url, "x-api-key", outsideKey), http.StatusUnauthorized, "RZ-AUTH-002")
		wantKey("active key after the rejection", fileKey)
	})
	t.Run("symlink leaving the secret root", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		wantRejected(t, top, n, keyedRoute(mock.Addr(), rawFileRef(link)), "RZ-CFG-026")
		wantCode(t, "key behind the symlink", get(t, c, url, "x-api-key", outsideKey), http.StatusUnauthorized, "RZ-AUTH-002")
	})
	t.Run("file writable by group or others", func(t *testing.T) {
		writeKey(groupWritable, keys[groupWritable], 0o620)
		wantRejected(t, top, n, keyedRoute(mock.Addr(), rawFileRef(groupWritable)), "RZ-CFG-026")
		writeKey(otherWritable, keys[otherWritable], 0o602)
		wantRejected(t, top, n, keyedRoute(mock.Addr(), rawFileRef(otherWritable)), "RZ-CFG-026")
		wantCode(t, "key of the writable file", get(t, c, url, "x-api-key", keys[groupWritable]), http.StatusUnauthorized, "RZ-AUTH-002")
		// Read permission for the group is no violation.
		writeKey(groupReadable, keys[groupReadable], 0o640)
		wantActivated(t, n, keyedRoute(mock.Addr(), rawFileRef(groupReadable)))
		wantKey("key of the group-readable file", keys[groupReadable])
	})
	t.Run("env names outside the allow list", func(t *testing.T) {
		wantRejected(t, top, n, keyedRoute(mock.Addr(), envRef("APP_KEY")), "RZ-CFG-026")
		wantCode(t, "key of a variable outside the allow list", get(t, c, url, "x-api-key", otherEnvKey), http.StatusUnauthorized, "RZ-AUTH-002")
		wantActivated(t, n, keyedRoute(mock.Addr(), envRef("RURALZ_SECRET_APP_KEY")))
		wantKey("key of a RURALZ_SECRET_ variable", envKey)
	})
}

// refusedStart starts the ruralzd under test with env, its clock command
// socket attached, and returns its exit code and standard error once it
// exits; a process still running after waitTimeout fails t.
func refusedStart(t *testing.T, env []string) (int, string) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	harness, child := os.NewFile(uintptr(fds[0]), "clock harness"), os.NewFile(uintptr(fds[1]), "clock")
	defer func() { _ = harness.Close() }()
	// The clock starts at the start command, which ruralzd reads first.
	if _, err := harness.WriteString(clock.CommandStart + " " + time.Now().UTC().Format(clock.CommandTimeLayout) + "\n"); err != nil {
		t.Fatal(err)
	}
	p, err := proc.Start(t.Context(), proc.Spec{Name: "refused", Path: bins.Ruralzd, Env: env, ExtraFiles: []*os.File{child}})
	_ = child.Close()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	code, err := p.Wait(ctx)
	if err != nil {
		_ = p.Kill()
		<-p.Done()
		t.Fatalf("ruralzd still running %v after start: %v", waitTimeout, err)
	}
	_, stderr := p.Output()
	return code, string(stderr)
}

func TestStartRefusals(t *testing.T) {
	if !bins.TestClock {
		t.Skip("needs the controllable-clock ruralzd build")
	}
	// base returns a working environment in a directory of its own:
	// the configuration, data directory, secret root, admin token and
	// TLS directory, and MAC key, all outside the secret root.
	type setup struct {
		dir, root, data, token, metrics, tlsDir, macKey string
		env                                             map[string]string
	}
	base := func(t *testing.T) *setup {
		t.Helper()
		dir := t.TempDir()
		s := &setup{dir: dir, root: filepath.Join(dir, "secrets"), data: filepath.Join(dir, "data")}
		for _, d := range []string{s.root, s.data, filepath.Join(dir, "admin-tls")} {
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		write := func(name, data string) string {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}
		config := write("bundle.yaml", string(bundle(gatewayDoc(webListener), upstreamDoc("backend", "127.0.0.1:9", ""), routeDoc("r", "/", "backend"))))
		s.token = write("admin-token", "rzcanary-0123456789abcdef0123456789abcdef\n")
		s.metrics = write("metrics-token", "rzcanary-fedcba9876543210fedcba9876543210\n")
		s.macKey = write("mac-key", "rzcanary-00112233445566778899aabbccddeeff\n")
		s.tlsDir = filepath.Join(dir, "admin-tls")
		s.env = map[string]string{
			"RURALZ_CONFIG": config, "RURALZ_DATA_DIR": s.data, "RURALZ_SECRET_ROOT": s.root,
			"RURALZ_ADMIN_TOKEN_FILE": s.token, "RURALZ_ADMIN_METRICS_TOKEN_FILE": s.metrics,
			"RURALZ_STATE_STORE_MAC_KEY_FILE": s.macKey, "RURALZ_BIND_LOOPBACK": "true",
		}
		return s
	}
	// into moves the file or directory a setting names into the secret
	// root and points the setting at it there.
	into := func(t *testing.T, s *setup, setting string) {
		t.Helper()
		from := s.env[setting]
		to := filepath.Join(s.root, filepath.Base(from))
		if err := os.Rename(from, to); err != nil {
			t.Fatal(err)
		}
		s.env[setting] = to
	}
	environ := func(m map[string]string) []string {
		out := make([]string, 0, len(m))
		for k, v := range m {
			out = append(out, k+"="+v)
		}
		return out
	}
	cases := []struct {
		name    string
		prepare func(t *testing.T, s *setup)
		// want names what standard error must mention.
		want []string
	}{
		{"data directory inside the secret root", func(t *testing.T, s *setup) { into(t, s, "RURALZ_DATA_DIR") }, []string{"RURALZ_DATA_DIR", "RURALZ_SECRET_ROOT"}},
		{"secret root is the data directory", func(_ *testing.T, s *setup) { s.env["RURALZ_SECRET_ROOT"] = s.data }, []string{"RURALZ_DATA_DIR", "RURALZ_SECRET_ROOT"}},
		{"admin token file inside the secret root", func(t *testing.T, s *setup) { into(t, s, "RURALZ_ADMIN_TOKEN_FILE") }, []string{"RURALZ_ADMIN_TOKEN_FILE", "RURALZ_SECRET_ROOT"}},
		{"metrics token file inside the secret root", func(t *testing.T, s *setup) { into(t, s, "RURALZ_ADMIN_METRICS_TOKEN_FILE") }, []string{"RURALZ_ADMIN_METRICS_TOKEN_FILE", "RURALZ_SECRET_ROOT"}},
		{"MAC key file inside the secret root", func(t *testing.T, s *setup) { into(t, s, "RURALZ_STATE_STORE_MAC_KEY_FILE") }, []string{"RURALZ_STATE_STORE_MAC_KEY_FILE", "RURALZ_SECRET_ROOT"}},
		{"admin TLS directory inside the secret root", func(t *testing.T, s *setup) {
			s.env["RURALZ_ADMIN_TLS_DIR"] = filepath.Join(s.root, "admin-tls")
		}, []string{"RURALZ_ADMIN_TLS_DIR", "RURALZ_SECRET_ROOT"}},
		{"secret root reached through a symlink", func(t *testing.T, s *setup) {
			alias := filepath.Join(s.dir, "alias")
			if err := os.Symlink(s.dir, alias); err != nil {
				t.Fatal(err)
			}
			s.env["RURALZ_SECRET_ROOT"] = alias
		}, []string{"RURALZ_DATA_DIR", "RURALZ_SECRET_ROOT"}},
		{"unparsable RURALZ_FETCH_ALLOW entry", func(_ *testing.T, s *setup) {
			s.env["RURALZ_FETCH_ALLOW"] = "127.0.0.0/8,not-an-address"
		}, []string{"RURALZ_FETCH_ALLOW"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := base(t)
			c.prepare(t, s)
			code, stderr := refusedStart(t, environ(s.env))
			if code != 2 {
				t.Fatalf("ruralzd exited %d, want 2; stderr:\n%s", code, stderr)
			}
			for _, w := range c.want {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr names no %s:\n%s", w, stderr)
				}
			}
			for _, secret := range []string{"rzcanary-0123", "rzcanary-fedc", "rzcanary-0011"} {
				if strings.Contains(stderr, secret) {
					t.Errorf("stderr repeats a credential:\n%s", stderr)
				}
			}
		})
	}
}

// jwtBundle is the Route /orders behind auth.jwt for issuer with the
// JWKS document at jwksURL, to the Upstream at addr.
func jwtBundle(addr, issuer, jwksURL string) []byte {
	return bundle(
		gatewayDoc(webListener),
		upstreamDoc("backend", addr, ""),
		policyDoc("jwt", "auth.jwt", fmt.Sprintf("    issuers:\n      - issuer: %q\n        jwksUrl: %q\n        audiences: [api]\n", issuer, jwksURL)),
		routeDoc("orders", "/orders", "backend", "jwt"),
	)
}

// oauthBundle is the Route /orders to the Upstream at addr, whose leg
// carries auth.upstream-oauth2 against tokenURL with the client secret
// of secret.
func oauthBundle(addr, tokenURL, secret string) []byte {
	return bundle(
		gatewayDoc(webListener),
		upstreamDoc("backend", addr, "  policies:\n    - name: oauth\n"),
		policyDoc("oauth", "auth.upstream-oauth2", fmt.Sprintf("    tokenUrl: %q\n    clientId: gateway-edge\n    clientSecret: %s\n    scopes: [orders.read]\n", tokenURL, fileRef(secret))),
		routeDoc("orders", "/orders", "backend"),
	)
}

func TestEgressGuardAndRedirects(t *testing.T) {
	// token mints a token for the Route's audience with k, iss issuer.
	token := func(t *testing.T, idp *mockidp.Server, k *mockidp.Key, issuer string) string {
		t.Helper()
		tok, err := idp.MintWith(k, nil, map[string]any{"iss": issuer, "aud": "api", "sub": "alice"})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	t.Run("RURALZ_FETCH_ALLOW without the IdP's address", func(t *testing.T) {
		top := newTopology(t, topology.Config{})
		idp, err := top.StartIdP(mockidp.Config{Now: top.Now})
		if err != nil {
			t.Fatal(err)
		}
		mock := startMock(t, top, mockup.Config{})
		nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{
			Bundle: jwtBundle(mock.Addr(), idp.Issuer(), idp.JWKSURL()), Env: []string{"RURALZ_FETCH_ALLOW=192.0.2.0/24"},
		})
		if err != nil {
			t.Fatal(err)
		}
		k, _ := idp.Key(mockidp.RS256)
		wantCode(t, "token without keys", get(t, top.HTTPClient(), nodes[0].URL("web")+"/orders", bearer(token(t, idp, k, idp.Issuer()))...),
			http.StatusUnauthorized, "RZ-AUTH-006")
		if n := idp.Stats().JWKSFetches; n != 0 {
			t.Fatalf("the IdP got %d JWKS requests; the guard refuses loopback before connecting", n)
		}
	})
	t.Run("https redirect to http", func(t *testing.T) {
		top := newTopology(t, topology.Config{})
		plain := startMock(t, top, mockup.Config{})
		idp, err := top.StartIdP(mockidp.Config{Now: top.Now})
		if err != nil {
			t.Fatal(err)
		}
		idp.SetJWKSOverride(mockidp.Override{Status: http.StatusFound, Header: http.Header{"Location": {plain.URL() + mockidp.JWKSPath}}})
		mock := startMock(t, top, mockup.Config{})
		nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{Bundle: jwtBundle(mock.Addr(), idp.Issuer(), idp.JWKSURL())})
		if err != nil {
			t.Fatal(err)
		}
		k, _ := idp.Key(mockidp.RS256)
		wantCode(t, "token after the refused redirect", get(t, top.HTTPClient(), nodes[0].URL("web")+"/orders", bearer(token(t, idp, k, idp.Issuer()))...),
			http.StatusUnauthorized, "RZ-AUTH-006")
		if n := idp.Stats().JWKSFetches; n != 1 {
			t.Fatalf("the IdP got %d JWKS requests, want the prefetch", n)
		}
		if st := plain.Stats(); st.Connections != 0 {
			t.Fatalf("the http target got %d connections; https to http is never followed", st.Connections)
		}
	})
	t.Run("https redirects to an allowed and a denied address", func(t *testing.T) {
		top := newTopology(t, topology.Config{})
		first, err := top.StartIdP(mockidp.Config{Now: top.Now})
		if err != nil {
			t.Fatal(err)
		}
		second, err := top.StartIdP(mockidp.Config{Now: top.Now})
		if err != nil {
			t.Fatal(err)
		}
		denied, err := top.ServerTLS("127.0.0.2")
		if err != nil {
			t.Fatal(err)
		}
		third, err := top.StartIdP(mockidp.Config{Now: top.Now, Listen: "127.0.0.2:0", TLS: denied})
		if err != nil {
			t.Fatal(err)
		}
		mock := startMock(t, top, mockup.Config{})
		// The first IdP's document moves to the second, which loopback
		// allows; the Node follows and verifies the second's keys.
		first.SetJWKSOverride(mockidp.Override{Status: http.StatusFound, Header: http.Header{"Location": {second.JWKSURL()}}})
		allowed, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{Bundle: jwtBundle(mock.Addr(), first.Issuer(), first.JWKSURL())})
		if err != nil {
			t.Fatal(err)
		}
		k2, _ := second.Key(mockidp.RS256)
		wantOK(t, "key behind an https redirect", get(t, top.HTTPClient(), allowed[0].URL("web")+"/orders", bearer(token(t, second, k2, first.Issuer()))...))
		// A Node allowing only 127.0.0.1 refuses the hop to 127.0.0.2.
		first.SetJWKSOverride(mockidp.Override{Status: http.StatusFound, Header: http.Header{"Location": {third.JWKSURL()}}})
		guarded, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{
			Bundle: jwtBundle(mock.Addr(), first.Issuer(), first.JWKSURL()), Env: []string{"RURALZ_FETCH_ALLOW=127.0.0.1/32"},
		})
		if err != nil {
			t.Fatal(err)
		}
		k3, _ := third.Key(mockidp.RS256)
		wantCode(t, "key behind a redirect to a denied address", get(t, top.HTTPClient(), guarded[0].URL("web")+"/orders", bearer(token(t, third, k3, first.Issuer()))...),
			http.StatusUnauthorized, "RZ-AUTH-006")
		if n := third.Stats().JWKSFetches; n != 0 {
			t.Fatalf("the denied IdP got %d JWKS requests", n)
		}
	})
	t.Run("tokenUrl redirect", func(t *testing.T) {
		top := newTopology(t, topology.Config{})
		issuer, err := top.StartIdP(mockidp.Config{Now: top.Now})
		if err != nil {
			t.Fatal(err)
		}
		elsewhere, err := top.StartIdP(mockidp.Config{Now: top.Now})
		if err != nil {
			t.Fatal(err)
		}
		issuer.SetTokenOverride(mockidp.Override{Status: http.StatusTemporaryRedirect, Header: http.Header{"Location": {elsewhere.TokenURL()}}})
		mock := startMock(t, top, mockup.Config{})
		secret := top.Canary()
		nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{
			Bundle:  oauthBundle(mock.Addr(), issuer.TokenURL(), "/etc/ruralz/oauth/client-secret"),
			Secrets: map[string][]byte{"file:/etc/ruralz/oauth/client-secret": []byte(secret)},
		})
		if err != nil {
			t.Fatal(err)
		}
		url := nodes[0].URL("web") + "/orders"
		c := top.HTTPClient()
		wantCode(t, "request whose token fetch was redirected", get(t, c, url), http.StatusUnauthorized, "RZ-AUTH-020")
		if n := elsewhere.Stats().TokenRequests; n != 0 {
			t.Fatalf("the redirect target got %d token requests; a tokenUrl redirect is never followed", n)
		}
		reqs := issuer.TokenRequests()
		if len(reqs) != 1 || reqs[0].ClientSecret != secret || reqs[0].AuthMethod != "client_secret_basic" {
			t.Fatalf("token requests at the tokenUrl = %+v, want one with the client secret by client_secret_basic", reqs)
		}
		if mock.Stats().Requests != 0 {
			t.Fatal("the Upstream was reached without a token")
		}
		// Once the endpoint answers, the next fetch, at least 1 s after
		// the failed one, brings a token to the Upstream.
		issuer.SetTokenOverride(mockidp.Override{})
		advance(t, top, refreshSpacing)
		wantOK(t, "request with a fetched token", get(t, c, url))
		rs, toks := mock.Requests(), issuer.TokenRequests()
		if len(rs) != 1 || len(toks) != 2 || toks[1].AccessToken == "" || rs[0].Header.Get("Authorization") != "Bearer "+toks[1].AccessToken {
			t.Fatalf("the Upstream got %d requests after %d token requests, want one carrying the issued token", len(rs), len(toks))
		}
	})
}

// refreshSpacing is the least time between two token fetches of one
// auth.upstream-oauth2 Policy (1 s target).
const refreshSpacing = time.Second
