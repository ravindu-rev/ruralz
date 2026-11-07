// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/testkit/ociregistry"
)

// Tests for the registry client against the in-process registry: push,
// tag resolution, pulls by digest with their digest and size checks,
// referrers, tags, errors by kind, credentials sent only to their host
// and the TLS-only, bounded redirect rule.

// newRepo returns a Repository of acme/config on s with creds.
func newRepo(t *testing.T, s *ociregistry.Server, creds *Credentials) *Repository {
	t.Helper()
	r, err := NewRepository(Reference{Registry: s.Host(), Repository: "acme/config"}, RegistryOptions{Credentials: creds, RootCAs: s.RootCAs()})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// credsFor returns credentials holding user and pass for host.
func credsFor(t *testing.T, host string, c Credential) *Credentials {
	t.Helper()
	entry := map[string]string{}
	switch {
	case c.IdentityToken != "":
		entry["identitytoken"] = c.IdentityToken
	case c.RegistryToken != "":
		entry["registrytoken"] = c.RegistryToken
	default:
		entry["auth"] = basic(c.Username, c.Password)
	}
	data, err := json.Marshal(map[string]any{"auths": map[string]any{host: entry}})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := ReadCredentialFile(writeFile(t, "config.json", string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return creds
}

// artifact pushes a one-layer artifact manifest tagged tag and returns the
// manifest's and the layer's descriptors.
func artifact(ctx context.Context, t *testing.T, r *Repository, tag string, layer []byte) (Descriptor, Descriptor) {
	t.Helper()
	cfg, err := r.PushBlob(ctx, MediaTypeEmpty, []byte(emptyConfig))
	if err != nil {
		t.Fatal(err)
	}
	l, err := r.PushBlob(ctx, "application/vnd.ruralz.test.layer", layer)
	if err != nil {
		t.Fatal(err)
	}
	m := referrerManifest{
		SchemaVersion: 2, MediaType: MediaTypeImageManifest, ArtifactType: "application/vnd.ruralz.test",
		Config: jsonDescriptor(cfg), Layers: []ociDescriptor{jsonDescriptor(l)},
	}
	content, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	md, err := r.PushManifest(ctx, MediaTypeImageManifest, content, tag)
	if err != nil {
		t.Fatal(err)
	}
	return md, l
}

func TestPushResolveFetch(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{})
	r := newRepo(t, s, nil)
	ctx := t.Context()
	md, ld := artifact(ctx, t, r, "prod", []byte("canonical revision bytes"))

	got, err := r.Resolve(ctx, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != md.Digest || got.Size != md.Size || got.MediaType != MediaTypeImageManifest {
		t.Fatalf("Resolve = %+v, want %+v", got, md)
	}
	manifest, err := r.FetchManifest(ctx, got)
	if err != nil || revision.Sum(manifest) != md.Digest {
		t.Fatalf("FetchManifest = %d bytes, %v", len(manifest), err)
	}
	// A descriptor without a media type fetches with the broad Accept.
	if _, err := r.FetchManifest(ctx, Descriptor{Digest: md.Digest, Size: md.Size}); err != nil {
		t.Fatalf("FetchManifest without a media type: %v", err)
	}
	layer, err := r.FetchBlob(ctx, ld, MaxArtifactBytes)
	if err != nil || string(layer) != "canonical revision bytes" {
		t.Fatalf("FetchBlob = %q, %v", layer, err)
	}
	// A blob the registry holds is not uploaded again.
	before := countRequests(s, http.MethodPost, "/blobs/uploads")
	if _, err := r.PushBlob(ctx, "application/vnd.ruralz.test.layer", []byte("canonical revision bytes")); err != nil {
		t.Fatal(err)
	}
	if after := countRequests(s, http.MethodPost, "/blobs/uploads"); after != before {
		t.Fatalf("an existing blob was uploaded again: %d uploads, want %d", after, before)
	}
	if r.Reference() != (Reference{Registry: s.Host(), Repository: "acme/config"}) {
		t.Fatalf("Reference() = %+v", r.Reference())
	}
}

// countRequests counts requests of method whose path holds part.
func countRequests(s *ociregistry.Server, method, part string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Method == method && strings.Contains(r.Path, part) {
			n++
		}
	}
	return n
}

func TestFetchChecksDigestAndSize(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{})
	r := newRepo(t, s, nil)
	ctx := t.Context()
	md, ld := artifact(ctx, t, r, "prod", []byte("layer content"))

	s.Tamper(md.Digest.String(), []byte(`{"schemaVersion":2,"tampered":true}`))
	if _, err := r.FetchManifest(ctx, md); !errors.Is(err, ErrMismatch) {
		t.Fatalf("tampered manifest: %v, want ErrMismatch", err)
	}
	s.Tamper(md.Digest.String(), nil)
	s.Tamper(ld.Digest.String(), []byte("layer contenT"))
	if _, err := r.FetchBlob(ctx, ld, MaxArtifactBytes); !errors.Is(err, ErrMismatch) {
		t.Fatalf("tampered blob: %v, want ErrMismatch", err)
	}
	s.Tamper(ld.Digest.String(), nil)
	// A descriptor whose size disagrees with the content.
	wrong := ld
	wrong.Size = ld.Size + 1
	if _, err := r.FetchBlob(ctx, wrong, MaxArtifactBytes); !errors.Is(err, ErrMismatch) {
		t.Fatalf("size mismatch: %v, want ErrMismatch", err)
	}
	// A zero digest never verifies.
	if _, err := r.FetchBlob(ctx, Descriptor{}, MaxArtifactBytes); !errors.Is(err, ErrNoDigest) {
		t.Fatalf("zero digest: %v", err)
	}
	if _, err := r.FetchBlob(ctx, ld, 0); !errors.Is(err, ErrLimit) {
		t.Fatalf("zero limit: %v", err)
	}
}

func TestFetchLimits(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{})
	r := newRepo(t, s, nil)
	ctx := t.Context()
	content := []byte(strings.Repeat("r", 100))
	_, ld := artifact(ctx, t, r, "prod", content)

	gets := countRequests(s, http.MethodGet, "/blobs/")
	if _, err := r.FetchBlob(ctx, ld, 99); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("declared size over the limit: %v", err)
	}
	if n := countRequests(s, http.MethodGet, "/blobs/"); n != gets {
		t.Fatalf("a blob known to be too large was requested: %d GETs, want %d", n, gets)
	}
	// A descriptor that hides its size: the response's length refuses it.
	hidden := ld
	hidden.Size = 0
	if _, err := r.FetchBlob(ctx, hidden, 99); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Content-Length over the limit: %v", err)
	}
	// A response without a length: the stream stops past the limit.
	s.Intercept(func(w http.ResponseWriter, req *http.Request) bool {
		if req.Method != http.MethodGet || !strings.Contains(req.URL.Path, "/blobs/") {
			return false
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		for range 10 {
			_, _ = w.Write(content[:50])
			f.Flush()
		}
		return true
	})
	if _, err := r.FetchBlob(ctx, hidden, 99); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("streamed over the limit: %v", err)
	}
	s.Intercept(nil)
	if _, err := r.FetchManifest(ctx, Descriptor{Digest: ld.Digest, Size: MaxManifestBytes + 1}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("manifest over the limit: %v", err)
	}
	big := make([]byte, MaxManifestBytes+1)
	if _, err := r.PushManifest(ctx, MediaTypeImageManifest, big, ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("push of a manifest over the limit: %v", err)
	}
}

