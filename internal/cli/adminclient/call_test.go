// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/buildinfo"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// Tests for each request's identity and credentials, the problem
// documents and other answers, and the body bounds of
// docs/reference/01-cli-and-api-surface.md "Admin API conventions".

// writeProblem answers with the Node's problem document for code, the
// trace ID of the request's traceparent as requestId, as an admin server
// does.
func writeProblem(w http.ResponseWriter, r *http.Request, code string) {
	id, _ := traceIDOf(r.Header.Get("Traceparent"))
	problem.Write(w, problem.New(code, id), nil)
}

// TestRequestIdentity checks User-Agent: ruralz/<version> and a fresh
// traceparent that is not sampled on every request.
func TestRequestIdentity(t *testing.T) {
	var rec recorder
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = io.WriteString(w, "{}")
	})
	c, _ := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
	for range 3 {
		if _, err := c.ConfigDump(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, r := range rec.all() {
		if ua := r.header.Get("User-Agent"); ua != "ruralz/"+testVersion {
			t.Errorf("User-Agent = %q", ua)
		}
		tp := r.header.Values("Traceparent")
		if len(tp) != 1 || !strings.HasSuffix(tp[0], "-00") {
			t.Fatalf("traceparent = %q, want one unsampled value", tp)
		}
		id, ok := traceIDOf(tp[0])
		if !ok || seen[id] {
			t.Errorf("traceparent %q is invalid or not fresh", tp[0])
		}
		seen[id] = true
		if r.header.Get("Authorization") != "" {
			t.Error("Authorization without a token file")
		}
	}
	// The first draw of countingRand: trace ID 01..10, parent ID 11..18.
	if got := rec.all()[0].header.Get("Traceparent"); got != "00-0102030405060708090a0b0c0d0e0f10-1112131415161718-00" {
		t.Errorf("first traceparent = %q", got)
	}
}

// TestDefaultVersion checks that User-Agent carries the build version
// without Options.Version.
func TestDefaultVersion(t *testing.T) {
	var rec recorder
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = io.WriteString(w, "{}")
	})
	c, err := New(Config{URL: srv.URL}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ConfigDump(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := rec.all()[0].header.Get("User-Agent"); got != "ruralz/"+buildinfo.Get().Version {
		t.Errorf("User-Agent = %q", got)
	}
	if _, ok := traceIDOf(rec.all()[0].header.Get("Traceparent")); !ok {
		t.Error("crypto/rand traceparent is invalid")
	}
}

// TestTraceparentFormat checks the format and the all-zero rule.
func TestTraceparentFormat(t *testing.T) {
	if got := formatTraceparent([24]byte{}); got != "00-00000000000000000000000000000001-0000000000000001-00" {
		t.Errorf("zero draw = %q", got)
	}
	if _, ok := traceIDOf(formatTraceparent([24]byte{})); !ok {
		t.Error("the zero draw gives an invalid traceparent")
	}
	draw, err := hex.DecodeString("a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7")
	if err != nil {
		t.Fatal(err)
	}
	if got := formatTraceparent([24]byte(draw)); got != "00-a0a1a2a3a4a5a6a7a8a9aaabacadaeaf-b0b1b2b3b4b5b6b7-00" {
		t.Errorf("draw = %q", got)
	}
	c, _ := newTestClient(t, Config{URL: DefaultURL})
	c.rand = bytes.NewReader(make([]byte, 10))
	if _, err := c.traceparent(); err == nil {
		t.Error("a short random source drew a traceparent")
	}
}

// TestTokenSentAndReadPerCall checks that the token goes as a bearer token
// and that the file is read again before each call, so a rotated token is
// sent and a removed file fails the call, exit 2, before any request.
func TestTokenSentAndReadPerCall(t *testing.T) {
	var rec recorder
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = io.WriteString(w, "{}")
	})
	path := tokenFile(t, testToken)
	c, _ := newTestClient(t, Config{URL: srv.URL, TokenFile: path, Timeout: DefaultTimeout})
	if _, err := c.ConfigDump(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(rotatedToken+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConfigDump(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := rec.all()
	if len(got) != 2 || got[0].header.Get("Authorization") != "Bearer "+testToken ||
		got[1].header.Get("Authorization") != "Bearer "+rotatedToken {
		t.Fatalf("requests = %+v", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err := c.ConfigDump(t.Context())
	if !errors.Is(err, os.ErrNotExist) || command.Code(t.Context(), err) != command.ExitNoResult ||
		!strings.HasPrefix(err.Error(), "--admin-token-file "+path+": ") {
		t.Errorf("ConfigDump with the file removed = %v", err)
	}
	if len(rec.all()) != 2 {
		t.Errorf("a request went out without a valid token file")
	}
}

// TestInvalidTokenFile checks the Node's token file rule at New: exit 2
// before any request, the token never quoted.
func TestInvalidTokenFile(t *testing.T) {
	var requests atomic.Int32
	srv := serve(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) })
	dir := t.TempDir()
	cases := map[string]string{
		"short":        writeFile(t, dir, "short", strings.Repeat("x", 21)),
		"not b64token": writeFile(t, dir, "space", "op canary 4fQ9x2LmZ7rT1vB8nK3sW6yH"),
		"two lines":    writeFile(t, dir, "lines", testToken+"\n"+testToken),
		"too long":     writeFile(t, dir, "long", strings.Repeat("y", 4097)),
		"empty":        writeFile(t, dir, "empty", " \n"),
		"directory":    dir,
		"missing":      filepath.Join(dir, "absent"),
	}
	for name, path := range cases {
		_, err := New(Config{URL: srv.URL, TokenFile: path, Names: Names{TokenFile: EnvTokenFile}}, Options{})
		if err == nil || command.Code(t.Context(), err) != command.ExitNoResult {
			t.Errorf("%s: New = %v, want exit 2", name, err)
			continue
		}
		if !strings.HasPrefix(err.Error(), EnvTokenFile+" "+path+": ") || strings.Contains(err.Error(), "4fQ9x2LmZ7rT1vB8nK3sW6yH") {
			t.Errorf("%s: New = %q", name, err)
		}
	}
	// 22 to 4,096 characters, trailing whitespace trimmed, are accepted.
	for _, tok := range []string{strings.Repeat("a", 22), strings.Repeat("b", 4096), testToken + " \t\r\n"} {
		if _, err := New(Config{URL: srv.URL, TokenFile: writeFile(t, t.TempDir(), "tok", tok)}, Options{}); err != nil {
			t.Errorf("New refused a valid token file: %v", err)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests went out", n)
	}
}

// TestProblemLine checks the one-line form of a problem document and that
// the document is kept exactly as received.
func TestProblemLine(t *testing.T) {
	cases := []struct {
		name, body, want string
		status           int
	}{
		{
			"Node problem", `{"title":"No active Revision","status":503,"code":"RZ-RT-024","requestId":"4bf92f3577b34da6a3ce929d0e0e4736"}`,
			"503 RZ-RT-024 No active Revision (requestId 4bf92f3577b34da6a3ce929d0e0e4736)", http.StatusServiceUnavailable,
		},
		{
			"detail and unknown members", "{\n  \"title\": \"Unauthorized\", \"status\": 401, \"code\": \"RZ-AUTH-001\",\n  \"detail\": \"token required\", \"requestId\": \"0123\", \"extra\": [1, 2]\n}\n",
			"401 RZ-AUTH-001 Unauthorized: token required (requestId 0123)", http.StatusUnauthorized,
		},
		{"no title, code or requestId", `{"status":404}`, "404 Not Found", http.StatusNotFound},
		{
			"control characters", "{\"title\":\"Tap\\u001b[31m limit\\nreached\",\"code\":\"RZ-RT-019\"}",
			"503 RZ-RT-019 Tap�[31m limit�reached", http.StatusServiceUnavailable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			})
			cl, _ := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
			_, err := cl.ConfigDump(t.Context())
			pe, ok := errors.AsType[*ProblemError](err)
			if !ok || command.Code(t.Context(), err) != command.ExitNoResult {
				t.Fatalf("ConfigDump = %v, want a ProblemError, exit 2", err)
			}
			if err.Error() != c.want || pe.Status != c.status {
				t.Errorf("ConfigDump = %q (status %d), want %q", err, pe.Status, c.want)
			}
			doc, ok := errors.AsType[command.ProblemDocument](err)
			if !ok || string(doc.ProblemJSON()) != c.body {
				t.Errorf("ProblemJSON = %q, want %q", doc.ProblemJSON(), c.body)
			}
		})
	}
}

