// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
)

// TestSingleVoterLifecycle: a bootstrapped store elects itself, sets the
// Control Store version after its Barrier, commits writes with their
// content, and comes back from a restart with its state.
func TestSingleVoterLifecycle(t *testing.T) {
	c := newCluster(t)
	r := c.bootstrap()
	l := c.leader()
	if l != r {
		t.Fatalf("leader = %s", l.id)
	}
	waitFor(t, "the Control Store version", func() bool {
		var set bool
		_ = r.store.View(func(v controlstore.View) { _, set = v.Version() })
		return set
	})
	select {
	case <-r.store.Ready():
	case <-time.After(waitBound):
		t.Fatal("Ready never closed")
	}
	plan := controlstore.Object{Kind: controlstore.KindPlan, Format: "plan.v1", Data: []byte("nodes")}
	res := c.propose(controlstore.Proposal{Content: []controlstore.Object{plan}, Ops: []controlstore.Op{
		controlstore.Put{Bucket: "rollouts", Key: "r1", Value: []byte("pending"), Refs: []controlstore.Digest{plan.Digest()}},
	}})
	if res.Index == 0 {
		t.Fatal("no index")
	}
	got, err := r.store.Content(boundedCtx(t), plan.Digest())
	if err != nil || !bytes.Equal(got.Data, plan.Data) {
		t.Fatalf("Content = %+v, %v", got, err)
	}
	// A later write persists the commit index covering the first.
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "rollouts", Key: "r2", Value: []byte("x")}}})
	if err := r.restart(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.store.Ready():
	case <-time.After(waitBound):
		t.Fatal("Ready never closed after the restart")
	}
	var rec controlstore.Record
	var ok bool
	_ = r.store.View(func(v controlstore.View) { rec, ok = v.Record("rollouts", "r1") })
	if !ok || string(rec.Value) != "pending" {
		t.Errorf("after the restart the record is %+v, %v", rec, ok)
	}
	if !r.store.HasContent(plan.Digest()) {
		t.Error("the plan object was lost")
	}
}

// TestProposeRules: only client ops pass Propose; a follower or a
// replica before its Barrier refuses with RZ-CP-004; references must
// travel with the proposal or be referenced; a rejected entry returns
// ErrRejected with the current fence.
func TestProposeRules(t *testing.T) {
	c := newCluster(t)
	r := c.bootstrap()
	c.leader()
	ctx := boundedCtx(t)
	if _, err := r.store.Propose(ctx, controlstore.Proposal{Ops: []controlstore.Op{controlstore.AdvanceVersion{}}}); !errors.Is(err, controlstore.ErrInvalid) {
		t.Errorf("an internal op = %v, want ErrInvalid", err)
	}
	if _, err := r.store.Propose(ctx, controlstore.Proposal{Ops: []controlstore.Op{
		controlstore.Put{Bucket: "x", Key: "y", Refs: []controlstore.Digest{controlstore.Sum([]byte("nowhere"))}},
	}}); !errors.Is(err, controlstore.ErrInvalid) {
		t.Errorf("an unreferenced ref = %v, want ErrInvalid", err)
	}
	wrongKind := controlstore.Object{Kind: controlstore.KindDiff, Format: "f", Data: []byte("rev")}
	if _, err := r.store.Propose(ctx, controlstore.Proposal{Content: []controlstore.Object{wrongKind}, Ops: []controlstore.Op{
		controlstore.RecordRevision{Revision: controlstore.RevisionRecord{Environment: "prod", Digest: wrongKind.Digest()}},
	}}); !errors.Is(err, controlstore.ErrInvalid) {
		t.Errorf("a Revision whose content is a diff = %v, want ErrInvalid", err)
	}
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.AcceptAnchorSet{AnchorSet: controlstore.AnchorSet{Version: 3, Epoch: 3, Signed: []byte("a")}}}})
	rev := controlstore.Object{Kind: controlstore.KindRevision, Format: "ruralz.canonical.v1", Data: []byte("{}")}
	c.propose(controlstore.Proposal{Content: []controlstore.Object{rev}, Ops: []controlstore.Op{
		controlstore.RecordRevision{Revision: controlstore.RevisionRecord{Environment: "prod", Digest: rev.Digest()}},
	}})
	a := controlstore.Assignment{Cluster: "prod-eu", Environment: "prod", Digest: rev.Digest(), Fence: controlstore.Fence{Epoch: 3, Seq: 2}, Signed: []byte("s")}
	res, err := r.store.Propose(ctx, controlstore.Proposal{Ops: []controlstore.Op{controlstore.Assign{Assignment: a}}})
	var rej *controlstore.Rejection
	if !errors.Is(err, controlstore.ErrRejected) || !errors.As(err, &rej) || rej.Fence != (controlstore.Fence{Epoch: 3}) || res.Rejected == nil {
		t.Errorf("a gap = %v, %+v", err, res)
	}
	if r.store.MayServe(a, false) {
		t.Error("a rejected assignment may be served")
	}
	a.Fence.Seq = 1
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.Assign{Assignment: a}}})
	if !r.store.MayServe(a, true) {
		t.Error("the leader may not serve its accepted assignment")
	}
	f, age := r.store.Freshness()
	if f != controlstore.Fresh || age != 0 || r.store.LeaderInfo() != 1 || r.store.LeaderContactAgeSeconds() != 0 {
		t.Errorf("leader freshness %v %v", f, age)
	}
	big := controlstore.Put{Bucket: "x", Key: "y", Value: bytes.Repeat([]byte{1}, controlstore.MaxEntryBytes)}
	if _, err := r.store.Propose(ctx, controlstore.Proposal{Ops: []controlstore.Op{big}}); !errors.Is(err, controlstore.ErrEntryTooLarge) {
		t.Errorf("a 1 MiB entry = %v", err)
	}
}

