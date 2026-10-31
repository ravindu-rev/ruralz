// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package gen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// update rewrites testdata/digests.golden from the generated Bundles.
var update = flag.Bool("update", false, "rewrite testdata/digests.golden")

// TestSizes checks the ladder's sizes and their resource counts against
// the size-class table: R Routes, R/10 Upstreams, R/20 Policies and one
// Gateway.
func TestSizes(t *testing.T) {
	want := map[int]int{100: 116, 1000: 1151, 5000: 5751, 10000: 11501, 17000: 19551}
	got := Sizes()
	if !slices.Equal(got, []int{100, 1000, 5000, 10000, 17000}) {
		t.Fatalf("Sizes() = %v", got)
	}
	for _, r := range got {
		s := Spec{Routes: r, Lab: LoopbackLab()}
		if s.Resources() != want[r] {
			t.Errorf("Spec{Routes: %d}.Resources() = %d, want %d", r, s.Resources(), want[r])
		}
		if s.Resources() > loader.DefaultMaxResources {
			t.Errorf("%d Routes make %d resources, above the default limit %d", r, s.Resources(), loader.DefaultMaxResources)
		}
	}
}

// TestValidate covers the refused specs: a Route count that is not a
// multiple of 20 of at least 60, a variant other than 0 or 1, and a lab
// without its addresses or with one mock port.
func TestValidate(t *testing.T) {
	ok := Spec{Routes: 100, Lab: LoopbackLab()}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate(%+v) = %v", ok, err)
	}
	for name, s := range map[string]Spec{
		"too few routes":  {Routes: 40, Lab: LoopbackLab()},
		"not a multiple":  {Routes: 110, Lab: LoopbackLab()},
		"variant":         {Routes: 100, Variant: 2, Lab: LoopbackLab()},
		"no lab":          {Routes: 100},
		"one mock port":   {Routes: 100, Lab: Lab{MockHost: "h", MockPorts: [2]int{8080, 8080}, JWKSURL: "https://i/j", OTLPEndpoint: "https://s"}},
		"mock port range": {Routes: 100, Lab: Lab{MockHost: "h", MockPorts: [2]int{8080, 70000}, JWKSURL: "https://i/j", OTLPEndpoint: "https://s"}},
	} {
		if err := s.Validate(); !errors.Is(err, ErrSpec) {
			t.Errorf("Validate(%s) = %v, want ErrSpec", name, err)
		}
		if _, err := Files(s); !errors.Is(err, ErrSpec) {
			t.Errorf("Files(%s) = %v, want ErrSpec", name, err)
		}
	}
}

