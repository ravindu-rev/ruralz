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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// Notice file names in each target's directory.
const (
	// thirdPartyName lists every linked component with its license texts.
	thirdPartyName = "THIRD_PARTY_LICENSES"
	// noticeName is the repository NOTICE plus the upstream attributions
	// and license elections of the linked modules (ADR-0002).
	noticeName = "NOTICE"
)

// Separators of THIRD_PARTY_LICENSES and NOTICE sections. A component
// section starts with sectionRule followed by its heading line, which
// releasekit verify reads.
const (
	sectionRule = "================================================================================"
	textRule    = "--------------------------------------------------------------------------------"
)

// stdHeading is the heading of the Go standard library section; the Go
// version follows it.
const stdHeading = "Go standard library and runtime"

// standardLibrary is the Go distribution every binary links.
type standardLibrary struct {
	// Version is the go command's GOVERSION, such as "go1.27.3".
	Version string
	// Texts are GOROOT's LICENSE and PATENTS.
	Texts []legalText
}

// goStandardLibrary reads the version and legal texts of the toolchain
// that builds the module in dir, the one `go list` used.
func goStandardLibrary(ctx context.Context, dir string) (standardLibrary, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "GOVERSION", "GOROOT")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return standardLibrary{}, fmt.Errorf("go env: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return standardLibrary{}, fmt.Errorf("go env printed %q, want GOVERSION and GOROOT", out)
	}
	version, root := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	texts, err := readLegal(root)
	if err != nil {
		return standardLibrary{}, fmt.Errorf("reading the Go distribution's license: %w", err)
	}
	if !slices.ContainsFunc(texts, func(t legalText) bool { return t.Kind == kindLicense }) {
		return standardLibrary{}, fmt.Errorf("no LICENSE in GOROOT %s", root)
	}
	return standardLibrary{Version: version, Texts: texts}, nil
}

// emptyNoticesDir checks that the notices directory is absent or empty,
// so no stale notice of an earlier run survives.
func emptyNoticesDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("notices directory %s is not empty", dir)
	}
	return nil
}

// embedsConsole reports whether a binary embeds Ruralz Console, whose
// production build internal/console embeds into ruralz-control
// (docs/engineering/02-repository-layout-and-conventions.md, "Ruralz
// Console assets"), so the Console's npm packages are part of its
// notices.
func embedsConsole(t target) bool { return t.binary == "ruralz-control" }

// writeNotices writes THIRD_PARTY_LICENSES and NOTICE for every target
// into dir/<binary>_<os>_<arch>/, those of ruralz-control with the npm
// packages of Ruralz Console. dir must be absent or empty.
func writeNotices(dir string, ts []target, results map[target]analysis, std standardLibrary, notice string, console []moduleNotice) error {
	if err := emptyNoticesDir(dir); err != nil {
		return err
	}
	for _, t := range ts {
		a := results[t]
		var npm []moduleNotice
		if embedsConsole(t) {
			npm = console
		}
		sub := filepath.Join(dir, t.name())
		if err := os.MkdirAll(sub, 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(sub, thirdPartyName), []byte(renderThirdParty(t, std, a.modules, npm)), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(sub, noticeName), []byte(renderNotice(t, notice, a.modules, npm)), 0o600); err != nil { //nolint:gosec // The notices directory is the -notices argument and the names are fixed.
			return err
		}
	}
	return nil
}

// writeCrateNotices writes the THIRD_PARTY_LICENSES and NOTICE of a PDK
// package into dir, which must be absent or empty.
func writeCrateNotices(dir string, res cargoResult, notice string) error {
	if err := emptyNoticesDir(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, thirdPartyName), []byte(renderCrateThirdParty(res)), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, noticeName), []byte(renderCrateNotice(res, notice)), 0o600) //nolint:gosec // The notices directory is the -notices argument and the names are fixed.
}

