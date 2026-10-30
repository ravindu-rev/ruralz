// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package protocol

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Secret references of the suite's Bundles.
const (
	refMainCert = "file:/etc/ruralz/tls/main.crt"
	refMainKey  = "file:/etc/ruralz/tls/main.key"
	refAltCert  = "file:/etc/ruralz/tls/alt.crt"
	refAltKey   = "file:/etc/ruralz/tls/alt.key"
)

// altHost is the SNI name of the https listener's second certificate.
const altHost = "alt.conformance.test"

// Hosts of the hosts and listener hostnames cases.
const (
	// routedHost is the only host of the Route hosted.
	routedHost = "routed.conformance.test"
	// namedHost is the only host name of the listener named.
	namedHost = "named.conformance.test"
)

// Origin of the cors Policy.
const corsOrigin = "https://app.conformance.test"

// Header ceilings of the main Bundle.
const (
	// headerLimit is limits.maxRequestHeaderBytes of the main Bundle, 16
	// KiB, below the 256 KiB process ceiling.
	headerLimit = 16 << 10
	// bodyLimit is limits.maxRequestBodyBytes of the main Bundle.
	bodyLimit = 64 << 10
)

// credentials are the Consumer credentials of the main Bundle, each value
// a canary, so the topology's teardown scan of the Node's logs and of the
// responses gateway.do captures proves the Node never logs or echoes one.
type credentials struct {
	apiKey        string
	basicUser     string
	basicPassword string
	basicHash     string
	idp           *mockidp.Server
}

// bundleOptions vary the main Bundle.
type bundleOptions struct {
	// headerLimit overrides limits.maxRequestHeaderBytes, as a Bundle
	// value such as "16Ki".
	headerLimit string
	// routeTimeout is the timeout of the Routes under /deadline/.
	routeTimeout string
}

// originConnectTimeout is the connectTimeout of the origin Upstreams:
// cases open hundreds of origin connections at once, as the 250 held
// streams do, on CI machines other jobs share, and no case measures the
// dial, so the 1 s default would only add flakes.
const originConnectTimeout = "10s"

// basicIterations is the PBKDF2 iteration count of the auth.basic
// credential, the lowest the configuration model accepts.
const basicIterations = 600000

// newCredentials makes the Consumer credentials of top: canary values, the
// PBKDF2 hash of the password and a mock IdP for auth.jwt.
func newCredentials(t testing.TB, top *topology.Topology) *credentials {
	t.Helper()
	c := &credentials{apiKey: top.Canary(), basicUser: "alice", basicPassword: top.Canary()}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	key, err := pbkdf2.Key(sha256.New, c.basicPassword, salt, basicIterations, 32)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	c.basicHash = "pbkdf2-sha256:" + enc.EncodeToString(salt) + ":" + enc.EncodeToString(key)
	if c.idp, err = top.StartIdP(mockidp.Config{}); err != nil {
		t.Fatal(err)
	}
	return c
}

// apiKeyHash returns the Consumer apiKeys hash of key.
func apiKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// listenerSecrets returns the secrets of the https listener: a main
// certificate for loopback and a second one for altHost and namedHost.
func listenerSecrets(t testing.TB, top *topology.Topology) map[string][]byte {
	t.Helper()
	mainCert, mainKey, err := top.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	altCert, altKey, err := top.ServerCertificate(altHost, namedHost)
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{refMainCert: mainCert, refMainKey: mainKey, refAltCert: altCert, refAltKey: altKey}
}

// composedPath returns the path of the one step of the composition Route
// to u, which sends every request to the origin's bHints behavior.
func composedPath(u upstream) string { return "/composed/" + u.String() + "/" + bHints }

// wireBundle renders the main Bundle of the suite: an http listener web,
// an https listener secure with two certificates and an https listener
// named limited to namedHost; the three origin transports as Upstreams
// h1, h1tls and h2 without retries; and the Routes the cases address:
//
//   - /up/<transport>/ plain to each Upstream;
//   - /composed/<transport>/ a sequential composition whose one step
//     takes each request to the Upstream's bHints behavior (composedPath);
//   - /auth/api-key/, /auth/basic/ and /auth/jwt/ behind one auth Policy
//     each, and /cors/ behind cors and auth.api-key;
//   - /methods/get (methods GET), /methods/head (methods HEAD) and
//     /methods/any (no methods);
//   - /hosted/ for routedHost only, /web-only/ on the listener web only;
//   - /policy/ with a Route headers Policy and /rewrite/ with a
//     transform.request path rewrite;
//   - /gated/ with a body gate, and /deadline/ with the routeTimeout.
//
// The Gateway's security-headers Policy is overridable: false.
func wireBundle(o *origin, c *credentials, opt bundleOptions) []byte {
	if opt.headerLimit == "" {
		opt.headerLimit = "16Ki"
	}
	if opt.routeTimeout == "" {
		opt.routeTimeout = "10s"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: conformance
spec:
  admin:
    port: 9901
  listeners:
    - name: web
      protocol: http
      port: 8080
    - name: secure
      protocol: https
      port: 8443
      tls:
        certificates:
          - name: a-main
            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls/main.crt}}
            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls/main.key}}
          - name: b-alt
            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls/alt.crt}}
            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls/alt.key}}
    - name: named
      protocol: https
      port: 8444
      hostnames: ["%s"]
      tls:
        certificates:
          - name: alt
            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls/alt.crt}}
            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls/alt.key}}
  limits:
    maxRequestHeaderBytes: %s
    maxRequestBodyBytes: 64Ki
  policies:
    - name: security-headers
