// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNoEndpoints reports an empty Endpoint set (05 req 11 step a); the
// Upstream layer answers 503 RZ-UP-008.
var ErrNoEndpoints = errors.New("balance: endpoint set is empty")

// Config configures a Balancer.
type Config struct {
	// Algorithm is the Upstream's loadBalancing.algorithm.
	Algorithm Algorithm
	// VirtualNodes is the planned virtual nodes per Endpoint of a ring-hash
	// ring (Plan.VirtualNodes); 0 means MaxVirtualNodes.
	VirtualNodes int
	// Fallback makes a ring-hash Upstream run weighted random because the
	// balancer budget has no room for its ring (05 req 17).
	Fallback bool
	// Endpoints is the initial set in Normalize's form.
	Endpoints []Endpoint
	// Payload is published with Endpoints (see Set.Payload).
	Payload any
	// Rand seeds each round-robin build's cursor start; nil uses the runtime
	// generator. Calls are serialized by the Balancer.
	Rand rand.Source
	// Now is the time of the initial build, the start of the first rebuild
	// interval.
	Now time.Time
}

// Info describes a Balancer's published structure for /debug/upstreams (05
// req 96) and telemetry.
type Info struct {
	// Algorithm is the configured algorithm.
	Algorithm Algorithm
	// Effective is the algorithm serving picks: Random for a ring-hash
	// Upstream under a budget fallback, else Algorithm.
	Effective Algorithm
	// VirtualNodes is the published ring's virtual nodes per Endpoint; 0
	// without a ring.
	VirtualNodes int
	// Fallback reports a ring-hash Upstream running weighted random for lack
	// of budget (degraded reason balancer_budget).
	Fallback bool
	// Bytes is the published structure's budgeted size.
	Bytes int64
	// Endpoints is the size of the current set.
	Endpoints int
	// Built is when the published structure was built.
	Built time.Time
	// Stale reports that the current set or plan differs from the published
	// structure, which a rebuild will fix.
	Stale bool
}

// Set is one published selection state of a Balancer: the Endpoint list
// current when it was published, the caller's payload published with that
// list, and the structures serving it. A Set is immutable and safe for
// concurrent use. Its Select and SelectRandom grade through a View indexed
// like its own list and return indices into that list, so the Upstream
// runtime loads one Set per attempt (Balancer.Load) and builds the View and
// its index mapping from that Set's Payload, never from a list loaded
// separately: a set change between the two can then never misalign them.
type Set struct {
	eps     []Endpoint
	payload any
	p       *Picker // throttled structure, built from an earlier or the current list
	// remap maps p's indices to eps indices (-1: left the set or weighs 0
	// now); nil when p's list and weights equal eps.
	remap []int32
	// present counts p's weighted Endpoints still usable in eps.
	present int
	// added counts weighted Endpoints of eps that p does not cover.
	added int
	// cur is the weighted random table of eps, rebuilt with every set change
	// (linear in Endpoints): the keyless ring-hash pick and the fallback
	// while p lags behind the set.
	cur *Picker
}

// Len returns the size of the Set's Endpoint list.
func (s *Set) Len() int { return len(s.eps) }

// Endpoint returns Endpoint i of the Set's list.
func (s *Set) Endpoint(i int) Endpoint { return s.eps[i] }

// Identity returns the identity of Endpoint i of the Set's list.
func (s *Set) Identity(i int) string { return s.eps[i].Identity }

// Payload returns the value published with the Set's list (Config.Payload
// or SetEndpoints): the Upstream runtime's per-Endpoint state, indexed like
// the list.
func (s *Set) Payload() any { return s.payload }

// Balancer selects Endpoints for one Upstream. SetEndpoints, SetPlan and
// Rebuild run off the request path (discovery, compile and the Upstream
// scheduler) and publish new Sets copy-on-write; request goroutines Load a
// Set and select on it without locks or allocation. Structures rebuild at
// most every RebuildInterval (05 req 16); until then Endpoints removed from
// the set are excluded and new ones wait, and health changes never cause a
// rebuild.
type Balancer struct {
	st atomic.Pointer[Set]

	mu        sync.Mutex
	alg       Algorithm
	v         int
	fallback  bool
	rnd       rand.Source
	cur       []Endpoint // current set, never mutated in place
	payload   any
	curTable  *Picker
	lastBuild time.Time
	seeded    bool // a structure over a non-empty set was built
	building  bool
	dirty     bool
	urgent    bool
}

