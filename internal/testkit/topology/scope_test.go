// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestNewScope: Config.CPUs must be a cpuset list, needs systemd-run and
// the user manager's variables, and keeps exactly those variables, so a
// topology without the means to start a scope fails at New instead of at
// each Node's start.
func TestNewScope(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	found := func(string) (string, error) { return "/usr/bin/systemd-run", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	if s, err := newScope("", env(nil), missing); s != nil || err != nil {
		t.Errorf("newScope(\"\") = %+v, %v; want no scope", s, err)
	}
	for _, c := range []struct {
		name, cpus string
		getenv     func(string) string
		lookPath   func(string) (string, error)
		want       string
	}{
		{"malformed cpuset", "2-x", env(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}), found, "is not a cpuset list"},
		{"reversed cpuset", "5-2", env(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}), found, "is not a cpuset list"},
		{"no systemd-run", "2-5", env(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}), missing, "need systemd-run"},
		{"no user manager", "2-5", env(map[string]string{"HOME": "/home/u"}), found, "XDG_RUNTIME_DIR or DBUS_SESSION_BUS_ADDRESS"},
	} {
		if s, err := newScope(c.cpus, c.getenv, c.lookPath); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: newScope(%q) = %+v, %v; want an error naming %q", c.name, c.cpus, s, err, c.want)
		}
	}
	s, err := newScope("2-5,8", env(map[string]string{
		"XDG_RUNTIME_DIR": "/run/user/1000", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus", "HOME": "/home/u",
	}), found)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus"}; s.run != "/usr/bin/systemd-run" || s.cpus != 5 || !slices.Equal(s.env, want) {
		t.Errorf("newScope(2-5,8) = %+v, want systemd-run on 5 CPUs with only %q", s, want)
	}
}

// TestScopeArgs: the scope is the user manager's, collected when it ends,
// on AllowedCPUs, and runs the command after "--" as given.
func TestScopeArgs(t *testing.T) {
	got := ScopeArgs("2-5", "/bin/ruralzd", "--flag")
	want := []string{"--user", "--scope", "--quiet", "--collect", "-p", "AllowedCPUs=2-5", "--", "/bin/ruralzd", "--flag"}
	if !slices.Equal(got, want) {
		t.Errorf("ScopeArgs = %q, want %q", got, want)
	}
}