// TestShape checks the generated resources against the ladder's shape
// rules on a validated 1,000-Route Bundle of each variant.
func TestShape(t *testing.T) {
	for _, variant := range []int{0, 1} {
		s := Spec{Routes: 1000, Variant: variant, Lab: LoopbackLab()}
		v := validated(t, s)
		b := v.Bundle
		if got := len(b.Routes()); got != 1000 {
			t.Fatalf("variant %d: %d Routes, want 1000", variant, got)
		}
		if got := len(b.Upstreams()); got != 100 {
			t.Errorf("variant %d: %d Upstreams, want 100", variant, got)
		}
		if got := len(b.Policies()); got != 50 {
			t.Errorf("variant %d: %d Policies, want 50", variant, got)
		}
		gw := b.Gateway()
		if gw == nil || gw.Metadata.Name != GatewayName || len(gw.Spec.Policies) != 2 ||
			gw.Spec.Policies[0].Name != JWTPolicy || gw.Spec.Policies[1].Name != RateLimitPolicy {
			t.Fatalf("variant %d: Gateway = %+v, want S2's with jwt-bench and ratelimit-bench", variant, gw)
		}
		authz := 0
		for _, p := range b.Policies() {
			switch p.Metadata.Name {
			case JWTPolicy:
				if p.Spec.Type != v1alpha1.PolicyTypeAuthJWT {
					t.Errorf("jwt-bench type %q", p.Spec.Type)
				}
			case RateLimitPolicy:
				if p.Spec.Type != v1alpha1.PolicyTypeRateLimit {
					t.Errorf("ratelimit-bench type %q", p.Spec.Type)
				}
			default:
				authz++
				var cfg v1alpha1.AuthzCELConfig
				err := json.Unmarshal(p.Spec.Config, &cfg)
				if p.Spec.Type != v1alpha1.PolicyTypeAuthzCEL || err != nil || cfg.Rule != `request.method == "GET"` {
					t.Errorf("Policy %s = %+v, want an authz.cel request.method == \"GET\"", p.Metadata.Name, p.Spec)
				}
			}
		}
		if authz != 48 {
			t.Errorf("variant %d: %d authz.cel Policies, want R/20 - 2 = 48", variant, authz)
		}
		for i := range 1000 {
			r, ok := b.Route(RouteName(i))
			if !ok {
				t.Fatalf("variant %d: no Route %s", variant, RouteName(i))
			}
			checkRoute(t, s, i, r)
		}
		for j := range 100 {
			u, ok := b.Upstream(UpstreamName(j))
			if !ok || len(u.Spec.Endpoints) != 1 {
				t.Fatalf("variant %d: Upstream %d = %+v, want one Endpoint", variant, j, u)
			}
			port := s.Lab.MockPorts[0]
			if variant == 1 && j%10 == 0 {
				port = s.Lab.MockPorts[1]
			}
			if got, want := u.Spec.Endpoints[0].Address, "127.0.0.1:"+strconv.Itoa(port); got != want {
				t.Errorf("variant %d: Upstream %d Endpoint = %q, want %q", variant, j, got, want)
			}
		}
		chain, ok := v.Chains[BenchRoute]
		if !ok {
			t.Fatalf("no chain for %s", BenchRoute)
		}
		for ph, entries := range chain.Client {
			if len(entries) != 0 {
				t.Errorf("bench-1k's chain holds %d entries in Phase %d, want none: it excludes both Gateway Policies", len(entries), ph)
			}
		}
	}
}

// checkRoute checks Route i of s.
func checkRoute(t *testing.T, s Spec, i int, r *v1alpha1.Route) {
	t.Helper()
	m := r.Spec.Match
	if len(m.Hosts) != 0 {
		t.Errorf("Route %d has match.hosts %v", i, m.Hosts)
	}
	switch {
	case i == 0:
		if m.Path == nil || m.Path.Exact != BenchPath || !slices.Equal(m.Methods, []string{"GET"}) {
			t.Errorf("Route 0 match = %+v, want S1's GET /bench/1k", m)
		}
		if len(r.Spec.ExcludePolicies) != 2 || r.Spec.ExcludePolicies[0].Name != JWTPolicy || r.Spec.ExcludePolicies[1].Name != RateLimitPolicy {
			t.Errorf("Route 0 excludePolicies = %+v, want both Gateway Policies", r.Spec.ExcludePolicies)
		}
	case i%2 == 0:
		if m.Path == nil || m.Path.Exact != "/r/"+strconv.Itoa(i) {
			t.Errorf("Route %d path = %+v, want exact /r/%d", i, m.Path, i)
		}
	default:
		if m.Path == nil || m.Path.Template != "/r/"+strconv.Itoa(i)+"/{id}" {
			t.Errorf("Route %d path = %+v, want template /r/%d/{id}", i, m.Path, i)
		}
	}
	wantPolicies := []string{}
	if i%20 == 19 {
		wantPolicies = []string{AuthzPolicyName(((i - 19) / 20) % (s.Policies() - 2))}
	}
	var got []string
	for _, p := range r.Spec.Policies {
		got = append(got, p.Name)
	}
	if !slices.Equal(append([]string{}, got...), wantPolicies) {
		t.Errorf("Route %d policies = %v, want %v", i, got, wantPolicies)
	}
	if len(r.Spec.Upstreams) != 1 || r.Spec.Upstreams[0].Name != UpstreamName(i%s.Upstreams()) {
		t.Errorf("Route %d upstreams = %+v, want %s", i, r.Spec.Upstreams, UpstreamName(i%s.Upstreams()))
	}
	want := 5 * time.Second
	if s.Variant == 1 {
		want = 6 * time.Second
	}
	if r.Spec.Timeout == nil || time.Duration(*r.Spec.Timeout) != want {
		t.Errorf("Route %d timeout = %v, want %s", i, r.Spec.Timeout, want)
	}
}

