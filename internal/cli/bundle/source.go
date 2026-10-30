// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/loader"
)

// readLimited reads r to its end, or to one byte past limit, which tells a
// source at the limit from a larger one: the loader then reports the
// larger one as RZ-CFG-001 at the first byte past the limit.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, min(limit, math.MaxInt64-1)+1))
}

// stdinSource is a one-file Bundle of data, the bytes read from standard
// input, whose file is named "-" in every diagnostic; a name without a
// .yaml, .yml or .json extension reads as YAML.
func stdinSource(data []byte) loader.Source {
	return loader.Source{FS: oneFile{name: stdinName, data: data}, Path: stdinName}
}

// oneFile is a file system that holds one regular file, name, in memory.
type oneFile struct {
	name string
	data []byte
}

// Open opens the file; every other name does not exist.
func (o oneFile) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if name != o.name {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &memFile{info: memInfo{name: o.name, size: int64(len(o.data))}, r: bytes.NewReader(o.data)}, nil
}

// memFile is the open file of a oneFile.
type memFile struct {
	info memInfo
	r    *bytes.Reader
}

// Stat describes the file.
func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }

// Read reads the file's bytes.
func (f *memFile) Read(p []byte) (int, error) { return f.r.Read(p) }

// Close does nothing: the bytes stay in memory.
func (f *memFile) Close() error { return nil }

// memInfo describes the file of a oneFile: a read-only regular file.
type memInfo struct {
	name string
	size int64
}

// Name returns the file's name.
func (i memInfo) Name() string { return i.name }

// Size returns the length of the file's bytes.
func (i memInfo) Size() int64 { return i.size }

// Mode returns the mode of a read-only regular file.
func (i memInfo) Mode() fs.FileMode { return 0o444 }

// ModTime returns the zero time: the bytes have no modification time.
func (i memInfo) ModTime() time.Time { return time.Time{} }

// IsDir reports false.
func (i memInfo) IsDir() bool { return false }

// Sys returns nil.
func (i memInfo) Sys() any { return nil }

// startsObject reports whether data starts with a JSON object: its first
// byte after JSON white space is "{", as every /config/dump document's is.
func startsObject(data []byte) bool {
	i := 0
	for i < len(data) && isJSONSpace(data[i]) {
		i++
	}
	return i < len(data) && data[i] == '{'
}

// isJSONSpace reports the white space RFC 8259 allows between tokens.
func isJSONSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// errNotObject reports a file that does not start with a JSON object, so
// it holds no /config/dump document.
var errNotObject = errors.New("not a JSON object")

// readObjectFile reads the file at path when it starts with a JSON object,
// to its end or to one byte past limit; a file that starts with anything
// else is errNotObject after reading no further than its first byte past
// the leading white space, so a YAML Bundle is read once, by the loader.
func readObjectFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a diff source the user names on the command line.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(io.LimitReader(f, min(limit, math.MaxInt64-1)+1))
	var head []byte
	for {
		b, err := r.ReadByte()
		switch {
		case errors.Is(err, io.EOF):
			return nil, errNotObject
		case err != nil:
			return nil, err
		}
		head = append(head, b)
		if isJSONSpace(b) {
			continue
		}
		if b != '{' {
			return nil, errNotObject
		}
		break
	}
	rest, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return append(head, rest...), nil
}
