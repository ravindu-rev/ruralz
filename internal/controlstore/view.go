// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"maps"
	"slices"
	"strings"
)

// View is a read-only view of committed state, valid only inside the
// function that received it. Returned values are copies of the view's
// containers; byte slices inside them are shared and never modified.
type View struct{ st *state }

// Index returns the last applied Raft index.
func (v View) Index() uint64 { return v.st.index }

// Version returns the Control Store version, and false before the first
// leader set it.
func (v View) Version() (Version, bool) { return v.st.version, v.st.versionSet }

// Epoch returns the current storeEpoch: the version of the anchor set
// that opened it, zero before any.
func (v View) Epoch() uint64 { return v.st.epoch }

// AnchorSet returns the highest accepted anchor set.
func (v View) AnchorSet() (AnchorSet, bool) {
	if v.st.anchor == nil {
		return AnchorSet{}, false
	}
	return *v.st.anchor, true
}

// Record returns a generic record.
func (v View) Record(bucket, key string) (Record, bool) {
	r := v.st.buckets[bucket][key]
	if r == nil {
		return Record{}, false
	}
	return *r, true
}

// Records calls fn for every record of bucket whose key starts with
// prefix, in key order, until fn returns false.
func (v View) Records(bucket, prefix string, fn func(Record) bool) {
	b := v.st.buckets[bucket]
	keys := slices.Sorted(maps.Keys(b))
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if !fn(*b[k]) {
			return
		}
	}
}

// Clusters returns the Clusters with fenced streams in the current
// epoch, sorted.
func (v View) Clusters() []string { return slices.Sorted(maps.Keys(v.st.clusters)) }

// AssignmentFence returns a Cluster's current assignment fence: the
// current epoch and the last accepted assignmentSeq. The next assignment
// carries its Next().
func (v View) AssignmentFence(cluster string) Fence {
	f := Fence{Epoch: v.st.epoch}
	if c := v.st.clusters[cluster]; c != nil {
		f.Seq = c.assignSeq
	}
	return f
}

// PromotionFence returns a Cluster's current promotion fence.
func (v View) PromotionFence(cluster string) Fence {
	f := Fence{Epoch: v.st.epoch}
	if c := v.st.clusters[cluster]; c != nil {
		f.Seq = c.promoSeq
	}
	return f
}

// Assignment returns a Cluster's accepted assignment at seq in the
// current epoch.
func (v View) Assignment(cluster string, seq uint64) (Assignment, bool) {
	c := v.st.clusters[cluster]
	if c == nil {
		return Assignment{}, false
	}
	a := c.assignments[seq]
	if a == nil {
		return Assignment{}, false
	}
	return *a, true
}

// LatestAssignment returns a Cluster's last accepted assignment: its last
// committed assignment, which a stale replica still sends a Node with no
// active digest.
func (v View) LatestAssignment(cluster string) (Assignment, bool) {
	c := v.st.clusters[cluster]
	if c == nil || c.assignSeq == 0 {
		return Assignment{}, false
	}
	return v.Assignment(cluster, c.assignSeq)
}

// Assignments returns a Cluster's accepted assignments, oldest first.
func (v View) Assignments(cluster string) []Assignment {
	c := v.st.clusters[cluster]
	if c == nil {
		return nil
	}
	out := make([]Assignment, 0, len(c.assignments))
	for _, seq := range slices.Sorted(maps.Keys(c.assignments)) {
		out = append(out, *c.assignments[seq])
	}
	return out
}

// Accepted reports whether a is exactly an assignment the state machine
// accepted and still holds: same Cluster, fence, digest, Environment,
// Rollout and signed form. Replicas serve only these, never a committed
// entry it rejected.
func (v View) Accepted(a Assignment) bool {
	got, ok := v.Assignment(a.Cluster, a.Fence.Seq)
	return ok && a.Fence.Epoch == v.st.epoch && got.Fence == a.Fence && got.Digest == a.Digest &&
		got.Environment == a.Environment && got.RolloutID == a.RolloutID && string(got.Signed) == string(a.Signed)
}

