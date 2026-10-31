// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwks

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/lestrrat-go/jwx/v4/jwk"

	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// Key limits of a JWKS document (Security and identity, "JWT and OIDC":
// "JWKS public keys only: RSA of 2,048 bits or more, P-256 or Ed25519; at
// most 256 keys and 1 MiB per document (target)").
const (
	// MaxKeys bounds the entries of a document's keys array.
	MaxKeys = 256
	// MaxDocumentBytes bounds a document.
	MaxDocumentBytes = 1 << 20
	// MinRSABits is the smallest RSA modulus kept.
	MinRSABits = 2048
)

// Family is the key family a JWS algorithm verifies with.
type Family uint8

// Families.
const (
	// FamilyNone is no family: an algorithm auth.jwt does not accept.
	FamilyNone Family = iota
	// FamilyRSA holds RSA keys (RS256, PS256).
	FamilyRSA
	// FamilyP256 holds ECDSA P-256 keys (ES256).
	FamilyP256
	// FamilyEd25519 holds Ed25519 keys (EdDSA).
	FamilyEd25519
	numFamilies
)

// String returns the family's name.
func (f Family) String() string {
	switch f {
	case FamilyRSA:
		return "RSA"
	case FamilyP256:
		return "P-256"
	case FamilyEd25519:
		return "Ed25519"
	default:
		return "none"
	}
}

// Key is one usable public key of an issuer.
type Key struct {
	// ID is the JWK kid; "" when absent.
	ID string
	// Alg is the JWK alg; "" when absent. A token naming another
	// algorithm is never verified with the key.
	Alg string
	// Family is the key's family.
	Family Family
	// Public is *rsa.PublicKey, *ecdsa.PublicKey on P-256 or
	// ed25519.PublicKey.
	Public crypto.PublicKey
}

// kidFamily indexes keys by kid and family.
type kidFamily struct {
	kid string
	f   Family
}

// Keys is the usable key set of one issuer from one fetch. It is immutable
// and safe for concurrent use.
type Keys struct {
	fetched, expires time.Time
	byKID            map[kidFamily]*Key
	// byFamily lists each family's keys in document order, for tokens
	// without a kid.
	byFamily [numFamilies][]*Key
	n        int
	extra    int // keys the document held that are not usable
}

// Errors of NewKeys; each failed document is a failed fetch.
var (
	// ErrDocument reports a document that is not a JWK Set: not one JSON
	// object with a "keys" array, or JSON that is not strict (invalid
	// UTF-8, a duplicate member name, nesting deeper than the scanner's
	// limit).
	ErrDocument = errors.New("jwks: not a JWK Set")
	// ErrTooLarge reports a document over MaxDocumentBytes or MaxKeys.
	ErrTooLarge = errors.New("jwks: document over its limit")
	// ErrNoUsableKeys reports a document without a usable key.
	ErrNoUsableKeys = errors.New("jwks: no usable keys")
)

// NewKeys parses the JWK Set doc of issuer and keeps its usable keys,
// stamped as fetched at fetched and expiring lifetime later. revoker, when
// not nil, drops every key it lists by (issuer, kid). The first of two
// usable keys sharing a kid and family wins; the other counts as
// unusable. Keys without a kid are all kept.
func NewKeys(doc []byte, issuer string, revoker identity.Revoker, fetched time.Time, lifetime time.Duration) (*Keys, error) {
	if len(doc) > MaxDocumentBytes {
		return nil, fmt.Errorf("%w: %d bytes above %d", ErrTooLarge, len(doc), MaxDocumentBytes)
	}
	elems, err := keyElements(doc)
	if err != nil {
		return nil, err
	}
	ks := &Keys{fetched: fetched, expires: fetched.Add(lifetime), byKID: make(map[kidFamily]*Key, len(elems))}
	for _, raw := range elems {
		k, ok := usableKey(raw)
		if !ok || (revoker != nil && k.ID != "" && revoker.RevokedKey(issuer, k.ID)) {
			ks.extra++
			continue
		}
		if k.ID != "" {
			at := kidFamily{k.ID, k.Family}
			if _, dup := ks.byKID[at]; dup {
				ks.extra++
				continue
			}
			ks.byKID[at] = k
		}
		ks.byFamily[k.Family] = append(ks.byFamily[k.Family], k)
		ks.n++
	}
	if ks.n == 0 {
		return nil, fmt.Errorf("%w: %d keys, none usable", ErrNoUsableKeys, len(elems))
	}
	return ks, nil
}

// Fetched returns when the set was fetched.
func (ks *Keys) Fetched() time.Time { return ks.fetched }

// Expires returns when the set stops serving: from then on the issuer has
// no usable keys until a fetch succeeds.
func (ks *Keys) Expires() time.Time { return ks.expires }

// Expired reports whether the set has expired at now.
func (ks *Keys) Expired(now time.Time) bool { return !now.Before(ks.expires) }

// Len returns the number of usable keys.
func (ks *Keys) Len() int { return ks.n }

