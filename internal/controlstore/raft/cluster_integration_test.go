// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package raft

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	hraft "github.com/hashicorp/raft"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
)

// The container-free Control Store suites run three replicas in one
// process over loopback mutual TLS, each with its own data directory
// (docs/engineering/03-testing-and-quality-strategy.md "Integration
// tests"). Raft runs on the real clock, so every wait is a condition poll
// under waitBound.

// TestJoinReplicateFailOver: two replicas join through pending members,
// become voters once caught up, replicate every entry, and the cluster
// survives the leader's loss with no committed write lost.
func TestJoinReplicateFailOver(t *testing.T) {
	c, rs := three(t)
	res := c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "rollouts", Key: "a", Value: []byte("1")}}})
	c.applied(res.Index, rs...)
	for _, r := range rs {
		var ok bool
		_ = r.store.View(func(v controlstore.View) { _, ok = v.Record("rollouts", "a") })
		if !ok {
			t.Errorf("replica %s lacks the committed record", r.id)
		}
		if f, _ := r.store.Freshness(); f != controlstore.Fresh {
			t.Errorf("replica %s is %v with a live leader", r.id, f)
		}
	}
	old := c.leader()
	old.stop()
	nl := c.leader()
	if nl == old {
		t.Fatal("the stopped replica still leads")
	}
	res = c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "rollouts", Key: "b", Value: []byte("2")}}})
	if err := old.restart(); err != nil {
		t.Fatal(err)
	}
	c.applied(res.Index, rs...)
	_ = old.store.View(func(v controlstore.View) {
		if _, ok := v.Record("rollouts", "b"); !ok {
			t.Error("the old leader did not catch up")
		}
	})
}

// TestContentQuorum: content reaches a quorum of voters before the entry
// referencing it, else RZ-CP-013; a replica that missed it fetches it,
// verified, before it may serve an assignment naming it.
func TestContentQuorum(t *testing.T) {
	c, rs := three(t)
	l := c.leader()
	fs := others(rs, l)
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.AcceptAnchorSet{AnchorSet: controlstore.AnchorSet{Version: 9, Epoch: 9, Signed: []byte("a")}}}})
	fs[1].stop()
	rev := controlstore.Object{Kind: controlstore.KindRevision, Format: "ruralz.canonical.v1", Data: []byte(`{"routes":[]}`)}
	src := controlstore.Object{Kind: controlstore.KindSource, Format: "source.v1", Data: []byte("bundle")}
	c.propose(controlstore.Proposal{Content: []controlstore.Object{rev, src}, Ops: []controlstore.Op{
		controlstore.RecordRevision{Revision: controlstore.RevisionRecord{Environment: "prod", Digest: rev.Digest(), Source: src.Digest()}},
	}})
	a := controlstore.Assignment{Cluster: "prod-eu", Environment: "prod", Digest: rev.Digest(), Fence: controlstore.Fence{Epoch: 9, Seq: 1}, Signed: []byte("s")}
	res := c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Assign{Assignment: a}}})
	if err := fs[1].restart(); err != nil {
		t.Fatal(err)
	}
	c.applied(res.Index, fs[1])
	waitFor(t, "the missed content to be fetched", func() bool {
		return fs[1].store.HasContent(rev.Digest()) && fs[1].store.HasContent(src.Digest())
	})
	waitFor(t, "the replica to serve the assignment", func() bool { return fs[1].store.MayServe(a, true) })
	got, err := fs[1].store.Content(boundedCtx(t), rev.Digest())
	if err != nil || !bytes.Equal(got.Data, rev.Data) {
		t.Errorf("Content = %+v, %v", got, err)
	}
	// Two voters down: no quorum for content.
	l = c.leader()
	for _, r := range others(rs, l) {
		r.stop()
	}
	err = l.store.PutContent(boundedCtx(t), controlstore.Object{Kind: controlstore.KindPlan, Format: "plan.v1", Data: []byte("p")})
	if !errors.Is(err, controlstore.ErrContentQuorum) || controlstore.CodeOf(err) != "RZ-CP-013" {
		t.Errorf("PutContent without a quorum = %v, want RZ-CP-013", err)
	}
}

