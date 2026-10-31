// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// Tests for the admin flags of docs/reference/01-cli-and-api-surface.md
// "Global flags" and "Bundle verbs in detail": their names and help,
// RURALZ_ADMIN_URL and RURALZ_ADMIN_TOKEN_FILE standing in for absent
// flags, a flag always winning, --timeout taking 0, and the per-side
// credentials of ruralz bundle diff falling back to the shared ones.

// parseFlags binds the admin flags with o, parses args and returns them
// with an IO over environ on goos.
func parseFlags(t *testing.T, o FlagOptions, goos string, environ []string, args ...string) (*Flags, command.IO) {
	t.Helper()
	f := command.NewFlags("ruralz test admin")
	fl := &Flags{}
	fl.Bind(f, o)
	if _, err := f.Parse(args); err != nil {
		t.Fatalf("Parse(%q) = %v", args, err)
	}
	return fl, command.NewIO(io.Discard, io.Discard, environ, epoch, goos)
}

// TestFlagNames checks the flags each option binds and their help.
func TestFlagNames(t *testing.T) {
	f := command.NewFlags("ruralz node dump")
	(&Flags{}).Bind(f, FlagOptions{URL: true, Timeout: true})
	var names []string
	details := map[string]command.FlagSpec{}
	for _, s := range f.Specs() {
		names = append(names, s.Name)
		details[s.Name] = s
	}
	want := []string{"admin", "admin-token-file", "ca-file", "client-cert", "client-key", "timeout"}
	if !slices.Equal(names, want) {
		t.Fatalf("flags = %q, want %q", names, want)
	}
	if s := details["admin"]; s.Placeholder != "URL" || s.Default != "" ||
		s.Usage != "Admin base URL of the Node (default $RURALZ_ADMIN_URL, else http://127.0.0.1:9901)" {
		t.Errorf("--admin = %+v", s)
	}
	if s := details["admin-token-file"]; s.Placeholder != "PATH" || s.Complete != command.CompleteFile ||
		s.Usage != "Operator admin token file (default $RURALZ_ADMIN_TOKEN_FILE)" {
		t.Errorf("--admin-token-file = %+v", s)
	}
	if s := details["timeout"]; s.Placeholder != "DURATION" || s.Default != "5m" || !s.ZeroNone {
		t.Errorf("--timeout = %+v", s)
	}

	g := command.NewFlags("ruralz dev tap")
	(&Flags{}).Bind(g, FlagOptions{URL: true})
	if slices.ContainsFunc(g.Specs(), func(s command.FlagSpec) bool { return s.Name == "timeout" }) {
		t.Error("a streaming command got --timeout")
	}

	h := command.NewFlags("ruralz bundle diff")
	(&Flags{}).Bind(h, FlagOptions{Timeout: true, Sides: []string{"from", "to"}})
	names = names[:0]
	for _, s := range h.Specs() {
		names = append(names, s.Name)
		if s.Name == "from-client-key" && s.Usage != "Key of --from-client-cert (default --client-key)" {
			t.Errorf("--from-client-key = %+v", s)
		}
		if s.Name == "to-admin-token-file" && s.Usage != "Operator admin token file for TO (default --admin-token-file)" {
			t.Errorf("--to-admin-token-file = %+v", s)
		}
	}
	want = []string{
		"admin-token-file", "ca-file", "client-cert", "client-key",
		"from-admin-token-file", "from-client-cert", "from-client-key", "timeout",
		"to-admin-token-file", "to-client-cert", "to-client-key",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("bundle diff flags = %q, want %q", names, want)
	}
}

