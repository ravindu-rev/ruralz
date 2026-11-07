// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// populated returns a fixture holding every kind of state.
func populated(t *testing.T) *fixture {
	t.Helper()
	f := newFixtureOf(t, Version{Major: 0, Minor: 3})
	f.openEpoch(50)
	d := f.record("rev-a")
	f.mustAccept(RecordRevision{Revision: RevisionRecord{Environment: "staging", Digest: digest("rev-s"), Source: digest("src"), Diff: digest("diff"), Meta: []byte(`{"commit":"abc"}`)}})
	f.mustAccept(Assign{Assignment: assignment("prod-eu", "prod", d, 50, 1, "sig")})
	f.mustAccept(Promote{Promotion: Promotion{Cluster: "prod-eu", PromotedDigest: d, Fence: Fence{Epoch: 50, Seq: 1}, Signed: []byte("psig")}})
	f.mustAccept(Put{Bucket: "users", Key: "alice", Value: []byte("a"), Refs: []Digest{digest("plan")}})
	f.mustAccept(Put{Bucket: "sessions", Key: "s1", Value: []byte("cookie")})
	f.mustAccept(Put{Bucket: "tokens", Key: "t1", Value: []byte("hash")})
	f.mustAccept(AppendAudit{Entries: [][]byte{[]byte("e1"), []byte("e2")}})
	f.mustAccept(SealAudit{Segment: digest("seg"), Size: 4, FirstSeq: 1, LastSeq: 1})
	f.mustAccept(RecordBackup{Backup: BackupRecord{ID: "b", Index: f.idx}})
	f.mustAccept(AddPending{Member: PendingMember{ServerID: "r9", Address: "10.0.0.9:8092"}})
	f.mustAccept(Report{ServerID: "r1", BinaryVersion: "0.3.0", SnapshotIndex: 3})
	f.mustAccept(AdvanceVersion{From: v02, To: Version{Major: 0, Minor: 3}})
	f.idx++
	f.m.SetConfiguration(f.idx, []Member{{ServerID: "r1", Address: "10.0.0.1:8092", Voter: true}, {ServerID: "r2", Address: "10.0.0.2:8092", Voter: true}})
	f.mustAccept(RevokePeer{ServerID: "r7"})
	return f
}

// encode writes a snapshot of m.
func encode(t *testing.T, m *Machine) []byte {
	t.Helper()
	var b bytes.Buffer
	if _, err := m.Snapshot().WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestSnapshotRoundTrip: a restored snapshot encodes to the same bytes,
// and its reference counts are rebuilt.
func TestSnapshotRoundTrip(t *testing.T) {
	f := populated(t)
	want := encode(t, f.m)
	if !bytes.Equal(want, encode(t, f.m)) {
		t.Fatal("two snapshots of one state differ")
	}
	snap, err := ReadSnapshot(bytes.NewReader(want), f.m.Binary())
	if err != nil {
		t.Fatal(err)
	}
	m := NewMachine(f.m.Binary())
	m.Restore(snap)
	if got := encode(t, m); !bytes.Equal(got, want) {
		t.Errorf("restored snapshot differs:\n%s\nwant:\n%s", got, want)
	}
	var refs []Digest
	m.View(func(v View) { refs = v.References() })
	if want := []Digest{digest("rev-a"), digest("rev-s"), digest("src"), digest("diff"), digest("plan"), digest("seg")}; !sameDigests(refs, want) {
		t.Errorf("References = %v, want %v", refs, want)
	}
}

// TestSnapshotRejectsDamage: truncation, a missing header, a wrong count
// and data after the end never restore.
func TestSnapshotRejectsDamage(t *testing.T) {
	f := populated(t)
	good := string(encode(t, f.m))
	lines := strings.SplitAfter(good, "\n")
	cases := map[string]string{
		"truncated":     strings.Join(lines[:len(lines)-2], ""),
		"no header":     strings.Join(lines[1:], ""),
		"wrong count":   strings.Replace(good, `{"t":"end","count":`, `{"t":"end","count":1`, 1),
		"after the end": good + lines[1],
		"unknown field": strings.Replace(good, `{"t":"header"`, `{"t":"header","bogus":1`, 1),
		"empty":         "",
		"two headers":   lines[0] + good,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadSnapshot(strings.NewReader(data), f.m.Binary()); err == nil {
				t.Error("ReadSnapshot accepted a damaged snapshot")
			}
		})
	}
}

