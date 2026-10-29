// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// sectionRule starts a component section of depgate's THIRD_PARTY_LICENSES;
// the section's heading line follows it: "<module path> <version>", or
// stdHeading and the Go version.
const sectionRule = "================================================================================"

// stdHeading names the Go standard library section of
// THIRD_PARTY_LICENSES.
const stdHeading = "Go standard library and runtime"

// thirdPartyHeadings returns the heading of every component section.
func thirdPartyHeadings(text []byte) map[string]bool {
	out := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	afterRule := false
	for sc.Scan() {
		line := sc.Text()
		if afterRule {
			out[line] = true
		}
		afterRule = line == sectionRule
	}
	return out
}

// legalProblems audits an artifact's NOTICE and THIRD_PARTY_LICENSES:
// NOTICE starts with the repository NOTICE (ADR-0002), and
// THIRD_PARTY_LICENSES has a section for the Go standard library the
// binary was built with and for every module it links, as its build
// information lists them.
func legalProblems(notice, thirdParty, repoNotice []byte, bi *buildinfo.BuildInfo) []string {
	var out []string
	if !bytes.HasPrefix(notice, bytes.TrimRight(repoNotice, "\r\n")) {
		out = append(out, "NOTICE does not start with the repository NOTICE")
	}
	h := thirdPartyHeadings(thirdParty)
	if !h[stdHeading+" "+bi.GoVersion] {
		out = append(out, fmt.Sprintf("THIRD_PARTY_LICENSES has no section for the Go standard library %s", bi.GoVersion))
	}
	for _, d := range bi.Deps {
		if !h[d.Path+" "+d.Version] {
			out = append(out, fmt.Sprintf("THIRD_PARTY_LICENSES has no section for %s %s, which the binary links", d.Path, d.Version))
		}
	}
	return out
}

// cdxComponent is the part of a CycloneDX component the audit reads.
type cdxComponent struct {
	Purl       string         `json:"purl"`
	Components []cdxComponent `json:"components"`
}

// cdxDocument is the part of a CycloneDX JSON document the audit reads.
type cdxDocument struct {
	BOMFormat   string         `json:"bomFormat"`
	SpecVersion string         `json:"specVersion"`
	Components  []cdxComponent `json:"components"`
}

// normalizePurl lower-cases a package URL and drops its qualifiers and
// subpath, so purls compare whatever escaping the generator chose.
func normalizePurl(p string) string {
	p, _, _ = strings.Cut(p, "#")
	p, _, _ = strings.Cut(p, "?")
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}
	return strings.ToLower(p)
}

// golangPurl is the package URL of a Go module version.
func golangPurl(path, version string) string {
	return strings.ToLower("pkg:golang/" + path + "@" + version)
}

// sbomProblems audits an archive's SBOM: CycloneDX 1.7 JSON naming every
// module the archived binary links.
func sbomProblems(file string, bi *buildinfo.BuildInfo) ([]string, error) {
	name := filepath.Base(file)
	data, err := os.ReadFile(file) //nolint:gosec // The SBOM of an expected archive in the release directory.
	if errors.Is(err, fs.ErrNotExist) {
		return []string{"SBOM " + name + " is missing (Syft writes it after package)"}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc cdxDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return []string{fmt.Sprintf("SBOM %s is not JSON: %v", name, err)}, nil
	}
	var out []string
	if doc.BOMFormat != "CycloneDX" || doc.SpecVersion != cycloneDXVersion {
		out = append(out, fmt.Sprintf("SBOM %s is %s %s, want CycloneDX %s", name, doc.BOMFormat, doc.SpecVersion, cycloneDXVersion))
	}
	purls := map[string]bool{}
	var walk func([]cdxComponent)
	walk = func(cs []cdxComponent) {
		for _, c := range cs {
			if c.Purl != "" {
				purls[normalizePurl(c.Purl)] = true
			}
			walk(c.Components)
		}
	}
	walk(doc.Components)
	for _, d := range bi.Deps {
		ok := purls[golangPurl(d.Path, d.Version)]
		if d.Replace != nil {
			ok = ok || purls[golangPurl(d.Replace.Path, d.Replace.Version)] || purls[golangPurl(d.Path, d.Replace.Version)]
		}
		if !ok {
			out = append(out, fmt.Sprintf("SBOM %s lacks %s %s, which the binary links", name, d.Path, d.Version))
		}
	}
	return out, nil
}
