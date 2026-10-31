// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package chaos is the nightly Chaos job's suite
// (docs/engineering/03-testing-and-quality-strategy.md "Chaos testing"):
// the M1 Scalability chaos experiments, the M1 System overview failure
// rows and the M1 lab gates of the Verification runbook, each injecting
// its fault into Nodes under load and asserting its criteria through
// metrics and responses, with one ruralz.chaos.v1 record per run. The runs
// are tests under the e2e build tag; the untagged files hold the harness
// they share with the drills of test/chaos/gameday.
package chaos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
)

// BuildTimeout bounds building the binaries under test.
const BuildTimeout = 10 * time.Minute

// Harness is the chaos suite of one test binary: the scale profile, the
// ruralzd builds under test, the configuration pipeline that renders the
// chaos Bundle, the commit and the topology's saturation S, measured once
// per job (docs/engineering/03-testing-and-quality-strategy.md "Chaos
// testing"). The drills of test/chaos/gameday build theirs the same way.
type Harness struct {
	// Profile is the scale profile RURALZ_CHAOS_SCALE selects.
	Profile Profile
	// Functional is the ruralzd the functional criteria run on: the
	// controllable-clock build of the commit, or the build
	// RURALZ_TEST_BIN_DIR names, which runs in real time when it is a
	// release build.
	Functional binaries.Set
	// Measurement is the release build the latency and CPU criteria
	// run on.
	Measurement binaries.Set
	// Commit is the commit under test.
	Commit string
	// Records is the directory RURALZ_CHAOS_RECORDS names; "" writes no
	// record.
	Records string
	// Root is the repository root.
	Root string

	pl     *pipeline.Pipeline
	binDir string

	satMu  sync.Mutex
	sat    float64
	satSet bool
	satErr error

	variantMu sync.Mutex
	variants  map[string]binaries.Set
}

// NewHarness returns the harness of a test binary: the profile from the
// environment, ruralzd built into dir as the controllable-clock and the
// release build (or taken from RURALZ_TEST_BIN_DIR for both), and the
// configuration pipeline. The target profile needs its RH-1 inventory and
// stops at ErrTargetPlacement (checkProfile).
func NewHarness(ctx context.Context, dir string) (*Harness, error) {
	p, err := ProfileFromEnv()
	if err != nil {
		return nil, err
	}
	if err := checkProfile(p, os.Getenv); err != nil {
		return nil, err
	}
	root, err := binaries.RepoRoot(".")
	if err != nil {
		return nil, err
	}
	h := &Harness{Profile: p, Root: root, Records: os.Getenv(EnvRecords), binDir: dir, variants: map[string]binaries.Set{}}
	h.Commit = gitCommit(ctx, root)
	bctx, cancel := context.WithTimeout(ctx, BuildTimeout)
	defer cancel()
	opts := binaries.BuildOptions{Commands: []string{binaries.Ruralzd}, Commit: h.Commit}
	if os.Getenv(binaries.EnvBinDir) != "" {
		// The build under test, for both kinds of criteria.
		opts.Commit = ""
		if h.Functional, err = binaries.Build(bctx, root, filepath.Join(dir, "functional"), opts); err != nil {
			return nil, err
		}
		h.Measurement = h.Functional
	} else {
		fopts := opts
		fopts.TestClock = true
		if h.Functional, err = binaries.Build(bctx, root, filepath.Join(dir, "functional"), fopts); err != nil {
			return nil, err
		}
		if h.Measurement, err = binaries.Build(bctx, root, filepath.Join(dir, "measurement"), opts); err != nil {
			return nil, err
		}
	}
	celc, err := cel.New()
	if err != nil {
		return nil, err
	}
	if h.pl, err = pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: celc, Now: clock.Real().Now}); err != nil {
		return nil, err
	}
	return h, nil
}

// gitCommit returns the commit of the repository at root, "unknown"
// without Git.
func gitCommit(ctx context.Context, root string) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return binaries.DefaultCommit
	}
	if c := strings.TrimSpace(string(out)); c != "" {
		return c
	}
	return binaries.DefaultCommit
}

// Pipeline returns the configuration pipeline the harness renders with.
func (h *Harness) Pipeline() *pipeline.Pipeline { return h.pl }

// Digest returns the Revision digest of a rendered Bundle as a file-mode
// Node computes it: the configuration pipeline over the one file, with no
// variables.
func (h *Harness) Digest(rendered []byte) (string, error) {
	res, err := h.pl.Run(context.Background(), pipeline.Request{
		Source:    loader.Source{FS: fstest.MapFS{"ruralz.yaml": &fstest.MapFile{Data: rendered}}, Path: "ruralz.yaml"},
		Variables: subst.Map{},
	})
	if err != nil {
		return "", err
	}
	if res.Validated == nil {
		return "", fmt.Errorf("chaos: the Bundle renders no Revision: %w", diagError(res.Diagnostics))
	}
	return res.Validated.Revision.Digest.String(), nil
}

// Variant returns a ruralzd variant built once per name with o, such as
// V-6's N-1 build; Commands and TestClock follow the functional build.
func (h *Harness) Variant(ctx context.Context, name string, o binaries.BuildOptions) (binaries.Set, error) {
	h.variantMu.Lock()
	defer h.variantMu.Unlock()
	if s, ok := h.variants[name]; ok {
		return s, nil
	}
	if name == "" || strings.ContainsAny(name, `/\.`) {
		return binaries.Set{}, fmt.Errorf("chaos: variant name %q", name)
	}
	o.Commands = []string{binaries.Ruralzd}
	o.TestClock = h.Functional.TestClock
	bctx, cancel := context.WithTimeout(ctx, BuildTimeout)
	defer cancel()
	s, err := binaries.Build(bctx, h.Root, filepath.Join(h.binDir, "variant-"+name), o)
	if err != nil {
		return binaries.Set{}, err
	}
	h.variants[name] = s
	return s, nil
}

// ErrNoSaturation is returned by Saturation before it was measured or
// set.
var ErrNoSaturation = errors.New("chaos: the topology's saturation is not measured")

// SetSaturation records the topology's saturation S, measured by
// MeasureSaturation or read from an earlier package's record.
func (h *Harness) SetSaturation(s float64) {
	h.satMu.Lock()
	defer h.satMu.Unlock()
	h.sat, h.satSet, h.satErr = s, true, nil
}

// FailSaturation records that measuring S failed, so the runs that need
// it fail at once instead of measuring it again, once per job.
func (h *Harness) FailSaturation(err error) {
	h.satMu.Lock()
	defer h.satMu.Unlock()
	if !h.satSet {
		h.satErr = err
	}
}

// Saturation returns the topology's saturation S: ErrNoSaturation before
// it was measured or set, or the error FailSaturation recorded.
func (h *Harness) Saturation() (float64, error) {
	h.satMu.Lock()
	defer h.satMu.Unlock()
	switch {
	case h.satSet:
		return h.sat, nil
	case h.satErr != nil:
		return 0, h.satErr
	default:
		return 0, ErrNoSaturation
	}
}
