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
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// cacheRecord is the debug record of a cache config.key that failed.
const cacheRecord = "cache config.key failed to evaluate; the request bypasses the cache"

// Circuit breaker bounds (docs/architecture/09-traffic-management-and-resilience.md
// "Circuit breaking", targets).
const (
	// breakerLegs is the most legs a breaker check sends before the
	// breaker must have opened: it opens once a rolling 10 s window holds
	// at least 20 legs, half of them failed.
	breakerLegs = 40
	// pastOpen is past the default openDuration of 30 s and its +20%
	// jitter: the breaker is half-open.
	pastOpen = 37 * time.Second
	// probes is how many probe successes close a half-open breaker.
	probes = 3
)

// cacheStatus checks a cache answer: 200, its Cache-Status naming a bypass
// or not.
func cacheStatus(bypass bool) func(t *testing.T, a answer) {
	return func(t *testing.T, a answer) {
		t.Helper()
		if a.status != http.StatusOK || strings.Contains(a.header.Get("Cache-Status"), "fwd=bypass") != bypass {
			t.Errorf("answer %v with Cache-Status %q, want 200 with bypass %v", a, a.header.Get("Cache-Status"), bypass)
		}
	}
}

// cacheBypass reads ruralz_cache_requests_total of the cache Route's
// bypasses.
func cacheBypass(set promtext.Set) float64 {
	return set.Sum(catalog.CacheRequestsTotal, promtext.L("route", "cache", "result", "bypass"))
}

// places returns every M1 CEL place with its variants.
func places() []place {
	h := func(pairs ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(pairs); i += 2 {
			m[pairs[i]] = pairs[i+1]
		}
		return m
	}
	ps := []place{
		{
			name: "Route.spec.match.when", path: "/match/guarded", contrast: true,
			normal: ok(h("x-route", "guarded"), ""),
			// 500 and no fallthrough to the fallback Route.
			failed: is(http.StatusInternalServerError, "RZ-RT-006", h("x-route", ""), ""),
		},
		{
			name: "Route.spec.composition.steps[].pathExpression", path: "/step-path", contrast: true,
			normal: ok(nil, `"path":"yes"`),
			failed: is(http.StatusBadGateway, "RZ-RT-015", nil, ""),
		},
		{
			name: "Route.spec.composition.steps[].pathExpression/optional", path: "/step-path-optional", contrast: true,
			normal: ok(h("ruralz-partial", ""), `"step":"last"`),
			// The step is left out.
			failed: ok(h("ruralz-partial", "true"), `"step":"last"`),
		},
		{
			name: "Route.spec.composition.steps[].when", path: "/step-when", contrast: true,
			normal: ok(nil, `"step":"second"`),
			failed: is(http.StatusBadGateway, "RZ-RT-015", nil, ""),
		},
		{
			name: "Route.spec.composition.steps[].when/optional", path: "/step-when-optional", contrast: true,
			normal: ok(nil, `"step":"second"`),
			// The erring when counts as false: the step is skipped.
			failed: ok(nil, `"step":"first"`),
		},
		{
			name: "authz.cel config.rule", path: "/authz", contrast: true,
			normal: ok(nil, ""),
			failed: is(http.StatusForbidden, "RZ-AUTH-015", nil, ""),
			signal: failures("authz-rule"), policy: "authz-rule", message: executorRecord,
		},
		{
			name: "cache config.key", path: "/cache", contrast: true,
			normal: cacheStatus(false), failed: cacheStatus(true),
			signal: cacheBypass, policy: "cache-key", message: cacheRecord,
		},
	}
	key := http.Header{"X-Api-Key": {apiKey}}
	for _, m := range modes() {
		closed := m == "closed"
		admission := ok(nil, "")
		requestSide := ok(nil, "")
		responseSide := func(header, body string) func(t *testing.T, a answer) {
			return ok(h(header, ""), body)
		}
		if closed {
			admission = is(http.StatusServiceUnavailable, "RZ-RL-005", nil, "")
			requestSide = is(http.StatusServiceUnavailable, "RZ-RT-011", nil, "")
			responseSide = func(string, string) func(t *testing.T, a answer) {
				return is(http.StatusBadGateway, "RZ-RT-012", nil, "")
			}
		}
		// Policy.spec.when: closed runs the Policy, open skips it, so the
		// control is the other outcome.
		when := place{
			name: "Policy.spec.when/" + m, path: "/policy-when/" + m, contrast: !closed,
			normal: ok(h("x-ran", "1"), ""), failed: ok(h("x-ran", ""), ""),
			signal: failures("when-" + m), policy: "when-" + m, message: executorRecord,
		}
		if closed {
			when.normal, when.failed = when.failed, when.normal
		}
		ps = append(ps, when)
		for _, f := range []struct{ place, policy, path string }{
			{"ratelimit config.key", "rl-key-", "/rl-key/"},
			{"ratelimit config.cost", "rl-cost-", "/rl-cost/"},
			{"ratelimit config.limits[].when", "rl-when-", "/rl-when/"},
			{"quota config.key", "quota-key-", "/quota-key/"},
			{"quota config.cost", "quota-cost-", "/quota-cost/"},
		} {
			p := place{
				name: f.place + "/" + m, path: f.path + m, contrast: true,
				normal: ok(nil, ""), failed: admission,
				signal: failures(f.policy + m), policy: f.policy + m, message: executorRecord,
			}
			if strings.HasPrefix(f.policy, "quota") {
				p.extra = key
			}
			ps = append(ps, p)
		}
		ps = append(ps,
			place{
				name: "headers config.request.set[].valueExpression/" + m, path: "/hdr-req/" + m, contrast: true,
				normal: ok(nil, ""), failed: requestSide,
				signal: failures("hdr-req-" + m), policy: "hdr-req-" + m, message: executorRecord,
			},
			place{
				name: "headers config.response.set[].valueExpression/" + m, path: "/hdr-resp/" + m, contrast: true,
				normal: ok(h("x-computed", "yes"), ""), failed: responseSide("x-computed", ""),
				signal: failures("hdr-resp-" + m), policy: "hdr-resp-" + m, message: executorRecord,
			},
			place{
				name: "transform.request config.body/" + m, path: "/trq-body/" + m, method: http.MethodPost, body: `{"k":1}`, contrast: true,
				normal: ok(nil, ""), failed: requestSide,
				signal: failures("trq-body-" + m), policy: "trq-body-" + m, message: executorRecord,
			},
			place{
				name: "transform.request config.set[].valueExpression/" + m, path: "/trq-set/" + m, method: http.MethodPost, body: `{"k":1}`, contrast: true,
				normal: ok(nil, ""), failed: requestSide,
				signal: failures("trq-set-" + m), policy: "trq-set-" + m, message: executorRecord,
			},
			place{
				name: "transform.response config.body/" + m, path: "/trs-body/" + m, contrast: true,
				normal: ok(nil, `"r":"yes"`), failed: responseSide("", `"ok":true`),
				signal: failures("trs-body-" + m), policy: "trs-body-" + m, message: executorRecord,
			},
			place{
				name: "transform.response config.set[].valueExpression/" + m, path: "/trs-set/" + m, contrast: true,
				normal: ok(h("x-t", "yes"), ""), failed: responseSide("x-t", ""),
				signal: failures("trs-set-" + m), policy: "trs-set-" + m, message: executorRecord,
			},
		)
	}
	return append(ps,
		place{
			// A runtime error picks a random Endpoint: the request goes on.
			name: "Upstream.spec.loadBalancing.hashKey", path: "/hash", contrast: true,
			normal: ok(nil, ""), failed: ok(nil, ""),
			signal: upstreamCEL("u-hash", "hashKey"),
		},
		retryOnPlace(),
		failureWhenPlace(),
		accessLogPlace(),
	)
}

