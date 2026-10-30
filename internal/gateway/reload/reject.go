// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/gateway/readiness"
	"github.com/ravindu-rev/ruralz/internal/gateway/source"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// rejection is why a load did not activate: the findings of the step that
// failed, sorted as diagnostics, and for a fault the error and stack to
// log.
type rejection struct {
	// digest is the rejected Revision's, zero when the load failed before
	// one.
	digest revision.Digest
	diags  diag.List
	err    error
	stack  []byte
}

// code returns the code of the first error, the rejection's code.
func (r rejection) code() string {
	if c := r.diags.FirstErrorCode(); c != "" {
		return c
	}
	return codeFault
}

// transient reports a rejection whose every error is transient in the
// validation gate (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "Validation gate", last column): a secretRef that does not resolve
// (RZ-CFG-026) or a State Store URL this Node refuses (RZ-CFG-042),
// values only this Node resolves; a port that cannot be bound
// (RZ-CFG-039); the compile deadline (RZ-CFG-044). Such a load is retried
// until it activates or the source changes.
func (r rejection) transient() bool {
	errs := 0
	for _, d := range r.diags {
		if d.Severity == diag.SeverityWarning {
			continue
		}
		errs++
		switch d.Code {
		case codeSecret, codeStoreURL, codeBind, codeDeadline:
		default:
			return false
		}
	}
	return errs > 0
}

// findingsRejection rejects digest with ds, sorted.
func findingsRejection(digest revision.Digest, ds diag.List) rejection {
	ds = append(diag.List(nil), ds...)
	ds.Sort()
	return rejection{digest: digest, diags: ds}
}

// deadlineRejection is the transient RZ-CFG-044 of a Revision not ready
// to publish at the compile deadline; it names no resource.
func deadlineRejection(digest revision.Digest, deadline time.Duration) rejection {
	return rejection{digest: digest, diags: diag.List{{
		Code: codeDeadline, Severity: diag.SeverityError,
		Message: fmt.Sprintf("the Revision was not ready to publish within the compile deadline of %v; the active Revision keeps serving", deadline),
	}}}
}

// faultRejection is the RZ-CFG-045 of a panic, or an error without an RZ
// code, while the Node validated, compiled or activated a Revision; it
// names no resource, and the fault is logged with its stack.
func faultRejection(digest revision.Digest, err error, stack []byte) rejection {
	return rejection{digest: digest, err: err, stack: stack, diags: diag.List{{
		Code: codeFault, Severity: diag.SeverityError,
		Message: "an internal fault while validating, compiling or activating the Revision; the active Revision keeps serving",
	}}}
}

// reject handles a rejected load: it records lastRejected, counts the
// load once in ruralz_config_activations_total with the code of its first
// error and writes one warn record with the number of diagnostics and up
// to ten of them, the rest at debug; it logs a fault with its stack. A
// source load whose every failure is transient is retried with
// full-jitter backoff, unless a newer load replaced it; while no Revision
// is active, the reasons of /readyz name the unresolved secretRef values
// and unbound listeners of such a boot load, and nothing for one that is
// not retried. c is nil for a load that failed before it had a Revision.
func (l *Loader) reject(ctx context.Context, c *candidate, r rejection) {
	now := l.clk.Now()
	code := r.code()
	transient := r.transient()
	l.mu.Lock()
	first := c == nil || !c.counted
	if c != nil {
		c.counted = true
	}
	rej := &adminapi.Rejection{Code: code, At: stamp(now)}
	if !r.digest.IsZero() {
		d := r.digest.String()
		rej.Digest = &d
	}
	l.lastRejected = rej
	noActive := l.active.Digest.IsZero()
	var retryIn time.Duration
	if transient && c != nil && c.origin == OriginSource && l.next == nil && !l.stopped {
		c.attempt++
		retryIn = l.backoffLocked(c.attempt)
		c.due = now.Add(retryIn)
		l.next = c
	}
	l.mu.Unlock()

	attrs := []any{catalog.KeyCode, code, "diagnostics", len(r.diags)}
	if !r.digest.IsZero() {
		attrs = append(attrs, catalog.KeyRevision, r.digest.Short(), "digest", r.digest.String())
	}
	if c != nil {
		attrs = append(attrs, "origin", c.origin.String())
		if c.attempt > 0 && !first {
			attrs = append(attrs, "attempt", c.attempt)
		}
	}
	if retryIn > 0 {
		attrs = append(attrs, "retry_in", retryIn.String())
	}
	if r.err != nil {
		attrs = append(attrs, catalog.KeyError, r.err.Error())
	}
	if r.stack != nil {
		l.log.ErrorContext(ctx, "fault while activating a revision", catalog.KeyCode, code, catalog.KeyError, errText(r.err),
			catalog.KeyStack, string(r.stack))
	} else if r.err != nil && code == codeFault {
		l.log.ErrorContext(ctx, "fault while activating a revision", catalog.KeyCode, code, catalog.KeyError, r.err.Error())
	}
	if first {
		l.metrics.Rejected.Inc(0, code)
		l.log.WarnContext(ctx, "revision rejected", append(attrs, "findings", texts(r.diags, maxLoggedDiagnostics))...)
		for _, d := range r.diags[min(len(r.diags), maxLoggedDiagnostics):] {
			l.log.DebugContext(ctx, "revision rejected: finding", catalog.KeyCode, d.Code, "finding", string(d.AppendText(nil)))
		}
	} else {
		l.log.InfoContext(ctx, "revision retry rejected", append(attrs, "findings", texts(r.diags, maxLoggedDiagnostics))...)
	}

	if noActive && (c == nil || c.origin == OriginSource) {
		var secrets, listeners []readiness.Issue
		if transient && retryIn > 0 {
			secrets, listeners = bootIssues(r.diags)
		}
		l.mu.Lock()
		l.bootIssues = true
		l.mu.Unlock()
		l.cfg.Readiness.SetSecrets(secrets...)
		l.cfg.Readiness.SetListeners(listeners...)
		l.syncStatus(ctx)
	}
}

