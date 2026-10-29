// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jws"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwt/jwks"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// Token rules M1 fixes instead of adding fields (Security and identity,
// "JWT and OIDC").
const (
	// MaxTokenBytes bounds the token after "Bearer ": 16 KiB (target).
	MaxTokenBytes = 16 << 10
	// ClockSkew is the leeway on exp and nbf: 60 s (target).
	ClockSkew = 60 * time.Second
	// MaxExpiry bounds how far ahead of now exp may lie: 24 hours
	// (target).
	MaxExpiry = 24 * time.Hour
)

// decodedLimit bounds the decoded header and claims of one token by the
// rule for decoded JSON bodies: filter.DecodedLimitFactor times the raw
// limit the bytes arrived under, MaxTokenBytes.
const decodedLimit = filter.DecodedLimitFactor * MaxTokenBytes

// The accepted algorithms: any other alg, HS256 included, is RZ-AUTH-002.
const (
	algRS256 = "RS256"
	algPS256 = "PS256"
	algES256 = "ES256"
	algEdDSA = "EdDSA"
)

// family returns the key family alg verifies with; FamilyNone for an
// algorithm auth.jwt does not accept.
func family(alg string) jwks.Family {
	switch alg {
	case algRS256, algPS256:
		return jwks.FamilyRSA
	case algES256:
		return jwks.FamilyP256
	case algEdDSA:
		return jwks.FamilyEd25519
	default:
		return jwks.FamilyNone
	}
}

// Token failures; each is RZ-AUTH-002 and none is logged or echoed.
var (
	errForm      = errors.New("jwt: not a JWS compact token")
	errHeader    = errors.New("jwt: invalid JOSE header")
	errAlgorithm = errors.New("jwt: algorithm not accepted")
	errCritical  = errors.New("jwt: crit header present")
	errPayload   = errors.New("jwt: payload is not a JSON object")
)

// token is a parsed JWS compact token whose signature is not yet checked.
type token struct {
	alg    string
	kid    string
	family jwks.Family
	claims claims
}

// scratch holds one request's token bytes: the raw token, whose header
// and payload segments are the JWS signing input, and the decoded header,
// payload and signature.
type scratch struct {
	raw, header, payload, sig []byte
}

// input returns the JWS signing input: the token up to its second dot.
func (s *scratch) input(t *parsed) []byte { return s.raw[:t.inputLen] }

// parsed is the outcome of parse.
type parsed struct {
	token
	inputLen int
}

// codec parses tokens and verifies signatures for every Filter of one
// Factory. It is safe for concurrent use.
type codec struct {
	enc       *base64.Encoding
	decoders  sync.Pool // *jsonval.Decoder
	scratches sync.Pool // *scratch
	rs256     jws.Verifier
	ps256     jws.Verifier
	es256     jws.Verifier
	eddsa     jws.Verifier
}

// newCodec resolves the verifier of each accepted algorithm.
func newCodec() (*codec, error) {
	c := &codec{enc: base64.RawURLEncoding.Strict()}
	for _, v := range []struct {
		dst *jws.Verifier
		alg jwa.SignatureAlgorithm
	}{
		{&c.rs256, jwa.RS256()}, {&c.ps256, jwa.PS256()}, {&c.es256, jwa.ES256()}, {&c.eddsa, jwa.EdDSA()},
	} {
		vf, err := jws.VerifierFor(v.alg)
		if err != nil {
			return nil, fmt.Errorf("jwt: verifier for %s: %w", v.alg, err)
		}
		*v.dst = vf
	}
	return c, nil
}

// verifier returns the verifier of an accepted algorithm.
func (c *codec) verifier(alg string) jws.Verifier {
	switch alg {
	case algRS256:
		return c.rs256
	case algPS256:
		return c.ps256
	case algES256:
		return c.es256
	default:
		return c.eddsa
	}
}

// getScratch returns a pooled scratch.
func (c *codec) getScratch() *scratch {
	if s, ok := c.scratches.Get().(*scratch); ok {
		return s
	}
	return &scratch{}
}

// putScratch returns s to the pool; its buffers never exceed
// MaxTokenBytes.
func (c *codec) putScratch(s *scratch) { c.scratches.Put(s) }

