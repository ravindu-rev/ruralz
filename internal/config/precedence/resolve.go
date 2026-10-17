// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// attachKey names one Policy attached at one scope. Phase selection
// depends on the Policy and the scope only, so it runs once per key
// however many Routes attach the Policy.
type attachKey struct {
	policy string
	scope  phase.Scope
}

// selection is the outcome of Phase selection for one attachKey.
type selection struct {
	// missing is set when the Plugin a plugin Policy names does not
	// resolve: the attachment is skipped silently (RZ-CFG-009 is stage
	// H's, 02 req 40).
	missing bool
	// scopeErr is the registry's RZ-CFG-020 finding.
	scopeErr *registry.ScopeError
	// err is a programming error; Resolve returns the first one.
	err error
	// entry is the per-Policy part of every hub.Entry of this key.
	entry hub.Entry
	// slot is the interned id of entry.Slot (state.slotID), set by
	// selectAll.
	slot int
	// res is the Policy resource, for the "declared in" location.
	res *hub.Resource
}

// attached is one valid entry of one spec.policies list, with the
// interned id of its slot.
type attached struct {
	entry hub.Entry
	slot  int
	res   *hub.Resource
}

// list is one checked spec.policies list: its valid entries in authored
// order, one per slot, with indexes by interned slot id and Policy name.
// The slot index never hashes or compares slot text, so checking a list
// costs the same whatever the length of its Policies' slots, which no
// schema rule bounds (WP-38 S2 round-4 blocker).
type list struct {
	items  []attached
	bySlot map[int]int
	// byName maps the name of every entry the list checked (the first of
	// a name listed twice) to its index in items, or to -1 for an entry
	// rejected with RZ-CFG-020.
	byName map[string]int
}

// legTemplate is the upstream-leg chain of one Upstream; Routes share its
// slices.
type legTemplate struct {
	phases [phase.Count][]hub.Entry
}

// sharedPhase is the content of one client Phase that several Routes
// share: the Gateway's entries of the Phase (base), possibly minus the
// entries some Routes removed (phaseMemo). Its slice has a capacity equal
// to its length.
type sharedPhase struct {
	entries []hub.Entry
	// bypass is the index of the first entry bypassedByCache in an
	// onRequestBody Phase, or -1; the cache guardrail reads it without a
	// per-Route scan.
	bypass int
}

// newSharedPhase returns the entries of Phase p as a sharedPhase. In
// onRequestBody it scans them for the first bypassedByCache, each entry
// scanned a unit of w as it is read; the scan stops at that entry.
func newSharedPhase(w *worker, p phase.Phase, entries []hub.Entry) *sharedPhase {
	sp := &sharedPhase{entries: entries, bypass: -1}
	if p != phase.OnRequestBody {
		return sp
	}
	for i := range entries {
		w.charge(siteShared, 1)
		if bypassedByCache(entries[i]) {
			sp.bypass = i
			break
		}
	}
	return sp
}

// phaseKey names the content of one client Phase that only removals
// changed: the Phase and the Gateway list positions removed from it, in
// increasing order, encoded as text.
type phaseKey struct {
	p       phase.Phase
	removed string
}

// phaseMemo builds each removal-only client Phase once per Resolve call,
// however many Routes remove the same Gateway Policies from it. Workers
// share it: the map is guarded by mu and each slot by its own lock, so
// distinct slots build in parallel.
type phaseMemo struct {
	mu    sync.Mutex
	slots map[phaseKey]*memoSlot
}

// memoSlot is one phaseMemo entry; mu guards sp, which is nil until built.
type memoSlot struct {
	mu sync.Mutex
	sp *sharedPhase
}

// get returns the slot of k, building it with build on first use. A build
// that panics leaves the slot unbuilt, so a later caller builds it again
// rather than reading a nil value.
func (m *phaseMemo) get(k phaseKey, build func() *sharedPhase) *sharedPhase {
	m.mu.Lock()
	slot, ok := m.slots[k]
	if !ok {
		slot = &memoSlot{}
		m.slots[k] = slot
	}
	m.mu.Unlock()
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.sp == nil {
		slot.sp = build()
	}
	return slot.sp
}

