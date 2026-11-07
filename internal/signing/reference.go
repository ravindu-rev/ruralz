// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// ErrReference reports a string that is not an OCI artifact reference.
var ErrReference = errors.New("signing: not an OCI artifact reference")

// maxTag is the longest tag the OCI Distribution Specification allows.
const maxTag = 128

// Reference names an OCI artifact: REGISTRY/REPOSITORY with a tag, a
// digest, or both, as in REGISTRY/REPOSITORY:TAG@sha256:<64 hex>, where
// the digest pins the artifact and the tag only labels it. The registry
// part is always explicit: a reference never defaults to a registry.
type Reference struct {
	// Registry is the registry's host and optional port.
	Registry string
	// Repository is the repository path inside the registry.
	Repository string
	// Tag is the tag, empty when the reference has none.
	Tag string
	// Digest is the manifest digest, zero when the reference has none.
	Digest revision.Digest
}

// ParseReference parses REGISTRY/REPOSITORY followed by :TAG, by
// @sha256:<64 lowercase hex>, or by both, tag first. The repository and
// tag follow the OCI Distribution Specification grammar; the digest is the
// full wire form ParseDigest accepts, never another algorithm or the
// display form. Every error matches ErrReference.
func ParseReference(s string) (Reference, error) {
	reg, rest, ok := strings.Cut(s, "/")
	if !ok || reg == "" || rest == "" {
		return Reference{}, fmt.Errorf("%w: want REGISTRY/REPOSITORY:TAG or REGISTRY/REPOSITORY@sha256:<64 hex>", ErrReference)
	}
	if err := checkRegistry(reg); err != nil {
		return Reference{}, err
	}
	ref := Reference{Registry: reg}
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		d, err := ParseDigest(rest[i+1:])
		if err != nil {
			return Reference{}, fmt.Errorf("%w: digest: %w", ErrReference, err)
		}
		ref.Digest, rest = d, rest[:i]
	}
	if i := strings.LastIndexByte(rest, ':'); i >= 0 && !strings.Contains(rest[i:], "/") {
		ref.Tag, rest = rest[i+1:], rest[:i]
		if err := checkTag(ref.Tag); err != nil {
			return Reference{}, err
		}
	}
	if err := checkRepository(rest); err != nil {
		return Reference{}, err
	}
	ref.Repository = rest
	if ref.Tag == "" && ref.Digest.IsZero() {
		return Reference{}, fmt.Errorf("%w: %s names no tag and no digest", ErrReference, s)
	}
	return ref, nil
}

// String returns the reference in the form ParseReference reads.
func (r Reference) String() string {
	var b strings.Builder
	b.WriteString(r.Registry)
	b.WriteByte('/')
	b.WriteString(r.Repository)
	if r.Tag != "" {
		b.WriteByte(':')
		b.WriteString(r.Tag)
	}
	if !r.Digest.IsZero() {
		b.WriteByte('@')
		b.WriteString(r.Digest.String())
	}
	return b.String()
}

// Name returns REGISTRY/REPOSITORY, the reference without tag or digest.
func (r Reference) Name() string { return r.Registry + "/" + r.Repository }

// checkRegistry checks a registry host with an optional port: a DNS name
// or IP literal (IPv6 in brackets), no user information or path.
func checkRegistry(reg string) error {
	u, err := url.Parse("oci://" + reg)
	if err != nil || u.Host != reg || u.User != nil || u.Path != "" || u.Hostname() == "" || strings.HasSuffix(reg, ":") {
		return fmt.Errorf("%w: registry %q is not a host with an optional port", ErrReference, reg)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
			return fmt.Errorf("%w: registry %q has an invalid port", ErrReference, reg)
		}
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return nil
	}
	if strings.Contains(reg, "[") {
		return fmt.Errorf("%w: registry %q is not a host with an optional port", ErrReference, reg)
	}
	for _, label := range strings.Split(host, ".") {
		if !dnsLabel(label) {
			return fmt.Errorf("%w: registry %q is not a host with an optional port", ErrReference, reg)
		}
	}
	return nil
}

// dnsLabel reports whether l is a DNS label: 1 to 63 letters, digits and
// inner hyphens.
func dnsLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// checkRepository checks a repository path: slash-separated components
// of lowercase letters and digits joined by ".", "_", "__" or runs of "-".
func checkRepository(repo string) error {
	if repo == "" {
		return fmt.Errorf("%w: empty repository", ErrReference)
	}
	for _, c := range strings.Split(repo, "/") {
		if !repositoryComponent(c) {
			return fmt.Errorf("%w: repository %q is not lowercase letters and digits joined by '.', '_', '__' or '-'", ErrReference, repo)
		}
	}
	return nil
}

// repositoryComponent reports whether c matches
// [a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*.
func repositoryComponent(c string) bool {
	alnum := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' }
	if c == "" || !alnum(c[0]) || !alnum(c[len(c)-1]) {
		return false
	}
	for i := 0; i < len(c); {
		if alnum(c[i]) {
			i++
			continue
		}
		// A separator: ".", "_", "__" or a run of "-", followed by an
		// alphanumeric (the last byte is one, so i+n < len(c)).
		switch c[i] {
		case '.':
			i++
		case '_':
			i++
			if i < len(c) && c[i] == '_' {
				i++
			}
		case '-':
			for i < len(c) && c[i] == '-' {
				i++
			}
		default:
			return false
		}
		if !alnum(c[i]) {
			return false
		}
	}
	return true
}

// checkTag checks a tag: a word character, then up to 127 word
// characters, dots and hyphens.
func checkTag(tag string) error {
	word := func(b byte) bool {
		return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
	}
	ok := tag != "" && len(tag) <= maxTag && word(tag[0])
	for i := 1; ok && i < len(tag); i++ {
		ok = word(tag[i]) || tag[i] == '.' || tag[i] == '-'
	}
	if !ok {
		return fmt.Errorf("%w: tag %q is not 1 to 128 word characters, dots and hyphens starting with a word character", ErrReference, tag)
	}
	return nil
}
