// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package controlstore is the Control Store contract of Ruralz Control
// (docs/architecture/04-control-plane-and-gitops.md "Control Store and
// high availability", ADR-0006): the Store every Ruralz Control component
// writes and reads through, and the deterministic Machine both drivers
// apply. The raft driver, the default, lives in
// internal/controlstore/raft, the only importer of hashicorp/raft;
// postgres is Planned (M4). Process configuration (controlStore.driver)
// picks one, with no feature difference (P1).
//
// # Data
//
// Raft-replicated state holds generic records in buckets (definitions,
// promotion and Rollout records, ACK bitmaps, Enrollment records, token
// hashes, revocations, encrypted keys, users, bindings), the fenced
// assignment and promotion streams, the storeEpoch and accepted anchor
// set, Revision metadata, unsealed audit entries, sealed segment records,
// the Raft membership mirror, pending members, revoked peers, replica
// status reports and the Control Store version. Content objects (Revision
// content, source trees, diffs, plans, sealed audit segments) live in the
// content store by digest: each reaches a replica quorum before the entry
// that references it, else RZ-CP-013; a reference count per object
// releases it once no record references it, and only a Raft entry drops a
// reference, so a backup at one index never misses content.
//
// # Fencing
//
// Assignments and promotions carry (storeEpoch, sequence) inside the
// leader's signature. The Machine accepts one only at the current epoch
// and the current per-Cluster sequence plus one, so of two signed entries
// at one sequence only the first in log order is accepted; the other
// commits, changes nothing and is never served. A rejected proposal
// returns the stream's current fence, and the leader re-reads and
// re-signs at the next sequence. An anchor set that opens an epoch resets
// every sequence. A replica without word from a leader for one election
// timeout is Stale and serves only its last committed assignment, and only
// to Nodes without an active digest (MayServeAssignment); after
// RebalanceAfter it is Rebalancing.
//
// # Versions
//
// The Machine halts (HaltError) rather than skip an entry, snapshot or
// state above its binary's Control Store version, and EvaluateFinalize
// and ReportRefusal hold the finalize and status-report rules of
// docs/engineering/04-release-versioning-and-compatibility.md "Ruralz
// Control upgrades".
package controlstore

import (
	"context"
	"io"
	"time"
)

