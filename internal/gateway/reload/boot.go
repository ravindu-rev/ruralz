// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"errors"
	"runtime/debug"

	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/gateway/lkg"
	"github.com/ravindu-rev/ruralz/internal/gateway/source"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// boot is the first job: the boot order of foundation pack section 8.2,
// then the end of the first boot attempt (Booted).
func (l *Loader) boot(ctx context.Context) {
	defer func() {
		l.mu.Lock()
		l.bootDone = true
		l.mu.Unlock()
		close(l.booted)
	}()
	switch l.cfg.Boot {
	case BootHolder:
		l.bootFileOrder(ctx)
	default:
		l.bootSuccessor(ctx)
	}
	if ctx.Err() != nil {
		return
	}
	l.mu.Lock()
	none, issues := l.active.Digest.IsZero(), l.bootIssues
	l.mu.Unlock()
	if !none {
		return
	}
	l.log.WarnContext(ctx, "no usable configuration: the Node stays not ready until a Revision activates")
	if !issues {
		// No boot load is being retried: /readyz names no_revision alone.
		l.cfg.Readiness.SetSecrets()
		l.cfg.Readiness.SetListeners()
		l.syncStatus(ctx)
	}
}

// bootFileOrder boots by the file-mode order: the source, else
// Last-Known-Good, else nothing. A source load whose every failure is
// transient keeps being retried after Last-Known-Good activated, so a
// secret file that appears later or a port that frees reaches service
// without a restart.
func (l *Loader) bootFileOrder(ctx context.Context) {
	l.mark()
	ld := l.cfg.Source.Load(ctx)
	if ctx.Err() != nil {
		return
	}
	var rejected revision.Digest
	if c, ok := l.fromLoad(ctx, ld); ok {
		if l.activate(ctx, c) {
			return
		}
		rejected = c.digest
	}
	if ctx.Err() != nil || l.isStopped() {
		return
	}
	l.bootLKG(ctx, rejected)
}

// fromLoad turns a boot load into a candidate, handling a load that has
// none: a source that cannot be read is logged, a failed load rejected.
func (l *Loader) fromLoad(ctx context.Context, ld source.Load) (*candidate, bool) {
	switch {
	case ld.Unavailable():
		l.log.WarnContext(ctx, "configuration source cannot be read", catalog.KeyError, errText(ld.Err))
		return nil, false
	case ld.Err != nil:
		l.loadFailed(ctx, ld)
		return nil, false
	}
	res := ld.Result
	if res.HasErrors() || res.Validated == nil {
		r := findingsRejection(revision.Digest{}, res.Diagnostics)
		if !res.HasErrors() {
			r = faultRejection(revision.Digest{}, errors.New("reload: the pipeline returned no Revision and no error"), nil)
		}
		l.reject(ctx, nil, r)
		return nil, false
	}
	return &candidate{res: res, origin: OriginSource, digest: res.Validated.Revision.Digest, trigger: ld.Trigger, spent: ld.Spent}, true
}

// bootLKG boots Last-Known-Good under degraded reason lkg_boot, unless
// there is none or it is the Revision the source just failed with, which
// would fail alike.
func (l *Loader) bootLKG(ctx context.Context, rejected revision.Digest) {
	e, err := l.cfg.LKG.LastKnownGood()
	switch {
	case errors.Is(err, lkg.ErrAbsent):
		l.log.InfoContext(ctx, "no Last-Known-Good to boot")
		return
	case err != nil:
		l.log.WarnContext(ctx, "Last-Known-Good cannot be read; it boots nothing", catalog.KeyError, err.Error())
		return
	case !rejected.IsZero() && e.Digest == rejected:
		l.log.InfoContext(ctx, "Last-Known-Good is the rejected source Revision; it boots nothing", catalog.KeyRevision, e.Digest.Short())
		return
	}
	if c, ok := l.fromCanonical(ctx, e, OriginLastKnownGood); ok {
		l.activate(ctx, c)
	}
}

