// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/lkg"
	"github.com/ravindu-rev/ruralz/internal/gateway/readiness"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/source"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Stages of ruralz_config_activation_duration_seconds.
const (
	stageVerify  = "verify"
	stageCompile = "compile"
	stageSwap    = "swap"
	stageTotal   = "total"
)

// candidate is one Revision the configuration pipeline accepted, on its
// way to activation.
type candidate struct {
	// res is the pipeline's run; res.Validated holds the Revision.
	res    *pipeline.Result
	origin Origin
	digest revision.Digest
	// trigger is why a source load ran.
	trigger source.Trigger
	// spent is the gate time the load and verification used, which counts
	// against the compile deadline; verify is the verification's part.
	spent, verify time.Duration
	// verified is set for canonical content, whose digest was checked; a
	// watched directory is unsigned and has no verify stage.
	verified bool
	// attempt counts the retries made after transient rejections; due is
	// when the next may run.
	attempt int
	due     time.Time
	// counted is set once its rejection counted.
	counted bool
}

// routes returns the Revision's Route count.
func (c *candidate) routes() int {
	if c.res == nil || c.res.Validated == nil || c.res.Validated.Bundle == nil {
		return 0
	}
	return len(c.res.Validated.Bundle.Routes())
}

// sizeClass is the size_class label of a Revision with n Routes.
func sizeClass(n int) string {
	switch {
	case n <= 1000:
		return "le1000"
	case n <= 10000:
		return "le10000"
	default:
		return "gt10000"
	}
}

// activate runs the activation of c, off the request path, in the order
// of docs/architecture/03-data-plane.md "Activation": compile (gate steps
// 7, 9 and 10), ready every Part (step 11, listeners bound), publish the
// snapshot with one atomic pointer store, switch the Node to it, commit
// the Parts, report readiness, write the candidate under lkg/ and let the
// retirer retire the previous snapshot. Everything up to the swap runs
// under what remains of the compile deadline after the load. It reports
// whether c activated; a rejection is handled here. A panic, or an error
// without an RZ code, is recovered at this boundary, logged with its
// stack, and rejects c with RZ-CFG-045; an activation Stop abandons
// before its swap ends quietly.
func (l *Loader) activate(ctx context.Context, c *candidate) (ok bool) {
	l.mark()
	start := l.clk.Now()
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := l.clk.AfterFunc(max(l.deadline-c.spent, 0), func() { cancel(errDeadline) })
	defer timer.Stop()
	var (
		cmp       Compiled
		sws       []Switch
		published bool
	)
	defer func() {
		if v := recover(); v != nil {
			stack := debug.Stack()
			abortAll(sws)
			if cmp != nil && !published {
				cmp.Discard()
			}
			l.reject(ctx, c, faultRejection(c.digest, fmt.Errorf("panic: %v", v), stack))
			ok = false
		}
	}()

	// Steps 7, 9 and 10.
	l.mu.Lock()
	deferred := l.deferred
	l.mu.Unlock()
	cmp, ds, err := l.cfg.Compiler.Compile(actx, compile.Input{
		Validated: c.res.Validated, Files: c.res.Files, Previous: l.cfg.Holder.Active(),
	})
	if cmp == nil && err == nil && len(ds) == 0 {
		err = errors.New("reload: the compiler returned neither a snapshot nor a finding")
	}
	if err != nil || len(ds) > 0 {
		if cmp != nil {
			cmp.Discard()
		}
		l.failed(ctx, actx, c, ds, err)
		return false
	}
	compiled := l.clk.Now()

	// A handover successor's held switches give way to the new ones.
	if deferred {
		l.mu.Lock()
		held := l.held
		l.held = nil
		l.mu.Unlock()
		abortAll(held)
	}
	// Step 11 and the other Parts.
	for _, p := range l.cfg.Parts {
		sw, perr := p.Prepare(actx, cmp)
		if perr != nil {
			abortAll(sws)
			cmp.Discard()
			l.failed(ctx, actx, c, nil, perr)
			return false
		}
		sws = append(sws, sw)
	}
	if cause := context.Cause(actx); cause != nil {
		abortAll(sws)
		cmp.Discard()
		l.failed(ctx, actx, c, nil, cause)
		return false
	}

	// Step 4 of Activation: one atomic pointer store.
	snap := cmp.Snapshot()
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		abortAll(sws)
		cmp.Discard()
		return false
	}
	if l.cfg.Meter != nil && snap.Metrics != nil {
		snap.Binding = l.cfg.Meter.Bind(snap.Metrics)
	}
	_, perr := l.cfg.Holder.Publish(snap)
	if perr != nil {
		l.mu.Unlock()
		if snap.Binding != nil {
			snap.Binding.Release()
		}
		abortAll(sws)
		cmp.Discard()
		l.unpublished(ctx, c, perr)
		return false
	}
	published = true
	prev := l.active
	l.active, l.origin = c.res.Validated.Revision, c.origin
	if l.next != nil && l.next.digest == c.digest {
		l.next = nil
	}
	if deferred {
		l.held = sws
	}
	l.setLKGBootLocked(c.origin == OriginLastKnownGood)
	l.setHeaderLimitCappedLocked(snap.Gateway.Limits.HeaderLimitCapped)
	l.mu.Unlock()
	cmp.Activate()

	var unbound []readiness.Issue
	if !deferred {
		unbound = commitAll(ctx, l, sws)
	}
	swapped := l.clk.Now()

	// Readiness: secrets and listeners first, then the Revision, so
	// /readyz never reads ready between two updates. A handover
	// successor's listeners stay not listening until Listen.
	l.cfg.Readiness.SetSecrets()
	if !deferred {
		l.cfg.Readiness.SetListeners(unbound...)
	}
	l.cfg.Readiness.SetRevision(c.digest)
	l.syncStatus(ctx)

	l.recordActivation(ctx, c, prev, start, compiled, swapped, cmp)
	// Step 6: the candidate, which file mode promotes to Last-Known-Good.
	l.writeLKG(ctx)
	return true
}

