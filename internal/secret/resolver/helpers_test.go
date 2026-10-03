// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// epoch is the fake clock's start. Test files get modification times an
// hour or more before it, so reads are trusted (not racy) unless a test
// sets a recent time on purpose.
func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// fakeCounter is an emit.Counter.
type fakeCounter struct{ n atomic.Uint64 }

func (c *fakeCounter) Add(_ emit.Stripe, n uint64) { c.n.Add(n) }

func (c *fakeCounter) value() uint64 { return c.n.Load() }

// fakeStatus is an emit.NodeStatus recording the sources holding
// secret_rotation_failed.
type fakeStatus struct {
	t     testing.TB
	mu    sync.Mutex
	on    map[string]bool
	calls int
}

func (s *fakeStatus) SetDegraded(r catalog.Reason, source string, on bool) {
	if r != catalog.ReasonSecretRotationFailed {
		s.t.Errorf("SetDegraded(%v): want %v", r, catalog.ReasonSecretRotationFailed)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if on {
		s.on[source] = true
	} else {
		delete(s.on, source)
	}
}

func (*fakeStatus) SetCleartextHops(catalog.Hop, int) {}

// raised returns the sources holding the reason, sorted.
func (s *fakeStatus) raised() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.on))
	for k := range s.on {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// syncBuffer is a bytes.Buffer safe for the resolver goroutine and the
// test to share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// harness is a Resolver over a temporary secret root with a fake clock,
// environment, metrics, status and a captured JSON log.
type harness struct {
	t       testing.TB
	r       *Resolver
	root    string
	clock   *clocktest.Fake
	envMu   sync.Mutex
	env     map[string]string
	fileN   *fakeCounter
	envN    *fakeCounter
	status  *fakeStatus
	logs    *syncBuffer
	mtimeSq atomic.Int64
}

// newHarness returns a harness; edit, when set, adjusts the Config.
func newHarness(t testing.TB, edit func(*Config)) *harness {
	t.Helper()
	h := &harness{
		t:      t,
		root:   t.TempDir(),
		clock:  clocktest.New(epoch()),
		env:    map[string]string{},
		fileN:  &fakeCounter{},
		envN:   &fakeCounter{},
		status: &fakeStatus{t: t, on: map[string]bool{}},
		logs:   &syncBuffer{},
	}
	cfg := Config{
		Root:      h.root,
		LookupEnv: h.lookupEnv,
		Clock:     h.clock,
		Logger:    slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		RotationFailures: func(p string) emit.Counter {
			switch p {
			case providerFile:
				return h.fileN
			case providerEnv:
				return h.envN
			}
			t.Errorf("RotationFailures(%q)", p)
			return nil
		},
		Status: h.status,
	}
	if edit != nil {
		edit(&cfg)
	}
	r, err := NewResolver(cfg)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	h.r = r
	return h
}

func (h *harness) lookupEnv(name string) (string, bool) {
	h.envMu.Lock()
	defer h.envMu.Unlock()
	v, ok := h.env[name]
	return v, ok
}

func (h *harness) setEnv(name, value string) {
	h.envMu.Lock()
	defer h.envMu.Unlock()
	h.env[name] = value
}

// path returns the absolute slash path of rel under the root.
func (h *harness) path(rel string) string { return filepath.ToSlash(filepath.Join(h.root, rel)) }

// write writes content to rel under the root (mode 0600) with a fresh,
// trusted modification time, so each write changes the fingerprint.
func (h *harness) write(rel, content string) {
	h.t.Helper()
	p := filepath.Join(h.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.touch(rel, epoch().Add(-time.Hour).Add(time.Duration(h.mtimeSq.Add(1))*time.Second))
}

// touch sets the modification time of rel.
func (h *harness) touch(rel string, mtime time.Time) {
	h.t.Helper()
	if err := os.Chtimes(filepath.Join(h.root, rel), mtime, mtime); err != nil {
		h.t.Fatal(err)
	}
}

// resolve resolves uses and fails the test on diagnostics.
func (h *harness) resolve(uses ...secret.Use) secret.Store {
	h.t.Helper()
	st, diags := h.r.Resolve(context.Background(), uses)
	if len(diags) > 0 || st == nil {
		h.t.Fatalf("Resolve: store %v, diagnostics:\n%s", st != nil, diagText(diags))
	}
	return st
}

// resolveDiags resolves uses expecting failure and returns the diagnostics.
func (h *harness) resolveDiags(uses ...secret.Use) diag.List {
	h.t.Helper()
	st, diags := h.r.Resolve(context.Background(), uses)
	if st != nil {
		h.t.Fatalf("Resolve returned a Store, want diagnostics")
	}
	if len(diags) == 0 {
		h.t.Fatalf("Resolve returned neither a Store nor diagnostics")
	}
	return diags
}

// cycle runs one poll and fan-out cycle on the test goroutine.
func (h *harness) cycle() { h.r.cycle(context.Background()) }

// get returns the revealed value of ref in st.
func get(t *testing.T, st secret.Store, ref secret.Ref) string {
	t.Helper()
	v, ok := st.Get(ref)
	if !ok {
		t.Fatalf("Get(%s): not held", ref)
	}
	return string(v.Reveal())
}

// diagText renders diagnostics in the text form.
func diagText(l diag.List) string {
	var b bytes.Buffer
	_ = diag.WriteText(&b, l)
	return b.String()
}

// fileRef and envRef build references.
func fileRef(name, key string) secret.Ref {
	return secret.Ref{Provider: providerFile, Name: name, Key: key}
}

func envRef(name string) secret.Ref { return secret.Ref{Provider: providerEnv, Name: name} }

// use builds a Use of ref at a fixed resource and path.
func use(ref secret.Ref, kind secret.Kind) secret.Use {
	return secret.Use{
		Ref:      ref,
		Resource: diag.ResourceID{Kind: "Gateway", Name: "edge"},
		Path:     diag.Path{diag.Field("spec"), diag.Field("stateStore"), diag.Field("url")},
		Kind:     kind,
	}
}

// pemFixtures holds generated PEM material.
type pemFixtures struct {
	cert, key, crl string
}

// newPEM generates a self-signed ECDSA P-256 CA certificate, its SEC 1
// key and a CRL it signs.
func newPEM(t testing.TB) pemFixtures {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ruralz test CA"},
		NotBefore:             epoch().Add(-time.Hour),
		NotAfter:              epoch().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		SubjectKeyId:          []byte{1, 2, 3, 4},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	crl, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:     big.NewInt(1),
		ThisUpdate: epoch().Add(-time.Hour),
		NextUpdate: epoch().Add(time.Hour),
	}, ca, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pemFixtures{
		cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		key:  string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		crl:  string(pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: crl})),
	}
}