// retryOnPlace is Upstream.spec.retries.retryOn: the retry mock answers
// 503 to every attempt, so a true retryOn retries once (two attempts) and
// a runtime error means no retry (one attempt). The retry delay is a Node
// timer, so a retrying request moves the clock.
func retryOnPlace() place {
	attempts := func(t *testing.T, s *suite, p *place, in input, mode sendMode) int64 {
		t.Helper()
		before := s.retry.Stats().Requests
		a := s.send(t, p, p.path, in, mode)
		if a.status != http.StatusServiceUnavailable || a.code != "" {
			t.Errorf("answer %v, want the Upstream's 503", a)
		}
		return s.retry.Stats().Requests - before
	}
	return place{
		name: "Upstream.spec.retries.retryOn", path: "/retry", contrast: true,
		control: func(t *testing.T, s *suite, p *place) {
			if n := attempts(t, s, p, inTrue, advancing); n != 2 {
				t.Errorf("a true retryOn made %d attempts, want 2", n)
			}
		},
		run: func(t *testing.T, s *suite, p *place, k kind) {
			signal := upstreamCEL("u-retry", "retryOn")
			before := scrape(t, s.top, s.n)
			if n := attempts(t, s, p, k.input(true), modeOf(k)); n != 1 {
				t.Errorf("a retryOn runtime error made %d attempts, want 1: no retry", n)
			}
			if d := signal(scrape(t, s.top, s.n)) - signal(before); d != 1 {
				t.Errorf("the runtime error added %v to its counter, want 1", d)
			}
		},
	}
}

