// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"crypto/sha256"
	"hash"
	"io"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
)

// hashingReader hashes what it reads, so a streamed object verifies
// itself without a second pass.
type hashingReader struct {
	r io.Reader
	h hash.Hash
}

// newHashingReader wraps r.
func newHashingReader(r io.Reader) *hashingReader { return &hashingReader{r: r, h: sha256.New()} }

// Read reads and hashes.
func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	h.h.Write(p[:n])
	return n, err
}

// digest returns the digest of everything read.
func (h *hashingReader) digest() controlstore.Digest {
	var d controlstore.Digest
	copy(d[:], h.h.Sum(nil))
	return d
}
