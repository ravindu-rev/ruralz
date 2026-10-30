// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// manifestFormat identifies the release manifest format.
const manifestFormat = "ruralz.release.v1"

// manifest describes one release directory: what package built, which
// checksums, notes and images belong to it. verify audits the directory
// against it, and the release publishes it, listed in SHA256SUMS.
type manifest struct {
	// Format is manifestFormat.
	Format string `json:"format"`
	// Version is the product version without "v"; Commit the full commit.
	Version string `json:"version"`
	Commit  string `json:"commit"`
	// CommitTime is the commit's time in RFC 3339 UTC, the time of every
	// archive entry.
	CommitTime string `json:"commitTime"`
	// Toolchain is the go.mod toolchain every binary was built with.
	Toolchain string `json:"toolchain"`
	// Archives lists the archives in artifacts() order.
	Archives []archiveInfo `json:"archives"`
	// Schemas lists the JSON Schema assets by name.
	Schemas []schemaInfo `json:"schemas"`
	// Images is the container image plan the release workflow pushes,
	// with the digests the image build recorded.
	Images []imagePlan `json:"images"`
	// Bench is the benchmark records archive.
	Bench benchInfo `json:"bench"`
	// Checksums, Signature and Notes name the other files of the
	// directory.
	Checksums string `json:"checksums"`
	Signature string `json:"signature"`
	Notes     string `json:"notes"`
}

// benchInfo is the benchmark records archive of the manifest: the
// ruralz.bench.v1 records of the release's Latency and soak jobs with the
// histograms and CPU profiles they name.
type benchInfo struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Records counts the records the archive holds.
	Records int `json:"records"`
}

// archiveInfo is one archive of the manifest.
type archiveInfo struct {
	Name    string `json:"name"`
	Binary  string `json:"binary"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Support string `json:"support"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	// SBOM names the archive's CycloneDX SBOM, generated after package.
	SBOM string `json:"sbom"`
	// Entries are the archive's members in stored order.
	Entries []entryInfo `json:"entries"`
}

// Entry types.
const (
	entryFile = "file"
	entryDir  = "dir"
)

// entryInfo is one archive member of the manifest.
type entryInfo struct {
	Path string `json:"path"`
	// Type is "file" or "dir".
	Type string `json:"type"`
	// Mode is the octal permission, such as "0644".
	Mode string `json:"mode"`
	// Size and SHA256 describe a file's content; a directory has neither.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// schemaInfo is one JSON Schema asset: a byte copy of a file under
// api/schema/.
type schemaInfo struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// sha256Hex is the hex SHA-256 of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// entryOf describes a member for the manifest.
func entryOf(m member) entryInfo {
	if m.isDir() {
		return entryInfo{Path: m.Path, Type: entryDir, Mode: fmt.Sprintf("%04o", m.Mode.Perm())}
	}
	return entryInfo{Path: m.Path, Type: entryFile, Mode: fmt.Sprintf("%04o", m.Mode.Perm()), Size: int64(len(m.Data)), SHA256: sha256Hex(m.Data)}
}

// encodeManifest renders a manifest as indented JSON with a final newline.
func encodeManifest(m *manifest) ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// readManifest reads a manifest strictly: one JSON object, no unknown
// member.
func readManifest(path string) (*manifest, error) {
	m, _, err := loadManifest(path)
	return m, err
}

// loadManifest reads a manifest strictly and also returns its bytes, which
// SHA256SUMS covers: the manifest is published with the release.
func loadManifest(path string) (*manifest, []byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The manifest path is the tool's own argument.
	if err != nil {
		return nil, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m manifest
	if err := dec.Decode(&m); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("%s: data after the manifest object", path)
	}
	return &m, data, nil
}

// parseCommitTime parses a manifest's commitTime: RFC 3339 in UTC, whole
// seconds.
func parseCommitTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || !strings.HasSuffix(s, "Z") || t.Nanosecond() != 0 {
		return time.Time{}, fmt.Errorf("commitTime %q is not RFC 3339 UTC in whole seconds", s)
	}
	return t, nil
}

// readToolchain returns the toolchain line of go.mod (such as go1.27.3),
// the Go release every release binary is built with.
func readToolchain(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod")) //nolint:gosec // go.mod of the repository root given on the command line.
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[0] == "toolchain" && regexp.MustCompile(`^go1\.[0-9]+(\.[0-9]+)?$`).MatchString(f[1]) {
			return f[1], nil
		}
	}
	return "", errors.New("go.mod has no toolchain line: release builds pin the newest Go patch with one")
}

// ensureEmptyDir creates dir, or accepts it when it exists and is empty,
// so no stale file of an earlier run survives in a release directory.
func ensureEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.MkdirAll(dir, 0o750)
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("output directory %s is not empty", dir)
	}
	return nil
}
