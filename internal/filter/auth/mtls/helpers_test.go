// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// t0 is the request time of the fixture; certificates are valid from a
// day before it for 30 days.
var t0 = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// Validity of the fixture's certificates.
var (
	notBefore = t0.Add(-24 * time.Hour)
	notAfter  = t0.Add(30 * 24 * time.Hour)
)

// policyName is the fixture Policy's name.
const policyName = "partner-mtls"

// References of the fixture's config.
var (
	caRef  = secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/mtls/ca.pem"}
	crlRef = secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/mtls/crl.pem"}
	keyRef = secret.Ref{Provider: v1alpha1.SecretProviderEnv, Name: "RURALZ_SECRET_PARTNER_KEY"}
)

// watch is one registration of store.
type watch struct {
	ref     secret.Ref
	fn      func(secret.Value) error
	stopped bool
	// failed models the resolver's secret_rotation_failed source: set
	// while the last delivery failed.
	failed bool
}

// store is a secret.Store over values a test publishes. Like the
// resolver, publish sets every value first and then delivers each changed
// reference to the watches registered before the publish, outside the
// store's lock; a watch registered during a delivery starts at the
// current value and is not called for it.
type store struct {
	mu      sync.Mutex
	values  map[secret.Ref][]byte
	watches []*watch
	// onWatch, when set, runs inside Watch after the registration (it may
	// publish values without delivering them, see move).
	onWatch func(secret.Ref)
}

func newStore(values map[secret.Ref][]byte) *store {
	s := &store{values: map[secret.Ref][]byte{}}
	for r, v := range values {
		s.values[r] = slices.Clone(v)
	}
	return s
}

// Get implements secret.Store.
func (s *store) Get(r secret.Ref) (secret.Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[r]
	if !ok {
		return secret.Value{}, false
	}
	return secret.NewValue(v), true
}

// Watch implements secret.Store.
func (s *store) Watch(r secret.Ref, fn func(secret.Value) error) func() {
	w := &watch{ref: r, fn: fn}
	s.mu.Lock()
	s.watches = append(s.watches, w)
	hook := s.onWatch
	s.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.stopped = true
		w.failed = false
	}
}

// publish sets the values of refs in one poll, then delivers them in the
// order given, and returns the callback errors.
func (s *store) publish(t *testing.T, refs []secret.Ref, values ...[]byte) []error {
	t.Helper()
	if len(refs) != len(values) {
		t.Fatal("publish: one value per reference")
	}
	s.mu.Lock()
	for i, r := range refs {
		s.values[r] = slices.Clone(values[i])
	}
	var due []*watch
	for _, r := range refs {
		for _, w := range s.watches {
			if w.ref == r && !w.stopped {
				due = append(due, w)
			}
		}
	}
	s.mu.Unlock()
	var errs []error
	for _, w := range due {
		s.mu.Lock()
		stopped := w.stopped
		v := secret.NewValue(s.values[w.ref])
		s.mu.Unlock()
		if stopped {
			continue
		}
		err := w.fn(v)
		s.mu.Lock()
		if !w.stopped {
			w.failed = err != nil
		}
		s.mu.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// move changes the value of r without delivering it, as a poll that has
// not reached the watchers yet.
func (s *store) move(r secret.Ref, v []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[r] = slices.Clone(v)
}

// live returns the number of live watches of r.
func (s *store) live(r secret.Ref) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, w := range s.watches {
		if w.ref == r && !w.stopped {
			n++
		}
	}
	return n
}

// failing returns the number of live watches whose last delivery failed.
func (s *store) failing() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, w := range s.watches {
		if !w.stopped && w.failed {
			n++
		}
	}
	return n
}

// status records crl_stale per source.
type status struct {
	t  *testing.T
	mu sync.Mutex
	on map[string]bool
	// changes counts the calls that changed a source.
	changes int
}

func newStatus(t *testing.T) *status { return &status{t: t, on: map[string]bool{}} }

// SetDegraded implements emit.NodeStatus.
func (s *status) SetDegraded(r catalog.Reason, source string, on bool) {
	if r != catalog.ReasonCRLStale {
		s.t.Errorf("SetDegraded(%v), want only %v", r, catalog.ReasonCRLStale)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.on[source] != on {
		s.changes++
	}
	if on {
		s.on[source] = true
	} else {
		delete(s.on, source)
	}
}

// SetCleartextHops implements emit.NodeStatus.
func (*status) SetCleartextHops(catalog.Hop, int) {}

// raised returns the number of sources holding crl_stale.
func (s *status) raised() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.on)
}

// decisions records ruralz_auth_decisions_total.
type decisions struct {
	mu    sync.Mutex
	allow int
	deny  []string
}

func (d *decisions) Allow(emit.Stripe) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allow++
}

func (d *decisions) Deny(_ emit.Stripe, code string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deny = append(d.deny, code)
}

// revoker revokes the certificates whose serials it holds.
type revoker struct {
	identity.NopRevoker
	mu      sync.Mutex
	serials map[string]bool
}

func (r *revoker) revoke(c *x509.Certificate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.serials == nil {
		r.serials = map[string]bool{}
	}
	r.serials[c.SerialNumber.String()] = true
}

// RevokedCert implements identity.Revoker.
func (r *revoker) RevokedCert(_ []byte, serial *big.Int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.serials[serial.String()]
}

