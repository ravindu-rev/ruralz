// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"

	hraft "github.com/hashicorp/raft"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// statusReplica is the status-forwarding request kind of a replica status
// report.
const statusReplica = "replicaStatus"

// replicaStatus is one replica's status report.
type replicaStatus struct {
	ServerID      controlstore.ServerID `json:"serverId"`
	Address       string                `json:"address"`
	BinaryVersion string                `json:"binaryVersion"`
	AppliedIndex  uint64                `json:"appliedIndex"`
	SnapshotIndex uint64                `json:"snapshotIndex"`
}

// statusRequest is one status-forwarding request.
type statusRequest struct {
	Kind    string          `json:"kind"`
	Replica *replicaStatus  `json:"replica,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// statusResponse answers one request: accepted, a redirect to the leader,
// or a rejection with its RZ code.
type statusResponse struct {
	Accepted bool            `json:"accepted"`
	Leader   string          `json:"leader,omitempty"`
	Code     string          `json:"code,omitempty"`
	Detail   string          `json:"detail,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// errNoLeaderReachable reports a status request no leader received.
var errNoLeaderReachable = errors.New("controlstore: no leader received the status request")

// serveStatus serves one status-forwarding connection: the store's
// replica status reports, and every other kind through StatusHandler.
func (s *Store) serveStatus(ctx context.Context, pc *PeerConn) {
	br := bufio.NewReader(pc)
	w := &limitedWriter{ctx: ctx, w: pc, lim: s.trans.lim.of(pc.Peer.ServerID)}
	for {
		var req statusRequest
		if err := readHeader(br, &req); err != nil {
			return
		}
		var resp statusResponse
		switch {
		case req.Kind == statusReplica && req.Replica != nil:
			resp = s.handleReport(ctx, pc.Peer.ServerID, *req.Replica)
		case req.Kind != "" && req.Kind != statusReplica:
			resp = s.handleStatus(ctx, pc.Peer, req.Kind, req.Payload)
		default:
			resp = statusResponse{Detail: fmt.Sprintf("malformed status request of kind %q", req.Kind)}
		}
		if err := writeHeader(w, resp); err != nil {
			return
		}
	}
}

// handleReport is the leader's side of a status report: the first 8092
// connection of a pending member adds it as a non-voter, a non-voter that
// reached the commit index becomes a voter, a binary newer than the
// Control Store version allows is rejected with RZ-CP-021, and a changed
// binary version or a snapshot reaching the last version-advance entry is
// committed.
func (s *Store) handleReport(ctx context.Context, from controlstore.ServerID, st replicaStatus) statusResponse {
	if from != st.ServerID {
		return statusResponse{Detail: "a replica reports only its own status"}
	}
	if s.leadership() == nil {
		_, leader := s.Leader()
		if leader == s.cfg.Address {
			leader = ""
		}
		return statusResponse{Leader: leader, Detail: "not the leader"}
	}
	if _, err := controlstore.ParseVersion(st.BinaryVersion); err != nil {
		return statusResponse{Code: controlstore.CodeFinalize, Detail: err.Error()}
	}
	s.mu.Lock()
	s.live[st.ServerID] = liveReport{status: st, at: s.clk.Now()}
	s.mu.Unlock()
	var pending, member, voter, set bool
	var joinAddr, memberAddr string
	var version controlstore.Version
	var last *controlstore.VersionAdvance
	var members []controlstore.ServerID
	var reports map[controlstore.ServerID]controlstore.ReplicaReport
	s.machine.View(func(v controlstore.View) {
		var p controlstore.PendingMember
		p, pending = v.PendingMember(st.ServerID)
		joinAddr = p.Address
		for _, m := range v.Members() {
			if m.ServerID == st.ServerID {
				member, voter, memberAddr = true, m.Voter, m.Address
			}
		}
		version, set = v.Version()
		if a, ok := v.LastAdvance(); ok {
			last = &a
		}
		members = v.MemberIDs()
		reports = v.Reports()
	})
	switch {
	case pending && !member:
		if err := s.wait(ctx, s.r.AddNonvoter(hraft.ServerID(st.ServerID), hraft.ServerAddress(joinAddr), 0, 0)); err != nil {
			s.log.WarnContext(ctx, "add a pending member as a non-voter", slog.String(catalog.KeyError, err.Error()))
		}
	case member && !voter && st.AppliedIndex >= s.r.CommitIndex():
		if err := s.wait(ctx, s.r.AddVoter(hraft.ServerID(st.ServerID), hraft.ServerAddress(memberAddr), 0, 0)); err != nil {
			s.log.WarnContext(ctx, "promote a caught-up non-voter", slog.String(catalog.KeyError, err.Error()))
		}
	}
	if !set {
		return statusResponse{Accepted: true}
	}
	if refusal := controlstore.ReportRefusal(version, st.BinaryVersion, last, members, reports); refusal != "" {
		return statusResponse{Code: controlstore.CodeFinalize, Detail: refusal}
	}
	committed, had := reports[st.ServerID]
	crossing := last != nil && st.SnapshotIndex >= last.Index && (committed.SnapshotIndex < last.Index || committed.SnapshotSince.IsZero())
	if !had || committed.BinaryVersion != st.BinaryVersion || crossing {
		op := controlstore.Report{ServerID: st.ServerID, BinaryVersion: st.BinaryVersion, SnapshotIndex: st.SnapshotIndex}
		if _, err := s.propose(ctx, controlstore.Proposal{Ops: []controlstore.Op{op}}); err != nil {
			s.log.WarnContext(ctx, "commit a status report", slog.String(catalog.KeyError, err.Error()))
		}
	}
	return statusResponse{Accepted: true}
}

// ownStatus is this replica's status report.
func (s *Store) ownStatus() replicaStatus {
	return replicaStatus{
		ServerID: s.id, Address: s.cfg.Address, BinaryVersion: s.cfg.BinaryVersion,
		AppliedIndex: s.machine.Index(), SnapshotIndex: s.newestSnapshot(),
	}
}

// reportLoop reports this replica's status to the leader every
// ReportInterval, at start, when the leader changes and after each forced
// snapshot; a report the leader rejects with RZ-CP-021 halts the replica.
// It also retries fetching missing content at the same cadence.
func (s *Store) reportLoop() {
	t := s.clk.NewTimer(ReportInterval)
	defer t.Stop()
	signal(s.reportCh)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C():
			t.Reset(ReportInterval)
		case <-s.reportCh:
		}
		s.reportOnce()
		signal(s.fetchCh)
	}
}