// New builds a Balancer and its first structure at once (compile time, off
// the request path).
func New(cfg Config) (*Balancer, error) {
	if !cfg.Algorithm.Valid() {
		return nil, fmt.Errorf("balance: invalid algorithm %d", cfg.Algorithm)
	}
	if err := checkNormalized(cfg.Endpoints); err != nil {
		return nil, err
	}
	b := &Balancer{
		alg:      cfg.Algorithm,
		v:        virtualNodes(cfg.VirtualNodes),
		fallback: cfg.Fallback,
		rnd:      source(cfg.Rand),
		cur:      slices.Clone(cfg.Endpoints),
		payload:  cfg.Payload,
		seeded:   len(cfg.Endpoints) > 0,
	}
	b.curTable = build(Random, b.cur, 0, 0)
	b.mu.Lock()
	defer b.mu.Unlock()
	alg, v := b.effectiveLocked()
	b.lastBuild = cfg.Now
	b.publishLocked(build(alg, b.cur, v, b.rnd.Uint64()))
	return b, nil
}

// virtualNodes applies the VirtualNodes default and bounds.
func virtualNodes(v int) int {
	if v <= 0 {
		return MaxVirtualNodes
	}
	return min(v, MaxRingNodes)
}

// effectiveLocked returns the algorithm and virtual nodes to build.
func (b *Balancer) effectiveLocked() (Algorithm, int) {
	if b.alg != RingHash {
		return b.alg, 0
	}
	if b.fallback {
		return Random, 0
	}
	return RingHash, b.v
}

// SetEndpoints replaces the current set (in Normalize's form) and publishes
// it in a new Set with payload (see Set.Payload). The published structure
// keeps serving: removed Endpoints are excluded at once, added ones join at
// the next rebuild (05 req 16).
func (b *Balancer) SetEndpoints(eps []Endpoint, payload any) error {
	if err := checkNormalized(eps); err != nil {
		return err
	}
	cur := slices.Clone(eps)
	table := build(Random, cur, 0, 0)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cur, b.payload, b.curTable = cur, payload, table
	b.publishLocked(b.st.Load().p)
	return nil
}

// SetPlan applies the budget plan for this Upstream (05 req 17): v virtual
// nodes per Endpoint and whether a ring-hash Upstream falls back to weighted
// random. A plan that shrinks the ring or starts a fallback makes the next
// rebuild due at once; one that grows the ring waits for the interval.
// Other algorithms ignore the plan.
func (b *Balancer) SetPlan(v int, fallback bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.v, b.fallback = virtualNodes(v), fallback
	b.markLocked(b.st.Load())
}

// Load returns the published Set, the state one attempt selects on.
func (b *Balancer) Load() *Set { return b.st.Load() }

// Len returns the size of the current set.
func (b *Balancer) Len() int { return b.st.Load().Len() }

// Algorithm returns the configured algorithm.
func (b *Balancer) Algorithm() Algorithm { return b.alg }

// publishLocked publishes p against the current set and payload.
func (b *Balancer) publishLocked(p *Picker) {
	st := &Set{eps: b.cur, payload: b.payload, p: p, cur: b.curTable}
	st.remap, st.present, st.added = remap(p, b.cur, b.curTable.w)
	b.st.Store(st)
	b.markLocked(st)
}

// markLocked recomputes whether a rebuild is needed and whether it is
// urgent, due before RebuildInterval has passed: the first structure over a
// non-empty set (an Upstream whose set was empty from the start), or a
// budget plan that shrank the ring or started a fallback (05 req 17 "lower
// v at once"). Every other change, a full replacement of the set included,
// waits for the interval (05 req 16) while the current set's weighted
// random table serves.
func (b *Balancer) markLocked(st *Set) {
	alg, v := b.effectiveLocked()
	b.dirty = alg != st.p.alg || v != st.p.v || st.remap != nil
	shrink := st.p.alg == RingHash && (alg != RingHash || v < st.p.v)
	first := !b.seeded && len(st.eps) > 0
	b.urgent = b.dirty && (first || shrink)
}

// remap maps p's indices to the current list by a merge over both sorted
// lists, counting p's Endpoints still usable and current ones p lacks. It
// returns a nil map when p's list and weights equal the current ones.
func remap(p *Picker, cur []Endpoint, curW []uint32) (m []int32, present, added int) {
	m = make([]int32, len(p.ids))
	same := len(p.ids) == len(cur)
	j := 0
	for i, id := range p.ids {
		for j < len(cur) && cur[j].Identity < id {
			if curW[j] > 0 {
				added++
			}
			j++
			same = false
		}
		m[i] = -1
		if j < len(cur) && cur[j].Identity == id {
			if curW[j] != p.w[i] {
				same = false
			}
			switch {
			case curW[j] > 0 && p.w[i] > 0:
				m[i] = int32(j)
				present++
			case curW[j] > 0:
				added++
			}
			j++
			continue
		}
		same = false
	}
	for ; j < len(cur); j++ {
		same = false
		if curW[j] > 0 {
			added++
		}
	}
	if same {
		return nil, present, 0
	}
	return m, present, added
}

