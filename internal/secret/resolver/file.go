// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"time"
)

// racyWindow is how close to a read a file's modification time must be for
// the read to be re-checked on the next poll: a same-size rewrite inside
// the file system's timestamp granularity leaves size, time and inode
// unchanged.
const racyWindow = 2 * time.Second

// Fixed reasons of the file provider; none repeats file content (spec 06
// requirement 89).
var (
	errNotAbsolute   = errors.New("file name must be an absolute slash path")
	errOutsideRoot   = errors.New("path is outside " + SettingSecretRoot)
	errRootOpen      = errors.New(SettingSecretRoot + " does not exist or cannot be opened")
	errNotExist      = errors.New("file does not exist")
	errPermission    = errors.New("permission denied")
	errEscapes       = errors.New("path escapes " + SettingSecretRoot + " through a symbolic link")
	errNotRegular    = errors.New("not a regular file")
	errWritable      = errors.New("file is writable by group or others")
	errOwner         = errors.New("file owner is not in the allowed owner list")
	errReadFailed    = errors.New("file cannot be read")
	errTooLargeValue = errors.New("value exceeds its size cap")
)

// escapeText is the os.Root error text for a path leaving the root.
const escapeText = "path escapes from parent"

// fingerprint identifies a file state for polling (spec 01 requirement
// 46): a change of size, modification time, inode, mode or owner re-reads
// the file. err is set, and the rest zero, when the file could not be
// examined; a repeated failure then compares equal and is not re-counted.
type fingerprint struct {
	err   string
	size  int64
	mtime int64
	mode  fs.FileMode
	dev   uint64
	ino   uint64
	uid   uint32
}

// fingerprintOf returns the fingerprint of fi.
func fingerprintOf(fi fs.FileInfo) fingerprint {
	fp := fingerprint{size: fi.Size(), mtime: fi.ModTime().UnixNano(), mode: fi.Mode()}
	fp.dev, fp.ino, fp.uid, _ = fileIdentity(fi)
	return fp
}

// failedFingerprint returns the fingerprint of a failure.
func failedFingerprint(err error) fingerprint { return fingerprint{err: err.Error()} }

// racyAt reports whether a read at now cannot trust fp: the file changed
// within racyWindow of now (or in the future).
func (fp fingerprint) racyAt(now time.Time) bool {
	if fp.err != "" {
		return false
	}
	return now.Sub(time.Unix(0, fp.mtime)) < racyWindow
}

// cleanName returns the cleaned absolute slash path of a file reference
// name (spec 06 requirement 88: the cleaned path must lie under the root).
func cleanName(name string) (string, error) {
	if !path.IsAbs(name) {
		return "", errNotAbsolute
	}
	return path.Clean(name), nil
}

// relative returns the cleaned name relative to the secret root, trying
// the root as configured and symlink-resolved (spec 06 requirement 88);
// errOutsideRoot when the name does not lie strictly inside the root.
func (r *Resolver) relative(name, resolvedRoot string) (string, error) {
	clean, err := cleanName(name)
	if err != nil {
		return "", err
	}
	p := filepath.FromSlash(clean)
	for _, root := range []string{r.root, resolvedRoot} {
		if root == "" {
			continue
		}
		if rel, err := filepath.Rel(root, p); err == nil && rel != "." && filepath.IsLocal(rel) {
			return rel, nil
		}
	}
	return "", errOutsideRoot
}

// resolvedRoot returns the symlink-resolved root, or "" when it does not
// resolve.
func (r *Resolver) resolvedRoot() string {
	p, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		return ""
	}
	return p
}

// openRoot opens the secret root; every file read goes through it, so ".."
// and symbolic links cannot leave it (spec 01 requirement 44).
func (r *Resolver) openRoot() (*os.Root, error) {
	root, err := os.OpenRoot(r.root)
	if err != nil {
		return nil, errRootOpen
	}
	return root, nil
}

// statTarget stats rel under root, following symbolic links, and returns
// the path under root to open. os.Root refuses every absolute symbolic
// link, while spec 06 requirement 88 (SEC "Secrets" rule 5) accepts a
// path whose symlink-resolved form lies under the root. On that refusal
// rel is therefore resolved with filepath.EvalSymlinks and, when the
// result lies inside the symlink-resolved root, stat'ed and later opened
// through root by that link-free path; a link changed in between still
// cannot leave root. A link chain ending at a missing path inside the
// root is errNotExist; any other path keeps errEscapes.
func statTarget(root *os.Root, rel string) (string, fs.FileInfo, error) {
	fi, err := root.Stat(rel)
	if err == nil {
		return rel, fi, nil
	}
	err = fileError(err)
	if !errors.Is(err, errEscapes) {
		return rel, nil, err
	}
	target, ok := resolveInside(root.Name(), rel)
	if !ok {
		if danglingInside(root.Name(), rel) {
			return rel, nil, errNotExist
		}
		return rel, nil, err
	}
	fi, err = root.Stat(target)
	if err != nil {
		return rel, nil, fileError(err)
	}
	return target, fi, nil
}

// resolveInside resolves every symbolic link of rel under the directory
// rootName and returns the result relative to the symlink-resolved
// rootName; false when either does not resolve or the result does not lie
// strictly inside the root.
func resolveInside(rootName, rel string) (string, bool) {
	base, err := filepath.EvalSymlinks(rootName)
	if err != nil {
		return "", false
	}
	p, err := filepath.EvalSymlinks(filepath.Join(rootName, rel))
	if err != nil {
		return "", false
	}
	target, err := filepath.Rel(base, p)
	if err != nil || target == "." || !filepath.IsLocal(target) {
		return "", false
	}
	return target, true
}

