// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockidp

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// Alg is a JWS signature algorithm (RFC 7518); auth.jwt accepts exactly
// these four (06 req 26).
type Alg string

// Algorithms.
const (
	// RS256 is RSASSA-PKCS1-v1_5 with SHA-256.
	RS256 Alg = "RS256"
	// PS256 is RSASSA-PSS with SHA-256 and MGF1 with SHA-256.
	PS256 Alg = "PS256"
	// ES256 is ECDSA with P-256 and SHA-256.
	ES256 Alg = "ES256"
	// EdDSA is Ed25519 (RFC 8037).
	EdDSA Alg = "EdDSA"
)

// RSABits is the modulus size GenerateKey uses for RS256 and PS256 (the
// auth.jwt minimum, 06 req 38).
const RSABits = 2048

// Key is a private signing key with its key ID and algorithm.
type Key struct {
	// ID is the JWK kid; GenerateKey sets the RFC 7638 thumbprint.
	ID string
	// Alg is the algorithm the key signs with.
	Alg    Alg
	signer crypto.Signer
}

// GenerateKey creates a key for alg with the standard library: RSA 2,048
// bits for RS256 and PS256, ECDSA P-256 for ES256, Ed25519 for EdDSA. Its
// ID is the RFC 7638 thumbprint.
func GenerateKey(alg Alg) (*Key, error) {
	var (
		signer crypto.Signer
		err    error
	)
	switch alg {
	case RS256, PS256:
		signer, err = rsa.GenerateKey(rand.Reader, RSABits)
	case ES256:
		signer, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case EdDSA:
		_, signer, err = ed25519.GenerateKey(rand.Reader)
	default:
		return nil, fmt.Errorf("mockidp: unsupported algorithm %q", alg)
	}
	if err != nil {
		return nil, fmt.Errorf("mockidp: generate %s key: %w", alg, err)
	}
	return NewKey(alg, "", signer)
}

// NewKey wraps an existing private key, for fixtures GenerateKey does not
// make (RSA 1,024, P-384 under ES256, a fixed kid). The key's family must
// fit alg: *rsa.PrivateKey for RS256 and PS256, *ecdsa.PrivateKey for
// ES256 (any curve), ed25519.PrivateKey for EdDSA. An empty id is replaced
// by the RFC 7638 thumbprint.
func NewKey(alg Alg, id string, signer crypto.Signer) (*Key, error) {
	ok := false
	switch alg {
	case RS256, PS256:
		_, ok = signer.(*rsa.PrivateKey)
	case ES256:
		_, ok = signer.(*ecdsa.PrivateKey)
	case EdDSA:
		_, ok = signer.(ed25519.PrivateKey)
	default:
		return nil, fmt.Errorf("mockidp: unsupported algorithm %q", alg)
	}
	if !ok {
		return nil, fmt.Errorf("mockidp: %T cannot sign %s", signer, alg)
	}
	k := &Key{ID: id, Alg: alg, signer: signer}
	if k.ID == "" {
		k.ID = k.Thumbprint()
	}
	return k, nil
}

// Public returns the public key.
func (k *Key) Public() crypto.PublicKey { return k.signer.Public() }

// Signer returns the private key.
func (k *Key) Signer() crypto.Signer { return k.signer }

