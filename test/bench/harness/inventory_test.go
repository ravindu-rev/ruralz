// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness_test

import (
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/test/bench/gen"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/scenarios"
)

// inventoryDir is the directory of the profile inventories
// (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Reference hardware": one inventory file per profile).
const inventoryDir = "deploy/benchhost"

// inventory reads the committed inventory of profile name with the
// harness's own parser.
func inventory(t *testing.T, name string) *harness.Profile {
	t.Helper()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	p, err := harness.ReadProfile(filepath.Join(root, filepath.FromSlash(inventoryDir), name+".json"))
	if err != nil {
		t.Fatalf("ReadProfile(%s.json) = %v", name, err)
	}
	return p
}

// TestInventoriesParse: deploy/benchhost holds exactly one inventory per
// profile, each named after its profile, and each passes the harness's
// strict parser and validation.
func TestInventoriesParse(t *testing.T) {
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(inventoryDir), "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, filepath.Base(f))
	}
	want := []string{harness.ProfileLoopback + ".json", harness.ProfileRH1 + ".json", harness.ProfileRH3 + ".json"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s holds %q, want one inventory per profile %q", inventoryDir, got, want)
	}
	for _, name := range []string{harness.ProfileLoopback, harness.ProfileRH1, harness.ProfileRH3} {
		if p := inventory(t, name); p.Name != name {
			t.Errorf("%s.json names profile %q", name, p.Name)
		}
	}
}

// TestLoopbackInventory: loopback.json is the profile harness.Loopback
// builds, at the fixed directories under the loopback secret root's
// parent: every role on 127.0.0.1 without ssh, the fixed services the
// loopback overlays name, no scopes and no host checks.
func TestLoopbackInventory(t *testing.T) {
	p := inventory(t, harness.ProfileLoopback)
	want := harness.Loopback(t.TempDir())
	if p.SecretRoot != harness.LoopbackSecretRoot || p.TLSDir != harness.LoopbackTLSDir {
		t.Errorf("secret root %q and TLS directory %q, want %q and %q", p.SecretRoot, p.TLSDir, harness.LoopbackSecretRoot, harness.LoopbackTLSDir)
	}
	svc := p.Services
	svc.StateStoreServer = ""
	if svc != harness.LoopbackServices() {
		t.Errorf("services = %+v, want LoopbackServices %+v", svc, harness.LoopbackServices())
	}
	if p.Scope || p.Checks || p.GOMAXPROCS != want.GOMAXPROCS {
		t.Errorf("scope %t, checks %t, gomaxprocs %d; want no scopes, no checks, gomaxprocs %d", p.Scope, p.Checks, p.GOMAXPROCS, want.GOMAXPROCS)
	}
	base := filepath.ToSlash(filepath.Dir(harness.LoopbackSecretRoot))
	for _, r := range harness.Roles() {
		h := p.Host(r)
		if h.SSH != "" || h.Address != want.Host(r).Address {
			t.Errorf("%s host = %+v, want %s without ssh", r, h, want.Host(r).Address)
		}
		if wantDir := base + "/hosts/" + string(r); h.Dir != wantDir {
			t.Errorf("%s dir = %q, want %q", r, h.Dir, wantDir)
		}
	}
}

// hostPort splits a host:port the test expects to be well formed.
func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("%q: %v", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("%q: %v", addr, err)
	}
	return host, n
}

// urlHostPort returns the host and port of an https URL.
func urlHostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return hostPort(t, u.Host)
}

