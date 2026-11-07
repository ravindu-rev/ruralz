// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// leadershipLoop follows raft's leadership signals. Any transition ends
// the published leadership; becoming leader starts a Barrier, and only
// once it applied every entry committed before the election do leader
// services start (ADR-0006 "Consensus").
func (s *Store) leadershipLoop() {
	ch := s.r.LeaderCh()
	for {
		select {
		case <-s.ctx.Done():
			s.endLeadership()
			return
		case isLeader := <-ch:
			gen := s.endLeadership()
			signal(s.reportCh)
			if isLeader {
				s.wg.Go(func() { s.becomeLeader(gen) })
			}
		}
	}
}

// endLeadership closes the published term, if any, and returns the new
// leadership generation.
func (s *Store) endLeadership() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leadGen++
	if s.lead != nil {
		close(s.lead.lost)
		s.lead = nil
		s.leaderUntil = s.rclk.Now()
		close(s.leadChanged)
		s.leadChanged = make(chan struct{})
	}
	return s.leadGen
}

// becomeLeader runs the Barrier of a new term and publishes it.
func (s *Store) becomeLeader(gen uint64) {
	term := s.r.CurrentTerm()
	if hook := s.beforeBarrier.Load(); hook != nil {
		(*hook)()
	}
	if err := s.r.Barrier(0).Error(); err != nil {
		s.log.Info("leadership ended before its barrier", slog.String(catalog.KeyError, err.Error()))
		return
	}
	if s.r.State() != hraft.Leader || s.r.CurrentTerm() != term {
		return
	}
	s.mu.Lock()
	if s.leadGen != gen || s.halted != nil || s.closed {
		s.mu.Unlock()
		return
	}
	lead := &leadState{term: term, barrier: s.machine.Index(), lost: make(chan struct{})}
	s.lead = lead
	close(s.leadChanged)
	s.leadChanged = make(chan struct{})
	s.mu.Unlock()
	s.log.Info("leader services start")
	s.leaderServices(lead)
}

// leadership returns the published leadership, or nil.
func (s *Store) leadership() *leadState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lead
}

// leaderServices are the store's own leader work for one term: the
// Control Store version of a new store, this replica's status report, the
// audit retention and the optional timed soak.
func (s *Store) leaderServices(lead *leadState) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.wg.Go(func() {
		select {
		case <-lead.lost:
		case <-s.ctx.Done():
		}
		cancel()
	})
	var set bool
	s.machine.View(func(v controlstore.View) { _, set = v.Version() })
	if !set {
		if _, err := s.propose(ctx, controlstore.Proposal{Ops: []controlstore.Op{controlstore.InitVersion{Version: s.binary}}}); err != nil &&
			!errors.Is(err, controlstore.ErrRejected) {
			s.log.WarnContext(ctx, "set the Control Store version", slog.String(catalog.KeyError, err.Error()))
		}
	}
	signal(s.reportCh)
	s.wg.Go(func() { s.auditLoop(ctx) })
	if s.cfg.TimedSoak > 0 {
		s.wg.Go(func() { s.timedSoakLoop(ctx) })
	}
}

// AwaitLeadership blocks until this replica leads after its Barrier.
func (s *Store) AwaitLeadership(ctx context.Context) (controlstore.Leadership, error) {
	for {
		if err := s.check(); err != nil {
			return controlstore.Leadership{}, err
		}
		s.mu.Lock()
		lead, ch := s.lead, s.leadChanged
		s.mu.Unlock()
		if lead != nil {
			return controlstore.NewLeadership(lead.term, lead.barrier, lead.lost), nil
		}
		select {
		case <-ctx.Done():
			return controlstore.Leadership{}, ctx.Err()
		case <-s.ctx.Done():
			return controlstore.Leadership{}, controlstore.ErrClosed
		case <-ch:
		}
	}
}

// Leader returns the current leader's server ID and address.
func (s *Store) Leader() (controlstore.ServerID, string) {
	addr, id := s.r.LeaderWithID()
	return controlstore.ServerID(id), string(addr)
}

