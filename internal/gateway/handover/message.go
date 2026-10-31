// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handover

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// Format is the identifier of the handover protocol, the handover marker
// of a ready message (docs/engineering/04-release-versioning-and-compatibility.md
// "Versioned surfaces").
const Format = "ruralz.handover.v1"

// LKGFormat is the format of the Last-Known-Good and candidate pointers
// this release reads and writes, the lkg marker of a ready message
// (docs/architecture/03-data-plane.md "Last-Known-Good files").
const LKGFormat = "ruralz.lkg.v1"

// MaxMessage bounds one message: a JSON object of at most 64 KiB, then a
// newline.
const MaxMessage = 64 << 10

// Message types, the type member of each message.
const (
	// TypeReady is the new process's report: verified, compiled, warmed
	// and bound, not listening.
	TypeReady = "ready"
	// TypeRefused ends the handover: the new process exits 1 holding no
	// listening socket.
	TypeRefused = "refused"
	// TypeReload asks the new process to reload the holder's newer
	// candidate, then send ready again.
	TypeReload = "reload"
	// TypeAccepted accepts the handover with the holder's usage.
	TypeAccepted = "accepted"
	// TypeUsage reports the holder's usage every UsageInterval after
	// accepted.
	TypeUsage = "usage"
	// TypeListening reports that the new process's sockets joined their
	// groups; the holder steers to them.
	TypeListening = "listening"
	// TypeReleased reports that the holder released the lock at its
	// Drain's start.
	TypeReleased = "released"
)

// Refusal reasons, the reason member of a refused message.
const (
	// ReasonUnknownMarker refuses a ready message the holder cannot read:
	// an unknown type, a missing marker or an identifier it does not read.
	ReasonUnknownMarker = "unknown_marker"
	// ReasonOlderCandidate refuses a new process whose active Revision is
	// not the holder's while the holder's candidate is older than its
	// active Revision (a failed candidate write) or missing.
	ReasonOlderCandidate = "older_candidate"
	// ReasonDraining refuses a new process once the holder's Drain began.
	ReasonDraining = "draining"
	// ReasonBusy refuses a new process while another one's handover is in
	// progress.
	ReasonBusy = "busy"
)

// Marker keys of a ready message's markers member.
const (
	MarkerHandover  = "handover"
	MarkerLKG       = "lkg"
	MarkerHolder    = "holder"
	MarkerCanonical = "canonical"
)

// Markers returns the format identifier this release reads and writes for
// each marker key; a ready message carries them.
func Markers() map[string]string {
	return map[string]string{
		MarkerHandover:  Format,
		MarkerLKG:       LKGFormat,
		MarkerHolder:    nodedir.HolderFormat,
		MarkerCanonical: canonical.Format,
	}
}

// Message errors.
var (
	// ErrProtocol reports a message that breaks ruralz.handover.v1: not one
	// JSON object of at most MaxMessage bytes on a line, a type the reader
	// does not expect, or a member it needs missing or malformed.
	ErrProtocol = errors.New("handover: protocol error")
	// ErrUnknownMarker reports a ready message whose markers lack a format
	// or name an identifier this release does not read.
	ErrUnknownMarker = errors.New("handover: unknown format marker")
)

// Usage is the holder's use of the Node-wide ceilings, which accepted and
// usage messages carry: the new process admits each ceiling minus it.
type Usage struct {
	// Connections are the client connections open.
	Connections int64 `json:"connections"`
	// InflightUnits are the in-flight units held.
	InflightUnits int64 `json:"inflightUnits"`
	// BufferedBytes are the bytes reserved from limits.maxBufferedBytes.
	BufferedBytes int64 `json:"bufferedBytes"`
	// PluginMemoryBytes is the Plugin memory held, Planned (M2): 0 until
	// Plugins run.
	PluginMemoryBytes int64 `json:"pluginMemoryBytes"`
	// RSSBytes is the holder's resident set size.
	RSSBytes int64 `json:"rssBytes"`
}

