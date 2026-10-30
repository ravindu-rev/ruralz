// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// packageOptions are the flags of `releasekit package`.
type packageOptions struct {
	// root is the repository root: LICENSE, README.md, deploy/systemd/,
	// api/schema/ and go.mod.
	root string
	// dist holds <os>_<arch>/<binary> from `make build` and the dry run.
	dist string
	// notices holds <binary>_<os>_<arch>/{NOTICE,THIRD_PARTY_LICENSES}
	// from `depgate -notices`.
	notices string
	// out receives the release directory; absent or empty.
	out string
	// version and commit are the product version and full commit.
	version, commit string
	// image is the OCI layout of the image build, whose digests the
	// manifest records; empty when the build made no image, which only a
	// development build may lack.
	image string
	// bench holds the ruralz.bench.v1 records of the release's Latency
	// and soak jobs; empty or missing gives an empty records archive.
	bench string
}

// packageDeps are what package reads besides files; tests replace them.
type packageDeps struct {
	git   gitRunner
	facts func(data []byte) (buildFacts, error)
	// toolchain, when set, replaces go.mod's toolchain line.
	toolchain string
	// artifacts, when set, replaces artifacts().
	artifacts []artifact
}

// findingsError carries audit findings, which exit 1, apart from usage and
// I/O errors, which exit 2.
type findingsError struct{ findings []string }

func (e *findingsError) Error() string {
	return fmt.Sprintf("%d finding(s):\n%s", len(e.findings), strings.Join(e.findings, "\n"))
}

// repoFiles are the repository files every archive carries, and the
// repository NOTICE every artifact NOTICE starts with.
type repoFiles struct {
	license, readme, notice []byte
	// systemd are deploy/systemd/ files, sorted by name.
	systemd []member
}

