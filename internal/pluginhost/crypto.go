// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"

	"github.com/tetratelabs/wazero/api"
)

// Host cryptography (docs/architecture/05-wasm-plugin-system.md "Host
// Function rules", Cryptography, keys and secrets): digests and HMACs over
// guest bytes, and the sign, verify and HMAC operations of host-held keys,
// which the guest names by the handle rz_configure received, so no key
// byte enters guest memory.

// keyOp is an operation a key's declaration allows.
type keyOp uint8

// Key operations of x-ruralz-key.operations.
const (
	opSign keyOp = 1 << iota
	opVerify
	opHMAC
)

// hostKey is one host-held key: its JWA algorithm, its declared
// operations and its material.
type hostKey struct {
	alg    string
	ops    keyOp
	signer crypto.Signer
	public crypto.PublicKey
	secret []byte
}

// errKey reports key material that does not fit its declaration.
var errKey = errors.New("pluginhost: key material does not fit its declared algorithm")

// jwaHash returns the hash of a JWA algorithm; 0 for EdDSA, which hashes
// itself.
func jwaHash(alg string) (crypto.Hash, bool) {
	switch alg {
	case "HS256", "RS256", "PS256", "ES256":
		return crypto.SHA256, true
	case "HS384", "RS384", "PS384", "ES384":
		return crypto.SHA384, true
	case "HS512", "RS512", "PS512", "ES512":
		return crypto.SHA512, true
	case "EdDSA":
		return 0, true
	default:
		return 0, false
	}
}

// keyOps returns the operations an algorithm supports: hmac for HS*, sign
// and verify for the others.
func keyOps(alg string) keyOp {
	if len(alg) > 2 && alg[:2] == "HS" {
		return opHMAC
	}
	return opSign | opVerify
}

// parseKey reads key material for alg and ops: an HMAC secret as a JWK of
// kty oct or as its raw bytes; an asymmetric key as a JWK or PEM (PKCS #8,
// PKCS #1 or SEC 1 private key, or a PKIX public key for verify alone).
func parseKey(alg string, ops keyOp, material []byte) (*hostKey, error) {
	if _, ok := jwaHash(alg); !ok {
		return nil, fmt.Errorf("pluginhost: unknown JWA algorithm %q", alg)
	}
	if ops&^keyOps(alg) != 0 {
		return nil, fmt.Errorf("pluginhost: %s keys do not support the declared operations", alg)
	}
	k := &hostKey{alg: alg, ops: ops}
	if keyOps(alg) == opHMAC {
		k.secret = octSecret(material)
		if len(k.secret) == 0 {
			return nil, errKey
		}
		return k, nil
	}
	priv, pub, err := asymmetric(material)
	if err != nil {
		return nil, err
	}
	if priv != nil {
		k.signer, k.public = priv, priv.Public()
	} else {
		k.public = pub
	}
	if ops&opSign != 0 && k.signer == nil {
		return nil, fmt.Errorf("pluginhost: a %s key that signs needs a private key", alg)
	}
	if !fits(alg, k.public) {
		return nil, errKey
	}
	return k, nil
}

// fits reports whether pub is the key type alg names.
func fits(alg string, pub crypto.PublicKey) bool {
	switch alg[:2] {
	case "RS", "PS":
		k, ok := pub.(*rsa.PublicKey)
		return ok && k.N.BitLen() >= 2048
	case "ES":
		k, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return false
		}
		switch alg {
		case "ES256":
			return k.Curve == elliptic.P256()
		case "ES384":
			return k.Curve == elliptic.P384()
		default:
			return k.Curve == elliptic.P521()
		}
	default:
		_, ok := pub.(ed25519.PublicKey)
		return ok
	}
}

// octSecret returns an HMAC secret: the k of a JWK of kty oct, else the
// bytes as given.
func octSecret(material []byte) []byte {
	var j struct {
		Kty string `json:"kty"`
		K   string `json:"k"`
	}
	if json.Unmarshal(material, &j) == nil && j.Kty == "oct" {
		b, err := base64.RawURLEncoding.DecodeString(j.K)
		if err != nil {
			return nil
		}
		return b
	}
	return append([]byte(nil), material...)
}

// asymmetric reads a JWK or PEM key: a private key, or a public key alone.
func asymmetric(material []byte) (crypto.Signer, crypto.PublicKey, error) {
	if blk, _ := pem.Decode(material); blk != nil {
		if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
			s, ok := k.(crypto.Signer)
			if !ok {
				return nil, nil, errKey
			}
			return s, nil, nil
		}
		if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
			return k, nil, nil
		}
		if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
			return k, nil, nil
		}
		if k, err := x509.ParsePKIXPublicKey(blk.Bytes); err == nil {
			return nil, k, nil
		}
		return nil, nil, errKey
	}
	return jwkKey(material)
}