// TestSnapshotAboveBinaryHalts: a snapshot in a newer Control Store
// version halts.
func TestSnapshotAboveBinaryHalts(t *testing.T) {
	f := populated(t)
	_, err := ReadSnapshot(bytes.NewReader(encode(t, f.m)), v02)
	var h *HaltError
	if !errors.As(err, &h) || h.Reason != HaltVersion {
		t.Errorf("ReadSnapshot = %v, want a version halt", err)
	}
}

// TestRebaseRules is the restore: a higher epoch from the plan's anchor
// set before any delivery, one voter under a new server ID, every old peer
// revoked, sessions and tokens dropped, an audit fork record, sequences
// reset.
func TestRebaseRules(t *testing.T) {
	f := populated(t)
	snap := f.m.Snapshot()
	at := t0.Add(time.Hour)
	plan := RestorePlan{
		ServerID: "restored-1", Address: "10.1.0.1:8092",
		AnchorSet:   AnchorSet{Version: 90, Epoch: 90, Signed: []byte("root-signed")},
		DropBuckets: []string{"sessions", "tokens"},
		Fork:        []byte("fork"), At: at,
	}
	out, err := snap.Rebase(plan)
	if err != nil {
		t.Fatal(err)
	}
	out.View(func(v View) {
		if v.Epoch() != 90 {
			t.Errorf("Epoch = %d, want 90", v.Epoch())
		}
		if got := v.Members(); len(got) != 1 || got[0] != (Member{ServerID: "restored-1", Address: "10.1.0.1:8092", Voter: true}) {
			t.Errorf("Members = %+v", got)
		}
		for _, id := range []ServerID{"r1", "r2", "r9", "r7"} {
			if !v.Revoked(id) {
				t.Errorf("old peer %s is not revoked", id)
			}
		}
		if len(v.Pending()) != 0 || len(v.Reports()) != 0 {
			t.Error("pending members or reports survived")
		}
		if _, ok := v.Record("sessions", "s1"); ok {
			t.Error("a session survived the restore")
		}
		if _, ok := v.Record("tokens", "t1"); ok {
			t.Error("an API token survived the restore")
		}
		if _, ok := v.Record("users", "alice"); !ok {
			t.Error("a user was lost")
		}
		if _, ok := v.LatestAssignment("prod-eu"); ok {
			t.Error("an old-epoch assignment survived")
		}
		e := v.AuditEntries()
		if last := e[len(e)-1]; string(last.Data) != "fork" || !last.At.Equal(at) {
			t.Errorf("last audit entry = %+v, want the fork record", last)
		}
		if v.Index() != snap.Index()+1 {
			t.Errorf("Index = %d, want %d", v.Index(), snap.Index()+1)
		}
	})
	bad := []struct {
		name string
		mod  func(*RestorePlan)
	}{
		{"no fork record", func(p *RestorePlan) { p.Fork = nil }},
		{"no address", func(p *RestorePlan) { p.Address = "" }},
		{"set opens no epoch", func(p *RestorePlan) { p.AnchorSet.Epoch = 50 }},
		{"set not higher", func(p *RestorePlan) { p.AnchorSet = AnchorSet{Version: 50, Epoch: 50, Signed: []byte("x")} }},
		{"old server ID", func(p *RestorePlan) { p.ServerID = "r1" }},
		{"pending server ID", func(p *RestorePlan) { p.ServerID = "r9" }},
		{"revoked server ID", func(p *RestorePlan) { p.ServerID = "r7" }},
		{"bad server ID", func(p *RestorePlan) { p.ServerID = "a b" }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			p := plan
			tc.mod(&p)
			if _, err := snap.Rebase(p); err == nil {
				t.Error("Rebase accepted the plan")
			}
		})
	}
}
