// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MaxEntryBytes bounds one encoded Raft entry: entries stay under 1 MiB
// (target), so large data goes to the content store and the entry
// carries its digest (ADR-0006 "Content store").
const MaxEntryBytes = 1 << 20

// Op is one operation of a Command. The state machine applies every op of
// a command in order, or none: an op that fails its precondition rejects
// the whole command.
type Op interface {
	// OpType returns the op's type tag.
	OpType() OpType
}

// OpType is an op's type tag in the encoded entry.
type OpType string

// Op type tags.
const (
	OpPut             OpType = "put"
	OpDelete          OpType = "delete"
	OpAssign          OpType = "assign"
	OpPromote         OpType = "promote"
	OpAnchorSet       OpType = "anchorSet"
	OpRecordRevision  OpType = "recordRevision"
	OpDeleteRevision  OpType = "deleteRevision"
	OpDropAssignments OpType = "dropAssignments"
	OpAppendAudit     OpType = "appendAudit"
	OpSealAudit       OpType = "sealAudit"
	OpPruneAudit      OpType = "pruneAudit"
	OpRecordBackup    OpType = "recordBackup"
	OpAddPending      OpType = "addPending"
	OpRemovePending   OpType = "removePending"
	OpRevokePeer      OpType = "revokePeer"
	OpReport          OpType = "report"
	OpAdvanceVersion  OpType = "advanceVersion"
	OpInitVersion     OpType = "initVersion"
)

// Cond is a record precondition: Absent requires no record at the key;
// a non-zero Index requires the record's ModifyIndex to equal it; neither
// means none.
type Cond struct {
	Absent bool   `json:"absent,omitempty"`
	Index  uint64 `json:"index,omitempty"`
}

// Put writes a generic record: definitions, promotion and Rollout
// records, ACK bitmaps, Enrollment records, token hashes, revocations,
// encrypted keys, users and bindings live in buckets of records. Refs name
// the content objects the record references, such as a Rollout's plan;
// each must already be referenced or travel in the same Proposal, so it
// reached a replica quorum first.
type Put struct {
	Bucket string   `json:"bucket"`
	Key    string   `json:"key"`
	Value  []byte   `json:"value"`
	Refs   []Digest `json:"refs,omitempty"`
	Cond   Cond     `json:"cond"`
}

// Delete removes a generic record; deleting an absent key without a
// precondition succeeds.
type Delete struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	Cond   Cond   `json:"cond"`
}

// Assign accepts a signed assignment at its Cluster's current
// assignmentSeq plus one in the current storeEpoch, for a recorded
// Revision of its Environment.
type Assign struct {
	Assignment Assignment `json:"assignment"`
}

// Promote accepts a signed promotion at its Cluster's current
// promotionSeq plus one in the current storeEpoch, for a recorded
// Revision.
type Promote struct {
	Promotion Promotion `json:"promotion"`
}

// AcceptAnchorSet accepts a root-signed anchor set of a higher version;
// one that opens an epoch resets every Cluster's sequences.
type AcceptAnchorSet struct {
	AnchorSet AnchorSet `json:"anchorSet"`
}

// RecordRevision records a built Revision whose content, source tree and
// diff reached a content-store quorum first.
type RecordRevision struct {
	Revision RevisionRecord `json:"revision"`
}

// DeleteRevision deletes a Revision record, and with it the content only
// it referenced: an unreferenced Revision beyond the latest
// RevisionsKept of its Environment, which no accepted assignment or
// promotion names.
type DeleteRevision struct {
	Environment string `json:"environment"`
	Digest      Digest `json:"digest"`
}

// DropAssignments forgets a Cluster's accepted assignments below a
// sequence, never its latest: the history plans no longer reference.
type DropAssignments struct {
	Cluster string `json:"cluster"`
	Below   uint64 `json:"below"`
}

// AppendAudit commits audit entries, which the state machine numbers in
// order and stamps with the command's time.
type AppendAudit struct {
	Entries [][]byte `json:"entries"`
}

// SealAudit moves the oldest unsealed entries, FirstSeq to LastSeq, into
// the sealed segment object Segment, which chains to the previous head
// Prev and reached a content-store quorum first.
type SealAudit struct {
	Segment  Digest `json:"segment"`
	Size     int    `json:"size"`
	FirstSeq uint64 `json:"firstSeq"`
	LastSeq  uint64 `json:"lastSeq"`
	Prev     Digest `json:"prev"`
}

// PruneAudit prunes sealed segments from the oldest through Through,
// each one only once its newest entry is older than Retention and a
// recorded backup holds it.
type PruneAudit struct {
	Through   Digest        `json:"through"`
	Retention time.Duration `json:"retention"`
}

// RecordBackup records a completed backup, which holds every segment
// sealed at or before its index.
type RecordBackup struct {
	Backup BackupRecord `json:"backup"`
}

