// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"cmp"
	"math/bits"
	"math/rand/v2"
	"slices"
	"sync/atomic"
)

// Grade ranks an Endpoint for one attempt by 05 req 11 steps (b) and (c).
// Lower is better; selection returns an Endpoint of the lowest grade
// present.
type Grade uint8

// Grades in order of preference.
const (
	// Eligible: neither Down nor Avoided.
	Eligible Grade = iota
	// Avoided: healthy, but already tried in this leg, suspect (05 req 21)
	// or at its in-flight cap (05 req 36); used only when every healthy
	// Endpoint is Avoided (step c "while another remains").
	Avoided
	// Down: passively ejected or actively unhealthy; used only when every
	// Endpoint is Down (step b panic mode).
	Down
	// DownAvoided: Down and Avoided; the last resort in panic mode.
	DownAvoided
)

// gone marks an Endpoint of a structure that has left the current set, or
// whose weight dropped to 0, since the structure was built: it counts as
// excluded until the rebuild and is never picked (05 req 16).
const gone Grade = 0xff

// GradeOf combines the two tests of 05 req 11: down is step (b) (passively
// ejected or actively unhealthy), avoid is step (c) (tried in this leg,
// suspect or capped).
func GradeOf(down, avoid bool) Grade {
	g := Eligible
	if avoid {
		g |= Avoided
	}
	if down {
		g |= Down
	}
	return g
}

// View is the per-attempt state selection consults, indexed like the
// Endpoint list of the Set (or Picker) selecting: the runtime builds it from
// the same Set it calls Select on (see Set). The Upstream runtime
// implements it over its per-Endpoint health, ejection, suspicion, in-flight
// and per-leg tried state. Implementations must be safe for concurrent use
// when shared between attempts. A nil View grades every Endpoint Eligible
// with load 0.
type View interface {
	// Grade grades Endpoint i. Values above DownAvoided count as
	// DownAvoided.
	Grade(i int) Grade
	// Load is Endpoint i's least-request load: in-flight attempts (the whole
	// attempt, response body streaming included) plus attempts that matched
	// failureWhen in the last second (05 req 13). Only least-request reads
	// it; negative values count as 0.
	Load(i int) int64
}

// Choice is the outcome of one selection.
type Choice struct {
	// Index is the chosen Endpoint's position in the list of the Set (or
	// Picker) that selected it.
	Index int
	// Grade is the chosen Endpoint's grade: above Eligible when step (c) or
	// panic mode had to relax.
	Grade Grade
}

// Panic reports whether no healthy Endpoint remained, so the choice came
// from panic mode (05 req 11 step b).
func (c Choice) Panic() bool { return c.Grade >= Down }

// Relaxed reports whether every usable Endpoint was tried, suspect or
// capped, so step (c) let an Avoided Endpoint through.
func (c Choice) Relaxed() bool { return c.Grade&Avoided != 0 }

// grader maps structure indices to current indices and grades them.
type grader struct {
	view View
	// remap maps a structure index to its current index, -1 when the
	// Endpoint left the set; nil when both lists are the same.
	remap []int32
}

// index returns the current index of structure index i.
func (g *grader) index(i int) int {
	if g.remap != nil {
		return int(g.remap[i])
	}
	return i
}

// grade grades structure index i.
func (g *grader) grade(i int) Grade {
	c := i
	if g.remap != nil {
		if c = int(g.remap[i]); c < 0 {
			return gone
		}
	}
	if g.view == nil {
		return Eligible
	}
	return min(g.view.Grade(c), DownAvoided)
}

// load returns the least-request load of structure index i.
func (g *grader) load(i int) uint64 {
	if g.view == nil {
		return 0
	}
	return uint64(max(g.view.Load(g.index(i)), 0))
}

// vnode is one ring virtual node (16 bytes, 05 req 14).
type vnode struct {
	pos uint64
	idx uint32
	_   uint32
}

