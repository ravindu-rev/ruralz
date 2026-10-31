// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func ids(items []Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// TestSlotsFitTheirTime checks the slot table: every M1 slot fits its
// 75 minutes, and the nightly items, a slot and the reserve fit the
// 3-hour job (PBB "Suite layers").
func TestSlotsFitTheirTime(t *testing.T) {
	want := map[int]int{1: 70, 2: 73, 3: 71, 4: 73}
	nightly := 0
	for _, it := range NightlyItems() {
		nightly += it.Minutes
	}
	if nightly != 95 {
		t.Errorf("nightly items = %d minutes, want 95 (S1 and S2 A/B 80, components 15)", nightly)
	}
	for s := 1; s <= Slots; s++ {
		n := 0
		for _, it := range SlotItems(s) {
			n += it.Minutes
		}
		if n != want[s] || n > SlotMinutes {
			t.Errorf("slot %d = %d minutes, want %d within %d", s, n, want[s], SlotMinutes)
		}
		if nightly+SlotMinutes+ReserveMinutes > JobMinutes {
			t.Errorf("the worst night exceeds %d minutes", JobMinutes)
		}
	}
	if SlotItems(0) != nil || SlotItems(5) != nil {
		t.Error("slots outside 1 to 4 hold items")
	}
}

// TestNewPlan covers the night's order: nightly items, then re-queued
// items first in the slot's time, then the slot's items that fit; the rest
// move to the night after.
func TestNewPlan(t *testing.T) {
	st := NewState()
	p := NewPlan(st, false)
	if got, want := ids(p.Items), []string{"S1", "S2", "components", "S3", "O1", "C2", "S3-ladder", "S5-zipf"}; !slices.Equal(got, want) {
		t.Errorf("first night = %v, want %v", got, want)
	}
	if p.Slot != 1 || len(p.Deferred) != 0 {
		t.Errorf("first night slot %d deferred %v", p.Slot, p.Deferred)
	}
	st.NextSlot, st.Requeued = 2, []string{"S3", "S1", "C2"}
	p = NewPlan(st, false)
	// S1 runs nightly anyway; S3 (40) and C2 (4) run first; of slot 2,
	// O2 (4) and G1 (15) fit after S5 (40) does not.
	if got, want := ids(p.Items), []string{"S1", "S2", "components", "S3", "C2", "O2", "G1"}; !slices.Equal(got, want) {
		t.Errorf("re-queued night = %v, want %v", got, want)
	}
	if got, want := p.Deferred, []string{"S5", "S5-ladder"}; !slices.Equal(got, want) {
		t.Errorf("deferred = %v, want %v", got, want)
	}
	if p.Minutes() > JobMinutes-ReserveMinutes {
		t.Errorf("planned %d minutes", p.Minutes())
	}
	all := NewPlan(st, true)
	if all.Slot != 0 || len(all.Items) != 3+5+4+5+5 {
		t.Errorf("release plan slot %d items %v", all.Slot, ids(all.Items))
	}
}

// TestAdvance covers the state rules: the rotation moves one slot, an
// inconclusive item re-queues once and fails the second time, deferred
// items stay queued, passed A/B items advance their baseline, and a
// release run changes nothing.
func TestAdvance(t *testing.T) {
	st := NewState()
	st.NextSlot = 4
	st.Baselines["S2"] = "1111111"
	st.Retries["S1-ladder"] = 1
	p := Plan{Items: []Item{{ID: "S1", Kind: KindAB, Scenario: "S1"}, {ID: "S2", Kind: KindAB, Scenario: "S2"}, {ID: "S1-ladder", Kind: KindLadder, Scenario: "S1"}, {ID: "R1", Kind: KindDriver}}, Slot: 4, Deferred: []string{"constants"}}
	next, failures := Advance(st, p, map[string]Outcome{"S1": Passed, "S2": Failed, "S1-ladder": Inconclusive, "R1": Inconclusive}, "abcdef0")
	if next.NextSlot != 1 {
		t.Errorf("next slot = %d, want 1 after 4", next.NextSlot)
	}
	if next.Baselines["S1"] != "abcdef0" || next.Baselines["S2"] != "1111111" {
		t.Errorf("baselines = %v: a pass advances, a failure keeps", next.Baselines)
	}
	if got, want := next.Requeued, []string{"R1", "constants"}; !slices.Equal(got, want) {
		t.Errorf("re-queued = %v, want %v", got, want)
	}
	if next.Retries["R1"] != 1 || len(failures) != 1 || failures[0] != "S1-ladder: inconclusive again after its re-run" {
		t.Errorf("retries %v failures %q", next.Retries, failures)
	}
	if st.Baselines["S1"] != "" {
		t.Error("Advance changed its input")
	}
	rel, f := Advance(st, Plan{Items: p.Items, Slot: 0}, map[string]Outcome{"S1": Passed}, "abcdef0")
	if rel.NextSlot != st.NextSlot || rel.Baselines["S1"] != "" || f != nil {
		t.Errorf("a release run changed the state: %+v", rel)
	}
}

func TestStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := ReadState(path)
	if err != nil || st.NextSlot != 1 || st.Format != StateFormat {
		t.Fatalf("ReadState(missing) = %+v, %v; want a fresh state", st, err)
	}
	st.NextSlot, st.Requeued, st.Baselines["S1"] = 3, []string{"G1"}, "abcdef0"
	if err := WriteState(path, st); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(path)
	if err != nil || got.NextSlot != 3 || !slices.Equal(got.Requeued, []string{"G1"}) || got.Baselines["S1"] != "abcdef0" {
		t.Errorf("round trip = %+v, %v", got, err)
	}
	bad := map[string]string{
		"unknown field":  `{"format":"ruralz.benchstate.v1","nextSlot":1,"requeued":[],"retries":{},"baselines":{},"extra":1}`,
		"slot":           `{"format":"ruralz.benchstate.v1","nextSlot":5,"requeued":[],"retries":{},"baselines":{}}`,
		"format":         `{"format":"v0","nextSlot":1,"requeued":[],"retries":{},"baselines":{}}`,
		"unknown item":   `{"format":"ruralz.benchstate.v1","nextSlot":1,"requeued":["S9"],"retries":{},"baselines":{}}`,
		"trailing value": `{"format":"ruralz.benchstate.v1","nextSlot":1,"requeued":[],"retries":{},"baselines":{}} {}`,
	}
	for name, body := range bad {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadState(path); err == nil {
			t.Errorf("ReadState(%s) = nil error", name)
		}
	}
}

func TestAccept(t *testing.T) {
	st := NewState()
	got, err := Accept(st, &Acceptance{Scenario: "S2", Commit: "abcdef0"})
	if err != nil || got.Baselines["S2"] != "abcdef0" || st.Baselines["S2"] != "" {
		t.Errorf("Accept = %+v, %v", got.Baselines, err)
	}
	if _, err := Accept(st, &Acceptance{Scenario: "S9", Commit: "abcdef0"}); err == nil {
		t.Error("Accept(S9) = nil error")
	}
	if got, err := Accept(st, nil); err != nil || len(got.Baselines) != 0 {
		t.Errorf("Accept(nil) = %+v, %v", got, err)
	}
}

func TestWorse(t *testing.T) {
	if worse(Passed, Inconclusive) != Inconclusive || worse(Failed, Inconclusive) != Failed || worse(Passed, Passed) != Passed {
		t.Error("worse is wrong")
	}
}
