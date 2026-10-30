// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// goModule is the module part of `go list -json` output.
type goModule struct {
	Path    string
	Version string
	Dir     string
	Main    bool
	Replace *goModule
}

// goPackage is the part of `go list -json` output the gates read.
type goPackage struct {
	ImportPath string
	Standard   bool
	Dir        string
	Module     *goModule
}

// goList lists the packages a binary links on one platform.
func goList(ctx context.Context, dir string, t target) ([]goPackage, error) {
	//nolint:gosec // Arguments come from the fixed target table, never from input.
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-test=false", "-json=ImportPath,Standard,Dir,Module", "./cmd/"+t.binary)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS="+t.goos, "GOARCH="+t.goarch, "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s for %s/%s: %w: %s", t.binary, t.goos, t.goarch, err, strings.TrimSpace(stderr.String()))
	}
	return decodePackages(bytes.NewReader(out))
}

func decodePackages(r io.Reader) ([]goPackage, error) {
	dec := json.NewDecoder(r)
	var pkgs []goPackage
	for {
		var p goPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			return pkgs, nil
		}
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
}

// moduleNotice is what one target's THIRD_PARTY_LICENSES and NOTICE say
// about one linked third-party module.
type moduleNotice struct {
	// Path and Version identify the module as go.mod requires it.
	Path, Version string
	// ReplacePath and ReplaceVersion name a replacement; ReplaceVersion is
	// empty for a directory replacement.
	ReplacePath, ReplaceVersion string
	// IDs are the classified licenses of every license text, sorted.
	IDs []string
	// Texts are the license, patent and notice texts at the module root
	// and in the nested directories that govern linked packages, sorted by
	// Rel.
	Texts []legalText
	// Override is the reviewed license that replaced an unreadable text.
	Override string
	// Election is the license Ruralz elects for a dual-licensed module.
	Election string
}

// analysis is the result of the gates on one target.
type analysis struct {
	findings []string
	// modules are the linked third-party modules, sorted by path.
	modules []moduleNotice
}

// governing finds, for a package directory inside a module, the nearest
// directory from the package up to the module root that holds a license
// text, and returns its slash path relative to the root ("." for the
// root) with its texts. The texts of that directory govern the package;
// none at all returns "." and no texts.
func governing(root, pkgDir string, read func(dir string) ([]legalText, error)) (string, []legalText, error) {
	rel, err := filepath.Rel(root, pkgDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		rel = "."
	}
	for {
		texts, err := read(filepath.Join(root, rel))
		if err != nil {
			return "", nil, err
		}
		if slices.ContainsFunc(texts, func(t legalText) bool { return t.Kind == kindLicense }) {
			return filepath.ToSlash(rel), texts, nil
		}
		if rel == "." {
			return ".", texts, nil
		}
		rel = filepath.Dir(rel)
	}
}

