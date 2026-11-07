// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	hraft "github.com/hashicorp/raft"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
)

// Backup stream frames: a header, the state-machine snapshot in chunks,
// every referenced content object, and an end frame with the counts, so a
// truncated stream never restores. ruralz control backup encrypts and
// signs the stream; this package writes and reads it in the clear.
const (
	frameHeader        = "header"
	frameSnapshot      = "snapshot"
	frameSnapshotEnd   = "snapshotEnd"
	frameObject        = "object"
	frameEnd           = "end"
	backupSnapshotPart = 1 << 20
)

// backupFrame is one frame header; a snapshot chunk's or object's bytes
// follow it.
type backupFrame struct {
	T             string                `json:"t"`
	Index         uint64                `json:"index,omitempty"`
	Version       *controlstore.Version `json:"version,omitempty"`
	BinaryVersion string                `json:"binaryVersion,omitempty"`
	Digest        *controlstore.Digest  `json:"digest,omitempty"`
	Kind          controlstore.Kind     `json:"kind,omitempty"`
	Format        string                `json:"format,omitempty"`
	Size          int64                 `json:"size,omitempty"`
	Objects       int                   `json:"objects,omitempty"`
}

// Backup writes the state at one index and every content object it
// references: the content store is pinned first, so no object a later
// entry releases goes before the backup holds it, and missing objects are
// fetched from peers. Failures are RZ-CP-015.
func (s *Store) Backup(ctx context.Context, w io.Writer) (controlstore.BackupInfo, error) {
	if err := s.check(); err != nil {
		return controlstore.BackupInfo{}, err
	}
	unpin := s.content.pin()
	defer unpin()
	snap := s.machine.Snapshot()
	var version controlstore.Version
	var refs []controlstore.Digest
	snap.View(func(v controlstore.View) {
		version, _ = v.Version()
		refs = v.References()
	})
	info := controlstore.BackupInfo{Index: snap.Index(), Version: version, BinaryVersion: s.cfg.BinaryVersion, Objects: len(refs)}
	fail := func(err error) (controlstore.BackupInfo, error) {
		return controlstore.BackupInfo{}, controlstore.Errorf(controlstore.ErrBackup, "backup at index %d: %w", info.Index, err)
	}
	bw := bufio.NewWriterSize(w, chunk)
	if err := writeHeader(bw, backupFrame{T: frameHeader, Index: info.Index, Version: &version, BinaryVersion: info.BinaryVersion}); err != nil {
		return fail(err)
	}
	cw := &chunkWriter{w: bw}
	if _, err := snap.WriteTo(cw); err != nil {
		return fail(err)
	}
	if err := cw.flush(); err != nil {
		return fail(err)
	}
	if err := writeHeader(bw, backupFrame{T: frameSnapshotEnd}); err != nil {
		return fail(err)
	}
	for _, d := range refs {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		o, err := s.content.get(d)
		if errors.Is(err, controlstore.ErrNotFound) {
			if err = s.fetchFromPeers(ctx, d); err == nil {
				o, err = s.content.get(d)
			}
		}
		if err != nil {
			return fail(err)
		}
		dd := d
		if err := writeHeader(bw, backupFrame{T: frameObject, Digest: &dd, Kind: o.Kind, Format: o.Format, Size: int64(len(o.Data))}); err != nil {
			return fail(err)
		}
		if _, err := bw.Write(o.Data); err != nil {
			return fail(err)
		}
	}
	if err := writeHeader(bw, backupFrame{T: frameEnd, Objects: len(refs)}); err != nil {
		return fail(err)
	}
	if err := bw.Flush(); err != nil {
		return fail(err)
	}
	return info, nil
}

// RestoreConfig configures an offline restore.
type RestoreConfig struct {
	// DataDir is the Control Store to fill; it must not exist or be empty.
	DataDir string
	// Plan is what the restore changes before the replica serves.
	Plan controlstore.RestorePlan
	// BinaryVersion is the restoring binary's product version.
	BinaryVersion string
	// Logger receives raft's records; nil discards.
	Logger *slog.Logger
}

