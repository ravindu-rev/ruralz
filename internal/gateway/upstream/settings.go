// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/discovery"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Codes Compile returns for an Upstream it cannot serve.
const (
	// codeSchema is a configuration shape validation normally rejects
	// first: an invalid Endpoint address or discovery port, ring-hash
	// without hashKey.
	codeSchema = "RZ-CFG-005"
	// codeUnserved is a feature this Node release does not serve: a
	// protocol other than http, kubernetes discovery (Planned (M2)).
	codeUnserved = "RZ-CFG-040"
)

// Config is one Upstream of a Revision as the snapshot compiler hands it to
// Manager.Compile.
type Config struct {
	// Upstream is the validated resource. Absent fields take the schema
	// defaults of docs/architecture/02-configuration-model.md "Upstream"
	// here, so a canonical form without materialized defaults compiles the
	// same.
	Upstream *v1alpha1.Upstream
	// HashKey is the compiled loadBalancing.hashKey; required with
	// ring-hash.
	HashKey expr.Program
	// RetryOn is the compiled retries.retryOn; nil runs the native form of
	// expr.DefaultRetryOn (resilience.DefaultRetryOn), which
	// TestNativeDefaultsEqualCEL holds equal to the compiled default.
	RetryOn expr.Program
	// FailureWhen is the compiled circuitBreaker.failureWhen; nil runs the
	// native form of expr.DefaultFailureWhen, held equal the same way.
	FailureWhen expr.Program
	// Metrics are the Upstream's handles from the Revision's admission
	// plan (emit.Plan.Upstream); nil records nothing.
	Metrics *emit.UpstreamMetrics
}

// settings is one Upstream's configuration in one snapshot: Upstream.spec
// converted, with the defaults applied.
type settings struct {
	name    string
	scheme  string
	source  discovery.Spec
	alg     balance.Algorithm
	res     resilience.Config
	passive health.PassivePolicy
	active  *health.ActivePolicy
	// tls is the client TLS setting; nil for a cleartext Upstream.
	tls *tlsconf.ClientSpec
	// conn holds the connection bounds of the Upstream's pools.
	conn connBounds
	// poolKey is the protocol, TLS and connection-bound part of the
	// connection pool key (docs/architecture/03-data-plane.md "Upstream
	// layer": pools are keyed by protocol, TLS settings, connection bounds
	// and Endpoint).
	poolKey string
}

// connBounds are the per-connection bounds of an Upstream's pools
// (docs/architecture/09-traffic-management-and-resilience.md "Deadlines"
// and "Health checking and outlier detection"): the dial (connectTimeout,
// whose expiry is error kind connect), the TLS handshake
// (tls.handshakeTimeout, error kind tls) and the HTTP/2 ping
// (healthCheck.pingTimeout, which closes the connection). The bounds are
// part of the pool key, since an http.Transport fixes its HTTP/2 ping
// timeout when it starts.
type connBounds struct {
	connect   time.Duration
	handshake time.Duration
	ping      time.Duration
}

// Check reports whether Manager.Compile can serve c with secrets, without
// creating a runtime, a source or a pool: the settings Compile converts
// (RZ-CFG-040 for an unserved feature, RZ-CFG-005 for a shape validation
// rejects first) and the TLS client configuration it builds from secrets
// (the tlsconf codes, RZ-CFG-026 for a certificate or key that does not
// load). The snapshot compiler checks every Upstream so it reports each
// one Compile would refuse, not only the first.
func Check(c *Config, secrets secret.Store) error {
	st, err := parseSettings(c)
	if err != nil {
		return err
	}
	if st.tls == nil {
		return nil
	}
	cfg, err := tlsconf.Upstream(*st.tls, secrets)
	if err != nil {
		return fmt.Errorf("upstream %q: %w", st.name, err)
	}
	cfg.Close()
	return nil
}