// failed rejects c after an activation step failed with findings ds or
// error err, unless Stop abandoned it: an error past the deadline is
// RZ-CFG-044, one with an RZ code a finding of that code, any other a
// fault (RZ-CFG-045).
func (l *Loader) failed(ctx, actx context.Context, c *candidate, ds diag.List, err error) {
	cause := context.Cause(actx)
	switch {
	case errors.Is(cause, errStopped) || l.isStopped():
		return
	case len(ds) > 0:
		l.reject(ctx, c, findingsRejection(c.digest, ds))
	case errors.Is(cause, errDeadline):
		l.reject(ctx, c, deadlineRejection(c.digest, l.deadline))
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		// The loop is ending.
		return
	case errors.As(err, new(*source.PanicError)):
		var pe *source.PanicError
		errors.As(err, &pe)
		l.reject(ctx, c, faultRejection(c.digest, pe, pe.Stack))
	default:
		if code, ok := errcode.CodeOf(err); ok && code != codeFault {
			l.reject(ctx, c, findingsRejection(c.digest, diag.List{{
				Code: code, Severity: diag.SeverityError, Resource: gatewayOf(c), Message: err.Error(),
			}}))
			return
		}
		l.reject(ctx, c, faultRejection(c.digest, err, nil))
	}
}

// unpublished handles a Publish the retirer refused: closed by the Drain,
// it ends activations for good; at its retirement bound, c waits again.
func (l *Loader) unpublished(ctx context.Context, c *candidate, err error) {
	switch {
	case errors.Is(err, retire.ErrClosed):
		l.mu.Lock()
		l.closed = true
		l.mu.Unlock()
	case errors.Is(err, retire.ErrBusy):
		l.mu.Lock()
		if l.next == nil && !l.stopped {
			l.next = c
		}
		l.mu.Unlock()
	default:
		l.reject(ctx, c, faultRejection(c.digest, err, nil))
	}
}

