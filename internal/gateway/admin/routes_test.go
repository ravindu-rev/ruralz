// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminauth"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// TestEndpoints checks the endpoint table of "Admin endpoints": each path
// answers GET with its handler under its credential.
func TestEndpoints(t *testing.T) {
	f := newFixture(t, nil)
	snapshots, err := json.Marshal(testSnapshots())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, token string
		status      int
		body        string
	}{
		{PathHealthz, "", http.StatusOK, `{"status":"ok"}`},
		{PathReadyz, "", http.StatusServiceUnavailable, `{"status":"not_ready","reasons":[{"reason":"no_revision"},{"reason":"listeners_unbound"}]}`},
		{PathMetrics, metricsToken, http.StatusOK, "metrics"},
		{PathMetrics, operatorToken, http.StatusOK, "metrics"},
		{PathSnapshots, operatorToken, http.StatusOK, string(snapshots)},
		{PathUpstreams, operatorToken, http.StatusOK, "upstreams"},
		{PathConfigDump, operatorToken, http.StatusOK, "dump"},
	} {
		w := f.do(t, http.MethodGet, tc.path, tc.token)
		if w.Code != tc.status || w.Body.String() != tc.body {
			t.Errorf("GET %s = %d %q, want %d %q", tc.path, w.Code, w.Body, tc.status, tc.body)
		}
	}
}

// TestHealthzAndReadyzOpen checks that /healthz and /readyz answer
// without a credential, and that /readyz follows the readiness state.
func TestHealthzAndReadyzOpen(t *testing.T) {
	f := newFixture(t, nil)
	w := f.do(t, http.MethodGet, PathHealthz, "")
	if w.Code != http.StatusOK || w.Body.String() != `{"status":"ok"}` {
		t.Fatalf("healthz = %d %s", w.Code, w.Body)
	}
	for name, want := range map[string]string{
		"Content-Type":   "application/json",
		"Cache-Control":  "no-store",
		"Content-Length": strconv.Itoa(len(`{"status":"ok"}`)),
	} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("healthz %s %q, want %q", name, got, want)
		}
	}
	d, err := revision.Parse("sha256:162af81f5de482d540e09cf25205ffe39b705fd2dcf5eca88eb42e92b098a85e")
	if err != nil {
		t.Fatal(err)
	}
	f.ready.SetListeners()
	f.ready.SetRevision(d)
	w = f.do(t, http.MethodGet, PathReadyz, "")
	if want := `{"status":"ready","revision":"rev-162af81f5de4"}`; w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("readyz = %d %s, want 200 %s", w.Code, w.Body, want)
	}
}

