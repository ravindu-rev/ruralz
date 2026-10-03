// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package retire

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
)

// Golden tests for the /debug/snapshots view (spec 04 req 73): states,
// display revision, digest, pins, RFC 3339 UTC times, pending and
// lastKnownGood as digests or null.

var update = flag.Bool("update", false, "rewrite testdata golden files")

func golden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := fs.ReadFile(os.DirFS("testdata"), name)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s mismatch:\n got: %s\nwant: %s", name, got, want)
	}
}

func TestReq73ViewGolden(t *testing.T) {
	x := newHarness(t, Config{}, false)
	var ps []pinned
	for i, name := range []string{"a", "b", "c", "d"} {
		if i > 0 {
			x.clock.Advance(time.Minute + 250*time.Millisecond)
		}
		x.publish(newSnap(name, nil))
		ps = append(ps, x.pin(0))
	}
	ps = append(ps, x.pin(1), x.pin(2))
	lkg := x.h.Active().Revision.Digest.String()
	golden(t, "snapshots.golden", x.h.View("sha256:0e1f2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0", lkg))
	x.clock.Advance(DefaultGrace)
	x.h.reconcile(context.Background())
	x.idle()
	golden(t, "snapshots-ending.golden", x.h.View("", ""))

	for _, p := range ps {
		x.unpin(p)
	}
	if err := x.h.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	golden(t, "snapshots-empty.golden", x.h.View("", ""))
}

// TestReq73ViewShape decodes the view with the admin wire type the CLI
// uses and checks the order: active first, then newest to oldest.
func TestReq73ViewShape(t *testing.T) {
	x := newHarness(t, Config{}, false)
	a, b := newSnap("a", nil), newSnap("b", nil)
	x.publish(a)
	p := x.pin(0)
	x.clock.Advance(time.Second)
	x.publish(b)
	v := x.h.View("", "")
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var back adminapi.Snapshots
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Snapshots) != 2 || back.Pending != nil || back.LastKnownGood != nil {
		t.Fatalf("view = %s", raw)
	}
	first, second := back.Snapshots[0], back.Snapshots[1]
	if first.State != adminapi.StateActive || first.Digest != b.Revision.Digest.String() || first.RetiredAt != "" {
		t.Fatalf("first entry = %+v, want the active snapshot b", first)
	}
	if second.State != adminapi.StateRetired || second.Revision != a.Revision.Digest.Short() || second.Pins != 1 ||
		second.RetiredAt != "2026-09-26T12:00:01Z" || second.GraceEndsAt != "" {
		t.Fatalf("second entry = %+v, want retired a with one pin", second)
	}
	x.unpin(p)
}