// AddPending records a pending member after a redeemed Join.
type AddPending struct {
	Member PendingMember `json:"member"`
}

// RemovePending forgets a pending member.
type RemovePending struct {
	ServerID ServerID `json:"serverId"`
}

// RevokePeer revokes a server ID's peer certificates, so its old volume
// cannot rejoin.
type RevokePeer struct {
	ServerID ServerID `json:"serverId"`
}

// Report records a change in a replica's status report: its binary
// version, or a snapshot reaching the last version-advance entry.
type Report struct {
	ServerID      ServerID `json:"serverId"`
	BinaryVersion string   `json:"binaryVersion"`
	SnapshotIndex uint64   `json:"snapshotIndex"`
}

// AdvanceVersion is the version-advance entry of a finalize.
type AdvanceVersion struct {
	From Version `json:"from"`
	To   Version `json:"to"`
}

// InitVersion sets the Control Store version of a new Control Store: the
// first leader of a bootstrapped store proposes its binary's minor before
// any other entry.
type InitVersion struct {
	Version Version `json:"version"`
}

// OpType returns OpPut.
func (Put) OpType() OpType { return OpPut }

// OpType returns OpDelete.
func (Delete) OpType() OpType { return OpDelete }

// OpType returns OpAssign.
func (Assign) OpType() OpType { return OpAssign }

// OpType returns OpPromote.
func (Promote) OpType() OpType { return OpPromote }

// OpType returns OpAnchorSet.
func (AcceptAnchorSet) OpType() OpType { return OpAnchorSet }

// OpType returns OpRecordRevision.
func (RecordRevision) OpType() OpType { return OpRecordRevision }

// OpType returns OpDeleteRevision.
func (DeleteRevision) OpType() OpType { return OpDeleteRevision }

// OpType returns OpDropAssignments.
func (DropAssignments) OpType() OpType { return OpDropAssignments }

// OpType returns OpAppendAudit.
func (AppendAudit) OpType() OpType { return OpAppendAudit }

// OpType returns OpSealAudit.
func (SealAudit) OpType() OpType { return OpSealAudit }

// OpType returns OpPruneAudit.
func (PruneAudit) OpType() OpType { return OpPruneAudit }

// OpType returns OpRecordBackup.
func (RecordBackup) OpType() OpType { return OpRecordBackup }

// OpType returns OpAddPending.
func (AddPending) OpType() OpType { return OpAddPending }

// OpType returns OpRemovePending.
func (RemovePending) OpType() OpType { return OpRemovePending }

// OpType returns OpRevokePeer.
func (RevokePeer) OpType() OpType { return OpRevokePeer }

// OpType returns OpReport.
func (Report) OpType() OpType { return OpReport }

// OpType returns OpAdvanceVersion.
func (AdvanceVersion) OpType() OpType { return OpAdvanceVersion }

// OpType returns OpInitVersion.
func (InitVersion) OpType() OpType { return OpInitVersion }

// ClientOp reports whether callers may propose op through Store.Propose.
// The store proposes the rest itself, after the checks their documents
// require: a content quorum for a seal, the backup and age rules of a
// prune, the retention rule of a Revision deletion, the finalize
// conditions, membership changes and status reports.
func ClientOp(op Op) bool {
	switch op.OpType() {
	case OpPut, OpDelete, OpAssign, OpPromote, OpAnchorSet, OpRecordRevision, OpDropAssignments, OpAppendAudit, OpRecordBackup, OpAddPending:
		return true
	default:
		return false
	}
}

// RevisionRecord is a recorded Revision: its digest, which is also its
// content object's, the source tree and diff objects (zero when none) and
// the builder's metadata, opaque to the store.
type RevisionRecord struct {
	Environment string    `json:"environment"`
	Digest      Digest    `json:"digest"`
	Source      Digest    `json:"source"`
	Diff        Digest    `json:"diff"`
	Meta        []byte    `json:"meta,omitempty"`
	RecordedAt  time.Time `json:"recordedAt"`
	Index       uint64    `json:"index,omitempty"`
}

// objects returns the record's content digests.
func (r RevisionRecord) objects() []Digest {
	out := []Digest{r.Digest}
	for _, d := range []Digest{r.Source, r.Diff} {
		if !d.IsZero() {
			out = append(out, d)
		}
	}
	return out
}

// RevisionsKept is how many Revisions per Environment the store keeps
// whatever their references: 50 (target); unreferenced ones beyond them
// are deleted (docs/architecture/04-control-plane-and-gitops.md "Building
// and recording a Revision").
const RevisionsKept = 50

// PendingMember is a replica whose Join was redeemed: the leader adds it
// as a non-voter on its first 8092 connection.
type PendingMember struct {
	ServerID ServerID  `json:"serverId"`
	Address  string    `json:"address"`
	At       time.Time `json:"at"`
}

