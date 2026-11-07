// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package serviceaccount reads Google Cloud service-account JSON keys, the
// value of an auth.upstream-oauth2 config.serviceAccountKey
// (docs/architecture/08-security-and-identity.md "Upstream
// authentication"): the client_email a jwt-bearer assertion names as its
// issuer, the private_key_id it names as its key ID, and the RSA private
// key that signs it. The secret resolver checks a serviceAccountKey value
// with Parse when it resolves and rotates it, so a key that does not parse
// is RZ-CFG-026 before a Revision activates and never replaces a working
// one; the Filter parses it again for each assertion it signs.
package serviceaccount

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
)

// MinRSABits is the smallest RSA modulus a key may have, 2,048 bits, the
// size Google issues and the smallest FIPS 140-3 signing allows.
const MinRSABits = 2048

// typeServiceAccount is the type member of a service-account key.
const typeServiceAccount = "service_account"

// Parse failures. Their messages are fixed and never repeat key material.
var (
	ErrNotJSON     = errors.New("service-account key is not a JSON object")
	ErrType        = errors.New(`service-account key type is not "service_account"`)
	ErrNoEmail     = errors.New("service-account key has no client_email")
	ErrNoKey       = errors.New("service-account key private_key holds no PEM private key")
	ErrKeyEncoding = errors.New("service-account key private_key does not parse (PKCS #8 or PKCS #1 expected)")
	ErrNotRSA      = errors.New("service-account key private_key is not an RSA key")
	ErrKeySize     = fmt.Errorf("service-account key private_key is shorter than %d bits", MinRSABits)
)

// Key is a parsed service-account key. It never prints its private key.
type Key struct {
	// Email is client_email, the service account.
	Email string
	// KeyID is private_key_id; "" when the key names none.
	KeyID string
	// Private is the RSA private key.
	Private *rsa.PrivateKey
}

// String returns the service account and key ID, never the key.
func (k Key) String() string { return "service account " + k.Email + " key " + k.KeyID }

// GoString returns String.
func (k Key) GoString() string { return k.String() }

// LogValue returns String for log/slog.
func (k Key) LogValue() slog.Value { return slog.StringValue(k.String()) }

// file is the part of a service-account JSON key Parse reads.
type file struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
}

// Parse reads a service-account JSON key: a JSON object whose type is
// service_account, with a client_email and a private_key holding one PEM
// block of an unencrypted RSA private key of at least MinRSABits, PKCS #8
// (PRIVATE KEY, as Google issues) or PKCS #1 (RSA PRIVATE KEY). Other
// members are ignored. The decoded key bytes are cleared once parsed.
func Parse(b []byte) (*Key, error) {
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, ErrNotJSON
	}
	if f.Type != typeServiceAccount {
		return nil, ErrType
	}
	if f.ClientEmail == "" {
		return nil, ErrNoEmail
	}
	blk, _ := pem.Decode([]byte(f.PrivateKey))
	if blk == nil {
		return nil, ErrNoKey
	}
	defer clear(blk.Bytes)
	var priv any
	var err error
	switch blk.Type {
	case "PRIVATE KEY":
		priv, err = x509.ParsePKCS8PrivateKey(blk.Bytes)
	case "RSA PRIVATE KEY":
		priv, err = x509.ParsePKCS1PrivateKey(blk.Bytes)
	default:
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, ErrKeyEncoding
	}
	k, ok := priv.(*rsa.PrivateKey)
	if !ok {
		return nil, ErrNotRSA
	}
	if k.N.BitLen() < MinRSABits {
		return nil, ErrKeySize
	}
	return &Key{Email: f.ClientEmail, KeyID: f.PrivateKeyID, Private: k}, nil
}
