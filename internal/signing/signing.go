// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package signing holds the digest checks every Revision and Plugin
// artifact goes through (spec 06 section 2.16, architecture R-10 and R-50).
// revision.Digest is the only digest type: an expected digest is always
// the full sha256:<64 lowercase hex>, never the display form rev-<12 hex>.
// Content that does not hash to its digest is RZ-CFG-027 for Revisions
// and RZ-CFG-028 for Plugin artifacts. The checks are always on and not
// configurable.
//
// M1 verifies digests only (DigestOnly). Verifier is the M2 extension
// point: WithSignature layers a Sigstore or Control-mode signature
// verifier after the digest check, which it can never skip.
// VerifyArtifact streams a Plugin artifact through SHA-256 under a size
// limit; no Plugin artifact reaches a Node or the CLI in M1, and the fetch
// wiring lands with Plugins in M2 (R-50).
package signing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// RZ codes raised by this package.
const (
	// CodeRevision is a Revision whose content does not hash to its digest.
	CodeRevision = "RZ-CFG-027"
	// CodeArtifact is a Plugin artifact that is unavailable, too large or
	// does not hash to its digest.
	CodeArtifact = "RZ-CFG-028"
)

// digestPrefix is the algorithm prefix of the wire form.
const digestPrefix = "sha256:"

// chunkSize is the read size of VerifyArtifact and ReadArtifact.
const chunkSize = 32 << 10

// maxEmptyReads is how many consecutive (0, nil) reads end a stream with
// io.ErrNoProgress (the bufio rule), so a broken reader cannot spin.
const maxEmptyReads = 100

// Errors. Messages never echo the content or the string being parsed.
var (
	// ErrDisplayForm reports a rev-<12 hex> string given as an expected
	// digest; the display form is for humans, logs and labels only.
	ErrDisplayForm = errors.New("signing: the display form rev-<12 hex> is never an expected digest")
	// ErrMismatch reports content that does not hash to its digest.
	ErrMismatch = errors.New("signing: content does not hash to its expected digest")
	// ErrNoDigest reports a verification without an expected digest.
	ErrNoDigest = errors.New("signing: no expected digest")
	// ErrTooLarge reports an artifact longer than its size limit.
	ErrTooLarge = errors.New("signing: artifact exceeds its size limit")
	// ErrLimit reports a size limit that is not positive.
	ErrLimit = errors.New("signing: the size limit must be positive")
)

// ParseDigest parses an expected digest: exactly "sha256:" followed by 64
// lowercase hex characters (spec 06 requirement 97). The display form
// rev-<12 hex>, uppercase hex, other lengths, a missing prefix and other
// algorithms are rejected. Every error matches revision.ErrSyntax; the
// display form also matches ErrDisplayForm.
func ParseDigest(s string) (revision.Digest, error) {
	d, err := revision.Parse(s)
	if err == nil {
		return d, nil
	}
	return revision.Digest{}, syntaxError(s, err)
}

// syntaxError explains why s is not a wire-form digest without quoting it.
func syntaxError(s string, err error) error {
	if strings.HasPrefix(s, "rev-") {
		return fmt.Errorf("%w: %w", err, ErrDisplayForm)
	}
	h, ok := strings.CutPrefix(s, digestPrefix)
	if !ok {
		return fmt.Errorf("%w: missing the sha256: prefix", err)
	}
	if len(h) != 2*sha256.Size {
		return fmt.Errorf("%w: %d hex characters, want %d", err, len(h), 2*sha256.Size)
	}
	for i := 0; i < len(h); i++ {
		if c := h[i]; c >= 'A' && c <= 'F' {
			return fmt.Errorf("%w: uppercase hex", err)
		}
	}
	return fmt.Errorf("%w: a character that is not lowercase hex", err)
}

// VerifyRevision checks that content (the exact ruralz.canonical.v1
// bytes) hashes to want. A mismatch or a zero want is RZ-CFG-027.
func VerifyRevision(content []byte, want revision.Digest) error {
	if err := compare(revision.Sum(content), want); err != nil {
		return errcode.Wrap(CodeRevision, err)
	}
	return nil
}

// compare checks got against want; a zero want never matches.
func compare(got, want revision.Digest) error {
	if want.IsZero() {
		return ErrNoDigest
	}
	if got != want {
		return fmt.Errorf("%w: want %s, got %s", ErrMismatch, want, got)
	}
	return nil
}

