// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamsigv4

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Header fields the signature owns: signing drops any value a request
// carries for them and writes its own.
const (
	headerAuthorization = "Authorization"
	headerDate          = "X-Amz-Date"
	headerSecurityToken = "X-Amz-Security-Token" //nolint:gosec // G101: a header field name, not a credential
	headerContentSHA256 = "X-Amz-Content-Sha256"
)

// PayloadUnsigned is the payload hash of config.payload unsigned
// (UNSIGNED-PAYLOAD), sent in X-Amz-Content-Sha256.
const PayloadUnsigned = "UNSIGNED-PAYLOAD"

// emptyPayloadHash is the hex SHA-256 of an empty payload.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// serviceS3 is the signing name of Amazon S3, the one service whose
// canonical path is the request's own: encoded once, not normalized
// (docs/architecture/08-security-and-identity.md "Upstream
// authentication").
const serviceS3 = "s3"

// Signing failures; their messages are fixed and carry no request data.
var (
	errStreamedBody = errors.New("the request body streams, so its SHA-256 cannot be signed")
	errNoURL        = errors.New("the outgoing request has no URL")
	errBadEscape    = errors.New("the request path holds an invalid percent escape")
)

// ownedFields returns the header fields the signature owns.
func ownedFields() []string {
	return []string{headerAuthorization, headerDate, headerSecurityToken, headerContentSHA256}
}

// params are the signing parameters of one Policy config.
type params struct {
	// region and service are config.region and config.service, the
	// credential scope.
	region, service string
	// unsigned is config.payload unsigned.
	unsigned bool
}

// paramsOf returns the parameters of cfg: config.payload absent is signed,
// its schema default.
func paramsOf(cfg *v1alpha1.AuthUpstreamSigV4Config) params {
	return params{
		region:   cfg.Region,
		service:  cfg.Service,
		unsigned: cfg.Payload != nil && *cfg.Payload == v1alpha1.SigV4PayloadUnsigned,
	}
}

// s3 reports whether the service is Amazon S3, whose canonical path is the
// request's path as sent.
func (p params) s3() bool { return p.service == serviceS3 }

// sign signs r in place at t with c, as the transport-time signing hook
// does: it merges the case variants of every header field under the
// canonical name, drops the fields the signature owns, sends the payload
// hash in X-Amz-Content-Sha256, gives the path the form AWS signs
// (escapePath), and signs the request as it will be sent
// (signCanonical).
func (p params) sign(ctx context.Context, s *v4.Signer, r *http.Request, c aws.Credentials, t time.Time) error {
	if r.URL == nil {
		return errNoURL
	}
	hash, err := p.payloadHash(r)
	if err != nil {
		return err
	}
	path, err := escapePath(routematch.RequestPath(r.URL))
	if err != nil {
		return err
	}
	if r.Header == nil {
		r.Header = http.Header{}
	}
	mergeCaseVariants(r.Header)
	for _, k := range ownedFields() {
		delete(r.Header, k)
	}
	r.Header[headerContentSHA256] = []string{hash}
	routematch.SetRequestPath(r.URL, path)
	return p.signCanonical(ctx, s, r, c, hash, t)
}

// escapePath returns escaped, a request path in escaped form, as AWS SDKs
// send a path and AWS URI-encodes one: each segment between raw slashes
// decoded, then every byte of it but the unreserved characters (A-Z, a-z,
// 0-9, "-", ".", "_" and "~") percent-encoded in upper-case hex, so an
// escaped slash (%2F) stays one and a service that decodes the path and
// one that takes it as received compute one canonical path. Ruralz's
// normalized path differs from this form only in the sub-delims, ":" and
// "@" it keeps unescaped. An empty path is "/"; an invalid escape, which
// request hardening leaves no path with, is an error.
func escapePath(escaped string) (string, error) {
	if escaped == "" {
		return "/", nil
	}
	segs := strings.Split(escaped, "/")
	var b strings.Builder
	b.Grow(len(escaped) + len(escaped)/2)
	for i, seg := range segs {
		if i > 0 {
			b.WriteByte('/')
		}
		raw, err := url.PathUnescape(seg)
		if err != nil {
			return "", errBadEscape
		}
		for j := range len(raw) {
			c := raw[j]
			if unreserved(c) {
				b.WriteByte(c)
				continue
			}
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
		}
	}
	return b.String(), nil
}

