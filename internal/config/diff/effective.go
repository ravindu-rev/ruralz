// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"context"
	"maps"
	"slices"
	"sort"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// effective lists the effective Filter Chain changes of every Route either
// side holds, in Route name order. A Route only one side holds has every
// Policy of its chain added or removed.
func effective(ctx context.Context, from, to map[string]*hub.Chain, workers int) ([]ChainChange, error) {
	names := map[string]bool{}
	for r := range from {
		names[r] = true
	}
	for r := range to {
		names[r] = true
	}
	routes := slices.Sorted(maps.Keys(names))
	per := make([][]ChainChange, len(routes))
	err := run(ctx, len(routes), workers, func(_, i int) error {
		per[i] = chainChanges(routes[i], from[routes[i]], to[routes[i]])
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []ChainChange
	for _, cs := range per {
		out = append(out, cs...)
	}
	return out, nil
}

// chainChanges compares one Route's chains (either may be nil): Phase by
// Phase in execution order, the client leg first, then each upstream leg
// by Upstream name.
func chainChanges(route string, a, b *hub.Chain) []ChainChange {
	legs := map[string]bool{}
	for _, c := range []*hub.Chain{a, b} {
		if c != nil {
			for _, l := range c.Legs {
				legs[l.Upstream] = true
			}
		}
	}
	names := slices.Sorted(maps.Keys(legs))
	var out []ChainChange
	for p := range phase.Count {
		out = entryChanges(out, route, p, "", clientPhase(a, p), clientPhase(b, p))
		for _, leg := range names {
			out = entryChanges(out, route, p, leg, legPhase(a, leg, p), legPhase(b, leg, p))
		}
	}
	return out
}

// clientPhase returns the client-leg entries of c in Phase p.
func clientPhase(c *hub.Chain, p phase.Phase) []hub.Entry {
	if c == nil {
		return nil
	}
	return c.Client[p]
}

// legPhase returns the entries of c's leg to upstream in Phase p.
func legPhase(c *hub.Chain, upstream string, p phase.Phase) []hub.Entry {
	if c == nil {
		return nil
	}
	if l, ok := c.Leg(upstream); ok {
		return l.Phases[p]
	}
	return nil
}

// entryKey identifies a chain entry: its Policy and, should a Policy run
// twice in one Phase, which occurrence it is.
type entryKey struct {
	policy string
	nth    int
}

// keyEntries returns the key of every entry and each key's position.
func keyEntries(es []hub.Entry) ([]entryKey, map[entryKey]int) {
	keys := make([]entryKey, len(es))
	at := make(map[entryKey]int, len(es))
	seen := map[string]int{}
	for i, e := range es {
		keys[i] = entryKey{policy: e.Policy, nth: seen[e.Policy]}
		seen[e.Policy]++
		at[keys[i]] = i
	}
	return keys, at
}

// entryChanges appends the changes from the entries a to b of one Phase of
// one leg: Policies only b runs are added and Policies only a runs are
// removed; of the Policies both run, the fewest whose moving restores b's
// order are moved (those outside a longest run kept in order). Each change
// names the Policy before it on its side.
func entryChanges(out []ChainChange, route string, p phase.Phase, leg string, a, b []hub.Entry) []ChainChange {
	if len(a) == 0 && len(b) == 0 {
		return out
	}
	ka, atA := keyEntries(a)
	kb, atB := keyEntries(b)
	change := func(op OpKind, es []hub.Entry, i int) ChainChange {
		c := ChainChange{Route: route, Phase: p, Leg: leg, Op: op, Policy: es[i].Policy, Class: es[i].Class}
		if i > 0 {
			c.After = es[i-1].Policy
		}
		return c
	}
	for j, k := range kb {
		if _, ok := atA[k]; !ok {
			out = append(out, change(OpAdd, b, j))
		}
	}
	for i, k := range ka {
		if _, ok := atB[k]; !ok {
			out = append(out, change(OpRemove, a, i))
		}
	}
	// The positions in b of the shared entries, in a's order; those in a
	// longest increasing run keep their relative order.
	var shared []int
	for _, k := range ka {
		if j, ok := atB[k]; ok {
			shared = append(shared, j)
		}
	}
	kept := increasingRun(shared)
	for j, k := range kb {
		if _, ok := atA[k]; ok && !kept[j] {
			out = append(out, change(OpMove, b, j))
		}
	}
	return out
}

// increasingRun returns the members of one longest strictly increasing
// subsequence of seq, a sequence of distinct values, in O(n log n):
// patience sorting, which picks the same run for the same input.
func increasingRun(seq []int) map[int]bool {
	tails := []int{} // index into seq of the smallest tail of each run length
	prev := make([]int, len(seq))
	for i, v := range seq {
		n := sort.Search(len(tails), func(t int) bool { return seq[tails[t]] >= v })
		prev[i] = -1
		if n > 0 {
			prev[i] = tails[n-1]
		}
		if n == len(tails) {
			tails = append(tails, i)
		} else {
			tails[n] = i
		}
	}
	kept := make(map[int]bool, len(tails))
	if len(tails) == 0 {
		return kept
	}
	for i := tails[len(tails)-1]; i >= 0; i = prev[i] {
		kept[seq[i]] = true
	}
	return kept
}
