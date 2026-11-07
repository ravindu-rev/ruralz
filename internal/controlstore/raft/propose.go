// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"context"
	"errors"
	"fmt"

	hraft "github.com/hashicorp/raft"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
)

// Propose writes the proposal's content to a replica quorum, then commits
// its ops as one entry. Only client ops pass.
func (s *Store) Propose(ctx context.Context, p controlstore.Proposal) (controlstore.Result, error) {
	if err := s.check(); err != nil {
		return controlstore.Result{}, err
	}
	for _, op := range p.Ops {
		if op == nil || !controlstore.ClientOp(op) {
			return controlstore.Result{}, fmt.Errorf("%w: op %T is the store's own", controlstore.ErrInvalid, op)
		}
	}
	return s.propose(ctx, p)
}

// opContent returns the content an op references, each with the kind it
// must have; a zero kind takes any.
func opContent(op controlstore.Op) map[controlstore.Digest]controlstore.Kind {
	out := map[controlstore.Digest]controlstore.Kind{}
	switch o := op.(type) {
	case controlstore.Put:
		for _, d := range o.Refs {
			out[d] = 0
		}
	case controlstore.RecordRevision:
		out[o.Revision.Digest] = controlstore.KindRevision
		if !o.Revision.Source.IsZero() {
			out[o.Revision.Source] = controlstore.KindSource
		}
		if !o.Revision.Diff.IsZero() {
			out[o.Revision.Diff] = controlstore.KindDiff
		}
	case controlstore.SealAudit:
		out[o.Segment] = controlstore.KindAuditSegment
	}
	return out
}

// propose commits ops after the Barrier of this replica's leadership,
// writing their content to a quorum first. A digest an op references is
// either in the proposal, so it reaches the quorum now, or already
// referenced, so it reached one before.
func (s *Store) propose(ctx context.Context, p controlstore.Proposal) (controlstore.Result, error) {
	if err := s.check(); err != nil {
		return controlstore.Result{}, err
	}
	if s.leadership() == nil {
		return controlstore.Result{}, s.notLeader()
	}
	if len(p.Ops) == 0 {
		return controlstore.Result{}, fmt.Errorf("%w: a proposal needs at least one op", controlstore.ErrInvalid)
	}
	kinds := map[controlstore.Digest]controlstore.Kind{}
	for _, o := range p.Content {
		if err := o.Validate(); err != nil {
			return controlstore.Result{}, err
		}
		kinds[o.Digest()] = o.Kind
	}
	var version controlstore.Version
	var set bool
	var refErr error
	s.machine.View(func(v controlstore.View) {
		version, set = v.Version()
		for _, op := range p.Ops {
			for d, want := range opContent(op) {
				got, inProposal := kinds[d]
				switch {
				case inProposal && want != 0 && got != want:
					refErr = fmt.Errorf("%w: content %s is a %s object, not a %s", controlstore.ErrInvalid, d, got, want)
				case !inProposal && !v.Referenced(d):
					refErr = fmt.Errorf("%w: content %s is neither in the proposal nor referenced", controlstore.ErrInvalid, d)
				}
			}
		}
	})
	if refErr != nil {
		return controlstore.Result{}, refErr
	}
	if !set {
		init, ok := p.Ops[0].(controlstore.InitVersion)
		if !ok || len(p.Ops) != 1 {
			return controlstore.Result{}, controlstore.Errorf(controlstore.ErrNoLeader, "the Control Store version is not set yet")
		}
		version = init.Version
	}
	data, err := controlstore.EncodeCommand(controlstore.Command{Version: version, At: s.clk.Now().UTC(), Ops: p.Ops})
	if err != nil {
		return controlstore.Result{}, err
	}
	if len(p.Content) > 0 {
		if err := s.putQuorum(ctx, p.Content); err != nil {
			return controlstore.Result{}, err
		}
	}
	f := s.r.Apply(data, 0)
	if err := s.wait(ctx, f); err != nil {
		if ctx.Err() != nil {
			return controlstore.Result{}, ctx.Err()
		}
		if errors.Is(err, hraft.ErrNotLeader) || errors.Is(err, hraft.ErrLeadershipLost) ||
			errors.Is(err, hraft.ErrLeadershipTransferInProgress) || errors.Is(err, hraft.ErrRaftShutdown) {
			return controlstore.Result{}, controlstore.Errorf(controlstore.ErrNoLeader, "the entry did not commit under this leader: %w", err)
		}
		return controlstore.Result{}, fmt.Errorf("controlstore: apply: %w", err)
	}
	resp, ok := f.Response().(*applyResponse)
	if !ok {
		return controlstore.Result{}, fmt.Errorf("controlstore: apply returned %T", f.Response())
	}
	if resp.err != nil {
		return controlstore.Result{}, fmt.Errorf("%w: %w", controlstore.ErrHalted, resp.err)
	}
	if resp.res.Rejected != nil {
		return resp.res, fmt.Errorf("%w: %w", controlstore.ErrRejected, resp.res.Rejected)
	}
	return resp.res, nil
}