// failureWhenPlace is Upstream.spec.circuitBreaker.failureWhen: a
// runtime error counts the leg as failed, so legs that err open the
// breaker, which then answers 503 RZ-UP-005, while legs whose failureWhen
// is false keep it closed. After each kind the clock passes openDuration
// and probe successes close the breaker again.
func failureWhenPlace() place {
	return place{
		name: "Upstream.spec.circuitBreaker.failureWhen", path: "/breaker", contrast: false,
		control: func(t *testing.T, s *suite, p *place) {
			for range breakerLegs {
				if a := s.send(t, p, p.path, inFalse, plain); a.status != http.StatusOK {
					t.Fatalf("answer %v to a leg whose failureWhen is false, want 200: the breaker opened", a)
				}
			}
			// Clear the rolling window.
			if err := s.top.Advance(t.Context(), pastOpen); err != nil {
				t.Fatal(err)
			}
		},
		run: func(t *testing.T, s *suite, p *place, k kind) {
			signal := upstreamCEL("u-breaker", "failureWhen")
			before := scrape(t, s.top, s.n)
			mode, counted, opened := modeOf(k), 0, false
			for range breakerLegs {
				a := s.send(t, p, p.path, k.input(false), mode)
				if mode == stopping {
					// The first leg proved the stop on the clock.
					mode = advancing
				}
				if a.status == http.StatusServiceUnavailable && a.code == "RZ-UP-005" {
					opened = true
					break
				}
				if a.status != http.StatusOK {
					t.Fatalf("answer %v, want 200 until the breaker opens", a)
				}
				counted++
			}
			if !opened {
				t.Fatalf("the breaker stayed closed through %d legs whose failureWhen erred: the errors did not count as failures", counted)
			}
			if d := signal(scrape(t, s.top, s.n)) - signal(before); d != float64(counted) {
				t.Errorf("%d legs erred, their counter grew by %v", counted, d)
			}
			if err := s.top.Advance(t.Context(), pastOpen); err != nil {
				t.Fatal(err)
			}
			for range probes {
				if a := s.send(t, p, p.path, inFalse, plain); a.status != http.StatusOK {
					t.Fatalf("probe answer %v, want 200", a)
				}
			}
		},
	}
}

// accessLogPlace is Gateway.spec.telemetry.accessLog.when, which reads
// the x-log-* headers behind the x-log gate: false writes no entry, a
// runtime error writes it and counts
// ruralz_telemetry_access_log_cel_errors_total. Each request has its own
// path, so its entry is found on stdout.
func accessLogPlace() place {
	entry := func(path string) *regexp.Regexp {
		return regexp.MustCompile(`"msg":"access".*"path":"` + regexp.QuoteMeta(path) + `"`)
	}
	waitEntry := func(t *testing.T, s *suite, path string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if _, err := s.n.Process().WaitOutput(ctx, entry(path)); err != nil {
			t.Fatalf("no access entry for %s: %v", path, err)
		}
	}
	return place{
		name: "Gateway.spec.telemetry.accessLog.when", path: "/log", prefix: logPrefix,
		extra: http.Header{"X-Log": {"1"}}, contrast: false,
		control: func(t *testing.T, s *suite, p *place) {
			if a := s.send(t, p, "/log/false", inFalse, plain); a.status != http.StatusOK {
				t.Fatalf("answer %v, want 200", a)
			}
			// Entries reach stdout in order: once a later one shows, the
			// false one would have.
			marker := &place{path: "/log/marker"}
			if a := s.send(t, marker, marker.path, inEval, plain); a.status != http.StatusOK {
				t.Fatalf("answer %v, want 200", a)
			}
			waitEntry(t, s, marker.path)
			if out, _ := s.n.Process().Output(); entry("/log/false").Match(out) {
				t.Error("accessLog.when false wrote an entry")
			}
		},
		run: func(t *testing.T, s *suite, p *place, k kind) {
			path := "/log/" + strings.ReplaceAll(k.name, " ", "-")
			before := scrape(t, s.top, s.n)
			if a := s.send(t, p, path, k.input(false), modeOf(k)); a.status != http.StatusOK {
				t.Fatalf("answer %v, want 200", a)
			}
			waitEntry(t, s, path)
			after := scrape(t, s.top, s.n)
			if d := after.Sum(catalog.TelemetryAccessLogCELErrorsTotal, nil) - before.Sum(catalog.TelemetryAccessLogCELErrorsTotal, nil); d != 1 {
				t.Errorf("the runtime error added %v to %s, want 1", d, catalog.TelemetryAccessLogCELErrorsTotal)
			}
		},
	}
}
