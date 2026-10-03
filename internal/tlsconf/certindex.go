// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tlsconf

import (
	"bytes"
	"cmp"
	"crypto/tls"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

// CertSpec is one entry of a listener's tls.certificates: its name and the
// references of its PEM certificate chain and private key (converted from
// v1alpha1.Certificate with secret.RefOf by the snapshot compiler).
type CertSpec struct {
	// Name identifies the certificate; entries are ordered by it.
	Name string
	// Certificate references the PEM certificate chain, leaf first.
	Certificate secret.Ref
	// PrivateKey references the PEM private key.
	PrivateKey secret.Ref
}

// CertIndex serves a listener's certificates by SNI (spec 06 requirement
// 75): the leaf's DNS SANs matched exactly (lowercased), then one-label
// wildcards "*.suffix"; no SNI or no match gives the first certificate in
// name order. Lookups are lock-free (one atomic load) and run per
// handshake. The index follows rotations of the referenced secrets
// through secret.Store.Watch (requirement 76): a rotated certificate or key
// is paired with the other half's current value (see rotatePair), and a
// pair that parses and matches replaces the entry for the following
// handshakes; a value that does not parse or whose key does not match
// keeps the last pair and returns an error from the watch callback, which
// the resolver counts in ruralz_config_secret_rotation_failures_total. A
// half whose callback failed, such as a certificate renewed in an earlier
// poll than its key, is registered again once the other half's callback
// forms a valid pair with that half's current Store value (rewatch), so
// the resolver's secret_rotation_failed reason clears with the pair
// instead of at the next renewal. The owner (the snapshot holding the
// index) calls Close when it retires.
type CertIndex struct {
	table   atomic.Pointer[certTable]
	secrets secret.Store // read by rotations for the other half of a pair

	mu      sync.Mutex
	entries []*certEntry // name order
	ready   bool         // initial values parsed
	closed  bool
	stops   []func() // entry i's certificate at 2i, its key at 2i+1
}

// certEntry is one certificate: the latest value known of each half
// (initial, delivered by a rotation or read for a valid pair) and its last
// valid pair. Guarded by CertIndex.mu.
type certEntry struct {
	spec            CertSpec
	certVal, keyVal secret.Value
	certSeen        bool // a rotation delivered certVal during setup
	keySeen         bool
	pair            *tls.Certificate
	cert, key       half // the watches of the two halves
}

// half is the watch of one half of a certificate and key pair (a CertIndex
// entry, a ClientConfig client pair): its reference, its callback, the
// index of its stop in the owner's stops and whether its last callback
// failed. ref, fn and stop are set before the watch is registered and
// never change; failed is guarded by the owner's mutex.
type half struct {
	ref    secret.Ref
	fn     func(secret.Value) error
	stop   int
	failed bool
}

// renewable marks sibling, the other half of a pair that a callback just
// formed, as no longer failed and returns it for rewatch when its last
// callback failed and the pair used its current Store value (current, see
// rotatePair); otherwise it returns nil. A pair formed with the sibling's
// last known value instead leaves the sibling's watch, and its raised
// reason, in place: the Store may hold a value of the sibling that its
// callback has not examined yet, such as a newer key published in the same
// poll as a certificate that pairs with the previous key, and a new watch
// would start at that value's version and skip it (secret.Store.Watch), so
// a value that does not match would be neither counted nor raised (spec 06
// requirement 76). The sibling's own callback then examines it. Callers
// hold the owner's mutex.
func renewable(sibling *half, current bool) *half {
	if !sibling.failed || !current {
		return nil
	}
	sibling.failed = false
	return sibling
}

// rewatch registers h again after the other half's callback formed a
// valid pair with h's current Store value while h's last callback had
// failed (renewable; spec 06 requirement 76). The resolver keeps the
// secret_rotation_failed source of a failed watcher raised until that
// watcher accepts a later value of its own reference, which for a
// certificate renewed in an earlier poll than its key is the next renewal.
// A stopped watch drops its source, and a new watch with the same callback
// starts at the current value, which the pair already holds
// (secret.Store.Watch); the counted failure stays counted. mu guards
// *stops and *closed and is not held while rewatch calls the Store; a
// Close meanwhile stops the new registration.
func rewatch(mu *sync.Mutex, closed *bool, stops *[]func(), secrets secret.Store, h *half) {
	mu.Lock()
	if *closed {
		mu.Unlock()
		return
	}
	stale := (*stops)[h.stop]
	(*stops)[h.stop] = nil
	mu.Unlock()
	if stale != nil {
		stale()
	}
	renewed := secrets.Watch(h.ref, h.fn)
	mu.Lock()
	if !*closed {
		(*stops)[h.stop], renewed = renewed, nil
	}
	mu.Unlock()
	if renewed != nil {
		renewed()
	}
}

// certTable is the immutable lookup structure published per change.
type certTable struct {
	exact    map[string]*tls.Certificate
	wildcard map[string]*tls.Certificate
	def      *tls.Certificate
}

// BuildCertIndex resolves every certificate from secrets and indexes it.
// An empty list or a duplicate name is RZ-CFG-005; an unresolved
// reference, a value that does not parse, a key that does not match its
// certificate or a key of a refused type is RZ-CFG-026 (all failures
// reported, no secret bytes in the messages).
func BuildCertIndex(certs []CertSpec, secrets secret.Store) (*CertIndex, error) {
	if len(certs) == 0 {
		return nil, errcode.Errorf(codeSchema, "an https listener needs at least one tls.certificates entry")
	}
	if secrets == nil {
		return nil, errcode.Errorf(codeResolution, "tls.certificates: no resolved secrets")
	}
	specs := slices.Clone(certs)
	slices.SortStableFunc(specs, func(a, b CertSpec) int { return cmp.Compare(a.Name, b.Name) })
	for i := 1; i < len(specs); i++ {
		if specs[i].Name == specs[i-1].Name {
			return nil, errcode.Errorf(codeSchema, "tls.certificates: duplicate name %q", specs[i].Name)
		}
	}
	ix := &CertIndex{secrets: secrets, entries: make([]*certEntry, len(specs))}
	for i, s := range specs {
		ix.entries[i] = &certEntry{spec: s}
	}
	// Watches are registered, and initial values read, without holding
	// ix.mu, so a resolver that calls watchers under its own lock cannot
	// deadlock with the build. A rotation delivered meanwhile wins over the
	// value read by Get, and every later rotation fires again.
	stops := make([]func(), 0, 2*len(specs))
	for i, e := range ix.entries {
		e.cert = half{ref: e.spec.Certificate, stop: 2 * i, fn: func(v secret.Value) error { return ix.rotate(e, true, v) }}
		e.key = half{ref: e.spec.PrivateKey, stop: 2*i + 1, fn: func(v secret.Value) error { return ix.rotate(e, false, v) }}
		stops = append(stops, secrets.Watch(e.cert.ref, e.cert.fn), secrets.Watch(e.key.ref, e.key.fn))
	}
	type initial struct {
		cert, key     secret.Value
		certOK, keyOK bool
	}
	first := make([]initial, len(ix.entries))
	for i, e := range ix.entries {
		first[i].cert, first[i].certOK = secrets.Get(e.spec.Certificate)
		first[i].key, first[i].keyOK = secrets.Get(e.spec.PrivateKey)
	}

	ix.mu.Lock()
	ix.stops = stops
	var errs []error
	for i, e := range ix.entries {
		resolved := true
		if !e.certSeen {
			if first[i].certOK {
				e.certVal = first[i].cert
			} else {
				errs = append(errs, fmt.Errorf("tls certificate %q: certificate %s is not resolved", e.spec.Name, e.spec.Certificate))
				resolved = false
			}
		}
		if !e.keySeen {
			if first[i].keyOK {
				e.keyVal = first[i].key
			} else {
				errs = append(errs, fmt.Errorf("tls certificate %q: privateKey %s is not resolved", e.spec.Name, e.spec.PrivateKey))
				resolved = false
			}
		}
		if !resolved {
			continue
		}
		pair, err := parseKeyPair(e.certVal.Reveal(), e.keyVal.Reveal())
		if err != nil {
			errs = append(errs, fmt.Errorf("tls certificate %q: %w", e.spec.Name, err))
			continue
		}
		e.pair = pair
	}
	if len(errs) > 0 {
		ix.closed = true
		ix.stops = nil
		ix.mu.Unlock()
		stopAll(stops)
		return nil, errcode.Wrap(codeResolution, errors.Join(errs...))
	}
	ix.ready = true
	ix.publishLocked()
	ix.mu.Unlock()
	return ix, nil
}

// rotate applies a rotated certificate (isCert) or private key value v
// (spec 06 requirement 76): v is paired by rotatePair, and only a value
// that pairs with neither known value of the other half is a failure. A
// valid pair formed with the other half's current Store value registers
// that half again when its last callback failed (renewable, rewatch).
func (ix *CertIndex) rotate(e *certEntry, isCert bool, v secret.Value) error {
	ix.mu.Lock()
	renew, err := ix.rotateLocked(e, isCert, v)
	ix.mu.Unlock()
	if renew != nil {
		rewatch(&ix.mu, &ix.closed, &ix.stops, ix.secrets, renew)
	}
	return err
}

// rotateLocked records v and publishes the pair it forms. It returns the
// other half for rotate to register again when renewable selects it.
// Callers hold ix.mu.
func (ix *CertIndex) rotateLocked(e *certEntry, isCert bool, v secret.Value) (*half, error) {
	if ix.closed {
		return nil, nil
	}
	self, sibling := &e.cert, &e.key
	if isCert {
		e.certVal, e.certSeen = v, true
	} else {
		e.keyVal, e.keySeen = v, true
		self, sibling = &e.key, &e.cert
	}
	if !ix.ready {
		return nil, nil // the build parses it
	}
	other, last := e.spec.PrivateKey, e.keyVal
	if !isCert {
		other, last = e.spec.Certificate, e.certVal
	}
	pair, used, current, err := rotatePair(ix.secrets, v, isCert, other, last)
	if err != nil {
		self.failed = true
		return nil, fmt.Errorf("tls certificate %q: %w", e.spec.Name, err)
	}
	self.failed = false
	if isCert {
		e.keyVal = used
	} else {
		e.certVal = used
	}
	e.pair = pair
	ix.publishLocked()
	return renewable(sibling, current), nil
}

// rotatePair parses the rotated half v of a certificate and key pair
// (vIsCert says which half) with the other half, whose reference is other
// and whose latest value known to the caller is last. The other half's
// current value in secrets comes first: the resolver updates every
// reference changed in a poll before it calls any watcher, so a
// certificate and key renewed together, or one PEM file used as both, pair
// up whichever watcher runs first. When that value does not pair (or
// secrets no longer holds other), last is tried: a Store kept by a
// carried-over owner can hold an older value than the one its watches
// follow. It returns the pair, the other half's value used and whether
// that value is the one secrets holds now (current; false when last was
// used); on failure the error of the first pairing tried, which never
// repeats secret bytes.
func rotatePair(secrets secret.Store, v secret.Value, vIsCert bool, other secret.Ref, last secret.Value) (*tls.Certificate, secret.Value, bool, error) {
	parse := func(o secret.Value) (*tls.Certificate, error) {
		if vIsCert {
			return parseKeyPair(v.Reveal(), o.Reveal())
		}
		return parseKeyPair(o.Reveal(), v.Reveal())
	}
	var firstErr error
	if secrets != nil {
		if cur, ok := secrets.Get(other); ok {
			pair, err := parse(cur)
			if err == nil {
				return pair, cur, true, nil
			}
			if sameValue(cur, last) {
				return nil, secret.Value{}, false, err
			}
			firstErr = err
		}
	}
	pair, err := parse(last)
	if err != nil {
		if firstErr != nil {
			err = firstErr
		}
		return nil, secret.Value{}, false, err
	}
	return pair, last, false, nil
}

// sameValue reports whether a and b hold the same bytes; the copies it
// compares are cleared.
func sameValue(a, b secret.Value) bool {
	if a.Len() != b.Len() || a.IsZero() != b.IsZero() {
		return false
	}
	x, y := a.Reveal(), b.Reveal()
	same := bytes.Equal(x, y)
	clear(x)
	clear(y)
	return same
}

// publishLocked rebuilds the lookup table from the entries' valid pairs
// (every entry has one once the build succeeded), first name wins on a
// shared SAN. Callers hold ix.mu.
func (ix *CertIndex) publishLocked() {
	t := &certTable{
		exact:    make(map[string]*tls.Certificate),
		wildcard: make(map[string]*tls.Certificate),
	}
	for _, e := range ix.entries {
		if t.def == nil {
			t.def = e.pair
		}
		for _, san := range e.pair.Leaf.DNSNames {
			name := strings.ToLower(strings.TrimSuffix(san, "."))
			if suffix, ok := strings.CutPrefix(name, "*."); ok {
				if suffix != "" && !strings.Contains(suffix, "*") {
					if _, dup := t.wildcard[suffix]; !dup {
						t.wildcard[suffix] = e.pair
					}
				}
				continue
			}
			if name == "" || strings.Contains(name, "*") {
				continue
			}
			if _, dup := t.exact[name]; !dup {
				t.exact[name] = e.pair
			}
		}
	}
	ix.table.Store(t)
}

// Lookup returns the certificate served for serverName (the SNI, possibly
// empty); nil only for an index without certificates.
func (ix *CertIndex) Lookup(serverName string) *tls.Certificate {
	t := ix.table.Load()
	if t == nil {
		return nil
	}
	name := normalizeSNI(serverName)
	if name != "" {
		if c, ok := t.exact[name]; ok {
			return c
		}
		if i := strings.IndexByte(name, '.'); i > 0 {
			if c, ok := t.wildcard[name[i+1:]]; ok {
				return c
			}
		}
	}
	return t.def
}

// GetCertificate is the tls.Config.GetCertificate of the listener's
// per-state configuration.
func (ix *CertIndex) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := ix.Lookup(hello.ServerName); c != nil {
		return c, nil
	}
	return nil, errNoCertificates
}

// errNoCertificates fails a handshake on an index without certificates.
var errNoCertificates = errors.New("tlsconf: listener has no certificate")

// Close stops following rotations; lookups keep serving the last values,
// so handshakes on a retiring snapshot still complete. It is idempotent.
func (ix *CertIndex) Close() {
	ix.mu.Lock()
	if ix.closed {
		ix.mu.Unlock()
		return
	}
	ix.closed = true
	stops := ix.stops
	ix.stops = nil
	ix.mu.Unlock()
	stopAll(stops)
}

// normalizeSNI lowercases an SNI and drops one trailing dot, allocating
// only when the name has upper-case letters.
func normalizeSNI(s string) string {
	s = strings.TrimSuffix(s, ".")
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			return strings.ToLower(s)
		}
	}
	return s
}

// stopAll unregisters watches.
func stopAll(stops []func()) {
	for _, stop := range stops {
		if stop != nil {
			stop()
		}
	}
}
