// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// TestVerifyRelease: a complete release directory passes the audit, with
// or without its Sigstore bundle.
func TestVerifyRelease(t *testing.T) {
	for _, version := range []string{"0.1.0", "0.1.0-rc.2", "0.0.0-dev"} {
		fx := newFixture(t, version)
		fx.release(t)
		if got := fx.verify(t); len(got) != 0 {
			t.Errorf("%s: findings %q", version, got)
		}
		writeFile(t, filepath.Join(fx.out, signatureName), `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)
		if got := fx.verify(t); len(got) != 0 {
			t.Errorf("%s with a signature: findings %q", version, got)
		}
	}
}

// rewriteManifest applies change to the release manifest on disk.
func rewriteManifest(t *testing.T, fx *fixture, change func(*manifest)) {
	t.Helper()
	p := filepath.Join(fx.out, manifestName)
	m, err := readManifest(p)
	if err != nil {
		t.Fatal(err)
	}
	change(m)
	data, err := encodeManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, string(data))
}

// TestVerifyFindings: each way a release directory can drift from its
// manifest, the repository or the release rules is its own finding.
func TestVerifyFindings(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, fx *fixture)
		want   string
	}{
		{"archive bytes", func(t *testing.T, fx *fixture) {
			writeFile(t, filepath.Join(fx.out, "ruralz_0.1.0_linux_arm64.tar.gz"), "other bytes")
		}, "ruralz_0.1.0_linux_arm64.tar.gz does not match its manifest size and SHA-256"},
		{"archive missing", func(_ *testing.T, fx *fixture) {
			_ = os.Remove(filepath.Join(fx.out, "ruralz_0.1.0_windows_amd64.zip"))
		}, "ruralz_0.1.0_windows_amd64.zip is missing"},
		{"repository LICENSE", func(t *testing.T, fx *fixture) {
			writeFile(t, filepath.Join(fx.root, "LICENSE"), "changed\n")
		}, "LICENSE differs from the repository's"},
		{"repository unit", func(t *testing.T, fx *fixture) {
			writeFile(t, filepath.Join(fx.root, "deploy", "systemd", "ruralzd.service"), "[Service]\nType=simple\n")
		}, "ruralzd_0.1.0_linux_amd64.tar.gz: deploy/systemd/ruralzd.service differs from the repository's"},
		{"repository sysusers", func(t *testing.T, fx *fixture) {
			writeFile(t, filepath.Join(fx.root, "deploy", "systemd", "ruralzd.sysusers"), "u ruralz 990 \"Ruralz Gateway\" /var/lib/ruralz\n")
		}, "ruralzd_0.1.0_linux_arm64.tar.gz: deploy/systemd/ruralzd.sysusers differs from the repository's"},
		{"SBOM missing", func(_ *testing.T, fx *fixture) {
			_ = os.Remove(filepath.Join(fx.out, "ruralzd_0.1.0_darwin_arm64.tar.gz"+sbomSuffix))
		}, "ruralzd_0.1.0_darwin_arm64.tar.gz.cdx.json is missing"},
		{"SBOM without the modules", func(t *testing.T, fx *fixture) {
			writeFile(t, filepath.Join(fx.out, "ruralz_0.1.0_darwin_amd64.tar.gz"+sbomSuffix), sbomFor(nil))
		}, "lacks example.com/Upper/mod v1.2.3+incompatible"},
		{"SHA256SUMS missing", func(_ *testing.T, fx *fixture) {
			_ = os.Remove(filepath.Join(fx.out, checksumsName))
		}, "SHA256SUMS is missing"},
		{"SHA256SUMS altered", func(t *testing.T, fx *fixture) {
			data := readBack(t, filepath.Join(fx.out, checksumsName))
			writeFile(t, filepath.Join(fx.out, checksumsName), string(data)+strings.Repeat("0", 64)+"  extra.zip\n")
		}, "SHA256SUMS does not list exactly"},
		// The manifest is published and listed in SHA256SUMS: even an edit
		// that keeps its content, such as compact JSON, is found.
		{"manifest reformatted", func(t *testing.T, fx *fixture) {
			m, err := readManifest(filepath.Join(fx.out, manifestName))
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(fx.out, manifestName), string(data)+"\n")
		}, "SHA256SUMS does not list exactly the archives, the schema files and release-manifest.json"},
		{"notes missing", func(_ *testing.T, fx *fixture) {
			_ = os.Remove(filepath.Join(fx.out, notesName))
		}, "NOTES.md is missing"},
		{"notes title", func(t *testing.T, fx *fixture) {
			writeFile(t, filepath.Join(fx.out, notesName), "# Ruralz 0.0.1\n")
		}, `NOTES.md starts with "# Ruralz 0.0.1", want "# Ruralz 0.1.0"`},
		{"stray file", func(t *testing.T, fx *fixture) {
			writeFile(t, filepath.Join(fx.out, "extra.txt"), "x")
		}, "unexpected extra.txt in the release directory"},
		{"signature", func(t *testing.T, fx *fixture) {
			writeFile(t, filepath.Join(fx.out, signatureName), "{}")
		}, "SHA256SUMS.sigstore.json is not a Sigstore bundle"},
		{"schema asset", func(t *testing.T, fx *fixture) {
			writeFile(t, filepath.Join(fx.out, "ruralz-v1alpha1-rendered.schema.json"), "{}\n")
		}, "schema ruralz-v1alpha1-rendered.schema.json does not match its manifest size and SHA-256"},
		{"schema source", func(t *testing.T, fx *fixture) {
			writeFile(t, filepath.Join(fx.root, "api", "schema", "ruralz", "v1alpha1", "authoring.schema.json"), "{}\n")
		}, "schema ruralz-v1alpha1-authoring.schema.json is not a byte copy of api/schema/ruralz/v1alpha1/authoring.schema.json"},
		{"go.mod toolchain", func(t *testing.T, fx *fixture) {
			writeFile(t, filepath.Join(fx.root, "go.mod"), "module m\n\ngo 1.26.0\n\ntoolchain go1.27.4\n")
		}, `manifest toolchain "go1.27.3", want go.mod's "go1.27.4"`},
		{"image plan", func(t *testing.T, fx *fixture) {
			rewriteManifest(t, fx, func(m *manifest) { m.Images[0].Tags = append(m.Images[0].Tags, "latest") })
		}, "manifest image plan"},
		{"archive list", func(t *testing.T, fx *fixture) {
			rewriteManifest(t, fx, func(m *manifest) { m.Archives = m.Archives[:8] })
		}, "manifest archives"},
		{"archive metadata", func(t *testing.T, fx *fixture) {
			rewriteManifest(t, fx, func(m *manifest) { m.Archives[2].Support = supportProduction })
		}, "ruralzd_0.1.0_darwin_arm64.tar.gz: manifest says ruralzd darwin/arm64 production"},
		{"commit time", func(t *testing.T, fx *fixture) {
			rewriteManifest(t, fx, func(m *manifest) { m.CommitTime = "2026-10-02T00:00:00Z" })
		}, "want the commit time 2026-10-02T00:00:00Z"},
		{"manifest format", func(t *testing.T, fx *fixture) {
			rewriteManifest(t, fx, func(m *manifest) { m.Format = "ruralz.release.v0" })
		}, `manifest format "ruralz.release.v0"`},
		{"manifest version", func(t *testing.T, fx *fixture) {
			rewriteManifest(t, fx, func(m *manifest) { m.Version = "v0.1.0" })
		}, "manifest: version \"v0.1.0\""},
		{"binary build", func(_ *testing.T, fx *fixture) {
			fx.tweak["ruralz_linux_amd64"] = func(f *buildFacts) { f.Stripped = false }
		}, "ruralz_0.1.0_linux_amd64.tar.gz: symbol table or DWARF present"},
		{"unlisted module", func(_ *testing.T, fx *fixture) {
			fx.tweak["ruralzd_linux_amd64"] = func(f *buildFacts) {
				f.Info.Deps = append(f.Info.Deps, &debug.Module{Path: "example.com/late", Version: "v0.1.0"})
			}
		}, "ruralzd_0.1.0_linux_amd64.tar.gz: THIRD_PARTY_LICENSES has no section for example.com/late v0.1.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t, "0.1.0")
			fx.release(t)
			c.change(t, fx)
			if got := fx.verify(t); !hasFinding(got, c.want) {
				t.Errorf("findings %q\nlack %q", got, c.want)
			}
		})
	}
}

