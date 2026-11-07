// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package raft

import (
	"errors"
	"sync"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
)

// The fencing test (ADR-0006 "Confirmation"): a stale replica, whether a
// deposed or partitioned leader, a follower cut off from the leader, an
// old volume or a restored deployment's predecessor
// (backup_integration_test.go), cannot commit and cannot serve an
// assignment as current.

// fencedCluster is three voters at storeEpoch 11 with Revisions A and B of
// prod recorded.
func fencedCluster(t *testing.T) (*cluster, []*replica, controlstore.Digest, controlstore.Digest) {
	t.Helper()
	c, rs := three(t)
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.AcceptAnchorSet{AnchorSet: controlstore.AnchorSet{Version: 11, Epoch: 11, Signed: []byte("root")}}}})
	var ds []controlstore.Digest
	for _, s := range []string{"A", "B"} {
		o := controlstore.Object{Kind: controlstore.KindRevision, Format: "ruralz.canonical.v1", Data: []byte("revision " + s)}
		c.propose(controlstore.Proposal{Content: []controlstore.Object{o}, Ops: []controlstore.Op{
			controlstore.RecordRevision{Revision: controlstore.RevisionRecord{Environment: "prod", Digest: o.Digest()}},
		}})
		ds = append(ds, o.Digest())
	}
	return c, rs, ds[0], ds[1]
}

// signed is an assignment as a leader signs it.
func signed(d controlstore.Digest, f controlstore.Fence, signer string) controlstore.Assignment {
	return controlstore.Assignment{Cluster: "prod-eu", Environment: "prod", Digest: d, RolloutID: "01JROLLOUT", Fence: f, Signed: []byte(signer)}
}

// fenceOf reads a replica's current assignment fence of prod-eu.
func fenceOf(r *replica) controlstore.Fence {
	var f controlstore.Fence
	_ = r.store.View(func(v controlstore.View) { f = v.AssignmentFence("prod-eu") })
	return f
}

