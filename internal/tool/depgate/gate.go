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
	Module     *goModule
}

// goList lists the packages a binary links on one platform.
func goList(ctx context.Context, dir string, t target) ([]goPackage, error) {
	//nolint:gosec // Arguments come from the fixed target table, never from input.
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-test=false", "-json=ImportPath,Standard,Module", "./cmd/"+t.binary)
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

// licenseReader returns the classified licenses of a module directory.
type licenseReader func(dir string) ([]string, error)

// analyze applies every gate to the packages one target links.
func analyze(t target, pkgs []goPackage, read licenseReader) ([]string, error) {
	var out []string
	where := fmt.Sprintf("%s %s/%s", t.binary, t.goos, t.goarch)
	seen := map[string]bool{}
	for _, p := range pkgs {
		if p.Standard || p.Module == nil || p.Module.Main {
			continue
		}
		mod := p.Module
		if msg := checkCrypto(p.ImportPath, mod.Path); msg != "" {
			out = append(out, fmt.Sprintf("%s: %s: %s", where, p.ImportPath, msg))
		}
		if seen[mod.Path] {
			continue
		}
		seen[mod.Path] = true
		if t.binary == "ruralzd" && slices.ContainsFunc(raftModules(), func(r string) bool {
			return mod.Path == r || strings.HasPrefix(mod.Path, r+"-") || strings.HasPrefix(mod.Path, r+"/")
		}) {
			out = append(out, fmt.Sprintf("%s: %s: ruralzd never links Raft (P2, ADR-0006)", where, mod.Path))
		}
		if floor, ok := advisoryFloors()[mod.Path]; ok && compareSemver(mod.Version, floor) < 0 {
			out = append(out, fmt.Sprintf("%s: %s %s is below the advisory floor %s", where, mod.Path, mod.Version, floor))
		}
		msg, err := checkLicense(mod, read)
		if err != nil {
			return nil, err
		}
		if msg != "" {
			out = append(out, fmt.Sprintf("%s: %s: %s", where, mod.Path, msg))
		}
	}
	return out, nil
}

// checkLicense applies G2 to one module and returns a finding or "".
func checkLicense(mod *goModule, read licenseReader) (string, error) {
	src := mod
	if mod.Replace != nil {
		src = mod.Replace
	}
	var found []string
	if id, ok := licenseOverrides()[mod.Path]; ok {
		found = []string{id}
	} else {
		if src.Dir == "" {
			return "module source not downloaded; run `go mod download`", nil
		}
		var err error
		if found, err = read(src.Dir); err != nil {
			return "", err
		}
	}
	for _, id := range found {
		switch {
		case slices.Contains(allowedLicenses(), id):
			return "", nil
		case id == "MPL-2.0" && slices.Contains(mplExceptions(), mod.Path):
			return "", nil
		case licenseElections()[mod.Path] == id:
			return "", nil
		}
	}
	if len(found) == 0 {
		return "no license file found; add a reviewed override (G2)", nil
	}
	return fmt.Sprintf("license %s is not allowed (G2)", strings.Join(found, ", ")), nil
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

// run applies the gates to every target of the module in dir.
func run(ctx context.Context, dir string) ([]string, error) {
	var all []string
	for _, t := range targets() {
		pkgs, err := goList(ctx, dir, t)
		if err != nil {
			return nil, err
		}
		findings, err := analyze(t, pkgs, moduleLicenses)
		if err != nil {
			return nil, err
		}
		all = append(all, findings...)
	}
	slices.Sort(all)
	return slices.Compact(all), nil
}