func TestNotFound(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{})
	r := newRepo(t, s, nil)
	ctx := t.Context()
	if _, err := r.Resolve(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing tag: %v", err)
	}
	d := Descriptor{MediaType: MediaTypeImageManifest, Digest: revision.Sum([]byte("nothing")), Size: 7}
	if _, err := r.FetchManifest(ctx, d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing manifest: %v", err)
	}
	if _, err := r.FetchBlob(ctx, d, 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing blob: %v", err)
	}
	if _, err := r.Resolve(ctx, "bad+tag"); !errors.Is(err, ErrReference) {
		t.Fatalf("invalid tag: %v", err)
	}
}

func TestBasicCredentials(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{Auth: ociregistry.AuthBasic, Users: map[string]string{"ci": "s3cret"}})
	ctx := t.Context()
	seed := newRepo(t, s, credsFor(t, s.Host(), Credential{Username: "ci", Password: "s3cret"}))
	md, _ := artifact(ctx, t, seed, "prod", []byte("x"))

	if _, err := newRepo(t, s, nil).Resolve(ctx, "prod"); !errors.Is(err, ErrDenied) {
		t.Fatalf("missing credentials: %v, want ErrDenied", err)
	}
	wrong := newRepo(t, s, credsFor(t, s.Host(), Credential{Username: "ci", Password: "wrong"}))
	if _, err := wrong.Resolve(ctx, "prod"); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong credentials: %v, want ErrDenied", err)
	}
	// Credentials of another host are not sent.
	other := newRepo(t, s, credsFor(t, "other.example.com", Credential{Username: "ci", Password: "other-pass"}))
	if _, err := other.Resolve(ctx, "prod"); !errors.Is(err, ErrDenied) {
		t.Fatalf("another host's credentials: %v, want ErrDenied", err)
	}
	got, err := seed.Resolve(ctx, "prod")
	if err != nil || got.Digest != md.Digest {
		t.Fatalf("Resolve with credentials = %+v, %v", got, err)
	}
	for _, req := range s.Requests() {
		if strings.Contains(req.Authorization, basic("ci", "other-pass")) {
			t.Fatalf("another host's credential reached the registry: %q", req.Authorization)
		}
	}
}