// TestFencingDeposedLeader: a deposed leader's entry at a taken sequence
// commits but is never accepted or delivered, also once it leads again,
// and a new leader proposes only after its Barrier.
func TestFencingDeposedLeader(t *testing.T) {
	c, rs, a, b := fencedCluster(t)
	l1 := c.leader()
	first := signed(a, controlstore.Fence{Epoch: 11, Seq: 1}, "leader-1")
	committed := c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Assign{Assignment: first}}})
	// The leader reads its fence and signs the next assignment, then is
	// deposed before it proposes.
	stale := signed(b, fenceOf(l1).Next(), "leader-1")

	held := make(chan *replica, 2)
	release := make(chan struct{})
	var once sync.Once
	for _, r := range others(rs, l1) {
		hook := func() {
			select {
			case <-release:
				return
			default:
			}
			held <- r
			<-release
		}
		r.store.beforeBarrier.Store(&hook)
	}
	if err := l1.store.TransferLeadership(boundedCtx(t), l1.id); err != nil {
		t.Fatalf("depose the leader: %v", err)
	}
	var l2 *replica
	select {
	case l2 = <-held:
	case <-time.After(waitBound):
		t.Fatal("no new leader")
	}
	// Raft leader, but before its Barrier: no proposal passes.
	if l2.store.r.State() != hraft.Leader || l2.store.leadership() != nil {
		t.Fatal("the new leader is not held before its Barrier")
	}
	if _, err := l2.store.Propose(boundedCtx(t), controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "x", Key: "y"}}}); !errors.Is(err, controlstore.ErrNoLeader) {
		t.Errorf("a proposal before the Barrier = %v, want RZ-CP-004", err)
	}
	once.Do(func() { close(release) })
	lead, err := l2.store.AwaitLeadership(boundedCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if lead.BarrierIndex < committed.Index {
		t.Errorf("BarrierIndex %d is below the entry %d committed before the election", lead.BarrierIndex, committed.Index)
	}
	// After its Barrier the new leader reads the current fence and signs
	// at the next sequence.
	if got := fenceOf(l2); got != first.Fence {
		t.Fatalf("the new leader reads fence %+v, want %+v", got, first.Fence)
	}
	fresh := signed(a, fenceOf(l2).Next(), "leader-2")
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Assign{Assignment: fresh}}})
	// The deposed leader's signed entry reaches the log at the taken
	// sequence: it commits and is rejected.
	res, err := l2.store.Propose(boundedCtx(t), controlstore.Proposal{Ops: []controlstore.Op{controlstore.Assign{Assignment: stale}}})
	var rej *controlstore.Rejection
	if !errors.Is(err, controlstore.ErrRejected) || !errors.As(err, &rej) || rej.Reason != controlstore.RejectFence || rej.Fence != fresh.Fence {
		t.Fatalf("the stale entry = %v, %+v; want a fence rejection at %+v", err, res.Rejected, fresh.Fence)
	}
	c.applied(res.Index, rs...)
	for _, r := range rs {
		_ = r.store.View(func(v controlstore.View) {
			if v.Accepted(stale) {
				t.Errorf("replica %s accepted the deposed leader's entry", r.id)
			}
			if !v.Accepted(fresh) {
				t.Errorf("replica %s lacks the new leader's entry", r.id)
			}
		})
		if r.store.MayServe(stale, false) || r.store.MayServe(stale, true) {
			t.Errorf("replica %s may serve the rejected entry", r.id)
		}
	}
	// The deposed leader leads again: its stale entry still never commits
	// as accepted.
	if err := l2.store.wait(boundedCtx(t), l2.store.r.LeadershipTransferToServer(hraft.ServerID(l1.id), hraft.ServerAddress(l1.addr))); err != nil {
		t.Fatalf("transfer back: %v", err)
	}
	if _, err := l1.store.AwaitLeadership(boundedCtx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := l1.store.Propose(boundedCtx(t), controlstore.Proposal{Ops: []controlstore.Op{controlstore.Assign{Assignment: stale}}}); !errors.Is(err, controlstore.ErrRejected) {
		t.Errorf("the stale entry under the old leader's new term = %v, want ErrRejected", err)
	}
}

// staleChecks asserts what a stale replica may still do: send its last
// committed assignment to a Node with no active digest, its last
// promotion, and nothing else; it cannot commit.
func staleChecks(t *testing.T, r *replica, older, latest controlstore.Assignment) {
	t.Helper()
	waitFor(t, "replica "+string(r.id)+" to go stale", func() bool { f, _ := r.store.Freshness(); return f == controlstore.Stale })
	if f, age := r.store.Freshness(); f != controlstore.Stale || age < r.store.electionTimeout() {
		t.Errorf("Freshness = %v, %v", f, age)
	}
	if r.store.LeaderInfo() != 0 {
		t.Error("a stale replica reports leader_info 1")
	}
	if r.store.MayServe(latest, true) {
		t.Error("a stale replica may send a Node with an active digest an assignment")
	}
	if !r.store.MayServe(latest, false) {
		t.Error("a stale replica may not send its last committed assignment to a new Node")
	}
	if r.store.MayServe(older, false) || r.store.MayServe(older, true) {
		t.Error("a stale replica may send an assignment older than its last committed")
	}
	_ = r.store.View(func(v controlstore.View) {
		if _, ok := v.Promotion("prod-eu"); !ok {
			t.Error("a stale replica lost its last promotion")
		}
	})
	if _, err := r.store.Propose(boundedCtx(t), controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "x", Key: "stale"}}}); !errors.Is(err, controlstore.ErrNoLeader) {
		t.Errorf("a stale replica's write = %v, want RZ-CP-004", err)
	}
}

