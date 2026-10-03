// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package corpus

import (
	"bytes"
	"strings"
)

// Archive is a txtar archive (the format of golang.org/x/tools/txtar): an
// optional comment followed by files, each introduced by a "-- name --"
// line. Bundle-level fuzz targets take a Bundle as one archive.
type Archive struct {
	Comment []byte
	Files   []File
}

// Format returns the archive text. Each file's data ends with a newline
// (one is added when missing), as txtar requires.
func (a *Archive) Format() []byte {
	var b bytes.Buffer
	b.Write(fixNL(a.Comment))
	for _, f := range a.Files {
		b.WriteString("-- ")
		b.WriteString(f.Name)
		b.WriteString(" --\n")
		b.Write(fixNL(f.Data))
	}
	return b.Bytes()
}

// ParseArchive parses txtar text. It never fails: text before the first
// marker line is the comment, and a marker line is "-- name --" with a
// non-empty name.
func ParseArchive(data []byte) *Archive {
	a := &Archive{}
	var name string
	a.Comment, name, data = findMarker(data)
	for name != "" {
		f := File{Name: name}
		f.Data, name, data = findMarker(data)
		a.Files = append(a.Files, f)
	}
	return a
}

// Map returns the files by name (a later duplicate wins).
func (a *Archive) Map() map[string][]byte {
	out := make(map[string][]byte, len(a.Files))
	for _, f := range a.Files {
		out[f.Name] = f.Data
	}
	return out
}

// findMarker returns the text before the next marker line, the marker's
// file name and the text after the marker line.
func findMarker(data []byte) (before []byte, name string, after []byte) {
	var i int
	for {
		if name, after = isMarker(data[i:]); name != "" {
			return data[:i], name, after
		}
		j := bytes.Index(data[i:], []byte("\n-- "))
		if j < 0 {
			return fixNL(data), "", nil
		}
		i += j + 1
	}
}

// isMarker reports whether data starts with a marker line and returns the
// file name and the text after the line.
func isMarker(data []byte) (name string, after []byte) {
	if !bytes.HasPrefix(data, []byte("-- ")) {
		return "", nil
	}
	line, rest, _ := bytes.Cut(data, []byte("\n"))
	s := strings.TrimRight(string(line), "\r")
	if !strings.HasSuffix(s, " --") || len(s) < len("-- x --") {
		return "", nil
	}
	return strings.TrimSpace(s[3 : len(s)-3]), rest
}

// fixNL returns data with a trailing newline added when it is non-empty
// and lacks one.
func fixNL(data []byte) []byte {
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return data
	}
	out := make([]byte, len(data)+1)
	copy(out, data)
	out[len(data)] = '\n'
	return out
}
