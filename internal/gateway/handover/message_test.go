// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handover

import (
	"bytes"
	"errors"
	"maps"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// digestA and digestB are two Revision digests.
var (
	digestA = revision.Sum([]byte("revision a"))
	digestB = revision.Sum([]byte("revision b"))
)

// TestMarkers checks the format identifiers a ready message carries:
// ruralz.handover.v1, ruralz.lkg.v1, ruralz.holder.v1 and
// ruralz.canonical.v1, as docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "Handover protocol" lists them.
func TestMarkers(t *testing.T) {
	want := map[string]string{
		"handover":  "ruralz.handover.v1",
		"lkg":       "ruralz.lkg.v1",
		"holder":    "ruralz.holder.v1",
		"canonical": "ruralz.canonical.v1",
	}
	if got := Markers(); !maps.Equal(got, want) {
		t.Errorf("Markers() = %v, want %v", got, want)
	}
}

// TestEncodeMessages checks each message's wire form: one JSON object on
// its own line with the members of "Handover protocol", a null
// activeDigest for a new process without an active Revision, the usage
// members on accepted and usage, and watchdogUsec on accepted only while
// the holder runs a watchdog.
func TestEncodeMessages(t *testing.T) {
	u := Usage{Connections: 3, InflightUnits: 4, BufferedBytes: 5, PluginMemoryBytes: 6, RSSBytes: 7}
	usage := `"connections":3,"inflightUnits":4,"bufferedBytes":5,"pluginMemoryBytes":6,"rssBytes":7`
	markers := `"markers":{"canonical":"ruralz.canonical.v1","handover":"ruralz.handover.v1","holder":"ruralz.holder.v1","lkg":"ruralz.lkg.v1"}`
	tests := []struct {
		name string
		v    any
		want string
	}{
		{
			name: "ready",
			v:    readyMessage(ready{Version: "0.1.0", PID: 4242, Markers: Markers(), ActiveDigest: digestA}),
			want: `{"type":"ready","version":"0.1.0","pid":4242,` + markers + `,"activeDigest":"` + digestA.String() + `"}`,
		},
		{
			name: "ready without an active Revision",
			v:    readyMessage(ready{Version: "0.1.0", PID: 4242, Markers: Markers()}),
			want: `{"type":"ready","version":"0.1.0","pid":4242,` + markers + `,"activeDigest":null}`,
		},
		{name: "refused", v: refusedWire{Type: TypeRefused, Reason: ReasonBusy}, want: `{"type":"refused","reason":"busy"}`},
		{name: "reload", v: reloadWire{Type: TypeReload, Digest: digestB}, want: `{"type":"reload","digest":"` + digestB.String() + `"}`},
		{name: "accepted", v: acceptedMessage(u, 0), want: `{"type":"accepted",` + usage + `}`},
		{
			name: "accepted with the holder's watchdog interval",
			v:    acceptedMessage(u, 30*time.Second),
			want: `{"type":"accepted",` + usage + `,"watchdogUsec":30000000}`,
		},
		{name: "usage", v: usageWire{Type: TypeUsage, Usage: u}, want: `{"type":"usage",` + usage + `}`},
		{name: "listening", v: bareWire{Type: TypeListening}, want: `{"type":"listening"}`},
		{name: "released", v: bareWire{Type: TypeReleased}, want: `{"type":"released"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := encode(tt.v)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got := string(b); got != tt.want+"\n" {
				t.Errorf("encode = %s, want %s", got, tt.want)
			}
		})
	}
}

// acceptedWatchdog reads line as a successor reads an accepted message and
// returns the holder's watchdog interval it carries.
func acceptedWatchdog(line string) (inbound, time.Duration, error) {
	m, err := decode([]byte(line))
	if err != nil {
		return inbound{}, 0, err
	}
	iv, err := m.watchdog()
	return m, iv, err
}

// TestAcceptedWatchdogRoundTrip checks that the holder's watchdog interval
// reaches the successor through accepted unchanged, from one microsecond,
// WATCHDOG_USEC's unit, to the largest a time.Duration holds, with the
// usage beside it, and that a holder without a watchdog sends an accepted
// the successor reads as none.
func TestAcceptedWatchdogRoundTrip(t *testing.T) {
	u := Usage{Connections: 3, InflightUnits: 4, BufferedBytes: 5, PluginMemoryBytes: 6, RSSBytes: 7}
	for _, iv := range []time.Duration{0, time.Microsecond, 30 * time.Second, time.Duration(maxWatchdogUsec) * time.Microsecond} {
		t.Run(iv.String(), func(t *testing.T) {
			b, err := encode(acceptedMessage(u, iv))
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			m, got, err := acceptedWatchdog(string(bytes.TrimSuffix(b, []byte("\n"))))
			if err != nil || m.Type != TypeAccepted || m.Usage != u || got != iv {
				t.Errorf("accepted %s read back as type %q, usage %+v, watchdog %v, %v; want accepted, %+v, %v", b, m.Type, m.Usage, got, err, u, iv)
			}
		})
	}
}

// TestDecodeAcceptedWatchdog checks how a successor reads watchdogUsec: an
// accepted without it, as an older holder or one without a watchdog
// sends, or with null or 0, carries no interval, so the successor runs
// none of its own; a whole number of microseconds up to the largest a
// time.Duration holds is the interval; a negative or larger value, or one
// that is not a whole number, breaks the protocol.
func TestDecodeAcceptedWatchdog(t *testing.T) {
	largest := strconv.FormatInt(maxWatchdogUsec, 10)
	tooLarge := strconv.FormatInt(maxWatchdogUsec+1, 10)
	tests := []struct {
		name    string
		line    string
		want    time.Duration
		wantErr error
	}{
		{
			name: "an older holder's accepted",
			line: `{"type":"accepted","connections":3,"inflightUnits":4,"bufferedBytes":5,"pluginMemoryBytes":6,"rssBytes":7}`,
		},
		{name: "null", line: `{"type":"accepted","watchdogUsec":null}`},
		{name: "zero", line: `{"type":"accepted","watchdogUsec":0}`},
		{name: "one microsecond", line: `{"type":"accepted","watchdogUsec":1}`, want: time.Microsecond},
		{name: "the T5 unit's 30 s", line: `{"type":"accepted","connections":3,"watchdogUsec":30000000}`, want: 30 * time.Second},
		{name: "the largest duration", line: `{"type":"accepted","watchdogUsec":` + largest + `}`, want: time.Duration(maxWatchdogUsec) * time.Microsecond},
		{name: "negative", line: `{"type":"accepted","watchdogUsec":-1}`, wantErr: ErrProtocol},
		{name: "beyond a duration", line: `{"type":"accepted","watchdogUsec":` + tooLarge + `}`, wantErr: ErrProtocol},
		{name: "a string", line: `{"type":"accepted","watchdogUsec":"30s"}`, wantErr: ErrProtocol},
		{name: "a fraction", line: `{"type":"accepted","watchdogUsec":1.5}`, wantErr: ErrProtocol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got, err := acceptedWatchdog(tt.line)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Errorf("accepted %s: watchdog %v, %v; want %v, %v", tt.line, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// TestEncodeRefusesLongMessage checks the 64 KiB bound on a message
// written.
func TestEncodeRefusesLongMessage(t *testing.T) {
	_, err := encode(refusedWire{Type: TypeRefused, Reason: strings.Repeat("x", MaxMessage)})
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("encode of a long message = %v, want ErrProtocol", err)
	}
}

// TestDecodeReady checks how the holder reads a ready message: unknown
// members and markers of other formats are ignored, a null or missing
// activeDigest means none, and another type, a missing marker, an
// identifier this release does not read, a malformed digest or a line
// that is not one JSON object is an error the holder refuses with
// unknown_marker.
func TestDecodeReady(t *testing.T) {
	markers := `{"handover":"ruralz.handover.v1","lkg":"ruralz.lkg.v1","holder":"ruralz.holder.v1","canonical":"ruralz.canonical.v1"}`
	tests := []struct {
		name    string
		line    string
		want    revision.Digest
		wantErr error
	}{
		{name: "active digest", line: `{"type":"ready","version":"0.1.0","pid":7,"markers":` + markers + `,"activeDigest":"` + digestA.String() + `"}`, want: digestA},
		{name: "null digest", line: `{"type":"ready","markers":` + markers + `,"activeDigest":null}`},
		{name: "missing digest", line: `{"type":"ready","markers":` + markers + `}`},
		{name: "unknown members", line: `{"type":"ready","markers":` + markers + `,"activeDigest":"` + digestA.String() + `","future":{"x":1}}`, want: digestA},
		{
			name: "markers of other formats",
			line: `{"type":"ready","markers":{"handover":"ruralz.handover.v1","lkg":"ruralz.lkg.v1","holder":"ruralz.holder.v1","canonical":"ruralz.canonical.v1","nodecount":"ruralz.nodecount.v1"}}`,
		},
		{name: "another type", line: `{"type":"hello","markers":` + markers + `}`, wantErr: ErrProtocol},
		{name: "no type", line: `{"markers":` + markers + `}`, wantErr: ErrProtocol},
		{name: "missing marker", line: `{"type":"ready","markers":{"handover":"ruralz.handover.v1","lkg":"ruralz.lkg.v1","holder":"ruralz.holder.v1"}}`, wantErr: ErrUnknownMarker},
		{name: "no markers", line: `{"type":"ready"}`, wantErr: ErrUnknownMarker},
		{name: "unknown identifier", line: `{"type":"ready","markers":{"handover":"ruralz.handover.v2","lkg":"ruralz.lkg.v1","holder":"ruralz.holder.v1","canonical":"ruralz.canonical.v1"}}`, wantErr: ErrUnknownMarker},
		{name: "malformed digest", line: `{"type":"ready","markers":` + markers + `,"activeDigest":"rev-0123456789ab"}`, wantErr: ErrProtocol},
		{name: "not JSON", line: `ready`, wantErr: ErrProtocol},
		{name: "an array", line: `[{"type":"ready"}]`, wantErr: ErrProtocol},
		{name: "trailing data", line: `{"type":"ready","markers":` + markers + `} {}`, wantErr: ErrProtocol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := decodeReady([]byte(tt.line))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("decodeReady(%s) error = %v, want %v", tt.line, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeReady(%s) error = %v", tt.line, err)
			}
			if r.ActiveDigest != tt.want {
				t.Errorf("decodeReady(%s) digest = %v, want %v", tt.line, r.ActiveDigest, tt.want)
			}
		})
	}
}

// TestPeerReadsLinesUpToTheBound checks the reader's 64 KiB bound: a line
// of exactly MaxMessage bytes arrives whole, a longer one ends the
// reading with ErrProtocol.
func TestPeerReadsLinesUpToTheBound(t *testing.T) {
	a, b := net.Pipe()
	p := newPeer(a)
	defer p.close()
	defer func() { _ = b.Close() }()
	long := strings.Repeat("x", MaxMessage)
	go func() {
		_, _ = b.Write([]byte(long + "\n"))
		_, _ = b.Write([]byte(long + "x\n"))
	}()
	select {
	case l := <-p.in:
		if l.err != nil || len(l.b) != MaxMessage {
			t.Fatalf("a %d-byte line: got %d bytes, error %v", MaxMessage, len(l.b), l.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the first line")
	}
	select {
	case l := <-p.in:
		if !errors.Is(l.err, ErrProtocol) {
			t.Fatalf("a longer line: error %v, want ErrProtocol", l.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the long line's error")
	}
}

// TestRefusedError checks that a refusal names its reason and matches
// ErrRefused.
func TestRefusedError(t *testing.T) {
	err := error(&RefusedError{Reason: ReasonOlderCandidate})
	if !errors.Is(err, ErrRefused) {
		t.Errorf("errors.Is(%v, ErrRefused) = false", err)
	}
	if !strings.Contains(err.Error(), ReasonOlderCandidate) {
		t.Errorf("Error() = %q, want the reason", err.Error())
	}
	for o, want := range map[Outcome]string{OutcomeHolder: "holder", OutcomeRefused: "refused", OutcomeLost: "lost", OutcomeStopped: "stopped", 0: "none"} {
		if got := o.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q, want %q", o, got, want)
		}
	}
}
