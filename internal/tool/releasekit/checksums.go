// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
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

// runChecksums writes SHA256SUMS for the archives and schema files the
// release directory's manifest lists and for the manifest itself, which
// the release publishes, after checking each file against its manifest
// SHA-256 and that no other archive or schema file is in the directory.
// It returns the findings; with any, nothing is written.
func runChecksums(dir string) ([]string, error) {
	m, manifestData, err := loadManifest(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	var a audit
	listed := map[string]string{}
	for _, info := range m.Archives {
		listed[info.Name] = info.SHA256
	}
	for _, s := range m.Schemas {
		listed[s.Name] = s.SHA256
	}
	names := make([]string, 0, len(listed))
	for name := range listed {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !baseName(name) {
			a.add("manifest names %q, which is not a file name", name)
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // A file of the release directory named by its manifest.
		switch {
		case errors.Is(err, fs.ErrNotExist):
			a.add("%s is listed in the manifest but missing", name)
		case err != nil:
			return nil, err
		case sha256Hex(data) != listed[name]:
			a.add("%s does not match its manifest SHA-256", name)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		n := e.Name()
		release := strings.HasSuffix(n, ".tar.gz") || strings.HasSuffix(n, ".zip") || strings.HasSuffix(n, ".schema.json")
		if _, ok := listed[n]; release && !ok {
			a.add("%s is not in the manifest", n)
		}
	}
	if len(a.findings) > 0 {
		return a.findings, nil
	}
	if err := os.WriteFile(filepath.Join(dir, checksumsName), checksumsText(m, sha256Hex(manifestData)), 0o600); err != nil {
		return nil, fmt.Errorf("writing %s: %w", checksumsName, err)
	}
	return nil, nil
}
