// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package nodedir owns the layout of a Node's data directory,
// ${RURALZ_DATA_DIR} (spec 04 requirement 5; pack 8.11):
//
//	lock              empty file; flock(LOCK_EX|LOCK_NB) for the holder's lifetime
//	holder.json       PID record ruralz.holder.v1, written after taking the lock
//	handover.sock     owner-only Unix socket for handover readiness gating
//	identity/node-id  node.id: 26-character ULID plus "\n", never rewritten
//	lkg/              Last-Known-Good and candidate
//	cache/oci/sha256/ reserved, Planned (M2)
//
// Directories are created 0700 and files 0600. Only the process holding
// the lock writes holder.json and files under lkg/ (spec 04 requirement
// 7); the write methods live on Lock so nothing can write after Release.
//
// Paths, holder.json and node.id are platform-neutral. The lock uses the
// standard library's BSD flock on Linux, macOS and the BSDs (R-26), and a
// stub returning errors.ErrUnsupported elsewhere (R-60), so the ruralz CLI
// still builds for Windows and reads holder.json with ReadHolderAt.
package nodedir

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ravindu-rev/ruralz/internal/ulid"
)

// DefaultRoot is the data directory when RURALZ_DATA_DIR is unset (spec 04
// requirement 2, spec 10 requirement 94).
const DefaultRoot = "/var/lib/ruralz"

// Names under the data directory, slash-separated (spec 04 requirement 5).
const (
	// LockFile is the flock file.
	LockFile = "lock"
	// HolderFile is the lock holder's PID record.
	HolderFile = "holder.json"
	// HandoverSocket is the holder's handover gate socket.
	HandoverSocket = "handover.sock"
	// IdentityDir holds node.id and, in Control mode (M2), the Enrollment
	// identity.
	IdentityDir = "identity"
	// NodeIDFile holds node.id.
	NodeIDFile = "identity/node-id"
	// LKGDir holds the Last-Known-Good and candidate files.
	LKGDir = "lkg"
	// OCICacheDir is reserved for verified OCI artifacts, Planned (M2);
	// Open does not create it.
	OCICacheDir = "cache/oci/sha256"
)

// Permissions of everything created under the data directory.
const (
	// DirPerm is the mode of created directories.
	DirPerm fs.FileMode = 0o700
	// FilePerm is the mode of created files.
	FilePerm fs.FileMode = 0o600
)

// Errors of the data directory.
var (
	// ErrLocked reports that another open file description, in this or
	// another process, holds the lock.
	ErrLocked = errors.New("nodedir: data dir locked by a live process")
	// ErrReleased reports a write through a Lock after Release.
	ErrReleased = errors.New("nodedir: lock released")
	// ErrRelativeRoot reports a data directory that is not absolute.
	ErrRelativeRoot = errors.New("nodedir: data dir is not an absolute path")
	// ErrNotDirectory reports a data directory path that is not a
	// directory.
	ErrNotDirectory = errors.New("nodedir: not a directory")
	// ErrNotOwner reports a data directory not owned by the effective
	// user.
	ErrNotOwner = errors.New("nodedir: not owned by the effective user")
	// ErrWritableByOthers reports a data directory, identity/ or lkg/
	// that every user may write (mode bit 0o002).
	ErrWritableByOthers = errors.New("nodedir: writable by all users")
	// ErrNodeIDInvalid reports a node-id file that does not hold one ULID.
	ErrNodeIDInvalid = errors.New("nodedir: invalid node-id file")
	// ErrBadName reports a relative path that leaves the data directory
	// or a file name with a separator.
	ErrBadName = errors.New("nodedir: invalid file name")
)

// Dir is an opened data directory. It is safe for concurrent use; it
// holds no open files.
type Dir struct {
	root string
}

// Open prepares the data directory at root: it creates root, identity/
// and lkg/ with mode 0700 when missing, and rejects any of them that is
// not a directory, is not owned by the effective user, or is writable by
// all users (on Unix; a group-writable mode is accepted). A successor in
// a handover opens the same directory while the holder runs; Open never
// writes files.
func Open(root string) (*Dir, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("%w: %q", ErrRelativeRoot, root)
	}
	root = filepath.Clean(root)
	for _, dir := range []string{root, filepath.Join(root, IdentityDir), filepath.Join(root, LKGDir)} {
		if err := ensureDir(dir); err != nil {
			return nil, err
		}
	}
	return &Dir{root: root}, nil
}