// Picker is one immutable balancer structure built from an Endpoint list;
// a Balancer swaps Pickers copy-on-write. Every method is safe for
// concurrent use. Indices refer to the normalized list the Picker was built
// from (see Identity).
type Picker struct {
	alg   Algorithm
	ids   []string
	w     []uint32 // effective weights
	live  int      // Endpoints of weight > 0
	total uint64   // sum of w
	cum   []uint64 // random, least-request: cumulative weights
	sched []uint32 // round-robin: Endpoint index per slot
	ring  []vnode  // ring-hash: virtual nodes by position
	v     int      // ring-hash: virtual nodes per Endpoint

	_      [64]byte // keeps the round-robin cursor off the read-only fields' cache line
	cursor atomic.Uint64
	_      [56]byte
}

// newPicker returns the common part of every structure over a normalized
// list.
func newPicker(alg Algorithm, eps []Endpoint) *Picker {
	p := &Picker{alg: alg, ids: make([]string, len(eps))}
	for i, ep := range eps {
		p.ids[i] = ep.Identity
	}
	p.w, p.total, p.live = effectiveWeights(eps)
	return p
}

// build builds the structure of alg over a normalized list. v is the ring's
// virtual nodes per Endpoint and seed the round-robin cursor start.
func build(alg Algorithm, eps []Endpoint, v int, seed uint64) *Picker {
	p := newPicker(alg, eps)
	switch alg {
	case RoundRobin:
		p.buildSchedule(seed)
	case RingHash:
		p.buildRing(v)
	case LeastRequest, Random:
		p.buildTable()
	}
	return p
}

// BuildRoundRobin builds a round-robin schedule over eps (in any order):
// L = min(65,536, 64 × E) slots of uint32 Endpoint indices (one per Endpoint
// beyond 65,536 Endpoints), weights normalized to n_i = max(1, round(w_i × L
// / Σw)) apportioned to exactly L (see quotas) and interleaved earliest
// deadline first; the cursor starts at seed modulo the schedule length (05
// req 12).
func BuildRoundRobin(eps []Endpoint, seed uint64) *Picker {
	return build(RoundRobin, Normalize(eps), 0, seed)
}

// BuildRing builds a ring-hash ring over eps (in any order) of L = min(65,536,
// v × E) virtual nodes (v ≤ 0: MaxVirtualNodes), Endpoint i owning max(1,
// round(L × w_i / Σw)) of them, apportioned to exactly L (see quotas), at
// positions FNV-1a-64(identity + "#" + j) through the splitmix64 finalizer
// for j = 0, 1, … (05 req 14). Two Nodes given the same set build identical
// rings.
func BuildRing(eps []Endpoint, v int) *Picker {
	return build(RingHash, Normalize(eps), v, 0)
}

// BuildLeastRequest builds the least-request table over eps (05 req 13).
func BuildLeastRequest(eps []Endpoint) *Picker {
	return build(LeastRequest, Normalize(eps), 0, 0)
}

// BuildRandom builds the weighted random table over eps (05 req 15).
func BuildRandom(eps []Endpoint) *Picker {
	return build(Random, Normalize(eps), 0, 0)
}

// buildTable fills the cumulative weights.
func (p *Picker) buildTable() {
	p.cum = make([]uint64, len(p.w))
	var acc uint64
	for i, x := range p.w {
		acc += uint64(x)
		p.cum[i] = acc
	}
}

// edfItem is one Endpoint in the schedule builder: its job k (0-based) is
// released at k/n of the schedule and due at (k+1)/n.
type edfItem struct {
	idx uint32
	k   uint32 // slots emitted
	n   uint32 // slots owed
}

// edfHeap is a binary min-heap of schedule items, ordered by deadline
// (k+1)/n, or by release time k/n when byRelease, then by index.
type edfHeap struct {
	items     []edfItem
	byRelease bool
}

// before reports whether a precedes b.
func (h *edfHeap) before(a, b edfItem) bool {
	ka, kb := uint64(a.k)+1, uint64(b.k)+1
	if h.byRelease {
		ka, kb = ka-1, kb-1
	}
	if l, r := ka*uint64(b.n), kb*uint64(a.n); l != r {
		return l < r
	}
	return a.idx < b.idx
}

