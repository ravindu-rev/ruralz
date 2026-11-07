// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
)

// cargoMetadata is the part of `cargo metadata --format-version 1` output
// the gate reads (docs/engineering/01-tech-stack-and-libraries.md,
// "License rules").
type cargoMetadata struct {
	Version          int            `json:"version"`
	Packages         []cargoPackage `json:"packages"`
	WorkspaceMembers []string       `json:"workspace_members"`
	WorkspaceRoot    string         `json:"workspace_root"`
	Resolve          *cargoResolve  `json:"resolve"`
}

// cargoPackage is one package of the output: a workspace member or a
// dependency.
type cargoPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	ID      string `json:"id"`
	// License is the manifest's license expression, null when absent.
	License *string `json:"license"`
	// LicenseFile is the manifest's license-file, relative to the
	// manifest, null when absent.
	LicenseFile *string `json:"license_file"`
	// Source is null for path dependencies and workspace members, else
	// "registry+URL", "sparse+URL" or "git+URL".
	Source       *string       `json:"source"`
	Targets      []cargoTarget `json:"targets"`
	ManifestPath string        `json:"manifest_path"`
}

// cargoTarget is one Cargo target of a package; a library target's kinds
// are its crate types, such as "lib" or "proc-macro".
type cargoTarget struct {
	Kind       []string `json:"kind"`
	CrateTypes []string `json:"crate_types"`
}

// cargoResolve is the resolved dependency graph.
type cargoResolve struct {
	Nodes []cargoNode `json:"nodes"`
	// Root is the package cargo metadata ran for, null in a virtual
	// workspace.
	Root *string `json:"root"`
}

// cargoNode is one package of the graph with its resolved dependencies.
type cargoNode struct {
	ID   string     `json:"id"`
	Deps []cargoDep `json:"deps"`
}

// cargoDep is one dependency edge of a node.
type cargoDep struct {
	Pkg      string         `json:"pkg"`
	DepKinds []cargoDepKind `json:"dep_kinds"`
}

// cargoDepKind is one kind an edge has: null for a normal dependency,
// "dev" or "build".
type cargoDepKind struct {
	Kind *string `json:"kind"`
}

// decodeCargo reads `cargo metadata --format-version 1` output.
func decodeCargo(r io.Reader) (cargoMetadata, error) {
	var md cargoMetadata
	if err := decodeOne(r, &md); err != nil {
		return cargoMetadata{}, fmt.Errorf("reading the cargo metadata output: %w", err)
	}
	switch {
	case md.Version != 1:
		return cargoMetadata{}, fmt.Errorf("the cargo metadata output is format version %d; depgate reads --format-version 1", md.Version)
	case md.Resolve == nil:
		return cargoMetadata{}, errors.New("the cargo metadata output has no resolve graph: run cargo metadata without --no-deps")
	case len(md.WorkspaceMembers) == 0:
		return cargoMetadata{}, errors.New("the cargo metadata output names no workspace member")
	}
	return md, nil
}

// cargoArtifact is a PDK as one Cargo run sees it: its root packages and
// the third-party packages it ships.
type cargoArtifact struct {
	roots, shipped []cargoPackage
}

// cargoShipped resolves what a PDK ships (ADR-0002, "Non-Go license
// gate"): from the package cargo metadata ran for, or every member of a
// virtual workspace, the packages its normal dependencies reach, again
// through normal dependencies. dev-dependencies build only tests,
// examples and benchmarks and build-dependencies only build scripts, and
// a proc-macro crate runs inside the compiler, so none of them, nor what
// only they reach, ends up in a Plugin. Workspace members are the PDK's
// own packages, not third-party ones. The graph lists what cargo resolved
// for the --filter-platform target, or for every platform without it.
func cargoShipped(md cargoMetadata) (cargoArtifact, error) {
	pkgs := map[string]cargoPackage{}
	for _, p := range md.Packages {
		pkgs[p.ID] = p
	}
	nodes := map[string]cargoNode{}
	for _, n := range md.Resolve.Nodes {
		nodes[n.ID] = n
	}
	rootIDs := md.WorkspaceMembers
	if r := md.Resolve.Root; r != nil && *r != "" {
		rootIDs = []string{*r}
	}
	var a cargoArtifact
	seen := map[string]bool{}
	queue := slices.Clone(rootIDs)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		p, ok := pkgs[id]
		if !ok {
			return cargoArtifact{}, fmt.Errorf("the cargo metadata output lists no package %s", id)
		}
		n, ok := nodes[id]
		if !ok {
			return cargoArtifact{}, fmt.Errorf("the cargo metadata resolve graph has no node for %s", id)
		}
		switch {
		case slices.Contains(rootIDs, id):
			a.roots = append(a.roots, p)
		case !slices.Contains(md.WorkspaceMembers, id):
			a.shipped = append(a.shipped, p)
		}
		if isProcMacro(p) {
			continue
		}
		for _, d := range n.Deps {
			dep, ok := pkgs[d.Pkg]
			if !ok {
				return cargoArtifact{}, fmt.Errorf("the cargo metadata output lists no package %s, a dependency of %s", d.Pkg, id)
			}
			if shipsWith(d) && !isProcMacro(dep) {
				queue = append(queue, d.Pkg)
			}
		}
	}
	byName := func(x, y cargoPackage) int {
		if c := strings.Compare(x.Name, y.Name); c != 0 {
			return c
		}
		return compareSemver(x.Version, y.Version)
	}
	slices.SortFunc(a.roots, byName)
	slices.SortFunc(a.shipped, byName)
	return a, nil
}