// commitAll commits sws after the swap and returns the listeners left
// unbound: a commit failing with RZ-CFG-039. Every failure is logged.
func commitAll(ctx context.Context, l *Loader, sws []Switch) []readiness.Issue {
	var unbound []readiness.Issue
	for _, sw := range sws {
		if err := sw.Commit(ctx); err != nil {
			code, _ := errcode.CodeOf(err)
			l.log.WarnContext(ctx, "an activated Revision's part did not switch", catalog.KeyCode, code, catalog.KeyError, err.Error())
			if code == codeBind {
				unbound = append(unbound, readiness.Issue{Code: code, Detail: err.Error()})
			}
		}
	}
	return unbound
}

// abortAll aborts every switch.
func abortAll(sws []Switch) {
	for _, sw := range sws {
		sw.Abort()
	}
}

// recordActivation counts, times and logs an activation.
func (l *Loader) recordActivation(ctx context.Context, c *candidate, prev revision.Revision, start, compiled, swapped time.Time, cmp Compiled) {
	class := sizeClass(c.routes())
	l.metrics.Activated.Add(0, 1)
	record := func(stage string, d time.Duration) {
		l.metrics.ActivationDuration(stage, class).Record(0, uint64(max(d, 0))) //nolint:gosec // G115: a non-negative duration.
	}
	if c.verified {
		record(stageVerify, c.verify)
	}
	record(stageCompile, c.spent-c.verify+compiled.Sub(start))
	record(stageSwap, swapped.Sub(compiled))
	record(stageTotal, c.spent+swapped.Sub(start))
	l.metrics.RevisionInfo("active", c.digest.Short())

	attrs := []any{
		catalog.KeyRevision, c.digest.Short(), "digest", c.digest.String(), "origin", c.origin.String(),
		"routes", c.routes(), "duration", (c.spent + swapped.Sub(start)).String(),
	}
	if !prev.Digest.IsZero() {
		attrs = append(attrs, "previous", prev.Digest.Short())
	}
	if c.trigger != source.TriggerNone {
		attrs = append(attrs, "trigger", c.trigger.String())
	}
	l.log.InfoContext(ctx, "revision activated", attrs...)
	if c.origin == OriginLastKnownGood {
		l.log.WarnContext(ctx, "serving Last-Known-Good: no source answered", catalog.KeyRevision, c.digest.Short(),
			catalog.KeyReason, catalog.ReasonLKGBoot.String())
	}
	for _, w := range cmp.Warnings() {
		wa := []any{catalog.KeyRevision, c.digest.Short()}
		if w.Resource != nil {
			wa = append(wa, catalog.KeyResourceKind, w.Resource.Kind, catalog.KeyResourceName, w.Resource.Name)
		}
		l.log.WarnContext(ctx, w.Message, wa...)
	}
	var warnings diag.List
	for _, d := range c.res.Diagnostics {
		if d.Severity == diag.SeverityWarning {
			warnings = append(warnings, d)
		}
	}
	if len(warnings) > 0 {
		l.log.WarnContext(ctx, "activated revision has warnings", catalog.KeyRevision, c.digest.Short(),
			"diagnostics", len(warnings), "findings", texts(warnings, maxLoggedDiagnostics))
		for _, d := range warnings[min(len(warnings), maxLoggedDiagnostics):] {
			l.log.DebugContext(ctx, "activated revision warning", catalog.KeyRevision, c.digest.Short(), "finding", string(d.AppendText(nil)))
		}
	}
	l.checkFileLimit(ctx, cmp.UpstreamConnections())
}

// setLKGBootLocked raises lkg_boot while the active Revision is
// Last-Known-Good and clears it once a source Revision activates or a
// source load renders the active Revision.
func (l *Loader) setLKGBootLocked(on bool) {
	if l.lkgBoot == on {
		return
	}
	l.lkgBoot = on
	if l.cfg.Status != nil {
		l.cfg.Status.SetDegraded(catalog.ReasonLKGBoot, statusSource, on)
	}
}