// bootIssues returns the readiness issues of a retried boot load, the
// reasons docs/architecture/03-data-plane.md "Admin endpoints" defines
// for /readyz: each unresolved secretRef (secrets_unresolved, RZ-CFG-026)
// and each unbound listener (listeners_unbound, RZ-CFG-039). A State
// Store URL this Node refuses (RZ-CFG-042) is retried but names no
// reason. Their details are the findings' messages, which never hold a
// secret value.
func bootIssues(ds diag.List) (secrets, listeners []readiness.Issue) {
	for _, d := range ds {
		switch d.Code {
		case codeSecret:
			secrets = append(secrets, readiness.Issue{Code: d.Code, Detail: d.Message})
		case codeBind:
			listeners = append(listeners, readiness.Issue{Code: d.Code, Detail: d.Message})
		}
	}
	return secrets, listeners
}

// texts returns the one-line text form of the first n diagnostics.
func texts(ds diag.List, n int) []string {
	out := make([]string, 0, min(len(ds), n))
	for _, d := range ds[:min(len(ds), n)] {
		out = append(out, string(d.AppendText(nil)))
	}
	return out
}

// errText returns err's text, "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Offer takes one load of the source: the Sink of the source Watcher. A
// source that cannot be read is logged and changes nothing. A load that
// failed is rejected; one past the compile deadline is loaded again after
// a full-jitter backoff. A Revision equal to the active one activates
// nothing, counts nothing and logs at debug; any other becomes the
// latest pending Revision, replacing any earlier one and its retries.
// Offers after Stop are ignored.
func (l *Loader) Offer(ld source.Load) {
	ctx := context.Background()
	if l.isStopped() {
		return
	}
	switch {
	case ld.Unavailable():
		l.log.WarnContext(ctx, "configuration source cannot be read; the active Revision keeps serving",
			catalog.KeyError, errText(ld.Err), "trigger", ld.Trigger.String())
		return
	case ld.Err != nil:
		l.loadFailed(ctx, ld)
		return
	}
	l.mu.Lock()
	l.reloadDue, l.reloadAttempt = time.Time{}, 0
	l.mu.Unlock()
	res := ld.Result
	if res.HasErrors() || res.Validated == nil {
		r := findingsRejection(revision.Digest{}, res.Diagnostics)
		if !res.HasErrors() {
			r = faultRejection(revision.Digest{}, errors.New("reload: the pipeline returned no Revision and no error"), nil)
		}
		l.reject(ctx, nil, r)
		return
	}
	c := &candidate{res: res, origin: OriginSource, digest: res.Validated.Revision.Digest, trigger: ld.Trigger, spent: ld.Spent}
	l.mu.Lock()
	if c.digest == l.active.Digest {
		l.next = nil
		l.setLKGBootLocked(false)
		l.mu.Unlock()
		l.log.DebugContext(ctx, "configuration source renders the active revision", catalog.KeyRevision, c.digest.Short(),
			"trigger", ld.Trigger.String())
		return
	}
	l.next = c
	l.mu.Unlock()
	l.wake()
}

// loadFailed rejects a load that has no result: one past the compile
// deadline (RZ-CFG-044), retried by loading the source again after a
// full-jitter backoff, or a fault (RZ-CFG-045).
func (l *Loader) loadFailed(ctx context.Context, ld source.Load) {
	var pe *source.PanicError
	switch {
	case errors.Is(ld.Err, source.ErrDeadline):
		l.mu.Lock()
		l.reloadAttempt++
		l.reloadDue = l.clk.Now().Add(l.backoffLocked(l.reloadAttempt))
		l.mu.Unlock()
		l.reject(ctx, nil, deadlineRejection(revision.Digest{}, l.deadline))
		l.wake()
	case errors.As(ld.Err, &pe):
		l.reject(ctx, nil, faultRejection(revision.Digest{}, pe, pe.Stack))
	default:
		l.reject(ctx, nil, faultRejection(revision.Digest{}, ld.Err, nil))
	}
}
