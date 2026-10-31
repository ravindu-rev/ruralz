// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/testkit/canary"
)

// canarySet holds the canaries the leak scan looks for.
type canarySet struct {
	mu     sync.Mutex
	values []canary.Canary
}

func (s *canarySet) add(c canary.Canary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = append(s.values, c)
}

func (s *canarySet) list() []canary.Canary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.values)
}

// carries reports whether b holds one of the canaries.
func (s *canarySet) carries(b []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.values {
		if bytes.Contains(b, []byte(c.Value)) {
			return true
		}
	}
	return false
}

// Canary returns a new canary, registered for the teardown leak scan: a
// secret value of its own, such as a token, an API key, a client secret, a
// password or a MAC key (41 bytes of b64token characters), or a part of
// one. It fails the test when the random source fails.
func (t *Topology) Canary() string {
	c, err := t.newCanary()
	if err != nil {
		t.t.Fatalf("topology: %v", err)
	}
	return c
}

func (t *Topology) newCanary() (string, error) {
	c, err := canary.New()
	if err != nil {
		return "", fmt.Errorf("topology: %w", err)
	}
	t.canaries.add(c)
	return c.Value, nil
}

// WithCanary returns pem after a line holding a new registered canary:
// explanatory text that PEM parsers skip, so a certificate, key or CA file
// carries a canary and still parses.
func (t *Topology) WithCanary(pem []byte) []byte {
	return append([]byte(t.Canary()+"\n"), pem...)
}

// ServerCertificate issues a server certificate and its key from the
// harness's CA for hosts (default localhost, 127.0.0.1 and ::1), each PEM
// carrying a canary (WithCanary), for a listener's tls.certificates.
func (t *Topology) ServerCertificate(hosts ...string) (certPEM, keyPEM []byte, err error) {
	if len(hosts) == 0 {
		hosts = loopbackHosts()
	}
	leaf, err := t.ca.Server(hosts...)
	if err != nil {
		return nil, nil, fmt.Errorf("topology: %w", err)
	}
	chain := append(bytes.Clone(leaf.CertPEM), t.ca.CertPEM...)
	return t.WithCanary(chain), t.WithCanary(leaf.KeyPEM), nil
}

// SecretRef is one secretRef of a rendered Bundle and the file its test
// render resolves it from.
type SecretRef struct {
	// Ref is the reference as authored, "provider:name" or
	// "provider:name#key" (internal/secret.Ref's form): the key of
	// NodeOptions.Secrets.
	Ref string
	// Provider, Name and Key are its members as authored.
	Provider, Name, Key string
	// Path is the file under the Node's RURALZ_SECRET_ROOT that the test
	// render names, with provider file and the same key.
	Path string
}

// refString returns a reference's NodeOptions.Secrets key.
func refString(provider, name, key string) string {
	s := provider + ":" + name
	if key != "" {
		s += "#" + key
	}
	return s
}

// secretPath maps a reference to its file under root: the provider, then
// the name as a relative path, so every reference of one provider and name
// shares a file and no name leaves root.
func secretPath(root, provider, name string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(strings.TrimLeft(name, `/\`)))
	if name == "" || provider == "" || !filepath.IsLocal(rel) || strings.ContainsAny(provider, `/\.`) {
		return "", fmt.Errorf("topology: secretRef %s cannot map to a file under the secret root", refString(provider, name, ""))
	}
	return filepath.Join(root, provider, rel), nil
}

// writeSecrets writes the files of refs under the Node's secret root and
// records each reference's value. A file holds the value supplied for its
// unkeyed reference ("provider:name") as it is, or else a JSON object of
// its keyed references' values, or else the value of its one unkeyed
// reference; a value never supplied is a fresh canary, kept across
// re-renders.
func (n *Node) writeSecrets(refs []SecretRef) error {
	byPath := map[string][]SecretRef{}
	var paths []string
	for _, r := range refs {
		if _, ok := byPath[r.Path]; !ok {
			paths = append(paths, r.Path)
		}
		byPath[r.Path] = append(byPath[r.Path], r)
	}
	for _, path := range paths {
		group := byPath[path]
		whole := refString(group[0].Provider, group[0].Name, "")
		if n.missing(whole) {
			continue
		}
		content, err := n.fileContent(whole, group)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("topology: %s: %w", n.name, err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return fmt.Errorf("topology: %s: %w", n.name, err)
		}
	}
	return nil
}

// fileContent returns the content of the file of group, whose unkeyed
// reference is whole.
func (n *Node) fileContent(whole string, group []SecretRef) ([]byte, error) {
	v, ok, err := n.supplied(whole)
	if err != nil {
		return nil, err
	}
	if ok {
		n.secrets[whole] = v
		return v, nil
	}
	keyed := map[string]string{}
	for _, r := range group {
		if r.Key == "" || n.missing(r.Ref) {
			continue
		}
		v, err := n.value(r.Ref)
		if err != nil {
			return nil, err
		}
		keyed[r.Key] = string(v)
	}
	if len(keyed) > 0 {
		b, err := json.Marshal(keyed)
		if err != nil {
			return nil, fmt.Errorf("topology: %s: secret %s: %w", n.name, whole, err)
		}
		return b, nil
	}
	return n.value(whole)
}

// value returns the value of ref: supplied, a State Store URL, kept from an
// earlier render, or a fresh canary.
func (n *Node) value(ref string) ([]byte, error) {
	v, ok, err := n.supplied(ref)
	if err != nil {
		return nil, err
	}
	if ok {
		n.secrets[ref] = v
		return v, nil
	}
	if v, ok := n.secrets[ref]; ok {
		return v, nil
	}
	c, err := n.t.newCanary()
	if err != nil {
		return nil, err
	}
	n.secrets[ref] = []byte(c)
	return []byte(c), nil
}

// supplied returns the value the options give ref: NodeOptions.Secrets,
// which must carry a canary, or the State Store URL of
// NodeOptions.StateStoreURLs.
func (n *Node) supplied(ref string) ([]byte, bool, error) {
	if v, ok := n.opts.Secrets[ref]; ok {
		if !n.t.canaries.carries(v) {
			return nil, false, fmt.Errorf("topology: %s: the value of secret %s carries no canary from Topology.Canary or WithCanary", n.name, ref)
		}
		return v, true, nil
	}
	if _, ok := n.opts.StateStoreURLs[ref]; ok {
		u, ok := n.storeURLs[ref]
		if !ok {
			return nil, false, fmt.Errorf("topology: %s: secret %s names a State Store the Node has no proxy for", n.name, ref)
		}
		return []byte(u), true, nil
	}
	return nil, false, nil
}

// missing reports whether ref is listed in NodeOptions.MissingSecrets.
func (n *Node) missing(ref string) bool { return slices.Contains(n.opts.MissingSecrets, ref) }

// checkSecretEnv refuses a secret process variable (RURALZ_STATE_STORE_URL
// or RURALZ_SECRET_*) whose value carries no canary.
func (t *Topology) checkSecretEnv(env []string) error {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k != "RURALZ_STATE_STORE_URL" && !strings.HasPrefix(k, "RURALZ_SECRET_") {
			continue
		}
		if !t.canaries.carries([]byte(v)) {
			return fmt.Errorf("topology: the secret variable %s carries no canary from Topology.Canary", k)
		}
	}
	return nil
}
