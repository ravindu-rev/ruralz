// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Signing failures; each is the internal error.type and names no data.
var (
	errSignature = errors.New("the signature could not be computed")
	errKeyFamily = errors.New("the signing key does not fit the algorithm")
)

// enc is the base64url encoding of the three JWS parts: unpadded (RFC
// 7515 section 2).
func enc() *base64.Encoding { return base64.RawURLEncoding }

// header returns the first part of every token a Policy signs with alg and
// kid, followed by its dot: the base64url JOSE header {"alg":alg}, with
// "kid":kid when kid is not empty, written compactly in that member order.
func header(alg v1alpha1.JWTSignAlgorithm, kid string) ([]byte, error) {
	if familyOf(alg) == 0 {
		return nil, fmt.Errorf("config.algorithm %q is not RS256, PS256, ES256 or EdDSA", alg)
	}
	h := []byte(`{"alg":`)
	h, _ = jsonval.AppendString(h, string(alg))
	if kid != "" {
		var err error
		h = append(h, `,"kid":`...)
		if h, err = jsonval.AppendString(h, kid); err != nil {
			return nil, errors.New("config.keyId is not valid UTF-8")
		}
	}
	h = append(h, '}')
	out := enc().AppendEncode(nil, h)
	return append(out, '.'), nil
}

// signer computes the JWS signatures of one algorithm (RFC 7518 sections
// 3.3 to 3.5, RFC 8037 section 3.1).
type signer struct {
	alg v1alpha1.JWTSignAlgorithm
	// head is the encoded header and its dot (header).
	head []byte
}

// appendToken appends to dst the compact JWS of payload signed with k:
// the header, the base64url payload, a dot and the base64url signature
// over the first two parts (RFC 7515 section 7.1). On failure dst keeps
// its length.
func (s *signer) appendToken(dst []byte, k *key, payload []byte) ([]byte, error) {
	start := len(dst)
	dst = append(dst, s.head...)
	dst = enc().AppendEncode(dst, payload)
	sig, err := s.sign(k, dst[start:])
	if err != nil {
		return dst[:start], err
	}
	dst = append(dst, '.')
	return enc().AppendEncode(dst, sig), nil
}

// sign returns the JWS signature of input under s.alg with k.
func (s *signer) sign(k *key, input []byte) ([]byte, error) {
	if k == nil || k.family != familyOf(s.alg) {
		return nil, errKeyFamily
	}
	if s.alg == v1alpha1.JWTSignAlgorithmEdDSA {
		priv, ok := k.signer.(ed25519.PrivateKey)
		if !ok {
			return nil, errKeyFamily
		}
		return ed25519.Sign(priv, input), nil
	}
	digest := sha256.Sum256(input)
	switch s.alg {
	case v1alpha1.JWTSignAlgorithmRS256, v1alpha1.JWTSignAlgorithmPS256:
		priv, ok := k.signer.(*rsa.PrivateKey)
		if !ok {
			return nil, errKeyFamily
		}
		var (
			sig []byte
			err error
		)
		if s.alg == v1alpha1.JWTSignAlgorithmRS256 {
			sig, err = rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
		} else {
			// The salt is as long as the hash (RFC 7518 section 3.5).
			sig, err = rsa.SignPSS(rand.Reader, priv, crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256})
		}
		if err != nil {
			return nil, errSignature
		}
		return sig, nil
	default:
		priv, ok := k.signer.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errKeyFamily
		}
		der, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
		if err != nil {
			return nil, errSignature
		}
		return rawECDSA(der)
	}
}

// ecdsaSize is the size of R and of S in an ES256 signature.
const ecdsaSize = 32

// rawECDSA converts an ASN.1 ECDSA signature to the JWS form: R and S as
// 32-byte big-endian integers, concatenated (RFC 7518 section 3.4).
func rawECDSA(der []byte) ([]byte, error) {
	var rs struct{ R, S *big.Int }
	rest, err := asn1.Unmarshal(der, &rs)
	if err != nil || len(rest) > 0 || rs.R == nil || rs.S == nil ||
		rs.R.Sign() <= 0 || rs.S.Sign() <= 0 || rs.R.BitLen() > 8*ecdsaSize || rs.S.BitLen() > 8*ecdsaSize {
		return nil, errSignature
	}
	out := make([]byte, 2*ecdsaSize)
	rs.R.FillBytes(out[:ecdsaSize])
	rs.S.FillBytes(out[ecdsaSize:])
	return out, nil
}
