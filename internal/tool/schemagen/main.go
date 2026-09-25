// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Command schemagen generates the authoring and rendered views of the
// ruralz/v1alpha1 JSON Schema from the Go types in pkg/config/v1alpha1. It
// runs through `go generate` in that package directory and uses only the
// standard library.
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
	flag.Parse()
	if err := run(*src, *out); err != nil {
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

func run(src, out string) error {
	if out == "" {
		return errors.New("-out is required")
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
	if err := os.MkdirAll(out, 0o750); err != nil {
		return err
	}
	for v, name := range outputs() {
		data, err := generate(in, v)
		if err != nil {
			return fmt.Errorf("%s view: %w", v, err)
		}
		if err := os.WriteFile(filepath.Join(out, name), data, 0o644); err != nil { //nolint:gosec // Schema files are published and world-readable by design.
			return err
		}
	}
	return nil
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
