// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package ociregistry is the in-process OCI registry of the test kit: an
// HTTPS server implementing the part of the OCI Distribution
// Specification v1.1 that internal/signing uses, for the unit tests that
// need no container (the integration tests run the registry image).
//
// It serves, under /v2/:
//
//   - manifests by tag or digest (GET, HEAD, PUT), recording each PUT
//     manifest's subject for the referrers API and answering with
//     OCI-Subject;
//   - blobs by digest (GET, HEAD) and uploads (POST, PATCH, PUT, a
//     monolithic POST with ?digest= included);
//   - the tags list and the referrers API, both paginated with Link
//     headers, the referrers list filtered by artifactType;
//   - authentication: none, Basic, or the distribution bearer token flow
//     with a token endpoint at TokenPath that takes Basic credentials
//     (GET) or an OAuth2 password or refresh token grant (POST).
//
// Tests seed content without HTTP (PutBlob, PutManifest), replace any
// answer (Intercept), serve other bytes for a digest (Tamper), and read
// every request with the Authorization header it carried (Requests), so a
// test can assert that a credential reached only its own host.
//
// Goroutines: the httptest.Server's serve loop and its per-connection
// goroutines, owned by the Server; the cleanup Start registers closes it
// and waits for every handler.
package ociregistry

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

// Media types the registry knows by name.
const (
	// MediaTypeImageManifest is the OCI image manifest.
	MediaTypeImageManifest = "application/vnd.oci.image.manifest.v1+json"
	// MediaTypeImageIndex is the OCI image index, the referrers API answer.
	MediaTypeImageIndex = "application/vnd.oci.image.index.v1+json"
	// MediaTypeEmpty is the OCI empty descriptor's media type.
	MediaTypeEmpty = "application/vnd.oci.empty.v1+json"
)

// TokenPath is the token endpoint of the bearer flow.
const TokenPath = "/token"

// maxBody bounds a manifest or blob upload, and the request log.
const (
	maxBody     = 64 << 20
	maxRequests = 4096
)

// Auth selects how the registry authenticates.
type Auth uint8

// Authentication modes.
const (
	// AuthNone serves every request.
	AuthNone Auth = iota
	// AuthBasic needs Basic credentials of Options.Users on every request.
	AuthBasic
	// AuthBearer needs a token from TokenPath on every request.
	AuthBearer
)

// Options configure a Server.
type Options struct {
	// Auth is the authentication mode.
	Auth Auth
	// Users maps user names to passwords, accepted by AuthBasic and by the
	// token endpoint of AuthBearer.
	Users map[string]string
	// IdentityTokens maps refresh tokens to user names, accepted by the
	// OAuth2 refresh token grant of AuthBearer.
	IdentityTokens map[string]string
	// NoReferrersAPI answers the referrers endpoint 404, so clients fall
	// back to the referrers tag schema.
	NoReferrersAPI bool
	// TagPageSize and ReferrersPageSize bound a page of the tags list and
	// of the referrers list; 0 means one page, unless the request's n is
	// smaller.
	TagPageSize, ReferrersPageSize int
}

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// Request is one request the registry received.
type Request struct {
	// Method and Path are the request line's; Query its raw query.
	Method, Path, Query string
	// Authorization is the header as received, empty without one.
	Authorization string
}

// Server is a running registry.
type Server struct {
	opts Options
	srv  *httptest.Server
	ca   *pki.CA

	mu        sync.Mutex
	repos     map[string]*repository
	tokens    map[string]string // bearer token -> user
	uploads   map[string]*bytes.Buffer
	tampered  map[string][]byte
	requests  []Request
	intercept func(w http.ResponseWriter, r *http.Request) bool
}

// repository is one repository's content.
type repository struct {
	manifests map[string]manifest // by digest
	tags      map[string]string   // tag -> digest
	blobs     map[string][]byte   // by digest
	referrers map[string][]Descriptor
}

// manifest is a stored manifest.
type manifest struct {
	mediaType string
	content   []byte
}