// electionTimeout returns raft's election timeout.
func (s *Store) electionTimeout() time.Duration { return s.r.ReloadableConfig().ElectionTimeout }

// Freshness returns this replica's fencing state: fresh while it leads or
// heard from a leader within one election timeout, measured on raft's own
// clock from its last leader contact, the end of its own leadership or
// its start, whichever is latest.
func (s *Store) Freshness() (controlstore.Freshness, time.Duration) {
	if s.leadership() != nil || s.r.State() == hraft.Leader {
		return controlstore.Fresh, 0
	}
	contact := s.r.LastContact()
	s.mu.Lock()
	if s.leaderUntil.After(contact) {
		contact = s.leaderUntil
	}
	s.mu.Unlock()
	if s.started.After(contact) {
		contact = s.started
	}
	age := max(s.rclk.Since(contact), 0)
	return controlstore.FreshnessOf(false, age, s.electionTimeout()), age
}

// LeaderInfo is ruralz_control_leader_info: 1 while this replica leads.
func (s *Store) LeaderInfo() int64 {
	if s.leadership() != nil {
		return 1
	}
	return 0
}

// LeaderContactAgeSeconds is ruralz_control_leader_contact_age_seconds:
// zero while this replica leads, else the age of its last leader contact.
func (s *Store) LeaderContactAgeSeconds() int64 {
	_, age := s.Freshness()
	return int64(age / time.Second)
}

// MayServe reports whether this replica may send assignment a to a Node
// now: an assignment the state machine accepted in the current epoch,
// whose content is local, under the fencing rule. A missing object starts
// a fetch.
func (s *Store) MayServe(a controlstore.Assignment, nodeHasActive bool) bool {
	if s.check() != nil {
		return false
	}
	var accepted, latest bool
	s.machine.View(func(v controlstore.View) {
		accepted = v.Accepted(a)
		la, ok := v.LatestAssignment(a.Cluster)
		latest = ok && la.Fence == a.Fence
	})
	if !accepted {
		return false
	}
	if !s.content.has(a.Digest) {
		signal(s.fetchCh)
		return false
	}
	f, _ := s.Freshness()
	return controlstore.MayServeAssignment(f, latest, nodeHasActive)
}

// notLeader returns RZ-CP-004 with the leader's address.
func (s *Store) notLeader() error {
	_, addr := s.Leader()
	if addr == s.cfg.Address {
		addr = ""
	}
	return controlstore.NotLeader(addr)
}

// TransferLeadership moves leadership to the most caught-up voter when
// from leads; a no-op otherwise. Like the transfer of a stopping leader,
// it waits up to TransferWait on the store's clock; RZ-CP-004 when no
// other voter took over by then.
func (s *Store) TransferLeadership(ctx context.Context, from controlstore.ServerID) error {
	if err := s.check(); err != nil {
		return err
	}
	leaderID, _ := s.Leader()
	if leaderID != from || from == "" {
		return nil
	}
	if from != s.id || s.leadership() == nil {
		return s.notLeader()
	}
	if s.otherVoters() == 0 {
		return controlstore.Errorf(controlstore.ErrNoLeader, "%w", errNoOtherVoter)
	}
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t := s.clk.AfterFunc(controlstore.TransferWait, cancel)
	defer t.Stop()
	if err := s.transferLeadership(tctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errNoOtherVoter) {
			return controlstore.Errorf(controlstore.ErrNoLeader, "%w", err)
		}
		return controlstore.Errorf(controlstore.ErrNoLeader, "no other voter took over within %s: %w", controlstore.TransferWait, err)
	}
	return nil
}

// errNoOtherVoter fails a leadership transfer of a replica that is the
// only voter.
var errNoOtherVoter = errors.New("no other voter to transfer leadership to")

