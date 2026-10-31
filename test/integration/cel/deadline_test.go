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
)

// The Route deadline case (docs/architecture/02-configuration-model.md
// "Limits", targets).
const (
	// routeDeadline is the timeout of the deadline Routes: shorter than
	// the 50 ms comprehension stop, so the Route deadline stops their
	// evaluations first.
	routeDeadline = 30 * time.Millisecond
	// logEntries is the size of the accessLog.when list of the authz
	// case: past the first interrupt check, after 100 iterations, and
	// small enough to fit the 64 KiB header cap beside the rule's list of
	// listEntries.
	logEntries = 1000
)

// logHeaders returns the headers that make accessLog.when evaluate its
// comprehension over logEntries entries, which, run to its end, makes it
// false.
func logHeaders() http.Header {
	h := http.Header{}
	h.Set(logGate, "1")
	h.Set(logPrefix+"list", strings.TrimSuffix(strings.Repeat("a,", logEntries), ","))
	h.Set(logPrefix+"needle", "zz")
	h.Set(logPrefix+"a", "a")
	h.Set(logPrefix+"b", "zz")
	return h
}

// deadlineSend sends req on client and returns its answer or error. It
// fails on an outcome while the clock stands still for stillFor, since the
// evaluation runs on, then moves the clock by routeDeadline in one step
// and waits up to stopBound of real time for the outcome with the clock
// standing still again: only a stop at the Route deadline, never the
// 50 ms stop, ends the evaluation then.
func (s *suite) deadlineSend(t *testing.T, client *http.Client, req *http.Request) (answer, error) {
	t.Helper()
	type result struct {
		a   answer
		err error
	}
	done := make(chan result, 1)
	go func() {
		a, err := s.doOn(client, req)
		done <- result{a, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("%s %s ended with %v, %v with the clock standing still: the comprehension stop ran on real time, or the evaluation ended first",
			req.Method, req.URL.Path, r.a, r.err)
	case <-time.After(stillFor):
	}
	if err := s.top.Advance(t.Context(), routeDeadline); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		return r.a, r.err
	case <-time.After(stopBound):
		t.Fatalf("%s %s: no outcome within %v of real time once the clock passed the %v Route deadline; the evaluation waits for the 50 ms stop",
			req.Method, req.URL.Path, stopBound, routeDeadline)
	}
	return answer{}, nil
}

// TestStopAtRouteDeadline drives evaluations on Routes whose 30 ms
// deadline comes before the 50 ms comprehension stop, so the stop is the
// Route deadline on the controllable clock: once the clock has moved
// 30 ms, a candidate's match.when stops with 500 RZ-RT-006, and the
// authz.cel rule of a matched Route stops with 403 RZ-AUTH-015 and its
// signal; accessLog.when, which then starts at the Route deadline, stops
// at its first interrupt check, so the entry is written and counted as a
// runtime error although its expression, run to its end, is false. The
// matched Route's response reaches no client: the connection's write
// deadline, dated on the wall clock 30 ms after the request start, has
// passed in real time by then, so the case reads the access entry.
func TestStopAtRouteDeadline(t *testing.T) {
	requireClock(t)
	s := startSuite(t)
	// One connection per request: a response the write deadline cuts is
	// then never retried on another connection.
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	t.Run("Route.spec.match.when", func(t *testing.T) {
		p := &place{path: "/deadline/match"}
		a, err := s.deadlineSend(t, client, s.request(t, p, p.path, inStopTrue))
		if err != nil {
			t.Fatalf("GET %s: %v; want 500 RZ-RT-006 at the Route deadline", p.path, err)
		}
		is(http.StatusInternalServerError, "RZ-RT-006", nil, "")(t, a)
	})

	t.Run("authz.cel config.rule then accessLog.when", func(t *testing.T) {
		p := &place{path: "/deadline/authz", extra: logHeaders()}
		before := scrape(t, s.top, s.n)
		a, err := s.deadlineSend(t, client, s.request(t, p, p.path, inStopTrue))
		if err == nil && (a.status != http.StatusForbidden || a.code != "RZ-AUTH-015") {
			t.Errorf("answer %v, want none, as the write deadline passed, or 403 RZ-AUTH-015", a)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		re := regexp.MustCompile(`"msg":"access".*"path":"` + regexp.QuoteMeta(p.path) + `".*"status":(\d+)(?:,"code":"([^"]*)")?.*\n`)
		m, err := s.n.Process().WaitOutput(ctx, re)
		if err != nil {
			t.Fatalf("no access entry for %s: %v; accessLog.when did not stop at once at the Route deadline", p.path, err)
		}
		if m[1] != "403" || m[2] != "RZ-AUTH-015" {
			t.Errorf("access entry status %s code %q, want 403 RZ-AUTH-015: the rule did not stop at the Route deadline", m[1], m[2])
		}
		after := scrape(t, s.top, s.n)
		if d := failures("authz-deadline")(after) - failures("authz-deadline")(before); d != 1 {
			t.Errorf("the rule's stop added %v to %s, want 1", d, catalog.FilterFailuresTotal)
		}
		if d := after.Sum(catalog.TelemetryAccessLogCELErrorsTotal, nil) - before.Sum(catalog.TelemetryAccessLogCELErrorsTotal, nil); d != 1 {
			t.Errorf("accessLog.when's stop added %v to %s, want 1", d, catalog.TelemetryAccessLogCELErrorsTotal)
		}
		s.waitRecord(t, executorRecord, "authz-deadline", "deadline")
	})
}
