// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// baseName reports whether name is a plain file name, never a path.
func baseName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && filepath.Base(name) == name
}

// releaseFile is one file SHA256SUMS covers: its name, and the SHA-256
// the manifest records for it, empty for a file the manifest only names
// (an SBOM, the notes), which is hashed as found.
type releaseFile struct {
	name, sha256 string
}

// releaseFiles lists every file of a release directory that SHA256SUMS
// covers, so its one signature covers every asset: the archives and their
// SBOMs, the image SBOM when the build recorded the image, the schema
// files, release-manifest.json, NOTES.md and the benchmark records archive
// (docs/engineering/04-release-versioning-and-compatibility.md, "Release
// artifacts"). The manifest's own digest is filled by the caller.
func releaseFiles(m *manifest) []releaseFile {
	out := []releaseFile{{name: manifestName}, {name: notesName}, {name: m.Bench.Name, sha256: m.Bench.SHA256}}
	for _, a := range m.Archives {
		out = append(out, releaseFile{name: a.Name, sha256: a.SHA256}, releaseFile{name: a.SBOM})
	}
	for _, s := range m.Schemas {
		out = append(out, releaseFile{name: s.Name, sha256: s.SHA256})
	}
	for _, img := range m.Images {
		if img.recorded() {
			out = append(out, releaseFile{name: img.SBOM})
		}
	}
	slices.SortFunc(out, func(a, b releaseFile) int { return strings.Compare(a.name, b.name) })
	return out
}

// checksumLines hashes the release files of dir and renders SHA256SUMS in
// `sha256sum` format: one "<hex>  <name>" line per file, sorted by name.
// A file whose manifest digest does not match, a missing file or a name
// that is not a file name is a finding.
func checksumLines(dir string, m *manifest, manifestData []byte) ([]byte, []string, error) {
	var a audit
	var b bytes.Buffer
	for _, f := range releaseFiles(m) {
		if !baseName(f.name) {
			a.add("manifest names %q, which is not a file name", f.name)
			continue
		}
		data := manifestData
		if f.name != manifestName {
			var err error
			data, err = os.ReadFile(filepath.Join(dir, f.name)) //nolint:gosec // A file of the release directory named by its manifest.
			switch {
			case errors.Is(err, fs.ErrNotExist):
				a.add("%s is missing", f.name)
				continue
			case err != nil:
				return nil, nil, err
			}
		}
		sum := sha256Hex(data)
		if f.sha256 != "" && sum != f.sha256 {
			a.add("%s does not match its manifest SHA-256", f.name)
		}
		b.WriteString(sum + "  " + f.name + "\n")
	}
	return b.Bytes(), a.findings, nil
}

// strayFiles reports the files of dir that are neither a release file nor
// SHA256SUMS or its signature.
func strayFiles(dir string, m *manifest) ([]string, error) {
	allowed := []string{checksumsName, signatureName}
	for _, f := range releaseFiles(m) {
		allowed = append(allowed, f.name)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !slices.Contains(allowed, e.Name()) || !e.Type().IsRegular() {
			out = append(out, fmt.Sprintf("unexpected %s in the release directory", e.Name()))
		}
	}
	return out, nil
}

// runChecksums writes SHA256SUMS over every release file of dir, after
// checking each against its manifest digest, that the SBOMs and NOTES.md
// exist and that no other file is in the directory. It returns the
// findings; with any, nothing is written.
func runChecksums(dir string) ([]string, error) {
	m, manifestData, err := loadManifest(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	text, findings, err := checksumLines(dir, m, manifestData)
	if err != nil {
		return nil, err
	}
	stray, err := strayFiles(dir, m)
	if err != nil {
		return nil, err
	}
	findings = append(findings, stray...)
	if len(findings) > 0 {
		return findings, nil
	}
	if err := os.WriteFile(filepath.Join(dir, checksumsName), text, 0o600); err != nil {
		return nil, fmt.Errorf("writing %s: %w", checksumsName, err)
	}
	return nil, nil
}
