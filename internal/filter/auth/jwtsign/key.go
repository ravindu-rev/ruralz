// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// family is the kind of key an algorithm signs with.
type family uint8

// Key families: RSA for RS256 and PS256, P-256 for ES256 and Ed25519 for
// EdDSA.
const (
	familyRSA family = iota + 1
	familyP256
	familyEd25519
)

// String names the family in refusals.
func (f family) String() string {
	switch f {
	case familyRSA:
		return "an RSA key"
	case familyP256:
		return "a P-256 key"
	case familyEd25519:
		return "an Ed25519 key"
	default:
		return "no key"
	}
}

// familyOf returns the family algorithm alg signs with; 0 for an
// algorithm the Policy does not offer.
func familyOf(alg v1alpha1.JWTSignAlgorithm) family {
	switch alg {
	case v1alpha1.JWTSignAlgorithmRS256, v1alpha1.JWTSignAlgorithmPS256:
		return familyRSA
	case v1alpha1.JWTSignAlgorithmES256:
		return familyP256
	case v1alpha1.JWTSignAlgorithmEdDSA:
		return familyEd25519
	default:
		return 0
	}
}

// MinRSABits is the smallest RSA modulus a signing key may have, the size
// auth.jwt accepts for a verification key (docs/architecture/08-security-
// and-identity.md "JWT and OIDC").
const MinRSABits = 2048

// Fixed reasons a signing key is refused. None repeats any part of the
// value: a reason names at most a JWK member or a PEM block type.
var (
	errEmptyKey     = errors.New("config.signingKey is empty")
	errNotKey       = errors.New("config.signingKey holds neither a JWK nor a PEM private key")
	errPublicKey    = errors.New("config.signingKey holds a public key, not a private key")
	errEncryptedKey = errors.New("config.signingKey holds an encrypted PEM private key")
	errTwoKeys      = errors.New("config.signingKey holds more than one PEM private key")
	errBadPEMKey    = errors.New("a PEM private key of config.signingKey does not parse (unencrypted PKCS #8, PKCS #1 or SEC 1 expected)")
	errKeyType      = errors.New("config.signingKey is not an RSA, P-256 or Ed25519 key")
	errRSASize      = fmt.Errorf("the RSA key of config.signingKey has fewer than %d bits", MinRSABits)
	errRSAKey       = errors.New("the RSA key of config.signingKey is not a consistent private key")
	errJWKNotObject = errors.New("config.signingKey is not a JWK: not one JSON object, or not strict JSON")
	errJWKSet       = errors.New("config.signingKey is a JWK Set; it holds one JWK")
	errJWKUse       = errors.New(`the JWK of config.signingKey has a use other than "sig"`)
	errJWKOps       = errors.New(`the JWK of config.signingKey has key_ops without "sign"`)
	errJWKPrimes    = errors.New("the RSA JWK of config.signingKey holds some but not all of p, q, dp, dq and qi")
	errJWKOth       = errors.New("the RSA JWK of config.signingKey has more than two prime factors (oth)")
	errJWKPublic    = errors.New("the public members of the JWK of config.signingKey do not match its private key")
)

// maxJWKBytes bounds a JWK's decoded tree: four times the 4 MiB a secret
// value may hold, the rule for decoded JSON (filter.DecodedLimitFactor).
const maxJWKBytes = 4 * (4 << 20)

// jwkMemberError is a JWK member that is absent, of the wrong type or not
// a valid value; its text names the member, never its value.
type jwkMemberError struct{ member, problem string }

// Error returns the refusal.
func (e *jwkMemberError) Error() string {
	return fmt.Sprintf("the JWK of config.signingKey: member %q %s", e.member, e.problem)
}

// key is one parsed private signing key.
type key struct {
	family family
	// signer is a *rsa.PrivateKey, *ecdsa.PrivateKey or ed25519.PrivateKey.
	signer crypto.Signer
	// alg is a JWK's alg member; "" when absent and for a PEM key.
	alg string
}

