// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command schemagen generates the authoring and rendered views of the
// ruralz/v1alpha1 JSON Schema from the Go types in pkg/config/v1alpha1, and
// the served-feature table internal/config/registry/planned_gen.go from
// their +ruralz:planned markers, checked against the release milestone
// that package declares. It runs through `go generate` in that package
// directory and uses only the standard library.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	src := flag.String("src", ".", "directory of the pkg/config/v1alpha1 Go sources")
	out := flag.String("out", "", "output directory for the schema files")
	registry := flag.String("registry", "", "directory of internal/config/registry: its "+releaseConst+
		" constant is read and the served-feature table "+plannedFile+" written there")
	flag.Parse()
	if err := run(*src, *out, *registry); err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
}

// outputs maps each view to its file name.
func outputs() map[view]string {
	return map[view]string{
		authoring: "authoring.schema.json",
		rendered:  "rendered.schema.json",
	}
}

// run writes both schema views into out and the served-feature table
// into registry. Every output is computed before the first is written, so
// a refused marker leaves the committed files as they were.
func run(src, out, registry string) error {
	switch {
	case out == "":
		return errors.New("-out is required")
	case registry == "":
		return errors.New("-registry is required")
	}
	files, err := sourceFiles(src)
	if err != nil {
		return err
	}
	info, err := parseSources(files)
	if err != nil {
		return err
	}
	in := input{source: info, resources: resourceTypes(), configs: policyConfigTypes()}
	views := map[string][]byte{}
	for v, name := range outputs() {
		data, err := generate(in, v)
		if err != nil {
			return fmt.Errorf("%s view: %w", v, err)
		}
		views[name] = data
	}
	release, err := readRelease(registry)
	if err != nil {
		return err
	}
	rows, err := plannedTable(in, release)
	if err != nil {
		return fmt.Errorf("served-feature table: %w", err)
	}
	if err := os.MkdirAll(out, 0o750); err != nil {
		return err
	}
	for name, data := range views {
		if err := os.WriteFile(filepath.Join(out, name), data, 0o644); err != nil { //nolint:gosec // Schema files are published and world-readable by design.
			return err
		}
	}
	return os.WriteFile(filepath.Join(registry, plannedFile), plannedSource(rows), 0o644) //nolint:gosec // A Go source file of the repository, world-readable like every other.
}

// sourceFiles lists the non-test Go files of the package, sorted.
func sourceFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("no Go files in %s", dir)
	}
	return files, nil
}