// otherVoters counts the committed voters other than this replica.
func (s *Store) otherVoters() int {
	n := 0
	s.machine.View(func(v controlstore.View) {
		for _, m := range v.Members() {
			if m.Voter && m.ServerID != s.id {
				n++
			}
		}
	})
	return n
}

// transferLeadership hands leadership to the most caught-up voter: raft
// catches that voter up and has it campaign, and gives up an attempt after
// one election timeout, which a voter far behind or a slow host exceeds.
// The store therefore repeats the attempt, one heartbeat timeout apart,
// until this replica no longer leads or ctx ends, and then returns the
// last attempt's error; errNoOtherVoter once no other voter is left.
func (s *Store) transferLeadership(ctx context.Context) error {
	for {
		if s.otherVoters() == 0 {
			return errNoOtherVoter
		}
		var err error
		if hook := s.beforeTransfer.Load(); hook != nil {
			err = (*hook)()
		}
		if err == nil {
			err = s.wait(ctx, s.r.LeadershipTransfer())
		}
		if err == nil || s.r.State() != hraft.Leader {
			return nil
		}
		if ctx.Err() != nil || s.rclk.Sleep(ctx, s.r.ReloadableConfig().HeartbeatTimeout) != nil {
			return err
		}
	}
}

// inContact reports whether the leader heard from voter id on a Raft
// connection within one election timeout, the window in which a follower
// still counts the leader fresh.
func (s *Store) inContact(id controlstore.ServerID) bool {
	at, ok := s.trans.lastHeard(id)
	return ok && s.rclk.Since(at) < s.electionTimeout()
}

// RemoveVoter removes a voter as a Raft RemoveServer, then revokes its
// peer certificates in an entry that also commits audit, so its old
// volume cannot rejoin (docs/architecture/04-control-plane-and-gitops.md
// "Removing a voter"). It is refused with RZ-CP-019 when id leads, or
// when the voters left in contact with the leader would not form a
// majority of the new voter set. A non-voter or pending member is removed
// the same way.
func (s *Store) RemoveVoter(ctx context.Context, id controlstore.ServerID, audit []byte) error {
	if err := s.check(); err != nil {
		return err
	}
	if err := id.Validate(); err != nil {
		return err
	}
	if s.leadership() == nil {
		return s.notLeader()
	}
	if id == s.id {
		return controlstore.Errorf(controlstore.ErrRemoveRefused, "server %s is the leader; transfer leadership first", id)
	}
	var member, voter, pending bool
	var newVoters []controlstore.ServerID
	s.machine.View(func(v controlstore.View) {
		for _, m := range v.Members() {
			if m.ServerID == id {
				member, voter = true, m.Voter
				continue
			}
			if m.Voter {
				newVoters = append(newVoters, m.ServerID)
			}
		}
		_, pending = v.PendingMember(id)
	})
	if !member && !pending {
		return fmt.Errorf("%w: server %s is not a member", controlstore.ErrNotFound, id)
	}
	if voter {
		contact := 0
		for _, v := range newVoters {
			if v == s.id || s.inContact(v) {
				contact++
			}
		}
		if contact <= len(newVoters)/2 {
			return controlstore.Errorf(controlstore.ErrRemoveRefused,
				"%d of the %d voters left are in contact with the leader, not a majority", contact, len(newVoters))
		}
	}
	if member {
		if err := s.wait(ctx, s.r.RemoveServer(hraft.ServerID(id), 0, 0)); err != nil {
			if errors.Is(err, hraft.ErrNotLeader) || errors.Is(err, hraft.ErrLeadershipLost) {
				return s.notLeader()
			}
			return fmt.Errorf("controlstore: remove server %s: %w", id, err)
		}
	}
	ops := []controlstore.Op{controlstore.RevokePeer{ServerID: id}}
	if len(audit) > 0 {
		ops = append(ops, controlstore.AppendAudit{Entries: [][]byte{audit}})
	}
	_, err := s.propose(ctx, controlstore.Proposal{Ops: ops})
	return err
}
