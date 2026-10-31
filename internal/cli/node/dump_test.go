// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// Tests of ruralz node dump against an admin endpoint on loopback
// (docs/reference/01-cli-and-api-surface.md, "Rollout, promotion and Node
// verbs" and "Admin API conventions").

// gatewayContent is ruralz.canonical.v1 content of one Gateway.
const gatewayContent = `{"format":"ruralz.canonical.v1","resources":[{"kind":"Gateway","metadata":{"name":"edge"},` +
	`"spec":{"listeners":[{"name":"http","port":8080,"protocol":"http"}]}}]}`

// testToken is an operator token canary of RFC 6750 b64token characters.
//
//nolint:gosec // G101: a test canary, not a credential.
const testToken = "op-canary-node-7Hq2Lx9Zp4Wm1Rt8"

// lkgDigest is the Last-Known-Good digest of the test dumps.
func lkgDigest() revision.Digest { return revision.Sum([]byte("previous Revision")) }

// dumpOf returns the /config/dump document the Node serves for content.
func dumpOf(t *testing.T, content string, lkg revision.Digest) string {
	t.Helper()
	b, ok := canonical.AppendDump(nil, revision.Revision{Content: []byte(content), Digest: revision.Sum([]byte(content))}, lkg)
	if !ok {
		t.Fatalf("AppendDump refused %s", content)
	}
	return string(b)
}

// rawDump is a dump document of content with the given digest and
// revision, as a Node that is broken or newer might send it.
func rawDump(content string, digest revision.Digest, rev string) string {
	return `{"content":` + content + `,"digest":"` + digest.String() + `","revision":"` + rev + `"}`
}

// adminServer is an admin endpoint that answers /config/dump with body,
// or with a problem document of code when code is set.
type adminServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recorded
}

// recorded is one request an adminServer saw.
type recorded struct {
	path   string
	header http.Header
}

