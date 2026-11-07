// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
)

// versionOf reads a replica's Control Store version.
func versionOf(r *replica) controlstore.Version {
	var v controlstore.Version
	_ = r.store.View(func(view controlstore.View) { v, _ = view.Version() })
	return v
}

// TestTimedSoakFinalizes: with a timed soak the leader commits the
// version advance itself, with the audit entry the soak builds, once the
// conditions held for the healthy time after the 24-hour soak.
func TestTimedSoakFinalizes(t *testing.T) {
	c := newCluster(t)
	r := c.bootstrap()
	c.leader()
	waitFor(t, "the Control Store version", func() bool { return versionOf(r) == (controlstore.Version{Major: 0, Minor: 2}) })
	r.cfg.BinaryVersion = "0.3.0"
	r.cfg.TimedSoak = time.Hour
	r.cfg.TimedFinalizeAudit = func(st controlstore.FinalizeStatus) []byte {
		return []byte("audit: timed finalize to " + st.Target.String())
	}
	if err := r.restart(); err != nil {
		t.Fatal(err)
	}
	c.leader()
	var st controlstore.FinalizeStatus
	waitFor(t, "the timed finalize time", func() bool {
		var err error
		st, err = r.store.FinalizeStatus()
		return err == nil && !st.FinalizesAt.IsZero()
	})
	if want := st.SoakStart.Add(controlstore.FinalizeSoak + time.Hour); !st.FinalizesAt.Equal(want) {
		t.Fatalf("FinalizesAt = %v, want %v", st.FinalizesAt, want)
	}
	if err := c.fake.BlockUntilDue(boundedCtx(t), st.FinalizesAt); err != nil {
		t.Fatal(err)
	}
	c.fake.Advance(controlstore.FinalizeSoak)
	if versionOf(r) != (controlstore.Version{Major: 0, Minor: 2}) {
		t.Fatal("finalized before the healthy time")
	}
	c.fake.Set(st.FinalizesAt)
	waitFor(t, "the timed finalize", func() bool { return versionOf(r) == (controlstore.Version{Major: 0, Minor: 3}) })
	_ = r.store.View(func(v controlstore.View) {
		adv, _ := v.LastAdvance()
		found := false
		for _, e := range v.AuditEntries() {
			found = found || (e.Index == adv.Index && string(e.Data) == "audit: timed finalize to 0.3")
		}
		if !found {
			t.Error("the timed finalize committed no audit entry with the advance")
		}
	})
}

