// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminauth"
	"github.com/ravindu-rev/ruralz/internal/gateway/tap"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// clientTrace is the trace ID of the valid traceparent the tests send.
const clientTrace = "0af7651916cd43dd8448eb211c80319c"

// TestRequestID checks the admin requestId: the trace ID of the request's
// valid traceparent, else a new trace ID the tracer draws from a decision
// without a traceparent at ratio 0, which never samples, so admin
// requests never take from the data plane's sampling caps.
func TestRequestID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string // traceparent field lines
		want   string
	}{
		{"sampled", []string{"00-" + clientTrace + "-b7ad6b7169203331-01"}, clientTrace},
		{"unsampled", []string{"00-" + clientTrace + "-b7ad6b7169203331-00"}, clientTrace},
		{"spaces around", []string{" 00-" + clientTrace + "-b7ad6b7169203331-01\t"}, clientTrace},
		{"none", nil, testRequestID},
		{"uppercase hex", []string{"00-0AF7651916CD43DD8448EB211C80319C-b7ad6b7169203331-01"}, testRequestID},
		{"zero trace ID", []string{"00-00000000000000000000000000000000-b7ad6b7169203331-01"}, testRequestID},
		{"version ff", []string{"ff-" + clientTrace + "-b7ad6b7169203331-01"}, testRequestID},
		{"too short", []string{"00-" + clientTrace + "-b7ad6b71692033-01"}, testRequestID},
		{"two field lines", []string{
			"00-" + clientTrace + "-b7ad6b7169203331-01", "00-" + clientTrace + "-b7ad6b7169203331-01",
		}, testRequestID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracer := &idTracer{id: testTraceID()}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/nope", nil)
			for _, v := range tc.values {
				r.Header.Add("Traceparent", v)
			}
			if got := RequestID(tracer)(r); got != tc.want {
				t.Fatalf("requestId = %q, want %q", got, tc.want)
			}
			headers, ratios := tracer.asked()
			if tc.want == clientTrace {
				if len(ratios) != 0 {
					t.Fatalf("a valid traceparent drew a new ID (%d decisions)", len(ratios))
				}
				return
			}
			if len(ratios) != 1 || ratios[0] != 0 || len(headers[0]) != 0 {
				t.Fatalf("decisions: headers %v ratios %v; want one at ratio 0 without a traceparent", headers, ratios)
			}
		})
	}
}

// TestAdminErrorsCarryRequestID checks that every admin problem document
// carries the decided requestId: the router's 404, adminauth's 401 and the
// tap Hub's 400, when the wiring gives them the one RequestID function.
func TestAdminErrorsCarryRequestID(t *testing.T) {
	rid := RequestID(&idTracer{id: testTraceID()})
	f := newFixture(t, func(c *Config) {
		c.RequestID = rid
		c.Auth = newAuthWith(t, rid)
		c.Tap = tap.New(tap.Config{RequestID: rid})
	})
	traceparent := "00-" + clientTrace + "-b7ad6b7169203331-01"
	for _, tc := range []struct {
		target, token, code string
		status              int
	}{
		{"/nope", operatorToken, CodeNotFound, http.StatusNotFound},
		{PathConfigDump, "", adminauth.CodeMissing, http.StatusUnauthorized},
		{PathTap + "?sample=2", operatorToken, tap.CodeInvalidSample, http.StatusBadRequest},
	} {
		for _, withParent := range []bool{true, false} {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.target, nil)
			req.RemoteAddr = loopbackPeer
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			want := testRequestID
			if withParent {
				req.Header.Set("Traceparent", traceparent)
				want = clientTrace
			}
			w := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(w, req)
			d, err := problem.Decode(w.Body.Bytes())
			if err != nil || w.Code != tc.status || d.Code != tc.code || d.RequestID != want {
				t.Errorf("GET %s (traceparent %v) = %d %+v, %v; want %d %s with requestId %s",
					tc.target, withParent, w.Code, d, err, tc.status, tc.code, want)
			}
		}
	}
}

// newAuthWith returns an operator Authenticator whose 401s carry rid.
func newAuthWith(t *testing.T, rid func(*http.Request) string) *adminauth.Authenticator {
	t.Helper()
	a, err := adminauth.New(operatorSettings(t), adminauth.Options{Logger: slog.New(slog.DiscardHandler), RequestID: rid})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
