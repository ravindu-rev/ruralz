// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build t5 && linux

package t5

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/handover"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
)

// buildBound bounds one build of ruralzd.
const buildBound = 10 * time.Minute

// refusedMarker is the handover marker of the refused-handover variant:
// an identifier no ruralzd reads, so a holder refuses the variant's ready
// message with unknown_marker
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "Handover
// protocol").
const refusedMarker = "ruralz.handover.t5-refused"

// buildCache builds each binary the t5 tests need at most once per test
// binary, in a directory TestMain removes at the end.
type buildCache struct {
	mu       sync.Mutex
	dir      string
	built    map[string]string
	failures map[string]error
}

// builds is the test binary's buildCache.
var builds = &buildCache{built: map[string]string{}, failures: map[string]error{}}

// get returns the binary kind, building it with build on the first call;
// a failed build fails every caller.
func (c *buildCache) get(t *testing.T, kind string, build func(ctx context.Context, root, dir string) (string, error)) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.built[kind]; ok {
		return p
	}
	if err, ok := c.failures[kind]; ok {
		t.Fatalf("building %s failed earlier: %v", kind, err)
	}
	if c.dir == "" {
		dir, err := os.MkdirTemp("", "ruralz-t5-bin-")
		if err != nil {
			t.Fatal(err)
		}
		c.dir = dir
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), buildBound)
	defer cancel()
	dir := filepath.Join(c.dir, kind)
	p, err := build(ctx, repoRoot(t), dir)
	if err != nil {
		c.failures[kind] = err
		t.Fatalf("building %s: %v", kind, err)
	}
	c.built[kind] = p
	return p
}

// cleanup removes the built binaries.
func (c *buildCache) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dir != "" {
		_ = os.RemoveAll(c.dir)
	}
}

// releaseRuralzd returns ruralzd built from this checkout with the release
// flags, or the build RURALZ_TEST_BIN_DIR names.
func releaseRuralzd(t *testing.T) string {
	t.Helper()
	return builds.get(t, "release", func(ctx context.Context, root, dir string) (string, error) {
		set, err := binaries.Build(ctx, root, dir, binaries.BuildOptions{Commands: []string{binaries.Ruralzd}})
		return set.Ruralzd, err
	})
}

// refusedRuralzd returns the refused-handover variant: ruralzd built from
// this checkout with the release flags and, through go build -overlay,
// the handover marker refusedMarker.
func refusedRuralzd(t *testing.T) string {
	t.Helper()
	return builds.get(t, "refused", func(ctx context.Context, root, dir string) (string, error) {
		const rel = "internal/gateway/handover/message.go"
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // G304: a fixed file of the repository
		if err != nil {
			return "", err
		}
		from := []byte("const Format = " + strconv.Quote(handover.Format))
		if !bytes.Contains(src, from) {
			return "", fmt.Errorf("%s no longer declares %s: update the refused-handover variant", rel, from)
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", err
		}
		repl := filepath.Join(dir, "message.go")
		variant := bytes.Replace(src, from, []byte("const Format = "+strconv.Quote(refusedMarker)), 1)
		if err := os.WriteFile(repl, variant, 0o600); err != nil {
			return "", err
		}
		set, err := binaries.Build(ctx, root, filepath.Join(dir, "bin"), binaries.BuildOptions{
			Commands: []string{binaries.Ruralzd}, Overlay: map[string]string{rel: repl},
		})
		return set.Ruralzd, err
	})
}
