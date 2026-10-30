// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

// ItemKind is how the Latency job runs an item.
type ItemKind string

// Item kinds.
const (
	// KindAB is a scenario's A/B comparison.
	KindAB ItemKind = "ab"
	// KindLadder is a scenario's rate ladder.
	KindLadder ItemKind = "ladder"
	// KindSingle is one run judged on absolute criteria.
	KindSingle ItemKind = "single"
	// KindCrossCheck is vegeta's S1 cross-check.
	KindCrossCheck ItemKind = "crosscheck"
	// KindComponents is the component benchmarks.
	KindComponents ItemKind = "components"
	// KindDriver is an item with its own driver (O1, O2, C1, C2, R1, G1,
	// constants runs).
	KindDriver ItemKind = "driver"
)

// Item is one Latency job item with its time (PBB "Suite layers").
type Item struct {
	ID       string   `json:"id"`
	Kind     ItemKind `json:"kind"`
	Scenario string   `json:"scenario"`
	Minutes  int      `json:"minutes"`
}

// Latency job time (PBB "Suite layers"), all targets.
const (
	// SlotMinutes is one rotation slot's time.
	SlotMinutes = 75
	// ReserveMinutes is the nightly reserve for host checks, warm-up
	// extensions and re-runs of single invalid runs.
	ReserveMinutes = 10
	// JobMinutes is the Latency job's limit.
	JobMinutes = 180
	// Slots is the rotation's slot count at M1.
	Slots = 4
)

// NightlyItems run every night: S1 and S2 A/B and the component
// benchmarks.
func NightlyItems() []Item {
	return []Item{
		{ID: "S1", Kind: KindAB, Scenario: "S1", Minutes: 40},
		{ID: "S2", Kind: KindAB, Scenario: "S2", Minutes: 40},
		{ID: "components", Kind: KindComponents, Minutes: 15},
	}
}

// SlotItems returns the items of rotation slots 1 to 4 at M1.
func SlotItems(slot int) []Item {
	switch slot {
	case 1:
		return []Item{
			{ID: "S3", Kind: KindAB, Scenario: "S3", Minutes: 40},
			{ID: "O1", Kind: KindDriver, Scenario: "O1", Minutes: 8},
			{ID: "C2", Kind: KindDriver, Scenario: "C2", Minutes: 4},
			{ID: "S3-ladder", Kind: KindLadder, Scenario: "S3", Minutes: 14},
			{ID: "S5-zipf", Kind: KindSingle, Scenario: "S5", Minutes: 4},
		}
	case 2:
		return []Item{
			{ID: "S5", Kind: KindAB, Scenario: "S5", Minutes: 40},
			{ID: "O2", Kind: KindDriver, Scenario: "O2", Minutes: 4},
			{ID: "G1", Kind: KindDriver, Scenario: "G1", Minutes: 15},
			{ID: "S5-ladder", Kind: KindLadder, Scenario: "S5", Minutes: 14},
		}
	case 3:
		return []Item{
			{ID: "S5x", Kind: KindAB, Scenario: "S5x", Minutes: 40},
			{ID: "C1", Kind: KindDriver, Scenario: "C1", Minutes: 10},
			{ID: "S2-pb15-pb16", Kind: KindSingle, Scenario: "S2", Minutes: 8},
			{ID: "PB-10", Kind: KindDriver, Scenario: "S2", Minutes: 4},
			{ID: "S2-gomemlimit", Kind: KindDriver, Scenario: "S2", Minutes: 9},
		}
	case 4:
		return []Item{
			{ID: "S1-ladder", Kind: KindLadder, Scenario: "S1", Minutes: 14},
			{ID: "S2-ladder", Kind: KindLadder, Scenario: "S2", Minutes: 14},
			{ID: "R1", Kind: KindDriver, Scenario: "R1", Minutes: 16},
			{ID: "constants", Kind: KindDriver, Minutes: 25},
			{ID: "S1-vegeta", Kind: KindCrossCheck, Scenario: "S1", Minutes: 4},
		}
	default:
		return nil
	}
}

// findItem returns the item id of the nightly items or any slot.
func findItem(id string) (Item, bool) {
	all := NightlyItems()
	for s := 1; s <= Slots; s++ {
		all = append(all, SlotItems(s)...)
	}
	for _, it := range all {
		if it.ID == id {
			return it, true
		}
	}
	return Item{}, false
}

// StateFormat identifies the rotation state file.
const StateFormat = "ruralz.benchstate.v1"

// State is the Latency job's state on the bench-records branch: the next
// slot, the re-queued items, how often each was re-queued, and each
// scenario's baseline (the commit of its last valid passing run, or an
// accepted commit).
type State struct {
	Format    string            `json:"format"`
	NextSlot  int               `json:"nextSlot"`
	Requeued  []string          `json:"requeued"`
	Retries   map[string]int    `json:"retries"`
	Baselines map[string]string `json:"baselines"`
}

// NewState returns the first night's state.
func NewState() State {
	return State{Format: StateFormat, NextSlot: 1, Requeued: []string{}, Retries: map[string]int{}, Baselines: map[string]string{}}
}

