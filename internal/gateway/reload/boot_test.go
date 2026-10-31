// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/lkg"
	"github.com/ravindu-rev/ruralz/internal/gateway/source"
)

// Tests for the boot order of foundation pack section 8.2 and
// docs/architecture/03-data-plane.md "Sources, Last-Known-Good and
// Drain": the source, else Last-Known-Good under lkg_boot, else not
// ready; a handover successor's candidate; the Last-Known-Good writes.

// unavailable is a boot Load of a source that cannot be read.
func unavailable() source.Load {
	return source.Load{Err: errors.Join(source.ErrUnavailable, os.ErrNotExist), Trigger: source.TriggerBoot}
}

func TestBootFallsBackToLastKnownGood(t *testing.T) {
	r := newRig(t, withSource(unavailable()))
	r.writeLKG("L")
	r.start()
	r.booted()
	if r.active() != rev("L").Digest || !r.ready.Ready() {
		t.Fatalf("active %s, /readyz %+v; want Last-Known-Good serving", r.active().Short(), r.ready.Readyz())
	}
	if !r.status.raised("lkg_boot") {
		t.Fatal("lkg_boot is not raised")
	}
	reqs := r.canon.requests()
	if len(reqs) != 1 || string(reqs[0].Content) != "L" || reqs[0].Name != r.store.Path(lkg.ContentName(rev("L").Digest)) {
		t.Fatalf("canonical requests %+v, want L's content file", reqs)
	}
	if got := r.metrics.stage("verify/le1000"); len(got) != 1 {
		t.Errorf("verify stage %v, want one sample for verified content", got)
	}
	if recs := r.logs.find("serving Last-Known-Good"); len(recs) != 1 || recs[0].attrs["reason"] != "lkg_boot" {
		t.Errorf("records %+v", recs)
	}
	if recs := r.logs.find("revision activated"); len(recs) != 1 || recs[0].attrs["origin"] != "last_known_good" {
		t.Errorf("activation records %+v", recs)
	}
	if recs := r.logs.find("cannot be read"); len(recs) != 1 {
		t.Errorf("unavailable records %+v", recs)
	}
}

func TestBootRejectedSourceFallsBackToLastKnownGood(t *testing.T) {
	r := newRig(t, withSource(load(failedResult(diag.List{finding("RZ-CFG-005", 2, "bad value")}))))
	r.writeLKG("L")
	r.start()
	r.booted()
	if r.active() != rev("L").Digest || !r.status.raised("lkg_boot") {
		t.Fatalf("active %s, lkg_boot %v", r.active().Short(), r.status.raised("lkg_boot"))
	}
	if n := r.metrics.rejections("RZ-CFG-005"); n != 1 {
		t.Fatalf("rejections = %d", n)
	}
}

func TestLastKnownGoodServesWhileTheSourceRetries(t *testing.T) {
	// A transient boot load is retried after Last-Known-Good activated:
	// a secret that appears reaches service without a restart, and the
	// source Revision clears lkg_boot.
	r := newRig(t)
	r.comp.script(rev("A").Digest, findings(finding("RZ-CFG-026", 1, "missing secret")),
		func(_ context.Context, in compile.Input) (Compiled, diag.List, error) {
			return r.comp.make(in), nil, nil
		})
	r.writeLKG("L")
	r.start()
	r.booted()
	if r.active() != rev("L").Digest || !r.status.raised("lkg_boot") {
		t.Fatal("Last-Known-Good is not serving")
	}
	r.armed(RetryMin)
	r.clk.Advance(RetryMin)
	r.waitActive("A")
	if r.status.raised("lkg_boot") {
		t.Fatal("lkg_boot stays raised after a source Revision activated")
	}
}

func TestSourceRenderingTheActiveRevisionClearsLKGBoot(t *testing.T) {
	r := newRig(t, withSource(unavailable()))
	r.writeLKG("L")
	r.start()
	r.booted()
	r.l.Offer(load(result("L")))
	r.idle()
	if r.status.raised("lkg_boot") {
		t.Fatal("lkg_boot stays raised once the source renders the active Revision")
	}
	if n := len(r.comp.calls()); n != 1 {
		t.Fatalf("compiled %d times, want 1", n)
	}
}