// TestProblemRequestIDNamesTheRequest checks that a Node's problem
// document names the CLI's request: its requestId is the trace ID of the
// traceparent the client sent.
func TestProblemRequestIDNamesTheRequest(t *testing.T) {
	var rec recorder
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		writeProblem(w, r, "RZ-RT-024")
	})
	c, _ := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
	_, err := c.ConfigDump(t.Context())
	pe, ok := errors.AsType[*ProblemError](err)
	if !ok {
		t.Fatalf("ConfigDump = %v", err)
	}
	sent, _ := traceIDOf(rec.all()[0].header.Get("Traceparent"))
	if pe.Doc.RequestID == "" || pe.Doc.RequestID != sent || pe.Doc.Code != "RZ-RT-024" {
		t.Errorf("problem %+v, sent trace ID %s", pe.Doc, sent)
	}
	if want := "503 RZ-RT-024 No active Revision (requestId " + sent + ")"; err.Error() != want {
		t.Errorf("ConfigDump = %q, want %q", err, want)
	}
}

// TestAdminProblems checks the admin problem documents the conventions
// name, each a ProblemError with its status and code, exit 2: 404
// RZ-RT-020, 405 RZ-RT-021, 400 RZ-RT-022 and 503 RZ-RT-024.
func TestAdminProblems(t *testing.T) {
	codes := map[string]string{
		"/notfound": "RZ-RT-020", "/method": "RZ-RT-021", "/sample": "RZ-RT-022", "/inactive": "RZ-RT-024",
	}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		prefix, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		code := codes["/"+prefix]
		p := problem.New(code, "")
		switch code {
		case "RZ-RT-020":
			p.Status = http.StatusNotFound
		case "RZ-RT-021":
			p.Status = http.StatusMethodNotAllowed
		case "RZ-RT-022":
			p.Status = http.StatusBadRequest
		default:
			p.Status = http.StatusServiceUnavailable
		}
		problem.Write(w, p, nil)
	})
	for prefix, code := range codes {
		c, _ := newTestClient(t, Config{URL: srv.URL + prefix, Timeout: DefaultTimeout})
		_, err := c.ConfigDump(t.Context())
		pe, ok := errors.AsType[*ProblemError](err)
		if !ok || pe.Doc.Code != code || pe.Status != pe.Doc.Status || command.Code(t.Context(), err) != command.ExitNoResult {
			t.Errorf("%s: ConfigDump = %v, want a ProblemError with %s", prefix, err, code)
		}
	}
}