// TestRemoveVoter: RZ-CP-019 for the leader and for a removal that would
// leave no majority in contact; a removal revokes the replica's peer
// certificates, so its old volume cannot rejoin: it never commits and,
// stale, serves no Node with an active digest.
func TestRemoveVoter(t *testing.T) {
	c, rs := three(t)
	l := c.leader()
	fs := others(rs, l)
	ctx := boundedCtx(t)
	if err := l.store.RemoveVoter(ctx, l.id, nil); !errors.Is(err, controlstore.ErrRemoveRefused) {
		t.Errorf("removing the leader = %v, want RZ-CP-019", err)
	}
	if err := fs[0].store.RemoveVoter(ctx, fs[1].id, nil); !errors.Is(err, controlstore.ErrNoLeader) {
		t.Errorf("a follower's removal = %v, want RZ-CP-004 to forward", err)
	}
	fs[1].stop()
	waitFor(t, "the leader to lose contact with the stopped voter", func() bool { return !l.store.inContact(fs[1].id) })
	if err := l.store.RemoveVoter(ctx, fs[0].id, nil); !errors.Is(err, controlstore.ErrRemoveRefused) {
		t.Errorf("removing the voter in contact while another is down = %v, want RZ-CP-019", err)
	}
	if err := l.store.RemoveVoter(ctx, fs[1].id, []byte("audit: voter removed")); err != nil {
		t.Fatalf("removing the lost voter: %v", err)
	}
	var revoked bool
	var audited bool
	_ = l.store.View(func(v controlstore.View) {
		revoked = v.Revoked(fs[1].id) && !v.IsMember(fs[1].id)
		for _, e := range v.AuditEntries() {
			audited = audited || string(e.Data) == "audit: voter removed"
		}
	})
	if !revoked || !audited {
		t.Fatalf("revoked %v, audited %v", revoked, audited)
	}
	// The old volume comes back.
	if err := fs[1].restart(); err != nil {
		t.Fatal(err)
	}
	res := c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "x", Key: "after", Value: []byte("1")}}})
	c.applied(res.Index, l, fs[0])
	waitFor(t, "the old volume to go stale", func() bool { f, _ := fs[1].store.Freshness(); return f != controlstore.Fresh })
	_ = fs[1].store.View(func(v controlstore.View) {
		if _, ok := v.Record("x", "after"); ok {
			t.Error("the revoked replica received an entry")
		}
	})
	if _, err := fs[1].store.Propose(ctx, controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "x", Key: "y"}}}); !errors.Is(err, controlstore.ErrNoLeader) {
		t.Errorf("the old volume's write = %v, want RZ-CP-004", err)
	}
	if err := l.store.RemoveVoter(ctx, "nobody", nil); !errors.Is(err, controlstore.ErrNotFound) {
		t.Errorf("removing an unknown server = %v", err)
	}
}

// TestLeadershipTransfer: a transfer from the leader moves leadership to
// a caught-up voter, repeating an attempt raft gave up; one naming a
// follower is a no-op; with no other voter, or none taking over within
// TransferWait, it is RZ-CP-004.
func TestLeadershipTransfer(t *testing.T) {
	c := newCluster(t)
	solo := c.bootstrap()
	c.leader()
	if err := solo.store.TransferLeadership(boundedCtx(t), "r1"); !errors.Is(err, controlstore.ErrNoLeader) {
		t.Errorf("a transfer with no other voter = %v, want RZ-CP-004", err)
	}
	c.join("r2")
	c.join("r3")
	l := c.leader()
	if err := l.store.TransferLeadership(boundedCtx(t), "not-the-leader"); err != nil {
		t.Errorf("a transfer from a follower = %v, want a no-op", err)
	}

	// No attempt succeeds: the transfer gives up once TransferWait passes
	// on the store's clock, and the replica still leads.
	never := func() error { return errors.New("injected: the voter is behind") }
	l.store.beforeTransfer.Store(&never)
	done := make(chan error, 1)
	go func() { done <- l.store.TransferLeadership(boundedCtx(t), l.id) }()
	var err error
	waitFor(t, "the transfer to give up", func() bool {
		c.fake.Advance(controlstore.TransferWait)
		select {
		case err = <-done:
			return true
		default:
			return false
		}
	})
	if !errors.Is(err, controlstore.ErrNoLeader) {
		t.Errorf("a transfer no voter took over = %v, want RZ-CP-004", err)
	}
	if c.leader() != l {
		t.Fatal("leadership moved although every attempt failed")
	}

	// The first attempt fails, as raft's does when the voter needs longer
	// than one election timeout to catch up; the next one hands over.
	var attempts atomic.Int32
	once := func() error {
		if attempts.Add(1) == 1 {
			return errors.New("injected: leadership transfer timeout")
		}
		return nil
	}
	l.store.beforeTransfer.Store(&once)
	if err := l.store.TransferLeadership(boundedCtx(t), l.id); err != nil {
		t.Fatalf("TransferLeadership after a failed attempt: %v", err)
	}
	if n := attempts.Load(); n < 2 {
		t.Errorf("attempts = %d, want the failed one repeated", n)
	}
	waitFor(t, "leadership to move", func() bool { return c.leader() != l })
}