func TestLastKnownGoodMismatchBootsNothing(t *testing.T) {
	// Content that does not hash to its digest boots nothing (RZ-CFG-027).
	r := newRig(t, withSource(unavailable()))
	r.writeLKG("L")
	if err := os.WriteFile(r.store.Path(lkg.ContentName(rev("L").Digest)), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.start()
	r.booted()
	if !r.active().IsZero() || r.status.raised("lkg_boot") {
		t.Fatal("tampered Last-Known-Good booted")
	}
	if n := r.metrics.rejections("RZ-CFG-027"); n != 1 {
		t.Fatalf("rejections{RZ-CFG-027} = %d", n)
	}
	reqs := r.canon.requests()
	if len(reqs) != 1 || reqs[0].Digest != rev("L").Digest {
		t.Fatalf("requests %+v, want the digest passed so the pipeline locates the mismatch", reqs)
	}
	if got := r.reasons(); !slices.Equal(got, []string{"no_revision/"}) {
		t.Fatalf("reasons %v", got)
	}
}

func TestLastKnownGoodNewerReleaseBootsNothing(t *testing.T) {
	r := newRig(t, withSource(unavailable()))
	r.writeLKG("L")
	r.canon.results = map[string]*pipeline.Result{"L": failedResult(diag.List{finding("RZ-CFG-024", 1, "field newer than this release")})}
	r.start()
	r.booted()
	if !r.active().IsZero() {
		t.Fatal("newer-release content booted")
	}
	if n := r.metrics.rejections("RZ-CFG-024"); n != 1 {
		t.Fatalf("rejections = %d", n)
	}
	if lr := r.l.Snapshots().LastRejected; lr == nil || lr.Digest == nil || *lr.Digest != rev("L").Digest.String() {
		t.Fatalf("lastRejected = %+v", lr)
	}
}

func TestLastKnownGoodSameAsTheRejectedSource(t *testing.T) {
	// Last-Known-Good holding the Revision the source failed with would
	// fail alike: it boots nothing and counts nothing more.
	r := newRig(t)
	r.comp.script(rev("A").Digest, findings(finding("RZ-CFG-040", 1, "unserved")))
	r.writeLKG("A")
	r.start()
	r.booted()
	if len(r.canon.requests()) != 0 {
		t.Fatal("Last-Known-Good was re-entered")
	}
	if n := r.metrics.totalRejections(); n != 1 {
		t.Fatalf("rejections = %d, want 1", n)
	}
	if recs := r.logs.find("is the rejected source Revision"); len(recs) != 1 {
		t.Fatalf("records %+v", recs)
	}
}

func TestNothingToBootStaysNotReady(t *testing.T) {
	r := newRig(t, withSource(unavailable()))
	r.start()
	r.booted()
	if !r.active().IsZero() || r.ready.Ready() {
		t.Fatal("a Node without configuration is ready")
	}
	if recs := r.logs.find("no Last-Known-Good"); len(recs) != 1 {
		t.Fatalf("records %+v", recs)
	}
	// Booted closed although nothing activated: READY=1 never waits.
	select {
	case <-r.l.Booted():
	default:
		t.Fatal("Booted is open")
	}
}

func TestHolderWatchesAfterTheBoot(t *testing.T) {
	r := newRig(t)
	r.start()
	r.booted()
	waitFor(t, "the watcher", func() bool {
		runs, _, _ := r.src.counts()
		return runs == 1
	})
}

func TestSuccessorBootsTheCandidate(t *testing.T) {
	r := newRig(t, withSuccessorBoot())
	r.held.Store(false)
	r.writeLKG("H")
	r.start()
	r.booted()
	if r.active() != rev("H").Digest || r.l.Active() != rev("H").Digest {
		t.Fatalf("active %s, want the holder's candidate", r.active().Short())
	}
	h := rev("H").Digest.Short()
	// Nothing listens before the handover is accepted.
	if r.ev.has("commit:listeners:"+h) || r.ready.Ready() {
		t.Fatalf("events %v, /readyz %+v: a successor listened before Listen", r.ev.all(), r.ready.Readyz())
	}
	if recs := r.logs.find("revision activated"); len(recs) != 1 || recs[0].attrs["origin"] != "candidate" {
		t.Fatalf("records %+v", recs)
	}
	if r.status.raised("lkg_boot") {
		t.Fatal("a candidate boot raised lkg_boot")
	}
	runs, _, _ := r.src.counts()
	if runs != 0 {
		t.Fatal("a successor watches the source before it holds the lock")
	}
	if err := r.l.Bound().Listen(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !r.ev.has("commit:listeners:"+h) || !r.ev.has("commit:admin:"+h) || !r.ev.has("listen:admin") {
		t.Fatalf("events %v, want the held switches committed and the Listeners listening", r.ev.all())
	}
	if !r.ready.Ready() {
		t.Fatalf("/readyz %+v after Listen", r.ready.Readyz())
	}
	// Later activations switch at once.
	r.l.TookLock()
	r.l.Offer(load(result("N")))
	r.waitActive("N")
	if !r.ev.has("commit:listeners:" + rev("N").Digest.Short()) {
		t.Fatal("an activation after Listen held its switches")
	}
}

func TestSuccessorWithoutCandidateBootsByFileOrder(t *testing.T) {
	r := newRig(t, withSuccessorBoot())
	r.held.Store(false)
	r.start()
	r.booted()
	if r.active() != rev("A").Digest {
		t.Fatalf("active %s, want the source", r.active().Short())
	}
	if r.ev.has("commit:listeners:" + rev("A").Digest.Short()) {
		t.Fatal("the successor's file-order boot listened")
	}
}

func TestSuccessorFailedCandidateBootsNothing(t *testing.T) {
	// An N-1 successor that cannot serve the holder's candidate
	// (RZ-CFG-024) boots nothing, and never the source instead: the holder
	// has an active Revision and refuses or asks for a reload.
	r := newRig(t, withSuccessorBoot())
	r.held.Store(false)
	r.writeLKG("H")
	r.canon.results = map[string]*pipeline.Result{"H": failedResult(diag.List{finding("RZ-CFG-024", 1, "newer field")})}
	r.start()
	r.booted()
	if !r.active().IsZero() {
		t.Fatal("something booted")
	}
	if len(r.comp.calls()) != 0 {
		t.Fatal("the source was compiled instead")
	}
}

func TestSuccessorReload(t *testing.T) {
	r := newRig(t, withSuccessorBoot())
	r.held.Store(false)
	r.writeLKG("H1")
	r.start()
	r.booted()
	// The holder activated H2 meanwhile and wrote it as its candidate;
	// the successor still holds no lock, so it writes nothing itself.
	r.writeLKG("H2")
	h1, h2 := rev("H1").Digest.Short(), rev("H2").Digest.Short()
	if err := r.l.Reload(t.Context(), rev("H2").Digest); err != nil {
		t.Fatalf("Reload = %v", err)
	}
	if r.l.Active() != rev("H2").Digest {
		t.Fatal("Active is not the reloaded digest")
	}
	// H1's held switches gave way before H2's were readied.
	ev := r.ev.all()
	if slices.Index(ev, "abort:listeners:"+h1) > slices.Index(ev, "prepare:listeners:"+h2) {
		t.Fatalf("events %v: H1's switches must abort before H2 prepares", ev)
	}
	if err := r.l.Bound().Listen(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !r.ev.has("commit:listeners:"+h2) || r.ev.has("commit:listeners:"+h1) {
		t.Fatalf("events %v", r.ev.all())
	}
	// A candidate that fails is a *RejectedError; a missing one an error.
	r.writeLKG("H3")
	r.canon.results = map[string]*pipeline.Result{"H3": failedResult(diag.List{finding("RZ-CFG-024", 1, "newer")})}
	var rej *RejectedError
	if err := r.l.Reload(t.Context(), rev("H3").Digest); !errors.As(err, &rej) || rej.Digest != rev("H3").Digest {
		t.Fatalf("Reload(H3) = %v, want a *RejectedError", err)
	}
	if cand, _, _ := r.store.Pointers(); cand != rev("H3").Digest {
		t.Fatalf("candidate.json names %s, want the holder's H3: a successor without the lock wrote", cand.Short())
	}
	if err := r.l.Reload(t.Context(), rev("H9").Digest); err == nil {
		t.Fatal("Reload of missing content succeeded")
	}
}

func TestSuccessorWatchesAndWritesOnceItTookTheLock(t *testing.T) {
	r := newRig(t, withSuccessorBoot())
	r.held.Store(false)
	r.start()
	r.booted()
	if _, _, err := r.store.Pointers(); err != nil {
		t.Fatal(err)
	}
	if cand, _, _ := r.store.Pointers(); !cand.IsZero() {
		t.Fatal("a successor wrote Last-Known-Good before it held the lock")
	}
	if err := r.l.Bound().Listen(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.held.Store(true)
	r.l.TookLock()
	waitFor(t, "the watcher and the Last-Known-Good write", func() bool {
		runs, _, _ := r.src.counts()
		cand, last, _ := r.store.Pointers()
		return runs == 1 && cand == rev("A").Digest && last == rev("A").Digest
	})
}

func TestBoundCloseClosesEverything(t *testing.T) {
	r := newRig(t, withSuccessorBoot())
	r.held.Store(false)
	r.writeLKG("H")
	r.start()
	r.booted()
	if err := r.l.Bound().Close(); err != nil {
		t.Fatal(err)
	}
	h := rev("H").Digest.Short()
	for _, want := range []string{"abort:listeners:" + h, "abort:admin:" + h, "stop:listeners", "stop:admin"} {
		if !r.ev.has(want) {
			t.Errorf("events %v lack %s", r.ev.all(), want)
		}
	}
}

func TestBoundListenReportsFailures(t *testing.T) {
	r := newRig(t, withSuccessorBoot())
	r.held.Store(false)
	r.writeLKG("H")
	r.parts[0].set(func(p *fakePart) { p.commitErr = bindError("listen: address in use") })
	r.parts[1].set(func(p *fakePart) { p.listenErr = errors.New("admin listen failed") })
	r.start()
	r.booted()
	if err := r.l.Bound().Listen(t.Context()); err == nil {
		t.Fatal("Listen succeeded")
	}
	if got := r.reasons(); !slices.Equal(got, []string{"listeners_unbound/RZ-CFG-039"}) {
		t.Fatalf("reasons %v", got)
	}
}

func TestLKGWriteFailureRetries(t *testing.T) {
	r := newRig(t)
	// A directory where candidate.json goes fails the write.
	if err := os.Mkdir(r.store.Path(lkg.CandidateFile), 0o700); err != nil {
		t.Fatal(err)
	}
	r.start()
	r.booted()
	if !r.lkgStat.raised("lkg_write_failed") {
		t.Fatal("lkg_write_failed is not raised")
	}
	if recs := r.logs.find("Last-Known-Good write failed"); len(recs) != 1 || recs[0].attrs["retry_in"] != "1s" {
		t.Fatalf("records %+v", recs)
	}
	// The second failure waits 2 s.
	r.armed(time.Second)
	r.clk.Advance(time.Second)
	waitFor(t, "the first retry", func() bool { return len(r.logs.find("Last-Known-Good write failed")) == 2 })
	r.idle()
	if recs := r.logs.find("Last-Known-Good write failed"); recs[1].attrs["retry_in"] != "2s" {
		t.Fatalf("second record %+v", recs[1])
	}
	if err := os.Remove(r.store.Path(lkg.CandidateFile)); err != nil {
		t.Fatal(err)
	}
	r.armed(2 * time.Second)
	r.clk.Advance(2 * time.Second)
	waitFor(t, "the write and lkg_write_failed cleared", func() bool {
		cand, last, _ := r.store.Pointers()
		return cand == rev("A").Digest && last == rev("A").Digest && !r.lkgStat.raised("lkg_write_failed")
	})
	r.idle()
	// The candidate the holder reports follows the write.
	revs, err := r.l.Hold(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.l.Release()
	if revs.Candidate != rev("A").Digest {
		t.Fatalf("Candidate = %s", revs.Candidate.Short())
	}
}

func TestNoLKGWriteWithoutTheLock(t *testing.T) {
	r := newRig(t)
	r.held.Store(false)
	r.start()
	r.booted()
	if cand, last, _ := r.store.Pointers(); !cand.IsZero() || !last.IsZero() {
		t.Fatal("a process without the lock wrote Last-Known-Good")
	}
}

func TestInitialPointers(t *testing.T) {
	r := newRig(t, withSource(unavailable()))
	r.writeLKG("L")
	r.start()
	r.booted()
	if got := r.metrics.revisionInfo("lkg"); got != rev("L").Digest.Short() {
		t.Fatalf("revision info lkg = %q", got)
	}
	v := r.l.Snapshots()
	if v.LastKnownGood == nil || *v.LastKnownGood != rev("L").Digest.String() {
		t.Fatalf("lastKnownGood = %v", v.LastKnownGood)
	}
}