// newCA returns a root CA valid over the fixture's validity.
func newCA(t testing.TB, cn string) *pki.CA {
	t.Helper()
	ca, err := pki.NewCA(pki.CAOptions{CommonName: cn, NotBefore: notBefore, NotAfter: notAfter.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// intermediate returns the issuing CA "Issuing", signed by parent.
func intermediate(t testing.TB, parent *pki.CA) *pki.CA {
	t.Helper()
	ca, err := parent.Intermediate(pki.CAOptions{CommonName: "Issuing", NotBefore: notBefore, NotAfter: notAfter.Add(12 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// client issues a clientAuth leaf "CN=<cn>,O=Ruralz tests" valid from nb
// to na (zero: the fixture's validity) with URI SANs uris.
func client(t testing.TB, ca *pki.CA, cn string, nb, na time.Time, uris ...string) *x509.Certificate {
	t.Helper()
	if nb.IsZero() {
		nb, na = notBefore, notAfter
	}
	l, err := ca.Issue(pki.LeafOptions{CommonName: cn, URIs: uris, Client: true, NotBefore: nb, NotAfter: na})
	if err != nil {
		t.Fatal(err)
	}
	return l.Cert
}

// custom signs tmpl with ca, filling serial, key and validity when unset.
func custom(t testing.TB, ca *pki.CA, tmpl *x509.Certificate) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.SerialNumber == nil {
		// The value matters to no test, only its uniqueness for CRLs.
		if tmpl.SerialNumber, err = rand.Int(rand.Reader, big.NewInt(1<<62)); err != nil {
			t.Fatal(err)
		}
	}
	if tmpl.NotBefore.IsZero() {
		tmpl.NotBefore, tmpl.NotAfter = notBefore, notAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// crl returns a PEM CRL of ca revoking certs, issued a day before t0 and
// due at next.
func crl(t testing.TB, ca *pki.CA, next time.Time, certs ...*x509.Certificate) []byte {
	t.Helper()
	b, err := ca.CRL(certs, t0.Add(-24*time.Hour), next)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// bundle concatenates the PEM certificates of cas.
func bundle(cas ...*pki.CA) []byte {
	var b []byte
	for _, ca := range cas {
		b = append(b, ca.CertPEM...)
	}
	return b
}

// config returns an auth.mtls config over caRef, and crlRef when withCRL,
// with the subject rules rules.
func config(withCRL bool, rules ...v1alpha1.CertificateSubject) *v1alpha1.AuthMTLSConfig {
	cfg := &v1alpha1.AuthMTLSConfig{
		CACertificate: v1alpha1.SecretValue{SecretRef: v1alpha1.SecretRef{Provider: caRef.Provider, Name: caRef.Name}},
		Subjects:      rules,
	}
	if withCRL {
		cfg.CRL = &v1alpha1.SecretValue{SecretRef: v1alpha1.SecretRef{Provider: crlRef.Provider, Name: crlRef.Name}}
	}
	return cfg
}

// fixture is one Node: its secrets, status, clock and decision counter.
type fixture struct {
	t       *testing.T
	store   *store
	status  *status
	clock   *clocktest.Fake
	rec     *decisions
	revoker *revoker
}

func newFixture(t *testing.T, values map[secret.Ref][]byte) *fixture {
	return &fixture{t: t, store: newStore(values), status: newStatus(t), clock: clocktest.New(t0), rec: &decisions{}, revoker: &revoker{}}
}

// env returns the BuildEnv of cfg with the Consumers cs.
func (fx *fixture) env(cfg any, cs ...*v1alpha1.Consumer) filter.BuildEnv {
	rs := make([]*hub.Resource, 0, len(cs))
	for _, c := range cs {
		rs = append(rs, &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindConsumer, Name: c.Metadata.Name}, Object: c})
	}
	return filter.BuildEnv{
		Policy:    &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: policyName}},
		Config:    cfg,
		Bundle:    hub.NewBundle(rs),
		Consumers: identity.CompileConsumers(cs).Map(),
		Secrets:   fx.store,
		Metrics:   &emit.PolicyMetrics{Auth: fx.rec},
		Status:    fx.status,
		Clock:     fx.clock,
	}
}

// build compiles cfg for the Consumers cs and closes the Filter at the
// end of the test.
func (fx *fixture) build(cfg any, cs ...*v1alpha1.Consumer) *Filter {
	fx.t.Helper()
	return fx.buildEnv(fx.env(cfg, cs...))
}

// buildEnv compiles env and closes the Filter at the end of the test.
func (fx *fixture) buildEnv(env filter.BuildEnv) *Filter {
	fx.t.Helper()
	built, err := NewFactory(fx.revoker).Build(fx.t.Context(), env)
	if err != nil {
		fx.t.Fatalf("Build: %v", err)
	}
	f := built.(*Filter)
	fx.t.Cleanup(func() { _ = f.Close() })
	return f
}

// conn returns the TLS state of a connection that requested a client
// certificate and got certs.
func conn(certs ...*x509.Certificate) *filter.ConnTLS {
	return &filter.ConnTLS{
		State:               &tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: certs},
		ClientCertRequested: true,
		Cache:               &filter.ConnCache{},
	}
}

// request returns a request at now over c.
func request(c *filter.ConnTLS, now time.Time) *filtertest.Exchange {
	return &filtertest.Exchange{Start: now, TLSV: c}
}

// consumer returns a Consumer bound by credentials.certificates certs.
func consumer(name string, certs ...v1alpha1.CertificateCredential) *v1alpha1.Consumer {
	return &v1alpha1.Consumer{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.ConsumerSpec{Credentials: v1alpha1.Credentials{Certificates: certs}},
	}
}

// subjectOf is the RFC 4514 subject of a fixture leaf named cn.
func subjectOf(cn string) string { return "CN=" + cn + ",O=Ruralz tests" }
