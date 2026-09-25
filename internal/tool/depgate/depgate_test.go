// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"Apache License\nVersion 2.0, January 2004":                                                               "Apache-2.0",
		"Permission is hereby granted, free of charge, to any person":                                             "MIT",
		"Redistribution and use in source and binary forms ... Neither the name of Google":                        "BSD-3-Clause",
		"Redistribution and use in source and binary forms, with or without\nmodification, are permitted":         "BSD-2-Clause",
		"Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee":    "ISC",
		"Mozilla Public License Version 2.0":                                                                      "MPL-2.0",
		"GNU GENERAL PUBLIC LICENSE Version 3":                                                                    "GPL",
		"GNU AFFERO GENERAL PUBLIC LICENSE":                                                                       "AGPL",
		"GNU LESSER GENERAL PUBLIC LICENSE":                                                                       "LGPL",
		"Server Side Public License":                                                                              "SSPL",
		"Business Source License 1.1":                                                                             "BUSL",
		"Redis Source Available License 2.0":                                                                      "RSAL",
		"Eclipse Public License - v 2.0":                                                                          "EPL",
		"Eclipse Distribution License - v 1.0 ... Redistribution and use in source and binary forms":              "EDL-1.0",
		"All rights reserved.":                                                                                    "unknown",
		"This GNU General Public License text also says Permission is hereby granted, free of charge, somewhere.": "GPL",
	}
	for text, want := range cases {
		if got := classify(text); got != want {
			t.Errorf("classify(%q) = %s, want %s", text, got, want)
		}
	}
}

func fakeReader(licenses map[string][]string) licenseReader {
	return func(dir string) ([]string, error) {
		if dir == "broken" {
			return nil, errors.New("read error")
		}
		return licenses[dir], nil
	}
}

func mod(path, version string) *goModule {
	return &goModule{Path: path, Version: version, Dir: path}
}

func TestAnalyze(t *testing.T) {
	read := fakeReader(map[string][]string{
		"example.com/mit":                     {"MIT"},
		"example.com/gpl":                     {"GPL"},
		"example.com/dual":                    {"Apache-2.0", "GPL"},
		"example.com/mpl":                     {"MPL-2.0"},
		"github.com/hashicorp/raft":           {"MPL-2.0"},
		"github.com/eclipse/paho.golang":      {"EDL-1.0", "EPL"},
		"github.com/gorilla/websocket":        {"BSD-2-Clause"},
		"example.com/crypto":                  {"MIT"},
		"golang.org/x/crypto":                 {"BSD-3-Clause"},
		"github.com/quic-go/quic-go":          {"MIT"},
		"example.com/replacement":             {"MIT"},
		"github.com/hashicorp/raft-boltdb/v2": {"MPL-2.0"},
	})
	pkgs := []goPackage{
		{ImportPath: "fmt", Standard: true},
		{ImportPath: "github.com/ravindu-rev/ruralz/internal/cli", Module: &goModule{Path: "github.com/ravindu-rev/ruralz", Main: true}},
		{ImportPath: "example.com/mit/a", Module: mod("example.com/mit", "v1.0.0")},
		{ImportPath: "example.com/mit/b", Module: mod("example.com/mit", "v1.0.0")},
		{ImportPath: "example.com/gpl", Module: mod("example.com/gpl", "v1.0.0")},
		{ImportPath: "example.com/dual", Module: mod("example.com/dual", "v1.0.0")},
		{ImportPath: "example.com/mpl", Module: mod("example.com/mpl", "v1.0.0")},
		{ImportPath: "github.com/hashicorp/raft", Module: mod("github.com/hashicorp/raft", "v1.8.0")},
		{ImportPath: "github.com/hashicorp/raft-boltdb/v2", Module: mod("github.com/hashicorp/raft-boltdb/v2", "v2.4.1")},
		{ImportPath: "github.com/eclipse/paho.golang/paho", Module: mod("github.com/eclipse/paho.golang", "v0.23.0")},
		{ImportPath: "github.com/gorilla/websocket", Module: mod("github.com/gorilla/websocket", "v1.5.1")},
		{ImportPath: "example.com/crypto/sign", Module: mod("example.com/crypto", "v1.0.0")},
		{ImportPath: "golang.org/x/crypto/chacha20", Module: mod("golang.org/x/crypto", "v0.40.0")},
		{ImportPath: "github.com/quic-go/quic-go/internal/handshake/crypto", Module: mod("github.com/quic-go/quic-go", "v0.63.0")},
		{ImportPath: "example.com/replaced", Module: &goModule{Path: "example.com/replaced", Version: "v1.0.0", Replace: mod("example.com/replacement", "")}},
		{ImportPath: "example.com/nodir", Module: &goModule{Path: "example.com/nodir", Version: "v1.0.0"}},
		{ImportPath: "example.com/nolicense", Module: mod("example.com/nolicense", "v1.0.0")},
	}
	got, err := analyze(target{"ruralzd", "linux", "amd64"}, pkgs, read)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"example.com/gpl: license GPL is not allowed",
		"example.com/mpl: license MPL-2.0 is not allowed",
		"github.com/hashicorp/raft: ruralzd never links Raft",
		"github.com/hashicorp/raft-boltdb/v2: ruralzd never links Raft",
		"github.com/hashicorp/raft-boltdb/v2 v2.4.1 is below the advisory floor v2.4.2",
		"github.com/gorilla/websocket v1.5.1 is below the advisory floor v1.5.3",
		"example.com/crypto/sign: cryptography outside Go crypto",
		"golang.org/x/crypto/chacha20: golang.org/x/crypto package off the FIPS delegation allowlist",
		"example.com/nodir: module source not downloaded",
		"example.com/nolicense: no license file found",
	}
	joined := strings.Join(got, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("missing finding %q in:\n%s", w, joined)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d findings, want %d:\n%s", len(got), len(want), joined)
	}
	for _, g := range got {
		if !strings.HasPrefix(g, "ruralzd linux/amd64: ") {
			t.Errorf("finding lacks its target: %q", g)
		}
	}
	// Raft is allowed outside ruralzd.
	got, err = analyze(target{"ruralz-control", "linux", "amd64"}, pkgs[:8], read)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if strings.Contains(g, "never links Raft") || strings.Contains(g, "hashicorp/raft: license") {
			t.Errorf("unexpected finding for ruralz-control: %q", g)
		}
	}
	if _, err := analyze(target{"ruralz", "linux", "amd64"}, []goPackage{{ImportPath: "x", Module: &goModule{Path: "x", Dir: "broken"}}}, fakeReader(nil)); err == nil {
		t.Error("read error not reported")
	}
}

