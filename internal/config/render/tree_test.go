// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// treeFiles is a small converted tree with nested directories.
func treeFiles() []File {
	return []File{
		{Path: "ruralz.yaml", Content: []byte("a: 1\n")},
		{Path: "overlays/prod/scale.yaml", Content: []byte("b: 2\n")},
		{Path: "routes/web.json", Content: []byte("{}\n")},
	}
}

// checkTree fails unless dir holds exactly files.
func checkTree(t *testing.T, dir string, files []File) {
	t.Helper()
	want := map[string]string{}
	for _, f := range files {
		want[f.Path] = string(f.Content)
	}
	got := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p) //nolint:gosec // G304: a file under t.TempDir.
		got[filepath.ToSlash(rel)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !mapsEqual(got, want) {
		t.Errorf("tree = %v, want %v", got, want)
	}
}

// leftovers returns the hidden temporary directories WriteTree left in
// parent.
func leftovers(t *testing.T, parent string) []string {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestWriteTreeCreates checks that WriteTree creates a missing output
// directory, and its missing parents, holding exactly the files at their
// paths, and leaves no temporary directory behind.
func TestWriteTreeCreates(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "a", "b", "out")
	if err := WriteTree(dir, treeFiles(), TreeOptions{}); err != nil {
		t.Fatal(err)
	}
	checkTree(t, dir, treeFiles())
	if l := leftovers(t, filepath.Dir(dir)); len(l) > 0 {
		t.Errorf("temporary directories left: %q", l)
	}
}

// TestWriteTreeFillsEmptyDirectory checks that an existing empty output
// directory is accepted and filled.
func TestWriteTreeFillsEmptyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := WriteTree(dir, treeFiles(), TreeOptions{}); err != nil {
		t.Fatal(err)
	}
	checkTree(t, dir, treeFiles())
}

// TestWriteTreeRefusesUsedDirectory checks ErrOutputDir for an output
// directory that is not empty, for a file, and for no name, leaving each
// as it was.
func TestWriteTreeRefusesUsedDirectory(t *testing.T) {
	parent := t.TempDir()
	full := filepath.Join(parent, "full")
	if err := os.MkdirAll(full, 0o750); err != nil {
		t.Fatal(err)
	}
	keep := []File{{Path: "keep.yaml", Content: []byte("keep\n")}}
	if err := os.WriteFile(filepath.Join(full, "keep.yaml"), keep[0].Content, 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(parent, "file.yaml")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{full, file, ""} {
		if err := WriteTree(dir, treeFiles(), TreeOptions{}); !errors.Is(err, ErrOutputDir) {
			t.Errorf("WriteTree(%q) error = %v, want ErrOutputDir", dir, err)
		}
	}
	checkTree(t, full, keep)
	if data, err := os.ReadFile(file); err != nil || string(data) != "x" { //nolint:gosec // G304: a file under t.TempDir.
		t.Errorf("the file was changed: %q, %v", data, err)
	}
	if l := leftovers(t, parent); len(l) > 0 {
		t.Errorf("temporary directories left: %q", l)
	}
}

// TestWriteTreeAllOrNothing checks that a failure while writing leaves no
// output directory, or the empty one the caller named, and no temporary
// directory: here a file path that another file needs as a directory.
func TestWriteTreeAllOrNothing(t *testing.T) {
	parent := t.TempDir()
	clash := []File{
		{Path: "a.yaml", Content: []byte("a\n")},
		{Path: "a.yaml/b.yaml", Content: []byte("b\n")},
	}
	dir := filepath.Join(parent, "out")
	if err := WriteTree(dir, clash, TreeOptions{}); err == nil {
		t.Fatal("WriteTree wrote a tree whose paths clash")
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed WriteTree left %s (%v)", dir, err)
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := WriteTree(dir, clash, TreeOptions{}); err == nil {
		t.Fatal("WriteTree wrote a tree whose paths clash")
	}
	checkTree(t, dir, nil)
	if l := leftovers(t, parent); len(l) > 0 {
		t.Errorf("temporary directories left: %q", l)
	}
}

// TestWriteTreeOutsideSource checks that an output directory equal to the
// source Bundle root or inside it is ErrOutputDir, while a sibling is
// accepted.
func TestWriteTreeOutsideSource(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, filepath.Join(root, "converted"), filepath.Join(root, "a", "b")} {
		if err := WriteTree(dir, treeFiles(), TreeOptions{SourceRoot: root}); !errors.Is(err, ErrOutputDir) {
			t.Errorf("WriteTree(%s) error = %v, want ErrOutputDir", dir, err)
		}
	}
	sibling := filepath.Join(filepath.Dir(root), "bundle-converted")
	if err := WriteTree(sibling, treeFiles(), TreeOptions{SourceRoot: root}); err != nil {
		t.Errorf("WriteTree(sibling) error = %v", err)
	}
	if l := leftovers(t, root); len(l) > 0 {
		t.Errorf("temporary directories left in the source: %q", l)
	}
}

// TestWriteTreeBadPaths checks ErrPath for paths that would leave the
// output directory or name no configuration file, and for duplicates.
func TestWriteTreeBadPaths(t *testing.T) {
	for _, files := range [][]File{
		{{Path: "../escape.yaml"}},
		{{Path: "/abs.yaml"}},
		{{Path: `a\b.yaml`}},
		{{Path: "a/./b.yaml"}},
		{{Path: "notes.txt"}},
		{{Path: "a.yaml"}, {Path: "a.yaml"}},
	} {
		dir := filepath.Join(t.TempDir(), "out")
		if err := WriteTree(dir, files, TreeOptions{}); !errors.Is(err, ErrPath) {
			t.Errorf("WriteTree(%q) error = %v, want ErrPath", files[0].Path, err)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("WriteTree(%q) created the output directory", files[0].Path)
		}
	}
}

// TestValidPath checks the path rule shared by Convert and WriteTree.
func TestValidPath(t *testing.T) {
	for p, want := range map[string]bool{
		"ruralz.yaml": true, "overlays/prod/a.yml": true, "a/b/c.json": true, ".hidden/a.yaml": true,
		"": false, ".": false, "..": false, "../a.yaml": false, "a/../b.yaml": false, "/a.yaml": false,
		"a//b.yaml": false, "a/": false, "./a.yaml": false, `a\b.yaml`: false, "a\x00.yaml": false,
	} {
		if got := validPath(p); got != want {
			t.Errorf("validPath(%q) = %v, want %v", p, got, want)
		}
	}
}
