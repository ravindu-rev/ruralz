// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package revalidate

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// TestRevalidationResults checks ruralz_cache_revalidations_total: each job
// that ran counts its result on the handles of the Route that queued it:
// refreshed, ended, kept, or skipped when it changed nothing.
func TestRevalidationResults(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *harness, k statestore.CacheKey)
		want  int
	}{
		{"refreshed", func(h *harness, _ statestore.CacheKey) { h.reply304(http.Header{"Etag": {`"v1"`}}) }, emit.RevalidationRefreshed},
		{"replaced", func(h *harness, _ statestore.CacheKey) {
			h.replyFull(http.StatusOK, http.Header{"Cache-Control": {"max-age=60"}}, "new")
		}, emit.RevalidationRefreshed},
		{"ended", func(h *harness, _ statestore.CacheKey) { h.replyStatus(http.StatusOK) }, emit.RevalidationEnded},
		{"kept", func(h *harness, _ statestore.CacheKey) { h.replyStatus(http.StatusServiceUnavailable) }, emit.RevalidationKept},
		{"lease held", func(h *harness, k statestore.CacheKey) { h.takeLease(k) }, emit.RevalidationSkipped},
		{"lease failed", func(h *harness, _ statestore.CacheKey) {
			h.store.setIntercept(func(w *statestore.Write) bool {
				w.Err = statestore.ErrBreakerOpen
				return true
			})
		}, emit.RevalidationSkipped},
		{"write failed", func(h *harness, _ statestore.CacheKey) {
			h.store.setIntercept(func(w *statestore.Write) bool {
				if w.Lease.Mode != statestore.LeaseRefresh {
					return false
				}
				w.Applied = false
				return true
			})
		}, emit.RevalidationSkipped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			k := testKey(1)
			h.seed(k)
			tt.setup(h, k)
			m, cs := routeMetrics()
			j := h.job(k, entry(staleHeader()))
			j.Metrics = m
			h.run()
			if !h.rv.Enqueue(j) {
				t.Fatal("Enqueue refused the job")
			}
			h.next()
			var want [emit.NumRevalidationResults]uint64
			want[tt.want] = 1
			if got := counts(cs); got != want {
				t.Errorf("revalidation results %v, want %v", got, want)
			}
		})
	}
}

// replyStatus answers every replay with status.
func (h *harness) replyStatus(status int) {
	h.rp.setReply(func(context.Context, *http.Request) (*http.Response, error) {
		r, _ := respond(status, nil)
		return r, nil
	})
}

// TestUnrunJobsCountNoResult checks that a job dropped for its queue wait
// and a job canceled at stop count no revalidation result.
func TestUnrunJobsCountNoResult(t *testing.T) {
	for _, tt := range []struct {
		name string
		want Outcome
	}{{"expired", OutcomeExpired}, {"canceled", OutcomeCanceled}} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			m, cs := routeMetrics()
			j := h.job(testKey(1), entry(staleHeader()))
			j.Metrics = m
			if !h.rv.Enqueue(j) {
				t.Fatal("Enqueue refused the job")
			}
			if tt.want == OutcomeExpired {
				h.clk.Advance(MaxQueueWait + time.Nanosecond)
				h.run()
			} else {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := h.rv.Run(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if o := h.next(); o != tt.want {
				t.Fatalf("outcome %v, want %v", o, tt.want)
			}
			if got := counts(cs); got != ([emit.NumRevalidationResults]uint64{}) {
				t.Errorf("revalidation results %v, want none", got)
			}
		})
	}
}