// TestConfigURL covers --admin, RURALZ_ADMIN_URL and the default: a flag
// always wins, even given empty, and an empty variable is unset.
func TestConfigURL(t *testing.T) {
	cases := []struct {
		name     string
		environ  []string
		args     []string
		url, src string
	}{
		{"default", nil, nil, DefaultURL, "--admin"},
		{"environment", []string{"RURALZ_ADMIN_URL=https://node-a:9901"}, nil, "https://node-a:9901", EnvURL},
		{"empty environment", []string{"RURALZ_ADMIN_URL="}, nil, DefaultURL, "--admin"},
		{"flag wins", []string{"RURALZ_ADMIN_URL=https://node-a:9901"}, []string{"--admin", "http://127.0.0.1:19901"}, "http://127.0.0.1:19901", "--admin"},
		{"empty flag wins", []string{"RURALZ_ADMIN_URL=https://node-a:9901"}, []string{"--admin="}, "", "--admin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fl, sio := parseFlags(t, FlagOptions{URL: true}, "linux", c.environ, c.args...)
			cfg := fl.Config(sio)
			if cfg.URL != c.url || cfg.Names.URL != c.src {
				t.Errorf("Config = %q from %q, want %q from %q", cfg.URL, cfg.Names.URL, c.url, c.src)
			}
		})
	}
	// An empty --admin is an admin URL without a host: a usage error.
	fl, sio := parseFlags(t, FlagOptions{URL: true}, "linux", nil, "--admin=")
	if _, err := New(fl.Config(sio), Options{}); err == nil {
		t.Error("New accepted an empty --admin")
	}
}

// TestConfigURLWindowsCase checks that on Windows the variable name
// matches without regard to case, as the process environment does there.
func TestConfigURLWindowsCase(t *testing.T) {
	fl, sio := parseFlags(t, FlagOptions{URL: true}, "windows", []string{"ruralz_admin_url=https://node-a:9901"})
	if cfg := fl.Config(sio); cfg.URL != "https://node-a:9901" {
		t.Errorf("Config URL = %q", cfg.URL)
	}
}

// TestConfigTokenFile covers --admin-token-file and
// RURALZ_ADMIN_TOKEN_FILE: the flag wins, given empty it sends no token,
// and an empty variable is unset.
func TestConfigTokenFile(t *testing.T) {
	cases := []struct {
		name      string
		environ   []string
		args      []string
		file, src string
	}{
		{"none", nil, nil, "", "--admin-token-file"},
		{"environment", []string{"RURALZ_ADMIN_TOKEN_FILE=/run/secrets/admin"}, nil, "/run/secrets/admin", EnvTokenFile},
		{"empty environment", []string{"RURALZ_ADMIN_TOKEN_FILE="}, nil, "", "--admin-token-file"},
		{"flag wins", []string{"RURALZ_ADMIN_TOKEN_FILE=/run/secrets/admin"}, []string{"--admin-token-file", "tok"}, "tok", "--admin-token-file"},
		{"empty flag sends none", []string{"RURALZ_ADMIN_TOKEN_FILE=/run/secrets/admin"}, []string{"--admin-token-file="}, "", "--admin-token-file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fl, sio := parseFlags(t, FlagOptions{URL: true}, "linux", c.environ, c.args...)
			cfg := fl.Config(sio)
			if cfg.TokenFile != c.file || cfg.Names.TokenFile != c.src {
				t.Errorf("Config token file = %q from %q, want %q from %q", cfg.TokenFile, cfg.Names.TokenFile, c.file, c.src)
			}
		})
	}
}

// TestConfigTimeout checks --timeout: 5m by default, 0 for none, and
// DefaultTimeout for a command without the flag.
func TestConfigTimeout(t *testing.T) {
	for _, c := range []struct {
		o    FlagOptions
		args []string
		want time.Duration
	}{
		{FlagOptions{URL: true, Timeout: true}, nil, 5 * time.Minute},
		{FlagOptions{URL: true, Timeout: true}, []string{"--timeout", "0"}, 0},
		{FlagOptions{URL: true, Timeout: true}, []string{"--timeout=90s"}, 90 * time.Second},
		{FlagOptions{URL: true}, nil, DefaultTimeout},
	} {
		fl, sio := parseFlags(t, c.o, "linux", nil, c.args...)
		if got := fl.Config(sio).Timeout; got != c.want {
			t.Errorf("Config(%q) timeout = %v, want %v", c.args, got, c.want)
		}
	}
}