// state is the per-call working set of Resolve. Fields are written by the
// sequential steps and the index-owned worker slots, then only read; memo
// and out are the structures workers write together, each under its own
// lock.
type state struct {
	r    *Resolver
	b    *hub.Bundle
	loc  locator
	pool pool
	// out collects the findings of every worker (01 req 50).
	out *collector

	gatewayRes *hub.Resource
	gatewayObj *v1alpha1.Gateway
	routes     []*hub.Resource
	routeObjs  []*v1alpha1.Route

	// keys are the attachKeys whose Policy resolves, in first-seen order,
	// with the Policy resources in keyRes; sels holds their selections.
	// A reference to a Policy that does not resolve takes no key, so the
	// working set never grows with the references stage I skips (02 req
	// 40).
	keys   []attachKey
	keyRes []*hub.Resource
	sels   map[attachKey]*selection
	// slotIDs interns every slot of sels: selectAll reads each slot's
	// text once per attachKey, and every later comparison of slots is one
	// of ids.
	slotIDs map[string]int

	gateway list
	// base holds, per client Phase, the Gateway list's entries in
	// execution order: the Phase of every Route that changes nothing in
	// it.
	base [phase.Count]*sharedPhase
	// gatewayCache indexes the cache entries of gateway.items.
	gatewayCache []int
	memo         phaseMemo

	upstreamOrder []string
	upstreamRes   []*hub.Resource
	upstreamObjs  []*v1alpha1.Upstream
	upstreams     map[string]*legTemplate

	// chains and found are the result: the chain of every Route, and the
	// first findings in compareKept order.
	chains map[string]*hub.Chain
	found  []kept
}

// object returns the typed view of res when res.Object is a non-nil *T.
// A resource without one (a nil Object, a typed nil or another type,
// which stage G never produces) is treated as absent, like an unresolved
// reference (02 req 40): no step of stage I dereferences it.
func object[T any](res *hub.Resource) (*T, bool) {
	t, ok := hub.Object[T](res)
	return t, ok && t != nil
}

// newState indexes b for one call on at most workers goroutines.
func newState(r *Resolver, b *hub.Bundle, files *tree.FileTable, workers int) *state {
	workers = max(1, workers)
	out := newCollector(keepOf(r.maxDiagnostics), workers)
	s := &state{
		r: r, b: b, loc: locator{files: files}, out: out, sels: make(map[attachKey]*selection), slotIDs: make(map[string]int),
		upstreams: make(map[string]*legTemplate), memo: phaseMemo{slots: make(map[phaseKey]*memoSlot)},
		pool: pool{size: workers, newWorker: func() *worker { return r.newWorker(out) }},
	}
	w := s.pool.worker(0)
	for _, res := range b.Resources() {
		w.charge(siteResource, 1)
		switch res.Kind {
		case v1alpha1.KindGateway:
			if g, ok := object[v1alpha1.Gateway](res); ok && s.gatewayRes == nil {
				s.gatewayRes, s.gatewayObj = res, g
				s.addKeys(w, g.Spec.Policies, phase.ScopeGateway)
			}
		case v1alpha1.KindUpstream:
			if u, ok := object[v1alpha1.Upstream](res); ok {
				s.upstreamOrder = append(s.upstreamOrder, res.Name)
				s.upstreamRes, s.upstreamObjs = append(s.upstreamRes, res), append(s.upstreamObjs, u)
				s.addKeys(w, u.Spec.Policies, phase.ScopeUpstream)
			}
		case v1alpha1.KindRoute:
			if rt, ok := object[v1alpha1.Route](res); ok {
				s.routes, s.routeObjs = append(s.routes, res), append(s.routeObjs, rt)
				s.addKeys(w, rt.Spec.Policies, phase.ScopeRoute)
			}
		default:
		}
	}
	return s
}

// addKeys records, in first-seen order, the attachKeys of refs whose
// Policy resolves to a typed Policy; a reference longer than maxRefBytes
// is not looked up.
func (s *state) addKeys(w *worker, refs []v1alpha1.PolicyRef, scope phase.Scope) {
	for _, ref := range refs {
		w.charge(siteKey, 1)
		if !w.resolvable(siteLongKey, ref.Name) {
			continue
		}
		k := attachKey{policy: ref.Name, scope: scope}
		if _, seen := s.sels[k]; seen {
			continue
		}
		res, _ := s.b.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: ref.Name})
		if _, ok := object[v1alpha1.Policy](res); !ok {
			continue
		}
		s.sels[k] = nil
		s.keys, s.keyRes = append(s.keys, k), append(s.keyRes, res)
	}
}

// run resolves every chain and collects every worker's findings.
func (s *state) run(ctx context.Context) error {
	if err := s.selectAll(ctx); err != nil {
		return err
	}
	s.attachGateway()
	if err := s.attachUpstreams(ctx); err != nil {
		return err
	}
	results := make([]*hub.Chain, len(s.routes))
	err := s.pool.forEach(ctx, len(s.routes), func(w *worker, i int) {
		w.begin(groupRoutes, i)
		results[i] = s.route(w, s.routes[i], s.routeObjs[i])
	})
	if err != nil {
		return err
	}
	w := s.pool.worker(0)
	s.chains = make(map[string]*hub.Chain, len(results))
	for _, c := range results {
		w.charge(siteChain, 1)
		s.chains[c.Route] = c
	}
	s.found = s.out.sorted(w)
	return nil
}

