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

// writeNotices writes THIRD_PARTY_LICENSES and NOTICE for every target
// into dir/<binary>_<os>_<arch>/. dir must be absent or empty, so no stale
// notice of an earlier run survives.
func writeNotices(dir string, ts []target, results map[target]analysis, std standardLibrary, notice string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("notices directory %s is not empty", dir)
	}
	for _, t := range ts {
		a := results[t]
		sub := filepath.Join(dir, t.name())
		if err := os.MkdirAll(sub, 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(sub, thirdPartyName), []byte(renderThirdParty(t, std, a.modules)), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(sub, noticeName), []byte(renderNotice(t, notice, a.modules)), 0o600); err != nil { //nolint:gosec // The notices directory is the -notices argument and the names are fixed.
			return err
		}
	}
	return nil
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

// sourceOf says where a module's source is available: the module proxy
// keeps every published version, so the address stays valid.
func sourceOf(m moduleNotice) string {
	p, v := m.Path, m.Version
	if m.ReplacePath != "" {
		if m.ReplaceVersion == "" {
			return "a local directory replacement (" + m.ReplacePath + "), not published"
		}
		p, v = m.ReplacePath, m.ReplaceVersion
	}
	return "https://proxy.golang.org/" + escapeModule(p) + "/@v/" + escapeModule(v) + ".zip"
}

// licenseSummary is the "License:" value of a module section.
func licenseSummary(m moduleNotice) string {
	if len(m.IDs) == 0 {
		return "unknown"
	}
	s := strings.Join(m.IDs, " or ")
	switch {
	case m.Override != "":
		s += " (reviewed)"
	case m.Election != "":
		s += "; Ruralz elects " + m.Election
	}
	return s
}

// isMPL reports whether a module carries an MPL-2.0 text.
func isMPL(m moduleNotice) bool { return slices.Contains(m.IDs, "MPL-2.0") }

// writeTexts appends the texts of the given kinds, each under its path.
func writeTexts(b *strings.Builder, texts []legalText, kinds ...string) {
	for _, t := range texts {
		if !slices.Contains(kinds, t.Kind) {
			continue
		}
		fmt.Fprintf(b, "%s:\n\n%s\n", t.Rel, t.Text)
	}
}

// renderThirdParty renders one target's THIRD_PARTY_LICENSES: the Go
// standard library, then each linked module by path, each with where its
// source is available and its license and patent texts. The output depends
// only on its inputs, so a rebuild of a commit reproduces it.
func renderThirdParty(t target, std standardLibrary, modules []moduleNotice) string {
	var b strings.Builder
	fmt.Fprintf(&b, "THIRD_PARTY_LICENSES for %s on %s/%s\n\n", t.binary, t.goos, t.goarch)
	fmt.Fprintf(&b, "%s is licensed under the Apache License, Version 2.0 (see LICENSE and\n", t.binary)
	fmt.Fprintf(&b, "NOTICE). On %s/%s it links the Go standard library and runtime and the\n", t.goos, t.goarch)
	b.WriteString("third-party Go modules listed below, as `go list -deps -test=false` reports\n")
	b.WriteString("them with CGO_ENABLED=0. Each section names a component, its license and\n")
	b.WriteString("where its source is available, then reproduces its license texts.\n\n")
	b.WriteString("Components:\n")
	fmt.Fprintf(&b, "  %s %s: %s\n", stdHeading, std.Version, stdLicense(std))
	for _, m := range modules {
		fmt.Fprintf(&b, "  %s %s: %s\n", m.Path, m.Version, licenseSummary(m))
	}
	if slices.ContainsFunc(modules, isMPL) {
		b.WriteString("\nMPL-2.0 components are used unmodified. The Source Code Form of each is\n")
		b.WriteString("available at the Source address of its section, under the MPL-2.0.\n")
	}

	fmt.Fprintf(&b, "\n%s\n%s %s\n", sectionRule, stdHeading, std.Version)
	fmt.Fprintf(&b, "License: %s\n", stdLicense(std))
	fmt.Fprintf(&b, "Source: https://go.dev/dl/%s.src.tar.gz\n%s\n", std.Version, textRule)
	writeTexts(&b, std.Texts, kindLicense, kindPatents)

	for _, m := range modules {
		fmt.Fprintf(&b, "\n%s\n%s %s\n", sectionRule, m.Path, m.Version)
		if m.ReplacePath != "" {
			fmt.Fprintf(&b, "Replaced by: %s %s\n", m.ReplacePath, m.ReplaceVersion)
		}
		fmt.Fprintf(&b, "License: %s\n", licenseSummary(m))
		fmt.Fprintf(&b, "Source: %s\n", sourceOf(m))
		if isMPL(m) {
			b.WriteString("MPL-2.0 Source Code Form: the Source address above.\n")
		}
		b.WriteString(textRule + "\n")
		if !slices.ContainsFunc(m.Texts, func(t legalText) bool { return t.Kind == kindLicense }) {
			b.WriteString("The module ships no license text; its license was reviewed as stated above.\n")
		}
		writeTexts(&b, m.Texts, kindLicense, kindPatents)
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

// renderNotice renders one target's NOTICE: the repository NOTICE, then
// the NOTICE texts of linked modules and the license elections, which
// Apache-2.0 section 4(d) and ADR-0002 require in the artifact's NOTICE.
// Without either, it is the repository NOTICE unchanged.
func renderNotice(t target, notice string, modules []moduleNotice) string {
	out := normalizeText(notice)
	var withNotice, elected []moduleNotice
	for _, m := range modules {
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
	fmt.Fprintf(&b, "\n%s\nThird-party notices\n\n", sectionRule)
	fmt.Fprintf(&b, "%s on %s/%s includes third-party software; THIRD_PARTY_LICENSES holds\n", t.binary, t.goos, t.goarch)
	b.WriteString("the licenses. The notices of the linked modules and the license elections\n")
	b.WriteString("follow.\n")
	for _, m := range withNotice {
		fmt.Fprintf(&b, "\n%s\n%s %s\n%s\n", textRule, m.Path, m.Version, textRule)
		writeTexts(&b, m.Texts, kindNotice)
	}
	if len(elected) > 0 {
		fmt.Fprintf(&b, "\n%s\nLicense elections\n%s\n", textRule, textRule)
		for _, m := range elected {
			fmt.Fprintf(&b, "%s %s is offered under %s; Ruralz elects %s.\n", m.Path, m.Version, strings.Join(m.IDs, " or "), m.Election)
		}
	}
	return b.String()
}