// parseKey reads a private signing key from a secret value: a JWK (RFC
// 7517, RFC 7518 section 6, RFC 8037), when the value, trimmed of
// whitespace, is a JSON object, else an unencrypted PEM private key. The
// key is an RSA key of at least MinRSABits bits, a P-256 key or an Ed25519
// key, never a public key. Text outside PEM blocks is ignored, and a
// value holds at most one private key.
func parseKey(b []byte) (*key, error) {
	t := bytes.TrimSpace(b)
	switch {
	case len(t) == 0:
		return nil, errEmptyKey
	case t[0] == '{':
		return parseJWK(t)
	default:
		return parsePEM(b)
	}
}

// fits checks that k signs with alg: a JWK naming an alg names alg, and k
// is of the family alg signs with.
func (k *key) fits(alg v1alpha1.JWTSignAlgorithm) error {
	if k.alg != "" && k.alg != string(alg) {
		return fmt.Errorf("the JWK of config.signingKey names alg %q, not config.algorithm %s", k.alg, alg)
	}
	if want := familyOf(alg); k.family != want {
		return fmt.Errorf("config.algorithm %s signs with %s; config.signingKey holds %s", alg, want, k.family)
	}
	return nil
}

// PEM block types.
const (
	pemPKCS8       = "PRIVATE KEY"
	pemPKCS1       = "RSA PRIVATE KEY"
	pemSEC1        = "EC PRIVATE KEY"
	pemEncrypted   = "ENCRYPTED PRIVATE KEY"
	pemPublic      = "PUBLIC KEY"
	pemRSAPublic   = "RSA PUBLIC KEY"
	pemCertificate = "CERTIFICATE"
)

// parsePEM reads the one PEM private key block of b: PKCS #8, PKCS #1 or
// SEC 1, unencrypted. Other blocks, such as a certificate or EC
// parameters, are skipped; a value whose only keys are public is
// errPublicKey.
func parsePEM(b []byte) (*key, error) {
	var (
		found  *key
		public bool
	)
	for rest := b; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		k, err := pemBlock(blk, found != nil)
		clear(blk.Bytes)
		switch {
		case err != nil:
			return nil, err
		case k != nil:
			found = k
		case blk.Type == pemPublic || blk.Type == pemRSAPublic || blk.Type == pemCertificate:
			public = true
		}
	}
	switch {
	case found != nil:
		return found, nil
	case public:
		return nil, errPublicKey
	default:
		return nil, errNotKey
	}
}

// pemBlock returns the key of a private key block, nil for another block;
// seen reports a private key block before it.
func pemBlock(blk *pem.Block, seen bool) (*key, error) {
	t := blk.Type
	private := t == pemPKCS8 || t == pemPKCS1 || t == pemSEC1 || strings.HasSuffix(t, " "+pemPKCS8)
	if !private {
		return nil, nil
	}
	switch {
	case t == pemEncrypted || strings.Contains(blk.Headers["Proc-Type"], "ENCRYPTED"):
		return nil, errEncryptedKey
	case seen:
		return nil, errTwoKeys
	}
	var (
		raw any
		err error
	)
	switch t {
	case pemPKCS8:
		raw, err = x509.ParsePKCS8PrivateKey(blk.Bytes)
	case pemPKCS1:
		raw, err = x509.ParsePKCS1PrivateKey(blk.Bytes)
	case pemSEC1:
		raw, err = x509.ParseECPrivateKey(blk.Bytes)
	default:
		return nil, errBadPEMKey
	}
	if err != nil {
		return nil, errBadPEMKey
	}
	return rawKey(raw)
}

// rawKey classifies a parsed private key.
func rawKey(raw any) (*key, error) {
	switch k := raw.(type) {
	case *rsa.PrivateKey:
		return rsaKey(k)
	case *ecdsa.PrivateKey:
		if k.Curve != elliptic.P256() {
			return nil, errKeyType
		}
		return &key{family: familyP256, signer: k}, nil
	case ed25519.PrivateKey:
		return &key{family: familyEd25519, signer: k}, nil
	default:
		return nil, errKeyType
	}
}

// rsaCheck is the message rsaKey signs to check a key: a key whose private
// exponent does not belong to its public key yields a signature its public
// key refuses.
const rsaCheck = "ruralz auth.jwt-sign key check"

