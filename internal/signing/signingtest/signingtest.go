// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package signingtest is the test support of internal/signing: Sigstore
// bundles made without the public Sigstore infrastructure, so signature
// tests stay hermetic.
//
// Bundle is a Sigstore bundle's JSON form, the protobuf JSON mapping of
// the Bundle message, which tests fill to make any bundle, valid or not.
// Key signs with a long-lived ECDSA P-256 key as cosign does with --key:
// a DSSE envelope of an in-toto statement, or a message signature over an
// artifact digest. Keyless bundles, which need a certificate authority, a
// transparency log and a timestamp authority, are made by the signing
// package's own tests on sigstore-go's VirtualSigstore, which links code
// no shipped or test-support package may link.
package signingtest

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"testing"
)

// Media types of the Sigstore formats.
const (
	// BundleMediaType is a v0.3 Sigstore bundle's media type.
	BundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	// PayloadTypeInToto is the DSSE payload type of an in-toto statement.
	PayloadTypeInToto = "application/vnd.in-toto+json"
)

// Bundle is a Sigstore bundle's JSON form: bytes are base64 and 64-bit
// integers strings, as the protobuf JSON mapping writes them.
type Bundle struct {
	MediaType            string            `json:"mediaType"`
	VerificationMaterial Material          `json:"verificationMaterial"`
	DSSEEnvelope         *Envelope         `json:"dsseEnvelope,omitempty"`
	MessageSignature     *MessageSignature `json:"messageSignature,omitempty"`
}

// Material is the bundle's verification material.
type Material struct {
	Certificate               *Certificate   `json:"certificate,omitempty"`
	PublicKey                 *PublicKeyHint `json:"publicKey,omitempty"`
	TlogEntries               []TlogEntry    `json:"tlogEntries"`
	TimestampVerificationData *TimestampData `json:"timestampVerificationData,omitempty"`
}

// Certificate is a DER certificate.
type Certificate struct {
	RawBytes []byte `json:"rawBytes"`
}

// PublicKeyHint names the key of a key-based bundle.
type PublicKeyHint struct {
	Hint string `json:"hint"`
}

// TlogEntry is a transparency-log entry.
type TlogEntry struct {
	LogIndex          string          `json:"logIndex"`
	LogID             LogID           `json:"logId"`
	KindVersion       KindVersion     `json:"kindVersion"`
	IntegratedTime    string          `json:"integratedTime"`
	InclusionPromise  *Promise        `json:"inclusionPromise,omitempty"`
	InclusionProof    *InclusionProof `json:"inclusionProof,omitempty"`
	CanonicalizedBody []byte          `json:"canonicalizedBody"`
}

// LogID is a log's key ID.
type LogID struct {
	KeyID []byte `json:"keyId"`
}

// KindVersion is an entry's kind and version.
type KindVersion struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

// Promise is a signed entry timestamp.
type Promise struct {
	SignedEntryTimestamp []byte `json:"signedEntryTimestamp"`
}

// InclusionProof is an entry's inclusion proof with its checkpoint.
type InclusionProof struct {
	LogIndex   string     `json:"logIndex"`
	RootHash   []byte     `json:"rootHash"`
	TreeSize   string     `json:"treeSize"`
	Hashes     [][]byte   `json:"hashes"`
	Checkpoint Checkpoint `json:"checkpoint"`
}

// Checkpoint is a signed tree head.
type Checkpoint struct {
	Envelope string `json:"envelope"`
}

// TimestampData holds RFC 3161 timestamps.
type TimestampData struct {
	RFC3161Timestamps []Timestamp `json:"rfc3161Timestamps"`
}

// Timestamp is one RFC 3161 timestamp response.
type Timestamp struct {
	SignedTimestamp []byte `json:"signedTimestamp"`
}

// Envelope is a DSSE envelope; Payload and each Sig are base64.
type Envelope struct {
	PayloadType string              `json:"payloadType"`
	Payload     string              `json:"payload"`
	Signatures  []EnvelopeSignature `json:"signatures"`
}

