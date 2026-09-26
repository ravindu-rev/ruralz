// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package adminapi holds the JSON wire shapes of the Ruralz Gateway admin
// API (port 9901) that ruralzd serves and the ruralz CLI reads: /readyz,
// /tap events and drop notices, /debug/snapshots. /config/dump is the
// RFC 8785 document of internal/config/canonical. Shapes are versioned
// surfaces: members are only added, and readers ignore unknown members.
package adminapi

// Readiness reasons of /readyz.
const (
	ReasonNoRevision        = "no_revision"
	ReasonSecretsUnresolved = "secrets_unresolved" //nolint:gosec // G101: a readiness reason, not a credential.
	ReasonListenersUnbound  = "listeners_unbound"
	ReasonDraining          = "draining"
)

// Readyz is the /readyz body: {"status":"ready","revision":"rev-…"} or
// {"status":"not_ready","reasons":[…]}.
type Readyz struct {
	Status   string        `json:"status"`
	Revision string        `json:"revision,omitempty"`
	Reasons  []ReadyReason `json:"reasons,omitempty"`
}

// ReadyReason is one reason a Node is not ready; Detail is non-secret.
type ReadyReason struct {
	Reason string `json:"reason"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// TapEvent is one NDJSON line of /tap per sampled exchange. Header values
// of credential headers are "[REDACTED]"; bodies and query strings never
// appear.
type TapEvent struct {
	Time            string              `json:"time"`
	TraceID         string              `json:"traceId"`
	Listener        string              `json:"listener"`
	Protocol        string              `json:"protocol"`
	Route           string              `json:"route"`
	Method          string              `json:"method"`
	Host            string              `json:"host"`
	Path            string              `json:"path"`
	Status          int                 `json:"status"`
	Code            string              `json:"code,omitempty"`
	DurationMs      float64             `json:"durationMs"`
	RequestBytes    int64               `json:"requestBytes"`
	ResponseBytes   int64               `json:"responseBytes"`
	Upstream        string              `json:"upstream,omitempty"`
	Endpoint        string              `json:"endpoint,omitempty"`
	Consumer        string              `json:"consumer,omitempty"`
	RequestHeaders  map[string][]string `json:"requestHeaders,omitempty"`
	ResponseHeaders map[string][]string `json:"responseHeaders,omitempty"`
}

// TapDropped is the NDJSON line sent before the next delivered event after
// a slow subscriber lost events: {"dropped":N}.
type TapDropped struct {
	Dropped int64 `json:"dropped"`
}

// Snapshot states of /debug/snapshots.
const (
	StateActive  = "active"
	StateRetired = "retired"
	StateClosing = "closing"
	StateEnding  = "ending"
)

// Snapshots is the /debug/snapshots body.
type Snapshots struct {
	Snapshots     []SnapshotInfo `json:"snapshots"`
	Pending       *string        `json:"pending"`
	LastKnownGood *string        `json:"lastKnownGood"`
}

// SnapshotInfo describes one live snapshot; times are RFC 3339 UTC.
type SnapshotInfo struct {
	State       string `json:"state"`
	Revision    string `json:"revision"`
	Digest      string `json:"digest"`
	Pins        int64  `json:"pins"`
	ActivatedAt string `json:"activatedAt"`
	RetiredAt   string `json:"retiredAt,omitempty"`
	GraceEndsAt string `json:"graceEndsAt,omitempty"`
}
