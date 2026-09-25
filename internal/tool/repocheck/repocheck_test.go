// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBadRepo(t *testing.T) {
	findings, err := run(filepath.Join("testdata", "bad"), "allow.txt")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.String())
	}
	want := []string{
		"cmd/app: cmd/<binary> holds only main.go",
		"cmd/app/main.go:3: cmd/ holds wiring only: move declarations",
		"cmd/app/main.go:7: cmd/ holds wiring only: only func main",
		`internal/a/a.go:3: import "C" is forbidden`,
		"internal/a/a.go:7: RZ-RT-" + "999 is not registered", // split so repocheck does not flag this file
		`internal/gate/gate.go:3: no-license-check: "licensekey"`,
		`internal/gate/gate.go:5: no-license-check: "revington.co"`,
		`pkg/p/p.go:6: public package imports "github.com/example/thirdparty"`,
		`pkg/p/p.go:7: public package imports "github.com/ravindu-rev/ruralz/internal/errcode"`,
		"console/app.ts:1: missing license header",
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if strings.HasPrefix(g, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing finding %q", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d findings, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
}

func TestRepositoryIsClean(t *testing.T) {
	findings, err := run(filepath.Join("..", "..", ".."), filepath.FromSlash("internal/tool/repocheck/nolicensecheck.allow"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}

func TestAllowlistFormat(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{
		"internal/ entitlement",             // no reason
		"internal/ entitlement #",           // empty reason
		"internal/ notaterm # reason",       // unknown term
		"internal/ entitlement extra # why", // too many fields
	} {
		p := filepath.Join(dir, "allow")
		if err := os.WriteFile(p, []byte(bad+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAllowlist(p); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	if a, err := loadAllowlist(filepath.Join(dir, "missing")); err != nil || a != nil {
		t.Errorf("missing allowlist: %v, %v", a, err)
	}
}

func TestNormalize(t *testing.T) {
	for _, s := range []string{"licenseKey", "license_key", "LICENSE-KEY", "license key"} {
		if !strings.Contains(normalize(s), "licensekey") {
			t.Errorf("normalize(%q) = %q", s, normalize(s))
		}
	}
}
