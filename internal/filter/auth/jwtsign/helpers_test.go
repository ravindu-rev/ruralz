// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// testLimit is the response body limit of the tests' Node.
const testLimit = 1 << 20

// start is the request start of the tests' exchanges.
func start() time.Time { return time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC) }

// expIn returns the NumericDate d after start, as JSON number text.
func expIn(d time.Duration) string {
	return big.NewInt(start().Add(d).Unix()).String()
}

// testKeys are the private keys of the tests, made once per test binary:
// two RSA keys of 2,048 bits and one of 1,024, P-256 and P-384 keys, and
// an Ed25519 key from a fixed seed.
type testKeys struct {
	rsa, rsa2, rsaSmall *rsa.PrivateKey
	p256, p384          *ecdsa.PrivateKey
	ed                  ed25519.PrivateKey
}

// generated holds the tests' keys; test-only package state.
var generated = sync.OnceValues(func() (*testKeys, error) {
	var k testKeys
	var err error
	if k.rsa, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		return nil, err
	}
	if k.rsa2, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		return nil, err
	}
	if k.rsaSmall, err = rsa.GenerateKey(rand.Reader, 1024); err != nil { //nolint:gosec // G403: a key under the 2,048-bit floor, which the key tests refuse
		return nil, err
	}
	if k.p256, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		return nil, err
	}
	if k.p384, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader); err != nil {
		return nil, err
	}
	k.ed = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	return &k, nil
})

// keys returns the tests' keys.
func keys(tb testing.TB) *testKeys {
	tb.Helper()
	k, err := generated()
	if err != nil {
		tb.Fatalf("generate keys: %v", err)
	}
	return k
}

// pemOf encodes der as one PEM block of type typ.
func pemOf(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// pkcs8 returns k as a PKCS #8 PEM private key.
func pkcs8(tb testing.TB, k any) []byte {
	tb.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		tb.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	return pemOf(pemPKCS8, der)
}

// sec1 returns k as a SEC 1 PEM private key.
func sec1(tb testing.TB, k *ecdsa.PrivateKey) []byte {
	tb.Helper()
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		tb.Fatalf("MarshalECPrivateKey: %v", err)
	}
	return pemOf(pemSEC1, der)
}

// spki returns pub as a PEM public key.
func spki(tb testing.TB, pub any) []byte {
	tb.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		tb.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	return pemOf(pemPublic, der)
}

// u returns the base64url of the big-endian bytes of n.
func u(n *big.Int) string { return base64.RawURLEncoding.EncodeToString(n.Bytes()) }

// b returns the base64url of raw.
func b(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

// jwk writes the members of a JWK as a JSON object, the members of extra
// added or replacing them; a nil extra value deletes the member.
func jwk(tb testing.TB, members, extra map[string]any) []byte {
	tb.Helper()
	m := map[string]any{}
	for k, v := range members {
		m[k] = v
	}
	for k, v := range extra {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	out, err := json.Marshal(m)
	if err != nil {
		tb.Fatalf("marshal JWK: %v", err)
	}
	return out
}

// rsaMembers returns the members of k's private JWK with its CRT values.
func rsaMembers(k *rsa.PrivateKey) map[string]any {
	return map[string]any{
		"kty": "RSA", "n": u(k.N), "e": u(big.NewInt(int64(k.E))), "d": u(k.D),
		"p": u(k.Primes[0]), "q": u(k.Primes[1]),
		"dp": u(k.Precomputed.Dp), "dq": u(k.Precomputed.Dq), "qi": u(k.Precomputed.Qinv),
	}
}

// ecMembers returns the members of k's private JWK.
func ecMembers(tb testing.TB, k *ecdsa.PrivateKey) map[string]any {
	tb.Helper()
	pub, err := k.PublicKey.Bytes()
	if err != nil {
		tb.Fatal(err)
	}
	d, err := k.Bytes()
	if err != nil {
		tb.Fatal(err)
	}
	return map[string]any{"kty": "EC", "crv": "P-256", "x": b(pub[1:33]), "y": b(pub[33:]), "d": b(d)}
}

// okpMembers returns the members of k's private JWK.
func okpMembers(k ed25519.PrivateKey) map[string]any {
	return map[string]any{"kty": "OKP", "crv": "Ed25519", "x": b(k.Public().(ed25519.PublicKey)), "d": b(k.Seed())}
}

// signingKeyRef is the secretRef of the tests' configs.
func signingKeyRef() v1alpha1.SecretRef {
	return v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/jwt/signing.key"}
}

// store is a resolved secret table with watches, as the resolver serves
// one: Get returns the latest value, and rotate publishes a value and
// calls the reference's watchers in order, returning their errors.
type store struct {
	mu       sync.Mutex
	vals     map[secret.Ref]secret.Value
	watchers map[secret.Ref][]*watch
}

// watch is one registration.
type watch struct {
	fn      func(secret.Value) error
	stopped bool
}

var _ secret.Store = (*store)(nil)

// newStore returns a table holding value for the tests' reference; a nil
// value holds none.
func newStore(value []byte) *store {
	s := &store{vals: map[secret.Ref]secret.Value{}, watchers: map[secret.Ref][]*watch{}}
	if value != nil {
		s.vals[secret.RefOf(signingKeyRef())] = secret.NewValue(value)
	}
	return s
}

// Get implements secret.Store.
func (s *store) Get(r secret.Ref) (secret.Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vals[r]
	return v, ok
}

// Watch implements secret.Store.
func (s *store) Watch(r secret.Ref, fn func(secret.Value) error) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := &watch{fn: fn}
	s.watchers[r] = append(s.watchers[r], w)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.stopped = true
	}
}