// decode decodes one strict JSON text: valid UTF-8, no duplicate member
// names, objects as map[string]any, within decodedLimit.
func (c *codec) decode(b []byte) (any, error) {
	d, ok := c.decoders.Get().(*jsonval.Decoder)
	if !ok {
		d = jsonval.NewDecoder(jsonval.Options{MaxCost: decodedLimit, MapObjects: true})
	}
	v, _, err := d.Decode(b)
	c.decoders.Put(d)
	return v, err
}

// parse reads a JWS compact token: three base64url segments (RFC 7515
// section 7.1, no padding, canonical), a JOSE header naming an accepted
// alg without crit (jku, x5u, jwk and x5c are ignored), and a payload that
// is a JSON object. The bytes land in s.
func (c *codec) parse(raw string, s *scratch) (parsed, error) {
	if raw == "" || len(raw) > MaxTokenBytes {
		return parsed{}, errForm
	}
	h, p, sig, ok := segments(raw)
	if !ok {
		return parsed{}, errForm
	}
	s.raw = append(s.raw[:0], raw...)
	var err error
	if s.header, err = c.enc.AppendDecode(s.header[:0], s.raw[:h]); err != nil {
		return parsed{}, errForm
	}
	if s.payload, err = c.enc.AppendDecode(s.payload[:0], s.raw[h+1:p]); err != nil {
		return parsed{}, errForm
	}
	if s.sig, err = c.enc.AppendDecode(s.sig[:0], s.raw[p+1:sig]); err != nil || len(s.sig) == 0 {
		return parsed{}, errForm
	}
	t := parsed{inputLen: p}
	if err := c.header(s.header, &t.token); err != nil {
		return parsed{}, err
	}
	v, err := c.decode(s.payload)
	if err != nil {
		return parsed{}, errPayload
	}
	m, ok := v.(map[string]any)
	if !ok {
		return parsed{}, errPayload
	}
	t.claims = claims(m)
	return t, nil
}

// header reads the JOSE header into t: alg must be accepted; crit makes
// the token invalid (no extension is understood), as does a b64 other than
// true (RFC 7797 requires crit with it); kid, when present, is a string.
func (c *codec) header(b []byte, t *token) error {
	v, err := c.decode(b)
	if err != nil {
		return errHeader
	}
	h, ok := v.(map[string]any)
	if !ok {
		return errHeader
	}
	alg, _ := h["alg"].(string)
	if t.family = family(alg); t.family == jwks.FamilyNone {
		return errAlgorithm
	}
	t.alg = alg
	if _, ok := h["crit"]; ok {
		return errCritical
	}
	if b64, ok := h["b64"]; ok && b64 != true {
		return errHeader
	}
	if kid, ok := h["kid"]; ok {
		if t.kid, ok = kid.(string); !ok {
			return errHeader
		}
	}
	return nil
}

// segments returns the end offsets of the header, payload and signature
// segments of a compact token: three non-empty runs of the base64url
// alphabet separated by two dots.
func segments(raw string) (h, p, sig int, ok bool) {
	h = strings.IndexByte(raw, '.')
	if h <= 0 {
		return 0, 0, 0, false
	}
	p = strings.IndexByte(raw[h+1:], '.')
	if p <= 0 {
		return 0, 0, 0, false
	}
	p += h + 1
	if p+1 >= len(raw) {
		return 0, 0, 0, false
	}
	for i := range len(raw) {
		if i != h && i != p && !base64URL(raw[i]) {
			return 0, 0, 0, false
		}
	}
	return h, p, len(raw), true
}

// base64URL reports a byte of the base64url alphabet (RFC 4648 section 5).
func base64URL(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_'
}

// bearerToken returns the token of an Authorization value with the Bearer
// scheme (RFC 6750 section 2.1; the scheme name is case-insensitive, RFC
// 9110 section 11.1), without surrounding whitespace; bearer is false for
// any other scheme, which is no bearer credential at all.
func bearerToken(v string) (tok string, bearer bool) {
	scheme, rest, _ := strings.Cut(strings.Trim(v, " \t"), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimLeft(rest, " "), true
}
