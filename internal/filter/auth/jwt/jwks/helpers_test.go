// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwks

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
)

// t0 is the fake start time of every test.
var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// genKey generates a mockidp key or fails the test.
func genKey(t testing.TB, alg mockidp.Alg) *mockidp.Key {
	t.Helper()
	k, err := mockidp.GenerateKey(alg)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// wrapKey wraps signer with a fixed kid or fails the test.
func wrapKey(t testing.TB, alg mockidp.Alg, kid string, signer crypto.Signer) *mockidp.Key {
	t.Helper()
	k, err := mockidp.NewKey(alg, kid, signer)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// jwkMap returns k's public JWK as a JSON object, for documents that add
// or change members.
func jwkMap(t testing.TB, k *mockidp.Key) map[string]any {
	t.Helper()
	b, err := json.Marshal(k.JWK())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// doc encodes a JWK Set of entries.
func doc(t testing.TB, entries ...any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"keys": entries})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// b64 encodes b as base64url without padding.
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// rsaKey generates an RSA key of bits or fails the test.
func rsaKey(t testing.TB, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// ecKey generates an ECDSA key on c or fails the test.
func ecKey(t testing.TB, c elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// edKey generates an Ed25519 key or fails the test.
func edKey(t testing.TB) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// privateRSAJWK returns the private JWK of k with kid.
func privateRSAJWK(k *rsa.PrivateKey, kid string) map[string]any {
	return map[string]any{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes()),
		"d": b64(k.D.Bytes()), "p": b64(k.Primes[0].Bytes()), "q": b64(k.Primes[1].Bytes()),
	}
}

// revoked is an identity.Revoker over (issuer, kid) pairs.
type revoked map[[2]string]bool

func (r revoked) RevokedJWT(string, string, string, string, time.Time) bool { return false }
func (r revoked) RevokedKey(iss, kid string) bool                           { return r[[2]string{iss, kid}] }
func (r revoked) RevokedAPIKey([32]byte) bool                               { return false }
func (r revoked) RevokedBasic(string) bool                                  { return false }
func (r revoked) RevokedCert([]byte, *big.Int) bool                         { return false }

// status records degraded states by reason and holder.
type status struct {
	mu   sync.Mutex
	held map[catalog.Reason]map[string]bool
}

func (s *status) SetDegraded(r catalog.Reason, holder string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held == nil {
		s.held = map[catalog.Reason]map[string]bool{}
	}
	if s.held[r] == nil {
		s.held[r] = map[string]bool{}
	}
	if on {
		s.held[r][holder] = true
	} else {
		delete(s.held[r], holder)
	}
}

func (s *status) SetCleartextHops(catalog.Hop, int) {}

// stale reports whether jwks_stale is held for holder.
func (s *status) stale(holder string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held[catalog.ReasonJWKSStale][holder]
}

// staleCount returns the number of holders of jwks_stale.
func (s *status) staleCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.held[catalog.ReasonJWKSStale])
}

// holders returns the holders of r.
func (s *status) holders(r catalog.Reason) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for h := range s.held[r] {
		out = append(out, h)
	}
	return out
}

// waitFor polls cond with a real-time bound; it only waits for goroutines
// the test started, never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}
