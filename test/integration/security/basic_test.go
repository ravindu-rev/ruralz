// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// The auth.basic throttle (docs/architecture/08-security-and-identity.md
// "Pre-authentication throttling"), in controlled time: each source.ip may
// fail 10 verifications per minute, after which its uncached attempts are
// refused; each username's bucket holds 10 attempts refilling one per
// second, an empty one holding at most 2 waiters for up to 2 s; every
// refusal is 429 RZ-AUTH-007 with Retry-After before any hash, counted by
// cause in ruralz_auth_basic_refusals_total; a success cache hit skips the
// throttle and the hash.

// The throttle targets.
const (
	// sourceRefill is how long one failure of a source takes to refill:
	// 10 per minute.
	sourceRefill = 6 * time.Second
	// userRefill is how long one username token takes to refill.
	userRefill = time.Second
	// throttleBurst is the failures a source, and the attempts a
	// username, may make at once.
	throttleBurst = 10
)

// basicHash returns the credentials.basic hash of password at the default
// 600,000 iterations: pbkdf2-sha256:<salt>:<key> in base64url.
func basicHash(t *testing.T, password string) string {
	t.Helper()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, 600_000, 32)
	if err != nil {
		t.Fatal(err)
	}
	return "pbkdf2-sha256:" + base64.RawURLEncoding.EncodeToString(salt) + ":" + base64.RawURLEncoding.EncodeToString(key)
}

// basicAuth is the Authorization field of user and password.
func basicAuth(user, password string) []string {
	return []string{"Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))}
}

// fromSource returns a client whose connections leave from the loopback
// address 127.0.0.<host>, host 1 to 254, which Linux answers without an
// alias: the Node sees it as source.ip.
func fromSource(t *testing.T, top *topology.Topology, host int) *http.Client {
	t.Helper()
	if host < 1 || host > 254 {
		t.Fatalf("fromSource: host %d is not 1 to 254", host)
		return nil
	}
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, byte(host))}}
	tr := &http.Transport{DialContext: d.DialContext, DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)
	return capturing(top, tr)
}

// wantThrottled fails t unless r is 429 RZ-AUTH-007 with a Retry-After of
// at least one second, and returns it.
func wantThrottled(t *testing.T, what string, r reply) time.Duration {
	t.Helper()
	wantCode(t, what, r, http.StatusTooManyRequests, "RZ-AUTH-007")
	s, err := strconv.Atoi(r.header.Get("Retry-After"))
	if err != nil || s < 1 {
		t.Fatalf("%s: Retry-After %q, want at least 1 s", what, r.header.Get("Retry-After"))
	}
	return time.Duration(s) * time.Second
}

func TestBasicAuthThrottle(t *testing.T) {
	top := newTopology(t, topology.Config{})
	mock := startMock(t, top, mockup.Config{})
	password := top.Canary()
	b := bundle(
		gatewayDoc(webListener),
		upstreamDoc("backend", mock.Addr(), ""),
		policyDoc("basic", "auth.basic", ""),
		"apiVersion: ruralz/v1alpha1\nkind: Consumer\nmetadata:\n  name: alice-app\nspec:\n  credentials:\n    basic:\n      - username: alice\n        hash: "+basicHash(t, password)+"\n",
		routeDoc("basic", "/basic", "backend", "basic"),
	)
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{Bundle: b})
	if err != nil {
		t.Fatal(err)
	}
	n := nodes[0]
	url := n.URL("web") + "/basic"
	refusals := func(cause string) float64 {
		return freshCounter(t, top, n, "ruralz_auth_basic_refusals_total", "cause", cause)
	}

	t.Run("source", func(t *testing.T) {
		c := fromSource(t, top, 1)
		wantOK(t, "alice's verified credential", get(t, c, url, basicAuth("alice", password)...))
		for i := range throttleBurst {
			wantCode(t, fmt.Sprintf("failure %d", i+1), get(t, c, url, basicAuth(fmt.Sprintf("guess-%d", i), "wrong")...), http.StatusUnauthorized, "RZ-AUTH-002")
		}
		retry := wantThrottled(t, "attempt past 10 failures", get(t, c, url, basicAuth("guess-10", "wrong")...))
		if retry > sourceRefill {
			t.Fatalf("Retry-After %v, want at most one refill of %v", retry, sourceRefill)
		}
		// The success cache skips the throttle: alice is not locked out
		// by the flood from her address.
		wantOK(t, "alice's cached credential from the flooded source", get(t, c, url, basicAuth("alice", password)...))
		// Once the bucket refills one failure, one more attempt is
		// verified, then refused again.
		advance(t, top, retry)
		wantCode(t, "attempt after the refill", get(t, c, url, basicAuth("guess-11", "wrong")...), http.StatusUnauthorized, "RZ-AUTH-002")
		wantThrottled(t, "attempt after the refilled failure", get(t, c, url, basicAuth("guess-12", "wrong")...))
		if got := refusals("source"); got != 2 {
			t.Fatalf("%v refusals by source, want 2", got)
		}
	})
	t.Run("username delay", func(t *testing.T) {
		// Ten attempts from ten sources empty the username's bucket
		// without filling any source's.
		for i := range throttleBurst {
			wantCode(t, fmt.Sprintf("attempt %d on victim", i+1), get(t, fromSource(t, top, 20+i), url, basicAuth("victim", "wrong")...), http.StatusUnauthorized, "RZ-AUTH-002")
		}
		// Three more at once: the empty bucket holds two until tokens
		// refill and refuses the third at once.
		replies := make(chan reply, 3)
		for i := range 3 {
			c := fromSource(t, top, 40+i)
			go func() {
				req, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodGet, url, nil)
				if err != nil {
					replies <- reply{}
					return
				}
				req.Header.Set("Authorization", basicAuth("victim", "wrong")[1])
				r, err := trySend(c, req)
				if err != nil {
					r = reply{status: -1, body: []byte(err.Error())}
				}
				replies <- r
			}()
		}
		wantThrottled(t, "a third waiter for the empty bucket", <-replies)
		for i := range 2 {
			advance(t, top, userRefill)
			wantCode(t, fmt.Sprintf("waiter %d after a refill", i+1), <-replies, http.StatusUnauthorized, "RZ-AUTH-002")
		}
		if got := refusals("delay"); got != 1 {
			t.Fatalf("%v refusals by delay, want 1", got)
		}
	})
}
