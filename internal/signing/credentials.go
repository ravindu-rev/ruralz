// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Container credential file names.
const (
	// EnvDockerConfig is the directory of the standard container
	// credential file.
	EnvDockerConfig = "DOCKER_CONFIG"
	// DockerConfigFile is the standard container credential file's name.
	DockerConfigFile = "config.json"
	// dockerConfigDir is the directory under the home directory that holds
	// the standard file when DOCKER_CONFIG is unset.
	dockerConfigDir = ".docker"
	// dockerHubKey is the auths key the container tools store Docker Hub
	// credentials under.
	dockerHubKey = "https://index.docker.io/v1/"
	// helperPrefix prefixes a credential helper's name to form its
	// executable.
	helperPrefix = "docker-credential-"
	// identityTokenUser is the user name a helper returns with an identity
	// token as its secret.
	identityTokenUser = "<token>"
	// helperNotFound is the answer a helper gives for a server it holds no
	// credentials for.
	helperNotFound = "credentials not found in native keychain"
)

// Size bounds of credential reads.
const (
	// MaxCredentialFileBytes bounds the credential file.
	MaxCredentialFileBytes = 1 << 20
	// maxHelperOutputBytes bounds a helper's standard output and error.
	maxHelperOutputBytes = 64 << 10
)

// Errors of the credential store. Messages name files, hosts and
// helpers, never a credential.
var (
	// ErrCredentialFile reports a credential file that cannot be read or
	// is not in the container credential file format.
	ErrCredentialFile = errors.New("signing: registry credential file")
	// ErrCredentialHelper reports a credential helper that failed.
	ErrCredentialHelper = errors.New("signing: registry credential helper failed")
)

// Credential is what one registry host gets: a user name and password, an
// identity token (an OAuth2 refresh token the registry's token service
// exchanges), or a registry token sent as the bearer token itself. The
// zero value is anonymous. Its text forms never print a secret.
type Credential struct {
	Username      string
	Password      string
	IdentityToken string
	RegistryToken string
}

// Anonymous reports whether c holds nothing.
func (c Credential) Anonymous() bool { return c == Credential{} }

// String names the credential's kind, never its secret.
func (c Credential) String() string {
	switch {
	case c.RegistryToken != "":
		return "registry token"
	case c.IdentityToken != "":
		return "identity token"
	case c.Username != "" || c.Password != "":
		return "user name and password"
	default:
		return "anonymous"
	}
}

// GoString is String.
func (c Credential) GoString() string { return c.String() }

// Format writes String for every verb.
func (c Credential) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// helperRunner runs a credential helper's get action: the executable
// name, the server address on standard input, standard output returned.
type helperRunner func(ctx context.Context, name string, stdin []byte) (stdout []byte, err error)

// Credentials resolves registry credentials from one file in the
// container credential file format: auths, keyed by registry host (a
// key with a scheme or path, such as https://index.docker.io/v1/, matches
// its host), each holding auth (base64 user:password), username and
// password, identitytoken or registrytoken; credHelpers, a helper per
// registry host; and credsStore, the helper for every other host. A
// helper is the executable docker-credential-<name> found on PATH, run
// with the get action. A credential goes only to the registry host it is
// keyed by. The zero value and a nil *Credentials are anonymous.
// Credentials is safe for concurrent use.
type Credentials struct {
	path    string
	auths   map[string]authEntry
	helpers map[string]string
	store   string
	run     helperRunner
}

// authEntry is one auths entry.
type authEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

// credentialFile is the part of the container credential file the store
// reads; other members are ignored.
type credentialFile struct {
	Auths       map[string]json.RawMessage `json:"auths"`
	CredHelpers map[string]string          `json:"credHelpers"`
	CredsStore  string                     `json:"credsStore"`
}

// ReadCredentialFile reads path, which must exist: RURALZ_REGISTRY_AUTH_FILE
// of a Node and the CLI's --registry-auth-file. Every error matches
// ErrCredentialFile.
func ReadCredentialFile(path string) (*Credentials, error) {
	data, err := readCredentialBytes(path)
	if err != nil {
		return nil, err
	}
	return parseCredentials(path, data)
}

