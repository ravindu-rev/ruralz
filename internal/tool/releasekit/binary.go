// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"fmt"
	"slices"
	"strings"
)

// buildFacts are what the release audit reads from a binary: its Go build
// information and whether symbols and DWARF were stripped.
type buildFacts struct {
	Info     *buildinfo.BuildInfo
	Stripped bool
	// Format is "elf", "macho" or "pe".
	Format string
}

// readBuildFacts reads a binary's build information and symbol state.
func readBuildFacts(data []byte) (buildFacts, error) {
	info, err := buildinfo.Read(bytes.NewReader(data))
	if err != nil {
		return buildFacts{}, fmt.Errorf("not a Go binary with build information: %w", err)
	}
	f := buildFacts{Info: info}
	f.Format, f.Stripped, err = symbolState(data)
	return f, err
}

// symbolState reports a binary's executable format and whether -s -w
// removed its symbol table and DWARF: an ELF without .symtab or debug
// sections, a Mach-O without a __DWARF segment or defined symbols (its
// undefined dynamic imports stay), a PE without COFF symbols or debug
// sections.
func symbolState(data []byte) (format string, stripped bool, err error) {
	debugSection := func(name string) bool {
		return strings.HasPrefix(name, ".debug_") || strings.HasPrefix(name, ".zdebug_") ||
			strings.HasPrefix(name, "__debug_") || strings.HasPrefix(name, "__zdebug_")
	}
	if f, err := elf.NewFile(bytes.NewReader(data)); err == nil {
		stripped := !slices.ContainsFunc(f.Sections, func(s *elf.Section) bool {
			return s.Name == ".symtab" || debugSection(s.Name)
		})
		return "elf", stripped, nil
	}
	if f, err := macho.NewFile(bytes.NewReader(data)); err == nil {
		stripped := f.Segment("__DWARF") == nil && !slices.ContainsFunc(f.Sections, func(s *macho.Section) bool { return debugSection(s.Name) })
		if f.Symtab != nil {
			const nType, nUndf = 0x0e, 0x0
			for _, s := range f.Symtab.Syms {
				if s.Type&nType != nUndf {
					stripped = false
					break
				}
			}
		}
		return "macho", stripped, nil
	}
	if f, err := pe.NewFile(bytes.NewReader(data)); err == nil {
		stripped := f.NumberOfSymbols == 0 && !slices.ContainsFunc(f.Sections, func(s *pe.Section) bool { return debugSection(s.Name) })
		return "pe", stripped, nil
	}
	return "", false, fmt.Errorf("not an ELF, Mach-O or PE executable")
}

// formatFor is the executable format of an OS.
func formatFor(goos string) string {
	switch goos {
	case "darwin":
		return "macho"
	case "windows":
		return "pe"
	default:
		return "elf"
	}
}

// buildProblems checks a binary against the release build rules
// (docs/engineering/01-tech-stack-and-libraries.md, "Version floor" and
// "Static builds"; stage 4 of docs/engineering/02-repository-layout-and-conventions.md):
// the binary's main package and platform, the pinned toolchain without a
// GOEXPERIMENT, CGO_ENABLED=0, -trimpath, no build tags, -s -w, and, for a
// release or candidate, a clean checkout of the tagged commit. With
// -trimpath the go command does not record -ldflags, so the -X metadata is
// checked through the commit and module version that Go stamps from Git.
func buildProblems(f buildFacts, a artifact, version, commit, toolchain string) []string {
	var out []string
	bi := f.Info
	if want := modulePath + "/cmd/" + a.Binary; bi.Path != want {
		out = append(out, fmt.Sprintf("main package %s, want %s", bi.Path, want))
	}
	if bi.Main.Path != modulePath {
		out = append(out, fmt.Sprintf("main module %s, want %s", bi.Main.Path, modulePath))
	}
	if toolchain != "" && bi.GoVersion != toolchain {
		out = append(out, fmt.Sprintf("built with %s, want the pinned toolchain %s", bi.GoVersion, toolchain))
	}
	settings := map[string]string{}
	for _, s := range bi.Settings {
		settings[s.Key] = s.Value
	}
	want := map[string]string{"GOOS": a.OS, "GOARCH": a.Arch, "CGO_ENABLED": "0", "-trimpath": "true"}
	for _, k := range []string{"GOOS", "GOARCH", "CGO_ENABLED", "-trimpath"} {
		if settings[k] != want[k] {
			out = append(out, fmt.Sprintf("build setting %s=%q, want %q", k, settings[k], want[k]))
		}
	}
	for _, k := range []string{"GOEXPERIMENT", "-tags"} {
		if v := settings[k]; v != "" {
			out = append(out, fmt.Sprintf("build setting %s=%q, want none", k, v))
		}
	}
	if f.Format != formatFor(a.OS) {
		out = append(out, fmt.Sprintf("%s executable, want %s for %s", f.Format, formatFor(a.OS), a.OS))
	}
	if !f.Stripped {
		out = append(out, "symbol table or DWARF present: build with -ldflags -s -w")
	}
	rev, hasRev := settings["vcs.revision"]
	if hasRev && rev != commit {
		out = append(out, fmt.Sprintf("built from commit %s, want %s", rev, commit))
	}
	if versionKind(version) != kindDev {
		switch {
		case !hasRev:
			out = append(out, "no Git commit recorded; build releases from a Git checkout")
		case settings["vcs.modified"] != "false":
			out = append(out, "built from a modified checkout; build releases from a clean one")
		}
		if bi.Main.Version != "v"+version {
			out = append(out, fmt.Sprintf("module version %s, want v%s from the release tag", bi.Main.Version, version))
		}
	}
	return out
}