// ready is a parsed ready message.
type ready struct {
	Version string
	PID     int
	Markers map[string]string
	// ActiveDigest is the new process's active Revision; zero for none
	// (null on the wire).
	ActiveDigest revision.Digest
}

// readyWire is the ready message as written.
type readyWire struct {
	Type         string            `json:"type"`
	Version      string            `json:"version"`
	PID          int               `json:"pid"`
	Markers      map[string]string `json:"markers"`
	ActiveDigest *revision.Digest  `json:"activeDigest"`
}

// refusedWire is the refused message.
type refusedWire struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// reloadWire is the reload message.
type reloadWire struct {
	Type   string          `json:"type"`
	Digest revision.Digest `json:"digest"`
}

// acceptedWire is the accepted message: the holder's usage at acceptance.
// A member the accepted message gains goes here.
type acceptedWire struct {
	Type string `json:"type"`
	Usage
}

// usageWire is the usage message.
type usageWire struct {
	Type string `json:"type"`
	Usage
}

// bareWire is a message of its type alone: listening and released.
type bareWire struct {
	Type string `json:"type"`
}

// readyMessage returns the ready message of a new process.
func readyMessage(r ready) readyWire {
	w := readyWire{Type: TypeReady, Version: r.Version, PID: r.PID, Markers: r.Markers}
	if !r.ActiveDigest.IsZero() {
		d := r.ActiveDigest
		w.ActiveDigest = &d
	}
	return w
}

// inbound is any message as read. Members its type does not use stay
// zero, and members no type uses are ignored, as readers ignore unknown
// members.
type inbound struct {
	Type         string            `json:"type"`
	Version      string            `json:"version"`
	PID          int               `json:"pid"`
	Markers      map[string]string `json:"markers"`
	ActiveDigest *string           `json:"activeDigest"`
	Reason       string            `json:"reason"`
	Digest       string            `json:"digest"`
	Usage
}

// encode returns v as one message line.
func encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("handover: encode: %w", err)
	}
	if len(b) > MaxMessage {
		return nil, fmt.Errorf("%w: a message of %d bytes is over %d", ErrProtocol, len(b), MaxMessage)
	}
	return append(b, '\n'), nil
}

// decode parses one message line, without its newline.
func decode(line []byte) (inbound, error) {
	var m inbound
	if err := json.Unmarshal(line, &m); err != nil {
		return inbound{}, fmt.Errorf("%w: %w", ErrProtocol, err)
	}
	return m, nil
}

// decodeReady parses a new process's first or next message as a ready
// message: an unknown type, a missing or unknown marker and a malformed
// active digest are errors, each refused with unknown_marker.
func decodeReady(line []byte) (ready, error) {
	m, err := decode(line)
	if err != nil {
		return ready{}, err
	}
	if m.Type != TypeReady {
		return ready{}, fmt.Errorf("%w: type %q, want %q", ErrProtocol, m.Type, TypeReady)
	}
	if err := checkMarkers(m.Markers); err != nil {
		return ready{}, err
	}
	r := ready{Version: m.Version, PID: m.PID, Markers: m.Markers}
	if m.ActiveDigest != nil {
		d, err := revision.Parse(*m.ActiveDigest)
		if err != nil {
			return ready{}, fmt.Errorf("%w: activeDigest: %w", ErrProtocol, err)
		}
		r.ActiveDigest = d
	}
	return r, nil
}

// checkMarkers checks that markers names, for every format this release
// reads, the identifier it reads; markers of other formats are ignored.
func checkMarkers(markers map[string]string) error {
	want := Markers()
	for _, key := range slices.Sorted(maps.Keys(want)) {
		got, ok := markers[key]
		switch {
		case !ok:
			return fmt.Errorf("%w: no %s marker", ErrUnknownMarker, key)
		case got != want[key]:
			return fmt.Errorf("%w: %s marker %q, this release reads %q", ErrUnknownMarker, key, got, want[key])
		}
	}
	return nil
}