// DefaultCredentialFile returns the standard container credential file:
// config.json under DOCKER_CONFIG, else under .docker in the home
// directory. lookupEnv reads the environment; home returns the home
// directory.
func DefaultCredentialFile(lookupEnv func(string) (string, bool), home func() (string, error)) (string, error) {
	if dir, ok := lookupEnv(EnvDockerConfig); ok && dir != "" {
		return filepath.Join(dir, DockerConfigFile), nil
	}
	h, err := home()
	if err != nil {
		return "", fmt.Errorf("%w: no home directory for %s: %w", ErrCredentialFile, filepath.Join(dockerConfigDir, DockerConfigFile), err)
	}
	return filepath.Join(h, dockerConfigDir, DockerConfigFile), nil
}

// ReadDefaultCredentials reads the standard container credential file
// (DefaultCredentialFile); a file that does not exist is anonymous, as it
// is for the container tools.
func ReadDefaultCredentials(lookupEnv func(string) (string, bool), home func() (string, error)) (*Credentials, error) {
	path, err := DefaultCredentialFile(lookupEnv, home)
	if err != nil {
		return nil, err
	}
	data, err := readCredentialBytes(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Credentials{path: path}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseCredentials(path, data)
}

// readCredentialBytes reads a regular file of at most
// MaxCredentialFileBytes, opened without blocking on a FIFO or device.
func readCredentialBytes(path string) ([]byte, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrCredentialFile, path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrCredentialFile, path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w %s: not a regular file (mode %v)", ErrCredentialFile, path, fi.Mode())
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxCredentialFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrCredentialFile, path, err)
	}
	if len(data) > MaxCredentialFileBytes {
		return nil, fmt.Errorf("%w %s: larger than %d bytes", ErrCredentialFile, path, MaxCredentialFileBytes)
	}
	return data, nil
}

// parseCredentials decodes the file's content. An empty or blank file is
// anonymous, as it is for the container tools.
func parseCredentials(path string, data []byte) (*Credentials, error) {
	c := &Credentials{path: path, auths: map[string]authEntry{}, helpers: map[string]string{}}
	if len(bytes.TrimSpace(data)) == 0 {
		return c, nil
	}
	var f credentialFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%w %s: not a JSON object with auths, credHelpers and credsStore: %w", ErrCredentialFile, path, jsonPosition(err))
	}
	for key, raw := range f.Auths {
		var e authEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("%w %s: auths entry %q: %w", ErrCredentialFile, path, key, jsonPosition(err))
		}
		if e.Auth != "" {
			if _, _, err := decodeAuth(e.Auth); err != nil {
				return nil, fmt.Errorf("%w %s: auths entry %q: %w", ErrCredentialFile, path, key, err)
			}
		}
		c.auths[key] = e
	}
	for host, name := range f.CredHelpers {
		if err := checkHelperName(name); err != nil {
			return nil, fmt.Errorf("%w %s: credHelpers entry %q: %w", ErrCredentialFile, path, host, err)
		}
		c.helpers[host] = name
	}
	if f.CredsStore != "" {
		if err := checkHelperName(f.CredsStore); err != nil {
			return nil, fmt.Errorf("%w %s: credsStore: %w", ErrCredentialFile, path, err)
		}
		c.store = f.CredsStore
	}
	return c, nil
}

// jsonPosition strips a decoding error to its kind and offset, so it never
// quotes a credential.
func jsonPosition(err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Errorf("invalid JSON at byte %d", se.Offset)
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return fmt.Errorf("member %q has the wrong type", te.Field)
	}
	return errors.New("invalid JSON")
}

// checkHelperName refuses a helper name that is empty or would name a
// path rather than a docker-credential-<name> executable on PATH.
func checkHelperName(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("helper name %q is not a plain name", name)
	}
	return nil
}

// decodeAuth decodes an auth member: base64 of "user:password".
func decodeAuth(s string) (user, pass string, err error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", "", errors.New("auth is not base64")
	}
	u, p, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", errors.New("auth is not base64 of user:password")
	}
	return u, p, nil
}

// Path returns the file the credentials came from, empty for none.
func (c *Credentials) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