// TestShutdownClosesStore: a single voter stops without attempting a
// leadership transfer, and calls after Shutdown fail with ErrClosed.
func TestShutdownClosesStore(t *testing.T) {
	c := newCluster(t)
	r := c.bootstrap()
	c.leader()
	s := r.store
	var attempts atomic.Int32
	count := func() error {
		attempts.Add(1)
		return errors.New("injected: a single voter has no one to hand over to")
	}
	s.beforeTransfer.Store(&count)
	transferred := make(chan error, 1)
	hook := func(err error) { transferred <- err }
	s.afterShutdownTransfer.Store(&hook)
	r.stop()
	if n := attempts.Load(); n != 0 {
		t.Errorf("a single voter made %d leadership transfer attempts at shutdown", n)
	}
	select {
	case err := <-transferred:
		t.Errorf("a single voter transferred leadership at shutdown: %v", err)
	default:
	}
	if err := s.View(func(controlstore.View) {}); !errors.Is(err, controlstore.ErrClosed) {
		t.Errorf("View after Shutdown = %v", err)
	}
	if _, err := s.Propose(boundedCtx(t), controlstore.Proposal{Ops: []controlstore.Op{controlstore.Put{Bucket: "a", Key: "b"}}}); !errors.Is(err, controlstore.ErrClosed) {
		t.Errorf("Propose after Shutdown = %v", err)
	}
	if err := s.Shutdown(boundedCtx(t)); err != nil {
		t.Errorf("a second Shutdown = %v", err)
	}
}

// TestOpenValidates refuses incomplete configurations.
func TestOpenValidates(t *testing.T) {
	c := newCluster(t)
	good := func() Config {
		return Config{
			DataDir: t.TempDir(), ServerID: "r1", Address: "127.0.0.1:8092", Listener: listen(t, ""),
			TLS: c.pki.source(t, "r1"), BinaryVersion: "0.2.0",
		}
	}
	cases := map[string]func(*Config){
		"no data directory": func(cfg *Config) { cfg.DataDir = "" },
		"no address":        func(cfg *Config) { cfg.Address = "" },
		"bad address":       func(cfg *Config) { cfg.Address = "nohost" },
		"bad server ID":     func(cfg *Config) { cfg.ServerID = "a b" },
		"no TLS":            func(cfg *Config) { cfg.TLS = nil },
		"bad version":       func(cfg *Config) { cfg.BinaryVersion = "dev" },
		"timed soak alone":  func(cfg *Config) { cfg.TimedSoak = time.Hour },
	}
	for name, mod := range cases {
		cfg := good()
		mod(&cfg)
		if s, err := Open(cfg); err == nil {
			_ = s.Shutdown(boundedCtx(t))
			t.Errorf("%s: Open succeeded", name)
		}
	}
}
