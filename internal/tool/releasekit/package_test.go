// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPackageArchives: one archive per artifact, named
// <binary>_<version>_<os>_<arch> (.zip on Windows), holding the binary,
// LICENSE, NOTICE, README.md and THIRD_PARTY_LICENSES; linux ruralzd
// archives add deploy/systemd/ with the helper executable.
func TestPackageArchives(t *testing.T) {
	fx := newFixture(t, "0.1.0")
	m, err := runPackage(t.Context(), fx.options(), fx.pkgDeps())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Archives) != len(artifacts()) {
		t.Fatalf("%d archives, want %d", len(m.Archives), len(artifacts()))
	}
	for i, a := range artifacts() {
		info := m.Archives[i]
		if info.Name != a.archiveName("0.1.0") || info.SBOM != info.Name+sbomSuffix || info.Support != a.Support {
			t.Errorf("archive %d = %+v", i, info)
		}
		data, err := os.ReadFile(filepath.Join(fx.out, info.Name))
		if err != nil {
			t.Fatal(err)
		}
		var got []stored
		if a.OS == "windows" {
			got, err = readZip(data)
		} else {
			got, _, err = readTarGz(data)
		}
		if err != nil {
			t.Fatal(err)
		}
		modes := map[string]string{}
		for _, s := range got {
			modes[s.Path] = entryOf(s.member).Mode
			if !s.ModTime.Equal(fixtureTime()) {
				t.Errorf("%s: %s dated %v", info.Name, s.Path, s.ModTime)
			}
		}
		want := map[string]string{a.executable(): "0755", "LICENSE": "0644", "NOTICE": "0644", "README.md": "0644", "THIRD_PARTY_LICENSES": "0644"}
		if a.withSystemd() {
			want["deploy/"], want["deploy/systemd/"] = "0755", "0755"
			want["deploy/systemd/ruralzd.service"], want["deploy/systemd/ruralzd-handover"] = "0644", "0755"
			want["deploy/systemd/ruralzd.env"], want["deploy/systemd/99-ruralzd.conf"] = "0644", "0644"
			want["deploy/systemd/ruralzd.sysusers"] = "0644"
		}
		if len(modes) != len(want) {
			t.Errorf("%s: members %v, want %v", info.Name, modes, want)
		}
		for p, mode := range want {
			if modes[p] != mode {
				t.Errorf("%s: %s mode %q, want %q", info.Name, p, modes[p], mode)
			}
		}
	}
	if !strings.HasSuffix(m.Archives[8].Name, "_windows_amd64.zip") {
		t.Errorf("windows archive %s", m.Archives[8].Name)
	}
}

// TestPackageDeterministic: packaging the same inputs twice gives
// byte-identical archives and manifests.
func TestPackageDeterministic(t *testing.T) {
	fx := newFixture(t, "0.1.0")
	if _, err := runPackage(t.Context(), fx.options(), fx.pkgDeps()); err != nil {
		t.Fatal(err)
	}
	first := fx.out
	fx.out = filepath.Join(t.TempDir(), "again")
	if _, err := runPackage(t.Context(), fx.options(), fx.pkgDeps()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		a, _ := os.ReadFile(filepath.Join(first, e.Name())) //nolint:gosec // A file of the test's release directory.
		b, err := os.ReadFile(filepath.Join(fx.out, e.Name()))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("%s differs between two runs (%v)", e.Name(), err)
		}
	}
}