// Unusable returns the number of document entries not kept.
func (ks *Keys) Unusable() int { return ks.extra }

// Lookup returns the key of family f with kid; an empty kid names no key
// (a token without a kid tries OfFamily).
func (ks *Keys) Lookup(kid string, f Family) (*Key, bool) {
	if kid == "" || f == FamilyNone || f >= numFamilies {
		return nil, false
	}
	k, ok := ks.byKID[kidFamily{kid, f}]
	return k, ok
}

// OfFamily returns the set's keys of family f in document order, the keys
// a token without a kid tries: never more than MaxKeys. The slice is
// shared; read it only.
func (ks *Keys) OfFamily(f Family) []*Key {
	if f == FamilyNone || f >= numFamilies {
		return nil
	}
	return ks.byFamily[f]
}

// keyElements returns the raw JSON text of each object of the top-level
// "keys" array of a JWK Set; an entry that is not an object is returned as
// nil (an unusable key). The scan is strict (jsonval): one JSON object,
// valid UTF-8, no duplicate member names at any depth.
func keyElements(doc []byte) ([][]byte, error) {
	var s jsonval.Scanner
	s.Reset(doc, jsonval.ScanOptions{})
	t, err := s.Next()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDocument, err)
	}
	if t.Kind != jsonval.KindObjectStart {
		return nil, fmt.Errorf("%w: the document is not an object", ErrDocument)
	}
	var elems [][]byte
	found := false
	for {
		t, err = s.Next()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrDocument, err)
		}
		if t.Kind == jsonval.KindObjectEnd {
			break
		}
		name := s.Text(t)
		if t, err = s.Next(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrDocument, err)
		}
		if name != "keys" {
			if err := skip(&s, t); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrDocument, err)
			}
			continue
		}
		found = true
		if t.Kind != jsonval.KindArrayStart {
			return nil, fmt.Errorf("%w: keys is not an array", ErrDocument)
		}
		if elems, err = arrayElements(&s, doc); err != nil {
			return nil, err
		}
	}
	if _, err = s.Next(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %w", ErrDocument, err)
	}
	if !found {
		return nil, fmt.Errorf("%w: no keys member", ErrDocument)
	}
	return elems, nil
}

// arrayElements reads the elements of the array whose start the scanner
// just returned, up to MaxKeys.
func arrayElements(s *jsonval.Scanner, doc []byte) ([][]byte, error) {
	var elems [][]byte
	for {
		t, err := s.Next()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrDocument, err)
		}
		if t.Kind == jsonval.KindArrayEnd {
			return elems, nil
		}
		if len(elems) == MaxKeys {
			return nil, fmt.Errorf("%w: more than %d keys", ErrTooLarge, MaxKeys)
		}
		start := t.Offset
		if err := skip(s, t); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrDocument, err)
		}
		if t.Kind == jsonval.KindObjectStart {
			elems = append(elems, doc[start:s.Offset()])
		} else {
			elems = append(elems, nil)
		}
	}
}

// skip consumes the value that starts with t, the token the scanner just
// returned.
func skip(s *jsonval.Scanner, t jsonval.Token) error {
	if t.Kind != jsonval.KindObjectStart && t.Kind != jsonval.KindArrayStart {
		return nil
	}
	depth := s.Depth()
	for s.Depth() >= depth {
		if _, err := s.Next(); err != nil {
			return err
		}
	}
	return nil
}

// usableKey parses one JWK and returns it when auth.jwt may verify with it:
// a public key of a known family and size, whose use (when present) is
// "sig" and whose key_ops (when present) include "verify".
func usableKey(raw []byte) (*Key, bool) {
	if raw == nil {
		return nil, false
	}
	k, err := jwk.ParseKey(raw)
	if err != nil {
		return nil, false
	}
	if use, ok := k.KeyUsage(); ok && use != string(jwk.ForSignature) {
		return nil, false
	}
	if ops, ok := k.KeyOps(); ok && !slices.Contains(ops, jwk.KeyOpVerify) {
		return nil, false
	}
	pub, err := jwk.Export[any](k)
	if err != nil {
		return nil, false
	}
	out := &Key{Public: pub}
	switch p := pub.(type) {
	case *rsa.PublicKey:
		if p.N == nil || p.N.BitLen() < MinRSABits {
			return nil, false
		}
		out.Family = FamilyRSA
	case *ecdsa.PublicKey:
		if p.Curve != elliptic.P256() {
			return nil, false
		}
		out.Family = FamilyP256
	case ed25519.PublicKey:
		if len(p) != ed25519.PublicKeySize {
			return nil, false
		}
		out.Family = FamilyEd25519
	default:
		// Private and symmetric keys, other curves and key types.
		return nil, false
	}
	if kid, ok := k.KeyID(); ok {
		out.ID = kid
	}
	if alg, ok := k.Algorithm(); ok {
		out.Alg = alg.String()
	}
	return out, true
}
