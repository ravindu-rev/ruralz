// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/controlstore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// FinalizeStatus evaluates the finalize conditions on committed state at
// the store's time.
func (s *Store) FinalizeStatus() (controlstore.FinalizeStatus, error) {
	if err := s.check(); err != nil {
		return controlstore.FinalizeStatus{}, err
	}
	return s.finalizeStatus(), nil
}

// finalizeStatus evaluates without the state check.
func (s *Store) finalizeStatus() controlstore.FinalizeStatus {
	var st controlstore.FinalizeStatus
	s.machine.View(func(v controlstore.View) {
		version, _ := v.Version()
		var last *controlstore.VersionAdvance
		if a, ok := v.LastAdvance(); ok {
			last = &a
		}
		st = controlstore.EvaluateFinalize(version, last, v.MemberIDs(), v.Reports(), s.clk.Now(), s.cfg.TimedSoak)
	})
	return st
}

// Finalize commits the version-advance entry, with its audit entry, once
// every replica reports N, the soak passed and every replica reported a
// snapshot past the previous version-advance entry; RZ-CP-021 otherwise,
// also when another entry moved the state between the check and the
// commit.
func (s *Store) Finalize(ctx context.Context, audit []byte) error {
	if err := s.check(); err != nil {
		return err
	}
	if s.leadership() == nil {
		return s.notLeader()
	}
	return s.finalize(ctx, audit)
}

// finalize checks and commits.
func (s *Store) finalize(ctx context.Context, audit []byte) error {
	st := s.finalizeStatus()
	if !st.Ready {
		return controlstore.Errorf(controlstore.ErrFinalize, "%s", strings.Join(st.Blockers, "; "))
	}
	ops := []controlstore.Op{controlstore.AdvanceVersion{From: st.Version, To: st.Target}}
	if len(audit) > 0 {
		ops = append(ops, controlstore.AppendAudit{Entries: [][]byte{audit}})
	}
	if _, err := s.propose(ctx, controlstore.Proposal{Ops: ops}); err != nil {
		if errors.Is(err, controlstore.ErrRejected) {
			return controlstore.Errorf(controlstore.ErrFinalize, "%w", err)
		}
		return err
	}
	return nil
}

// timedSoakLoop finalizes without an operator: once the conditions have
// held without a break for the timed soak's healthy time, the leader
// commits the version advance with the audit entry TimedFinalizeAudit
// builds. It re-evaluates after every applied index and when the due
// time arrives.
func (s *Store) timedSoakLoop(ctx context.Context) {
	var timer clock.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	due := make(chan struct{}, 1)
	for {
		changed := s.machine.Changed()
		st := s.finalizeStatus()
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		if !st.FinalizesAt.IsZero() {
			if st.Ready && !s.clk.Now().Before(st.FinalizesAt) {
				if err := s.finalize(ctx, s.cfg.TimedFinalizeAudit(st)); err != nil && ctx.Err() == nil {
					// Retry at the status-report cadence, as nothing may
					// commit meanwhile.
					s.log.WarnContext(ctx, "timed finalize", slog.String(catalog.KeyError, err.Error()))
					timer = s.clk.AfterFunc(ReportInterval, func() { signal(due) })
				}
			} else {
				timer = s.clk.AfterFuncAt(st.FinalizesAt, func() { signal(due) })
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-due:
		}
	}
}
