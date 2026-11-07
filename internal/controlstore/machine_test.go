// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"slices"
	"testing"
	"time"
)

// TestAssignmentAcceptedOnlyAtNextSequence is the apply-time sequence
// check: an assignment is accepted only at the current epoch and the
// current sequence plus one; a taken sequence, a gap and another epoch are
// rejected with the stream's current fence, and change nothing.
func TestAssignmentAcceptedOnlyAtNextSequence(t *testing.T) {
	f := newFixture(t)
	f.openEpoch(100)
	d := f.record("rev-a")
	f.mustAccept(Assign{Assignment: assignment("prod-eu", "prod", d, 100, 1, "leader-1")})
	cases := []struct {
		name  string
		fence Fence
	}{
		{"taken sequence", Fence{Epoch: 100, Seq: 1}},
		{"gap", Fence{Epoch: 100, Seq: 3}},
		{"older epoch", Fence{Epoch: 99, Seq: 2}},
		{"newer epoch", Fence{Epoch: 101, Seq: 2}},
		{"zero", Fence{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rej := f.rejection(RejectFence, Assign{Assignment: assignment("prod-eu", "prod", d, tc.fence.Epoch, tc.fence.Seq, "deposed")})
			if want := (Fence{Epoch: 100, Seq: 1}); rej.Fence != want {
				t.Errorf("rejection fence = %+v, want %+v so the leader re-signs at %+v", rej.Fence, want, want.Next())
			}
		})
	}
	f.view(func(v View) {
		if got := v.AssignmentFence("prod-eu"); got != (Fence{Epoch: 100, Seq: 1}) {
			t.Errorf("AssignmentFence = %+v, want (100, 1)", got)
		}
		if a, _ := v.LatestAssignment("prod-eu"); string(a.Signed) != "leader-1" {
			t.Errorf("latest assignment signed by %q, want leader-1", a.Signed)
		}
	})
	f.mustAccept(Assign{Assignment: assignment("prod-eu", "prod", d, 100, 2, "leader-1")})
}

// TestFirstEntryAtASequenceWins is the deposed-leader case: two signed
// entries at one sequence both commit; only the first in log order is
// accepted, and the other is never an accepted (servable) assignment.
func TestFirstEntryAtASequenceWins(t *testing.T) {
	f := newFixture(t)
	f.openEpoch(7)
	d := f.record("rev-a")
	e := f.record("rev-b")
	newLeader := assignment("prod-eu", "prod", d, 7, 1, "new-leader")
	deposed := assignment("prod-eu", "prod", e, 7, 1, "deposed-leader")
	f.mustAccept(Assign{Assignment: newLeader})
	f.mustReject(RejectFence, Assign{Assignment: deposed})
	f.view(func(v View) {
		if !v.Accepted(newLeader) {
			t.Error("the first entry at sequence 1 is not accepted")
		}
		if v.Accepted(deposed) {
			t.Error("the deposed leader's entry at the taken sequence is accepted")
		}
	})
}

// TestPromotionFenced applies the same rule to promotions.
func TestPromotionFenced(t *testing.T) {
	f := newFixture(t)
	f.openEpoch(5)
	d := f.record("rev-a")
	p := func(seq uint64, s string) Promote {
		return Promote{Promotion: Promotion{Cluster: "prod-eu", PromotedDigest: d, Fence: Fence{Epoch: 5, Seq: seq}, Signed: []byte(s)}}
	}
	f.mustAccept(p(1, "first"))
	rej := f.rejection(RejectFence, p(1, "deposed"))
	if rej.Fence != (Fence{Epoch: 5, Seq: 1}) {
		t.Errorf("rejection fence = %+v", rej.Fence)
	}
	f.mustReject(RejectFence, p(3, "gap"))
	f.mustAccept(p(2, "second"))
	f.view(func(v View) {
		got, _ := v.Promotion("prod-eu")
		if string(got.Signed) != "second" || got.Fence.Seq != 2 {
			t.Errorf("promotion = %+v, want the second", got)
		}
	})
	f.mustReject(RejectRevision, Promote{Promotion: Promotion{Cluster: "prod-eu", PromotedDigest: digest("unknown"), Fence: Fence{Epoch: 5, Seq: 3}, Signed: []byte("x")}})
}

