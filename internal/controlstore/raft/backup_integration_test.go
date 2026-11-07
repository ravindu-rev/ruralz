// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package raft

import (
	"bytes"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
)

// seeded writes every kind of state onto c: an epoch, a Revision with
// source and diff, a plan, users, sessions and tokens, an assignment, a
// promotion, audit entries and a sealed segment.
func seeded(t *testing.T, c *cluster) (rev controlstore.Object, old controlstore.Assignment, objs []controlstore.Object) {
	t.Helper()
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.AcceptAnchorSet{AnchorSet: controlstore.AnchorSet{Version: 11, Epoch: 11, Signed: []byte("root")}}}})
	rev = controlstore.Object{Kind: controlstore.KindRevision, Format: "ruralz.canonical.v1", Data: []byte(`{"kind":"Route"}`)}
	src := controlstore.Object{Kind: controlstore.KindSource, Format: "source.v1", Data: []byte("bundle tree")}
	diff := controlstore.Object{Kind: controlstore.KindDiff, Format: "ruralz.diff.v1", Data: []byte(`{"ops":[]}`)}
	plan := controlstore.Object{Kind: controlstore.KindPlan, Format: "plan.v1", Data: []byte("nodes")}
	c.propose(controlstore.Proposal{Content: []controlstore.Object{rev, src, diff}, Ops: []controlstore.Op{
		controlstore.RecordRevision{Revision: controlstore.RevisionRecord{Environment: "prod", Digest: rev.Digest(), Source: src.Digest(), Diff: diff.Digest()}},
	}})
	c.propose(controlstore.Proposal{Content: []controlstore.Object{plan}, Ops: []controlstore.Op{
		controlstore.Put{Bucket: "rollouts", Key: "01JR", Value: []byte("pending"), Refs: []controlstore.Digest{plan.Digest()}},
		controlstore.Put{Bucket: "users", Key: "alice", Value: []byte("admin")},
		controlstore.Put{Bucket: "sessions", Key: "s1", Value: []byte("cookie")},
		controlstore.Put{Bucket: "tokens", Key: "t1", Value: []byte("hash")},
	}})
	old = signed(rev.Digest(), controlstore.Fence{Epoch: 11, Seq: 1}, "old-online-key")
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Assign{Assignment: old}}})
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.AppendAudit{Entries: [][]byte{[]byte("e1"), []byte("e2")}}}})
	seg := controlstore.Object{Kind: controlstore.KindAuditSegment, Format: "audit.v1", Data: []byte("e1")}
	l := c.leader()
	if err := l.store.SealAudit(boundedCtx(t), seg, 1, 1); err != nil {
		t.Fatal(err)
	}
	return rev, old, []controlstore.Object{rev, src, diff, plan, seg}
}