// TestNamedInventoriesMatchOverlays: the RH-1 and RH-3 inventories name
// the lab hosts and services exactly as the rh-1 and rh-3 overlays and the
// generated Bundles do (test/bench/gen NamedLab): the bench.example names
// deploy/benchhost resolves on every host, the mock, IdP and OTLP sink
// ports, the State Store host the S5 overlays place in a zone, and the
// secret root and TLS directory under which the overlays name the run's
// CA. Each remote host is reached over ssh by its lab name.
func TestNamedInventoriesMatchOverlays(t *testing.T) {
	l := gen.NamedLab()
	for _, name := range []string{harness.ProfileRH1, harness.ProfileRH3} {
		t.Run(name, func(t *testing.T) {
			p := inventory(t, name)
			if p.SecretRoot != scenarios.RHSecretRoot || p.TLSDir != scenarios.RHSecretRoot+"/tls" {
				t.Errorf("secret root %q and TLS directory %q, want %q and %q/tls", p.SecretRoot, p.TLSDir, scenarios.RHSecretRoot, scenarios.RHSecretRoot)
			}
			if l.OTLPCA != p.TLSDir+"/"+harness.FileCA {
				t.Errorf("the overlays' OTLP CA %q is not the inventory's %s/%s", l.OTLPCA, p.TLSDir, harness.FileCA)
			}
			checks := []struct {
				what, gotAddr, wantHost string
				wantPort                int
			}{
				{"mock", p.Services.Mock, l.MockHost, l.MockPorts[0]},
				{"idp", p.Services.IDP, harness.NameIDP, 0},
				{"sink", p.Services.Sink, harness.NameSink, 0},
				{"stateStore", p.Services.StateStore, harness.NameStateStore, 0},
				{"faultProxy", p.Services.FaultProxy, harness.NameStateStore, 0},
			}
			_, checks[1].wantPort = urlHostPort(t, l.JWKSURL)
			_, checks[2].wantPort = urlHostPort(t, l.OTLPEndpoint)
			for _, c := range checks {
				host, port := hostPort(t, c.gotAddr)
				if host != c.wantHost || (c.wantPort != 0 && port != c.wantPort) {
					t.Errorf("services.%s = %q, want host %s port %d", c.what, c.gotAddr, c.wantHost, c.wantPort)
				}
			}
			if jwks, _ := urlHostPort(t, l.JWKSURL); jwks != harness.NameIDP {
				t.Errorf("the overlays' JWKS host %q is not %s", jwks, harness.NameIDP)
			}
			if sink, _ := urlHostPort(t, l.OTLPEndpoint); sink != harness.NameSink {
				t.Errorf("the overlays' OTLP host %q is not %s", sink, harness.NameSink)
			}
			for role, want := range map[harness.Role]string{
				harness.RoleGateway: harness.NameGateway, harness.RoleUpstream: harness.NameMock, harness.RoleStateStore: harness.NameStateStore,
			} {
				if got := p.Host(role).Address; got != want {
					t.Errorf("%s address = %q, want %s", role, got, want)
				}
			}
			for _, r := range harness.Roles() {
				h := p.Host(r)
				if r != harness.RoleGateway && h.SSH != h.Address {
					t.Errorf("%s ssh destination %q, want its lab name %q", r, h.SSH, h.Address)
				}
			}
			if !p.Scope || p.GOMAXPROCS != 4 {
				t.Errorf("scope %t, gomaxprocs %d; want scopes and the Node's 4 cores (target)", p.Scope, p.GOMAXPROCS)
			}
		})
	}
}

// cpus parses a cpuset the inventory's validation already accepted.
func cpus(t *testing.T, s string) []int {
	t.Helper()
	if s == "" {
		return nil
	}
	c, ok := proc.ParseCPUList(s)
	if !ok {
		t.Fatalf("cpuset %q", s)
	}
	return c
}

// TestRH1InventoryFollowsSettingsBaseline: the RH-1 inventory gives each
// measured process the cpuset of the settings baseline (target): ruralzd
// 4 cores with GOMAXPROCS 4, the load tools 8, the mock 8 and the OTLP sink
// and IdP 4, Valkey 4 and the fault proxy 2, no two of a host's sets
// sharing a core; it runs the pre-run host checks at the 50 µs round trip
// (hypothesis).
func TestRH1InventoryFollowsSettingsBaseline(t *testing.T) {
	p := inventory(t, harness.ProfileRH1)
	want := map[harness.Role][2]int{
		harness.RoleGateway:    {4, 0},
		harness.RoleLoad:       {8, 0},
		harness.RoleUpstream:   {8, 4},
		harness.RoleStateStore: {4, 2},
	}
	for r, n := range want {
		h := p.Host(r)
		measured, aux := cpus(t, h.CPUs), cpus(t, h.AuxCPUs)
		if len(measured) != n[0] || len(aux) != n[1] {
			t.Errorf("%s cpus %q and auxCpus %q: %d and %d cores, want %d and %d", r, h.CPUs, h.AuxCPUs, len(measured), len(aux), n[0], n[1])
		}
		for _, c := range aux {
			if slices.Contains(measured, c) {
				t.Errorf("%s: core %d is in both cpus %q and auxCpus %q", r, c, h.CPUs, h.AuxCPUs)
			}
		}
	}
	if g := cpus(t, p.Host(harness.RoleGateway).CPUs); p.GOMAXPROCS != len(g) {
		t.Errorf("gomaxprocs %d, want the gateway cpuset's %d cores", p.GOMAXPROCS, len(g))
	}
	if !p.Scope || !p.Checks || p.MaxRoundTripMicros != 50 {
		t.Errorf("scope %t, checks %t, round trip %d µs; want scopes, host checks and 50 µs", p.Scope, p.Checks, p.MaxRoundTripMicros)
	}
}

// TestRH3InventoryIsNeverGated: RH-3, the published and never gated
// reproduction profile, keeps the Node's 4 dedicated vCPUs (target) and
// runs no host checks, which a cloud VM's fixed frequency cannot pass.
func TestRH3InventoryIsNeverGated(t *testing.T) {
	p := inventory(t, harness.ProfileRH3)
	if p.Checks {
		t.Error("rh-3 runs the host checks")
	}
	if g := cpus(t, p.Host(harness.RoleGateway).CPUs); len(g) != 4 {
		t.Errorf("rh-3 gateway cpuset %q, want 4 cores", p.Host(harness.RoleGateway).CPUs)
	}
}