// TestOtherAnswers covers answers that are neither the endpoint's body nor
// a valid problem document: exit 2 naming the status and content type.
func TestOtherAnswers(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		want        string
	}{
		{"proxy error page", http.StatusBadGateway, "text/html", "<h1>bad gateway</h1>", "unexpected 502 Bad Gateway (text/html)"},
		{"no content", http.StatusNoContent, "", "", "unexpected 204 No Content"},
		{"unknown status", 599, "", "", "unexpected 599"},
		{"invalid problem JSON", http.StatusServiceUnavailable, "application/problem+json", `{"title":`, "unexpected 503 Service Unavailable (application/problem+json): problem document: "},
		{"problem that is not an object", http.StatusUnauthorized, "application/problem+json", `null`, "unexpected 401 Unauthorized (application/problem+json): problem document: not a JSON object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				if c.contentType != "" {
					w.Header().Set("Content-Type", c.contentType)
				}
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			})
			cl, _ := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
			_, err := cl.ConfigDump(t.Context())
			if _, ok := errors.AsType[*StatusError](err); !ok || command.Code(t.Context(), err) != command.ExitNoResult {
				t.Fatalf("ConfigDump = %v, want a StatusError, exit 2", err)
			}
			if _, ok := errors.AsType[command.ProblemDocument](err); ok {
				t.Error("a StatusError carries a problem document")
			}
			if want := "GET " + srv.URL + "/config/dump: " + c.want; !strings.HasPrefix(err.Error(), want) {
				t.Errorf("ConfigDump = %q, want prefix %q", err, want)
			}
		})
	}
}