// TestNoEpochNoAssignment: before an anchor set opens a storeEpoch nothing
// is assigned or promoted.
func TestNoEpochNoAssignment(t *testing.T) {
	f := newFixture(t)
	d := f.record("rev-a")
	f.mustReject(RejectNoEpoch, Assign{Assignment: assignment("prod-eu", "prod", d, 0, 1, "x")})
	f.mustReject(RejectNoEpoch, Promote{Promotion: Promotion{Cluster: "prod-eu", PromotedDigest: d, Fence: Fence{Seq: 1}, Signed: []byte("x")}})
}

// TestNewEpochResetsSequences: an anchor set that opens an epoch resets
// every Cluster's sequences, including inflated ones, so the next
// assignment is (epoch, 1); entries of the old epoch are no longer
// accepted or servable.
func TestNewEpochResetsSequences(t *testing.T) {
	f := newFixture(t)
	f.openEpoch(10)
	d := f.record("rev-a")
	for seq := uint64(1); seq <= 3; seq++ {
		f.mustAccept(Assign{Assignment: assignment("prod-eu", "prod", d, 10, seq, "old-key")})
	}
	old := assignment("prod-eu", "prod", d, 10, 3, "old-key")
	f.openEpoch(20)
	f.view(func(v View) {
		if v.Epoch() != 20 {
			t.Errorf("Epoch = %d, want 20", v.Epoch())
		}
		if got := v.AssignmentFence("prod-eu"); got != (Fence{Epoch: 20}) {
			t.Errorf("AssignmentFence = %+v, want (20, 0)", got)
		}
		if v.Accepted(old) {
			t.Error("an assignment of the old epoch is still accepted")
		}
	})
	f.mustReject(RejectFence, Assign{Assignment: assignment("prod-eu", "prod", d, 10, 4, "old-key")})
	f.mustAccept(Assign{Assignment: assignment("prod-eu", "prod", d, 20, 1, "new-key")})
}

// TestAnchorSetOrdering: only a higher version is accepted; a set that
// does not open an epoch carries the current one; the first opens one.
func TestAnchorSetOrdering(t *testing.T) {
	f := newFixture(t)
	f.mustReject(RejectAnchorSet, AcceptAnchorSet{AnchorSet: AnchorSet{Version: 5, Epoch: 3, Signed: []byte("a")}})
	f.openEpoch(5)
	f.mustReject(RejectAnchorSet, AcceptAnchorSet{AnchorSet: AnchorSet{Version: 5, Epoch: 5, Signed: []byte("same")}})
	f.mustReject(RejectAnchorSet, AcceptAnchorSet{AnchorSet: AnchorSet{Version: 4, Epoch: 4, Signed: []byte("lower")}})
	f.mustReject(RejectAnchorSet, AcceptAnchorSet{AnchorSet: AnchorSet{Version: 6, Epoch: 2, Signed: []byte("other epoch")}})
	f.mustAccept(AcceptAnchorSet{AnchorSet: AnchorSet{Version: 6, Epoch: 5, Signed: []byte("rotation")}})
	f.view(func(v View) {
		a, _ := v.AnchorSet()
		if a.Version != 6 || v.Epoch() != 5 {
			t.Errorf("anchor %d epoch %d, want 6 and 5", a.Version, v.Epoch())
		}
	})
	f.mustReject(RejectInvalid, AcceptAnchorSet{AnchorSet: AnchorSet{Version: 9, Epoch: 9}})
}

// TestRejectedCommandChangesNothing: a command is atomic, so a failing op
// rolls back the ops before it.
func TestRejectedCommandChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.openEpoch(1)
	d := f.record("rev-a")
	rej := f.rejection(RejectFence,
		Put{Bucket: "rollouts", Key: "r1", Value: []byte("state")},
		AppendAudit{Entries: [][]byte{[]byte("audit")}},
		Assign{Assignment: assignment("prod-eu", "prod", d, 1, 2, "x")},
	)
	if rej.Op != 2 {
		t.Errorf("rejected op = %d, want 2", rej.Op)
	}
	f.view(func(v View) {
		if _, ok := v.Record("rollouts", "r1"); ok {
			t.Error("the put of a rejected command applied")
		}
		if len(v.AuditEntries()) != 0 {
			t.Error("the audit entry of a rejected command applied")
		}
	})
}

