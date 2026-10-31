// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestChecksums: SHA256SUMS lists every other file of the release
// directory, so its one signature covers every asset: the archives and
// their SBOMs, the image SBOM, the schema files, release-manifest.json,
// NOTES.md and the benchmark records archive, as sorted "<sha256>  <name>"
// lines that sha256sum -c reads.
func TestChecksums(t *testing.T) {
	fx := newFixture(t, "0.1.0")
	m := fx.release(t)
	data, err := os.ReadFile(filepath.Join(fx.out, checksumsName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var names []string
	for _, l := range lines {
		sum, name, ok := strings.Cut(l, "  ")
		if !ok || len(sum) != 64 {
			t.Fatalf("line %q is not sha256sum format", l)
		}
		content, err := os.ReadFile(filepath.Join(fx.out, name)) //nolint:gosec // A file of the test's release directory.
		if err != nil || sha256Hex(content) != sum {
			t.Errorf("%s: checksum does not match (%v)", name, err)
		}
		names = append(names, name)
	}
	want := []string{manifestName, notesName, m.Bench.Name, m.Images[0].SBOM}
	for _, a := range m.Archives {
		want = append(want, a.Name, a.SBOM)
	}
	for _, s := range m.Schemas {
		want = append(want, s.Name)
	}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("SHA256SUMS lists %v, want %v", names, want)
	}
	entries, err := os.ReadDir(fx.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want)+1 {
		t.Errorf("the release directory holds %d files, want the %d SHA256SUMS lists and SHA256SUMS", len(entries), len(want))
	}

	// A development build without an image has no image SBOM to list.
	dev := newFixture(t, "0.0.0-dev")
	dev.image = ""
	dm := dev.release(t)
	data, err = os.ReadFile(filepath.Join(dev.out, checksumsName))
	if err != nil {
		t.Fatal(err)
	}
	if dm.Images[0].recorded() || strings.Contains(string(data), "_image.cdx.json") {
		t.Errorf("a build without an image lists an image SBOM:\n%s", data)
	}
}

// TestChecksumsFindings: a changed, missing or unexpected release file is
// a finding, and SHA256SUMS is not written.
func TestChecksumsFindings(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, fx *fixture, m *manifest)
		want   string
	}{
		{"changed archive", func(t *testing.T, fx *fixture, m *manifest) {
			writeFile(t, filepath.Join(fx.out, m.Archives[0].Name), "tampered")
		}, m0 + " does not match its manifest SHA-256"},
		{"changed records archive", func(t *testing.T, fx *fixture, m *manifest) {
			writeFile(t, filepath.Join(fx.out, m.Bench.Name), "tampered")
		}, "ruralz_0.1.0_bench.tar.gz does not match its manifest SHA-256"},
		{"missing schema", func(_ *testing.T, fx *fixture, m *manifest) {
			_ = os.Remove(filepath.Join(fx.out, m.Schemas[0].Name))
		}, "ruralz-v1alpha1-authoring.schema.json is missing"},
		{"missing archive SBOM", func(_ *testing.T, fx *fixture, m *manifest) {
			_ = os.Remove(filepath.Join(fx.out, m.Archives[3].SBOM))
		}, "ruralzd_0.1.0_darwin_amd64.tar.gz.cdx.json is missing"},
		{"missing image SBOM", func(_ *testing.T, fx *fixture, m *manifest) {
			_ = os.Remove(filepath.Join(fx.out, m.Images[0].SBOM))
		}, "ruralzd_0.1.0_image.cdx.json is missing"},
		{"missing notes", func(_ *testing.T, fx *fixture, _ *manifest) {
			_ = os.Remove(filepath.Join(fx.out, notesName))
		}, "NOTES.md is missing"},
		{"unexpected archive", func(t *testing.T, fx *fixture, _ *manifest) {
			writeFile(t, filepath.Join(fx.out, "ruralz_0.0.9_linux_amd64.tar.gz"), "old")
		}, "unexpected ruralz_0.0.9_linux_amd64.tar.gz in the release directory"},
		{"directory", func(t *testing.T, fx *fixture, _ *manifest) {
			if err := os.Mkdir(filepath.Join(fx.out, "extra"), 0o750); err != nil {
				t.Fatal(err)
			}
		}, "unexpected extra in the release directory"},
		{"path in manifest", func(t *testing.T, fx *fixture, m *manifest) {
			m.Schemas[0].Name = "../outside.schema.json"
			data, _ := encodeManifest(m)
			writeFile(t, filepath.Join(fx.out, manifestName), string(data))
		}, "is not a file name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t, "0.1.0")
			m := fx.release(t)
			_ = os.Remove(filepath.Join(fx.out, checksumsName))
			c.change(t, fx, m)
			findings, err := runChecksums(fx.out)
			if err != nil {
				t.Fatal(err)
			}
			if !hasFinding(findings, c.want) {
				t.Errorf("findings %q lack %q", findings, c.want)
			}
			if _, err := os.Stat(filepath.Join(fx.out, checksumsName)); !os.IsNotExist(err) {
				t.Error("SHA256SUMS written despite findings")
			}
		})
	}
	if _, err := runChecksums(t.TempDir()); err == nil {
		t.Error("a directory without a manifest is not an error")
	}
}

// m0 is the first archive of a 0.1.0 release.
const m0 = "ruralzd_0.1.0_linux_amd64.tar.gz"