// TestDropCounters checks DropCounters: one Node-wide handle per reason of
// ruralz_cache_revalidation_drops_total, in label order, and the lookup's
// error.
func TestDropCounters(t *testing.T) {
	var asked []string
	got, err := DropCounters(func(name string, values ...string) (emit.Counter, error) {
		if name != catalog.CacheRevalidationDropsTotal || len(values) != 1 {
			t.Fatalf("lookup(%s, %v)", name, values)
		}
		asked = append(asked, values[0])
		return &counter{}, nil
	})
	if err != nil || !slices.Equal(asked, []string{"queue_full", "queue_wait", "partitions_full"}) ||
		got[DropQueueFull] == nil || got[DropQueueWait] == nil || got[DropPartitionsFull] == nil {
		t.Fatalf("DropCounters = %v, %v after asking %v", got, err, asked)
	}
	lookupErr := errors.New("unknown family")
	if _, err := DropCounters(func(string, ...string) (emit.Counter, error) { return nil, lookupErr }); !errors.Is(err, lookupErr) {
		t.Errorf("DropCounters with a failing lookup = %v", err)
	}
}

// dropHarness returns a harness whose Revalidator records drops and the
// degraded reason.
func dropHarness(t *testing.T) (*harness, [NumDropReasons]*counter, *status) {
	h := newHarness(t)
	var drops [NumDropReasons]emit.Counter
	var cs [NumDropReasons]*counter
	for i := range cs {
		cs[i] = &counter{}
		drops[i] = cs[i]
	}
	st := &status{}
	h.rv = New(Options{Clock: h.clk, Rand: h.rnd, Drops: drops, Status: st})
	h.rv.finished = func(o Outcome) { h.done <- o }
	return h, cs, st
}

// dropCounts returns the values of cs, indexed by Drop* reason.
func dropCounts(cs [NumDropReasons]*counter) [NumDropReasons]uint64 {
	var out [NumDropReasons]uint64
	for i, c := range cs {
		out[i] = c.n.Load()
	}
	return out
}

