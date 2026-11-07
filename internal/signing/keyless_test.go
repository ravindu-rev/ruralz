// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"

	"github.com/ravindu-rev/ruralz/internal/signing/signingtest"
)

// The keyless test deployment: sigstore-go's VirtualSigstore, a local
// certificate authority, transparency log and timestamp authority, so
// keyless bundles verify without the public Sigstore infrastructure.
// VirtualSigstore links rekor's PGP code, so it stays in test files,
// which no shipped or test-support package links.
//
// VirtualSigstore dates its certificates and log keys from the real clock
// when it makes them, and the verifier checks them only against the times
// a bundle carries, so a bundle verifies whatever the clock reads, as long
// as a test signs and verifies within the deployment's hour.

// testSigstore is a local Sigstore deployment.
type testSigstore struct {
	ss *ca.VirtualSigstore
}

// newSigstore starts a deployment.
func newSigstore(t *testing.T) *testSigstore {
	t.Helper()
	ss, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	return &testSigstore{ss: ss}
}

// trustedRoot returns the deployment's trusted root in its JSON form.
func (s *testSigstore) trustedRoot(t *testing.T) []byte {
	t.Helper()
	tr, err := root.NewTrustedRoot(root.TrustedRootMediaType01,
		s.ss.FulcioCertificateAuthorities(), rawLogIDs(t, s.ss.CTLogs()), s.ss.TimestampingAuthorities(), rawLogIDs(t, s.ss.RekorLogs()))
	if err != nil {
		t.Fatal(err)
	}
	data, err := tr.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// rawLogIDs returns logs with each log's ID as the raw key ID its map key
// encodes in hex: VirtualSigstore holds the hex text itself, which a
// trusted root's JSON would carry as the key ID, so no entry would match
// its log after the round trip.
func rawLogIDs(t *testing.T, logs map[string]*root.TransparencyLog) map[string]*root.TransparencyLog {
	t.Helper()
	out := make(map[string]*root.TransparencyLog, len(logs))
	for id, l := range logs {
		raw, err := hex.DecodeString(id)
		if err != nil {
			t.Fatal(err)
		}
		c := *l
		c.ID = raw
		out[id] = &c
	}
	return out
}

// keylessOptions vary a keyless bundle.
type keylessOptions struct {
	// noTlog drops the transparency-log entry.
	noTlog bool
	// noTimestamp drops the timestamp authority's timestamp.
	noTimestamp bool
	// noInclusionPromise drops the log's signed entry timestamp, so the
	// log's time is no verified timestamp.
	noInclusionPromise bool
	// payload, when set, replaces the signed statement in the bundle after
	// signing, so its signature no longer verifies.
	payload []byte
	// timestamps, when set, edits the timestamp authority's RFC 3161
	// responses before they go into the bundle.
	timestamps func(t *testing.T, ts [][]byte) [][]byte
}

// keyless returns a bundle in which identity, certified for issuer, signs
// statement, an in-toto statement, with the deployment's transparency log
// and timestamp authority.
func (s *testSigstore) keyless(t *testing.T, identity, issuer string, statement []byte, o keylessOptions) []byte {
	t.Helper()
	// VirtualSigstore's leaf certificates start at the real clock's now,
	// so the log entry's time must follow it.
	te, err := s.ss.AttestAtTime(identity, issuer, statement, time.Now().Add(5*time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	vc, err := te.VerificationContent()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := te.SignatureContent()
	if err != nil {
		t.Fatal(err)
	}
	env, ok := sc.(*bundle.Envelope)
	if !ok {
		t.Fatalf("signature content %T is no envelope", sc)
	}
	raw := env.RawEnvelope()
	b := signingtest.Bundle{
		MediaType:            signingtest.BundleMediaType,
		VerificationMaterial: signingtest.Material{Certificate: &signingtest.Certificate{RawBytes: vc.Certificate().Raw}, TlogEntries: []signingtest.TlogEntry{}},
		DSSEEnvelope:         &signingtest.Envelope{PayloadType: raw.PayloadType, Payload: raw.Payload},
	}
	for _, sig := range raw.Signatures {
		b.DSSEEnvelope.Signatures = append(b.DSSEEnvelope.Signatures, signingtest.EnvelopeSignature{Sig: sig.Sig, KeyID: sig.KeyID})
	}
	if o.payload != nil {
		b.DSSEEnvelope.Payload = base64.StdEncoding.EncodeToString(o.payload)
	}
	if !o.noTimestamp {
		ts, err := te.Timestamps()
		if err != nil {
			t.Fatal(err)
		}
		if o.timestamps != nil {
			ts = o.timestamps(t, ts)
		}
		tvd := &signingtest.TimestampData{}
		for _, raw := range ts {
			tvd.RFC3161Timestamps = append(tvd.RFC3161Timestamps, signingtest.Timestamp{SignedTimestamp: raw})
		}
		b.VerificationMaterial.TimestampVerificationData = tvd
	}
	if !o.noTlog {
		entries, err := te.TlogEntries()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b.VerificationMaterial.TlogEntries = append(b.VerificationMaterial.TlogEntries, s.tlogEntry(t, e, !o.noInclusionPromise))
		}
	}
	return b.JSON(t)
}

// tlogEntry converts a VirtualSigstore log entry to the bundle's form,
// signing its entry timestamp again since the entry keeps it private.
func (s *testSigstore) tlogEntry(t *testing.T, e *tlog.Entry, promise bool) signingtest.TlogEntry {
	t.Helper()
	tle := e.TransparencyLogEntry()
	out := signingtest.TlogEntry{
		LogIndex:          strconv.FormatInt(tle.GetLogIndex(), 10),
		LogID:             signingtest.LogID{KeyID: tle.GetLogId().GetKeyId()},
		KindVersion:       signingtest.KindVersion{Kind: "dsse", Version: "0.0.1"},
		IntegratedTime:    strconv.FormatInt(tle.GetIntegratedTime(), 10),
		CanonicalizedBody: tle.GetCanonicalizedBody(),
	}
	if p := tle.GetInclusionProof(); p != nil {
		out.InclusionProof = &signingtest.InclusionProof{
			LogIndex: strconv.FormatInt(p.GetLogIndex(), 10), RootHash: p.GetRootHash(),
			TreeSize: strconv.FormatInt(p.GetTreeSize(), 10), Hashes: p.GetHashes(),
			Checkpoint: signingtest.Checkpoint{Envelope: p.GetCheckpoint().GetEnvelope()},
		}
		if out.InclusionProof.Hashes == nil {
			out.InclusionProof.Hashes = [][]byte{}
		}
	}
	if promise {
		set, err := s.ss.RekorSignPayload(tlog.RekorPayload{
			Body:           base64.StdEncoding.EncodeToString(tle.GetCanonicalizedBody()),
			IntegratedTime: tle.GetIntegratedTime(),
			LogIndex:       tle.GetLogIndex(),
			LogID:          hex.EncodeToString(tle.GetLogId().GetKeyId()),
		})
		if err != nil {
			t.Fatal(err)
		}
		out.InclusionPromise = &signingtest.Promise{SignedEntryTimestamp: set}
	}
	return out
}
