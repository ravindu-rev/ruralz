// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"os"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
)

// TestDockerValkey: the pinned Valkey image runs as the lab user that owns
// the run's mode 0600 key files, on the State Store host's measured
// cpuset, with every benchmark process's open-file limit and the TLS
// directory read-only, every option ahead of the image; the loopback
// profile, without cpusets, sets none.
func TestDockerValkey(t *testing.T) {
	p := &Profile{
		Name: ProfileRH1, TLSDir: "/var/lib/ruralz-bench/secrets/tls",
		Hosts: map[Role]Host{RoleStateStore: {Address: NameStateStore, CPUs: "2-5", AuxCPUs: "6-7", Dir: "/var/lib/ruralz-bench"}},
	}
	got := DockerValkey(p, "1001:1001")
	want := []string{
		"docker", "run", "--rm", "--network", "host", "--user", "1001:1001",
		"--ulimit", "nofile=1048576:1048576", "-v", p.TLSDir + ":" + p.TLSDir + ":ro",
		"--cpuset-cpus", "2-5", redisserver.ImageValkey9, "valkey-server",
	}
	if !slices.Equal(got, want) {
		t.Errorf("DockerValkey(rh-1) = %q, want %q", got, want)
	}
	lp := Loopback(t.TempDir())
	got = DockerValkey(lp, "1000:1000")
	if slices.Contains(got, "--cpuset-cpus") {
		t.Errorf("DockerValkey(loopback) = %q, want no cpuset", got)
	}
	if got[len(got)-2] != redisserver.ImageValkey9 || got[len(got)-1] != "valkey-server" {
		t.Errorf("DockerValkey(loopback) = %q, want the image and valkey-server last", got)
	}
}

// TestOwner: the owner of the run's files is the uid:gid the State Store
// host's runner reports, here the test's own.
func TestOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX user IDs")
	}
	got, err := Owner(t.Context(), Loopback(t.TempDir()).Runner(RoleStateStore))
	if err != nil {
		t.Fatal(err)
	}
	if want := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()); got != want {
		t.Errorf("owner = %q, want %q", got, want)
	}
}
