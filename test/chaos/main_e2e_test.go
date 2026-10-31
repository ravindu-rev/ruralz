// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
)

// suite is the harness TestMain builds for the whole test binary.
var suite *Harness

// envSaturation hands an InNetNS child its parent's saturation S.
const envSaturation = "RURALZ_TEST_CHAOS_SATURATION"

// TestMain builds the ruralzd builds the experiments run, once per test
// binary: the controllable-clock build and the release build of this
// commit, or the build RURALZ_TEST_BIN_DIR names.
func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

func runSuite(m *testing.M) int {
	dir, err := os.MkdirTemp("", "ruralz-chaos-bin-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "chaos: %v\n", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(dir) }()
	h, err := NewHarness(context.Background(), dir)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "chaos: %v\n", err)
		return 2
	}
	if v := os.Getenv(envSaturation); v != "" {
		s, err := strconv.ParseFloat(v, 64)
		if err != nil || s <= 0 {
			_, _ = fmt.Fprintf(os.Stderr, "chaos: %s=%q\n", envSaturation, v)
			return 2
		}
		h.SetSaturation(s)
	}
	suite = h
	return m.Run()
}

// saturation returns the topology's saturation S, measuring it in a
// subtest of the first experiment that needs it; once that measurement
// failed, every later run fails at once.
func saturation(t *testing.T) float64 {
	t.Helper()
	s, err := suite.Saturation()
	switch {
	case err == nil:
		return s
	case !errors.Is(err, ErrNoSaturation):
		t.Fatal(err)
	}
	if !t.Run(Saturation, func(t *testing.T) { s = suite.MeasureSaturation(t) }) {
		suite.FailSaturation(fmt.Errorf("chaos: measuring the saturation S failed in %s", t.Name()))
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