// selectAll runs Phase selection for every attachKey on the worker pool
// and returns the first programming error in key order. It interns the
// slot of every selected entry on worker 0, once per attachKey, so no
// later step reads slot text: a Policy's slot is as long as its author
// made it, and every Route attaching the Policy would otherwise hash it
// again (WP-38 S2 round-4 blocker).
func (s *state) selectAll(ctx context.Context) error {
	out := make([]selection, len(s.keys))
	if err := s.pool.forEach(ctx, len(s.keys), func(w *worker, i int) { out[i] = s.selectOne(w, s.keys[i], s.keyRes[i]) }); err != nil {
		return err
	}
	w := s.pool.worker(0)
	for i, k := range s.keys {
		w.charge(siteSelected, 1)
		if out[i].err != nil {
			return out[i].err
		}
		if !out[i].missing && out[i].scopeErr == nil {
			out[i].slot = s.slotID(w, out[i].entry.Slot)
		}
		s.sels[k] = &out[i]
	}
	return nil
}

// slotID returns the id of slot, interning it on first use. Reading the
// slot's text (to hash it, and to compare it on a hit) is one unit of w
// per byteUnit bytes, charged before the read.
func (s *state) slotID(w *worker, slot string) int {
	w.charge(siteSlotIntern, 1+len(slot)/byteUnit)
	id, ok := s.slotIDs[slot]
	if !ok {
		id = len(s.slotIDs)
		s.slotIDs[slot] = id
	}
	return id
}

// selectOne asks the registry for the Phases of the Policy res at k's
// scope on worker w. A plugin Policy whose Plugin does not resolve, or
// has no typed view, is missing.
func (s *state) selectOne(w *worker, k attachKey, res *hub.Resource) selection {
	p, _ := object[v1alpha1.Policy](res)
	e, ok := s.r.reg.Lookup(p.Spec.Type)
	if !ok {
		return selection{err: fmt.Errorf("precedence: Policy %q: %w %q", k.policy, registry.ErrUnknownType, p.Spec.Type)}
	}
	var pluginPhases []v1alpha1.Phase
	if e.PhasesFromPlugin {
		if !w.resolvable(siteLongPlugin, p.Spec.Plugin) {
			return selection{missing: true}
		}
		plRes, _ := s.b.Get(hub.ID{Kind: v1alpha1.KindPlugin, Name: p.Spec.Plugin})
		pl, ok := object[v1alpha1.Plugin](plRes)
		if !ok {
			return selection{missing: true}
		}
		pluginPhases = pl.Spec.Phases
	}
	set, err := s.r.reg.Phases(registry.Attachment{
		Policy: p, Config: res.Config, Scope: k.scope, PluginPhases: pluginPhases,
	}, s.r.body)
	if err != nil {
		var se *registry.ScopeError
		if errors.As(err, &se) {
			return selection{scopeErr: se, res: res}
		}
		return selection{err: fmt.Errorf("precedence: Policy %q at %s scope: %w", k.policy, k.scope, err)}
	}
	return selection{res: res, entry: hub.Entry{
		Policy:      p.Metadata.Name,
		Type:        p.Spec.Type,
		Class:       e.EffectiveClass(p),
		Slot:        e.EffectiveSlot(p),
		Scope:       k.scope,
		FailureMode: e.EffectiveFailureMode(p),
		Overridable: p.Spec.Overridable == nil || *p.Spec.Overridable,
		Phases:      set,
	}}
}