func newAdminServer(t *testing.T, body, code string) *adminServer {
	t.Helper()
	a := &adminServer{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.reqs = append(a.reqs, recorded{path: r.URL.Path, header: r.Header.Clone()})
		a.mu.Unlock()
		if code != "" {
			id, _ := traceID(r.Header.Get("Traceparent"))
			problem.Write(w, problem.New(code, id), nil)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(a.Close)
	return a
}

// last returns the last request the server saw.
func (a *adminServer) last(t *testing.T) recorded {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.reqs) == 0 {
		t.Fatal("the server saw no request")
	}
	return a.reqs[len(a.reqs)-1]
}

// traceID returns the trace ID of a version 00 traceparent, the requestId
// of the Node's problem documents.
func traceID(tp string) (string, bool) {
	m := regexp.MustCompile(`^00-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$`).FindStringSubmatch(tp)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// dumpOptions are the node commands' options on a fake clock.
func dumpOptions() Options {
	return Options{Clock: clocktest.New(epoch()), Version: testVersion}
}

// TestDumpWritesDocumentAsReceived: a dump that passes its check reaches
// standard output byte for byte, and nothing else is printed.
func TestDumpWritesDocumentAsReceived(t *testing.T) {
	for _, tc := range []struct {
		name string
		lkg  revision.Digest
	}{{"with Last-Known-Good", lkgDigest()}, {"without Last-Known-Good", revision.Digest{}}} {
		t.Run(tc.name, func(t *testing.T) {
			body := dumpOf(t, gatewayContent, tc.lkg)
			srv := newAdminServer(t, body, "")
			r := execute(t.Context(), dumpOptions(), "linux", nil, "node", "dump", "--admin", srv.URL)
			if r.code != command.ExitOK || r.stdout != body || r.stderr != "" {
				t.Fatalf("dump = exit %d, stdout %q, stderr %q; want 0, the document, nothing", r.code, r.stdout, r.stderr)
			}
			if got := srv.last(t).path; got != "/config/dump" {
				t.Errorf("request path = %q, want /config/dump", got)
			}
		})
	}
}

// TestDumpOutputFile: with --output-file the document goes to the file
// exactly as received, standard output stays empty, and one line on
// standard error names the Revision, the full digest and the
// Last-Known-Good digest.
func TestDumpOutputFile(t *testing.T) {
	digest := revision.Sum([]byte(gatewayContent))
	for _, tc := range []struct {
		name string
		lkg  revision.Digest
		line string
	}{
		{"with Last-Known-Good", lkgDigest(), "saved " + digest.Short() + " " + digest.String() + ", Last-Known-Good " + lkgDigest().String() + "\n"},
		{"without Last-Known-Good", revision.Digest{}, "saved " + digest.Short() + " " + digest.String() + ", no Last-Known-Good\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := dumpOf(t, gatewayContent, tc.lkg)
			srv := newAdminServer(t, body, "")
			path := filepath.Join(t.TempDir(), "dump.json")
			writeFile(t, path, "an older dump")
			r := execute(t.Context(), dumpOptions(), "linux", nil, "node", "dump", "--admin", srv.URL, "--output-file", path)
			if r.code != command.ExitOK || r.stdout != "" || r.stderr != tc.line {
				t.Fatalf("dump = exit %d, stdout %q, stderr %q; want 0, nothing, %q", r.code, r.stdout, r.stderr, tc.line)
			}
			got, err := os.ReadFile(path) //nolint:gosec // G304: a file under t.TempDir.
			if err != nil || string(got) != body {
				t.Fatalf("file = %q, %v; want the document", got, err)
			}
		})
	}
}

// TestDumpFailedCheck: a document whose content does not hash to its
// digest (RZ-CFG-027), does not decode at the served schema levels
// (RZ-CFG-024), is not in canonical form (RZ-CFG-027), names another
// Revision, or is no dump document exits 2 and writes nothing, to
// standard output or to --output-file.
func TestDumpFailedCheck(t *testing.T) {
	gw := revision.Sum([]byte(gatewayContent))
	newer := strings.Replace(gatewayContent, `"spec":{`, `"spec":{"futureField":1,`, 1)
	misordered := `{"format":"ruralz.canonical.v1","resources":[{"kind":"Gateway",` +
		`"spec":{"listeners":[{"name":"http","port":8080,"protocol":"http"}]},"metadata":{"name":"edge"}}]}`
	cases := []struct {
		name, body string
		// want is the message after "ruralz node dump: GET <url>: ".
		want string
	}{
		{
			name: "digest mismatch",
			body: rawDump(gatewayContent, revision.Sum([]byte("other")), revision.Sum([]byte("other")).Short()),
			want: "the document fails its check: RZ-CFG-027: content hashes to " + gw.String() + ", want " + revision.Sum([]byte("other")).String(),
		},
		{
			name: "newer field",
			body: dumpOf(t, newer, lkgDigest()),
			want: "the document fails its check: RZ-CFG-024: Gateway/edge spec.futureField: ",
		},
		{
			name: "not canonical",
			body: rawDump(misordered, revision.Sum([]byte(misordered)), revision.Sum([]byte(misordered)).Short()),
			want: "the document fails its check: RZ-CFG-027: Gateway/edge ",
		},
		{
			name: "revision names another digest",
			body: rawDump(gatewayContent, gw, "rev-000000000000"),
			want: `the document fails its check: RZ-CFG-027: revision: revision "rev-000000000000" does not name digest ` + gw.String(),
		},
		{
			name: "no content",
			body: `{"digest":"` + gw.String() + `","revision":"` + gw.Short() + `"}`,
			want: "not a /config/dump document: no content member",
		},
		{
			name: "not an object",
			body: `[]`,
			want: "not a /config/dump document: not a JSON object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAdminServer(t, tc.body, "")
			base := srv.URL + "/node-a"
			path := filepath.Join(t.TempDir(), "dump.json")
			for _, args := range [][]string{nil, {"--output-file", path}} {
				r := execute(t.Context(), dumpOptions(), "linux", nil, append([]string{"node", "dump", "--admin", base}, args...)...)
				want := "ruralz node dump: GET " + base + "/config/dump: " + tc.want
				if r.code != command.ExitNoResult || r.stdout != "" || !strings.HasPrefix(r.stderr, want) || strings.Count(r.stderr, "\n") != 1 {
					t.Fatalf("dump %q = exit %d, stdout %q, stderr %q; want 2, nothing, %q...", args, r.code, r.stdout, r.stderr, want)
				}
			}
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("--output-file written after a failed check: %v", err)
			}
		})
	}
}

