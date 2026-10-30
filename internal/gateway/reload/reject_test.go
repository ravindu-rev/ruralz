// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/source"
)

// Tests for rejections of docs/architecture/03-data-plane.md "Activation"
// and the validation gate: the warn record, the count, lastRejected,
// transient retries with full-jitter backoff, readiness while no Revision
// is active, the compile deadline (RZ-CFG-044) and faults (RZ-CFG-045).

func TestRejectedRevisionKeepsServing(t *testing.T) {
	r := newRig(t)
	r.comp.script(rev("B").Digest, findings(finding("RZ-CFG-040", 3, "kind Plugin is not served by this release")))
	r.start()
	r.booted()
	r.l.Offer(load(result("B")))
	waitFor(t, "the rejection", func() bool { return r.metrics.rejections("RZ-CFG-040") == 1 })
	r.idle()
	if r.active() != rev("A").Digest || !r.ready.Ready() {
		t.Fatal("a rejected Revision changed what the Node serves")
	}
	recs := r.logs.find("revision rejected")
	if len(recs) != 1 {
		t.Fatalf("records %+v, want one warn record", recs)
	}
	rec := recs[0]
	if rec.level != 4 || rec.attrs["code"] != "RZ-CFG-040" || rec.attrs["diagnostics"] != int64(1) || rec.attrs["revision"] != rev("B").Digest.Short() {
		t.Errorf("record %+v", rec)
	}
	if f, ok := rec.attrs["findings"].([]string); !ok || len(f) != 1 || f[0] != "ruralz.yaml:3:1 error RZ-CFG-040: kind Plugin is not served by this release" {
		t.Errorf("findings %#v", rec.attrs["findings"])
	}
	lr := r.l.Snapshots().LastRejected
	if lr == nil || lr.Digest == nil || *lr.Digest != rev("B").Digest.String() || lr.Code != "RZ-CFG-040" || lr.At != "2026-09-26T12:00:00Z" {
		t.Fatalf("lastRejected = %+v", lr)
	}
	// Deterministic: never retried.
	r.clk.Advance(2 * RetryMax)
	r.idle()
	if n := len(r.comp.calls()); n != 2 {
		t.Fatalf("compiled %d times, want 2: a deterministic rejection is not retried", n)
	}
}

func TestParseFailureIsRejectedWithoutDigest(t *testing.T) {
	r := newRig(t)
	r.start()
	r.booted()
	r.clk.Advance(1500 * time.Millisecond)
	r.l.Offer(load(failedResult(diag.List{finding("RZ-CFG-001", 7, "parse error")})))
	r.idle()
	if n := r.metrics.rejections("RZ-CFG-001"); n != 1 {
		t.Fatalf("rejections{RZ-CFG-001} = %d, want 1: parse failures count", n)
	}
	lr := r.l.Snapshots().LastRejected
	if lr == nil || lr.Digest != nil || lr.Code != "RZ-CFG-001" || lr.At != "2026-09-26T12:00:01.5Z" {
		t.Fatalf("lastRejected = %+v, want a null digest", lr)
	}
	if r.active() != rev("A").Digest {
		t.Fatal("the active Revision changed")
	}
}

func TestRejectionLogsTenFindings(t *testing.T) {
	// Up to ten diagnostics in the warn record, the rest at debug.
	r := newRig(t)
	var ds diag.List
	for i := range 12 {
		ds = append(ds, finding("RZ-CFG-009", i+1, "reference "+strconv.Itoa(i)))
	}
	r.start()
	r.booted()
	r.l.Offer(load(failedResult(ds)))
	r.idle()
	recs := r.logs.find("revision rejected")
	var warn, debug int
	for _, rec := range recs {
		switch rec.level {
		case 4:
			warn++
			if f := rec.attrs["findings"].([]string); len(f) != 10 || rec.attrs["diagnostics"] != int64(12) {
				t.Errorf("warn record carries %d findings of %v, want 10 of 12", len(f), rec.attrs["diagnostics"])
			}
		case -4:
			debug++
		}
	}
	if warn != 1 || debug != 2 {
		t.Fatalf("%d warn and %d debug records, want 1 and 2", warn, debug)
	}
	if n := r.metrics.rejections("RZ-CFG-009"); n != 1 {
		t.Fatalf("rejections = %d, want one per load", n)
	}
}

