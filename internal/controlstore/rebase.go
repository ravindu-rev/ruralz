// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"fmt"
	"time"
)

// RestorePlan is what ruralz control restore changes in a backup's state
// before the restored replica serves
// (docs/architecture/04-control-plane-and-gitops.md "Backup, restore and
// postgres", docs/reference/01-cli-and-api-surface.md "Ruralz Control
// replica verbs"): the root-signed anchor set holding the new online key
// opens a higher storeEpoch before any delivery; the Raft membership
// becomes one voter, this replica under a new server ID; old peer
// certificates are dropped; sessions and API tokens are invalidated; and
// the audit log gets a fork record.
type RestorePlan struct {
	ServerID ServerID
	Address  string
	// AnchorSet opens the new epoch: its Epoch equals its Version, above
	// the backup's accepted anchor set.
	AnchorSet AnchorSet
	// DropBuckets are the buckets the restore empties: the sessions and
	// API tokens.
	DropBuckets []string
	// Fork is the audit log's fork record, appended as an entry.
	Fork []byte
	// At is the restore time, from the restoring host's clock.
	At time.Time
}

// Rebase returns a copy of s changed by plan, at the next index.
func (s *Snapshot) Rebase(plan RestorePlan) (*Snapshot, error) {
	st := s.st
	if err := plan.ServerID.Validate(); err != nil {
		return nil, err
	}
	switch {
	case plan.Address == "":
		return nil, fmt.Errorf("%w: a restore needs the replica's peer address", ErrInvalid)
	case len(plan.Fork) == 0:
		return nil, fmt.Errorf("%w: a restore appends an audit fork record", ErrInvalid)
	case !plan.AnchorSet.OpensEpoch() || len(plan.AnchorSet.Signed) == 0:
		return nil, fmt.Errorf("%w: the restore's anchor set must open a storeEpoch", ErrInvalid)
	case st.anchor != nil && plan.AnchorSet.Version <= st.anchor.Version:
		return nil, fmt.Errorf("%w: anchor set version %d is not above the backup's %d", ErrInvalid, plan.AnchorSet.Version, st.anchor.Version)
	case plan.AnchorSet.Version <= st.epoch:
		return nil, fmt.Errorf("%w: storeEpoch %d is not above the backup's %d", ErrInvalid, plan.AnchorSet.Version, st.epoch)
	case st.isMember(plan.ServerID):
		return nil, fmt.Errorf("%w: server ID %s is a member of the backed-up deployment; a restore takes a new one", ErrInvalid, plan.ServerID)
	}
	if _, ok := st.pending[plan.ServerID]; ok {
		return nil, fmt.Errorf("%w: server ID %s is pending in the backup; a restore takes a new one", ErrInvalid, plan.ServerID)
	}
	if _, ok := st.revoked[plan.ServerID]; ok {
		return nil, fmt.Errorf("%w: server ID %s is revoked in the backup", ErrInvalid, plan.ServerID)
	}
	n := st.clone()
	n.index++
	// Drop every old peer certificate: members and pending members are
	// revoked, so the predecessor's replicas cannot reach this one.
	for _, m := range n.members {
		n.revoked[m.ServerID] = n.index
	}
	for id := range n.pending {
		n.revoked[id] = n.index
	}
	n.pending = map[ServerID]PendingMember{}
	n.reports = map[ServerID]ReplicaReport{}
	n.members = []Member{{ServerID: plan.ServerID, Address: plan.Address, Voter: true}}
	n.configIdx = n.index
	for _, b := range plan.DropBuckets {
		delete(n.buckets, b)
	}
	a := plan.AnchorSet
	a.Index = n.index
	n.anchor = &a
	n.epoch = a.Version
	n.clusters = map[string]*cluster{}
	n.audit.entries = append(n.audit.entries, &AuditEntry{Seq: n.audit.nextSeq, Index: n.index, At: plan.At, Data: plan.Fork})
	n.audit.nextSeq++
	n.recomputeRefs()
	return &Snapshot{st: n}, nil
}