// TestConfigTLSFiles checks that --ca-file, --client-cert and
// --client-key reach the Config with their names.
func TestConfigTLSFiles(t *testing.T) {
	fl, sio := parseFlags(t, FlagOptions{URL: true}, "linux", nil,
		"--ca-file", "ca.pem", "--client-cert", "c.pem", "--client-key", "k.pem")
	cfg := fl.Config(sio)
	if cfg.CAFile != "ca.pem" || cfg.ClientCert != "c.pem" || cfg.ClientKey != "k.pem" ||
		cfg.Names.ClientCert != "--client-cert" || cfg.Names.ClientKey != "--client-key" {
		t.Errorf("Config = %+v", cfg)
	}
}

// TestNodeConfigSides covers ruralz bundle diff's per-side credentials:
// each side's flags win, else the shared flags, else the environment; a
// side's client certificate pair replaces the shared pair whole; --ca-file
// and --timeout serve both sides.
func TestNodeConfigSides(t *testing.T) {
	o := FlagOptions{Timeout: true, Sides: []string{"from", "to"}}
	env := []string{"RURALZ_ADMIN_TOKEN_FILE=/env/token"}
	fl, sio := parseFlags(t, o, "linux", env,
		"--from-admin-token-file", "from.tok", "--client-cert", "shared.crt", "--client-key", "shared.key",
		"--to-client-cert", "to.crt", "--to-client-key", "to.key", "--ca-file", "ca.pem", "--timeout", "1m")
	from := fl.NodeConfig(sio, "from", "FROM", "https://node-a:9901")
	to := fl.NodeConfig(sio, "to", "TO", "https://node-b:9901")
	if from.URL != "https://node-a:9901" || from.Names.URL != "FROM" ||
		from.TokenFile != "from.tok" || from.Names.TokenFile != "--from-admin-token-file" ||
		from.ClientCert != "shared.crt" || from.ClientKey != "shared.key" || from.Names.ClientCert != "--client-cert" {
		t.Errorf("FROM = %+v", from)
	}
	if to.Names.URL != "TO" || to.TokenFile != "/env/token" || to.Names.TokenFile != EnvTokenFile ||
		to.ClientCert != "to.crt" || to.ClientKey != "to.key" ||
		to.Names.ClientCert != "--to-client-cert" || to.Names.ClientKey != "--to-client-key" {
		t.Errorf("TO = %+v", to)
	}
	for _, cfg := range []Config{from, to} {
		if cfg.CAFile != "ca.pem" || cfg.Timeout != time.Minute {
			t.Errorf("shared values = %q, %v", cfg.CAFile, cfg.Timeout)
		}
	}

	// The shared token file stands in for a side's, and wins over the
	// environment; an empty side flag sends no token.
	fl, sio = parseFlags(t, o, "linux", env, "--admin-token-file", "shared.tok", "--to-admin-token-file=")
	if cfg := fl.NodeConfig(sio, "from", "FROM", "http://127.0.0.1:9901"); cfg.TokenFile != "shared.tok" || cfg.Names.TokenFile != "--admin-token-file" {
		t.Errorf("FROM token = %q from %q", cfg.TokenFile, cfg.Names.TokenFile)
	}
	if cfg := fl.NodeConfig(sio, "to", "TO", "http://127.0.0.1:9901"); cfg.TokenFile != "" {
		t.Errorf("TO token = %q, want none", cfg.TokenFile)
	}
}

// TestNodeConfigHalfPair checks that a side's certificate without its key
// is a usage error naming the side's flags, even with a shared pair.
func TestNodeConfigHalfPair(t *testing.T) {
	fl, sio := parseFlags(t, FlagOptions{Sides: []string{"from", "to"}}, "linux", nil,
		"--from-client-cert", "from.crt", "--client-cert", "shared.crt", "--client-key", "shared.key")
	_, err := New(fl.NodeConfig(sio, "from", "FROM", "https://node-a:9901"), Options{})
	if _, ok := errors.AsType[*command.UsageError](err); !ok || err.Error() != "--from-client-cert and --from-client-key come together" {
		t.Fatalf("New = %v", err)
	}
	fl, sio = parseFlags(t, FlagOptions{URL: true}, "linux", nil, "--client-key", "k.pem")
	_, err = New(fl.Config(sio), Options{})
	if _, ok := errors.AsType[*command.UsageError](err); !ok || err.Error() != "--client-cert and --client-key come together" {
		t.Fatalf("New = %v", err)
	}
}
