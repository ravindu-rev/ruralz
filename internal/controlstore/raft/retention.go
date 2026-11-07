// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/controlstore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// SealAudit seals the oldest unsealed entries firstSeq to lastSeq into a
// segment object of at most 4 MiB chained to the current head: the object
// reaches a replica quorum, then the seal entry commits.
func (s *Store) SealAudit(ctx context.Context, segment controlstore.Object, firstSeq, lastSeq uint64) error {
	if err := s.check(); err != nil {
		return err
	}
	if segment.Kind != controlstore.KindAuditSegment {
		return fmt.Errorf("%w: an audit segment is a %s object, not %s", controlstore.ErrInvalid, controlstore.KindAuditSegment, segment.Kind)
	}
	if err := segment.Validate(); err != nil {
		return err
	}
	var head controlstore.Digest
	s.machine.View(func(v controlstore.View) { head = v.AuditHead() })
	op := controlstore.SealAudit{Segment: segment.Digest(), Size: len(segment.Data), FirstSeq: firstSeq, LastSeq: lastSeq, Prev: head}
	_, err := s.propose(ctx, controlstore.Proposal{Content: []controlstore.Object{segment}, Ops: []controlstore.Op{op}})
	return err
}

// PruneAudit prunes, oldest first, the sealed segments whose newest entry
// is older than the audit retention and that a recorded backup holds;
// it returns how many.
func (s *Store) PruneAudit(ctx context.Context) (int, error) {
	if err := s.check(); err != nil {
		return 0, err
	}
	if s.leadership() == nil {
		return 0, s.notLeader()
	}
	n, _, err := s.pruneAudit(ctx)
	return n, err
}

// pruneAudit prunes the eligible prefix and returns its length and when
// the next held segment comes of age, zero when none waits on time.
func (s *Store) pruneAudit(ctx context.Context) (int, time.Time, error) {
	retention := s.cfg.AuditRetention
	now := s.clk.Now()
	var through controlstore.Digest
	n := 0
	var next time.Time
	s.machine.View(func(v controlstore.View) {
		b, held := v.Backup()
		for _, seg := range v.AuditSegments() {
			if !held || seg.SealIndex > b.Index {
				return
			}
			if due := seg.LastAt.Add(retention); due.After(now) {
				next = due
				return
			}
			through = seg.Digest
			n++
		}
	})
	if n == 0 {
		return 0, next, nil
	}
	if _, err := s.propose(ctx, controlstore.Proposal{Ops: []controlstore.Op{controlstore.PruneAudit{Through: through, Retention: retention}}}); err != nil {
		return 0, next, err
	}
	return n, next, nil
}

// auditLoop is the leader's audit retention: it prunes whenever a backup
// is recorded or a segment comes of age, at the exact time the oldest held
// segment passes the retention.
func (s *Store) auditLoop(ctx context.Context) {
	var timer clock.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	due := make(chan struct{}, 1)
	for {
		changed := s.machine.Changed()
		_, next, err := s.pruneAudit(ctx)
		if err != nil && ctx.Err() == nil && !errors.Is(err, controlstore.ErrRejected) {
			s.log.WarnContext(ctx, "audit retention", slog.String(catalog.KeyError, err.Error()))
		}
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		if !next.IsZero() {
			timer = s.clk.AfterFuncAt(next, func() { signal(due) })
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-due:
		}
	}
}

// PruneRevisions deletes the Revisions beyond the newest RevisionsKept of
// each Environment that no accepted assignment or promotion names and
// keep does not hold (active and Last-Known-Good digests Nodes report,
// rollback targets), one entry each; the state machine checks the rule
// again. It returns how many it deleted.
func (s *Store) PruneRevisions(ctx context.Context, keep func(env string, d controlstore.Digest) bool) (int, error) {
	if err := s.check(); err != nil {
		return 0, err
	}
	if s.leadership() == nil {
		return 0, s.notLeader()
	}
	type candidate struct {
		env string
		d   controlstore.Digest
	}
	var cands []candidate
	s.machine.View(func(v controlstore.View) {
		for _, env := range v.Environments() {
			revs := v.Revisions(env)
			for i := controlstore.RevisionsKept; i < len(revs); i++ {
				if !v.RevisionInUse(env, revs[i].Digest) {
					cands = append(cands, candidate{env: env, d: revs[i].Digest})
				}
			}
		}
	})
	n := 0
	for _, c := range cands {
		if keep != nil && keep(c.env, c.d) {
			continue
		}
		_, err := s.propose(ctx, controlstore.Proposal{Ops: []controlstore.Op{controlstore.DeleteRevision{Environment: c.env, Digest: c.d}}})
		switch {
		case err == nil:
			n++
		case errors.Is(err, controlstore.ErrRejected):
		default:
			return n, err
		}
	}
	return n, nil
}
