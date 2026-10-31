// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mtls

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Fixed reasons a value is refused; none repeats any part of a value.
var (
	errNoCA      = errors.New("auth.mtls: config.caCertificate holds no PEM certificate")
	errBadCA     = errors.New("auth.mtls: a certificate of config.caCertificate does not parse")
	errNoCRL     = errors.New("auth.mtls: config.crl holds no PEM X509 CRL")
	errBadCRL    = errors.New("auth.mtls: a CRL of config.crl does not parse")
	errCRLIssuer = errors.New("auth.mtls: a CRL of config.crl is not signed by a certificate of config.caCertificate")
)

// PEM block types.
const (
	pemCertificate = "CERTIFICATE"
	pemCRL         = "X509 CRL"
)

// trustState is one immutable version of a Policy's trust material, built
// from one caCertificate value and one crl value.
type trustState struct {
	roots *x509.CertPool
	// crls holds, per CA (issuerKey of the bundle certificate that signed
	// them), the serials its CRLs revoke; nil without config.crl.
	crls map[[32]byte]*revocations
	// staleAt is the earliest, over CAs, of the latest nextUpdate among
	// the CA's CRLs: from then on the Node reports crl_stale. Zero when no
	// CRL has a nextUpdate.
	staleAt time.Time
}

// revocations are the serials one CA's CRLs revoke.
type revocations struct {
	serials map[string]struct{}
	// next is the latest nextUpdate of the CA's CRLs; zero when none has
	// one.
	next time.Time
}

// newTrustState parses a PEM CA bundle and, withCRL, PEM CRLs. Every
// CERTIFICATE block of the bundle must parse and one must be present;
// every X509 CRL block must parse, one must be present, and each CRL must
// be signed by a certificate of the bundle (Security and identity:
// config.crl holds CRLs for the Policy's CAs). Other PEM blocks are
// skipped.
func newTrustState(caPEM, crlPEM []byte, withCRL bool) (*trustState, error) {
	cas, err := parseBundle(caPEM)
	if err != nil {
		return nil, err
	}
	ts := &trustState{roots: x509.NewCertPool()}
	for _, c := range cas {
		ts.roots.AddCert(c)
	}
	if !withCRL {
		return ts, nil
	}
	lists, err := parseCRLs(crlPEM)
	if err != nil {
		return nil, err
	}
	ts.crls = make(map[[32]byte]*revocations, len(lists))
	for _, l := range lists {
		signer := signerOf(cas, l)
		if signer == nil {
			return nil, errCRLIssuer
		}
		k := issuerKey(signer)
		r := ts.crls[k]
		if r == nil {
			r = &revocations{serials: make(map[string]struct{}, len(l.RevokedCertificateEntries))}
			ts.crls[k] = r
		}
		for _, e := range l.RevokedCertificateEntries {
			r.serials[serialKey(e.SerialNumber)] = struct{}{}
		}
		if l.NextUpdate.After(r.next) {
			r.next = l.NextUpdate
		}
	}
	for _, r := range ts.crls {
		if !r.next.IsZero() && (ts.staleAt.IsZero() || r.next.Before(ts.staleAt)) {
			ts.staleAt = r.next
		}
	}
	return ts, nil
}

// parseBundle parses the CERTIFICATE blocks of a PEM CA bundle.
func parseBundle(b []byte) ([]*x509.Certificate, error) {
	var cas []*x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != pemCertificate {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, errBadCA
		}
		cas = append(cas, c)
	}
	if len(cas) == 0 {
		return nil, errNoCA
	}
	return cas, nil
}

// parseCRLs parses the X509 CRL blocks of a PEM value.
func parseCRLs(b []byte) ([]*x509.RevocationList, error) {
	var lists []*x509.RevocationList
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != pemCRL {
			continue
		}
		l, err := x509.ParseRevocationList(blk.Bytes)
		if err != nil {
			return nil, errBadCRL
		}
		lists = append(lists, l)
	}
	if len(lists) == 0 {
		return nil, errNoCRL
	}
	return lists, nil
}

// signerOf returns the certificate of cas that issued and signed l: its
// subject is l's issuer, it may sign CRLs and l's signature verifies with
// its key. nil when none did.
func signerOf(cas []*x509.Certificate, l *x509.RevocationList) *x509.Certificate {
	for _, c := range cas {
		if bytes.Equal(c.RawSubject, l.RawIssuer) && l.CheckSignatureFrom(c) == nil {
			return c
		}
	}
	return nil
}