// bootSuccessor boots a handover successor: the holder's active Revision
// from its candidate, verified and through the full gate, without a boot
// wait; when there is no candidate, the holder has no active Revision, so
// the file-mode order instead. A candidate that fails boots nothing: the
// holder then refuses the handover or asks for a reload.
func (l *Loader) bootSuccessor(ctx context.Context) {
	l.mark()
	e, err := l.cfg.LKG.Candidate()
	switch {
	case err == nil:
		if c, ok := l.fromCanonical(ctx, e, OriginCandidate); ok {
			l.activate(ctx, c)
		}
	case errors.Is(err, lkg.ErrAbsent):
		l.bootFileOrder(ctx)
	default:
		l.log.WarnContext(ctx, "the holder's candidate cannot be read; it boots nothing", catalog.KeyError, err.Error())
	}
}

// reload boots the holder's newer candidate of digest d through the full
// gate, replacing the Revision booted before: a handover successor's
// answer to the holder's reload message.
func (l *Loader) reload(ctx context.Context, d revision.Digest) error {
	if l.isStopped() {
		return ErrStopped
	}
	l.mark()
	e, err := l.cfg.LKG.Revision(d)
	if err != nil {
		return err
	}
	c, ok := l.fromCanonical(ctx, e, OriginCandidate)
	if !ok {
		return errRejected(d)
	}
	if !l.activate(ctx, c) {
		return errRejected(d)
	}
	return nil
}

// errRejected is a reload whose Revision did not activate.
func errRejected(d revision.Digest) error {
	return &RejectedError{Digest: d}
}

// RejectedError reports a handover reload whose candidate did not
// activate; its rejection was logged and counted.
type RejectedError struct {
	Digest revision.Digest
}

// Error names the Revision.
func (e *RejectedError) Error() string {
	return "reload: the candidate " + e.Digest.Short() + " was rejected"
}

// fromCanonical verifies and re-validates canonical content read from
// lkg/: the full sha256 against the digest that names it (verify stage;
// RZ-CFG-027 on a mismatch), then stages F to M (RZ-CFG-024 for content a
// newer release wrote, RZ-CFG-001 past the size limit), under the compile
// deadline. A failure is rejected here.
func (l *Loader) fromCanonical(ctx context.Context, e lkg.Entry, origin Origin) (*candidate, bool) {
	start := l.clk.Now()
	matches := revision.Sum(e.Content) == e.Digest
	verify := l.clk.Since(start)
	req := pipeline.CanonicalRequest{Content: e.Content, Name: e.Path}
	if !matches {
		// The pipeline locates the mismatch.
		req.Digest = e.Digest
	}
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := l.clk.AfterFunc(max(l.deadline-verify, 0), func() { cancel(errDeadline) })
	defer timer.Stop()
	res, err := l.runCanonical(cctx, req)
	spent := l.clk.Since(start)
	c := &candidate{res: res, origin: origin, digest: e.Digest, spent: spent, verify: verify, verified: true}
	switch {
	case err != nil:
		l.failed(ctx, cctx, c, nil, err)
		return nil, false
	case res == nil:
		l.reject(ctx, c, faultRejection(e.Digest, errors.New("reload: the pipeline returned no result"), nil))
		return nil, false
	case res.HasErrors() || res.Validated == nil:
		r := findingsRejection(e.Digest, res.Diagnostics)
		if !res.HasErrors() {
			r = faultRejection(e.Digest, errors.New("reload: the pipeline returned no Revision and no error"), nil)
		}
		l.reject(ctx, c, r)
		return nil, false
	}
	c.digest = res.Validated.Revision.Digest
	return c, true
}

// runCanonical runs the pipeline on canonical content, recovering a panic
// as a fault.
func (l *Loader) runCanonical(ctx context.Context, req pipeline.CanonicalRequest) (res *pipeline.Result, err error) {
	defer func() {
		if v := recover(); v != nil {
			res, err = nil, &source.PanicError{Value: v, Stack: debug.Stack()}
		}
	}()
	return l.cfg.Pipeline.FromCanonical(ctx, req)
}
