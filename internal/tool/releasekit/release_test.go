// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"strings"
	"testing"
)

// TestArtifacts: ruralzd for linux (production) and darwin (development
// only), ruralz for linux, darwin and windows/amd64; no ruralz-control
// archive before M2.
func TestArtifacts(t *testing.T) {
	var got []string
	for _, a := range artifacts() {
		got = append(got, a.target()+" "+a.Support)
	}
	want := []string{
		"ruralzd_linux_amd64 production", "ruralzd_linux_arm64 production",
		"ruralzd_darwin_arm64 development", "ruralzd_darwin_amd64 development",
		"ruralz_linux_amd64 production", "ruralz_linux_arm64 production",
		"ruralz_darwin_arm64 production", "ruralz_darwin_amd64 production",
		"ruralz_windows_amd64 production",
	}
	if !slices.Equal(got, want) {
		t.Errorf("artifacts() = %v\nwant %v", got, want)
	}
}

func TestArtifactNames(t *testing.T) {
	cases := []struct {
		a                         artifact
		archive, exe, dist, notes string
		systemd                   bool
	}{
		{artifact{"ruralzd", "linux", "amd64", supportProduction}, "ruralzd_0.1.0_linux_amd64.tar.gz", "ruralzd", "linux_amd64", "ruralzd_linux_amd64", true},
		{artifact{"ruralzd", "darwin", "arm64", supportDevelopment}, "ruralzd_0.1.0_darwin_arm64.tar.gz", "ruralzd", "darwin_arm64", "ruralzd_darwin_arm64", false},
		{artifact{"ruralz", "linux", "arm64", supportProduction}, "ruralz_0.1.0_linux_arm64.tar.gz", "ruralz", "linux_arm64", "ruralz_linux_arm64", false},
		{artifact{"ruralz", "windows", "amd64", supportProduction}, "ruralz_0.1.0_windows_amd64.zip", "ruralz.exe", "windows_amd64", "ruralz_windows_amd64", false},
	}
	for _, c := range cases {
		if got := c.a.archiveName("0.1.0"); got != c.archive {
			t.Errorf("archiveName = %q, want %q", got, c.archive)
		}
		if c.a.executable() != c.exe || c.a.distDir() != c.dist || c.a.target() != c.notes || c.a.withSystemd() != c.systemd {
			t.Errorf("%+v: executable %q dist %q target %q systemd %v", c.a, c.a.executable(), c.a.distDir(), c.a.target(), c.a.withSystemd())
		}
	}
}

func TestCheckVersion(t *testing.T) {
	good := []string{"0.1.0", "1.2.3", "0.1.0-rc.1", "0.0.0-dev", "0.1.0-3-gabcdef1", "0.1.0-3-gabcdef1-dirty", "10.20.30"}
	bad := []string{"", "v0.1.0", "0.1", "01.2.3", "0.1.0+build.1", "0.1.0-", "0.1.0-rc..1", "0.1.0 ", "0.1.0-01"}
	for _, v := range good {
		if err := checkVersion(v); err != nil {
			t.Errorf("checkVersion(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range bad {
		if err := checkVersion(v); err == nil {
			t.Errorf("checkVersion(%q) = nil, want an error", v)
		}
	}
}

func TestVersionKind(t *testing.T) {
	cases := map[string]string{
		"0.1.0": kindRelease, "1.0.0": kindRelease, "0.1.0-rc.1": kindCandidate, "0.1.0-rc.12": kindCandidate,
		"0.1.0-rc.0": kindDev, "0.1.0-rc1": kindDev, "0.0.0-dev": kindDev, "0.1.0-3-gabcdef1-dirty": kindDev,
	}
	for v, want := range cases {
		if got := versionKind(v); got != want {
			t.Errorf("versionKind(%q) = %q, want %q", v, got, want)
		}
	}
}

// TestImageTags: X.Y.Z and X.Y for a release, X.Y.Z-rc.N for a
// candidate, nothing for a development build, never latest.
func TestImageTags(t *testing.T) {
	cases := map[string][]string{
		"0.1.0":                  {"0.1.0", "0.1"},
		"1.12.3":                 {"1.12.3", "1.12"},
		"0.1.0-rc.2":             {"0.1.0-rc.2"},
		"0.0.0-dev":              {},
		"0.1.0-3-gabcdef1-dirty": {},
	}
	for v, want := range cases {
		got := imageTags(v)
		if !slices.Equal(got, want) || got == nil {
			t.Errorf("imageTags(%q) = %#v, want %#v", v, got, want)
		}
		if slices.Contains(got, "latest") {
			t.Errorf("imageTags(%q) has latest", v)
		}
	}
}

// TestImages: the ruralzd image for linux/amd64 and linux/arm64, built from
// deploy/container/Dockerfile's ruralzd stage.
func TestImages(t *testing.T) {
	got := images("0.1.0")
	if len(got) != 1 {
		t.Fatalf("images = %+v", got)
	}
	img := got[0]
	if img.Repository != "ghcr.io/ravindu-rev/ruralzd" || img.Binary != "ruralzd" || img.Dockerfile != "deploy/container/Dockerfile" ||
		img.Target != "ruralzd" || !slices.Equal(img.Platforms, []string{"linux/amd64", "linux/arm64"}) || !slices.Equal(img.Tags, []string{"0.1.0", "0.1"}) {
		t.Errorf("image plan = %+v", img)
	}
}

func TestCheckCommit(t *testing.T) {
	for _, c := range []string{fixtureCommit, strings.Repeat("ab", 32)} {
		if err := checkCommit(c); err != nil {
			t.Errorf("checkCommit(%q) = %v", c, err)
		}
	}
	for _, c := range []string{"", "unknown", "0123456", strings.ToUpper(fixtureCommit), fixtureCommit + "0"} {
		if err := checkCommit(c); err == nil {
			t.Errorf("checkCommit(%q) = nil", c)
		}
	}
}
