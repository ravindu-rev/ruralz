// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"cmp"
	"time"
)

// Fence is the (storeEpoch, sequence) pair the leader signs into an
// assignment (assignmentSeq) or a promotion (promotionSeq)
// (docs/architecture/04-control-plane-and-gitops.md "Fencing stale
// replicas"). Fences order by epoch, then sequence: a Node keeps the
// highest it accepted and refuses a lower one, and the state machine
// accepts an entry only at the current epoch and the current sequence plus
// one.
type Fence struct {
	Epoch uint64 `json:"epoch"`
	Seq   uint64 `json:"seq"`
}

// Compare orders f against g: -1, 0 or +1.
func (f Fence) Compare(g Fence) int {
	if c := cmp.Compare(f.Epoch, g.Epoch); c != 0 {
		return c
	}
	return cmp.Compare(f.Seq, g.Seq)
}

// Next returns the fence the next entry of the stream must carry.
func (f Fence) Next() Fence { return Fence{Epoch: f.Epoch, Seq: f.Seq + 1} }

// Assignment is one signed assignment of a Revision to a Cluster. Its
// fields are what the leader signs (Cluster, Environment, digest,
// rolloutId, storeEpoch, assignmentSeq); Signed is the signed form Nodes
// receive, opaque to the store.
type Assignment struct {
	Cluster     string `json:"cluster"`
	Environment string `json:"environment"`
	Digest      Digest `json:"digest"`
	RolloutID   string `json:"rolloutId"`
	Fence       Fence  `json:"fence"`
	Signed      []byte `json:"signed"`
	// Index is the Raft index the state machine accepted it at; zero in a
	// proposal.
	Index uint64 `json:"index,omitempty"`
}

// Promotion is one signed promotion: the Cluster's promoted digest, the
// Revision of its last complete Rollout, at (storeEpoch, promotionSeq).
type Promotion struct {
	Cluster        string `json:"cluster"`
	PromotedDigest Digest `json:"promotedDigest"`
	Fence          Fence  `json:"fence"`
	Signed         []byte `json:"signed"`
	Index          uint64 `json:"index,omitempty"`
}

// AnchorSet is a root-signed anchor set as far as the store orders it.
// Version is the root signing time in the set's own encoding, compared as
// an unsigned integer: the store accepts only a higher one. Epoch is the
// storeEpoch the set carries: its own Version when the set opens an epoch
// (a restore, Control Store move, emergency removal or server CA
// replacement), else the current epoch. Signed is the set as Nodes
// receive it. The leader verifies the root signature before it proposes
// the set; Nodes verify it again.
type AnchorSet struct {
	Version uint64 `json:"version"`
	Epoch   uint64 `json:"epoch"`
	Signed  []byte `json:"signed"`
	Index   uint64 `json:"index,omitempty"`
}

// OpensEpoch reports whether the set opens a new storeEpoch.
func (a AnchorSet) OpensEpoch() bool { return a.Epoch == a.Version }

// RebalanceAfter is how long a replica stays stale before it sends its
// Nodes Reconnect (rebalance): 30 s (target).
const RebalanceAfter = 30 * time.Second

// Freshness is a replica's fencing state (docs/architecture/11-scalability-and-distributed-state.md
// "Ruralz Control availability", Figure 2).
type Freshness uint8

// Freshness states.
const (
	// Fresh: the replica leads, or heard from a leader within one
	// election timeout.
	Fresh Freshness = iota + 1
	// Stale: no word from a leader for one election timeout; the replica
	// sends its last committed assignment only to Nodes with no active
	// digest, nothing to others, and its last promotion in HeartbeatReply.
	Stale
	// Rebalancing: stale for RebalanceAfter; the replica also sends its
	// Nodes Reconnect (rebalance).
	Rebalancing
)

// String returns the state name.
func (f Freshness) String() string {
	switch f {
	case Fresh:
		return "fresh"
	case Stale:
		return "stale"
	case Rebalancing:
		return "rebalancing"
	default:
		return ""
	}
}

// FreshnessOf returns the state of a replica that leads (leader) or last
// heard from a leader contactAge ago, with the given election timeout. A
// voter campaigning in a minority partition is not the leader, so it goes
// stale too.
func FreshnessOf(leader bool, contactAge, electionTimeout time.Duration) Freshness {
	switch {
	case leader || contactAge < electionTimeout:
		return Fresh
	case contactAge < electionTimeout+RebalanceAfter:
		return Stale
	default:
		return Rebalancing
	}
}

// MayServeAssignment applies the fencing rule to one accepted assignment
// of the current epoch whose content is local: a fresh replica serves it,
// and a stale or rebalancing one serves only the Cluster's last committed
// assignment (latest), and only to a Node with no active digest.
func MayServeAssignment(f Freshness, latest, nodeHasActive bool) bool {
	switch f {
	case Fresh:
		return true
	case Stale, Rebalancing:
		return latest && !nodeHasActive
	default:
		return false
	}
}
