// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// CodeMissing is the code of an Environment's explicit spec.overlay
// naming an overlay directory the Bundle does not have (01 req 23, risk
// 18).
const CodeMissing = "RZ-CFG-009"

// Root is the Bundle-relative directory holding the overlays; base
// discovery skips it (01 req 3).
const Root = "overlays/"

// Selection is the one overlay a render applies, overlays/<Name>/ (01 req
// 23): an Environment's spec.overlay, or its metadata.name when
// spec.overlay is absent. The loader discovers and orders the
// directory's files as it does base files (01 req 3 to 5) and passes
// their documents to Apply.
type Selection struct {
	// Name is the overlay directory name.
	Name string
	// Explicit reports that spec.overlay was set rather than defaulted.
	Explicit bool
	// Environment is the Environment's metadata.name.
	Environment string
	// Location is where spec.overlay is written in the --environments
	// file; used when Explicit.
	Location diag.Location
}

// ValidName reports whether name is one path segment matching
// ^[A-Za-z0-9][A-Za-z0-9._-]*$, the rule of spec.overlay (01 req 21), so
// an overlay never names a directory outside overlays/.
func ValidName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !alnum && (i == 0 || (c != '.' && c != '_' && c != '-')) {
			return false
		}
	}
	return true
}

// Dir returns the overlay directory relative to the Bundle root, with a
// trailing slash: "overlays/prod/".
func (s Selection) Dir() string { return Root + s.Name + "/" }

// Contains reports whether the slash-separated root-relative path lies
// in the selected overlay directory.
func (s Selection) Contains(path string) bool {
	return ValidName(s.Name) && strings.HasPrefix(path, s.Dir())
}

// Absent returns the diagnostics for a selected overlay directory that
// does not exist (01 req 23, risk 18): none when spec.overlay was
// defaulted (the overlay is then empty), and RZ-CFG-009 at spec.overlay
// when it was set explicitly, hinting the nearest of the existing overlay
// names.
func (s Selection) Absent(existing []string) diag.List {
	if !s.Explicit {
		return nil
	}
	d := diag.Diagnostic{
		Code: CodeMissing, Severity: diag.SeverityError, Location: s.Location,
		Path:    diag.Path{diag.Field("spec"), diag.Field("overlay")},
		Message: "overlay directory " + strconv.Quote(s.Dir()) + " does not exist",
	}
	if s.Environment != "" {
		d.Resource = &diag.ResourceID{Kind: "Environment", Name: s.Environment}
	}
	if near, ok := diag.Nearest(s.Name, existing); ok {
		d.Hint = "did you mean " + strconv.Quote(near) + "?"
	}
	return diag.List{d}
}