// Restore fills an empty Control Store from a backup stream, under the
// restore rules: the plan's anchor set opens a higher storeEpoch, the
// membership becomes this one voter, old peer certificates are dropped,
// the plan's buckets (sessions and API tokens) are emptied and an audit
// fork record is appended. Serving the directory then elects this replica.
// Every failure is RZ-CP-015 and leaves no Control Store behind.
func Restore(ctx context.Context, r io.Reader, cfg RestoreConfig) (controlstore.BackupInfo, error) {
	fail := func(err error) (controlstore.BackupInfo, error) {
		return controlstore.BackupInfo{}, controlstore.Errorf(controlstore.ErrBackup, "restore: %w", err)
	}
	binary, err := controlstore.ParseVersion(cfg.BinaryVersion)
	if err != nil {
		return fail(err)
	}
	if empty, err := emptyDir(cfg.DataDir); err != nil || !empty {
		if err == nil {
			err = fmt.Errorf("%s is not an empty Control Store", cfg.DataDir)
		}
		return fail(err)
	}
	info, err := restoreInto(ctx, r, cfg, binary)
	if err != nil {
		_ = os.RemoveAll(cfg.DataDir)
		return fail(err)
	}
	return info, nil
}

// emptyDir reports whether dir is absent or empty.
func emptyDir(dir string) (bool, error) {
	if dir == "" {
		return false, errors.New("no data directory")
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// restoreInto reads the backup and writes the Control Store.
func restoreInto(ctx context.Context, r io.Reader, cfg RestoreConfig, binary controlstore.Version) (controlstore.BackupInfo, error) {
	br := bufio.NewReaderSize(r, chunk)
	var hdr backupFrame
	if err := readHeader(br, &hdr); err != nil || hdr.T != frameHeader || hdr.Version == nil {
		return controlstore.BackupInfo{}, fmt.Errorf("not a Control Store backup: %w", err)
	}
	if hdr.Version.Compare(binary) > 0 {
		return controlstore.BackupInfo{}, fmt.Errorf("the backup's Control Store version %s is above this binary's %s", hdr.Version, binary)
	}
	sr := &snapshotReader{br: br}
	snap, err := controlstore.ReadSnapshot(sr, binary)
	if err != nil {
		return controlstore.BackupInfo{}, err
	}
	if err := sr.drain(); err != nil {
		return controlstore.BackupInfo{}, err
	}
	if snap.Index() != hdr.Index {
		return controlstore.BackupInfo{}, fmt.Errorf("the snapshot is at index %d, the header says %d", snap.Index(), hdr.Index)
	}
	rebased, err := snap.Rebase(cfg.Plan)
	if err != nil {
		return controlstore.BackupInfo{}, err
	}
	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "raft"), filepath.Join(cfg.DataDir, "snapshots")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return controlstore.BackupInfo{}, err
		}
	}
	content, err := openContent(filepath.Join(cfg.DataDir, "content"), func(controlstore.Digest) bool { return true })
	if err != nil {
		return controlstore.BackupInfo{}, err
	}
	objects := 0
	for {
		if err := ctx.Err(); err != nil {
			return controlstore.BackupInfo{}, err
		}
		var f backupFrame
		if err := readHeader(br, &f); err != nil {
			return controlstore.BackupInfo{}, fmt.Errorf("truncated backup: %w", err)
		}
		if f.T == frameEnd {
			if f.Objects != objects {
				return controlstore.BackupInfo{}, fmt.Errorf("the backup holds %d objects, its end frame counts %d", objects, f.Objects)
			}
			break
		}
		if f.T != frameObject || f.Digest == nil {
			return controlstore.BackupInfo{}, fmt.Errorf("unexpected backup frame %q", f.T)
		}
		probe := controlstore.Object{Kind: f.Kind, Format: f.Format}
		if err := probe.Validate(); err != nil || f.Size < 0 {
			return controlstore.BackupInfo{}, fmt.Errorf("object %s: malformed header", f.Digest)
		}
		if err := content.write(*f.Digest, objectHeader{Kind: f.Kind, Format: f.Format, Size: f.Size}, io.LimitReader(br, f.Size)); err != nil {
			return controlstore.BackupInfo{}, err
		}
		objects++
	}
	var refs []controlstore.Digest
	var version controlstore.Version
	rebased.View(func(v controlstore.View) {
		refs = v.References()
		version, _ = v.Version()
	})
	if missing := content.missing(refs); len(missing) > 0 {
		return controlstore.BackupInfo{}, fmt.Errorf("the backup lacks %d referenced objects, first %s", len(missing), missing[0])
	}
	if err := writeRaftState(cfg, rebased); err != nil {
		return controlstore.BackupInfo{}, err
	}
	return controlstore.BackupInfo{Index: rebased.Index(), Version: version, BinaryVersion: hdr.BinaryVersion, Objects: objects}, nil
}