// Start starts a registry over TLS on 127.0.0.1 with a certificate from a
// new test CA, closed when tb ends.
func Start(tb testing.TB, opts Options) *Server {
	tb.Helper()
	ca, err := pki.NewCA(pki.CAOptions{CommonName: "ociregistry test CA"})
	if err != nil {
		tb.Fatal(err)
	}
	leaf, err := ca.Server("127.0.0.1", "localhost")
	if err != nil {
		tb.Fatal(err)
	}
	cert, err := leaf.TLSCertificate()
	if err != nil {
		tb.Fatal(err)
	}
	s := &Server{
		opts: opts, ca: ca,
		repos: map[string]*repository{}, tokens: map[string]string{},
		uploads: map[string]*bytes.Buffer{}, tampered: map[string][]byte{},
	}
	s.srv = httptest.NewUnstartedServer(s)
	// Handshakes that tests make fail on purpose are not log noise.
	s.srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	s.srv.StartTLS()
	tb.Cleanup(s.srv.Close)
	return s
}

// Host returns the registry's host and port, the registry part of a
// reference.
func (s *Server) Host() string {
	u, _ := url.Parse(s.srv.URL)
	return u.Host
}

// URL returns the registry's base URL, https://host:port.
func (s *Server) URL() string { return s.srv.URL }

// RootCAs returns a pool holding the CA that issued the registry's
// certificate.
func (s *Server) RootCAs() *x509.CertPool { return s.ca.Pool() }

// CA returns the test CA of the registry's certificate, which can issue
// certificates for other test servers the same pool trusts.
func (s *Server) CA() *pki.CA { return s.ca }

// Intercept installs fn, which runs before the registry handles each
// request and handles it instead when it returns true; nil removes it.
func (s *Server) Intercept(fn func(w http.ResponseWriter, r *http.Request) bool) {
	s.mu.Lock()
	s.intercept = fn
	s.mu.Unlock()
}

// Tamper serves content for every GET of the manifest or blob digest
// instead of the stored bytes, keeping the stored size and digest in the
// headers; nil content stops tampering.
func (s *Server) Tamper(digest string, content []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if content == nil {
		delete(s.tampered, digest)
		return
	}
	s.tampered[digest] = content
}

// Requests returns every request received so far, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// PutBlob stores content in repository repo and returns its descriptor
// with mediaType.
func (s *Server) PutBlob(repo, mediaType string, content []byte) Descriptor {
	d := DigestOf(content)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo(repo).blobs[d] = bytes.Clone(content)
	return Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(content))}
}

// PutManifest stores content as a manifest of mediaType in repo, tagged
// tag unless tag is empty, records its subject for the referrers API, and
// returns its descriptor.
func (s *Server) PutManifest(repo, tag, mediaType string, content []byte) (Descriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putManifest(s.repo(repo), tag, mediaType, content)
}

// Tag points tag of repo at the manifest digest, as a retag does.
func (s *Server) Tag(repo, tag, digest string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo(repo).tags[tag] = digest
}

// Untag removes tag from repo.
func (s *Server) Untag(repo, tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.repo(repo).tags, tag)
}

// Referrers returns the descriptors that name subject in repo.
func (s *Server) Referrers(repo, subject string) []Descriptor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.repo(repo).referrers[subject])
}

// Manifest returns the stored manifest digest of repo and whether it
// exists.
func (s *Server) Manifest(repo, digest string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.repo(repo).manifests[digest]
	return bytes.Clone(m.content), ok
}

// DigestOf returns "sha256:<64 hex>" of b.
func DigestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// repo returns the repository name, creating it; s.mu is held.
func (s *Server) repo(name string) *repository {
	r, ok := s.repos[name]
	if !ok {
		r = &repository{
			manifests: map[string]manifest{}, tags: map[string]string{},
			blobs: map[string][]byte{}, referrers: map[string][]Descriptor{},
		}
		s.repos[name] = r
	}
	return r
}

// manifestFields are the members of a manifest the registry reads.
type manifestFields struct {
	MediaType    string            `json:"mediaType"`
	ArtifactType string            `json:"artifactType"`
	Config       *Descriptor       `json:"config"`
	Subject      *Descriptor       `json:"subject"`
	Annotations  map[string]string `json:"annotations"`
}