// analyze applies every gate to the packages one target links and
// collects the legal texts of each linked third-party module.
func analyze(t target, pkgs []goPackage, read dirReader) (analysis, error) {
	var out []string
	where := fmt.Sprintf("%s %s/%s", t.binary, t.goos, t.goarch)
	add := func(subject, msg string) { out = append(out, fmt.Sprintf("%s: %s: %s", where, subject, msg)) }

	// Each directory is read once per target.
	cache := map[string][]legalText{}
	cached := func(dir string) ([]legalText, error) {
		if texts, ok := cache[dir]; ok {
			return texts, nil
		}
		texts, err := read(dir)
		if err != nil {
			return nil, err
		}
		cache[dir] = texts
		return texts, nil
	}

	modules := map[string]*moduleNotice{}
	checked := map[string]bool{} // module path, or module path + "/" + governing directory
	for _, p := range pkgs {
		if msg := checkTestSupport(p); msg != "" {
			add(p.ImportPath, msg)
		}
		if p.Standard || p.Module == nil || p.Module.Main {
			continue
		}
		mod := p.Module
		if msg := checkCrypto(p.ImportPath, mod.Path); msg != "" {
			add(p.ImportPath, msg)
		}
		m, seen := modules[mod.Path]
		if !seen {
			m = &moduleNotice{Path: mod.Path, Version: mod.Version, Election: licenseElections()[mod.Path]}
			if mod.Replace != nil {
				m.ReplacePath, m.ReplaceVersion = mod.Replace.Path, mod.Replace.Version
			}
			modules[mod.Path] = m
			if t.binary == "ruralzd" && denied(mod.Path, raftModules()) {
				add(mod.Path, "ruralzd never links Raft (P2, ADR-0006)")
			}
			if t.binary == "ruralz" && denied(mod.Path, jwxModules()) {
				add(mod.Path, "ruralz never links jwx: the offline Policy checks it links import no module the CLI does not link (Dependency graph)")
			}
			if floor, ok := advisoryFloors()[mod.Path]; ok && compareSemver(mod.Version, floor) < 0 {
				out = append(out, fmt.Sprintf("%s: %s %s is below the advisory floor %s", where, mod.Path, mod.Version, floor))
			}
		}
		src := mod
		if mod.Replace != nil {
			src = mod.Replace
		}
		if src.Dir == "" {
			if !checked[mod.Path] {
				checked[mod.Path] = true
				add(mod.Path, "module source not downloaded; run `go mod download`")
			}
			continue
		}
		// The module root's texts always enter the notices; a nested
		// directory's enter when it governs a linked package.
		rootTexts, err := cached(src.Dir)
		if err != nil {
			return analysis{}, err
		}
		dir, texts, err := governing(src.Dir, p.Dir, cached)
		if err != nil {
			return analysis{}, err
		}
		key := mod.Path
		if dir != "." {
			key = mod.Path + "/" + dir
		}
		m.Texts = mergeTexts(m.Texts, "", rootTexts)
		if dir != "." {
			m.Texts = mergeTexts(m.Texts, dir, texts)
		}
		if checked[key] {
			continue
		}
		checked[key] = true
		ids, override, msg := checkLicense(key, mod.Path, texts)
		if override != "" {
			m.Override = override
		}
		for _, id := range ids {
			if !slices.Contains(m.IDs, id) {
				m.IDs = append(m.IDs, id)
			}
		}
		if msg != "" {
			subject := mod.Path
			if dir != "." {
				subject = mod.Path + " (" + dir + ")"
			}
			add(subject, msg)
		}
	}

	a := analysis{findings: out}
	for _, m := range modules {
		slices.Sort(m.IDs)
		a.modules = append(a.modules, *m)
	}
	slices.SortFunc(a.modules, func(x, y moduleNotice) int { return strings.Compare(x.Path, y.Path) })
	return a, nil
}

// mergeTexts adds the texts of one directory (slash path relative to the
// module root, "" for the root) to a module's texts, once each, sorted by
// Rel.
func mergeTexts(have []legalText, dir string, texts []legalText) []legalText {
	for _, t := range texts {
		if dir != "" {
			t.Rel = path.Join(dir, t.Rel)
		}
		if !slices.ContainsFunc(have, func(h legalText) bool { return h.Rel == t.Rel }) {
			have = append(have, t)
		}
	}
	slices.SortFunc(have, func(a, b legalText) int { return strings.Compare(a.Rel, b.Rel) })
	return have
}

// checkLicense applies G2 to the license texts that govern some linked
// packages of a module. key is the module path, or the module path and the
// governing directory, as licenseOverrides keys them. It returns the
// classified licenses, the reviewed override if one applied, and a finding
// or "".
func checkLicense(key, modulePath string, texts []legalText) (ids []string, override, finding string) {
	if id, ok := licenseOverrides()[key]; ok {
		ids, override = []string{id}, id
	} else {
		for _, t := range texts {
			if t.Kind == kindLicense {
				if id := classify(t.Text); !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
		}
		slices.Sort(ids)
	}
	for _, id := range ids {
		switch {
		case slices.Contains(allowedLicenses(), id):
			return ids, override, ""
		case id == "MPL-2.0" && slices.Contains(mplExceptions(), modulePath):
			return ids, override, ""
		case licenseElections()[modulePath] == id:
			return ids, override, ""
		}
	}
	if len(ids) == 0 {
		return ids, override, "no license text found; add a reviewed override (G2)"
	}
	return ids, override, fmt.Sprintf("license %s is not allowed (G2)", strings.Join(ids, ", "))
}

// checkCrypto applies G3 to one package and returns a finding or "".
func checkCrypto(importPath, modulePath string) string {
	if importPath == "golang.org/x/crypto" || strings.HasPrefix(importPath, "golang.org/x/crypto/") {
		if slices.Contains(cryptoDelegation(), importPath) {
			return ""
		}
		return "golang.org/x/crypto package off the FIPS delegation allowlist (G3)"
	}
	if strings.Contains(importPath, "crypto") {
		if _, ok := cryptoExceptions()[modulePath]; ok {
			return ""
		}
		return "cryptography outside Go crypto/... needs a G3 exception row"
	}
	return ""
}

// denied reports whether module path is one of roots, a later major
// version or a path below one ("<root>/v4"), or a sibling module named
// after one ("<root>-boltdb/v2").
func denied(path string, roots []string) bool {
	return slices.ContainsFunc(roots, func(r string) bool {
		return path == r || strings.HasPrefix(path, r+"-") || strings.HasPrefix(path, r+"/")
	})
}

// under reports whether p is root or a path below it.
func under(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+"/")
}

