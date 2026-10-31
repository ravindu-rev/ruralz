// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/handover"
	"github.com/ravindu-rev/ruralz/internal/problem"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tests for what drives the loader from outside: the handover holder's
// Hold and Release, the Drain's Stop, the watchdog probe, and the
// /debug/snapshots and /config/dump handlers.

// The loader implements the handover's interfaces.
var (
	_ handover.Activations = (*Loader)(nil)
	_ handover.Bound       = (*Bound)(nil)
)

// blocked returns a compile script that waits until release is closed.
func blocked(r *rig, release <-chan struct{}) compileFn {
	return func(_ context.Context, in compile.Input) (Compiled, diag.List, error) {
		<-release
		return r.comp.make(in), nil, nil
	}
}

func TestHoldWaitsForTheActivation(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	r.comp.script(rev("B").Digest, blocked(r, release))
	r.start()
	r.booted()
	r.l.Offer(load(result("B")))
	waitFor(t, "B to compile", func() bool { return r.ev.has("compile:" + rev("B").Digest.Short()) })
	got := make(chan handover.Revisions, 1)
	go func() {
		revs, err := r.l.Hold(t.Context())
		if err != nil {
			t.Error(err)
		}
		got <- revs
	}()
	select {
	case <-got:
		t.Fatal("Hold returned while an activation ran")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	var revs handover.Revisions
	select {
	case revs = <-got:
	case <-time.After(bound):
		t.Fatal("Hold did not return after the activation")
	}
	// The activation's candidate write is included.
	if revs.Active != rev("B").Digest || revs.Candidate != rev("B").Digest {
		t.Fatalf("Revisions = %s, %s; want B and its candidate", revs.Active.Short(), revs.Candidate.Short())
	}
	// Held: a load waits, pending.
	r.l.Offer(load(result("C")))
	r.idle()
	if r.l.Active() != rev("B").Digest {
		t.Fatal("an activation ran during a hold")
	}
	if p := r.l.Snapshots().Pending; p == nil || *p != rev("C").Digest.String() {
		t.Fatal("C is not pending during the hold")
	}
	r.l.Release()
	r.waitActive("C")
	r.l.Release() // an extra Release does nothing
}

func TestHoldWithoutActivation(t *testing.T) {
	r := newRig(t)
	r.start()
	r.booted()
	revs, err := r.l.Hold(t.Context())
	if err != nil || revs.Active != rev("A").Digest || revs.Candidate != rev("A").Digest {
		t.Fatalf("Hold = %+v, %v", revs, err)
	}
	r.l.Release()
}

func TestHoldContextEnds(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	defer close(release)
	r.comp.script(rev("B").Digest, blocked(r, release))
	r.start()
	r.booted()
	r.l.Offer(load(result("B")))
	waitFor(t, "B to compile", func() bool { return r.ev.has("compile:" + rev("B").Digest.Short()) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.l.Hold(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Hold = %v, want context.Canceled", err)
	}
	r.l.mu.Lock()
	holds := r.l.holds
	r.l.mu.Unlock()
	if holds != 0 {
		t.Fatalf("holds = %d after a canceled Hold", holds)
	}
}

func TestStop(t *testing.T) {
	// At a Drain's start nothing activates any more: the activation before
	// its swap is abandoned quietly, the pending Revision dropped, the
	// source stopped, later loads ignored.
	r := newRig(t)
	r.comp.script(rev("B").Digest, func(ctx context.Context, _ compile.Input) (Compiled, diag.List, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	r.start()
	r.booted()
	r.l.Offer(load(result("B")))
	waitFor(t, "B to compile", func() bool { return r.ev.has("compile:" + rev("B").Digest.Short()) })
	r.l.Offer(load(result("C")))
	r.l.Stop()
	r.l.Stop()
	r.idle()
	if r.l.Active() != rev("A").Digest {
		t.Fatal("an activation finished after Stop")
	}
	if n := r.metrics.totalRejections(); n != 0 {
		t.Fatalf("rejections = %d: an abandoned activation is no rejection", n)
	}
	if p := r.l.Snapshots().Pending; p != nil {
		t.Fatal("a pending Revision survived Stop")
	}
	if _, _, stops := r.src.counts(); stops != 1 {
		t.Fatalf("source stopped %d times, want 1", stops)
	}
	r.l.Offer(load(result("D")))
	r.clk.Advance(time.Minute)
	r.idle()
	for _, in := range r.comp.calls() {
		if d := in.Validated.Revision.Digest; d == rev("C").Digest || d == rev("D").Digest {
			t.Fatalf("%s compiled after Stop", d.Short())
		}
	}
	if err := r.l.Reload(t.Context(), rev("A").Digest); !errors.Is(err, ErrStopped) {
		t.Fatalf("Reload after Stop = %v, want ErrStopped", err)
	}
}

func TestStopAbandonsAnActivationBeforeItsSwap(t *testing.T) {
	// A compile that finishes after Stop never publishes.
	r := newRig(t)
	release := make(chan struct{})
	r.comp.script(rev("B").Digest, blocked(r, release))
	r.start()
	r.booted()
	r.l.Offer(load(result("B")))
	waitFor(t, "B to compile", func() bool { return r.ev.has("compile:" + rev("B").Digest.Short()) })
	r.l.Stop()
	close(release)
	r.idle()
	if r.active() != rev("A").Digest {
		t.Fatal("B published after Stop")
	}
	if c := r.comp.last(); c.discarded.Load() != 1 {
		t.Fatal("B was not discarded")
	}
}

func TestProbe(t *testing.T) {
	r := newRig(t)
	r.start()
	r.booted()
	ctx, cancel := context.WithTimeout(t.Context(), bound)
	defer cancel()
	if err := r.l.Probe(ctx); err != nil {
		t.Fatalf("Probe = %v on an idle loop", err)
	}
	// A job within its deadline still lets the loop answer.
	release := make(chan struct{})
	r.comp.script(rev("B").Digest, func(_ context.Context, in compile.Input) (Compiled, diag.List, error) {
		<-release
		return r.comp.make(in), nil, nil
	})
	r.l.Offer(load(result("B")))
	waitFor(t, "B to compile", func() bool { return r.ev.has("compile:" + rev("B").Digest.Short()) })
	if err := r.l.Probe(ctx); err != nil {
		t.Fatalf("Probe = %v during a compile within its deadline", err)
	}
	// Past the deadline, a job that ignores it keeps the loop from
	// answering: the watchdog sees the loop stuck.
	r.clk.Advance(CompileDeadline + time.Second)
	done := make(chan error, 1)
	go func() { done <- r.l.Probe(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("Probe = %v while a job overran its deadline", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Probe = %v after the job ended", err)
		}
	case <-time.After(bound):
		t.Fatal("Probe did not return after the job ended")
	}
	// A probe whose context ended does not stay registered, answered or
	// not.
	short, stop := context.WithCancel(t.Context())
	stop()
	_ = r.l.Probe(short)
	r.l.mu.Lock()
	n := len(r.l.probes)
	r.l.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d probes registered", n)
	}
}

func TestProbeWithoutRun(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := r.l.Probe(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Probe = %v without Run, want the context's error", err)
	}
}

func TestSnapshotsView(t *testing.T) {
	r := newRig(t)
	r.comp.script(rev("B").Digest, findings(finding("RZ-CFG-026", 1, "missing")))
	r.start()
	r.booted()
	v := r.l.Snapshots()
	if len(v.Snapshots) != 1 || v.Snapshots[0].State != "active" || v.Snapshots[0].Digest != rev("A").Digest.String() {
		t.Fatalf("snapshots %+v", v.Snapshots)
	}
	if v.LastKnownGood == nil || *v.LastKnownGood != rev("A").Digest.String() || v.Pending != nil || v.LastRejected != nil {
		t.Fatalf("view %+v", v)
	}
	r.l.Offer(load(result("B")))
	waitFor(t, "the rejection", func() bool { return r.metrics.rejections("RZ-CFG-026") == 1 })
	r.idle()
	v = r.l.Snapshots()
	if v.Pending == nil || *v.Pending != rev("B").Digest.String() {
		t.Fatal("the retried Revision is not pending")
	}
	if v.LastRejected == nil || v.LastRejected.Code != "RZ-CFG-026" {
		t.Fatalf("lastRejected %+v", v.LastRejected)
	}
	// The view is a copy.
	*v.LastRejected.Digest = "changed"
	if *r.l.Snapshots().LastRejected.Digest == "changed" {
		t.Fatal("Snapshots shares lastRejected")
	}
	b, err := json.Marshal(r.l.Snapshots())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"snapshots", "pending", "lastKnownGood", "lastRejected"} {
		if _, ok := m[k]; !ok {
			t.Errorf("member %s missing from %s", k, b)
		}
	}
}

func TestConfigDumpBeforeTheFirstActivation(t *testing.T) {
	r := newRig(t)
	w := httptest.NewRecorder()
	r.l.ConfigDump().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/config/dump", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", w.Code)
	}
	doc, err := problem.Decode(w.Body.Bytes())
	if err != nil || doc.Code != "RZ-RT-024" || doc.RequestID != testRequestID || w.Header().Get("Content-Type") != problem.ContentType {
		t.Fatalf("problem %+v, %v", doc, err)
	}
}

func TestConfigDump(t *testing.T) {
	content := `{"format":"ruralz.canonical.v1","resources":[]}`
	r := newRig(t, withSource(load(result(content))))
	r.start()
	r.booted()
	w := httptest.NewRecorder()
	r.l.ConfigDump().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/config/dump", nil))
	body, _ := io.ReadAll(w.Body)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, headers %v", w.Code, w.Header())
	}
	d := rev(content).Digest
	want, _ := canonical.AppendDump(nil, rev(content), d)
	if string(body) != string(want) {
		t.Fatalf("body %s, want %s", body, want)
	}
	var doc struct {
		Content       json.RawMessage `json:"content"`
		Digest        string          `json:"digest"`
		LastKnownGood string          `json:"lastKnownGood"`
		Revision      string          `json:"revision"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc.Content) != content || doc.Digest != d.String() || doc.LastKnownGood != d.String() || doc.Revision != d.Short() {
		t.Fatalf("dump %+v", doc)
	}
}

func TestMetricsDefaults(t *testing.T) {
	// A Loader without metrics records into no-ops.
	m := nopMetrics()
	m.Activated.Add(0, 1)
	m.Rejected.Inc(0, "RZ-CFG-001")
	m.ActivationDuration("total", "le1000").Record(0, 1)
	m.ActivationDuration("total", "le1000").RecordExemplar(0, 1, emit.Exemplar{})
	m.RevisionInfo("active", "rev-x")
	fill(&m, emit.ConfigMetrics{})
	if m.Activated == nil || m.Rejected == nil || m.ActivationDuration == nil || m.RevisionInfo == nil {
		t.Fatal("fill cleared a handle")
	}
}