// putManifest stores a manifest; s.mu is held.
func (s *Server) putManifest(r *repository, tag, mediaType string, content []byte) (Descriptor, error) {
	var f manifestFields
	if err := json.Unmarshal(content, &f); err != nil {
		return Descriptor{}, fmt.Errorf("ociregistry: manifest: %w", err)
	}
	d := Descriptor{MediaType: mediaType, Digest: DigestOf(content), Size: int64(len(content))}
	r.manifests[d.Digest] = manifest{mediaType: mediaType, content: bytes.Clone(content)}
	if tag != "" {
		r.tags[tag] = d.Digest
	}
	if f.Subject != nil && f.Subject.Digest != "" {
		ref := Descriptor{MediaType: mediaType, Digest: d.Digest, Size: d.Size, ArtifactType: f.ArtifactType, Annotations: maps.Clone(f.Annotations)}
		if ref.ArtifactType == "" && f.Config != nil {
			ref.ArtifactType = f.Config.MediaType
		}
		list := r.referrers[f.Subject.Digest]
		if !slices.ContainsFunc(list, func(x Descriptor) bool { return x.Digest == ref.Digest }) {
			r.referrers[f.Subject.Digest] = append(list, ref)
		}
	}
	return d, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if len(s.requests) < maxRequests {
		s.requests = append(s.requests, Request{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Authorization: r.Header.Get("Authorization"),
		})
	}
	fn := s.intercept
	s.mu.Unlock()
	if fn != nil && fn(w, r) {
		return
	}
	if r.URL.Path == TokenPath {
		s.serveToken(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v2/") {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "not a registry path")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	if rest == "" {
		if !s.authorized(w, r, "", "") {
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	name, kind, arg, ok := route(rest)
	if !ok {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown endpoint")
		return
	}
	action := "pull"
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		action = "push"
	}
	if !s.authorized(w, r, name, action) {
		return
	}
	switch kind {
	case "manifests":
		s.serveManifest(w, r, name, arg)
	case "blobs":
		s.serveBlob(w, r, name, arg)
	case "uploads":
		s.serveUpload(w, r, name, arg)
	case "tags":
		s.serveTags(w, r, name)
	case "referrers":
		s.serveReferrers(w, r, name, arg)
	default:
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown endpoint")
	}
}

// route splits the path after /v2/ into the repository name, the
// endpoint kind and its argument: the endpoint is the marker found last,
// so a repository name may hold any of the words.
func route(rest string) (name, kind, arg string, ok bool) {
	if n, found := strings.CutSuffix(rest, "/tags/list"); found && n != "" {
		return n, "tags", "", true
	}
	if n, found := strings.CutSuffix(rest, "/blobs/uploads"); found && n != "" {
		return n, "uploads", "", true
	}
	best := -1
	for _, sep := range []struct{ marker, kind string }{
		{"/manifests/", "manifests"},
		{"/blobs/", "blobs"},
		{"/referrers/", "referrers"},
		{"/blobs/uploads/", "uploads"},
	} {
		if i := strings.LastIndex(rest, sep.marker); i > 0 && i >= best {
			best, name, kind, arg = i, rest[:i], sep.kind, rest[i+len(sep.marker):]
		}
	}
	if best < 0 {
		return "", "", "", false
	}
	return name, kind, arg, true
}

// authorized checks the request's credentials and answers 401 with the
// challenge when they do not pass.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request, name, action string) bool {
	switch s.opts.Auth {
	case AuthBasic:
		user, pass, ok := r.BasicAuth()
		if ok && s.password(user) == pass && pass != "" {
			return true
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="ociregistry"`)
	case AuthBearer:
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		_, valid := s.tokens[token]
		s.mu.Unlock()
		if ok && valid {
			return true
		}
		challenge := fmt.Sprintf(`Bearer realm="%s%s",service="ociregistry"`, s.srv.URL, TokenPath)
		if name != "" {
			scope := "repository:" + name + ":pull"
			if action == "push" {
				scope += ",push"
			}
			challenge += `,scope="` + scope + `"`
		}
		w.Header().Set("WWW-Authenticate", challenge)
	default:
		return true
	}
	writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
	return false
}

// password returns the password of user, "" when unknown.
func (s *Server) password(user string) string { return s.opts.Users[user] }

// serveToken is the token endpoint: GET with Basic credentials (the
// distribution token flow) or POST with an OAuth2 password or refresh
// token grant.
func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) {
	user := ""
	switch r.Method {
	case http.MethodGet:
		u, p, ok := r.BasicAuth()
		if !ok || p == "" || s.password(u) != p {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "bad credentials")
			return
		}
		user = u
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			writeError(w, http.StatusBadRequest, "UNSUPPORTED", err.Error())
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "password":
			u, p := r.PostForm.Get("username"), r.PostForm.Get("password")
			if p == "" || s.password(u) != p {
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "bad credentials")
				return
			}
			user = u
		case "refresh_token":
			u, ok := s.opts.IdentityTokens[r.PostForm.Get("refresh_token")]
			if !ok {
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "bad refresh token")
				return
			}
			user = u
		default:
			writeError(w, http.StatusBadRequest, "UNSUPPORTED", "grant type")
			return
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method")
		return
	}
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	token := hex.EncodeToString(raw[:])
	s.mu.Lock()
	s.tokens[token] = user
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "access_token": token, "expires_in": 300})
}

// serveManifest serves GET, HEAD and PUT of a manifest by tag or digest.
func (s *Server) serveManifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.mu.Lock()
		rp := s.repo(name)
		digest := ref
		if !strings.HasPrefix(ref, "sha256:") {
			digest = rp.tags[ref]
		}
		m, ok := rp.manifests[digest]
		body := m.content
		if t, tampered := s.tampered[digest]; tampered && ok {
			body = t
		}
		s.mu.Unlock()
		if !ok {
			writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		w.Header().Set("Content-Type", m.mediaType)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		tag := ref
		if strings.HasPrefix(ref, "sha256:") {
			if DigestOf(body) != ref {
				writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest mismatch")
				return
			}
			tag = ""
		}
		s.mu.Lock()
		d, err := s.putManifest(s.repo(name), tag, r.Header.Get("Content-Type"), body)
		subject := ""
		var f manifestFields
		if json.Unmarshal(body, &f) == nil && f.Subject != nil {
			subject = f.Subject.Digest
		}
		s.mu.Unlock()
		if err != nil {
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		if subject != "" && !s.opts.NoReferrersAPI {
			w.Header().Set("OCI-Subject", subject)
		}
		w.Header().Set("Docker-Content-Digest", d.Digest)
		w.Header().Set("Location", "/v2/"+name+"/manifests/"+d.Digest)
		w.WriteHeader(http.StatusCreated)
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method")
	}
}

// serveBlob serves GET and HEAD of a blob by digest.
func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request, name, digest string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method")
		return
	}
	s.mu.Lock()
	b, ok := s.repo(name).blobs[digest]
	size := len(b)
	if t, tampered := s.tampered[digest]; tampered && ok {
		b = t
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", digest)
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// serveUpload serves blob uploads: POST starts one (or, with ?digest=,
// uploads monolithically), PATCH appends and PUT with ?digest= completes.
func (s *Server) serveUpload(w http.ResponseWriter, r *http.Request, name, id string) {
	switch {
	case r.Method == http.MethodPost && id == "":
		if d := r.URL.Query().Get("digest"); d != "" {
			s.completeUpload(w, r, name, d, nil)
			return
		}
		var raw [12]byte
		_, _ = rand.Read(raw[:])
		uid := hex.EncodeToString(raw[:])
		s.mu.Lock()
		s.uploads[uid] = &bytes.Buffer{}
		s.mu.Unlock()
		w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+uid)
		w.Header().Set("Docker-Upload-UUID", uid)
		w.Header().Set("Range", "0-0")
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPatch && id != "":
		s.mu.Lock()
		buf, ok := s.uploads[id]
		s.mu.Unlock()
		if !ok {
			writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload unknown")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			writeError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", err.Error())
			return
		}
		s.mu.Lock()
		buf.Write(body)
		n := buf.Len()
		s.mu.Unlock()
		w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+id)
		w.Header().Set("Range", fmt.Sprintf("0-%d", max(n-1, 0)))
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut && id != "":
		s.mu.Lock()
		buf, ok := s.uploads[id]
		delete(s.uploads, id)
		s.mu.Unlock()
		if !ok {
			writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload unknown")
			return
		}
		s.completeUpload(w, r, name, r.URL.Query().Get("digest"), buf.Bytes())
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method")
	}
}

// completeUpload stores prefix plus the request body as the blob digest.
func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, name, digest string, prefix []byte) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", err.Error())
		return
	}
	content := append(bytes.Clone(prefix), body...)
	if DigestOf(content) != digest {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest mismatch")
		return
	}
	s.mu.Lock()
	s.repo(name).blobs[digest] = content
	s.mu.Unlock()
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Location", "/v2/"+name+"/blobs/"+digest)
	w.WriteHeader(http.StatusCreated)
}

// serveTags serves the tags list in lexical order, paginated.
func (s *Server) serveTags(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.Lock()
	tags := slices.Sorted(maps.Keys(s.repo(name).tags))
	s.mu.Unlock()
	q := r.URL.Query()
	if last := q.Get("last"); last != "" {
		i, _ := slices.BinarySearch(tags, last)
		for i < len(tags) && tags[i] <= last {
			i++
		}
		tags = tags[i:]
	}
	n := pageSize(q.Get("n"), s.opts.TagPageSize)
	if n > 0 && len(tags) > n {
		tags = tags[:n]
		next := url.Values{"n": {strconv.Itoa(n)}, "last": {tags[len(tags)-1]}}
		w.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?%s>; rel="next"`, name, next.Encode()))
	}
	if tags == nil {
		tags = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "tags": tags})
}