// TestAuditRetention: the leader prunes a sealed segment once its newest
// entry passed the retention and a recorded backup holds it, and only
// then; pruned segment objects are released.
func TestAuditRetention(t *testing.T) {
	c := newCluster(t)
	c.tweak = func(cfg *Config) { cfg.AuditRetention = 48 * time.Hour }
	r := c.bootstrap()
	c.leader()
	ctx := boundedCtx(t)
	appendAudit := func(data ...string) []uint64 {
		var es [][]byte
		for _, d := range data {
			es = append(es, []byte(d))
		}
		return c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.AppendAudit{Entries: es}}}).AuditSeqs
	}
	seqs := appendAudit("e1", "e2")
	seg1 := controlstore.Object{Kind: controlstore.KindAuditSegment, Format: "audit.v1", Data: []byte("e1 e2")}
	if err := r.store.SealAudit(ctx, seg1, seqs[0], seqs[1]); err != nil {
		t.Fatal(err)
	}
	backup := c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "x", Key: "y"}}})
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.RecordBackup{Backup: controlstore.BackupRecord{ID: "b1", Index: backup.Index}}}})
	seqs = appendAudit("e3")
	seg2 := controlstore.Object{Kind: controlstore.KindAuditSegment, Format: "audit.v1", Data: []byte("e3")}
	if err := r.store.SealAudit(ctx, seg2, seqs[0], seqs[0]); err != nil {
		t.Fatal(err)
	}
	if err := r.store.SealAudit(ctx, seg2, seqs[0], seqs[0]); !errors.Is(err, controlstore.ErrRejected) {
		t.Errorf("sealing a sealed range = %v, want ErrRejected", err)
	}
	if err := c.fake.BlockUntilDue(ctx, testStart.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	c.fake.Advance(48 * time.Hour)
	segments := func() []controlstore.AuditSegment {
		var out []controlstore.AuditSegment
		_ = r.store.View(func(v controlstore.View) { out = v.AuditSegments() })
		return out
	}
	waitFor(t, "the held segment to be pruned", func() bool { return len(segments()) == 1 })
	if got := segments(); got[0].Digest != seg2.Digest() {
		t.Errorf("kept %v, want the segment no backup holds", got[0].Digest)
	}
	if r.store.HasContent(seg1.Digest()) {
		t.Error("the pruned segment's object was not released")
	}
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.RecordBackup{Backup: controlstore.BackupRecord{ID: "b2", Index: r.store.machine.Index()}}}})
	waitFor(t, "the newly held segment to be pruned", func() bool { return len(segments()) == 0 })
	if n, err := r.store.PruneAudit(ctx); err != nil || n != 0 {
		t.Errorf("PruneAudit = %d, %v", n, err)
	}
	notAudit := controlstore.Object{Kind: controlstore.KindPlan, Format: "f", Data: []byte("x")}
	if err := r.store.SealAudit(ctx, notAudit, 1, 1); !errors.Is(err, controlstore.ErrInvalid) {
		t.Errorf("sealing a plan object = %v", err)
	}
}

// TestPruneRevisions deletes unreferenced Revisions beyond the newest 50
// of an Environment, never one an assignment names or keep holds.
func TestPruneRevisions(t *testing.T) {
	c := newCluster(t)
	r := c.bootstrap()
	c.leader()
	ctx := boundedCtx(t)
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.AcceptAnchorSet{AnchorSet: controlstore.AnchorSet{Version: 4, Epoch: 4, Signed: []byte("a")}}}})
	var revs []controlstore.Object
	record := func(i int) {
		o := controlstore.Object{Kind: controlstore.KindRevision, Format: "ruralz.canonical.v1", Data: []byte("revision " + strconv.Itoa(i))}
		c.propose(controlstore.Proposal{Content: []controlstore.Object{o}, Ops: []controlstore.Op{
			controlstore.RecordRevision{Revision: controlstore.RevisionRecord{Environment: "prod", Digest: o.Digest()}},
		}})
		revs = append(revs, o)
	}
	for i := range controlstore.RevisionsKept + 2 {
		record(i)
	}
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Assign{Assignment: controlstore.Assignment{
		Cluster: "prod-eu", Environment: "prod", Digest: revs[0].Digest(), Fence: controlstore.Fence{Epoch: 4, Seq: 1}, Signed: []byte("s"),
	}}}})
	n, err := r.store.PruneRevisions(ctx, nil)
	if err != nil || n != 1 {
		t.Fatalf("PruneRevisions = %d, %v; want 1", n, err)
	}
	if r.store.HasContent(revs[1].Digest()) || !r.store.HasContent(revs[0].Digest()) {
		t.Error("the wrong Revision was deleted")
	}
	record(controlstore.RevisionsKept + 2)
	keep := func(_ string, d controlstore.Digest) bool { return d == revs[2].Digest() }
	if n, err := r.store.PruneRevisions(ctx, keep); err != nil || n != 0 {
		t.Errorf("PruneRevisions with keep = %d, %v; want 0", n, err)
	}
}