// checkTestSupport denies test support in a binary: this module's test
// support, CI tool and suite packages, the standard library's test
// packages and the test-only third-party modules. It returns a finding or
// "".
func checkTestSupport(p goPackage) string {
	switch {
	case p.Standard:
		if slices.ContainsFunc(testOnlyStandard(), func(s string) bool { return under(p.ImportPath, s) }) {
			return "standard library test support is never linked into a binary"
		}
	case p.Module == nil:
	case p.Module.Main:
		if rel, ok := strings.CutPrefix(p.ImportPath, p.Module.Path+"/"); ok && isTestSupport(rel) {
			return "test support and CI tools are never linked into a binary"
		}
	default:
		if slices.ContainsFunc(testOnlyModules(), func(m string) bool { return under(p.Module.Path, m) }) {
			return "test-only module is never linked into a binary"
		}
	}
	return ""
}

// isTestSupport reports whether a package path relative to this module is
// test support: under a testSupportRoots tree, or a "<name>test" package
// directly below "<name>" (internal/clock/clocktest) or below one.
func isTestSupport(rel string) bool {
	if slices.ContainsFunc(testSupportRoots(), func(root string) bool { return under(rel, root) }) {
		return true
	}
	elems := strings.Split(rel, "/")
	for i := 1; i < len(elems); i++ {
		if elems[i] == elems[i-1]+"test" {
			return true
		}
	}
	return false
}

// compareSemver compares two vMAJOR.MINOR.PATCH[-pre] versions.
func compareSemver(a, b string) int {
	coreA, preA := splitSemver(a)
	coreB, preB := splitSemver(b)
	for i := range 3 {
		if c := coreA[i] - coreB[i]; c != 0 {
			if c < 0 {
				return -1
			}
			return 1
		}
	}
	switch {
	case preA == preB:
		return 0
	case preA == "":
		return 1
	case preB == "":
		return -1
	default:
		return strings.Compare(preA, preB)
	}
}

func splitSemver(v string) ([3]int, string) {
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "+")
	core, pre, _ := strings.Cut(v, "-")
	var out [3]int
	for i, part := range strings.SplitN(core, ".", 3) {
		out[i], _ = strconv.Atoi(part)
	}
	return out, pre
}

// options are the command-line settings of a run.
type options struct {
	// dir is the module root.
	dir string
	// notices, when set, is the directory that receives each target's
	// THIRD_PARTY_LICENSES and NOTICE once every gate passes.
	notices string
}

// run applies the gates to every target of the module and, when asked and
// no gate failed, writes every target's notices. It returns the findings.
func run(ctx context.Context, o options) ([]string, error) {
	var all []string
	results := map[target]analysis{}
	for _, t := range targets() {
		pkgs, err := goList(ctx, o.dir, t)
		if err != nil {
			return nil, err
		}
		a, err := analyze(t, pkgs, readLegal)
		if err != nil {
			return nil, err
		}
		all = append(all, a.findings...)
		results[t] = a
	}
	slices.Sort(all)
	all = slices.Compact(all)
	if len(all) > 0 || o.notices == "" {
		return all, nil
	}
	std, err := goStandardLibrary(ctx, o.dir)
	if err != nil {
		return nil, err
	}
	notice, err := os.ReadFile(filepath.Join(o.dir, "NOTICE"))
	if err != nil {
		return nil, fmt.Errorf("reading the repository NOTICE: %w", err)
	}
	if err := writeNotices(o.notices, targets(), results, std, string(notice)); err != nil {
		return nil, err
	}
	return nil, nil
}