// attachList checks one spec.policies list of owner at scope on worker w:
// RZ-CFG-020 for a disallowed scope and RZ-CFG-018 for a slot already
// taken in the list (02 req 35.3 and 35.4), both offered to w. Both drop
// the entry from the chain, so one mistake yields one finding: an entry
// rejected with RZ-CFG-020 holds no slot, so a later entry in its slot is
// no RZ-CFG-018 (TestScopeErrorHoldsNoSlot), and an entry naming a Policy
// an earlier entry of the list already names is skipped before both
// checks, because it is stage G's RZ-CFG-005 duplicate (the list is an
// orderedMap keyed by name, 01 req 34), and RZ-CFG-018 is for two
// distinct Policies (01 req 39). RZ-CFG-018 carries the location of the
// first entry in the slot as a related "first entry at" location when it
// is known (01 req 39: second ref, first ref related). Its cost is linear
// in the list's length and independent of the length of the slots and of
// the references: slots compare by interned id, RZ-CFG-018 quotes at most
// maxQuoted code points of the slot (clip), and a reference longer than
// maxRefBytes is skipped unread.
func (s *state) attachList(w *worker, owner *hub.Resource, scope phase.Scope, refs []v1alpha1.PolicyRef) list {
	l := list{items: make([]attached, 0, len(refs)), bySlot: make(map[int]int, len(refs)), byName: make(map[string]int, len(refs))}
	at := s.loc.list(w, owner, fieldPolicies)
	for i, ref := range refs {
		w.charge(siteListEntry, 1)
		if !w.resolvable(siteLongList, ref.Name) {
			continue
		}
		sel := s.sels[attachKey{policy: ref.Name, scope: scope}]
		if sel == nil || sel.missing {
			continue
		}
		if _, again := l.byName[ref.Name]; again {
			continue
		}
		loc := at.entry(i, ref.Name)
		if sel.scopeErr != nil {
			l.byName[ref.Name] = -1
			if w.admits(loc, CodeScope) {
				d := sel.scopeErr.Diagnostic(entryPath(fieldPolicies, ref.Name))
				d.Resource, d.Location = owner.ResourceID(), loc
				w.offer(d)
			}
			continue
		}
		e := sel.entry
		e.Position, e.Ref = i, loc
		if j, dup := l.bySlot[sel.slot]; dup {
			if w.admits(loc, CodeSlot) {
				first := l.items[j].entry
				d := diag.Diagnostic{
					Code: CodeSlot, Severity: diag.SeverityError, Location: loc, Resource: owner.ResourceID(),
					Path:    entryPath(fieldPolicies, ref.Name),
					Message: fmt.Sprintf("Policies %q and %q share slot %q at %s scope", first.Policy, e.Policy, clip(e.Slot), scope),
				}
				if first.Ref.File != "" {
					d.Related = []diag.Related{{Location: first.Ref, Message: "first entry at"}}
				}
				w.offer(d)
			}
			l.byName[ref.Name] = -1
			continue
		}
		l.bySlot[sel.slot] = len(l.items)
		l.byName[ref.Name] = len(l.items)
		l.items = append(l.items, attached{entry: e, slot: sel.slot, res: sel.res})
	}
	return l
}

// attachGateway checks the Gateway list once and builds the client Phases
// every Route starts from, on worker 0.
func (s *state) attachGateway() {
	if s.gatewayObj == nil {
		return
	}
	w := s.pool.worker(0)
	w.begin(groupGateway, 0)
	s.gateway = s.attachList(w, s.gatewayRes, phase.ScopeGateway, s.gatewayObj.Spec.Policies)
	w.flush()
	entries := make([]hub.Entry, len(s.gateway.items))
	for j, it := range s.gateway.items {
		w.charge(siteGatewayItem, 1)
		entries[j] = it.entry
		if it.entry.Type == v1alpha1.PolicyTypeCache {
			s.gatewayCache = append(s.gatewayCache, j)
		}
	}
	var phases [phase.Count][]hub.Entry
	fill(w, &phases, entries)
	for p := range phase.Count {
		if len(phases[p]) > 0 {
			s.base[p] = newSharedPhase(w, p, phases[p])
		}
	}
}

// attachUpstreams checks every Upstream list once and builds its leg.
func (s *state) attachUpstreams(ctx context.Context) error {
	legs := make([]legTemplate, len(s.upstreamRes))
	err := s.pool.forEach(ctx, len(s.upstreamRes), func(w *worker, i int) {
		w.begin(groupUpstreams, i)
		l := s.attachList(w, s.upstreamRes[i], phase.ScopeUpstream, s.upstreamObjs[i].Spec.Policies)
		entries := make([]hub.Entry, len(l.items))
		for j, it := range l.items {
			w.charge(siteLegItem, 1)
			entries[j] = it.entry
		}
		fill(w, &legs[i].phases, entries)
	})
	if err != nil {
		return err
	}
	w := s.pool.worker(0)
	for i, name := range s.upstreamOrder {
		w.charge(siteUpstream, 1)
		s.upstreams[name] = &legs[i]
	}
	return nil
}

// removal is the per-Route fate of one Gateway list entry.
type removal struct {
	reason hub.RemovalReason
	by     string
}