// BackupRecord is a completed backup.
type BackupRecord struct {
	ID    string    `json:"id"`
	Index uint64    `json:"index"`
	At    time.Time `json:"at"`
}

// Command is one Raft entry: ops applied atomically, the proposer's time,
// which every time the ops record takes, and the Control Store version the
// entry was written in.
type Command struct {
	Version Version
	At      time.Time
	Ops     []Op
}

// wireCommand is the encoded form of a Command.
type wireCommand struct {
	Version Version   `json:"v"`
	At      time.Time `json:"at"`
	Ops     []wireOp  `json:"ops"`
}

// wireOp is one encoded op: its type tag and its fields.
type wireOp struct {
	Type OpType          `json:"type"`
	Op   json.RawMessage `json:"op"`
}

// UnknownOpError reports an op type this binary does not know: the
// replica stops before serving and never skips the entry (rule 4 of
// "Ruralz Control upgrades").
type UnknownOpError struct{ Type OpType }

// Error names the op type.
func (e *UnknownOpError) Error() string {
	return fmt.Sprintf("controlstore: unknown entry op %q", e.Type)
}

// EncodeCommand encodes c, refusing an empty command and an entry of
// MaxEntryBytes or more.
func EncodeCommand(c Command) ([]byte, error) {
	if len(c.Ops) == 0 {
		return nil, fmt.Errorf("%w: a command needs at least one op", ErrInvalid)
	}
	w := wireCommand{Version: c.Version, At: c.At.UTC(), Ops: make([]wireOp, 0, len(c.Ops))}
	for _, op := range c.Ops {
		if op == nil {
			return nil, fmt.Errorf("%w: nil op", ErrInvalid)
		}
		b, err := json.Marshal(op)
		if err != nil {
			return nil, fmt.Errorf("controlstore: encode %s op: %w", op.OpType(), err)
		}
		w.Ops = append(w.Ops, wireOp{Type: op.OpType(), Op: b})
	}
	data, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("controlstore: encode command: %w", err)
	}
	if len(data) >= MaxEntryBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrEntryTooLarge, len(data))
	}
	return data, nil
}

// DecodeCommand decodes an entry. An unknown op type is an
// *UnknownOpError; any other malformation is an error wrapping ErrInvalid.
func DecodeCommand(data []byte) (Command, error) {
	var w wireCommand
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Command{}, fmt.Errorf("%w: command: %w", ErrInvalid, err)
	}
	c := Command{Version: w.Version, At: w.At, Ops: make([]Op, 0, len(w.Ops))}
	for _, wo := range w.Ops {
		op, err := decodeOp(wo)
		if err != nil {
			return Command{}, err
		}
		c.Ops = append(c.Ops, op)
	}
	if len(c.Ops) == 0 {
		return Command{}, fmt.Errorf("%w: command without ops", ErrInvalid)
	}
	return c, nil
}

// decodeOp decodes one op by its type tag.
func decodeOp(wo wireOp) (Op, error) {
	var op Op
	var err error
	switch wo.Type {
	case OpPut:
		op, err = decodeAs[Put](wo.Op)
	case OpDelete:
		op, err = decodeAs[Delete](wo.Op)
	case OpAssign:
		op, err = decodeAs[Assign](wo.Op)
	case OpPromote:
		op, err = decodeAs[Promote](wo.Op)
	case OpAnchorSet:
		op, err = decodeAs[AcceptAnchorSet](wo.Op)
	case OpRecordRevision:
		op, err = decodeAs[RecordRevision](wo.Op)
	case OpDeleteRevision:
		op, err = decodeAs[DeleteRevision](wo.Op)
	case OpDropAssignments:
		op, err = decodeAs[DropAssignments](wo.Op)
	case OpAppendAudit:
		op, err = decodeAs[AppendAudit](wo.Op)
	case OpSealAudit:
		op, err = decodeAs[SealAudit](wo.Op)
	case OpPruneAudit:
		op, err = decodeAs[PruneAudit](wo.Op)
	case OpRecordBackup:
		op, err = decodeAs[RecordBackup](wo.Op)
	case OpAddPending:
		op, err = decodeAs[AddPending](wo.Op)
	case OpRemovePending:
		op, err = decodeAs[RemovePending](wo.Op)
	case OpRevokePeer:
		op, err = decodeAs[RevokePeer](wo.Op)
	case OpReport:
		op, err = decodeAs[Report](wo.Op)
	case OpAdvanceVersion:
		op, err = decodeAs[AdvanceVersion](wo.Op)
	case OpInitVersion:
		op, err = decodeAs[InitVersion](wo.Op)
	default:
		return nil, &UnknownOpError{Type: wo.Type}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s op: %w", ErrInvalid, wo.Type, err)
	}
	return op, nil
}

// decodeAs decodes one op value strictly.
func decodeAs[T Op](raw json.RawMessage) (Op, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data")
	}
	return v, nil
}