// raftVoters returns the voters of a replica's raft configuration.
func raftVoters(t *testing.T, r *replica) int {
	t.Helper()
	f := r.store.r.GetConfiguration()
	if err := f.Error(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range f.Configuration().Servers {
		if s.Suffrage == hraft.Voter {
			n++
		}
	}
	return n
}

// TestRestartKeepsMembership is the regression test of replicas that
// came back with an empty raft configuration ("no known peers") after a
// restart: raft's own committed-entry replay skipped the configuration
// entries it replayed when no snapshot held them. Every replica of a
// stopped cluster restarts knowing its peers, elects a leader and
// commits.
func TestRestartKeepsMembership(t *testing.T) {
	c, rs := three(t)
	for _, r := range rs {
		r.stop()
	}
	for _, r := range rs {
		if err := r.restart(); err != nil {
			t.Fatal(err)
		}
		if got := raftVoters(t, r); got != 3 {
			t.Errorf("replica %s restarted with %d voters, want 3", r.id, got)
		}
	}
	res := c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "x", Key: "after-restart"}}})
	c.applied(res.Index, rs...)
}

// TestForwardingClasses: a follower's SendStatus reaches the leader's
// StatusHandler with the follower's identity, the leader handles its own
// at once, a handler's RZ code comes back, and DialLeader reaches the
// leader's ForwardHandler.
func TestForwardingClasses(t *testing.T) {
	c := newCluster(t)
	var mu sync.Mutex
	seen := map[controlstore.ServerID][]controlstore.ServerID{}
	c.tweak = func(cfg *Config) {
		self := cfg.ServerID
		cfg.StatusHandler = func(_ context.Context, from controlstore.PeerIdentity, kind string, _ json.RawMessage) (json.RawMessage, error) {
			mu.Lock()
			seen[self] = append(seen[self], from.ServerID)
			mu.Unlock()
			if kind == "refuse" {
				return nil, controlstore.Errorf(controlstore.ErrFinalize, "refused")
			}
			return json.RawMessage(`"handled by ` + string(self) + `"`), nil
		}
		cfg.ForwardHandler = func(_ context.Context, pc *PeerConn) {
			line, err := bufio.NewReader(pc).ReadString('\n')
			if err == nil {
				_, _ = io.WriteString(pc, string(self)+" got "+line)
			}
		}
	}
	c.bootstrap()
	c.leader()
	c.join("r2")
	c.join("r3")
	l := c.leader()
	var f *replica
	for _, r := range c.replicas {
		if r != l {
			f = r
		}
	}
	ctx := boundedCtx(t)
	out, err := f.store.SendStatus(ctx, "ack", json.RawMessage(`{"nonce":1}`))
	if err != nil || string(out) != `"handled by `+string(l.id)+`"` {
		t.Fatalf("SendStatus from a follower = %s, %v", out, err)
	}
	mu.Lock()
	if got := seen[l.id]; len(got) != 1 || got[0] != f.id {
		t.Errorf("the leader saw %v, want the follower %s", got, f.id)
	}
	mu.Unlock()
	if out, err := l.store.SendStatus(ctx, "ack", nil); err != nil || string(out) != `"handled by `+string(l.id)+`"` {
		t.Errorf("SendStatus on the leader = %s, %v", out, err)
	}
	var se *StatusError
	if _, err := f.store.SendStatus(ctx, "refuse", nil); !errors.As(err, &se) || se.Code != controlstore.CodeFinalize {
		t.Errorf("a refused status = %v, want RZ-CP-021", err)
	}
	if _, err := f.store.SendStatus(ctx, statusReplica, nil); !errors.Is(err, controlstore.ErrInvalid) {
		t.Errorf("the store's own kind = %v, want ErrInvalid", err)
	}
	conn, err := f.store.DialLeader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "write\n"); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || reply != string(l.id)+" got write\n" {
		t.Errorf("forwarded reply = %q, %v", reply, err)
	}
	if _, err := l.store.DialLeader(ctx); !errors.Is(err, controlstore.ErrInvalid) {
		t.Errorf("DialLeader on the leader = %v, want ErrInvalid", err)
	}
}

// TestShutdownTransfersLeadership: a stopping leader hands leadership to
// a caught-up voter before it stops, repeating an attempt that failed.
func TestShutdownTransfersLeadership(t *testing.T) {
	c, _ := three(t)
	l := c.leader()
	got := make(chan error, 1)
	hook := func(err error) { got <- err }
	l.store.afterShutdownTransfer.Store(&hook)
	var attempts atomic.Int32
	once := func() error {
		if attempts.Add(1) == 1 {
			return errors.New("injected: leadership transfer timeout")
		}
		return nil
	}
	l.store.beforeTransfer.Store(&once)
	l.stop()
	if n := attempts.Load(); n < 2 {
		t.Errorf("attempts = %d, want the failed one repeated", n)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Errorf("the shutdown transfer failed: %v", err)
		}
	default:
		t.Fatal("a stopping leader made no leadership transfer")
	}
	if c.leader() == l {
		t.Error("the stopped replica still leads")
	}
}