// TestBackupThenRestore: a backup at one index holds the state and every
// object it references; a restore into an empty deployment opens a higher
// storeEpoch before any delivery, keeps one voter under a new server ID,
// drops old peer certificates, sessions and API tokens, and appends the
// audit fork record. The restored deployment's predecessor is fenced: its
// replicas cannot reach the restored one, and its assignments, of a lower
// epoch, are never served there.
func TestBackupThenRestore(t *testing.T) {
	c, rs := three(t)
	rev, old, objs := seeded(t, c)
	l := c.leader()
	follower := others(rs, l)[0]
	c.applied(l.store.machine.Index(), follower)
	var buf bytes.Buffer
	info, err := follower.store.Backup(boundedCtx(t), &buf)
	if err != nil {
		t.Fatal(err)
	}
	if info.Objects != len(objs) || info.Index == 0 || info.Version != (controlstore.Version{Major: 0, Minor: 2}) {
		t.Fatalf("BackupInfo = %+v", info)
	}
	ln := listen(t, "")
	dir := filepath.Join(t.TempDir(), "restored")
	plan := controlstore.RestorePlan{
		ServerID: "restored-1", Address: ln.Addr().String(),
		AnchorSet:   controlstore.AnchorSet{Version: 99, Epoch: 99, Signed: []byte("root-signed, new online key")},
		DropBuckets: []string{"sessions", "tokens"}, Fork: []byte("audit fork: restored"), At: testStart,
	}
	if _, err := Restore(boundedCtx(t), bytes.NewReader(buf.Bytes()), RestoreConfig{DataDir: dir, Plan: plan, BinaryVersion: "0.2.0"}); err != nil {
		t.Fatal(err)
	}
	restored := &replica{t: t, id: "restored-1", dir: dir, addr: ln.Addr().String()}
	_, restored.port, _ = net.SplitHostPort(ln.Addr().String())
	restored.cfg = c.config(restored, false, nil)
	restored.cfg.Listener = ln
	restored.store, err = Open(restored.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restored.stop)
	if _, err := restored.store.AwaitLeadership(boundedCtx(t)); err != nil {
		t.Fatal(err)
	}
	_ = restored.store.View(func(v controlstore.View) {
		if v.Epoch() != 99 {
			t.Errorf("Epoch = %d, want 99 before any delivery", v.Epoch())
		}
		if m := v.Members(); len(m) != 1 || m[0].ServerID != "restored-1" || !m[0].Voter {
			t.Errorf("Members = %+v", m)
		}
		for _, r := range rs {
			if !v.Revoked(r.id) {
				t.Errorf("old peer %s is not revoked", r.id)
			}
		}
		for _, b := range []string{"sessions", "tokens"} {
			v.Records(b, "", func(r controlstore.Record) bool {
				t.Errorf("record %s/%s survived the restore", r.Bucket, r.Key)
				return true
			})
		}
		if _, ok := v.Record("users", "alice"); !ok {
			t.Error("a user was lost")
		}
		if _, ok := v.Revision("prod", rev.Digest()); !ok {
			t.Error("the Revision was lost")
		}
		if v.Accepted(old) {
			t.Error("the predecessor's assignment is accepted after the restore")
		}
		e := v.AuditEntries()
		if string(e[len(e)-1].Data) != "audit fork: restored" {
			t.Errorf("the last audit entry is %q, want the fork record", e[len(e)-1].Data)
		}
		if len(v.AuditSegments()) != 1 {
			t.Errorf("segments = %d", len(v.AuditSegments()))
		}
	})
	for _, o := range objs {
		got, err := restored.store.Content(boundedCtx(t), o.Digest())
		if err != nil || !bytes.Equal(got.Data, o.Data) {
			t.Errorf("restored content %s: %v", o.Digest(), err)
		}
	}
	// The predecessor cannot commit into the restored deployment, nor be
	// served there as current.
	for _, r := range rs {
		if tc, _, err := r.store.trans.dial(boundedCtx(t), restored.addr, controlstore.ClassStatus); err == nil {
			_ = tc.SetReadDeadline(time.Now().Add(waitBound))
			if _, rerr := tc.Read(make([]byte, 1)); rerr == nil {
				t.Errorf("the restored replica talked to predecessor replica %s", r.id)
			}
			r.store.trans.release(tc)
		}
	}
	if restored.store.MayServe(old, false) || restored.store.MayServe(old, true) {
		t.Error("the restored replica may serve the predecessor's assignment")
	}
	if old.Fence.Compare(controlstore.Fence{Epoch: 99, Seq: 1}) >= 0 {
		t.Error("a predecessor fence orders at or above the restored epoch")
	}
	next := signed(rev.Digest(), controlstore.Fence{Epoch: 99, Seq: 1}, "new-online-key")
	ctx := boundedCtx(t)
	if _, err := restored.store.Propose(ctx, controlstore.Proposal{Ops: []controlstore.Op{controlstore.Assign{Assignment: next}}}); err != nil {
		t.Fatalf("the first assignment of the new epoch: %v", err)
	}
	if !restored.store.MayServe(next, true) {
		t.Error("the restored replica may not serve its new assignment")
	}
	if _, err := Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreConfig{DataDir: dir, Plan: plan, BinaryVersion: "0.2.0"}); !errors.Is(err, controlstore.ErrBackup) {
		t.Errorf("a restore into a non-empty Control Store = %v, want RZ-CP-015", err)
	}
}

// TestCommittedEntriesReplayWithoutLeader: a replica restarted while no
// majority is up applies its committed entries from the persisted commit
// index, so it loads, and is ready, with them before any leader contact.
func TestCommittedEntriesReplayWithoutLeader(t *testing.T) {
	c, rs := three(t)
	res := c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "rollouts", Key: "a", Value: []byte("1")}}})
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "rollouts", Key: "b", Value: []byte("2")}}})
	c.applied(res.Index+1, rs...)
	for _, r := range rs {
		r.stop()
	}
	if err := rs[0].restart(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rs[0].store.Ready():
	case <-time.After(waitBound):
		t.Fatal("Ready never closed without a quorum")
	}
	if l, _ := rs[0].store.Leader(); l != "" {
		t.Fatalf("a leader %s without a quorum", l)
	}
	if got := raftVoters(t, rs[0]); got != 3 {
		t.Errorf("the lone replica knows %d voters, want 3", got)
	}
	_ = rs[0].store.View(func(v controlstore.View) {
		for _, k := range []string{"a", "b"} {
			if _, ok := v.Record("rollouts", k); !ok {
				t.Errorf("committed record %s was not replayed", k)
			}
		}
	})
}