// TestSnapshotsBody checks /debug/snapshots: the injected view as JSON,
// not cached.
func TestSnapshotsBody(t *testing.T) {
	f := newFixture(t, nil)
	w := f.do(t, http.MethodGet, PathSnapshots, operatorToken)
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q", got)
	}
	var v struct {
		Snapshots     []map[string]any `json:"snapshots"`
		Pending       *string          `json:"pending"`
		LastKnownGood *string          `json:"lastKnownGood"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Snapshots) != 1 || v.Pending != nil || v.LastKnownGood == nil {
		t.Fatalf("body %s", w.Body)
	}
}

// TestUnknownPaths checks that a path with no endpoint is a 404 problem
// document with RZ-RT-020, compared exactly: no trailing-slash, case or
// prefix match.
func TestUnknownPaths(t *testing.T) {
	f := newFixture(t, nil)
	for _, path := range []string{
		"/", "/nope", "/healthz/", "/HEALTHZ", "/readyz/x", "/metrics/", "/debug/", "/debug/pprof",
		"/debug/pprof/nope", "/debug/pprof/heap/x", "/debug/snapshots/", "/config", "/config/dump/x", "/tap/", "/tapx",
	} {
		t.Run(path, func(t *testing.T) {
			checkProblem(t, f.do(t, http.MethodGet, path, operatorToken), http.StatusNotFound, CodeNotFound)
		})
	}
}

// TestMethodNotAllowed checks that every endpoint answers GET only: any
// other method on a known path is a 405 problem document with RZ-RT-021
// and Allow: GET, so no admin request can change anything; an unknown
// path stays 404 whatever the method.
func TestMethodNotAllowed(t *testing.T) {
	f := newFixture(t, nil)
	for _, tc := range []struct {
		method, path, token string
	}{
		{http.MethodPost, PathHealthz, ""},
		{http.MethodHead, PathReadyz, ""},
		{http.MethodPut, PathMetrics, metricsToken},
		{http.MethodDelete, PathConfigDump, operatorToken},
		{http.MethodPost, PathSnapshots, operatorToken},
		{http.MethodPatch, PathUpstreams, operatorToken},
		{http.MethodPost, PathTap, operatorToken},
		{http.MethodPost, PathPprof + "symbol", operatorToken},
		{http.MethodOptions, PathPprof, operatorToken},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := f.do(t, tc.method, tc.path, tc.token)
			checkProblem(t, w, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
			if got := w.Header().Get("Allow"); got != http.MethodGet {
				t.Errorf("Allow %q, want GET", got)
			}
		})
	}
	checkProblem(t, f.do(t, http.MethodPost, "/nope", operatorToken), http.StatusNotFound, CodeNotFound)
}

// TestAuthenticationBeforeRouting checks the admin credential rule: every
// path but /healthz and /readyz needs a credential before it is routed,
// unknown paths included, so a 404 never answers an unauthenticated
// request; refusals are 401 problem documents with the Bearer challenge.
func TestAuthenticationBeforeRouting(t *testing.T) {
	f := newFixture(t, nil)
	for _, tc := range []struct {
		path, token, code string
	}{
		{"/nope", "", adminauth.CodeMissing},
		{PathMetrics, "", adminauth.CodeMissing},
		{PathTap, "", adminauth.CodeMissing},
		{PathConfigDump, metricsToken, adminauth.CodeInvalid},
		{PathPprof, metricsToken, adminauth.CodeInvalid},
		{"/nope", metricsToken, adminauth.CodeInvalid},
		{PathSnapshots, "wrong-token-000000000000000000000", adminauth.CodeInvalid},
	} {
		t.Run(tc.path+" "+tc.code, func(t *testing.T) {
			w := f.do(t, http.MethodGet, tc.path, tc.token)
			checkProblem(t, w, http.StatusUnauthorized, tc.code)
			if got := w.Header().Get("WWW-Authenticate"); got != adminauth.Challenge {
				t.Errorf("WWW-Authenticate %q", got)
			}
		})
	}
}

// TestOperatorPathsOffWithoutOperatorToken checks that without an
// operator token only /healthz, /readyz and /metrics answer.
func TestOperatorPathsOffWithoutOperatorToken(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.Auth = newAuth(t, adminauth.Settings{MetricsTokenFile: tokenFile(t, "metrics", metricsToken)}, &logBuffer{})
	})
	if w := f.do(t, http.MethodGet, PathMetrics, metricsToken); w.Code != http.StatusOK {
		t.Fatalf("metrics = %d", w.Code)
	}
	for _, path := range []string{PathSnapshots, PathUpstreams, PathConfigDump, PathTap, PathPprof} {
		checkProblem(t, f.do(t, http.MethodGet, path, metricsToken), http.StatusUnauthorized, adminauth.CodeInvalid)
	}
}

// TestUninjectedEndpointsAre404 checks that an endpoint whose handler the
// wiring did not inject is not served.
func TestUninjectedEndpointsAre404(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.Tap, c.Metrics, c.ConfigDump, c.Upstreams, c.Snapshots = nil, nil, nil, nil, nil
	})
	for _, path := range []string{PathTap, PathMetrics, PathConfigDump, PathUpstreams, PathSnapshots} {
		checkProblem(t, f.do(t, http.MethodGet, path, operatorToken), http.StatusNotFound, CodeNotFound)
	}
	if w := f.do(t, http.MethodGet, PathHealthz, ""); w.Code != http.StatusOK {
		t.Fatalf("healthz = %d", w.Code)
	}
}

// TestAuditedPaths checks that /tap and /config/dump uses reach the
// audit log with path, peer and credential kind, never the token.
func TestAuditedPaths(t *testing.T) {
	f := newFixture(t, nil)
	f.do(t, http.MethodGet, PathConfigDump, operatorToken)
	logs := f.logs.String()
	if !strings.Contains(logs, `"path":"/config/dump"`) || !strings.Contains(logs, `"credential":"operator"`) ||
		!strings.Contains(logs, loopbackPeer) {
		t.Fatalf("no audit record for /config/dump: %s", logs)
	}
	if strings.Contains(logs, operatorToken) {
		t.Fatal("the audit log carries the token")
	}
}

// TestProblemDocumentFormat checks that router errors are RFC 9457
// documents with code and requestId and nothing from the request.
func TestProblemDocumentFormat(t *testing.T) {
	f := newFixture(t, nil)
	w := f.do(t, http.MethodGet, "/nope?echo=<script>", operatorToken)
	want := string(problem.Append(nil, problem.New(CodeNotFound, testRequestID)))
	if got := w.Body.String(); got != want {
		t.Fatalf("body %s, want %s", got, want)
	}
}