func TestTransientRejectionIsRetried(t *testing.T) {
	r := newRig(t)
	secret := finding("RZ-CFG-026", 4, "file:/etc/ruralz/tls.key is unreadable")
	// Two rejections, then the third compile succeeds.
	r.comp.script(rev("B").Digest, findings(secret), findings(secret), func(_ context.Context, in compile.Input) (Compiled, diag.List, error) {
		return r.comp.make(in), nil, nil
	})
	r.start()
	r.booted()
	r.l.Offer(load(result("B")))
	waitFor(t, "the first rejection", func() bool { return r.metrics.rejections("RZ-CFG-026") == 1 })
	r.idle()
	// The fixed jitter draws 0: the first retry waits RetryMin.
	r.clk.Advance(RetryMin - time.Millisecond)
	r.idle()
	if n := len(r.comp.calls()); n != 2 {
		t.Fatalf("compiled %d times before the retry was due", n)
	}
	r.clk.Advance(time.Millisecond)
	waitFor(t, "the first retry", func() bool { return len(r.comp.calls()) == 3 })
	r.idle()
	if p := r.l.Snapshots().Pending; p == nil || *p != rev("B").Digest.String() {
		t.Fatal("the retried Revision is not pending")
	}
	r.clk.Advance(RetryMin)
	r.waitActive("B")
	// Counted once; the failed retry logged at info.
	if n := r.metrics.rejections("RZ-CFG-026"); n != 1 {
		t.Fatalf("rejections = %d, want 1 for the one load", n)
	}
	if recs := r.logs.find("revision retry rejected"); len(recs) != 1 || recs[0].level != 0 {
		t.Fatalf("retry records %+v", recs)
	}
}

func TestBackoff(t *testing.T) {
	// Full jitter from 1 s to min(60 s, 1 s × 2^n).
	r := newRig(t)
	for n, ceiling := range map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 5: 32 * time.Second, 6: time.Minute, 9: time.Minute} {
		r.l.rnd = fixedRand{v: 0}
		if got := r.l.backoffLocked(n); got != RetryMin {
			t.Errorf("backoff(%d) with the lowest draw = %v, want 1s", n, got)
		}
		r.l.rnd = fixedRand{v: ^uint64(0)}
		if got := r.l.backoffLocked(n); got != ceiling {
			t.Errorf("backoff(%d) with the highest draw = %v, want %v", n, got, ceiling)
		}
	}
}

func TestEveryTransientCode(t *testing.T) {
	for _, code := range []string{"RZ-CFG-026", "RZ-CFG-039", "RZ-CFG-042", "RZ-CFG-044"} {
		if !(rejection{diags: diag.List{finding(code, 1, "x")}}).transient() {
			t.Errorf("%s is not transient", code)
		}
	}
	mixed := rejection{diags: diag.List{finding("RZ-CFG-026", 1, "x"), finding("RZ-CFG-040", 2, "y")}}
	if mixed.transient() {
		t.Error("a load with one deterministic failure is transient")
	}
	warnOnly := rejection{diags: diag.List{{Code: "RZ-CFG-043", Severity: diag.SeverityWarning}}}
	if warnOnly.transient() {
		t.Error("a load without errors is transient")
	}
	withWarn := rejection{diags: diag.List{finding("RZ-CFG-039", 1, "x"), {Code: "RZ-CFG-043", Severity: diag.SeverityWarning}}}
	if !withWarn.transient() {
		t.Error("a warning beside transient errors made the load deterministic")
	}
}