// TestUpgradeFinalize walks one deployment from 0.2 to 0.3: replicas
// report the new binary, finalize waits for the 24-hour soak, the
// version-advance entry commits with its audit entry, every replica
// snapshots past it, a 0.2 binary then halts, and a 0.4 binary's report
// is rejected with RZ-CP-021 while a replica has not reported its
// snapshot past the advance.
func TestUpgradeFinalize(t *testing.T) {
	c, rs := three(t)
	ctx := boundedCtx(t)
	if err := c.leader().store.Finalize(ctx, nil); !errors.Is(err, controlstore.ErrFinalize) {
		t.Errorf("finalize with no upgrade = %v, want RZ-CP-021", err)
	}
	for _, r := range rs {
		r.cfg.BinaryVersion = "0.3.0"
		if err := r.restart(); err != nil {
			t.Fatal(err)
		}
		c.leader()
	}
	waitFor(t, "every replica to report 0.3.0", func() bool {
		c.fake.Advance(ReportInterval)
		st, err := c.leader().store.FinalizeStatus()
		return err == nil && !st.SoakStart.IsZero()
	})
	l := c.leader()
	if err := l.store.Finalize(ctx, nil); !errors.Is(err, controlstore.ErrFinalize) {
		t.Errorf("finalize during the soak = %v, want RZ-CP-021", err)
	}
	// r3 stops: it will not report a snapshot past the advance.
	lagging := others(rs, l)[1]
	lagging.stop()
	c.fake.Advance(controlstore.FinalizeSoak)
	l = c.leader()
	if err := l.store.Finalize(ctx, []byte("audit: finalize 0.3")); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	var adv controlstore.VersionAdvance
	_ = l.store.View(func(v controlstore.View) {
		adv, _ = v.LastAdvance()
		if got, _ := v.Version(); got != (controlstore.Version{Major: 0, Minor: 3}) {
			t.Errorf("Version = %v", got)
		}
		found := false
		for _, e := range v.AuditEntries() {
			found = found || (string(e.Data) == "audit: finalize 0.3" && e.Index == adv.Index)
		}
		if !found {
			t.Error("the finalize audit entry is not in the version-advance entry")
		}
	})
	waitFor(t, "the live replicas to snapshot past the advance and truncate their logs", func() bool {
		for _, r := range others(rs, lagging) {
			first, err := r.store.logs.FirstIndex()
			if err != nil || r.store.newestSnapshot() < adv.Index || (first != 0 && first <= adv.Index) {
				return false
			}
		}
		return true
	})
	// A 0.4 binary is refused while r3 has not reported its snapshot.
	resp := l.store.handleReport(ctx, "r9", replicaStatus{ServerID: "r9", Address: "127.0.0.1:1", BinaryVersion: "0.4.0"})
	if resp.Accepted || resp.Code != controlstore.CodeFinalize {
		t.Errorf("a 0.4 report = %+v, want RZ-CP-021", resp)
	}
	down := others(rs, l)[0]
	if down == lagging {
		down = others(rs, l)[1]
	}
	// A 0.4 replica's report is rejected, and it halts before serving.
	down.cfg.BinaryVersion = "0.4.0"
	rejected := make(chan *controlstore.HaltError, 1)
	down.cfg.OnHalt = func(h *controlstore.HaltError) { rejected <- h }
	if err := down.restart(); err != nil {
		t.Fatalf("a 0.4 binary on a snapshot past the advance: %v", err)
	}
	select {
	case h := <-rejected:
		if h.Reason != controlstore.HaltReport {
			t.Errorf("halt = %v, want a rejected report", h)
		}
	case <-time.After(waitBound):
		t.Fatal("the 0.4 replica kept serving after its report was rejected")
	}
	if err := down.store.View(func(controlstore.View) {}); !errors.Is(err, controlstore.ErrHalted) {
		t.Errorf("a halted replica serves views: %v", err)
	}
	// A 0.2 binary meets a Control Store version above its own and halts.
	down.cfg.BinaryVersion = "0.2.0"
	var halted *controlstore.HaltError
	down.cfg.OnHalt = func(h *controlstore.HaltError) { halted = h }
	err := down.restart()
	var h *controlstore.HaltError
	if !errors.As(err, &h) || h.Reason != controlstore.HaltVersion || halted == nil {
		t.Errorf("a 0.2 binary after finalize: %v (OnHalt %v), want a version halt", err, halted)
	}
}