// issuerKey identifies a CA by its subject and key, so a CRL a bundle
// certificate signed applies to every certificate the same CA issued,
// whichever copy of the CA a chain holds.
func issuerKey(c *x509.Certificate) [32]byte {
	h := sha256.New()
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(c.RawSubject))) //nolint:gosec // G115: a certificate subject is far below 4 GiB
	h.Write(n[:])
	h.Write(c.RawSubject)
	h.Write(c.RawSubjectPublicKeyInfo)
	var k [32]byte
	h.Sum(k[:0])
	return k
}

// serialKey is the map key of a serial number: its magnitude, prefixed
// with "-" when negative.
func serialKey(n *big.Int) string {
	if n == nil {
		return ""
	}
	if n.Sign() < 0 {
		return "-" + string(n.Bytes())
	}
	return string(n.Bytes())
}

// revoked reports whether a CRL of an issuer in chain lists the serial of
// the certificate that issuer signed. The last certificate, the anchor
// from config.caCertificate, is trusted as it is.
func (ts *trustState) revoked(chain []*x509.Certificate) bool {
	if len(ts.crls) == 0 {
		return false
	}
	for i := 0; i+1 < len(chain); i++ {
		r := ts.crls[issuerKey(chain[i+1])]
		if r == nil {
			continue
		}
		if _, ok := r.serials[serialKey(chain[i].SerialNumber)]; ok {
			return true
		}
	}
	return false
}

// half is one watched reference of a holder: config.caCertificate or
// config.crl. Its fields are guarded by the holder's mutex.
type half struct {
	ref  secret.Ref
	fn   func(secret.Value) error
	stop func()
	// failed is set while the reference's last callback refused its value
	// (the resolver then holds secret_rotation_failed for the watch).
	failed bool
	// accepted is the value the published trust state was built from.
	accepted secret.Value
}

// holder follows the rotations of one Policy's caCertificate and crl
// (Security and identity, "Certificate rotation") and publishes the trust
// state. The Filters of the Policy share it across snapshots through
// BuildEnv.Previous; the last one to close stops its watches.
//
// A rotated value is combined with the other reference's current Store
// value, else with the value in use: the two may change in either order,
// or in one poll. A combination that does not hold, a CRL no certificate
// of the bundle signed included, keeps the published state and fails the
// callback, which the resolver counts as a rotation failure. When a
// callback succeeds with the other reference's current value while that
// reference's last callback had failed, the other reference is watched
// anew, so the resolver drops the secret_rotation_failed source of a
// value that now forms a valid pair.
//
// While the published state holds a CRL past its nextUpdate, the holder
// raises crl_stale; the CRL stays enforced. A timer raises it when the
// time comes, and a rotation to fresh CRLs clears it.
type holder struct {
	secrets secret.Store
	clock   clock.Clock
	status  emit.NodeStatus
	// source is the crl_stale source, unique per holder.
	source string

	ca  *half
	crl *half // nil without config.crl

	cur atomic.Pointer[trustState]

	// mu guards the fields below and the halves. secret.Store never calls
	// a watch callback from Watch or Get, so mu is held across them.
	mu     sync.Mutex
	refs   int
	closed bool
	timer  clock.Timer
	stale  bool
}