// ReadState reads a state file strictly; a missing file is a fresh state.
func ReadState(path string) (State, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the state file the job names
	if errors.Is(err, os.ErrNotExist) {
		return NewState(), nil
	}
	if err != nil {
		return State{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var st State
	if err := dec.Decode(&st); err != nil {
		return State{}, fmt.Errorf("harness: state %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return State{}, fmt.Errorf("harness: state %s: data after the state", path)
	}
	if st.Format != StateFormat || st.NextSlot < 1 || st.NextSlot > Slots {
		return State{}, fmt.Errorf("harness: state %s: format %q, next slot %d", path, st.Format, st.NextSlot)
	}
	for _, id := range st.Requeued {
		if _, ok := findItem(id); !ok {
			return State{}, fmt.Errorf("harness: state %s: unknown re-queued item %q", path, id)
		}
	}
	if st.Retries == nil {
		st.Retries = map[string]int{}
	}
	if st.Baselines == nil {
		st.Baselines = map[string]string{}
	}
	if st.Requeued == nil {
		st.Requeued = []string{}
	}
	return st, nil
}

// WriteState writes st as indented JSON.
func WriteState(path string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// Plan is one night's items in run order.
type Plan struct {
	// Items run in order: the nightly items, then the re-queued items,
	// then the slot's items that fit.
	Items []Item
	// Slot is the night's rotation slot; 0 runs every slot (release).
	Slot int
	// Deferred are items that did not fit and move to the next night.
	Deferred []string
}

// NewPlan plans one night from st: S1, S2 and the component benchmarks;
// then the re-queued items, which run first in the slot's 75 minutes; then
// the slot's items while they fit; the rest move to the night after. With
// all set (release), every slot runs without the limit.
func NewPlan(st State, all bool) Plan {
	p := Plan{Items: NightlyItems(), Slot: st.NextSlot}
	if all {
		p.Slot = 0
		for s := 1; s <= Slots; s++ {
			p.Items = append(p.Items, SlotItems(s)...)
		}
		return p
	}
	used := 0
	planned := map[string]bool{}
	for _, it := range p.Items {
		planned[it.ID] = true
	}
	add := func(it Item) {
		if planned[it.ID] {
			return
		}
		if used+it.Minutes <= SlotMinutes || used == 0 {
			p.Items = append(p.Items, it)
			used += it.Minutes
		} else {
			p.Deferred = append(p.Deferred, it.ID)
		}
		planned[it.ID] = true
	}
	for _, id := range st.Requeued {
		if it, ok := findItem(id); ok {
			add(it)
		}
	}
	for _, it := range SlotItems(st.NextSlot) {
		add(it)
	}
	return p
}

// Minutes returns the plan's planned minutes.
func (p Plan) Minutes() int {
	n := 0
	for _, it := range p.Items {
		n += it.Minutes
	}
	return n
}

// Outcome is an item's result for the state.
type Outcome int

// Item outcomes.
const (
	// Passed is a valid item whose A/B comparison passed or had no
	// baseline and whose budgets held.
	Passed Outcome = iota
	// Failed is a regression, a missed budget or an error.
	Failed
	// Inconclusive is noise, an invalid run left after its re-run, or a
	// baseline that refused the Revision.
	Inconclusive
)

// Advance returns the next night's state after a night's outcomes: the
// rotation moves one slot; inconclusive items re-queue once, and fail the
// second time (an issue opens); deferred items stay queued; a passed A/B
// item's scenario takes the head commit as its baseline. A release run
// (plan.Slot 0) changes nothing.
func Advance(st State, p Plan, outcomes map[string]Outcome, head string) (State, []string) {
	if p.Slot == 0 {
		return st, nil
	}
	next := State{
		Format: StateFormat, NextSlot: p.Slot%Slots + 1, Requeued: []string{},
		Retries: map[string]int{}, Baselines: map[string]string{},
	}
	for k, v := range st.Baselines {
		next.Baselines[k] = v
	}
	var failures []string
	for _, it := range p.Items {
		switch outcomes[it.ID] {
		case Passed:
			if it.Kind == KindAB {
				next.Baselines[it.Scenario] = head
			}
		case Inconclusive:
			if st.Retries[it.ID] >= 1 {
				failures = append(failures, it.ID+": inconclusive again after its re-run")
				continue
			}
			next.Requeued = append(next.Requeued, it.ID)
			next.Retries[it.ID] = st.Retries[it.ID] + 1
		case Failed:
		}
	}
	for _, id := range p.Deferred {
		if !slices.Contains(next.Requeued, id) {
			next.Requeued = append(next.Requeued, id)
			if r, ok := st.Retries[id]; ok {
				next.Retries[id] = r
			}
		}
	}
	return next, failures
}

// Accept applies a maintainer's acceptance: the scenario's next baseline
// is the accepted commit, which the next record names.
func Accept(st State, a *Acceptance) (State, error) {
	if a == nil {
		return st, nil
	}
	if _, ok := Find(Catalog(), a.Scenario); !ok {
		return st, fmt.Errorf("harness: accepted scenario %q is not in the catalog", a.Scenario)
	}
	out := st
	out.Baselines = map[string]string{}
	for k, v := range st.Baselines {
		out.Baselines[k] = v
	}
	out.Baselines[a.Scenario] = a.Commit
	return out, nil
}
