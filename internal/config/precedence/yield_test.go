// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"math/bits"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// stepClock is a fake clock that advances by step on every read; it is
// safe for concurrent use and reads no wall clock.
type stepClock struct {
	step  time.Duration
	reads atomic.Int64
}

func (c *stepClock) now() time.Time {
	return time.Unix(0, 0).Add(time.Duration(c.reads.Add(1)) * c.step)
}

// yields sums the yields of every worker of s.
func yields(s *state) int {
	n := 0
	for _, w := range s.pool.workers {
		n += w.yields
	}
	return n
}

// TestWorkerCheck covers the yield timer of 01 req 53 on one worker: no
// clock read below 256 units, one read when they reach it (also in one
// charge), a yield once YieldAfter has passed since the last one and none
// before, a second read after the yield to restart the interval, and
// nothing at all without a clock.
func TestWorkerCheck(t *testing.T) {
	var at time.Duration
	reads := 0
	now := func() time.Time {
		reads++
		return time.Unix(0, 0).Add(at)
	}
	w := &worker{now: now, after: 100 * time.Microsecond, last: time.Unix(0, 0)}
	w.charge(siteKey, yieldCheck-1)
	if reads != 0 {
		t.Errorf("%d clock reads below %d units", reads, yieldCheck)
	}
	at = 99 * time.Microsecond
	w.charge(siteKey, 1)
	if reads != 1 || w.yields != 0 {
		t.Errorf("at 99 µs: %d reads, %d yields; want 1 read, no yield", reads, w.yields)
	}
	at = 100 * time.Microsecond
	w.charge(siteKey, 3*yieldCheck)
	if reads != 3 || w.yields != 1 || w.last != time.Unix(0, 0).Add(at) {
		t.Errorf("at 100 µs: %d reads, %d yields, last %v; want 3 reads (check and restart), 1 yield", reads, w.yields, w.last)
	}
	w.check()
	if reads != 4 || w.yields != 1 {
		t.Errorf("right after a yield: %d reads, %d yields", reads, w.yields)
	}
	if w.total != 4*yieldCheck || w.sites[siteKey] != 4*yieldCheck {
		t.Errorf("total %d, site %d", w.total, w.sites[siteKey])
	}

	idle := &worker{}
	idle.charge(siteKey, 10*yieldCheck)
	idle.check()
	if idle.yields != 0 || idle.total != 10*yieldCheck {
		t.Errorf("worker without a clock: %d yields, %d units", idle.yields, idle.total)
	}
	var none *worker
	none.charge(siteKey, 1)
	none.check()
}