// reportOnce sends one report.
func (s *Store) reportOnce() {
	if s.haltErr() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, ioTimeout)
	defer cancel()
	resp, err := s.sendReport(ctx, s.ownStatus())
	if err != nil {
		s.log.DebugContext(ctx, "status report not delivered", slog.String(catalog.KeyError, err.Error()))
		return
	}
	if !resp.Accepted && resp.Code == controlstore.CodeFinalize {
		s.haltWith(&controlstore.HaltError{Reason: controlstore.HaltReport, Err: fmt.Errorf("%s: %s", resp.Code, resp.Detail)})
	}
}

// sendReport delivers a report to the leader: handled here when this
// replica leads, else sent over the status-forwarding class.
func (s *Store) sendReport(ctx context.Context, st replicaStatus) (statusResponse, error) {
	if s.leadership() != nil {
		return s.handleReport(ctx, s.id, st), nil
	}
	return s.sendToLeader(ctx, statusRequest{Kind: statusReplica, Replica: &st})
}

// statusTargets returns where a status request may reach the leader: the
// known leader, the configured peers, then the members.
func (s *Store) statusTargets() []string {
	var targets []string
	add := func(a string) {
		if a != "" && a != s.cfg.Address && !slices.Contains(targets, a) {
			targets = append(targets, a)
		}
	}
	_, leader := s.Leader()
	add(leader)
	for _, p := range s.cfg.Peers {
		add(p)
	}
	s.machine.View(func(v controlstore.View) {
		for _, m := range v.Members() {
			add(m.Address)
		}
	})
	return targets
}

// sendToLeader sends one request over the status-forwarding class until
// the leader answers it, following a redirect to the leader.
func (s *Store) sendToLeader(ctx context.Context, req statusRequest) (statusResponse, error) {
	targets := s.statusTargets()
	var errs []error
	for i := 0; i < len(targets); i++ {
		resp, err := s.sendTo(ctx, targets[i], req)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if resp.Accepted || resp.Code != "" {
			return resp, nil
		}
		if resp.Leader != "" && !slices.Contains(targets[:i+1], resp.Leader) {
			// Try the leader next.
			rest := slices.DeleteFunc(slices.Clone(targets[i+1:]), func(a string) bool { return a == resp.Leader })
			targets = append(append(targets[:i+1:i+1], resp.Leader), rest...)
		}
	}
	return statusResponse{}, fmt.Errorf("%w: %w", errNoLeaderReachable, errors.Join(errs...))
}

// sendTo sends one request to one peer.
func (s *Store) sendTo(ctx context.Context, address string, req statusRequest) (statusResponse, error) {
	tc, id, err := s.trans.dial(ctx, address, controlstore.ClassStatus)
	if err != nil {
		return statusResponse{}, err
	}
	defer s.trans.release(tc)
	stop := context.AfterFunc(ctx, func() { _ = tc.Close() })
	defer stop()
	w := &limitedWriter{ctx: ctx, w: tc, lim: s.trans.lim.of(id.ServerID)}
	if err := writeHeader(w, req); err != nil {
		return statusResponse{}, err
	}
	var resp statusResponse
	if err := readHeader(bufio.NewReader(tc), &resp); err != nil {
		return statusResponse{}, err
	}
	return resp, nil
}