// TestReadyz covers /readyz: 200 and 503 with its body are answers, a 503
// problem document an error; unknown members are ignored.
func TestReadyz(t *testing.T) {
	answers := map[string]func(w http.ResponseWriter, r *http.Request){
		"/ready/readyz": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ready","revision":"rev-162af81f5de4","later":true}`)
		},
		"/not-ready/readyz": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":"not_ready","reasons":[{"reason":"no_revision"},{"reason":"secrets_unresolved","code":"RZ-CFG-026","detail":"file:/run/secrets/tls.pem"}]}`)
		},
		"/proxy/readyz": func(w http.ResponseWriter, r *http.Request) { writeProblem(w, r, "RZ-RT-016") },
		"/garbled/readyz": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":`)
		},
	}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { answers[r.URL.Path](w, r) })

	c, _ := newTestClient(t, Config{URL: srv.URL + "/ready", Timeout: DefaultTimeout})
	r, err := c.Readyz(t.Context())
	if err != nil || !r.Ready || r.Status != "ready" || r.Revision != "rev-162af81f5de4" {
		t.Errorf("ready: %+v, %v", r, err)
	}

	c, _ = newTestClient(t, Config{URL: srv.URL + "/not-ready", Timeout: DefaultTimeout})
	r, err = c.Readyz(t.Context())
	want := []adminapi.ReadyReason{{Reason: adminapi.ReasonNoRevision}, {Reason: adminapi.ReasonSecretsUnresolved, Code: "RZ-CFG-026", Detail: "file:/run/secrets/tls.pem"}}
	if err != nil || r.Ready || r.Status != "not_ready" || len(r.Reasons) != 2 || r.Reasons[0] != want[0] || r.Reasons[1] != want[1] {
		t.Errorf("not ready: %+v, %v", r, err)
	}

	c, _ = newTestClient(t, Config{URL: srv.URL + "/proxy", Timeout: DefaultTimeout})
	if _, err := c.Readyz(t.Context()); !isProblemCode(err, "RZ-RT-016") {
		t.Errorf("problem: %v", err)
	}

	c, _ = newTestClient(t, Config{URL: srv.URL + "/garbled", Timeout: DefaultTimeout})
	if _, err := c.Readyz(t.Context()); err == nil || command.Code(t.Context(), err) != command.ExitNoResult ||
		!strings.Contains(err.Error(), "the /readyz body does not decode") {
		t.Errorf("garbled: %v", err)
	}
}

// isProblemCode reports whether err is a ProblemError with code.
func isProblemCode(err error, code string) bool {
	pe, ok := errors.AsType[*ProblemError](err)
	return ok && pe.Doc.Code == code
}

