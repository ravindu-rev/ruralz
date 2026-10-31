// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package drain

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// TestTimelineDefaults checks the target values of "Drain timeline
// defaults": a 5 s accept window, K = 8 waves of 1.25 s across a 10 s
// jitter window, the deadline 20 s after stop-accepting, a 5 s flush, and
// exit bounds of 30 s after a stop signal and 25 s after a handover's
// Drain starts.
func TestTimelineDefaults(t *testing.T) {
	for _, tt := range []struct {
		name      string
		got, want time.Duration
	}{
		{name: "accept window", got: AcceptWindow, want: 5 * time.Second},
		{name: "jitter window", got: JitterWindow, want: 10 * time.Second},
		{name: "wave", got: wave, want: 1250 * time.Millisecond},
		{name: "deadline", got: Deadline, want: 20 * time.Second},
		{name: "flush bound", got: FlushBound, want: 5 * time.Second},
		{name: "exit bound", got: ExitBound, want: 30 * time.Second},
		{name: "handover exit bound", got: HandoverExitBound, want: 25 * time.Second},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	if Waves != 8 {
		t.Errorf("Waves = %d, want K = 8", Waves)
	}
}

// TestPlanOffsets checks the schedule of each kind: a stop signal or a
// failure stops accepting 5 s after the start and reaches the deadline at
// 25 s; a handover stops accepting at its start and reaches the deadline
// at 20 s; a skipped Drain reaches the deadline at once.
func TestPlanOffsets(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name           string
		k              kind
		skip           bool
		stop, deadline time.Duration
	}{
		{name: "signal", k: kindSignal, stop: 5 * time.Second, deadline: 25 * time.Second},
		{name: "failure", k: kindFailure, stop: 5 * time.Second, deadline: 25 * time.Second},
		{name: "handover", k: kindHandover, stop: 0, deadline: 20 * time.Second},
		{name: "skipped", k: kindSignal, skip: true, stop: 0, deadline: 0},
	} {
		s := plan(tt.k, start, tt.skip, half)
		if s.skip != tt.skip || s.stopAccepting.Sub(start) != tt.stop || s.deadline.Sub(start) != tt.deadline {
			t.Errorf("%s: skip %t, stop-accepting %v, deadline %v; want %t, %v, %v",
				tt.name, s.skip, s.stopAccepting.Sub(start), s.deadline.Sub(start), tt.skip, tt.stop, tt.deadline)
		}
		if got, want := s.exitBy().Sub(start), tt.deadline+FlushBound; got != want {
			t.Errorf("%s: exit bound %v, want %v", tt.name, got, want)
		}
	}
}

// TestPlanWavesStayInTheirEighths checks, for many draws, that shard i
// shuts down within the i-th eighth of the jitter window, so the waves are
// in order, and that no 1 s slice of the window holds more than two waves,
// the bound CE-2 asserts.
func TestPlanWavesStayInTheirEighths(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	src := rand.NewPCG(1, 2)
	for range 2000 {
		s := plan(kindSignal, start, false, src)
		for i, at := range s.waves {
			lo := s.stopAccepting.Add(time.Duration(i) * wave)
			if at.Before(lo) || !at.Before(lo.Add(wave)) {
				t.Fatalf("wave %d at %v, outside [%v, %v)", i, at.Sub(start), lo.Sub(start), lo.Add(wave).Sub(start))
			}
			n := 0
			for _, other := range s.waves {
				if !other.Before(at) && other.Before(at.Add(time.Second)) {
					n++
				}
			}
			if n > 2 {
				t.Fatalf("%d waves within 1 s of wave %d: %v", n, i, s.waves)
			}
		}
	}
}

// TestUniform checks the draw within a wave: [0, d) from the whole range
// of the source.
func TestUniform(t *testing.T) {
	for _, tt := range []struct {
		v    uint64
		want time.Duration
	}{
		{v: 0, want: 0},
		{v: 1 << 63, want: wave / 2},
		{v: math.MaxUint64, want: wave - 1},
	} {
		if got := uniform(constSource(tt.v), wave); got != tt.want {
			t.Errorf("uniform(%#x, %v) = %v, want %v", tt.v, wave, got, tt.want)
		}
	}
	if got := uniform(constSource(math.MaxUint64), 0); got != 0 {
		t.Errorf("uniform over an empty range = %v, want 0", got)
	}
}
