// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package buildinfo reports the version, commit and build flavor that the
// release build embeds in every Ruralz binary, plus the contract levels the
// binary serves.
//
// The three package-level variables are the one exception to the rule
// against mutable package-level state: the linker sets them through
// -ldflags -X (see the Makefile), and nothing assigns them at run time.
package buildinfo

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Set by the linker: -X github.com/ravindu-rev/ruralz/internal/buildinfo.version=...
var (
	version = "0.0.0-dev"
	commit  = "unknown"
	flavor  = "default"
)

// APIVersion is the only configuration apiVersion this build serves.
const APIVersion = "ruralz/v1alpha1"

// Info is the build metadata printed by `ruralz version`.
type Info struct {
	// Version is the product SemVer without the leading "v".
	Version string `json:"version"`
	// Commit is the full Git commit the binary was built from.
	Commit string `json:"commit"`
	// Flavor is the build flavor: "default", or "fips" from M5.
	Flavor string `json:"flavor"`
	// APIVersions lists the configuration apiVersions the binary serves.
	APIVersions []string `json:"apiVersions"`
}

// Get returns the build metadata of the running binary.
func Get() Info {
	return Info{
		Version:     version,
		Commit:      commit,
		Flavor:      flavor,
		APIVersions: []string{APIVersion},
	}
}

// WriteText writes the metadata as aligned "key: value" lines.
func (i Info) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "version:     %s\ncommit:      %s\nflavor:      %s\napiVersions: %s\n",
		i.Version, i.Commit, i.Flavor, strings.Join(i.APIVersions, ", "))
	return err
}

// WriteJSON writes the metadata as one indented JSON object.
func (i Info) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(i)
}