// serveReferrers serves the referrers API: an image index of the
// manifests whose subject is digest, filtered by artifactType, paginated.
func (s *Server) serveReferrers(w http.ResponseWriter, r *http.Request, name, digest string) {
	if s.opts.NoReferrersAPI {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "no referrers API")
		return
	}
	s.mu.Lock()
	list := slices.Clone(s.repo(name).referrers[digest])
	s.mu.Unlock()
	q := r.URL.Query()
	if at := q.Get("artifactType"); at != "" {
		list = slices.DeleteFunc(list, func(d Descriptor) bool { return d.ArtifactType != at })
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	start := 0
	if last := q.Get("last"); last != "" {
		if i := slices.IndexFunc(list, func(d Descriptor) bool { return d.Digest == last }); i >= 0 {
			start = i + 1
		}
	}
	list = list[start:]
	n := pageSize(q.Get("n"), s.opts.ReferrersPageSize)
	if n > 0 && len(list) > n {
		list = list[:n]
		next := url.Values{"n": {strconv.Itoa(n)}, "last": {list[len(list)-1].Digest}}
		if at := q.Get("artifactType"); at != "" {
			next.Set("artifactType", at)
		}
		w.Header().Set("Link", fmt.Sprintf(`</v2/%s/referrers/%s?%s>; rel="next"`, name, digest, next.Encode()))
	}
	if list == nil {
		list = []Descriptor{}
	}
	w.Header().Set("Content-Type", MediaTypeImageIndex)
	_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 2, "mediaType": MediaTypeImageIndex, "manifests": list})
}

// pageSize returns the smaller positive of the request's n and the
// configured page size, 0 for no bound.
func pageSize(n string, configured int) int {
	v, err := strconv.Atoi(n)
	if err != nil || v <= 0 {
		return configured
	}
	if configured > 0 && configured < v {
		return configured
	}
	return v
}

// writeError writes an OCI error document.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

// BasicAuth returns the Basic Authorization header value of user and
// pass.
func BasicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}