// VerifyArtifact streams r to its end through SHA-256 and checks the sum
// against want. It reads at most limit+1 bytes: longer content is
// RZ-CFG-028 ErrTooLarge as soon as the extra byte arrives. A read error
// (the artifact is unavailable), a mismatch and a zero want are also
// RZ-CFG-028; ctx is checked between reads and its error is returned
// as is. limit must be positive.
func VerifyArtifact(ctx context.Context, r io.Reader, want revision.Digest, limit int64) error {
	return stream(ctx, r, want, limit, nil)
}

// ReadArtifact is VerifyArtifact that also returns the content, only when
// it hashes to want; on any error the bytes read are discarded.
func ReadArtifact(ctx context.Context, r io.Reader, want revision.Digest, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	if err := stream(ctx, r, want, limit, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// stream reads r up to limit+1 bytes, hashing and copying to sink (when
// not nil), and verifies the sum.
func stream(ctx context.Context, r io.Reader, want revision.Digest, limit int64, sink *bytes.Buffer) error {
	if limit <= 0 {
		return ErrLimit
	}
	if want.IsZero() {
		return errcode.Wrap(CodeArtifact, ErrNoDigest)
	}
	// Read one byte past the limit to tell "exactly limit" from "more".
	readMax := limit
	if limit < math.MaxInt64 {
		readMax++
	}
	h := sha256.New()
	buf := make([]byte, min(int64(chunkSize), readMax))
	var total int64
	empty := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := buf[:min(int64(len(buf)), readMax-total)]
		n, err := r.Read(p)
		if n == 0 && err == nil {
			if empty++; empty >= maxEmptyReads {
				return errcode.Wrap(CodeArtifact, fmt.Errorf("signing: read artifact: %w", io.ErrNoProgress))
			}
			continue
		}
		empty = 0
		if n > 0 {
			total += int64(n)
			if total > limit {
				return errcode.Wrap(CodeArtifact, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, limit))
			}
			_, _ = h.Write(p[:n])
			if sink != nil {
				_, _ = sink.Write(p[:n])
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errcode.Wrap(CodeArtifact, fmt.Errorf("signing: read artifact: %w", err))
		}
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	if err := compare(digestOf(sum), want); err != nil {
		return errcode.Wrap(CodeArtifact, err)
	}
	return nil
}

// digestOf converts a raw sum to a revision.Digest, whose bytes are
// private and set only by Sum and Parse; the hex round trip is one
// allocation per verification.
func digestOf(sum [sha256.Size]byte) revision.Digest {
	d, _ := revision.Parse(digestPrefix + hex.EncodeToString(sum[:]))
	return d
}

// Verifier checks that content is what want names. The M1 implementation
// is DigestOnly; M2 adds Sigstore (OCI Revisions and Plugins) and
// Control-mode signatures through WithSignature. Implementations are safe
// for concurrent use and never include content in errors.
type Verifier interface {
	// Verify returns nil when content is accepted under want, else an
	// error carrying an RZ code.
	Verify(ctx context.Context, content []byte, want revision.Digest) error
}

// DigestOnly is the M1 Verifier: the SHA-256 digest check of Revisions,
// RZ-CFG-027 on mismatch.
type DigestOnly struct{}

// Verify implements Verifier with VerifyRevision.
func (DigestOnly) Verify(_ context.Context, content []byte, want revision.Digest) error {
	return VerifyRevision(content, want)
}

// Default returns the Verifier of this release: DigestOnly.
func Default() Verifier { return DigestOnly{} }

// WithSignature returns a Verifier that runs the digest check first and,
// only when it passes, sig (the M2 Sigstore or Control-mode slot). The
// digest check is always on: a nil sig gives DigestOnly.
func WithSignature(sig Verifier) Verifier {
	if sig == nil {
		return DigestOnly{}
	}
	return layered{sig: sig}
}

// layered is DigestOnly followed by a signature verifier.
type layered struct{ sig Verifier }

// Verify implements Verifier.
func (l layered) Verify(ctx context.Context, content []byte, want revision.Digest) error {
	if err := VerifyRevision(content, want); err != nil {
		return err
	}
	return l.sig.Verify(ctx, content, want)
}