// JWK is a public JSON Web Key (RFC 7517) as the JWKS document serves it.
type JWK struct {
	Kty    string   `json:"kty"`
	Kid    string   `json:"kid,omitempty"`
	Use    string   `json:"use,omitempty"`
	Alg    string   `json:"alg,omitempty"`
	KeyOps []string `json:"key_ops,omitempty"`
	// N and E are the RSA modulus and exponent.
	N string `json:"n,omitempty"`
	E string `json:"e,omitempty"`
	// Crv, X and Y are the curve and point of an EC or OKP key.
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// coordSize is the byte length of a curve coordinate and of each half of
// a JWS ECDSA signature.
func coordSize(c elliptic.Curve) int { return (c.Params().BitSize + 7) / 8 }

// JWK returns the public JWK with kid, "use": "sig" and alg.
func (k *Key) JWK() JWK {
	j := publicJWK(k.Public())
	j.Kid, j.Use, j.Alg = k.ID, "sig", string(k.Alg)
	return j
}

// publicJWK encodes the key material members of a public key.
func publicJWK(pub crypto.PublicKey) JWK {
	switch p := pub.(type) {
	case *rsa.PublicKey:
		return JWK{Kty: "RSA", N: b64(p.N.Bytes()), E: b64(big.NewInt(int64(p.E)).Bytes())}
	case *ecdsa.PublicKey:
		raw, err := p.Bytes()
		if err != nil {
			return JWK{Kty: "EC"}
		}
		n := coordSize(p.Curve)
		return JWK{Kty: "EC", Crv: p.Curve.Params().Name, X: b64(raw[1 : 1+n]), Y: b64(raw[1+n:])}
	case ed25519.PublicKey:
		return JWK{Kty: "OKP", Crv: "Ed25519", X: b64(p)}
	}
	return JWK{}
}

// Thumbprint returns the RFC 7638 JWK thumbprint (SHA-256, base64url).
func (k *Key) Thumbprint() string { return thumbprint(publicJWK(k.Public())) }

// thumbprint hashes the required members of j in lexicographic order.
func thumbprint(j JWK) string {
	var members string
	switch j.Kty {
	case "RSA":
		members = fmt.Sprintf(`{"e":%q,"kty":"RSA","n":%q}`, j.E, j.N)
	case "EC":
		members = fmt.Sprintf(`{"crv":%q,"kty":"EC","x":%q,"y":%q}`, j.Crv, j.X, j.Y)
	default:
		members = fmt.Sprintf(`{"crv":%q,"kty":"OKP","x":%q}`, j.Crv, j.X)
	}
	sum := sha256.Sum256([]byte(members))
	return b64(sum[:])
}

// Sign returns a JWS compact serialization of claims. The protected
// header is {"alg": k.Alg, "kid": k.ID, "typ": "JWT"} merged with header,
// where a nil value removes a member (a token without kid) and any other
// value sets it (a crit, jku or mismatching alg member for negative
// tests); the signature always uses k.Alg. A nil claims map encodes as {}.
func (k *Key) Sign(header, claims map[string]any) (string, error) {
	h := map[string]any{"alg": string(k.Alg), "typ": "JWT"}
	if k.ID != "" {
		h["kid"] = k.ID
	}
	for name, v := range header {
		if v == nil {
			delete(h, name)
			continue
		}
		h[name] = v
	}
	if claims == nil {
		claims = map[string]any{}
	}
	hb, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("mockidp: encode header: %w", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("mockidp: encode claims: %w", err)
	}
	return k.SignCompact(hb, cb)
}

// SignCompact signs arbitrary header and payload bytes (base64url-encoded
// here), for malformed-token fixtures such as duplicate members or a
// payload that is not an object.
func (k *Key) SignCompact(header, payload []byte) (string, error) {
	input := b64(header) + "." + b64(payload)
	sig, err := k.signInput([]byte(input))
	if err != nil {
		return "", err
	}
	return input + "." + b64(sig), nil
}

// signInput computes the JWS signature of the signing input.
func (k *Key) signInput(input []byte) ([]byte, error) {
	digest := sha256.Sum256(input)
	switch k.Alg {
	case RS256:
		return k.sign(digest[:], crypto.SHA256)
	case PS256:
		return k.sign(digest[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256})
	case ES256:
		der, err := k.sign(digest[:], crypto.SHA256)
		if err != nil {
			return nil, err
		}
		var rs struct{ R, S *big.Int }
		if _, err := asn1.Unmarshal(der, &rs); err != nil {
			return nil, fmt.Errorf("mockidp: decode ECDSA signature: %w", err)
		}
		pub, ok := k.Public().(*ecdsa.PublicKey)
		if !ok {
			return nil, errors.New("mockidp: ES256 key is not ECDSA")
		}
		n := coordSize(pub.Curve)
		out := make([]byte, 2*n)
		rs.R.FillBytes(out[:n])
		rs.S.FillBytes(out[n:])
		return out, nil
	case EdDSA:
		return k.sign(input, crypto.Hash(0))
	}
	return nil, fmt.Errorf("mockidp: unsupported algorithm %q", k.Alg)
}

func (k *Key) sign(msg []byte, opts crypto.SignerOpts) ([]byte, error) {
	sig, err := k.signer.Sign(rand.Reader, msg, opts)
	if err != nil {
		return nil, fmt.Errorf("mockidp: sign %s: %w", k.Alg, err)
	}
	return sig, nil
}
