// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package statestore

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/respproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
)

// replyHold is how long a counting proxy holds the replies a phase holds:
// long enough that a response waiting for a held reply would show it, and
// far below the State client's 10 s keepalive, so no connection is lost.
const replyHold = time.Second

// cacheable is the mock Upstream's default answer on the round-trip
// Routes: a small response a shared cache stores for 10 minutes.
func cacheable() mockup.Behavior {
	return mockup.Behavior{Body: []byte("cached body\n"), ContentType: "text/plain", Header: http.Header{"Cache-Control": {"max-age=600"}}}
}

// roundTripBundle is the round trip's Bundle: a Route per case on the
// State Store of kind k, with the Response Cache on the main connection.
func roundTripBundle(k redisserver.Kind, upstream string) []byte {
	b := bundle{
		stateStore: redisStore(topologyName(k), urlRef),
		upstream:   upstream,
		docs: []string{
			consumer(2, "1h"),
			apiKeyPolicy(),
			// rl-local's bucket holds its 2 tokens per minute: a third
			// request is a local denial.
			rateLimit("rl-local", 2, "1m", "", ""),
			// rl-gcra's GCRA admits 1 and then one per 30 s (burst 0)
			// while its bucket holds 2: the second request is a GCRA deny.
			rateLimit("rl-gcra", 2, "1m", "0", ""),
			// rl-s1 and rl-s2 key the same value, so their GCRA keys share
			// a hash slot.
			rateLimit("rl-s1", 100, "1m", "", ""),
			rateLimit("rl-s2", 50, "1m", "", ""),
			// rl-k2 keys x-other, another slot than rl-gcra's x-client.
			policy("rl-k2", "ratelimit", "config:", `  key: 'request.headers["x-other"]'`, "  limits: [{requests: 100, window: 1m}]"),
			policy("quota-q", "quota", "config: {consumerQuota: q}"),
			// rl-consumer keys consumer.name, the quota's default key, so
			// both consumptive calls share a slot.
			policy("rl-consumer", "ratelimit", "config:", "  key: consumer.name", "  limits: [{requests: 100, window: 1m}]"),
			policy("quota-refund", "quota", "config: {consumerQuota: r}"),
			policy("cache", "cache", "config: {}"),
			route("local", "/local", "rl-local"),
			route("gcra", "/gcra", "rl-gcra"),
			route("shared", "/shared", "rl-s1", "rl-s2"),
			route("split", "/split", "rl-gcra", "rl-k2"),
			route("quota", "/quota", "apikey", "quota-q"),
			route("mixed", "/mixed", "apikey", "rl-consumer", "quota-q"),
			deadRoute("refund", "/refund", "apikey", "quota-refund"),
			route("cache", "/cache", "cache"),
			deadUpstream(),
		},
	}
	// The Consumer of the refund Route has a quota of its own.
	b.docs[0] = strings.Replace(b.docs[0], "  quotas:\n", "  quotas:\n    - {name: r, unit: requests, limit: 100, window: 1h}\n", 1)
	return b.render()
}

// deadUpstream returns the Upstream "dead", whose one Endpoint refuses
// connections, so no attempt reaches it; it makes no retry, whose backoff
// would wait on the Node clock.
func deadUpstream() string {
	return "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: dead\nspec:\n  protocol: http\n" +
		"  endpoints:\n    - address: \"127.0.0.1:1\"\n  retries:\n    attempts: 0\n"
}

// deadRoute returns a Route to the Upstream "dead".
func deadRoute(name, prefix string, policies ...string) string {
	return strings.Replace(route(name, prefix, policies...), "    - name: backend\n", "    - name: dead\n", 1)
}

// memoryConfig gives a process State Store the maxmemory the Response
// Cache's memory rules need before a shared connection admits a store,
// under noeviction.
func memoryConfig() []string {
	return []string{"maxmemory 1gb", "maxmemory-policy noeviction"}
}

// TestRoundTrip is the round-trip test of ADR-0008 for every M1 State
// Store Policy type under pack 8.7 rules 1 to 4, on every kind
// redisserver selects, counting request-path commands only: ratelimit
// local denials and over-limit hits send nothing, an admitted request one
// call per Policy, consumptive keys sharing a slot one script and other
// keys one call each, in order, stopping at the first deny, without a
// CROSSSLOT reply; a quota denial cached up to 60 s sends nothing and a
// refund follows the response; a cache lookup is one pipelined batch of
// GET, HMGET and TIME, and the store follows the response.
func TestRoundTrip(t *testing.T) {
	for _, k := range allKinds() {
		t.Run(string(k), func(t *testing.T) {
			e := newEnv(t, envOptions{kind: k, config: memoryConfig(), upstream: cacheable()})
			setMaxMemory(e, e.stores["main"])
			e.start(roundTripBundle(k, e.up.Addr()), mainURLs())
			m := e.meter("main")
			// The first memory readings, which the shared connection's
			// stores wait for, follow the client's first maintenance pass.
			e.advance(time.Second)
			// The State client opens its pipelined connections at first
			// use; the phases count no connection setup, which a counting
			// proxy logs apart, and no round trip of it.
			e.warmUp("/shared", "x-client", "warm-up")

			rateLimitRoundTrips(t, e, m)
			quotaRoundTrips(t, e, m)
			refundFollowsResponse(t, e, m)
			cacheRoundTrips(t, e, m)
		})
	}
}

