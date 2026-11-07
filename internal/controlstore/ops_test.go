// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// everyOp returns one op of each type.
func everyOp() []Op {
	d := digest("x")
	return []Op{
		Put{Bucket: "b", Key: "k", Value: []byte("v"), Refs: []Digest{d}, Cond: Cond{Index: 3}},
		Delete{Bucket: "b", Key: "k", Cond: Cond{Index: 4}},
		Assign{Assignment: assignment("c", "e", d, 1, 2, "s")},
		Promote{Promotion: Promotion{Cluster: "c", PromotedDigest: d, Fence: Fence{Epoch: 1, Seq: 1}, Signed: []byte("p")}},
		AcceptAnchorSet{AnchorSet: AnchorSet{Version: 2, Epoch: 2, Signed: []byte("a")}},
		RecordRevision{Revision: RevisionRecord{Environment: "e", Digest: d, Source: digest("s"), Meta: []byte("m")}},
		DeleteRevision{Environment: "e", Digest: d},
		DropAssignments{Cluster: "c", Below: 2},
		AppendAudit{Entries: [][]byte{[]byte("a1")}},
		SealAudit{Segment: d, Size: 9, FirstSeq: 1, LastSeq: 2, Prev: digest("p")},
		PruneAudit{Through: d, Retention: time.Hour},
		RecordBackup{Backup: BackupRecord{ID: "b", Index: 9}},
		AddPending{Member: PendingMember{ServerID: "r2", Address: "h:8092"}},
		RemovePending{ServerID: "r2"},
		RevokePeer{ServerID: "r3"},
		Report{ServerID: "r1", BinaryVersion: "0.2.0", SnapshotIndex: 4},
		AdvanceVersion{From: v02, To: Version{Major: 0, Minor: 3}},
		InitVersion{Version: v02},
	}
}

// TestCommandRoundTrip encodes and decodes every op type.
func TestCommandRoundTrip(t *testing.T) {
	c := Command{Version: v02, At: t0, Ops: everyOp()}
	data, err := EncodeCommand(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeCommand(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != c.Version || !got.At.Equal(c.At) || len(got.Ops) != len(c.Ops) {
		t.Fatalf("decoded %+v", got)
	}
	for i := range c.Ops {
		if !reflect.DeepEqual(got.Ops[i], c.Ops[i]) {
			t.Errorf("op %d = %#v, want %#v", i, got.Ops[i], c.Ops[i])
		}
	}
}

// TestCommandLimits: empty commands and entries of 1 MiB or more are
// refused before they are proposed.
func TestCommandLimits(t *testing.T) {
	if _, err := EncodeCommand(Command{Version: v02, At: t0}); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty command: %v", err)
	}
	big := Put{Bucket: "b", Key: "k", Value: bytes.Repeat([]byte{'x'}, MaxEntryBytes)}
	if _, err := EncodeCommand(Command{Version: v02, At: t0, Ops: []Op{big}}); !errors.Is(err, ErrEntryTooLarge) {
		t.Errorf("1 MiB entry: %v", err)
	}
	fits := Put{Bucket: "b", Key: "k", Value: bytes.Repeat([]byte{'x'}, 700<<10)}
	if _, err := EncodeCommand(Command{Version: v02, At: t0, Ops: []Op{fits}}); err != nil {
		t.Errorf("700 KiB value: %v", err)
	}
	if _, err := EncodeCommand(Command{Version: v02, At: t0, Ops: []Op{nil}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("nil op: %v", err)
	}
}

// TestDecodeRefusals: an unknown op type is an UnknownOpError; an unknown
// field, trailing data or no ops is invalid.
func TestDecodeRefusals(t *testing.T) {
	var u *UnknownOpError
	if _, err := DecodeCommand([]byte(`{"v":{"major":0,"minor":2},"at":"2026-11-07T10:00:00Z","ops":[{"type":"later","op":{}}]}`)); !errors.As(err, &u) || u.Type != "later" {
		t.Errorf("unknown op: %v", err)
	}
	for name, data := range map[string]string{
		"unknown field": `{"v":{"major":0,"minor":2},"at":"2026-11-07T10:00:00Z","ops":[],"x":1}`,
		"no ops":        `{"v":{"major":0,"minor":2},"at":"2026-11-07T10:00:00Z","ops":[]}`,
		"bad op":        `{"v":{"major":0,"minor":2},"at":"2026-11-07T10:00:00Z","ops":[{"type":"put","op":{"bucket":1}}]}`,
		"not JSON":      `x`,
	} {
		if _, err := DecodeCommand([]byte(data)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

// TestClientOps: callers propose only the client ops; the store's own
// checks guard the rest.
func TestClientOps(t *testing.T) {
	client := map[OpType]bool{
		OpPut: true, OpDelete: true, OpAssign: true, OpPromote: true, OpAnchorSet: true, OpRecordRevision: true,
		OpDropAssignments: true, OpAppendAudit: true, OpRecordBackup: true, OpAddPending: true,
	}
	for _, op := range everyOp() {
		if got := ClientOp(op); got != client[op.OpType()] {
			t.Errorf("ClientOp(%s) = %v, want %v", op.OpType(), got, client[op.OpType()])
		}
	}
}

// TestUnknownOpErrorNamesType checks the message.
func TestUnknownOpErrorNamesType(t *testing.T) {
	if e := (&UnknownOpError{Type: "x"}); !strings.Contains(e.Error(), `"x"`) {
		t.Errorf("Error() = %q", e.Error())
	}
}