// newHolder watches ca and crl (nil without config.crl), builds the
// trust state from their current values and holds one reference for the
// caller. An unresolved reference or a refused value is an error, and
// nothing keeps running.
func newHolder(secrets secret.Store, ca secret.Ref, crl *secret.Ref, clk clock.Clock, status emit.NodeStatus, policy string) (*holder, error) {
	h := &holder{secrets: secrets, clock: clk, status: status, refs: 1}
	h.source = fmt.Sprintf("auth.mtls %s#%p", policy, h)
	h.ca = &half{ref: ca}
	h.ca.fn = func(v secret.Value) error { return h.rotate(h.ca, h.crl, v) }
	if crl != nil {
		h.crl = &half{ref: *crl}
		h.crl.fn = func(v secret.Value) error { return h.rotate(h.crl, h.ca, v) }
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Watch first, then read: a rotation published in between reaches a
	// callback, which waits for mu and then combines the newer value.
	h.ca.stop = secrets.Watch(h.ca.ref, h.ca.fn)
	if h.crl != nil {
		h.crl.stop = secrets.Watch(h.crl.ref, h.crl.fn)
	}
	ts, err := h.initialLocked()
	if err != nil {
		h.closed = true
		h.stopLocked()
		return nil, err
	}
	h.publishLocked(ts)
	return h, nil
}

// initialLocked builds the first trust state from the Store's values.
func (h *holder) initialLocked() (*trustState, error) {
	caVal, ok := h.secrets.Get(h.ca.ref)
	if !ok {
		return nil, fmt.Errorf("auth.mtls: config.caCertificate %s is not resolved", h.ca.ref)
	}
	var crlVal secret.Value
	if h.crl != nil {
		if crlVal, ok = h.secrets.Get(h.crl.ref); !ok {
			return nil, fmt.Errorf("auth.mtls: config.crl %s is not resolved", h.crl.ref)
		}
	}
	ts, err := newTrustState(caVal.Reveal(), crlVal.Reveal(), h.crl != nil)
	if err != nil {
		return nil, err
	}
	h.ca.accepted = caVal
	if h.crl != nil {
		h.crl.accepted = crlVal
	}
	return ts, nil
}

// state returns the published trust state.
func (h *holder) state() *trustState { return h.cur.Load() }

// follows reports whether h watches ca and crl.
func (h *holder) follows(ca secret.Ref, crl *secret.Ref) bool {
	if h.ca.ref != ca || (h.crl == nil) != (crl == nil) {
		return false
	}
	return crl == nil || h.crl.ref == *crl
}

// rotate is the watch callback of self, which delivered v; other is the
// other half, nil without config.crl.
func (h *holder) rotate(self, other *half, v secret.Value) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	if other == nil {
		ts, err := newTrustState(v.Reveal(), nil, false)
		if err != nil {
			self.failed = true
			return err
		}
		self.failed, self.accepted = false, v
		h.publishLocked(ts)
		return nil
	}
	current, inStore := h.secrets.Get(other.ref)
	candidates := make([]secret.Value, 0, 2)
	if inStore {
		candidates = append(candidates, current)
	}
	if !inStore || !sameValue(current, other.accepted) {
		candidates = append(candidates, other.accepted)
	}
	var first error
	for i, o := range candidates {
		caVal, crlVal := v, o
		if self == h.crl {
			caVal, crlVal = o, v
		}
		ts, err := newTrustState(caVal.Reveal(), crlVal.Reveal(), true)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		self.failed, self.accepted = false, v
		other.accepted = o
		if other.failed && inStore && i == 0 {
			h.rewatchLocked(other, o)
		}
		h.publishLocked(ts)
		return nil
	}
	self.failed = true
	return first
}

// rewatchLocked registers s again after a callback of the other half
// formed a valid pair with used, s's current Store value, while s's last
// callback had failed. A new watch starts at the current version of its
// reference (secret.Store.Watch), so the old one is stopped, dropping its
// secret_rotation_failed source, only when the Store still holds used;
// when it moved meanwhile, the new watch is dropped and the old one
// examines the newer value on the next poll.
func (h *holder) rewatchLocked(s *half, used secret.Value) {
	renewed := h.secrets.Watch(s.ref, s.fn)
	if cur, ok := h.secrets.Get(s.ref); !ok || !sameValue(cur, used) {
		renewed()
		return
	}
	old := s.stop
	s.stop, s.failed = renewed, false
	if old != nil {
		old()
	}
}

// sameValue reports whether a and b hold the same bytes.
func sameValue(a, b secret.Value) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() == b.IsZero()
	}
	return a.Len() == b.Len() && bytes.Equal(a.Reveal(), b.Reveal())
}

// publishLocked makes ts the trust state and updates crl_stale.
func (h *holder) publishLocked(ts *trustState) {
	h.cur.Store(ts)
	h.evaluateLocked()
}

// evaluateLocked raises crl_stale while the published state is past its
// staleAt, clears it otherwise, and arms a timer for a staleAt to come.
func (h *holder) evaluateLocked() {
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	at := h.cur.Load().staleAt
	if at.IsZero() {
		h.setStaleLocked(false)
		return
	}
	now := h.clock.Now()
	if !now.Before(at) {
		h.setStaleLocked(true)
		return
	}
	h.setStaleLocked(false)
	h.timer = h.clock.AfterFunc(at.Sub(now), h.onTimer)
}

// onTimer re-evaluates crl_stale when a CRL reaches its nextUpdate; it
// does nothing once the holder is closed.
func (h *holder) onTimer() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.evaluateLocked()
}

// setStaleLocked raises or clears crl_stale for h's source.
func (h *holder) setStaleLocked(on bool) {
	if on == h.stale {
		return
	}
	h.stale = on
	if h.status != nil {
		h.status.SetDegraded(catalog.ReasonCRLStale, h.source, on)
	}
}

// retain takes a reference for a Filter carrying h over; false once h is
// closed.
func (h *holder) retain() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.refs++
	return true
}

// release drops a reference; the last one stops the watches and the timer
// and clears crl_stale.
func (h *holder) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	if h.refs--; h.refs > 0 {
		return
	}
	h.closed = true
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.setStaleLocked(false)
	h.stopLocked()
}

// stopLocked stops both watches.
func (h *holder) stopLocked() {
	for _, s := range []*half{h.ca, h.crl} {
		if s != nil && s.stop != nil {
			s.stop()
			s.stop = nil
		}
	}
}
