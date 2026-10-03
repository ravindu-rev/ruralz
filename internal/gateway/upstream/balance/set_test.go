// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

// runtimeState is a test payload: the Upstream runtime's per-Endpoint state
// published with one list, indexed like it.
type runtimeState struct {
	ids    []string
	grades []Grade
}

// newRuntimeState grades list[i] Down when down(i).
func newRuntimeState(list []Endpoint, down func(i int) bool) *runtimeState {
	st := &runtimeState{ids: make([]string, len(list)), grades: make([]Grade, len(list))}
	for i, ep := range list {
		st.ids[i] = ep.Identity
		st.grades[i] = GradeOf(down(i), false)
	}
	return st
}

// stateView is the View a runtime builds from one Set's payload; it counts
// indices outside that payload instead of panicking.
type stateView struct {
	st  *runtimeState
	bad *atomic.Int64
}

func (v stateView) Grade(i int) Grade {
	if i < 0 || i >= len(v.st.grades) {
		v.bad.Add(1)
		return DownAvoided
	}
	return v.st.grades[i]
}

func (v stateView) Load(i int) int64 {
	if i < 0 || i >= len(v.st.grades) {
		v.bad.Add(1)
		return 0
	}
	return int64(i % 3)
}

// TestSetSnapshot covers the View contract behind 05 reqs 11 and 16: a Set
// keeps selecting from its own list and payload after the Balancer moved
// on, so a View built from one Set never meets another Set's indices.
func TestSetSnapshot(t *testing.T) {
	ids := func(names ...string) []Endpoint {
		out := make([]Endpoint, len(names))
		for i, n := range names {
			out[i] = Endpoint{Identity: n, Weight: 1}
		}
		return out
	}
	for _, alg := range allAlgorithms {
		t.Run(alg.String(), func(t *testing.T) {
			small := ids("b", "d", "f")
			oldState := newRuntimeState(small, func(i int) bool { return i == 0 })
			b := mustNew(t, Config{Algorithm: alg, VirtualNodes: 64, Endpoints: small, Payload: oldState})
			old := b.Load()
			big := ids("a", "b", "c", "d", "e", "f")
			newState := newRuntimeState(big, func(i int) bool { return i%2 == 1 }) // b, d, f Down
			if err := b.SetEndpoints(big, newState); err != nil {
				t.Fatal(err)
			}
			cur := b.Load()
			if old.Len() != 3 || old.Payload() != oldState || cur.Len() != 6 || cur.Payload() != newState {
				t.Fatalf("Sets: old %d %p, current %d %p", old.Len(), old.Payload(), cur.Len(), cur.Payload())
			}
			if cur.Endpoint(4) != (Endpoint{"e", 1}) || old.Identity(2) != "f" {
				t.Fatalf("accessors: %+v %q", cur.Endpoint(4), old.Identity(2))
			}
			var bad atomic.Int64
			src := pcg(5)
			for k := range uint64(500) {
				key := k * 0x9e3779b97f4a7c15
				c, err := old.Select(key, src, stateView{oldState, &bad})
				if err != nil || c.Index < 0 || c.Index >= 3 || old.Identity(c.Index) == "b" || c.Grade != Eligible {
					t.Fatalf("old Set: %+v, %v", c, err)
				}
				c, err = cur.Select(key, src, stateView{newState, &bad})
				if err != nil || c.Index < 0 || c.Index >= 6 || c.Grade != Eligible || newState.grades[c.Index] != Eligible {
					t.Fatalf("current Set: %+v, %v", c, err)
				}
				if c, err = cur.SelectRandom(src, stateView{newState, &bad}); err != nil || c.Grade != Eligible {
					t.Fatalf("current Set SelectRandom: %+v, %v", c, err)
				}
			}
			if bad.Load() != 0 {
				t.Fatalf("%d indices outside the Set's list", bad.Load())
			}
		})
	}
}

// TestSetSnapshotUnderChurn runs attempts the way the Upstream runtime does,
// one loaded Set per attempt with its View built from that Set's payload,
// on many goroutines while the set churns, plans change and structures
// rebuild (run under -race): every graded index lies inside the Set's list,
// and every choice is a member of that list with the lowest grade present
// in it (05 req 11).
func TestSetSnapshotUnderChurn(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	listFor := func(round int) []Endpoint {
		var out []Endpoint
		for i, n := range names {
			if (round+i)%3 != 0 || i == round%len(names) {
				out = append(out, Endpoint{Identity: n, Weight: u32(1 + i%3)})
			}
		}
		return out
	}
	for _, alg := range allAlgorithms {
		t.Run(alg.String(), func(t *testing.T) {
			first := listFor(0)
			b := mustNew(t, Config{Algorithm: alg, VirtualNodes: 64, Endpoints: first, Payload: newRuntimeState(first, func(int) bool { return false })})
			ctx, cancel := context.WithCancel(context.Background())
			var bad, picks atomic.Int64
			var wg, started sync.WaitGroup
			started.Add(4)
			for g := range uint64(4) {
				wg.Go(func() {
					ran := sync.OnceFunc(started.Done)
					defer ran()
					for k := uint64(0); ctx.Err() == nil; k++ {
						set := b.Load()
						st, ok := set.Payload().(*runtimeState)
						if !ok {
							t.Error("payload of another type")
							return
						}
						c, err := set.Select(k*0x9e3779b97f4a7c15+g, nil, stateView{st, &bad})
						if errors.Is(err, ErrNoEndpoints) && set.Len() == 0 {
							continue
						}
						low := slices.Min(st.grades)
						if err != nil || c.Index < 0 || c.Index >= set.Len() || st.ids[c.Index] != set.Identity(c.Index) || c.Grade != st.grades[c.Index] || c.Grade != low {
							t.Errorf("Select on a Set of %d = %+v, %v (lowest grade %d)", set.Len(), c, err, low)
							return
						}
						picks.Add(1)
						ran()
					}
				})
			}
			// Every attempt goroutine makes one pick before the churn starts;
			// otherwise a fast loop can end before any of them is scheduled.
			started.Wait()
			gate := NewBuildGate(2)
			now := t0
			for round := range 300 {
				list := listFor(round)
				st := newRuntimeState(list, func(i int) bool { return (i+round)%4 != 0 })
				if err := b.SetEndpoints(list, st); err != nil {
					t.Fatal(err)
				}
				if round%2 == 0 {
					now = now.Add(RebuildInterval)
					if _, err := b.Rebuild(ctx, gate, now); err != nil {
						t.Fatal(err)
					}
				}
				b.SetPlan(64<<(round%3), round%7 == 0)
			}
			cancel()
			wg.Wait()
			if bad.Load() != 0 {
				t.Fatalf("%d graded indices outside the Set's list", bad.Load())
			}
			if picks.Load() == 0 {
				t.Fatal("no attempt ran")
			}
		})
	}
}
