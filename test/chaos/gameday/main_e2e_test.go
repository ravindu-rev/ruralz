// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package gameday

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// suite is the harness TestMain builds for the whole test binary.
var suite *chaos.Harness

// envSaturation hands an InNetNS child its parent's saturation S.
const envSaturation = "RURALZ_TEST_CHAOS_SATURATION"

// TestMain builds the ruralzd builds the drills run, once per test binary,
// as package chaos does: the controllable-clock build and the release build
// of this commit, or the build RURALZ_TEST_BIN_DIR names.
func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

func runSuite(m *testing.M) int {
	dir, err := os.MkdirTemp("", "ruralz-gameday-bin-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gameday: %v\n", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(dir) }()
	h, err := chaos.NewHarness(context.Background(), dir)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gameday: %v\n", err)
		return 2
	}
	if v := os.Getenv(envSaturation); v != "" {
		s, err := strconv.ParseFloat(v, 64)
		if err != nil || s <= 0 {
			_, _ = fmt.Fprintf(os.Stderr, "gameday: %s=%q\n", envSaturation, v)
			return 2
		}
		h.SetSaturation(s)
	}
	suite = h
	return m.Run()
}

// saturation returns the topology's saturation S: the job's saturation
// record when an earlier package of the job wrote one, else measured in a
// subtest of the first drill that needs it; once that measurement failed,
// every later run fails at once.
func saturation(t *testing.T) float64 {
	t.Helper()
	s, err := suite.Saturation()
	switch {
	case err == nil:
		return s
	case !errors.Is(err, chaos.ErrNoSaturation):
		t.Fatal(err)
	}
	if !t.Run(chaos.Saturation, func(t *testing.T) { s = suite.MeasureSaturation(t) }) {
		suite.FailSaturation(fmt.Errorf("gameday: measuring the saturation S failed in %s", t.Name()))
		t.FailNow()
	}
	return s
}

// childEnv is the environment of an InNetNS child: the parent's builds
// and saturation.
func childEnv() []string {
	env := []string{binaries.EnvBinDir + "=" + filepath.Dir(suite.Functional.Ruralzd)}
	if s, err := suite.Saturation(); err == nil {
		env = append(env, envSaturation+"="+strconv.FormatFloat(s, 'f', -1, 64))
	}
	return env
}

// testWriter logs a child's output.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Logf("%s", b)
	return len(b), nil
}
