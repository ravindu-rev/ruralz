// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const rh1Inventory = `{
  "profile": "rh-1",
  "hosts": {
    "gateway": {"address": "10.0.0.1", "cpus": "2-5", "dir": "/srv/bench"},
    "load": {"ssh": "rh-1l", "address": "10.0.0.2", "cpus": "0-7", "dir": "/srv/bench"},
    "upstream": {"ssh": "rh-1u", "address": "10.0.0.3", "cpus": "0-7", "auxCpus": "8-11", "dir": "/srv/bench"},
    "statestore": {"ssh": "rh-1s", "address": "10.0.0.4", "cpus": "0-3", "auxCpus": "4-5", "dir": "/srv/bench"}
  },
  "sshUser": "bench", "sshKey": "/home/bench/.ssh/id_ed25519", "knownHosts": "/home/bench/.ssh/known_hosts",
  "secretRoot": "/srv/bench/secrets", "tlsDir": "/srv/bench/tls", "scope": true, "gomaxprocs": 4, "checks": true,
  "services": {"mock": "10.0.0.3:8080", "idp": "10.0.0.3:443", "sink": "10.0.0.3:4317", "stateStore": "10.0.0.4:6379", "faultProxy": "10.0.0.4:6380", "echoPort": 7000}
}`

func TestReadProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rh-1.json")
	if err := os.WriteFile(path, []byte(rh1Inventory), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := ReadProfile(path)
	if err != nil {
		t.Fatalf("ReadProfile = %v", err)
	}
	if p.HardwareProfile() != "RH-1" || p.GOMAXPROCS != 4 || !p.Runner(RoleLoad).Remote() || p.Runner(RoleGateway).Remote() {
		t.Errorf("profile = %+v", p)
	}
	if p.roundTripBound() != 50 || p.adminPort() != 9901 {
		t.Errorf("defaults: round trip %d µs, admin port %d", p.roundTripBound(), p.adminPort())
	}
	bad := []struct{ name, from, to string }{
		{"unknown field", `"checks": true`, `"checks": true, "extra": 1`},
		{"profile name", `"rh-1"`, `"rh-9"`},
		{"cpuset", `"2-5"`, `"5-2"`},
		{"remote gateway", `"gateway": {"address"`, `"gateway": {"ssh": "rh-1", "address"`},
		{"relative dir", `"dir": "/srv/bench"}`, `"dir": "bench"}`},
		{"ssh settings", `"sshUser": "bench", `, ``},
		{"relative secret root", `"/srv/bench/secrets"`, `"secrets"`},
		{"service port", `"10.0.0.3:8080"`, `"10.0.0.3:0"`},
		{"echo port", `"echoPort": 7000`, `"echoPort": 0`},
		{"negative gomaxprocs", `"gomaxprocs": 4`, `"gomaxprocs": -1`},
	}
	for _, tt := range bad {
		body := strings.Replace(rh1Inventory, tt.from, tt.to, 1)
		if body == rh1Inventory {
			t.Fatalf("%s: replacement did not apply", tt.name)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadProfile(path); err == nil {
			t.Errorf("ReadProfile(%s) = nil error", tt.name)
		}
	}
	if err := os.WriteFile(path, []byte(rh1Inventory+"{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProfile(path); err == nil {
		t.Error("ReadProfile(trailing data) = nil error")
	}
}

func TestLoopbackProfile(t *testing.T) {
	p := Loopback(t.TempDir())
	if err := p.Validate(); err != nil {
		t.Fatalf("Loopback().Validate() = %v", err)
	}
	if p.HardwareProfile() != "loopback" || p.Scope || p.Checks {
		t.Errorf("loopback = %+v: no scopes, no host checks", p)
	}
	for _, r := range Roles() {
		if p.Runner(r).Remote() {
			t.Errorf("loopback %s is remote", r)
		}
	}
}

// TestRunnerArgv covers the command lines: local, in a scope on a cpuset,
// and over ssh with every argument quoted.
func TestRunnerArgv(t *testing.T) {
	p := Loopback(t.TempDir())
	if got := p.Runner(RoleUpstream).Argv([]string{"labhost", "mock"}, "0-3"); !slices.Equal(got, []string{"labhost", "mock"}) {
		t.Errorf("loopback argv = %q", got)
	}
	p.Scope = true
	p.SSHUser, p.SSHKey, p.KnownHosts = "bench", "/k", "/kh"
	h := p.Hosts[RoleUpstream]
	h.SSH = "rh-1u"
	p.Hosts[RoleUpstream] = h
	got := p.Runner(RoleUpstream).Argv([]string{"labhost", "run", "--save", "", ">pass word"}, "0-3")
	want := "systemd-run --user --scope --quiet --collect -p AllowedCPUs=0-3 -- labhost run --save '' '>pass word'"
	if got[0] != "ssh" || got[len(got)-1] != want || !slices.Contains(got, "rh-1u") || !slices.Contains(got, "BatchMode=yes") {
		t.Errorf("ssh argv = %q, want the scoped command %q last", got, want)
	}
	if got := p.Runner(RoleGateway).Argv([]string{"ruralzd"}, ""); !slices.Equal(got, []string{"ruralzd"}) {
		t.Errorf("no cpuset = %q, want no scope", got)
	}
}

func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"plain":       "plain",
		"":            "''",
		"a b":         "'a b'",
		"it's":        `'it'\''s'`,
		"/x/y:1,2=3+": "/x/y:1,2=3+",
		"$HOME":       "'$HOME'",
	}
	for in, want := range tests {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTail(t *testing.T) {
	tl := &tail{max: 4}
	_, _ = tl.Write([]byte("ab"))
	_, _ = tl.Write([]byte("cdef"))
	if got := tl.String(); got != "cdef" {
		t.Errorf("tail = %q, want cdef", got)
	}
}
