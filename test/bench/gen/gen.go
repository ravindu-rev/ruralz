// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package gen generates the synthetic Bundles of the reload ladder (R1)
// and of the loader compile ladder
// (docs/architecture/12-performance-budgets-and-benchmarking.md "Startup
// and reload time by configuration size"): R Routes, R/10 Upstreams and
// R/20 Policies plus one Gateway, no Plugins, under the 20,000-resource
// default limit, generated at run time and deterministically, so equal
// inputs give byte-equal files and equal Revision digests.
//
// The Gateway is S2's: it attaches S2's jwt-bench and ratelimit-bench,
// two of the R/20 Policies, and the other R/20 - 2 are authz.cel Policies
// whose rule is request.method == "GET". Route i, for i from 0 to R - 1,
// has no match.hosts; an even i matches the exact path /r/<i>, an odd i
// the template path /r/<i>/{id}. Route 0 is S1's bench-1k Route instead,
// on Upstream 0 and excluding both Gateway Policies, so a loaded run
// offers S1's workload to it. Route i with i mod 20 = 19 attaches authz.cel
// Policy number ((i - 19) / 20) mod (R/20 - 2), round robin. Route i uses
// Upstream i mod R/10, and each Upstream has one Endpoint on the mock.
//
// R1 alternates two variants: every Route's timeout is 5 s in variant 0
// and 6 s in variant 1, and each Upstream j with j mod 10 = 0 moves its
// Endpoint from the mock's first port to its second, so every Route
// recompiles and the swap warms R/100 new pools.
package gen

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"testing/fstest"
)

// Sizes returns the ladder's Route counts, the rows of the size-class
// table: 100, 1,000, 5,000, 10,000 and 17,000 Routes.
func Sizes() []int { return []int{100, 1000, 5000, 10000, 17000} }

// Names of the generated resources.
const (
	// GatewayName is S2's Gateway.
	GatewayName = "bench"
	// JWTPolicy and RateLimitPolicy are S2's Gateway Policies.
	JWTPolicy       = "jwt-bench"
	RateLimitPolicy = "ratelimit-bench"
	// BenchRoute is S1's Route, Route 0, and BenchPath its path.
	BenchRoute = "bench-1k"
	BenchPath  = "/bench/1k"
	// Issuer and Audience are jwt-bench's (S2).
	Issuer   = "https://idp.bench.example"
	Audience = "bench"
)

// Files of a generated Bundle.
const (
	FileRoot      = "ruralz.yaml"
	FileUpstreams = "upstreams.yaml"
	FilePolicies  = "policies.yaml"
	FileRoutes    = "routes.yaml"
)

// Lab names the lab services a generated Bundle points at, as a host
// profile's overlay does for the scenario Bundles.
type Lab struct {
	// MockHost is the mock Upstream's host; MockPorts are its two ports
	// (8080 and 8081 on RH-1), between which variant 1 moves every tenth
	// Upstream's Endpoint.
	MockHost  string
	MockPorts [2]int
	// JWKSURL is jwt-bench's https key set URL.
	JWKSURL string
	// OTLPEndpoint is the telemetry export endpoint; OTLPCA, when set, is
	// the file secret under the Node's secret root that verifies it
	// (telemetry.otlp.tls.caCertificate).
	OTLPEndpoint string
	OTLPCA       string
}

// Fixed lab addresses: LoopbackLab's are the loopback profile's
// (test/bench/harness LoopbackServices and LoopbackTLSDir, the second mock
// port one above the first), NamedLab's the bench.example names and the
// secret root of the RH-1 and RH-3 overlays.
const (
	loopbackSecretRoot = "/tmp/ruralz-bench/secrets"
	namedSecretRoot    = "/var/lib/ruralz-bench/secrets"
)

// LoopbackLab returns the loopback profile's lab.
func LoopbackLab() Lab {
	return Lab{
		MockHost: "127.0.0.1", MockPorts: [2]int{18080, 18081},
		JWKSURL:      "https://127.0.0.1:18443/.well-known/jwks.json",
		OTLPEndpoint: "https://127.0.0.1:14317", OTLPCA: loopbackSecretRoot + "/tls/ca.pem",
	}
}

// NamedLab returns the RH-1 and RH-3 profiles' lab: the lab hosts by
// their bench.example names, which deploy/benchhost resolves on every
// host.
func NamedLab() Lab {
	return Lab{
		MockHost: "upstream-mock.bench.example", MockPorts: [2]int{8080, 8081},
		JWKSURL:      "https://idp.bench.example:8443/.well-known/jwks.json",
		OTLPEndpoint: "https://otel-sink.bench.example:4317", OTLPCA: namedSecretRoot + "/tls/ca.pem",
	}
}

