// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package nodedir

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ravindu-rev/ruralz/internal/ulid"
)

// HolderFormat is the format marker of holder.json (R-25).
const HolderFormat = "ruralz.holder.v1"

// MaxHolderBytes bounds a holder.json read.
const MaxHolderBytes = 64 << 10

// Holder errors.
var (
	// ErrNoHolder reports a missing holder.json; the error also matches
	// fs.ErrNotExist. ruralz node drain exits 2 with "no lock holder
	// recorded in <dir>" (spec 10 requirement 96.1).
	ErrNoHolder = errors.New("nodedir: no lock holder recorded")
	// ErrHolderInvalid reports a holder.json that does not parse, has
	// another format or lacks a PID or node.id; ruralz node drain exits 2
	// with "unreadable holder record" (spec 10 requirement 96.1).
	ErrHolderInvalid = errors.New("nodedir: unreadable holder record")
)

// Holder is the PID record ruralz.holder.v1 in holder.json (spec 04
// requirement 5, spec 10 requirement 95, R-25). Members are written in
// this order; readers ignore members they do not know, so later members
// are additive.
type Holder struct {
	// Format is HolderFormat.
	Format string `json:"format"`
	// PID is the holder's process ID in its PID namespace.
	PID int `json:"pid"`
	// StartTime is field 22 of /proc/<pid>/stat (clock ticks since boot);
	// 0 where /proc does not exist.
	StartTime uint64 `json:"startTime"`
	// NodeID is the Node's node.id.
	NodeID ulid.ULID `json:"nodeId"`
	// Version is the holder's build version.
	Version string `json:"version"`
	// PIDNamespace is readlink(/proc/self/ns/pid), such as
	// "pid:[4026531836]"; empty (and omitted) where /proc does not exist
	// (OQ-cli-and-api-surface-10 (a)).
	PIDNamespace string `json:"pidNamespace,omitempty"`
}

// NewHolder returns the record of the calling process: its PID, start
// time and PID namespace (Linux), nodeID and version. When a /proc read
// fails it still returns the record, with the unreadable members zero,
// and an error the caller logs; writing that record makes ruralz node
// drain refuse to signal (a stale record), never signal the wrong process.
func NewHolder(nodeID ulid.ULID, version string) (Holder, error) {
	h := Holder{Format: HolderFormat, PID: os.Getpid(), NodeID: nodeID, Version: version}
	start, errStart := processStartTime()
	ns, errNS := processPIDNamespace()
	h.StartTime, h.PIDNamespace = start, ns
	if err := errors.Join(errStart, errNS); err != nil {
		return h, fmt.Errorf("nodedir: reading the holder's process identity: %w", err)
	}
	return h, nil
}

// Validate checks the members a reader relies on: the format, a positive
// PID (a PID of 0 or below would address a process group) and a node.id.
func (h Holder) Validate() error {
	switch {
	case h.Format != HolderFormat:
		return fmt.Errorf("%w: format %q, want %q", ErrHolderInvalid, h.Format, HolderFormat)
	case h.PID <= 0:
		return fmt.Errorf("%w: pid %d", ErrHolderInvalid, h.PID)
	case h.NodeID.IsZero():
		return fmt.Errorf("%w: no nodeId", ErrHolderInvalid)
	}
	return nil
}

// Encode returns the holder.json bytes: one compact JSON object and "\n".
func (h Holder) Encode() ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("nodedir: %w", err)
	}
	return append(data, '\n'), nil
}

// DecodeHolder parses holder.json bytes and validates the record.
func DecodeHolder(data []byte) (Holder, error) {
	var h Holder
	if err := json.Unmarshal(data, &h); err != nil {
		return Holder{}, fmt.Errorf("%w: %w", ErrHolderInvalid, err)
	}
	if err := h.Validate(); err != nil {
		return Holder{}, err
	}
	return h, nil
}

// ReadHolderAt reads holder.json under root without opening, creating or
// locking anything else, for ruralz node drain (spec 10 requirements 95
// and 96). A missing record matches ErrNoHolder and fs.ErrNotExist; an
// unparsable one matches ErrHolderInvalid, and so does, without blocking
// or reading through it, a holder.json that is a symbolic link, a hard
// link (more than one link), a FIFO or anything else but a regular file
// (on Unix).
func ReadHolderAt(root string) (Holder, error) {
	path := filepath.Join(root, HolderFile)
	data, err := readLimited(path, MaxHolderBytes, true)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Holder{}, fmt.Errorf("%w in %s: %w", ErrNoHolder, root, err)
	case err != nil:
		return Holder{}, fmt.Errorf("%w: %w", ErrHolderInvalid, err)
	}
	h, err := DecodeHolder(data)
	if err != nil {
		return Holder{}, fmt.Errorf("%s: %w", path, err)
	}
	return h, nil
}

// ReadHolder reads this directory's holder.json with ReadHolderAt.
func (d *Dir) ReadHolder() (Holder, error) { return ReadHolderAt(d.root) }