`, namedHost, opt.headerLimit)
	for _, u := range append(upstreams(), upWire, upBlackhole) {
		tls := ""
		if u == upH1TLS || u == upH2 || u == upBlackhole {
			tls = "\n  tls: {}"
		}
		fmt.Fprintf(&b, `---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: %s
spec:
  protocol: http
  connectTimeout: %s
  endpoints:
    - address: "%s"
  retries:
    attempts: 0%s
`, u, originConnectTimeout, o.addr(u), tls)
		fmt.Fprintf(&b, `---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: up-%s
spec:
  match:
    path: {prefix: /up/%s/}
  upstreams:
    - name: %s
`, u, u, u)
	}
	for _, u := range upstreams() {
		fmt.Fprintf(&b, `---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: composed-%s
spec:
  match:
    path: {prefix: /composed/%s/}
  composition:
    mode: sequential
    steps:
      - name: only
        upstream: %s
        path: %s
`, u, u, u, composedPath(u))
	}
	fmt.Fprintf(&b, `---
apiVersion: ruralz/v1alpha1
kind: Consumer
metadata:
  name: alice
spec:
  credentials:
    apiKeys:
      - name: primary
        hash: "%s"
    basic:
      - username: %s
        hash: "%s"
    jwt:
      - issuer: "%s"
        subject: alice
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: security-headers}
spec:
  type: headers
  overridable: false
  config:
    response:
      set: [{name: strict-transport-security, value: "max-age=31536000"}]
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: api-key}
spec:
  type: auth.api-key
  config: {header: x-api-key}
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: basic}
spec:
  type: auth.basic
  config: {}
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: jwt}
spec:
  type: auth.jwt
  config:
    issuers:
      - issuer: "%s"
        jwksUrl: "%s"
        audiences: [conformance]
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: cors}
spec:
  type: cors
  config:
    allowOrigins: ["%s"]
    allowMethods: [GET, POST]
    allowHeaders: [x-api-key]
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: route-response}
spec:
  type: headers
  config:
    response:
      remove: [x-trailer-declared, server]
      set: [{name: x-route-response, value: applied}]
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: rewrite-path}
spec:
  type: transform.request
  config:
    set:
      - target: path
        valueExpression: 'request.headers["x-new-path"]'
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: body-gate}
spec:
  type: transform.request
  config:
    replace: [{pattern: conformance-never-in-a-body, literal: true}]
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: body-rule}
spec:
  type: authz.cel
  config:
    rule: 'request.body != null'
`, apiKeyHash(c.apiKey), c.basicUser, c.basicHash, c.idp.Issuer(), c.idp.Issuer(), c.idp.JWKSURL(), corsOrigin)
	route := func(name, match, policies, extra string) {
		fmt.Fprintf(&b, "---\napiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: %s\nspec:\n  match:\n%s", name, match)
		if policies != "" {
			b.WriteString("  policies:\n")
			for p := range strings.SplitSeq(policies, ",") {
				b.WriteString("    - name: " + p + "\n")
			}
		}
		b.WriteString(extra)
		b.WriteString("  upstreams:\n    - name: h1\n")
	}
	route("auth-api-key", "    path: {prefix: /auth/api-key/}\n", "api-key", "")
	route("auth-basic", "    path: {prefix: /auth/basic/}\n", "basic", "")
	route("auth-jwt", "    path: {prefix: /auth/jwt/}\n", "jwt", "")
	route("cors", "    path: {prefix: /cors/}\n", "cors,api-key", "")
	route("methods-get", "    path: {exact: /methods/get}\n    methods: [GET]\n", "", "")
	route("methods-head", "    path: {exact: /methods/head}\n    methods: [HEAD]\n", "", "")
	route("methods-any", "    path: {exact: /methods/any}\n", "", "")
	route("hosted", "    path: {prefix: /hosted/}\n    hosts: [\""+routedHost+"\"]\n", "", "")
	route("web-only", "    path: {prefix: /web-only/}\n", "", "  listeners: [web]\n")
	route("policy", "    path: {prefix: /policy/}\n", "route-response", "")
	route("rewrite", "    path: {prefix: /rewrite/}\n", "rewrite-path", "")
	route("gated", "    path: {prefix: /gated/}\n", "body-gate", "")
	route("decoded", "    path: {prefix: /decoded/}\n", "body-rule", "")
	route("deadline", "    path: {prefix: /deadline/}\n", "", "  timeout: "+opt.routeTimeout+"\n")
	route("deadline-gated", "    path: {prefix: /deadline-gated/}\n", "body-gate", "  timeout: "+opt.routeTimeout+"\n")
	route("named", "    path: {prefix: /named/}\n", "", "  listeners: [named]\n")
	fmt.Fprintf(&b, "---\napiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: blackhole\nspec:\n  match:\n    path: {prefix: /blackhole/}\n  upstreams:\n    - name: blackhole\n")
	return []byte(b.String())
}

// startGateway starts one Node on the main Bundle with the origin behind
// it and returns the means to reach it.
func startGateway(t testing.TB, opt bundleOptions) *gateway {
	t.Helper()
	top := newTopology(t)
	o := startOrigin(t, top)
	creds := newCredentials(t, top)
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{
		Bundle:  wireBundle(o, creds, opt),
		Secrets: listenerSecrets(t, top),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &gateway{top: top, node: nodes[0], origin: o, tls: top.ClientTLS(), creds: creds}
}