// shipsWith reports whether a dependency edge reaches the artifact: one
// of its kinds is a normal dependency, or it lists no kinds.
func shipsWith(d cargoDep) bool {
	if len(d.DepKinds) == 0 {
		return true
	}
	return slices.ContainsFunc(d.DepKinds, func(k cargoDepKind) bool {
		return k.Kind == nil || *k.Kind != "dev" && *k.Kind != "build"
	})
}

// isProcMacro reports whether a package's library is a proc-macro crate.
func isProcMacro(p cargoPackage) bool {
	return slices.ContainsFunc(p.Targets, func(t cargoTarget) bool {
		return slices.Contains(t.Kind, "proc-macro") || slices.Contains(t.CrateTypes, "proc-macro")
	})
}

// crateSource says where a crate's source is available: the crates.io
// download of that version, which crates.io keeps for every published
// version, the Git repository and commit, another registry, or a local
// path relative to the workspace root, so the notices name no machine
// path (docs/engineering/04-release-versioning-and-compatibility.md,
// "Packaging and audit").
func crateSource(p cargoPackage, workspaceRoot string) string {
	if p.Source == nil {
		dir := filepath.Dir(p.ManifestPath)
		if rel, err := filepath.Rel(workspaceRoot, dir); err == nil {
			dir = rel
		}
		return "a local path (" + filepath.ToSlash(dir) + "), not published"
	}
	s := *p.Source
	switch {
	case s == "registry+https://github.com/rust-lang/crates.io-index" || s == "sparse+https://index.crates.io/":
		return "https://static.crates.io/crates/" + p.Name + "/" + p.Version + "/download"
	case strings.HasPrefix(s, "git+"):
		repo, commit, _ := strings.Cut(strings.TrimPrefix(s, "git+"), "#")
		repo, _, _ = strings.Cut(repo, "?")
		if commit == "" {
			return repo
		}
		return repo + " at commit " + commit
	case strings.HasPrefix(s, "registry+") || strings.HasPrefix(s, "sparse+"):
		_, index, _ := strings.Cut(s, "+")
		return "the registry whose index is " + index
	default:
		return s
	}
}

// cargoResult is the gate's result on one Cargo artifact.
type cargoResult struct {
	// title names the PDK's root packages: "<name> <version>", comma
	// separated.
	title string
	// plural is set when the title names several packages.
	plural   bool
	findings []string
	// crates are the shipped crates with their legal texts, sorted.
	crates []moduleNotice
}

// analyzeCargo applies G2 to every crate a PDK ships and collects their
// legal texts.
func analyzeCargo(md cargoMetadata, pol spdxPolicy, read dirReader, readFile func(string) ([]byte, error)) (cargoResult, error) {
	a, err := cargoShipped(md)
	if err != nil {
		return cargoResult{}, err
	}
	var res cargoResult
	names := make([]string, len(a.roots))
	for i, r := range a.roots {
		names[i] = r.Name + " " + r.Version
	}
	res.title, res.plural = strings.Join(names, ", "), len(names) > 1
	for _, c := range a.shipped {
		p := spdxPackage{Name: c.Name, Version: c.Version, Source: crateSource(c, md.WorkspaceRoot), Dir: filepath.Dir(c.ManifestPath)}
		if c.License != nil {
			p.Declared = *c.License
		}
		if c.LicenseFile != nil && *c.LicenseFile != "" {
			p.LicenseFiles = []string{*c.LicenseFile}
			if strings.TrimSpace(p.Declared) == "" {
				p.Missing = "declares only the license file " + *c.LicenseFile + " and no license expression"
			}
		}
		m, finding := checkPackage(pol, p)
		if finding != "" {
			res.findings = append(res.findings, fmt.Sprintf("%s: %s %s: %s", res.title, c.Name, c.Version, finding))
		}
		if m.Texts, err = packageTexts(read, readFile, p); err != nil {
			return cargoResult{}, err
		}
		res.crates = append(res.crates, m)
	}
	sortPackages(res.crates)
	return res, nil
}