// maxLinkHops bounds the symbolic links danglingInside follows, as the
// kernel's own limit does.
const maxLinkHops = 40

// danglingInside reports whether rel under the directory rootName is a
// chain of symbolic links ending at a missing path that lies inside the
// symlink-resolved rootName. It only selects the reason reported for a
// path os.Root refused; nothing it finds is opened.
func danglingInside(rootName, rel string) bool {
	base, err := filepath.EvalSymlinks(rootName)
	if err != nil {
		return false
	}
	p := filepath.Join(rootName, rel)
	for range maxLinkHops {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return missingInside(base, p)
		}
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			return false
		}
		link, err := os.Readlink(p)
		if err != nil {
			return false
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(p), link)
		}
		p = filepath.Clean(link)
	}
	return false
}

// missingInside reports whether the missing path p lies strictly inside
// base once its deepest existing ancestor is symlink-resolved.
func missingInside(base, p string) bool {
	dir, rest := filepath.Dir(p), filepath.Base(p)
	for {
		if d, err := filepath.EvalSymlinks(dir); err == nil {
			target, err := filepath.Rel(base, filepath.Join(d, rest))
			return err == nil && target != "." && filepath.IsLocal(target)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir, rest = parent, filepath.Join(filepath.Base(dir), rest)
	}
}

// statFile returns the fingerprint of rel under root, following symbolic
// links that resolve inside the root.
func statFile(root *os.Root, rel string) (fingerprint, error) {
	_, fi, err := statTarget(root, rel)
	if err != nil {
		return failedFingerprint(err), err
	}
	return fingerprintOf(fi), nil
}

// readFile reads rel under root: a regular file not writable by group or
// others, owned by an allowed user when Config.FileOwners is set, of at
// most limit bytes. The returned fingerprint belongs to the opened file
// (or the failure). The caller clears the returned bytes when done.
func (r *Resolver) readFile(root *os.Root, rel string, limit int64) ([]byte, fingerprint, error) {
	rel, fi, err := statTarget(root, rel)
	if err != nil {
		return nil, failedFingerprint(err), err
	}
	if !fi.Mode().IsRegular() {
		// Never open a FIFO or device: opening can block or act.
		return nil, fingerprintOf(fi), errNotRegular
	}
	f, err := root.OpenFile(rel, openFlags, 0)
	if err != nil {
		err = fileError(err)
		return nil, failedFingerprint(err), err
	}
	defer func() { _ = f.Close() }()
	fi, err = f.Stat()
	if err != nil {
		return nil, failedFingerprint(errReadFailed), errReadFailed
	}
	fp := fingerprintOf(fi)
	if !fi.Mode().IsRegular() {
		return nil, fp, errNotRegular
	}
	if err := r.checkOwnerMode(fi); err != nil {
		return nil, fp, err
	}
	if fi.Size() > limit {
		return nil, fp, tooLarge(limit)
	}
	b, err := readLimited(f, fi.Size(), limit)
	if err != nil {
		return nil, fp, err
	}
	return b, fp, nil
}

// readLimited reads f to its end into one buffer sized for the expected
// size, growing it by copying and clearing the old buffer, so no stray
// copy of the content is left behind; more than limit bytes is a size cap
// failure.
func readLimited(f io.Reader, size, limit int64) ([]byte, error) {
	buf := make([]byte, 0, min(max(size, 0)+512, limit+1))
	for {
		if len(buf) == cap(buf) {
			if int64(len(buf)) > limit {
				clear(buf)
				return nil, tooLarge(limit)
			}
			grown := make([]byte, len(buf), min(int64(2*cap(buf))+512, limit+1))
			copy(grown, buf)
			clear(buf)
			buf = grown
		}
		n, err := f.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			clear(buf)
			return nil, errReadFailed
		}
	}
	if int64(len(buf)) > limit {
		clear(buf)
		return nil, tooLarge(limit)
	}
	return buf, nil
}

// tooLarge returns the size cap failure.
func tooLarge(limit int64) error { return fmt.Errorf("%w of %d bytes", errTooLargeValue, limit) }

// fileError maps an os error to a fixed reason; the result never includes
// anything read from the file.
func fileError(err error) error {
	var pe *fs.PathError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errNotExist
	case errors.Is(err, fs.ErrPermission):
		return errPermission
	case errors.As(err, &pe) && pe.Err != nil && pe.Err.Error() == escapeText:
		return errEscapes
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("%w: %s", errReadFailed, errno.Error())
	}
	return errReadFailed
}

// checkOwnerMode enforces the mode and owner rules on an opened file (Unix
// only): not writable by group or others and, when Config.FileOwners is
// set, owned by one of its users.
func (r *Resolver) checkOwnerMode(fi fs.FileInfo) error {
	if !ownerModeChecked {
		return nil
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return errWritable
	}
	_, _, uid, ok := fileIdentity(fi)
	if !ok || len(r.owners) == 0 {
		return nil
	}
	for _, o := range r.owners {
		if o >= 0 && uint64(o) == uint64(uid) {
			return nil
		}
	}
	return errOwner
}