// TestDumpNoActiveRevision: 503 RZ-RT-024 from a Node with no active
// Revision exits 2 with the problem's one line and writes nothing.
func TestDumpNoActiveRevision(t *testing.T) {
	srv := newAdminServer(t, "", "RZ-RT-024")
	path := filepath.Join(t.TempDir(), "dump.json")
	r := execute(t.Context(), dumpOptions(), "linux", nil, "node", "dump", "--admin", srv.URL, "--output-file", path)
	id, ok := traceID(srv.last(t).header.Get("Traceparent"))
	if !ok {
		t.Fatalf("traceparent %q", srv.last(t).header.Get("Traceparent"))
	}
	want := "ruralz node dump: 503 RZ-RT-024 No active Revision (requestId " + id + ")\n"
	if r.code != command.ExitNoResult || r.stdout != "" || r.stderr != want {
		t.Fatalf("dump = exit %d, stdout %q, stderr %q; want 2, nothing, %q", r.code, r.stdout, r.stderr, want)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("--output-file written after 503: %v", err)
	}
}

// TestDumpTakesNoOutputFlag: --output is not a dump flag, so a problem
// document never reaches standard output.
func TestDumpTakesNoOutputFlag(t *testing.T) {
	r := execute(t.Context(), dumpOptions(), "linux", nil, "node", "dump", "--output", "json")
	want := "ruralz node dump: unknown flag --output\nRun 'ruralz node dump --help' for usage.\n"
	if r.code != command.ExitNoResult || r.stdout != "" || r.stderr != want {
		t.Fatalf("dump --output json = exit %d, stdout %q, stderr %q; want 2, %q", r.code, r.stdout, r.stderr, want)
	}
}

// TestDumpTimeout: --timeout replaces the call deadline on the injected
// clock, and an expired deadline exits 2 naming it.
func TestDumpTimeout(t *testing.T) {
	arrived := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	clk := clocktest.New(epoch())
	run := start(t.Context(), Options{Clock: clk, Version: testVersion}, "node", "dump", "--admin", srv.URL, "--timeout", "1s")
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the request did not arrive")
	}
	clk.Advance(time.Second)
	r := run.wait(t)
	want := "ruralz node dump: GET " + srv.URL + "/config/dump: not complete within 1s (--timeout)\n"
	if r.code != command.ExitNoResult || r.stdout != "" || r.stderr != want {
		t.Fatalf("dump = exit %d, stdout %q, stderr %q; want 2, %q", r.code, r.stdout, r.stderr, want)
	}
}

// TestDumpEnvironment: RURALZ_ADMIN_URL and RURALZ_ADMIN_TOKEN_FILE stand
// in for --admin and --admin-token-file, the token going as a bearer token.
func TestDumpEnvironment(t *testing.T) {
	body := dumpOf(t, gatewayContent, lkgDigest())
	srv := newAdminServer(t, body, "")
	tok := filepath.Join(t.TempDir(), "token")
	writeFile(t, tok, testToken+"\n")
	environ := []string{"RURALZ_ADMIN_URL=" + srv.URL, "RURALZ_ADMIN_TOKEN_FILE=" + tok}
	r := execute(t.Context(), dumpOptions(), "linux", environ, "node", "dump")
	if r.code != command.ExitOK || r.stdout != body {
		t.Fatalf("dump = exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	req := srv.last(t)
	if got := req.header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want the bearer token", got)
	}
	if got := req.header.Get("User-Agent"); got != "ruralz/"+testVersion {
		t.Errorf("User-Agent = %q, want ruralz/%s", got, testVersion)
	}
}

// TestDumpOutputFileParentMissing: an --output-file whose directory does
// not exist exits 2 and prints nothing on standard output.
func TestDumpOutputFileParentMissing(t *testing.T) {
	srv := newAdminServer(t, dumpOf(t, gatewayContent, lkgDigest()), "")
	path := filepath.Join(t.TempDir(), "missing", "dump.json")
	r := execute(t.Context(), dumpOptions(), "linux", nil, "node", "dump", "--admin", srv.URL, "--output-file", path)
	want := "ruralz node dump: --output-file " + path + ": parent directory does not exist\n"
	if r.code != command.ExitNoResult || r.stdout != "" || r.stderr != want {
		t.Fatalf("dump = exit %d, stdout %q, stderr %q; want 2, %q", r.code, r.stdout, r.stderr, want)
	}
}

// TestDumpOnEveryPlatform: ruralz node dump has no platform gate.
func TestDumpOnEveryPlatform(t *testing.T) {
	body := dumpOf(t, gatewayContent, lkgDigest())
	srv := newAdminServer(t, body, "")
	for _, goos := range []string{"linux", "darwin", "windows"} {
		r := execute(t.Context(), dumpOptions(), goos, nil, "node", "dump", "--admin", srv.URL)
		if r.code != command.ExitOK || r.stdout != body {
			t.Errorf("%s: dump = exit %d, stderr %q", goos, r.code, r.stderr)
		}
	}
}