func TestModuleLicenses(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"LICENSE-APACHE": "Apache License\nVersion 2.0",
		"LICENSE-MIT":    "Permission is hereby granted, free of charge",
		"README.md":      "GNU General Public License",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := moduleLicenses(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "Apache-2.0,MIT" {
		t.Errorf("moduleLicenses = %v", got)
	}
	if _, err := moduleLicenses(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing directory not reported")
	}
}

func TestDecodePackages(t *testing.T) {
	stream := `{"ImportPath":"fmt","Standard":true}
{"ImportPath":"example.com/a","Module":{"Path":"example.com/a","Version":"v1.2.3","Dir":"/m/a"}}`
	pkgs, err := decodePackages(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 || !pkgs[0].Standard || pkgs[1].Module.Version != "v1.2.3" {
		t.Fatalf("pkgs = %+v", pkgs)
	}
	if _, err := decodePackages(strings.NewReader("{")); err == nil {
		t.Error("bad JSON accepted")
	}
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.5.3", "v1.5.3", 0},
		{"v1.5.2", "v1.5.3", -1},
		{"v1.10.0", "v1.9.9", 1},
		{"v2.0.0-rc.1", "v2.0.0", -1},
		{"v2.0.0", "v2.0.0-rc.1", 1},
		{"v0.0.0-20260101000000-abcdef123456", "v0.0.1", -1},
		{"v1.2.3+incompatible", "v1.2.3", 0},
	}
	for _, c := range cases {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compareSemver(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestTargets(t *testing.T) {
	n := map[string]int{}
	for _, tg := range targets() {
		n[tg.binary]++
	}
	if n["ruralzd"] != 2 || n["ruralz-control"] != 2 || n["ruralz"] != 5 {
		t.Errorf("targets per binary = %v", n)
	}
}

func TestRepositoryPasses(t *testing.T) {
	findings, err := run(t.Context(), filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}
