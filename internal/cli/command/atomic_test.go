// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Tests for spec 10 req 17: the atomic --output-file write.

func TestWriteFileAtomicReq17(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rev.json")
	if err := WriteFileAtomic(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("two"), 0o644); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: a file under t.TempDir.
	if err != nil || string(got) != "two" {
		t.Fatalf("content = %q, %v", got, err)
	}
	// Mode 0644 before umask: equal to a file created the same way.
	ref := filepath.Join(dir, "ref")
	f, err := os.OpenFile(ref, os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G304, G302: the reference file for the 0644 mode check.
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	st, _ := os.Stat(path)
	rst, _ := os.Stat(ref)
	if st.Mode().Perm() != rst.Mode().Perm() {
		t.Errorf("mode = %v, want %v", st.Mode().Perm(), rst.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("directory holds %d entries, want 2 (no temporary file left)", len(entries))
	}
}

func TestWriteFileAtomicRelativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := WriteFileAtomic("out.yaml", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile("out.yaml"); err != nil || string(b) != "x" {
		t.Fatalf("read back %q, %v", b, err)
	}
}

func TestWriteFileAtomicTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.yaml")
	var tmp string
	rename := func(oldpath, newpath string) error {
		tmp = oldpath
		if b, err := os.ReadFile(oldpath); err != nil || string(b) != "data" { //nolint:gosec // G304: a file under t.TempDir.
			t.Errorf("temporary file content %q, %v", b, err)
		}
		return os.Rename(oldpath, newpath)
	}
	rnd := bytes.NewReader([]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef})
	if err := writeFileAtomic(path, []byte("data"), 0o644, rnd, rename); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, ".out.yaml.tmp-0123456789abcdef"); tmp != want {
		t.Errorf("temporary file %q, want %q", tmp, want)
	}
}

func TestWriteFileAtomicFailures(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "busy", "child"), 0o750); err != nil {
		t.Fatal(err)
	}
	failRename := func(string, string) error { return errors.New("cross-device link") }
	cases := []struct {
		name   string
		path   string
		rnd    io.Reader
		rename func(string, string) error
		want   string
		notDir bool
	}{
		{"missing parent", filepath.Join(dir, "missing", "out"), nil, nil, "parent directory", true},
		{"parent is a file", filepath.Join(file, "out"), nil, nil, "is not a directory", false},
		{"directory path", dir + string(filepath.Separator), nil, nil, "not a file path", false},
		{"random failure", filepath.Join(sub, "out"), iotest{}, nil, "random name", false},
		{"rename failure", filepath.Join(sub, "out"), nil, failRename, "cross-device link", false},
		{"target is a directory", filepath.Join(sub, "busy"), nil, nil, "rename", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rnd, rename := c.rnd, c.rename
			if rnd == nil {
				rnd = bytes.NewReader(bytes.Repeat([]byte{7}, 8))
			}
			if rename == nil {
				rename = os.Rename
			}
			err := writeFileAtomic(c.path, []byte("x"), 0o644, rnd, rename)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if c.notDir && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("err = %v, want fs.ErrNotExist", err)
			}
			entries, _ := os.ReadDir(sub)
			for _, e := range entries {
				if strings.Contains(e.Name(), ".tmp-") {
					t.Errorf("temporary file %s left behind", e.Name())
				}
			}
		})
	}
}

type iotest struct{}

func (iotest) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestWriteDataReq17(t *testing.T) {
	var out bytes.Buffer
	sio := IO{Stdout: &out}
	if err := WriteData(sio, "", []byte("doc\n")); err != nil || out.String() != "doc\n" {
		t.Fatalf("stdout: %q, %v", out.String(), err)
	}
	path := filepath.Join(t.TempDir(), "dump.json")
	out.Reset()
	if err := WriteData(sio, path, []byte("{}\n")); err != nil || out.Len() != 0 {
		t.Fatalf("file: stdout %q, %v", out.String(), err)
	}
	if b, _ := os.ReadFile(path); string(b) != "{}\n" { //nolint:gosec // G304: a file under t.TempDir.
		t.Errorf("file content %q", b)
	}
	missing := filepath.Join(t.TempDir(), "no", "dump.json")
	err := WriteData(sio, missing, []byte("x"))
	if ee, ok := errors.AsType[*ExitError](err); !ok || ee.Code != ExitNoResult ||
		err.Error() != "--output-file "+missing+": parent directory does not exist" {
		t.Errorf("missing parent: %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file written despite the error")
	}
	if runtime.GOOS != "windows" {
		busy := t.TempDir()
		if err := os.MkdirAll(filepath.Join(busy, "d", "x"), 0o750); err != nil {
			t.Fatal(err)
		}
		err = WriteData(sio, filepath.Join(busy, "d"), []byte("x"))
		if ee, ok := errors.AsType[*ExitError](err); !ok || ee.Code != ExitNoResult {
			t.Errorf("rename over a directory: %v", err)
		}
	}
	err = WriteData(IO{Stdout: failWriter{}}, "", []byte("x"))
	if ee, ok := errors.AsType[*ExitError](err); !ok || ee.Code != ExitNoResult {
		t.Errorf("stdout failure: %v", err)
	}
}
