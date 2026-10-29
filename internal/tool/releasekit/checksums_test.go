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

// TestChecksums: SHA256SUMS lists every archive, every schema file and the
// published release-manifest.json, and only those, as sorted
// "<sha256>  <name>" lines that sha256sum -c reads.
func TestChecksums(t *testing.T) {
	fx := newFixture(t, "0.1.0")
	m, err := runPackage(t.Context(), fx.options(), fx.pkgDeps())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := runChecksums(fx.out)
	if err != nil || len(findings) > 0 {
		t.Fatalf("runChecksums: %q %v", findings, err)
	}
	data, err := os.ReadFile(filepath.Join(fx.out, checksumsName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if want := len(m.Archives) + len(m.Schemas) + 1; len(lines) != want {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), want, data)
	}
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
		if strings.HasSuffix(name, sbomSuffix) || name == notesName {
			t.Errorf("SHA256SUMS lists %s", name)
		}
		names = append(names, name)
	}
	if !slices.IsSorted(names) {
		t.Errorf("names not sorted: %v", names)
	}
	if !slices.Contains(names, manifestName) {
		t.Errorf("SHA256SUMS does not list %s: %v", manifestName, names)
	}
}

// TestChecksumsFindings: a changed, missing or unlisted release file is a
// finding, and SHA256SUMS is not written.
func TestChecksumsFindings(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, fx *fixture, m *manifest)
		want   string
	}{
		{"changed archive", func(t *testing.T, fx *fixture, m *manifest) {
			writeFile(t, filepath.Join(fx.out, m.Archives[0].Name), "tampered")
		}, "does not match its manifest SHA-256"},
		{"missing schema", func(_ *testing.T, fx *fixture, m *manifest) {
			_ = os.Remove(filepath.Join(fx.out, m.Schemas[0].Name))
		}, "listed in the manifest but missing"},
		{"unlisted archive", func(t *testing.T, fx *fixture, _ *manifest) {
			writeFile(t, filepath.Join(fx.out, "ruralz_0.0.9_linux_amd64.tar.gz"), "old")
		}, "ruralz_0.0.9_linux_amd64.tar.gz is not in the manifest"},
		{"path in manifest", func(t *testing.T, fx *fixture, m *manifest) {
			m.Schemas[0].Name = "../outside.schema.json"
			data, _ := encodeManifest(m)
			writeFile(t, filepath.Join(fx.out, manifestName), string(data))
		}, "is not a file name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t, "0.1.0")
			m, err := runPackage(t.Context(), fx.options(), fx.pkgDeps())
			if err != nil {
				t.Fatal(err)
			}
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
