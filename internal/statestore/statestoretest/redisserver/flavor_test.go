// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// TestParseFlavor checks that RURALZ_TEST_STATESTORE takes exactly the
// values the testing strategy defines.
func TestParseFlavor(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Flavor
		ok   bool
	}{
		{"", FlavorNone, true},
		{"process", FlavorProcess, true},
		{"container", FlavorContainer, true},
		{"Process", FlavorNone, false},
		{" process", FlavorNone, false},
		{"docker", FlavorNone, false},
	} {
		got, err := ParseFlavor(tc.in)
		if got != tc.want || (err == nil) != tc.ok {
			t.Errorf("ParseFlavor(%q) = %v, %v; want %v, ok %v", tc.in, got, err, tc.want, tc.ok)
		}
		if err == nil && got.String() != tc.in {
			t.Errorf("ParseFlavor(%q).String() = %q", tc.in, got.String())
		}
	}
}

// TestKindProperties checks each kind against the Integration tests
// dependency table: its flavor, topology and image, and that each flavor
// lists exactly its kinds.
func TestKindProperties(t *testing.T) {
	for _, tc := range []struct {
		k        Kind
		flavor   Flavor
		topology statestore.Topology
		image    string
	}{
		{Process, FlavorProcess, statestore.TopologyStandalone, ""},
		{ProcessCluster, FlavorProcess, statestore.TopologyCluster, ""},
		{Redis8, FlavorContainer, statestore.TopologyStandalone, ImageRedis8},
		{Valkey9, FlavorContainer, statestore.TopologyStandalone, ImageValkey9},
		{Dragonfly, FlavorContainer, statestore.TopologyStandalone, ImageDragonfly},
		{Redis8Cluster, FlavorContainer, statestore.TopologyCluster, ImageRedis8},
		{Valkey9Cluster, FlavorContainer, statestore.TopologyCluster, ImageValkey9},
		{Kind("nope"), FlavorNone, statestore.TopologyStandalone, ""},
	} {
		if tc.k.Flavor() != tc.flavor || tc.k.Topology() != tc.topology || tc.k.Image() != tc.image {
			t.Errorf("%s: flavor %v, topology %v, image %q; want %v, %v, %q",
				tc.k, tc.k.Flavor(), tc.k.Topology(), tc.k.Image(), tc.flavor, tc.topology, tc.image)
		}
		if tc.flavor != FlavorNone && !slices.Contains(tc.flavor.Kinds(), tc.k) {
			t.Errorf("%v.Kinds() lacks %s", tc.flavor, tc.k)
		}
	}
	for _, f := range []Flavor{FlavorProcess, FlavorContainer} {
		for _, k := range f.Kinds() {
			if k.Flavor() != f {
				t.Errorf("%v.Kinds() holds %s of flavor %v", f, k, k.Flavor())
			}
		}
	}
	if FlavorNone.Kinds() != nil {
		t.Errorf("FlavorNone.Kinds() = %v, want none", FlavorNone.Kinds())
	}
	if !strings.HasPrefix(ImageRedis8, "redis:8") || !strings.HasPrefix(ImageValkey9, "valkey/valkey:9.0") {
		t.Errorf("images %q and %q, want the redis:8 and valkey/valkey:9.0 the tech stack names", ImageRedis8, ImageValkey9)
	}
}

// TestKindsFromEnvironment checks that Kinds follows RURALZ_TEST_STATESTORE
// and skips without it.
func TestKindsFromEnvironment(t *testing.T) {
	t.Setenv(EnvStateStore, "process")
	if got := Kinds(t); !slices.Equal(got, []Kind{Process, ProcessCluster}) {
		t.Errorf("Kinds with process = %v", got)
	}
	t.Setenv(EnvStateStore, "container")
	if got := Kinds(t); !slices.Equal(got, FlavorContainer.Kinds()) {
		t.Errorf("Kinds with container = %v", got)
	}
	t.Run("unset skips", func(t *testing.T) {
		t.Setenv(EnvStateStore, "")
		Kinds(t)
		t.Fatal("Kinds returned with RURALZ_TEST_STATESTORE unset; want a skip")
	})
}