// TestYieldBetweenUnits covers the WP-38 S2 round-3 finding that stage I
// never yielded (01 req 53, 04 req 55): with a clock on which YieldAfter
// passes at every read, each worker yields before every work unit,
// on 1 and 4 workers, so 1,000 Routes whose units do a few units of work
// each yield at least 1,000 times, far more than their work alone (a few
// thousand units, one read per 256) would.
func TestYieldBetweenUnits(t *testing.T) {
	b := newBuilder()
	for i := range 1000 {
		b.route(fmt.Sprintf("r%04d", i), nil, nil, nil)
	}
	bundle := b.bundle()
	for _, workers := range []int{1, 4} {
		clock := &stepClock{step: time.Microsecond}
		r := mustNew(t, nil, Options{Now: clock.now, YieldAfter: time.Microsecond})
		s, err := r.resolve(t.Context(), bundle, nil, workers, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := yields(s); got < 1000 {
			t.Errorf("workers %d: %d yields for 1,000 Route units, want at least one per unit", workers, got)
		}
	}
}

// TestYieldInsideUnit covers the yield inside one long work unit (01 req
// 53: checked every 256 nodes): one Route with 100,000 excludePolicies
// entries is one unit of 100,000 work units, which yields at least once
// per 256 of them on a clock where YieldAfter passes at every read.
func TestYieldInsideUnit(t *testing.T) {
	const n = 100_000
	b := newBuilder()
	ex := make([]string, n)
	for i := range ex {
		ex[i] = fmt.Sprintf("absent%06d", i)
	}
	b.route("r", nil, ex, nil)
	for _, workers := range []int{1, 4} {
		clock := &stepClock{step: time.Microsecond}
		r := mustNew(t, nil, Options{Now: clock.now, YieldAfter: time.Microsecond})
		s, err := r.resolve(t.Context(), b.bundle(), nil, workers, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := yields(s); got < n/yieldCheck {
			t.Errorf("workers %d: %d yields in one unit of %d work units, want at least %d", workers, got, n, n/yieldCheck)
		}
	}
}

// TestYieldAfterHonored covers YieldAfter: on a clock that advances 1 µs
// per read, one worker with YieldAfter 50 µs yields about once per 51
// reads (50 checks without a yield, then the yielding check and the
// restart read), never once per read.
func TestYieldAfterHonored(t *testing.T) {
	b := newBuilder()
	for i := range 3000 {
		b.route(fmt.Sprintf("r%04d", i), nil, nil, nil)
	}
	clock := &stepClock{step: time.Microsecond}
	r := mustNew(t, nil, Options{Now: clock.now, YieldAfter: 50 * time.Microsecond})
	s, err := r.resolve(t.Context(), b.bundle(), nil, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	y, reads := yields(s), int(clock.reads.Load())
	if y == 0 || reads < 51*y-51 || reads > 51*y+51 {
		t.Errorf("%d yields over %d clock reads, want one per 51 reads", y, reads)
	}
}

// TestYieldNilNowAndResult covers the two promises of Options.Now beside
// yielding: without a clock no worker yields or reads one, and the result
// never depends on the yields (chains and findings equal with and without
// a clock, on 1 and 4 workers, over the shop Bundle and the fuzz seeds).
func TestYieldNilNowAndResult(t *testing.T) {
	shop, files := loadBundle(t, shopBundle)
	bundles := []struct {
		b     *hub.Bundle
		files *tree.FileTable
	}{{shop, files}}
	for _, seed := range fuzzSeeds(t) {
		bundles = append(bundles, struct {
			b     *hub.Bundle
			files *tree.FileTable
		}{genBundle(seed), nil})
	}
	for i, in := range bundles {
		for _, workers := range []int{1, 4} {
			plain := mustNew(t, nil, Options{Body: bodyOracle{}})
			s, ks, err := plain.run(t.Context(), in.b, in.files, workers)
			if err != nil {
				t.Fatal(err)
			}
			if y := yields(s); y != 0 {
				t.Errorf("bundle %d workers %d: %d yields without a clock", i, workers, y)
			}
			clock := &stepClock{step: time.Microsecond}
			yielding := mustNew(t, nil, Options{Body: bodyOracle{}, Now: clock.now, YieldAfter: time.Microsecond})
			s2, ks2, err := yielding.run(t.Context(), in.b, in.files, workers)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(s.chains, s2.chains) || !reflect.DeepEqual(diagnostics(ks), diagnostics(ks2)) {
				t.Errorf("bundle %d workers %d: the result changes with yields", i, workers)
			}
			if yields(s2) == 0 {
				t.Errorf("bundle %d workers %d: no yield with a clock", i, workers)
			}
		}
	}
}

// siteCounts runs stage I on b on one worker without a clock and returns the
// work units of every site: Run's when run is set, else Resolve's.
func siteCounts(t *testing.T, b *hub.Bundle, files *tree.FileTable, run bool) [siteCount]int {
	t.Helper()
	r := mustNew(t, nil, Options{Body: bodyOracle{}})
	var s *state
	var err error
	if run {
		s, _, err = r.run(t.Context(), b, files, 1)
	} else {
		s, err = r.resolve(t.Context(), b, files, 1, false)
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [siteCount]int
	for _, w := range s.pool.workers {
		for i := range out {
			out[i] += w.sites[i]
		}
	}
	return out
}

// shapes builds the Bundles of TestWorkCharged.
type shapes struct{}

func (shapes) names(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%05d", prefix, i)
	}
	return out
}

// gateway returns a Gateway listing n ratelimit Policies (onRequestHeaders
// only, each in its own slot) and Routes built by routes.
func (sh shapes) gateway(n int, routes func(b *builder, gw []string)) *hub.Bundle {
	b := newBuilder()
	gw := sh.names("g", n)
	for _, p := range gw {
		b.policy(p, v1alpha1.PolicyTypeRateLimit, nil)
	}
	b.gateway(gw...)
	if routes != nil {
		routes(b, gw)
	}
	return b.bundle()
}

// quotaRoute returns a Route listing n quota Policies (onRequestHeaders
// and onLog, each in its own slot) plus extra.
func (sh shapes) quotaRoute(n int, extra func(b *builder) []string) *hub.Bundle {
	b := newBuilder()
	ps := sh.names("q", n)
	for _, p := range ps {
		b.policy(p, v1alpha1.PolicyTypeQuota, nil)
	}
	if extra != nil {
		ps = append(extra(b), ps...)
	}
	b.route("r", ps, nil, nil)
	return b.bundle()
}

// TestWorkCharged covers the class of the round-3 yield finding: every
// loop of stage I whose length depends on the input counts its elements
// at its own site, so the yield timer is read every 256 units inside a
// unit wherever the work is (01 req 53). Each row is one loop: a Bundle
// whose size n drives it, and the exact count of its site for two sizes
// (comparison sites: between n-1 and n·log2(n) + n, as sorting n already
// ordered elements costs). Removing the charge of any loop fails its row.
func TestWorkCharged(t *testing.T) {
	var sh shapes
	exact := func(f func(n int) int) func(n, got int) bool { return func(n, got int) bool { return got == f(n) } }
	// heapSorting is the count of heapSort on m elements: at most two
	// comparisons per level an element moves down, while building the
	// heap and while popping it.
	heapSorting := func(f func(n int) int) func(n, got int) bool {
		return func(n, got int) bool {
			m := f(n)
			return got >= m-1 && got <= 2*m*bits.Len(uint(m))+2*m
		}
	}
	long := func(prefix string, n int) []string {
		out := sh.names(prefix, n)
		for i := range out {
			out[i] += strings.Repeat("x", maxRefBytes)
		}
		return out
	}
	excludeAll := func(b *builder, gw []string) { b.route("r", nil, gw, nil) }
	upstreams := func(n int, reversed bool) *hub.Bundle {
		b := newBuilder()
		us := sh.names("u", n)
		if reversed {
			slices.Reverse(us)
		}
		b.route("r", nil, nil, us)
		return b.bundle()
	}
	tests := []struct {
		name  string
		site  site
		run   bool
		build func(n int) *hub.Bundle
		files func(b *hub.Bundle) *tree.FileTable
		want  func(n, got int) bool
	}{
		{"newState: every resource", siteResource, false, func(n int) *hub.Bundle {
			b := newBuilder()
			for _, c := range sh.names("c", n) {
				b.add(v1alpha1.KindConsumer, c, &v1alpha1.Consumer{Metadata: v1alpha1.ObjectMeta{Name: c}})
			}
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"addKeys: every list entry", siteKey, false, func(n int) *hub.Bundle {
			b := newBuilder()
			b.route("r", slices.Repeat([]string{"ghost"}, n), nil, nil)
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"selectAll: every attachKey", siteSelected, false, func(n int) *hub.Bundle { return sh.quotaRoute(n, nil) }, nil, exact(func(n int) int { return n })},
		{"addKeys: every reference too long to resolve", siteLongKey, false, func(n int) *hub.Bundle {
			b := newBuilder()
			b.route("r", long("p", n), nil, nil)
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"attachList: every reference too long to resolve", siteLongList, false, func(n int) *hub.Bundle {
			b := newBuilder()
			b.route("r", long("p", n), nil, nil)
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"selectOne: every spec.plugin too long to resolve", siteLongPlugin, false, func(n int) *hub.Bundle {
			b := newBuilder()
			gw := sh.names("pp", n)
			for i, p := range gw {
				b.policy(p, v1alpha1.PolicyTypePlugin, nil, pluginOf(long("pl", n)[i], v1alpha1.FilterClassCustom))
			}
			b.gateway(gw...)
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"slotID: every attachKey's slot, per byteUnit bytes", siteSlotIntern, false, func(n int) *hub.Bundle {
			b := newBuilder()
			ps := sh.names("p", n)
			for i, p := range ps {
				b.policy(p, v1alpha1.PolicyTypeRateLimit, nil, slot(fmt.Sprintf("%0*d", 10*byteUnit, i)))
			}
			b.route("r", ps, nil, nil)
			return b.bundle()
		}, nil, exact(func(n int) int { return 11 * n })},
		{"slotID: one slot of n·byteUnit bytes at two scopes", siteSlotIntern, false, func(n int) *hub.Bundle {
			b := newBuilder()
			b.policy("p", v1alpha1.PolicyTypeRateLimit, nil, slot(strings.Repeat("s", n*byteUnit)))
			b.gateway("p")
			b.route("r", []string{"p"}, nil, nil)
			return b.bundle()
		}, nil, exact(func(n int) int { return 2 * (n + 1) })},
		{"attachList: every entry, missing ones included", siteListEntry, false, func(n int) *hub.Bundle {
			b := newBuilder()
			b.route("r", slices.Repeat([]string{"ghost"}, n), nil, nil)
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"listLoc: every item read", siteLocate, false, func(n int) *hub.Bundle {
			bb := sh.quotaRoute(n, nil)
			res, _ := bb.Get(hub.ID{Kind: v1alpha1.KindRoute, Name: "r"})
			res.Tree = listTree(1, 1, sh.names("q", n))
			return bb
		}, nil, exact(func(n int) int { return n })},
		{"listLoc: every tree name too long to index", siteLongLocate, false, func(n int) *hub.Bundle {
			bb := sh.quotaRoute(1, nil)
			res, _ := bb.Get(hub.ID{Kind: v1alpha1.KindRoute, Name: "r"})
			res.Tree = listTree(1, 1, append(long("t", n), "q00000"))
			return bb
		}, nil, exact(func(n int) int { return n })},
		{"attachGateway: every valid entry", siteGatewayItem, false, func(n int) *hub.Bundle { return sh.gateway(n, nil) }, nil, exact(func(n int) int { return n })},
		{"fill: every entry counted", siteFillCount, false, func(n int) *hub.Bundle { return sh.gateway(n, nil) }, nil, exact(func(n int) int { return n })},
		{"fill: every placement", siteFillPlace, false, func(n int) *hub.Bundle { return sh.gateway(n, nil) }, nil, exact(func(n int) int { return n })},
		{"newSharedPhase: every onRequestBody entry scanned", siteShared, false, func(n int) *hub.Bundle {
			b := newBuilder()
			b.plugin("pl", v1alpha1.PhaseOnRequestBody)
			gw := sh.names("pp", n)
			for _, p := range gw {
				b.policy(p, v1alpha1.PolicyTypePlugin, nil, pluginOf("pl", v1alpha1.FilterClassCustom))
			}
			b.gateway(gw...)
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"attachUpstreams: every leg", siteUpstream, false, func(n int) *hub.Bundle {
			b := newBuilder()
			for _, u := range sh.names("u", n) {
				b.upstream(u)
			}
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"attachUpstreams: every valid entry", siteLegItem, false, func(n int) *hub.Bundle {
			b := newBuilder()
			hs := sh.names("h", n)
			for _, h := range hs {
				b.policy(h, v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
			}
			b.upstream("up", hs...)
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"route: every excludePolicies entry", siteExclude, false, func(n int) *hub.Bundle {
			b := newBuilder()
			b.route("r", nil, sh.names("absent", n), nil)
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"route: every exclusion too long to resolve", siteLongExclude, false, func(n int) *hub.Bundle {
			return sh.gateway(16, func(b *builder, _ []string) { b.route("r", nil, long("g", n), nil) })
		}, nil, exact(func(n int) int { return n })},
		{"route: every valid Route entry", siteKept, false, func(n int) *hub.Bundle { return sh.quotaRoute(n, nil) }, nil, exact(func(n int) int { return n })},
		{"route: every removed Gateway entry", siteRemoved, false, func(n int) *hub.Bundle { return sh.gateway(n, excludeAll) }, nil, exact(func(n int) int { return n })},
		{"route: every comparison sorting removals", siteRemovedSort, false, func(n int) *hub.Bundle { return sh.gateway(n, excludeAll) }, nil, heapSorting(func(n int) int { return n })},
		{"route: every removal read, once per reason", siteRemovedList, false, func(n int) *hub.Bundle { return sh.gateway(n, excludeAll) }, nil, exact(func(n int) int { return 2 * n })},
		{"route: every leg", siteLeg, false, func(n int) *hub.Bundle { return upstreams(n, false) }, nil, exact(func(n int) int { return n })},
		{"client: every kept entry counted", siteKeptIn, false, func(n int) *hub.Bundle { return sh.quotaRoute(n, nil) }, nil, exact(func(n int) int { return n })},
		{"client: every kept entry placed in class order", siteKeptOrder, false, func(n int) *hub.Bundle { return sh.quotaRoute(n, nil) }, nil, exact(func(n int) int { return n })},
		{"client: every removed position", siteRemovedPos, false, func(n int) *hub.Bundle { return sh.gateway(n, excludeAll) }, nil, exact(func(n int) int { return n })},
		{"removedIn: every removal of a Phase the Route adds to", siteRemovedIn, false, func(n int) *hub.Bundle {
			return sh.gateway(n, func(b *builder, gw []string) {
				b.policy("q", v1alpha1.PolicyTypeQuota, nil)
				b.route("r", []string{"q"}, gw, nil)
			})
		}, nil, exact(func(n int) int { return n })},
		{"removalKey: every removal of a removal-only Phase", siteRemovalKey, false, func(n int) *hub.Bundle { return sh.gateway(n, excludeAll) }, nil, exact(func(n int) int { return n })},
		{"without: every entry counted", siteWithoutCount, false, func(n int) *hub.Bundle {
			return sh.gateway(n, func(b *builder, gw []string) { b.route("r", nil, gw[:1], nil) })
		}, nil, exact(func(n int) int { return n })},
		{"without: every entry copied", siteWithoutCopy, false, func(n int) *hub.Bundle {
			return sh.gateway(n, func(b *builder, gw []string) { b.route("r", nil, gw[:1], nil) })
		}, nil, exact(func(n int) int { return n })},
		{"client: every Gateway entry copied", siteCopyBase, false, func(n int) *hub.Bundle {
			return sh.gateway(n, func(b *builder, _ []string) {
				b.policy("q", v1alpha1.PolicyTypeQuota, nil)
				b.route("r", []string{"q"}, nil, nil)
			})
		}, nil, exact(func(n int) int { return n })},
		{"client: every kept entry copied, per Phase", siteCopyKept, false, func(n int) *hub.Bundle { return sh.quotaRoute(n, nil) }, nil, exact(func(n int) int { return 2 * n })},
		{"cacheGuardrail: every kept entry", siteGuardrailKept, false, func(n int) *hub.Bundle {
			return sh.quotaRoute(n, func(b *builder) []string {
				b.policy("cache", v1alpha1.PolicyTypeCache, nil)
				b.policy("authz", v1alpha1.PolicyTypeAuthzCEL, authzCfg("request.body.x"))
				return []string{"cache", "authz"}
			})
		}, nil, exact(func(n int) int { return n + 2 })},
		{"legOrder: every spec.upstreams name", siteLegUpstream, false, func(n int) *hub.Bundle { return upstreams(n, false) }, nil, exact(func(n int) int { return n })},
		{"legOrder: every Upstream name too long to resolve", siteLongLeg, false, func(n int) *hub.Bundle {
			b := newBuilder()
			rt := b.route("r", nil, nil, long("u", n))
			rt.Spec.Composition = &v1alpha1.Composition{Mode: v1alpha1.CompositionModeSequential}
			for i, u := range long("s", n) {
				rt.Spec.Composition.Steps = append(rt.Spec.Composition.Steps, v1alpha1.CompositionStep{Name: fmt.Sprintf("s%d", i), Upstream: u})
			}
			return b.bundle()
		}, nil, exact(func(n int) int { return 2 * n })},
		{"legOrder: every comparison", siteLegSort, false, func(n int) *hub.Bundle { return upstreams(n, true) }, nil, heapSorting(func(n int) int { return n })},
		{"legOrder: every composition step", siteLegStep, false, func(n int) *hub.Bundle {
			b := newBuilder()
			rt := b.route("r", nil, nil, nil)
			rt.Spec.Composition = &v1alpha1.Composition{Mode: v1alpha1.CompositionModeSequential}
			for i, u := range sh.names("u", n) {
				rt.Spec.Composition.Steps = append(rt.Spec.Composition.Steps, v1alpha1.CompositionStep{Name: fmt.Sprintf("s%d", i), Upstream: u})
			}
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"legOrder: every name deduplicated", siteLegDedupe, false, func(n int) *hub.Bundle { return upstreams(n, false) }, nil, exact(func(n int) int { return n })},
		{"run: every chain", siteChain, false, func(n int) *hub.Bundle {
			b := newBuilder()
			for _, r := range sh.names("r", n) {
				b.route(r, nil, nil, nil)
			}
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"worker: every diagnostic offered", siteOffer, false, func(n int) *hub.Bundle {
			b := newBuilder()
			b.policy("oa", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
			for _, r := range sh.names("r", n) {
				b.route(r, []string{"oa"}, nil, nil)
			}
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
		{"collector: every comparison sorting the kept ones", siteKeepSort, false, func(n int) *hub.Bundle {
			b := newBuilder()
			b.policy("oa", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
			for _, r := range sh.names("r", n) {
				b.route(r, []string{"oa"}, nil, nil)
			}
			return b.bundle()
		}, nil, heapSorting(func(n int) int { return n })},
		{"flush: every diagnostic merged and each comparison merging it", siteFlush, true, func(n int) *hub.Bundle {
			b := newBuilder()
			b.policy("oa", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
			for _, r := range sh.names("r", n) {
				b.route(r, []string{"oa"}, nil, nil)
			}
			for _, p := range sh.names("jwt", n) {
				b.policy(p, v1alpha1.PolicyTypeAuthJWT, nil, failOpen)
			}
			return b.bundle()
		}, nil, func(n, got int) bool { return got >= 2*n && got <= 2*n*(1+bits.Len(uint(2*n))) }},
		{"checkPolicies: every resource", sitePolicyCheck, true, func(n int) *hub.Bundle {
			b := newBuilder()
			for _, c := range sh.names("c", n) {
				b.add(v1alpha1.KindConsumer, c, &v1alpha1.Consumer{Metadata: v1alpha1.ObjectMeta{Name: c}})
			}
			return b.bundle()
		}, nil, exact(func(n int) int { return n })},
	}
	covered := map[site]bool{siteGuardrailGateway: true} // TestWorkChargedGatewayCache
	for _, tc := range tests {
		covered[tc.site] = true
		t.Run(tc.name, func(t *testing.T) {
			for _, n := range []int{100, 300} {
				b := tc.build(n)
				var files *tree.FileTable
				if tc.files != nil {
					files = tc.files(b)
				}
				if got := siteCounts(t, b, files, tc.run)[tc.site]; !tc.want(n, got) {
					t.Errorf("n=%d: %d units at the site", n, got)
				}
			}
		})
	}
	for s := range siteCount {
		if !covered[s] {
			t.Errorf("site %d has no row", s)
		}
	}
}

// TestWorkChargedGatewayCache is TestWorkCharged's row for the Gateway
// cache entries the guardrail reads, which registry.New cannot reach
// (cache is Route-only; see TestCacheGuardrailGatewayCache): with n
// Gateway cache entries injected beside an onRequestBody authz entry, a
// Route charges one unit per Gateway cache entry.
func TestWorkChargedGatewayCache(t *testing.T) {
	for _, n := range []int{100, 300} {
		b := newBuilder()
		var gw []string
		for i := range n {
			gw = append(gw, fmt.Sprintf("c%04d", i))
		}
		b.gateway(append(gw, "z")...)
		b.route("keep", nil, nil, nil)
		bundle := b.bundle()
		s := newState(mustNew(t, nil, Options{}), bundle, nil, 1)
		w0 := s.pool.worker(0)
		for _, c := range gw {
			s.sels[attachKey{policy: c, scope: phase.ScopeGateway}] = &selection{slot: s.slotID(w0, c), entry: hub.Entry{
				Policy: c, Type: v1alpha1.PolicyTypeCache, Class: phase.ClassCache, Slot: c, Scope: phase.ScopeGateway,
				Overridable: true, Phases: phase.Of(phase.OnRequestHeaders),
			}}
		}
		s.sels[attachKey{policy: "z", scope: phase.ScopeGateway}] = &selection{slot: s.slotID(w0, "z"), entry: hub.Entry{
			Policy: "z", Type: v1alpha1.PolicyTypeAuthzCEL, Class: phase.ClassAuthz, Slot: "z", Scope: phase.ScopeGateway,
			Overridable: true, Phases: phase.Of(phase.OnRequestBody),
		}}
		s.attachGateway()
		res, _ := bundle.Get(hub.ID{Kind: v1alpha1.KindRoute, Name: "keep"})
		rt, _ := hub.Object[v1alpha1.Route](res)
		w := s.r.newWorker(newCollector(keepOf(DefaultMaxDiagnostics), 1))
		s.route(w, res, rt)
		if got := w.sites[siteGuardrailGateway]; got != n {
			t.Errorf("n=%d: %d units, want %d", n, got, n)
		}
		if got := w.offered; got != n {
			t.Errorf("n=%d: %d RZ-CFG-038 offered, want %d", n, got, n)
		}
	}
}

// TestNoBulkCharges covers the WP-38 S2 round-2 minor that three loops
// charged their work only after an uninterruptible pass over up to a
// whole Phase (newSharedPhase's scan of onRequestBody, the client's scan
// of a built onRequestBody, and the removal sort with its key
// collection), so the yield timer was read once after up to 20,000
// elements instead of every 256 (01 req 53), and the class: every loop of
// a work unit charges each element as it reads it. On one worker whose
// clock records the worker's work units at every read, a run over 4·256
// elements of each shape never charges more than 256 units plus the few
// one flush of the collector charges at once between two reads: the
// onRequestBody scan of a shared Phase, the merge of a built
// onRequestBody, removals sorted and listed, legs sorted, kept findings
// sorted, and an Upstream leg filled. Before the fix the first three
// charged 1,024 at once.
func TestNoBulkCharges(t *testing.T) {
	const n = 4 * yieldCheck
	var sh shapes
	plugins := func(b *builder, k int) []string {
		b.plugin("pl", v1alpha1.PhaseOnRequestBody)
		ps := sh.names("pp", k)
		for _, p := range ps {
			b.policy(p, v1alpha1.PolicyTypePlugin, nil, pluginOf("pl", v1alpha1.FilterClassCustom))
		}
		return ps
	}
	for _, tc := range []struct {
		name  string
		build func() *hub.Bundle
	}{
		{"shared onRequestBody scanned", func() *hub.Bundle {
			b := newBuilder()
			b.gateway(plugins(b, n)...)
			b.route("r", nil, nil, nil)
			return b.bundle()
		}},
		{"built onRequestBody merged", func() *hub.Bundle {
			b := newBuilder()
			b.gateway(plugins(b, n)...)
			b.policy("own", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl", v1alpha1.FilterClassCustom))
			b.route("r", []string{"own"}, nil, nil)
			return b.bundle()
		}},
		{"removals sorted and listed", func() *hub.Bundle {
			return sh.gateway(n, func(b *builder, gw []string) {
				ex := slices.Clone(gw)
				slices.Reverse(ex)
				b.route("r", nil, ex, nil)
			})
		}},
		{"legs sorted", func() *hub.Bundle {
			b := newBuilder()
			us := sh.names("u", n)
			slices.Reverse(us)
			b.route("r", nil, nil, us)
			return b.bundle()
		}},
		{"kept findings sorted", func() *hub.Bundle {
			b := newBuilder()
			b.policy("oa", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
			for _, r := range sh.names("r", n) {
				b.route(r, []string{"oa"}, nil, nil)
			}
			return b.bundle()
		}},
		{"Upstream leg filled", func() *hub.Bundle {
			b := newBuilder()
			hs := sh.names("h", n)
			for _, h := range hs {
				b.policy(h, v1alpha1.PolicyTypeHeaders, headersCfg(true, true))
			}
			b.upstream("up", hs...)
			return b.bundle()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cur *worker
			var marks []int
			r := mustNew(t, nil, Options{Body: bodyOracle{}, YieldAfter: time.Hour, Now: func() time.Time {
				if cur != nil {
					marks = append(marks, cur.total)
				}
				return time.Unix(0, 0)
			}})
			s := newState(r, tc.build(), nil, 1)
			cur = s.pool.worker(0)
			start := cur.total
			if err := s.run(t.Context()); err != nil {
				t.Fatal(err)
			}
			marks = append(marks, cur.total)
			most, prev := 0, start
			for _, m := range marks {
				most, prev = max(most, m-prev), m
			}
			// One flush merges one finding with at most two comparisons
			// per level of the collector's heap.
			if slack := 1 + 2*bits.Len(uint(n)); most > yieldCheck+slack || cur.total-start < n {
				t.Errorf("%d units charged between two clock reads (of %d in run), want at most %d", most, cur.total-start, yieldCheck+slack)
			}
		})
	}
}