// ensureDir creates dir with DirPerm (exactly, whatever the umask), and
// missing parents owner-only, or checks an existing dir.
func ensureDir(dir string) error {
	err := os.Mkdir(dir, DirPerm)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), DirPerm); err != nil {
			return fmt.Errorf("nodedir: %w", err)
		}
		err = os.Mkdir(dir, DirPerm)
	}
	switch {
	case err == nil:
		if err := os.Chmod(dir, DirPerm); err != nil {
			return fmt.Errorf("nodedir: %w", err)
		}
		return nil
	case !errors.Is(err, fs.ErrExist):
		return fmt.Errorf("nodedir: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("nodedir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s", ErrNotDirectory, dir)
	}
	if err := checkDir(fi); err != nil {
		return fmt.Errorf("%w: %s", err, dir)
	}
	return nil
}

// Root returns the cleaned absolute data directory.
func (d *Dir) Root() string { return d.root }

// Path returns the path of rel, a slash-separated name under the data
// directory such as LockFile or NodeIDFile.
func (d *Dir) Path(rel string) string {
	return filepath.Join(d.root, filepath.FromSlash(rel))
}

// LockPath returns the path of the lock file.
func (d *Dir) LockPath() string { return d.Path(LockFile) }

// HolderPath returns the path of holder.json.
func (d *Dir) HolderPath() string { return d.Path(HolderFile) }

// HandoverSocketPath returns the path of handover.sock. A Unix socket
// path is limited to about 104 bytes (sun_path); the handover package
// checks the length when it binds.
func (d *Dir) HandoverSocketPath() string { return d.Path(HandoverSocket) }

// NodeIDPath returns the path of the node-id file.
func (d *Dir) NodeIDPath() string { return d.Path(NodeIDFile) }

// LKGPath returns the path of the Last-Known-Good directory.
func (d *Dir) LKGPath() string { return d.Path(LKGDir) }

// localPath resolves rel under the root, rejecting a path that leaves it.
func (d *Dir) localPath(rel string) (string, error) {
	p := filepath.FromSlash(rel)
	if !filepath.IsLocal(p) {
		return "", fmt.Errorf("%w: %q", ErrBadName, rel)
	}
	return filepath.Join(d.root, p), nil
}

// NodeID returns node.id, creating identity/node-id with gen on first
// boot (spec 04 requirements 5 and 6). An existing file is never
// rewritten: two processes starting at once both return the ULID of the
// one that linked its file first. gen is typically
// func() (ulid.ULID, error) { return ulid.New(clk.Now(), nil) }.
func (d *Dir) NodeID(gen func() (ulid.ULID, error)) (ulid.ULID, error) {
	path := d.NodeIDPath()
	id, err := readNodeID(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	if gen == nil {
		return ulid.ULID{}, errors.New("nodedir: NodeID needs a generator")
	}
	id, err = gen()
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("nodedir: generating node.id: %w", err)
	}
	data, _ := id.AppendText(make([]byte, 0, ulid.EncodedLen+1))
	data = append(data, '\n')
	if err := createExclusive(filepath.Dir(path), filepath.Base(path), data); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return readNodeID(path)
		}
		return ulid.ULID{}, err
	}
	return id, nil
}

// readNodeID reads a node-id file: 26 characters, optionally followed by
// one "\n".
func readNodeID(path string) (ulid.ULID, error) {
	data, err := readLimited(path, 64)
	if err != nil {
		return ulid.ULID{}, err
	}
	s := string(data)
	if n := len(s); n > 0 && s[n-1] == '\n' {
		s = s[:n-1]
	}
	id, err := ulid.Parse(s)
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("%w %s: %w", ErrNodeIDInvalid, path, err)
	}
	return id, nil
}

// readLimited reads the regular file path, refusing more than limit
// bytes. On Unix the open neither follows a final symbolic link nor
// blocks on a FIFO or device (openNoFollow), and the type check uses the
// opened file, so the checked file is the read file.
func readLimited(path string, limit int64) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("nodedir: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("nodedir: %s is not a regular file (mode %v)", path, fi.Mode())
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("nodedir: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("nodedir: %s is larger than %d bytes", path, limit)
	}
	return data, nil
}
