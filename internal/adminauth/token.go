// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminauth

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"unicode"
)

// Token limits (spec 06 requirement 93; the maximum is proposed here).
const (
	// MinTokenBytes is the shortest accepted token after trimming.
	MinTokenBytes = 22
	// MaxTokenBytes is the longest accepted token, configured or
	// presented.
	MaxTokenBytes = 4096
	// maxTokenFileBytes caps a token file read, trailing whitespace
	// included.
	maxTokenFileBytes = 64 << 10
)

// Token file errors. Messages never contain file content.
var (
	// ErrTokenEmpty reports a token file that is empty after trimming.
	ErrTokenEmpty = errors.New("adminauth: the admin token is empty")
	// ErrTokenShort reports a token under MinTokenBytes.
	ErrTokenShort = errors.New("adminauth: the admin token is shorter than 22 bytes")
	// ErrTokenLong reports a token over MaxTokenBytes.
	ErrTokenLong = errors.New("adminauth: the admin token is longer than 4096 bytes")
	// ErrTokenSyntax reports a token with a character outside the RFC 6750
	// b64token syntax, which a Bearer credential cannot carry.
	ErrTokenSyntax = errors.New("adminauth: the admin token has a character outside the RFC 6750 b64token syntax")
	// ErrNotRegular reports a token or TLS file that is not a regular
	// file.
	ErrNotRegular = errors.New("adminauth: not a regular file")
	// ErrTokenRevoked reports a token file that was removed: its token is
	// refused until a valid file appears at the path again.
	ErrTokenRevoked = errors.New("adminauth: the admin token file was removed; its token is revoked")
)

// digest is the SHA-256 of a token; the token itself is never kept.
type digest = [sha256.Size]byte

// readRegular reads at most limit+1 bytes of the regular file at path and
// returns them with the opened file's info. It checks the mode with a stat
// before opening and again on the opened file (closing the window between
// the two), and opens without blocking on unix, so a FIFO or device at
// path is ErrNotRegular instead of a hang.
func readRegular(path string, limit int64) ([]byte, os.FileInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, ErrNotRegular
	}
	f, err := openNoBlock(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	if fi, err = f.Stat(); err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, ErrNotRegular
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	return b, fi, nil
}

// readToken reads the token file at path, trims trailing whitespace,
// validates the token and returns its digest with the opened file's info.
func readToken(path string) (digest, os.FileInfo, error) {
	b, fi, err := readRegular(path, maxTokenFileBytes)
	if err != nil {
		return digest{}, nil, err
	}
	defer clear(b)
	if len(b) > maxTokenFileBytes {
		return digest{}, nil, ErrTokenLong
	}
	tok := bytes.TrimRightFunc(b, unicode.IsSpace)
	if err := checkToken(tok); err != nil {
		return digest{}, nil, err
	}
	return sha256.Sum256(tok), fi, nil
}

// checkToken validates a configured token.
func checkToken(tok []byte) error {
	switch {
	case len(tok) == 0:
		return ErrTokenEmpty
	case len(tok) < MinTokenBytes:
		return ErrTokenShort
	case len(tok) > MaxTokenBytes:
		return ErrTokenLong
	case !b64token(tok):
		return ErrTokenSyntax
	}
	return nil
}

// b64token reports whether s matches RFC 6750 section 2.1:
// 1*( ALPHA / DIGIT / "-" / "." / "_" / "~" / "+" / "/" ) *"=".
func b64token[T string | []byte](s T) bool {
	i := 0
	for i < len(s) && b64char(s[i]) {
		i++
	}
	if i == 0 {
		return false
	}
	for ; i < len(s); i++ {
		if s[i] != '=' {
			return false
		}
	}
	return true
}

// b64char reports whether c is a b64token character other than "=".
func b64char(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	switch c {
	case '-', '.', '_', '~', '+', '/':
		return true
	}
	return false
}