// setHeaderLimitCappedLocked raises header_limit_capped while the active
// Revision's limits.maxRequestHeaderBytes exceeds the 256 KiB process
// ceiling, which caps it (docs/architecture/03-data-plane.md "Listeners
// and protocols"), and clears it once a Revision within the ceiling
// activates.
func (l *Loader) setHeaderLimitCappedLocked(on bool) {
	if l.headerCapped == on {
		return
	}
	l.headerCapped = on
	if l.cfg.Status != nil {
		l.cfg.Status.SetDegraded(catalog.ReasonHeaderLimitCapped, statusSource, on)
	}
}

// syncStatus sends STATUS= after a readiness change.
func (l *Loader) syncStatus(ctx context.Context) {
	if l.cfg.Notifier == nil {
		return
	}
	if err := l.cfg.Notifier.SyncStatus(ctx); err != nil {
		l.log.DebugContext(ctx, "STATUS= not sent", catalog.KeyError, err.Error())
	}
}

// isStopped reports whether Stop ran.
func (l *Loader) isStopped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopped
}

// lock returns the data-directory lock the process holds, nil otherwise.
func (l *Loader) lock() *nodedir.Lock {
	if l.cfg.Lock == nil {
		return nil
	}
	return l.cfg.Lock()
}

// lkgOwedLocked reports whether lkg/ does not name the active Revision
// yet: a failed write to retry, or a handover successor that took the
// lock.
func (l *Loader) lkgOwedLocked() bool {
	d := l.active.Digest
	return !d.IsZero() && (l.lkgCandidate != d || l.lkgPointer != d)
}

// writeLKG writes the active Revision under lkg/ as the candidate and, in
// file mode, Last-Known-Good, when the process holds the lock. A failed
// write retries after lkg.RetryDelay; the Store raises lkg_write_failed
// meanwhile. Without the lock nothing is written, and nothing is owed
// until it is held.
func (l *Loader) writeLKG(ctx context.Context) {
	lock := l.lock()
	l.mu.Lock()
	rev, stopped := l.active, l.stopped
	l.mu.Unlock()
	if lock == nil || stopped || rev.Digest.IsZero() {
		return
	}
	err := l.cfg.LKG.Write(lock, rev, true)
	now := l.clk.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case err == nil:
		l.lkgCandidate, l.lkgPointer = rev.Digest, rev.Digest
		l.lkgFailures, l.lkgDue = 0, time.Time{}
		l.metrics.RevisionInfo("lkg", rev.Digest.Short())
		l.log.DebugContext(ctx, "Last-Known-Good written", catalog.KeyRevision, rev.Digest.Short())
	case errors.Is(err, lkg.ErrNotHolder), errors.Is(err, nodedir.ErrReleased):
	default:
		l.lkgFailures++
		wait := lkg.RetryDelay(l.lkgFailures)
		l.lkgDue = now.Add(wait)
		l.log.WarnContext(ctx, "Last-Known-Good write failed", catalog.KeyRevision, rev.Digest.Short(),
			catalog.KeyError, err.Error(), catalog.KeyReason, catalog.ReasonLKGWriteFailed.String(), "retry_in", wait.String())
	}
}

// initPointers reads the digests lkg/ names at start.
func (l *Loader) initPointers(ctx context.Context) {
	cand, last, err := l.cfg.LKG.Pointers()
	if err != nil {
		l.log.WarnContext(ctx, "Last-Known-Good pointers unreadable", catalog.KeyError, err.Error())
	}
	l.mu.Lock()
	l.lkgCandidate, l.lkgPointer = cand, last
	l.mu.Unlock()
	if !last.IsZero() {
		l.metrics.RevisionInfo("lkg", last.Short())
	}
}

// gatewayOf returns the Gateway resource of c's Revision, the resource an
// activation-wide finding names: a listener or the admin port.
func gatewayOf(c *candidate) *diag.ResourceID {
	if c == nil || c.res == nil || c.res.Validated == nil || c.res.Validated.Bundle == nil {
		return nil
	}
	for _, r := range c.res.Validated.Bundle.Resources() {
		if r.Kind == v1alpha1.KindGateway {
			return r.ResourceID()
		}
	}
	return nil
}
