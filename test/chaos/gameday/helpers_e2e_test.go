// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package gameday

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// nodeIncrease returns how much a counter grew over (from, to] on one
// Node.
func nodeIncrease(r *chaos.Run, node, name string, match promtext.Labels, from, to time.Time) float64 {
	return chaos.Increase(r.Sampler.Trace(node, name, match), from, to)
}

// nodeOf returns the Node the http balancer attributes a request's
// connection to: the backend of its client address, open or among the
// balancer's recently closed connections; "" when unknown.
func nodeOf(r *chaos.Run) func(chaos.Request) string {
	return func(q chaos.Request) string {
		if q.Node != "" {
			return q.Node
		}
		if q.Local == "" {
			return ""
		}
		n, _ := r.HTTP.BackendOf(q.Local)
		return n
	}
}

// writeConfig replaces a Node's RURALZ_CONFIG by an atomic rename, which
// the Node's watcher loads at its next poll.
func writeConfig(n *topology.Node, b []byte) error {
	tmp := n.ConfigPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, n.ConfigPath())
}

// waitUntil polls cond every topology.PollInterval of real time for at
// most d and reports whether it held: a condition wait under a real-time
// deadline, never a bare sleep.
func waitUntil(ctx context.Context, d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(topology.PollInterval):
		}
	}
	return true
}

// revisions returns each Node's active Revision from /readyz.
func revisions(ctx context.Context, t *testing.T, r *chaos.Run) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range r.Top.Nodes() {
		rz, _, err := n.Readyz(ctx)
		if err != nil {
			t.Fatalf("%s: %v", n.Name(), err)
		}
		out[n.Name()] = rz.Revision
	}
	return out
}

// logStreams adds every stream's interval log beside the record.
func logStreams(r *chaos.Run, l *chaos.Load) {
	for _, st := range l.Streams {
		r.IntervalLog(st.Spec.Name, st)
	}
}
