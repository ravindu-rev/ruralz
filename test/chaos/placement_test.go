// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
)

// rh1Placement is a placement on the committed RH-1 inventory with the
// addresses its hosts resolve to filled in.
func rh1Placement(t *testing.T) *Placement {
	t.Helper()
	p, err := TargetHosts(inventoryEnv(t, harness.ProfileRH1))
	if err != nil {
		t.Fatal(err)
	}
	return &Placement{
		Profile: p, gateway: "10.0.0.1", store: "10.0.0.4", mockHost: "upstream-mock.bench.example", owner: "998:998",
		bins: map[harness.Role]string{
			harness.RoleLoad: "/var/lib/ruralz-bench/bin/labhost", harness.RoleUpstream: "/var/lib/ruralz-bench/bin/labhost",
			harness.RoleStateStore: "/var/lib/ruralz-bench/bin/labhost",
		},
	}
}

// TestPlacementPlan: on the RH-1 inventory the Nodes share the gateway
// host's cpuset and the balancers listen on its address, which the load
// host reaches; on the loopback inventory, which has no scopes, the Nodes
// share none.
func TestPlacementPlan(t *testing.T) {
	pl := rh1Placement(t)
	if got := pl.NodeCPUs(); got != "2-5" {
		t.Errorf("NodeCPUs = %q, want the gateway host's cpuset 2-5", got)
	}
	if got := pl.BalancerListen(); got != "10.0.0.1:0" {
		t.Errorf("BalancerListen = %q, want the gateway host's address", got)
	}
	loop := &Placement{Profile: harness.Loopback(t.TempDir())}
	if got := loop.NodeCPUs(); got != "" {
		t.Errorf("NodeCPUs on the loopback inventory = %q, want none", got)
	}
	if names := pl.StateStoreHost().Names(); !slices.Equal(names, []string{"statestore.bench.example"}) {
		t.Errorf("the State Store host's names = %q, want its inventory name", names)
	}
	if names := loop.StateStoreHost().Names(); len(names) != 0 {
		t.Errorf("the loopback State Store host's names = %q, want none for an IP address", names)
	}
	if got := pl.StateStoreHost().Addr(); got != "10.0.0.4" {
		t.Errorf("the State Store host's address = %q", got)
	}
}

// TestServerArgv: each State Store server runs under labhost run, ready
// once it wrote its readiness line and its port accepts connections: the
// pinned Valkey 9 image as the inventory's user with host networking, its
// deployment directory shared at its own path, on the State Store host's
// measured cpuset with the benchmark processes' open-file limit, and
// valkey-server the container's own process, so the signals docker kill
// delivers reach the server, not the image's tini; with the
// inventory's server executable, that server runs directly.
func TestServerArgv(t *testing.T) {
	pl := rh1Placement(t)
	dir := "/var/lib/ruralz-bench/chaos/redisserver-1a2b3c4d"
	s := redisserver.HostServer{
		Name: "redis-server-sentinel-primary", Dir: dir, Port: 40001,
		Args: []string{dir + "/redis-server-sentinel-primary-9/redis.conf"}, Ready: "Ready to accept connections",
	}
	container := "ruralz-chaos-0a1b2c3d4e5f-redis-server-sentinel-primary"
	want := []string{
		"/var/lib/ruralz-bench/bin/labhost", "run", "-ready", "10.0.0.4:40001", "-ready-line", "Ready to accept connections",
		"-container", container, "--",
		"docker", "run", "--rm", "--name", container, "--network", "host", "--user", "998:998",
		"--ulimit", fmt.Sprintf("nofile=%d:%d", harness.OpenFiles, harness.OpenFiles), "-v", dir + ":" + dir,
		"--entrypoint", "valkey-server", "--cpuset-cpus", "2-5", redisserver.ImageValkey9, s.Args[0],
	}
	if got := pl.serverArgv(s, container); !slices.Equal(got, want) {
		t.Errorf("serverArgv =\n%q\nwant\n%q", got, want)
	}
	pl.Profile.Services.StateStoreServer = "/usr/bin/valkey-server"
	s.Args = append(s.Args, "--sentinel")
	want = []string{
		"/var/lib/ruralz-bench/bin/labhost", "run", "-ready", "10.0.0.4:40001", "-ready-line", "Ready to accept connections",
		"--", "/usr/bin/valkey-server", s.Args[0], "--sentinel",
	}
	if got := pl.serverArgv(s, ""); !slices.Equal(got, want) {
		t.Errorf("serverArgv with a server executable =\n%q\nwant\n%q", got, want)
	}
}

// TestPlacedRunRefusesMockConfig: a run placed on the RH-1 hosts fails
// before it starts anything when its experiment configures the mock
// Upstreams, which run there as labhost's mocks and take no
// mockup.Config.
func TestPlacedRunRefusesMockConfig(t *testing.T) {
	ft := &fatalTB{TB: t}
	h := &Harness{Profile: Profile{Name: Reduced, Nodes: 1, CE5Nodes: 1, LoadShare: 0.5, HotRatio: 40}, place: rh1Placement(t)}
	var wg sync.WaitGroup
	wg.Go(func() { h.Start(ft, Spec{Name: "mocked", Mock: func(int, *mockup.Config) {}}) })
	wg.Wait()
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if !strings.Contains(ft.fatal, "configures its mock Upstreams") {
		t.Errorf("Start failed with %q, want the refusal of the mock configuration", ft.fatal)
	}
}

// TestResolveIP: an inventory address that is an IP address is its own.
func TestResolveIP(t *testing.T) {
	for _, ip := range []string{"10.0.0.4", "127.0.0.1", "::1"} {
		if got, err := resolveIP(t.Context(), ip); err != nil || got != ip {
			t.Errorf("resolveIP(%s) = %q, %v", ip, got, err)
		}
	}
}