// assigned commits two assignments and a promotion and returns them.
func assigned(t *testing.T, c *cluster, rs []*replica, a, b controlstore.Digest) (controlstore.Assignment, controlstore.Assignment) {
	t.Helper()
	older := signed(a, controlstore.Fence{Epoch: 11, Seq: 1}, "s1")
	latest := signed(b, controlstore.Fence{Epoch: 11, Seq: 2}, "s2")
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Assign{Assignment: older}}})
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Assign{Assignment: latest}}})
	res := c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Promote{Promotion: controlstore.Promotion{
		Cluster: "prod-eu", PromotedDigest: a, Fence: controlstore.Fence{Epoch: 11, Seq: 1}, Signed: []byte("p1"),
	}}}})
	c.applied(res.Index, rs...)
	for _, r := range rs {
		waitFor(t, "content on replica "+string(r.id), func() bool { return r.store.HasContent(a) && r.store.HasContent(b) })
		if !r.store.MayServe(latest, true) || !r.store.MayServe(older, true) {
			t.Fatalf("fresh replica %s may not serve accepted assignments", r.id)
		}
	}
	return older, latest
}

// TestFencingPartitionedLeader: a leader cut off from every voter loses
// its lease, cannot commit, goes stale and serves only new Nodes; it is
// fresh again once the voters return.
func TestFencingPartitionedLeader(t *testing.T) {
	c, rs, a, b := fencedCluster(t)
	older, latest := assigned(t, c, rs, a, b)
	l := c.leader()
	for _, r := range others(rs, l) {
		r.stop()
	}
	staleChecks(t, l, older, latest)
	for _, r := range others(rs, l) {
		if err := r.restart(); err != nil {
			t.Fatal(err)
		}
	}
	c.leader()
	waitFor(t, "the old leader to be fresh again", func() bool { f, _ := l.store.Freshness(); return f == controlstore.Fresh })
	if !l.store.MayServe(latest, true) {
		t.Error("a fresh replica may not serve its accepted assignment")
	}
}

// TestFencingCutOffFollower: a follower that loses the leader and any
// majority goes stale under the same rule, while still campaigning.
func TestFencingCutOffFollower(t *testing.T) {
	c, rs, a, b := fencedCluster(t)
	older, latest := assigned(t, c, rs, a, b)
	l := c.leader()
	fs := others(rs, l)
	l.stop()
	fs[0].stop()
	staleChecks(t, fs[1], older, latest)
}

// TestFencingOldVolume: a removed voter's old volume is revoked on every
// replica: it cannot reach them, receives nothing, cannot commit and is
// stale.
func TestFencingOldVolume(t *testing.T) {
	c, rs, a, b := fencedCluster(t)
	older, latest := assigned(t, c, rs, a, b)
	l := c.leader()
	gone := others(rs, l)[1]
	gone.stop()
	waitFor(t, "the leader to lose contact", func() bool { return !l.store.inContact(gone.id) })
	if err := l.store.RemoveVoter(boundedCtx(t), gone.id, nil); err != nil {
		t.Fatal(err)
	}
	res := c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "x", Key: "after"}}})
	c.applied(res.Index, others(rs, gone)...)
	if err := gone.restart(); err != nil {
		t.Fatal(err)
	}
	for _, r := range others(rs, gone) {
		if tc, _, err := gone.store.trans.dial(boundedCtx(t), r.addr, controlstore.ClassStatus); err == nil {
			// The handshake may finish before the peer refuses; the
			// refusal then closes the connection.
			_ = tc.SetReadDeadline(time.Now().Add(waitBound))
			if _, rerr := tc.Read(make([]byte, 1)); rerr == nil {
				t.Errorf("replica %s talked to the revoked old volume", r.id)
			}
			gone.store.trans.release(tc)
		}
	}
	staleChecks(t, gone, older, latest)
	_ = gone.store.View(func(v controlstore.View) {
		if _, ok := v.Record("x", "after"); ok {
			t.Error("the old volume received an entry after its removal")
		}
	})
}