// TestRoutePath checks the request paths that reach each Route.
func TestRoutePath(t *testing.T) {
	for i, want := range map[int]string{0: "/bench/1k", 2: "/r/2", 3: "/r/3/1", 19: "/r/19/1"} {
		if got := RoutePath(i); got != want {
			t.Errorf("RoutePath(%d) = %q, want %q", i, got, want)
		}
	}
}

// TestVariantsDiffer checks what R1's swap changes: every Route's
// timeout, and the Endpoint port of every tenth Upstream only.
func TestVariantsDiffer(t *testing.T) {
	a, err := Files(Spec{Routes: 100, Lab: LoopbackLab()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Files(Spec{Routes: 100, Variant: 1, Lab: LoopbackLab()})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a[FilePolicies], b[FilePolicies]) {
		t.Error("the variants' Policies differ")
	}
	if got := bytes.Count(b[FileUpstreams], []byte(":18081\"")); got != 1 {
		t.Errorf("variant 1 moves %d of 10 Upstreams to the second port, want R/100 = 1", got)
	}
	if bytes.Contains(a[FileUpstreams], []byte(":18081")) {
		t.Error("variant 0 uses the mock's second port")
	}
	if got := bytes.Count(b[FileRoutes], []byte("timeout: 6s")); got != 100 {
		t.Errorf("variant 1 sets %d Route timeouts to 6s, want every Route", got)
	}
}

// TestDeterministic checks equal specs give byte-equal files, and Write
// writes exactly Files.
func TestDeterministic(t *testing.T) {
	s := Spec{Routes: 1000, Variant: 1, Lab: NamedLab()}
	a, err := Files(s)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Write(dir, s); err != nil {
		t.Fatal(err)
	}
	for name, want := range a {
		got, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: the test's own directory
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Write(%s) differs from Files", name)
		}
	}
	b, err := Files(s)
	if err != nil {
		t.Fatal(err)
	}
	for name := range a {
		if !bytes.Equal(a[name], b[name]) {
			t.Errorf("Files is not deterministic in %s", name)
		}
	}
}

// toolchain is the configuration pipeline as ruralz builds it.
var toolchain = sync.OnceValues(func() (*pipeline.Pipeline, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	return pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: c, Workers: runtime.GOMAXPROCS(0)})
})

// validated runs the pipeline on s's in-memory Bundle and fails on any
// error.
func validated(t *testing.T, s Spec) *hub.Validated {
	t.Helper()
	p, err := toolchain()
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := FS(s)
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Run(context.WithoutCancel(t.Context()), pipeline.Request{Source: loader.Source{FS: fsys, Path: "."}})
	if err != nil {
		t.Fatal(err)
	}
	if res.HasErrors() || res.Validated == nil {
		t.Fatalf("%d Routes, variant %d: %v", s.Routes, s.Variant, res.Diagnostics)
	}
	return res.Validated
}

// TestDigests pins the two Revision digests of each size for the loopback
// lab and the named lab of RH-1 and RH-3 in testdata/digests.golden.
func TestDigests(t *testing.T) {
	var got strings.Builder
	for _, lab := range []struct {
		name string
		lab  Lab
	}{{"loopback", LoopbackLab()}, {"named", NamedLab()}} {
		for _, r := range Sizes() {
			for _, variant := range []int{0, 1} {
				v := validated(t, Spec{Routes: r, Variant: variant, Lab: lab.lab})
				fmt.Fprintf(&got, "%s %d %d %s\n", lab.name, r, variant, v.Revision.Digest)
			}
		}
	}
	golden := filepath.Join("testdata", "digests.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden) //nolint:gosec // G304: a golden file under testdata
	if err != nil {
		t.Fatalf("%v (run go test ./test/bench/gen -run TestDigests -update)", err)
	}
	if got.String() != string(want) {
		t.Errorf("Revision digests changed (go test ./test/bench/gen -run TestDigests -update rewrites them):\ngot\n%swant\n%s", got.String(), want)
	}
}