// fakeHost is a machine for the gates.
func fakeHost(env map[string]string, paths map[string]string, goos string, docker error) (host, *int) {
	calls := 0
	return host{
		getenv: func(k string) string { return env[k] },
		lookPath: func(p string) (string, error) {
			if found, ok := paths[p]; ok {
				return found, nil
			}
			return "", errors.New("executable file not found")
		},
		goos: goos,
		docker: func(context.Context) error {
			calls++
			return docker
		},
	}, &calls
}

// TestRedisServerLookup checks how the process flavor finds redis-server:
// RURALZ_TEST_REDIS_SERVER first, where a wrong path is an error; then
// PATH; else the reason there is none, naming the variable.
func TestRedisServerLookup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		env         map[string]string
		paths       map[string]string
		want        string
		wantMissing bool
		wantErr     bool
	}{
		{"variable", map[string]string{EnvRedisServer: "/opt/redis-server"}, map[string]string{"/opt/redis-server": "/opt/redis-server", "redis-server": "/usr/bin/redis-server"}, "/opt/redis-server", false, false},
		{"variable naming nothing", map[string]string{EnvRedisServer: "/missing"}, map[string]string{"redis-server": "/usr/bin/redis-server"}, "", false, true},
		{"PATH", nil, map[string]string{"redis-server": "/usr/bin/redis-server"}, "/usr/bin/redis-server", false, false},
		{"none", nil, nil, "", true, false},
	} {
		h, _ := fakeHost(tc.env, tc.paths, "linux", nil)
		got, missing, err := h.redisServer()
		if got != tc.want || (missing != "") != tc.wantMissing || (err != nil) != tc.wantErr {
			t.Errorf("%s: redisServer() = %q, missing %q, %v", tc.name, got, missing, err)
		}
		if missing != "" && !strings.Contains(missing, EnvRedisServer) {
			t.Errorf("%s: %q does not name %s", tc.name, missing, EnvRedisServer)
		}
	}
}

// TestContainerMissing checks what a container kind needs from its host:
// a cluster a Linux host, which is checked before Docker, and every kind
// a Docker daemon.
func TestContainerMissing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		goos       string
		k          Kind
		docker     error
		want       string
		wantDocker int
	}{
		{"cluster off Linux", "windows", Redis8Cluster, nil, "Linux", 0},
		{"no Docker", "linux", Dragonfly, errors.New("no daemon"), "Docker is unavailable: no daemon", 1},
		{"standalone off Linux", "darwin", Valkey9, nil, "", 1},
		{"cluster on Linux", "linux", Valkey9Cluster, nil, "", 1},
	} {
		h, calls := fakeHost(nil, nil, tc.goos, tc.docker)
		got := h.containerMissing(t.Context(), tc.k)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) || *calls != tc.wantDocker {
			t.Errorf("%s: containerMissing = %q after %d Docker checks; want %q after %d", tc.name, got, *calls, tc.want, tc.wantDocker)
		}
	}
}