// parseSettings converts one Config. Unserved features are RZ-CFG-040;
// shapes validation rejects first are RZ-CFG-005.
func parseSettings(c *Config) (settings, error) {
	u := c.Upstream
	if u == nil {
		return settings{}, errors.New("upstream: nil Upstream")
	}
	name := u.Metadata.Name
	sp := &u.Spec
	if sp.Protocol != v1alpha1.UpstreamProtocolHTTP {
		return settings{}, errcode.Errorf(codeUnserved, "Upstream %q: protocol %q is not served by this release", name, sp.Protocol)
	}
	st := settings{name: name, scheme: "http", passive: health.PassivePolicy{}.WithDefaults()}
	src, err := sourceSpec(name, sp)
	if err != nil {
		return settings{}, err
	}
	st.source = src
	if st.alg, err = algorithm(sp.LoadBalancing); err != nil {
		return settings{}, errcode.Errorf(codeSchema, "Upstream %q: %w", name, err)
	}
	if st.alg == balance.RingHash && c.HashKey == nil {
		return settings{}, errcode.Errorf(codeSchema, "Upstream %q: ring-hash requires loadBalancing.hashKey", name)
	}
	st.res = resilienceConfig(sp, c)
	st.conn = connBounds{
		connect:   bound(sp.ConnectTimeout, resilience.DefaultConnectTimeout),
		handshake: resilience.DefaultHandshakeTimeout,
		ping:      health.DefaultPingTimeout,
	}
	if hc := sp.HealthCheck; hc != nil {
		if hc.Passive != nil {
			st.passive = passivePolicy(hc.Passive)
		}
		st.passive.StallTimeout = bound(hc.StallTimeout, health.DefaultStallTimeout)
		st.conn.ping = bound(hc.PingTimeout, health.DefaultPingTimeout)
		if hc.Active != nil {
			a := activePolicy(hc.Active)
			st.active = &a
		}
	}
	if t := sp.TLS; t != nil {
		st.scheme = "https"
		st.tls = clientSpec(t)
		st.conn.handshake = bound(t.HandshakeTimeout, resilience.DefaultHandshakeTimeout)
	}
	st.poolKey = poolKey(sp.Protocol, st.tls, st.conn)
	return st, nil
}

// sourceSpec builds the Endpoint source: static endpoints with the weight
// default 1, or discovery.type dns.
func sourceSpec(name string, sp *v1alpha1.UpstreamSpec) (discovery.Spec, error) {
	var src discovery.Spec
	if d := sp.Discovery; d != nil {
		switch d.Type {
		case v1alpha1.DiscoveryTypeDNS:
		case v1alpha1.DiscoveryTypeKubernetes:
			return src, errcode.Errorf(codeUnserved, "Upstream %q: kubernetes discovery is Planned (M2)", name)
		default:
			return src, errcode.Errorf(codeSchema, "Upstream %q: unknown discovery type %q", name, d.Type)
		}
		dns := &discovery.DNSSpec{Service: d.Service}
		if p := d.Port; p != nil {
			if p.IsString {
				dns.PortName = p.Str
			} else if p.Int > 0 && p.Int <= 65535 {
				dns.Port = uint16(p.Int)
			}
		}
		src.DNS = dns
	} else {
		src.Static = make([]discovery.StaticEndpoint, 0, len(sp.Endpoints))
		for _, e := range sp.Endpoints {
			w := uint32(1)
			if e.Weight != nil {
				w = uint32(max(*e.Weight, 0))
			}
			src.Static = append(src.Static, discovery.StaticEndpoint{Address: e.Address, Weight: w})
		}
	}
	if err := src.Validate(); err != nil {
		return src, errcode.Errorf(codeSchema, "Upstream %q: %w", name, err)
	}
	return src, nil
}

// algorithm returns loadBalancing.algorithm, least-request when absent.
func algorithm(lb *v1alpha1.LoadBalancing) (balance.Algorithm, error) {
	if lb == nil || lb.Algorithm == nil {
		return balance.LeastRequest, nil
	}
	return balance.ParseAlgorithm(string(*lb.Algorithm))
}

// resilienceConfig converts timeout, retries and circuitBreaker, starting
// from resilience.DefaultConfig and overriding the fields present.
func resilienceConfig(sp *v1alpha1.UpstreamSpec, c *Config) resilience.Config {
	rc := resilience.DefaultConfig()
	rc.Timeout = duration(sp.Timeout, 0)
	if r := sp.Retries; r != nil {
		rc.Retry.Attempts = count(r.Attempts, rc.Retry.Attempts)
		rc.Retry.PerTryTimeout = duration(r.PerTryTimeout, 0)
	}
	rc.Retry.RetryOn = c.RetryOn
	if b := sp.CircuitBreaker; b != nil {
		rc.Bulkhead.MaxConnections = count(b.MaxConnections, rc.Bulkhead.MaxConnections)
		rc.Bulkhead.MaxPendingRequests = count(b.MaxPendingRequests, rc.Bulkhead.MaxPendingRequests)
		rc.Breaker.ConsecutiveFailures = count(b.ConsecutiveFailures, rc.Breaker.ConsecutiveFailures)
		rc.Breaker.OpenDuration = duration(b.OpenDuration, rc.Breaker.OpenDuration)
	}
	rc.Breaker.FailureWhen = c.FailureWhen
	rc.Normalize()
	return rc
}