// route computes the chain of the Route res, whose typed view is rt, on
// worker w, which offers its findings (02 req 35, 39, 41). Its cost is
// linear in the Route's own lists, its legs and the Gateway entries of
// each client Phase whose content it does not share, plus k log k for k
// removals: a Phase it adds a Policy to holds its own copy of that
// Phase's remaining Gateway entries, and a Phase it changes only by
// removals shares one copy with every Route removing the same Gateway
// Policies from it (phaseMemo), so a Route whose removal set no other
// Route has pays O(Gateway Policies) for that Phase too (see Resolve).
func (s *state) route(w *worker, res *hub.Resource, rt *v1alpha1.Route) *hub.Chain {
	c := &hub.Chain{Route: res.Name}
	gw := s.gateway.items
	// fate holds the removed Gateway entries by index into gw, and removed
	// those indexes as they were first removed; both stay nil for a Route
	// that removes none.
	var fate map[int]removal
	var removed []int
	remove := func(j int, r removal) {
		if fate == nil {
			fate = make(map[int]removal)
		}
		if _, again := fate[j]; !again {
			w.charge(siteRemoved, 1)
			removed = append(removed, j)
		}
		fate[j] = r
	}

	// Rule 1: Gateway policies minus excludePolicies. A name listed twice
	// is stage G's RZ-CFG-005 duplicate (spec.excludePolicies is a map
	// keyed by name, 01 req 34): its second entry sets the fate the first
	// did, and raises no second RZ-CFG-019.
	var excluded *listLoc
	var lockedSeen map[int]struct{}
	for i, ex := range rt.Spec.ExcludePolicies {
		w.charge(siteExclude, 1)
		if !w.resolvable(siteLongExclude, ex.Name) {
			continue
		}
		j, ok := s.gateway.byName[ex.Name]
		if !ok || j < 0 {
			continue
		}
		if g := gw[j]; !g.entry.Overridable {
			if _, again := lockedSeen[j]; again {
				continue
			}
			if lockedSeen == nil {
				lockedSeen = make(map[int]struct{})
			}
			lockedSeen[j] = struct{}{}
			if excluded == nil {
				excluded = s.loc.list(w, res, fieldExclude)
			}
			s.notOverridable(w, res, excluded.entry(i, ex.Name), entryPath(fieldExclude, ex.Name), g,
				func() string { return "cannot exclude a Policy with overridable: false" })
			continue
		}
		remove(j, removal{reason: hub.Excluded})
	}

	// Rule 2: Route policies replace same-slot Gateway policies.
	own := s.attachList(w, res, phase.ScopeRoute, rt.Spec.Policies)
	routeEntries := own.items[:0:0]
	for _, it := range own.items {
		w.charge(siteKept, 1)
		if j, ok := s.gateway.bySlot[it.slot]; ok && fate[j].reason == 0 {
			g := gw[j]
			if !g.entry.Overridable {
				s.notOverridable(w, res, it.entry.Ref, entryPath(fieldPolicies, it.entry.Policy), g, func() string {
					return fmt.Sprintf("cannot replace Gateway Policy %q in slot %q: it has overridable: false", g.entry.Policy, clip(g.entry.Slot))
				})
				continue
			}
			remove(j, removal{reason: hub.Replaced, by: it.entry.Policy})
			it.entry.Replaces = g.entry.Policy
		}
		routeEntries = append(routeEntries, it)
	}

	// Client leg: class, scope, position; response Phases reversed.
	heapSort(w, siteRemovedSort, removed, func(a, b *int) int { return cmp.Compare(*a, *b) })
	bypass := s.client(w, c, removed, routeEntries)
	s.cacheGuardrail(w, res, bypass, fate, routeEntries)

	for _, name := range legOrder(w, rt) {
		w.charge(siteLeg, 1)
		if t, ok := s.upstreams[name]; ok {
			c.Legs = append(c.Legs, hub.Leg{Upstream: name, Phases: t.phases})
		}
	}
	// Removed: replaced before excluded, each in Gateway list order.
	if len(removed) > 0 {
		c.Removed = make([]hub.Removed, 0, len(removed))
	}
	for _, reason := range []hub.RemovalReason{hub.Replaced, hub.Excluded} {
		for _, j := range removed {
			w.charge(siteRemovedList, 1)
			if f := fate[j]; f.reason == reason {
				g := gw[j].entry
				c.Removed = append(c.Removed, hub.Removed{Policy: g.Policy, Slot: g.Slot, By: f.by, Reason: f.reason})
			}
		}
	}
	return c
}