// escapeModule escapes a module path or version for the module proxy
// protocol: each upper-case letter becomes "!" and its lower-case form.
func escapeModule(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			b.WriteByte('!')
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sourceOf says where a component's source is available: for a crate or
// npm package the address its analysis found, for a Go module the module
// proxy, which keeps every published version, so the address stays valid.
func sourceOf(m moduleNotice) string {
	if m.Source != "" {
		return m.Source
	}
	p, v := m.Path, m.Version
	if m.ReplacePath != "" {
		if m.ReplaceVersion == "" {
			return "a local directory replacement (" + m.ReplacePath + "), not published"
		}
		p, v = m.ReplacePath, m.ReplaceVersion
	}
	return "https://proxy.golang.org/" + escapeModule(p) + "/@v/" + escapeModule(v) + ".zip"
}

// licenseOffer is the license a component is offered under: a crate's or
// npm package's SPDX expression, or a Go module's classified licenses.
func licenseOffer(m moduleNotice) string {
	if m.Expression != "" {
		return m.Expression
	}
	return strings.Join(m.IDs, " or ")
}

// licenseSummary is the "License:" value of a component section.
func licenseSummary(m moduleNotice) string {
	s := licenseOffer(m)
	if s == "" {
		return "unknown"
	}
	switch {
	case m.Override != "":
		s += " (reviewed)"
	case m.Election != "":
		s += "; Ruralz elects " + m.Election
	}
	return s
}

// isMPL reports whether a component is used under MPL-2.0: a Go module
// with an MPL-2.0 text, or a crate or npm package whose expression
// resolves only through its MPL-2.0 exception.
func isMPL(m moduleNotice) bool { return m.MPL || slices.Contains(m.IDs, "MPL-2.0") }

// writeTexts appends the texts of the given kinds, each under its path.
func writeTexts(b *strings.Builder, texts []legalText, kinds ...string) {
	for _, t := range texts {
		if !slices.Contains(kinds, t.Kind) {
			continue
		}
		fmt.Fprintf(b, "%s:\n\n%s\n", t.Rel, t.Text)
	}
}

// writeComponentList appends the "Components:" lines of components.
func writeComponentList(b *strings.Builder, components []moduleNotice) {
	for _, m := range components {
		fmt.Fprintf(b, "  %s %s: %s\n", m.Path, m.Version, licenseSummary(m))
	}
}

// writeMPLStatement appends the statement on MPL-2.0 sources when any
// component is used under MPL-2.0.
func writeMPLStatement(b *strings.Builder, components []moduleNotice) {
	if slices.ContainsFunc(components, isMPL) {
		b.WriteString("\nMPL-2.0 components are used unmodified. The Source Code Form of each is\n")
		b.WriteString("available at the Source address of its section, under the MPL-2.0.\n")
	}
}

// writeSection appends one component's section: its heading, license,
// source and license and patent texts.
func writeSection(b *strings.Builder, m moduleNotice) {
	fmt.Fprintf(b, "\n%s\n%s %s\n", sectionRule, m.Path, m.Version)
	if m.ReplacePath != "" {
		fmt.Fprintf(b, "Replaced by: %s %s\n", m.ReplacePath, m.ReplaceVersion)
	}
	fmt.Fprintf(b, "License: %s\n", licenseSummary(m))
	fmt.Fprintf(b, "Source: %s\n", sourceOf(m))
	if isMPL(m) {
		b.WriteString("MPL-2.0 Source Code Form: the Source address above.\n")
	}
	b.WriteString(textRule + "\n")
	if !slices.ContainsFunc(m.Texts, func(t legalText) bool { return t.Kind == kindLicense }) {
		switch {
		case m.Expression == "":
			b.WriteString("The module ships no license text; its license was reviewed as stated above.\n")
		case m.Override != "":
			b.WriteString("The package ships no license text; its license was reviewed as stated above.\n")
		default:
			b.WriteString("The package ships no license text; its manifest declares the license above.\n")
		}
	}
	writeTexts(b, m.Texts, kindLicense, kindPatents)
}

// renderThirdParty renders one target's THIRD_PARTY_LICENSES: the Go
// standard library, then each linked module by path, then, for a binary
// that embeds Ruralz Console, each of its npm packages, each with where
// its source is available and its license and patent texts. The output
// depends only on its inputs, so a rebuild of a commit reproduces it.
func renderThirdParty(t target, std standardLibrary, modules, console []moduleNotice) string {
	var b strings.Builder
	fmt.Fprintf(&b, "THIRD_PARTY_LICENSES for %s on %s/%s\n\n", t.binary, t.goos, t.goarch)
	fmt.Fprintf(&b, "%s is licensed under the Apache License, Version 2.0 (see LICENSE and\n", t.binary)
	fmt.Fprintf(&b, "NOTICE). On %s/%s it links the Go standard library and runtime and the\n", t.goos, t.goarch)
	b.WriteString("third-party Go modules listed below, as `go list -deps -test=false` reports\n")
	b.WriteString("them with CGO_ENABLED=0. Each section names a component, its license and\n")
	b.WriteString("where its source is available, then reproduces its license texts.\n\n")
	if len(console) > 0 {
		b.WriteString("It also embeds Ruralz Console, whose bundle holds the npm packages listed\n")
		b.WriteString("below after the Go modules, as `pnpm licenses list --prod` reports them.\n\n")
	}
	b.WriteString("Components:\n")
	fmt.Fprintf(&b, "  %s %s: %s\n", stdHeading, std.Version, stdLicense(std))
	all := slices.Concat(modules, console)
	writeComponentList(&b, all)
	writeMPLStatement(&b, all)

	fmt.Fprintf(&b, "\n%s\n%s %s\n", sectionRule, stdHeading, std.Version)
	fmt.Fprintf(&b, "License: %s\n", stdLicense(std))
	fmt.Fprintf(&b, "Source: https://go.dev/dl/%s.src.tar.gz\n%s\n", std.Version, textRule)
	writeTexts(&b, std.Texts, kindLicense, kindPatents)

	for _, m := range all {
		writeSection(&b, m)
	}
	return b.String()
}

// renderCrateThirdParty renders the THIRD_PARTY_LICENSES of a PDK package:
// each crate it ships by name and version, with where its source is
// available and its license and patent texts.
func renderCrateThirdParty(res cargoResult) string {
	verb, pronoun := "is", "it"
	if res.plural {
		verb, pronoun = "are", "them"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "THIRD_PARTY_LICENSES for %s\n\n", res.title)
	fmt.Fprintf(&b, "%s %s licensed under the Apache License, Version 2.0\n", res.title, verb)
	fmt.Fprintf(&b, "(see LICENSE and NOTICE). A Plugin built with %s links the third-party\n", pronoun)
	b.WriteString("crates listed below: the packages its normal dependencies resolve to, as\n")
	b.WriteString("`cargo metadata` reports them, without proc-macro crates, build-dependencies,\n")
	b.WriteString("dev-dependencies and what only they reach, which never ship. Each section\n")
	b.WriteString("names a component, its license and where its source is available, then\n")
	b.WriteString("reproduces its license texts.\n\n")
	b.WriteString("Components:\n")
	if len(res.crates) == 0 {
		b.WriteString("  None.\n")
	}
	writeComponentList(&b, res.crates)
	writeMPLStatement(&b, res.crates)
	for _, m := range res.crates {
		writeSection(&b, m)
	}
	return b.String()
}

// stdLicense classifies the Go distribution's LICENSE.
func stdLicense(std standardLibrary) string {
	for _, t := range std.Texts {
		if t.Kind == kindLicense {
			return classify(t.Text)
		}
	}
	return "unknown"
}

// withThirdPartyNotices returns the repository NOTICE followed by the
// NOTICE texts of the components and the license elections, which
// Apache-2.0 section 4(d) and ADR-0002 require in an artifact's NOTICE,
// after intro. Without either, it is the repository NOTICE unchanged.
func withThirdPartyNotices(notice, intro string, components []moduleNotice) string {
	out := normalizeText(notice)
	var withNotice, elected []moduleNotice
	for _, m := range components {
		if slices.ContainsFunc(m.Texts, func(x legalText) bool { return x.Kind == kindNotice }) {
			withNotice = append(withNotice, m)
		}
		if m.Election != "" {
			elected = append(elected, m)
		}
	}
	if len(withNotice) == 0 && len(elected) == 0 {
		return out
	}
	var b strings.Builder
	b.WriteString(out)
	fmt.Fprintf(&b, "\n%s\nThird-party notices\n\n%s", sectionRule, intro)
	for _, m := range withNotice {
		fmt.Fprintf(&b, "\n%s\n%s %s\n%s\n", textRule, m.Path, m.Version, textRule)
		writeTexts(&b, m.Texts, kindNotice)
	}
	if len(elected) > 0 {
		fmt.Fprintf(&b, "\n%s\nLicense elections\n%s\n", textRule, textRule)
		for _, m := range elected {
			fmt.Fprintf(&b, "%s %s is offered under %s; Ruralz elects %s.\n", m.Path, m.Version, licenseOffer(m), m.Election)
		}
	}
	return b.String()
}

// renderNotice renders one target's NOTICE: the repository NOTICE, then
// the NOTICE texts of the linked modules, and of the npm packages of an
// embedded Ruralz Console, and the license elections.
func renderNotice(t target, notice string, modules, console []moduleNotice) string {
	intro := fmt.Sprintf("%s on %s/%s includes third-party software; THIRD_PARTY_LICENSES holds\n", t.binary, t.goos, t.goarch) +
		"the licenses. The notices of the linked modules and the license elections\n" +
		"follow.\n"
	if len(console) > 0 {
		intro = fmt.Sprintf("%s on %s/%s includes third-party software; THIRD_PARTY_LICENSES holds\n", t.binary, t.goos, t.goarch) +
			"the licenses. The notices of the linked modules and of the npm packages of\n" +
			"the embedded Ruralz Console, and the license elections, follow.\n"
	}
	return withThirdPartyNotices(notice, intro, slices.Concat(modules, console))
}

// renderCrateNotice renders a PDK package's NOTICE: the repository NOTICE,
// then the NOTICE texts of the crates it ships and the license elections.
func renderCrateNotice(res cargoResult, notice string) string {
	intro := fmt.Sprintf("A Plugin built with %s includes third-party software;\n", res.title) +
		"THIRD_PARTY_LICENSES holds the licenses. The notices of the crates it ships\n" +
		"and the license elections follow.\n"
	return withThirdPartyNotices(notice, intro, res.crates)
}