// RebuildDue reports whether the structure differs from the current set or
// plan and may rebuild now: RebuildInterval after the last build, or at
// once when urgent (05 req 16).
func (b *Balancer) RebuildDue(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dueLocked(now)
}

// dueLocked is RebuildDue under mu.
func (b *Balancer) dueLocked(now time.Time) bool {
	return b.dirty && !b.building && (b.urgent || !now.Before(b.lastBuild.Add(RebuildInterval)))
}

// NextRebuild returns when a rebuild becomes due, for the Upstream
// scheduler's queue, and false when the structure is current.
func (b *Balancer) NextRebuild() (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.dirty {
		return time.Time{}, false
	}
	if b.urgent {
		return b.lastBuild, true
	}
	return b.lastBuild.Add(RebuildInterval), true
}

// Rebuild builds a new structure from the current set and plan and
// publishes it, if one is due at now. gate bounds concurrent builds per
// Node (nil: unbounded); waiting for a slot ends with ctx. It reports
// whether it published a new structure. Concurrent calls build at most
// once.
func (b *Balancer) Rebuild(ctx context.Context, gate *BuildGate, now time.Time) (bool, error) {
	if !b.RebuildDue(now) {
		return false, nil
	}
	if gate != nil {
		if err := gate.Acquire(ctx); err != nil {
			return false, err
		}
		defer gate.Release()
	}
	b.mu.Lock()
	if !b.dueLocked(now) {
		b.mu.Unlock()
		return false, nil
	}
	b.building = true
	eps := b.cur
	alg, v := b.effectiveLocked()
	seed := b.rnd.Uint64()
	b.mu.Unlock()

	p := build(alg, eps, v, seed)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.building = false
	b.lastBuild = now
	b.seeded = b.seeded || len(eps) > 0
	b.publishLocked(p)
	return true, nil
}

// Select chooses an Endpoint of the Set for one attempt (05 req 11 steps a
// to d): ErrNoEndpoints for an empty set, else an Endpoint of the lowest
// grade present picked by the algorithm. key is the leg's ring-hash key
// (HashKey of the hashKey value, reused by retries); other algorithms
// ignore it. src supplies randomness (nil: the runtime generator); view
// grades Endpoints by their index in the Set's list, and Choice.Index is an
// index into that list.
func (s *Set) Select(key uint64, src rand.Source, view View) (Choice, error) {
	if len(s.eps) == 0 {
		return Choice{Index: -1}, ErrNoEndpoints
	}
	src = source(src)
	if s.present > 0 {
		g := grader{view: view, remap: s.remap}
		// A structure that lags the set may lack a better Endpoint only when
		// the set gained Endpoints; then the current table decides.
		if i, gr, ok := s.p.pick(key, src, &g); ok && (gr == Eligible || s.added == 0) {
			return Choice{Index: g.index(i), Grade: gr}, nil
		}
	}
	return s.pickCurrent(src, view)
}

// SelectRandom chooses a weighted random Endpoint of the Set with the same
// grading as Select. A ring-hash leg whose hashKey failed at runtime uses it
// (05 req 14).
func (s *Set) SelectRandom(src rand.Source, view View) (Choice, error) {
	if len(s.eps) == 0 {
		return Choice{Index: -1}, ErrNoEndpoints
	}
	return s.pickCurrent(source(src), view)
}

// pickCurrent picks from the weighted random table of the Set's list.
func (s *Set) pickCurrent(src rand.Source, view View) (Choice, error) {
	g := grader{view: view}
	i, gr, ok := s.cur.pickRandom(src, &g, -1)
	if !ok {
		return Choice{Index: -1}, ErrNoEndpoints
	}
	return Choice{Index: i, Grade: gr}, nil
}

// Info describes the published structure.
func (b *Balancer) Info() Info {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.st.Load()
	return Info{
		Algorithm:    b.alg,
		Effective:    st.p.alg,
		VirtualNodes: st.p.v,
		Fallback:     b.alg == RingHash && st.p.alg == Random,
		Bytes:        st.p.Bytes(),
		Endpoints:    st.Len(),
		Built:        b.lastBuild,
		Stale:        b.dirty,
	}
}