// client fills the client Phases of c from the Gateway entries (minus
// removed, the removed indexes into gateway.items in increasing order) and
// routeEntries, the Route's entries that stay in its chain, and returns
// the onRequestBody entry the cache guardrail checks, if any. A Phase no
// Route entry runs in is shared (see Resolve): the Gateway's slice when
// nothing was removed from it, else the memoized slice of its removals,
// built by the first Route that needs it. Every other Phase is built for
// the Route, all of them in one exactly sized backing array, by merging
// the Gateway entries (already in the Phase's execution order) with the
// Route entries taken in class order: no comparison sort, so each step is
// a unit of w as it is done.
func (s *state) client(w *worker, c *hub.Chain, removed []int, routeEntries []attached) *hub.Entry {
	gw := s.gateway.items
	var keptIn [phase.Count]int
	var classStart [phase.NumClasses + 1]int
	for _, it := range routeEntries {
		w.charge(siteKeptIn, 1)
		classStart[it.entry.Class+1]++
		for p := range phase.Count {
			if it.entry.Phases.Has(p) {
				keptIn[p]++
			}
		}
	}
	var removedPos map[int]bool
	if len(removed) > 0 {
		removedPos = make(map[int]bool, len(removed))
		for _, j := range removed {
			w.charge(siteRemovedPos, 1)
			removedPos[gw[j].entry.Position] = true
		}
	}
	var bypass *hub.Entry
	var sizes [phase.Count]int
	total := 0
	for p := range phase.Count {
		base := s.base[p]
		if keptIn[p] > 0 {
			n := keptIn[p]
			if base != nil {
				n += len(base.entries) - removedIn(w, gw, removed, p)
			}
			sizes[p] = n
			total += n
			continue
		}
		if base == nil {
			continue
		}
		sp := base
		if key, changed := removalKey(w, gw, removed, p); changed {
			sp = s.memo.get(phaseKey{p: p, removed: key}, func() *sharedPhase {
				return newSharedPhase(w, p, without(w, base.entries, removedPos))
			})
		}
		if len(sp.entries) > 0 {
			c.Client[p] = sp.entries
		}
		if sp.bypass >= 0 {
			bypass = &sp.entries[sp.bypass]
		}
	}
	if total == 0 {
		return bypass
	}
	// The Route entries in class order, each class in list order: a
	// counting placement (classStart holds each class's start).
	for k := 1; k < len(classStart); k++ {
		classStart[k] += classStart[k-1]
	}
	next := classStart
	order := make([]int, len(routeEntries))
	for i, it := range routeEntries {
		w.charge(siteKeptOrder, 1)
		order[next[it.entry.Class]] = i
		next[it.entry.Class]++
	}
	buf := make([]hub.Entry, total)
	off := 0
	for p := range phase.Count {
		n := sizes[p]
		if n == 0 {
			continue
		}
		l := buf[off : off : off+n]
		off += n
		var base []hub.Entry
		if s.base[p] != nil {
			base = s.base[p].entries
		}
		l, first := mergePhase(w, p, l, base, removedPos, routeEntries, order, &classStart)
		c.Client[p] = l
		if first >= 0 {
			bypass = &l[first]
		}
	}
	return bypass
}

// mergePhase appends to l the entries of Phase p in execution order: by
// Filter class, then scope, then position (foundation pack 8.12 rule 5),
// reversed in a response Phase (onUpstreamResponseHeaders,
// onUpstreamResponseBody, onResponse and onChunk, spec 02 R-12). base is
// the Gateway's entries of p already in that order, of which those whose
// Position is in removed are left out; the Route entries come from
// routeEntries through order, their indexes in class order (class c from
// classStart[c] to classStart[c+1]), each class in list order. Within a
// class Gateway entries run before Route entries in request order and
// after them in reverse. Each Gateway entry read and each Route entry
// considered is a unit of w as it is read. In onRequestBody it also
// returns the index of the first entry bypassedByCache, which the cache
// guardrail reads, found as the entries are appended; else, or without
// one, -1.
func mergePhase(w *worker, p phase.Phase, l, base []hub.Entry, removed map[int]bool, routeEntries []attached, order []int, classStart *[phase.NumClasses + 1]int) ([]hub.Entry, int) {
	first := -1
	add := func(e hub.Entry) {
		if first < 0 && p == phase.OnRequestBody && bypassedByCache(e) {
			first = len(l)
		}
		l = append(l, e)
	}
	bi := 0
	takeBase := func(c phase.Class) {
		for bi < len(base) && base[bi].Class == c {
			w.charge(siteCopyBase, 1)
			if !removed[base[bi].Position] {
				add(base[bi])
			}
			bi++
		}
	}
	takeRoute := func(c phase.Class, reverse bool) {
		lo, hi := classStart[c], classStart[c+1]
		for k := range hi - lo {
			idx := lo + k
			if reverse {
				idx = hi - 1 - k
			}
			w.charge(siteCopyKept, 1)
			if e := routeEntries[order[idx]].entry; e.Phases.Has(p) {
				add(e)
			}
		}
	}
	if !p.IsResponse() {
		for c := range phase.NumClasses {
			takeBase(c)
			takeRoute(c, false)
		}
		return l, first
	}
	for c := phase.NumClasses - 1; ; c-- {
		takeRoute(c, true)
		takeBase(c)
		if c == 0 {
			return l, first
		}
	}
}