// push adds x.
func (h *edfHeap) push(x edfItem) {
	h.items = append(h.items, x)
	for i := len(h.items) - 1; i > 0; {
		parent := (i - 1) / 2
		if !h.before(h.items[i], h.items[parent]) {
			break
		}
		h.items[i], h.items[parent] = h.items[parent], h.items[i]
		i = parent
	}
}

// pop removes and returns the minimum.
func (h *edfHeap) pop() edfItem {
	top := h.items[0]
	last := len(h.items) - 1
	h.items[0] = h.items[last]
	h.items = h.items[:last]
	for i := 0; ; {
		m, l := i, 2*i+1
		if l < len(h.items) && h.before(h.items[l], h.items[m]) {
			m = l
		}
		if r := l + 1; r < len(h.items) && h.before(h.items[r], h.items[m]) {
			m = r
		}
		if m == i {
			return top
		}
		h.items[i], h.items[m] = h.items[m], h.items[i]
		i = m
	}
}

// buildSchedule fills the round-robin schedule by earliest deadline first
// over the quotas (05 req 12): at slot t the released job (k/n ≤ t/S) with
// the earliest deadline (k+1)/n runs. Releasing jobs no earlier than their
// share keeps every Endpoint within one slot of its ideal count in every
// prefix of the schedule, the smoothness the tests check. Two binary heaps
// make it O(L log E).
func (p *Picker) buildSchedule(seed uint64) {
	length := min(MaxScheduleSlots, SlotsPerEndpoint*p.live)
	n := quotas(p.w, p.total, length)
	ready := edfHeap{items: make([]edfItem, 0, p.live)}
	pending := edfHeap{items: make([]edfItem, 0, p.live), byRelease: true}
	var slots uint64
	for i, q := range n {
		if q > 0 {
			ready.push(edfItem{idx: uint32(i), n: q})
			slots += uint64(q)
		}
	}
	p.sched = make([]uint32, 0, slots)
	for t := range slots {
		for len(pending.items) > 0 && uint64(pending.items[0].k)*slots <= t*uint64(pending.items[0].n) {
			ready.push(pending.pop())
		}
		if len(ready.items) == 0 { // unreachable: Σ released jobs > t at every slot
			ready.push(pending.pop())
		}
		it := ready.pop()
		p.sched = append(p.sched, it.idx)
		if it.k++; it.k < it.n {
			pending.push(it)
		}
	}
	if slots > 0 {
		p.cursor.Store(seed % slots)
	}
}

// buildRing fills and sorts the ring (05 req 14). The per-Endpoint table is
// built too, for keyless picks.
func (p *Picker) buildRing(v int) {
	if v <= 0 {
		v = MaxVirtualNodes
	}
	p.v = min(v, MaxRingNodes)
	length := int(min(int64(MaxRingNodes), int64(p.v)*int64(p.live)))
	n := quotas(p.w, p.total, length)
	var nodes int
	for _, q := range n {
		nodes += int(q)
	}
	p.ring = make([]vnode, 0, nodes)
	for i, q := range n {
		if q == 0 {
			continue
		}
		prefix := vnodePrefix(p.ids[i])
		for j := range int(q) {
			p.ring = append(p.ring, vnode{pos: vnodePosition(prefix, j), idx: uint32(i)})
		}
	}
	slices.SortFunc(p.ring, func(a, b vnode) int {
		if c := cmp.Compare(a.pos, b.pos); c != 0 {
			return c
		}
		return cmp.Compare(a.idx, b.idx)
	})
	p.buildTable()
}

// Algorithm returns the algorithm the structure serves.
func (p *Picker) Algorithm() Algorithm { return p.alg }

// Len returns the number of Endpoints the structure was built from.
func (p *Picker) Len() int { return len(p.ids) }