// rsaKey checks an RSA private key: at least MinRSABits bits, consistent
// primes and CRT values when it holds them (rsa.PrivateKey.Validate), and
// a signature its public key verifies, which also covers a JWK key
// without primes.
func rsaKey(k *rsa.PrivateKey) (*key, error) {
	if k.N == nil || k.N.BitLen() < MinRSABits {
		return nil, errRSASize
	}
	k.Precompute()
	if len(k.Primes) > 0 {
		if err := k.Validate(); err != nil {
			return nil, errRSAKey
		}
	}
	digest := sha256.Sum256([]byte(rsaCheck))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
	if err != nil || rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, digest[:], sig) != nil {
		return nil, errRSAKey
	}
	return &key{family: familyRSA, signer: k}, nil
}

// JWK members.
const (
	jwkKty    = "kty"
	jwkUse    = "use"
	jwkKeyOps = "key_ops"
	jwkAlg    = "alg"
	jwkCrv    = "crv"
	jwkX      = "x"
	jwkY      = "y"
	jwkD      = "d"
	jwkN      = "n"
	jwkE      = "e"
	jwkOth    = "oth"
)

// rsaCRT lists the optional RSA private members, all present or all absent
// (RFC 7518 section 6.3.2).
func rsaCRT() []string { return []string{"p", "q", "dp", "dq", "qi"} }

// parseJWK reads a private JWK: strict JSON, one object without duplicate
// member names, whose members a reader does not know are ignored (RFC 7517
// section 4). use, when present, is "sig", key_ops, when present, holds
// "sign", and alg, when present, is a string fits compares; kty is RSA,
// EC (crv P-256) or OKP (crv Ed25519), with its private member d.
func parseJWK(b []byte) (*key, error) {
	v, _, err := jsonval.Decode(b, jsonval.Options{MapObjects: true, MaxCost: maxJWKBytes})
	m, ok := v.(map[string]any)
	if err != nil || !ok {
		return nil, errJWKNotObject
	}
	if _, set := m["keys"]; set {
		if _, one := m[jwkKty]; !one {
			return nil, errJWKSet
		}
	}
	kty, err := jwkString(m, jwkKty, true)
	if err != nil {
		return nil, err
	}
	use, err := jwkString(m, jwkUse, false)
	if err != nil {
		return nil, err
	}
	if _, present := m[jwkUse]; present && use != "sig" {
		return nil, errJWKUse
	}
	if err := jwkOps(m); err != nil {
		return nil, err
	}
	alg, err := jwkString(m, jwkAlg, false)
	if err != nil {
		return nil, err
	}
	if _, private := m[jwkD]; !private && (kty == "RSA" || kty == "EC" || kty == "OKP") {
		return nil, errPublicKey
	}
	var k *key
	switch kty {
	case "RSA":
		k, err = rsaJWK(m)
	case "EC":
		k, err = ecJWK(m)
	case "OKP":
		k, err = okpJWK(m)
	default:
		return nil, errKeyType
	}
	if err != nil {
		return nil, err
	}
	k.alg = alg
	return k, nil
}

// jwkString returns the string member name of m; "" when it is absent and
// not required.
func jwkString(m map[string]any, name string, required bool) (string, error) {
	v, ok := m[name]
	if !ok {
		if required {
			return "", &jwkMemberError{member: name, problem: "is absent"}
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", &jwkMemberError{member: name, problem: "is not a string"}
	}
	return s, nil
}

// jwkOps checks key_ops: when present, an array of strings holding "sign"
// (RFC 7517 section 4.3).
func jwkOps(m map[string]any) error {
	v, ok := m[jwkKeyOps]
	if !ok {
		return nil
	}
	ops, ok := v.([]any)
	if !ok {
		return &jwkMemberError{member: jwkKeyOps, problem: "is not an array of strings"}
	}
	sign := false
	for _, o := range ops {
		s, ok := o.(string)
		if !ok {
			return &jwkMemberError{member: jwkKeyOps, problem: "is not an array of strings"}
		}
		sign = sign || s == "sign"
	}
	if !sign {
		return errJWKOps
	}
	return nil
}

// b64 is the base64url form of JWK members: unpadded, canonical.
func b64() *base64.Encoding { return base64.RawURLEncoding.Strict() }

// jwkBytes returns the base64url member name of m, decoded; it is required.
// With size above 0 it must decode to exactly size bytes.
func jwkBytes(m map[string]any, name string, size int) ([]byte, error) {
	s, err := jwkString(m, name, true)
	if err != nil {
		return nil, err
	}
	b, err := b64().DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, &jwkMemberError{member: name, problem: "is not base64url"}
	}
	if size > 0 && len(b) != size {
		clear(b)
		return nil, &jwkMemberError{member: name, problem: fmt.Sprintf("does not decode to %d bytes", size)}
	}
	return b, nil
}

