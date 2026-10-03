// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

// ClientSpec is the client TLS setting of an Upstream (Upstream.spec.tls)
// or of the OTLP exporter (telemetry.otlp.tls, the same shape,
// OQ-observability-2 (a)), with references converted by secret.RefOf.
type ClientSpec struct {
	// SNI is sent and verified instead of the dialed host when set.
	SNI string
	// CACertificate, when set, replaces the system roots.
	CACertificate *secret.Ref
	// ClientCertificate and ClientKey are the client certificate chain and
	// key, both or neither.
	ClientCertificate *secret.Ref
	// ClientKey is the client private key.
	ClientKey *secret.Ref
}

// ClientConfig is a client configuration that follows rotations of its
// referenced secrets (spec 06 requirement 79): the transport's
// DialTLSContext calls ForHost (or Current) per new connection, so rotated
// values reach new connections only. A CA bundle rotation publishes a new
// configuration through an atomic pointer; a client certificate rotation
// is served by GetClientCertificate, so even a consumer that keeps one
// configuration (gRPC) presents the current certificate. A rotated client
// certificate or key is paired with the other half's current value, as in
// CertIndex, so a pair renewed together is never a failure. A value that
// does not parse, or whose key matches neither known value of the other
// half, keeps the last one and fails its watch callback, which the
// resolver counts as a rotation failure; once the other half's callback
// forms a valid pair with the failed half's current Store value, the
// failed half is registered again (renewable, rewatch), so
// secret_rotation_failed clears with the pair. The owner (the Upstream
// runtime or the telemetry runtime) calls Close when it drops the
// configuration.
type ClientConfig struct {
	cur     atomic.Pointer[tls.Config]
	pair    atomic.Pointer[tls.Certificate]
	secrets secret.Store // read by rotations for the other half of the pair

	mu                     sync.Mutex
	spec                   ClientSpec
	caVal, certVal, keyVal secret.Value // latest values known
	caSeen, certSeen       bool         // delivered by a rotation during setup
	keySeen                bool
	ready, closed          bool
	stops                  []func()
	cert, key              half // the watches of the client pair's halves
}

// Upstream builds the TLS configuration of an Upstream with a tls block
// (spec 06 requirement 79): TLS 1.2 or newer with the TLS12Suites, server
// always verified against CACertificate or the system roots, ServerName
// from SNI or the Endpoint host (ForHost), and the optional client
// certificate. ClientCertificate without ClientKey (or the reverse) is
// RZ-CFG-005; an unresolved or unparsable value is RZ-CFG-026. A
// handshake or verification failure is the Upstream layer's RZ-UP-002.
func Upstream(spec ClientSpec, secrets secret.Store) (*ClientConfig, error) {
	return newClientConfig(spec, secrets)
}

// OTLP builds the TLS configuration of an https OTLP endpoint (spec 06
// requirement 81): with a nil spec, TLS 1.2 or newer against the system
// roots; with telemetry.otlp.tls, the same rules as Upstream.
func OTLP(spec *ClientSpec, secrets secret.Store) (*ClientConfig, error) {
	if spec == nil {
		return newClientConfig(ClientSpec{}, secrets)
	}
	return newClientConfig(*spec, secrets)
}

func newClientConfig(spec ClientSpec, secrets secret.Store) (*ClientConfig, error) {
	if (spec.ClientCertificate == nil) != (spec.ClientKey == nil) {
		return nil, errcode.Errorf(codeSchema, "tls.clientCertificate and tls.clientKey must be set together")
	}
	c := &ClientConfig{spec: spec, secrets: secrets}
	refs := spec.CACertificate != nil || spec.ClientCertificate != nil
	if refs && secrets == nil {
		return nil, errcode.Errorf(codeResolution, "tls: no resolved secrets")
	}
	// As in BuildCertIndex, watches and initial reads happen without c.mu.
	var stops []func()
	if spec.CACertificate != nil {
		stops = append(stops, secrets.Watch(*spec.CACertificate, func(v secret.Value) error { return c.rotate(slotCA, v) }))
	}
	if spec.ClientCertificate != nil {
		c.cert = half{ref: *spec.ClientCertificate, stop: len(stops), fn: func(v secret.Value) error { return c.rotate(slotCert, v) }}
		c.key = half{ref: *spec.ClientKey, stop: len(stops) + 1, fn: func(v secret.Value) error { return c.rotate(slotKey, v) }}
		stops = append(stops, secrets.Watch(c.cert.ref, c.cert.fn), secrets.Watch(c.key.ref, c.key.fn))
	}
	var ca, cert, key secret.Value
	var caOK, certOK, keyOK bool
	if spec.CACertificate != nil {
		ca, caOK = secrets.Get(*spec.CACertificate)
	}
	if spec.ClientCertificate != nil {
		cert, certOK = secrets.Get(*spec.ClientCertificate)
		key, keyOK = secrets.Get(*spec.ClientKey)
	}

	c.mu.Lock()
	c.stops = stops
	var errs []error
	if spec.CACertificate != nil {
		if !c.caSeen {
			if caOK {
				c.caVal = ca
			} else {
				errs = append(errs, fmt.Errorf("tls.caCertificate %s is not resolved", spec.CACertificate))
			}
		}
	}
	if spec.ClientCertificate != nil {
		if !c.certSeen {
			if certOK {
				c.certVal = cert
			} else {
				errs = append(errs, fmt.Errorf("tls.clientCertificate %s is not resolved", spec.ClientCertificate))
			}
		}
		if !c.keySeen {
			if keyOK {
				c.keyVal = key
			} else {
				errs = append(errs, fmt.Errorf("tls.clientKey %s is not resolved", spec.ClientKey))
			}
		}
	}
	if len(errs) == 0 {
		if err := c.buildLocked(slotCA); err != nil {
			errs = append(errs, err)
		}
		if err := c.buildLocked(slotCert); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		c.closed = true
		c.stops = nil
		c.mu.Unlock()
		stopAll(stops)
		return nil, errcode.Wrap(codeResolution, errors.Join(errs...))
	}
	c.ready = true
	c.mu.Unlock()
	return c, nil
}