// setMaxMemory gives a container State Store, which runs its image as it
// is, the maxmemory of memoryConfig through CONFIG SET on every primary;
// process State Stores take it from their configuration.
func setMaxMemory(e *env, s interface {
	Deployment() *redisserver.Deployment
},
) {
	e.t.Helper()
	if e.kind.Flavor() != redisserver.FlavorContainer {
		return
	}
	for _, srv := range s.Deployment().Nodes() {
		if _, err := srv.Do(e.t.Context(), "CONFIG", "SET", "maxmemory", "1gb"); err != nil {
			e.t.Fatalf("CONFIG SET maxmemory on %s: %v", srv.Name(), err)
		}
	}
}

// one is the request-path command map of n EVALSHA calls.
func evalsha(n int64) map[string]int64 {
	if n == 0 {
		return map[string]int64{}
	}
	return map[string]int64{"EVALSHA": n}
}

// rateLimitRoundTrips runs the ratelimit cases of the round trip.
func rateLimitRoundTrips(t *testing.T, e *env, m *meter) {
	for i := range 2 {
		d := m.phase(func() { expect(t, "an admitted request", e.get("/local", "x-client", "l"), http.StatusOK, "") })
		expectCommands(t, "admitted request "+string(rune('1'+i)), d, evalsha(1), 1)
	}
	d := m.phase(func() {
		expect(t, "a local denial", e.get("/local", "x-client", "l"), http.StatusTooManyRequests, "RZ-RL-001")
	})
	expectCommands(t, "a local denial", d, evalsha(0), 0)

	d = m.phase(func() { expect(t, "the GCRA admission", e.get("/gcra", "x-client", "g"), http.StatusOK, "") })
	expectCommands(t, "the GCRA admission", d, evalsha(1), 1)
	d = m.phase(func() {
		expect(t, "the GCRA deny", e.get("/gcra", "x-client", "g"), http.StatusTooManyRequests, "RZ-RL-002")
	})
	expectCommands(t, "the GCRA deny", d, evalsha(1), 1)
	d = m.phase(func() {
		for range 3 {
			expect(t, "an over-limit hit", e.get("/gcra", "x-client", "g"), http.StatusTooManyRequests, "RZ-RL-002")
		}
	})
	expectCommands(t, "over-limit cache hits", d, evalsha(0), 0)
	// The deny stays cached until its retry-after, 30 s on the Node clock;
	// then the key asks GCRA again, whose server clock has not moved.
	e.advance(31 * time.Second)
	d = m.phase(func() {
		expect(t, "the GCRA deny again", e.get("/gcra", "x-client", "g"), http.StatusTooManyRequests, "RZ-RL-002")
	})
	expectCommands(t, "the request after the retry-after", d, evalsha(1), 1)

	d = m.phase(func() { expect(t, "two Policies on one slot", e.get("/shared", "x-client", "s"), http.StatusOK, "") })
	expectCommands(t, "two consumptive keys sharing a slot", d, evalsha(1), 1)

	d = m.phase(func() {
		expect(t, "two Policies on two slots", e.get("/split", "x-client", "k", "x-other", "o"), http.StatusOK, "")
	})
	expectCommands(t, "two consumptive keys on two slots", d, evalsha(2), 2)
	d = m.phase(func() {
		expect(t, "the first Policy's deny", e.get("/split", "x-client", "k", "x-other", "o"), http.StatusTooManyRequests, "RZ-RL-002")
	})
	expectCommands(t, "a deny before another slot's call", d, evalsha(1), 1)
	if n := d.errors["CROSSSLOT"]; n != 0 {
		t.Errorf("the State Store answered CROSSSLOT %d times", n)
	}
}

// quotaRoundTrips runs the quota cases of the round trip: the Consumer's
// quota q admits 2 requests per hour.
func quotaRoundTrips(t *testing.T, e *env, m *meter) {
	key := []string{"x-api-key", apiKey}
	d := m.phase(func() { expect(t, "a quota admission", e.get("/quota", key...), http.StatusOK, "") })
	expectCommands(t, "a quota admission", d, evalsha(1), 1)
	d = m.phase(func() { expect(t, "a ratelimit and a quota", e.get("/mixed", key...), http.StatusOK, "") })
	expectCommands(t, "a ratelimit and a quota call sharing a slot", d, evalsha(1), 1)
	d = m.phase(func() {
		expect(t, "the quota denial", e.get("/quota", key...), http.StatusTooManyRequests, "RZ-RL-003")
	})
	expectCommands(t, "the quota denial", d, evalsha(1), 1)
	d = m.phase(func() {
		for range 3 {
			expect(t, "a cached quota denial", e.get("/quota", key...), http.StatusTooManyRequests, "RZ-RL-003")
		}
	})
	expectCommands(t, "cached quota denials", d, evalsha(0), 0)
	// The denial is cached up to 60 s on the Node clock.
	e.advance(59 * time.Second)
	d = m.phase(func() {
		expect(t, "a cached quota denial", e.get("/quota", key...), http.StatusTooManyRequests, "RZ-RL-003")
	})
	expectCommands(t, "a quota denial within 60 s", d, evalsha(0), 0)
	e.advance(2 * time.Second)
	d = m.phase(func() {
		expect(t, "the quota denial again", e.get("/quota", key...), http.StatusTooManyRequests, "RZ-RL-003")
	})
	expectCommands(t, "a quota denial after 60 s", d, evalsha(1), 1)
}

