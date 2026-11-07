// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"fmt"
	"slices"
	"time"
)

// txn applies one command's ops to the live state, recording how to undo
// each change, so a failing op rolls the whole command back.
type txn struct {
	st        *state
	index     uint64
	cmd       Command
	undo      []func()
	unrefd    []Digest
	auditSeqs []uint64
}

// reject builds a rejection.
func reject(reason RejectReason, format string, args ...any) *Rejection {
	return &Rejection{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// save records an undo step.
func (t *txn) save(restore func()) { t.undo = append(t.undo, restore) }

// rollback undoes every change in reverse order.
func (t *txn) rollback() {
	for i := len(t.undo) - 1; i >= 0; i-- {
		t.undo[i]()
	}
	t.undo = nil
}

// setMap sets m[k] and records the undo.
func setMap[K comparable, V any](t *txn, m map[K]V, k K, v V) {
	old, had := m[k]
	m[k] = v
	t.save(func() {
		if had {
			m[k] = old
		} else {
			delete(m, k)
		}
	})
}

// delMap deletes m[k] and records the undo.
func delMap[K comparable, V any](t *txn, m map[K]V, k K) {
	old, had := m[k]
	if !had {
		return
	}
	delete(m, k)
	t.save(func() { m[k] = old })
}

// ref adds one reference to a content object.
func (t *txn) ref(d Digest) {
	old, had := t.st.refs[d]
	t.st.refs[d] = old + 1
	t.save(func() {
		if had {
			t.st.refs[d] = old
		} else {
			delete(t.st.refs, d)
		}
	})
}

// unref drops one reference; an object left without one is released
// when the command commits.
func (t *txn) unref(d Digest) {
	old := t.st.refs[d]
	if old == 0 {
		return
	}
	t.st.refs[d] = old - 1
	t.unrefd = append(t.unrefd, d)
	t.save(func() { t.st.refs[d] = old })
}

// released returns, sorted, the objects whose last reference this
// committed command dropped, and forgets their counts.
func (t *txn) released() []Digest {
	var out []Digest
	for _, d := range t.unrefd {
		if n, ok := t.st.refs[d]; ok && n == 0 {
			delete(t.st.refs, d)
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b Digest) int { return slices.Compare(a[:], b[:]) })
	return slices.Compact(out)
}

// apply applies one op.
func (t *txn) apply(op Op) *Rejection {
	switch o := op.(type) {
	case Put:
		return t.put(o)
	case Delete:
		return t.delete(o)
	case Assign:
		return t.assign(o.Assignment)
	case Promote:
		return t.promote(o.Promotion)
	case AcceptAnchorSet:
		return t.anchorSet(o.AnchorSet)
	case RecordRevision:
		return t.recordRevision(o.Revision)
	case DeleteRevision:
		return t.deleteRevision(o)
	case DropAssignments:
		return t.dropAssignments(o)
	case AppendAudit:
		return t.appendAudit(o)
	case SealAudit:
		return t.sealAudit(o)
	case PruneAudit:
		return t.pruneAudit(o)
	case RecordBackup:
		return t.recordBackup(o.Backup)
	case AddPending:
		return t.addPending(o.Member)
	case RemovePending:
		delMap(t, t.st.pending, o.ServerID)
		return nil
	case RevokePeer:
		return t.revokePeer(o.ServerID)
	case Report:
		return t.report(o)
	case AdvanceVersion:
		return t.advanceVersion(o)
	case InitVersion:
		return t.initVersion(o)
	default:
		return reject(RejectInvalid, "op %T", op)
	}
}

// validBucket checks a bucket name: 1 to 64 characters from
// [a-z0-9._-].
func validBucket(b string) bool {
	if b == "" || len(b) > 64 {
		return false
	}
	for i := range len(b) {
		if !bucketByte(b[i]) {
			return false
		}
	}
	return true
}

// bucketByte reports whether c is in [a-z0-9._-].
func bucketByte(c byte) bool {
	return 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '.' || c == '_' || c == '-'
}

// checkCond applies a record precondition to the current record.
func checkCond(c Cond, cur *Record) *Rejection {
	switch {
	case c.Absent && c.Index != 0:
		return reject(RejectInvalid, "a precondition is absent or an index, not both")
	case c.Absent && cur != nil:
		return reject(RejectCondition, "the record exists at index %d", cur.ModifyIndex)
	case c.Index != 0 && cur == nil:
		return reject(RejectCondition, "the record is absent, not at index %d", c.Index)
	case c.Index != 0 && cur.ModifyIndex != c.Index:
		return reject(RejectCondition, "the record is at index %d, not %d", cur.ModifyIndex, c.Index)
	default:
		return nil
	}
}

// uniqueRefs sorts and deduplicates references, refusing the zero digest.
func uniqueRefs(refs []Digest) ([]Digest, bool) {
	out := slices.Clone(refs)
	for _, d := range out {
		if d.IsZero() {
			return nil, false
		}
	}
	slices.SortFunc(out, func(a, b Digest) int { return slices.Compare(a[:], b[:]) })
	return slices.Compact(out), true
}

func (t *txn) put(o Put) *Rejection {
	if !validBucket(o.Bucket) || o.Key == "" {
		return reject(RejectInvalid, "bucket %q or key %q is malformed", o.Bucket, o.Key)
	}
	refs, ok := uniqueRefs(o.Refs)
	if !ok {
		return reject(RejectInvalid, "a reference is the zero digest")
	}
	b := t.st.buckets[o.Bucket]
	var cur *Record
	if b != nil {
		cur = b[o.Key]
	}
	if rej := checkCond(o.Cond, cur); rej != nil {
		return rej
	}
	rec := &Record{Bucket: o.Bucket, Key: o.Key, Value: o.Value, Refs: refs, CreateIndex: t.index, ModifyIndex: t.index}
	if cur != nil {
		rec.CreateIndex = cur.CreateIndex
	}
	if b == nil {
		b = map[string]*Record{}
		setMap(t, t.st.buckets, o.Bucket, b)
	}
	setMap(t, b, o.Key, rec)
	for _, d := range refs {
		t.ref(d)
	}
	if cur != nil {
		for _, d := range cur.Refs {
			t.unref(d)
		}
	}
	return nil
}

func (t *txn) delete(o Delete) *Rejection {
	if !validBucket(o.Bucket) || o.Key == "" || o.Cond.Absent {
		return reject(RejectInvalid, "bucket %q, key %q or precondition is malformed", o.Bucket, o.Key)
	}
	b := t.st.buckets[o.Bucket]
	var cur *Record
	if b != nil {
		cur = b[o.Key]
	}
	if rej := checkCond(o.Cond, cur); rej != nil {
		return rej
	}
	if cur == nil {
		return nil
	}
	delMap(t, b, o.Key)
	for _, d := range cur.Refs {
		t.unref(d)
	}
	if len(b) == 0 {
		delMap(t, t.st.buckets, o.Bucket)
	}
	return nil
}

// fenceCheck applies the apply-time sequence check: the entry is at the
// current storeEpoch and the current sequence plus one.
func (t *txn) fenceCheck(got Fence, seq uint64) *Rejection {
	if t.st.epoch == 0 {
		return reject(RejectNoEpoch, "no anchor set has opened a storeEpoch")
	}
	cur := Fence{Epoch: t.st.epoch, Seq: seq}
	if got != cur.Next() {
		r := reject(RejectFence, "fence (%d, %d) is not (%d, %d)", got.Epoch, got.Seq, cur.Epoch, cur.Seq+1)
		r.Fence = cur
		return r
	}
	return nil
}

func (t *txn) assign(a Assignment) *Rejection {
	if a.Cluster == "" || a.Environment == "" || a.Digest.IsZero() || len(a.Signed) == 0 {
		return reject(RejectInvalid, "an assignment needs a Cluster, an Environment, a digest and its signed form")
	}
	var seq uint64
	c := t.st.clusters[a.Cluster]
	if c != nil {
		seq = c.assignSeq
	}
	if rej := t.fenceCheck(a.Fence, seq); rej != nil {
		return rej
	}
	if t.st.envs[a.Environment][a.Digest] == nil {
		return reject(RejectRevision, "revision %s is not recorded for Environment %s", a.Digest, a.Environment)
	}
	nc := &cluster{assignments: map[uint64]*Assignment{}}
	if c != nil {
		nc = c.clone()
	}
	a.Index = t.index
	nc.assignSeq = a.Fence.Seq
	nc.assignments[a.Fence.Seq] = &a
	setMap(t, t.st.clusters, a.Cluster, nc)
	return nil
}

func (t *txn) promote(p Promotion) *Rejection {
	if p.Cluster == "" || p.PromotedDigest.IsZero() || len(p.Signed) == 0 {
		return reject(RejectInvalid, "a promotion needs a Cluster, a promoted digest and its signed form")
	}
	var seq uint64
	c := t.st.clusters[p.Cluster]
	if c != nil {
		seq = c.promoSeq
	}
	if rej := t.fenceCheck(p.Fence, seq); rej != nil {
		return rej
	}
	recorded := false
	for _, env := range t.st.envs {
		if env[p.PromotedDigest] != nil {
			recorded = true
			break
		}
	}
	if !recorded {
		return reject(RejectRevision, "revision %s is not recorded", p.PromotedDigest)
	}
	nc := &cluster{assignments: map[uint64]*Assignment{}}
	if c != nil {
		nc = c.clone()
	}
	p.Index = t.index
	nc.promoSeq = p.Fence.Seq
	nc.promotion = &p
	setMap(t, t.st.clusters, p.Cluster, nc)
	return nil
}

func (t *txn) anchorSet(a AnchorSet) *Rejection {
	if a.Version == 0 || a.Epoch == 0 || len(a.Signed) == 0 {
		return reject(RejectInvalid, "an anchor set needs a version, a storeEpoch and its signed form")
	}
	if t.st.anchor != nil && a.Version <= t.st.anchor.Version {
		return reject(RejectAnchorSet, "version %d is not above the accepted %d", a.Version, t.st.anchor.Version)
	}
	if a.OpensEpoch() {
		// A new epoch resets every Cluster's sequences, including those a
		// stolen key inflated.
		oldEpoch, oldClusters := t.st.epoch, t.st.clusters
		t.st.epoch = a.Version
		t.st.clusters = map[string]*cluster{}
		t.save(func() { t.st.epoch, t.st.clusters = oldEpoch, oldClusters })
	} else if t.st.epoch == 0 || a.Epoch != t.st.epoch {
		return reject(RejectAnchorSet, "storeEpoch %d is neither the current %d nor the set's version %d", a.Epoch, t.st.epoch, a.Version)
	}
	a.Index = t.index
	old := t.st.anchor
	t.st.anchor = &a
	t.save(func() { t.st.anchor = old })
	return nil
}

func (t *txn) recordRevision(r RevisionRecord) *Rejection {
	if r.Environment == "" || r.Digest.IsZero() {
		return reject(RejectInvalid, "a Revision needs an Environment and a digest")
	}
	env := t.st.envs[r.Environment]
	if env != nil && env[r.Digest] != nil {
		return nil
	}
	if env == nil {
		env = map[Digest]*RevisionRecord{}
		setMap(t, t.st.envs, r.Environment, env)
	}
	r.RecordedAt = t.cmd.At
	r.Index = t.index
	setMap(t, env, r.Digest, &r)
	for _, d := range r.objects() {
		t.ref(d)
	}
	return nil
}

func (t *txn) deleteRevision(o DeleteRevision) *Rejection {
	env := t.st.envs[o.Environment]
	r := env[o.Digest]
	if r == nil {
		return reject(RejectRevision, "revision %s is not recorded for Environment %s", o.Digest, o.Environment)
	}
	recent := t.st.revisionsByAge(o.Environment)
	for i := 0; i < len(recent) && i < RevisionsKept; i++ {
		if recent[i].Digest == o.Digest {
			return reject(RejectRetained, "revision %s is among the newest %d of Environment %s", o.Digest, RevisionsKept, o.Environment)
		}
	}
	if t.st.revisionInUse(o.Environment, o.Digest) {
		return reject(RejectRetained, "an accepted assignment or promotion names revision %s", o.Digest)
	}
	delMap(t, env, o.Digest)
	for _, d := range r.objects() {
		t.unref(d)
	}
	if len(env) == 0 {
		delMap(t, t.st.envs, o.Environment)
	}
	return nil
}

func (t *txn) dropAssignments(o DropAssignments) *Rejection {
	c := t.st.clusters[o.Cluster]
	if c == nil {
		return nil
	}
	if o.Below > c.assignSeq {
		return reject(RejectInvalid, "below %d would drop the latest assignment %d", o.Below, c.assignSeq)
	}
	nc := c.clone()
	for seq := range nc.assignments {
		if seq < o.Below {
			delete(nc.assignments, seq)
		}
	}
	setMap(t, t.st.clusters, o.Cluster, nc)
	return nil
}

// setAudit replaces the audit state and records the undo.
func (t *txn) setAudit(a auditState) {
	old := t.st.audit
	t.st.audit = a
	t.save(func() { t.st.audit = old })
}

func (t *txn) appendAudit(o AppendAudit) *Rejection {
	if len(o.Entries) == 0 {
		return reject(RejectInvalid, "no audit entries")
	}
	a := t.st.audit
	a.entries = slices.Clone(a.entries)
	for _, e := range o.Entries {
		if len(e) == 0 {
			return reject(RejectInvalid, "an empty audit entry")
		}
		seq := a.nextSeq
		a.nextSeq++
		a.entries = append(a.entries, &AuditEntry{Seq: seq, Index: t.index, At: t.cmd.At, Data: e})
		t.auditSeqs = append(t.auditSeqs, seq)
	}
	t.setAudit(a)
	return nil
}

func (t *txn) sealAudit(o SealAudit) *Rejection {
	a := t.st.audit
	if o.Segment.IsZero() || o.Size <= 0 || o.Size > MaxAuditSegmentBytes {
		return reject(RejectInvalid, "a segment needs a digest and 1 to %d bytes", MaxAuditSegmentBytes)
	}
	if len(a.entries) == 0 {
		return reject(RejectAudit, "no unsealed entries")
	}
	if o.FirstSeq != a.entries[0].Seq || o.LastSeq < o.FirstSeq {
		return reject(RejectAudit, "entries %d to %d are not the oldest unsealed, from %d", o.FirstSeq, o.LastSeq, a.entries[0].Seq)
	}
	last := o.LastSeq - o.FirstSeq
	if last >= uint64(len(a.entries)) {
		return reject(RejectAudit, "entry %d is not committed", o.LastSeq)
	}
	if o.Prev != a.head {
		return reject(RejectAudit, "the segment chains to %s, not the head %s", o.Prev, a.head)
	}
	seg := &AuditSegment{
		Digest: o.Segment, Size: o.Size, FirstSeq: o.FirstSeq, LastSeq: o.LastSeq, Prev: o.Prev,
		FirstAt: a.entries[0].At, LastAt: a.entries[last].At, SealedAt: t.cmd.At, SealIndex: t.index,
	}
	a.entries = slices.Clone(a.entries[last+1:])
	a.segments = append(slices.Clone(a.segments), seg)
	a.head = o.Segment
	t.setAudit(a)
	t.ref(o.Segment)
	return nil
}

func (t *txn) pruneAudit(o PruneAudit) *Rejection {
	if o.Retention <= 0 {
		return reject(RejectInvalid, "a retention must be positive")
	}
	a := t.st.audit
	k := slices.IndexFunc(a.segments, func(s *AuditSegment) bool { return s.Digest == o.Through })
	if k < 0 {
		return reject(RejectAudit, "segment %s is not sealed", o.Through)
	}
	for _, seg := range a.segments[:k+1] {
		if seg.LastAt.Add(o.Retention).After(t.cmd.At) {
			return reject(RejectAudit, "segment %s holds entries within the %s retention", seg.Digest, o.Retention)
		}
		if t.st.backup == nil || seg.SealIndex > t.st.backup.Index {
			return reject(RejectAudit, "no recorded backup holds segment %s", seg.Digest)
		}
	}
	pruned := a.segments[:k+1]
	a.segments = slices.Clone(a.segments[k+1:])
	t.setAudit(a)
	for _, seg := range pruned {
		t.unref(seg.Digest)
	}
	return nil
}

func (t *txn) recordBackup(b BackupRecord) *Rejection {
	if b.ID == "" || b.Index == 0 || b.Index > t.index {
		return reject(RejectInvalid, "a backup needs an ID and an index at or below %d", t.index)
	}
	if t.st.backup != nil && b.Index <= t.st.backup.Index {
		return nil
	}
	b.At = t.cmd.At
	old := t.st.backup
	t.st.backup = &b
	t.save(func() { t.st.backup = old })
	return nil
}

func (t *txn) addPending(p PendingMember) *Rejection {
	if p.ServerID.Validate() != nil || p.Address == "" {
		return reject(RejectInvalid, "a pending member needs a server ID and an address")
	}
	if _, ok := t.st.revoked[p.ServerID]; ok {
		return reject(RejectMember, "server %s is revoked", p.ServerID)
	}
	if t.st.isMember(p.ServerID) {
		return reject(RejectMember, "server %s is a member", p.ServerID)
	}
	p.At = t.cmd.At
	setMap(t, t.st.pending, p.ServerID, p)
	return nil
}

func (t *txn) revokePeer(id ServerID) *Rejection {
	if id.Validate() != nil {
		return reject(RejectInvalid, "server ID %q is malformed", id)
	}
	if t.st.isMember(id) {
		return reject(RejectMember, "server %s is a member; remove it first", id)
	}
	if _, ok := t.st.revoked[id]; !ok {
		setMap(t, t.st.revoked, id, t.index)
	}
	delMap(t, t.st.pending, id)
	delMap(t, t.st.reports, id)
	return nil
}

func (t *txn) report(o Report) *Rejection {
	if o.ServerID.Validate() != nil {
		return reject(RejectInvalid, "server ID %q is malformed", o.ServerID)
	}
	if _, err := ParseVersion(o.BinaryVersion); err != nil {
		return reject(RejectInvalid, "binary version %q is malformed", o.BinaryVersion)
	}
	if _, ok := t.st.revoked[o.ServerID]; ok {
		return reject(RejectMember, "server %s is revoked", o.ServerID)
	}
	r, had := t.st.reports[o.ServerID]
	r.ServerID = o.ServerID
	if !had || r.BinaryVersion != o.BinaryVersion {
		r.BinaryVersion = o.BinaryVersion
		r.Since = t.cmd.At
	}
	r.SnapshotIndex = o.SnapshotIndex
	switch {
	case t.st.advance == nil:
	case o.SnapshotIndex >= t.st.advance.Index:
		if r.SnapshotSince.IsZero() {
			r.SnapshotSince = t.cmd.At
		}
	default:
		r.SnapshotSince = time.Time{}
	}
	setMap(t, t.st.reports, o.ServerID, r)
	return nil
}

func (t *txn) advanceVersion(o AdvanceVersion) *Rejection {
	if !t.st.versionSet || o.From != t.st.version {
		return reject(RejectVersion, "the Control Store version is %s, not %s", t.st.version, o.From)
	}
	if !o.To.Succeeds(o.From) {
		return reject(RejectVersion, "%s does not follow %s", o.To, o.From)
	}
	oldV, oldA, oldR := t.st.version, t.st.advance, t.st.reports
	t.st.version = o.To
	t.st.advance = &VersionAdvance{Index: t.index, From: o.From, To: o.To, At: t.cmd.At}
	// Every replica reports a snapshot past this entry anew.
	reports := make(map[ServerID]ReplicaReport, len(oldR))
	for id, r := range oldR {
		r.SnapshotSince = time.Time{}
		reports[id] = r
	}
	t.st.reports = reports
	t.save(func() { t.st.version, t.st.advance, t.st.reports = oldV, oldA, oldR })
	return nil
}

func (t *txn) initVersion(o InitVersion) *Rejection {
	if t.st.versionSet {
		return reject(RejectVersion, "the Control Store version is already %s", t.st.version)
	}
	old := t.st.version
	t.st.version, t.st.versionSet = o.Version, true
	t.save(func() { t.st.version, t.st.versionSet = old, false })
	return nil
}