// Spec is one generated Bundle.
type Spec struct {
	// Routes is R: a multiple of 20 of at least 60, so R/10 Upstreams and
	// R/20 Policies are whole and at least one authz.cel Policy exists.
	Routes int
	// Variant is 0 or 1, the two Revisions R1 alternates.
	Variant int
	Lab     Lab
}

// ErrSpec reports a Spec the generator refuses.
var ErrSpec = errors.New("gen: invalid spec")

// Validate checks the Spec.
func (s Spec) Validate() error {
	var errs []error
	if s.Routes < 60 || s.Routes%20 != 0 {
		errs = append(errs, fmt.Errorf("%w: %d Routes is not a multiple of 20 of at least 60", ErrSpec, s.Routes))
	}
	if s.Variant != 0 && s.Variant != 1 {
		errs = append(errs, fmt.Errorf("%w: variant %d is not 0 or 1", ErrSpec, s.Variant))
	}
	l := s.Lab
	if l.MockHost == "" || l.JWKSURL == "" || l.OTLPEndpoint == "" {
		errs = append(errs, fmt.Errorf("%w: the lab needs a mock host, a JWKS URL and an OTLP endpoint", ErrSpec))
	}
	for _, p := range l.MockPorts {
		if p < 1 || p > 65535 {
			errs = append(errs, fmt.Errorf("%w: mock port %d", ErrSpec, p))
		}
	}
	if l.MockPorts[0] == l.MockPorts[1] {
		errs = append(errs, fmt.Errorf("%w: the mock's two ports are equal", ErrSpec))
	}
	return errors.Join(errs...)
}

// Upstreams returns R/10.
func (s Spec) Upstreams() int { return s.Routes / 10 }

// Policies returns R/20.
func (s Spec) Policies() int { return s.Routes / 20 }

// Resources returns the resource count: R + R/10 + R/20 + 1, the size
// table's second figure (116 for 100 Routes, 19,551 for 17,000).
func (s Spec) Resources() int { return s.Routes + s.Upstreams() + s.Policies() + 1 }

// RouteName returns Route i's name: bench-1k for Route 0, else r-<i>.
func RouteName(i int) string {
	if i == 0 {
		return BenchRoute
	}
	return "r-" + strconv.Itoa(i)
}

// UpstreamName returns Upstream j's name.
func UpstreamName(j int) string { return "u-" + strconv.Itoa(j) }

// AuthzPolicyName returns authz.cel Policy k's name.
func AuthzPolicyName(k int) string { return "authz-" + strconv.Itoa(k) }

// RoutePath returns the path a request to Route i uses: /bench/1k for
// Route 0, /r/<i> for an even i and /r/<i>/1 for an odd i.
func RoutePath(i int) string {
	switch {
	case i == 0:
		return BenchPath
	case i%2 == 0:
		return "/r/" + strconv.Itoa(i)
	default:
		return "/r/" + strconv.Itoa(i) + "/1"
	}
}

// AuthzOf returns the authz.cel Policy Route i attaches, and false for a
// Route that attaches none.
func (s Spec) AuthzOf(i int) (int, bool) {
	if i%20 != 19 {
		return 0, false
	}
	return ((i - 19) / 20) % (s.Policies() - 2), true
}

// EndpointPort returns Upstream j's Endpoint port in the Spec's variant.
func (s Spec) EndpointPort(j int) int {
	if s.Variant == 1 && j%10 == 0 {
		return s.Lab.MockPorts[1]
	}
	return s.Lab.MockPorts[0]
}

// Timeout returns every Route's timeout in the Spec's variant.
func (s Spec) Timeout() string {
	if s.Variant == 1 {
		return "6s"
	}
	return "5s"
}

// Files returns the Bundle's files by slash path: the Gateway in
// ruralz.yaml, then the Upstreams, Policies and Routes, one file each.
func Files(s Spec) (map[string][]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return map[string][]byte{
		FileRoot:      s.gateway(),
		FileUpstreams: s.upstreams(),
		FilePolicies:  s.policies(),
		FileRoutes:    s.routes(),
	}, nil
}

// FS returns the Bundle as an in-memory file system rooted at the Bundle
// root, for a load that reads no disk (the loader compile ladder).
func FS(s Spec) (fs.FS, error) {
	files, err := Files(s)
	if err != nil {
		return nil, err
	}
	m := fstest.MapFS{}
	for name, b := range files {
		m[name] = &fstest.MapFile{Data: b, Mode: 0o600}
	}
	return m, nil
}

