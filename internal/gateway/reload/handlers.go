// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"net/http"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// Snapshots returns the /debug/snapshots body (admin.Config.Snapshots):
// the retirer's live snapshots with the pending and Last-Known-Good
// digests, and the last rejected load, null before one.
func (l *Loader) Snapshots() adminapi.Snapshots {
	l.mu.Lock()
	pending, lastKnownGood := "", ""
	if l.next != nil {
		pending = l.next.digest.String()
	}
	if !l.lkgPointer.IsZero() {
		lastKnownGood = l.lkgPointer.String()
	}
	var rej *adminapi.Rejection
	if r := l.lastRejected; r != nil {
		c := *r
		if r.Digest != nil {
			d := *r.Digest
			c.Digest = &d
		}
		rej = &c
	}
	l.mu.Unlock()
	v := l.cfg.Holder.View(pending, lastKnownGood)
	v.LastRejected = rej
	return v
}

// ConfigDump returns the /config/dump handler (admin.Config.ConfigDump):
// 200 with the RFC 8785 document {"content","digest","lastKnownGood",
// "revision"} of the active Revision, its exact ruralz.canonical.v1 bytes,
// secretRef shown and secrets never resolved into it, which ruralz node
// dump checks and saves; before the first activation 503 RZ-RT-024. The
// admin server routes only GET here and logs every use.
func (l *Loader) ConfigDump() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		active, lastKnownGood := l.active, l.lkgPointer
		l.mu.Unlock()
		body, ok := canonical.AppendDump(nil, active, lastKnownGood)
		if !ok {
			problem.Write(w, problem.New(codeNoRevision, l.reqID(r)), nil)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}