func TestBearerCredentials(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{
		Auth: ociregistry.AuthBearer, Users: map[string]string{"ci": "s3cret"}, IdentityTokens: map[string]string{"refresh-1": "ci"},
	})
	ctx := t.Context()
	seed := newRepo(t, s, credsFor(t, s.Host(), Credential{Username: "ci", Password: "s3cret"}))
	md, _ := artifact(ctx, t, seed, "prod", []byte("x"))

	if _, err := newRepo(t, s, nil).Resolve(ctx, "prod"); !errors.Is(err, ErrDenied) {
		t.Fatalf("anonymous against a bearer registry: %v, want ErrDenied", err)
	}
	wrong := newRepo(t, s, credsFor(t, s.Host(), Credential{Username: "ci", Password: "wrong"}))
	if _, err := wrong.Resolve(ctx, "prod"); err == nil {
		t.Fatal("wrong credentials resolved")
	}
	refresh := newRepo(t, s, credsFor(t, s.Host(), Credential{IdentityToken: "refresh-1"}))
	if got, err := refresh.Resolve(ctx, "prod"); err != nil || got.Digest != md.Digest {
		t.Fatalf("identity token: %+v, %v", got, err)
	}
	// A registry token is sent as the bearer token itself.
	token := issueToken(t, s, "ci", "s3cret")
	direct := newRepo(t, s, credsFor(t, s.Host(), Credential{RegistryToken: token}))
	if got, err := direct.Resolve(ctx, "prod"); err != nil || got.Digest != md.Digest {
		t.Fatalf("registry token: %+v, %v", got, err)
	}
	if countRequests(s, http.MethodPost, ociregistry.TokenPath) == 0 {
		t.Fatal("the identity token was never exchanged at the token endpoint")
	}
}

// issueToken gets a bearer token from s's token endpoint.
func issueToken(t *testing.T, s *ociregistry.Server, user, pass string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL()+ociregistry.TokenPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user, pass)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: s.RootCAs(), MinVersion: tls.VersionTLS12}}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		t.Fatalf("token: %v", err)
	}
	return out.Token
}

// recorder is a TLS server that records the Authorization header of every
// request and answers with body.
type recorder struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth []string
	body []byte
}

func startRecorder(t *testing.T, s *ociregistry.Server, body []byte) *recorder {
	t.Helper()
	rec := &recorder{body: body}
	leaf, err := s.CA().Server("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := leaf.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	rec.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.auth = append(rec.auth, r.Header.Get("Authorization"))
		rec.mu.Unlock()
		_, _ = w.Write(rec.body)
	}))
	rec.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	rec.srv.StartTLS()
	t.Cleanup(rec.srv.Close)
	return rec
}

func TestCredentialsStayOnTheirHost(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{Auth: ociregistry.AuthBasic, Users: map[string]string{"ci": "s3cret"}})
	ctx := t.Context()
	r := newRepo(t, s, credsFor(t, s.Host(), Credential{Username: "ci", Password: "s3cret"}))
	content := []byte("blob served from a blob store")
	_, ld := artifact(ctx, t, r, "prod", content)
	// The registry redirects the blob to a blob store on another origin,
	// as registries backed by object storage do.
	store := startRecorder(t, s, content)
	s.Intercept(func(w http.ResponseWriter, req *http.Request) bool {
		if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/blobs/") && req.Header.Get("Authorization") != "" {
			http.Redirect(w, req, store.srv.URL+"/object", http.StatusTemporaryRedirect)
			return true
		}
		return false
	})
	got, err := r.FetchBlob(ctx, ld, MaxArtifactBytes)
	if err != nil || string(got) != string(content) {
		t.Fatalf("FetchBlob through the redirect = %q, %v", got, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.auth) == 0 {
		t.Fatal("the blob store was never asked")
	}
	for _, a := range store.auth {
		if a != "" {
			t.Fatalf("the registry's credential reached the blob store: %q", a)
		}
	}
}

