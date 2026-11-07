// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Tests for registry credentials in the container credential file format
// (docs/architecture/04-control-plane-and-gitops.md "Node process
// configuration", RURALZ_REGISTRY_AUTH_FILE; docs/reference/01-cli-and-api-surface.md
// "Bundle verbs in detail"): auths, credHelpers and credsStore, the
// credential helper protocol, and each credential sent only to its host.

// TestMain runs the test binary as a credential helper when it is
// started under a docker-credential- name (fakeHelper).
func TestMain(m *testing.M) {
	if strings.HasPrefix(filepath.Base(os.Args[0]), helperPrefix) {
		os.Exit(fakeHelper(os.Stdin, os.Stdout, os.Stderr, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// helperModeEnv selects what fakeHelper answers.
const helperModeEnv = "RURALZ_TEST_HELPER_MODE"

// fakeHelper is a credential helper: get reads the server on standard
// input and answers by the mode helperModeEnv names.
func fakeHelper(stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	if len(args) != 1 || args[0] != "get" {
		_, _ = fmt.Fprintln(stderr, "usage: get")
		return 2
	}
	server, _ := io.ReadAll(stdin)
	switch os.Getenv(helperModeEnv) {
	case "notfound":
		_, _ = fmt.Fprintln(stdout, helperNotFound)
		return 1
	case "fail":
		_, _ = fmt.Fprintln(stderr, "helper-stderr-secret")
		return 3
	case "token":
		_ = json.NewEncoder(stdout).Encode(helperAnswer{ServerURL: string(server), Username: identityTokenUser, Secret: "identity-" + string(server)}) //nolint:gosec // G117: the fake helper's answer carries a test secret by design.
		return 0
	case "big":
		_, _ = stdout.Write([]byte(strings.Repeat("x", maxHelperOutputBytes+1)))
		return 0
	default:
		_ = json.NewEncoder(stdout).Encode(helperAnswer{ServerURL: string(server), Username: "helper-user", Secret: "pass-" + string(server)}) //nolint:gosec // G117: the fake helper's answer carries a test secret by design.
		return 0
	}
}

// writeFile writes data to a new file in a test directory.
func writeFile(t *testing.T, name, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// basic is the auth member of user and pass.
func basic(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

func TestReadCredentialFileAuths(t *testing.T) {
	file := fmt.Sprintf(`{
		"auths": {
			"ghcr.io": {"auth": %q},
			"registry.example.com:5000": {"username": "u2", "password": "p2"},
			"https://legacy.example.com/v1/": {"auth": %q},
			"oauth.example.com": {"identitytoken": "refresh-1"},
			"token.example.com": {"registrytoken": "bearer-1"},
			"https://index.docker.io/v1/": {"auth": %q}
		},
		"psFormat": "ignored"
	}`, basic("u1", "p1:with:colons"), basic("u3", "p3"), basic("hub", "hubpass"))
	c, err := ReadCredentialFile(writeFile(t, "config.json", file))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	tests := []struct {
		host string
		want Credential
	}{
		{"ghcr.io", Credential{Username: "u1", Password: "p1:with:colons"}},
		{"registry.example.com:5000", Credential{Username: "u2", Password: "p2"}},
		{"legacy.example.com", Credential{Username: "u3", Password: "p3"}},
		{"oauth.example.com", Credential{IdentityToken: "refresh-1"}},
		{"token.example.com", Credential{RegistryToken: "bearer-1"}},
		{"docker.io", Credential{Username: "hub", Password: "hubpass"}},
		{"registry-1.docker.io", Credential{Username: "hub", Password: "hubpass"}},
		// A credential goes to its own host only: no port, subdomain or
		// other host shares it.
		{"registry.example.com", Credential{}},
		{"ghcr.io:443", Credential{}},
		{"evil.ghcr.io", Credential{}},
		{"other.example.com", Credential{}},
	}
	for _, tt := range tests {
		got, err := c.Lookup(ctx, tt.host)
		if err != nil || got != tt.want {
			t.Errorf("Lookup(%q) = %v %v, want %v", tt.host, got, err, tt.want)
		}
	}
	if c.Path() == "" {
		t.Error("Path() is empty")
	}
}

func TestReadCredentialFileRefuses(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]struct {
		path string
		is   error
		not  string // text the error must not hold
	}{
		"missing":         {path: filepath.Join(dir, "missing.json"), is: fs.ErrNotExist},
		"directory":       {path: dir},
		"too large":       {path: writeFile(t, "big.json", `{"auths":{}}`+strings.Repeat(" ", MaxCredentialFileBytes))},
		"not JSON":        {path: writeFile(t, "bad.json", `{"auths": {"ghcr.io": {"auth": "secret-value`), not: "secret-value"},
		"wrong type":      {path: writeFile(t, "type.json", `{"auths": {"ghcr.io": {"password": 42}}}`)},
		"auth not base64": {path: writeFile(t, "b64.json", `{"auths": {"ghcr.io": {"auth": "not base64!"}}}`)},
		"auth no colon":   {path: writeFile(t, "colon.json", fmt.Sprintf(`{"auths": {"ghcr.io": {"auth": %q}}}`, base64.StdEncoding.EncodeToString([]byte("nocolon"))))},
		"helper path":     {path: writeFile(t, "helper.json", `{"credHelpers": {"ghcr.io": "../evil"}}`)},
		"store path":      {path: writeFile(t, "store.json", `{"credsStore": "/bin/sh"}`)},
		"empty helper":    {path: writeFile(t, "empty.json", `{"credHelpers": {"ghcr.io": ""}}`)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c, err := ReadCredentialFile(tt.path)
			if err == nil {
				t.Fatalf("ReadCredentialFile = %v, want an error", c)
			}
			if !errors.Is(err, ErrCredentialFile) {
				t.Errorf("error %v does not match ErrCredentialFile", err)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("error %v does not match %v", err, tt.is)
			}
			if tt.not != "" && strings.Contains(err.Error(), tt.not) {
				t.Errorf("error %q quotes the file", err)
			}
		})
	}
}

func TestEmptyCredentialFileIsAnonymous(t *testing.T) {
	for _, content := range []string{"", "  \n", "{}"} {
		c, err := ReadCredentialFile(writeFile(t, "config.json", content))
		if err != nil {
			t.Fatalf("ReadCredentialFile(%q): %v", content, err)
		}
		if got, err := c.Lookup(t.Context(), "ghcr.io"); err != nil || !got.Anonymous() {
			t.Errorf("Lookup on %q = %v %v", content, got, err)
		}
	}
	var none *Credentials
	if got, err := none.Lookup(t.Context(), "ghcr.io"); err != nil || !got.Anonymous() || none.Path() != "" {
		t.Errorf("nil Credentials: %v %v", got, err)
	}
}

func TestDefaultCredentialFile(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	home := func() (string, error) { return filepath.Join("home", "user"), nil }
	got, err := DefaultCredentialFile(env(map[string]string{"DOCKER_CONFIG": filepath.Join("etc", "docker")}), home)
	if err != nil || got != filepath.Join("etc", "docker", "config.json") {
		t.Errorf("with DOCKER_CONFIG = %q, %v", got, err)
	}
	got, err = DefaultCredentialFile(env(nil), home)
	if err != nil || got != filepath.Join("home", "user", ".docker", "config.json") {
		t.Errorf("without DOCKER_CONFIG = %q, %v", got, err)
	}
	got, err = DefaultCredentialFile(env(map[string]string{"DOCKER_CONFIG": ""}), home)
	if err != nil || got != filepath.Join("home", "user", ".docker", "config.json") {
		t.Errorf("with an empty DOCKER_CONFIG = %q, %v", got, err)
	}
	if _, err := DefaultCredentialFile(env(nil), func() (string, error) { return "", errors.New("no home") }); !errors.Is(err, ErrCredentialFile) {
		t.Errorf("without a home directory: %v", err)
	}
}

func TestReadDefaultCredentials(t *testing.T) {
	dir := t.TempDir()
	env := func(k string) (string, bool) {
		if k == EnvDockerConfig {
			return dir, true
		}
		return "", false
	}
	home := func() (string, error) { return "", errors.New("unused") }
	c, err := ReadDefaultCredentials(env, home)
	if err != nil {
		t.Fatalf("a missing standard file: %v", err)
	}
	if got, _ := c.Lookup(t.Context(), "ghcr.io"); !got.Anonymous() || c.Path() != filepath.Join(dir, "config.json") {
		t.Fatalf("missing standard file: %v at %q", got, c.Path())
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auths":{"ghcr.io":{"username":"u","password":"p"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = ReadDefaultCredentials(env, home)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Lookup(t.Context(), "ghcr.io"); got != (Credential{Username: "u", Password: "p"}) {
		t.Fatalf("Lookup = %v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDefaultCredentials(env, home); !errors.Is(err, ErrCredentialFile) {
		t.Fatalf("an invalid standard file: %v", err)
	}
}

// recordingRunner answers like a helper and records the calls.
type recordingRunner struct {
	calls []string
	out   map[string][]byte
	err   error
}

func (r *recordingRunner) run(_ context.Context, name string, stdin []byte) ([]byte, error) {
	r.calls = append(r.calls, name+" "+string(stdin))
	return r.out[name], r.err
}

func TestLookupOrder(t *testing.T) {
	file := fmt.Sprintf(`{
		"auths": {"ghcr.io": {"auth": %q}, "plain.example.com": {"auth": %q}},
		"credHelpers": {"ghcr.io": "ghcr", "https://index.docker.io/v1/": "hub"},
		"credsStore": "store"
	}`, basic("file", "file"), basic("file", "file"))
	c, err := ReadCredentialFile(writeFile(t, "config.json", file))
	if err != nil {
		t.Fatal(err)
	}
	answer := func(user string) []byte {
		b, _ := json.Marshal(helperAnswer{Username: user, Secret: "s-" + user}) //nolint:gosec // G117: a fake helper answer with a test secret.
		return b
	}
	rr := &recordingRunner{out: map[string][]byte{
		"docker-credential-ghcr": answer("ghcr"), "docker-credential-hub": answer("hub"), "docker-credential-store": answer("store"),
	}}
	c.run = rr.run
	ctx := t.Context()
	for host, user := range map[string]string{
		// credHelpers for the host first, then credsStore for every other
		// host, which hides the auths entries as the container tools do.
		"ghcr.io": "ghcr", "docker.io": "hub", "plain.example.com": "store", "other.example.com": "store",
	} {
		got, err := c.Lookup(ctx, host)
		if err != nil || got != (Credential{Username: user, Password: "s-" + user}) {
			t.Errorf("Lookup(%q) = %v %v, want %s", host, got, err, user)
		}
	}
	want := []string{"docker-credential-ghcr ghcr.io", "docker-credential-hub https://index.docker.io/v1/", "docker-credential-store plain.example.com", "docker-credential-store other.example.com"}
	for _, w := range want {
		found := false
		for _, c := range rr.calls {
			found = found || c == w
		}
		if !found {
			t.Errorf("no helper call %q among %q", w, rr.calls)
		}
	}
}

func TestHelperAnswers(t *testing.T) {
	c, err := ReadCredentialFile(writeFile(t, "config.json", `{"credsStore": "fake"}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	tests := []struct {
		name    string
		out     string
		err     error
		want    Credential
		wantErr bool
	}{
		{name: "user and secret", out: `{"ServerURL":"ghcr.io","Username":"u","Secret":"s"}`, want: Credential{Username: "u", Password: "s"}},
		{name: "identity token", out: `{"ServerURL":"ghcr.io","Username":"<token>","Secret":"refresh"}`, want: Credential{IdentityToken: "refresh"}},
		{name: "not found", out: "credentials not found in native keychain\n", err: errors.New("exit status 1"), want: Credential{}},
		{name: "failure", out: "boom-secret", err: errors.New("exit status 2"), wantErr: true},
		{name: "not the protocol", out: "garbage-secret", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.run = func(context.Context, string, []byte) ([]byte, error) { return []byte(tt.out), tt.err }
			got, err := c.Lookup(ctx, "ghcr.io")
			if tt.wantErr {
				if !errors.Is(err, ErrCredentialHelper) {
					t.Fatalf("Lookup = %v %v, want ErrCredentialHelper", got, err)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatalf("error %q leaks the helper's output", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Lookup = %v %v, want %v", got, err, tt.want)
			}
		})
	}
}

// installHelper puts the test binary on a new PATH as
// docker-credential-<name>, answering in mode.
func installHelper(t *testing.T, name, mode string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe) //nolint:gosec // G304: the running test binary.
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := helperPrefix + name
	if runtime.GOOS == "windows" {
		file += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, file), data, 0o700); err != nil { //nolint:gosec // G306: an executable the test runs.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv(helperModeEnv, mode)
}

func TestHelperExecutable(t *testing.T) {
	c, err := ReadCredentialFile(writeFile(t, "config.json", `{"credHelpers": {"ghcr.io": "fake"}}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	installHelper(t, "fake", "ok")
	got, err := c.Lookup(ctx, "ghcr.io")
	if err != nil || got != (Credential{Username: "helper-user", Password: "pass-ghcr.io"}) {
		t.Fatalf("ok: %v %v", got, err)
	}
	t.Setenv(helperModeEnv, "token")
	if got, err := c.Lookup(ctx, "ghcr.io"); err != nil || got != (Credential{IdentityToken: "identity-ghcr.io"}) {
		t.Fatalf("token: %v %v", got, err)
	}
	t.Setenv(helperModeEnv, "notfound")
	if got, err := c.Lookup(ctx, "ghcr.io"); err != nil || !got.Anonymous() {
		t.Fatalf("notfound: %v %v", got, err)
	}
	t.Setenv(helperModeEnv, "fail")
	if got, err := c.Lookup(ctx, "ghcr.io"); !errors.Is(err, ErrCredentialHelper) || strings.Contains(err.Error(), "helper-stderr-secret") {
		t.Fatalf("fail: %v %v", got, err)
	}
	t.Setenv(helperModeEnv, "big")
	if got, err := c.Lookup(ctx, "ghcr.io"); !errors.Is(err, ErrCredentialHelper) {
		t.Fatalf("big: %v %v", got, err)
	}
	// A helper that is not installed fails.
	c2, err := ReadCredentialFile(writeFile(t, "config.json", `{"credHelpers": {"ghcr.io": "absent"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c2.Lookup(ctx, "ghcr.io"); !errors.Is(err, ErrCredentialHelper) {
		t.Fatalf("absent helper: %v %v", got, err)
	}
}

func TestCredentialNeverPrintsSecrets(t *testing.T) {
	for _, c := range []Credential{
		{Username: "alice", Password: "hunter2"}, {IdentityToken: "hunter2"}, {RegistryToken: "hunter2"}, {},
	} {
		for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s %q", c, c, c, c, c), c.String(), c.GoString()} {
			if strings.Contains(s, "hunter2") || strings.Contains(s, "alice") {
				t.Errorf("%q prints a secret", s)
			}
		}
	}
}
