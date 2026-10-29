// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// maxArchiveBytes bounds the uncompressed content verify reads from one
// archive: ruralzd's stripped budget is 160 MiB (target), so 1 GiB leaves
// room while stopping a corrupt archive from exhausting memory.
const maxArchiveBytes = 1 << 30

// Archive member modes.
const (
	modeFile = 0o644
	modeExec = 0o755
	modeDir  = 0o755
)

// member is one entry of a release archive. A directory's Path ends in
// "/" and it has no Data.
type member struct {
	Path string
	Mode fs.FileMode
	Data []byte
}

func (m member) isDir() bool { return strings.HasSuffix(m.Path, "/") }

// sortMembers orders members by path bytes, the order archives store them
// in, so the same inputs always give the same archive.
func sortMembers(ms []member) {
	slices.SortFunc(ms, func(a, b member) int { return strings.Compare(a.Path, b.Path) })
}

// checkMembers rejects paths an archive must not hold: absolute, with a
// ".." or empty element, a backslash, or twice.
func checkMembers(ms []member) error {
	seen := map[string]bool{}
	for _, m := range ms {
		p := strings.TrimSuffix(m.Path, "/")
		if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
			return fmt.Errorf("archive member %q is not a clean relative path", m.Path)
		}
		if seen[p] {
			return fmt.Errorf("archive member %q appears twice", m.Path)
		}
		seen[p] = true
	}
	return nil
}

// writeTarGz writes members as a deterministic gzip-compressed tar: sorted
// entries, every one dated mtime, owner and group 0 without names, USTAR
// headers, default compression and a gzip header without name or time.
func writeTarGz(w io.Writer, ms []member, mtime time.Time) error {
	ms = slices.Clone(ms)
	sortMembers(ms)
	if err := checkMembers(ms); err != nil {
		return err
	}
	gz, err := gzip.NewWriterLevel(w, gzip.DefaultCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	for _, m := range ms {
		h := &tar.Header{
			Name:    m.Path,
			Mode:    int64(m.Mode.Perm()),
			ModTime: mtime.UTC(),
			Format:  tar.FormatUSTAR,
		}
		if m.isDir() {
			h.Typeflag = tar.TypeDir
		} else {
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(m.Data))
		}
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("%s: %w", m.Path, err)
		}
		if !m.isDir() {
			if _, err := tw.Write(m.Data); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// writeZip writes members as a deterministic zip: sorted entries, Deflate
// at the default level, every one dated mtime in UTC with Unix modes.
func writeZip(w io.Writer, ms []member, mtime time.Time) error {
	ms = slices.Clone(ms)
	sortMembers(ms)
	if err := checkMembers(ms); err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.DefaultCompression)
	})
	for _, m := range ms {
		h := &zip.FileHeader{Name: m.Path, Method: zip.Deflate, Modified: mtime.UTC()}
		mode := m.Mode.Perm()
		if m.isDir() {
			mode |= fs.ModeDir
			h.Method = zip.Store
		}
		h.SetMode(mode)
		fw, err := zw.CreateHeader(h)
		if err != nil {
			return fmt.Errorf("%s: %w", m.Path, err)
		}
		if !m.isDir() {
			if _, err := fw.Write(m.Data); err != nil {
				return err
			}
		}
	}
	return zw.Close()
}

// stored is one entry as read back from an archive, with the metadata the
// audit checks.
type stored struct {
	member
	ModTime      time.Time
	UID, GID     int
	Uname, Gname string
	// Regular and Dir report the entry type; anything else (a link, a
	// device) is neither.
	Regular, Dir bool
}

// gzipFacts are the gzip header fields the audit checks.
type gzipFacts struct {
	Name    string
	ModTime time.Time
}

// readTarGz reads every entry of a gzip-compressed tar in stored order.
func readTarGz(data []byte) ([]stored, gzipFacts, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, gzipFacts{}, err
	}
	facts := gzipFacts{Name: gz.Name, ModTime: gz.ModTime}
	tr := tar.NewReader(gz)
	var out []stored
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, facts, err
		}
		perm := h.Mode & 0o777
		s := stored{
			member:  member{Path: h.Name, Mode: fs.FileMode(perm)},
			ModTime: h.ModTime, UID: h.Uid, GID: h.Gid, Uname: h.Uname, Gname: h.Gname,
			Regular: h.Typeflag == tar.TypeReg, Dir: h.Typeflag == tar.TypeDir,
		}
		if s.Regular {
			if total += h.Size; total > maxArchiveBytes {
				return nil, facts, fmt.Errorf("archive content exceeds %d bytes", maxArchiveBytes)
			}
			if s.Data, err = io.ReadAll(io.LimitReader(tr, h.Size)); err != nil {
				return nil, facts, err
			}
		}
		out = append(out, s)
	}
	// Draining the stream lets gzip check its trailer; past the content
	// bound the archive digest still guards the bytes.
	if _, err := io.Copy(io.Discard, io.LimitReader(gz, maxArchiveBytes)); err != nil {
		return nil, facts, err
	}
	return out, facts, gz.Close()
}

// readZip reads every entry of a zip in stored order. Zip has no owner, so
// UID and GID read as 0.
func readZip(data []byte) ([]stored, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	var out []stored
	var total uint64
	for _, f := range zr.File {
		mode := f.Mode()
		s := stored{
			member:  member{Path: f.Name, Mode: mode.Perm()},
			ModTime: f.Modified,
			Regular: mode.IsRegular(), Dir: mode.IsDir(),
		}
		if s.Regular {
			if total += f.UncompressedSize64; total > maxArchiveBytes {
				return nil, fmt.Errorf("archive content exceeds %d bytes", maxArchiveBytes)
			}
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Name, err)
			}
			// The reader checks the CRC and the declared size at EOF.
			s.Data, err = io.ReadAll(io.LimitReader(rc, maxArchiveBytes+1))
			_ = rc.Close()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Name, err)
			}
		}
		out = append(out, s)
	}
	return out, nil
}