// writeRaftState writes the rebased state as the newest snapshot, with
// the one-voter configuration, at term 1, so serving the directory elects
// this replica.
func writeRaftState(cfg RestoreConfig, snap *controlstore.Snapshot) error {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	rlog := newRaftLogger(logger)
	snaps, err := hraft.NewFileSnapshotStoreWithLogger(filepath.Join(cfg.DataDir, "snapshots"), snapshotsKept, rlog)
	if err != nil {
		return err
	}
	_, codec := hraft.NewInmemTransport(hraft.ServerAddress(cfg.Plan.Address))
	conf := hraft.Configuration{Servers: []hraft.Server{{Suffrage: hraft.Voter, ID: hraft.ServerID(cfg.Plan.ServerID), Address: hraft.ServerAddress(cfg.Plan.Address)}}}
	sink, err := snaps.Create(hraft.SnapshotVersionMax, snap.Index(), 1, conf, snap.Index(), codec)
	if err != nil {
		return err
	}
	if _, err := snap.WriteTo(sink); err != nil {
		_ = sink.Cancel()
		return err
	}
	if err := sink.Close(); err != nil {
		return err
	}
	logs, err := openLog(filepath.Join(cfg.DataDir, "raft", "raft.db"))
	if err != nil {
		return err
	}
	if err := logs.SetUint64([]byte("CurrentTerm"), 1); err != nil {
		_ = logs.Close()
		return err
	}
	return logs.Close()
}

// chunkWriter frames the snapshot into chunks of at most
// backupSnapshotPart bytes as it is written, so a backup never holds the
// whole snapshot in memory.
type chunkWriter struct {
	w   io.Writer
	buf []byte
}

// Write buffers p, emitting every full chunk.
func (c *chunkWriter) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	for len(c.buf) >= backupSnapshotPart {
		if err := c.emit(c.buf[:backupSnapshotPart]); err != nil {
			return 0, err
		}
		c.buf = append(c.buf[:0], c.buf[backupSnapshotPart:]...)
	}
	return len(p), nil
}

// flush emits the last partial chunk.
func (c *chunkWriter) flush() error {
	if len(c.buf) == 0 {
		return nil
	}
	err := c.emit(c.buf)
	c.buf = c.buf[:0]
	return err
}

// emit writes one snapshot frame.
func (c *chunkWriter) emit(b []byte) error {
	if err := writeHeader(c.w, backupFrame{T: frameSnapshot, Size: int64(len(b))}); err != nil {
		return err
	}
	_, err := c.w.Write(b)
	return err
}

// snapshotReader yields the snapshot chunks of a backup stream until its
// snapshotEnd frame.
type snapshotReader struct {
	br     *bufio.Reader
	left   int64
	done   bool
	frames int
}

// Read reads the current chunk, moving to the next frame at its end.
func (s *snapshotReader) Read(p []byte) (int, error) {
	for s.left == 0 {
		if s.done {
			return 0, io.EOF
		}
		var f backupFrame
		if err := readHeader(s.br, &f); err != nil {
			return 0, err
		}
		switch f.T {
		case frameSnapshot:
			if f.Size <= 0 || f.Size > backupSnapshotPart {
				return 0, fmt.Errorf("%w: snapshot chunk of %d bytes", errFrame, f.Size)
			}
			s.left = f.Size
			s.frames++
		case frameSnapshotEnd:
			s.done = true
		default:
			return 0, fmt.Errorf("%w: frame %q inside the snapshot", errFrame, f.T)
		}
	}
	if int64(len(p)) > s.left {
		p = p[:s.left]
	}
	n, err := s.br.Read(p)
	s.left -= int64(n)
	return n, err
}

// drain consumes the rest of the snapshot section.
func (s *snapshotReader) drain() error {
	_, err := io.Copy(io.Discard, s)
	return err
}