// jwkInt returns the base64urlUInt member name of m as an integer.
func jwkInt(m map[string]any, name string) (*big.Int, error) {
	b, err := jwkBytes(m, name, 0)
	if err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(b)
	clear(b)
	return n, nil
}

// rsaJWK reads an RSA private JWK: n, e and d, with all or none of p, q,
// dp, dq and qi, and no oth.
func rsaJWK(m map[string]any) (*key, error) {
	if _, ok := m[jwkOth]; ok {
		return nil, errJWKOth
	}
	n, err := jwkInt(m, jwkN)
	if err != nil {
		return nil, err
	}
	e, err := jwkInt(m, jwkE)
	if err != nil {
		return nil, err
	}
	if e.Sign() <= 0 || e.BitLen() > 31 {
		return nil, &jwkMemberError{member: jwkE, problem: "is not a public exponent"}
	}
	d, err := jwkInt(m, jwkD)
	if err != nil {
		return nil, err
	}
	k := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: n, E: int(e.Int64())}, D: d}
	crt := rsaCRT()
	present := 0
	for _, name := range crt {
		if _, ok := m[name]; ok {
			present++
		}
	}
	switch present {
	case 0:
	case len(crt):
		vals := make([]*big.Int, len(crt))
		for i, name := range crt {
			if vals[i], err = jwkInt(m, name); err != nil {
				return nil, err
			}
		}
		k.Primes = []*big.Int{vals[0], vals[1]}
		k.Precomputed.Dp, k.Precomputed.Dq, k.Precomputed.Qinv = vals[2], vals[3], vals[4]
	default:
		return nil, errJWKPrimes
	}
	return rsaKey(k)
}

// ecJWK reads an EC private JWK on P-256: x, y and d, each 32 bytes (RFC
// 7518 sections 6.2.1 and 6.2.2), the public point that of d.
func ecJWK(m map[string]any) (*key, error) {
	crv, err := jwkString(m, jwkCrv, true)
	if err != nil {
		return nil, err
	}
	if crv != "P-256" {
		return nil, errKeyType
	}
	const size = 32
	x, err := jwkBytes(m, jwkX, size)
	if err != nil {
		return nil, err
	}
	y, err := jwkBytes(m, jwkY, size)
	if err != nil {
		return nil, err
	}
	d, err := jwkBytes(m, jwkD, size)
	if err != nil {
		return nil, err
	}
	defer clear(d)
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d)
	if err != nil {
		return nil, &jwkMemberError{member: jwkD, problem: "is not a P-256 private scalar"}
	}
	pub, err := priv.PublicKey.Bytes()
	if err != nil || !bytes.Equal(pub, slices.Concat([]byte{4}, x, y)) {
		return nil, errJWKPublic
	}
	return &key{family: familyP256, signer: priv}, nil
}

// okpJWK reads an OKP private JWK on Ed25519 (RFC 8037 section 2): x, the
// public key, and d, the seed, each 32 bytes, x that of d.
func okpJWK(m map[string]any) (*key, error) {
	crv, err := jwkString(m, jwkCrv, true)
	if err != nil {
		return nil, err
	}
	if crv != "Ed25519" {
		return nil, errKeyType
	}
	x, err := jwkBytes(m, jwkX, ed25519.PublicKeySize)
	if err != nil {
		return nil, err
	}
	d, err := jwkBytes(m, jwkD, ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	priv := ed25519.NewKeyFromSeed(d)
	clear(d)
	pub, _ := priv.Public().(ed25519.PublicKey)
	if !bytes.Equal(pub, x) {
		return nil, errJWKPublic
	}
	return &key{family: familyEd25519, signer: priv}, nil
}
