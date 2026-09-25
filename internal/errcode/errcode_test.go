// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package errcode

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestRegistry(t *testing.T) {
	seen := map[string]bool{}
	prev := Code{}
	for _, c := range All() {
		if !Valid(c.ID) {
			t.Errorf("%s: invalid syntax", c.ID)
		}
		if seen[c.ID] {
			t.Errorf("%s: registered twice", c.ID)
		}
		seen[c.ID] = true
		if !strings.HasPrefix(c.ID, "RZ-"+string(c.Area)+"-") {
			t.Errorf("%s: area %s does not match", c.ID, c.Area)
		}
		if c.Area.Owner() == "" {
			t.Errorf("%s: area %s has no owner", c.ID, c.Area)
		}
		if c.Meaning == "" {
			t.Errorf("%s: no meaning", c.ID)
		}
		if c.Status != 0 && (c.Status < 400 || c.Status > 599) {
			t.Errorf("%s: status %d is not an error status", c.ID, c.Status)
		}
		if c.Status != 0 && c.StatusNote != "" {
			t.Errorf("%s: both Status and StatusNote set", c.ID)
		}
		if prev.Area == c.Area && prev.ID >= c.ID {
			t.Errorf("%s: out of order after %s", c.ID, prev.ID)
		}
		prev = c
	}
}

func TestLookup(t *testing.T) {
	c, ok := Lookup("RZ-RT-001")
	if !ok || c.Status != 404 || c.Area != AreaRT {
		t.Fatalf("Lookup(RZ-RT-001) = %+v, %v", c, ok)
	}
	if _, ok := Lookup("RZ-RT-999"); ok {
		t.Fatal("Lookup(RZ-RT-999) found a code")
	}
}

func TestValid(t *testing.T) {
	for _, id := range []string{"RZ-CFG-001", "RZ-STS-005", "RZ-AUTH-020"} {
		if !Valid(id) {
			t.Errorf("Valid(%q) = false", id)
		}
	}
	for _, id := range []string{"", "RZ-CFG-01", "RZ-XYZ-001", "RZ-CFG-0a1", "rz-cfg-001", "RZ-CFG-0001", "RZ-CFG"} {
		if Valid(id) {
			t.Errorf("Valid(%q) = true", id)
		}
	}
	if !regexp.MustCompile("^" + Pattern + "$").MatchString("RZ-PLG-011") {
		t.Error("Pattern does not match a valid code")
	}
}

// TestMatchesDocs keeps the registry equal to the codes the design documents
// define: every code a document names is registered, and every registered
// code is named in a document.
func TestMatchesDocs(t *testing.T) {
	re := regexp.MustCompile(Pattern)
	inDocs := map[string]string{}
	root := filepath.Join("..", "..", "docs")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // Test reads the repository's own documents.
		if err != nil {
			return err
		}
		for _, id := range re.FindAllString(string(data), -1) {
			inDocs[id] = path
		}
		// A range row such as "RZ-AUTH-010 to RZ-AUTH-014" defines the codes between.
		for _, m := range regexp.MustCompile(`RZ-([A-Z]+)-(\d{3}) to RZ-([A-Z]+)-(\d{3})`).FindAllStringSubmatch(string(data), -1) {
			if m[1] != m[3] {
				continue
			}
			lo, _ := strconv.Atoi(m[2])
			hi, _ := strconv.Atoi(m[4])
			for n := lo; n <= hi; n++ {
				inDocs[fmt.Sprintf("RZ-%s-%03d", m[1], n)] = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for id, path := range inDocs {
		if _, ok := Lookup(id); !ok {
			t.Errorf("%s is named in %s but not registered", id, path)
		}
	}
	for _, c := range All() {
		if _, ok := inDocs[c.ID]; !ok {
			t.Errorf("%s is registered but no document names it", c.ID)
		}
	}
}