// Write writes the Bundle into dir, which it creates; files already there
// are replaced.
func Write(dir string, s Spec) error {
	files, err := Files(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(path.Clean(name))), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// header writes a document's envelope.
func header(b *bytes.Buffer, kind, name string) {
	if b.Len() > 0 {
		b.WriteString("---\n")
	}
	b.WriteString("apiVersion: ruralz/v1alpha1\nkind: ")
	b.WriteString(kind)
	b.WriteString("\nmetadata:\n  name: ")
	b.WriteString(name)
	b.WriteString("\nspec:\n")
}

// quote returns s as a double-quoted YAML scalar; the generator's strings
// hold no character that needs an escape.
func quote(s string) string { return strconv.Quote(s) }

// gateway is S2's Gateway on the lab.
func (s Spec) gateway() []byte {
	var b bytes.Buffer
	b.WriteString("# R1 ladder Bundle: ")
	b.WriteString(strconv.Itoa(s.Routes))
	b.WriteString(" Routes, variant ")
	b.WriteString(strconv.Itoa(s.Variant))
	b.WriteString(", generated by test/bench/gen.\n")
	header(&b, "Gateway", GatewayName)
	b.WriteString("  listeners:\n    - name: http\n      protocol: http\n      port: 8080\n")
	b.WriteString("  admin:\n    port: 9901\n")
	b.WriteString("  telemetry:\n    otlp:\n      endpoint: ")
	b.WriteString(quote(s.Lab.OTLPEndpoint))
	b.WriteString("\n")
	if s.Lab.OTLPCA != "" {
		b.WriteString("      tls:\n        caCertificate: {secretRef: {provider: file, name: ")
		b.WriteString(s.Lab.OTLPCA)
		b.WriteString("}}\n")
	}
	b.WriteString("    traceSampling: 0.01\n")
	b.WriteString("  stateStore:\n    driver: memory\n")
	b.WriteString("  policies:\n    - name: ")
	b.WriteString(JWTPolicy)
	b.WriteString("\n    - name: ")
	b.WriteString(RateLimitPolicy)
	b.WriteString("\n")
	return b.Bytes()
}

// upstreams are the R/10 Upstreams, each S2's mock Upstream with one
// Endpoint on the mock.
func (s Spec) upstreams() []byte {
	var b bytes.Buffer
	b.Grow(s.Upstreams() * 330)
	for j := range s.Upstreams() {
		header(&b, "Upstream", UpstreamName(j))
		b.WriteString("  protocol: http\n  endpoints:\n    - address: ")
		b.WriteString(quote(s.Lab.MockHost + ":" + strconv.Itoa(s.EndpointPort(j))))
		b.WriteString("\n  loadBalancing: {algorithm: round-robin}\n")
		b.WriteString("  circuitBreaker:\n    maxConnections: 65536\n    maxPendingRequests: 1024\n")
		b.WriteString("  timeout: 2s\n")
	}
	return b.Bytes()
}

// policies are S2's two Gateway Policies and the R/20 - 2 authz.cel
// Policies.
func (s Spec) policies() []byte {
	var b bytes.Buffer
	b.Grow(s.Policies() * 160)
	header(&b, "Policy", JWTPolicy)
	b.WriteString("  type: auth.jwt\n  config:\n    issuers:\n      - issuer: ")
	b.WriteString(Issuer)
	b.WriteString("\n        jwksUrl: ")
	b.WriteString(s.Lab.JWKSURL)
	b.WriteString("\n        audiences: [")
	b.WriteString(Audience)
	b.WriteString("]\n")
	header(&b, "Policy", RateLimitPolicy)
	b.WriteString("  type: ratelimit\n  config:\n")
	b.WriteString("    key: 'request.headers[\"x-bench-key\"]'\n")
	b.WriteString("    limits: [{requests: 20000, window: 1s}]\n")
	for k := range s.Policies() - 2 {
		header(&b, "Policy", AuthzPolicyName(k))
		b.WriteString("  type: authz.cel\n  config:\n    rule: 'request.method == \"GET\"'\n")
	}
	return b.Bytes()
}

// routes are the R Routes.
func (s Spec) routes() []byte {
	var b bytes.Buffer
	b.Grow(s.Routes * 190)
	timeout := s.Timeout()
	for i := range s.Routes {
		header(&b, "Route", RouteName(i))
		b.WriteString("  match:\n    path: ")
		switch {
		case i == 0:
			b.WriteString("{exact: " + BenchPath + "}\n    methods: [GET]\n")
		case i%2 == 0:
			b.WriteString("{exact: /r/" + strconv.Itoa(i) + "}\n")
		default:
			b.WriteString("{template: \"/r/" + strconv.Itoa(i) + "/{id}\"}\n")
		}
		if i == 0 {
			b.WriteString("  excludePolicies:\n    - name: " + JWTPolicy + "\n    - name: " + RateLimitPolicy + "\n")
		}
		if k, ok := s.AuthzOf(i); ok {
			b.WriteString("  policies:\n    - name: " + AuthzPolicyName(k) + "\n")
		}
		b.WriteString("  upstreams:\n    - name: ")
		b.WriteString(UpstreamName(i % s.Upstreams()))
		b.WriteString("\n  timeout: ")
		b.WriteString(timeout)
		b.WriteString("\n")
	}
	return b.Bytes()
}