// Identity returns the identity of Endpoint i.
func (p *Picker) Identity(i int) string { return p.ids[i] }

// Weight returns the effective weight of Endpoint i (see the package weight
// rule).
func (p *Picker) Weight(i int) uint32 { return p.w[i] }

// VirtualNodes returns the ring's virtual nodes per Endpoint, 0 for other
// algorithms.
func (p *Picker) VirtualNodes() int { return p.v }

// Slots returns the length of the round-robin schedule or of the ring.
func (p *Picker) Slots() int { return max(len(p.sched), len(p.ring)) }

// Bytes returns the budgeted size of the structure (05 req 17): 4 bytes per
// schedule slot, 16 bytes per ring virtual node, 8 bytes per Endpoint for
// the random and least-request tables. Per-Endpoint side arrays (weights,
// identities, a ring's keyless table) are linear in Endpoints and not
// budgeted.
func (p *Picker) Bytes() int64 {
	switch p.alg {
	case RoundRobin:
		return SlotBytes * int64(len(p.sched))
	case RingHash:
		return VirtualNodeBytes * int64(len(p.ring))
	case LeastRequest, Random:
		return TableBytes * int64(len(p.cum))
	}
	return 0
}

// Pick selects an Endpoint of the structure for one attempt: the lowest
// grade present, found in the algorithm's own order (05 req 11 steps b to
// d). key is the ring-hash key (HashKey of the hashKey value; other
// algorithms ignore it); src supplies randomness (nil: the runtime
// generator); view grades Endpoints by structure index (nil: all Eligible).
// It returns false only when the structure holds no Endpoint.
func (p *Picker) Pick(key uint64, src rand.Source, view View) (Choice, bool) {
	g := grader{view: view}
	i, gr, ok := p.pick(key, source(src), &g)
	if !ok {
		return Choice{Index: -1, Grade: DownAvoided}, false
	}
	return Choice{Index: i, Grade: gr}, true
}

// pick dispatches to the algorithm.
func (p *Picker) pick(key uint64, src rand.Source, g *grader) (int, Grade, bool) {
	switch p.alg {
	case RoundRobin:
		return p.pickRoundRobin(g)
	case RingHash:
		return p.pickRing(key, g)
	case Random:
		return p.pickRandom(src, g, -1)
	case LeastRequest:
		return p.pickLeastRequest(src, g)
	}
	return -1, gone, false
}

// seenCap is the largest structure whose distinct Endpoints the probe of a
// walk tracks, to stop early once it has graded all of them.
const seenCap = 512

// probeGrades bounds the Endpoints the probe of a walk grades. A probe that
// finds no Eligible Endpoint hands over to one linear pass over the
// Endpoint list for the lowest grade present, so a degraded pick (panic
// mode, everything Avoided) grades O(E) Endpoints instead of every slot.
const probeGrades = 64

// resumeCap is the Endpoint index range the resumed walk tracks, one bit
// each (an 8 KiB bitmap). Only a structure of more than 65,536 Endpoints has
// larger indices, and its slots then number at most max(65,536, E), so the
// walk still grades O(E) Endpoints.
const resumeCap = MaxRingNodes

// walk finds the first Endpoint of the lowest grade along a structure's
// order, grading each run of consecutive slots of one Endpoint once.
type walk struct {
	g        *grader
	best     int
	bestG    Grade
	last     int
	live     int
	track    bool
	distinct int
	graded   int
	seen     [seenCap / 64]uint64
}

// visit grades Endpoint i, once per walk while the walk tracks distinct
// Endpoints, and reports whether the walk can stop: an Eligible Endpoint
// was found, or every Endpoint was graded.
func (w *walk) visit(i int) bool {
	if i == w.last {
		return false
	}
	w.last = i
	if w.track {
		word, bit := i>>6, uint64(1)<<(i&63)
		if w.seen[word]&bit != 0 {
			return false
		}
		w.seen[word] |= bit
		w.distinct++
	}
	w.graded++
	if gr := w.g.grade(i); gr < w.bestG {
		w.best, w.bestG = i, gr
		if gr == Eligible {
			return true
		}
	}
	return w.track && w.distinct == w.live
}

