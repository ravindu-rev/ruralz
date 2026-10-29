// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fixtureCommit and fixtureTime are the commit and commit time of the
// fixture release.
const fixtureCommit = "0123456789abcdef0123456789abcdef01234567"

func fixtureTime() time.Time { return time.Date(2026, 10, 1, 12, 30, 45, 0, time.UTC) }

// fixtureNotice is the fixture repository's NOTICE.
const fixtureNotice = "Ruralz\nCopyright 2026 Revington\n"

// fixture is a repository root, build output and notices for package,
// with a stub git and stub build facts.
type fixture struct {
	root, dist, notices, out string
	version                  string
	// deps are the modules every fake binary links.
	deps []*debug.Module
	// tweak changes the facts of one target, such as "ruralz_linux_amd64".
	tweak map[string]func(*buildFacts)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readBack reads a file the test wrote or built. It reads through os.Open,
// which gosec does not count as a taint source as it does os.ReadFile, so
// data the tests write back never makes writeFile, which has more callers
// than gosec examines, a nondeterministic finding.
func readBack(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // A file the test itself wrote or built.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// thirdPartyText is a THIRD_PARTY_LICENSES with a section per component.
func thirdPartyText(goVersion string, deps []*debug.Module) string {
	var b strings.Builder
	b.WriteString("THIRD_PARTY_LICENSES\n\n" + sectionRule + "\n" + stdHeading + " " + goVersion + "\nLicense: BSD-3-Clause\n")
	for _, d := range deps {
		fmt.Fprintf(&b, "\n%s\n%s %s\nLicense: MIT\n", sectionRule, d.Path, d.Version)
	}
	return b.String()
}

// newFixture lays out a repository, every artifact's fake binary and its
// notices for version.
func newFixture(t *testing.T, version string) *fixture {
	t.Helper()
	base := t.TempDir()
	fx := &fixture{
		root: filepath.Join(base, "repo"), dist: filepath.Join(base, "dist"),
		notices: filepath.Join(base, "dist", "notices"), out: filepath.Join(base, "release"),
		version: version,
		deps:    []*debug.Module{{Path: "example.com/Upper/mod", Version: "v1.2.3+incompatible"}},
		tweak:   map[string]func(*buildFacts){},
	}
	files := map[string]string{
		"LICENSE":                         "Apache License\nVersion 2.0, January 2004\n",
		"README.md":                       "# Ruralz\n",
		"NOTICE":                          fixtureNotice,
		"go.mod":                          "module github.com/ravindu-rev/ruralz\n\ngo 1.26.0\n\ntoolchain go1.27.3\n",
		"deploy/systemd/ruralzd.service":  "[Service]\nType=notify\n",
		"deploy/systemd/ruralzd-handover": "#!/usr/bin/env bash\nexit 0\n",
		"deploy/systemd/ruralzd.env":      "RURALZ_CONFIG=/etc/ruralzd/bundle\n",
		"deploy/systemd/99-ruralzd.conf":  "net.ipv4.tcp_migrate_req = 1\n",
		"deploy/systemd/ruralzd.sysusers": "u ruralz - \"Ruralz Gateway\" /var/lib/ruralz\n",
		"api/schema/ruralz/v1alpha1/authoring.schema.json": `{"title":"authoring"}` + "\n",
		"api/schema/ruralz/v1alpha1/rendered.schema.json":  `{"title":"rendered"}` + "\n",
	}
	for name, content := range files {
		writeFile(t, filepath.Join(fx.root, filepath.FromSlash(name)), content)
	}
	for _, a := range artifacts() {
		writeFile(t, filepath.Join(fx.dist, a.distDir(), a.executable()), "fake:"+a.Binary+":"+a.OS+":"+a.Arch)
		writeFile(t, filepath.Join(fx.notices, a.target(), noticeMember), fixtureNotice+"\nThird-party notices\n")
		writeFile(t, filepath.Join(fx.notices, a.target(), thirdPartyMember), thirdPartyText("go1.27.3", fx.deps))
	}
	return fx
}

// stubGit answers the commit time of fixtureCommit and fails otherwise.
func stubGit(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "show" && slices.Contains(args, fixtureCommit) {
		return []byte(strconv.FormatInt(fixtureTime().Unix(), 10) + "\n"), nil
	}
	return nil, fmt.Errorf("stub git: unexpected %q", args)
}

// facts returns stub build facts: a fake binary "fake:<binary>:<os>:<arch>"
// reads as a release build of that artifact at the fixture commit.
func (fx *fixture) facts() func([]byte) (buildFacts, error) {
	return func(data []byte) (buildFacts, error) {
		parts := strings.Split(string(data), ":")
		if len(parts) != 4 || parts[0] != "fake" {
			return buildFacts{}, errors.New("not a Go binary with build information")
		}
		bin, goos, goarch := parts[1], parts[2], parts[3]
		f := buildFacts{
			Info: &buildinfo.BuildInfo{
				GoVersion: "go1.27.3",
				Path:      modulePath + "/cmd/" + bin,
				Main:      debug.Module{Path: modulePath, Version: "v" + fx.version},
				Deps:      fx.deps,
				Settings: []debug.BuildSetting{
					{Key: "-trimpath", Value: "true"},
					{Key: "CGO_ENABLED", Value: "0"},
					{Key: "GOARCH", Value: goarch},
					{Key: "GOOS", Value: goos},
					{Key: "vcs.revision", Value: fixtureCommit},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			Stripped: true,
			Format:   formatFor(goos),
		}
		if tw := fx.tweak[bin+"_"+goos+"_"+goarch]; tw != nil {
			tw(&f)
		}
		return f, nil
	}
}

// options are the package flags of the fixture.
func (fx *fixture) options() packageOptions {
	return packageOptions{root: fx.root, dist: fx.dist, notices: fx.notices, out: fx.out, version: fx.version, commit: fixtureCommit}
}

func (fx *fixture) pkgDeps() packageDeps {
	return packageDeps{git: stubGit, facts: fx.facts()}
}

// sbomFor is a CycloneDX 1.7 SBOM naming the given modules.
func sbomFor(deps []*debug.Module) string {
	var comps []string
	for _, d := range deps {
		comps = append(comps, fmt.Sprintf(`{"type":"library","purl":"pkg:golang/%s@%s?type=module"}`,
			strings.ToLower(d.Path), strings.ReplaceAll(d.Version, "+", "%2B")))
	}
	return `{"bomFormat":"CycloneDX","specVersion":"1.7","components":[` + strings.Join(comps, ",") + `]}`
}

// release builds a complete release directory: package, checksums, the
// notes and an SBOM per archive.
func (fx *fixture) release(t *testing.T) {
	t.Helper()
	m, err := runPackage(t.Context(), fx.options(), fx.pkgDeps())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := runChecksums(fx.out)
	if err != nil || len(findings) > 0 {
		t.Fatalf("checksums: %v %v", findings, err)
	}
	writeFile(t, filepath.Join(fx.out, notesName), notesTitle(fx.version)+"\n\nnotes\n")
	for _, a := range m.Archives {
		writeFile(t, filepath.Join(fx.out, a.SBOM), sbomFor(fx.deps))
	}
}

// verifyFixture audits the fixture's release directory.
func (fx *fixture) verify(t *testing.T) []string {
	t.Helper()
	findings, err := runVerify(verifyOptions{root: fx.root, manifest: filepath.Join(fx.out, manifestName)}, verifyDeps{facts: fx.facts()})
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

// hasFinding reports whether a finding contains want.
func hasFinding(findings []string, want string) bool {
	return slices.ContainsFunc(findings, func(f string) bool { return strings.Contains(f, want) })
}
