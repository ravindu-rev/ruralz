// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRepo is the repository the fake GitHub API serves.
const fakeRepo = "ravindu-rev/ruralz"

// Commits of the fake repository.
const (
	commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	commitC = "cccccccccccccccccccccccccccccccccccccccc"
)

// fakeIssue is an issue with its label events and comments.
type fakeIssue struct {
	issue
	events   []issueEvent
	comments []issueComment
}

// fakeGitHub is an in-memory GitHub REST API for one repository: tags,
// collaborators and their signing keys, releases, workflow runs and jobs,
// issues, labels and comments. Listings are split into pages of pageSize
// items with rel="next" links, as GitHub does.
type fakeGitHub struct {
	t        *testing.T
	mu       sync.Mutex
	srv      *httptest.Server
	pageSize int

	refs          map[string]gitObject // tag name -> ref object
	tagObjects    map[string]gitTag    // annotated tag SHA -> object
	collaborators []collaborator
	roles         map[string]string // login -> role_name
	sshKeys       map[string][]sshSigningKey
	gpgKeys       map[string][]gpgKey
	releases      []release
	runs          []workflowRun
	jobs          map[int64][]workflowJob
	issues        []*fakeIssue
	labels        []string
	// requests records "METHOD path" of every request.
	requests []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t: t, pageSize: 2,
		refs: map[string]gitObject{}, tagObjects: map[string]gitTag{}, roles: map[string]string{},
		sshKeys: map[string][]sshSigningKey{}, gpgKeys: map[string][]gpgKey{}, jobs: map[int64][]workflowJob{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// client returns a GitHub client of the fake.
func (f *fakeGitHub) client(t *testing.T) *github {
	t.Helper()
	g, err := newGitHub(f.srv.URL, fakeRepo, "test-token", f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// env is the run environment of the fake at now.
func (f *fakeGitHub) env(now time.Time, git gitRunner) environ {
	vars := map[string]string{
		"GITHUB_API_URL": f.srv.URL, "GITHUB_REPOSITORY": fakeRepo, "GH_TOKEN": "test-token",
		"GITHUB_SERVER_URL": "https://github.example",
	}
	return environ{getenv: func(k string) string { return vars[k] }, now: func() time.Time { return now }, client: f.srv.Client(), git: git}
}

// addMaintainer adds a collaborator with role.
func (f *fakeGitHub) addCollaborator(login, role string) {
	f.collaborators = append(f.collaborators, collaborator{Login: login, RoleName: role})
	f.roles[login] = role
}

// addTag adds an annotated tag on commit with a verification result.
func (f *fakeGitHub) addTag(name, commit, signature string, verified bool) {
	sha := fmt.Sprintf("%040x", len(f.tagObjects)+1)
	f.refs[name] = gitObject{Type: "tag", SHA: sha}
	t := gitTag{Tag: name, SHA: sha, Object: gitObject{Type: "commit", SHA: commit}}
	t.Verification.Verified = verified
	t.Verification.Reason = "valid"
	if !verified {
		t.Verification.Reason = "unknown_key"
	}
	t.Verification.Signature = signature
	f.tagObjects[sha] = t
}

// addRelease adds a published release (or pre-release) of tag.
func (f *fakeGitHub) addRelease(tag string, prerelease bool, published time.Time) {
	f.releases = append(f.releases, release{TagName: tag, Prerelease: prerelease, PublishedAt: published.UTC().Format(time.RFC3339)})
}

// issueByNumber returns the issue numbered n.
func (f *fakeGitHub) issueByNumber(n int) *fakeIssue {
	for _, i := range f.issues {
		if i.Number == n {
			return i
		}
	}
	return nil
}

// addIssue adds an issue and returns it.
func (f *fakeGitHub) addIssue(title, author, state, body string, labels ...string) *fakeIssue {
	i := &fakeIssue{}
	i.Number, i.Title, i.State, i.Body = len(f.issues)+1, title, state, body
	i.User.Login = author
	for _, l := range labels {
		i.Labels = append(i.Labels, ghLabel{Name: l})
	}
	f.issues = append(f.issues, i)
	return i
}

// label applies a label to an issue as actor.
func (i *fakeIssue) label(name, actor string) {
	i.Labels = append(i.Labels, ghLabel{Name: name})
	e := issueEvent{Event: "labeled", Label: &ghLabel{Name: name}}
	e.Actor.Login = actor
	i.events = append(i.events, e)
}

// comment adds a comment by login.
func (i *fakeIssue) comment(login, body string) {
	c := issueComment{Body: body}
	c.User.Login = login
	i.comments = append(i.comments, c)
}

// writeList answers one page of items.
func writeList[T any](f *fakeGitHub, w http.ResponseWriter, r *http.Request, items []T, wrap string) {
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	lo := min((page-1)*f.pageSize, len(items))
	hi := min(lo+f.pageSize, len(items))
	if hi < len(items) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		next := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path, RawQuery: q.Encode()}
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next", <%s>; rel="last"`, next.String(), next.String()))
	}
	pageItems := append([]T{}, items[lo:hi]...)
	if wrap != "" {
		writeJSON(w, http.StatusOK, map[string]any{wrap: pageItems, "total_count": len(items)})
		return
	}
	writeJSON(w, http.StatusOK, pageItems)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// serve routes one request of the fake API.
func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer test-token" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/repos/"+fakeRepo)
	m := func(pattern string) []string { return regexp.MustCompile("^" + pattern + "$").FindStringSubmatch(p) }
	q := r.URL.Query()
	notFound := func() { writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"}) }
	switch {
	case r.Method == http.MethodGet && m(`/git/ref/tags/(.+)`) != nil:
		name := m(`/git/ref/tags/(.+)`)[1]
		obj, ok := f.refs[name]
		if !ok {
			notFound()
			return
		}
		writeJSON(w, http.StatusOK, gitRef{Ref: "refs/tags/" + name, Object: obj})
	case r.Method == http.MethodGet && m(`/git/matching-refs/tags/(.+)`) != nil:
		prefix := m(`/git/matching-refs/tags/(.+)`)[1]
		var refs []gitRef
		var names []string
		for name := range f.refs {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if strings.HasPrefix(name, prefix) {
				refs = append(refs, gitRef{Ref: "refs/tags/" + name, Object: f.refs[name]})
			}
		}
		writeList(f, w, r, refs, "")
	case r.Method == http.MethodGet && m(`/git/tags/([0-9a-f]+)`) != nil:
		t, ok := f.tagObjects[m(`/git/tags/([0-9a-f]+)`)[1]]
		if !ok {
			notFound()
			return
		}
		writeJSON(w, http.StatusOK, t)
	case r.Method == http.MethodGet && p == "/collaborators":
		if q.Get("affiliation") != "all" {
			f.t.Errorf("collaborators listed with affiliation %q, want all", q.Get("affiliation"))
		}
		writeList(f, w, r, f.collaborators, "")
	case r.Method == http.MethodGet && m(`/collaborators/([^/]+)/permission`) != nil:
		role, ok := f.roles[m(`/collaborators/([^/]+)/permission`)[1]]
		if !ok {
			notFound()
			return
		}
		perm := role
		if role == "maintain" {
			perm = "write"
		}
		writeJSON(w, http.StatusOK, map[string]string{"permission": perm, "role_name": role})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/") && strings.HasSuffix(r.URL.Path, "/ssh_signing_keys"):
		writeList(f, w, r, f.sshKeys[strings.Split(r.URL.Path, "/")[2]], "")
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/") && strings.HasSuffix(r.URL.Path, "/gpg_keys"):
		writeList(f, w, r, f.gpgKeys[strings.Split(r.URL.Path, "/")[2]], "")
	case r.Method == http.MethodGet && p == "/releases":
		writeList(f, w, r, f.releases, "")
	case r.Method == http.MethodGet && m(`/releases/tags/(.+)`) != nil:
		tag := m(`/releases/tags/(.+)`)[1]
		for _, rel := range f.releases {
			if rel.TagName == tag {
				writeJSON(w, http.StatusOK, rel)
				return
			}
		}
		notFound()
	case r.Method == http.MethodGet && p == "/actions/workflows/release.yml/runs":
		var runs []workflowRun
		for _, run := range f.runs {
			if (q.Get("head_sha") == "" || run.HeadSHA == q.Get("head_sha")) && q.Get("event") == "push" {
				runs = append(runs, run)
			}
		}
		writeList(f, w, r, runs, "workflow_runs")
	case r.Method == http.MethodGet && m(`/actions/runs/([0-9]+)/jobs`) != nil:
		if q.Get("filter") != "latest" {
			f.t.Errorf("jobs listed with filter %q, want latest", q.Get("filter"))
		}
		id, _ := strconv.ParseInt(m(`/actions/runs/([0-9]+)/jobs`)[1], 10, 64)
		jobs, ok := f.jobs[id]
		if !ok {
			notFound()
			return
		}
		writeList(f, w, r, jobs, "jobs")
	case r.Method == http.MethodGet && p == "/issues":
		var out []issue
		for i := len(f.issues) - 1; i >= 0; i-- { // newest first
			is := f.issues[i]
			if st := q.Get("state"); st != "all" && is.State != st {
				continue
			}
			if l := q.Get("labels"); l != "" && !is.hasLabel(l) {
				continue
			}
			if c := q.Get("creator"); c != "" && is.User.Login != c {
				continue
			}
			out = append(out, is.issue)
		}
		writeList(f, w, r, out, "")
	case r.Method == http.MethodPost && p == "/issues":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		i := f.addIssue(body["title"], trackingAuthor, "open", body["body"])
		writeJSON(w, http.StatusCreated, i.issue)
	case r.Method == http.MethodPatch && m(`/issues/([0-9]+)`) != nil:
		n, _ := strconv.Atoi(m(`/issues/([0-9]+)`)[1])
		i := f.issueByNumber(n)
		if i == nil {
			notFound()
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		i.Body = body["body"]
		writeJSON(w, http.StatusOK, i.issue)
	case r.Method == http.MethodGet && m(`/issues/([0-9]+)/events`) != nil:
		n, _ := strconv.Atoi(m(`/issues/([0-9]+)/events`)[1])
		writeList(f, w, r, f.issueByNumber(n).events, "")
	case r.Method == http.MethodGet && m(`/issues/([0-9]+)/comments`) != nil:
		n, _ := strconv.Atoi(m(`/issues/([0-9]+)/comments`)[1])
		writeList(f, w, r, f.issueByNumber(n).comments, "")
	case r.Method == http.MethodPost && p == "/labels":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if slices.Contains(f.labels, body["name"]) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
			return
		}
		f.labels = append(f.labels, body["name"])
		writeJSON(w, http.StatusCreated, body)
	default:
		_, _ = io.Copy(io.Discard, r.Body)
		f.t.Errorf("fake GitHub: unexpected %s %s", r.Method, r.URL)
		notFound()
	}
}

// readBack reads a file the test wrote, through os.Open as releasekit's
// tests do, so gosec's taint rules see one reviewed read.
func readBack(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // A file the test itself wrote.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// sshWire encodes SSH wire-format strings.
func sshWire(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = binary.BigEndian.AppendUint32(out, uint32(len(p))) //nolint:gosec // Test data far below 4 GiB.
		out = append(out, p...)
	}
	return out
}

// sshKeyPair is an Ed25519 key's SSH wire blob and its authorized_keys
// line.
func sshKeyPair(seed byte) (blob []byte, line string) {
	// An Ed25519 private key is its seed followed by its public key.
	pub := ed25519.NewKeyFromSeed(make32(seed))[ed25519.SeedSize:]
	blob = sshWire([]byte("ssh-ed25519"), pub)
	return blob, "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " signing key"
}

func make32(b byte) []byte {
	s := make([]byte, 32)
	for i := range s {
		s[i] = b
	}
	return s
}

// sshSignature is an armored SSHSIG of the key blob in namespace.
func sshSignature(keyBlob []byte, namespace string) string {
	blob := append([]byte("SSHSIG"), 0, 0, 0, 1)
	blob = append(blob, sshWire(keyBlob, []byte(namespace), nil, []byte("sha512"), sshWire([]byte("ssh-ed25519"), make([]byte, 64)))...)
	return armor("SSH SIGNATURE", blob, "")
}

// armor wraps data in an ASCII-armored block of 64-column lines.
func armor(label string, data []byte, header string) string {
	enc := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	b.WriteString("-----BEGIN " + label + "-----\n")
	if header != "" {
		b.WriteString(header + "\n\n")
	}
	for len(enc) > 64 {
		b.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	b.WriteString(enc + "\n")
	if label == "PGP SIGNATURE" {
		b.WriteString("=AbCd\n")
	}
	b.WriteString("-----END " + label + "-----\n")
	return b.String()
}

// pgpSignature is an armored OpenPGP v4 signature naming the issuer
// fingerprint (20 bytes) in a hashed subpacket and its key ID in an
// unhashed one, in a new-format packet.
func pgpSignature(fingerprint []byte) string {
	hashed := append([]byte{22, 33, 4}, fingerprint...)
	unhashed := append([]byte{9, 16}, fingerprint[12:20]...)
	body := []byte{4, 0x00, 22, 10}
	body = binary.BigEndian.AppendUint16(body, uint16(len(hashed))) //nolint:gosec // A short test subpacket area.
	body = append(body, hashed...)
	body = binary.BigEndian.AppendUint16(body, uint16(len(unhashed))) //nolint:gosec // A short test subpacket area.
	body = append(body, unhashed...)
	body = append(body, 0xAB, 0xCD, 0, 8, 0xFF)
	packet := append([]byte{0xC2, byte(len(body))}, body...)
	return armor("PGP SIGNATURE", packet, "Comment: test")
}
