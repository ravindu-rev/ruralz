// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lkg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Format is the format marker of the pointer files, the lkg marker of
// the handover protocol.
const Format = "ruralz.lkg.v1"

// Names in lkg/.
const (
	// CandidateFile points at the Revision the Node last wrote after
	// activating it.
	CandidateFile = "candidate.json"
	// LKGFile points at the Last-Known-Good Revision.
	LKGFile = "lkg.json"
	// contentPrefix and contentSuffix frame a content file name:
	// sha256-<64 hex>.json.
	contentPrefix = "sha256-"
	contentSuffix = ".json"
)

// Retry backoff of a failed write (target values): the n-th consecutive
// failure waits RetryMin doubled n-1 times, at most RetryMax.
const (
	RetryMin = time.Second
	RetryMax = 60 * time.Second
)

// maxPointerBytes bounds a pointer read; a pointer is one short JSON
// object, so anything larger is malformed.
const maxPointerBytes = 64 << 10

// statusSource is the NodeStatus source of lkg_write_failed.
const statusSource = "lkg"

// Errors.
var (
	// ErrAbsent reports a pointer that is missing, malformed or of an
	// unknown format: there is no such Revision to read.
	ErrAbsent = errors.New("lkg: absent")
	// ErrNotHolder reports a Write without the data-directory lock.
	ErrNotHolder = errors.New("lkg: the process does not hold the data-directory lock")
	// ErrConfig reports an unusable Config.
	ErrConfig = errors.New("lkg: invalid configuration")
)

// RetryDelay returns the wait before the retry that follows the n-th
// (n ≥ 1) consecutive failed write: 1 s, 2 s, 4 s and so on, at most 60 s.
func RetryDelay(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n > 6 {
		return RetryMax
	}
	return min(RetryMax, RetryMin<<(n-1))
}

// ContentName returns the content file name of d: sha256-<64 hex>.json.
func ContentName(d revision.Digest) string { return contentPrefix + d.Hex() + contentSuffix }

// Pointer is the JSON of candidate.json and lkg.json. Members are written
// in this order; readers ignore unknown members.
type Pointer struct {
	// Format is Format; another value counts as absent.
	Format string `json:"format"`
	// Digest is the Revision's "sha256:<64 hex>".
	Digest string `json:"digest"`
	// WrittenAt is when the pointer was written, RFC 3339 UTC.
	WrittenAt string `json:"writtenAt"`
	// Version is the build version of the ruralzd that wrote it.
	Version string `json:"version"`
}

// Entry is one Revision read from lkg/.
type Entry struct {
	// Digest is the digest the pointer, or the caller, named. The content
	// is not re-hashed here: the configuration pipeline verifies it.
	Digest revision.Digest
	// Content is the content file's bytes, at most one byte past the
	// Store's limit.
	Content []byte
	// Path is the content file's path, the name diagnostics carry.
	Path string
	// Pointer is the pointer that named the content; zero for Revision.
	Pointer Pointer
}

// Config configures a Store.
type Config struct {
	// Dir is the data directory; required.
	Dir *nodedir.Dir
	// Version is the build version written in pointers.
	Version string
	// MaxBytes is the Node's encoded size limit, the configuration
	// pipeline's Limits.MaxSourceBytes: content reads stop one byte past
	// it. Zero or less means loader.DefaultMaxSourceBytes.
	MaxBytes int64
	// Clock dates the pointers; nil means clock.Real().
	Clock clock.Clock
	// Logger receives garbage-collection failures and unreadable
	// pointers; nil discards them.
	Logger *slog.Logger
	// Status raises and clears lkg_write_failed; nil reports nothing.
	Status emit.NodeStatus
}

// Store reads and writes the files of lkg/. Reads may run at any time and
// from any process; writes need the data-directory lock. Its methods are
// safe for concurrent use; writes run one at a time.
type Store struct {
	dir      *nodedir.Dir
	version  string
	maxBytes int64
	clk      clock.Clock
	log      *slog.Logger
	status   emit.NodeStatus

	mu      sync.Mutex // serializes writes and the degraded state
	failing bool       // lkg_write_failed is raised

	// afterPointer runs after read read a pointer, before the content.
	// Tests only.
	afterPointer func()
}