func TestRetryEndsWhenTheSourceChanges(t *testing.T) {
	r := newRig(t)
	r.comp.script(rev("B").Digest, findings(finding("RZ-CFG-039", 1, "listener \"web\": port 8080 in use")))
	r.start()
	r.booted()
	r.l.Offer(load(result("B")))
	waitFor(t, "the rejection", func() bool { return r.metrics.rejections("RZ-CFG-039") == 1 })
	r.l.Offer(load(result("C")))
	r.waitActive("C")
	r.clk.Advance(2 * RetryMax)
	r.idle()
	n := 0
	for _, in := range r.comp.calls() {
		if in.Validated.Revision.Digest == rev("B").Digest {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("B compiled %d times, want 1: a newer load replaces the retry", n)
	}
}

func TestBootTransientRejectionReadiness(t *testing.T) {
	// While no Revision is active, /readyz names each unresolved secretRef
	// and unbound listener of the boot load being retried; a refused State
	// Store URL is retried but names no reason.
	r := newRig(t)
	r.comp.script(rev("A").Digest, findings(
		finding("RZ-CFG-026", 3, "file:/run/secrets/tls.pem is unreadable"),
		finding("RZ-CFG-042", 5, "the State Store URL has no credentials"),
		finding("RZ-CFG-039", 9, "listener \"web\": port 8080 in use"),
	), func(_ context.Context, in compile.Input) (Compiled, diag.List, error) {
		return r.comp.make(in), nil, nil
	})
	r.start()
	r.booted()
	want := []string{"no_revision/", "secrets_unresolved/RZ-CFG-026", "listeners_unbound/RZ-CFG-039"}
	if got := r.reasons(); !slices.Equal(got, want) {
		t.Fatalf("reasons %v, want %v", got, want)
	}
	if d := r.ready.Readyz().Reasons[1].Detail; d != "file:/run/secrets/tls.pem is unreadable" {
		t.Errorf("detail %q", d)
	}
	// The secret appears: the retry activates and the Node is ready.
	r.clk.Advance(RetryMin)
	r.waitActive("A")
	if !r.ready.Ready() {
		t.Fatalf("/readyz %+v after the retry activated", r.ready.Readyz())
	}
}

func TestBootStoreURLRejectionReadiness(t *testing.T) {
	// A boot load refused only for its State Store URL (RZ-CFG-042) is
	// retried, and /readyz names nothing for it beside no_revision.
	r := newRig(t)
	r.comp.script(rev("A").Digest, findings(
		finding("RZ-CFG-042", 5, "the State Store URL has no credentials"),
	), func(_ context.Context, in compile.Input) (Compiled, diag.List, error) {
		return r.comp.make(in), nil, nil
	})
	r.start()
	r.booted()
	if got := r.reasons(); !slices.Equal(got, []string{"no_revision/"}) {
		t.Fatalf("reasons %v, want no_revision only", got)
	}
	r.clk.Advance(RetryMin)
	r.waitActive("A")
	if !r.ready.Ready() {
		t.Fatalf("/readyz %+v after the retry activated", r.ready.Readyz())
	}
}

func TestBootDeterministicRejectionReadiness(t *testing.T) {
	// A boot load that is not retried leaves only no_revision.
	r := newRig(t)
	r.comp.script(rev("A").Digest, findings(finding("RZ-CFG-040", 2, "unserved")))
	r.start()
	r.booted()
	if got := r.reasons(); !slices.Equal(got, []string{"no_revision/"}) {
		t.Fatalf("reasons %v, want no_revision only", got)
	}
	if recs := r.logs.find("no usable configuration"); len(recs) != 1 {
		t.Fatalf("records %+v", recs)
	}
}

func TestNewerFailureNeverChangesReadiness(t *testing.T) {
	r := newRig(t)
	r.comp.script(rev("B").Digest, findings(finding("RZ-CFG-026", 1, "missing secret"), finding("RZ-CFG-039", 2, "port in use")))
	r.start()
	r.booted()
	syncs := r.notifier.n.Load()
	r.l.Offer(load(result("B")))
	waitFor(t, "the rejection", func() bool { return r.metrics.rejections("RZ-CFG-026") == 1 })
	r.idle()
	if !r.ready.Ready() || r.ready.Readyz().Revision != rev("A").Digest.Short() {
		t.Fatalf("/readyz %+v, want ready with A", r.ready.Readyz())
	}
	if r.notifier.n.Load() != syncs {
		t.Error("a failed newer Revision synced STATUS=")
	}
}

func TestLoadPastTheDeadlineReloads(t *testing.T) {
	r := newRig(t)
	r.start()
	r.booted()
	r.l.Offer(source.Load{Err: source.ErrDeadline, Trigger: source.TriggerSettled})
	waitFor(t, "the rejection", func() bool { return r.metrics.rejections("RZ-CFG-044") == 1 })
	if lr := r.l.Snapshots().LastRejected; lr == nil || lr.Digest != nil {
		t.Fatalf("lastRejected = %+v", lr)
	}
	r.clk.Advance(RetryMin)
	waitFor(t, "the source to load again", func() bool {
		_, reloads, _ := r.src.counts()
		return reloads == 1
	})
}

func TestLoadPanicIsAFault(t *testing.T) {
	r := newRig(t)
	r.start()
	r.booted()
	r.l.Offer(source.Load{Err: &source.PanicError{Value: "stage F", Stack: []byte("goroutine 1 [running]:\n")}})
	waitFor(t, "the rejection", func() bool { return r.metrics.rejections("RZ-CFG-045") == 1 })
	recs := r.logs.find("fault while activating")
	if len(recs) != 1 || recs[0].level != 8 || recs[0].attrs["stack"] != "goroutine 1 [running]:\n" {
		t.Fatalf("fault records %+v, want one error record with the stack", recs)
	}
}

func TestCompileDeadline(t *testing.T) {
	// A compile still running when what remains of the 60 s deadline after
	// the load expires is rejected with the transient RZ-CFG-044.
	r := newRig(t)
	started := make(chan struct{})
	r.comp.script(rev("B").Digest, func(ctx context.Context, _ compile.Input) (Compiled, diag.List, error) {
		close(started)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}, func(_ context.Context, in compile.Input) (Compiled, diag.List, error) {
		return r.comp.make(in), nil, nil
	})
	r.start()
	r.booted()
	ld := load(result("B"))
	ld.Spent = 20 * time.Second
	r.l.Offer(ld)
	<-started
	r.clk.Advance(40*time.Second - time.Millisecond)
	if n := r.metrics.rejections("RZ-CFG-044"); n != 0 {
		t.Fatal("rejected before the deadline")
	}
	r.clk.Advance(time.Millisecond)
	waitFor(t, "the deadline rejection", func() bool { return r.metrics.rejections("RZ-CFG-044") == 1 })
	recs := r.logs.find("revision rejected")
	if len(recs) != 1 || recs[0].attrs["retry_in"] == nil {
		t.Fatalf("records %+v, want a retried rejection", recs)
	}
	r.clk.Advance(RetryMin)
	r.waitActive("B")
}

func TestCompilePanicIsRecovered(t *testing.T) {
	r := newRig(t)
	r.comp.script(rev("B").Digest, func(context.Context, compile.Input) (Compiled, diag.List, error) {
		panic("broken compile")
	})
	r.start()
	r.booted()
	r.l.Offer(load(result("B")))
	waitFor(t, "the fault", func() bool { return r.metrics.rejections("RZ-CFG-045") == 1 })
	r.idle()
	if r.active() != rev("A").Digest {
		t.Fatal("the Node stopped serving A")
	}
	recs := r.logs.find("fault while activating")
	if len(recs) != 1 || recs[0].attrs["stack"] == nil {
		t.Fatalf("records %+v, want the stack", recs)
	}
	// Deterministic once reproduced: not retried.
	r.clk.Advance(2 * RetryMax)
	r.idle()
	if n := len(r.comp.calls()); n != 2 {
		t.Fatalf("compiled %d times", n)
	}
}

func TestErrorWithoutCodeIsAFault(t *testing.T) {
	r := newRig(t)
	r.comp.script(rev("B").Digest, func(context.Context, compile.Input) (Compiled, diag.List, error) {
		return nil, nil, errors.New("index out of range")
	})
	r.start()
	r.booted()
	r.l.Offer(load(result("B")))
	waitFor(t, "the fault", func() bool { return r.metrics.rejections("RZ-CFG-045") == 1 })
	if recs := r.logs.find("fault while activating"); len(recs) != 1 || recs[0].attrs["error"] != "index out of range" {
		t.Fatalf("records %+v", recs)
	}
}

func TestPrepareFailureRejectsWithItsCode(t *testing.T) {
	// A port that cannot be bound rejects the Revision with the transient
	// RZ-CFG-039; the Parts readied before abort, the snapshot is
	// discarded, the active Revision keeps serving.
	r := newRig(t)
	r.start()
	r.booted()
	r.parts[1].set(func(p *fakePart) { p.prepareErr = errorsWrap(codeBind, "admin port 9902: address in use") })
	r.l.Offer(load(result("B")))
	waitFor(t, "the rejection", func() bool { return r.metrics.rejections("RZ-CFG-039") == 1 })
	r.idle()
	b := rev("B").Digest.Short()
	if !r.ev.has("abort:listeners:"+b) || r.ev.has("activate:"+b) {
		t.Fatalf("events %v, want the listeners aborted and B never activated", r.ev.all())
	}
	if c := r.comp.last(); c.discarded.Load() != 1 {
		t.Fatal("the rejected snapshot was not discarded")
	}
	if r.active() != rev("A").Digest {
		t.Fatal("the active Revision changed")
	}
	lr := r.l.Snapshots().LastRejected
	if lr == nil || lr.Code != "RZ-CFG-039" {
		t.Fatalf("lastRejected %+v", lr)
	}
	// Retried: the port frees.
	r.parts[1].set(func(p *fakePart) { p.prepareErr = nil })
	r.clk.Advance(RetryMin)
	r.waitActive("B")
}

func TestPrepareErrorWithoutCodeIsAFault(t *testing.T) {
	r := newRig(t)
	r.start()
	r.booted()
	r.parts[0].set(func(p *fakePart) { p.prepareErr = errors.New("unexpected") })
	r.l.Offer(load(result("B")))
	waitFor(t, "the fault", func() bool { return r.metrics.rejections("RZ-CFG-045") == 1 })
}
