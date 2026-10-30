// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/buildinfo"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
)

// versionInfo is what ruralz version prints
// (docs/reference/01-cli-and-api-surface.md "Output formats and exit
// codes"): the build metadata, the configuration apiVersions the binary
// serves and, for each, the highest schema level it serves. pluginAbi and
// controlStream name contracts the binary serves, so this build, which
// serves neither Plugin ABI v1 nor the Control Stream (Planned (M2)),
// omits them.
type versionInfo struct {
	Version      string         `json:"version"`
	Commit       string         `json:"commit"`
	Flavor       string         `json:"flavor"`
	APIVersions  []string       `json:"apiVersions"`
	SchemaLevels map[string]int `json:"schemaLevels"`
}

// versionSpec returns the ruralz version command.
func versionSpec(o Options) command.Spec {
	return command.Spec{
		Path:    []string{"version"},
		Summary: "Version, commit, flavor, levels",
		New:     func() command.Command { return &versionCommand{opts: o} },
	}
}

// versionCommand is one run of ruralz version.
type versionCommand struct {
	opts   Options
	output string
}

// Flags binds --output text|json.
func (c *versionCommand) Flags(f *command.Flags) { f.Output(&c.output, "text", "text", "json") }

// Run prints the build metadata.
func (c *versionCommand) Run(_ context.Context, sio command.IO, _ []string) error {
	info, err := c.info()
	if err != nil {
		return command.NoResult(err)
	}
	if c.output == "json" {
		return command.WriteJSON(sio.Stdout, info)
	}
	if err := writeVersionText(sio, info); err != nil {
		return command.NoResultf("write standard output: %w", err)
	}
	return nil
}

// info gathers the build metadata of this binary: buildinfo's version,
// commit, flavor and served apiVersions, a set Version option replacing
// the version, and the schema level of each served apiVersion from the
// embedded schemas (schemaidx.Index.Level).
func (c *versionCommand) info() (versionInfo, error) {
	bi := buildinfo.Get()
	if c.opts.Version != "" {
		bi.Version = c.opts.Version
	}
	set, err := schemaidx.Embedded()
	if err != nil {
		return versionInfo{}, fmt.Errorf("load the schemas this CLI serves: %w", err)
	}
	levels := map[string]int{}
	for _, v := range bi.APIVersions {
		if idx, ok := set.Index(v); ok {
			levels[v] = idx.Level()
		}
	}
	return versionInfo{
		Version: bi.Version, Commit: bi.Commit, Flavor: bi.Flavor,
		APIVersions: bi.APIVersions, SchemaLevels: levels,
	}, nil
}

// writeVersionText writes info as aligned "key: value" lines; the schema
// levels print as "<apiVersion>=<level>", such as ruralz/v1alpha1=0.
func writeVersionText(sio command.IO, info versionInfo) error {
	versions := slices.Sorted(maps.Keys(info.SchemaLevels))
	levels := make([]string, 0, len(versions))
	for _, v := range versions {
		levels = append(levels, v+"="+strconv.Itoa(info.SchemaLevels[v]))
	}
	rows := [][2]string{
		{"version", info.Version},
		{"commit", info.Commit},
		{"flavor", info.Flavor},
		{"apiVersions", strings.Join(info.APIVersions, ", ")},
		{"schemaLevels", strings.Join(levels, ", ")},
	}
	width := 0
	for _, r := range rows {
		width = max(width, len(r[0]))
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r[0])
		b.WriteString(":")
		b.WriteString(strings.Repeat(" ", width-len(r[0])+1))
		b.WriteString(r[1])
		b.WriteString("\n")
	}
	_, err := sio.Stdout.Write([]byte(b.String()))
	return err
}