// New returns a Store over cfg.Dir's lkg/.
func New(cfg Config) (*Store, error) {
	if cfg.Dir == nil {
		return nil, fmt.Errorf("%w: Dir is required", ErrConfig)
	}
	s := &Store{dir: cfg.Dir, version: cfg.Version, maxBytes: cfg.MaxBytes, clk: cfg.Clock, log: cfg.Logger, status: cfg.Status}
	if s.maxBytes <= 0 {
		s.maxBytes = loader.DefaultMaxSourceBytes
	}
	if s.clk == nil {
		s.clk = clock.Real()
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// Path returns the path of name in lkg/.
func (s *Store) Path(name string) string { return filepath.Join(s.dir.LKGPath(), name) }

// LastKnownGood reads the Revision lkg.json names. The error matches
// ErrAbsent when the pointer is missing, malformed or of an unknown
// format, and fs.ErrNotExist when its content file is missing.
func (s *Store) LastKnownGood() (Entry, error) { return s.read(LKGFile) }

// Candidate reads the Revision candidate.json names. A content file that
// is gone while the pointer now names another Revision, as when the lock
// holder wrote a newer candidate meanwhile and deleted the older content,
// is read again from the new pointer, up to three times.
func (s *Store) Candidate() (Entry, error) {
	var (
		e   Entry
		err error
	)
	for range 3 {
		e, err = s.read(CandidateFile)
		if !errors.Is(err, fs.ErrNotExist) {
			return e, err
		}
		again, _, perr := s.pointer(CandidateFile)
		if perr != nil || again == e.Digest {
			return e, err
		}
	}
	return e, err
}

// Revision reads the content file of d, such as the newer candidate a
// handover holder asks its successor to reload. A missing file matches
// fs.ErrNotExist.
func (s *Store) Revision(d revision.Digest) (Entry, error) {
	path := s.Path(ContentName(d))
	// One byte past the limit is enough for the pipeline to report the
	// content as too large (RZ-CFG-001); the rest is never read.
	data, err := readFile(path, s.maxBytes+1)
	if err != nil && !errors.Is(err, errTooLarge) {
		return Entry{Digest: d, Path: path}, fmt.Errorf("lkg: read %s: %w", path, err)
	}
	return Entry{Digest: d, Content: data, Path: path}, nil
}

// Pointers returns the digests candidate.json and lkg.json name, each
// zero when absent; I/O errors other than a missing file are returned
// with what was read.
func (s *Store) Pointers() (candidate, lastKnownGood revision.Digest, err error) {
	var errs []error
	for _, p := range []struct {
		name string
		to   *revision.Digest
	}{{CandidateFile, &candidate}, {LKGFile, &lastKnownGood}} {
		d, _, perr := s.pointer(p.name)
		switch {
		case perr == nil:
			*p.to = d
		case !errors.Is(perr, ErrAbsent):
			errs = append(errs, perr)
		}
	}
	return candidate, lastKnownGood, errors.Join(errs...)
}

// read reads the pointer name and the content it names.
func (s *Store) read(name string) (Entry, error) {
	d, p, err := s.pointer(name)
	if err != nil {
		return Entry{}, err
	}
	if s.afterPointer != nil {
		s.afterPointer()
	}
	e, err := s.Revision(d)
	e.Pointer = p
	return e, err
}

// pointer reads and checks the pointer name: ErrAbsent when it is
// missing, malformed or of an unknown format.
func (s *Store) pointer(name string) (revision.Digest, Pointer, error) {
	path := s.Path(name)
	data, err := readFile(path, maxPointerBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return revision.Digest{}, Pointer{}, fmt.Errorf("%w: %s: %w", ErrAbsent, path, err)
	case errors.Is(err, errTooLarge):
		s.log.Warn("Last-Known-Good pointer is unreadable; it counts as absent", catalog.KeyPath, path, catalog.KeyError, err.Error())
		return revision.Digest{}, Pointer{}, fmt.Errorf("%w: %s: %w", ErrAbsent, path, err)
	case err != nil:
		return revision.Digest{}, Pointer{}, fmt.Errorf("lkg: read %s: %w", path, err)
	}
	var p Pointer
	if err := json.Unmarshal(data, &p); err != nil {
		s.log.Warn("Last-Known-Good pointer is unreadable; it counts as absent", catalog.KeyPath, path, catalog.KeyError, err.Error())
		return revision.Digest{}, Pointer{}, fmt.Errorf("%w: %s is not a pointer: %w", ErrAbsent, path, err)
	}
	if p.Format != Format {
		return revision.Digest{}, p, fmt.Errorf("%w: %s has format %q, want %q", ErrAbsent, path, p.Format, Format)
	}
	d, err := revision.Parse(p.Digest)
	if err != nil {
		s.log.Warn("Last-Known-Good pointer is unreadable; it counts as absent", catalog.KeyPath, path, catalog.KeyError, err.Error())
		return revision.Digest{}, p, fmt.Errorf("%w: %s: digest: %w", ErrAbsent, path, err)
	}
	return d, p, nil
}

// Write writes rev under lkg/ through lock, the data-directory lock the
// process holds: its content file, then candidate.json, then, when
// promote, lkg.json (file mode promotes on activation), each atomically,
// then deletes every file no pointer names. A nil lock is ErrNotHolder,
// and a lock released meanwhile fails with nodedir.ErrReleased; neither
// is a failed write. Any other failure raises lkg_write_failed until a
// write succeeds. A file the cleanup cannot delete is logged; the write
// still succeeded.
func (s *Store) Write(lock *nodedir.Lock, rev revision.Revision, promote bool) error {
	if lock == nil {
		return ErrNotHolder
	}
	if rev.Digest.IsZero() {
		return fmt.Errorf("lkg: write: no Revision")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.write(lock, rev, promote); err != nil {
		if !errors.Is(err, nodedir.ErrReleased) {
			s.setFailing(true)
		}
		return err
	}
	s.setFailing(false)
	s.collect(lock)
	return nil
}

// write writes the content file and the pointers.
func (s *Store) write(lock *nodedir.Lock, rev revision.Revision, promote bool) error {
	if err := lock.WriteFile(rel(ContentName(rev.Digest)), rev.Content); err != nil {
		return fmt.Errorf("lkg: write the content of %s: %w", rev.Digest.Short(), err)
	}
	p, err := json.Marshal(Pointer{
		Format: Format, Digest: rev.Digest.String(),
		WrittenAt: s.clk.Now().UTC().Format(time.RFC3339Nano), Version: s.version,
	})
	if err != nil {
		return fmt.Errorf("lkg: encode the pointer: %w", err)
	}
	p = append(p, '\n')
	names := []string{CandidateFile}
	if promote {
		names = append(names, LKGFile)
	}
	for _, n := range names {
		if err := lock.WriteFile(rel(n), p); err != nil {
			return fmt.Errorf("lkg: write %s: %w", n, err)
		}
	}
	return nil
}

// collect deletes every file of lkg/ that no pointer names: content of
// older Revisions, temporary files a crash left, anything else.
// Directories are left alone.
func (s *Store) collect(lock *nodedir.Lock) {
	keep := map[string]bool{CandidateFile: true, LKGFile: true}
	for _, n := range []string{CandidateFile, LKGFile} {
		if d, _, err := s.pointer(n); err == nil {
			keep[ContentName(d)] = true
		}
	}
	entries, err := os.ReadDir(s.dir.LKGPath())
	if err != nil {
		s.log.Warn("Last-Known-Good cleanup could not list lkg/", catalog.KeyError, err.Error())
		return
	}
	for _, e := range entries {
		if e.IsDir() || keep[e.Name()] {
			continue
		}
		if err := lock.Remove(rel(e.Name())); err != nil {
			s.log.Warn("Last-Known-Good cleanup could not delete a file", catalog.KeyPath, s.Path(e.Name()), catalog.KeyError, err.Error())
		}
	}
}

// setFailing raises or clears lkg_write_failed; the caller holds mu.
func (s *Store) setFailing(on bool) {
	if s.failing == on {
		return
	}
	s.failing = on
	if s.status != nil {
		s.status.SetDegraded(catalog.ReasonLKGWriteFailed, statusSource, on)
	}
}

// Failing reports whether lkg_write_failed is raised: the last write
// failed.
func (s *Store) Failing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failing
}

// rel returns the data-directory path of name in lkg/.
func rel(name string) string { return nodedir.LKGDir + "/" + name }

// errTooLarge reports a file larger than a read allows.
var errTooLarge = errors.New("larger than the read limit")

// readFile reads path, a regular file, without following a symbolic link
// or blocking on a FIFO: at most limit bytes, errTooLarge when it holds
// more.
func readFile(path string, limit int64) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode %v)", path, fi.Mode())
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return data[:limit], fmt.Errorf("%s: %w of %d bytes", path, errTooLarge, limit)
	}
	return data, nil
}