func TestRedirectRules(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{})
	ctx := t.Context()
	r := newRepo(t, s, nil)
	_, ld := artifact(ctx, t, r, "prod", []byte("x"))
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) }))
	t.Cleanup(plain.Close)
	s.Intercept(func(w http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/blobs/") {
			http.Redirect(w, req, plain.URL+"/object", http.StatusTemporaryRedirect)
			return true
		}
		return false
	})
	if _, err := r.FetchBlob(ctx, ld, MaxArtifactBytes); err == nil || !strings.Contains(err.Error(), "not https") {
		t.Fatalf("redirect to http: %v", err)
	}
	// A redirect loop stops after 10 hops.
	s.Intercept(func(w http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/blobs/") {
			http.Redirect(w, req, req.URL.Path+"x", http.StatusTemporaryRedirect) //nolint:gosec // G710: the test registry redirects to itself to make a loop.
			return true
		}
		return false
	})
	if _, err := r.FetchBlob(ctx, ld, MaxArtifactBytes); err == nil || !strings.Contains(err.Error(), "10 redirects") {
		t.Fatalf("redirect loop: %v", err)
	}
}

func TestUnavailable(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{})
	ctx := t.Context()
	r := newRepo(t, s, nil)
	md, _ := artifact(ctx, t, r, "prod", []byte("x"))
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		s.Intercept(func(w http.ResponseWriter, _ *http.Request) bool { w.WriteHeader(status); return true })
		if _, err := r.Resolve(ctx, "prod"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("Resolve answered %d: %v, want ErrUnavailable", status, err)
		}
		if _, err := r.FetchManifest(ctx, md); !errors.Is(err, ErrUnavailable) {
			t.Errorf("FetchManifest answered %d: %v, want ErrUnavailable", status, err)
		}
	}
	s.Intercept(func(w http.ResponseWriter, _ *http.Request) bool { w.WriteHeader(http.StatusTeapot); return true })
	if _, err := r.FetchManifest(ctx, md); err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotFound) {
		t.Errorf("unexpected status: %v", err)
	}
	s.Intercept(nil)
	// A registry whose certificate the client does not trust.
	untrusted, err := NewRepository(Reference{Registry: s.Host(), Repository: "acme/config"}, RegistryOptions{RootCAs: x509.NewCertPool()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Resolve(ctx, "prod"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("untrusted certificate: %v, want ErrUnavailable", err)
	}
	// A registry that is not listening.
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	gone, err := NewRepository(Reference{Registry: addr, Repository: "acme/config"}, RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gone.Resolve(ctx, "prod"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("connection refused: %v, want ErrUnavailable", err)
	}
	// A canceled context is not unavailability.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Resolve(cctx, "prod"); errors.Is(err, ErrUnavailable) || !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}

func TestNewRepositoryRefuses(t *testing.T) {
	for _, ref := range []Reference{{Registry: "", Repository: "x"}, {Registry: "ghcr.io", Repository: "UPPER"}, {Registry: "ghcr.io", Repository: ""}} {
		if _, err := NewRepository(ref, RegistryOptions{}); !errors.Is(err, ErrReference) {
			t.Errorf("NewRepository(%+v) = %v", ref, err)
		}
	}
}

// pushReferrer pushes a manifest of artifactType whose subject is subject.
func pushReferrer(ctx context.Context, t *testing.T, r *Repository, subject Descriptor, artifactType, note string) Descriptor {
	t.Helper()
	cfg, err := r.PushBlob(ctx, MediaTypeEmpty, []byte(emptyConfig))
	if err != nil {
		t.Fatal(err)
	}
	l, err := r.PushBlob(ctx, artifactType, []byte(note))
	if err != nil {
		t.Fatal(err)
	}
	sub := jsonDescriptor(Descriptor{MediaType: subject.MediaType, Digest: subject.Digest, Size: subject.Size})
	content, err := json.Marshal(referrerManifest{
		SchemaVersion: 2, MediaType: MediaTypeImageManifest, ArtifactType: artifactType,
		Config: jsonDescriptor(cfg), Layers: []ociDescriptor{jsonDescriptor(l)}, Subject: &sub,
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.PushManifest(ctx, MediaTypeImageManifest, content, "")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestReferrers(t *testing.T) {
	for _, noAPI := range []bool{false, true} {
		t.Run(fmt.Sprintf("noReferrersAPI=%v", noAPI), func(t *testing.T) {
			s := ociregistry.Start(t, ociregistry.Options{NoReferrersAPI: noAPI, ReferrersPageSize: 1})
			r := newRepo(t, s, nil)
			ctx := t.Context()
			md, _ := artifact(ctx, t, r, "prod", []byte("x"))
			var want []revision.Digest
			for i := range 3 {
				want = append(want, pushReferrer(ctx, t, r, md, MediaTypeSigstoreBundle, fmt.Sprint("sig", i)).Digest)
			}
			pushReferrer(ctx, t, r, md, "application/vnd.example.sbom", "sbom")

			got, err := r.Referrers(ctx, md, MediaTypeSigstoreBundle, 16)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 3 {
				t.Fatalf("Referrers = %d descriptors, want 3", len(got))
			}
			for i, d := range got {
				if d.ArtifactType != MediaTypeSigstoreBundle {
					t.Errorf("referrer %d has artifact type %q", i, d.ArtifactType)
				}
				if !noAPI && d.Digest != want[i] {
					t.Errorf("referrer %d = %s, want %s in registry order", i, d.Digest.Short(), want[i].Short())
				}
			}
			two, err := r.Referrers(ctx, md, MediaTypeSigstoreBundle, 2)
			if err != nil || len(two) != 2 {
				t.Fatalf("Referrers with limit 2 = %d, %v", len(two), err)
			}
			none, err := r.Referrers(ctx, Descriptor{MediaType: MediaTypeImageManifest, Digest: revision.Sum([]byte("other")), Size: 5}, MediaTypeSigstoreBundle, 16)
			if err != nil || len(none) != 0 {
				t.Fatalf("Referrers of a manifest without any = %v, %v", none, err)
			}
			if _, err := r.Referrers(ctx, md, MediaTypeSigstoreBundle, 0); !errors.Is(err, ErrLimit) {
				t.Fatalf("limit 0: %v", err)
			}
		})
	}
}

func TestReferrersStopListingAtTheLimit(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{ReferrersPageSize: 1})
	r := newRepo(t, s, nil)
	ctx := t.Context()
	md, _ := artifact(ctx, t, r, "prod", []byte("x"))
	for i := range 5 {
		pushReferrer(ctx, t, r, md, MediaTypeSigstoreBundle, fmt.Sprint("sig", i))
	}
	before := countRequests(s, http.MethodGet, "/referrers/")
	got, err := r.Referrers(ctx, md, MediaTypeSigstoreBundle, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("Referrers = %d, %v", len(got), err)
	}
	if pages := countRequests(s, http.MethodGet, "/referrers/") - before; pages != 2 {
		t.Fatalf("read %d pages for 2 referrers of one per page, want 2", pages)
	}
}

func TestTags(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{TagPageSize: 2})
	r := newRepo(t, s, nil)
	ctx := t.Context()
	md, _ := artifact(ctx, t, r, "a", []byte("x"))
	for _, tag := range []string{"b", "c", "d", "e"} {
		s.Tag("acme/config", tag, md.Digest.String())
	}
	got, err := r.Tags(ctx, 10)
	if err != nil || strings.Join(got, ",") != "a,b,c,d,e" {
		t.Fatalf("Tags = %v, %v", got, err)
	}
	if _, err := r.Tags(ctx, 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := r.Tags(ctx, 0); !errors.Is(err, ErrLimit) {
		t.Fatalf("limit 0: %v", err)
	}
}

func TestWithinTimesOut(t *testing.T) {
	s := ociregistry.Start(t, ociregistry.Options{})
	clk := clocktest.New(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	r, err := NewRepository(Reference{Registry: s.Host(), Repository: "acme/config"}, RegistryOptions{RootCAs: s.RootCAs(), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	s.Intercept(func(_ http.ResponseWriter, req *http.Request) bool {
		<-req.Context().Done()
		return true
	})
	done := make(chan error, 1)
	go func() {
		done <- r.within(t.Context(), 10*time.Second, func(ctx context.Context) error {
			_, err := r.Resolve(ctx, "prod")
			return err
		})
	}()
	if err := clk.BlockUntil(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(10 * time.Second)
	err = <-done
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("within = %v, want ErrTimeout", err)
	}
	// A call that ends in time keeps its own result.
	s.Intercept(nil)
	err = r.within(t.Context(), 10*time.Second, func(context.Context) error { return io.EOF })
	if !errors.Is(err, io.EOF) || errors.Is(err, ErrTimeout) {
		t.Fatalf("within = %v, want io.EOF", err)
	}
}