// craft replaces the first archive of the fixture's release (ruralzd for
// linux/amd64) with a gzip-compressed tar written by write, and records
// its new size and digest in the manifest.
func craft(t *testing.T, fx *fixture, gzName string, write func(tw *tar.Writer, members []stored)) {
	t.Helper()
	path := filepath.Join(fx.out, artifacts()[0].archiveName(fx.version))
	members, _, err := readTarGz(readBack(t, path))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Name = gzName
	tw := tar.NewWriter(gz)
	write(tw, members)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, buf.String())
	rewriteManifest(t, fx, func(m *manifest) {
		m.Archives[0].Size, m.Archives[0].SHA256 = int64(buf.Len()), sha256Hex(buf.Bytes())
	})
}

// put writes one stored member with its own metadata, changed by edit.
func put(t *testing.T, tw *tar.Writer, s stored, edit func(*tar.Header)) {
	t.Helper()
	h := &tar.Header{Name: s.Path, Mode: int64(s.Mode), ModTime: s.ModTime, Typeflag: tar.TypeReg, Size: int64(len(s.Data))}
	if s.Dir {
		h.Typeflag, h.Size = tar.TypeDir, 0
	}
	if edit != nil {
		edit(h)
	}
	if err := tw.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if h.Typeflag == tar.TypeReg {
		if _, err := tw.Write(s.Data); err != nil {
			t.Fatal(err)
		}
	}
}