// refundFollowsResponse checks that a quota refund, for a request whose
// attempt never reached its Upstream, is written after the response: with
// a counting proxy holding every EVALSHA reply, the response completes
// while the refund is unanswered.
func refundFollowsResponse(t *testing.T, e *env, m *meter) {
	key := []string{"x-api-key", apiKey}
	before := m.read()
	release := m.hold("EVALSHA", replyHold)
	r := e.get("/refund", key...)
	at := m.since(before)
	release()
	expect(t, "a request no attempt reached the Upstream of", r, http.StatusBadGateway, "RZ-UP-001")
	if m.proxied() && answered(at.log, "EVALSHA") != 1 {
		t.Errorf("when the response completed the State Store had answered %d EVALSHA calls, want the reservation alone: %v", answered(at.log, "EVALSHA"), at.log)
	}
	m.e.waitFor("the refund", func() bool { return m.since(before).requestPath["EVALSHA"] >= 2 })
	expectCommands(t, "the reservation and its refund", m.settled(before, 2), evalsha(2), 2)
}

// cacheRoundTrips checks the Response Cache: a lookup is one pipelined
// batch of GET, HMGET and TIME per server it reaches (the generation key
// and the partition hash to different cluster slots), and the store, two
// scripts, follows the response.
func cacheRoundTrips(t *testing.T, e *env, m *meter) {
	lookup := []string{"GET", "HMGET", "TIME"}
	before := m.read()
	release := m.hold("EVALSHA", replyHold)
	r := e.get("/cache/item")
	at := m.since(before)
	release()
	expect(t, "a cache miss", r, http.StatusOK, "")
	if got := r.header.Get("Cache-Status"); !strings.Contains(got, "fwd=uri-miss") {
		t.Errorf("Cache-Status = %q, want a uri-miss", got)
	}
	if m.proxied() {
		if answered(at.log, "EVALSHA") != 0 {
			t.Errorf("when the miss completed the State Store had answered a store: %v", at.log)
		}
		if n, s := trips(at.log, lookup...), servers(at.log, lookup...); n != s || n == 0 {
			t.Errorf("the miss lookup took %d round trips over %d servers, want one pipelined batch per server: %v", n, s, at.log)
		}
	}
	m.e.waitFor("the store", func() bool { return m.since(before).requestPath["EVALSHA"] >= 2 })
	d := m.settled(before, 2)
	expectCommands(t, "the miss and its store", d, map[string]int64{"GET": 1, "HMGET": 1, "TIME": 1, "EVALSHA": 2},
		servers(d.log, lookup...)+2)

	d = m.phase(func() {
		r := e.get("/cache/item")
		expect(t, "a cache hit", r, http.StatusOK, "")
		if got := r.header.Get("Cache-Status"); !strings.Contains(got, "hit") {
			t.Errorf("Cache-Status = %q, want a hit", got)
		}
	})
	if d.requestPath["HMGET"] != 1 || d.requestPath["TIME"] != 1 || d.requestPath["GET"] > 1 || len(d.requestPath) > 3 {
		t.Errorf("the hit's lookup sent %s, want HMGET and TIME, with or without the GET of a token the Node holds", d.commands())
	}
	if n, s := trips(d.log, lookup...), servers(d.log, lookup...); m.proxied() && n != s {
		t.Errorf("the hit's lookup took %d round trips over %d servers, want one pipelined batch per server: %v", n, s, d.log)
	}
}

// hold makes every counting proxy of m hold the replies to command for d
// and returns the function releasing them; it does nothing without
// counting proxies.
func (m *meter) hold(command string, d time.Duration) (release func()) {
	m.e.t.Helper()
	for _, p := range m.proxies {
		if err := p.SetReplyDelays(respproxy.ReplyDelay{Command: command, Delay: d}); err != nil {
			m.e.t.Fatal(err)
		}
	}
	return func() {
		for _, p := range m.proxies {
			_ = p.SetReplyDelays()
		}
	}
}

// settled waits until the request-path commands since earlier include n
// answered EVALSHA calls, then returns them.
func (m *meter) settled(earlier reading, n int) delta {
	m.e.t.Helper()
	var d delta
	m.e.waitFor("answered writes", func() bool {
		d = m.since(earlier)
		return !m.proxied() || answered(d.log, "EVALSHA") >= n
	})
	return d
}

// answered counts the commands named name in log whose reply arrived.
func answered(log []logged, name string) int {
	n := 0
	for _, c := range log {
		if c.Name == name && c.Reply.Kind != 0 {
			n++
		}
	}
	return n
}
