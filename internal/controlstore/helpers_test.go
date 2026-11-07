// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"errors"
	"testing"
	"time"
)

// v02 is the binary and Control Store version of most tests.
var v02 = Version{Major: 0, Minor: 2}

// t0 is the fixed start of test time.
var t0 = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

// fixture is a machine with a counter of applied indexes.
type fixture struct {
	t   *testing.T
	m   *Machine
	idx uint64
	at  time.Time
}

// newFixture returns a machine of a 0.2 binary at Control Store version
// 0.2.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureOf(t, v02)
}

// newFixtureOf returns a machine of a binary at Control Store version 0.2.
func newFixtureOf(t *testing.T, binary Version) *fixture {
	t.Helper()
	f := &fixture{t: t, m: NewMachine(binary), at: t0}
	f.mustAccept(InitVersion{Version: v02})
	return f
}

// apply applies one command at the next index.
func (f *fixture) apply(ops ...Op) (Result, error) {
	f.t.Helper()
	f.idx++
	data, err := EncodeCommand(Command{Version: v02, At: f.at, Ops: ops})
	if err != nil {
		f.t.Fatalf("EncodeCommand: %v", err)
	}
	return f.m.Apply(f.idx, data)
}

// mustAccept applies a command that must be accepted.
func (f *fixture) mustAccept(ops ...Op) Result {
	f.t.Helper()
	res, err := f.apply(ops...)
	if err != nil {
		f.t.Fatalf("Apply: %v", err)
	}
	if res.Rejected != nil {
		f.t.Fatalf("Apply rejected: %v", res.Rejected)
	}
	return res
}

// mustReject applies a command that must be rejected for reason.
func (f *fixture) mustReject(reason RejectReason, ops ...Op) {
	f.t.Helper()
	_ = f.rejection(reason, ops...)
}

// rejection applies a command that must be rejected for reason and
// returns the rejection.
func (f *fixture) rejection(reason RejectReason, ops ...Op) *Rejection {
	f.t.Helper()
	res, err := f.apply(ops...)
	if err != nil {
		f.t.Fatalf("Apply: %v", err)
	}
	if res.Rejected == nil {
		f.t.Fatalf("Apply accepted, want a %s rejection", reason)
	}
	if res.Rejected.Reason != reason {
		f.t.Fatalf("rejection = %v, want reason %s", res.Rejected, reason)
	}
	return res.Rejected
}

// mustHalt applies a command that must halt the machine.
func (f *fixture) mustHalt(reason string, data []byte) {
	f.t.Helper()
	f.idx++
	_, err := f.m.Apply(f.idx, data)
	var h *HaltError
	if !errors.As(err, &h) || h.Reason != reason {
		f.t.Fatalf("Apply = %v, want a %s halt", err, reason)
	}
}

// view runs fn on the committed state.
func (f *fixture) view(fn func(View)) { f.m.View(fn) }

// openEpoch accepts an anchor set opening epoch e.
func (f *fixture) openEpoch(e uint64) {
	f.t.Helper()
	f.mustAccept(AcceptAnchorSet{AnchorSet: AnchorSet{Version: e, Epoch: e, Signed: []byte("anchor")}})
}

// digest returns a test digest.
func digest(s string) Digest { return Sum([]byte(s)) }

// record records a Revision of environment prod with content named s.
func (f *fixture) record(s string) Digest {
	f.t.Helper()
	d := digest(s)
	f.mustAccept(RecordRevision{Revision: RevisionRecord{Environment: "prod", Digest: d}})
	return d
}

// assignment returns a signed assignment for cluster at fence.
func assignment(cluster, env string, d Digest, epoch, seq uint64, signer string) Assignment {
	return Assignment{Cluster: cluster, Environment: env, Digest: d, RolloutID: "01JROLLOUT", Fence: Fence{Epoch: epoch, Seq: seq}, Signed: []byte(signer)}
}