// TestDropReasons checks ruralz_cache_revalidation_drops_total: queue_full
// for the full queue, partitions_full for the MaxHeld tracked partitions,
// queue_wait for a job that waited too long; duplicates and a stopped
// Revalidator's refusals count nothing.
func TestDropReasons(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, h *harness)
		want [NumDropReasons]uint64
	}{
		{"queue full", func(_ *testing.T, h *harness) {
			for i := range QueueSize {
				h.enqueue(testKey(i), entry(staleHeader()))
			}
			_ = h.rv.Enqueue(h.job(testKey(QueueSize), entry(staleHeader())))
			_ = h.rv.Enqueue(h.job(testKey(0), entry(staleHeader()))) // a duplicate
		}, [NumDropReasons]uint64{DropQueueFull: 1}},
		{"tracked partitions full", func(_ *testing.T, h *harness) {
			h.rv.maxHeld = 1
			h.enqueue(testKey(1), entry(staleHeader()))
			_ = h.rv.Enqueue(h.job(testKey(2), entry(staleHeader())))
		}, [NumDropReasons]uint64{DropPartitionsFull: 1}},
		{"queue wait", func(t *testing.T, h *harness) {
			h.enqueue(testKey(1), entry(staleHeader()))
			h.clk.Advance(MaxQueueWait + time.Nanosecond)
			h.run()
			if o := h.next(); o != OutcomeExpired {
				t.Fatalf("outcome %v, want expired", o)
			}
		}, [NumDropReasons]uint64{DropQueueWait: 1}},
		{"stopped", func(t *testing.T, h *harness) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := h.rv.Run(ctx); err != nil {
				t.Fatal(err)
			}
			_ = h.rv.Enqueue(h.job(testKey(1), entry(staleHeader())))
		}, [NumDropReasons]uint64{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, cs, _ := dropHarness(t)
			tt.run(t, h)
			if got := dropCounts(cs); got != tt.want {
				t.Errorf("drops by reason %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSaturatedCountsEveryDropReason checks that a partitions_full drop
// counts in the saturation share like a full queue: one of two offered
// revalidations dropped raises the reason.
func TestSaturatedCountsEveryDropReason(t *testing.T) {
	h, _, st := dropHarness(t)
	h.rv.maxHeld = 1
	h.enqueue(testKey(1), entry(staleHeader()))
	_ = h.rv.Enqueue(h.job(testKey(2), entry(staleHeader())))
	raised := degradedCall{catalog.ReasonCacheRevalidationSaturated, SaturatedSource, true}
	if calls := st.recorded(); !slices.Equal(calls, []degradedCall{raised}) {
		t.Errorf("SetDegraded calls %v, want one raise", calls)
	}
}

// TestSaturated checks cache_revalidation_saturated: raised once the drops
// of the last minute exceed 10% of the revalidations offered (target),
// cleared by Run's re-check once they age out, and cleared when Run stops.
func TestSaturated(t *testing.T) {
	h, _, st := dropHarness(t)
	e := entry(staleHeader())
	for i := range QueueSize {
		h.enqueue(testKey(i), e)
	}
	// 256 offered and queued: dropped*100 > offered*10 first holds at the
	// 29th refusal, 29 of 285 offered.
	for i := range 28 {
		_ = h.rv.Enqueue(h.job(testKey(QueueSize+i), e))
	}
	if calls := st.recorded(); len(calls) != 0 {
		t.Fatalf("raised at 28 of 284 dropped: %v", calls)
	}
	_ = h.rv.Enqueue(h.job(testKey(QueueSize+28), e))
	raised := degradedCall{catalog.ReasonCacheRevalidationSaturated, SaturatedSource, true}
	if calls := st.recorded(); !slices.Equal(calls, []degradedCall{raised}) {
		t.Fatalf("SetDegraded calls %v, want one raise", calls)
	}
	// Run serves the queued jobs at once (no wait); its re-check, every
	// second of the fake clock, clears the reason once the window passed
	// without drops.
	h.run()
	for range QueueSize {
		h.next()
	}
	cleared := degradedCall{catalog.ReasonCacheRevalidationSaturated, SaturatedSource, false}
	deadline := time.Now().Add(waitLimit)
	for !slices.Equal(st.recorded(), []degradedCall{raised, cleared}) {
		if time.Now().After(deadline) {
			t.Fatalf("SetDegraded calls %v; the re-check never cleared the reason", st.recorded())
		}
		h.clk.Advance(SaturatedCheck)
		time.Sleep(time.Millisecond)
	}
	if elapsed := h.clk.Now().Sub(t0); elapsed < SaturatedWindow {
		t.Errorf("cleared %v after the drops, inside the %v window", elapsed, SaturatedWindow)
	}
	// Raised again while the workers are busy, it clears when Run stops.
	g := newGate(t)
	h.rp.setReply(func(ctx context.Context, _ *http.Request) (*http.Response, error) {
		select {
		case <-g.ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r, _ := respond(http.StatusNotModified, nil)
		return r, nil
	})
	for i := range Workers {
		h.enqueue(testKey(2000+i), e)
	}
	for range Workers {
		h.awaitReplay()
	}
	for i := range QueueSize + 40 {
		_ = h.rv.Enqueue(h.job(testKey(3000+i), e))
	}
	h.shutdown()
	if calls := st.recorded(); len(calls) != 4 || calls[2] != raised || calls[3] != cleared {
		t.Errorf("SetDegraded calls %v, want a second raise cleared at stop", calls)
	}
}

// TestSaturationWindow checks the rolling window: per-second counts, kept
// for one minute, older ones dropped.
func TestSaturationWindow(t *testing.T) {
	var w satWindow
	w.add(t0, 10, 2)
	w.add(t0.Add(30*time.Second), 5, 1)
	if o, d := w.sums(t0.Add(59 * time.Second)); o != 15 || d != 3 {
		t.Errorf("sums inside the window = %d, %d; want 15, 3", o, d)
	}
	if o, d := w.sums(t0.Add(60 * time.Second)); o != 5 || d != 1 {
		t.Errorf("sums after the first second aged out = %d, %d; want 5, 1", o, d)
	}
	w.add(t0.Add(60*time.Second), 1, 1) // reuses t0's bucket
	if o, d := w.sums(t0.Add(61 * time.Second)); o != 6 || d != 2 {
		t.Errorf("sums with a reused bucket = %d, %d; want 6, 2", o, d)
	}
}