// live returns the number of watches of the tests' reference not stopped.
func (s *store) live() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, w := range s.watchers[secret.RefOf(signingKeyRef())] {
		if !w.stopped {
			n++
		}
	}
	return n
}

// rotate publishes value for the tests' reference and calls its watchers.
func (s *store) rotate(value []byte) error {
	r := secret.RefOf(signingKeyRef())
	s.mu.Lock()
	v := secret.NewValue(value)
	s.vals[r] = v
	ws := append([]*watch(nil), s.watchers[r]...)
	s.mu.Unlock()
	var errs []error
	for _, w := range ws {
		if !w.stopped {
			if err := w.fn(v); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// pathsConfig returns a paths config signed with alg under kid.
func pathsConfig(alg v1alpha1.JWTSignAlgorithm, kid string, paths ...string) *v1alpha1.AuthJWTSignConfig {
	return &v1alpha1.AuthJWTSignConfig{
		Paths: paths, Algorithm: alg, KeyID: kid,
		SigningKey: v1alpha1.SecretValue{SecretRef: signingKeyRef()},
	}
}

// claimsConfig returns a claims config writing at target, signed with alg.
func claimsConfig(alg v1alpha1.JWTSignAlgorithm, claims, target string) *v1alpha1.AuthJWTSignConfig {
	return &v1alpha1.AuthJWTSignConfig{
		Claims: claims, Target: target, Algorithm: alg,
		SigningKey: v1alpha1.SecretValue{SecretRef: signingKeyRef()},
	}
}

// buildEnv returns the BuildEnv of the Policy jwt-sign-test with config
// cfg at Route scope, compiling through a real CEL builder, its secrets
// from st.
func buildEnv(tb testing.TB, cfg *v1alpha1.AuthJWTSignConfig, st secret.Store) filter.BuildEnv {
	tb.Helper()
	caps := celtest.Caps([]*expr.Route{{Name: "orders"}}, &expr.Consumer{Name: "acme"})
	return filter.BuildEnv{
		Policy: &v1alpha1.Policy{
			Metadata: v1alpha1.ObjectMeta{Name: "jwt-sign-test"},
			Spec:     v1alpha1.PolicySpec{Type: Type},
		},
		Config: cfg,
		Scope:  phase.ScopeRoute,
		Phases: phase.Of(phase.OnResponse),
		CEL:    celtest.Compiler(tb).NewBuilder(nil, caps),
		Site: func(p expr.PlaceID) expr.Site {
			return expr.Site{Place: p, Scope: phase.ScopeRoute, PolicyType: Type}
		},
		Secrets: st,
		Limits:  filter.Limits{MaxRequestBodyBytes: testLimit, MaxResponseBodyBytes: testLimit, MaxBufferedBytes: 64 << 20},
	}
}

// build compiles cfg with st's secrets, or stops the test; the Filter is
// closed when the test ends.
func build(tb testing.TB, cfg *v1alpha1.AuthJWTSignConfig, st secret.Store) *Filter {
	tb.Helper()
	f, err := Factory{}.Build(context.Background(), buildEnv(tb, cfg, st))
	if err != nil {
		tb.Fatalf("Build: %v", err)
	}
	jf, ok := f.(*Filter)
	if !ok {
		tb.Fatalf("Build returned %T", f)
	}
	tb.Cleanup(func() { _ = jf.Close() })
	return jf
}

// jsonHeader returns a header map with Content-Type: application/json.
func jsonHeader() http.Header { return http.Header{"Content-Type": {"application/json"}} }

// decoder is the CEL compiler that decodes the tests' bodies, built once;
// test-only package state.
var decoder = sync.OnceValues(cel.New)

// bodyValue returns the CEL value of body under h, as the data plane
// decodes it: the decoded document for a JSON body, null otherwise.
func bodyValue(tb testing.TB, h http.Header, body string) expr.Value {
	tb.Helper()
	ct := h.Values("Content-Type")
	if body == "" || len(ct) != 1 || !jsonval.IsJSONMediaType(ct[0]) {
		return celtypes.Null()
	}
	c, err := decoder()
	if err != nil {
		tb.Fatalf("cel.New: %v", err)
	}
	v, _, err := c.DecodeJSON([]byte(body), 0)
	if err != nil {
		return celtypes.Null()
	}
	return v
}

// message is a filter.Message that counts its body writes and can fail
// them.
type message struct {
	filtertest.Message
	// bodyErr fails Body, setBodyErr SetBody and decodedErr Decoded.
	bodyErr, setBodyErr, decodedErr error
	// setBodies counts the SetBody calls.
	setBodies int
}

// Body fails with bodyErr when set.
func (m *message) Body(ctx context.Context) ([]byte, error) {
	if m.bodyErr != nil {
		return nil, m.bodyErr
	}
	return m.Message.Body(ctx)
}

// SetBody counts the write and fails with setBodyErr when set.
func (m *message) SetBody(ctx context.Context, b []byte) error {
	m.setBodies++
	if m.setBodyErr != nil {
		return m.setBodyErr
	}
	return m.Message.SetBody(ctx, b)
}

// Decoded fails with decodedErr when set.
func (m *message) Decoded(ctx context.Context) (expr.Value, error) {
	if m.decodedErr != nil {
		return nil, m.decodedErr
	}
	return m.Message.Decoded(ctx)
}

// onResponse returns the onResponse Exchange of a GET request whose
// response has status, the header map h and body, response.body decoded
// as the data plane decodes it, by Consumer acme authenticated with a JWT
// whose claims are {"sub":"alice","exp":<start + 1h>}.
func onResponse(tb testing.TB, status int, h http.Header, body string) (*filtertest.Exchange, *message) {
	tb.Helper()
	if h == nil {
		h = http.Header{}
	}
	m := &message{Message: filtertest.Message{Hdr: h, Raw: rawBody(body), Code: status}}
	v := &expr.Vars{
		Request:  &expr.Request{Method: http.MethodGet, Scheme: "https", Host: "api.shop.example", Path: "/v1/token", Header: http.Header{}},
		Source:   &expr.Source{},
		Route:    celtest.Route("orders", nil),
		Consumer: celtest.Consumer(expr.Consumer{Name: "acme", Tier: "gold"}),
		Auth:     &expr.Auth{Method: "jwt", Claims: celtest.JSON(tb, `{"sub":"alice","exp":`+expIn(time.Hour)+`}`)},
		Response: &expr.Response{Status: status, Header: h, Body: bodyValue(tb, h, body)},
		Now:      start(),
	}
	return &filtertest.Exchange{MethodV: http.MethodGet, PathV: "/v1/token", Start: start(), Msg: m, VarsV: v}, m
}

// rawBody returns the body bytes of s, nil for an empty body.
func rawBody(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

// errorType returns the error.type a Result's cause names, "" for none.
func errorType(res filter.Result) string {
	var typed interface{ ErrorType() string }
	if errors.As(res.Err, &typed) {
		return typed.ErrorType()
	}
	return ""
}

// wantNext fails unless res continues.
func wantNext(tb testing.TB, what string, res filter.Result) {
	tb.Helper()
	if res.Outcome != filter.Continue {
		tb.Fatalf("%s: Handle = %+v (error type %q), want Continue", what, res, errorType(res))
	}
}

// wantUndecided fails unless res cannot decide with the class default
// code and the error type kind.
func wantUndecided(tb testing.TB, what string, res filter.Result, kind string) {
	tb.Helper()
	if res.Outcome != filter.CannotDecide || res.Code != "" || errorType(res) != kind {
		tb.Fatalf("%s: Handle = %+v (error type %q), want CannotDecide with error type %q", what, res, errorType(res), kind)
	}
}

// parsed is a token split into its decoded parts.
type parsed struct {
	header  map[string]any
	payload []byte
	input   []byte
	sig     []byte
}

// splitToken decodes the three parts of a compact JWS, each base64url
// without padding.
func splitToken(tb testing.TB, tok string) parsed {
	tb.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		tb.Fatalf("token %q has %d parts, want 3", tok, len(parts))
	}
	dec := base64.RawURLEncoding.Strict()
	h, err := dec.DecodeString(parts[0])
	if err != nil {
		tb.Fatalf("header: %v", err)
	}
	var p parsed
	if err := json.Unmarshal(h, &p.header); err != nil {
		tb.Fatalf("header %s: %v", h, err)
	}
	if p.payload, err = dec.DecodeString(parts[1]); err != nil {
		tb.Fatalf("payload: %v", err)
	}
	if p.sig, err = dec.DecodeString(parts[2]); err != nil {
		tb.Fatalf("signature: %v", err)
	}
	p.input = []byte(parts[0] + "." + parts[1])
	return p
}

// verify checks tok's signature under alg with pub, the standard
// library's verifiers, and returns its parts.
func verify(tb testing.TB, tok string, alg v1alpha1.JWTSignAlgorithm, pub crypto.PublicKey) parsed {
	tb.Helper()
	p := splitToken(tb, tok)
	if got := p.header["alg"]; got != string(alg) {
		tb.Fatalf("alg header = %v, want %s", got, alg)
	}
	digest := sha256.Sum256(p.input)
	var err error
	switch alg {
	case v1alpha1.JWTSignAlgorithmRS256:
		err = rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA256, digest[:], p.sig)
	case v1alpha1.JWTSignAlgorithmPS256:
		err = rsa.VerifyPSS(pub.(*rsa.PublicKey), crypto.SHA256, digest[:], p.sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	case v1alpha1.JWTSignAlgorithmES256:
		if len(p.sig) != 64 {
			tb.Fatalf("ES256 signature of %d bytes, want 64", len(p.sig))
		}
		r, s := new(big.Int).SetBytes(p.sig[:32]), new(big.Int).SetBytes(p.sig[32:])
		if !ecdsa.Verify(pub.(*ecdsa.PublicKey), digest[:], r, s) {
			err = errors.New("ecdsa: verification failed")
		}
	case v1alpha1.JWTSignAlgorithmEdDSA:
		if !ed25519.Verify(pub.(ed25519.PublicKey), p.input, p.sig) {
			err = errors.New("ed25519: verification failed")
		}
	default:
		tb.Fatalf("algorithm %s", alg)
	}
	if err != nil {
		tb.Fatalf("signature of %s token does not verify: %v", alg, err)
	}
	return p
}

// tokenAt returns the string at member path of the JSON body, failing
// when it is no string.
func tokenAt(tb testing.TB, body []byte, path ...any) string {
	tb.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		tb.Fatalf("body %s: %v", body, err)
	}
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				tb.Fatalf("body %s: no object at %v", body, path)
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k >= len(a) {
				tb.Fatalf("body %s: no element at %v", body, path)
			}
			v = a[k]
		}
	}
	s, ok := v.(string)
	if !ok {
		tb.Fatalf("body %s: %v is %T, want a token string", body, path, v)
	}
	return s
}