// slot names the rotating part of a ClientConfig.
type slot uint8

const (
	slotCA slot = iota
	slotCert
	slotKey
)

// rotate records a rotated value and rebuilds the part it belongs to. A
// valid client pair formed with the other half's current Store value
// registers that half again when its last callback failed (renewable,
// rewatch).
func (c *ClientConfig) rotate(s slot, v secret.Value) error {
	c.mu.Lock()
	renew, err := c.rotateLocked(s, v)
	c.mu.Unlock()
	if renew != nil {
		rewatch(&c.mu, &c.closed, &c.stops, c.secrets, renew)
	}
	return err
}

// rotateLocked records v and rebuilds its part; for the client pair it
// returns the half to register again (rotatePairLocked). Callers hold
// c.mu.
func (c *ClientConfig) rotateLocked(s slot, v secret.Value) (*half, error) {
	if c.closed {
		return nil, nil
	}
	switch s {
	case slotCA:
		c.caVal, c.caSeen = v, true
	case slotCert:
		c.certVal, c.certSeen = v, true
	case slotKey:
		c.keyVal, c.keySeen = v, true
	}
	if !c.ready {
		return nil, nil // the build parses it
	}
	if s == slotCA {
		return nil, c.buildLocked(slotCA)
	}
	return c.rotatePairLocked(s == slotCert, v)
}

// rotatePairLocked pairs a rotated client certificate (isCert) or key v
// with the other half (rotatePair) and publishes the pair. On error the
// published pair is unchanged. It returns the other half to register
// again when renewable selects it. Callers hold c.mu.
func (c *ClientConfig) rotatePairLocked(isCert bool, v secret.Value) (*half, error) {
	self, sibling := &c.cert, &c.key
	other, last := *c.spec.ClientKey, c.keyVal
	if !isCert {
		self, sibling = &c.key, &c.cert
		other, last = *c.spec.ClientCertificate, c.certVal
	}
	pair, used, current, err := rotatePair(c.secrets, v, isCert, other, last)
	if err != nil {
		self.failed = true
		return nil, fmt.Errorf("tls.clientCertificate: %w", err)
	}
	self.failed = false
	if isCert {
		c.keyVal = used
	} else {
		c.certVal = used
	}
	c.pair.Store(pair)
	return renewable(sibling, current), nil
}

// buildLocked parses the CA bundle (slotCA) and publishes a configuration,
// or parses the client pair from the latest values (slotCert, slotKey;
// the initial build) and publishes it. On error the published values are
// unchanged. Callers hold c.mu.
func (c *ClientConfig) buildLocked(s slot) error {
	switch s {
	case slotCA:
		var roots *x509.CertPool
		if c.spec.CACertificate != nil {
			pool, err := parseCertPool(c.caVal.Reveal())
			if err != nil {
				return fmt.Errorf("tls.caCertificate: %w", err)
			}
			roots = pool
		}
		cfg := Client(roots)
		cfg.ServerName = c.spec.SNI
		if c.spec.ClientCertificate != nil {
			cfg.GetClientCertificate = c.clientCertificate
		}
		c.cur.Store(cfg)
		return nil
	case slotCert, slotKey:
		if c.spec.ClientCertificate == nil {
			return nil
		}
		pair, err := parseKeyPair(c.certVal.Reveal(), c.keyVal.Reveal())
		if err != nil {
			return fmt.Errorf("tls.clientCertificate: %w", err)
		}
		c.pair.Store(pair)
		return nil
	default:
		return nil
	}
}

// clientCertificate is the configuration's GetClientCertificate: the
// current client pair.
func (c *ClientConfig) clientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	if p := c.pair.Load(); p != nil {
		return p, nil
	}
	return &tls.Certificate{}, nil
}

// Current returns the current configuration. It is shared: callers must
// not modify it (ForHost returns a private copy).
func (c *ClientConfig) Current() *tls.Config { return c.cur.Load() }

// ForHost returns a copy of the current configuration for a connection to
// host (an Endpoint or URL host, without port): ServerName is the SNI when
// set, else host; an IP literal verifies IP SANs and sends no SNI. The
// caller may set NextProtos on the copy.
func (c *ClientConfig) ForHost(host string) *tls.Config {
	cfg := c.cur.Load().Clone()
	if cfg.ServerName == "" {
		cfg.ServerName = serverName(host)
	}
	return cfg
}

// Close stops following rotations; the last configuration stays usable.
// It is idempotent.
func (c *ClientConfig) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	stops := c.stops
	c.stops = nil
	c.mu.Unlock()
	stopAll(stops)
}
