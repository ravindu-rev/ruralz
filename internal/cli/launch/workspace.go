// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// tokenBytes is the randomness of the generated operator token: 32 bytes,
// 43 base64url characters, all RFC 6750 b64token characters, within the
// 22 to 4,096 a token file holds.
const tokenBytes = 32

// stagedConfig is the file a test render is written to before it is
// checked and renamed over ConfigFile; ruralzd watches only ConfigFile.
const stagedConfig = ".bundle.yaml.next"

// Workspace is the private directory of one launch: mode 0700, its path
// with symbolic links resolved, so every path below it is written as
// ruralzd resolves it.
type Workspace struct {
	root string
}

// NewWorkspace creates the private directory of one launch in the
// system's temporary directory, its name starting with prefix, with the
// data directory and the operator token file, a fresh token drawn from rnd
// (nil is crypto/rand.Reader). A failure removes what it created and
// exits 2.
func NewWorkspace(prefix string, rnd io.Reader) (w *Workspace, err error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return nil, noResult("create the private directory", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return nil, noResult("resolve the private directory", err)
	}
	w = &Workspace{root: dir}
	if err := os.Mkdir(w.DataDir(), 0o700); err != nil {
		return nil, noResult("create the data directory", err)
	}
	var raw [tokenBytes]byte
	if _, err := io.ReadFull(rnd, raw[:]); err != nil {
		return nil, noResult("draw the operator token", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:]) + "\n"
	if err := os.WriteFile(w.TokenFile(), []byte(token), 0o600); err != nil {
		return nil, noResult("write the operator token", err)
	}
	return w, nil
}

// Root returns the private directory.
func (w *Workspace) Root() string { return w.root }

// Config returns the test render's path, RURALZ_CONFIG.
func (w *Workspace) Config() string { return filepath.Join(w.root, ConfigFile) }

// DataDir returns RURALZ_DATA_DIR.
func (w *Workspace) DataDir() string { return filepath.Join(w.root, DataDir) }

// SecretRoot returns the private RURALZ_SECRET_ROOT of secret overrides.
func (w *Workspace) SecretRoot() string { return filepath.Join(w.root, SecretDir) }

// TokenFile returns the operator token file, RURALZ_ADMIN_TOKEN_FILE.
func (w *Workspace) TokenFile() string { return filepath.Join(w.root, TokenFile) }

// staged returns the path a test render is staged at.
func (w *Workspace) staged() string { return filepath.Join(w.root, stagedConfig) }

// Holder reads ruralzd's holder.json in the data directory.
func (w *Workspace) Holder() (nodedir.Holder, error) { return nodedir.ReadHolderAt(w.DataDir()) }

// Remove deletes the private directory and everything in it; call it
// once the child has exited.
func (w *Workspace) Remove() error {
	if err := os.RemoveAll(w.root); err != nil {
		return fmt.Errorf("remove the private directory %s: %w", w.root, err)
	}
	return nil
}

// writeAtomic writes data to path with mode 0600 through a temporary file
// in its directory that is renamed over it, so a reader sees the old file
// or the new one, never a part.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
