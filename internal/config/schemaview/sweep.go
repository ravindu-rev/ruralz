// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"cmp"
	"slices"
)

// The mapping's memory does not grow with the validation errors that the
// diagnostic cap drops. The memo (memo) adds a step for every new
// instance location an error has and interns a text for every new path a
// diagnostic has, but the mapper keeps at most 2*keep diagnostics
// (mapper.record) and pending secret findings at most twice those of
// keep groups (mapper.pruneSecrets), so most steps and texts are needed
// only while their error is mapped. A sweep keeps the steps and texts on
// the paths the mapper holds (its diagnostics, the bound, its pending
// secret findings and the path the caller is extending), the steps that
// were costly to resolve (a large object's dispatch or a large element's
// key-aware text, which the instance pays for once) and the steps above
// those, and drops the rest: a later error at a dropped location resolves
// it again, at the cost the first resolution had, below keepCost. A sweep
// runs when the memo has grown by more than it kept and by more than
// sweepMin since the last one, so its work is amortized over the growth,
// and the memo holds at most what the mapper's paths and the costly steps
// need plus as much again or sweepMin, whichever is more, however many
// errors there are (TestMappingMemoryBounded).
//
// Interning stays exact: a text the table drops is on no path the mapper
// holds, so a path it builds again gets a new text node that no held path
// shares, and two held paths have the same text exactly when they have
// the same node. A sweep runs only when a step or a path is added
// (memo.step, memo.child), and keeps the path that call extends; every
// other path a caller uses after such a call lies above that path (the
// uniqueness walk's frames, the reserved-prefix check's metadata path)
// or belongs to a pending secret finding.

// sweepMin is the least growth, in steps and interned texts, between two
// sweeps: about a megabyte at most, so documents with a few thousand
// errors never sweep and a sweep's fixed cost is amortized.
const sweepMin = 4096

// keepCost is the work, in members, elements and bytes read, from which a
// resolved step is costly (step.costly) and kept by every sweep: one
// yield's worth, so dropping a step can repeat at most that much work
// per error that passes it again.
const keepCost = yieldEvery

// grow counts one step or interned text added to the memo.
func (x *memo) grow() {
	x.grown++
	x.peak = max(x.peak, x.kept+x.grown)
}

// sweepIfGrown sweeps when the memo has grown by more than the last sweep
// kept and by more than minSweep (sweepMin when zero) since. pin is the
// path the caller is extending, which the sweep keeps.
func (x *memo) sweepIfGrown(pin *pathNode) {
	least := x.minSweep
	if least == 0 {
		least = sweepMin
	}
	if x.grown > max(least, x.kept) {
		x.sweep(pin)
	}
}

// sweep drops the steps and interned texts that no held path needs: it
// keeps pin and the mapper's paths (memo.roots) with every path above
// them, the costly steps and the steps above any kept step, the texts of
// every kept path and step, and the remembered common prefixes between
// kept texts. Each path marked, step visited, text kept and pair of
// common prefixes visited is a work unit.
func (x *memo) sweep(pin *pathNode) {
	x.sweeps++
	live := map[*pathNode]bool{}
	mark := func(p *pathNode) {
		for ; p != nil && !live[p]; p = p.parent {
			x.work.tick()
			live[p] = true
		}
	}
	mark(pin)
	if x.roots != nil {
		x.roots(mark)
	}
	texts := make(map[textAt]*textNode, 2*len(live))
	keepText := func(t *textNode) {
		for ; t != nil; t = t.parent {
			at := textAt{parent: t.parent, text: t.text}
			if _, ok := texts[at]; ok {
				return
			}
			x.work.tick()
			texts[at] = t
		}
	}
	steps := 0
	var keepBelow func(s *step)
	keepStep := func(c *step) bool {
		x.work.tick()
		keepBelow(c)
		if !c.costly && !live[&c.pn] && len(c.few) == 0 && len(c.many) == 0 {
			return false
		}
		steps++
		keepText(c.pn.text)
		return true
	}
	keepBelow = func(s *step) {
		var kept []*step
		for _, c := range s.few {
			if keepStep(c) {
				kept = append(kept, c)
			}
		}
		if s.many != nil {
			for _, c := range s.many {
				if keepStep(c) {
					kept = append(kept, c)
				}
			}
			// The order a scan of few meets the children in, the order
			// they were added, does not depend on map iteration.
			slices.SortFunc(kept, func(a, b *step) int { return cmp.Compare(a.seq, b.seq) })
		}
		s.few, s.many = nil, nil
		if len(kept) <= fewSteps {
			s.few = kept
			return
		}
		s.many = make(map[string]*step, len(kept))
		for _, c := range kept {
			s.many[c.tok] = c
		}
	}
	if x.top != nil {
		keepBelow(x.top)
	}
	for p := range live {
		x.work.tick()
		keepText(p.text)
	}
	held := func(t *textNode) bool { return t != nil && texts[textAt{parent: t.parent, text: t.text}] == t }
	if x.lcp != nil {
		lcp := map[textPair]int{}
		for pair, n := range x.lcp {
			x.work.tick()
			if held(pair.a) && held(pair.b) {
				lcp[pair] = n
			}
		}
		x.lcp = lcp
	}
	for _, t := range texts {
		x.work.tick()
		if t.lcp != nil && t.lcp.peer != nil && !held(t.lcp.peer) {
			t.lcp.peer, t.lcp.n = nil, 0
		}
	}
	x.texts = texts
	x.kept, x.grown = steps+len(texts), 0
}
