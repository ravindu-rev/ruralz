// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestRun checks that the command writes exactly the committed files, as
// `make generate` does (WP-28 "Done when").
func TestRun(t *testing.T) {
	out := filepath.Join(t.TempDir(), "schema")
	if err := run(pkgDir, out); err != nil {
		t.Fatal(err)
	}
	for _, name := range outputs() {
		got, err := os.ReadFile(filepath.Join(out, name)) //nolint:gosec // Test reads files it wrote.
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(schemaDir, name)) //nolint:gosec // Test reads the committed schema files.
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the committed file", name)
		}
	}
}

func TestRunErrors(t *testing.T) {
	cases := []struct {
		name, src, out, want string
	}{
		{"no out", pkgDir, "", "-out is required"},
		{"no sources", t.TempDir(), t.TempDir(), "no Go files"},
	}
	for _, c := range cases {
		err := run(c.src, c.out)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	if err := run(filepath.Join(t.TempDir(), "absent"), t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing source directory: err = %v, want fs.ErrNotExist", err)
	}
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "x.go"), []byte("package x\nfunc {"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(bad, t.TempDir()); err == nil {
		t.Error("a source that does not parse was accepted")
	}
}

// TestSourceFilesSkipsTests checks that only non-test Go files are read.
func TestSourceFilesSkipsTests(t *testing.T) {
	files, err := sourceFiles(pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || !strings.HasSuffix(f, ".go") {
			t.Errorf("source file %s", f)
		}
	}
}

// TestPolicyDispatchErrors covers the Policy type table checks.
func TestPolicyDispatchErrors(t *testing.T) {
	in := realInput(t)
	all := policyConfigTypes()
	cases := []struct {
		name      string
		resources []reflect.Type
		configs   []reflect.Type
		want      string
	}{
		{"missing config", in.resources, all[1:], "is not in the config table"},
		{"duplicate config", in.resources, append(append([]reflect.Type{}, all...), all[0]), "has two configs"},
		{"unmarked config", in.resources, append(append([]reflect.Type{}, all...), reflect.TypeFor[v1alpha1.JWTIssuer]()), "lacks +ruralz:policyType"},
		{"no PolicySpec", []reflect.Type{reflect.TypeFor[v1alpha1.Gateway]()}, all, "no PolicySpec"},
		{"not a kind", []reflect.Type{reflect.TypeFor[v1alpha1.GatewaySpec]()}, nil, "is not a Kind constant"},
		{"no resources", nil, nil, "no resource types"},
	}
	for _, c := range cases {
		_, err := generate(input{source: in.source, resources: c.resources, configs: c.configs}, rendered)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// TestTagErrors covers the json tag rules.
func TestTagErrors(t *testing.T) {
	info, err := parseSources([]string{"fixture_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	for typ, want := range map[reflect.Type]string{
		reflect.TypeFor[MissingTag](): "missing json tag",
		reflect.TypeFor[DashTag]():    "must name the field",
	} {
		_, err := generate(input{source: info, resources: []reflect.Type{typ}}, rendered)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", typ.Name(), err, want)
		}
	}
}