// jwk is the members of an RSA, EC or OKP JSON Web Key the host reads.
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	D   string `json:"d"`
	P   string `json:"p"`
	Q   string `json:"q"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// jwkKey reads a JSON Web Key (RFC 7517, RFC 8037).
func jwkKey(material []byte) (crypto.Signer, crypto.PublicKey, error) {
	var j jwk
	if err := json.Unmarshal(material, &j); err != nil {
		return nil, nil, errKey
	}
	num := func(s string) (*big.Int, bool) {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil || len(b) == 0 {
			return nil, false
		}
		return new(big.Int).SetBytes(b), true
	}
	switch j.Kty {
	case "RSA":
		n, ok1 := num(j.N)
		e, ok2 := num(j.E)
		if !ok1 || !ok2 || !e.IsInt64() || e.Int64() > 1<<31-1 {
			return nil, nil, errKey
		}
		pub := &rsa.PublicKey{N: n, E: int(e.Int64())}
		if j.D == "" {
			return nil, pub, nil
		}
		d, ok1 := num(j.D)
		p, ok2 := num(j.P)
		q, ok3 := num(j.Q)
		if !ok1 || !ok2 || !ok3 {
			return nil, nil, errKey
		}
		priv := &rsa.PrivateKey{PublicKey: *pub, D: d, Primes: []*big.Int{p, q}}
		if err := priv.Validate(); err != nil {
			return nil, nil, errKey
		}
		priv.Precompute()
		return priv, nil, nil
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
			return nil, nil, errKey
		}
		x, ok1 := num(j.X)
		y, ok2 := num(j.Y)
		if !ok1 || !ok2 {
			return nil, nil, errKey
		}
		pub, err := ecdhPublic(curve, x, y)
		if err != nil {
			return nil, nil, err
		}
		if j.D == "" {
			return nil, pub, nil
		}
		dRaw, err := base64.RawURLEncoding.DecodeString(j.D)
		if err != nil {
			return nil, nil, errKey
		}
		priv, err := ecdsa.ParseRawPrivateKey(curve, leftPad(dRaw, (curve.Params().BitSize+7)/8))
		if err != nil || !priv.PublicKey.Equal(pub) {
			return nil, nil, errKey
		}
		return priv, nil, nil
	case "OKP":
		if j.Crv != "Ed25519" {
			return nil, nil, errKey
		}
		x, err := base64.RawURLEncoding.DecodeString(j.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, nil, errKey
		}
		if j.D == "" {
			return nil, ed25519.PublicKey(x), nil
		}
		d, err := base64.RawURLEncoding.DecodeString(j.D)
		if err != nil || len(d) != ed25519.SeedSize {
			return nil, nil, errKey
		}
		priv := ed25519.NewKeyFromSeed(d)
		if !priv.Public().(ed25519.PublicKey).Equal(ed25519.PublicKey(x)) {
			return nil, nil, errKey
		}
		return priv, nil, nil
	default:
		return nil, nil, errKey
	}
}

// ecdhPublic returns the ECDSA public key of a point, checked on curve.
func ecdhPublic(curve elliptic.Curve, x, y *big.Int) (*ecdsa.PublicKey, error) {
	size := (curve.Params().BitSize + 7) / 8
	raw := append([]byte{4}, leftPad(x.Bytes(), size)...)
	raw = append(raw, leftPad(y.Bytes(), size)...)
	pub, err := ecdsa.ParseUncompressedPublicKey(curve, raw)
	if err != nil {
		return nil, errKey
	}
	return pub, nil
}

// leftPad pads b with leading zeros to n bytes.
func leftPad(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	return append(make([]byte, n-len(b)), b...)
}

// sign returns the JWS signature (RFC 7518) of msg: PKCS #1 v1.5 or PSS
// with a salt as long as the hash for RS and PS, the fixed-size r and s
// for ES, Ed25519 for EdDSA.
func (k *hostKey) sign(rand io.Reader, msg []byte) ([]byte, error) {
	h, _ := jwaHash(k.alg)
	switch k.alg[:2] {
	case "Ed":
		return k.signer.Sign(rand, msg, crypto.Hash(0))
	case "PS":
		return k.signer.Sign(rand, digest(h, msg), &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: h})
	case "ES":
		priv, ok := k.signer.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errKey
		}
		r, s, err := ecdsa.Sign(rand, priv, digest(h, msg))
		if err != nil {
			return nil, err
		}
		size := (priv.Curve.Params().BitSize + 7) / 8
		return append(leftPad(r.Bytes(), size), leftPad(s.Bytes(), size)...), nil
	default:
		return k.signer.Sign(rand, digest(h, msg), h)
	}
}

// verify checks a JWS signature of msg.
func (k *hostKey) verify(msg, sig []byte) bool {
	h, _ := jwaHash(k.alg)
	switch k.alg[:2] {
	case "Ed":
		pub, ok := k.public.(ed25519.PublicKey)
		return ok && ed25519.Verify(pub, msg, sig)
	case "PS":
		pub, ok := k.public.(*rsa.PublicKey)
		return ok && rsa.VerifyPSS(pub, h, digest(h, msg), sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: h}) == nil
	case "ES":
		pub, ok := k.public.(*ecdsa.PublicKey)
		if !ok {
			return false
		}
		size := (pub.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return false
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		return ecdsa.Verify(pub, digest(h, msg), r, s)
	default:
		pub, ok := k.public.(*rsa.PublicKey)
		return ok && rsa.VerifyPKCS1v15(pub, h, digest(h, msg), sig) == nil
	}
}

// mac returns the HMAC of msg under the key's hash.
func (k *hostKey) mac(msg []byte) []byte {
	h, _ := jwaHash(k.alg)
	m := hmac.New(h.New, k.secret)
	m.Write(msg)
	return m.Sum(nil)
}

// digest hashes msg with h.
func digest(h crypto.Hash, msg []byte) []byte {
	d := h.New()
	d.Write(msg)
	return d.Sum(nil)
}

// algHash returns the hash constructor of a crypto_digest or crypto_hmac
// algorithm.
func algHash(alg int32) (func() hash.Hash, bool) {
	switch alg {
	case algSHA256:
		return sha256.New, true
	case algSHA384:
		return sha512.New384, true
	case algSHA512:
		return sha512.New, true
	default:
		return nil, false
	}
}

// hostCryptoDigest is crypto_digest(alg, in, o, c).
func hostCryptoDigest(_ *call, m api.Memory, stack []uint64) int32 {
	alg := s32(stack)
	in := mem(m, u32(stack, 1), u32(stack, 2))
	o, n := u32(stack, 3), u32(stack, 4)
	checkRange(m, o, n)
	newHash, ok := algHash(alg)
	if !ok {
		return errInvalid
	}
	h := newHash()
	h.Write(in)
	return output(m, o, n, h.Sum(nil))
}

// hostCryptoHMAC is crypto_hmac(alg, key, in, o, c), over a key the guest
// holds.
func hostCryptoHMAC(_ *call, m api.Memory, stack []uint64) int32 {
	alg := s32(stack)
	key := mem(m, u32(stack, 1), u32(stack, 2))
	in := mem(m, u32(stack, 3), u32(stack, 4))
	o, n := u32(stack, 5), u32(stack, 6)
	checkRange(m, o, n)
	newHash, ok := algHash(alg)
	if !ok {
		return errInvalid
	}
	h := hmac.New(newHash, key)
	h.Write(in)
	return output(m, o, n, h.Sum(nil))
}

// key returns the host-held key of handle for op: ERR_INVALID for an
// unknown handle, the sticky ERR_DENIED for an operation its declaration
// does not allow.
func (c *call) key(handle int32, op keyOp) (*hostKey, int32) {
	keys := c.inst.p.keys
	if handle < 1 || int(handle) > len(keys) {
		return nil, errInvalid
	}
	k := keys[handle-1]
	if k.ops&op == 0 {
		return nil, c.stick(errDenied, "the key's declaration does not allow the operation")
	}
	return k, 0
}

// hostCryptoKeySign is crypto_key_sign(key, in, o, c).
func hostCryptoKeySign(c *call, m api.Memory, stack []uint64) int32 {
	in := mem(m, u32(stack, 1), u32(stack, 2))
	o, n := u32(stack, 3), u32(stack, 4)
	checkRange(m, o, n)
	k, code := c.key(s32(stack), opSign)
	if k == nil {
		return code
	}
	sig, err := k.sign(c.e.rand, in)
	if err != nil {
		return errInvalid
	}
	return output(m, o, n, sig)
}

// hostCryptoKeyVerify is crypto_key_verify(key, in, sig): 0 for a valid
// signature, ERR_INVALID otherwise.
func hostCryptoKeyVerify(c *call, m api.Memory, stack []uint64) int32 {
	in := mem(m, u32(stack, 1), u32(stack, 2))
	sig := mem(m, u32(stack, 3), u32(stack, 4))
	k, code := c.key(s32(stack), opVerify)
	if k == nil {
		return code
	}
	if !k.verify(in, sig) {
		return errInvalid
	}
	return 0
}

// hostCryptoKeyHMAC is crypto_key_hmac(key, in, o, c).
func hostCryptoKeyHMAC(c *call, m api.Memory, stack []uint64) int32 {
	in := mem(m, u32(stack, 1), u32(stack, 2))
	o, n := u32(stack, 3), u32(stack, 4)
	checkRange(m, o, n)
	k, code := c.key(s32(stack), opHMAC)
	if k == nil {
		return code
	}
	return output(m, o, n, k.mac(in))
}