// EnvelopeSignature is one DSSE signature.
type EnvelopeSignature struct {
	Sig   string `json:"sig"`
	KeyID string `json:"keyid,omitempty"`
}

// MessageSignature signs an artifact digest.
type MessageSignature struct {
	MessageDigest Hash   `json:"messageDigest"`
	Signature     []byte `json:"signature"`
}

// Hash is a digest and its algorithm.
type Hash struct {
	Algorithm string `json:"algorithm"`
	Digest    []byte `json:"digest"`
}

// JSON encodes the bundle; tb fails on an error.
func (b Bundle) JSON(tb testing.TB) []byte {
	tb.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		tb.Fatal(err)
	}
	return data
}

// PAE is DSSE's pre-authentication encoding of a payload, which a DSSE
// signature signs.
func PAE(payloadType string, payload []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "DSSEv1 %d %s %d ", len(payloadType), payloadType, len(payload))
	b.Write(payload)
	return b.Bytes()
}

// Key is a long-lived ECDSA P-256 signing key.
type Key struct {
	priv *ecdsa.PrivateKey
}

// NewKey generates a key.
func NewKey(tb testing.TB) *Key {
	tb.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	return &Key{priv: k}
}

// PublicKeyPEM returns the key's public half as a PEM PUBLIC KEY block.
func (k *Key) PublicKeyPEM(tb testing.TB) []byte {
	tb.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.priv.PublicKey)
	if err != nil {
		tb.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// Sign returns a bundle in which the key signs statement, an in-toto
// statement, as a DSSE envelope; payload, when set, replaces the
// statement after signing, so the signature no longer verifies.
func (k *Key) Sign(tb testing.TB, statement, payload []byte) []byte {
	tb.Helper()
	sig := k.sign(tb, PAE(PayloadTypeInToto, statement))
	if payload == nil {
		payload = statement
	}
	return Bundle{
		MediaType:            BundleMediaType,
		VerificationMaterial: Material{PublicKey: &PublicKeyHint{Hint: k.hint(tb)}, TlogEntries: []TlogEntry{}},
		DSSEEnvelope: &Envelope{
			PayloadType: PayloadTypeInToto, Payload: base64.StdEncoding.EncodeToString(payload),
			Signatures: []EnvelopeSignature{{Sig: base64.StdEncoding.EncodeToString(sig)}},
		},
	}.JSON(tb)
}

// SignDigest returns a bundle in which the key signs the sha256 digest
// raw of an artifact as a message signature, which signs no annotation.
func (k *Key) SignDigest(tb testing.TB, raw []byte) []byte {
	tb.Helper()
	sig, err := ecdsa.SignASN1(rand.Reader, k.priv, raw)
	if err != nil {
		tb.Fatal(err)
	}
	return Bundle{
		MediaType:            BundleMediaType,
		VerificationMaterial: Material{PublicKey: &PublicKeyHint{Hint: k.hint(tb)}, TlogEntries: []TlogEntry{}},
		MessageSignature:     &MessageSignature{MessageDigest: Hash{Algorithm: "SHA2_256", Digest: raw}, Signature: sig},
	}.JSON(tb)
}

// sign signs the SHA-256 of msg.
func (k *Key) sign(tb testing.TB, msg []byte) []byte {
	tb.Helper()
	h := sha256.Sum256(msg)
	sig, err := k.priv.Sign(rand.Reader, h[:], crypto.SHA256)
	if err != nil {
		tb.Fatal(err)
	}
	return sig
}

// hint names the key in a bundle: base64 of the SHA-256 of its DER
// public key, as cosign writes it.
func (k *Key) hint(tb testing.TB) string {
	tb.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.priv.PublicKey)
	if err != nil {
		tb.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return base64.StdEncoding.EncodeToString(sum[:])
}