// passivePolicy converts healthCheck.passive.
func passivePolicy(p *v1alpha1.PassiveHealthCheck) health.PassivePolicy {
	return health.PassivePolicy{
		ConsecutiveErrors: count(p.ConsecutiveErrors, 0),
		EjectionTime:      duration(p.EjectionTime, 0),
	}.WithDefaults()
}

// activePolicy converts healthCheck.active.
func activePolicy(a *v1alpha1.ActiveHealthCheck) health.ActivePolicy {
	p := health.ActivePolicy{
		Interval:           duration(a.Interval, 0),
		Timeout:            duration(a.Timeout, 0),
		HealthyThreshold:   count(a.HealthyThreshold, 0),
		UnhealthyThreshold: count(a.UnhealthyThreshold, 0),
	}
	if a.Path != nil {
		p.Path = *a.Path
	}
	return p.WithDefaults()
}

// clientSpec converts Upstream.spec.tls.
func clientSpec(t *v1alpha1.UpstreamTLS) *tlsconf.ClientSpec {
	cs := &tlsconf.ClientSpec{SNI: t.SNI}
	ref := func(v *v1alpha1.SecretValue) *secret.Ref {
		if v == nil {
			return nil
		}
		r := secret.RefOf(v.SecretRef)
		return &r
	}
	cs.CACertificate = ref(t.CACertificate)
	cs.ClientCertificate = ref(t.ClientCertificate)
	cs.ClientKey = ref(t.ClientKey)
	return cs
}

// poolKey is the pool key part of an Upstream's settings: the protocol;
// for a TLS setting (cs not nil) its SNI, its references (never values)
// and its handshake bound; then the dial and ping bounds. A Hot Reload that
// keeps them keeps the pools and their connections.
func poolKey(p v1alpha1.UpstreamProtocol, cs *tlsconf.ClientSpec, conn connBounds) string {
	var b strings.Builder
	b.WriteString(string(p))
	if cs != nil {
		b.WriteString("|tls|")
		b.WriteString(cs.SNI)
		for _, r := range []*secret.Ref{cs.CACertificate, cs.ClientCertificate, cs.ClientKey} {
			b.WriteByte('|')
			if r != nil {
				b.WriteString(r.String())
			}
		}
		b.WriteString("|handshake=")
		b.WriteString(conn.handshake.String())
	}
	b.WriteString("|connect=")
	b.WriteString(conn.connect.String())
	b.WriteString("|ping=")
	b.WriteString(conn.ping.String())
	return b.String()
}

// bound returns a connection bound or stall timeout: *d, or def when
// absent. A zero duration, which the schema admits, takes def too, since a
// zero bound would fail every dial, handshake or ping at once and stall
// every attempt.
func bound(d *v1alpha1.Duration, def time.Duration) time.Duration {
	if d == nil || *d <= 0 {
		return def
	}
	return time.Duration(*d)
}

// duration returns *d, or def when absent.
func duration(d *v1alpha1.Duration, def time.Duration) time.Duration {
	if d == nil {
		return def
	}
	return time.Duration(*d)
}

// count returns *n, or def when absent.
func count(n *int32, def int) int {
	if n == nil {
		return def
	}
	return int(*n)
}

// errUnknownUpstream is wrapped with filter.ErrNotAvailable when a leg
// names an Upstream the snapshot does not hold.
var errUnknownUpstream = errors.New("upstream: no such Upstream in the snapshot")

// unknownUpstream returns the error of RunLeg for a name the snapshot does
// not hold.
func unknownUpstream(name string, sentinel error) error {
	return fmt.Errorf("%w %q: %w", errUnknownUpstream, name, sentinel)
}
