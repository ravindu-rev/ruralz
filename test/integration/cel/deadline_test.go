// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package cel

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
)

// The Route deadline case (docs/architecture/02-configuration-model.md
// "Limits", targets).
const (
	// routeDeadline is the timeout of the deadline Route, whose Upstream
	// holds its answer for upstreamHold of real time.
	routeDeadline = 30 * time.Millisecond
	upstreamHold  = time.Minute
	// logEntries is the size of the accessLog.when list of the case within
	// the cost limit: a comprehension of 1,000 iterations, about 10,000
	// units.
	logEntries = 1000
)

// longLogHeaders returns the accessLog.when headers of the case within the
// cost limit: a list of logEntries entries, none starting with the needle,
// and an a that holds b, so the expression, run to its end, is true.
func longLogHeaders() http.Header {
	h := inTrue.headers(logPrefix)
	h.Set(logPrefix+"list", strings.TrimSuffix(strings.Repeat("a,", logEntries), ","))
	return h
}

// TestNoStopAtRouteDeadline checks that the cost limit alone bounds an
// evaluation, so a Route deadline that has passed stops none
// (docs/architecture/02-configuration-model.md "Limits"): a request on a
// Route with a 30 ms timeout, whose Upstream holds its answer, ends with
// 504 RZ-UP-003 once the clock has moved 30 ms, and accessLog.when, which
// then evaluates past the Route deadline, runs its comprehension of
// logEntries iterations to its end, true, so the entry is written without
// a runtime error; over the comprehension case's input it ends at the cost
// limit, so the entry is written and the error counted. The access entry
// shows each outcome after the Node has finished the request.
func TestNoStopAtRouteDeadline(t *testing.T) {
	requireClock(t)
	s := startSuite(t)
	// One connection per request: a request the Node ends is then never
	// retried on another connection.
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, tc := range []struct {
		name   string
		header http.Header
		// errors is what the evaluation adds to
		// ruralz_telemetry_access_log_cel_errors_total.
		errors float64
	}{
		{"within the cost limit", longLogHeaders(), 0},
		{"past the cost limit", inLimitFalse.headers(logPrefix), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/deadline/" + strings.ReplaceAll(tc.name, " ", "-")
			if err := s.main.SetRoute(path, mockup.Behavior{Delay: mockup.Fixed(upstreamHold)}); err != nil {
				t.Fatal(err)
			}
			before := scrape(t, s.top, s.n)
			arrived := uint64(max(s.main.Stats().Requests, 0)) + 1
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.n.URL("web")+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = tc.header.Clone()
			req.Header.Set(logGate, "1")
			go func() {
				if resp, err := client.Do(req); err == nil {
					_ = resp.Body.Close()
				}
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := s.main.WaitRequests(ctx, arrived); err != nil {
				t.Fatalf("the Upstream never received %s: %v", path, err)
			}
			if err := s.top.Advance(t.Context(), routeDeadline); err != nil {
				t.Fatal(err)
			}
			re := regexp.MustCompile(`"msg":"access".*"path":"` + regexp.QuoteMeta(path) + `".*"status":(\d+)(?:,"code":"([^"]*)")?.*\n`)
			m, err := s.n.Process().WaitOutput(ctx, re)
			if err != nil {
				t.Fatalf("no access entry for %s: %v", path, err)
			}
			if m[1] != "504" || m[2] != "RZ-UP-003" {
				t.Errorf("access entry status %s code %q, want 504 RZ-UP-003: the request did not run to its Route deadline", m[1], m[2])
			}
			after := scrape(t, s.top, s.n)
			if d := after.Sum(catalog.TelemetryAccessLogCELErrorsTotal, nil) - before.Sum(catalog.TelemetryAccessLogCELErrorsTotal, nil); d != tc.errors {
				t.Errorf("accessLog.when past the Route deadline added %v to %s, want %v", d, catalog.TelemetryAccessLogCELErrorsTotal, tc.errors)
			}
		})
	}
}
