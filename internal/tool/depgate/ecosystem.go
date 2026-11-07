// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Ecosystems of the non-Go license gate (ADR-0002, "Non-Go license gate"):
// the crates a Rust PDK ships and the npm packages Ruralz Console ships,
// each under the SPDX license expression its manifest declares. The
// policy tables key a crate as "cargo:<name>" and an npm package as
// "npm:<name>".
const (
	ecosystemCargo = "cargo"
	ecosystemNPM   = "npm"
)

// spdxPolicy is what G2 admits for the packages of one ecosystem.
type spdxPolicy struct {
	// ecosystem is ecosystemCargo or ecosystemNPM.
	ecosystem string
	// admitted are the admission keys of the G2 list and of the
	// ecosystem's addendum.
	admitted map[string]bool
	// mpl are the packages, by name, that a named exception admits under
	// MPL-2.0.
	mpl map[string]bool
	// elections map package names to the license term Ruralz elects, as
	// written, and electionKeys to its admission key.
	elections, electionKeys map[string]string
	// overrides map package names to a reviewed SPDX expression.
	overrides map[string]string
}

// ecosystemPolicy builds the policy of one ecosystem from the tables of
// policy.go.
func ecosystemPolicy(ecosystem string) (spdxPolicy, error) {
	return newSPDXPolicy(ecosystem, allowedLicenses(), licenseAddenda()[ecosystem], mplExceptions(), licenseElections(), licenseOverrides())
}

// newSPDXPolicy builds the policy of one ecosystem: the G2 list and the
// ecosystem's addendum, and the entries of the exception, election and
// override tables whose keys carry the ecosystem's prefix.
func newSPDXPolicy(ecosystem string, allowed, addendum, mpl []string, elections, overrides map[string]string) (spdxPolicy, error) {
	p := spdxPolicy{
		ecosystem: ecosystem,
		admitted:  map[string]bool{},
		mpl:       map[string]bool{},
		elections: map[string]string{}, electionKeys: map[string]string{},
		overrides: map[string]string{},
	}
	for _, term := range slices.Concat(allowed, addendum) {
		key, err := spdxTermKey(term)
		if err != nil {
			return spdxPolicy{}, fmt.Errorf("the %s policy admits %q: %w", ecosystem, term, err)
		}
		p.admitted[key] = true
	}
	prefix := ecosystem + ":"
	for _, k := range mpl {
		if name, ok := strings.CutPrefix(k, prefix); ok {
			p.mpl[name] = true
		}
	}
	for k, term := range elections {
		if name, ok := strings.CutPrefix(k, prefix); ok {
			key, err := spdxTermKey(term)
			if err != nil {
				return spdxPolicy{}, fmt.Errorf("the election for %s: %w", k, err)
			}
			p.elections[name], p.electionKeys[name] = term, key
		}
	}
	for k, expr := range overrides {
		if name, ok := strings.CutPrefix(k, prefix); ok {
			p.overrides[name] = expr
		}
	}
	return p, nil
}

// spdxPackage is one crate or npm package an artifact ships.
type spdxPackage struct {
	// Name and Version identify the package as its manifest does.
	Name, Version string
	// Declared is the license expression the manifest declares, or "".
	Declared string
	// Missing says what the manifest holds instead of an expression, for
	// the finding of a package that declares none.
	Missing string
	// Source says where the package's source is available.
	Source string
	// Dir is the package root, whose legal texts the notices reproduce.
	Dir string
	// LicenseFiles are further license files the manifest names, as slash
	// paths relative to Dir.
	LicenseFiles []string
}