// tokenFile is one configured token file. The digest in use is published
// through an atomic pointer; a refresh re-reads the file when its
// identity, size or modification time changed, so a rotated token is
// picked up without a restart.
//
// Rotation rule: a rotation to an empty, invalid, unreadable or
// non-regular file keeps the last valid token (it covers non-atomic
// rewrites); removing the file revokes the token (requests presenting it
// get RZ-AUTH-002) until a valid file appears at the path again. A restart
// with the file still absent refuses start.
type tokenFile struct {
	path string
	// cur is the digest in use; nil once the file was removed.
	cur atomic.Pointer[digest]

	// mu serializes refreshes; the request path only tries it, and waits
	// for a refresh in progress only when the presented token matched
	// nothing.
	mu sync.Mutex
	// seen is the file info of the last read, or of the last stat whose
	// read failed (nil after a failed stat).
	seen os.FileInfo
	// failing is set while the file cannot be used, so a failure is
	// reported once per episode.
	failing bool
}

// loadTokenFile reads path at start; any error refuses start.
func loadTokenFile(env, path string) (*tokenFile, error) {
	d, fi, err := readToken(path)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", env, path, err)
	}
	tf := &tokenFile{path: path, seen: fi}
	tf.cur.Store(&d)
	return tf, nil
}

// match compares d with the token in use in constant time; a revoked
// token matches nothing.
func (tf *tokenFile) match(d *digest) bool {
	cur := tf.cur.Load()
	if cur == nil {
		return false
	}
	return subtle.ConstantTimeCompare(d[:], cur[:]) == 1
}

// refreshResult is what a refresh did.
type refreshResult uint8

const (
	// unchanged: the file did not change, or a failure already reported.
	unchanged refreshResult = iota
	// rotated: a new token is in use.
	rotated
	// failed: the file changed into one that cannot be used, or cannot
	// be checked; the last valid token stays in use.
	failed
	// revoked: the file was removed and its token is no longer accepted.
	revoked
	// busy: another refresh was in progress; the caller did not wait.
	busy
)

// tryRefresh is the request-path refresh: when another refresh runs, it
// returns busy at once.
func (tf *tokenFile) tryRefresh() (refreshResult, error) {
	if !tf.mu.TryLock() {
		return busy, nil
	}
	defer tf.mu.Unlock()
	return tf.refreshLocked(false)
}

// wait returns once the refresh in progress, if any, has published its
// result. It never starts a refresh.
func (tf *tokenFile) wait() {
	tf.mu.Lock()
	defer tf.mu.Unlock()
}

// reload re-reads the file whatever its info says.
func (tf *tokenFile) reload() (refreshResult, error) {
	tf.mu.Lock()
	defer tf.mu.Unlock()
	return tf.refreshLocked(true)
}

// refreshLocked re-reads the file when forced or when it changed.
func (tf *tokenFile) refreshLocked(force bool) (refreshResult, error) {
	fi, err := os.Stat(tf.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		tf.seen = nil
		return tf.revoke(force, err)
	case err != nil:
		tf.seen = nil
		return tf.fail(force, err)
	}
	if !force && tf.seen != nil && sameFile(tf.seen, fi) {
		return unchanged, nil
	}
	tf.seen = fi
	if !fi.Mode().IsRegular() {
		// Checked before readToken opens it: opening a FIFO blocks.
		return tf.fail(true, ErrNotRegular)
	}
	d, rfi, err := readToken(tf.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Removed between the stat and the read.
		tf.seen = nil
		return tf.revoke(true, err)
	case err != nil:
		return tf.fail(true, err)
	}
	tf.seen = rfi
	tf.failing = false
	if cur := tf.cur.Load(); cur != nil && *cur == d {
		return unchanged, nil
	}
	tf.cur.Store(&d)
	return rotated, nil
}

// fail records a failure; report is false for a repeat of a failure
// already reported by a lazy refresh.
func (tf *tokenFile) fail(report bool, err error) (refreshResult, error) {
	if !report && tf.failing {
		return unchanged, nil
	}
	tf.failing = true
	return failed, fmt.Errorf("%s: %w", tf.path, err)
}

// revoke drops the token of a removed file. A lazy refresh reports it
// once; Reload always returns it.
func (tf *tokenFile) revoke(report bool, err error) (refreshResult, error) {
	dropped := tf.cur.Swap(nil) != nil
	if !dropped && !report && tf.failing {
		return unchanged, nil
	}
	tf.failing = true
	return revoked, fmt.Errorf("%s: %w: %w", tf.path, ErrTokenRevoked, err)
}

// sameFile reports whether a and b describe the same file content by
// identity, size and modification time.
func sameFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}