// slot returns the Endpoint index at position pos of the schedule or ring.
func (p *Picker) slot(pos int) int {
	if p.alg == RingHash {
		return int(p.ring[pos].idx)
	}
	return int(p.sched[pos])
}

// wrap returns position start + k of a pass over n slots (start, k < n).
func wrap(start, k, n int) int {
	if pos := start + k; pos < n {
		return pos
	}
	return start + k - n
}

// walkFrom walks the schedule or ring forward from slot start for at most
// one pass and returns the first Endpoint of the lowest grade present (05
// reqs 12 and 14). A probe grades up to probeGrades Endpoints and serves
// every pick that meets an Eligible one; otherwise a linear pass finds the
// lowest grade present: if the probe already holds it the walk ends, else
// it resumes where the probe stopped and ends at the first slot of that
// grade. The answer is the one of a full walk, at O(E) grades.
func (p *Picker) walkFrom(start int, g *grader) (int, Grade, bool) {
	n := p.Slots()
	w := walk{g: g, best: -1, bestG: gone, last: -1, live: p.live, track: len(p.ids) <= seenCap}
	k := 0
	for ; k < n && w.graded < probeGrades; k++ {
		if w.visit(p.slot(wrap(start, k, n))) {
			return w.best, w.bestG, w.best >= 0
		}
	}
	if k == n {
		return w.best, w.bestG, w.best >= 0
	}
	low := p.lowestGrade(g)
	if low >= w.bestG {
		return w.best, w.bestG, w.best >= 0
	}
	return p.resume(start, k, low, g, w.best, w.bestG)
}

// lowestGrade grades every weighted Endpoint of the structure in list
// order and returns the lowest grade present, gone when none is usable.
func (p *Picker) lowestGrade(g *grader) Grade {
	low := gone
	for i, x := range p.w {
		if x == 0 {
			continue
		}
		if gr := g.grade(i); gr < low {
			if low = gr; gr == Eligible {
				break
			}
		}
	}
	return low
}

// resume continues a walk at slot k of the pass that started at start,
// after the linear pass found grade low present but the probe met only
// worse ones (best, bestG): it ends at the first slot of grade low or
// better, grading each Endpoint at most once. It is kept out of line so
// that its 8 KiB bitmap stays off the stack frame of every other pick.
//
//go:noinline
func (p *Picker) resume(start, k int, low Grade, g *grader, best int, bestG Grade) (int, Grade, bool) {
	var seen [resumeCap / 64]uint64
	n := p.Slots()
	last := -1
	for ; k < n; k++ {
		i := p.slot(wrap(start, k, n))
		if i == last {
			continue
		}
		last = i
		if i < resumeCap {
			word, bit := i>>6, uint64(1)<<(i&63)
			if seen[word]&bit != 0 {
				continue
			}
			seen[word] |= bit
		}
		if gr := g.grade(i); gr < bestG {
			best, bestG = i, gr
			if gr <= low {
				break
			}
		}
	}
	return best, bestG, best >= 0
}

// pickRoundRobin takes the slot at the shared cursor and walks forward past
// slots of excluded Endpoints for at most one pass (05 req 12); if the pass
// finds no Eligible Endpoint it takes the first of the lowest grade.
func (p *Picker) pickRoundRobin(g *grader) (int, Grade, bool) {
	n := len(p.sched)
	if n == 0 {
		return -1, gone, false
	}
	start := int((p.cursor.Add(1) - 1) % uint64(n)) //nolint:gosec // G115: the remainder is below n, an int.
	return p.walkFrom(start, g)
}