// TestSelectedFlavorFailsWithoutPrerequisite checks the gates of Start: a
// kind whose flavor RURALZ_TEST_STATESTORE selects fails when it cannot
// run here (no redis-server, no Docker daemon, a cluster container off
// Linux); a kind of an unselected flavor skips with the reason instead,
// a container kind without asking Docker; a process kind runs wherever a
// redis-server is found; an invalid variable fails every kind.
func TestSelectedFlavorFailsWithoutPrerequisite(t *testing.T) {
	process := map[string]string{EnvStateStore: "process"}
	container := map[string]string{EnvStateStore: "container"}
	found := map[string]string{"redis-server": "/usr/bin/redis-server"}
	down := errors.New("no daemon")
	for _, tc := range []struct {
		name       string
		env        map[string]string
		paths      map[string]string
		goos       string
		docker     error
		k          Kind
		wantPath   string
		wantSkip   string
		wantErr    string
		wantDocker int
	}{
		{"process selected, no redis-server", process, nil, "linux", nil, Process, "", "", "no redis-server", 0},
		{"process unset, no redis-server", nil, nil, "linux", nil, ProcessCluster, "", "no redis-server", "", 0},
		{"process unselected, no redis-server", container, nil, "linux", nil, Process, "", "no redis-server", "", 0},
		{"process unset, redis-server found", nil, found, "linux", nil, Process, "/usr/bin/redis-server", "", "", 0},
		{"process selected, redis-server found", process, found, "linux", nil, ProcessCluster, "/usr/bin/redis-server", "", "", 0},
		{"container selected, no Docker", container, nil, "linux", down, Redis8, "", "", "Docker is unavailable", 1},
		{"container selected, cluster off Linux", container, nil, "darwin", nil, Valkey9Cluster, "", "", "Linux", 0},
		{"container selected, Docker answers", container, nil, "linux", nil, Dragonfly, "", "", "", 1},
		{"container unset", nil, nil, "linux", down, Redis8, "", EnvStateStore + "=container", "", 0},
		{"container unselected", process, nil, "linux", down, Valkey9Cluster, "", EnvStateStore + "=container", "", 0},
		{"invalid variable", map[string]string{EnvStateStore: "x"}, found, "linux", nil, Process, "", "", EnvStateStore, 0},
	} {
		h, calls := fakeHost(tc.env, tc.paths, tc.goos, tc.docker)
		path, skip, err := h.gate(t.Context(), tc.k)
		gotErr := ""
		if err != nil {
			gotErr = err.Error()
		}
		if path != tc.wantPath || (skip == "") != (tc.wantSkip == "") || !strings.Contains(skip, tc.wantSkip) ||
			(gotErr == "") != (tc.wantErr == "") || !strings.Contains(gotErr, tc.wantErr) || *calls != tc.wantDocker {
			t.Errorf("%s: gate = %q, skip %q, error %q after %d Docker checks; want %q, skip with %q, error with %q, %d checks",
				tc.name, path, skip, gotErr, *calls, tc.wantPath, tc.wantSkip, tc.wantErr, tc.wantDocker)
		}
	}
}

// TestStartGates checks that start validates before it gates, refuses an
// unknown kind, skips an unselected kind and fails a selected one that
// cannot run.
func TestStartGates(t *testing.T) {
	none, _ := fakeHost(nil, nil, "linux", nil)
	selected, _ := fakeHost(map[string]string{EnvStateStore: "process"}, nil, "linux", nil)
	for _, tc := range []struct {
		name     string
		h        host
		k        Kind
		o        Options
		wantSkip bool
	}{
		{"process without redis-server", none, Process, Options{}, true},
		{"container unselected", none, Redis8, Options{}, true},
		{"container with options", none, Redis8, Options{TLS: true}, false},
		{"unknown kind", none, Kind("memcached"), Options{}, false},
		{"selected process without redis-server", selected, Process, Options{}, false},
	} {
		d, skip, err := start(t, tc.h, tc.k, tc.o)
		if d != nil || (skip != "") != tc.wantSkip || (err == nil) != tc.wantSkip {
			t.Errorf("%s: start = %v, skip %q, %v", tc.name, d, skip, err)
		}
	}
	bad, _ := fakeHost(map[string]string{EnvRedisServer: "/missing"}, nil, "linux", nil)
	if _, skip, err := start(t, bad, ProcessCluster, Options{}); err == nil || skip != "" {
		t.Errorf("start with a wrong %s = skip %q, %v; want an error", EnvRedisServer, skip, err)
	}
}

// TestStartSkipsUnselectedContainer checks the public Start: a container
// kind without RURALZ_TEST_STATESTORE=container skips the test before
// any Docker call.
func TestStartSkipsUnselectedContainer(t *testing.T) {
	t.Run("skip", func(t *testing.T) {
		t.Setenv(EnvStateStore, "")
		Start(t, Redis8, Options{})
		t.Fatal("Start returned; want a skip")
	})
}
