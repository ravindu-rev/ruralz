// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

// verifyOptions are the flags of `releasekit verify`.
type verifyOptions struct {
	// root is the repository root the archives are compared with.
	root string
	// manifest is the release directory's release-manifest.json.
	manifest string
}

// verifyDeps replace what verify reads besides files in tests.
type verifyDeps struct {
	facts     func(data []byte) (buildFacts, error)
	toolchain string
	artifacts []artifact
}

// sigstoreBundleType prefixes the media type of a Sigstore bundle.
const sigstoreBundleType = "application/vnd.dev.sigstore.bundle"

// audit collects findings.
type audit struct{ findings []string }

func (a *audit) add(format string, args ...any) {
	a.findings = append(a.findings, fmt.Sprintf(format, args...))
}

// runVerify audits a release directory against its manifest and the
// repository (the ADR-0002 release audit of archives): the manifest's
// fields, every archive's bytes, members, timestamps, legal files and
// binary build, every SBOM, the schema assets, SHA256SUMS, the notes and
// the image plan, and no stray file. It returns the findings.
func runVerify(o verifyOptions, d verifyDeps) ([]string, error) {
	m, manifestData, err := loadManifest(o.manifest)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(o.manifest)
	repo, err := readRepoFiles(o.root)
	if err != nil {
		return nil, err
	}
	toolchain := d.toolchain
	if toolchain == "" {
		if toolchain, err = readToolchain(o.root); err != nil {
			return nil, err
		}
	}
	arts := d.artifacts
	if arts == nil {
		arts = artifacts()
	}
	var a audit

	// The manifest's own fields.
	if m.Format != manifestFormat {
		a.add("manifest format %q, want %q", m.Format, manifestFormat)
	}
	if err := checkVersion(m.Version); err != nil {
		a.add("manifest: %v", err)
	}
	if err := checkCommit(m.Commit); err != nil {
		a.add("manifest: %v", err)
	}
	mtime, err := parseCommitTime(m.CommitTime)
	if err != nil {
		a.add("manifest: %v", err)
	}
	if m.Toolchain != toolchain {
		a.add("manifest toolchain %q, want go.mod's %q", m.Toolchain, toolchain)
	}
	if m.Checksums != checksumsName || m.Signature != signatureName || m.Notes != notesName {
		a.add("manifest names checksums %q, signature %q and notes %q, want %q, %q and %q",
			m.Checksums, m.Signature, m.Notes, checksumsName, signatureName, notesName)
	}
	if want := images(m.Version); !reflect.DeepEqual(m.Images, want) {
		a.add("manifest image plan %s, want %s", jsonText(m.Images), jsonText(want))
	}

	// Every archive the release must have, and no other.
	var wantNames, gotNames []string
	for _, art := range arts {
		wantNames = append(wantNames, art.archiveName(m.Version))
	}
	for _, info := range m.Archives {
		gotNames = append(gotNames, info.Name)
	}
	if !slices.Equal(gotNames, wantNames) {
		a.add("manifest archives %v, want %v", gotNames, wantNames)
	}
	for _, info := range m.Archives {
		i := slices.Index(wantNames, info.Name)
		if i < 0 {
			continue
		}
		if err := verifyArchive(&a, dir, info, arts[i], m, mtime, toolchain, repo, d); err != nil {
			return nil, err
		}
	}

	// Schema assets: byte copies of every file under api/schema/ruralz/.
	sources, err := schemaSources(o.root)
	if err != nil {
		return nil, err
	}
	var wantSchemas, gotSchemas []string
	for _, s := range sources {
		wantSchemas = append(wantSchemas, s[0]+" "+s[1])
	}
	for _, s := range m.Schemas {
		gotSchemas = append(gotSchemas, s.Name+" "+s.Source)
	}
	if !slices.Equal(gotSchemas, wantSchemas) {
		a.add("manifest schemas %v, want %v", gotSchemas, wantSchemas)
	}
	for _, s := range m.Schemas {
		// Only expected names and sources are read, so a manifest cannot
		// point the audit outside the release directory.
		if !slices.Contains(wantSchemas, s.Name+" "+s.Source) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, s.Name)) //nolint:gosec // An expected schema asset of the release directory.
		if err != nil {
			a.add("schema %s: %v", s.Name, err)
			continue
		}
		if int64(len(data)) != s.Size || sha256Hex(data) != s.SHA256 {
			a.add("schema %s does not match its manifest size and SHA-256", s.Name)
		}
		src, err := os.ReadFile(filepath.Join(o.root, filepath.FromSlash(s.Source)))
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(data, src) {
			a.add("schema %s is not a byte copy of %s", s.Name, s.Source)
		}
	}

	// SHA256SUMS over the archives and schema files.
	sums, err := os.ReadFile(filepath.Join(dir, checksumsName)) //nolint:gosec // A file of the release directory.
	switch {
	case errors.Is(err, fs.ErrNotExist):
		a.add("%s is missing (releasekit checksums)", checksumsName)
	case err != nil:
		return nil, err
	case !bytes.Equal(sums, checksumsText(m, sha256Hex(manifestData))):
		a.add("%s does not list exactly the archives, the schema files and %s with their SHA-256", checksumsName, manifestName)
	}

	// The release notes.
	notes, err := os.ReadFile(filepath.Join(dir, notesName)) //nolint:gosec // A file of the release directory.
	switch {
	case errors.Is(err, fs.ErrNotExist):
		a.add("%s is missing (releasekit notes)", notesName)
	case err != nil:
		return nil, err
	default:
		first, _, _ := strings.Cut(string(notes), "\n")
		if first != notesTitle(m.Version) {
			a.add("%s starts with %q, want %q", notesName, first, notesTitle(m.Version))
		}
	}

	// The signature exists only after `make release`; when present it is a
	// Sigstore bundle.
	if sig, err := os.ReadFile(filepath.Join(dir, signatureName)); err == nil { //nolint:gosec // A file of the release directory.
		var bundle struct {
			MediaType string `json:"mediaType"`
		}
		if json.Unmarshal(sig, &bundle) != nil || !strings.HasPrefix(bundle.MediaType, sigstoreBundleType) {
			a.add("%s is not a Sigstore bundle", signatureName)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	// Nothing else.
	allowed := []string{manifestName, checksumsName, signatureName, notesName}
	for _, info := range m.Archives {
		allowed = append(allowed, info.Name, info.SBOM)
	}
	for _, s := range m.Schemas {
		allowed = append(allowed, s.Name)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !slices.Contains(allowed, e.Name()) || !e.Type().IsRegular() {
			a.add("unexpected %s in the release directory", e.Name())
		}
	}
	return a.findings, nil
}

// verifyArchive audits one archive against its manifest entry, the
// artifact registry and the repository.
func verifyArchive(a *audit, dir string, info archiveInfo, art artifact, m *manifest, mtime time.Time, toolchain string, repo repoFiles, d verifyDeps) error {
	name := info.Name
	if info.Binary != art.Binary || info.OS != art.OS || info.Arch != art.Arch || info.Support != art.Support || info.SBOM != name+sbomSuffix {
		a.add("%s: manifest says %s %s/%s %s with SBOM %s, want %s %s/%s %s with SBOM %s", name,
			info.Binary, info.OS, info.Arch, info.Support, info.SBOM, art.Binary, art.OS, art.Arch, art.Support, name+sbomSuffix)
	}
	data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // An archive of the release directory named by the manifest.
	if errors.Is(err, fs.ErrNotExist) {
		a.add("%s is missing", name)
		return nil
	}
	if err != nil {
		return err
	}
	if int64(len(data)) != info.Size || sha256Hex(data) != info.SHA256 {
		a.add("%s does not match its manifest size and SHA-256", name)
	}

	var members []stored
	if art.OS == "windows" {
		members, err = readZip(data)
	} else {
		var gz gzipFacts
		members, gz, err = readTarGz(data)
		if err == nil && (gz.Name != "" || !gz.ModTime.IsZero()) {
			a.add("%s: gzip header carries a name or time", name)
		}
	}
	if err != nil {
		a.add("%s: unreadable archive: %v", name, err)
		return nil
	}

	// Deterministic layout: sorted unique paths, files and directories
	// only, the commit time, owner and group 0 without names.
	var got []entryInfo
	var paths []string
	for _, s := range members {
		paths = append(paths, s.Path)
		switch {
		case !s.Regular && !s.Dir:
			a.add("%s: %s is neither a file nor a directory", name, s.Path)
		case s.Dir != s.isDir():
			a.add("%s: %s has a type its path does not show", name, s.Path)
		}
		if !mtime.IsZero() && !s.ModTime.Equal(mtime) {
			a.add("%s: %s is dated %s, want the commit time %s", name, s.Path, s.ModTime.UTC().Format(time.RFC3339), mtime.Format(time.RFC3339))
		}
		if s.UID != 0 || s.GID != 0 || s.Uname != "" || s.Gname != "" {
			a.add("%s: %s has owner %d:%d (%q:%q), want 0:0 without names", name, s.Path, s.UID, s.GID, s.Uname, s.Gname)
		}
		got = append(got, entryOf(s.member))
	}
	if !slices.IsSorted(paths) || len(slices.Compact(slices.Clone(paths))) != len(paths) {
		a.add("%s: members are not sorted and unique: %v", name, paths)
	}
	if !reflect.DeepEqual(got, info.Entries) {
		a.add("%s: members differ from the manifest", name)
	}

	// Exactly the members the artifact carries, with the repository's
	// LICENSE, README.md and deploy/systemd/.
	byPath := map[string]stored{}
	for _, s := range members {
		byPath[s.Path] = s
	}
	want := archiveMembers(art, nil, nil, nil, repo)
	var wantPaths []string
	for _, w := range want {
		wantPaths = append(wantPaths, w.Path)
		s, ok := byPath[w.Path]
		switch {
		case !ok:
			a.add("%s: missing %s", name, w.Path)
			continue
		case s.Mode != w.Mode:
			a.add("%s: %s has mode %04o, want %04o", name, w.Path, s.Mode, w.Mode)
		}
		if w.Data != nil && !bytes.Equal(s.Data, w.Data) {
			a.add("%s: %s differs from the repository's", name, w.Path)
		}
	}
	for _, p := range paths {
		if !slices.Contains(wantPaths, p) {
			a.add("%s: unexpected member %s", name, p)
		}
	}

	bin, ok := byPath[art.executable()]
	if !ok {
		return nil
	}
	facts, err := d.facts(bin.Data)
	if err != nil {
		a.add("%s: %s: %v", name, art.executable(), err)
		return nil
	}
	for _, p := range buildProblems(facts, art, m.Version, m.Commit, toolchain) {
		a.add("%s: %s", name, p)
	}
	for _, p := range legalProblems(byPath[noticeMember].Data, byPath[thirdPartyMember].Data, repo.notice, facts.Info) {
		a.add("%s: %s", name, p)
	}
	problems, err := sbomProblems(filepath.Join(dir, name+sbomSuffix), facts.Info)
	if err != nil {
		return err
	}
	for _, p := range problems {
		a.add("%s: %s", name, p)
	}
	return nil
}

// checksumsText renders SHA256SUMS in `sha256sum` format: one
// "<hex>  <name>" line per archive, schema file and the manifest, whose
// digest manifestSum is, sorted by name.
func checksumsText(m *manifest, manifestSum string) []byte {
	type line struct{ name, sum string }
	lines := []line{{manifestName, manifestSum}}
	for _, info := range m.Archives {
		lines = append(lines, line{info.Name, info.SHA256})
	}
	for _, s := range m.Schemas {
		lines = append(lines, line{s.Name, s.SHA256})
	}
	slices.SortFunc(lines, func(x, y line) int { return strings.Compare(x.name, y.name) })
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l.sum + "  " + l.name + "\n")
	}
	return b.Bytes()
}

// jsonText renders v as compact JSON for a finding.
func jsonText(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(data)
}