// StatusError is a status-forwarding request the leader's StatusHandler
// refused, with the RZ code it gave.
type StatusError struct {
	Code   string
	Detail string
}

// Error describes the refusal.
func (e *StatusError) Error() string {
	if e.Code == "" {
		return "controlstore: status request refused: " + e.Detail
	}
	return e.Code + ": " + e.Detail
}

// SendStatus delivers one status-forwarding request of kind to the
// leader and returns its answer: the class that carries the Control
// Stream's ACK, NACK and heartbeat aggregates from followers to the
// leader, whose StatusHandler serves them. With no leader reachable it is
// RZ-CP-004.
func (s *Store) SendStatus(ctx context.Context, kind string, payload json.RawMessage) (json.RawMessage, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	if kind == "" || kind == statusReplica {
		return nil, fmt.Errorf("%w: status kind %q", controlstore.ErrInvalid, kind)
	}
	var resp statusResponse
	if s.leadership() != nil {
		resp = s.handleStatus(ctx, controlstore.PeerIdentity{Role: controlstore.RoleReplica, ServerID: s.id}, kind, payload)
	} else {
		var err error
		if resp, err = s.sendToLeader(ctx, statusRequest{Kind: kind, Payload: payload}); err != nil {
			return nil, controlstore.Errorf(controlstore.ErrNoLeader, "%w", err)
		}
	}
	if !resp.Accepted {
		return nil, &StatusError{Code: resp.Code, Detail: resp.Detail}
	}
	return resp.Payload, nil
}

// handleStatus serves a status request of an embedder kind on the leader.
func (s *Store) handleStatus(ctx context.Context, from controlstore.PeerIdentity, kind string, payload json.RawMessage) statusResponse {
	if s.leadership() == nil {
		_, leader := s.Leader()
		if leader == s.cfg.Address {
			leader = ""
		}
		return statusResponse{Leader: leader, Detail: "not the leader"}
	}
	if s.cfg.StatusHandler == nil {
		return statusResponse{Code: controlstore.CodeNoLeader, Detail: fmt.Sprintf("no handler for status kind %q", kind)}
	}
	out, err := s.cfg.StatusHandler(ctx, from, kind, payload)
	if err != nil {
		return statusResponse{Code: controlstore.CodeOf(err), Detail: err.Error()}
	}
	return statusResponse{Accepted: true, Payload: out}
}

// DialLeader opens a write-forwarding connection to the leader, whose
// ForwardHandler re-authorizes each forwarded write as its original
// principal. RZ-CP-004 when no leader is known; ErrInvalid on the leader
// itself, which applies writes directly.
func (s *Store) DialLeader(ctx context.Context) (net.Conn, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	if s.leadership() != nil {
		return nil, fmt.Errorf("%w: this replica leads; it forwards nothing", controlstore.ErrInvalid)
	}
	_, leader := s.Leader()
	if leader == "" || leader == s.cfg.Address {
		return nil, controlstore.NotLeader("")
	}
	tc, id, err := s.trans.dial(ctx, leader, controlstore.ClassForward)
	if err != nil {
		return nil, controlstore.Errorf(controlstore.ErrNoLeader, "dial the leader: %w", err)
	}
	return &dialedConn{Conn: tc, t: s.trans, lim: s.trans.lim.of(id.ServerID)}, nil
}

// truncateLoop forces a snapshot past each version-advance entry and
// truncates the log behind it, then reports the snapshot (rule 3 of
// "Ruralz Control upgrades").
func (s *Store) truncateLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.truncCh:
		}
		if err := s.snapshotAndTruncate(); err != nil && s.ctx.Err() == nil {
			s.log.Error("snapshot past the version-advance entry", slog.String(catalog.KeyError, err.Error()))
			continue
		}
		signal(s.reportCh)
	}
}

// snapshotAndTruncate takes a snapshot keeping no trailing log entries.
func (s *Store) snapshotAndTruncate() error {
	rc := s.r.ReloadableConfig()
	trailing := rc.TrailingLogs
	rc.TrailingLogs = 0
	if err := s.r.ReloadConfig(rc); err != nil {
		return err
	}
	err := s.wait(s.ctx, s.r.Snapshot())
	rc.TrailingLogs = trailing
	if rerr := s.r.ReloadConfig(rc); err == nil {
		err = rerr
	}
	if errors.Is(err, hraft.ErrNothingNewToSnapshot) {
		return nil
	}
	return err
}