// ringStart returns the first virtual node at or after key, wrapping.
func (p *Picker) ringStart(key uint64) int {
	lo, hi := 0, len(p.ring)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if p.ring[m].pos < key {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo == len(p.ring) {
		return 0
	}
	return lo
}

// pickRing looks up the first position at or after key and walks forward
// past excluded Endpoints for at most one full ring (05 req 14).
func (p *Picker) pickRing(key uint64, g *grader) (int, Grade, bool) {
	if len(p.ring) == 0 {
		return -1, gone, false
	}
	return p.walkFrom(p.ringStart(key), g)
}

// draw returns a weighted random Endpoint: a uniform value in [0, Σw)
// located by binary search over the cumulative weights (05 req 15).
func (p *Picker) draw(src rand.Source) int {
	r := uint64n(src, p.total)
	lo, hi := 0, len(p.cum)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if p.cum[m] <= r {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

// pickRandom draws up to RejectionDraws weighted Endpoints and keeps the
// first Eligible one; otherwise it scans the list (05 req 15). exclude, when
// not -1, is never returned.
func (p *Picker) pickRandom(src rand.Source, g *grader, exclude int) (int, Grade, bool) {
	if p.live == 0 {
		return -1, gone, false
	}
	for range RejectionDraws {
		i := p.draw(src)
		if i != exclude && g.grade(i) == Eligible {
			return i, Eligible, true
		}
	}
	return p.scan(src, g, exclude, gone)
}

// scan is the linear fallback: a weighted pick among the Endpoints of the
// lowest grade present, or of grade want when want is not gone; exclude,
// when not -1, is skipped. Grades read in the first pass decide the target
// grade; if concurrent health changes leave the second pass short, it keeps
// the last matching Endpoint.
func (p *Picker) scan(src rand.Source, g *grader, exclude int, want Grade) (int, Grade, bool) {
	var byGrade [DownAvoided + 1]struct {
		sum   uint64
		first int
	}
	for i, x := range p.w {
		if x == 0 || i == exclude {
			continue
		}
		gr := g.grade(i)
		if gr == gone || (want != gone && gr != want) {
			continue
		}
		if byGrade[gr].sum == 0 {
			byGrade[gr].first = i
		}
		byGrade[gr].sum += uint64(x)
	}
	sum, last := uint64(0), -1
	for gr := range byGrade {
		if byGrade[gr].sum > 0 {
			want, sum, last = Grade(gr), byGrade[gr].sum, byGrade[gr].first
			break
		}
	}
	if sum == 0 {
		return -1, gone, false
	}
	r := uint64n(src, sum)
	var acc uint64
	for i, x := range p.w {
		if x == 0 || i == exclude || g.grade(i) != want {
			continue
		}
		last = i
		if acc += uint64(x); r < acc {
			return i, want, true
		}
	}
	return last, want, true
}

// pickLeastRequest draws two distinct candidates of the lowest grade by
// weighted random and keeps the lower (in-flight + recent failures) ÷
// weight, a tie going either way at random (05 req 13).
func (p *Picker) pickLeastRequest(src rand.Source, g *grader) (int, Grade, bool) {
	a, ga, ok := p.pickRandom(src, g, -1)
	if !ok || p.live == 1 {
		return a, ga, ok
	}
	b := -1
	for range RejectionDraws {
		if i := p.draw(src); i != a && g.grade(i) == ga {
			b = i
			break
		}
	}
	if b < 0 {
		if b, _, ok = p.scan(src, g, a, ga); !ok {
			return a, ga, true
		}
	}
	switch compareScores(g.load(a), p.w[a], g.load(b), p.w[b]) {
	case -1:
		return a, ga, true
	case 1:
		return b, ga, true
	}
	if src.Uint64()&1 == 0 {
		return a, ga, true
	}
	return b, ga, true
}

// compareScores compares la/wa with lb/wb without division or overflow.
func compareScores(la uint64, wa uint32, lb uint64, wb uint32) int {
	ah, al := bits.Mul64(la, uint64(wb))
	bh, bl := bits.Mul64(lb, uint64(wa))
	if c := cmp.Compare(ah, bh); c != 0 {
		return c
	}
	return cmp.Compare(al, bl)
}