// TestVerifyArchiveLayout: unsorted members, an owner, a gzip name, a link
// or an extra member each break the deterministic layout the audit checks.
func TestVerifyArchiveLayout(t *testing.T) {
	cases := []struct {
		name   string
		gzName string
		write  func(t *testing.T, tw *tar.Writer, ms []stored)
		want   string
	}{
		{"unsorted", "", func(t *testing.T, tw *tar.Writer, ms []stored) {
			for i := len(ms) - 1; i >= 0; i-- {
				put(t, tw, ms[i], nil)
			}
		}, "members are not sorted and unique"},
		{"owner", "", func(t *testing.T, tw *tar.Writer, ms []stored) {
			for _, s := range ms {
				put(t, tw, s, func(h *tar.Header) { h.Uid, h.Uname = 1000, "builder" })
			}
		}, `has owner 1000:0 ("builder":"")`},
		{"gzip name", "ruralzd.tar", func(t *testing.T, tw *tar.Writer, ms []stored) {
			for _, s := range ms {
				put(t, tw, s, nil)
			}
		}, "gzip header carries a name or time"},
		{"link", "", func(t *testing.T, tw *tar.Writer, ms []stored) {
			for _, s := range ms {
				put(t, tw, s, nil)
			}
			put(t, tw, stored{member: member{Path: "zlink", Mode: modeFile}}, func(h *tar.Header) {
				h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, "ruralzd", 0
			})
		}, "zlink is neither a file nor a directory"},
		{"extra member", "", func(t *testing.T, tw *tar.Writer, ms []stored) {
			for _, s := range ms {
				put(t, tw, s, nil)
			}
			put(t, tw, stored{member: member{Path: "zz-extra", Mode: modeFile, Data: []byte("x")}, ModTime: fixtureTime()}, nil)
		}, "unexpected member zz-extra"},
		{"mode", "", func(t *testing.T, tw *tar.Writer, ms []stored) {
			for _, s := range ms {
				put(t, tw, s, func(h *tar.Header) {
					if h.Name == "ruralzd" {
						h.Mode = 0o700
					}
				})
			}
		}, "ruralzd has mode 0700, want 0755"},
		{"without the sysusers entry", "", func(t *testing.T, tw *tar.Writer, ms []stored) {
			for _, s := range ms {
				if s.Path != "deploy/systemd/ruralzd.sysusers" {
					put(t, tw, s, nil)
				}
			}
		}, "ruralzd_0.1.0_linux_amd64.tar.gz: missing deploy/systemd/ruralzd.sysusers"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t, "0.1.0")
			fx.release(t)
			craft(t, fx, c.gzName, func(tw *tar.Writer, ms []stored) { c.write(t, tw, ms) })
			if got := fx.verify(t); !hasFinding(got, c.want) {
				t.Errorf("findings %q\nlack %q", got, c.want)
			}
		})
	}
}

// TestVerifyInputErrors: an unreadable or malformed manifest is an error,
// not a finding.
func TestVerifyInputErrors(t *testing.T) {
	fx := newFixture(t, "0.1.0")
	fx.release(t)
	p := filepath.Join(fx.out, manifestName)
	for name, content := range map[string]string{
		"unknown member": `{"format":"ruralz.release.v1","extra":true}`,
		"two objects":    `{"format":"ruralz.release.v1"}{}`,
		"not JSON":       `format: ruralz.release.v1`,
	} {
		writeFile(t, p, content)
		if _, err := runVerify(verifyOptions{root: fx.root, manifest: p}, verifyDeps{facts: fx.facts()}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := runVerify(verifyOptions{root: fx.root, manifest: filepath.Join(fx.out, "absent.json")}, verifyDeps{facts: fx.facts()}); err == nil {
		t.Error("missing manifest: no error")
	}
}
