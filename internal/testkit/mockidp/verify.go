// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockidp

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// PublicKey is one verification key of a parsed JWK Set.
type PublicKey struct {
	// ID is the kid, "" when absent.
	ID string
	// Alg is the JWK alg member, "" when absent.
	Alg Alg
	// Key is *rsa.PublicKey, *ecdsa.PublicKey or ed25519.PublicKey.
	Key crypto.PublicKey
}

// ParseJWKS parses a JWK Set with the standard library into its RSA, EC
// (P-256, P-384, P-521) and OKP Ed25519 public keys, in document order;
// other key types are skipped and private members ignored. A malformed
// member of a supported key is an error.
func ParseJWKS(doc []byte) ([]PublicKey, error) {
	var set struct {
		Keys []JWK `json:"keys"`
	}
	if err := json.Unmarshal(doc, &set); err != nil {
		return nil, fmt.Errorf("mockidp: parse JWKS: %w", err)
	}
	var out []PublicKey
	for i, j := range set.Keys {
		pub, err := parseJWK(j)
		if err != nil {
			return nil, fmt.Errorf("mockidp: JWKS key %d: %w", i, err)
		}
		if pub != nil {
			out = append(out, PublicKey{ID: j.Kid, Alg: Alg(j.Alg), Key: pub})
		}
	}
	return out, nil
}

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.Strict().DecodeString(s) }

// parseJWK returns the public key of j, nil for an unsupported type.
func parseJWK(j JWK) (crypto.PublicKey, error) {
	switch j.Kty {
	case "RSA":
		n, err := unb64(j.N)
		if err != nil || len(n) == 0 {
			return nil, fmt.Errorf("bad n: %w", errOr(err))
		}
		e, err := unb64(j.E)
		if err != nil || len(e) == 0 || len(e) > 3 {
			return nil, fmt.Errorf("bad e: %w", errOr(err))
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}, nil
	case "EC":
		var curve elliptic.Curve
		switch j.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, nil
		}
		x, errX := unb64(j.X)
		y, errY := unb64(j.Y)
		size := coordSize(curve)
		if errX != nil || errY != nil || len(x) != size || len(y) != size {
			return nil, errors.New("bad EC point encoding")
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, append(append([]byte{4}, x...), y...))
		if err != nil {
			return nil, fmt.Errorf("bad EC point: %w", err)
		}
		return pub, nil
	case "OKP":
		if j.Crv != "Ed25519" {
			return nil, nil
		}
		x, err := unb64(j.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("bad Ed25519 x: %w", errOr(err))
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, nil
}

func errOr(err error) error {
	if err != nil {
		return err
	}
	return errors.New("empty or oversized")
}

// Verify checks a JWS compact token against keys with the standard
// library and returns its header and claims (numbers as json.Number). The
// header alg must be RS256, PS256, ES256 or EdDSA; a kid selects the keys
// with that ID, else every key is tried; a key whose alg member differs
// from the header's is skipped. Claims are not validated (exp, aud and
// the rest are the gateway's to check).
func Verify(token string, keys []PublicKey) (header, claims map[string]any, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, errors.New("mockidp: token is not a JWS compact serialization")
	}
	hb, err := unb64(parts[0])
	if err != nil {
		return nil, nil, fmt.Errorf("mockidp: header encoding: %w", err)
	}
	if header, err = decodeObject(hb); err != nil {
		return nil, nil, fmt.Errorf("mockidp: header: %w", err)
	}
	alg, _ := header["alg"].(string)
	switch Alg(alg) {
	case RS256, PS256, ES256, EdDSA:
	default:
		return nil, nil, fmt.Errorf("mockidp: algorithm %q not accepted", alg)
	}
	sig, err := unb64(parts[2])
	if err != nil {
		return nil, nil, fmt.Errorf("mockidp: signature encoding: %w", err)
	}
	kid, hasKid := header["kid"].(string)
	input := []byte(parts[0] + "." + parts[1])
	verified := false
	for _, k := range keys {
		if (hasKid && k.ID != kid) || (k.Alg != "" && k.Alg != Alg(alg)) {
			continue
		}
		if verifySignature(Alg(alg), k.Key, input, sig) {
			verified = true
			break
		}
	}
	if !verified {
		return nil, nil, errors.New("mockidp: no key verifies the signature")
	}
	pb, err := unb64(parts[1])
	if err != nil {
		return nil, nil, fmt.Errorf("mockidp: payload encoding: %w", err)
	}
	if claims, err = decodeObject(pb); err != nil {
		return nil, nil, fmt.Errorf("mockidp: claims: %w", err)
	}
	return header, claims, nil
}

func decodeObject(b []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("not a JSON object")
	}
	if d.More() {
		return nil, errors.New("trailing data")
	}
	return m, nil
}

func verifySignature(alg Alg, pub crypto.PublicKey, input, sig []byte) bool {
	digest := sha256.Sum256(input)
	switch alg {
	case RS256:
		p, ok := pub.(*rsa.PublicKey)
		return ok && rsa.VerifyPKCS1v15(p, crypto.SHA256, digest[:], sig) == nil
	case PS256:
		p, ok := pub.(*rsa.PublicKey)
		return ok && rsa.VerifyPSS(p, crypto.SHA256, digest[:], sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
	case ES256:
		p, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return false
		}
		n := coordSize(p.Curve)
		if len(sig) != 2*n {
			return false
		}
		r := new(big.Int).SetBytes(sig[:n])
		s := new(big.Int).SetBytes(sig[n:])
		return ecdsa.Verify(p, digest[:], r, s)
	case EdDSA:
		p, ok := pub.(ed25519.PublicKey)
		return ok && ed25519.Verify(p, input, sig)
	}
	return false
}