// readRepoFiles reads LICENSE, README.md, NOTICE and deploy/systemd/.
func readRepoFiles(root string) (repoFiles, error) {
	var r repoFiles
	var err error
	if r.license, err = os.ReadFile(filepath.Join(root, licenseMember)); err != nil { //nolint:gosec // A repository file under the root given on the command line.
		return r, err
	}
	if r.notice, err = os.ReadFile(filepath.Join(root, noticeMember)); err != nil { //nolint:gosec // A repository file under the root given on the command line.
		return r, err
	}
	if r.readme, err = os.ReadFile(filepath.Join(root, readmeMember)); err != nil { //nolint:gosec // A repository file under the root given on the command line.
		return r, err
	}
	// deploy/systemd holds exactly systemdFiles(), so an archive never
	// ships a stray file or lacks one of the T5 table.
	dir := filepath.Join(root, filepath.FromSlash(systemdDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return r, err
	}
	for _, e := range entries {
		if !slices.Contains(systemdFiles(), e.Name()) || !e.Type().IsRegular() {
			return r, fmt.Errorf("%s/%s is not one of the shipped files %s", systemdDir, e.Name(), strings.Join(systemdFiles(), ", "))
		}
	}
	for _, name := range systemdFiles() {
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // A shipped file of the repository root given on the command line.
		if err != nil {
			return r, err
		}
		r.systemd = append(r.systemd, member{Path: path.Join(systemdDir, name), Mode: scriptMode(data), Data: data})
	}
	sortMembers(r.systemd)
	return r, nil
}

// scriptMode is 0755 for a file starting with "#!" and 0644 otherwise, so
// archive modes never depend on the build host's file system.
func scriptMode(data []byte) fs.FileMode {
	if bytes.HasPrefix(data, []byte("#!")) {
		return modeExec
	}
	return modeFile
}

// archiveMembers assembles the members of one archive: the binary, the
// legal files and README.md, and deploy/systemd/ for linux ruralzd.
func archiveMembers(a artifact, binary, notice, thirdParty []byte, repo repoFiles) []member {
	ms := []member{
		{Path: a.executable(), Mode: modeExec, Data: binary},
		{Path: licenseMember, Mode: modeFile, Data: repo.license},
		{Path: noticeMember, Mode: modeFile, Data: notice},
		{Path: readmeMember, Mode: modeFile, Data: repo.readme},
		{Path: thirdPartyMember, Mode: modeFile, Data: thirdParty},
	}
	if a.withSystemd() {
		ms = append(ms, member{Path: "deploy/", Mode: modeDir}, member{Path: systemdDir + "/", Mode: modeDir})
		ms = append(ms, repo.systemd...)
	}
	sortMembers(ms)
	return ms
}

// schemaSources lists the JSON Schema files under api/schema/ruralz/ as
// (asset name, slash source path): api/schema/ruralz/<apiversion>/<view>.schema.json
// ships as ruralz-<apiversion>-<view>.schema.json.
func schemaSources(root string) ([][2]string, error) {
	base := filepath.Join(root, "api", "schema", "ruralz")
	versions, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var out [][2]string
	for _, v := range versions {
		if !v.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(base, v.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			view, ok := strings.CutSuffix(f.Name(), ".schema.json")
			if !ok || !f.Type().IsRegular() {
				continue
			}
			out = append(out, [2]string{
				fmt.Sprintf("ruralz-%s-%s.schema.json", v.Name(), view),
				path.Join("api/schema/ruralz", v.Name(), f.Name()),
			})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no JSON Schema under api/schema/ruralz")
	}
	slices.SortFunc(out, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	return out, nil
}

// runPackage builds the release directory: one deterministic archive per
// artifact, the JSON Schema assets, the benchmark records archive and
// release-manifest.json with the image digests. A binary that breaks the
// release build rules is a finding; nothing is packaged for it. A release
// or candidate without an image is refused.
func runPackage(ctx context.Context, o packageOptions, d packageDeps) (*manifest, error) {
	if err := checkVersion(o.version); err != nil {
		return nil, err
	}
	if err := checkCommit(o.commit); err != nil {
		return nil, err
	}
	plans := images(o.version)
	if o.image == "" {
		if k := versionKind(o.version); k != kindDev {
			return nil, &findingsError{[]string{fmt.Sprintf("the %s %s has no image: build it with Docker (make release-dry-run builds it when Docker is present)", k, o.version)}}
		}
	} else {
		img, ordered, err := layoutImage(o.image)
		if err != nil {
			return nil, err
		}
		plans[0] = plans[0].withDigests(img.Digest, ordered)
	}
	toolchain := d.toolchain
	if toolchain == "" {
		var err error
		if toolchain, err = readToolchain(o.root); err != nil {
			return nil, err
		}
	}
	arts := d.artifacts
	if arts == nil {
		arts = artifacts()
	}
	mtime, err := commitTime(ctx, d.git, o.root, o.commit)
	if err != nil {
		return nil, err
	}
	repo, err := readRepoFiles(o.root)
	if err != nil {
		return nil, err
	}
	if err := ensureEmptyDir(o.out); err != nil {
		return nil, err
	}

	m := &manifest{
		Format: manifestFormat, Version: o.version, Commit: o.commit,
		CommitTime: mtime.Format(time.RFC3339), Toolchain: toolchain,
		Images: plans, Checksums: checksumsName, Signature: signatureName, Notes: notesName,
	}
	var findings []string
	for _, a := range arts {
		info, problems, err := packageArtifact(o, d, a, toolchain, mtime, repo)
		if err != nil {
			return nil, err
		}
		for _, p := range problems {
			findings = append(findings, fmt.Sprintf("%s: %s", a.target(), p))
		}
		if len(problems) == 0 {
			m.Archives = append(m.Archives, info)
		}
	}
	if len(findings) > 0 {
		return nil, &findingsError{findings}
	}

	sources, err := schemaSources(o.root)
	if err != nil {
		return nil, err
	}
	for _, s := range sources {
		data, err := os.ReadFile(filepath.Join(o.root, filepath.FromSlash(s[1])))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(o.out, s[0]), data, 0o600); err != nil { //nolint:gosec // The asset name is built from file names under api/schema/ruralz.
			return nil, err
		}
		m.Schemas = append(m.Schemas, schemaInfo{Name: s[0], Source: s[1], Size: int64(len(data)), SHA256: sha256Hex(data)})
	}

	bench, records, err := benchMembers(o.bench)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeTarGz(&buf, bench, mtime); err != nil {
		return nil, err
	}
	m.Bench = benchInfo{Name: benchArchiveName(o.version), Size: int64(buf.Len()), SHA256: sha256Hex(buf.Bytes()), Records: records}
	if err := os.WriteFile(filepath.Join(o.out, m.Bench.Name), buf.Bytes(), 0o600); err != nil {
		return nil, err
	}

	data, err := encodeManifest(m)
	if err != nil {
		return nil, err
	}
	return m, os.WriteFile(filepath.Join(o.out, manifestName), data, 0o600)
}

// packageArtifact checks one binary and writes its archive. It returns the
// archive's manifest entry, or the build problems that stop it.
func packageArtifact(o packageOptions, d packageDeps, a artifact, toolchain string, mtime time.Time, repo repoFiles) (archiveInfo, []string, error) {
	binary, err := os.ReadFile(filepath.Join(o.dist, a.distDir(), a.executable()))
	if err != nil {
		return archiveInfo{}, nil, fmt.Errorf("%s: %w (run make build and the dry run's darwin builds first)", a.target(), err)
	}
	// A file that is no Go binary is a finding about the input, not a
	// tool error.
	facts, factsErr := d.facts(binary)
	var problems []string
	if factsErr != nil {
		problems = []string{factsErr.Error()}
	} else {
		problems = buildProblems(facts, a, o.version, o.commit, toolchain)
	}
	if len(problems) > 0 {
		return archiveInfo{}, problems, nil
	}
	dir := filepath.Join(o.notices, a.target())
	notice, err := os.ReadFile(filepath.Join(dir, noticeMember)) //nolint:gosec // depgate output under the notices directory given on the command line.
	if err != nil {
		return archiveInfo{}, nil, fmt.Errorf("%s: %w (run depgate -notices first)", a.target(), err)
	}
	thirdParty, err := os.ReadFile(filepath.Join(dir, thirdPartyMember)) //nolint:gosec // depgate output under the notices directory given on the command line.
	if err != nil {
		return archiveInfo{}, nil, fmt.Errorf("%s: %w (run depgate -notices first)", a.target(), err)
	}
	if problems := legalProblems(notice, thirdParty, repo.notice, facts.Info); len(problems) > 0 {
		return archiveInfo{}, problems, nil
	}

	ms := archiveMembers(a, binary, notice, thirdParty, repo)
	var buf bytes.Buffer
	if a.OS == "windows" {
		err = writeZip(&buf, ms, mtime)
	} else {
		err = writeTarGz(&buf, ms, mtime)
	}
	if err != nil {
		return archiveInfo{}, nil, fmt.Errorf("%s: %w", a.target(), err)
	}
	name := a.archiveName(o.version)
	if err := os.WriteFile(filepath.Join(o.out, name), buf.Bytes(), 0o600); err != nil {
		return archiveInfo{}, nil, err
	}
	info := archiveInfo{
		Name: name, Binary: a.Binary, OS: a.OS, Arch: a.Arch, Support: a.Support,
		Size: int64(buf.Len()), SHA256: sha256Hex(buf.Bytes()), SBOM: name + sbomSuffix,
	}
	for _, mb := range ms {
		info.Entries = append(info.Entries, entryOf(mb))
	}
	return info, nil, nil
}
