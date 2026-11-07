// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Digest addresses one content object: the SHA-256 of its bytes, so every
// object verifies itself (docs/architecture/04-control-plane-and-gitops.md
// "Control Store and high availability"). A Revision's content object has
// the Revision digest. The zero value means none.
type Digest [sha256.Size]byte

// digestPrefix is the algorithm prefix of the text form.
const digestPrefix = "sha256:"

// Sum returns the digest of b.
func Sum(b []byte) Digest { return sha256.Sum256(b) }

// ParseDigest accepts exactly "sha256:" followed by 64 lowercase hex
// characters, the form every verification carries (pack section 8.1).
func ParseDigest(s string) (Digest, error) {
	h, ok := strings.CutPrefix(s, digestPrefix)
	if !ok || len(h) != 2*sha256.Size || strings.ToLower(h) != h {
		return Digest{}, fmt.Errorf("%w: digest %q is not sha256:<64 lowercase hex>", ErrInvalid, s)
	}
	var d Digest
	if _, err := hex.Decode(d[:], []byte(h)); err != nil {
		return Digest{}, fmt.Errorf("%w: digest %q is not sha256:<64 lowercase hex>", ErrInvalid, s)
	}
	return d, nil
}

// String returns "sha256:<64 hex>".
func (d Digest) String() string { return digestPrefix + hex.EncodeToString(d[:]) }

// IsZero reports whether d is the zero value.
func (d Digest) IsZero() bool { return d == Digest{} }

// MarshalText encodes the text form.
func (d Digest) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText parses the text form.
func (d *Digest) UnmarshalText(b []byte) error {
	v, err := ParseDigest(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// Kind is what a content object holds: the data table of "Control Store
// and high availability" puts Revision content, diffs, plans and sealed
// audit segments in the content store, and "Building and recording a
// Revision" adds the source tree.
type Kind uint8

// Content kinds.
const (
	// KindRevision is a Revision's ruralz.canonical.v1 content.
	KindRevision Kind = iota + 1
	// KindSource is the source tree a Revision was built from.
	KindSource
	// KindDiff is a ruralz.diff.v1 diff.
	KindDiff
	// KindPlan is a Rollout plan: sorted node.id list, batch boundaries
	// and one digest per batch.
	KindPlan
	// KindAuditSegment is a sealed, hash-chained audit segment.
	KindAuditSegment
	numKinds
)

// kindNames are the text forms in Kind order.
func kindNames() []string { return []string{"revision", "source", "diff", "plan", "auditSegment"} }

// String returns the text form, or "" for an unknown kind.
func (k Kind) String() string {
	if k == 0 || k >= numKinds {
		return ""
	}
	return kindNames()[k-1]
}

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool { return k > 0 && k < numKinds }

// MarshalText encodes the text form.
func (k Kind) MarshalText() ([]byte, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("%w: content kind %d", ErrInvalid, k)
	}
	return []byte(k.String()), nil
}

// UnmarshalText parses the text form.
func (k *Kind) UnmarshalText(b []byte) error {
	for i, n := range kindNames() {
		if string(b) == n {
			*k = Kind(i + 1)
			return nil
		}
	}
	return fmt.Errorf("%w: content kind %q", ErrInvalid, b)
}

// MaxAuditSegmentBytes is the largest sealed audit segment: 4 MiB (target)
// (docs/architecture/04-control-plane-and-gitops.md "Audit and security").
const MaxAuditSegmentBytes = 4 << 20

// maxFormatLen bounds a format marker.
const maxFormatLen = 64

// Object is one immutable content object. Format is its format marker:
// objects are never re-encoded, and every release decodes every format
// still referenced or retained
// (docs/engineering/04-release-versioning-and-compatibility.md "Versioned
// surfaces"). The producer names the format, such as ruralz.canonical.v1
// for Revision content and ruralz.diff.v1 for diffs.
type Object struct {
	Kind   Kind
	Format string
	Data   []byte
}

// Digest returns the object's address.
func (o Object) Digest() Digest { return Sum(o.Data) }

// Validate checks the kind, the format marker (1 to 64 printable ASCII
// characters) and the audit segment size bound.
func (o Object) Validate() error {
	if !o.Kind.Valid() {
		return fmt.Errorf("%w: content kind %d", ErrInvalid, o.Kind)
	}
	if err := validFormat(o.Format); err != nil {
		return err
	}
	if o.Kind == KindAuditSegment && len(o.Data) > MaxAuditSegmentBytes {
		return fmt.Errorf("%w: audit segment of %d bytes exceeds %d", ErrInvalid, len(o.Data), MaxAuditSegmentBytes)
	}
	return nil
}

// validFormat checks a format marker.
func validFormat(f string) error {
	if f == "" || len(f) > maxFormatLen {
		return fmt.Errorf("%w: format marker %q is not 1 to %d characters", ErrInvalid, f, maxFormatLen)
	}
	for i := range len(f) {
		if f[i] <= ' ' || f[i] > '~' {
			return fmt.Errorf("%w: format marker %q is not printable ASCII", ErrInvalid, f)
		}
	}
	return nil
}