// TestHaltOnNewerControlStore: a binary older than the Control Store
// version stops before serving, and OnHalt is told.
func TestHaltOnNewerControlStore(t *testing.T) {
	c := newCluster(t)
	c.binary = "0.3.0"
	r := c.bootstrap()
	c.leader()
	waitFor(t, "the Control Store version", func() bool { return versionOf(r) == (controlstore.Version{Major: 0, Minor: 3}) })
	r.cfg.BinaryVersion = "0.2.0"
	var told *controlstore.HaltError
	r.cfg.OnHalt = func(h *controlstore.HaltError) { told = h }
	err := r.restart()
	var h *controlstore.HaltError
	if !errors.As(err, &h) || h.Reason != controlstore.HaltVersion || told == nil {
		t.Errorf("Open = %v, OnHalt %v; want a version halt", err, told)
	}
}

// TestAwaitLeadershipEnds: a leadership's Lost channel closes when the
// replica stops leading.
func TestAwaitLeadershipEnds(t *testing.T) {
	c := newCluster(t)
	r := c.bootstrap()
	lead, err := r.store.AwaitLeadership(boundedCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if lead.Term == 0 {
		t.Error("term 0")
	}
	r.stop()
	select {
	case <-lead.Lost():
	case <-time.After(waitBound):
		t.Fatal("Lost never closed")
	}
}

// TestRestoreRefusals: a restore refuses anything but a complete backup
// into an empty Control Store with a plan opening a higher epoch, with
// RZ-CP-015, leaving no Control Store behind; a backup missing content
// fails the same way.
func TestRestoreRefusals(t *testing.T) {
	c := newCluster(t)
	r := c.bootstrap()
	c.leader()
	plan := controlstore.Object{Kind: controlstore.KindPlan, Format: "plan.v1", Data: []byte("p")}
	c.propose(controlstore.Proposal{Content: []controlstore.Object{plan}, Ops: []controlstore.Op{
		controlstore.Put{Bucket: "rollouts", Key: "r", Refs: []controlstore.Digest{plan.Digest()}},
	}})
	var buf bytes.Buffer
	if _, err := r.store.Backup(boundedCtx(t), &buf); err != nil {
		t.Fatal(err)
	}
	good := controlstore.RestorePlan{
		ServerID: "new", Address: "127.0.0.1:8092", AnchorSet: controlstore.AnchorSet{Version: 7, Epoch: 7, Signed: []byte("a")},
		Fork: []byte("fork"), At: testStart,
	}
	full := buf.Bytes()
	occupied := t.TempDir()
	if err := os.WriteFile(filepath.Join(occupied, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	badPlan := good
	badPlan.AnchorSet.Epoch = 3
	recount := bytes.Replace(full, []byte(`{"t":"end","objects":1}`), []byte(`{"t":"end","objects":2}`), 1)
	if bytes.Equal(recount, full) {
		t.Fatal("the backup's end frame was not found")
	}
	cases := []struct {
		name   string
		data   []byte
		dir    string
		binary string
		plan   controlstore.RestorePlan
	}{
		{"not empty", full, occupied, "0.2.0", good},
		{"truncated", full[:len(full)/2], "", "0.2.0", good},
		{"end count", recount, "", "0.2.0", good},
		{"older binary", full, "", "0.1.0", good},
		{"plan opens no epoch", full, "", "0.2.0", badPlan},
		{"not a backup", []byte("garbage"), "", "0.2.0", good},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.dir
			if dir == "" {
				dir = filepath.Join(t.TempDir(), "store")
			}
			_, err := Restore(boundedCtx(t), bytes.NewReader(tc.data), RestoreConfig{DataDir: dir, Plan: tc.plan, BinaryVersion: tc.binary})
			if !errors.Is(err, controlstore.ErrBackup) || controlstore.CodeOf(err) != "RZ-CP-015" {
				t.Errorf("Restore = %v, want RZ-CP-015", err)
			}
			if tc.dir == "" {
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("a failed restore left %s behind: %v", dir, err)
				}
			}
		})
	}
	r.store.content.remove(plan.Digest())
	if _, err := r.store.Backup(boundedCtx(t), io.Discard); !errors.Is(err, controlstore.ErrBackup) {
		t.Errorf("a backup missing an object = %v, want RZ-CP-015", err)
	}
}