// TestRecordPreconditions covers absent and index conditions.
func TestRecordPreconditions(t *testing.T) {
	f := newFixture(t)
	res := f.mustAccept(Put{Bucket: "users", Key: "alice", Value: []byte("v1"), Cond: Cond{Absent: true}})
	f.mustReject(RejectCondition, Put{Bucket: "users", Key: "alice", Value: []byte("v2"), Cond: Cond{Absent: true}})
	f.mustReject(RejectCondition, Put{Bucket: "users", Key: "alice", Value: []byte("v2"), Cond: Cond{Index: res.Index + 100}})
	f.mustAccept(Put{Bucket: "users", Key: "alice", Value: []byte("v2"), Cond: Cond{Index: res.Index}})
	f.mustReject(RejectCondition, Delete{Bucket: "users", Key: "alice", Cond: Cond{Index: res.Index}})
	f.mustReject(RejectInvalid, Put{Bucket: "Users", Key: "x"})
	f.mustReject(RejectInvalid, Put{Bucket: "users", Key: ""})
	f.mustReject(RejectInvalid, Put{Bucket: "users", Key: "x", Cond: Cond{Absent: true, Index: 1}})
	f.view(func(v View) {
		r, ok := v.Record("users", "alice")
		if !ok || string(r.Value) != "v2" || r.CreateIndex != res.Index {
			t.Errorf("record = %+v, want v2 created at %d", r, res.Index)
		}
	})
	f.mustAccept(Delete{Bucket: "users", Key: "alice"})
	f.mustAccept(Delete{Bucket: "users", Key: "nobody"})
	f.mustReject(RejectCondition, Delete{Bucket: "users", Key: "nobody", Cond: Cond{Index: 3}})
}

// TestReferencesReleaseContent: an object is released only when its last
// reference goes, by an overwrite or a delete.
func TestReferencesReleaseContent(t *testing.T) {
	f := newFixture(t)
	plan := digest("plan")
	shared := digest("shared")
	f.mustAccept(Put{Bucket: "rollouts", Key: "a", Value: []byte("a"), Refs: []Digest{plan, shared}})
	f.mustAccept(Put{Bucket: "rollouts", Key: "b", Value: []byte("b"), Refs: []Digest{shared}})
	res := f.mustAccept(Put{Bucket: "rollouts", Key: "a", Value: []byte("a2")})
	if !slices.Equal(res.Released, []Digest{plan}) {
		t.Errorf("Released = %v, want only the plan", res.Released)
	}
	if res := f.mustAccept(Delete{Bucket: "rollouts", Key: "b"}); !slices.Equal(res.Released, []Digest{shared}) {
		t.Errorf("Released = %v, want the shared object", res.Released)
	}
	f.view(func(v View) {
		if v.Referenced(plan) || v.Referenced(shared) {
			t.Error("released objects are still referenced")
		}
	})
	f.mustReject(RejectInvalid, Put{Bucket: "rollouts", Key: "c", Refs: []Digest{{}}})
}