// TestBounds covers the body bounds: /config/dump refused by its
// Content-Length before anything is read, the bound itself accepted and
// one byte more refused, for /config/dump, /readyz and problem documents.
func TestBounds(t *testing.T) {
	t.Run("config dump Content-Length", func(t *testing.T) {
		srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(MaxConfigDumpBytes+1))
			w.WriteHeader(http.StatusOK)
		})
		c, _ := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
		_, err := c.ConfigDump(t.Context())
		if want := "GET " + srv.URL + "/config/dump: the /config/dump body exceeds 256 MiB"; err == nil || err.Error() != want ||
			command.Code(t.Context(), err) != command.ExitNoResult {
			t.Errorf("ConfigDump = %v, want %q", err, want)
		}
	})
	t.Run("config dump streamed", func(t *testing.T) {
		const limit = 4096
		for _, n := range []int{limit, limit + 1} {
			body := strings.Repeat("d", n)
			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body[:10])
				_ = http.NewResponseController(w).Flush() // chunked: no Content-Length
				_, _ = io.WriteString(w, body[10:])
			})
			c, _ := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
			c.limits.dump = limit
			got, err := c.ConfigDump(t.Context())
			switch {
			case n == limit && (err != nil || string(got) != body):
				t.Errorf("a dump of exactly the bound: %d bytes, %v", len(got), err)
			case n > limit && (err == nil || !strings.HasSuffix(err.Error(), "the /config/dump body exceeds 4 KiB")):
				t.Errorf("a dump over the bound: %v", err)
			}
		}
	})
	t.Run("readyz", func(t *testing.T) {
		for _, n := range []int{MaxReadyzBytes, MaxReadyzBytes + 1} {
			body := `{"status":"ready"}` + strings.Repeat(" ", n-len(`{"status":"ready"}`))
			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
			c, _ := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
			r, err := c.Readyz(t.Context())
			switch {
			case n == MaxReadyzBytes && (err != nil || !r.Ready):
				t.Errorf("a /readyz body of exactly 64 KiB: %v", err)
			case n > MaxReadyzBytes && (err == nil || !strings.HasSuffix(err.Error(), "the /readyz body exceeds 64 KiB")):
				t.Errorf("a /readyz body over 64 KiB: %v", err)
			}
		}
	})
	t.Run("problem document", func(t *testing.T) {
		for _, n := range []int{MaxProblemBytes, MaxProblemBytes + 1} {
			body := `{"title":"Unauthorized","code":"RZ-AUTH-002"}` + strings.Repeat(" ", n-len(`{"title":"Unauthorized","code":"RZ-AUTH-002"}`))
			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", problem.ContentType)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, body)
			})
			c, _ := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
			_, err := c.ConfigDump(t.Context())
			switch {
			case n == MaxProblemBytes && !isProblemCode(err, "RZ-AUTH-002"):
				t.Errorf("a problem document of exactly 64 KiB: %v", err)
			case n > MaxProblemBytes && (err == nil || !strings.HasSuffix(err.Error(), "(application/problem+json): the problem document exceeds 64 KiB")):
				t.Errorf("a problem document over 64 KiB: %v", err)
			}
		}
	})
}

// TestSizeText checks the bound formatting of messages.
func TestSizeText(t *testing.T) {
	for n, want := range map[int64]string{
		MaxConfigDumpBytes: "256 MiB", MaxProblemBytes: "64 KiB", MaxTapLineBytes: "1 MiB", 4096: "4 KiB", 100: "100 bytes", 1536: "1536 bytes",
	} {
		if got := size(n); got != want {
			t.Errorf("size(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestCanceledBeforeTheCall checks that a context already canceled sends
// nothing.
func TestCanceledBeforeTheCall(t *testing.T) {
	var requests atomic.Int32
	srv := serve(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) })
	c, _ := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
	ctx, interrupt := interruptible(t)
	interrupt()
	_, err := c.ConfigDump(ctx)
	if command.Code(ctx, err) != command.ExitInterrupted || requests.Load() != 0 {
		t.Errorf("ConfigDump = %v (exit %d), %d requests", err, command.Code(ctx, err), requests.Load())
	}
	if _, ok := errors.AsType[command.Interrupted](err); !ok {
		t.Errorf("ConfigDump = %v, want the interruption as its cause", err)
	}
}
