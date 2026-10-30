// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// Status values of the /readyz body.
const (
	StatusReady    = "ready"
	StatusNotReady = "not_ready"
)

// Issue is one failed condition: its registered RZ code, such as
// RZ-CFG-026 for an unresolved secretRef or RZ-CFG-039 for a listener
// that could not be bound, and a non-secret detail; either may be empty.
// The detail is served to unauthenticated probes, so it names a reference
// or a listener, never a value.
type Issue struct {
	Code   string
	Detail string
}

// Tracker holds the readiness conditions and the encoded /readyz answer.
type Tracker struct {
	cur atomic.Pointer[answer]
	// answered is set once /readyz answered 200.
	answered atomic.Bool

	mu        sync.Mutex
	revision  revision.Digest
	secrets   []Issue
	listeners []Issue
	draining  bool
}

// answer is one encoded /readyz response.
type answer struct {
	ready  bool
	status int
	body   []byte
	view   adminapi.Readyz
}

// New returns a Tracker that is not ready: no Revision, listeners not yet
// bound.
func New() *Tracker {
	t := &Tracker{listeners: []Issue{{}}}
	t.mu.Lock()
	t.publishLocked()
	t.mu.Unlock()
	return t
}

// SetRevision records the active validated Revision; the zero Digest
// means none (no_revision).
func (t *Tracker) SetRevision(d revision.Digest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.revision = d
	t.publishLocked()
}

// SetSecrets records the secretRef values of the active Revision that
// have not resolved, one secrets_unresolved reason each; none means every
// one resolved.
func (t *Tracker) SetSecrets(unresolved ...Issue) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.secrets = slices.Clone(unresolved)
	t.publishLocked()
}

// SetListeners records the listeners of the active Revision that are not
// bound, one listeners_unbound reason each; none means every listener is
// bound.
func (t *Tracker) SetListeners(unbound ...Issue) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.listeners = slices.Clone(unbound)
	t.publishLocked()
}

// Drain fails readiness with reason draining from now on; it never
// clears.
func (t *Tracker) Drain() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.draining = true
	t.publishLocked()
}

// Ready reports whether /readyz answers 200.
func (t *Tracker) Ready() bool { return t.cur.Load().ready }

// Answered reports whether /readyz has ever answered 200, so a balancer
// may have seen the Node ready. A Node that never did and has no client
// listener bound skips the Drain's accept window
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "Drain
// timeline defaults").
func (t *Tracker) Answered() bool { return t.answered.Load() }

// Readyz returns the current /readyz body, for sd_notify STATUS= and logs.
func (t *Tracker) Readyz() adminapi.Readyz {
	v := t.cur.Load().view
	v.Reasons = slices.Clone(v.Reasons)
	return v
}

// ServeHTTP answers /readyz (GET; the admin server routes): 200 with
// {"status":"ready","revision":"rev-<12 hex>"}, or 503 with
// {"status":"not_ready","reasons":[...]}.
func (t *Tracker) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	a := t.cur.Load()
	if a.ready && !t.answered.Load() {
		t.answered.Store(true)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(a.body)))
	w.WriteHeader(a.status)
	_, _ = w.Write(a.body)
}

// publishLocked encodes the conditions and publishes the answer; the
// caller holds mu.
func (t *Tracker) publishLocked() {
	var reasons []adminapi.ReadyReason
	if t.revision.IsZero() {
		reasons = append(reasons, adminapi.ReadyReason{Reason: adminapi.ReasonNoRevision})
	}
	for _, is := range t.secrets {
		reasons = append(reasons, adminapi.ReadyReason{Reason: adminapi.ReasonSecretsUnresolved, Code: is.Code, Detail: is.Detail})
	}
	for _, is := range t.listeners {
		reasons = append(reasons, adminapi.ReadyReason{Reason: adminapi.ReasonListenersUnbound, Code: is.Code, Detail: is.Detail})
	}
	if t.draining {
		reasons = append(reasons, adminapi.ReadyReason{Reason: adminapi.ReasonDraining})
	}
	a := &answer{ready: len(reasons) == 0, status: http.StatusOK}
	if a.ready {
		a.view = adminapi.Readyz{Status: StatusReady, Revision: t.revision.Short()}
	} else {
		a.status = http.StatusServiceUnavailable
		a.view = adminapi.Readyz{Status: StatusNotReady, Reasons: reasons}
	}
	body, err := json.Marshal(a.view)
	if err != nil {
		// A Readyz holds only strings; encoding cannot fail.
		panic(err)
	}
	a.body = body
	t.cur.Store(a)
}