// Store is one replica's Control Store. Writes go to the leader; reads
// serve this replica's committed state. A halted replica answers every
// call with ErrHalted, and a shut-down one with ErrClosed.
type Store interface {
	// ServerID returns this replica's server ID.
	ServerID() ServerID
	// Ready is closed once the Control Store loaded: the latest snapshot
	// restored and the committed entries after it applied, the condition
	// /readyz on 9902 needs (never quorum or freshness).
	Ready() <-chan struct{}
	// Halted returns the *HaltError that stopped this replica, or nil.
	Halted() error

	// Propose writes the proposal's content to a replica quorum, then
	// commits its ops as one Raft entry and waits until it applies. It
	// runs only on the leader after its Barrier (RZ-CP-004 otherwise, with
	// the leader's address when one is known) and takes only ClientOp
	// ops. A committed entry the Machine rejected returns ErrRejected
	// wrapping its *Rejection.
	Propose(ctx context.Context, p Proposal) (Result, error)
	// View runs fn on this replica's committed state.
	View(fn func(View)) error
	// Changed returns a channel closed once the next index applies.
	Changed() <-chan struct{}

	// PutContent stores objects locally and on a replica quorum, else
	// RZ-CP-013.
	PutContent(ctx context.Context, objs ...Object) error
	// Content returns an object, fetching a missing one from a peer and
	// verifying its digest.
	Content(ctx context.Context, d Digest) (Object, error)
	// HasContent reports whether an object is stored on this replica.
	HasContent(d Digest) bool

	// AwaitLeadership blocks until this replica leads and its Barrier
	// applied every entry committed before the election; leader services
	// start then and stop when Lost is closed.
	AwaitLeadership(ctx context.Context) (Leadership, error)
	// Leader returns the current leader's server ID and peer address, or
	// empty strings when none is known.
	Leader() (ServerID, string)
	// Freshness returns this replica's fencing state and its leader
	// contact age, zero while it leads.
	Freshness() (Freshness, time.Duration)
	// MayServe reports whether this replica may send a Node assignment a
	// now: an accepted assignment of the current epoch whose content is
	// local, under MayServeAssignment.
	MayServe(a Assignment, nodeHasActive bool) bool

	// TransferLeadership moves leadership to the most caught-up voter
	// when from leads, and is a no-op otherwise. It waits up to
	// TransferWait; RZ-CP-004 when no other voter took over by then.
	TransferLeadership(ctx context.Context, from ServerID) error
	// RemoveVoter removes a voter and revokes its peer certificates,
	// committing audit, the audit entry of the removal, in the same
	// entry; RZ-CP-019 when it names the leader or the voters left in
	// contact with the leader would not form a majority of the new voter
	// set.
	RemoveVoter(ctx context.Context, id ServerID, audit []byte) error

	// FinalizeStatus evaluates the finalize conditions now.
	FinalizeStatus() (FinalizeStatus, error)
	// Finalize commits the version-advance entry with audit, the finalize
	// audit entry, or RZ-CP-021.
	Finalize(ctx context.Context, audit []byte) error

	// SealAudit seals the oldest unsealed entries firstSeq to lastSeq
	// into segment, a KindAuditSegment object of at most 4 MiB chained to
	// the current head.
	SealAudit(ctx context.Context, segment Object, firstSeq, lastSeq uint64) error
	// PruneAudit prunes the sealed segments past the audit retention that
	// a recorded backup holds, oldest first, and returns how many.
	PruneAudit(ctx context.Context) (int, error)
	// PruneRevisions deletes the Revisions beyond the newest
	// RevisionsKept of each Environment that no accepted assignment or
	// promotion names and keep does not hold, and returns how many.
	PruneRevisions(ctx context.Context, keep func(env string, d Digest) bool) (int, error)

	// Backup writes the state at one index and every content object it
	// references.
	Backup(ctx context.Context, w io.Writer) (BackupInfo, error)

	// Shutdown transfers leadership when this replica leads, waiting up
	// to TransferWait, then stops.
	Shutdown(ctx context.Context) error
}

// Proposal is one write: content objects, which reach a replica quorum
// first, and the ops of one Raft entry. Every digest an op references is
// either among Content or already referenced in committed state.
type Proposal struct {
	Content []Object
	Ops     []Op
}

// Leadership is one term of this replica's leadership, begun after its
// Barrier.
type Leadership struct {
	// Term is the Raft term.
	Term uint64
	// BarrierIndex is the applied index after the Barrier: every entry
	// committed before the election is at or below it.
	BarrierIndex uint64
	lost         <-chan struct{}
}

// NewLeadership returns a Leadership whose Lost channel is lost.
func NewLeadership(term, barrier uint64, lost <-chan struct{}) Leadership {
	return Leadership{Term: term, BarrierIndex: barrier, lost: lost}
}

// Lost is closed when this term's leadership ends.
func (l Leadership) Lost() <-chan struct{} { return l.lost }

// TransferWait bounds a leadership transfer, a stopping leader's or a
// requested one: 10 s (target)
// (docs/architecture/04-control-plane-and-gitops.md "Leadership
// transfer").
const TransferWait = 10 * time.Second

// DefaultAuditRetention is the audit retention without a setting: 365
// days (target) (docs/operations/03-capacity-planning.md).
const DefaultAuditRetention = 365 * 24 * time.Hour

// BackupInfo describes a written backup.
type BackupInfo struct {
	// Index is the Raft index the backup is consistent at.
	Index uint64 `json:"index"`
	// Version is the backup's Control Store version.
	Version Version `json:"version"`
	// BinaryVersion is the product version of the binary that wrote it.
	BinaryVersion string `json:"binaryVersion"`
	// Objects is how many content objects it holds.
	Objects int `json:"objects"`
}
