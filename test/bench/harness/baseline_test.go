// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"debug/buildinfo"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// TestReleaseBuildOf: a release run's baseline is the previous release's
// ruralzd under the commit and toolchain its build information records,
// so the baseline runs and the comparison name that release, never the
// head; a binary without a commit or of another version is refused.
func TestReleaseBuildOf(t *testing.T) {
	const prev = "1111111111111111111111111111111111111111"
	info := func(rev, version string) *buildinfo.BuildInfo {
		bi := &buildinfo.BuildInfo{GoVersion: "go1.27.3", Main: debug.Module{Path: "github.com/ravindu-rev/ruralz", Version: version}}
		if rev != "" {
			bi.Settings = []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "false"}}
		}
		return bi
	}
	b, err := releaseBuildOf("/baseline/ruralzd", "0.1.0", info(prev, "v0.1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if b.Ruralzd != "/baseline/ruralzd" || b.Commit != prev || b.Version != "0.1.0" || b.Toolchain != "go1.27.3" {
		t.Errorf("releaseBuildOf = %+v, want ruralzd 0.1.0 at %s built with go1.27.3", b, prev)
	}
	for name, c := range map[string]struct {
		info *buildinfo.BuildInfo
		want string
	}{
		"no commit":       {info("", "v0.1.0"), "records no commit"},
		"another version": {info(prev, "v0.2.0"), `module version "v0.2.0", not the release v0.1.0`},
		"a development":   {info(prev, "(devel)"), "not the release v0.1.0"},
	} {
		if _, err := releaseBuildOf("/baseline/ruralzd", "0.1.0", c.info); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, c.want)
		}
	}
	notGo := filepath.Join(t.TempDir(), "ruralzd")
	if err := os.WriteFile(notGo, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseBuild(notGo, "0.1.0"); err == nil || !strings.Contains(err.Error(), "the baseline ruralzd") {
		t.Errorf("ReleaseBuild of a file that is no Go binary: %v", err)
	}
}
