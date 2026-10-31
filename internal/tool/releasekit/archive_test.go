// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"
)

func sampleMembers() []member {
	return []member{
		{Path: "ruralzd", Mode: modeExec, Data: []byte("binary")},
		{Path: "deploy/systemd/ruralzd.service", Mode: modeFile, Data: []byte("[Unit]\n")},
		{Path: "LICENSE", Mode: modeFile, Data: []byte("license\n")},
		{Path: "deploy/", Mode: modeDir},
		{Path: "deploy/systemd/", Mode: modeDir},
	}
}

// TestWriteTarGzDeterministic: the same members in any order give the same
// bytes; entries come back sorted, dated mtime, owned by 0 without names,
// with their modes and types, under a gzip header without name or time.
func TestWriteTarGzDeterministic(t *testing.T) {
	mtime := time.Date(2026, 10, 1, 12, 0, 0, 0, time.FixedZone("x", 5*3600))
	var a, b bytes.Buffer
	ms := sampleMembers()
	if err := writeTarGz(&a, ms, mtime); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(ms)
	if err := writeTarGz(&b, ms, mtime); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("member order changed the archive bytes")
	}
	got, gz, err := readTarGz(a.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		t.Errorf("gzip header = %+v", gz)
	}
	var paths []string
	for _, s := range got {
		paths = append(paths, s.Path)
		if !s.ModTime.Equal(mtime) || s.UID != 0 || s.GID != 0 || s.Uname != "" || s.Gname != "" {
			t.Errorf("%s: time %v owner %d:%d %q:%q", s.Path, s.ModTime, s.UID, s.GID, s.Uname, s.Gname)
		}
		if s.Dir != s.isDir() || s.Regular == s.Dir {
			t.Errorf("%s: dir %v regular %v", s.Path, s.Dir, s.Regular)
		}
	}
	want := []string{"LICENSE", "deploy/", "deploy/systemd/", "deploy/systemd/ruralzd.service", "ruralzd"}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	if got[4].Mode != modeExec || got[0].Mode != modeFile || got[1].Mode != modeDir || string(got[4].Data) != "binary" {
		t.Errorf("modes or data: %+v", got)
	}
}

func TestWriteZipDeterministic(t *testing.T) {
	mtime := time.Date(2026, 10, 1, 12, 0, 1, 0, time.UTC)
	ms := []member{
		{Path: "ruralz.exe", Mode: modeExec, Data: bytes.Repeat([]byte("x"), 4096)},
		{Path: "NOTICE", Mode: modeFile, Data: []byte("notice\n")},
	}
	var a, b bytes.Buffer
	if err := writeZip(&a, ms, mtime); err != nil {
		t.Fatal(err)
	}
	if err := writeZip(&b, []member{ms[1], ms[0]}, mtime); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("member order changed the zip bytes")
	}
	got, err := readZip(a.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != "NOTICE" || got[1].Path != "ruralz.exe" {
		t.Fatalf("entries = %+v", got)
	}
	for _, s := range got {
		if !s.ModTime.Equal(mtime) || !s.Regular {
			t.Errorf("%s: time %v regular %v", s.Path, s.ModTime, s.Regular)
		}
	}
	if got[1].Mode != modeExec || got[0].Mode != modeFile || len(got[1].Data) != 4096 {
		t.Errorf("modes or data: %v %v %d", got[0].Mode, got[1].Mode, len(got[1].Data))
	}
}

func TestCheckMembers(t *testing.T) {
	for _, p := range []string{"/abs", "../up", "a/../b", "a\\b", "a//b", "./a", "", "a/./b"} {
		if err := checkMembers([]member{{Path: p, Mode: modeFile}}); err == nil {
			t.Errorf("checkMembers(%q) = nil", p)
		}
	}
	if err := checkMembers([]member{{Path: "a", Mode: modeFile}, {Path: "a/", Mode: modeDir}}); err == nil {
		t.Error("a path twice accepted")
	}
	if err := checkMembers(sampleMembers()); err != nil {
		t.Errorf("valid members: %v", err)
	}
}

// TestReadTarGzBound: verify refuses an archive whose content exceeds
// maxArchiveBytes before reading it.
func TestReadTarGzBound(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "big", Typeflag: tar.TypeReg, Mode: 0o644, Size: maxArchiveBytes + 1}); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readTarGz(buf.Bytes()); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("readTarGz = %v, want the size bound", err)
	}
}

func TestScriptMode(t *testing.T) {
	if scriptMode([]byte("#!/usr/bin/env bash\n")) != fs.FileMode(modeExec) || scriptMode([]byte("[Unit]\n")) != fs.FileMode(modeFile) {
		t.Error("scriptMode does not follow the shebang")
	}
}