// TestRevisionRetention: a Revision is deleted only beyond the newest 50
// of its Environment and while no accepted assignment or promotion names
// it; its objects are released unless another Revision shares them.
func TestRevisionRetention(t *testing.T) {
	f := newFixture(t)
	f.openEpoch(1)
	src := digest("source")
	var first Digest
	for i := range RevisionsKept + 2 {
		d := digest("rev-" + string(rune('A'+i)))
		if i == 0 {
			first = d
		}
		f.mustAccept(RecordRevision{Revision: RevisionRecord{Environment: "prod", Digest: d, Source: src, Diff: digest("diff-" + string(rune('A'+i)))}})
	}
	newest := digest("rev-" + string(rune('A'+RevisionsKept+1)))
	f.mustReject(RejectRetained, DeleteRevision{Environment: "prod", Digest: newest})
	f.mustAccept(Assign{Assignment: assignment("prod-eu", "prod", first, 1, 1, "x")})
	f.mustReject(RejectRetained, DeleteRevision{Environment: "prod", Digest: first})
	second := digest("rev-B")
	res := f.mustAccept(DeleteRevision{Environment: "prod", Digest: second})
	if want := []Digest{second, digest("diff-B")}; !sameDigests(res.Released, want) {
		t.Errorf("Released = %v, want the content and diff, not the shared source", res.Released)
	}
	f.mustReject(RejectRevision, DeleteRevision{Environment: "prod", Digest: second})
	f.mustReject(RejectRevision, Assign{Assignment: assignment("prod-eu", "staging", first, 1, 2, "x")})
	f.view(func(v View) {
		if !v.Referenced(src) {
			t.Error("the shared source tree was released")
		}
		if got := len(v.Revisions("prod")); got != RevisionsKept+1 {
			t.Errorf("Revisions = %d, want %d", got, RevisionsKept+1)
		}
	})
	// Recording the same Revision again keeps the first record.
	before := f.idx
	f.mustAccept(RecordRevision{Revision: RevisionRecord{Environment: "prod", Digest: first}})
	f.view(func(v View) {
		if r, _ := v.Revision("prod", first); r.Index > before {
			t.Errorf("a repeated record moved the Revision to index %d", r.Index)
		}
	})
}