// Promotion returns a Cluster's last accepted promotion in the current
// epoch, which every replica, fresh or stale, sends in HeartbeatReply.
func (v View) Promotion(cluster string) (Promotion, bool) {
	c := v.st.clusters[cluster]
	if c == nil || c.promotion == nil {
		return Promotion{}, false
	}
	return *c.promotion, true
}

// Environments returns the Environments with recorded Revisions, sorted.
func (v View) Environments() []string { return slices.Sorted(maps.Keys(v.st.envs)) }

// Revision returns one recorded Revision.
func (v View) Revision(env string, d Digest) (RevisionRecord, bool) {
	r := v.st.envs[env][d]
	if r == nil {
		return RevisionRecord{}, false
	}
	return *r, true
}

// Revisions returns an Environment's recorded Revisions, newest first.
func (v View) Revisions(env string) []RevisionRecord {
	recs := v.st.revisionsByAge(env)
	out := make([]RevisionRecord, len(recs))
	for i, r := range recs {
		out[i] = *r
	}
	return out
}

// RevisionInUse reports whether an accepted assignment or promotion of
// the current epoch names a Revision of env.
func (v View) RevisionInUse(env string, d Digest) bool { return v.st.revisionInUse(env, d) }

// Referenced reports whether a committed record references content d.
func (v View) Referenced(d Digest) bool { return v.st.refs[d] > 0 }

// References returns every referenced content digest, sorted.
func (v View) References() []Digest {
	out := make([]Digest, 0, len(v.st.refs))
	for d, n := range v.st.refs {
		if n > 0 {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b Digest) int { return slices.Compare(a[:], b[:]) })
	return out
}

// AuditEntries returns the unsealed audit entries, oldest first.
func (v View) AuditEntries() []AuditEntry {
	out := make([]AuditEntry, len(v.st.audit.entries))
	for i, e := range v.st.audit.entries {
		out[i] = *e
	}
	return out
}

// AuditSegments returns the sealed segments still retained, oldest first.
func (v View) AuditSegments() []AuditSegment {
	out := make([]AuditSegment, len(v.st.audit.segments))
	for i, s := range v.st.audit.segments {
		out[i] = *s
	}
	return out
}

// AuditHead returns the digest of the newest sealed segment, the chain
// head the next segment links to; zero before the first seal.
func (v View) AuditHead() Digest { return v.st.audit.head }

// Backup returns the newest recorded backup.
func (v View) Backup() (BackupRecord, bool) {
	if v.st.backup == nil {
		return BackupRecord{}, false
	}
	return *v.st.backup, true
}

// Members returns the committed Raft configuration, sorted by server ID.
func (v View) Members() []Member { return slices.Clone(v.st.members) }

// IsMember reports whether id is in the committed configuration.
func (v View) IsMember(id ServerID) bool { return v.st.isMember(id) }

// Pending returns the pending members, sorted by server ID.
func (v View) Pending() []PendingMember {
	out := slices.Collect(maps.Values(v.st.pending))
	slices.SortFunc(out, func(a, b PendingMember) int { return strings.Compare(string(a.ServerID), string(b.ServerID)) })
	return out
}

// PendingMember returns one pending member.
func (v View) PendingMember(id ServerID) (PendingMember, bool) {
	p, ok := v.st.pending[id]
	return p, ok
}

// Revoked reports whether id's peer certificates are revoked.
func (v View) Revoked(id ServerID) bool {
	_, ok := v.st.revoked[id]
	return ok
}

// KnowsMembers reports whether the committed state names any member: a
// replica that joined applies none until the leader's first snapshot or
// entries reach it.
func (v View) KnowsMembers() bool { return len(v.st.members) > 0 }

// Reports returns the committed replica status reports.
func (v View) Reports() map[ServerID]ReplicaReport { return maps.Clone(v.st.reports) }

// LastAdvance returns the last version-advance entry.
func (v View) LastAdvance() (VersionAdvance, bool) {
	if v.st.advance == nil {
		return VersionAdvance{}, false
	}
	return *v.st.advance, true
}

// MemberIDs returns the committed configuration's server IDs, sorted.
func (v View) MemberIDs() []ServerID { return v.st.memberIDs() }
