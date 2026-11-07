// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
)

// consoleName prefixes the findings on the npm packages of Ruralz
// Console.
const consoleName = "Ruralz Console"

// pnpmPackage is one entry of `pnpm licenses list --json` output: a
// package name with every version and directory pnpm found it at, listed
// under the license pnpm reports for it.
type pnpmPackage struct {
	Name string `json:"name"`
	// Versions are registry versions, or the specifier of a package no
	// registry serves, such as "file:../vendor/x".
	Versions []string `json:"versions"`
	// Paths are the package directories; pnpm 11 writes null for a
	// package it found no directory for.
	Paths []*string `json:"paths"`
	// RegistryName is the named registry the package came from, empty
	// for the default registry.
	RegistryName string `json:"registryName"`
}

// decodePnpm reads `pnpm licenses list --json` output, an object mapping
// each license pnpm reports to its packages, and returns the packages.
func decodePnpm(r io.Reader) ([]pnpmPackage, error) {
	var groups map[string][]pnpmPackage
	if err := decodeOne(r, &groups); err != nil {
		return nil, fmt.Errorf("reading the pnpm licenses output: %w", err)
	}
	if groups == nil {
		return nil, errors.New("the pnpm licenses output is not a JSON object of licenses")
	}
	licenses := make([]string, 0, len(groups))
	for l := range groups {
		licenses = append(licenses, l)
	}
	slices.Sort(licenses)
	var out []pnpmPackage
	for _, l := range licenses {
		for _, p := range groups[l] {
			if p.Name == "" {
				return nil, fmt.Errorf("the pnpm licenses output lists a package without a name under %q", l)
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// npmManifest is the part of a package.json the gate reads.
type npmManifest struct {
	Name     string          `json:"name"`
	Version  string          `json:"version"`
	License  json.RawMessage `json:"license"`
	Licenses json.RawMessage `json:"licenses"`
}

// declaredLicense returns the license expression a package.json declares,
// read as pnpm reads it: "license", else the legacy "licenses"; a string,
// an object's "type" or "name", or an array of those, whose licenses are
// alternatives. A manifest that declares none, or defers to a file with
// "SEE LICENSE IN <file>", gets "" and what it holds instead. pnpm then
// guesses a license from license names in a license file's text, so its
// own report is never read as a declaration.
func declaredLicense(m npmManifest) (expr, missing string) {
	expr = licenseField(m.License)
	if expr == "" {
		expr = licenseField(m.Licenses)
	}
	switch {
	case expr == "":
		return "", "declares no license expression"
	case strings.Contains(strings.ToLower(expr), "see license"):
		return "", fmt.Sprintf("declares %q, no license expression", expr)
	}
	return expr, ""
}

// licenseField reads one license field of a package.json.
func licenseField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var names []string
		for _, e := range list {
			if n := licenseType(e); n != "" {
				names = append(names, n)
			}
		}
		if len(names) > 1 {
			return "(" + strings.Join(names, " OR ") + ")"
		}
		return strings.Join(names, "")
	}
	return licenseType(raw)
}

// licenseType reads one license entry: a string, or an object's "type" or
// else its "name".
func licenseType(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	for _, k := range []string{"type", "name"} {
		if v, ok := obj[k]; ok && json.Unmarshal(v, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// npmSource says where an npm package's source is available: the npm
// registry's tarball of that version, the named registry pnpm resolved it
// from, or, for a package pnpm reports by a specifier rather than a
// registry version, that specifier.
func npmSource(name, version, registry string, reported, specifiers []string) string {
	switch {
	case registry != "":
		return "the pnpm named registry " + registry
	case !slices.Contains(reported, version):
		if len(specifiers) == 0 {
			return "no registry"
		}
		return "no registry; pnpm reports it as " + strings.Join(specifiers, ", ")
	}
	base := name[strings.LastIndex(name, "/")+1:]
	return "https://registry.npmjs.org/" + name + "/-/" + base + "-" + version + ".tgz"
}

// consoleResult is the gate's result on the npm packages of Ruralz
// Console.
type consoleResult struct {
	findings []string
	// packages are the shipped packages with their legal texts, sorted.
	packages []moduleNotice
}

// npmInstance is one package directory of a pnpm entry with its manifest.
type npmInstance struct {
	dir      string
	manifest npmManifest
}

// analyzeConsole applies G2 to every npm package `pnpm licenses list
// --prod` reports for Ruralz Console, which are the `dependencies` and
// `optionalDependencies` of console/package.json, direct and transitive,
// as the lockfile resolves and pnpm installed them; devDependencies are
// build-only and never ship. It reads each package's own package.json for
// its name, version and declared license, and its legal texts.
func analyzeConsole(entries []pnpmPackage, pol spdxPolicy, read dirReader, readFile func(string) ([]byte, error)) (consoleResult, error) {
	var res consoleResult
	finding := func(subject, msg string) {
		res.findings = append(res.findings, fmt.Sprintf("%s: %s: %s", consoleName, subject, msg))
	}
	seen := map[string]bool{}
	for _, e := range entries {
		var found []npmInstance
		for _, p := range e.Paths {
			if p == nil {
				finding(e.Name+" "+strings.Join(e.Versions, ", "), "pnpm reports no directory for it; install with pnpm install --frozen-lockfile")
				continue
			}
			data, err := readFile(filepath.Join(*p, "package.json"))
			if errors.Is(err, fs.ErrNotExist) {
				finding(e.Name+" "+strings.Join(e.Versions, ", "), "no package.json at "+*p+"; install with pnpm install --frozen-lockfile")
				continue
			}
			if err != nil {
				return consoleResult{}, fmt.Errorf("reading the manifest of %s: %w", e.Name, err)
			}
			var m npmManifest
			if err := json.Unmarshal(data, &m); err != nil {
				return consoleResult{}, fmt.Errorf("parsing %s: %w", filepath.Join(*p, "package.json"), err)
			}
			if m.Name == "" {
				m.Name = e.Name
			}
			if m.Version == "" {
				finding(e.Name+" "+strings.Join(e.Versions, ", "), "the package.json at "+*p+" declares no version")
				continue
			}
			found = append(found, npmInstance{dir: *p, manifest: m})
		}
		slices.SortFunc(found, func(a, b npmInstance) int { return strings.Compare(a.dir, b.dir) })
		// pnpm reports a package that no registry serves by its
		// specifier; the specifiers are the reported versions no
		// manifest of the entry has.
		var specifiers []string
		for _, v := range e.Versions {
			if !slices.ContainsFunc(found, func(i npmInstance) bool { return i.manifest.Version == v }) {
				specifiers = append(specifiers, v)
			}
		}
		for _, inst := range found {
			m := inst.manifest
			// Peer-dependency variants put one version in several
			// directories; the package is the same.
			id := m.Name + "@" + m.Version + "\x00" + e.RegistryName
			if seen[id] {
				continue
			}
			seen[id] = true
			p := spdxPackage{
				Name: m.Name, Version: m.Version, Dir: inst.dir,
				Source: npmSource(m.Name, m.Version, e.RegistryName, e.Versions, specifiers),
			}
			p.Declared, p.Missing = declaredLicense(m)
			n, msg := checkPackage(pol, p)
			if msg != "" {
				finding(m.Name+" "+m.Version, msg)
			}
			var err error
			if n.Texts, err = packageTexts(read, readFile, p); err != nil {
				return consoleResult{}, err
			}
			res.packages = append(res.packages, n)
		}
	}
	slices.Sort(res.findings)
	res.findings = slices.Compact(res.findings)
	sortPackages(res.packages)
	return res, nil
}