// sameDigests compares digest sets.
func sameDigests(a, b []Digest) bool {
	key := func(ds []Digest) []string {
		out := make([]string, len(ds))
		for i, d := range ds {
			out[i] = d.String()
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(key(a), key(b))
}

// TestDropAssignmentsKeepsLatest bounds history without dropping the
// latest assignment.
func TestDropAssignmentsKeepsLatest(t *testing.T) {
	f := newFixture(t)
	f.openEpoch(1)
	d := f.record("rev-a")
	for seq := uint64(1); seq <= 3; seq++ {
		f.mustAccept(Assign{Assignment: assignment("prod-eu", "prod", d, 1, seq, "x")})
	}
	f.mustReject(RejectInvalid, DropAssignments{Cluster: "prod-eu", Below: 4})
	f.mustAccept(DropAssignments{Cluster: "prod-eu", Below: 3})
	f.view(func(v View) {
		if got := len(v.Assignments("prod-eu")); got != 1 {
			t.Errorf("assignments = %d, want 1", got)
		}
		if _, ok := v.Assignment("prod-eu", 3); !ok {
			t.Error("the latest assignment was dropped")
		}
	})
}

// TestAuditSealAndPrune: entries number in order; a seal takes the oldest
// entries and chains to the head; a prune needs the retention and a
// backup holding the segment.
func TestAuditSealAndPrune(t *testing.T) {
	f := newFixture(t)
	res := f.mustAccept(AppendAudit{Entries: [][]byte{[]byte("e1"), []byte("e2"), []byte("e3")}})
	if !slices.Equal(res.AuditSeqs, []uint64{1, 2, 3}) {
		t.Fatalf("AuditSeqs = %v", res.AuditSeqs)
	}
	seg1 := digest("segment-1")
	f.mustReject(RejectAudit, SealAudit{Segment: seg1, Size: 10, FirstSeq: 2, LastSeq: 2})
	f.mustReject(RejectAudit, SealAudit{Segment: seg1, Size: 10, FirstSeq: 1, LastSeq: 9})
	f.mustReject(RejectAudit, SealAudit{Segment: seg1, Size: 10, FirstSeq: 1, LastSeq: 2, Prev: digest("x")})
	f.mustReject(RejectInvalid, SealAudit{Segment: seg1, Size: MaxAuditSegmentBytes + 1, FirstSeq: 1, LastSeq: 2})
	f.mustAccept(SealAudit{Segment: seg1, Size: 10, FirstSeq: 1, LastSeq: 2})
	f.at = f.at.Add(time.Hour)
	f.mustAccept(AppendAudit{Entries: [][]byte{[]byte("e4")}})
	seg2 := digest("segment-2")
	f.mustReject(RejectAudit, SealAudit{Segment: seg2, Size: 10, FirstSeq: 3, LastSeq: 4})
	f.mustAccept(SealAudit{Segment: seg2, Size: 10, FirstSeq: 3, LastSeq: 4, Prev: seg1})
	f.view(func(v View) {
		if v.AuditHead() != seg2 || len(v.AuditSegments()) != 2 || len(v.AuditEntries()) != 0 {
			t.Errorf("head %v, %d segments, %d entries", v.AuditHead(), len(v.AuditSegments()), len(v.AuditEntries()))
		}
	})
	retention := 24 * time.Hour
	f.at = t0.Add(25 * time.Hour)
	f.mustReject(RejectAudit, PruneAudit{Through: seg1, Retention: retention})
	backupIdx := f.idx
	f.mustAccept(RecordBackup{Backup: BackupRecord{ID: "b1", Index: backupIdx}})
	f.mustReject(RejectAudit, PruneAudit{Through: seg2, Retention: 2 * retention})
	res = f.mustAccept(PruneAudit{Through: seg1, Retention: retention})
	if !slices.Equal(res.Released, []Digest{seg1}) {
		t.Errorf("Released = %v, want segment 1", res.Released)
	}
	f.at = t0.Add(72 * time.Hour)
	// Segment 2 was sealed before the backup: held.
	f.mustAccept(PruneAudit{Through: seg2, Retention: retention})
	f.mustReject(RejectInvalid, PruneAudit{Through: seg2})
}

// TestAuditPruneNeedsBackupOfTheSegment: a backup older than a segment's
// seal does not hold it.
func TestAuditPruneNeedsBackupOfTheSegment(t *testing.T) {
	f := newFixture(t)
	f.mustAccept(AppendAudit{Entries: [][]byte{[]byte("e1")}})
	f.mustAccept(RecordBackup{Backup: BackupRecord{ID: "early", Index: f.idx}})
	seg := digest("seg")
	f.mustAccept(SealAudit{Segment: seg, Size: 1, FirstSeq: 1, LastSeq: 1})
	f.at = t0.Add(400 * 24 * time.Hour)
	f.mustReject(RejectAudit, PruneAudit{Through: seg, Retention: DefaultAuditRetention})
	f.mustAccept(RecordBackup{Backup: BackupRecord{ID: "late", Index: f.idx}})
	f.mustAccept(PruneAudit{Through: seg, Retention: DefaultAuditRetention})
}

// TestBackupRecordKeepsNewest keeps the highest index.
func TestBackupRecordKeepsNewest(t *testing.T) {
	f := newFixture(t)
	f.mustAccept(Put{Bucket: "x", Key: "y"})
	f.mustAccept(RecordBackup{Backup: BackupRecord{ID: "b2", Index: 2}})
	f.mustAccept(RecordBackup{Backup: BackupRecord{ID: "b1", Index: 1}})
	f.mustReject(RejectInvalid, RecordBackup{Backup: BackupRecord{ID: "future", Index: 999}})
	f.view(func(v View) {
		if b, _ := v.Backup(); b.ID != "b2" {
			t.Errorf("backup = %+v, want b2", b)
		}
	})
}

// TestMembershipRecords: the configuration mirror clears pending members;
// a revoked or current member cannot become pending; a current member
// cannot be revoked; revoking drops a pending member and its report.
func TestMembershipRecords(t *testing.T) {
	f := newFixture(t)
	f.mustAccept(AddPending{Member: PendingMember{ServerID: "r2", Address: "10.0.0.2:8092"}})
	f.mustAccept(Report{ServerID: "r2", BinaryVersion: "0.2.0", SnapshotIndex: 0})
	f.idx++
	f.m.SetConfiguration(f.idx, []Member{{ServerID: "r1", Address: "10.0.0.1:8092", Voter: true}, {ServerID: "r2", Address: "10.0.0.2:8092"}})
	f.view(func(v View) {
		if _, ok := v.PendingMember("r2"); ok {
			t.Error("a member is still pending")
		}
		if !v.IsMember("r2") || !v.KnowsMembers() {
			t.Error("the configuration mirror lacks r2")
		}
	})
	f.mustReject(RejectMember, AddPending{Member: PendingMember{ServerID: "r2", Address: "a:1"}})
	f.mustReject(RejectMember, RevokePeer{ServerID: "r2"})
	f.mustAccept(AddPending{Member: PendingMember{ServerID: "r3", Address: "10.0.0.3:8092"}})
	f.mustAccept(Report{ServerID: "r3", BinaryVersion: "0.2.0"})
	f.mustAccept(RevokePeer{ServerID: "r3"})
	f.view(func(v View) {
		if _, ok := v.PendingMember("r3"); ok || !v.Revoked("r3") {
			t.Error("r3 is pending or not revoked")
		}
		if _, ok := v.Reports()["r3"]; ok {
			t.Error("a revoked server keeps its report")
		}
	})
	f.mustReject(RejectMember, AddPending{Member: PendingMember{ServerID: "r3", Address: "a:1"}})
	f.mustReject(RejectMember, Report{ServerID: "r3", BinaryVersion: "0.2.0"})
	f.mustReject(RejectInvalid, AddPending{Member: PendingMember{ServerID: "bad id", Address: "a:1"}})
}

// TestReportSinceAndSnapshot: Since restarts when the binary version
// changes; SnapshotSince marks the first report at or past the last
// version-advance entry and resets at the next advance.
func TestReportSinceAndSnapshot(t *testing.T) {
	f := newFixtureOf(t, Version{Major: 0, Minor: 3})
	f.mustAccept(Report{ServerID: "r1", BinaryVersion: "0.3.0"})
	first := f.at
	f.at = f.at.Add(time.Hour)
	f.mustAccept(Report{ServerID: "r1", BinaryVersion: "0.3.0", SnapshotIndex: 5})
	f.view(func(v View) {
		if r := v.Reports()["r1"]; !r.Since.Equal(first) || r.SnapshotIndex != 5 {
			t.Errorf("report = %+v, want Since at the first report", r)
		}
	})
	f.at = f.at.Add(time.Hour)
	f.mustAccept(Report{ServerID: "r1", BinaryVersion: "0.2.0"})
	f.view(func(v View) {
		if r := v.Reports()["r1"]; !r.Since.Equal(f.at) {
			t.Errorf("Since = %v, want the time of the version change", r.Since)
		}
	})
	f.mustAccept(AdvanceVersion{From: v02, To: Version{Major: 0, Minor: 3}})
	adv := f.idx
	f.at = f.at.Add(time.Hour)
	f.mustAccept(Report{ServerID: "r1", BinaryVersion: "0.3.0", SnapshotIndex: adv - 1})
	f.view(func(v View) {
		if r := v.Reports()["r1"]; !r.SnapshotSince.IsZero() {
			t.Errorf("SnapshotSince = %v before the snapshot passed the advance", r.SnapshotSince)
		}
	})
	f.mustAccept(Report{ServerID: "r1", BinaryVersion: "0.3.0", SnapshotIndex: adv})
	f.view(func(v View) {
		if r := v.Reports()["r1"]; !r.SnapshotSince.Equal(f.at) {
			t.Errorf("SnapshotSince = %v, want %v", r.SnapshotSince, f.at)
		}
	})
}

// TestVersionEntries: InitVersion sets the version once; AdvanceVersion
// moves exactly one release minor from the current version.
func TestVersionEntries(t *testing.T) {
	f := newFixture(t)
	f.mustReject(RejectVersion, InitVersion{Version: v02})
	f.mustReject(RejectVersion, AdvanceVersion{From: Version{Major: 0, Minor: 1}, To: v02})
	f.mustReject(RejectVersion, AdvanceVersion{From: v02, To: v02})
	f.view(func(v View) {
		if got, set := v.Version(); !set || got != v02 {
			t.Errorf("Version = %v %v", got, set)
		}
		if _, ok := v.LastAdvance(); ok {
			t.Error("a last advance before any")
		}
	})
}

// TestHaltNeverSkips: an entry in a newer Control Store version, an
// advance or init above the binary, and an unknown op halt the machine
// without applying anything.
func TestHaltNeverSkips(t *testing.T) {
	encode := func(c Command) []byte {
		t.Helper()
		data, err := EncodeCommand(c)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	v03 := Version{Major: 0, Minor: 3}
	cases := []struct {
		name   string
		reason string
		data   []byte
	}{
		{"entry version above the binary", HaltVersion, encode(Command{Version: v03, At: t0, Ops: []Op{Put{Bucket: "a", Key: "b"}}})},
		{"advance above the binary", HaltVersion, encode(Command{Version: v02, At: t0, Ops: []Op{AdvanceVersion{From: v02, To: v03}}})},
		{"unknown op", HaltEntry, []byte(`{"v":{"major":0,"minor":2},"at":"2026-11-07T10:00:00Z","ops":[{"type":"futureOp","op":{}}]}`)},
		{"unknown field", HaltEntry, []byte(`{"v":{"major":0,"minor":2},"at":"2026-11-07T10:00:00Z","ops":[{"type":"put","op":{"bucket":"a","key":"b","value":null,"cond":{},"newField":1}}]}`)},
		{"malformed", HaltEntry, []byte("{")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			before := f.m.Index()
			f.mustHalt(tc.reason, tc.data)
			if f.m.Index() != before {
				t.Errorf("Index = %d after a halt, want %d", f.m.Index(), before)
			}
		})
	}
	f := &fixture{t: t, m: NewMachine(v02), at: t0}
	f.mustHalt(HaltVersion, encode(Command{Version: v02, At: t0, Ops: []Op{InitVersion{Version: v03}}}))
}

// TestCheckRun applies rule 4 at startup.
func TestCheckRun(t *testing.T) {
	v01, v03, v04 := Version{Major: 0, Minor: 1}, Version{Major: 0, Minor: 3}, Version{Major: 0, Minor: 4}
	mk := func(binary Version, advanceAt uint64) *Machine {
		m := NewMachine(binary)
		m.st.version, m.st.versionSet = v02, true
		if advanceAt > 0 {
			m.st.advance = &VersionAdvance{Index: advanceAt, From: v01, To: v02}
		}
		return m
	}
	cases := []struct {
		name     string
		binary   Version
		advance  uint64
		snapshot uint64
		reason   string
	}{
		{"same version", v02, 10, 0, ""},
		{"binary older", v01, 0, 0, HaltVersion},
		{"next minor, no advance", v03, 0, 0, ""},
		{"next minor, snapshot past the advance", v03, 10, 10, ""},
		{"next minor, snapshot before the advance", v03, 10, 9, HaltSnapshot},
		{"skipped minor", v04, 0, 0, HaltSkippedMinor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mk(tc.binary, tc.advance).CheckRun(tc.snapshot)
			var h *HaltError
			switch {
			case tc.reason == "" && err != nil:
				t.Errorf("CheckRun = %v, want nil", err)
			case tc.reason != "" && (!asHalt(err, &h) || h.Reason != tc.reason):
				t.Errorf("CheckRun = %v, want a %s halt", err, tc.reason)
			}
		})
	}
	if err := NewMachine(v02).CheckRun(0); err != nil {
		t.Errorf("CheckRun of an empty store = %v", err)
	}
}

// asHalt is errors.As for a *HaltError.
func asHalt(err error, h **HaltError) bool {
	x, ok := err.(*HaltError) //nolint:errorlint // CheckRun returns the *HaltError unwrapped.
	if ok {
		*h = x
	}
	return ok
}

// TestChangedWakes: every applied index closes the Changed channel.
func TestChangedWakes(t *testing.T) {
	f := newFixture(t)
	ch := f.m.Changed()
	select {
	case <-ch:
		t.Fatal("Changed closed before an apply")
	default:
	}
	f.mustReject(RejectCondition, Delete{Bucket: "a", Key: "b", Cond: Cond{Index: 9}})
	select {
	case <-ch:
	default:
		t.Fatal("a rejected entry did not wake Changed")
	}
}