// Lookup returns the credential of a registry host (host or host:port, as
// a reference names it): its credHelpers helper, else the credsStore
// helper, else its auths entry, else anonymous. A helper that holds no
// credential for the host is anonymous; one that fails is an error
// matching ErrCredentialHelper.
func (c *Credentials) Lookup(ctx context.Context, host string) (Credential, error) {
	if c == nil {
		return Credential{}, nil
	}
	keys := serverKeys(host)
	for _, k := range keys {
		if name, ok := c.helpers[k]; ok {
			return c.fromHelper(ctx, name, k)
		}
	}
	if c.store != "" {
		return c.fromHelper(ctx, c.store, keys[0])
	}
	for _, k := range keys {
		if e, ok := c.auths[k]; ok {
			return e.credential()
		}
	}
	// A key with a scheme or path matches its host; keys are tried in
	// sorted order, so two keys of one host always resolve alike.
	for _, key := range slices.Sorted(maps.Keys(c.auths)) {
		if h := hostOf(key); h == host || (isDockerHub(host) && isDockerHub(h)) {
			return c.auths[key].credential()
		}
	}
	return Credential{}, nil
}

// serverKeys returns the keys a host's credentials are stored under, in
// lookup order: Docker Hub's hosts use the key the container tools write.
func serverKeys(host string) []string {
	if isDockerHub(host) {
		return []string{dockerHubKey, "index.docker.io", "docker.io", "registry-1.docker.io"}
	}
	return []string{host}
}

// isDockerHub reports whether host is one of Docker Hub's registry names.
func isDockerHub(host string) bool {
	switch host {
	case "docker.io", "registry-1.docker.io", "index.docker.io":
		return true
	default:
		return false
	}
}

// hostOf returns the host of an auths key that carries a scheme or a path,
// such as https://registry.example.com/v1/.
func hostOf(key string) string {
	k := key
	if i := strings.Index(k, "://"); i >= 0 {
		k = k[i+3:]
	}
	if i := strings.IndexByte(k, '/'); i >= 0 {
		k = k[:i]
	}
	return k
}

// credential converts an auths entry.
func (e authEntry) credential() (Credential, error) {
	c := Credential{Username: e.Username, Password: e.Password, IdentityToken: e.IdentityToken, RegistryToken: e.RegistryToken}
	if e.Auth != "" {
		u, p, err := decodeAuth(e.Auth)
		if err != nil {
			return Credential{}, err
		}
		c.Username, c.Password = u, p
	}
	return c, nil
}

// helperAnswer is a helper's get output.
type helperAnswer struct {
	ServerURL string `json:"ServerURL"`
	Username  string `json:"Username"`
	Secret    string `json:"Secret"`
}

// fromHelper runs the helper name's get action for server.
func (c *Credentials) fromHelper(ctx context.Context, name, server string) (Credential, error) {
	run := c.run
	if run == nil {
		run = runHelper
	}
	exe := helperPrefix + name
	out, err := run(ctx, exe, []byte(server))
	if err != nil {
		if strings.Contains(string(out), helperNotFound) {
			return Credential{}, nil
		}
		return Credential{}, fmt.Errorf("%w: %s get for %s: %w", ErrCredentialHelper, exe, server, err)
	}
	var a helperAnswer
	if err := json.Unmarshal(out, &a); err != nil {
		return Credential{}, fmt.Errorf("%w: %s get for %s: the answer is not the helper protocol's JSON", ErrCredentialHelper, exe, server)
	}
	if a.Username == identityTokenUser {
		return Credential{IdentityToken: a.Secret}, nil
	}
	return Credential{Username: a.Username, Password: a.Secret}, nil
}

// runHelper runs the helper executable name found on PATH with the get
// action, server on standard input, standing in the process environment.
// Standard output and error are bounded; standard error never reaches the
// returned error, which could leak it.
func runHelper(ctx context.Context, name string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, "get")
	cmd.Stdin = bytes.NewReader(stdin)
	stdout := &boundedBuffer{max: maxHelperOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = &boundedBuffer{max: maxHelperOutputBytes}
	err := cmd.Run()
	if stdout.over {
		return nil, fmt.Errorf("standard output over %d bytes", maxHelperOutputBytes)
	}
	return stdout.Bytes(), err
}

// boundedBuffer keeps the first max bytes written to it and discards the
// rest, recording that it did.
type boundedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

// Write keeps what fits and reports the whole write as done.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		b.over = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