// upperHex is the digit set of escapePath.
const upperHex = "0123456789ABCDEF"

// unreserved reports an RFC 3986 unreserved character, which AWS URI
// encoding leaves as is.
func unreserved(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

// payloadHash returns the payload hash r is signed with: UNSIGNED-PAYLOAD
// for an unsigned payload, else the hex SHA-256 of the body the attempt
// sends, read from a fresh reader of GetBody so the body itself is left
// for the transport. A body without GetBody streams and cannot be hashed
// before it is sent.
func (p params) payloadHash(r *http.Request) (string, error) {
	if p.unsigned {
		return PayloadUnsigned, nil
	}
	if r.Body == nil || r.Body == http.NoBody {
		return emptyPayloadHash, nil
	}
	if r.GetBody == nil {
		return "", errStreamedBody
	}
	rc, err := r.GetBody()
	if err != nil {
		return "", fmt.Errorf("open the request body to hash it: %w", err)
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", fmt.Errorf("hash the request body: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// signCanonical signs r with the payload hash hash, without touching any
// header field the signer does not own: the signature covers every field
// r carries, its host, its query and its canonical path (canonicalPath).
// The signer works on a copy of r whose URL carries the canonical path, as
// aws/signer/v4 reads a pre-escaped path from URL.Opaque, so r keeps its
// own path; the copy shares r's header map, which receives X-Amz-Date,
// X-Amz-Security-Token for a session token, and Authorization. r then
// takes the host the signature covers (the authority without a default
// port) and the query in the canonical form it was signed in, so the
// transport sends what was signed.
func (p params) signCanonical(ctx context.Context, s *v4.Signer, r *http.Request, c aws.Credentials, hash string, t time.Time) error {
	if r.URL == nil {
		return errNoURL
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	u := *r.URL
	u.Opaque = "//" + host + p.canonicalPath(routematch.RequestPath(r.URL))
	u.RawPath = ""
	cp := *r
	cp.URL = &u
	s3 := p.s3()
	err := s.SignHTTP(ctx, c, &cp, hash, p.service, p.region, t, func(o *v4.SignerOptions) {
		o.DisableURIPathEscaping = s3
	})
	if err != nil {
		return fmt.Errorf("sign the request: %w", err)
	}
	r.Host = cp.Host
	r.URL.RawQuery = u.RawQuery
	return nil
}

// canonicalPath returns the path the service computes the signature over,
// before the signer's encoding: for Amazon S3 the escaped path as sent,
// which the signer then takes as is (S3 URI-encodes a path once); for
// every other service the escaped path with its dot segments resolved and
// its empty segments dropped (normalizePath), which the signer encodes
// once more (every other service URI-encodes it twice).
func (p params) canonicalPath(escaped string) string {
	if p.s3() {
		if escaped == "" {
			return "/"
		}
		return escaped
	}
	return normalizePath(escaped)
}

// normalizePath resolves the "." and ".." segments of p, an escaped path,
// as RFC 3986 section 5.2.4 does, and drops its empty segments, keeping a
// trailing slash, which a final dot segment also leaves: "//a//" is "/a/",
// "/a/b/.." is "/a/", and a path left without a segment is "/". An escaped
// slash (%2F) separates no segment.
func normalizePath(p string) string {
	segs := strings.Split(p, "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		switch s {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return "/"
	}
	n := "/" + strings.Join(out, "/")
	switch segs[len(segs)-1] {
	case "", ".", "..":
		n += "/"
	default:
	}
	return n
}

// mergeCaseVariants moves the lines h holds for one field under a key
// that is not the field's canonical name to the canonical key, after the
// canonical key's own lines and in key order, so the field the transport
// writes and the field the signature covers are one, in one order.
func mergeCaseVariants(h http.Header) {
	var odd []string
	for k := range h {
		if http.CanonicalHeaderKey(k) != k {
			odd = append(odd, k)
		}
	}
	if len(odd) == 0 {
		return
	}
	slices.Sort(odd)
	for _, k := range odd {
		ck := http.CanonicalHeaderKey(k)
		vs := h[ck]
		h[ck] = append(vs[:len(vs):len(vs)], h[k]...)
		delete(h, k)
	}
}
