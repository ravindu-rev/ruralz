// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package gameday

import (
	"context"
	"fmt"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// FC-16's runs and the codes its tokens get
// (docs/architecture/08-security-and-identity.md "JWT and OIDC").
const (
	fc16Name       = "FC-16"
	fc16KeyRemoval = "FC-16/key-removal"
	// fc16NoKeys answers a token whose issuer has no usable keys, and
	// fc16UnknownKid one whose kid the issuer's key set lacks.
	fc16NoKeys     = "RZ-AUTH-006"
	fc16UnknownKid = "RZ-AUTH-005"
	// fc16Tokens is how many tokens, of distinct subjects, each signing
	// key mints for a run; a stream sends them in turn.
	fc16Tokens = 4
	// fc16Evidence is the stretch of the load's schedule a run keeps
	// running once a state is reached, so its criteria count requests in
	// that state.
	fc16Evidence = 10 * time.Second
)

// fc16Keep are the metric families FC-16 samples beside chaos.DefaultKeep:
// the key sets' age and the auth decisions.
func fc16Keep() []string {
	return []string{catalog.AuthJWKSAgeSeconds, catalog.AuthDecisionsTotal}
}

// TestFC16JWKSUnreachable is FC-16, "IdP or JWKS unreachable", on the
// controllable clock: every Node takes the jwt-chaos Policy in a Hot
// Reload and fetches the mock IdP's keys, served with max-age=300; after
// the baseline the IdP refuses every fetch. Tokens keep passing on the
// cached keys while jwks_stale is raised and ruralz_auth_jwks_age_seconds
// grows, the half-life refresh having failed; once the keys expire, 300 s
// after their fetch on the Nodes' clock, which the run walks there, every
// token gets 401 RZ-AUTH-006, while /plain, outside the row's blast
// radius, never fails. The IdP then answers again: every Node refetches,
// tokens pass and jwks_stale clears
// (docs/operations/04-high-availability-and-disaster-recovery.md
// "Failure catalog"; docs/engineering/03-testing-and-quality-strategy.md
// "Drills and failure rows").
func TestFC16JWKSUnreachable(t *testing.T) {
	if !suite.Functional.TestClock {
		t.Skip("gameday: FC-16 walks the Nodes' clock to the keys' expiry, which needs the controllable-clock build")
	}
	s := saturation(t)
	r := suite.Start(t, chaos.Spec{Name: fc16Name, SampleEvery: 250 * time.Millisecond, Keep: fc16Keep()})
	ctx := r.Context()
	idp, err := r.Top.StartIDP(mockidp.Config{CacheControl: FC16CacheControl})
	if err != nil {
		t.Fatal(err)
	}
	signer, ok := idp.Key(mockidp.RS256)
	if !ok {
		t.Fatal("gameday: the mock IdP publishes no RS256 key")
	}
	tokens := fc16Mint(t, idp, signer, "rs256")
	fc16Publish(ctx, t, r, idp)
	rate := suite.Profile.LoadShare * s
	l := r.StartLoad(chaos.LoadSpec{Duration: chaos.Baseline + 4*time.Minute + chaos.RecoveryTime, Streams: []chaos.StreamSpec{
		{Name: "jwt", Path: chaos.PathJWT, Rate: rate / 2, Prepare: fc16Bearer(tokens), Counted: []string{fc16NoKeys}},
		{Name: "plain", Path: chaos.PathPlain, Rate: rate / 2},
	}})
	r.Begin(l)
	base := r.Hold(l, chaos.PhaseBaseline, chaos.Baseline)
	refuseAt := r.Inject("refuse-jwks", "mock-idp", func() error {
		idp.SetJWKSOverride(mockidp.Override{Status: http.StatusServiceUnavailable})
		return nil
	})
	// Walk the Nodes' clock past the latest expiry the fetches so far
	// allow, and one retry beyond, then keep the expired state under load.
	fetched := FC16Fetched(idp.JWKSFetches(), http.StatusOK, time.Time{}, time.Now())
	if len(fetched) == 0 {
		t.Fatal("gameday: no Node fetched the issuer's keys")
	}
	_, reach, _ := FC16Expiry(fetched, r.CauseSpan, r.NodeSpan, FC16Lifetime)
	reach = reach.Add(FC16Retry)
	if !r.WalkUntil(FC16Lifetime+2*FC16Retry, func() bool { return !r.Top.Now().Before(reach) }) {
		t.Logf("gameday: the walk ended before the Nodes' clock reached %v past the refusal", reach.Sub(r.NodeTime(refuseAt)))
	}
	if err := l.WaitUntil(ctx, time.Now().Add(fc16Evidence)); err != nil {
		t.Fatal(err)
	}
	fault := r.Mark(chaos.PhaseFault, refuseAt)
	restoreAt := r.Inject("restore-jwks", "mock-idp", func() error {
		idp.SetJWKSOverride(mockidp.Override{})
		return nil
	})
	rec := r.Hold(l, chaos.PhaseRecovery, chaos.RecoveryTime)
	l.Stop()
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	logStreams(r, l)
	r.Baseline(l, base)
	jwt, plain := l.StreamNamed("jwt"), l.StreamNamed("plain")
	nodes := r.NodeNames()

	// The expiry window of the cached key sets, from every fetch the IdP
	// answered before it recovered: those of the reload, and any refresh
	// that got through before the refusal.
	fetched = FC16Fetched(idp.JWKSFetches(), http.StatusOK, time.Time{}, restoreAt)
	earliest, latest, _ := FC16Expiry(fetched, r.CauseSpan, r.NodeSpan, FC16Lifetime)
	refuseNode := r.NodeTime(refuseAt)
	r.Breakdown("fc16_earliest_expiry_after_refusal_s", earliest.Sub(refuseNode).Seconds())
	r.Breakdown("fc16_latest_expiry_after_refusal_s", latest.Sub(refuseNode).Seconds())
	r.Breakdown("fc16_walked_s", r.NodeTime(fault.End).Sub(refuseNode).Seconds())
	earliestReal, ok := r.Pump.RealTime(earliest)
	if !ok {
		earliestReal = fault.End
	}

	// Behavior: cached keys serve until they expire, then RZ-AUTH-006.
	baseLo, _ := r.NodeSpan(base.Start)
	cached := FC16Decided(jwt.Requests(), r.NodeSpan, baseLo, earliest, fc16NoKeys)
	restoreLo, _ := r.NodeSpan(restoreAt)
	expired := FC16Decided(jwt.Requests(), r.NodeSpan, latest, restoreLo, fc16NoKeys)
	r.Add(
		chaos.Check("FC-16: /jwt requests admitted on the cached keys", chaos.Functional, float64(cached.Admitted), chaos.GT, 0),
		chaos.Check("FC-16: /jwt requests failing before the cached keys could expire", chaos.Functional, float64(cached.Code+cached.Other), chaos.EQ, 0),
		chaos.Check("FC-16: /jwt requests answered RZ-AUTH-006 after the keys expired", chaos.Functional, float64(expired.Code), chaos.GT, 0),
		chaos.Check("FC-16: /jwt requests answered otherwise after the keys expired", chaos.Functional, float64(expired.Admitted+expired.Other), chaos.EQ, 0),
		chaos.Check("FC-16: RZ-AUTH-006 answers whose status is not 401", chaos.Functional, float64(fc16StatusOtherThan(jwt, fc16NoKeys, http.StatusUnauthorized)), chaos.EQ, 0),
		chaos.Check("FC-16: /plain requests failed", chaos.Functional, float64(chaos.PhaseStats(plain, base.Start, rec.End).Failed), chaos.EQ, 0),
	)

	// Detection: jwks_stale raised and the keys' age growing while the
	// cached keys still serve.
	stale := r.Sampler.ReasonRaised(nodes, catalog.ReasonJWKSStale.String(), refuseAt, earliestReal)
	grew := 0
	for _, n := range nodes {
		tr := chaos.Between(r.Sampler.Trace(n, catalog.AuthJWKSAgeSeconds, promtext.L()), refuseAt, earliestReal)
		if len(tr) >= 2 && tr[len(tr)-1].Value > tr[0].Value {
			grew++
		} else {
			t.Logf("gameday: %s: %d ruralz_auth_jwks_age_seconds samples while the keys were cached: %v", n, len(tr), tr)
		}
	}
	r.Add(
		chaos.Check("FC-16: Nodes raising jwks_stale while serving cached keys", chaos.Functional, float64(len(stale)), chaos.EQ, float64(len(nodes))),
		chaos.Check("FC-16: Nodes whose ruralz_auth_jwks_age_seconds grew while serving cached keys", chaos.Functional, float64(grew), chaos.EQ, float64(len(nodes))),
	)

	// Recovery: every Node refetches, one fetch each, then tokens pass and
	// jwks_stale clears.
	refetched := FC16Fetched(idp.JWKSFetches(), http.StatusOK, restoreAt, rec.End)
	r.Add(chaos.Check("FC-16: JWKS fetches answered after the IdP recovered", chaos.Functional, float64(len(refetched)), chaos.GE, float64(len(nodes))))
	if len(refetched) >= len(nodes) {
		// A fetch the IdP answered ends within the fetch bound.
		back := refetched[len(nodes)-1].Add(FC16FetchBound)
		r.Breakdown("fc16_keys_back_after_restore_s", back.Sub(restoreAt).Seconds())
		r.Add(chaos.Check("FC-16: /jwt requests failing once every Node refetched its keys", chaos.Functional, float64(fc16NotAdmitted(jwt, back, rec.End)), chaos.EQ, 0))
	}
	clearedAt, ok := r.Sampler.ReasonClearedAt(nodes, catalog.ReasonJWKSStale.String(), restoreAt)
	r.Add(chaos.Holds("FC-16: jwks_stale cleared within the recovery", chaos.Functional, ok && clearedAt.Before(rec.End)))
	r.StaleReasons(nodes, base.End, rec.End)
	r.Finish()
}

// TestFC16KeyRemoved is FC-16's second run, on the release build and the
// real clock: the mock IdP publishes two RS256 keys with max-age=300, and
// tokens signed by either pass; after the baseline it removes the old key
// from its document. Tokens the old key signed keep passing on the cached
// set until the Nodes refresh it at its 150 s half-life, then get 401
// RZ-AUTH-005 for good, Node by Node, while the kept key's tokens and
// /plain never fail (docs/engineering/03-testing-and-quality-strategy.md
// "Drills and failure rows"; docs/architecture/08-security-and-identity.md
// "JWT and OIDC").
func TestFC16KeyRemoved(t *testing.T) {
	s := saturation(t)
	// Measurement picks the release build in real time; MeasureKind
	// Functional keeps this run's validity rules and criteria functional,
	// so they gate wherever functional criteria do, unlike a latency or
	// CPU measurement's.
	r := suite.Start(t, chaos.Spec{
		Name: fc16KeyRemoval, Measurement: true, MeasureKind: chaos.Functional,
		SampleEvery: 250 * time.Millisecond, Keep: fc16Keep(),
	})
	ctx := r.Context()
	removed, err := mockidp.GenerateKey(mockidp.RS256)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := mockidp.GenerateKey(mockidp.RS256)
	if err != nil {
		t.Fatal(err)
	}
	idp, err := r.Top.StartIDP(mockidp.Config{Keys: []*mockidp.Key{removed, kept}, CacheControl: FC16CacheControl})
	if err != nil {
		t.Fatal(err)
	}
	removedTokens := fc16Mint(t, idp, removed, "removed")
	keptTokens := fc16Mint(t, idp, kept, "kept")
	fc16Publish(ctx, t, r, idp)
	rate := suite.Profile.LoadShare * s
	l := r.StartLoad(chaos.LoadSpec{Duration: chaos.Baseline + FC16HalfLife + FC16Retry + chaos.RecoveryTime + 2*time.Minute, Streams: []chaos.StreamSpec{
		{Name: "jwt-removed", Path: chaos.PathJWT, Rate: rate / 3, Prepare: fc16Bearer(removedTokens), Counted: []string{fc16UnknownKid}},
		{Name: "jwt-kept", Path: chaos.PathJWT, Rate: rate / 3, Prepare: fc16Bearer(keptTokens)},
		{Name: "plain", Path: chaos.PathPlain, Rate: rate / 3},
	}})
	r.Begin(l)
	base := r.Hold(l, chaos.PhaseBaseline, chaos.Baseline)
	removeAt := r.Inject("remove-key", "mock-idp", func() error {
		if n := idp.RemoveKey(removed.ID); n != 1 {
			return fmt.Errorf("removed %d keys of kid %s, want 1", n, removed.ID)
		}
		return nil
	})
	// Each Node refreshes its set at the half-life of its last fetch, the
	// reload's, at most FC16HalfLife after the removal; a refresh the IdP
	// answered ends within the fetch bound.
	refreshes := func() []time.Time {
		return FC16Fetched(idp.JWKSFetches(), http.StatusOK, removeAt, time.Now().Add(time.Hour))
	}
	if !waitNodeTime(ctx, r, FC16HalfLife+FC16FetchBound+FC16Retry, func() bool { return len(refreshes()) >= r.Nodes }) {
		t.Logf("gameday: %d of %d Nodes refreshed the key set after the removal", len(refreshes()), r.Nodes)
	}
	if err := l.WaitUntil(ctx, time.Now().Add(FC16FetchBound+fc16Evidence)); err != nil {
		t.Fatal(err)
	}
	r.Mark(chaos.PhaseFault, removeAt)
	rec := r.Hold(l, chaos.PhaseRecovery, chaos.RecoveryTime)
	l.Stop()
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	logStreams(r, l)
	r.Baseline(l, base)
	removedSt, keptSt, plain := l.StreamNamed("jwt-removed"), l.StreamNamed("jwt-kept"), l.StreamNamed("plain")
	nodes := r.NodeNames()

	// The removed key's tokens pass until the first refresh can come: the
	// half-life after the earliest fetch.
	before := FC16Fetched(idp.JWKSFetches(), http.StatusOK, time.Time{}, removeAt)
	after := FC16Fetched(idp.JWKSFetches(), http.StatusOK, removeAt, rec.End)
	if len(before) == 0 {
		t.Fatal("gameday: no Node fetched the issuer's keys before the removal")
	}
	early := FC16Decided(removedSt.Requests(), r.NodeSpan, base.Start, before[0].Add(FC16HalfLife), fc16UnknownKid)
	r.Add(
		chaos.Check("FC-16: removed-key requests failing before the half-life passed", chaos.Functional, float64(early.Code+early.Other), chaos.EQ, 0),
		chaos.Check("FC-16: JWKS refreshes after the removal", chaos.Functional, float64(len(after)), chaos.GE, float64(len(nodes))),
	)
	if len(after) > 0 {
		gap := after[0].Sub(before[0])
		r.Breakdown("fc16_first_refresh_after_first_fetch_s", gap.Seconds())
		r.Add(chaos.Check("FC-16: seconds from the first fetch to the first refresh after the removal", chaos.Functional, gap.Seconds(), chaos.GE, FC16HalfLife.Seconds()))
	}

	// Then they fail with 401 RZ-AUTH-005, on every Node, for good.
	first, admittedAfter := FC16Switch(removedSt.Requests(), nodeOf(r), fc16UnknownKid)
	for n, at := range first {
		r.Breakdown("fc16_switch_after_removal_s_"+n, at.Sub(removeAt).Seconds())
	}
	r.Add(
		chaos.Check("FC-16: Nodes answering RZ-AUTH-005 to the removed key's tokens", chaos.Functional, float64(len(first)), chaos.EQ, float64(len(nodes))),
		chaos.Check("FC-16: removed-key requests a Node admitted after its first RZ-AUTH-005", chaos.Functional, float64(admittedAfter), chaos.EQ, 0),
		chaos.Check("FC-16: removed-key requests failing otherwise than with 401 RZ-AUTH-005", chaos.Functional, float64(fc16FailedOtherwise(removedSt, fc16UnknownKid, http.StatusUnauthorized, base.Start, rec.End)), chaos.EQ, 0),
		chaos.Check("FC-16: kept-key requests failed", chaos.Functional, float64(chaos.PhaseStats(keptSt, base.Start, rec.End).Failed), chaos.EQ, 0),
		chaos.Check("FC-16: /plain requests failed", chaos.Functional, float64(chaos.PhaseStats(plain, base.Start, rec.End).Failed), chaos.EQ, 0),
	)
	r.StaleReasons(nodes, base.End, rec.End)
	r.Finish()
}

// fc16Publish renders the chaos Bundle with OverlayJWT for idp and moves
// every Node to it in one Hot Reload each: the test renders are replaced
// first and the Nodes then signaled together, so their compiles prefetch
// the issuer's keys within moments of each other, and their refreshes fall
// due together (docs/architecture/08-security-and-identity.md "JWT and
// OIDC"). The run's Bundle and parameters follow.
func fc16Publish(ctx context.Context, t *testing.T, r *chaos.Run, idp *mockidp.Server) {
	t.Helper()
	params := r.Params
	params.Overlay = chaos.OverlayJWT
	params.JWTIssuer, params.JWKSURL = idp.Issuer(), idp.JWKSURL()
	rendered, err := chaos.RenderBundle(ctx, suite.Pipeline(), params)
	if err != nil {
		t.Fatal(err)
	}
	nodes := r.Top.Nodes()
	for _, n := range nodes {
		if _, err := n.Reload(ctx, rendered); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range nodes {
		if err := n.Signal(ctx, syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range nodes {
		if err := n.WaitRevision(ctx, n.TestDigest()); err != nil {
			t.Fatal(err)
		}
	}
	r.Params, r.Bundle = params, rendered
}

// fc16Mint mints fc16Tokens tokens of jwt-chaos's audience signed by k,
// each of its own subject, issued by the IdP's clock now and expiring an
// hour later (mockidp.DefaultTokenTTL): past every walk of the Nodes'
// clock and within the 24 hour bound of exp.
func fc16Mint(t *testing.T, idp *mockidp.Server, k *mockidp.Key, label string) []string {
	t.Helper()
	out := make([]string, fc16Tokens)
	for i := range out {
		tok, err := idp.MintWith(k, nil, map[string]any{"sub": fmt.Sprintf("fc16-%s-%d", label, i+1), "aud": chaos.JWTAudience})
		if err != nil {
			t.Fatal(err)
		}
		out[i] = tok
	}
	return out
}

// fc16Bearer sends tokens in turn as each request's bearer token.
func fc16Bearer(tokens []string) func(*http.Request, uint64) {
	return func(q *http.Request, seq uint64) {
		q.Header.Set("Authorization", "Bearer "+tokens[seq%uint64(len(tokens))])
	}
}

// fc16StatusOtherThan counts the responses of st carrying code whose
// status is not status.
func fc16StatusOtherThan(st *chaos.Stream, code string, status int) int64 {
	var n int64
	for _, q := range st.Requests() {
		if q.Code == code && q.Status != status {
			n++
		}
	}
	return n
}

// fc16NotAdmitted counts the requests of st due in [from, to) without a
// 2xx answer, those a stream counts as achieved included.
func fc16NotAdmitted(st *chaos.Stream, from, to time.Time) int64 {
	var n int64
	for _, q := range st.Requests() {
		if !q.Intended.Before(from) && q.Intended.Before(to) && !chaos.Admitted(q) {
			n++
		}
	}
	return n
}

// fc16FailedOtherwise counts the requests of st due in [from, to) that
// were neither admitted nor answered status with code.
func fc16FailedOtherwise(st *chaos.Stream, code string, status int, from, to time.Time) int64 {
	var n int64
	for _, q := range st.Requests() {
		if q.Intended.Before(from) || !q.Intended.Before(to) || chaos.Admitted(q) {
			continue
		}
		if q.Code != code || q.Status != status {
			n++
		}
	}
	return n
}