// checkPackage applies G2 to the license expression of one package: the
// reviewed override that names it, or else the expression its manifest
// declares. The expression resolves when the G2 list, the ecosystem's
// addendum, a named MPL-2.0 exception or the package's election admits
// its terms, as spdxNode.resolves combines them; an election must name a
// license the expression offers. It returns the package's notice, without
// its texts, and a finding or "".
func checkPackage(pol spdxPolicy, p spdxPackage) (moduleNotice, string) {
	m := moduleNotice{Path: p.Name, Version: p.Version, Source: p.Source}
	expr, legacySlash := p.Declared, pol.ecosystem == ecosystemCargo
	if o, ok := pol.overrides[p.Name]; ok {
		expr, legacySlash, m.Override = o, false, o
	}
	if strings.TrimSpace(expr) == "" {
		missing := p.Missing
		if missing == "" {
			missing = "declares no license expression"
		}
		return m, missing + "; add a reviewed override (G2)"
	}
	n, err := parseSPDX(expr, legacySlash)
	if err != nil {
		if m.Override != "" {
			return m, fmt.Sprintf("reviewed override %q is not a valid SPDX expression: %v (G2)", expr, err)
		}
		return m, fmt.Sprintf("license expression %q is not a valid SPDX expression: %v; add a reviewed override (G2)", expr, err)
	}
	m.Expression = n.String()
	elected, hasElection := pol.electionKeys[p.Name]
	if hasElection {
		m.Election = pol.elections[p.Name]
		if !slices.Contains(n.keys(), elected) {
			return m, fmt.Sprintf("elected license %s is not among the package's licenses %s (G2)", m.Election, m.Expression)
		}
	}
	admit := func(withMPL bool) func(string) bool {
		return func(key string) bool {
			switch {
			case key == "mpl-2.0":
				return withMPL && (pol.mpl[p.Name] || hasElection && key == elected)
			case pol.admitted[key]:
				return true
			default:
				return hasElection && key == elected
			}
		}
	}
	if !n.resolves(admit(true)) {
		return m, fmt.Sprintf("license expression %s does not resolve to the G2 list or the %s addendum; not admitted: %s (G2)",
			m.Expression, pol.ecosystem, strings.Join(n.blocking(admit(true)), ", "))
	}
	// A package that resolves only through MPL-2.0 is used under it.
	m.MPL = !n.resolves(admit(false))
	return m, ""
}

// packageTexts reads the legal texts at a package's root and the license
// files its manifest names besides them, sorted by Rel.
func packageTexts(read dirReader, readFile func(string) ([]byte, error), p spdxPackage) ([]legalText, error) {
	texts, err := read(p.Dir)
	if err != nil {
		return nil, fmt.Errorf("reading the legal texts of %s %s: %w", p.Name, p.Version, err)
	}
	for _, f := range p.LicenseFiles {
		rel := path.Clean(filepath.ToSlash(f))
		if slices.ContainsFunc(texts, func(t legalText) bool { return t.Rel == rel }) {
			continue
		}
		data, err := readFile(filepath.Join(p.Dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("reading the license file of %s %s: %w", p.Name, p.Version, err)
		}
		texts = append(texts, legalText{Rel: rel, Kind: kindLicense, Text: normalizeText(string(data))})
	}
	slices.SortFunc(texts, func(a, b legalText) int { return strings.Compare(a.Rel, b.Rel) })
	return texts, nil
}

// sortPackages orders notices by package name, version and source, so
// the same version from two registries keeps one order too.
func sortPackages(ms []moduleNotice) {
	slices.SortFunc(ms, func(a, b moduleNotice) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if c := compareSemver(a.Version, b.Version); c != 0 {
			return c
		}
		return strings.Compare(a.Source, b.Source)
	})
}

// openInput opens the -cargo or -pnpm input: the named file, or standard
// input for "-".
func openInput(name string, stdin io.Reader) (io.ReadCloser, error) {
	if name == "-" {
		if stdin == nil {
			return nil, errors.New("no standard input to read")
		}
		return io.NopCloser(stdin), nil
	}
	return os.Open(name) //nolint:gosec // The file is the -cargo or -pnpm argument.
}

// decodeOne decodes the single JSON value that r holds into v.
func decodeOne(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("the input holds more than one JSON value")
	}
	return nil
}
