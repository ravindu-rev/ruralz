// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// benchFiles is the part of a ruralz.bench.v1 record that names the files
// beside it: raw histogram logs, CPU profiles and, for a summary, its runs,
// each relative to the record's directory.
type benchFiles struct {
	Format  string `json:"format"`
	Latency struct {
		HistogramFiles []string `json:"histogramFiles"`
	} `json:"latency"`
	Resources struct {
		CPUProfileFiles []string `json:"cpuProfileFiles"`
	} `json:"resources"`
	Summary *struct {
		Runs []string `json:"runs"`
	} `json:"summary"`
}

// named returns the slash paths, relative to the records root, of the
// files the record at rel names.
func (b benchFiles) named(rel string) []string {
	var out []string
	add := func(names []string) {
		for _, n := range names {
			out = append(out, path.Join(path.Dir(rel), n))
		}
	}
	add(b.Latency.HistogramFiles)
	add(b.Resources.CPUProfileFiles)
	if b.Summary != nil {
		add(b.Summary.Runs)
	}
	return out
}

// cleanRel reports whether p is a clean slash path inside its root.
func cleanRel(p string) bool {
	return p != "" && p != "." && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && !strings.Contains(p, `\`)
}

// benchMembers returns the members of the benchmark records archive and
// the number of records: every ruralz.bench.v1 record below dir and every
// file a record names, with their directories. A state.json that is no
// record (the Latency job's rotation state) is left out; any other file is
// an error, so nothing enters the archive unnoticed and no result goes
// missing. An empty or missing dir gives an empty archive.
func benchMembers(dir string) ([]member, int, error) {
	if dir == "" {
		return nil, 0, nil
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	data := map[string][]byte{}
	var records []string
	var others []string
	named := map[string]string{} // named file -> the record naming it
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}
		f, err := os.Open(p) //nolint:gosec // A file below the records directory given on the command line.
		if err != nil {
			return err
		}
		content, err := io.ReadAll(io.LimitReader(f, maxArchiveBytes+1))
		_ = f.Close()
		if err != nil {
			return err
		}
		if len(content) > maxArchiveBytes {
			return fmt.Errorf("%s exceeds %d bytes", rel, maxArchiveBytes)
		}
		if path.Ext(rel) == ".json" {
			var b benchFiles
			if json.Unmarshal(content, &b) == nil && b.Format == benchFormat {
				records = append(records, rel)
				data[rel] = content
				for _, n := range b.named(rel) {
					named[n] = rel
				}
				return nil
			}
			if path.Base(rel) == stateFile {
				return nil
			}
		}
		others = append(others, rel)
		data[rel] = content
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("benchmark records: %w", err)
	}
	for _, o := range others {
		if _, ok := named[o]; !ok {
			return nil, 0, fmt.Errorf("benchmark records: %s is neither a %s record nor a file a record names", o, benchFormat)
		}
	}
	for n, rec := range named {
		if !cleanRel(n) {
			return nil, 0, fmt.Errorf("benchmark records: %s names %q, outside the records directory", rec, n)
		}
		if _, ok := data[n]; !ok {
			return nil, 0, fmt.Errorf("benchmark records: %s names %s, which is missing", rec, n)
		}
	}
	var ms []member
	dirs := map[string]bool{}
	for rel, content := range data {
		ms = append(ms, member{Path: rel, Mode: modeFile, Data: content})
		for d := path.Dir(rel); d != "."; d = path.Dir(d) {
			dirs[d+"/"] = true
		}
	}
	for d := range dirs {
		ms = append(ms, member{Path: d, Mode: modeDir})
	}
	sortMembers(ms)
	return ms, len(records), nil
}

// benchProblems audits the benchmark records archive's members: modes,
// every JSON member a record, every file a record names present, nothing
// else, and the record count.
func benchProblems(members []stored, records int) []string {
	var out []string
	files := map[string]bool{}
	for _, s := range members {
		if s.Regular {
			files[s.Path] = true
		}
	}
	named := map[string]bool{}
	count := 0
	for _, s := range members {
		want := fs.FileMode(modeFile)
		if s.Dir {
			want = modeDir
		}
		if s.Mode != want {
			out = append(out, fmt.Sprintf("%s has mode %04o, want %04o", s.Path, s.Mode, want))
		}
		if !s.Regular {
			continue
		}
		var b benchFiles
		if path.Ext(s.Path) != ".json" || json.Unmarshal(s.Data, &b) != nil || b.Format != benchFormat {
			continue
		}
		count++
		for _, n := range b.named(s.Path) {
			named[n] = true
			if !files[n] {
				out = append(out, fmt.Sprintf("%s names %s, which the archive lacks", s.Path, n))
			}
		}
	}
	for _, s := range members {
		var b benchFiles
		isRecord := path.Ext(s.Path) == ".json" && json.Unmarshal(s.Data, &b) == nil && b.Format == benchFormat
		if s.Regular && !isRecord && !named[s.Path] {
			out = append(out, fmt.Sprintf("%s is neither a record nor a file a record names", s.Path))
		}
	}
	if count != records {
		out = append(out, fmt.Sprintf("holds %d records, the manifest says %d", count, records))
	}
	slices.Sort(out)
	return out
}