// removedIn counts the entries of removed (indexes into gw) that run in
// p; each is a unit of w.
func removedIn(w *worker, gw []attached, removed []int, p phase.Phase) int {
	n := 0
	for _, j := range removed {
		w.charge(siteRemovedIn, 1)
		if gw[j].entry.Phases.Has(p) {
			n++
		}
	}
	return n
}

// removalKey returns the Gateway list positions of the entries of removed
// (indexes into gw, increasing) that run in p, encoded as text, and
// whether there is any; each entry is a unit of w.
func removalKey(w *worker, gw []attached, removed []int, p phase.Phase) (string, bool) {
	var key []byte
	for _, j := range removed {
		w.charge(siteRemovalKey, 1)
		if e := gw[j].entry; e.Phases.Has(p) {
			key = strconv.AppendInt(append(key, ','), int64(e.Position), 10)
		}
	}
	return string(key), len(key) > 0
}

// without returns the entries whose Position is not in removed, in order,
// in a slice of capacity equal to its length; each entry read is a unit
// of w.
func without(w *worker, entries []hub.Entry, removed map[int]bool) []hub.Entry {
	n := 0
	for _, e := range entries {
		w.charge(siteWithoutCount, 1)
		if !removed[e.Position] {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	out := make([]hub.Entry, 0, n)
	for _, e := range entries {
		w.charge(siteWithoutCopy, 1)
		if !removed[e.Position] {
			out = append(out, e)
		}
	}
	return out
}

// notOverridable offers to w the RZ-CFG-019 finding at path of the Route
// res, with the position where the Gateway Policy g declares
// overridable: false as a related "declared in" location when it is
// known. The message and the related location are built only when w may
// keep the finding.
func (s *state) notOverridable(w *worker, res *hub.Resource, at diag.Location, path diag.Path, g attached, msg func() string) {
	if !w.admits(at, CodeOverridable) {
		return
	}
	d := diag.Diagnostic{
		Code: CodeOverridable, Severity: diag.SeverityError, Location: at, Resource: res.ResourceID(), Path: path, Message: msg(),
	}
	if decl := s.loc.path(w, g.res, diag.Path{diag.Field("spec"), diag.Field("overridable")}, true); decl.File != "" {
		d.Related = []diag.Related{{Location: decl, Message: "declared in"}}
	}
	w.offer(d)
}

// cacheGuardrail offers to w RZ-CFG-038 at each cache attachment of a
// chain that also runs an authz or validation Policy, or a plugin auth or
// authz Policy, in onRequestBody: a cache hit skips onRequestBody, so it
// would bypass them (02 req 39, OQ-traffic-management-and-resilience-21
// (a)). x is the first such onRequestBody entry, or nil; the chain's
// cache entries are the Gateway's that fate did not remove, then those of
// routeEntries, the Route's entries that stay in its chain.
func (s *state) cacheGuardrail(w *worker, res *hub.Resource, x *hub.Entry, fate map[int]removal, routeEntries []attached) {
	if x == nil {
		return
	}
	add := func(owner *hub.Resource, e hub.Entry) {
		if !w.admits(e.Ref, CodeCacheGuardrail) {
			return
		}
		w.offer(diag.Diagnostic{
			Code: CodeCacheGuardrail, Severity: diag.SeverityError, Location: e.Ref, Resource: owner.ResourceID(),
			Path: entryPath(fieldPolicies, e.Policy),
			Message: fmt.Sprintf("Response Cache Policy %q is combined with Policy %q (%s, Filter class %s), which runs in onRequestBody: a cache hit would skip it",
				e.Policy, x.Policy, x.Type, x.Class),
		})
	}
	for _, j := range s.gatewayCache {
		w.charge(siteGuardrailGateway, 1)
		if _, gone := fate[j]; !gone {
			add(s.gatewayRes, s.gateway.items[j].entry)
		}
	}
	for _, it := range routeEntries {
		w.charge(siteGuardrailKept, 1)
		if it.entry.Type == v1alpha1.PolicyTypeCache {
			add(res, it.entry)
		}
	}
}

// bypassedByCache reports whether an onRequestBody entry may not share a
// chain with a cache Policy.
func bypassedByCache(e hub.Entry) bool {
	switch e.Class {
	case phase.ClassAuthz, phase.ClassValidation:
		return true
	case phase.ClassAuth:
		return e.Type == v1alpha1.PolicyTypePlugin
	default:
		return false
	}
}

// legOrder returns the Upstream names of rt in leg order (02 req 41):
// spec.upstreams by name, as stage G's map sorting leaves them, then
// spec.composition.steps in authored order; each name once. A name longer
// than maxRefBytes names no Upstream and is left out unread. Each name
// read or compared is a unit of w.
func legOrder(w *worker, rt *v1alpha1.Route) []string {
	steps := 0
	if rt.Spec.Composition != nil {
		steps = len(rt.Spec.Composition.Steps)
	}
	names := make([]string, 0, len(rt.Spec.Upstreams)+steps)
	for _, u := range rt.Spec.Upstreams {
		w.charge(siteLegUpstream, 1)
		if w.resolvable(siteLongLeg, u.Name) {
			names = append(names, u.Name)
		}
	}
	heapSort(w, siteLegSort, names, func(a, b *string) int { return cmp.Compare(*a, *b) })
	if rt.Spec.Composition != nil {
		for _, st := range rt.Spec.Composition.Steps {
			w.charge(siteLegStep, 1)
			if w.resolvable(siteLongLeg, st.Upstream) {
				names = append(names, st.Upstream)
			}
		}
	}
	seen := make(map[string]struct{}, len(names))
	out := names[:0]
	for _, n := range names {
		w.charge(siteLegDedupe, 1)
		if _, dup := seen[n]; !dup {
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}

// fill puts every entry of one list (one scope, in position order) into
// each Phase it runs in, in execution order: by Filter class, then
// position, reversed in a response Phase (foundation pack 8.12 rule 5,
// spec 02 R-12). It places them by counting instead of sorting: each
// entry goes straight to its slot among the entries of its class, so
// every step is a unit of w as it is done. All Phases share one exactly
// sized backing array, and a Phase without entries stays nil.
func fill(w *worker, phases *[phase.Count][]hub.Entry, entries []hub.Entry) {
	var counts [phase.Count][phase.NumClasses]int
	var sizes [phase.Count]int
	total := 0
	for i := range entries {
		w.charge(siteFillCount, 1)
		for p := range phase.Count {
			if entries[i].Phases.Has(p) {
				counts[p][entries[i].Class]++
				sizes[p]++
				total++
			}
		}
	}
	if total == 0 {
		return
	}
	buf := make([]hub.Entry, total)
	var next [phase.Count][phase.NumClasses]int
	off := 0
	for p := range phase.Count {
		if sizes[p] == 0 {
			continue
		}
		phases[p] = buf[off : off+sizes[p] : off+sizes[p]]
		off += sizes[p]
		at := 0
		for c := range phase.NumClasses {
			next[p][c] = at
			at += counts[p][c]
		}
	}
	for i := range entries {
		e := &entries[i]
		for p := range phase.Count {
			if !e.Phases.Has(p) {
				continue
			}
			w.charge(siteFillPlace, 1)
			k := next[p][e.Class]
			next[p][e.Class]++
			if p.IsResponse() {
				k = sizes[p] - 1 - k
			}
			phases[p][k] = *e
		}
	}
}

// List fields that attach or exclude Policies.
const (
	fieldPolicies = "policies"
	fieldExclude  = "excludePolicies"
)

// entryPath returns spec.<field>[name=<name>].
func entryPath(field, name string) diag.Path {
	return diag.Path{diag.Field("spec"), diag.Field(field), diag.Keyed("name", name)}
}

// maxQuoted bounds the code points of a slot quoted in a message, as the
// messages of stages B, F and G bound the authored values they quote.
const maxQuoted = 64

// clip shortens a slot for a message: its first maxQuoted code points (an
// invalid UTF-8 byte counting as one) and "...", or s itself when it has
// no more. No schema rule bounds spec.slot, and a finding per Route that
// copied the whole slot would cost its length per finding (WP-38 S2
// round-4 blocker). It keeps the bytes of s as they are: every message
// quotes the slot with %q, which escapes an invalid byte the same way
// whether the slot was clipped or not. It reads at most maxQuoted+1 code
// points of s and allocates only the result.
func clip(s string) string {
	n := 0
	for i := range s {
		if n == maxQuoted {
			return s[:i] + "..."
		}
		n++
	}
	return s
}
