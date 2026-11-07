// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ociregistry

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Self-tests of the in-process registry: routing, uploads, the manifest
// digest check, authentication challenges and pagination.

func TestRoute(t *testing.T) {
	tests := []struct {
		in, name, kind, arg string
	}{
		{"acme/app/manifests/v1", "acme/app", "manifests", "v1"},
		{"acme/app/blobs/sha256:00", "acme/app", "blobs", "sha256:00"},
		{"acme/app/blobs/uploads/", "acme/app", "uploads", ""},
		{"acme/app/blobs/uploads/abc", "acme/app", "uploads", "abc"},
		{"acme/app/blobs/uploads", "acme/app", "uploads", ""},
		{"acme/app/tags/list", "acme/app", "tags", ""},
		{"acme/app/referrers/sha256:00", "acme/app", "referrers", "sha256:00"},
		{"a/manifests/blobs/sha256:00", "a/manifests", "blobs", "sha256:00"},
	}
	for _, tt := range tests {
		name, kind, arg, ok := route(tt.in)
		if !ok || name != tt.name || kind != tt.kind || arg != tt.arg {
			t.Errorf("route(%q) = %q %q %q %v, want %q %q %q", tt.in, name, kind, arg, ok, tt.name, tt.kind, tt.arg)
		}
	}
	for _, in := range []string{"acme", "tags/list", "/manifests/x"} {
		if _, _, _, ok := route(in); ok {
			t.Errorf("route(%q) matched", in)
		}
	}
}

// client returns an HTTP client trusting s.
func client(s *Server) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: s.RootCAs(), MinVersion: tls.VersionTLS12}}}
}

// answer is a response with its body read.
type answer struct {
	StatusCode int
	Header     http.Header
}

// do sends a request and returns the answer and its body.
func do(t *testing.T, c *http.Client, method, url string, body []byte, header map[string]string) (answer, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{StatusCode: resp.StatusCode, Header: resp.Header}, b
}

func TestUploadAndManifest(t *testing.T) {
	s := Start(t, Options{})
	c := client(s)
	base := s.URL() + "/v2/acme/app"
	resp, _ := do(t, c, http.MethodPost, base+"/blobs/uploads/", nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST upload = %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	resp, _ = do(t, c, http.MethodPatch, s.URL()+loc, []byte("hel"), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("PATCH = %d", resp.StatusCode)
	}
	resp, _ = do(t, c, http.MethodPut, s.URL()+loc+"?digest="+DigestOf([]byte("hello")), []byte("lo"), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	resp, body := do(t, c, http.MethodGet, base+"/blobs/"+DigestOf([]byte("hello")), nil, nil)
	if resp.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("GET blob = %d %q", resp.StatusCode, body)
	}
	// A wrong digest is refused.
	resp, _ = do(t, c, http.MethodPost, base+"/blobs/uploads/?digest="+DigestOf([]byte("x")), []byte("y"), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("monolithic upload with a wrong digest = %d", resp.StatusCode)
	}
	manifest := []byte(`{"schemaVersion":2,"mediaType":"` + MediaTypeImageManifest + `","subject":{"mediaType":"x","digest":"sha256:aa","size":1},"artifactType":"t"}`)
	resp, _ = do(t, c, http.MethodPut, base+"/manifests/"+DigestOf([]byte("other")), manifest, map[string]string{"Content-Type": MediaTypeImageManifest})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("manifest under another digest = %d", resp.StatusCode)
	}
	resp, _ = do(t, c, http.MethodPut, base+"/manifests/v1", manifest, map[string]string{"Content-Type": MediaTypeImageManifest})
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("OCI-Subject") != "sha256:aa" {
		t.Fatalf("PUT manifest = %d, OCI-Subject %q", resp.StatusCode, resp.Header.Get("OCI-Subject"))
	}
	if refs := s.Referrers("acme/app", "sha256:aa"); len(refs) != 1 || refs[0].ArtifactType != "t" {
		t.Fatalf("referrers = %+v", refs)
	}
	resp, body = do(t, c, http.MethodGet, base+"/manifests/v1", nil, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, manifest) || resp.Header.Get("Docker-Content-Digest") != DigestOf(manifest) {
		t.Fatalf("GET manifest = %d %q", resp.StatusCode, body)
	}
}

func TestChallenges(t *testing.T) {
	basic := Start(t, Options{Auth: AuthBasic, Users: map[string]string{"u": "p"}})
	resp, _ := do(t, client(basic), http.MethodGet, basic.URL()+"/v2/", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
		t.Fatalf("Basic challenge = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	resp, _ = do(t, client(basic), http.MethodGet, basic.URL()+"/v2/", nil, map[string]string{"Authorization": BasicAuth("u", "p")})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Basic credentials = %d", resp.StatusCode)
	}
	bearer := Start(t, Options{Auth: AuthBearer, Users: map[string]string{"u": "p"}})
	resp, _ = do(t, client(bearer), http.MethodPut, bearer.URL()+"/v2/acme/app/manifests/v1", []byte("{}"), nil)
	if ch := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(ch, `scope="repository:acme/app:pull,push"`) {
		t.Fatalf("Bearer challenge = %d %q", resp.StatusCode, ch)
	}
	resp, body := do(t, client(bearer), http.MethodGet, bearer.URL()+TokenPath, nil, map[string]string{"Authorization": BasicAuth("u", "p")})
	var tok struct {
		Token string `json:"token"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &tok) != nil || tok.Token == "" {
		t.Fatalf("token = %d %s", resp.StatusCode, body)
	}
	resp, _ = do(t, client(bearer), http.MethodGet, bearer.URL()+"/v2/", nil, map[string]string{"Authorization": "Bearer " + tok.Token})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer token = %d", resp.StatusCode)
	}
}

func TestPagination(t *testing.T) {
	s := Start(t, Options{TagPageSize: 2})
	if _, err := s.PutManifest("acme/app", "a", MediaTypeImageManifest, []byte(`{"schemaVersion":2}`)); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"b", "c"} {
		s.Tag("acme/app", tag, DigestOf([]byte(`{"schemaVersion":2}`)))
	}
	resp, body := do(t, client(s), http.MethodGet, s.URL()+"/v2/acme/app/tags/list", nil, nil)
	if !strings.Contains(string(body), `"a","b"`) || !strings.Contains(resp.Header.Get("Link"), "last=b") {
		t.Fatalf("first page = %s, Link %q", body, resp.Header.Get("Link"))
	}
	_, body = do(t, client(s), http.MethodGet, s.URL()+"/v2/acme/app/tags/list?n=2&last=b", nil, nil)
	if !strings.Contains(string(body), `["c"]`) {
		t.Fatalf("second page = %s", body)
	}
	s.Untag("acme/app", "c")
	if _, ok := s.Manifest("acme/app", DigestOf([]byte(`{"schemaVersion":2}`))); !ok {
		t.Fatal("the manifest went with its tag")
	}
}