// TestPackageManifest: the manifest records the version, commit, commit
// time, toolchain, every member, the schema assets and the image plan.
func TestPackageManifest(t *testing.T) {
	fx := newFixture(t, "0.1.0-rc.1")
	if _, err := runPackage(t.Context(), fx.options(), fx.pkgDeps()); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(filepath.Join(fx.out, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != manifestFormat || m.Version != "0.1.0-rc.1" || m.Commit != fixtureCommit ||
		m.CommitTime != "2026-10-01T12:30:45Z" || m.Toolchain != "go1.27.3" ||
		m.Checksums != checksumsName || m.Signature != signatureName || m.Notes != notesName {
		t.Errorf("manifest header = %+v", m)
	}
	if len(m.Images) != 1 || !slices.Equal(m.Images[0].Tags, []string{"0.1.0-rc.1"}) {
		t.Errorf("image plan = %+v", m.Images)
	}
	var schemas []string
	for _, s := range m.Schemas {
		schemas = append(schemas, s.Name+" "+s.Source)
		data, err := os.ReadFile(filepath.Join(fx.out, s.Name))
		src, _ := os.ReadFile(filepath.Join(fx.root, filepath.FromSlash(s.Source)))
		if err != nil || !bytes.Equal(data, src) || sha256Hex(data) != s.SHA256 {
			t.Errorf("schema %s is not a byte copy of %s", s.Name, s.Source)
		}
	}
	want := []string{
		"ruralz-v1alpha1-authoring.schema.json api/schema/ruralz/v1alpha1/authoring.schema.json",
		"ruralz-v1alpha1-rendered.schema.json api/schema/ruralz/v1alpha1/rendered.schema.json",
	}
	if !slices.Equal(schemas, want) {
		t.Errorf("schemas = %v, want %v", schemas, want)
	}
	e := m.Archives[0].Entries
	if e[0].Path != "LICENSE" || e[0].Type != entryFile || e[0].SHA256 == "" || e[0].Size == 0 {
		t.Errorf("first entry = %+v", e[0])
	}
}

// TestPackageRefusesBadBuilds: a binary that breaks the release build
// rules is a finding naming its artifact, and no manifest is written.
func TestPackageRefusesBadBuilds(t *testing.T) {
	fx := newFixture(t, "0.1.0")
	fx.tweak["ruralz_darwin_arm64"] = func(f *buildFacts) { f.Stripped = false }
	fx.tweak["ruralzd_linux_arm64"] = func(f *buildFacts) { withSetting(f, "CGO_ENABLED", "1") }
	_, err := runPackage(t.Context(), fx.options(), fx.pkgDeps())
	var fe *findingsError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want findings", err)
	}
	if !hasFinding(fe.findings, "ruralz_darwin_arm64: symbol table or DWARF present") || !hasFinding(fe.findings, "ruralzd_linux_arm64: build setting CGO_ENABLED") || len(fe.findings) != 2 {
		t.Errorf("findings = %q", fe.findings)
	}
	if _, err := os.Stat(filepath.Join(fx.out, manifestName)); !os.IsNotExist(err) {
		t.Error("a manifest was written despite findings")
	}
	// A file that is no Go binary is a finding too.
	fx2 := newFixture(t, "0.1.0")
	writeFile(t, filepath.Join(fx2.dist, "linux_amd64", "ruralz"), "#!/bin/sh\n")
	_, err = runPackage(t.Context(), fx2.options(), fx2.pkgDeps())
	if !errors.As(err, &fe) || !hasFinding(fe.findings, "ruralz_linux_amd64: not a Go binary") {
		t.Errorf("non-Go binary: %v", err)
	}
}

// TestPackageAuditsNotices: notices that do not cover the binary (another
// toolchain, a missing module, a foreign NOTICE) are findings.
func TestPackageAuditsNotices(t *testing.T) {
	fx := newFixture(t, "0.1.0")
	writeFile(t, filepath.Join(fx.notices, "ruralzd_linux_amd64", thirdPartyMember), thirdPartyText("go1.27.3", nil))
	writeFile(t, filepath.Join(fx.notices, "ruralz_windows_amd64", noticeMember), "Something else\n")
	_, err := runPackage(t.Context(), fx.options(), fx.pkgDeps())
	var fe *findingsError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want findings", err)
	}
	if !hasFinding(fe.findings, "ruralzd_linux_amd64: THIRD_PARTY_LICENSES has no section for example.com/Upper/mod") ||
		!hasFinding(fe.findings, "ruralz_windows_amd64: NOTICE does not start with the repository NOTICE") {
		t.Errorf("findings = %q", fe.findings)
	}
}

// TestPackageInputErrors: bad flags and missing inputs are errors, not
// findings.
func TestPackageInputErrors(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *fixture, *packageOptions)
		want   string
	}{
		{"version", func(_ *testing.T, _ *fixture, o *packageOptions) { o.version = "v0.1.0" }, "without its leading v"},
		{"commit", func(_ *testing.T, _ *fixture, o *packageOptions) { o.commit = "HEAD" }, "not a full lower-case Git commit"},
		{"binary", func(_ *testing.T, fx *fixture, _ *packageOptions) {
			_ = os.Remove(filepath.Join(fx.dist, "darwin_amd64", "ruralzd"))
		}, "ruralzd_darwin_amd64"},
		{"notices", func(_ *testing.T, fx *fixture, _ *packageOptions) {
			_ = os.RemoveAll(filepath.Join(fx.notices, "ruralz_linux_arm64"))
		}, "run depgate -notices first"},
		{"out not empty", func(t *testing.T, fx *fixture, _ *packageOptions) {
			writeFile(t, filepath.Join(fx.out, "stale.tar.gz"), "x")
		}, "is not empty"},
		{"toolchain", func(t *testing.T, fx *fixture, _ *packageOptions) {
			writeFile(t, filepath.Join(fx.root, "go.mod"), "module m\n\ngo 1.26.0\n")
		}, "no toolchain line"},
		{"systemd", func(_ *testing.T, fx *fixture, _ *packageOptions) { _ = os.RemoveAll(filepath.Join(fx.root, "deploy")) }, "deploy"},
		{"sysusers missing", func(_ *testing.T, fx *fixture, _ *packageOptions) {
			_ = os.Remove(filepath.Join(fx.root, "deploy", "systemd", "ruralzd.sysusers"))
		}, "ruralzd.sysusers"},
		{"stray systemd file", func(t *testing.T, fx *fixture, _ *packageOptions) {
			writeFile(t, filepath.Join(fx.root, "deploy", "systemd", "extra.conf"), "x\n")
		}, "deploy/systemd/extra.conf is not one of the shipped files"},
		{"schemas", func(_ *testing.T, fx *fixture, _ *packageOptions) { _ = os.RemoveAll(filepath.Join(fx.root, "api")) }, "schema"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t, "0.1.0")
			o := fx.options()
			c.change(t, fx, &o)
			_, err := runPackage(t.Context(), o, fx.pkgDeps())
			var fe *findingsError
			if err == nil || errors.As(err, &fe) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want an error containing %q", err, c.want)
			}
		})
	}
}
