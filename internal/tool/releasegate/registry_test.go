// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRegistry is an OCI distribution registry of one repository that
// answers anonymous requests with a bearer challenge, as ghcr.io does, and
// issues a pull or push token for the expected credentials.
type fakeRegistry struct {
	t         *testing.T
	mu        sync.Mutex
	srv       *httptest.Server
	manifests map[string][]byte // digest -> bytes
	tags      map[string]string // tag -> digest
	puts      []string
	// noType answers manifests without a Content-Type.
	noType bool
	// corrupt serves other bytes for the digest.
	corrupt bool
}

const fakeIndexType = "application/vnd.oci.image.index.v1+json"

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	r := &fakeRegistry{t: t, manifests: map[string][]byte{}, tags: map[string]string{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

// add stores a manifest and returns its digest.
func (r *fakeRegistry) add(data []byte) string {
	d := digestOf(data)
	r.manifests[d] = data
	return d
}

func (r *fakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.URL.Path == "/token" {
		user, pass, ok := req.BasicAuth()
		if !ok || user != "jane" || pass != "test-token" || req.URL.Query().Get("service") != "ghcr.example" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		scope := req.URL.Query().Get("scope")
		writeJSON(w, http.StatusOK, map[string]string{"token": base64.StdEncoding.EncodeToString([]byte(scope))})
		return
	}
	name, ref, ok := strings.Cut(strings.TrimPrefix(req.URL.Path, "/v2/"), "/manifests/")
	if !ok || name != "ravindu-rev/ruralzd" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	want := "repository:ravindu-rev/ruralzd:pull"
	if req.Method == http.MethodPut {
		want += ",push"
	}
	scope, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
	if !strings.HasPrefix(string(scope), want) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.srv.URL+`/token",service="ghcr.example",scope="`+want+`"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch req.Method {
	case http.MethodGet:
		d := ref
		if !strings.HasPrefix(ref, "sha256:") {
			d = r.tags[ref]
		}
		data, ok := r.manifests[d]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.corrupt {
			data = append(bytes.Clone(data), ' ')
		}
		if r.noType {
			w.Header()["Content-Type"] = nil // no type, and no sniffed one
		} else {
			w.Header().Set("Content-Type", fakeIndexType)
		}
		_, _ = w.Write(data)
	case http.MethodPut:
		data, _ := io.ReadAll(req.Body)
		if req.Header.Get("Content-Type") != fakeIndexType {
			r.t.Errorf("PUT %s with Content-Type %q", ref, req.Header.Get("Content-Type"))
		}
		d := r.add(data)
		r.tags[ref] = d
		r.puts = append(r.puts, ref)
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// TestRunImage: the publish job tags the verified index byte for byte, X.Y.Z
// and X.Y for a release, X.Y.Z-rc.N for a candidate, after checking that
// the immutable tag is absent or names the digest already; a tag naming
// another digest is never pushed again.
func TestRunImage(t *testing.T) {
	index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	other := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{}]}`)
	run := func(t *testing.T, r *fakeRegistry, tag, digest string, check bool) ([]string, error) {
		t.Helper()
		var out bytes.Buffer
		return runImage(t.Context(), imageOptions{
			tag: tag, digest: digest, check: check, repository: "ghcr.example/ravindu-rev/ruralzd",
			registry: r.srv.URL, user: "jane", token: "test-token",
		}, r.srv.Client(), &out)
	}

	r := newFakeRegistry(t)
	d := r.add(index)
	if findings, err := run(t, r, "v0.2.0", d, true); err != nil || len(findings) != 0 || len(r.puts) != 0 {
		t.Fatalf("-check: %q %v, puts %v", findings, err, r.puts)
	}
	if findings, err := run(t, r, "v0.2.0", d, false); err != nil || len(findings) != 0 {
		t.Fatalf("tag: %q %v", findings, err)
	}
	if r.tags["0.2.0"] != d || r.tags["0.2"] != d || len(r.tags) != 2 {
		t.Errorf("tags %v, want 0.2.0 and 0.2 at %s", r.tags, d)
	}
	// A re-run of the publish job: the tag already names the digest.
	if findings, err := run(t, r, "v0.2.0", d, false); err != nil || len(findings) != 0 {
		t.Errorf("re-run: %q %v", findings, err)
	}
	// A patch moves X.Y.
	d2 := r.add(other)
	if findings, err := run(t, r, "v0.2.1", d2, false); err != nil || len(findings) != 0 || r.tags["0.2"] != d2 || r.tags["0.2.0"] != d {
		t.Errorf("patch: %q %v, tags %v", findings, err, r.tags)
	}
	// Another digest under an existing version's tag is refused.
	if findings, err := run(t, r, "v0.2.0", d2, false); err != nil || !strings.Contains(strings.Join(findings, "\n"), "is never pushed again") {
		t.Errorf("re-push: %q %v", findings, err)
	}
	if findings, err := run(t, r, "v0.2.0", d2, true); err != nil || len(findings) != 1 {
		t.Errorf("-check of a taken tag: %q %v", findings, err)
	}
	// A candidate gets only its own tag.
	r.puts = nil
	if findings, err := run(t, r, "v0.3.0-rc.1", d2, false); err != nil || len(findings) != 0 || strings.Join(r.puts, " ") != "0.3.0-rc.1" {
		t.Errorf("candidate: %q %v, puts %v", findings, err, r.puts)
	}

	missing := "sha256:" + strings.Repeat("0", 64)
	if findings, err := run(t, r, "v0.4.0", missing, false); err != nil || !strings.Contains(strings.Join(findings, "\n"), "is not in the registry") {
		t.Errorf("unknown digest: %q %v", findings, err)
	}
	r.corrupt = true
	if findings, err := run(t, r, "v0.4.0", d, false); err != nil || !strings.Contains(strings.Join(findings, "\n"), "returned content of digest") {
		t.Errorf("corrupt registry: %q %v", findings, err)
	}
	r.corrupt, r.noType = false, true
	if findings, err := run(t, r, "v0.5.0", d, false); err != nil || len(findings) != 0 || r.tags["0.5.0"] != d {
		t.Errorf("manifest without a Content-Type: %q %v", findings, err)
	}

	for _, c := range []struct{ tag, digest, repository string }{
		{"0.2.0", d, "ghcr.example/ravindu-rev/ruralzd"},
		{"v0.2.0", "sha256:abc", "ghcr.example/ravindu-rev/ruralzd"},
		{"v0.2.0", d, "ruralzd"},
	} {
		_, err := runImage(t.Context(), imageOptions{tag: c.tag, digest: c.digest, repository: c.repository, registry: r.srv.URL, token: "t"}, r.srv.Client(), io.Discard)
		if err == nil {
			t.Errorf("runImage(%q, %q, %q) accepted bad input", c.tag, c.digest, c.repository)
		}
	}
	if _, err := run(t, r, "v0.6.0", d, false); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := runImage(t.Context(), imageOptions{
		tag: "v0.6.0", digest: d, repository: "ghcr.example/ravindu-rev/ruralzd",
		registry: r.srv.URL, user: "jane", token: "wrong",
	}, r.srv.Client(), &out); err == nil || !strings.Contains(err.Error(), "registry token: 401") {
		t.Errorf("wrong credentials: %v", err)
	}
}

// TestParseChallenge: the bearer realm, service and scope of a
// WWW-Authenticate header.
func TestParseChallenge(t *testing.T) {
	got, ok := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:a/b:pull"`)
	if !ok || got["realm"] != "https://ghcr.io/token" || got["service"] != "ghcr.io" || got["scope"] != "repository:a/b:pull" {
		t.Errorf("parseChallenge = %v, %t", got, ok)
	}
	if _, ok := parseChallenge(`Basic realm="x"`); ok {
		t.Error("a Basic challenge parsed as bearer")
	}
}
